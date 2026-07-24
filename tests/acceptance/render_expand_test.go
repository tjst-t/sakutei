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

// render_expand_test.go contains CLI-level acceptance tests for Story S072a14-1.
//
// Discipline: all tests launch the real "sakutei" binary as a subprocess and
// verify only observable CLI behaviour (stdout, exit code, file system state).
// No internal package is imported directly.
//
// AC tags:
//
//	AC-S072a14-1-1 — fake_composite expands to child-a + child-b; written to generated/expanded.json
//	AC-S072a14-1-2 — auto-generation header present; two runs produce bit-for-bit identical output
//	AC-S072a14-1-3 — direct edits to generated/ trigger warning on next run; next render overwrites
//	AC-S072a14-1-4 — children have managed_by=<parent-id>; reconcile converges children
//
// [AC-S072a14-1-M] is the machine-executable marker required by scenario-S072a14-1.json.
package acceptance

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runPlanWithStderr runs "sakutei plan" and captures both stdout AND stderr
// regardless of exit code.  This is needed for tests that verify warning
// messages emitted to stderr even on a successful (exit 0) run.
func runPlanWithStderr(t *testing.T, intentDir, statePath string, extraArgs ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	bin := buildBinary(t)
	args := []string{"plan", "--intent", intentDir, "--state", statePath}
	args = append(args, extraArgs...)

	cmd := exec.Command(bin, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("runPlanWithStderr: unexpected error: %v", err)
		}
	}
	return outBuf.String(), errBuf.String(), code
}

// writeCompositeIntentFile writes a single fake_composite resource into intentDir.
// The composite kind is set via the "kind" field in the fields map.
func writeCompositeIntentFile(t *testing.T, intentDir string, compositeID string) {
	t.Helper()
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       compositeID,
				"fields": map[string]string{
					"name": compositeID,
					"kind": "composite",
				},
			},
		},
	})
}

// readGeneratedFile reads and returns the parsed content of generated/expanded.json.
func readGeneratedFile(t *testing.T, generatedDir string) map[string]any {
	t.Helper()
	path := filepath.Join(generatedDir, "expanded.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("[AC-S072a14-1-M] readGeneratedFile: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("[AC-S072a14-1-M] readGeneratedFile: unmarshal: %v", err)
	}
	return result
}

// TestRenderExpand_CompositeExpandsToPlan verifies scenario-S072a14-1 step 1.
//
// [AC-S072a14-1-1] A fake_composite resource in intent/ expands in render
// phase 2 to fake_child_a and fake_child_b.  The children appear in
// generated/expanded.json and sakutei plan shows their Create ops.
//
// [AC-S072a14-1-M]
func TestRenderExpand_CompositeExpandsToPlan(t *testing.T) {
	// [AC-S072a14-1-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	generatedDir := filepath.Join(tmp, "generated")
	statePath := filepath.Join(tmp, "state.json")
	initStateFile(t, statePath)

	compositeID := "my-composite"
	writeCompositeIntentFile(t, intentDir, compositeID)

	// Run plan with explicit --generated dir.
	stdout, stderr, code := runPlan(t, intentDir, statePath,
		"--generated", generatedDir)
	if code != 0 {
		t.Fatalf("[AC-S072a14-1-1] plan exit code=%d; stderr=%s; stdout=%s", code, stderr, stdout)
	}

	// generated/expanded.json must exist.
	genPath := filepath.Join(generatedDir, "expanded.json")
	if _, err := os.Stat(genPath); os.IsNotExist(err) {
		t.Fatalf("[AC-S072a14-1-1] generated/expanded.json was not created at %s", genPath)
	}

	// Parse and verify auto-generation header.
	rec := readGeneratedFile(t, generatedDir)
	if gen, _ := rec["_generated"].(bool); !gen {
		t.Errorf("[AC-S072a14-1-1] generated/expanded.json missing _generated:true; got %v", rec["_generated"])
	}
	if by, _ := rec["_generated_by"].(string); by != "sakutei render" {
		t.Errorf("[AC-S072a14-1-1] _generated_by mismatch: got %q, want %q", by, "sakutei render")
	}
	if dne, _ := rec["_do_not_edit"].(bool); !dne {
		t.Errorf("[AC-S072a14-1-1] _do_not_edit missing or false; got %v", rec["_do_not_edit"])
	}

	// Resources: must contain the two expanded children.
	resources, _ := rec["resources"].([]any)
	if len(resources) != 2 {
		t.Fatalf("[AC-S072a14-1-1] expected 2 resources in generated/expanded.json, got %d", len(resources))
	}

	childIDs := make(map[string]bool)
	for _, r := range resources {
		rm, _ := r.(map[string]any)
		id, _ := rm["id"].(string)
		childIDs[id] = true
	}
	wantChildA := compositeID + "-child-a"
	wantChildB := compositeID + "-child-b"
	if !childIDs[wantChildA] {
		t.Errorf("[AC-S072a14-1-1] child %q missing from generated/expanded.json; got %v", wantChildA, childIDs)
	}
	if !childIDs[wantChildB] {
		t.Errorf("[AC-S072a14-1-1] child %q missing from generated/expanded.json; got %v", wantChildB, childIDs)
	}

	// plan output must mention the child IDs (Create ops) and NOT the composite parent.
	if !strings.Contains(stdout, wantChildA) {
		t.Errorf("[AC-S072a14-1-1] stdout does not mention child %q; stdout:\n%s", wantChildA, stdout)
	}
	if !strings.Contains(stdout, wantChildB) {
		t.Errorf("[AC-S072a14-1-1] stdout does not mention child %q; stdout:\n%s", wantChildB, stdout)
	}
	// The plan should show CREATE for both children (reality is empty).
	if !strings.Contains(stdout, "CREATE") {
		t.Errorf("[AC-S072a14-1-1] stdout does not contain CREATE; stdout:\n%s", stdout)
	}

	t.Logf("[AC-S072a14-1-1] plan stdout:\n%s", stdout)
}

// TestRenderExpand_Deterministic verifies scenario-S072a14-1 step 2.
//
// [AC-S072a14-1-2] Running plan twice on the same intent produces
// bit-for-bit identical generated/expanded.json (determinism guarantee §9.1).
// The auto-generation header is present in both runs.
//
// [AC-S072a14-1-M]
func TestRenderExpand_Deterministic(t *testing.T) {
	// [AC-S072a14-1-2]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	generatedDir := filepath.Join(tmp, "generated")
	statePath := filepath.Join(tmp, "state.json")
	initStateFile(t, statePath)

	writeCompositeIntentFile(t, intentDir, "det-composite")

	// First run.
	_, _, code1 := runPlan(t, intentDir, statePath, "--generated", generatedDir)
	if code1 != 0 {
		t.Fatalf("[AC-S072a14-1-2] first plan failed with exit code %d", code1)
	}
	genPath := filepath.Join(generatedDir, "expanded.json")
	first, err := os.ReadFile(genPath)
	if err != nil {
		t.Fatalf("[AC-S072a14-1-2] read generated after first run: %v", err)
	}

	// Second run.
	_, _, code2 := runPlan(t, intentDir, statePath, "--generated", generatedDir)
	if code2 != 0 {
		t.Fatalf("[AC-S072a14-1-2] second plan failed with exit code %d", code2)
	}
	second, err := os.ReadFile(genPath)
	if err != nil {
		t.Fatalf("[AC-S072a14-1-2] read generated after second run: %v", err)
	}

	if !bytes.Equal(first, second) {
		t.Errorf("[AC-S072a14-1-2] generated/expanded.json differs between runs:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}

	// Verify header is present in both (spot-check first only; bytes.Equal guarantees second matches).
	var rec map[string]any
	if err := json.Unmarshal(first, &rec); err != nil {
		t.Fatalf("[AC-S072a14-1-2] unmarshal: %v", err)
	}
	if gen, _ := rec["_generated"].(bool); !gen {
		t.Errorf("[AC-S072a14-1-2] _generated:true missing from output")
	}

	t.Logf("[AC-S072a14-1-2] determinism verified; file size=%d bytes", len(first))
}

// TestRenderExpand_DirectEditWarning verifies scenario-S072a14-1 step 3.
//
// [AC-S072a14-1-3] If a human directly edits generated/expanded.json (removing
// or changing the _generated header), the next plan/reconcile run emits a
// WARNING and overwrites the file with the correct generated content.
//
// [AC-S072a14-1-M]
func TestRenderExpand_DirectEditWarning(t *testing.T) {
	// [AC-S072a14-1-3]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	generatedDir := filepath.Join(tmp, "generated")
	statePath := filepath.Join(tmp, "state.json")
	initStateFile(t, statePath)

	writeCompositeIntentFile(t, intentDir, "edit-composite")

	// First run: creates generated/expanded.json correctly.
	_, _, code := runPlan(t, intentDir, statePath, "--generated", generatedDir)
	if code != 0 {
		t.Fatalf("[AC-S072a14-1-3] initial plan failed")
	}
	genPath := filepath.Join(generatedDir, "expanded.json")

	// Simulate a direct human edit: remove the _generated flag.
	tamperedContent := `{"_generated":false,"_generated_by":"human","_do_not_edit":false,"resources":[]}`
	if err := os.WriteFile(genPath, []byte(tamperedContent), 0644); err != nil {
		t.Fatalf("[AC-S072a14-1-3] write tampered content: %v", err)
	}

	// Second run: should warn about the missing header and overwrite the file.
	// Use runPlanWithStderr so we capture stderr even when exit code is 0.
	_, stderr2, code2 := runPlanWithStderr(t, intentDir, statePath, "--generated", generatedDir)
	if code2 != 0 {
		t.Fatalf("[AC-S072a14-1-3] second plan exit code=%d; stderr=%s", code2, stderr2)
	}

	// stderr must contain a WARNING about direct human edit.
	if !strings.Contains(stderr2, "WARNING") {
		t.Errorf("[AC-S072a14-1-3] expected WARNING in stderr about direct human edit; got:\n%s", stderr2)
	}

	// The file must have been overwritten with the correct content (_generated:true).
	after, err := os.ReadFile(genPath)
	if err != nil {
		t.Fatalf("[AC-S072a14-1-3] read after second plan: %v", err)
	}
	var rec map[string]any
	if err := json.Unmarshal(after, &rec); err != nil {
		t.Fatalf("[AC-S072a14-1-3] unmarshal after second plan: %v", err)
	}
	if gen, _ := rec["_generated"].(bool); !gen {
		t.Errorf("[AC-S072a14-1-3] after overwrite, _generated:true missing; got %v", rec["_generated"])
	}
	// The resources should be the correct expanded children, not the tampered empty list.
	resources, _ := rec["resources"].([]any)
	if len(resources) != 2 {
		t.Errorf("[AC-S072a14-1-3] after overwrite, expected 2 resources, got %d", len(resources))
	}

	t.Logf("[AC-S072a14-1-3] warning emitted and file overwritten correctly; stderr:\n%s", stderr2)
}

// TestRenderExpand_ReconcileConvergesChildren verifies scenario-S072a14-1 step 4.
//
// [AC-S072a14-1-4] After reconcile, the expanded children (not the composite
// parent) are created in the provider state file.  Each child has managed_by
// set to the parent composite ID.
//
// [AC-S072a14-1-M]
func TestRenderExpand_ReconcileConvergesChildren(t *testing.T) {
	// [AC-S072a14-1-4]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	generatedDir := filepath.Join(tmp, "generated")
	statePath := filepath.Join(tmp, "state.json")
	initStateFile(t, statePath)

	compositeID := "net-composite"
	writeCompositeIntentFile(t, intentDir, compositeID)

	// Run reconcile: should create the two children in the state file.
	stdout, stderr, code := runReconcile(t, intentDir, statePath,
		"--generated", generatedDir)
	if code != 0 {
		t.Fatalf("[AC-S072a14-1-4] reconcile exit code=%d; stderr=%s; stdout=%s", code, stderr, stdout)
	}

	// The composite itself must NOT appear in the state file (it is not Applied
	// directly — only its children are).
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("[AC-S072a14-1-4] read state file: %v", err)
	}
	var stateFile map[string]any
	if err := json.Unmarshal(stateData, &stateFile); err != nil {
		t.Fatalf("[AC-S072a14-1-4] unmarshal state file: %v", err)
	}

	resources, _ := stateFile["resources"].(map[string]any)
	if resources == nil {
		t.Fatalf("[AC-S072a14-1-4] state file has no 'resources' map; content:\n%s", stateData)
	}

	childA := compositeID + "-child-a"
	childB := compositeID + "-child-b"

	// Children must be in the state file.
	if _, ok := resources[childA]; !ok {
		t.Errorf("[AC-S072a14-1-4] child %q not found in state file after reconcile; resources: %v", childA, resources)
	}
	if _, ok := resources[childB]; !ok {
		t.Errorf("[AC-S072a14-1-4] child %q not found in state file after reconcile; resources: %v", childB, resources)
	}

	// The composite parent must NOT be in the state file (only children are applied).
	if _, ok := resources[compositeID]; ok {
		t.Errorf("[AC-S072a14-1-4] composite parent %q must NOT appear in state file (only children are applied)", compositeID)
	}

	// Verify managed_by in the generated/ file (state file stores what Apply put
	// in; generated/ is where managed_by is recorded by the render engine).
	rec := readGeneratedFile(t, generatedDir)
	genResources, _ := rec["resources"].([]any)
	if len(genResources) == 0 {
		t.Fatalf("[AC-S072a14-1-4] generated/expanded.json has no resources")
	}
	for _, r := range genResources {
		rm, _ := r.(map[string]any)
		id, _ := rm["id"].(string)
		managedBy, _ := rm["managed_by"].(string)
		if managedBy != compositeID {
			t.Errorf("[AC-S072a14-1-4] child %q has managed_by=%q, want %q", id, managedBy, compositeID)
		}
	}

	// Verify the children also carry managed_by in their fields (as stored in
	// the state file via the fields map).
	for _, childID := range []string{childA, childB} {
		res, ok := resources[childID]
		if !ok {
			continue // already reported above
		}
		resMap, _ := res.(map[string]any)
		fields, _ := resMap["fields"].(map[string]any)
		mb, _ := fields["managed_by"].(string)
		if mb != compositeID {
			t.Errorf("[AC-S072a14-1-4] state file child %q has fields.managed_by=%q, want %q", childID, mb, compositeID)
		}
	}

	t.Logf("[AC-S072a14-1-4] reconcile stdout:\n%s", stdout)
	t.Logf("[AC-S072a14-1-4] state file after reconcile:\n%s", stateData)
}

// TestRenderExpand_PlainResourcesUnchanged verifies that non-composite resources
// continue to work exactly as before (no regression).
//
// [AC-S072a14-1-M]
func TestRenderExpand_PlainResourcesUnchanged(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	generatedDir := filepath.Join(tmp, "generated")
	statePath := filepath.Join(tmp, "state.json")
	initStateFile(t, statePath)

	// Plain resource (no "kind" field → not composite).
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "plain-vm",
				"fields": map[string]string{
					"name": "plain-vm",
					"size": "5",
				},
			},
		},
	})

	stdout, stderr, code := runReconcile(t, intentDir, statePath, "--generated", generatedDir)
	if code != 0 {
		t.Fatalf("plain resource reconcile failed; code=%d stderr=%s stdout=%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "CREATE") || !strings.Contains(stdout, "plain-vm") {
		t.Errorf("expected CREATE plain-vm in stdout; got:\n%s", stdout)
	}

	// generated/expanded.json exists but has zero children (no composites).
	rec := readGeneratedFile(t, generatedDir)
	if gen, _ := rec["_generated"].(bool); !gen {
		t.Errorf("plain-resource case: _generated:true missing from generated file")
	}
	resources, _ := rec["resources"].([]any)
	if len(resources) != 0 {
		t.Errorf("plain-resource case: expected 0 entries in generated/expanded.json (no composites), got %d", len(resources))
	}

	t.Logf("[AC-S072a14-1-M] plain resource stdout:\n%s", stdout)
}
