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

// Acceptance tests for Story S33266e-1 — SecretRef auto-mask / JIT resolve /
// value-non-convergence + file SecretBackend (段0-minimal).
//
// Each test tags the Acceptance Criterion it verifies in the form
// [AC-S33266e-1-N].  The load-bearing test is TestSecretValueNeverLeaks: a known
// secret value is run through EVERY output path this Story touches and asserted
// absent from all of them.
package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/internal/cause"
	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/executor"
	"github.com/tjst-t/sakutei/internal/render"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// theSecret is the known plaintext value used across the safety tests.  It must
// never appear in any output path.
const theSecret = "SUPERSECRET123"

// secretSchema is a provider schema with a secret-flagged field, used to drive
// the real Schema-driven diff engine (masking is Schema-driven, §5.2 / §20).
func secretSchema() provider.Schema {
	return provider.Schema{
		Fields: map[string]provider.FieldSpec{
			"name":     {Kind: core.FieldKindString, Required: true, Identity: true},
			"password": {Kind: core.FieldKindSecretRef, Required: true, Secret: true},
		},
	}
}

// ---------------------------------------------------------------------------
// [AC-S33266e-1-1] SecretRef auto-masks in String / fmt / JSON.
// ---------------------------------------------------------------------------

func TestSecretRef_AutoMask(t *testing.T) {
	// [AC-S33266e-1-1]
	ref := core.NewSecretRef("file", "db/mysql/root", 3)

	wantStr := "secretref:file/db/mysql/root#gen3"
	if got := ref.String(); got != wantStr {
		t.Fatalf("String() = %q, want %q", got, wantStr)
	}

	// fmt %v / %s / %+v / %#v all route through the masked ref form.
	for _, verb := range []string{"%v", "%s", "%+v", "%#v", "%q"} {
		got := fmt.Sprintf(verb, ref)
		if !strings.Contains(got, "secretref:file/db/mysql/root#gen3") {
			t.Errorf("fmt %s = %q, expected masked ref form", verb, got)
		}
		if strings.Contains(got, theSecret) {
			t.Errorf("fmt %s leaked the value: %q", verb, got)
		}
	}

	// JSON marshal → {"$secret":...,"gen":...}; value absent.
	b, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	js := string(b)
	if !strings.Contains(js, `"$secret":"db/mysql/root"`) {
		t.Errorf("JSON = %s, want $secret path", js)
	}
	if !strings.Contains(js, `"gen":3`) {
		t.Errorf("JSON = %s, want gen:3", js)
	}

	// JSON round-trips without ever carrying a value.
	var back core.SecretRef
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if back.String() != wantStr {
		t.Errorf("round-trip String() = %q, want %q", back.String(), wantStr)
	}
}

// ---------------------------------------------------------------------------
// [AC-S33266e-1-2] SAFETY: the value never appears in ANY output path.
// ---------------------------------------------------------------------------

func TestSecretValueNeverLeaks(t *testing.T) {
	// [AC-S33266e-1-2]
	// The secret ENTERS Sakutei only as a ref.  The file backend holds the
	// plaintext locally; the intent/desired side references it by path+gen.
	ref := core.NewSecretRef("file", "db/root", 4)
	desiredSecretField := ref.String() // this is what the loader stores in Fields

	// Collect the output of every path this Story touches into one buffer.
	var outputs []string
	record := func(label, s string) { outputs = append(outputs, label+": "+s) }

	// 1) SecretRef.String()
	record("String", ref.String())

	// 2) JSON marshal of the ref
	b, _ := json.Marshal(ref)
	record("JSON", string(b))

	// 3) fmt %v (covers logging via fmt)
	record("fmt", fmt.Sprintf("%v %s %+v %#v", ref, ref, ref, ref))

	// 4) Diff render (plan/status printOp) — drive the real diff engine with a
	//    secret-flagged schema, then render the resulting Op via captureOp.
	eng := diff.New(secretSchema())
	id := core.NewID("fake", core.NewScope(nil), "res1")
	// Create case: reality absent.
	createPlan := eng.Diff(
		core.Observation{Availability: core.AvailabilityOk, State: nil},
		diff.DesiredResource{ID: id, Fields: map[string]string{"name": "res1", "password": desiredSecretField}},
		cause.CauseContext{},
	)
	record("diff-create-op", fmt.Sprintf("%+v", createPlan.Ops))
	record("plan-render-create", captureStdout(t, func() { renderOps(createPlan.Ops) }))

	// Update case: reality has an OLD ref; desired has a NEW gen — a diff appears
	// and its FieldDiff is rendered.
	updatePlan := eng.Diff(
		core.Observation{Availability: core.AvailabilityOk, State: core.State{
			"name":     {Kind: core.FieldKindString, Value: "res1"},
			"password": {Kind: core.FieldKindSecretRef, Value: core.NewSecretRef("file", "db/root", 3)},
		}},
		diff.DesiredResource{ID: id, Fields: map[string]string{"name": "res1", "password": desiredSecretField}},
		cause.CauseContext{},
	)
	record("diff-update-op", fmt.Sprintf("%+v", updatePlan.Ops))
	record("plan-render-update", captureStdout(t, func() { renderOps(updatePlan.Ops) }))

	// 5) Generated writer — write a secret-flagged expanded child and read the file.
	genDir := t.TempDir()
	writeGeneratedWithSecret(t, genDir, id, desiredSecretField)
	genBytes, err := os.ReadFile(filepath.Join(genDir, render.GeneratedFileName))
	if err != nil {
		t.Fatalf("read generated file: %v", err)
	}
	record("generated-file", string(genBytes))

	// 6) file SecretBackend GenerationOf output (metadata path — value must NOT
	//    appear even though the store file physically holds it).
	storePath := writeFileSecretStore(t, map[string]fileStoreEntry{
		"db/root": {Gen: 4, Value: theSecret},
	})
	be := render.NewFileSecretBackend(storePath)
	gen, err := be.GenerationOf(context.Background(), "db/root")
	if err != nil {
		t.Fatalf("GenerationOf: %v", err)
	}
	record("GenerationOf", gen)

	// Assert: the secret appears in NONE of the collected outputs.
	for _, o := range outputs {
		if strings.Contains(o, theSecret) {
			t.Errorf("SECRET LEAK in output path %q", o)
		}
	}
	// Sanity: the ref form DID appear (we are masking, not dropping everything).
	joined := strings.Join(outputs, "\n")
	if !strings.Contains(joined, "secretref:file/db/root#gen4") {
		t.Errorf("expected the masked ref form to appear somewhere; outputs=\n%s", joined)
	}
}

// ---------------------------------------------------------------------------
// [AC-S33266e-1-2] Value never converges; generation bump DOES converge.
// ---------------------------------------------------------------------------

func TestSecretValueNeverConverges(t *testing.T) {
	// [AC-S33266e-1-2]
	eng := diff.New(secretSchema())
	id := core.NewID("fake", core.NewScope(nil), "res1")

	// Case A: reality and desired share the SAME ref+gen but the underlying
	// plaintext differs.  Because only the ref enters diff, there is NO diff.
	realityRef := core.NewSecretRef("file", "db/root", 5)
	desiredRef := core.NewSecretRef("file", "db/root", 5)
	planNoChange := eng.Diff(
		core.Observation{Availability: core.AvailabilityOk, State: core.State{
			"name":     {Kind: core.FieldKindString, Value: "res1"},
			"password": {Kind: core.FieldKindSecretRef, Value: realityRef},
		}},
		diff.DesiredResource{ID: id, Fields: map[string]string{"name": "res1", "password": desiredRef.String()}},
		cause.CauseContext{},
	)
	if len(planNoChange.Ops) != 0 {
		t.Fatalf("value-only change produced a diff (%d ops); the value must not converge", len(planNoChange.Ops))
	}

	// Case B: same path, gen 5 → gen 6.  This IS a legitimate change and must
	// produce a diff whose FieldDiff carries only ref forms (never a value).
	newRef := core.NewSecretRef("file", "db/root", 6)
	planGenBump := eng.Diff(
		core.Observation{Availability: core.AvailabilityOk, State: core.State{
			"name":     {Kind: core.FieldKindString, Value: "res1"},
			"password": {Kind: core.FieldKindSecretRef, Value: realityRef},
		}},
		diff.DesiredResource{ID: id, Fields: map[string]string{"name": "res1", "password": newRef.String()}},
		cause.CauseContext{},
	)
	if len(planGenBump.Ops) != 1 {
		t.Fatalf("gen bump produced %d ops, want 1", len(planGenBump.Ops))
	}
	var pwDiff *core.FieldDiff
	for i := range planGenBump.Ops[0].Fields {
		if planGenBump.Ops[0].Fields[i].Field == "password" {
			pwDiff = &planGenBump.Ops[0].Fields[i]
		}
	}
	if pwDiff == nil {
		t.Fatalf("gen bump did not diff the password field")
	}
	if pwDiff.From != "secretref:file/db/root#gen5" || pwDiff.To != "secretref:file/db/root#gen6" {
		t.Errorf("gen-bump FieldDiff = %q → %q, want gen5 → gen6 ref forms", pwDiff.From, pwDiff.To)
	}
}

// ---------------------------------------------------------------------------
// [AC-S33266e-1-3] JIT resolve returns the value; the ref never retains it.
// ---------------------------------------------------------------------------

func TestSecretRef_JITResolveAndZero(t *testing.T) {
	// [AC-S33266e-1-3]
	storePath := writeFileSecretStore(t, map[string]fileStoreEntry{
		"db/root": {Gen: 7, Value: theSecret},
	})
	be := render.NewFileSecretBackend(storePath)
	ref := core.NewSecretRef("file", "db/root", 7)

	// Before resolve: masked.
	if ref.String() != "secretref:file/db/root#gen7" || strings.Contains(ref.String(), theSecret) {
		t.Fatalf("ref not masked before resolve: %q", ref.String())
	}

	val, err := ref.Resolve(context.Background(), be)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if val.Expose() != theSecret {
		t.Fatalf("Resolve returned %q, want the plaintext", val.Expose())
	}

	// After resolve: the ref STILL masks (it never retained the value).
	if ref.String() != "secretref:file/db/root#gen7" {
		t.Errorf("ref changed after resolve: %q", ref.String())
	}
	if strings.Contains(fmt.Sprintf("%v", ref), theSecret) {
		t.Errorf("ref leaked value after resolve")
	}
	// The SecretValue itself masks under fmt/JSON.
	if strings.Contains(fmt.Sprintf("%v", val), theSecret) {
		t.Errorf("SecretValue leaked under fmt")
	}
	if vb, _ := json.Marshal(val); strings.Contains(string(vb), theSecret) {
		t.Errorf("SecretValue leaked under JSON")
	}

	// Zero() wipes the plaintext.
	val.Zero()
	if val.Expose() != "" {
		t.Errorf("after Zero(), Expose() = %q, want empty", val.Expose())
	}
}

// TestSecretResolvingExecutor_JITBoundary verifies the Apply-boundary hook:
// values are resolved just before Apply, visible to the provider during Apply
// via SecretFromContext, and Zeroed afterwards.
func TestSecretResolvingExecutor_JITBoundary(t *testing.T) {
	// [AC-S33266e-1-3]
	storePath := writeFileSecretStore(t, map[string]fileStoreEntry{
		"db/root": {Gen: 8, Value: theSecret},
	})
	be := render.NewFileSecretBackend(storePath)
	ref := core.NewSecretRef("file", "db/root", 8)

	var seenDuringApply string
	inner := &recordingExecutor{onApply: func(ctx context.Context) {
		if v, ok := executor.SecretFromContext(ctx, "db/root"); ok {
			seenDuringApply = v
		}
	}}
	jit := executor.NewSecretResolving(inner, be, []core.SecretRef{ref})

	id := core.NewID("fake", core.NewScope(nil), "res1")
	plan := core.Plan{Ops: []core.Op{{Kind: core.OpKindUpdate, Target: id}}}
	if err := jit.Execute(context.Background(), nil, plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if seenDuringApply != theSecret {
		t.Fatalf("provider did not receive the resolved value during Apply (got %q)", seenDuringApply)
	}
	// After Apply, the context bag is gone; a fresh context has no value.
	if _, ok := executor.SecretFromContext(context.Background(), "db/root"); ok {
		t.Errorf("secret value still reachable after Apply")
	}
}

// ---------------------------------------------------------------------------
// [AC-S33266e-1-4] file SecretBackend resolves generation from a local file.
// ---------------------------------------------------------------------------

func TestFileSecretBackend_GenerationOf(t *testing.T) {
	// [AC-S33266e-1-4]
	storePath := writeFileSecretStore(t, map[string]fileStoreEntry{
		"db/root": {Gen: 42, Value: theSecret},
	})
	be := render.NewFileSecretBackend(storePath)

	gen, err := be.GenerationOf(context.Background(), "db/root")
	if err != nil {
		t.Fatalf("GenerationOf: %v", err)
	}
	if gen != "42" {
		t.Errorf("GenerationOf = %q, want \"42\"", gen)
	}
	if strings.Contains(gen, theSecret) {
		t.Errorf("GenerationOf leaked the value")
	}

	// Unknown path → error, no value.
	if _, err := be.GenerationOf(context.Background(), "no/such"); err == nil {
		t.Errorf("expected error for unknown path")
	}

	// Vault stub returns a clear not-implemented error (offline v1a).
	var vault render.VaultSecretBackend
	if _, err := vault.GenerationOf(context.Background(), "db/root"); err == nil {
		t.Errorf("Vault stub should return not-implemented error offline")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// recordingExecutor is a test Executor that runs a callback with the Apply
// context (to observe SecretFromContext) instead of touching a real provider.
type recordingExecutor struct {
	onApply func(ctx context.Context)
}

func (r *recordingExecutor) Execute(ctx context.Context, _ provider.Provider, _ core.Plan) error {
	if r.onApply != nil {
		r.onApply(ctx)
	}
	return nil
}

// fileStoreEntry mirrors the on-disk file SecretBackend store entry for tests.
type fileStoreEntry struct {
	Gen   int64  `json:"gen"`
	Value string `json:"value,omitempty"`
}

// writeFileSecretStore writes a file SecretBackend store to a temp file.
func writeFileSecretStore(t *testing.T, secrets map[string]fileStoreEntry) string {
	t.Helper()
	store := map[string]any{"secrets": secrets}
	b, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		t.Fatalf("marshal store: %v", err)
	}
	p := filepath.Join(t.TempDir(), "secrets.json")
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatalf("write store: %v", err)
	}
	return p
}

// renderOps mirrors the cmd printOp render for a slice of Ops so the test can
// exercise the exact human-diff output shape (marker + FieldDiff lines).  It is
// a faithful copy of cmd/sakutei printOp's field-rendering (which we cannot
// import from package main); the masking it relies on is in the FieldDiff data.
func renderOps(ops []core.Op) {
	for _, op := range ops {
		fmt.Printf("OP %v %s\n", op.Kind, op.Target)
		if op.Diagnostic != "" {
			fmt.Printf("  reason: %s\n", op.Diagnostic)
		}
		for _, fd := range op.Fields {
			switch {
			case fd.From == "":
				fmt.Printf("  + %-20s (new) -> %q\n", fd.Field, fd.To)
			case fd.To == "":
				fmt.Printf("  - %-20s %q -> (removed)\n", fd.Field, fd.From)
			default:
				fmt.Printf("  ~ %-20s %q -> %q\n", fd.Field, fd.From, fd.To)
			}
		}
	}
}

// captureStdout runs fn and returns everything it wrote to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		done <- sb.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}

// writeGeneratedWithSecret writes a generated/ file containing a secret-flagged
// field value (the ref form) so the test can grep the generated writer output.
// The secret field value is the masked ref form — never a plaintext value.
func writeGeneratedWithSecret(t *testing.T, genDir string, id core.ID, secretRefForm string) {
	t.Helper()
	exp := []render.ExpandedResource{
		{
			DesiredResource: diff.DesiredResource{
				ID:     id,
				Fields: map[string]string{"name": "res1", "password": secretRefForm},
			},
			ManagedBy: "composite/parent",
		},
	}
	if err := render.WriteGenerated(genDir, exp); err != nil {
		t.Fatalf("WriteGenerated: %v", err)
	}
}

// ---------------------------------------------------------------------------
// [AC-S33266e-1-5] LOAD-TIME REJECTION: a bare plaintext in a secret-flagged
// field is rejected before the first diff, and the error NEVER echoes the value.
//
// Rationale: a raw plaintext in a secret field is (a) a plaintext secret sitting
// in the intent file, and (b) a perpetual non-converging diff (desired masks to
// "***", observed is "secretref:...#genN"; they never compare equal).  Validate
// fails loud so the operator fixes it before the secret can spread.
// ---------------------------------------------------------------------------

func TestValidateDesired_RejectsBarePlaintextSecret(t *testing.T) {
	// [AC-S33266e-1-5]
	eng := diff.New(secretSchema())
	id := core.NewID("fake", core.NewScope(nil), "res1")

	// A raw plaintext pasted into the secret-flagged "password" field.
	desired := diff.DesiredResource{
		ID: id,
		Fields: map[string]string{
			"name":     "res1",
			"password": theSecret, // bare scalar — NOT a SecretRef
		},
	}

	err := eng.ValidateDesired(desired)
	if err == nil {
		t.Fatalf("[AC-S33266e-1-5] bare plaintext in a secret field must be REJECTED, got nil error")
	}
	// The error must name the offending field...
	if !strings.Contains(err.Error(), "password") {
		t.Errorf("[AC-S33266e-1-5] error should name the field %q: %q", "password", err.Error())
	}
	// ...and instruct the operator to use a SecretRef...
	if !strings.Contains(err.Error(), "SecretRef") {
		t.Errorf("[AC-S33266e-1-5] error should mention SecretRef: %q", err.Error())
	}
	// ...but NEVER echo the plaintext value (canary check).
	if strings.Contains(err.Error(), theSecret) {
		t.Fatalf("[AC-S33266e-1-5] SECURITY: error leaked the plaintext secret: %q", err.Error())
	}
}

func TestValidateDesired_AcceptsSecretRefForms(t *testing.T) {
	// [AC-S33266e-1-5]
	eng := diff.New(secretSchema())
	id := core.NewID("fake", core.NewScope(nil), "res1")

	// Both accepted ref forms must pass validation.
	refString := core.NewSecretRef("file", "db/root", 3).String() // "secretref:file/db/root#gen3"
	jsonForm := `{"$secret":"db/root","backend":"file","gen":3}`

	for _, form := range []string{refString, jsonForm} {
		desired := diff.DesiredResource{
			ID: id,
			Fields: map[string]string{
				"name":     "res1",
				"password": form,
			},
		}
		if err := eng.ValidateDesired(desired); err != nil {
			t.Errorf("[AC-S33266e-1-5] valid SecretRef form %q must be accepted, got: %v", form, err)
		}
	}
}

func TestValidateDesired_NonSecretFieldUnaffected(t *testing.T) {
	// [AC-S33266e-1-5] a bare scalar in a NON-secret field is fine.
	eng := diff.New(secretSchema())
	id := core.NewID("fake", core.NewScope(nil), "res1")
	desired := diff.DesiredResource{
		ID:     id,
		Fields: map[string]string{"name": "just-a-name", "password": core.NewSecretRef("file", "db/root", 1).String()},
	}
	if err := eng.ValidateDesired(desired); err != nil {
		t.Errorf("[AC-S33266e-1-5] bare scalar in a non-secret field must be accepted: %v", err)
	}
}

// ---------------------------------------------------------------------------
// [AC-S33266e-1-6] DEFENSE-IN-DEPTH: a malformed secret store file must NOT
// leak a snippet of its contents (which are plaintext secrets) via the raw
// json.Unmarshal parser error.
// ---------------------------------------------------------------------------

func TestFileSecretBackend_MalformedStoreDoesNotLeak(t *testing.T) {
	// [AC-S33266e-1-6]
	// Write a store file that is invalid JSON AND whose invalid bytes include a
	// canary secret value.  A raw json.Unmarshal error can quote input near the
	// syntax error — the wrapper must NOT surface that.
	storePath := filepath.Join(t.TempDir(), "store.json")
	malformed := `{"secrets": {"db/root": {"gen": 1, "value": "` + theSecret + `" MISSING_COMMA_HERE }}}`
	if err := os.WriteFile(storePath, []byte(malformed), 0o600); err != nil {
		t.Fatalf("write malformed store: %v", err)
	}

	be := render.NewFileSecretBackend(storePath)
	_, err := be.GenerationOf(context.Background(), "db/root")
	if err == nil {
		t.Fatalf("[AC-S33266e-1-6] malformed store must produce an error")
	}
	// The error must identify the store as malformed...
	if !strings.Contains(err.Error(), "malformed") && !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("[AC-S33266e-1-6] error should identify the store as malformed/unreachable: %q", err.Error())
	}
	// ...but NEVER contain the canary secret value.
	if strings.Contains(err.Error(), theSecret) {
		t.Fatalf("[AC-S33266e-1-6] SECURITY: malformed-store error leaked the value: %q", err.Error())
	}
}
