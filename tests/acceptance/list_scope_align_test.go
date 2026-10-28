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

// list_scope_align_test.go — #169 (§5.4, §12.2, §12.4): the scope of the IDs a
// provider's List returns is aligned with the declarations before status's
// unmanaged and prune's set-difference match them.
//
// The fake provider lists every resource with the root scope unless the state
// seeds a scope for it (seedStateScopes) — the same as a real provider whose
// settings carry no scope (proxmox-vm without a cluster scope, dnszone without
// --zone-scope, …).  So these tests hold for any provider.
//
// AC tags:
//
//	[AC-169-1]  scoped declarations + --claim: only the undeclared resource is
//	            unmanaged, nothing outside the claim is.
//	[AC-169-2]  without --claim the declared resources are not unmanaged.
//	[AC-169-3]  a provider scope contradicting the declared scope is noted
//	            (exit 3) and prune halts.
//	[AC-169-4]  claimless PruneWithinScope: declared scoped resources are not
//	            deletion candidates.
package acceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// listScopeSetup seeds reality with vm-a, vm-b (declared) and vm-x (made by
// hand, undeclared), all listed with the root scope, and declares vm-a and
// vm-b at scope node=mgmt (plus any extra declarations).
func listScopeSetup(t *testing.T, policy string, extra ...map[string]any) (intentDir, statePath string) {
	t.Helper()
	tmp := t.TempDir()
	intentDir = filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath = filepath.Join(tmp, "state.json")
	reality := map[string]map[string]any{
		"vm-a": {"name": "vm-a", "size": "1"},
		"vm-b": {"name": "vm-b", "size": "1"},
		"vm-x": {"name": "vm-x", "size": "1"},
	}
	for _, e := range extra {
		local := e["id"].(string)
		reality[local] = map[string]any{"name": local, "size": "1"}
	}
	seedStateWithResources(t, statePath, reality)

	resources := []map[string]any{}
	for _, local := range []string{"vm-a", "vm-b"} {
		resources = append(resources, map[string]any{
			"provider":        "fake",
			"id":              local,
			"scope":           map[string]string{"node": "mgmt"},
			"deletion_policy": policy,
			"fields":          map[string]string{"name": local, "size": "1"},
		})
	}
	resources = append(resources, extra...)
	writeIntentFile(t, intentDir, map[string]any{"resources": resources})
	return intentDir, statePath
}

type listScopeStatus struct {
	Complete  bool `json:"complete"`
	Resources []struct {
		ID      string            `json:"id"`
		LocalID string            `json:"local_id"`
		Scope   map[string]string `json:"scope"`
		State   string            `json:"state"`
	} `json:"resources"`
	Undetectable []struct {
		Provider string `json:"provider"`
		Kind     string `json:"kind"`
		Note     string `json:"note"`
	} `json:"unmanaged_undetectable"`
}

func runListScopeStatus(t *testing.T, intentDir, statePath string, args ...string) (listScopeStatus, int, string) {
	t.Helper()
	stdout, stderr, code := runStatusCmd(t, intentDir, statePath, append([]string{"--json"}, args...)...)
	if code != 0 && code != 3 {
		t.Fatalf("status exited %d; stderr:\n%s", code, stderr)
	}
	var st listScopeStatus
	if err := json.Unmarshal([]byte(stdout), &st); err != nil {
		t.Fatalf("status --json: %v\n%s", err, stdout)
	}
	return st, code, stderr
}

// unmanagedIDs returns the composite IDs of the unmanaged resources, sorted.
func (s listScopeStatus) unmanagedIDs() []string {
	var out []string
	for _, r := range s.Resources {
		if r.State == "unmanaged" {
			out = append(out, r.ID)
		}
	}
	sort.Strings(out)
	return out
}

func (s listScopeStatus) undetectableKinds() []string {
	var out []string
	for _, u := range s.Undetectable {
		out = append(out, u.Kind)
	}
	return out
}

func TestListScope_ClaimShowsOnlyUndeclared(t *testing.T) {
	// [AC-169-1]
	intentDir, statePath := listScopeSetup(t, "ExplicitAbsentOnly")

	st, code, stderr := runListScopeStatus(t, intentDir, statePath, "--claim", "node=mgmt")
	if code != 0 {
		t.Fatalf("[AC-169-1] exit %d, want 0; stderr:\n%s", code, stderr)
	}
	// vm-x is undeclared; every declaration of fake is at node=mgmt, so the
	// hand-made vm-x is taken to be in node=mgmt — inside the claim.
	if got := st.unmanagedIDs(); len(got) != 1 || got[0] != "fake/node=mgmt/vm-x" {
		t.Errorf("[AC-169-1] unmanaged = %v, want only fake/node=mgmt/vm-x", got)
	}
	if len(st.Undetectable) != 0 {
		t.Errorf("[AC-169-1] unexpected notes: %+v", st.Undetectable)
	}

	// Outside the claim nothing is unmanaged.
	st, _, _ = runListScopeStatus(t, intentDir, statePath, "--claim", "node=edge")
	if got := st.unmanagedIDs(); len(got) != 0 {
		t.Errorf("[AC-169-1] --claim node=edge: unmanaged = %v, want none", got)
	}
}

func TestListScope_ClaimUndecidedIsNoted(t *testing.T) {
	// [AC-169-1] The provider's declarations have two scopes: whether vm-x is
	// in the claim cannot be decided — noted, never silently hidden.
	intentDir, statePath := listScopeSetup(t, "ExplicitAbsentOnly", map[string]any{
		"provider": "fake",
		"id":       "vm-e",
		"scope":    map[string]string{"node": "edge"},
		"fields":   map[string]string{"name": "vm-e", "size": "1"},
	})

	st, code, _ := runListScopeStatus(t, intentDir, statePath, "--claim", "node=mgmt")
	if code != 0 {
		t.Errorf("[AC-169-1] exit %d, want 0 (an undecided scope is a note, not an observe failure)", code)
	}
	if got := st.unmanagedIDs(); len(got) != 0 {
		t.Errorf("[AC-169-1] unmanaged = %v, want none", got)
	}
	if kinds := st.undetectableKinds(); len(kinds) != 1 || kinds[0] != "scope_undecided" {
		t.Fatalf("[AC-169-1] notes = %+v, want one scope_undecided", st.Undetectable)
	}
	if !strings.Contains(st.Undetectable[0].Note, "scope が決まらない 1 件") {
		t.Errorf("[AC-169-1] note = %q", st.Undetectable[0].Note)
	}

	// Without a claim the undecided vm-x is unmanaged at the root scope.
	st, _, _ = runListScopeStatus(t, intentDir, statePath)
	if got := st.unmanagedIDs(); len(got) != 1 || got[0] != "fake//vm-x" {
		t.Errorf("[AC-169-1] no claim: unmanaged = %v, want only fake//vm-x", got)
	}
}

func TestListScope_NoClaimDeclaredNotUnmanaged(t *testing.T) {
	// [AC-169-2]
	intentDir, statePath := listScopeSetup(t, "ExplicitAbsentOnly")

	st, code, stderr := runListScopeStatus(t, intentDir, statePath)
	if code != 0 {
		t.Fatalf("[AC-169-2] exit %d, want 0; stderr:\n%s", code, stderr)
	}
	if got := st.unmanagedIDs(); len(got) != 1 || got[0] != "fake/node=mgmt/vm-x" {
		t.Errorf("[AC-169-2] unmanaged = %v, want only fake/node=mgmt/vm-x (vm-a, vm-b are declared)", got)
	}
	for _, r := range st.Resources {
		if (r.LocalID == "vm-a" || r.LocalID == "vm-b") && r.State != "conformant" {
			t.Errorf("[AC-169-2] %s state = %s, want conformant", r.ID, r.State)
		}
	}
}

func TestListScope_MismatchNotedAndPruneHalts(t *testing.T) {
	// [AC-169-3] The provider says vm-a is at node=edge, the declaration says
	// node=mgmt.
	intentDir, statePath := listScopeSetup(t, "PruneWithinScope")
	seedStatePruneableCapability(t, statePath)
	seedStateScopes(t, statePath, map[string]map[string]string{"vm-a": {"node": "edge"}})

	st, code, _ := runListScopeStatus(t, intentDir, statePath)
	if code != 3 || st.Complete {
		t.Errorf("[AC-169-3] exit %d complete=%v, want 3/false (like a List error)", code, st.Complete)
	}
	if got := st.unmanagedIDs(); len(got) != 0 {
		t.Errorf("[AC-169-3] unmanaged = %v, want none (nothing derived from a contradicting List)", got)
	}
	if kinds := st.undetectableKinds(); len(kinds) != 1 || kinds[0] != "scope_mismatch" {
		t.Fatalf("[AC-169-3] notes = %+v, want one scope_mismatch", st.Undetectable)
	}
	if n := st.Undetectable[0].Note; !strings.Contains(n, "scope の食い違い") ||
		!strings.Contains(n, "provider は node=edge") || !strings.Contains(n, "宣言は node=mgmt") {
		t.Errorf("[AC-169-3] note = %q", n)
	}

	combined, _, _ := runReconcileCombined(t, intentDir, statePath)
	if !strings.Contains(combined, "prune halted") || !strings.Contains(combined, "scope の食い違い") {
		t.Errorf("[AC-169-3] reconcile must halt prune on the scope mismatch; output:\n%s", combined)
	}
	for _, local := range []string{"vm-a", "vm-b", "vm-x"} {
		if !resourceExistsInState(t, statePath, local) {
			t.Errorf("[AC-169-3] %s was deleted; prune must halt", local)
		}
	}
}

func TestListScope_PruneKeepsDeclaredScoped(t *testing.T) {
	// [AC-169-4] Claimless PruneWithinScope with scoped declarations: the
	// declared ones are never candidates, and the root-scope hand-made vm-x is
	// not inferred into the closed world (prune never infers, 原則3).
	intentDir, statePath := listScopeSetup(t, "PruneWithinScope")
	seedStatePruneableCapability(t, statePath)

	for _, args := range [][]string{nil, {"--claim", "node=mgmt"}} {
		combined, _, code := runReconcileCombined(t, intentDir, statePath, args...)
		if code != 0 {
			t.Fatalf("[AC-169-4] reconcile %v exited %d; output:\n%s", args, code, combined)
		}
		if strings.Contains(combined, "DELETE") || strings.Contains(combined, "prune halted") {
			t.Errorf("[AC-169-4] reconcile %v: no delete, no halt expected; output:\n%s", args, combined)
		}
		for _, local := range []string{"vm-a", "vm-b", "vm-x"} {
			if !resourceExistsInState(t, statePath, local) {
				t.Fatalf("[AC-169-4] reconcile %v deleted %s", args, local)
			}
		}
	}
}
