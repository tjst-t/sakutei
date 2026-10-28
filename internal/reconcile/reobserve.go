// Copyright 2026 tjst-t
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// 依存先の Apply のあとの観測し直し（§8.1、Backlog #177）。
//
// 観測は周の頭で Apply の前にまとめて行う。そのため、FW の許可（依存先）と、
// その許可を通って初めて届くホストの上のリソース（依存元、depends_on で依存先を
// 指す）を同じ回で組むと、依存元は周の頭では観測できず（unknown）skip される。
// ここでは、同じ回の中で依存先の Apply が効いた（Apply のあとに読み直して収束を
// 確かめた、§11.1 Tier2）あと、それに（直接・間接に）依存していて観測できなかった
// リソースだけを、もう一度観測する。
//
//   - 一回性の reconcile: 観測できれば、その回のうちに Diff に戻し、予算・承認・
//     順序をいまと同じに通して Apply まで進める（pkg/cli）。
//   - 常駐: Apply は非同期で、Apply を出したリソースの依存元は同じ周には出さない
//     （#12 の周の壁）。Apply の確かめのあとに直接の依存元を観測し直し、届けば
//     次の周を待たずに始める（Daemon.wake）。届いたときの観測は使わない（次の周が
//     新しく観測する。観測 state レス）。
//
// 上限: 1 つのリソースを観測し直す回数（MaxPerResource）と、回（常駐では周）の
// 全体で観測し直しに使う時間（Time）。上限に当たったら、いまどおり unknown で
// skip し、次の回（周）に任せる。観測し直しても届かないときも同じ。
//
// 観測し直しは Read（最小階級は ReadFor）で行う。周の BulkObserve の結果は Apply
// の前のものなので使わない。

package reconcile

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// 観測し直しの既定の上限（#177）。
const (
	// DefaultReobserveMax は 1 つのリソースを観測し直す回数の既定。
	DefaultReobserveMax = 3
	// DefaultReobserveTime は回（周）の全体で観測し直しに使う時間の既定。
	DefaultReobserveTime = 30 * time.Second
	// reobserveRetryInterval は同じリソースを続けて観測し直すときの間。
	reobserveRetryInterval = 500 * time.Millisecond
)

// ReobserveLimits は観測し直しの上限（#177）。ゼロ値は既定の上限。
type ReobserveLimits struct {
	// Off は観測し直しをしない（いまどおり次の回に任せる）。
	Off bool
	// MaxPerResource は 1 つのリソースを観測し直す回数（0 は既定）。
	MaxPerResource int
	// Time は回（周）の全体で観測し直しに使う時間（0 は既定）。
	Time time.Duration
}

func (l ReobserveLimits) resolved() ReobserveLimits {
	if l.MaxPerResource <= 0 {
		l.MaxPerResource = DefaultReobserveMax
	}
	if l.Time <= 0 {
		l.Time = DefaultReobserveTime
	}
	return l
}

// String は上限を人が読む形にする。
func (l ReobserveLimits) String() string {
	if l.Off {
		return "off"
	}
	l = l.resolved()
	return fmt.Sprintf("%d per resource, %s per run", l.MaxPerResource, l.Time)
}

// Reobserver は 1 つの回（常駐では周）の観測し直しの回数と時間を数える。並行に
// 使ってよい。
type Reobserver struct {
	lim   ReobserveLimits
	mu    sync.Mutex
	tries map[string]int
	spent time.Duration
}

// NewReobserver は上限 lim の Reobserver を返す。
func NewReobserver(lim ReobserveLimits) *Reobserver {
	return &Reobserver{lim: lim.resolved(), tries: map[string]int{}}
}

// Enabled は観測し直しをするか。
func (r *Reobserver) Enabled() bool { return !r.lim.Off }

// Limits は解決した上限。
func (r *Reobserver) Limits() ReobserveLimits { return r.lim }

// Exhausted は local をもう観測し直せないか（回数か時間の上限）。
func (r *Reobserver) Exhausted(local string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lim.Off || r.tries[local] >= r.lim.MaxPerResource || r.spent >= r.lim.Time
}

// take は 1 回の観測し直しの枠を取る。残りの時間を返す（取れなければ why）。
func (r *Reobserver) take(local string) (left time.Duration, why string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.lim.Off:
		return 0, "re-observation is off"
	case r.tries[local] >= r.lim.MaxPerResource:
		return 0, fmt.Sprintf("re-observation limit reached (%d per resource)", r.lim.MaxPerResource)
	case r.spent >= r.lim.Time:
		return 0, fmt.Sprintf("re-observation time limit reached (%s per run)", r.lim.Time)
	}
	r.tries[local]++
	return r.lim.Time - r.spent, ""
}

func (r *Reobserver) spend(d time.Duration) {
	r.mu.Lock()
	r.spent += d
	r.mu.Unlock()
}

// Observe は res を、届くまで（Availability Ok）上限の中で観測し直す。続けて
// 観測し直すときは間を置く（その間も時間に数える）。届けば ok で観測を返し、
// 届かなければ why に理由（最後に見えたこと、または当たった上限）を返す。
func (r *Reobserver) Observe(ctx context.Context, p provider.Provider, res diff.DesiredResource) (obs core.Observation, ok bool, why string) {
	local := res.ID.Local
	last := ""
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			left, stop := r.leftTime()
			if stop != "" {
				return core.Observation{}, false, last + "; " + stop
			}
			wait := reobserveRetryInterval
			if wait > left {
				wait = left
			}
			start := time.Now()
			select {
			case <-ctx.Done():
				return core.Observation{}, false, last + "; " + ctx.Err().Error()
			case <-time.After(wait):
			}
			r.spend(time.Since(start))
		}
		left, stop := r.take(local)
		if stop != "" {
			if last == "" {
				return core.Observation{}, false, stop
			}
			return core.Observation{}, false, last + "; " + stop
		}
		start := time.Now()
		octx, cancel := context.WithTimeout(ctx, left)
		o, err := ReadFresh(octx, p, res)
		cancel()
		r.spend(time.Since(start))
		switch {
		case err != nil:
			last = fmt.Sprintf("observe error: %v", err)
		case o.Availability != core.AvailabilityOk:
			last = fmt.Sprintf("availability=%v", o.Availability)
		default:
			return o, true, ""
		}
	}
}

// leftTime は残りの時間（尽きていれば stop）。
func (r *Reobserver) leftTime() (time.Duration, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.spent >= r.lim.Time {
		return 0, fmt.Sprintf("re-observation time limit reached (%s per run)", r.lim.Time)
	}
	return r.lim.Time - r.spent, ""
}

// ReadFresh は res をいま読む（最小階級は desired の dry run、§28.7）。
func ReadFresh(ctx context.Context, p provider.Provider, res diff.DesiredResource) (core.Observation, error) {
	if _, ok := p.(provider.CheckReader); ok {
		return provider.ReadFor(ctx, p, res.ID.Local, res.Fields)
	}
	return p.Read(ctx, res.ID.Local)
}

// Settled は、依存元から見て収束しているリソースの集合を返す。base(local) が
// 真で、desired の中の依存先がすべて Settled のもの（Apply の波で上流の失敗が
// 推移的に伝わるのと同じに、間接の依存先も見る）。desired に無い依存先は満たされて
// いるとみなす（buildDAG と同じ）。
func Settled(desired []diff.DesiredResource, base func(local string) bool) map[string]bool {
	byLocal := make(map[string]diff.DesiredResource, len(desired))
	for _, r := range desired {
		byLocal[r.ID.Local] = r
	}
	memo := make(map[string]bool, len(desired))
	visiting := map[string]bool{}
	var settled func(local string) bool
	settled = func(local string) bool {
		r, ok := byLocal[local]
		if !ok {
			return true
		}
		if v, ok := memo[local]; ok {
			return v
		}
		if visiting[local] {
			return false // 循環は読み込みで断っている。念のため収束とみなさない。
		}
		visiting[local] = true
		v := base(local)
		for _, dep := range r.DependsOn {
			if v && !settled(dep) {
				v = false
			}
		}
		delete(visiting, local)
		memo[local] = v
		return v
	}
	out := make(map[string]bool, len(desired))
	for _, r := range desired {
		if settled(r.ID.Local) {
			out[r.ID.Local] = true
		}
	}
	return out
}

// ReobserveCandidates は観測し直す候補を desired の順で返す: unreached（周の頭で
// 観測できなかった）のうち、直接の依存先がすべて settled で、依存先を（直接・
// 間接に）たどると applied（この回に Apply して収束を確かめた）に行き当たるもの。
func ReobserveCandidates(desired []diff.DesiredResource, unreached, settled, applied map[string]bool) []diff.DesiredResource {
	byLocal := make(map[string]diff.DesiredResource, len(desired))
	for _, r := range desired {
		byLocal[r.ID.Local] = r
	}
	memo := map[string]bool{}
	var reaches func(local string, seen map[string]bool) bool
	reaches = func(local string, seen map[string]bool) bool {
		if v, ok := memo[local]; ok {
			return v
		}
		if seen[local] {
			return false
		}
		seen[local] = true
		v := false
		for _, dep := range byLocal[local].DependsOn {
			if applied[dep] || reaches(dep, seen) {
				v = true
				break
			}
		}
		memo[local] = v
		return v
	}
	var out []diff.DesiredResource
	for _, r := range desired {
		if !unreached[r.ID.Local] {
			continue
		}
		ready := true
		for _, dep := range r.DependsOn {
			if _, in := byLocal[dep]; in && !settled[dep] {
				ready = false
				break
			}
		}
		if ready && reaches(r.ID.Local, map[string]bool{}) {
			out = append(out, r)
		}
	}
	return out
}

// ReobserveSubDAG は観測し直して届いた recovered と、その依存元（eligible なもの）
// からなる部分の DAG を desired の順で返す。依存元は、直接の依存先がすべて部分の
// DAG の中か settled のときだけ入る（部分の外の依存先の失敗を見落とさないため。
// ApplyWaves は desired の外の依存先を満たされているとみなす）。
func ReobserveSubDAG(desired []diff.DesiredResource, recovered, settled map[string]bool, eligible func(local string) bool) []diff.DesiredResource {
	in := make(map[string]bool, len(recovered))
	for local := range recovered {
		in[local] = true
	}
	inDesired := make(map[string]bool, len(desired))
	for _, r := range desired {
		inDesired[r.ID.Local] = true
	}
	for changed := true; changed; {
		changed = false
		for _, r := range desired {
			local := r.ID.Local
			if in[local] || settled[local] || !eligible(local) {
				continue
			}
			touches, ok := false, true
			for _, dep := range r.DependsOn {
				switch {
				case in[dep]:
					touches = true
				case settled[dep] || !inDesired[dep]:
				default:
					ok = false
				}
			}
			if touches && ok {
				in[local] = true
				changed = true
			}
		}
	}
	var out []diff.DesiredResource
	for _, r := range desired {
		if in[r.ID.Local] {
			out = append(out, r)
		}
	}
	return out
}

// DirectDependents は local に直接依存する desired の中のリソース。
func DirectDependents(desired []diff.DesiredResource, local string) []diff.DesiredResource {
	var out []diff.DesiredResource
	for _, r := range desired {
		for _, dep := range r.DependsOn {
			if dep == local {
				out = append(out, r)
				break
			}
		}
	}
	return out
}
