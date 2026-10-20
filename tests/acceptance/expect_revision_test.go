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

// expect_revision_test.go — Backlog #46: "sakutei reconcile --expect-revision"
// (§26.6).  The rendered_revision confirmed by plan is passed to reconcile; if
// intent, allocations or inventory moved in between, reconcile Applies nothing
// and exits non-zero.
//
//	[#46-1] plan's rendered_revision → reconcile converges
//	[#46-2] intent changed after plan → nothing applied, non-zero exit
//	[#46-3] inventory changed after plan → nothing applied, non-zero exit
//	[#46-4] allocation changed after plan (pending at plan, allocated by
//	        reconcile) → nothing applied; re-plan then reconcile converges
package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func expectRevisionSetup(t *testing.T) (intentDir, statePath string) {
	t.Helper()
	tmp := t.TempDir()
	intentDir = filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath = filepath.Join(tmp, "state.json")
	initStateFile(t, statePath)
	writeOneResourceIntent(t, intentDir)
	return intentDir, statePath
}

func planRenderedRevision(t *testing.T, args ...string) string {
	t.Helper()
	_, rendered := extractRevisions(mustRun(t, append([]string{"plan"}, args...)...))
	if rendered == "" {
		t.Fatalf("plan printed no rendered_revision (%v)", args)
	}
	return rendered
}

// assertMismatchAbort runs reconcile and checks it exits non-zero with a
// RevisionMismatch and leaves the state file byte-for-byte unchanged.
func assertMismatchAbort(t *testing.T, tag, statePath string, args ...string) {
	t.Helper()
	before := readFile(t, statePath)
	out, code := runSakutei(t, append([]string{"reconcile"}, args...)...)
	if code == 0 || !strings.Contains(out, "RevisionMismatch") {
		t.Fatalf("[%s] a moved rendered_revision must abort with RevisionMismatch (exit %d):\n%s", tag, code, out)
	}
	if after := readFile(t, statePath); after != before {
		t.Fatalf("[%s] nothing may be applied on a mismatch:\nbefore=%s\nafter=%s", tag, before, after)
	}
}

func TestExpectRevision_MatchConverges(t *testing.T) {
	// [#46-1]
	intentDir, statePath := expectRevisionSetup(t)
	rev := planRenderedRevision(t, "--intent", intentDir, "--state", statePath)

	out, code := runSakutei(t, "reconcile", "--intent", intentDir, "--state", statePath, "--expect-revision", rev)
	if code != 0 {
		t.Fatalf("[#46-1] reconcile with plan's rendered_revision must converge (exit %d):\n%s", code, out)
	}
	if !strings.Contains(readFile(t, statePath), `"phase1-vm"`) {
		t.Fatalf("[#46-1] phase1-vm must exist after reconcile")
	}
	if _, got := extractRevisions(out); got != rev {
		t.Fatalf("[#46-1] reconcile's rendered_revision %q must equal plan's %q", got, rev)
	}
}

func TestExpectRevision_IntentChangedAborts(t *testing.T) {
	// [#46-2]
	intentDir, statePath := expectRevisionSetup(t)
	rev := planRenderedRevision(t, "--intent", intentDir, "--state", statePath)

	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{{
			"provider": "fake",
			"id":       "phase1-vm",
			"fields":   map[string]string{"name": "phase1-vm", "size": "8"},
		}},
	})
	assertMismatchAbort(t, "#46-2", statePath, "--intent", intentDir, "--state", statePath, "--expect-revision", rev)
}

func TestExpectRevision_InventoryChangedAborts(t *testing.T) {
	// [#46-3]
	intentDir, statePath := expectRevisionSetup(t)
	rev := planRenderedRevision(t, "--intent", intentDir, "--state", statePath, "--inventory-hash", "inv-h1")

	assertMismatchAbort(t, "#46-3", statePath, "--intent", intentDir, "--state", statePath,
		"--inventory-hash", "inv-h2", "--expect-revision", rev)

	// The same inventory as plan converges.
	if out, code := runSakutei(t, "reconcile", "--intent", intentDir, "--state", statePath,
		"--inventory-hash", "inv-h1", "--expect-revision", rev); code != 0 {
		t.Fatalf("[#46-3] the inventory plan saw must converge (exit %d):\n%s", code, out)
	}
}

func TestExpectRevision_AllocationChangedAborts(t *testing.T) {
	// [#46-4]
	intent, state, store := setupAllocCLI(t)
	rev := planRenderedRevision(t, "--intent", intent, "--state", state, "--alloc-store", store)

	// plan only looks up (all pending); reconcile allocates, so the rendered
	// input differs from what plan showed.
	assertMismatchAbort(t, "#46-4", state, "--intent", intent, "--state", state, "--alloc-store", store, "--expect-revision", rev)

	rev2 := planRenderedRevision(t, "--intent", intent, "--state", state, "--alloc-store", store)
	if rev2 == rev {
		t.Fatalf("[#46-4] re-plan must see the allocations (rendered_revision unchanged %q)", rev)
	}
	if out, code := runSakutei(t, "reconcile", "--intent", intent, "--state", state, "--alloc-store", store, "--expect-revision", rev2); code != 0 {
		t.Fatalf("[#46-4] reconcile with the re-planned rendered_revision must converge (exit %d):\n%s", code, out)
	}
	if !strings.Contains(readFile(t, state), `"db-net"`) {
		t.Fatalf("[#46-4] db-net must exist after reconcile")
	}
}
