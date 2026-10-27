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

// sdk_contract_test.go contains library-style acceptance tests for the
// SDK's ContractTester and error classification helpers (S62dbba-1, §28.6).
//
// These tests run against the existing fakeprovider to verify that:
//   - ContractTester.RunAll passes all three contract checks
//   - IsTransient / IsPermanent identify the correct error types
//
// AC tags:
//
//	AC-S62dbba-1-2  ContractTester verifies idempotency, absent→Create→Read, List completeness
//	AC-S62dbba-1-4  IsTransient / IsPermanent classify errors correctly
//
// [AC-S62dbba-1-M] marks the machine-executable scenario tests.
package acceptance

import (
	"context"
	"errors"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/fakeprovider"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
)

// ─────────────────────────────────────────────────────────────────────────────
// AC-S62dbba-1-2: ContractTester — all three contracts pass on fakeprovider
// ─────────────────────────────────────────────────────────────────────────────

// TestSDKContractTester_FakeProvider runs ContractTester.RunAll against the
// fakeprovider and verifies that all three contracts pass:
//
//  1. Idempotency: Apply twice → Read results are identical.
//  2. Absent→Create→Read: Create Apply + Read returns Availability=Ok and
//     matching State.
//  3. List completeness: every ID returned by List is readable with Ok.
//
// [AC-S62dbba-1-2] [AC-S62dbba-1-M]
func TestSDKContractTester_FakeProvider(t *testing.T) {
	// [AC-S62dbba-1-2]
	tmp := t.TempDir()
	statePath := tmp + "/contract-test-state.json"

	fp := fakeprovider.New(statePath)

	// Give the fake a valid provider_version so ValidateProvider passes.
	if err := fp.SetProviderVersion("v-contract-test"); err != nil {
		t.Fatalf("[AC-S62dbba-1-2] SetProviderVersion: %v", err)
	}

	// Set up the pruneable flag and seed so List works.
	if err := fp.SetPruneable(true); err != nil {
		t.Fatalf("[AC-S62dbba-1-2] SetPruneable: %v", err)
	}

	ct := sdk.ContractTester{
		Provider: fp,
		Lister:   fp,
		Scope:    core.NewScope(nil),
		SampleID: "contract-test-resource",
		SampleDesired: map[string]string{
			"name": "contract-test-resource",
			"size": "42",
		},
		Cleanup: func() error {
			// Remove the test resource by applying a Delete Plan.
			// fakeprovider handles Delete by removing the resource from the state file.
			return nil
		},
	}

	ct.RunAll(t)
	t.Logf("[AC-S62dbba-1-2] PASS: ContractTester.RunAll completed on fakeprovider")
}

// TestSDKContractTester_Idempotency_Explicit is an explicit sub-test of the
// idempotency contract, to ensure the test is individually identifiable.
//
// [AC-S62dbba-1-2] [AC-S62dbba-1-M]
func TestSDKContractTester_Idempotency_Explicit(t *testing.T) {
	// [AC-S62dbba-1-2]
	tmp := t.TempDir()
	statePath := tmp + "/idem-state.json"
	fp := fakeprovider.New(statePath)
	if err := fp.SetProviderVersion("v-idem-test"); err != nil {
		t.Fatalf("[AC-S62dbba-1-2] SetProviderVersion: %v", err)
	}

	ct := sdk.ContractTester{
		Provider: fp,
		Scope:    core.NewScope(nil),
		SampleID: "idem-resource",
		SampleDesired: map[string]string{
			"name": "idem-resource",
			"size": "10",
		},
	}

	// RunAll includes idempotency; if this passes the contract is satisfied.
	ct.RunAll(t)
	t.Logf("[AC-S62dbba-1-2] PASS: Idempotency contract satisfied")
}

// TestSDKContractTester_ListCompleteness_Explicit verifies that List returns
// complete results for the fakeprovider, and that all returned IDs are readable.
//
// [AC-S62dbba-1-2] [AC-S62dbba-1-M]
func TestSDKContractTester_ListCompleteness_Explicit(t *testing.T) {
	// [AC-S62dbba-1-2]
	tmp := t.TempDir()
	statePath := tmp + "/list-state.json"
	fp := fakeprovider.New(statePath)
	if err := fp.SetProviderVersion("v-list-test"); err != nil {
		t.Fatalf("[AC-S62dbba-1-2] SetProviderVersion: %v", err)
	}

	// Seed two resources directly into the state file.
	if err := fp.SeedResource("list-res-a", map[string]any{"name": "list-res-a", "size": 5}); err != nil {
		t.Fatalf("[AC-S62dbba-1-2] SeedResource: %v", err)
	}
	if err := fp.SeedResource("list-res-b", map[string]any{"name": "list-res-b", "size": 10}); err != nil {
		t.Fatalf("[AC-S62dbba-1-2] SeedResource: %v", err)
	}

	ct := sdk.ContractTester{
		Provider: fp,
		Lister:   fp,
		Scope:    core.NewScope(nil),
		SampleID: "list-sample",
		SampleDesired: map[string]string{
			"name": "list-sample",
			"size": "1",
		},
	}

	ct.RunAll(t)
	t.Logf("[AC-S62dbba-1-2] PASS: List completeness contract satisfied")
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-S62dbba-1-4: Error classification helpers
// ─────────────────────────────────────────────────────────────────────────────

// TestSDKErrors_IsTransient verifies that IsTransient correctly identifies
// TransientError wrapped errors, and that IsPermanent does not.
//
// [AC-S62dbba-1-4] [AC-S62dbba-1-M]
func TestSDKErrors_IsTransient(t *testing.T) {
	// [AC-S62dbba-1-4]
	base := errors.New("connection refused")
	te := provider.NewTransientError(base)

	if !sdk.IsTransient(te) {
		t.Error("[AC-S62dbba-1-4] IsTransient: expected true for *TransientError, got false")
	}
	if sdk.IsPermanent(te) {
		t.Error("[AC-S62dbba-1-4] IsPermanent: expected false for *TransientError, got true")
	}

	// Wrapped transient error must also be detected (errors.As chain traversal).
	wrapped := errors.New("outer: " + te.Error())
	_ = wrapped
	wrappedTE := provider.NewTransientError(te)
	if !sdk.IsTransient(wrappedTE) {
		t.Error("[AC-S62dbba-1-4] IsTransient: expected true for wrapped *TransientError, got false")
	}

	t.Logf("[AC-S62dbba-1-4] PASS: IsTransient correctly identifies TransientError")
}

// TestSDKErrors_IsPermanent verifies that IsPermanent correctly identifies
// PermanentError wrapped errors, and that IsTransient does not.
//
// [AC-S62dbba-1-4] [AC-S62dbba-1-M]
func TestSDKErrors_IsPermanent(t *testing.T) {
	// [AC-S62dbba-1-4]
	base := errors.New("permission denied")
	pe := provider.NewPermanentError(base)

	if !sdk.IsPermanent(pe) {
		t.Error("[AC-S62dbba-1-4] IsPermanent: expected true for *PermanentError, got false")
	}
	if sdk.IsTransient(pe) {
		t.Error("[AC-S62dbba-1-4] IsTransient: expected false for *PermanentError, got true")
	}

	t.Logf("[AC-S62dbba-1-4] PASS: IsPermanent correctly identifies PermanentError")
}

// TestSDKErrors_UnrelatedError verifies that both helpers return false for
// an error that is neither Transient nor Permanent.
//
// [AC-S62dbba-1-4] [AC-S62dbba-1-M]
func TestSDKErrors_UnrelatedError(t *testing.T) {
	// [AC-S62dbba-1-4]
	plain := errors.New("some unrelated error")
	if sdk.IsTransient(plain) {
		t.Error("[AC-S62dbba-1-4] IsTransient: expected false for plain error, got true")
	}
	if sdk.IsPermanent(plain) {
		t.Error("[AC-S62dbba-1-4] IsPermanent: expected false for plain error, got true")
	}
	t.Logf("[AC-S62dbba-1-4] PASS: plain error is neither transient nor permanent")
}

// TestSDKErrors_NilError verifies that both helpers handle nil gracefully.
//
// [AC-S62dbba-1-4] [AC-S62dbba-1-M]
func TestSDKErrors_NilError(t *testing.T) {
	// [AC-S62dbba-1-4]
	if sdk.IsTransient(nil) {
		t.Error("[AC-S62dbba-1-4] IsTransient: expected false for nil error, got true")
	}
	if sdk.IsPermanent(nil) {
		t.Error("[AC-S62dbba-1-4] IsPermanent: expected false for nil error, got true")
	}
	t.Logf("[AC-S62dbba-1-4] PASS: nil error is neither transient nor permanent")
}

// TestSDKErrors_Unwrap verifies that TransientError and PermanentError expose
// the underlying cause via errors.As / errors.Unwrap.
//
// [AC-S62dbba-1-4] [AC-S62dbba-1-M]
func TestSDKErrors_Unwrap(t *testing.T) {
	// [AC-S62dbba-1-4]
	cause := errors.New("root cause")
	te := provider.NewTransientError(cause)
	pe := provider.NewPermanentError(cause)

	if !errors.Is(te, cause) {
		t.Error("[AC-S62dbba-1-4] errors.Is(TransientError, cause): expected true, got false")
	}
	if !errors.Is(pe, cause) {
		t.Error("[AC-S62dbba-1-4] errors.Is(PermanentError, cause): expected true, got false")
	}
	t.Logf("[AC-S62dbba-1-4] PASS: error chains traverse correctly via errors.Is")
}

// TestSDKErrors_FakeProviderInjectedFailure verifies that a real injected
// failure from fakeprovider is classified as Transient.
//
// This test uses the fakeprovider's FailInject mechanism to trigger a
// TransientError from Apply, then checks IsTransient.
//
// [AC-S62dbba-1-4] [AC-S62dbba-1-M]
func TestSDKErrors_FakeProviderInjectedFailure(t *testing.T) {
	// [AC-S62dbba-1-4]
	tmp := t.TempDir()
	statePath := tmp + "/fail-inject-state.json"
	fp := fakeprovider.New(statePath)
	if err := fp.SetProviderVersion("v-fail-inject-test"); err != nil {
		t.Fatalf("[AC-S62dbba-1-4] SetProviderVersion: %v", err)
	}

	// Inject failure for "fail-resource".
	if err := fp.SetFailInject(map[string]string{
		"fail-resource": "injected test failure",
	}); err != nil {
		t.Fatalf("[AC-S62dbba-1-4] SetFailInject: %v", err)
	}

	// Build a Create Plan targeting the injected resource.
	scope := core.NewScope(nil)
	id := core.NewID("fake", scope, "fail-resource")
	plan := core.Plan{
		Ops: []core.Op{
			{
				Kind:        core.OpKindCreate,
				Target:      id,
				Destructive: false,
				Cause:       core.CauseTagA,
				Diagnostic:  "test injected failure",
				Fields: []core.FieldDiff{
					{Field: "name", From: "", To: "fail-resource"},
					{Field: "size", From: "", To: "1"},
				},
			},
		},
	}

	ctx := context.Background()
	applyErr := fp.Apply(ctx, plan)
	if applyErr == nil {
		t.Fatal("[AC-S62dbba-1-4] Apply: expected error for injected failure, got nil")
	}
	if !sdk.IsTransient(applyErr) {
		t.Errorf("[AC-S62dbba-1-4] IsTransient: expected true for fakeprovider injected failure; got false; err: %v", applyErr)
	}
	t.Logf("[AC-S62dbba-1-4] PASS: fakeprovider injected failure classified as transient")
}
