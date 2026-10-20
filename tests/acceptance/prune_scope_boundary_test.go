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

// prune_scope_boundary_test.go — Backlog #38: PruneWithinScope closes only
// the declared scope (§12.2).  Reality in a scope where no resource declares
// PruneWithinScope (e.g. a brownfield scope not yet promoted, §24.2) is
// open-world and is never deleted — by plan, reconcile, or the daemon — even
// without --claim or mode: sync.
//
// Tags:
//   [#38-1] scope A's orphan is pruned; scope B's reality survives (plan,
//           reconcile, daemon)

package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const pruneScopeIntentYAML = `resources:
  - provider: fake
    id: web-1
    scope:
      site: tokyo
    deletion_policy: PruneWithinScope
    fields:
      name: web-1
      size: "2"
`

// syncState holds web-1 and orphan-t in site=tokyo and old-1 in site=legacy.
func setupPruneScope(t *testing.T) (intent, state string) {
	t.Helper()
	root := t.TempDir()
	intent = filepath.Join(root, "intent")
	if err := os.MkdirAll(intent, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStateJSON(t, filepath.Join(intent, "intent.yaml"), pruneScopeIntentYAML)
	state = filepath.Join(root, "state.json")
	writeStateJSON(t, state, syncState)
	return intent, state
}

func TestPruneScopeBoundary_PlanAndReconcile(t *testing.T) {
	// [#38-1]
	intent, state := setupPruneScope(t)

	out := mustRun(t, "plan", "--intent", intent, "--state", state)
	if strings.Contains(out, "old-1") {
		t.Fatalf("[#38-1] plan must not delete reality outside the PruneWithinScope scope:\n%s", out)
	}
	if !strings.Contains(out, "DELETE fake/site=tokyo/orphan-t") {
		t.Fatalf("[#38-1] plan must still prune the orphan inside the scope:\n%s", out)
	}

	mustRun(t, "reconcile", "--intent", intent, "--state", state)
	res := fakeResources(t, state)
	if _, ok := res["old-1"]; !ok {
		t.Fatalf("[#38-1] reconcile deleted site=legacy/old-1 outside the PruneWithinScope scope: %v", res)
	}
	if _, ok := res["orphan-t"]; ok || res["web-1"]["size"] != "2" {
		t.Fatalf("[#38-1] reconcile must still converge and prune site=tokyo: %v", res)
	}
}

func TestPruneScopeBoundary_Daemon(t *testing.T) {
	// [#38-1]
	intent, state := setupPruneScope(t)
	cmd, stdout, stderr := startDaemonWithArgs(t, intent, state, "100ms")
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if !waitForCondition(10*time.Second, func() bool {
		res, ok := tryFakeResources(state)
		_, orphan := res["orphan-t"]
		return ok && !orphan && res["web-1"]["size"] == "2"
	}) {
		t.Fatalf("[#38-1] the daemon must converge and prune site=tokyo:\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	time.Sleep(500 * time.Millisecond) // a few more cycles
	if _, ok := fakeResources(t, state)["old-1"]; !ok {
		t.Fatalf("[#38-1] the daemon deleted site=legacy/old-1 outside the PruneWithinScope scope:\nstderr:\n%s", stderr.String())
	}
}
