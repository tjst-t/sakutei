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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/approval"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/graph"
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
	// Checks は Condition の check(名前) の確かめ（E-B9）。
	Checks map[string]core.CheckFunc
	// Only が nil でなければ、この宣言だけを観測し直して判定する（同じ周の観測
	// し直し、E-J7）。範囲の List と prune はしない。
	Only map[core.ID]bool
	// Used はこの周の前の波で当てた数（Policy ごと）。予算はこれと合わせて数える
	// （E-J1・不変条件 20）。
	Used map[string]BudgetUse
	// Approvals は Record の承認の依頼と判断（読むだけ、E-L2・不変条件 21）。
	Approvals map[string]record.Approval
	// Handover は手順が受け持つ項目（対象 → drift_fields）。収束では当てない
	// （E-F2・E-A5・N-A4）。
	Handover map[core.ID][]string
	// Held は止まった手順の対象（→ 理由）。収束でも宣言どおりに戻さない
	// （不変条件 18、E-A9）。
	Held map[core.ID]string
}

// Strip は手順が受け持つ項目を宣言から外す（収束が比べて当てるのは残りだけ、
// E-F2・E-A5・N-A4）。
func Strip(res []core.Resource, handover map[core.ID][]string) []core.Resource {
	if len(handover) == 0 {
		return res
	}
	out := make([]core.Resource, len(res))
	for i, r := range res {
		if fields, ok := handover[r.ID]; ok && r.Fields != nil {
			f := make(core.Fields, len(r.Fields))
			for k, v := range r.Fields {
				if !has(fields, k) {
					f[k] = v
				}
			}
			r.Fields = f
		}
		out[i] = r
	}
	return out
}

// Decide は List と Observe をして宣言と比べ、prune の守り・依存の待ち・Policy の
// mode と予算・効かない回数で、操作ごとにこの周に当てるかを決める。現実も
// Record も変えない。
func Decide(ctx context.Context, in Input) Result {
	out := Result{Summary: map[Op]int{}, ReportOnly: in.ReportOnly}
	full := in.Resources
	resources := Strip(in.Resources, in.Handover)
	g := graph.New(resources)
	subjects := resources
	if in.Only != nil {
		subjects = nil
		for _, r := range resources {
			if in.Only[r.ID] {
				subjects = append(subjects, r)
			}
		}
	}

	// 段 3：範囲ごとに List する。prune の根拠は List の ID だけ（不変条件 8）。
	candidates := map[core.ID]int{} // 宣言外の ID → 範囲の番号
	if in.Only == nil {
		for i, rg := range in.Ranges {
			st := listRange(ctx, rg, in.Readers[rg.Provider], in.Marks)
			if st.Dropped && rg.Prune {
				out.Halts = append(out.Halts, Halt{Scope: "range " + st.Range,
					Reason: fmt.Sprintf("List が前の周の %d から %d に減った（max_drop %d%% を超える）。この範囲は消さない", *st.Baseline, st.Listed, rg.MaxDrop)})
			}
			for _, id := range st.IDs {
				if g.Resource(id) != nil {
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
	}

	// 段 3：観測する。対象の宣言・待ちの判定に要るもの（上流・when の参照・消す
	// 順序を見る下流）・宣言外の候補を provider ごとに 1 回で（E-D8）。
	var ids []core.ID
	for _, r := range subjects {
		ids = append(ids, r.ID)
		ids = append(ids, g.Refs(r.ID)...)
		ids = append(ids, g.Dependents(r.ID)...)
	}
	candIDs := make([]core.ID, 0, len(candidates))
	for id := range candidates {
		candIDs = append(candIDs, id)
	}
	sort.Slice(candIDs, func(i, j int) bool { return candIDs[i].String() < candIDs[j].String() })
	ids = append(ids, candIDs...)
	obs := Observe(ctx, ids, in.Readers)

	// 段 4：比べる。観測した宣言は全部比べる（上流が宣言どおりかを見る）。
	ops := map[core.ID]Op{}
	for id := range obs {
		if r := g.Resource(id); r != nil {
			ops[id] = Compare(*r, in.Readers[id.Provider()].Schema().Kinds[id.Kind()], obs[id]).Op
		}
	}
	fullByID := make(map[core.ID]core.Resource, len(full))
	for _, r := range full {
		fullByID[r.ID] = r
	}
	for _, r := range subjects {
		ks := in.Readers[r.ID.Provider()].Schema().Kinds[r.ID.Kind()]
		c := Compare(r, ks, obs[r.ID])
		if fields, ok := in.Handover[r.ID]; ok && obs[r.ID].Availability == core.Present {
			// 手順が受け持つ項目のずれ（手順を起こすきっかけ、E-F2）。
			for _, f := range Compare(fullByID[r.ID], ks, obs[r.ID]).Fields {
				if has(fields, f.Name) {
					c.Handover = append(c.Handover, f.Name)
				}
			}
		}
		c.Destructive = c.Op == Delete || (c.Op == Update && c.Replace)
		c.labels = r.Labels
		c.DesiredHash = DesiredHash(r)
		if c.Op.Writes() {
			for _, d := range g.Downstream(r.ID) {
				c.Downstream = append(c.Downstream, d.String())
			}
		}
		out.Changes = append(out.Changes, c)
	}
	for _, id := range candIDs {
		i := candidates[id]
		if c, ok := pruneChange(id, in.Ranges[i], out.Ranges[i], in.Readers[id.Provider()], obs[id]); ok {
			out.Changes = append(out.Changes, c)
		}
	}

	// 段 5：この周に当てるか。
	env := &condEnv{obs: obs, g: g, checks: runChecks(ctx, subjects, in.Checks, obs)}
	for i := range out.Changes {
		c := &out.Changes[i]
		if !c.Op.Writes() {
			continue
		}
		limit := core.Unlimited
		for _, p := range in.Policies {
			if p.Select.Matches(c.ID.Type, c.labels) {
				c.Policies = append(c.Policies, p.Name)
				if b := p.Budget; b != nil && b.MaxIneffectiveApplies != core.Unlimited && (limit == core.Unlimited || b.MaxIneffectiveApplies < limit) {
					limit = b.MaxIneffectiveApplies
				}
			}
		}
		if limit == core.Unlimited {
			limit = core.DefaultMaxIneffectiveApplies
		}
		c.IneffectiveLimit = limit
		if c.Action != "" {
			continue // prune の守り
		}
		c.Action = ActApply
		if why, ok := in.Held[c.ID]; ok {
			c.Action, c.Blocked = ActKeep, why
			continue
		}
		// 依存の待ち。Unknown の上流の下流を進めない（不変条件 3、E-J7・E-D6）。
		switch c.Op {
		case Create, Update:
			if act, why := ready(g.Resource(c.ID), ops, obs, env); act != ActApply {
				c.Action, c.Blocked = act, why
				if act == ActHalt {
					out.Halts = append(out.Halts, Halt{Scope: "resource " + c.Resource, Reason: why})
				}
			}
		case Delete:
			// 消す宣言は、それに依る宣言が無くなってから（下流から消す、§3.5）。
			if why := dependentsLeft(c.ID, g, obs); why != "" {
				c.Action, c.Blocked = ActWait, why
			}
		}
	}
	for i := range out.Changes {
		c := &out.Changes[i]
		if c.Action != ActApply {
			continue
		}
		var report []string
		for _, p := range in.Policies {
			if p.Mode == core.ModeReport && has(c.Policies, p.Name) {
				report = append(report, p.Name)
			}
		}
		// 当たる Policy の最も厳しいもの（report）が勝つ（E-J5・不変条件 19）。
		switch {
		case len(report) > 0:
			c.Action, c.Blocked = ActReport, fmt.Sprintf("policy %s は report", strings.Join(report, "・"))
		case in.ReportOnly != "":
			c.Action, c.Blocked = ActReport, in.ReportOnly
		default:
			// 効かない Apply が上限に達したものは当てない（不変条件 14、E-J6・E-K10）。
			// 宣言を直せば数え直す。
			if m, ok := in.Marks.Ineffective[c.Resource]; ok && m.Desired == c.DesiredHash && m.Count >= c.IneffectiveLimit {
				c.Ineffective = m.Count
				c.Action = ActHalt
				c.Blocked = fmt.Sprintf("効かない Apply が %d 回続いた（max_ineffective_applies %d）。宣言を直すか現実を宣言どおりにすれば数え直す", m.Count, c.IneffectiveLimit)
				out.Halts = append(out.Halts, Halt{Scope: "resource " + c.Resource, Reason: c.Blocked})
			}
		}
		if c.Action == ActApply {
			needApproval(c, in, &out)
		}
	}
	checkBudgets(&out, in)

	for _, c := range out.Changes {
		out.Summary[c.Op]++
	}
	sort.SliceStable(out.Changes, func(i, j int) bool { return out.Changes[i].Resource < out.Changes[j].Resource })
	return out
}

// ChangeRequestID は変更 1 つの承認の依頼の ID。同じ宣言の同じ操作には同じ ID
// （宣言を直せば頼み直す）。
func ChangeRequestID(c Change) string {
	sum := sha256.Sum256([]byte(c.Resource + "\n" + string(c.Op) + "\n" + c.DesiredHash))
	return "chg-" + hex.EncodeToString(sum[:8])
}

// needApproval は当たる Policy の承認を確かめる（E-L2・N-L2・E-J10・N-G10・
// 不変条件 21）。承認が揃えば当て、拒否なら止め、揃わなければ承認待ち。依頼が
// 無い・使った・期限切れ・要件が変わったなら、周が新しく頼む（NewRequest）。
func needApproval(c *Change, in Input, out *Result) {
	var rules []record.Rule
	var applicants []string
	var timeout time.Duration
	for _, p := range in.Policies {
		if p.Approval == nil || !has(c.Policies, p.Name) {
			continue
		}
		rules = append(rules, approval.Rules("policy "+p.Name, p.Approval))
		if p.Applicant != "" && !has(applicants, p.Applicant) {
			applicants = append(applicants, p.Applicant)
		}
		if t := p.Approval.Timeout; t > 0 && (timeout == 0 || t < timeout) {
			timeout = t
		}
	}
	if len(rules) == 0 {
		return
	}
	sort.Strings(applicants)
	id := ChangeRequestID(*c)
	c.Request = id
	a, ok := in.Approvals[id]
	st := approval.Evaluate(a, ok, in.Now)
	if ok && !approval.SameRules(a.Request.Rules, rules) {
		st.State = approval.Missing // 要件が変わった：頼み直す
	}
	switch st.State {
	case approval.Approved:
		c.ApprovedBy = st.Approvers
		return
	case approval.Rejected:
		c.Action = ActHalt
		c.Blocked = fmt.Sprintf("承認の依頼 %s を %s が拒否した。宣言を直せば頼み直す", id, strings.Join(st.RejectedBy, "・"))
		out.Halts = append(out.Halts, Halt{Scope: "resource " + c.Resource, Reason: c.Blocked})
		return
	case approval.Pending:
		c.Action = ActPending
		c.Blocked = fmt.Sprintf("sakutei approve %s（%s）", id, st.Summary())
		return
	}
	// 無い・使った・期限切れ・要件が変わった。
	req := record.ApprovalRequest{ID: id, Kind: "change", Subject: fmt.Sprintf("%s %s", c.Op, c.Resource),
		Applicant: strings.Join(applicants, ","), Rules: rules, CreatedAt: in.Now, State: record.RequestPending}
	if timeout > 0 {
		exp := in.Now.Add(timeout)
		req.Expires = &exp
	}
	c.Action, c.NewRequest, c.Fresh = ActPending, &req, ok
	c.Blocked = fmt.Sprintf("sakutei approve %s（%s）", id, approvalNeed(rules))
}

func approvalNeed(rules []record.Rule) string {
	var parts []string
	for _, r := range rules {
		of := "申請者でない誰か"
		if len(r.Of) > 0 {
			of = strings.Join(r.Of, ", ")
		}
		parts = append(parts, fmt.Sprintf("%s 0/%d（%s）", r.From, r.N, of))
	}
	return strings.Join(parts, "・")
}

// DesiredHash は宣言（ensure と項目）のハッシュ。宣言外は空。
func DesiredHash(r core.Resource) string {
	data, _ := json.Marshal(struct {
		Ensure string      `json:"ensure"`
		Fields core.Fields `json:"fields"`
	}{r.Ensure.String(), r.Fields})
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// condEnv は Condition を評価する環境。今の周の観測だけを引く（E-J9）。
type condEnv struct {
	obs     map[core.ID]core.Observation
	g       *graph.Graph
	checks  map[string]checkResult
	desired core.Fields
}

type checkResult struct {
	t   core.Tri
	why string
}

func (e *condEnv) Observation(id core.ID) (core.Observation, bool) {
	o, ok := e.obs[id]
	return o, ok
}

func (e *condEnv) Select(s core.Selection) []core.ID { return e.g.Select(s) }

func (e *condEnv) Desired() core.Fields { return e.desired }

func (e *condEnv) Check(name string) core.Tri { return e.checks[name].t }

// runChecks は対象の when が使う確かめを 1 回ずつ呼ぶ（E-B9）。登録の無い名前は
// 不明。
func runChecks(ctx context.Context, subjects []core.Resource, funcs map[string]core.CheckFunc, obs map[core.ID]core.Observation) map[string]checkResult {
	out := map[string]checkResult{}
	view := &condEnv{obs: obs}
	for _, r := range subjects {
		for _, d := range r.DependsOn {
			if d.When == nil {
				continue
			}
			for _, name := range d.When.Checks() {
				if _, done := out[name]; done {
					continue
				}
				f, ok := funcs[name]
				if !ok {
					out[name] = checkResult{core.TriUnknown, "登録した確かめに無い"}
					continue
				}
				t, why := f(ctx, view)
				out[name] = checkResult{t, why}
			}
		}
	}
	return out
}

// ready は依存の待ちを判定する（§3.5）。when があれば when が真になるまで待ち、
// 不明なら on_unknown（wait・block・ignore）に従う（E-C4・N-C8・N-H6）。when が
// 無ければ、宣言した上流は宣言どおり（None）、宣言に無い上流は Present まで待つ。
// どちらでも、名指しの上流が Unknown・作業中なら待つ（不変条件 3）。止める
// （block）ものが 1 つでもあれば待つより強い。
func ready(r *core.Resource, ops map[core.ID]Op, obs map[core.ID]core.Observation, env *condEnv) (Action, string) {
	if r == nil {
		return ActApply, ""
	}
	env.desired = r.Fields
	act, why := ActApply, ""
	hold := func(a Action, msg string) {
		if act == ActApply || (a == ActHalt && act != ActHalt) {
			act, why = a, msg
		}
	}
	for _, d := range r.DependsOn {
		if d.When != nil {
			switch d.When.Eval(env) {
			case core.TriFalse:
				hold(ActWait, fmt.Sprintf("when %q が偽%s", d.When.String(), env.checkNotes(d.When)))
				continue
			case core.TriUnknown:
				switch d.OnUnknown {
				case core.OnUnknownBlock:
					hold(ActHalt, fmt.Sprintf("when %q が分からない（on_unknown: block）%s", d.When.String(), env.checkNotes(d.When)))
					continue
				case core.OnUnknownWait:
					hold(ActWait, fmt.Sprintf("when %q が分からない%s", d.When.String(), env.checkNotes(d.When)))
					continue
				}
			}
			if d.HasOn() {
				if a := obs[d.On].Availability; a == core.Unknown || a == core.Converging {
					hold(ActWait, fmt.Sprintf("上流 %s が %s", d.On, a))
				}
			}
			continue
		}
		if !d.HasOn() {
			continue
		}
		if op, declared := ops[d.On]; declared {
			if op != None {
				hold(ActWait, fmt.Sprintf("上流 %s が %s", d.On, opLabel[op]))
			}
			continue
		}
		switch o := obs[d.On]; o.Availability {
		case core.Present:
		case core.Absent:
			hold(ActWait, fmt.Sprintf("上流 %s が無い", d.On))
		default:
			hold(ActWait, fmt.Sprintf("上流 %s が %s", d.On, o.Availability))
		}
	}
	return act, why
}

func (e *condEnv) checkNotes(c core.Condition) string {
	var parts []string
	for _, name := range c.Checks() {
		if r := e.checks[name]; r.why != "" {
			parts = append(parts, fmt.Sprintf("check(%s): %s", name, r.why))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "（" + strings.Join(parts, "・") + "）"
}

// dependentsLeft は id に依る宣言のうち、まだ無くなっていないものを返す。観測が
// Absent でなければ（Unknown・作業中を含む）残っているとみる（不変条件 3）。
func dependentsLeft(id core.ID, g *graph.Graph, obs map[core.ID]core.Observation) string {
	var left []string
	for _, d := range g.Dependents(id) {
		if obs[d].Availability != core.Absent {
			left = append(left, d.String())
		}
	}
	if len(left) == 0 {
		return ""
	}
	sort.Strings(left)
	return "下流 " + strings.Join(left, ", ") + " が残る（下流から消す）"
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
		// 前の波で当てた分と合わせて数える（同じ周の観測し直し、E-J7）。
		if used, ok := in.Used[p.Name]; ok {
			u.Changes, u.Destructive = used.Changes, used.Destructive
		}
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
	// 済んでいない手順（待つ・承認待ち・常駐が要る、§3.7）。
	for _, s := range r.Runs {
		if s.Active() {
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
