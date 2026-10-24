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

package eventlog

// level の報告（§22.3: 収束失敗・ずれ・halt は firing / resolved で出し、毎周同じ
// 知らせを撒かない）。
//
// Level は常駐ループの周をまたいで、続いている level の Event（core.EventKind.IsLevel）
// をキー（kind + resource・scope・provider）ごとに覚える。状態は S3（メモリだけ）。
// デーモンを起こし直すと空から始まるので、続いているものは改めて firing になる
// （取りこぼさない側）。
//
//   - 初めて出た周: state=firing・count=1・since=その時刻で出す。
//   - 続く周: count を 1 つ上げ、内容（fingerprint）が変わったときと stuck に
//     上がったときだけ firing（更新）として出す。それ以外は出さない。
//   - 消えた周: その周に上がらず、呼び手が「消えた」と言えたもの（EndCycle の
//     resolvable）を state=resolved で 1 回出して忘れる。
//
// transient → stuck: 続いた周数がしきい値（stuckAfter、既定 DefaultStuckAfter）に
// 達したら上げる。permanent な失敗（権限・欠損など。Attrs["failure"]="permanent"）と
// 容量の halt は初めから stuck。stuck の Event は severity=critical。

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/pkg/core"
)

// DefaultStuckAfter は transient → stuck に上げる連続の周数の既定値（§22.3）。
const DefaultStuckAfter = 5

// AttrFailure は失敗の分類の Attrs のキー。値 FailurePermanent は初めから stuck。
const (
	AttrFailure      = "failure"
	FailurePermanent = "permanent"
)

// volatileAttrs は毎周変わりうる数で、内容の変化とみなさない Attrs のキー。
var volatileAttrs = map[string]bool{
	"consecutive_unreachable_skips": true,
	"held":                          true,
}

// volatileMessage は Message が毎周変わる（前の周との比較・連続回数を含む）kind。
// これらは Message を内容の比較に使わない（中身は Attrs で比べる）。
var volatileMessage = map[core.EventKind]bool{
	core.EventDrift:                             true,
	core.EventCycleSkipped:                      true,
	core.EventExternalInputUnreachableProlonged: true,
	// v3.18 (Backlog #112): a RequiresTask Message carries elapsed/waiting
	// time or a launched Task's ID, which changes every cycle even when the
	// reason (and its Severity) has not; comparing it would defeat leveling.
	core.EventRequiresTask: true,
}

// Level は level の Event の状態（S3）。
type Level struct {
	mu         sync.Mutex
	stuckAfter int
	now        func() time.Time
	active     map[string]*levelEntry
	// raised はこの周に上がったキー（EndCycle で空にする）。
	raised map[string]bool
}

type levelEntry struct {
	last  core.Event // この周までに上がった最後の Event（元の形）
	fp    string
	since time.Time
	count int
	class core.EventClass
}

// NewLevel は空の Level を返す。stuckAfter <= 0 なら DefaultStuckAfter。
func NewLevel(stuckAfter int) *Level {
	if stuckAfter <= 0 {
		stuckAfter = DefaultStuckAfter
	}
	return &Level{stuckAfter: stuckAfter, now: time.Now,
		active: map[string]*levelEntry{}, raised: map[string]bool{}}
}

// StuckAfter は transient → stuck に上げる連続の周数を返す。
func (l *Level) StuckAfter() int { return l.stuckAfter }

// Key は ev の level のキー（kind + resource・scope・provider）。
func Key(ev core.Event) string {
	var b strings.Builder
	b.WriteString(string(ev.Kind))
	b.WriteByte('|')
	if ev.HasResource() {
		b.WriteString(ev.Resource.String())
	}
	b.WriteByte('|')
	b.WriteString(ev.Scope.String())
	b.WriteByte('|')
	b.WriteString(ev.Attrs["provider"])
	return b.String()
}

// fingerprint は ev の内容（変わったら更新として出す）。
func fingerprint(ev core.Event) string {
	var b strings.Builder
	b.WriteString(string(ev.Severity))
	b.WriteByte('|')
	b.WriteString(string(ev.Cause))
	b.WriteByte('|')
	if !volatileMessage[ev.Kind] {
		b.WriteString(ev.Message)
	}
	keys := make([]string, 0, len(ev.Attrs))
	for k := range ev.Attrs {
		if !volatileAttrs[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "|%s=%s", k, ev.Attrs[k])
	}
	return b.String()
}

// permanent は ev が初めから stuck か（権限・欠損・容量、§22.3）。
func permanent(ev core.Event) bool {
	return ev.Attrs[AttrFailure] == FailurePermanent || ev.Kind == core.EventCapacityHalted
}

// Wrap は w に Emit される level の Event を l に通す Emitter を返す。level で
// ない Event と Event でない書き込みは w にそのまま渡す。
func (l *Level) Wrap(w io.Writer) io.Writer {
	return &leveled{l: l, w: w}
}

type leveled struct {
	l *Level
	w io.Writer
}

func (x *leveled) Write(p []byte) (int, error) { return x.w.Write(p) }

func (x *leveled) Emit(ev core.Event) {
	// An Event with a State already set came from a Level (EndCycle's
	// resolved); it passes as is.
	if !ev.Kind.IsLevel() || ev.State != "" {
		Emit(x.w, ev)
		return
	}
	if out, ok := x.l.observe(ev); ok {
		Emit(x.w, out)
	}
}

// observe は ev を覚え、出すなら出す形の Event と true を返す。
func (l *Level) observe(ev core.Event) (core.Event, bool) {
	if ev.Time.IsZero() {
		ev.Time = l.now()
	}
	k, fp := Key(ev), fingerprint(ev)
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.active[k]
	if e == nil {
		e = &levelEntry{last: ev, fp: fp, since: ev.Time, count: 1, class: l.classify(ev, 1)}
		l.active[k] = e
		l.raised[k] = true
		return e.stamp(ev, false), true
	}
	if !l.raised[k] {
		// この周で初めて: 続いた周数を 1 つ上げる。
		l.raised[k] = true
		e.count++
	}
	changed := fp != e.fp
	class := l.classify(ev, e.count)
	if e.class == core.EventStuck {
		class = core.EventStuck // 続く間は下げない
	}
	promoted := class != e.class
	e.last, e.fp, e.class = ev, fp, class
	if !changed && !promoted {
		return core.Event{}, false
	}
	return e.stamp(ev, true), true
}

func (l *Level) classify(ev core.Event, count int) core.EventClass {
	if permanent(ev) || count >= l.stuckAfter {
		return core.EventStuck
	}
	return core.EventTransient
}

// stamp は ev に firing の state・since・count・class を付ける。update は続く周の
// 更新（内容の変化か stuck への昇格）で、text の文の末尾に count と since を足す。
func (e *levelEntry) stamp(ev core.Event, update bool) core.Event {
	ev.State = core.EventFiring
	ev.Since = e.since
	ev.Count = e.count
	ev.Class = e.class
	if e.class == core.EventStuck {
		ev.Severity = core.SeverityCritical
	}
	// text: the first firing keeps the line the path always printed (count=1,
	// since=now); an update says how long it has held.
	if update {
		ev.Text = fmt.Sprintf("%s [firing %s count=%d since=%s]", ev.TextLine(), e.class, e.count,
			e.since.UTC().Format(time.RFC3339))
	}
	return ev
}

// EndCycle は周の終わりに呼ぶ。この周に上がらなかったキーのうち、resolvable が
// 「消えた」と言えるもの（引数はそのキーで最後に上がった Event）を
// state=resolved で w に 1 回ずつ出して忘れる。resolvable が false のもの
// （この周に確かめられなかったもの：観測していない・周を飛ばした）は覚えたまま。
// cycle は text の文に出す周の番号（0 なら出さない）。
func (l *Level) EndCycle(w io.Writer, cycle int, resolvable func(core.Event) bool) {
	l.mu.Lock()
	var resolved []core.Event
	keys := make([]string, 0, len(l.active))
	for k := range l.active {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	now := l.now()
	for _, k := range keys {
		e := l.active[k]
		if l.raised[k] || resolvable == nil || !resolvable(e.last) {
			continue
		}
		delete(l.active, k)
		resolved = append(resolved, e.resolvedEvent(now, cycle))
	}
	l.raised = map[string]bool{}
	l.mu.Unlock()
	for _, ev := range resolved {
		Emit(w, ev)
	}
}

// resolvedEvent は e が消えたことの Event。
func (e *levelEntry) resolvedEvent(now time.Time, cycle int) core.Event {
	last := e.last
	prefix := "sakutei: "
	if cycle > 0 {
		prefix = fmt.Sprintf("sakutei: cycle %d: ", cycle)
	}
	msg := fmt.Sprintf("resolved after %d cycle(s) (firing since %s)", e.count, e.since.UTC().Format(time.RFC3339))
	return core.Event{
		Kind:     last.Kind,
		Severity: core.SeverityInfo,
		Time:     now,
		Resource: last.Resource,
		Scope:    last.Scope,
		Cause:    last.Cause,
		Message:  msg,
		Attrs:    last.Attrs,
		State:    core.EventResolved,
		Since:    e.since,
		Count:    e.count,
		Class:    e.class,
		Cycle:    cycle,
		Text:     fmt.Sprintf("%sRESOLVED %s%s: %s", prefix, last.Kind, target(last), msg),
	}
}

// target は text の文で level の Event の対象を示す部分（" <resource>" など）。
func target(ev core.Event) string {
	switch {
	case ev.HasResource():
		return " " + ev.Resource.String()
	case len(ev.Scope.Labels) > 0:
		return " " + ev.Scope.String()
	case ev.Attrs["provider"] != "":
		return " provider=" + ev.Attrs["provider"]
	}
	return ""
}

// Active は続いている level の Event の数を返す。
func (l *Level) Active() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.active)
}
