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

// allocation_write_back_test.go — Backlog #157: what the allocator hands out
// is written back to a file on the intent side and read there as the source
// of truth; the store is its copy plus the working records (§19.7, §24.5).
//
// Tags:
//   [AC-157-1] commit: reconcile writes the allocations to
//              generated/allocations/<pool>.yaml — plain YAML with
//              schema_version — and commits it to git as the system
//   [AC-157-2] the store is lost: reconcile stops the Apply and points at
//              `sakutei allocator import`; import rebuilds the store from the
//              file (recorded in S4) and the same values come back — nothing
//              renumbered; an import that disagrees with the store writes
//              nothing
//   [AC-157-3] the file and the store disagree: the owner's Apply is stopped
//              and reported (AllocationHeld), plan shows the hold
//   [AC-157-4] review: the value is committed to a review branch and its
//              Apply waits (plan / status: 書き戻しの取り込み待ち) until the
//              branch is taken into the intent; then it goes ahead
//   [AC-157-5] release removes the owner from the file (commit: committed)
//   [AC-157-6] file: the file is written without git

package acceptance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/loader"
)

const writeBackIntent = `pools:
  - name: vlans
    range: "100-199"
    write_back: true
resources:
  - provider: fake
    id: net-a
    fields:
      name: net-a
      vlan:
        from_pool: vlans
  - provider: fake
    id: net-b
    fields:
      name: net-b
      vlan:
        from_pool: vlans
`

type wbRig struct {
	root, intent, state, store, file string
}

// newWBRig lays out <root>/intent/net.yaml; with git it makes <root> a git
// repository with the intent committed.
func newWBRig(t *testing.T, withGit bool) *wbRig {
	t.Helper()
	root := t.TempDir()
	r := &wbRig{
		root: root, intent: filepath.Join(root, "intent"),
		state: filepath.Join(t.TempDir(), "state.json"), store: filepath.Join(t.TempDir(), "alloc.json"),
		file: filepath.Join(root, "generated", "allocations", "vlans.yaml"),
	}
	if err := os.MkdirAll(r.intent, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStateJSON(t, filepath.Join(r.intent, "net.yaml"), writeBackIntent)
	writeStateJSON(t, r.state, `{"resources": {}, "provider_version": "v-test"}`)
	if withGit {
		r.git(t, "init", "-q", "-b", "main")
		r.git(t, "add", "-A")
		r.git(t, "commit", "-q", "-m", "intent")
	}
	return r
}

func (r *wbRig) git(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=person", "-c", "user.email=person@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = r.root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *wbRig) run(t *testing.T, verb string, extra ...string) (string, int) {
	t.Helper()
	return runSakutei(t, append([]string{verb, "--intent", r.intent, "--state", r.state, "--alloc-store", r.store}, extra...)...)
}

func (r *wbRig) readAlloc(t *testing.T) map[string]int {
	t.Helper()
	f, exists, err := loader.ReadAllocationFile(r.file, "vlans")
	if err != nil || !exists {
		t.Fatalf("allocation file %s: exists=%v err=%v", r.file, exists, err)
	}
	out := map[string]int{}
	for _, e := range f.Allocations {
		out[e.Owner] = e.Value
	}
	return out
}

func TestAllocationWriteBack_CommitAndImport(t *testing.T) {
	r := newWBRig(t, true)

	// [AC-157-1]
	out, code := r.run(t, "reconcile")
	if code != 0 {
		t.Fatalf("[AC-157-1] reconcile: exit %d\n%s", code, out)
	}
	got := r.readAlloc(t)
	if got["fake/net-a#vlan"] != 100 || got["fake/net-b#vlan"] != 101 || len(got) != 2 {
		t.Fatalf("[AC-157-1] the file must hold both allocations: %v", got)
	}
	body := readFile(t, r.file)
	for _, want := range []string{"schema_version: 1\n", "pool: \"vlans\"\n", "allocations:\n", "owner: \"fake/net-a#vlan\"", "value: 100", "allocated_at: "} {
		if !strings.Contains(body, want) {
			t.Fatalf("[AC-157-1] the file must be plain YAML with %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "{") || strings.Contains(body, "expires_at") || strings.Contains(body, "generation") {
		t.Fatalf("[AC-157-1] the file must not be the store's JSON:\n%s", body)
	}
	if st := r.git(t, "status", "--porcelain", "--", "generated/allocations"); st != "" {
		t.Fatalf("[AC-157-1] the file must be committed (work tree clean), got:\n%s", st)
	}
	logOut := r.git(t, "log", "-1", "--format=%an <%ae>%n%B")
	if !strings.Contains(logOut, "sakutei-sync <sakutei-sync@localhost>") || !strings.Contains(logOut, "Sakutei-Allocation: vlans") {
		t.Fatalf("[AC-157-1] the commit must be the system's with the trailer:\n%s", logOut)
	}
	commits := r.git(t, "rev-list", "--count", "HEAD")
	if out, code := r.run(t, "reconcile"); code != 0 || !strings.Contains(out, "Converged (no changes)") {
		t.Fatalf("[AC-157-1] re-running must be a no-op: exit %d\n%s", code, out)
	}
	if r.git(t, "rev-list", "--count", "HEAD") != commits {
		t.Fatalf("[AC-157-1] nothing changed: no new commit")
	}

	// [AC-157-2] lose the store.  A new resource must not be handed a value
	// of the file the store no longer knows about.
	if err := os.Remove(r.store); err != nil {
		t.Fatal(err)
	}
	out, code = r.run(t, "reconcile")
	if !strings.Contains(out, "sakutei allocator import") || !strings.Contains(out, "allocation held") {
		t.Fatalf("[AC-157-2] with the store lost reconcile must hold the Apply and point at import: exit %d\n%s", code, out)
	}
	if strings.Contains(readFile(t, r.store), "fake/net-a#vlan") {
		t.Fatalf("[AC-157-2] nothing may be allocated again while the store lacks the file's entries:\n%s", readFile(t, r.store))
	}
	auditLog := filepath.Join(t.TempDir(), "audit.jsonl")
	out, code = runSakutei(t, "allocator", "import", "--intent", r.intent, "--store", r.store, "--audit-log", auditLog)
	if code != 0 || !strings.Contains(out, "pool vlans: imported 2 allocation(s)") {
		t.Fatalf("[AC-157-2] import: exit %d\n%s", code, out)
	}
	if a := readFile(t, auditLog); !strings.Contains(a, `"kind":"allocation.imported"`) || !strings.Contains(a, "imported pool vlans") {
		t.Fatalf("[AC-157-2] the import must be recorded in S4:\n%s", a)
	}
	out, code = r.run(t, "reconcile")
	if code != 0 || !strings.Contains(out, "Converged (no changes)") {
		t.Fatalf("[AC-157-2] after import the same values must come back (nothing to change): exit %d\n%s", code, out)
	}
	st := readFile(t, r.state)
	if !strings.Contains(st, `"vlan": "100"`) || !strings.Contains(st, `"vlan": "101"`) {
		t.Fatalf("[AC-157-2] the values must stay 100 / 101:\n%s", st)
	}
	// Importing again changes nothing; a file that disagrees with the store
	// is refused and nothing is written.
	if out, code := runSakutei(t, "allocator", "import", "--intent", r.intent, "--store", r.store); code != 0 || !strings.Contains(out, "imported 0 allocation(s)") {
		t.Fatalf("[AC-157-2] a second import must add nothing: exit %d\n%s", code, out)
	}
	before := readFile(t, r.store)
	edited := strings.Replace(readFile(t, r.file), "value: 101", "value: 150", 1)
	if err := os.WriteFile(r.file, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, code := runSakutei(t, "allocator", "import", "--intent", r.intent, "--store", r.store); code != 1 || !strings.Contains(out, "refused, nothing written") || !strings.Contains(out, "the file says 150, the store says 101") {
		t.Fatalf("[AC-157-2] a disagreeing import must be refused: exit %d\n%s", code, out)
	}
	if readFile(t, r.store) != before {
		t.Fatalf("[AC-157-2] a refused import must write nothing")
	}
}

func TestAllocationWriteBack_ConflictHoldsApply(t *testing.T) {
	// [AC-157-3]
	r := newWBRig(t, true)
	if out, code := r.run(t, "reconcile"); code != 0 {
		t.Fatalf("reconcile: exit %d\n%s", code, out)
	}
	// The file now says 150 for net-b; the store says 101.  And net-b gets a
	// change of its own, which must not be applied while held.
	edited := strings.Replace(readFile(t, r.file), "value: 101", "value: 150", 1)
	if err := os.WriteFile(r.file, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	writeStateJSON(t, filepath.Join(r.intent, "net.yaml"), strings.Replace(writeBackIntent, "name: net-b", "name: net-b-renamed", 1))

	out, code := r.run(t, "reconcile", "--log-format", "json")
	if !strings.Contains(out, `"kind":"AllocationHeld"`) || !strings.Contains(out, "says 150 for fake/net-b#vlan but the store says 101") {
		t.Fatalf("[AC-157-3] a disagreement must stop the Apply and raise AllocationHeld: exit %d\n%s", code, out)
	}
	st := readFile(t, r.state)
	if strings.Contains(st, "net-b-renamed") || strings.Contains(st, `"vlan": "150"`) {
		t.Fatalf("[AC-157-3] net-b must not be applied while held:\n%s", st)
	}
	if !strings.Contains(st, `"vlan": "100"`) {
		t.Fatalf("[AC-157-3] net-a is not held and stays converged:\n%s", st)
	}
	out, _ = r.run(t, "plan")
	if !strings.Contains(out, "allocation held") || !strings.Contains(out, "fake/net-b#vlan") {
		t.Fatalf("[AC-157-3] plan must show the hold:\n%s", out)
	}
	if got := r.readAlloc(t); got["fake/net-b#vlan"] != 150 {
		t.Fatalf("[AC-157-3] neither side is preferred: the file must stay as the person left it: %v", got)
	}
	if !strings.Contains(readFile(t, r.store), `"value": 101`) {
		t.Fatalf("[AC-157-3] neither side is preferred: the store must keep 101:\n%s", readFile(t, r.store))
	}
}

func TestAllocationWriteBack_ReviewWaitsUntilTakenIn(t *testing.T) {
	// [AC-157-4]
	r := newWBRig(t, true)
	cfg := filepath.Join(t.TempDir(), "write-back.yaml")
	writeStateJSON(t, cfg, "mode: review\nbranch: allocations-review\n")

	out, code := r.run(t, "reconcile", "--alloc-write-back", cfg)
	if !strings.Contains(out, "書き戻しの取り込み待ち") {
		t.Fatalf("[AC-157-4] review: reconcile must hold the Apply awaiting review: exit %d\n%s", code, out)
	}
	if st := readFile(t, r.state); strings.Contains(st, "net-a") {
		t.Fatalf("[AC-157-4] nothing may be applied before the review is taken in:\n%s", st)
	}
	if _, err := os.Stat(r.file); err == nil {
		t.Fatalf("[AC-157-4] review must not write the intent's file itself")
	}
	if st := r.git(t, "status", "--porcelain", "--", "generated/allocations", "intent"); st != "" {
		t.Fatalf("[AC-157-4] review must leave the work tree alone:\n%s", st)
	}
	branchFile := r.git(t, "show", "allocations-review:generated/allocations/vlans.yaml")
	if !strings.Contains(branchFile, "fake/net-a#vlan") || !strings.Contains(branchFile, "value: 101") {
		t.Fatalf("[AC-157-4] the review branch must carry the allocations:\n%s", branchFile)
	}
	for _, verb := range []string{"plan", "status"} {
		out, _ := r.run(t, verb, "--alloc-write-back", cfg)
		if !strings.Contains(out, "書き戻しの取り込み待ち") {
			t.Fatalf("[AC-157-4] %s must show 書き戻しの取り込み待ち:\n%s", verb, out)
		}
	}
	// Running again neither hands out other values nor stacks commits.
	tip := r.git(t, "rev-parse", "allocations-review")
	if out, _ := r.run(t, "reconcile", "--alloc-write-back", cfg); !strings.Contains(out, "書き戻しの取り込み待ち") {
		t.Fatalf("[AC-157-4] still held:\n%s", out)
	}
	if r.git(t, "rev-parse", "allocations-review") != tip {
		t.Fatalf("[AC-157-4] an unchanged review must not commit again")
	}

	// A person takes the review in.
	r.git(t, "merge", "-q", "--ff-only", "allocations-review")
	out, code = r.run(t, "reconcile", "--alloc-write-back", cfg)
	if code != 0 {
		t.Fatalf("[AC-157-4] after the review is taken in reconcile must go ahead: exit %d\n%s", code, out)
	}
	st := readFile(t, r.state)
	if !strings.Contains(st, `"vlan": "100"`) || !strings.Contains(st, `"vlan": "101"`) {
		t.Fatalf("[AC-157-4] the reviewed values must be applied:\n%s", st)
	}
}

func TestAllocationWriteBack_ReleaseRemovesFromFile(t *testing.T) {
	// [AC-157-5]
	r := newWBRig(t, true)
	if out, code := r.run(t, "reconcile"); code != 0 {
		t.Fatalf("reconcile: exit %d\n%s", code, out)
	}
	writeStateJSON(t, filepath.Join(r.intent, "net.yaml"), strings.SplitN(writeBackIntent, "  - provider: fake\n    id: net-b", 2)[0])
	r.git(t, "commit", "-q", "-am", "drop net-b")
	out, code := runSakutei(t, "allocator", "release", "--pool", "vlans", "--owner", "fake/net-b#vlan", "--store", r.store, "--intent", r.intent)
	if code != 0 || !strings.Contains(out, "released value 101") {
		t.Fatalf("[AC-157-5] release: exit %d\n%s", code, out)
	}
	got := r.readAlloc(t)
	if _, ok := got["fake/net-b#vlan"]; ok || got["fake/net-a#vlan"] != 100 {
		t.Fatalf("[AC-157-5] the released owner must leave the file, the other stay: %v", got)
	}
	if st := r.git(t, "status", "--porcelain", "--", "generated/allocations"); st != "" {
		t.Fatalf("[AC-157-5] the removal must be committed:\n%s", st)
	}
	if !strings.Contains(readFile(t, r.store), `"cooldown"`) || !strings.Contains(readFile(t, r.store), `"101"`) {
		t.Fatalf("[AC-157-5] the store keeps the cooldown (a working record):\n%s", readFile(t, r.store))
	}
	if out, code := r.run(t, "reconcile"); code != 0 {
		t.Fatalf("[AC-157-5] reconcile after release: exit %d\n%s", code, out)
	}
}

func TestAllocationWriteBack_FileModeWithoutGit(t *testing.T) {
	// [AC-157-6]
	r := newWBRig(t, false)
	cfg := filepath.Join(t.TempDir(), "write-back.json")
	dir := filepath.Join(t.TempDir(), "allocs")
	writeStateJSON(t, cfg, `{"mode": "file", "path": "`+dir+`"}`)
	r.file = filepath.Join(dir, "vlans.yaml")
	if out, code := r.run(t, "reconcile", "--alloc-write-back", cfg); code != 0 {
		t.Fatalf("[AC-157-6] reconcile: exit %d\n%s", code, out)
	}
	if got := r.readAlloc(t); len(got) != 2 {
		t.Fatalf("[AC-157-6] file mode must write the file: %v", got)
	}
}
