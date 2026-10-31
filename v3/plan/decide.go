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

package plan

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/record"
)

// Input は 1 周の判定（§2.1 の段 3〜5）の入力。
type Input struct {
	Resources []core.Resource
	Ranges    []core.Range
	Policies  []core.Policy
	Readers   map[string]core.Reader
	Marks     record.Marks // 前の周の目印（読むだけ）
	Now       time.Time
	// ReportOnly が空でなければ、当てる操作を全部 report にする（その理由）。
	// Record が読めない周・ロックがほかにある周（E-M5・N-M7）。
	ReportOnly string
}

// Decide は List と Observe をして宣言と比べ、prune の守り・依存の待ち・Policy の
// mode と予算で、操作ごとにこの周に当てるかを決める。現実も Record も変えない。
func Decide(ctx context.Context, in Input) Result {
	out := Result{Summary: map[Op]int{}, ReportOnly: in.ReportOnly}
	declared := make(map[core.ID]*core.Resource, len(in.Resources))
	for i := range in.Resources {
		declared[in.Resources[i].ID] = &in.Resources[i]
	}

	// 段 3：範囲ごとに List する。prune の根拠は List の ID だけ（不変条件 8）。
	candidates := map[core.ID]int{} // 宣言外の ID → 範囲の番号
	for i, rg := range in.Ranges {
		st := listRange(ctx, rg, in.Readers[rg.Provider], in.Marks)
		if st.Dropped && rg.Prune {
			out.Halts = append(out.Halts, Halt{Scope: "range " + st.Range,
				Reason: fmt.Sprintf("List が前の周の %d から %d に減った（max_drop %d%% を超える）。この範囲は消さない", *st.Baseline, st.Listed, rg.MaxDrop)})
		}
		for _, id := range st.IDs {
			if _, ok := declared[id]; ok {
				continue
			}
			if excluded(rg, id) {
				continue // 外の持ち主のもの（E-D2・E-T2）
			}
			if _, dup := candidates[id]; !dup {
				candidates[id] = i
			}
		}
		out.Ranges = append(out.Ranges, st)
	}

	// 段 3：観測する。宣言・宣言に無い依存先・宣言外の候補を provider ごとに 1 回で。
	var ids []core.ID
	for _, r := range in.Resources {
		ids = append(ids, r.ID)
		for _, d := range r.DependsOn {
			if _, ok := declared[d.On]; !ok {
				ids = append(ids, d.On)
			}
		}
	}
	candIDs := make([]core.ID, 0, len(candidates))
	for id := range candidates {
		candIDs = append(candIDs, id)
	}
	sort.Slice(candIDs, func(i, j int) bool { return candIDs[i].String() < candIDs[j].String() })
	ids = append(ids, candIDs...)
	obs := Observe(ctx, ids, in.Readers)

	// 段 4：比べる。
	index := map[core.ID]int{}
	for _, r := range in.Resources {
		ks := in.Readers[r.ID.Provider()].Schema().Kinds[r.ID.Kind()]
		c := Compare(r, ks, obs[r.ID])
		c.Destructive = c.Op == Delete || (c.Op == Update && c.Replace)
		c.labels = r.Labels
		index[r.ID] = len(out.Changes)
		out.Changes = append(out.Changes, c)
	}
	for _, id := range candIDs {
		i := candidates[id]
		if c, ok := pruneChange(id, in.Ranges[i], out.Ranges[i], in.Readers[id.Provider()], obs[id]); ok {
			out.Changes = append(out.Changes, c)
		}
	}

	// 段 5：この周に当てるか。
	for i := range out.Changes {
		c := &out.Changes[i]
		if !c.Op.Writes() || c.Action != "" {
			continue
		}
		c.Action = ActApply
		// 上流が宣言どおりでなければ下流は待つ。Unknown の上流の下流を進めない
		// （不変条件 3、E-J7・E-D6）。消す宣言は待たない（下流から消す順序は段 4）。
		if c.Op == Create || c.Op == Update {
			if why := waiting(declared[c.ID], out.Changes, index, obs); why != "" {
				c.Action, c.Blocked = ActWait, why
			}
		}
	}
	for i := range out.Changes {
		c := &out.Changes[i]
		if !c.Op.Writes() {
			continue
		}
		var report []string
		for _, p := range in.Policies {
			if p.Select.Matches(c.ID.Type, c.labels) {
				c.Policies = append(c.Policies, p.Name)
				if p.Mode == core.ModeReport {
					report = append(report, p.Name)
				}
			}
		}
		if c.Action != ActApply {
			continue
		}
		// 当たる Policy の最も厳しいもの（report）が勝つ（E-J5・不変条件 19）。
		switch {
		case len(report) > 0:
			c.Action, c.Blocked = ActReport, fmt.Sprintf("policy %s は report", strings.Join(report, "・"))
		case in.ReportOnly != "":
			c.Action, c.Blocked = ActReport, in.ReportOnly
		}
	}
	checkBudgets(&out, in)

	for _, c := range out.Changes {
		out.Summary[c.Op]++
	}
	sort.SliceStable(out.Changes, func(i, j int) bool { return out.Changes[i].Resource < out.Changes[j].Resource })
	return out
}

// listRange は範囲を List し、前の周の件数と比べる。List の誤り・不完全・別の
// provider の ID が混じる List は「不完全」で、その範囲の宣言外を見ない（E-K2・E-F3）。
func listRange(ctx context.Context, rg core.Range, r core.Reader, m record.Marks) RangeStatus {
	st := RangeStatus{Range: rg.Key(), Provider: rg.Provider, Prune: rg.Prune}
	if b, ok := m.RangeCounts[st.Range]; ok {
		st.Baseline = &b
	}
	if r == nil {
		st.Note = fmt.Sprintf("provider %q が無い", rg.Provider)
		return st
	}
	l, err := r.List(ctx, rg)
	switch {
	case err != nil:
		st.Note = "List に失敗: " + err.Error()
		return st
	case !l.Complete:
		st.Note = "List が不完全"
		return st
	}
	for _, id := range l.IDs {
		if id.Provider() != rg.Provider {
			st.Note = fmt.Sprintf("List が別の provider の ID %s を返した", id)
			return st
		}
	}
	st.Complete, st.Listed, st.IDs = true, len(l.IDs), l.IDs
	if b := st.Baseline; b != nil && *b > 0 && (*b-st.Listed)*100 > *b*rg.MaxDrop {
		st.Dropped = true
	}
	return st
}

func excluded(rg core.Range, id core.ID) bool {
	for _, e := range rg.ExternalOwners {
		if e.Matches(id) {
			return true
		}
	}
	return false
}

// pruneChange は宣言外の 1 件を判定する（不変条件 6・7・9）。消すのは、prune を
// 許す範囲で、List が完全で（ここに来る前に確かめた）、観測が Present で、消せる
// 種類で、前の周の件数があり、急減していないときだけ。観測で Absent なら出さない。
func pruneChange(id core.ID, rg core.Range, st RangeStatus, r core.Reader, o core.Observation) (Change, bool) {
	c := Change{Resource: id.String(), ID: id, Current: o, Range: st.Range, labels: rg.Labels}
	if o.Availability == core.Absent {
		return c, false
	}
	ks, known := r.Schema().Kinds[id.Kind()]
	switch {
	case !rg.Prune:
		c.Op, c.Note = Unmanaged, "範囲は prune: false"
	case o.Availability == core.Unknown:
		c.Op, c.Note = Unknown, o.Note
	case o.Availability == core.Converging:
		c.Op, c.Note = Converging, o.Note
	case o.Availability != core.Present:
		c.Op, c.Note = Unknown, fmt.Sprintf("知らない観測の値 %d", o.Availability)
	case !known:
		c.Op, c.Note = Unmanaged, fmt.Sprintf("provider の Schema に種類 %q が無い", id.Kind())
	case ks.ObserveOnly:
		c.Op, c.Note = Unmanaged, "観測だけの種類"
	case ks.DestructiveDelete:
		c.Op, c.Note = Unmanaged, "中身を失う種類は宣言外でも消さない"
	default:
		c.Op, c.Destructive = Prune, true
		switch {
		case st.Baseline == nil:
			c.Action, c.Blocked = ActHold, "前の周の List の件数が無い（初めての周）ので消さない"
		case st.Dropped:
			c.Action, c.Blocked = ActHold, "List が急減したので消さない"
		}
	}
	return c, true
}

// waiting は上流が済んでいなければ理由を返す。宣言した上流は宣言どおり（None）、
// 宣言に無い上流は Present であること。
func waiting(r *core.Resource, changes []Change, index map[core.ID]int, obs map[core.ID]core.Observation) string {
	if r == nil {
		return ""
	}
	for _, d := range r.DependsOn {
		if i, ok := index[d.On]; ok {
			if op := changes[i].Op; op != None {
				return fmt.Sprintf("上流 %s が %s", d.On, opLabel[op])
			}
			continue
		}
		switch o := obs[d.On]; o.Availability {
		case core.Present:
		case core.Absent:
			return fmt.Sprintf("上流 %s が無い", d.On)
		default:
			return fmt.Sprintf("上流 %s が %s", d.On, o.Availability)
		}
	}
	return ""
}

// checkBudgets は Policy ごとに当てる操作を数え、どれか 1 つでも上限を超えれば
// 超過分の保留でなく周全体を止める（不変条件 20、E-J1）。周をまたいだ壊す変更は
// Record の目印の窓で数える。
func checkBudgets(out *Result, in Input) {
	for _, p := range in.Policies {
		if p.Budget == nil {
			continue
		}
		b := p.Budget
		u := BudgetUse{Policy: p.Name, PerCycle: b.PerCycle, DestructivePerCycle: b.DestructivePerCycle, DestructivePerWindow: b.DestructivePerWindow}
		for _, c := range out.Changes {
			if c.Action != ActApply || !has(c.Policies, p.Name) {
				continue
			}
			u.Changes++
			if c.Destructive {
				u.Destructive++
			}
		}
		if b.DestructivePerWindow != core.Unlimited {
			w := in.Marks.Window(p.Name, b.Window, in.Now)
			u.WindowStart, u.WindowUsed = w.Start, w.Destructive+u.Destructive
		}
		var over []string
		if b.PerCycle != core.Unlimited && u.Changes > b.PerCycle {
			over = append(over, fmt.Sprintf("変更 %d が per_cycle %d を超える", u.Changes, b.PerCycle))
		}
		if b.DestructivePerCycle != core.Unlimited && u.Destructive > b.DestructivePerCycle {
			over = append(over, fmt.Sprintf("壊す変更 %d が destructive_per_cycle %d を超える", u.Destructive, b.DestructivePerCycle))
		}
		if b.DestructivePerWindow != core.Unlimited && u.WindowUsed > b.DestructivePerWindow {
			over = append(over, fmt.Sprintf("窓 %s の壊す変更 %d が destructive_per_window %d を超える", b.Window, u.WindowUsed, b.DestructivePerWindow))
		}
		if len(over) > 0 {
			out.Halts = append(out.Halts, Halt{Scope: "policy " + p.Name, Reason: "予算: " + strings.Join(over, "・")})
		}
		out.Budgets = append(out.Budgets, u)
	}
	budgetHalted := false
	for _, h := range out.Halts {
		budgetHalted = budgetHalted || strings.HasPrefix(h.Scope, "policy ")
	}
	if !budgetHalted {
		return
	}
	for i := range out.Changes {
		if c := &out.Changes[i]; c.Action == ActApply {
			c.Action, c.Blocked = ActHalt, "予算を超えたので周全体を止めた"
		}
	}
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Halted は止めた理由があるか（一回実行の終了コード 4、E-J14）。
func (r Result) Halted() bool { return len(r.Halts) > 0 }

// Remaining はこの周の終わりにずれ・Unknown が残る数（終了コード 3、E-J14）。
// 宣言外で消さないものは数えない。prune の範囲で List が不完全なら数える。
func (r Result) Remaining() int {
	n := 0
	for _, c := range r.Changes {
		if !c.Final().Settled() {
			n++
		}
	}
	for _, st := range r.Ranges {
		if st.Prune && !st.Complete {
			n++
		}
	}
	return n
}

// Failed は当てて失敗した数と周の誤りの数（終了コード 1）。
func (r Result) Failed() int {
	n := len(r.Errors)
	for _, c := range r.Changes {
		if c.Outcome == OutcomeError {
			n++
		}
	}
	return n
}
