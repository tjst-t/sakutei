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

package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/sdk/fake"
)

type fixedClock struct{ t time.Time }

func (c *fixedClock) Now() time.Time { return c.t }

// human は Stage 5 の CLI を end-to-end で動かす環境（Procedure の操作を持つ偽の provider）。
type human struct {
	t                           *testing.T
	intent, config, state, recd string
	clock                       *fixedClock
}

func newHuman(t *testing.T, intentYAML string) *human {
	t.Helper()
	root := t.TempDir()
	h := &human{t: t, intent: filepath.Join(root, "intent"), config: filepath.Join(root, "config.yaml"),
		state: filepath.Join(root, "state.json"), recd: filepath.Join(root, "record"),
		clock: &fixedClock{t: time.Date(2026, 11, 5, 13, 0, 0, 0, time.UTC)}}
	write(t, h.config, `providers:
  sw:
    module: fake
    settings:
      state: `+h.state+`
      kinds:
        device:
          fields:
            os: string
            note: string
          ops:
            upgrade: {set: {os: "2.0"}}
`)
	write(t, filepath.Join(h.intent, "a.yaml"), intentYAML)
	write(t, h.state, `{"sw/device/gw1": {"availability": "present", "fields": {"os": "1.0", "note": "a"}}}`+"\n")
	return h
}

func (h *human) run(args ...string) (int, string, string) {
	h.t.Helper()
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), args, &stdout, &stderr, cli.WithModule("fake", fake.Module), cli.WithClock(h.clock))
	return code, stdout.String(), stderr.String()
}

func (h *human) reconcile() (int, string) {
	h.t.Helper()
	code, out, errOut := h.run("reconcile", "--intent", h.intent, "--config", h.config, "--record", h.recd)
	return code, out + errOut
}

var requestRe = regexp.MustCompile(`sakutei approve ([A-Za-z0-9._@-]+)`)

// 不変条件 21：申請者と承認者が同じなら拒否する。承認者の一覧に無い人も拒否する。
func TestApproveCLI(t *testing.T) {
	h := newHuman(t, `policies:
  - name: platform
    select: {all: true}
    applicant: carol
    approval: {quorum: {n: 1, of: [alice, carol]}, separation: true, timeout: 24h}
resources:
  - {type: sw/device, id: gw1, fields: {note: b}}
`)
	code, out := h.reconcile()
	if code != cli.ExitRemaining || !strings.Contains(out, "承認待ち: sakutei approve") {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	m := requestRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("依頼の ID が出ない:\n%s", out)
	}
	id := m[1]
	code, out, _ = h.run("approvals", "--record", h.recd)
	if code != 0 || !strings.Contains(out, id) || !strings.Contains(out, "申請者: carol") {
		t.Fatalf("approvals: %s", out)
	}
	if code, _, errOut := h.run("approve", id, "--record", h.recd, "--as", "carol"); code != cli.ExitError || !strings.Contains(errOut, "申請者") {
		t.Fatalf("申請者の承認を受け付けた: %d %s", code, errOut)
	}
	if code, _, errOut := h.run("approve", id, "--record", h.recd, "--as", "dave"); code != cli.ExitError || !strings.Contains(errOut, "承認者") {
		t.Fatalf("一覧に無い人の承認を受け付けた: %d %s", code, errOut)
	}
	if code, _, errOut := h.run("reject", id, "--record", h.recd, "--as", "alice"); code != cli.ExitError || !strings.Contains(errOut, "--comment") {
		t.Fatalf("理由の無い拒否を受け付けた: %d %s", code, errOut)
	}
	if code, _, errOut := h.run("approve", "--record", h.recd, "--as", "alice", "no-such"); code != cli.ExitError || !strings.Contains(errOut, "無い") {
		t.Fatalf("無い依頼: %d %s", code, errOut)
	}
	if code, out, errOut := h.run("approve", id, "--record", h.recd, "--as", "alice", "--comment", "確認済み"); code != 0 {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	if code, out := h.reconcile(); code != cli.ExitOK || !strings.Contains(out, "承認: alice") {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	if code, out, _ := h.run("approve", id, "--record", h.recd, "--as", "alice"); code != cli.ExitError || out != "" {
		t.Fatalf("使った依頼に判断を受け付けた: %d", code)
	}
	if code, out, _ := h.run("audit", "verify", "--record", h.recd); code != 0 {
		t.Fatalf("audit verify: %s", out)
	}
}

// 不変条件 22：freeze は理由と期限が要り、期限で消える。unfreeze で終える。
func TestFreezeCLI(t *testing.T) {
	h := newHuman(t, `resources:
  - {type: sw/device, id: gw1, fields: {note: b}}
`)
	for _, args := range [][]string{
		{"freeze", "--record", h.recd, "--until", "1h", "--as", "alice"},
		{"freeze", "--record", h.recd, "--reason", "x", "--as", "alice"},
		{"freeze", "--record", h.recd, "--reason", "x", "--until", "2026-11-05T12:00:00Z", "--as", "alice"},
		{"freeze", "--record", h.recd, "--reason", "x", "--until", "1h", "--mode", "apply", "--as", "alice"},
		{"freeze", "--record", h.recd, "--reason", "x", "--until", "1h", "--select", "site", "--as", "alice"},
	} {
		if code, _, errOut := h.run(args...); code != cli.ExitError {
			t.Fatalf("%v を受け付けた: %s", args, errOut)
		}
	}
	code, out, errOut := h.run("freeze", "--record", h.recd, "--reason", "障害対応中", "--until", "2h", "--name", "inc1",
		"--select", "type=sw/device", "--as", "alice")
	if code != 0 {
		t.Fatalf("%s %s", out, errOut)
	}
	if code, out := h.reconcile(); code != cli.ExitRemaining || !strings.Contains(out, "一時的な Policy inc1") || !strings.Contains(out, "障害対応中") {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	if code, _, _ := h.run("plan", "--intent", h.intent, "--config", h.config, "--record", h.recd); code != 0 {
		t.Fatal("plan")
	}
	// 期限が過ぎれば消える。
	h.clock.t = h.clock.t.Add(2 * time.Hour)
	if code, out := h.reconcile(); code != cli.ExitOK {
		t.Fatalf("期限のあと: exit=%d\n%s", code, out)
	}
	// unfreeze。
	write(t, h.state, `{"sw/device/gw1": {"availability": "present", "fields": {"os": "1.0", "note": "a"}}}`+"\n")
	h.run("freeze", "--record", h.recd, "--reason", "手作業", "--until", "24h", "--name", "f2", "--as", "alice")
	if code, _ := h.reconcile(); code != cli.ExitRemaining {
		t.Fatal("freeze が効かない")
	}
	if code, _, errOut := h.run("unfreeze", "f2", "--record", h.recd, "--as", "bob"); code != cli.ExitError || !strings.Contains(errOut, "理由") {
		t.Fatalf("理由の無い unfreeze: %s", errOut)
	}
	if code, _, errOut := h.run("unfreeze", "f2", "--record", h.recd, "--as", "bob", "--reason", "終わり"); code != 0 {
		t.Fatal(errOut)
	}
	if code, out := h.reconcile(); code != cli.ExitOK {
		t.Fatalf("unfreeze のあと: exit=%d\n%s", code, out)
	}
	data, _ := os.ReadFile(filepath.Join(h.recd, "audit", "000001.jsonl"))
	for _, k := range []string{`"freeze.start"`, `"freeze.expire"`, `"freeze.end"`} {
		if !strings.Contains(string(data), k) {
			t.Fatalf("監査に %s が無い:\n%s", k, data)
		}
	}
}

// Procedure を人が start し、gate を承認し、観測で閉じる。取り消し（cancel）。
func TestStartAndCancelCLI(t *testing.T) {
	h := newHuman(t, `resources:
  - {type: sw/device, id: gw1, fields: {note: a}}
procedures:
  - name: gw-upgrade
    target: {type: sw/device, id: gw1}
    trigger: manual
    steps:
      - name: approve
        gate: {approval: {quorum: {n: 1, of: [alice, carol]}, separation: true}}
      - name: upgrade
        invoke: {op: upgrade}
    done_when: 'observe(sw/device/{target}).os == "2.0"'
`)
	if code, _, errOut := h.run("start", "gw-upgrade", "--record", h.recd, "--as", "carol"); code != 0 {
		t.Fatal(errOut)
	}
	code, out := h.reconcile()
	if code != cli.ExitRemaining || !strings.Contains(out, "Procedure gw-upgrade sw/device/gw1: 実行中") {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	id := requestRe.FindStringSubmatch(out)[1]
	if code, _, _ := h.run("approve", id, "--record", h.recd, "--as", "carol"); code != cli.ExitError {
		t.Fatal("start した人（申請者）の承認を受け付けた")
	}
	h.run("approve", id, "--record", h.recd, "--as", "alice")
	if code, out := h.reconcile(); code != cli.ExitOK || !strings.Contains(out, "完了") {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	data, _ := os.ReadFile(h.state)
	if !strings.Contains(string(data), `"2.0"`) {
		t.Fatalf("送っていない: %s", data)
	}

	// もう一度 start して取り消す。
	write(t, h.state, `{"sw/device/gw1": {"availability": "present", "fields": {"os": "1.0", "note": "a"}}}`+"\n")
	h.run("start", "gw-upgrade", "--record", h.recd, "--as", "carol")
	_, out = h.reconcile()
	runID := regexp.MustCompile(`（(gw-upgrade-[^）]+)）`).FindStringSubmatch(out)
	if runID == nil {
		t.Fatalf("run の ID が無い:\n%s", out)
	}
	if code, _, _ := h.run("cancel", runID[1], "--record", h.recd, "--as", "carol"); code != cli.ExitError {
		t.Fatal("理由の無い取り消し")
	}
	h.run("cancel", runID[1], "--record", h.recd, "--as", "carol", "--reason", "やめる")
	if code, out := h.reconcile(); code != cli.ExitOK || !strings.Contains(out, "取り消した") {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	// 存在しない Procedure の start の依頼は拒否して片づける。
	h.run("start", "nope", "--record", h.recd, "--as", "carol")
	h.reconcile()
	ents, _ := os.ReadDir(filepath.Join(h.recd, "commands"))
	if len(ents) != 0 {
		t.Fatalf("依頼が残る: %v", ents)
	}
	if code, out, _ := h.run("audit", "verify", "--record", h.recd); code != 0 {
		t.Fatal(out)
	}
	if code, out, _ := h.run("approvals", "--record", h.recd, "--all", "--json"); code != 0 || !json.Valid([]byte(out)) {
		t.Fatal(out)
	}
}
