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

package approval_test

import (
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/approval"
	"github.com/tjst-t/sakutei/v3/record"
)

var t0 = time.Date(2026, 11, 5, 22, 0, 0, 0, time.UTC)

func req(applicant string, rules ...record.Rule) record.ApprovalRequest {
	exp := t0.Add(time.Hour)
	return record.ApprovalRequest{ID: "x", Applicant: applicant, Rules: rules, CreatedAt: t0, Expires: &exp, State: record.RequestPending}
}

func dec(by, d string, at time.Duration) record.Decision {
	return record.Decision{By: by, Decision: d, At: t0.Add(at)}
}

// 不変条件 21 の単体：申請者≠承認者・要件の of・異なる人・拒否・期限。
func TestEvaluate(t *testing.T) {
	two := record.Rule{From: "policy p", N: 2, Of: []string{"alice", "bob", "carol"}}
	cases := []struct {
		name      string
		a         record.Approval
		now       time.Duration
		state     string
		approvers string
	}{
		{"足りない", record.Approval{Request: req("carol", two), Decisions: []record.Decision{dec("alice", record.Approve, 0)}}, 0, approval.Pending, "alice"},
		{"申請者は数えない", record.Approval{Request: req("carol", two), Decisions: []record.Decision{
			dec("alice", record.Approve, 0), dec("carol", record.Approve, 0)}}, 0, approval.Pending, "alice"},
		{"2 人", record.Approval{Request: req("carol", two), Decisions: []record.Decision{
			dec("alice", record.Approve, 0), dec("bob", record.Approve, 0)}}, 0, approval.Approved, "alice,bob"},
		{"of に無い", record.Approval{Request: req("", two), Decisions: []record.Decision{
			dec("alice", record.Approve, 0), dec("mallory", record.Approve, 0)}}, 0, approval.Pending, "alice"},
		{"拒否が勝つ", record.Approval{Request: req("", two), Decisions: []record.Decision{
			dec("alice", record.Approve, 0), dec("bob", record.Approve, 0), dec("carol", record.Reject, 0)}}, 0, approval.Rejected, "alice,bob"},
		{"申請者の拒否は数えない", record.Approval{Request: req("carol", two), Decisions: []record.Decision{
			dec("alice", record.Approve, 0), dec("bob", record.Approve, 0), dec("carol", record.Reject, 0)}}, 0, approval.Approved, "alice,bob"},
		{"期限の後の判断", record.Approval{Request: req("", two), Decisions: []record.Decision{
			dec("alice", record.Approve, 0), dec("bob", record.Approve, 2*time.Hour)}}, 2 * time.Hour, approval.Expired, "alice"},
		{"期限の前に揃えば期限の後も承認", record.Approval{Request: req("", two), Decisions: []record.Decision{
			dec("alice", record.Approve, 0), dec("bob", record.Approve, 0)}}, 2 * time.Hour, approval.Approved, "alice,bob"},
		{"申請者が分からない separation", record.Approval{Request: req("", record.Rule{From: "gate", N: 1, Separation: true}),
			Decisions: []record.Decision{dec("alice", record.Approve, 0)}}, 0, approval.Pending, ""},
		{"of が空なら申請者でない誰でも", record.Approval{Request: req("carol", record.Rule{From: "resume", N: 1}),
			Decisions: []record.Decision{dec("carol", record.Approve, 0), dec("dave", record.Approve, 0)}}, 0, approval.Approved, "dave"},
		{"全部の要件", record.Approval{Request: req("", two, record.Rule{From: "policy q", N: 1, Of: []string{"dave"}}),
			Decisions: []record.Decision{dec("alice", record.Approve, 0), dec("bob", record.Approve, 0)}}, 0, approval.Pending, "alice,bob"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := approval.Evaluate(tc.a, true, t0.Add(tc.now))
			if st.State != tc.state || strings.Join(st.Approvers, ",") != tc.approvers {
				t.Fatalf("%+v", st)
			}
		})
	}
	if st := approval.Evaluate(record.Approval{}, false, t0); st.State != approval.Missing {
		t.Fatal(st)
	}
	used := record.Approval{Request: req("", two), Decisions: []record.Decision{dec("alice", record.Approve, 0), dec("bob", record.Approve, 0)}}
	used.Request.State = record.RequestUsed
	if st := approval.Evaluate(used, true, t0); st.State != approval.Used {
		t.Fatalf("使った依頼を承認と数えた: %+v", st)
	}
}
