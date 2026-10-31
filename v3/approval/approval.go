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

// Package approval は承認を数える（設計 §3.6・§3.7・§5.4、不変条件 21）。承認者が
// 条件（quorum・申請者≠承認者）を満たすかは周の中で数え、CLI と承認ツールは
// 判断のファイルを書くだけ。Policy の承認と手順の gate は同じ数え方をする
// （E-L2・N-L2・E-A5・E-J8）。
package approval

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/record"
)

// 依頼の数えた結果。
const (
	Missing  = "missing"  // 依頼がまだ無い
	Pending  = "pending"  // 足りない
	Approved = "approved" // 全部の要件を満たした
	Rejected = "rejected" // 数える人が拒否した
	Expired  = "expired"  // 満たす前に期限が切れた
	Used     = "used"     // 使った（同じ依頼を二度使わない）
)

// Status は依頼 1 つを数えた結果。
type Status struct {
	State      string
	Approvers  []string // 数えた承認者（全部の要件のどれかに数えたもの）
	RejectedBy []string
	// Ignored は数えなかった判断と理由（申請者・要件の of に無い・期限の後）。
	Ignored []string
	// Progress は要件ごとの数（"policy a 1/2"）。
	Progress []string
}

// Rules は承認の要件を依頼の形にする。
func Rules(from string, a *core.Approval) record.Rule {
	return record.Rule{From: from, N: a.N, Of: append([]string(nil), a.Of...), Separation: a.Separation}
}

// SameRules は依頼の要件が今の宣言の要件と同じか（宣言が変われば頼み直す）。
func SameRules(a, b []record.Rule) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// Evaluate は依頼の判断を数える（不変条件 21）。申請者は数えない。要件の of に
// 無い人・期限の後の判断・同じ人の 2 つ目は数えない。数える人が 1 人でも拒否
// すれば拒否。申請者が分からない依頼は、separation の要件を満たせない。
func Evaluate(a record.Approval, ok bool, now time.Time) Status {
	if !ok {
		return Status{State: Missing}
	}
	req := a.Request
	st := Status{}
	switch req.State {
	case record.RequestUsed:
		st.State = Used
		return st
	case record.RequestRejected:
		st.State = Rejected
	case record.RequestExpired:
		st.State = Expired
	}
	applicant := map[string]bool{}
	for _, x := range strings.Split(req.Applicant, ",") {
		if x = strings.TrimSpace(x); x != "" {
			applicant[x] = true
		}
	}
	counted := map[string]bool{}
	rejected := map[string]bool{}
	approved := true
	for _, rule := range req.Rules {
		n := 0
		for _, d := range a.Decisions {
			why := ""
			switch {
			case applicant[d.By]:
				why = "申請者は承認者に数えない"
			case rule.Separation && len(applicant) == 0:
				why = "申請者が分からない（separation）"
			case len(rule.Of) > 0 && !contains(rule.Of, d.By):
				why = fmt.Sprintf("%s の承認者（%s）に無い", rule.From, strings.Join(rule.Of, ", "))
			case req.Expires != nil && !d.At.Before(*req.Expires):
				why = "期限の後の判断"
			case d.Decision != record.Approve && d.Decision != record.Reject:
				why = fmt.Sprintf("知らない判断 %q", d.Decision)
			}
			if why != "" {
				st.Ignored = appendOnce(st.Ignored, d.By+": "+why)
				continue
			}
			if d.Decision == record.Reject {
				rejected[d.By] = true
				continue
			}
			counted[d.By] = true
			n++
		}
		st.Progress = append(st.Progress, fmt.Sprintf("%s %d/%d", rule.From, min(n, rule.N), rule.N))
		if n < rule.N {
			approved = false
		}
	}
	// 数える人が 1 人でも拒否したら、ほかに数えたものに関わらず拒否（E-L2）。
	for name := range rejected {
		st.RejectedBy = append(st.RejectedBy, name)
		delete(counted, name)
	}
	for name := range counted {
		st.Approvers = append(st.Approvers, name)
	}
	sort.Strings(st.RejectedBy)
	sort.Strings(st.Approvers)
	// 確定した状態（拒否・期限切れ）は戻さない。
	if st.State != "" {
		return st
	}
	switch {
	case len(st.RejectedBy) > 0:
		st.State = Rejected
	case approved && len(req.Rules) > 0:
		st.State = Approved
	case req.Expires != nil && !now.Before(*req.Expires):
		st.State = Expired
	default:
		st.State = Pending
	}
	return st
}

// Summary は承認待ちの 1 行。
func (s Status) Summary() string {
	parts := []string{strings.Join(s.Progress, "・")}
	if len(s.Ignored) > 0 {
		parts = append(parts, "数えない: "+strings.Join(s.Ignored, "・"))
	}
	return strings.Join(parts, "。")
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func appendOnce(list []string, s string) []string {
	if contains(list, s) {
		return list
	}
	return append(list, s)
}

// DecideError は判断を書く前に断った理由（CLI と承認ツール、§5.4）。
type DecideError struct {
	Code    string // DecideNotFound・DecideClosed・DecideForbidden・DecideInvalid
	Message string
}

func (e *DecideError) Error() string { return e.Message }

// DecideError の Code。承認ツールは HTTP の状態に写す。
const (
	DecideInvalid   = "invalid"   // 名前・理由の書き方の誤り
	DecideNotFound  = "not_found" // 依頼が無い
	DecideClosed    = "closed"    // 依頼が待っていない（使った・拒否・期限切れ）
	DecideForbidden = "forbidden" // 申請者・要件に無い人（不変条件 21）
)

// Decide は依頼 id への判断 d を確かめてから Record に書く（CLI の approve・reject と
// 承認ツール sakutei-approve で同じ、§5.4・不変条件 21、E-M4・N-M4）。申請者と
// 要件に無い人の判断・待っていない依頼への判断は書く前に断る。数えて効かせる
// のは周。書いたあとの数え方を返す。
func Decide(dir, id string, d record.Decision) (Status, error) {
	refuse := func(code, format string, a ...any) (Status, error) {
		return Status{}, &DecideError{Code: code, Message: fmt.Sprintf(format, a...)}
	}
	if !record.ValidName(id) {
		return refuse(DecideInvalid, "依頼の ID %q に使えない文字がある", id)
	}
	if d.By == "" || !record.ValidName(d.By) {
		return refuse(DecideInvalid, "承認者の名前を書く（英数字と . _ @ -）")
	}
	switch d.Decision {
	case record.Approve:
	case record.Reject:
		if strings.TrimSpace(d.Comment) == "" {
			return refuse(DecideInvalid, "拒否の理由を書く")
		}
	default:
		return refuse(DecideInvalid, "知らない判断 %q（%s・%s）", d.Decision, record.Approve, record.Reject)
	}
	a, found, err := record.ReadApproval(dir, id)
	if err != nil {
		return Status{}, err
	}
	if !found {
		return refuse(DecideNotFound, "依頼 %s が無い", id)
	}
	req := a.Request
	if st := Evaluate(a, true, d.At); st.State != Pending && st.State != Approved {
		return refuse(DecideClosed, "依頼 %s は %s（判断を受け付けない）", id, st.State)
	}
	for _, x := range strings.Split(req.Applicant, ",") {
		if strings.TrimSpace(x) == d.By {
			return refuse(DecideForbidden, "%s は依頼 %s の申請者なので承認者になれない（申請者≠承認者）", d.By, id)
		}
	}
	listed := false
	for _, r := range req.Rules {
		listed = listed || len(r.Of) == 0 || contains(r.Of, d.By)
	}
	if !listed {
		return refuse(DecideForbidden, "%s は依頼 %s の承認者（%s）に無い", d.By, id, RulesText(req.Rules))
	}
	if err := record.WriteDecision(dir, id, d); err != nil {
		return Status{}, err
	}
	a.Decisions = append(a.Decisions, d)
	return Evaluate(a, true, d.At), nil
}

// RulesText は依頼の要件の 1 行（「policy: 2 人（alice, bob）」）。
func RulesText(rules []record.Rule) string {
	var parts []string
	for _, r := range rules {
		of := "申請者でない誰か"
		if len(r.Of) > 0 {
			of = strings.Join(r.Of, ", ")
		}
		parts = append(parts, fmt.Sprintf("%s: %d 人（%s）", r.From, r.N, of))
	}
	return strings.Join(parts, "・")
}
