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

// display_fixes_test.go — Backlog #111: 2026-10-23 の試用で見つけた表示の不具合
// （文言・出し方。動きには関わらない）。
//
// Tags:
//
//	[AC-111-1]  採番待ちのリソースの skip の行は「unobservable」ではなく waiting-allocation と言う（1 回収束・常駐）
//	[AC-111-2]  plan・status はプールが尽きたことを言う（尽きている・一部だけ払い出せる・払い出せる）
//	[AC-111-3]  plan・status の --claim: 他のシャードの要求を「割り当て待ち」と出さず、claim の外を出さない
//	[AC-111-4]  Sync の List 不完全は sync の文言で出し、止まった組があれば最後の行がそれを言う
//	[AC-111-5]  デーモンの起動行は使っていない fake の state を出さず、mode は --mode の値と分かる文にする
//	[AC-111-6]  PlannedChangeWithdrawn の宣言の名前（internal/budget の単体テスト）
//	[AC-111-7]  1 回収束の planned_change の Event は標準エラーに `sakutei reconcile:` 付きで出る
//	[AC-111-8]  sakutei workflow list のゲートの期限と承認の数（workflow_test.go の TestWorkflowCLI）
//	[AC-111-9]  sakutei status の DRIFTED に、ずれたフィールドと値が出る
//	[AC-111-10] plan は remediation schedule の下で、今 reconcile が直すか（窓の内か外か）を言う

package acceptance

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runSplit runs the binary and returns stdout, stderr and the exit code.
func runSplit(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var o, e bytes.Buffer
	cmd := exec.Command(buildBinary(t), args...)
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run sakutei %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return o.String(), e.String(), code
}

// exhaustedIntent: a and b both draw from the one-value pool p, so b waits.
const exhaustedIntent = `{
  "pools": [ { "name": "p", "range": "100-100" } ],
  "resources": [
    { "provider": "fake", "id": "a", "fields": { "name": "a", "vlan": { "from_pool": "p" } } },
    { "provider": "fake", "id": "b", "fields": { "name": "b", "vlan": { "from_pool": "p" } } }
  ]
}`

func setupExhausted(t *testing.T, intentJSON string) (intent, state, store string) {
	t.Helper()
	tmp := t.TempDir()
	intent = filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intent, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStateJSON(t, filepath.Join(intent, "i.json"), intentJSON)
	state = filepath.Join(tmp, "state.json")
	writeStateJSON(t, state, `{"resources": {}, "provider_version": "v-test"}`)
	return intent, state, filepath.Join(tmp, "alloc.json")
}

func TestDisplay_WaitingAllocationSkipIsNotUnobservable(t *testing.T) {
	// [AC-111-1]
	intent, state, store := setupExhausted(t, exhaustedIntent)
	_, stderr, code := runSplit(t, "reconcile", "--intent", intent, "--state", state, "--alloc-store", store)
	if code != 0 {
		t.Fatalf("[AC-111-1] waiting-allocation is not a failure (exit %d):\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "sakutei: skip fake//b: waiting-allocation (not Observed or Applied until allocated") {
		t.Fatalf("[AC-111-1] the skip line must say waiting-allocation:\n%s", stderr)
	}
	if strings.Contains(stderr, "unobservable") {
		t.Fatalf("[AC-111-1] a waiting resource is not unobservable:\n%s", stderr)
	}

	// The daemon says the same.
	intent, state, store = setupExhausted(t, exhaustedIntent)
	cmd, stdout, dErr := startDaemonWithArgs(t, intent, state, "100ms", "--alloc-store", store)
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if !waitForCondition(10*time.Second, func() bool {
		return strings.Contains(dErr.String(), "sakutei: skip fake//b: waiting-allocation (not Observed")
	}) {
		t.Fatalf("[AC-111-1] the daemon's skip line must say waiting-allocation:\nstdout:\n%s\nstderr:\n%s", stdout.String(), dErr.String())
	}
	if strings.Contains(dErr.String(), "in-flight or unobservable") {
		t.Fatalf("[AC-111-1] the daemon must not call a waiting resource unobservable:\n%s", dErr.String())
	}
}

func TestDisplay_PlanStatusSayPoolExhausted(t *testing.T) {
	// [AC-111-2]
	intent, state, store := setupExhausted(t, exhaustedIntent)

	// Before any allocation: 1 free value for 2 requests.
	out, code := runSakutei(t, "plan", "--intent", intent, "--state", state, "--alloc-store", store)
	if code != 0 || !strings.Contains(out, `pool has 1 free value(s) for 2 pending request(s)`) {
		t.Fatalf("[AC-111-2] plan must say only some pending requests can be allocated (exit %d):\n%s", code, out)
	}

	mustRun(t, "reconcile", "--intent", intent, "--state", state, "--alloc-store", store)
	for _, verb := range []string{"plan", "status"} {
		out, code := runSakutei(t, verb, "--intent", intent, "--state", state, "--alloc-store", store)
		want := `sakutei ` + verb + `: pending allocation: fake//b field "vlan" from pool "p" (pool exhausted: reconcile cannot allocate it`
		if code != 0 || !strings.Contains(out, want) {
			t.Fatalf("[AC-111-2] %s must say the pool is exhausted (exit %d):\n%s", verb, code, out)
		}
		if strings.Contains(out, "allocated by reconcile") {
			t.Fatalf("[AC-111-2] %s must not promise that reconcile allocates it:\n%s", verb, out)
		}
	}

	// A pool with room still says reconcile allocates it.
	roomy := strings.Replace(exhaustedIntent, `"100-100"`, `"100-109"`, 1)
	intent, state, store = setupExhausted(t, roomy)
	if out, code := runSakutei(t, "plan", "--intent", intent, "--state", state, "--alloc-store", store); code != 0 ||
		strings.Count(out, "(allocated by reconcile)") != 2 || strings.Contains(out, "exhausted") {
		t.Fatalf("[AC-111-2] a pool with room must say allocated by reconcile (exit %d):\n%s", code, out)
	}
}

// shardedAllocIntent: site=a and site=b each have their own claimed pool, and
// each shard keeps its own allocation store (§19.8).
const shardedAllocIntent = `{
  "pools": [
    { "name": "pa", "range": "100-109", "claim": { "site": "a" } },
    { "name": "pb", "range": "200-209", "claim": { "site": "b" } }
  ],
  "resources": [
    { "provider": "fake", "id": "ra", "scope": { "site": "a" }, "fields": { "name": "ra", "vlan": { "from_pool": "pa" } } },
    { "provider": "fake", "id": "rb", "scope": { "site": "b" }, "fields": { "name": "rb", "vlan": { "from_pool": "pb" } } }
  ]
}`

func TestDisplay_PlanStatusClaim(t *testing.T) {
	// [AC-111-3]
	intent, state, storeA := setupExhausted(t, shardedAllocIntent)
	mustRun(t, "reconcile", "--intent", intent, "--state", state, "--alloc-store", storeA, "--claim", "site=a")

	// Without --claim, plan sees shard b's request as pending in shard a's store.
	if out, _ := runSakutei(t, "plan", "--intent", intent, "--state", state, "--alloc-store", storeA); !strings.Contains(out, "pending allocation: fake/site=b/rb") {
		t.Fatalf("[AC-111-3] precondition: without --claim the other shard's request shows as pending:\n%s", out)
	}

	out, code := runSakutei(t, "plan", "--intent", intent, "--state", state, "--alloc-store", storeA, "--claim", "site=a")
	if code != 0 || strings.Contains(out, "pending allocation") || strings.Contains(out, "rb") ||
		!strings.Contains(out, "--claim site=a: 1 resource(s) in claim, 1 excluded") || !strings.Contains(out, "Converged (no changes)") {
		t.Fatalf("[AC-111-3] plan --claim must show only its claim (exit %d):\n%s", code, out)
	}
	out, code = runSakutei(t, "status", "--intent", intent, "--state", state, "--alloc-store", storeA, "--claim", "site=a")
	if code != 0 || strings.Contains(out, "pending allocation") || strings.Contains(out, "rb") || !strings.Contains(out, "drift なし") {
		t.Fatalf("[AC-111-3] status --claim must report only its claim (exit %d):\n%s", code, out)
	}
	if out, code := runSakutei(t, "plan", "--intent", intent, "--state", state, "--claim", "site"); code == 0 || !strings.Contains(out, "invalid --claim") {
		t.Fatalf("[AC-111-3] a malformed --claim must be rejected (exit %d):\n%s", code, out)
	}
}

func TestDisplay_SyncIncompleteListWording(t *testing.T) {
	// [AC-111-4]
	_, intent, state := setupSyncRepo(t)
	writeStateJSON(t, state, strings.Replace(syncState, `"pruneable_capability": true`, `"pruneable_capability": true, "list_partial": true`, 1))
	stdout, stderr, code := runSplit(t, "sync", "--intent", intent, "--provider", "fake", "--state", state, "--samples", "1")
	all := stdout + stderr
	if code == 0 {
		t.Fatalf("[AC-111-4] an incomplete List must fail the pass:\n%s", all)
	}
	if strings.Contains(all, "refusing to import") || !strings.Contains(stderr, "List is not complete (status partial); not recording an incomplete view of reality (§24.5)") {
		t.Fatalf("[AC-111-4] the incomplete List must be worded for sync:\n%s", all)
	}
	if strings.Contains(stdout, "sync: nothing to commit\n") {
		t.Fatalf("[AC-111-4] a pass with a stopped pair must not end with a bare \"nothing to commit\":\n%s", stdout)
	}
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	if last := lines[len(lines)-1]; !strings.Contains(last, "sync: 1 of 1 scope/provider pair(s) stopped this pass") {
		t.Fatalf("[AC-111-4] the last line must say a pair stopped; got %q:\n%s", last, stderr)
	}
}

func TestDisplay_DaemonStartLine(t *testing.T) {
	// [AC-111-5]
	tmp := t.TempDir()
	intent := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intent, 0o755); err != nil {
		t.Fatal(err)
	}
	zone := filepath.Join(tmp, "example.com.zone")
	writeStateJSON(t, zone, "$ORIGIN example.com.\n")
	writeDNSZoneIntent(t, intent, []map[string]any{dnszoneResource("www.example.com./A", "www.example.com.", "A", "192.0.2.10", "300", "")})

	start := func(args ...string) string {
		t.Helper()
		var o syncBuffer
		cmd := exec.Command(buildBinary(t), append([]string{"--intent", intent, "--interval", "100ms"}, args...)...)
		cmd.Dir = tmp
		cmd.Stdout, cmd.Stderr = &o, &o
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
		if !waitForCondition(10*time.Second, func() bool { return strings.Contains(o.String(), "convergence loop started") }) {
			t.Fatalf("no start line:\n%s", o.String())
		}
		for _, l := range strings.Split(o.String(), "\n") {
			if strings.Contains(l, "convergence loop started") {
				return l
			}
		}
		return ""
	}

	line := start("--zone", zone)
	if strings.Contains(line, "state=") || !strings.Contains(line, "zone="+zone) {
		t.Fatalf("[AC-111-5] the start line must show the zone file only, not the unused fake state: %s", line)
	}
	if !strings.Contains(line, "--mode auto;") || strings.Contains(line, "mode=auto") {
		t.Fatalf("[AC-111-5] the start line must say the mode is --mode's value: %s", line)
	}
	if line := start("--zone", zone, "--freeze", "--freeze-reason", "test", "--audit-log", filepath.Join(filepath.Dir(zone), "audit.jsonl")); !strings.Contains(line, "--freeze: alert-only [FROZEN]") {
		t.Fatalf("[AC-111-5] the frozen start line: %s", line)
	}
	if line := start("--state", filepath.Join(tmp, "s.json"), "--zone", zone); !strings.Contains(line, "state="+filepath.Join(tmp, "s.json")+", zone=") {
		t.Fatalf("[AC-111-5] an explicit --state is shown: %s", line)
	}
}

func TestDisplay_OneShotPlannedChangeEventsOnStderr(t *testing.T) {
	// [AC-111-7]
	r := newPCRig(t)
	r.write(nil, map[string]any{"approval": pcApproval("alice")})
	stdout, stderr, _ := runSplit(t, "reconcile", "--intent", r.intentDir, "--state", r.statePath,
		"--max-destructive-per-cycle=1", "--audit-log", r.auditPath)
	if !strings.Contains(stderr, "sakutei reconcile: PlannedChangeOpened: planned_change(scope=") ||
		!strings.Contains(stderr, "sakutei reconcile: PlannedChange: planned_change(scope=") {
		t.Fatalf("[AC-111-7] the lane notices must be on stderr with the verb's prefix:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if strings.Contains(stdout, "PlannedChange") {
		t.Fatalf("[AC-111-7] the lane notices must not be on stdout without a prefix:\n%s", stdout)
	}
}

func TestDisplay_StatusDriftedShowsFields(t *testing.T) {
	// [AC-111-9]
	tmp := t.TempDir()
	intent := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intent, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStateJSON(t, filepath.Join(intent, "i.json"), `{"resources": [
    { "provider": "fake", "id": "web", "fields": { "name": "web", "size": "2" } }
  ]}`)
	state := filepath.Join(tmp, "state.json")
	writeStateJSON(t, state, `{"resources": {"web": {"fields": {"name": "web", "size": 5}}}, "provider_version": "v-test"}`)
	out, code := runSakutei(t, "status", "--intent", intent, "--state", state)
	if code != 0 || !strings.Contains(out, "DRIFTED fake//web") || !strings.Contains(out, "ずれ（現実 → 宣言）:") ||
		!strings.Contains(out, `~ size: "5" → "2"`) {
		t.Fatalf("[AC-111-9] DRIFTED must show the field and its values (exit %d):\n%s", code, out)
	}
	if strings.Contains(out, "name:") {
		t.Fatalf("[AC-111-9] only the fields that are out of sync are listed:\n%s", out)
	}
}

func TestDisplay_PlanSaysRemediationWindow(t *testing.T) {
	// [AC-111-10]
	tmp := t.TempDir()
	state := filepath.Join(tmp, "state.json")
	writeStateJSON(t, state, `{"resources": {"web": {"fields": {"name": "web", "size": 5}}}, "provider_version": "v-test"}`)
	plan := func(mode string) string {
		t.Helper()
		intent := filepath.Join(tmp, "intent-"+mode)
		if err := os.MkdirAll(intent, 0o755); err != nil {
			t.Fatal(err)
		}
		writeStateJSON(t, filepath.Join(intent, "i.json"), `{
  "remediation": { "default": "auto", "schedule": [ { "window": "*", "mode": "`+mode+`" } ] },
  "resources": [ { "provider": "fake", "id": "web", "fields": { "name": "web", "size": "2" } } ]
}`)
		return mustRun(t, "plan", "--intent", intent, "--state", state)
	}
	if out := plan("alert-only"); !strings.Contains(out, "Remediation mode now: alert-only") ||
		!strings.Contains(out, "outside the remediation window: a reconcile now reports these changes but does not apply them") {
		t.Fatalf("[AC-111-10] plan must say a reconcile now would not fix the diff:\n%s", out)
	}
	if out := plan("auto"); !strings.Contains(out, "Remediation mode now: auto") || !strings.Contains(out, "a reconcile now applies these changes") {
		t.Fatalf("[AC-111-10] plan must say a reconcile now would fix the diff:\n%s", out)
	}

	// Without a remediation: declaration, plan says nothing about it.
	intent := filepath.Join(tmp, "intent-none")
	if err := os.MkdirAll(intent, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStateJSON(t, filepath.Join(intent, "i.json"), `{"resources": [ { "provider": "fake", "id": "web", "fields": { "name": "web", "size": "2" } } ]}`)
	if out := mustRun(t, "plan", "--intent", intent, "--state", state); strings.Contains(out, "Remediation mode") {
		t.Fatalf("[AC-111-10] no schedule, no remediation line:\n%s", out)
	}
}
