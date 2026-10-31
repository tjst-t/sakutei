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
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/approval"
	"github.com/tjst-t/sakutei/v3/record"
)

// 不変条件 21：申請者・要件に無い人の判断は書く前に断る。待っていない依頼も断る。
func TestDecide(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "record")
	rec, err := record.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 5, 22, 0, 0, 0, time.UTC)
	exp := now.Add(time.Hour)
	if err := rec.WriteRequest(record.ApprovalRequest{ID: "chg-1", Kind: "change", Subject: "fake/thing/a", Applicant: "carol",
		Rules: []record.Rule{{From: "platform", N: 1, Of: []string{"alice", "carol"}}}, CreatedAt: now, Expires: &exp, State: record.RequestPending}, false); err != nil {
		t.Fatal(err)
	}
	code := func(err error) string {
		var de *approval.DecideError
		if errors.As(err, &de) {
			return de.Code
		}
		return ""
	}
	for _, c := range []struct {
		name string
		id   string
		d    record.Decision
		want string
	}{
		{"申請者", "chg-1", record.Decision{By: "carol", Decision: record.Approve, At: now}, approval.DecideForbidden},
		{"要件に無い人", "chg-1", record.Decision{By: "mallory", Decision: record.Approve, At: now}, approval.DecideForbidden},
		{"理由の無い拒否", "chg-1", record.Decision{By: "alice", Decision: record.Reject, At: now}, approval.DecideInvalid},
		{"無い依頼", "chg-2", record.Decision{By: "alice", Decision: record.Approve, At: now}, approval.DecideNotFound},
		{"期限の後", "chg-1", record.Decision{By: "alice", Decision: record.Approve, At: exp}, approval.DecideClosed},
		{"名前の誤り", "chg-1", record.Decision{By: "../x", Decision: record.Approve, At: now}, approval.DecideInvalid},
	} {
		if _, err := approval.Decide(dir, c.id, c.d); code(err) != c.want {
			t.Errorf("%s: %v（%s のはず）", c.name, err, c.want)
		}
	}
	if files, _ := os.ReadDir(filepath.Join(dir, "approvals", "chg-1")); len(files) != 1 {
		t.Fatalf("断った判断を書いた: %d ファイル", len(files))
	}
	st, err := approval.Decide(dir, "chg-1", record.Decision{By: "alice", Decision: record.Approve, At: now})
	if err != nil || st.State != approval.Approved || len(st.Approvers) != 1 || st.Approvers[0] != "alice" {
		t.Fatalf("%+v %v", st, err)
	}
}
