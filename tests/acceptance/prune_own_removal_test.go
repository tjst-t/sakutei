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

// prune_own_removal_test.go — #75: Sakutei 自身が prune で消した分は、次の周の
// List 急減判定（§12.4（b）、設計書 v3.13）で基準の件数から差し引く。
//
//	[AC-75-1]  試用 D3：ゾーンに www・old1・old2、intent は www だけを
//	           PruneWithinScope。--max-destructive-per-cycle 1
//	           --approve-large-change 2 のデーモンは cycle 1 で 2 件を消し、
//	           次の周から急減 halt せず converged が続く。
//	[AC-75-2]  予算で止めた Delete は差し引かない：止めたあと外から 3 → 1 に
//	           減らすと、今どおり急減 halt する。
//
// 自分が消していない ID の消失を同じ周に重ねた場合（差し引いても急減なら halt）
// と、Delete の完了が次の List より後になる場合は internal/prune の
// TestPruneEngine_CountDrop_OwnPruneNotCounted で示す。
package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const ownPruneZone = "; D3\n" +
	"$ORIGIN example.com.\n" +
	"$TTL 300\n" +
	"@\t300 IN SOA ns1 admin 2024010101 3600 900 604800 300\n" +
	"@\t300 IN NS ns1\n" +
	"www 300 IN A 192.0.2.1\n" +
	"old1 300 IN A 192.0.2.11\n" +
	"old2 300 IN A 192.0.2.12\n"

func TestPruneOwnRemoval_D3ApprovedPruneThenConverges(t *testing.T) {
	// [AC-75-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	zonePath := filepath.Join(tmp, "example.com.zone")
	logPath := filepath.Join(tmp, "daemon.log")
	writeZoneFile(t, zonePath, ownPruneZone)
	writeIntentFile(t, intentDir, map[string]any{"resources": []map[string]any{
		dnszoneResource("www.example.com./A", "www.example.com.", "A", "192.0.2.1", "300", "PruneWithinScope"),
	}})

	cmd, waitCmd := startDaemonWithFlags(t, logPath,
		"--intent", intentDir, "--state", filepath.Join(tmp, "state.json"),
		"--zone", zonePath, "--interval", "50ms",
		"--max-destructive-per-cycle", "1", "--approve-large-change", "2")
	if !waitForLogFile(logPath, "cycle 5:", 10*time.Second) {
		_ = cmd.Process.Kill()
		_ = waitCmd()
		t.Fatalf("[AC-75-1] daemon did not reach cycle 5; log:\n%s", readFileString(logPath))
	}
	_ = cmd.Process.Kill()
	_ = waitCmd()
	log := readFileString(logPath)

	zone := readZoneFile(t, zonePath)
	if strings.Contains(zone, "old1") || strings.Contains(zone, "old2") || !strings.Contains(zone, "www") {
		t.Errorf("[AC-75-1] zone after the approved prune, want only www left:\n%s", zone)
	}
	if strings.Contains(log, "prune HALT") {
		t.Errorf("[AC-75-1] the daemon took its own prune for a sudden drop; log:\n%s", log)
	}
	sum := cycleSummaries(log)
	for _, c := range []string{"2", "3", "4"} {
		if s := sum[c]; s != "converged (no changes)" {
			t.Errorf("[AC-75-1] cycle %s summary %q, want converged (no changes); log:\n%s", c, s, log)
		}
	}
}

func TestPruneOwnRemoval_BudgetHeldDeleteNotDiscounted(t *testing.T) {
	// [AC-75-2]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	logPath := filepath.Join(tmp, "daemon.log")
	seedManyPruneableResources(t, statePath, 3)
	writeDesiredKeepOneResource(t, intentDir, "vm-01")

	cmd, waitCmd := startDaemonWithFlags(t, logPath,
		"--intent", intentDir, "--state", statePath, "--interval", "50ms",
		"--max-destructive-per-cycle", "1")
	killed := false
	defer func() {
		if !killed {
			_ = cmd.Process.Kill()
			_ = waitCmd()
		}
	}()
	// The budget holds both deletes (List baseline 3; nothing deleted).
	if !waitForLogFile(logPath, "BUDGET HALTED", 10*time.Second) {
		t.Fatalf("[AC-75-2] daemon did not emit BUDGET HALTED; log:\n%s", readFileString(logPath))
	}
	// Someone else removes vm-02 and vm-03: 3 → 1 must still halt — the
	// held deletes were never ours.
	seedManyPruneableResources(t, statePath, 1)
	if !waitForLogFile(logPath, "prune HALT: List count dropped from 3 to 1", 10*time.Second) {
		t.Fatalf("[AC-75-2] a budget-held delete was discounted from the drop gate; log:\n%s", readFileString(logPath))
	}
	_ = cmd.Process.Kill()
	_ = waitCmd()
	killed = true
}
