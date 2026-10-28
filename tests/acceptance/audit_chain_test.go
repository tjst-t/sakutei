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

package acceptance

// Backlog #187: 監査の記録を固める（ふつうの Apply も・改ざんしにくい形・外へすぐ送る）。
//
//	[AC-187-1] Apply（常駐・一回性の reconcile・Workflow の apply の段）と prune と
//	           承認ゲートの承認と freeze の始まり・終わりが、1 つの監査のログに
//	           鎖（seq と前の行のハッシュ）つきで残る。Apply・prune の記録は
//	           resource・op・変えた欄・cause・結果を持つ。
//	[AC-187-2] sakutei audit verify は無傷のログを intact（exit 0）とし、行の
//	           書き換え・抜き取り・差し込み・回したファイルの抜けを見つける（exit 3）。
//	[AC-187-3] --audit-syslog の受け手（RFC 5424、TCP と TLS）へ 1 件ずつ届く。
//	           受け手が止まっている間もコマンドは動き、記録は手元に貯まり、
//	           戻ったら順に送り直す。送った所を超える末尾の切り詰めは verify が
//	           見つける。

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/audit"
	"github.com/tjst-t/sakutei/v2/internal/audit/syslogtest"
	"github.com/tjst-t/sakutei/v2/internal/fakeprovider"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
)

// seedPruneScene はリアルに直す 1 件（keep: size 1 → 42）と消す 1 件（orphan）を置き、
// intent に keep と作る 1 件（fresh）を書く。
func seedPruneScene(t *testing.T, intentDir, statePath string) {
	t.Helper()
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seedStateWithResources(t, statePath, map[string]map[string]any{
		"keep":   {"name": "keep", "size": "1"},
		"orphan": {"name": "orphan", "size": "9"},
	})
	seedStatePruneableCapability(t, statePath)
	seedStateScopes(t, statePath, map[string]map[string]string{
		"keep": {"site": "tokyo"}, "orphan": {"site": "tokyo"},
	})
	res := func(id, size string) map[string]any {
		return map[string]any{"provider": "fake", "id": id, "scope": map[string]string{"site": "tokyo"},
			"deletion_policy": "PruneWithinScope", "fields": map[string]string{"name": id, "size": size}}
	}
	writeIntentFile(t, intentDir, map[string]any{"resources": []map[string]any{res("keep", "42"), res("fresh", "3")}})
}

func recordsOf(t *testing.T, path string, kind audit.Kind) []audit.Record {
	t.Helper()
	recs, err := audit.ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []audit.Record
	for _, r := range recs {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

func auditLines(t *testing.T, path string) [][]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n"))
}

func writeAuditLines(t *testing.T, path string, lines [][]byte) {
	t.Helper()
	if err := os.WriteFile(path, append(bytes.Join(lines, []byte("\n")), '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAuditChain_AllChangesAndApprovalsChained(t *testing.T) {
	// Workflow：常駐の Apply・Workflow の apply の段・承認ゲートの承認（CLI）。
	r := newWFRig(t, rollingIntent)
	r.runToFirstGate()
	out, code := runSakutei(t, "approve", "--store", r.storePath, "--audit-log", r.auditPath,
		"--approver", "alice", "--signature", "sig-a", "rolling", "s1")
	if code != 0 {
		t.Fatalf("approve: exit %d\n%s", code, out)
	}
	r.cycleUntil(10, awaiting("s2"))
	auditPath := r.auditPath

	// 常駐の wave の Apply（drift-vm: size 1 → 42、cause B）。
	driftDir := filepath.Join(r.root, "intent-drift")
	driftState := filepath.Join(r.root, "state-drift.json")
	if err := os.MkdirAll(driftDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seedDriftedState(t, driftState)
	writeDriftIntent(t, driftDir)
	d := reconcile.NewDaemon(reconcile.Config{
		IntentDir:    driftDir,
		GeneratedDir: filepath.Join(r.root, "generated-drift"),
		Provider:     fakeprovider.New(driftState),
		Out:          &syncBuf{},
		ErrOut:       &syncBuf{},
		Audit:        audit.NewLog(auditPath),
	})
	if _, err := d.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.Wait(5 * time.Second)
	if got := mustDriftSize(t, driftState); got != "42" {
		t.Fatalf("the daemon did not repair the drift: size=%s", got)
	}

	// freeze の始まりと終わり。
	freezeState := filepath.Join(r.root, "freeze.json")
	if out, stderr, code := runSakuteiFreeze(t, "freeze", "start", "--state", freezeState, "--audit-log", auditPath,
		"--by", "bob", "--reason", "switch replacement"); code != 0 {
		t.Fatalf("freeze start: exit %d\n%s%s", code, out, stderr)
	}
	if out, stderr, code := runSakuteiFreeze(t, "freeze", "end", "--state", freezeState, "--audit-log", auditPath,
		"--by", "bob", "--reason", "done"); code != 0 {
		t.Fatalf("freeze end: exit %d\n%s%s", code, out, stderr)
	}

	// 一回性の reconcile：Apply（update と create）と prune。
	intentDir := filepath.Join(r.root, "intent2")
	statePath := filepath.Join(r.root, "state2.json")
	seedPruneScene(t, intentDir, statePath)
	if stdout, stderr, code := runReconcile(t, intentDir, statePath, "--audit-log", auditPath); code != 0 {
		t.Fatalf("reconcile: exit %d\n%s\n%s", code, stdout, stderr)
	}
	if resourceExistsInState(t, statePath, "orphan") {
		t.Fatal("the orphan was not pruned")
	}

	// [AC-187-1]
	applies := recordsOf(t, auditPath, audit.KindApply)
	byRes := map[string]audit.Record{}
	for _, a := range applies {
		byRes[a.Resource] = a
	}
	if a := byRes["fake/site=tokyo/keep"]; a.Op != "update" || strings.Join(a.Fields, ",") != "size" || a.Cause == "" || a.Result != audit.ResultOK {
		t.Errorf("[AC-187-1] the update of keep must be recorded with its field, cause and result: %+v", a)
	}
	if a := byRes["fake/site=tokyo/fresh"]; a.Op != "create" || a.Result != audit.ResultOK {
		t.Errorf("[AC-187-1] the create of fresh must be recorded: %+v", a)
	}
	if a := byRes["fake//drift-vm"]; a.Op != "update" || strings.Join(a.Fields, ",") != "size" || a.Cause != "B" || a.Result != audit.ResultOK {
		t.Errorf("[AC-187-1] the daemon's Apply of drift-vm must be recorded with cause B: %+v", a)
	}
	if a := byRes["fake//n1"]; a.Op != "update" || strings.Join(a.Fields, ",") != "active" {
		t.Errorf("[AC-187-1] the Workflow's apply step on n1 must be recorded: %+v", a)
	}
	prunes := recordsOf(t, auditPath, audit.KindPrune)
	if len(prunes) != 1 || prunes[0].Resource != "fake/site=tokyo/orphan" || prunes[0].Op != "delete" || prunes[0].Result != audit.ResultOK {
		t.Errorf("[AC-187-1] the prune of orphan must be recorded: %+v", prunes)
	}
	if g := recordsOf(t, auditPath, audit.KindGateApproved); len(g) != 1 || g[0].Actor != "alice" {
		t.Errorf("[AC-187-1] the gate approval must be recorded: %+v", g)
	}
	if s, e := recordsOf(t, auditPath, audit.KindFreezeStarted), recordsOf(t, auditPath, audit.KindFreezeEnded); len(s) != 1 || len(e) != 1 {
		t.Errorf("[AC-187-1] the freeze start and end must be recorded: %+v %+v", s, e)
	}
	recs, _ := audit.ReadAll(auditPath)
	for i, rec := range recs {
		if rec.Seq != int64(i+1) || (i > 0) != (rec.Prev != "") {
			t.Fatalf("[AC-187-1] record %d is not chained: %+v", i, rec)
		}
	}

	// [AC-187-2] 無傷のログは intact。
	out, code = runSakutei(t, "audit", "verify", "--audit-log", auditPath)
	if code != 0 || !strings.Contains(out, "intact") {
		t.Fatalf("[AC-187-2] verify of an intact log: exit %d\n%s", code, out)
	}
	var rep struct {
		OK      bool  `json:"ok"`
		Records int64 `json:"records"`
		LastSeq int64 `json:"last_seq"`
	}
	out, code = runSakutei(t, "audit", "verify", "--audit-log", auditPath, "--json")
	if err := json.Unmarshal([]byte(out), &rep); err != nil || code != 0 || !rep.OK || rep.LastSeq != int64(len(recs)) {
		t.Fatalf("[AC-187-2] verify --json: exit %d %v\n%s", code, err, out)
	}

	orig := auditLines(t, auditPath)
	tampers := map[string]func([][]byte) [][]byte{
		// 承認者を書き換える。
		"rewrite": func(l [][]byte) [][]byte {
			out := append([][]byte{}, l...)
			for i, b := range out {
				if bytes.Contains(b, []byte(`"workflow.gate.approved"`)) {
					out[i] = bytes.Replace(b, []byte(`"alice"`), []byte(`"mallory"`), 1)
				}
			}
			return out
		},
		// 承認の記録を抜く（末尾の切り詰めは送った所で見つける、下の syslog の試験）。
		"remove": func(l [][]byte) [][]byte {
			var out [][]byte
			for _, b := range l {
				if !bytes.Contains(b, []byte(`"workflow.gate.approved"`)) {
					out = append(out, b)
				}
			}
			return out
		},
		// 偽の承認を差し込む。
		"insert": func(l [][]byte) [][]byte {
			out := append([][]byte{}, l[:2]...)
			out = append(out, []byte(`{"time":"2026-01-01T00:00:00Z","kind":"workflow.gate.approved","actor":"mallory"}`))
			return append(out, l[2:]...)
		},
	}
	for name, tamper := range tampers {
		writeAuditLines(t, auditPath, tamper(orig))
		out, code := runSakutei(t, "audit", "verify", "--audit-log", auditPath)
		if code != 3 || !strings.Contains(out, "BROKEN") {
			t.Errorf("[AC-187-2] verify must find the %s (exit %d):\n%s", name, code, out)
		}
	}
	writeAuditLines(t, auditPath, orig)
	if _, code := runSakutei(t, "audit", "verify", "--audit-log", auditPath); code != 0 {
		t.Fatalf("restored log: exit %d", code)
	}
}

// [AC-187-2] 回すときは新しいファイルへ続け、前のファイルは変えない。回したファイルが
// 抜けると verify が見つける。
func TestAuditChain_RotatesIntoNewFiles(t *testing.T) {
	tmp := t.TempDir()
	auditPath := filepath.Join(tmp, "audit.jsonl")
	freezeState := filepath.Join(tmp, "freeze.json")
	var first []byte
	for i := 0; i < 3; i++ {
		for _, verb := range []string{"start", "end"} {
			if out, stderr, code := runSakuteiFreeze(t, "freeze", verb, "--state", freezeState, "--audit-log", auditPath,
				"--audit-rotate-bytes", "300", "--by", "bob", "--reason", "rotation"); code != 0 {
				t.Fatalf("freeze %s: exit %d\n%s%s", verb, code, out, stderr)
			}
			if first == nil {
				first, _ = os.ReadFile(auditPath)
			}
		}
	}
	if b, _ := os.ReadFile(auditPath); !bytes.Equal(b, first) {
		t.Errorf("[AC-187-2] the first file changed after it was rotated")
	}
	if _, err := os.Stat(auditPath + ".000002"); err != nil {
		t.Fatalf("[AC-187-2] want rotated files: %v", err)
	}
	if recs, _ := audit.ReadAll(auditPath); len(recs) != 6 || recs[5].Seq != 6 {
		t.Fatalf("[AC-187-2] the chain must run across the files: %+v", recs)
	}
	if out, code := runSakutei(t, "audit", "verify", "--audit-log", auditPath); code != 0 {
		t.Fatalf("[AC-187-2] verify across files: exit %d\n%s", code, out)
	}
	if err := os.Remove(auditPath + ".000001"); err != nil {
		t.Fatal(err)
	}
	if out, code := runSakutei(t, "audit", "verify", "--audit-log", auditPath); code != 3 || !strings.Contains(out, "missing") {
		t.Errorf("[AC-187-2] a removed rotated file must be found (exit %d):\n%s", code, out)
	}
}

func TestAuditChain_SyslogDeliversAndResends(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	statePath := filepath.Join(tmp, "state.json")
	auditPath := filepath.Join(tmp, "audit.jsonl")
	freezeState := filepath.Join(tmp, "freeze.json")
	seedPruneScene(t, intentDir, statePath)

	srv, err := syslogtest.Start("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	syslogURL := "tcp://" + srv.Addr

	// [AC-187-3] 一回性の reconcile の Apply・prune が 1 件ずつ届く。
	if stdout, stderr, code := runReconcile(t, intentDir, statePath, "--audit-log", auditPath, "--audit-syslog", syslogURL); code != 0 {
		t.Fatalf("reconcile: exit %d\n%s\n%s", code, stdout, stderr)
	}
	recs, _ := audit.ReadAll(auditPath)
	msgs := srv.Wait(len(recs), 5*time.Second)
	if len(recs) < 3 || len(msgs) != len(recs) {
		t.Fatalf("[AC-187-3] %d record(s) written, %d delivered", len(recs), len(msgs))
	}
	for i, m := range msgs {
		var got audit.Record
		if err := json.Unmarshal([]byte(m.Body), &got); err != nil || got.Seq != recs[i].Seq || got.Kind != recs[i].Kind {
			t.Errorf("[AC-187-3] message %d = %q (%v), want the record seq %d", i, m.Raw, err, recs[i].Seq)
		}
		if !strings.HasPrefix(m.Raw, "<10") || !strings.Contains(m.Raw, `[meta sequenceId="`) {
			t.Errorf("[AC-187-3] not an RFC 5424 message: %q", m.Raw)
		}
	}

	// 受け手が止まっている間もコマンドは動き、記録は手元に残る。
	srv.Stop()
	for _, verb := range []string{"start", "end"} {
		out, stderr, code := runSakuteiFreeze(t, "freeze", verb, "--state", freezeState, "--audit-log", auditPath,
			"--audit-syslog", syslogURL, "--by", "bob", "--reason", "while the receiver is down")
		if code != 0 {
			t.Fatalf("[AC-187-3] freeze %s while the receiver is down: exit %d\n%s%s", verb, code, out, stderr)
		}
		if !strings.Contains(stderr, "cannot forward to syslog") {
			t.Errorf("[AC-187-3] the run must say the record waits:\n%s", stderr)
		}
	}
	if n := len(recordsOf(t, auditPath, audit.KindFreezeStarted)); n != 1 {
		t.Fatalf("freeze started records: %d", n)
	}
	delivered := len(srv.Messages())

	// 戻ったら、貯まった分を順に送り直す。
	if err := srv.Restart(); err != nil {
		t.Fatal(err)
	}
	out, code := runSakutei(t, "audit", "forward", "--audit-log", auditPath, "--audit-syslog", syslogURL)
	if code != 0 || !strings.Contains(out, "sent 2 record(s)") {
		t.Fatalf("[AC-187-3] audit forward: exit %d\n%s", code, out)
	}
	msgs = srv.Wait(delivered+2, 5*time.Second)
	if len(msgs) != delivered+2 || msgs[delivered].MsgID != string(audit.KindFreezeStarted) || msgs[delivered+1].MsgID != string(audit.KindFreezeEnded) {
		t.Fatalf("[AC-187-3] after the restart: %d message(s)", len(msgs))
	}

	// 送った所を超える末尾の切り詰めは verify が見つける（送り先の状態が anchor）。
	if out, code := runSakutei(t, "audit", "verify", "--audit-log", auditPath); code != 0 || !strings.Contains(out, "sent to syslog") {
		t.Fatalf("[AC-187-3] verify with the syslog state: exit %d\n%s", code, out)
	}
	lines := auditLines(t, auditPath)
	writeAuditLines(t, auditPath, lines[:len(lines)-2])
	if out, code := runSakutei(t, "audit", "verify", "--audit-log", auditPath); code != 3 || !strings.Contains(out, "truncated") {
		t.Errorf("[AC-187-3] verify must find the truncation (exit %d):\n%s", code, out)
	}
}

// [AC-187-3] TLS（RFC 5425）の受け手へも届き、信じない受け手には送らない。
func TestAuditChain_SyslogTLS(t *testing.T) {
	tmp := t.TempDir()
	auditPath := filepath.Join(tmp, "audit.jsonl")
	freezeState := filepath.Join(tmp, "freeze.json")
	cfg, caPEM, err := syslogtest.SelfSigned()
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(tmp, "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	srv, err := syslogtest.Start("127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	out, stderr, code := runSakuteiFreeze(t, "freeze", "start", "--state", freezeState, "--audit-log", auditPath,
		"--audit-syslog", "tls://"+srv.Addr, "--audit-syslog-ca", caPath, "--by", "bob", "--reason", "tls")
	if code != 0 {
		t.Fatalf("freeze start: exit %d\n%s%s", code, out, stderr)
	}
	if msgs := srv.Wait(1, 5*time.Second); len(msgs) != 1 || msgs[0].MsgID != string(audit.KindFreezeStarted) {
		t.Fatalf("[AC-187-3] TLS delivery: %+v\n%s", msgs, stderr)
	}
	// CA を渡さなければ自己署名の受け手は信じない。
	_, stderr, code = runSakuteiFreeze(t, "freeze", "end", "--state", freezeState, "--audit-log", auditPath,
		"--audit-syslog", "tls://"+srv.Addr, "--by", "bob", "--reason", "tls")
	if code != 0 || !strings.Contains(stderr, "cannot forward") {
		t.Fatalf("[AC-187-3] an untrusted receiver: exit %d\n%s", code, stderr)
	}
	if n := len(srv.Messages()); n != 1 {
		t.Errorf("[AC-187-3] a record reached an untrusted receiver (%d)", n)
	}
}
