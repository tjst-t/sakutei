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

// registry_test.go contains CLI-level acceptance tests for Story S62dbba-2.
//
// Test discipline: all tests launch the real "sakutei" binary as a subprocess
// and inspect its stdout, stderr, and exit code.  No internal package is
// imported directly; only the binary's observable behaviour is verified.
//
// AC tags map to scenario-S62dbba-2.json and 旧 ROADMAP.json（Backlog に移行済み）:
//
//	[AC-S62dbba-2-M] machine-executable scenario tests (exit code + output).
//
//	AC-S62dbba-2-1 — intent provider="fake" resolves via registry; plan works
//	AC-S62dbba-2-2 — cmd/sakutei/{plan,reconcile,main}.go do NOT import fakeprovider directly
//	AC-S62dbba-2-3 — unknown provider name causes plan to exit 1 with "unknown provider" error
//	AC-S62dbba-2-4 — existing acceptance tests still pass (regression guard)
package acceptance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// AC-S62dbba-2-1: fake provider resolves via registry
// ---------------------------------------------------------------------------

// TestRegistry_FakeProviderResolvesViaRegistry verifies that "sakutei plan"
// with a provider="fake" resource works identically when the provider is
// resolved via the ProviderRegistry (not hardcoded).
//
// [AC-S62dbba-2-M] [AC-S62dbba-2-1]
func TestRegistry_FakeProviderResolvesViaRegistry(t *testing.T) {
	// [AC-S62dbba-2-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	initStateFile(t, statePath)

	// Write intent with provider="fake" (resolved via registry, not hardcoded).
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "registry-vm",
				"fields": map[string]string{
					"name": "registry-vm",
					"size": "5",
				},
			},
		},
	})

	// Run plan: registry resolves "fake" → fakeprovider; empty reality → CREATE diff.
	stdout, stderr, exitCode := runPlan(t, intentDir, statePath)
	_ = stderr
	if exitCode != 0 {
		t.Fatalf("[AC-S62dbba-2-1] sakutei plan exited %d; stderr: %s\nstdout: %s", exitCode, stderr, stdout)
	}
	if !strings.Contains(stdout, "CREATE") {
		t.Errorf("[AC-S62dbba-2-1] expected CREATE in plan output; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "registry-vm") {
		t.Errorf("[AC-S62dbba-2-1] expected resource id 'registry-vm' in plan output; got:\n%s", stdout)
	}
}

// TestRegistry_FakeProviderReconcileViaRegistry verifies that "sakutei reconcile"
// with provider="fake" also works via the registry.
//
// [AC-S62dbba-2-M] [AC-S62dbba-2-1]
func TestRegistry_FakeProviderReconcileViaRegistry(t *testing.T) {
	// [AC-S62dbba-2-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	initStateFile(t, statePath)

	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "registry-vm",
				"fields": map[string]string{
					"name": "registry-vm",
					"size": "5",
				},
			},
		},
	})

	// First reconcile: should create the resource.
	stdout, stderr, exitCode := runReconcile(t, intentDir, statePath)
	if exitCode != 0 {
		t.Fatalf("[AC-S62dbba-2-1] sakutei reconcile (1st) exited %d; stderr: %s\nstdout: %s", exitCode, stderr, stdout)
	}
	if !strings.Contains(stdout, "change(s) applied") {
		t.Errorf("[AC-S62dbba-2-1] expected 'change(s) applied' in output; got:\n%s", stdout)
	}

	// Second reconcile: already converged.
	stdout2, stderr2, exitCode2 := runReconcile(t, intentDir, statePath)
	if exitCode2 != 0 {
		t.Fatalf("[AC-S62dbba-2-1] sakutei reconcile (2nd) exited %d; stderr: %s\nstdout: %s", exitCode2, stderr2, stdout2)
	}
	if !strings.Contains(stdout2, "Converged (no changes)") {
		t.Errorf("[AC-S62dbba-2-1] expected 'Converged (no changes)' on 2nd reconcile; got:\n%s", stdout2)
	}
}

// ---------------------------------------------------------------------------
// AC-S62dbba-2-2: cmd/ does NOT directly import fakeprovider
// ---------------------------------------------------------------------------

// TestRegistry_NoCmdFakeproviderImport verifies that cmd/sakutei source files
// do not contain a direct import of internal/fakeprovider.
//
// The registry/DI layer (pkg/registry) is the only place allowed to import
// fakeprovider (in pkg/registry/default.go).  cmd/ must go through the
// registry seam.
//
// [AC-S62dbba-2-M] [AC-S62dbba-2-2]
func TestRegistry_NoCmdFakeproviderImport(t *testing.T) {
	// [AC-S62dbba-2-2]
	// Find the repo root from the test binary's working directory.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("cannot find repo root: %v", err)
	}

	// Files that must NOT contain the fakeprovider import.
	cmdFiles := []string{
		filepath.Join(root, "cmd", "sakutei", "plan.go"),
		filepath.Join(root, "cmd", "sakutei", "reconcile.go"),
		filepath.Join(root, "cmd", "sakutei", "main.go"),
		filepath.Join(root, "cmd", "sakutei", "providers.go"),
	}

	for _, f := range cmdFiles {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Errorf("[AC-S62dbba-2-2] cannot read %s: %v", f, err)
			continue
		}
		content := string(data)
		if strings.Contains(content, `"github.com/tjst-t/sakutei/internal/fakeprovider"`) {
			t.Errorf("[AC-S62dbba-2-2] %s contains direct import of internal/fakeprovider — must use registry instead", f)
		}
	}

	// Verify that pkg/registry/default.go DOES import fakeprovider (it's the
	// intended registration point).
	defaultGo := filepath.Join(root, "pkg", "registry", "default.go")
	data, err := os.ReadFile(defaultGo)
	if err != nil {
		t.Fatalf("[AC-S62dbba-2-2] cannot read pkg/registry/default.go: %v", err)
	}
	if !strings.Contains(string(data), `"github.com/tjst-t/sakutei/internal/fakeprovider"`) {
		t.Errorf("[AC-S62dbba-2-2] pkg/registry/default.go should import fakeprovider (it's the registration point)")
	}
}

// ---------------------------------------------------------------------------
// AC-S62dbba-2-3: unknown provider name errors with "unknown provider"
// ---------------------------------------------------------------------------

// TestRegistry_UnknownProviderErrors verifies that an intent file with an
// unregistered provider name causes "sakutei plan" to exit 1 with a message
// containing "unknown provider: <name>".
//
// [AC-S62dbba-2-M] [AC-S62dbba-2-3]
func TestRegistry_UnknownProviderErrors(t *testing.T) {
	// [AC-S62dbba-2-3]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	initStateFile(t, statePath)

	// Intent with an unregistered provider name.
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "unknown-provider-xyz",
				"id":       "ghost-resource",
				"fields": map[string]string{
					"name": "ghost-resource",
				},
			},
		},
	})

	// Run plan: should exit 1 with "unknown provider" error.
	bin := buildBinary(t)
	cmd := exec.Command(bin, "plan", "--intent", intentDir, "--state", statePath)
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf
	err := cmd.Run()

	if err == nil {
		t.Fatal("[AC-S62dbba-2-3] sakutei plan should have exited non-zero for unknown provider, but exited 0")
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("[AC-S62dbba-2-3] unexpected error type: %v", err)
	}
	if exitErr.ExitCode() != 1 {
		t.Errorf("[AC-S62dbba-2-3] expected exit code 1, got %d", exitErr.ExitCode())
	}

	stderrStr := stderrBuf.String()
	if !strings.Contains(stderrStr, "unknown provider") {
		t.Errorf("[AC-S62dbba-2-3] expected 'unknown provider' in stderr; got: %q", stderrStr)
	}
	if !strings.Contains(stderrStr, "unknown-provider-xyz") {
		t.Errorf("[AC-S62dbba-2-3] expected provider name 'unknown-provider-xyz' in stderr; got: %q", stderrStr)
	}
}

// TestRegistry_UnknownProviderReconcileErrors verifies the same behaviour for
// "sakutei reconcile" with an unknown provider.
//
// [AC-S62dbba-2-M] [AC-S62dbba-2-3]
func TestRegistry_UnknownProviderReconcileErrors(t *testing.T) {
	// [AC-S62dbba-2-3]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	initStateFile(t, statePath)

	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "not-a-real-provider",
				"id":       "missing-resource",
				"fields": map[string]string{
					"name": "missing-resource",
				},
			},
		},
	})

	bin := buildBinary(t)
	cmd := exec.Command(bin, "reconcile", "--intent", intentDir, "--state", statePath)
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf
	err := cmd.Run()

	if err == nil {
		t.Fatal("[AC-S62dbba-2-3] sakutei reconcile should have exited non-zero for unknown provider")
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("[AC-S62dbba-2-3] unexpected error type: %v", err)
	}
	if exitErr.ExitCode() != 1 {
		t.Errorf("[AC-S62dbba-2-3] expected exit code 1, got %d", exitErr.ExitCode())
	}

	stderrStr := stderrBuf.String()
	if !strings.Contains(stderrStr, "unknown provider") {
		t.Errorf("[AC-S62dbba-2-3] expected 'unknown provider' in stderr; got: %q", stderrStr)
	}
}

// ---------------------------------------------------------------------------
// AC-S62dbba-2-4: regression guard — existing tests still pass
// (The go test ./... invocation in the Makefile covers this; the test here
// is a smoke-test to confirm the binary still builds and runs for basic flows.)
// ---------------------------------------------------------------------------

// TestRegistry_RegressionSmoke verifies that a basic plan + reconcile cycle
// with the "fake" provider still works correctly after the registry introduction.
//
// [AC-S62dbba-2-4]
func TestRegistry_RegressionSmoke(t *testing.T) {
	// [AC-S62dbba-2-4]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	initStateFile(t, statePath)

	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "smoke-resource",
				"fields": map[string]string{
					"name": "smoke-resource",
					"size": "42",
				},
			},
		},
	})

	// Plan: CREATE should be reported.
	stdout, stderr, exitCode := runPlan(t, intentDir, statePath)
	if exitCode != 0 {
		t.Fatalf("[AC-S62dbba-2-4] plan exited %d; stderr=%s stdout=%s", exitCode, stderr, stdout)
	}
	if !strings.Contains(stdout, "CREATE") {
		t.Errorf("[AC-S62dbba-2-4] plan should show CREATE; stdout=%s", stdout)
	}

	// Reconcile: apply the CREATE.
	stdout2, stderr2, exitCode2 := runReconcile(t, intentDir, statePath)
	if exitCode2 != 0 {
		t.Fatalf("[AC-S62dbba-2-4] reconcile exited %d; stderr=%s stdout=%s", exitCode2, stderr2, stdout2)
	}

	// Reconcile again: should converge with no changes.
	stdout3, stderr3, exitCode3 := runReconcile(t, intentDir, statePath)
	if exitCode3 != 0 {
		t.Fatalf("[AC-S62dbba-2-4] 2nd reconcile exited %d; stderr=%s stdout=%s", exitCode3, stderr3, stdout3)
	}
	if !strings.Contains(stdout3, "Converged (no changes)") {
		t.Errorf("[AC-S62dbba-2-4] expected converged on 2nd reconcile; got: %s", stdout3)
	}
}
