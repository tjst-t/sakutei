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

package cycle

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/approval"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/procedure"
	"github.com/tjst-t/sakutei/v3/record"
)

// state は cycle の初めに Record から読む、cycle をまたいだ人の判断と Procedure の進み（§5.1）。
type state struct {
	policies  []core.Policy // intent・一時的な Policy・Procedure の step の policy
	temporary []plan.Temporary
	approvals map[string]record.Approval
	runs      []record.Run
	// reportOnly は読めないものがあって全体を report に寄せる理由（不変条件 22）。
	reportOnly string
	errs       []string
}

// stateExec はロックを持つ cycle が読むときに要るもの。期限切れ・終えた一時的な
// Policy を消し、新しい判断・期限切れ・拒否を監査に残す。
type stateExec struct {
	rec   *record.Record
	audit *record.Audit
}

// readState は一時的な Policy・承認・Procedure の進みを読む。exec が nil なら
// 読むだけ（plan）。読めなければ止める側に寄せる（不変条件 22）。
func readState(dir string, now time.Time, intentPolicies []core.Policy, exec *stateExec) state {
	st := state{policies: append([]core.Policy(nil), intentPolicies...), approvals: map[string]record.Approval{}}
	if dir == "" {
		return st
	}
	fail := func(what string, err error) {
		st.errs = append(st.errs, what+"が読めない: "+err.Error())
		if st.reportOnly == "" {
			st.reportOnly = what + "が読めない"
		}
	}
	auditE := func(e record.Entry) {
		e.At = now
		if _, err := exec.audit.Append(e); err != nil {
			st.errs = append(st.errs, "監査に書けない: "+err.Error())
		}
	}

	// 一時的な Policy（freeze）。期限・終えたものは監査に残して消す（E-J4・N-Q1）。
	temps, err := record.ReadTempPolicies(dir)
	if err != nil {
		fail("一時的な Policy（temp-policies/）", err)
	}
	for _, t := range temps {
		if exec != nil && !t.Audited {
			auditE(record.Entry{Kind: "freeze.start", Policies: []string{t.Name}, By: t.By, Reason: t.Reason,
				Error: fmt.Sprintf("select %s・until %s", t.Select.String(), t.Until.Format(time.RFC3339))})
			if err := exec.rec.MarkTempPolicyAudited(t); err != nil {
				st.errs = append(st.errs, "一時的な Policy に印を書けない: "+err.Error())
			}
		}
		ended := t.Ended != nil
		expired := !now.Before(t.Until)
		if ended || expired {
			if exec != nil {
				e := record.Entry{Kind: "freeze.expire", Policies: []string{t.Name}, By: t.By, Reason: t.Reason}
				if ended {
					e.Kind, e.By, e.Reason = "freeze.end", t.Ended.By, t.Ended.Reason
				}
				auditE(e)
				if err := exec.rec.RemoveTempPolicy(t.Name); err != nil {
					st.errs = append(st.errs, "一時的な Policy を消せない: "+err.Error())
				}
			}
			continue
		}
		st.policies = append(st.policies, t.Policy())
		st.temporary = append(st.temporary, plan.Temporary{Name: t.Name, Select: t.Select.String(), Reason: t.Reason,
			By: t.By, Until: t.Until, Source: "freeze"})
	}

	// Procedure の進み。
	runs, err := record.ReadRuns(dir)
	if err != nil {
		fail("Procedure の進み（runs/）", err)
	}
	st.runs = runs
	st.policies = append(st.policies, procedure.Policies(runs)...)

	// 承認。新しい判断・拒否・期限切れを監査に残す（E-L2・E-L6・N-L3・不変条件 21）。
	approvals, err := record.ReadApprovals(dir)
	if err != nil {
		fail("承認（approvals/）", err)
		return st
	}
	ids := make([]string, 0, len(approvals))
	for id := range approvals {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		a := approvals[id]
		if exec != nil && a.Request.State == record.RequestPending {
			a = settleApproval(a, now, auditE, exec.rec, &st.errs)
			approvals[id] = a
		}
	}
	st.approvals = approvals
	return st
}

// settleApproval は依頼の新しい判断を監査に残し、拒否・期限切れを確定させる。書けなければ
// その依頼を使わない（不変条件 21）。
func settleApproval(a record.Approval, now time.Time, auditE func(record.Entry), rec *record.Record, errs *[]string) record.Approval {
	req := a.Request
	changed := false
	ev := approval.Evaluate(a, true, now)
	ignored := map[string]string{}
	for _, s := range ev.Ignored {
		name, why, _ := strings.Cut(s, ": ")
		ignored[name] = why
	}
	for _, d := range a.Decisions {
		if req.Audited[d.By] == d.Decision {
			continue
		}
		e := record.Entry{Kind: "approval." + d.Decision, Request: req.ID, Resource: req.Subject, By: d.By, Reason: d.Comment, Result: "ok"}
		if why, ok := ignored[d.By]; ok {
			e.Result, e.Error = "ignored", why
		}
		auditE(e)
		if req.Audited == nil {
			req.Audited = map[string]string{}
		}
		req.Audited[d.By] = d.Decision
		changed = true
	}
	switch ev.State {
	case approval.Rejected:
		req.State, changed = record.RequestRejected, true
		auditE(record.Entry{Kind: "approval.rejected", Request: req.ID, Resource: req.Subject, Error: strings.Join(ev.RejectedBy, ","), Result: "rejected"})
	case approval.Expired:
		req.State, changed = record.RequestExpired, true
		auditE(record.Entry{Kind: "approval.expired", Request: req.ID, Resource: req.Subject, ApprovedBy: ev.Approvers, Result: "expired"})
	}
	if !changed {
		return a
	}
	if err := rec.WriteRequest(req, false); err != nil {
		*errs = append(*errs, "承認の依頼を書けない: "+err.Error())
		// 書けなければ使わない：使った依頼として扱う（不変条件 21）。
		req.State = record.RequestUsed
	}
	a.Request = req
	return a
}

// useApproval は承認を使った印を送る前に書き、監査に残す（不変条件 21）。
func useApproval(rec *record.Record, audit *record.Audit, a record.Approval, c *plan.Change, now time.Time) error {
	req := a.Request
	req.State = record.RequestUsed
	if err := rec.WriteRequest(req, false); err != nil {
		return err
	}
	_, err := audit.Append(record.Entry{At: now, Kind: "approval.used", Request: c.Request, Resource: c.Resource,
		ApprovedBy: c.ApprovedBy, Policies: c.Policies, Result: "ok"})
	return err
}

// reserve は Procedure の書き込む step 1 つを、該当する Policy の budget に cycle の
// 変更と合わせて数える。超えれば cycle 全体を halt する（不変条件 20）。破壊的な
// 操作は window の使用量を先に書く。
func reserve(r *plan.Result, policies []core.Policy, names []string, destructive bool, used map[string]plan.BudgetUse,
	fired map[string]int, marks record.Marks, base map[string]record.WindowUse, now time.Time, rec *record.Record) string {
	for _, h := range r.Halts {
		if strings.HasPrefix(h.Scope, "policy ") {
			return "budget を超えたので cycle 全体を halt した"
		}
	}
	var over []string
	for _, p := range policies {
		b := p.Budget
		if b == nil || !contains(names, p.Name) {
			continue
		}
		u := used[p.Name]
		var mine []string
		if b.PerCycle != core.Unlimited && u.Changes+1 > b.PerCycle {
			mine = append(mine, fmt.Sprintf("per_cycle %d", b.PerCycle))
		}
		if destructive && b.DestructivePerCycle != core.Unlimited && u.Destructive+1 > b.DestructivePerCycle {
			mine = append(mine, fmt.Sprintf("destructive_per_cycle %d", b.DestructivePerCycle))
		}
		if destructive && b.DestructivePerWindow != core.Unlimited {
			w := marks.Window(p.Name, b.Window, now)
			if w.Destructive+fired[p.Name]+1 > b.DestructivePerWindow {
				mine = append(mine, fmt.Sprintf("destructive_per_window %d", b.DestructivePerWindow))
			}
		}
		if len(mine) > 0 {
			r.Halts = append(r.Halts, plan.Halt{Scope: "policy " + p.Name, Reason: "budget: Procedure の書き込む step で " + strings.Join(mine, "・") + " を超える"})
			over = append(over, "policy "+p.Name+" の "+strings.Join(mine, "・"))
		}
	}
	if len(over) > 0 {
		return "budget: " + strings.Join(over, "・") + " を超える"
	}
	for _, p := range policies {
		if p.Budget == nil || !contains(names, p.Name) {
			continue
		}
		u := used[p.Name]
		u.Changes++
		if destructive {
			u.Destructive++
			fired[p.Name]++
		}
		used[p.Name] = u
	}
	if destructive {
		pre := marks
		pre.BudgetWindows = windowsWith(base, fired, *r, nil)
		if err := rec.WriteMarks(pre); err != nil {
			return "Record に書けないので送らない"
		}
	}
	return ""
}
