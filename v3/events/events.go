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

// Package events は周の結果から Event を作り（設計 §3.9・§5.2）、sink へ送り出す。
//
// 状態型の Event（Drift・Unknown・Converging・Unmanaged・Waiting・Halt・
// DeleteRefused）は、キー（種類＋対象）ごとに firing になった周と resolved に
// なった周にだけ送り出しの列へ積む（E-H1・N-H2）。続いた周は count を数え、
// cycle.stuck_after の周数で 1 回だけ stuck を積む。確かめられなかった周
// （対象が Unknown・作業中、範囲の List が不完全）には resolved にせず、count も
// 進めない（不変条件 29）。
//
// 通知の状態は Record に置き、メモリに持たない。だから一回実行の列と常駐が
// 同じ Event を出す（不変条件 30、E-O3）。
package events

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/plan"
)

// 状態型の Event の種類（§5.2）。
const (
	KindDrift         = "Drift"
	KindUnknown       = "Unknown"
	KindConverging    = "Converging"
	KindUnmanaged     = "Unmanaged"
	KindWaiting       = "Waiting"
	KindHalt          = "Halt"
	KindDeleteRefused = "DeleteRefused"
	// 段 5（§3.6・§3.7）。
	KindApprovalPending      = "ApprovalPending"      // 承認待ち（Policy・gate・続ける承認、E-L2）
	KindFrozen               = "Frozen"               // 一時的な Policy が効いている（E-J4・N-Q1）
	KindProcedureNeedsDaemon = "ProcedureNeedsDaemon" // 一回実行が critical の段の手前（§2.2）
)

// 出来事型の Event の種類（§5.2）。
const (
	KindApplied   = "Applied"
	KindDeleted   = "Deleted"
	KindCycleDone = "CycleDone"
	// KindProcedureStep は手順の段を済ませた・手順が済んだ・取り消した（§3.7）。
	KindProcedureStep = "ProcedureStep"
)

// Input は Derive の入力。
type Input struct {
	Result     plan.Result
	Prev       []core.Event // 前の周までに firing の Event（Record の events/state.json）
	Seq        uint64       // 最後に積んだ Event の番号
	Now        time.Time
	Duration   time.Duration // 周にかかった時間（CycleDone、N-H4）
	StuckAfter int
}

// Output は Derive の結果。
type Output struct {
	Firing  []core.Event // 次の周に渡す firing の一覧
	Emitted []core.Event // この周に送り出しの列へ積む Event（番号つき）
	Seq     uint64       // 最後に積んだ番号
}

// Derive は周の結果から通知の状態を進める。入力だけから決まる（同じ Record と
// 結果からは同じ Event、不変条件 30）。
func Derive(in Input) Output {
	r := in.Result
	now := map[string]core.Event{}
	add := func(e core.Event) {
		e.Type, e.State = core.StateEvent, core.Firing
		now[e.Key()] = e
	}
	changes := map[string]plan.Change{}
	for _, c := range r.Changes {
		changes[c.Resource] = c
		res := core.Event{Resource: c.Resource, Labels: c.Labels()}
		f := c.Final()
		switch f {
		case plan.Create, plan.Update, plan.Delete, plan.Drift:
			res.Kind, res.Message = KindDrift, driftMessage(c)
			add(res)
		case plan.Prune, plan.Unmanaged:
			res.Kind, res.Message = KindUnmanaged, driftMessage(c)
			res.Scope = c.Range
			add(res)
		case plan.Unknown:
			res.Kind, res.Message = KindUnknown, note(c)
			add(res)
		case plan.Converging:
			res.Kind, res.Message = KindConverging, note(c)
			add(res)
		}
		if c.Action == plan.ActWait {
			add(core.Event{Kind: KindWaiting, Resource: c.Resource, Labels: c.Labels(), Message: c.Blocked})
		}
		if c.Action == plan.ActPending {
			add(core.Event{Kind: KindApprovalPending, Resource: c.Resource, Labels: c.Labels(), Message: c.Blocked})
		}
		if c.Outcome == plan.OutcomeRefused {
			add(core.Event{Kind: KindDeleteRefused, Resource: c.Resource, Labels: c.Labels(), Message: c.Error})
		}
	}
	ranges := map[string]plan.RangeStatus{}
	for _, st := range r.Ranges {
		ranges[st.Range] = st
		if !st.Complete {
			add(core.Event{Kind: KindUnknown, Scope: "range " + st.Range, Message: st.Note})
		}
	}
	for _, h := range r.Halts {
		add(core.Event{Kind: KindHalt, Scope: h.Scope, Message: h.Reason})
	}
	for _, t := range r.Temporary {
		msg := fmt.Sprintf("%s を report（%s）。理由: %s", t.Select, t.Source, t.Reason)
		if !t.Until.IsZero() {
			msg += "。期限 " + t.Until.Format(time.RFC3339)
		}
		if t.By != "" {
			msg += "。" + t.By
		}
		add(core.Event{Kind: KindFrozen, Scope: "policy " + t.Name, Message: msg})
	}
	for _, s := range r.Runs {
		if !s.Active() || s.Waiting == "" {
			continue
		}
		e := core.Event{Kind: KindWaiting, Scope: "procedure " + s.Procedure + " " + s.Target, Labels: s.Labels, Message: s.Waiting}
		switch {
		case s.NeedsDaemon:
			e.Kind = KindProcedureNeedsDaemon
		case s.Request != "":
			e.Kind = KindApprovalPending
		}
		if s.Instruction != "" {
			e.Message += "。人の作業: " + s.Instruction
		}
		add(e)
	}
	// 止まった手順の続ける承認（E-A5・E-K5）。
	for _, s := range r.Runs {
		if s.State == "halted" && s.Request != "" {
			add(core.Event{Kind: KindApprovalPending, Scope: "procedure " + s.Procedure + " " + s.Target, Labels: s.Labels, Message: s.Waiting})
		}
	}

	out := Output{Seq: in.Seq}
	emit := func(e core.Event) {
		out.Seq++
		e.Seq, e.At = out.Seq, in.Now
		out.Emitted = append(out.Emitted, e)
	}
	prev := map[string]core.Event{}
	for _, e := range in.Prev {
		prev[e.Key()] = e
	}

	// 前から firing のもの：続いていれば数え、確かめて消えていれば resolved、
	// 確かめられなければそのまま（不変条件 29）。
	for _, key := range sortedKeys(prev) {
		p := prev[key]
		if cur, ok := now[key]; ok {
			p.Count++
			p.Message, p.Labels, p.Scope = cur.Message, cur.Labels, cur.Scope
			if in.StuckAfter > 0 && p.Count >= in.StuckAfter && !p.Stuck {
				p.Stuck = true
				emit(p) // 積む写しは新しい番号。一覧は firing になった番号のまま
			}
			out.Firing = append(out.Firing, p)
			continue
		}
		if !confirmed(p, changes, ranges) {
			out.Firing = append(out.Firing, p)
			continue
		}
		p.State = core.Resolved
		p.Stuck = false
		p.Message = resolvedMessage(p, changes)
		emit(p)
	}
	// 新しく firing になったもの。
	for _, key := range sortedKeys(now) {
		if _, ok := prev[key]; ok {
			continue
		}
		e := now[key]
		since := in.Now
		e.Since, e.Count = &since, 1
		if in.StuckAfter == 1 {
			e.Stuck = true
		}
		emit(e)
		e.Seq, e.At = out.Seq, in.Now
		out.Firing = append(out.Firing, e)
	}
	sort.SliceStable(out.Firing, func(i, j int) bool { return out.Firing[i].Key() < out.Firing[j].Key() })

	// 出来事型。
	for _, c := range r.Changes {
		if c.Outcome != plan.OutcomeOK {
			continue
		}
		e := core.Event{Type: core.IncidentEvent, Resource: c.Resource, Labels: c.Labels()}
		switch c.Op {
		case plan.Create, plan.Update:
			e.Kind = KindApplied
			var names []string
			for _, f := range c.Fields {
				names = append(names, f.Name)
			}
			e.Message = fmt.Sprintf("%s（%s）", c.Op, strings.Join(names, ", "))
		case plan.Delete, plan.Prune:
			e.Kind, e.Message = KindDeleted, string(c.Op)
		default:
			continue
		}
		emit(e)
	}
	for _, s := range r.Runs {
		scope := "procedure " + s.Procedure + " " + s.Target
		if s.RunID == "" {
			continue
		}
		for _, name := range s.Passed {
			emit(core.Event{Kind: KindProcedureStep, Type: core.IncidentEvent, Scope: scope, Labels: s.Labels,
				Message: fmt.Sprintf("%s: 段 %s を済ませた", s.RunID, name)})
		}
		switch s.State {
		case "done", "cancelled":
			emit(core.Event{Kind: KindProcedureStep, Type: core.IncidentEvent, Scope: scope, Labels: s.Labels,
				Message: fmt.Sprintf("%s: %s（%s）", s.RunID, s.State, s.Reason)})
		}
	}
	emit(cycleDone(r, in.Duration))
	return out
}

// confirmed は前の周に firing だった p が、この周に確かめて消えたか。
func confirmed(p core.Event, changes map[string]plan.Change, ranges map[string]plan.RangeStatus) bool {
	if p.Resource == "" {
		switch {
		case strings.HasPrefix(p.Scope, "range "):
			// 範囲の List の不完全・急減：範囲が完全に List できた周に消える。
			// 範囲を宣言から外したなら消える。
			st, ok := ranges[strings.TrimPrefix(p.Scope, "range ")]
			return !ok || st.Complete
		default:
			// Policy の予算は毎周数え直す。
			return true
		}
	}
	c, ok := changes[p.Resource]
	if !ok {
		if p.Kind == KindUnmanaged {
			// 宣言外：その範囲が完全に List できて出てこなかった（または範囲を
			// 外した）ときだけ消える。
			st, ok := ranges[p.Scope]
			return !ok || st.Complete
		}
		// 宣言から外し、どの範囲の宣言外にも出ない。
		return true
	}
	f := c.Final()
	switch p.Kind {
	case KindUnknown:
		return f != plan.Unknown
	case KindConverging:
		return f != plan.Converging
	}
	if f == plan.Unknown || f == plan.Converging {
		return false
	}
	if p.Kind == KindDeleteRefused {
		// 消す操作がまだ残る（この周は試していない）なら、断りが続くかは分からない。
		return f != plan.Delete && f != plan.Prune
	}
	return true
}

func resolvedMessage(p core.Event, changes map[string]plan.Change) string {
	if p.Resource != "" {
		if _, ok := changes[p.Resource]; !ok && p.Kind != KindUnmanaged {
			return "宣言から外した"
		}
	}
	return ""
}

func driftMessage(c plan.Change) string {
	var parts []string
	if c.Note != "" {
		parts = append(parts, c.Note)
	}
	var names []string
	for _, f := range c.Fields {
		names = append(names, f.Name)
	}
	if len(names) > 0 {
		parts = append(parts, string(c.Final())+": "+strings.Join(names, ", "))
	} else {
		parts = append(parts, string(c.Final()))
	}
	switch {
	case c.Outcome == plan.OutcomeError:
		parts = append(parts, "失敗: "+c.Error)
	case c.Action != "" && c.Action != plan.ActApply:
		parts = append(parts, string(c.Action)+": "+c.Blocked)
	}
	return strings.Join(parts, "。")
}

func note(c plan.Change) string {
	if c.AfterNote != "" {
		return c.AfterNote
	}
	return c.Note
}

// cycleDone は周の件数と時間（E-H3・N-H4）。数えたものだけを載せる。
func cycleDone(r plan.Result, d time.Duration) core.Event {
	v := map[string]int{"duration_ms": int(d / time.Millisecond)}
	for _, c := range r.Changes {
		switch c.Final() {
		case plan.Create, plan.Update, plan.Delete, plan.Drift:
			v["drift"]++
		case plan.Unknown:
			v["unknown"]++
		case plan.Converging:
			v["converging"]++
		case plan.Prune, plan.Unmanaged:
			v["unmanaged"]++
		}
		switch c.Outcome {
		case plan.OutcomeOK:
			v["applied"]++
		case plan.OutcomeError:
			v["failed"]++
		}
	}
	v["resources"] = len(r.Changes)
	v["halts"] = len(r.Halts)
	for _, u := range r.Budgets {
		if u.PerCycle != core.Unlimited {
			v["budget_remaining."+u.Policy] = u.PerCycle - u.Changes
		}
		if u.DestructivePerWindow != core.Unlimited {
			v["budget_window_remaining."+u.Policy] = u.DestructivePerWindow - u.WindowUsed
		}
	}
	return core.Event{Kind: KindCycleDone, Type: core.IncidentEvent, Values: v,
		Message: fmt.Sprintf("drift %d・unknown %d・止めた %d", v["drift"], v["unknown"], v["halts"])}
}

func sortedKeys(m map[string]core.Event) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
