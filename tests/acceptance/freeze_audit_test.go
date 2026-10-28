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

// Backlog #184: freeze を監査に残す（誰が・なぜ・いつまで）。
//
//	[AC-184-1] 理由の無い freeze（--freeze・sakutei freeze start）と、監査の記録の
//	           無い --freeze は断る。何も変えず、監査にも何も書かない。
//	[AC-184-2] 一回性の reconcile の --freeze は始まりと終わりを、誰が・理由・
//	           期限つきで監査に残し、level の Event Frozen を出す。
//	[AC-184-3] 動いている常駐を止めずに sakutei freeze start / end で freeze し、
//	           始まり・終わりが監査に残る。freeze の間は直さず、status と常駐の
//	           健全性に freeze が出る。期限を過ぎた freeze は解け、期限切れが
//	           監査に残る。
//	[AC-184-4] 起動に失敗した常駐の --freeze は監査に何も残さない。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/audit"
)

func runSakuteiFreeze(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(buildBinary(t), args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run sakutei %v: %v", args, err)
	}
	return out.String(), errb.String(), code
}

func freezeRecords(t *testing.T, path string) []audit.Record {
	t.Helper()
	recs, err := audit.ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []audit.Record
	for _, r := range recs {
		if strings.HasPrefix(string(r.Kind), "freeze.") {
			out = append(out, r)
		}
	}
	return out
}

func freezeKinds(recs []audit.Record) string {
	var ks []string
	for _, r := range recs {
		ks = append(ks, string(r.Kind))
	}
	return strings.Join(ks, ",")
}

func TestFreezeAudit_RefusesWithoutReason(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	statePath := filepath.Join(tmp, "state.json")
	auditPath := filepath.Join(tmp, "audit.jsonl")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seedDriftedState(t, statePath)
	writeDriftIntent(t, intentDir)
	before := stateFileBytes(t, statePath)

	_, stderr, code := runReconcile(t, intentDir, statePath, "--freeze", "--audit-log", auditPath)
	if code != 1 || !strings.Contains(stderr, "--freeze needs --freeze-reason") {
		t.Errorf("[AC-184-1] --freeze without a reason must be refused (exit %d):\n%s", code, stderr)
	}
	_, stderr, code = runReconcile(t, intentDir, statePath, "--freeze", "--freeze-reason", "   ", "--audit-log", auditPath)
	if code != 1 {
		t.Errorf("[AC-184-1] a blank reason must be refused (exit %d):\n%s", code, stderr)
	}
	_, stderr, code = runReconcile(t, intentDir, statePath, "--freeze", "--freeze-reason", "maintenance")
	if code != 1 || !strings.Contains(stderr, "--freeze needs --audit-log") {
		t.Errorf("[AC-184-1] --freeze without an audit log must be refused (exit %d):\n%s", code, stderr)
	}
	freezeState := filepath.Join(tmp, "freeze.json")
	_, stderr, code = runSakuteiFreeze(t, "freeze", "start", "--state", freezeState, "--audit-log", auditPath, "--by", "alice")
	if code != 1 || !strings.Contains(stderr, "needs a reason") {
		t.Errorf("[AC-184-1] sakutei freeze start without --reason must be refused (exit %d):\n%s", code, stderr)
	}
	if _, err := os.Stat(freezeState); !os.IsNotExist(err) {
		t.Errorf("[AC-184-1] a refused freeze must not write the state file: %v", err)
	}
	if recs := freezeRecords(t, auditPath); len(recs) != 0 {
		t.Errorf("[AC-184-1] a refused freeze must not be recorded: %s", freezeKinds(recs))
	}
	if !bytes.Equal(before, stateFileBytes(t, statePath)) {
		t.Error("[AC-184-1] a refused run must not change reality")
	}
}

func TestFreezeAudit_OneShotFlagStartAndEnd(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	statePath := filepath.Join(tmp, "state.json")
	auditPath := filepath.Join(tmp, "audit.jsonl")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seedDriftedState(t, statePath)
	writeDriftIntent(t, intentDir)
	before := stateFileBytes(t, statePath)

	stdout, stderr, code := runSakuteiFreeze(t, "reconcile", "--intent", intentDir, "--state", statePath, "--freeze", "--freeze-reason", "storage maintenance",
		"--freeze-by", "alice", "--freeze-until", "2h", "--audit-log", auditPath)
	if code != 0 || !strings.Contains(stdout, "FROZEN") {
		t.Fatalf("[AC-184-2] exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !bytes.Equal(before, stateFileBytes(t, statePath)) {
		t.Error("[AC-184-2] a frozen run must not change reality")
	}
	if !strings.Contains(stderr, "Frozen") || !strings.Contains(stderr, "storage maintenance") {
		t.Errorf("[AC-184-2] the run must raise Frozen with the reason:\n%s", stderr)
	}
	recs := freezeRecords(t, auditPath)
	if freezeKinds(recs) != "freeze.started,freeze.ended" {
		t.Fatalf("[AC-184-2] want started then ended, got %s", freezeKinds(recs))
	}
	for _, r := range recs {
		if r.Actor != "alice" || !strings.Contains(r.Detail, "storage maintenance") || !strings.Contains(r.Detail, "until ") {
			t.Errorf("[AC-184-2] the record must carry who, why and until when: %+v", r)
		}
	}
}

func TestFreezeAudit_DaemonFrozenWithoutStopping(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	statePath := filepath.Join(tmp, "state.json")
	auditPath := filepath.Join(tmp, "audit.jsonl")
	freezeState := filepath.Join(tmp, "freeze.json")
	daemonStatus := filepath.Join(tmp, "daemon-status.json")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seedDriftedState(t, statePath)
	writeDriftIntent(t, intentDir)

	// Frozen before the daemon's first cycle, through the write side.
	out, stderr, code := runSakuteiFreeze(t, "freeze", "start", "--state", freezeState, "--audit-log", auditPath,
		"--by", "bob", "--reason", "switch replacement")
	if code != 0 {
		t.Fatalf("[AC-184-3] freeze start: exit %d\n%s%s", code, out, stderr)
	}
	_, stderr, code = runSakuteiFreeze(t, "freeze", "start", "--state", freezeState, "--audit-log", auditPath,
		"--by", "carol", "--reason", "again")
	if code != 1 || !strings.Contains(stderr, "already frozen by bob") {
		t.Errorf("[AC-184-3] a second freeze while one is in effect must be refused (exit %d):\n%s", code, stderr)
	}

	logPath := filepath.Join(tmp, "daemon.log")
	cmd, _ := startDaemonWithFlags(t, logPath, "--intent", intentDir, "--state", statePath, "--interval", "100ms",
		"--freeze-state", freezeState, "--audit-log", auditPath, "--daemon-status", daemonStatus)
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	if !waitForLogFileCount(logPath, "FROZEN (alert-only)", 3, 10*time.Second) {
		b, _ := os.ReadFile(logPath)
		t.Fatalf("[AC-184-3] the daemon must hold the drift while frozen:\n%s", b)
	}
	if got := mustDriftSize(t, statePath); got != "1" {
		t.Fatalf("[AC-184-3] a frozen daemon must not repair: size=%s", got)
	}
	if b, _ := os.ReadFile(logPath); !strings.Contains(string(b), "Frozen") || !strings.Contains(string(b), "switch replacement") {
		t.Errorf("[AC-184-3] the daemon must raise Frozen with the reason:\n%s", b)
	}
	if body, ok := waitForFile(daemonStatus, 5*time.Second, func(s string) bool { return strings.Contains(s, `"freeze"`) }); !ok {
		t.Errorf("[AC-184-3] the daemon status must carry the freeze:\n%s", body)
	}
	out, _, _ = runSakuteiFreeze(t, "status", "--intent", intentDir, "--state", statePath, "--freeze-state", freezeState, "--daemon-status", daemonStatus)
	if !strings.Contains(out, "FROZEN") || !strings.Contains(out, "誰が bob") || !strings.Contains(out, "switch replacement") {
		t.Errorf("[AC-184-3] status must show the freeze with who and why:\n%s", out)
	}
	out, _, _ = runSakuteiFreeze(t, "status", "--intent", intentDir, "--state", statePath, "--freeze-state", freezeState, "--json")
	var js struct {
		Freeze *struct{ By, Reason string } `json:"freeze"`
	}
	if err := json.Unmarshal([]byte(out), &js); err != nil || js.Freeze == nil || js.Freeze.By != "bob" {
		t.Errorf("[AC-184-3] status --json must carry the freeze (%v):\n%s", err, out)
	}

	// Ended without stopping the daemon: it repairs again.
	if out, stderr, code := runSakuteiFreeze(t, "freeze", "end", "--state", freezeState, "--audit-log", auditPath,
		"--by", "bob", "--reason", "switch replaced"); code != 0 {
		t.Fatalf("[AC-184-3] freeze end: exit %d\n%s%s", code, out, stderr)
	}
	if !waitForDriftSize(statePath, "42", 10*time.Second) {
		b, _ := os.ReadFile(logPath)
		t.Fatalf("[AC-184-3] the daemon must repair once the freeze is ended:\n%s", b)
	}
	if !waitForLogFile(logPath, "resolved", 5*time.Second) {
		b, _ := os.ReadFile(logPath)
		t.Errorf("[AC-184-3] Frozen must resolve:\n%s", b)
	}

	// A freeze with a deadline is lifted when it passes, and the expiry recorded.
	if out, stderr, code := runSakuteiFreeze(t, "freeze", "start", "--state", freezeState, "--audit-log", auditPath,
		"--by", "dave", "--reason", "short check", "--until", "1500ms"); code != 0 {
		t.Fatalf("[AC-184-3] freeze start --until: exit %d\n%s%s", code, out, stderr)
	}
	if !waitForLogFile(logPath, "short check", 5*time.Second) {
		b, _ := os.ReadFile(logPath)
		t.Fatalf("[AC-184-3] the daemon must take the second freeze in:\n%s", b)
	}
	seedDriftedState(t, statePath)
	if !waitForLogFile(logPath, "expired at", 10*time.Second) {
		b, _ := os.ReadFile(logPath)
		t.Fatalf("[AC-184-3] the freeze must expire:\n%s", b)
	}
	if !waitForDriftSize(statePath, "42", 10*time.Second) {
		t.Fatal("[AC-184-3] the daemon must repair once the freeze expired")
	}
	if b, _ := os.ReadFile(freezeState); strings.Contains(string(b), "short check") {
		t.Errorf("[AC-184-3] an expired freeze must be cleared from the state file:\n%s", b)
	}

	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()

	recs := freezeRecords(t, auditPath)
	if got := freezeKinds(recs); got != "freeze.started,freeze.ended,freeze.started,freeze.expired" {
		t.Fatalf("[AC-184-3] audit records: %s", got)
	}
	want := []struct{ actor, detail string }{
		{"bob", "switch replacement"}, {"bob", "switch replaced"}, {"dave", "short check"}, {"dave", "short check"},
	}
	for i, w := range want {
		if recs[i].Actor != w.actor || !strings.Contains(recs[i].Detail, w.detail) {
			t.Errorf("[AC-184-3] record %d: want %s / %q, got %+v", i, w.actor, w.detail, recs[i])
		}
	}
	if !strings.Contains(recs[2].Detail, "until ") {
		t.Errorf("[AC-184-3] the deadline must be recorded: %+v", recs[2])
	}
}

func TestFreezeAudit_DaemonFlagExpires(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	statePath := filepath.Join(tmp, "state.json")
	auditPath := filepath.Join(tmp, "audit.jsonl")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seedDriftedState(t, statePath)
	writeDriftIntent(t, intentDir)

	logPath := filepath.Join(tmp, "daemon.log")
	cmd, _ := startDaemonWithFlags(t, logPath, "--intent", intentDir, "--state", statePath, "--interval", "100ms",
		"--freeze", "--freeze-reason", "upgrade window", "--freeze-by", "erin", "--freeze-until", "1500ms", "--audit-log", auditPath)
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	if !waitForLogFile(logPath, "FROZEN (alert-only)", 5*time.Second) {
		b, _ := os.ReadFile(logPath)
		t.Fatalf("[AC-184-3] the --freeze daemon must start frozen:\n%s", b)
	}
	if !waitForDriftSize(statePath, "42", 10*time.Second) {
		b, _ := os.ReadFile(logPath)
		t.Fatalf("[AC-184-3] the --freeze must be lifted at its deadline:\n%s", b)
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
	recs := freezeRecords(t, auditPath)
	if got := freezeKinds(recs); got != "freeze.started,freeze.expired" {
		t.Fatalf("[AC-184-3] an expired --freeze is recorded as expired, not ended again: %s", got)
	}
	for _, r := range recs {
		if r.Actor != "erin" || !strings.Contains(r.Detail, "upgrade window") {
			t.Errorf("[AC-184-3] record: %+v", r)
		}
	}
}

func mustDriftSize(t *testing.T, statePath string) string {
	t.Helper()
	s, err := driftSize(statePath)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func driftSize(statePath string) (string, error) {
	b, err := os.ReadFile(statePath)
	if err != nil {
		return "", err
	}
	var sf struct {
		Resources map[string]struct {
			Fields map[string]string `json:"fields"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(b, &sf); err != nil {
		return "", err
	}
	return sf.Resources["drift-vm"].Fields["size"], nil
}

func waitForDriftSize(statePath, want string, deadline time.Duration) bool {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if s, err := driftSize(statePath); err == nil && s == want {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// [AC-184-4] 起動に失敗した常駐の --freeze は監査に何も残さない（一度も効いて
// いない freeze の started だけが残ると、終わりの無い freeze が今も効いている
// ように見える）。起動の検証と listen が全部済んでから始まりを記録する。
func TestFreezeAudit_DaemonStartFailureLeavesNoFreeze(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	statePath := filepath.Join(tmp, "state.json")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seedDriftedState(t, statePath)
	writeDriftIntent(t, intentDir)

	// An address already in use makes the /metrics listen fail.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	cases := []struct {
		name  string
		extra []string
	}{
		{"invalid --claim", []string{"--claim", "no-equals-sign"}},
		{"--shard-id without --fencing-store", []string{"--shard-id", "s1"}},
		{"invalid --alloc-lookup-ttl", []string{"--alloc-store", filepath.Join(tmp, "alloc.json"), "--alloc-lookup-ttl", "soon"}},
		{"metrics listen fails", []string{"--metrics-listen", ln.Addr().String()}},
	}
	for i, c := range cases {
		auditPath := filepath.Join(tmp, fmt.Sprintf("audit-%d.jsonl", i))
		args := append([]string{"--intent", intentDir, "--state", statePath, "--interval", "100ms",
			"--freeze", "--freeze-reason", "maintenance", "--audit-log", auditPath}, c.extra...)
		cmd := exec.Command(buildBinary(t), args...)
		var errb bytes.Buffer
		cmd.Stderr = &errb
		done := make(chan error, 1)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
				t.Errorf("[AC-184-4] %s: want exit 1, got %v\n%s", c.name, err, errb.String())
			}
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Fatalf("[AC-184-4] %s: the daemon did not fail to start:\n%s", c.name, errb.String())
		}
		if recs := freezeRecords(t, auditPath); len(recs) != 0 {
			t.Errorf("[AC-184-4] %s: a daemon that failed to start must leave no freeze in the audit log: %s", c.name, freezeKinds(recs))
		}
	}
}
