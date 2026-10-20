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

// workflow_nofm_test.go: 承認ゲートの N-of-M・役割制約・申請者≠承認者（#43、§14.4 / §6.4）。
//
// workflow_test.go のローリング（rollingIntent）のゲート s1 に requirements
// （quorum 2-of-3・roles [platform-admin]・separation applicant_ne_approver）を
// 足し、申請者を alice にする。
//
//	AC-v1.5-nofm-1 — 2-of-3 のゲートは 1 人の承認では待ちに留まり、2 人目で通る
//	AC-v1.5-nofm-2 — 同じ承認者の 2 回の承認は 1 と数え、通らない
//	AC-v1.5-nofm-3 — 申請者本人の承認は拒否される（403、数えない）
//	AC-v1.5-nofm-4 — 役割の合わない承認は数えない（403）
//	AC-v1.5-nofm-5 — 全承認者（承認者 ID・役割・署名）が S4 に残る
//	AC-v1.5-nofm-6 — 誤った requirements（n > of・未知のキー・applicant の無い separation）はロード時に拒否される
package acceptance

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/internal/audit"
	"github.com/tjst-t/sakutei/internal/loader"
	"github.com/tjst-t/sakutei/internal/workflow"
)

const nofmGate = `"gate": { "manual_approval": { "requirements": {
          "quorum": { "n": 2, "of": 3 },
          "roles": ["platform-admin"],
          "separation": "applicant_ne_approver" } }, "timeout": "1h" } },
        { "name": "s2"`

func nofmIntent(t *testing.T) string {
	t.Helper()
	from := `"gate": { "manual_approval": "required", "timeout": "1h" } },
        { "name": "s2"`
	if !strings.Contains(rollingIntent, from) {
		t.Fatal("fixture does not contain the s1 gate")
	}
	s := strings.Replace(rollingIntent, from, nofmGate, 1)
	return strings.Replace(s, `"workflow": "rolling",`, `"workflow": "rolling", "applicant": "alice",`, 1)
}

// [AC-v1.5-nofm-1] [AC-v1.5-nofm-2] [AC-v1.5-nofm-3] [AC-v1.5-nofm-4] [AC-v1.5-nofm-5]
func TestWorkflowGateNofM(t *testing.T) {
	r := newWFRig(t, nofmIntent(t))
	res := r.runToFirstGate()
	if !hasEvent(res, workflow.EventAwaiting, "2 approval(s); roles platform-admin, separation applicant_ne_approver") {
		t.Fatalf("awaiting event does not show the requirements: %v", res.WorkflowEvents)
	}
	run, _ := r.run()
	if run.Applicant != "alice" || run.GateRequirements.Needed() != 2 {
		t.Fatalf("run = %+v, want applicant alice and the gate's requirements in S2", run)
	}
	srv := r.api()
	url := srv.URL + "/workflows/rolling/gates/s1/approve"

	// [AC-v1.5-nofm-3] 申請者本人の承認は、役割が合っていても拒否する。
	if code, body := post(t, url, `{"approver":"alice","roles":["platform-admin"],"signature":"sig-a"}`); code != http.StatusForbidden ||
		!strings.Contains(body, "applicant") {
		t.Fatalf("self-approval by the applicant: %d %s, want 403", code, body)
	}
	// [AC-v1.5-nofm-4] 役割の合わない承認・役割を名乗らない承認は数えない。
	if code, body := post(t, url, `{"approver":"dave","roles":["developer"],"signature":"sig-d"}`); code != http.StatusForbidden {
		t.Fatalf("approval without the required role: %d %s, want 403", code, body)
	}
	if code, body := post(t, url, `{"approver":"erin","signature":"sig-e"}`); code != http.StatusForbidden {
		t.Fatalf("approval without any role: %d %s, want 403", code, body)
	}
	if run, _ := r.run(); len(run.Approvals) != 0 {
		t.Fatalf("refused approvals reached S2: %+v", run.Approvals)
	}

	// [AC-v1.5-nofm-1] 1 人目（bob）では待ちに留まる。
	if code, body := post(t, url, `{"approver":"bob","roles":["platform-admin"],"signature":"sig-b1"}`); code != http.StatusOK {
		t.Fatalf("approve by bob: %d %s", code, body)
	}
	if res := r.cycle(); !hasEvent(res, workflow.EventWaiting, "1 of 2 approval(s)") {
		t.Fatalf("after one approval: %v", res.WorkflowEvents)
	}
	// 再起動しても 1 of 2 のまま待ちに留まる（承認の数え上げは S2 にある）。
	r.d = r.newDaemon(true)
	if res := r.cycle(); !hasEvent(res, workflow.EventWaiting, "1 of 2 approval(s)") {
		t.Fatalf("after restart: %v", res.WorkflowEvents)
	}

	// [AC-v1.5-nofm-2] 同じ bob の 2 回目は受け付けるが 1 と数える。
	if code, body := post(t, url, `{"approver":"bob","roles":["platform-admin"],"signature":"sig-b2"}`); code != http.StatusOK {
		t.Fatalf("second approve by bob: %d %s", code, body)
	}
	for i := 0; i < 2; i++ {
		if res := r.cycle(); !hasEvent(res, workflow.EventWaiting, "1 of 2 approval(s)") {
			t.Fatalf("after bob approved twice: %v", res.WorkflowEvents)
		}
	}
	if run, _ := r.run(); run.Phase != workflow.PhaseAwaitingApproval || run.Gate != "s1" {
		t.Fatalf("run = %+v, want still awaiting s1", run)
	}
	if r.field("n2", "size") != "1" || r.field("n2", "active") != "true" {
		t.Fatal("n2 was touched before the gate s1 reached its quorum")
	}

	// [AC-v1.5-nofm-1] 2 人目（carol、役割は複数名乗ってよい）で通る。
	if code, body := post(t, url, `{"approver":"carol","roles":["auditor","platform-admin"],"signature":"sig-c"}`); code != http.StatusOK {
		t.Fatalf("approve by carol: %d %s", code, body)
	}
	res = r.cycle()
	if !hasEvent(res, workflow.EventApproved, "gate s1 approved by bob, carol (2 of 2 required)") {
		t.Fatalf("expected GateApproved by bob and carol, got %v", res.WorkflowEvents)
	}
	r.cycleUntil(10, awaiting("s2"))
	if r.field("n2", "size") != "2" {
		t.Fatal("n2 was not processed after the gate s1 passed")
	}
	// 通ったゲートはもう承認できない。
	if code, _ := post(t, url, `{"approver":"frank","roles":["platform-admin"],"signature":"sig-f"}`); code != http.StatusConflict {
		t.Fatalf("approve after the quorum: %d, want 409", code)
	}
	// requirements の無い s2 は単一承認で、申請者本人でも通る（separation はゲート単位）。
	if code, body := post(t, srv.URL+"/workflows/rolling/gates/s2/approve", `{"approver":"alice","signature":"sig-a2"}`); code != http.StatusOK {
		t.Fatalf("approve s2: %d %s", code, body)
	}
	r.cycleUntil(10, func(run workflow.Run) bool { return run.Phase == workflow.PhaseDone })

	// [AC-v1.5-nofm-5] 全承認者が S4 に残る（拒否した承認は残らない）。
	recs := r.audit()
	type rec struct{ actor, sig, roles string }
	var got []rec
	for _, x := range recs {
		if x.Kind != audit.KindGateApproved || x.Workflow != "rolling" || x.RunID != run.RunID {
			t.Fatalf("unexpected audit record %+v", x)
		}
		got = append(got, rec{x.Actor, x.Signature, strings.Join(x.Roles, ",")})
	}
	want := []rec{
		{"bob", "sig-b1", "platform-admin"},
		{"bob", "sig-b2", "platform-admin"},
		{"carol", "sig-c", "auditor,platform-admin"},
		{"alice", "sig-a2", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("audit = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("audit[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if !strings.Contains(recs[1].Detail, "1 of 2 required: bob") || !strings.Contains(recs[1].Detail, "still awaiting") {
		t.Fatalf("detail of bob's second approval = %q", recs[1].Detail)
	}
	if !strings.Contains(recs[2].Detail, "2 of 2 required: bob, carol") || !strings.Contains(recs[2].Detail, "proceeding") {
		t.Fatalf("detail of carol's approval = %q", recs[2].Detail)
	}
}

// [AC-v1.5-nofm-3] CLI（sakutei approve --role）も同じ判定。
func TestWorkflowGateNofMCLI(t *testing.T) {
	r := newWFRig(t, nofmIntent(t))
	r.runToFirstGate()
	approve := func(approver string, roles ...string) (string, int) {
		args := []string{"approve", "--store", r.storePath, "--audit-log", r.auditPath, "--approver", approver, "--signature", "sig-" + approver}
		for _, role := range roles {
			args = append(args, "--role", role)
		}
		return runSakutei(t, append(args, "rolling", "s1")...)
	}
	if out, code := approve("alice", "platform-admin"); code == 0 || !strings.Contains(out, "applicant") {
		t.Fatalf("self-approval via CLI: exit %d\n%s", code, out)
	}
	if out, code := approve("dave", "developer"); code == 0 || !strings.Contains(out, "required roles") {
		t.Fatalf("approval without the role via CLI: exit %d\n%s", code, out)
	}
	if out, code := approve("bob", "platform-admin"); code != 0 || !strings.Contains(out, "1 of 2 approval(s), still awaiting") {
		t.Fatalf("approve by bob: exit %d\n%s", code, out)
	}
	if out, code := approve("carol", "auditor,platform-admin"); code != 0 || !strings.Contains(out, "proceeds on its next cycle") {
		t.Fatalf("approve by carol: exit %d\n%s", code, out)
	}
	if res := r.cycle(); !hasEvent(res, workflow.EventApproved, "bob, carol") {
		t.Fatalf("expected GateApproved, got %v", res.WorkflowEvents)
	}
}

// [AC-v1.5-nofm-6]
func TestWorkflowGateRequirementsLoad(t *testing.T) {
	base := nofmIntent(t)
	cases := map[string]struct{ from, to string }{
		"n>of":         {`"n": 2, "of": 3`, `"n": 4, "of": 3`},
		"n=0":          {`"n": 2, "of": 3`, `"n": 0, "of": 3`},
		"unknown":      {`"roles": ["platform-admin"],`, `"role": ["platform-admin"],`},
		"separation":   {`"applicant_ne_approver"`, `"four_eyes"`},
		"no-applicant": {`"applicant": "alice",`, ``},
		"empty-roles":  {`"roles": ["platform-admin"],`, `"roles": [],`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(base, c.from) {
				t.Fatalf("fixture does not contain %q", c.from)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "intent.json"), []byte(strings.Replace(base, c.from, c.to, 1)), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := loader.LoadIntent(dir); err == nil {
				t.Fatalf("intent with bad requirements (%s) was accepted", name)
			}
		})
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "intent.json"), []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	in, err := loader.LoadIntent(dir)
	if err != nil {
		t.Fatalf("valid intent rejected: %v", err)
	}
	def := in.Workflows[0]
	req := def.Stages[0].Gate.Requirements
	if def.Applicant != "alice" || req == nil || req.Quorum == nil || req.Quorum.N != 2 || req.Quorum.Of != 3 ||
		len(req.Roles) != 1 || req.Roles[0] != "platform-admin" || req.Separation != workflow.SeparationApplicantNeApprover ||
		def.Stages[1].Gate.Requirements != nil {
		t.Fatalf("loaded workflow = %+v (requirements %+v)", def, req)
	}
}
