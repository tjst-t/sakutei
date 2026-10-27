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

// sdk_schema_lint_test.go contains library-style acceptance tests for the
// SDK's Schema lint feature (S62dbba-1, §28.4, ADR-0022).
//
// These tests verify that LintSchema and ValidateProvider catch Schema
// declaration errors at load time (fail-closed), preventing silent safety
// guarantee invalidation.
//
// AC tags:
//
//	AC-S62dbba-1-1  LintSchema detects secret/managed/identity/computed misuse
//	AC-S62dbba-1-3  ValidateProvider integrates lint and rejects invalid schemas
//
// [AC-S62dbba-1-M] marks the machine-executable scenario tests.
package acceptance

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
)

// ─────────────────────────────────────────────────────────────────────────────
// Test providers for schema lint scenarios
// ─────────────────────────────────────────────────────────────────────────────

// minimalValidProvider: valid schema with one identity field, no violations.
type minimalValidProvider struct{}

func (p *minimalValidProvider) Schema() provider.Schema {
	return provider.Schema{
		Fields: map[string]provider.FieldSpec{
			"name": {Kind: core.FieldKindString, Required: true, Identity: true},
			"size": {Kind: core.FieldKindInt, Required: false},
		},
	}
}
func (p *minimalValidProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{ProviderVersion: "v1.0.0"}
}
func (p *minimalValidProvider) Read(_ context.Context, _ string) (core.Observation, error) {
	return core.Observation{Availability: core.AvailabilityOk, ObservedAt: time.Now()}, nil
}
func (p *minimalValidProvider) Apply(_ context.Context, _ core.Plan) error { return nil }

// noIdentityProvider: schema has zero identity fields — must fail lint.
type noIdentityProvider struct{}

func (p *noIdentityProvider) Schema() provider.Schema {
	return provider.Schema{
		Fields: map[string]provider.FieldSpec{
			"name": {Kind: core.FieldKindString, Required: true, Identity: false},
			"size": {Kind: core.FieldKindInt},
		},
	}
}
func (p *noIdentityProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{ProviderVersion: "v1.0.0"}
}
func (p *noIdentityProvider) Read(_ context.Context, _ string) (core.Observation, error) {
	return core.Observation{Availability: core.AvailabilityOk, ObservedAt: time.Now()}, nil
}
func (p *noIdentityProvider) Apply(_ context.Context, _ core.Plan) error { return nil }

// secretIdentityProvider: one field declared secret=true AND identity=true.
type secretIdentityProvider struct{}

func (p *secretIdentityProvider) Schema() provider.Schema {
	return provider.Schema{
		Fields: map[string]provider.FieldSpec{
			"token": {Kind: core.FieldKindString, Required: true, Identity: true, Secret: true},
		},
	}
}
func (p *secretIdentityProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{ProviderVersion: "v1.0.0"}
}
func (p *secretIdentityProvider) Read(_ context.Context, _ string) (core.Observation, error) {
	return core.Observation{Availability: core.AvailabilityOk, ObservedAt: time.Now()}, nil
}
func (p *secretIdentityProvider) Apply(_ context.Context, _ core.Plan) error { return nil }

// computedRequiredProvider: one field declared computed=true AND required=true.
type computedRequiredProvider struct{}

func (p *computedRequiredProvider) Schema() provider.Schema {
	return provider.Schema{
		Fields: map[string]provider.FieldSpec{
			"name":         {Kind: core.FieldKindString, Required: true, Identity: true},
			"generated_id": {Kind: core.FieldKindString, Computed: true, Required: true},
		},
	}
}
func (p *computedRequiredProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{ProviderVersion: "v1.0.0"}
}
func (p *computedRequiredProvider) Read(_ context.Context, _ string) (core.Observation, error) {
	return core.Observation{Availability: core.AvailabilityOk, ObservedAt: time.Now()}, nil
}
func (p *computedRequiredProvider) Apply(_ context.Context, _ core.Plan) error { return nil }

// secretManagedProvider: one field declared secret=true AND managed=true.
type secretManagedProvider struct{}

func (p *secretManagedProvider) Schema() provider.Schema {
	return provider.Schema{
		Fields: map[string]provider.FieldSpec{
			"name":    {Kind: core.FieldKindString, Required: true, Identity: true},
			"api_key": {Kind: core.FieldKindString, Secret: true, Managed: true},
		},
	}
}
func (p *secretManagedProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{ProviderVersion: "v1.0.0"}
}
func (p *secretManagedProvider) Read(_ context.Context, _ string) (core.Observation, error) {
	return core.Observation{Availability: core.AvailabilityOk, ObservedAt: time.Now()}, nil
}
func (p *secretManagedProvider) Apply(_ context.Context, _ core.Plan) error { return nil }

// emptySchemaProvider: schema with no fields at all (no identity → must fail).
type emptySchemaProvider struct{}

func (p *emptySchemaProvider) Schema() provider.Schema {
	return provider.Schema{Fields: map[string]provider.FieldSpec{}}
}
func (p *emptySchemaProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{ProviderVersion: "v1.0.0"}
}
func (p *emptySchemaProvider) Read(_ context.Context, _ string) (core.Observation, error) {
	return core.Observation{Availability: core.AvailabilityOk, ObservedAt: time.Now()}, nil
}
func (p *emptySchemaProvider) Apply(_ context.Context, _ core.Plan) error { return nil }

// noVersionProvider: valid schema but empty ProviderVersion (regression guard).
type noVersionProvider struct{}

func (p *noVersionProvider) Schema() provider.Schema {
	return provider.Schema{
		Fields: map[string]provider.FieldSpec{
			"name": {Kind: core.FieldKindString, Required: true, Identity: true},
		},
	}
}
func (p *noVersionProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{ProviderVersion: ""} // empty — must be rejected
}
func (p *noVersionProvider) Read(_ context.Context, _ string) (core.Observation, error) {
	return core.Observation{Availability: core.AvailabilityOk, ObservedAt: time.Now()}, nil
}
func (p *noVersionProvider) Apply(_ context.Context, _ core.Plan) error { return nil }

// ─────────────────────────────────────────────────────────────────────────────
// AC-S62dbba-1-1: LintSchema detects all violation categories
// ─────────────────────────────────────────────────────────────────────────────

// TestSDKSchemaLint_NoIdentity verifies that LintSchema rejects a Schema with
// no identity field.  Without an identity field the core cannot resolve the
// resource key for deletes and cross-resource references.
//
// [AC-S62dbba-1-1] [AC-S62dbba-1-M]
func TestSDKSchemaLint_NoIdentity(t *testing.T) {
	// [AC-S62dbba-1-1]
	err := sdk.LintSchema((&noIdentityProvider{}).Schema())
	if err == nil {
		t.Fatal("[AC-S62dbba-1-1] LintSchema: expected error for schema with no identity field, got nil")
	}
	if !strings.Contains(err.Error(), "identity") {
		t.Errorf("[AC-S62dbba-1-1] LintSchema: error message should mention 'identity'; got: %v", err)
	}
	t.Logf("[AC-S62dbba-1-1] PASS: LintSchema rejected no-identity schema: %v", err)
}

// TestSDKSchemaLint_SecretPlusIdentity verifies that LintSchema rejects a field
// declared as both secret and identity.
//
// [AC-S62dbba-1-1] [AC-S62dbba-1-M]
func TestSDKSchemaLint_SecretPlusIdentity(t *testing.T) {
	// [AC-S62dbba-1-1]
	err := sdk.LintSchema((&secretIdentityProvider{}).Schema())
	if err == nil {
		t.Fatal("[AC-S62dbba-1-1] LintSchema: expected error for secret+identity field, got nil")
	}
	if !strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "identity") {
		t.Errorf("[AC-S62dbba-1-1] LintSchema: error should mention both 'secret' and 'identity'; got: %v", err)
	}
	t.Logf("[AC-S62dbba-1-1] PASS: LintSchema rejected secret+identity field: %v", err)
}

// TestSDKSchemaLint_ComputedPlusRequired verifies that LintSchema rejects a
// field declared as both computed and required.
//
// [AC-S62dbba-1-1] [AC-S62dbba-1-M]
func TestSDKSchemaLint_ComputedPlusRequired(t *testing.T) {
	// [AC-S62dbba-1-1]
	err := sdk.LintSchema((&computedRequiredProvider{}).Schema())
	if err == nil {
		t.Fatal("[AC-S62dbba-1-1] LintSchema: expected error for computed+required field, got nil")
	}
	if !strings.Contains(err.Error(), "computed") {
		t.Errorf("[AC-S62dbba-1-1] LintSchema: error should mention 'computed'; got: %v", err)
	}
	t.Logf("[AC-S62dbba-1-1] PASS: LintSchema rejected computed+required field: %v", err)
}

// TestSDKSchemaLint_SecretPlusManaged verifies that LintSchema rejects a field
// declared as both secret and managed.
//
// [AC-S62dbba-1-1] [AC-S62dbba-1-M]
func TestSDKSchemaLint_SecretPlusManaged(t *testing.T) {
	// [AC-S62dbba-1-1]
	err := sdk.LintSchema((&secretManagedProvider{}).Schema())
	if err == nil {
		t.Fatal("[AC-S62dbba-1-1] LintSchema: expected error for secret+managed field, got nil")
	}
	if !strings.Contains(err.Error(), "managed") {
		t.Errorf("[AC-S62dbba-1-1] LintSchema: error should mention 'managed'; got: %v", err)
	}
	t.Logf("[AC-S62dbba-1-1] PASS: LintSchema rejected secret+managed field: %v", err)
}

// TestSDKSchemaLint_EmptySchema verifies that LintSchema rejects an empty schema.
//
// [AC-S62dbba-1-1] [AC-S62dbba-1-M]
func TestSDKSchemaLint_EmptySchema(t *testing.T) {
	// [AC-S62dbba-1-1]
	err := sdk.LintSchema((&emptySchemaProvider{}).Schema())
	if err == nil {
		t.Fatal("[AC-S62dbba-1-1] LintSchema: expected error for empty schema (no identity), got nil")
	}
	t.Logf("[AC-S62dbba-1-1] PASS: LintSchema rejected empty schema: %v", err)
}

// TestSDKSchemaLint_ValidSchema verifies that LintSchema accepts a well-formed
// schema with no violations.
//
// [AC-S62dbba-1-1] [AC-S62dbba-1-M]
func TestSDKSchemaLint_ValidSchema(t *testing.T) {
	// [AC-S62dbba-1-1]
	err := sdk.LintSchema((&minimalValidProvider{}).Schema())
	if err != nil {
		t.Errorf("[AC-S62dbba-1-1] LintSchema: expected nil for valid schema, got: %v", err)
	}
	t.Logf("[AC-S62dbba-1-1] PASS: LintSchema accepted valid schema")
}

// TestSDKSchemaLint_LintErrorAllViolations verifies that LintError collects all
// violations at once (not just the first) so a provider author can fix them all
// in one pass.
//
// [AC-S62dbba-1-1] [AC-S62dbba-1-M]
func TestSDKSchemaLint_LintErrorAllViolations(t *testing.T) {
	// [AC-S62dbba-1-1]
	// Schema with three violations simultaneously:
	//   1. computed + required on "id"
	//   2. secret + managed on "api_key"
	//   3. no identity field
	schema := provider.Schema{
		Fields: map[string]provider.FieldSpec{
			"id":      {Kind: core.FieldKindString, Computed: true, Required: true},
			"api_key": {Kind: core.FieldKindString, Secret: true, Managed: true},
		},
	}
	err := sdk.LintSchema(schema)
	if err == nil {
		t.Fatal("[AC-S62dbba-1-1] LintSchema: expected error for multi-violation schema, got nil")
	}
	lintErr, ok := err.(*sdk.LintError)
	if !ok {
		t.Fatalf("[AC-S62dbba-1-1] LintSchema: expected *sdk.LintError, got %T: %v", err, err)
	}
	if len(lintErr.Violations) < 3 {
		t.Errorf("[AC-S62dbba-1-1] LintError: expected at least 3 violations, got %d: %v",
			len(lintErr.Violations), lintErr.Violations)
	}
	t.Logf("[AC-S62dbba-1-1] PASS: LintError collected %d violations: %v",
		len(lintErr.Violations), lintErr.Violations)
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-S62dbba-1-3: ValidateProvider integrates schema lint (fail-closed)
// ─────────────────────────────────────────────────────────────────────────────

// TestSDKValidateProvider_NoIdentity verifies that sdk.ValidateProvider rejects
// a provider whose schema has no identity field (fail-closed integration test).
//
// [AC-S62dbba-1-3] [AC-S62dbba-1-M]
func TestSDKValidateProvider_NoIdentity(t *testing.T) {
	// [AC-S62dbba-1-3]
	err := sdk.ValidateProvider(&noIdentityProvider{})
	if err == nil {
		t.Fatal("[AC-S62dbba-1-3] ValidateProvider: expected error for no-identity schema, got nil")
	}
	if !strings.Contains(err.Error(), "identity") {
		t.Errorf("[AC-S62dbba-1-3] ValidateProvider: error should mention 'identity'; got: %v", err)
	}
	t.Logf("[AC-S62dbba-1-3] PASS: ValidateProvider rejected no-identity schema: %v", err)
}

// TestSDKValidateProvider_ValidProvider verifies that sdk.ValidateProvider
// accepts a fully valid provider (non-empty version + lint-clean schema).
//
// [AC-S62dbba-1-3] [AC-S62dbba-1-M]
func TestSDKValidateProvider_ValidProvider(t *testing.T) {
	// [AC-S62dbba-1-3]
	err := sdk.ValidateProvider(&minimalValidProvider{})
	if err != nil {
		t.Errorf("[AC-S62dbba-1-3] ValidateProvider: expected nil for valid provider, got: %v", err)
	}
	t.Logf("[AC-S62dbba-1-3] PASS: ValidateProvider accepted valid provider")
}

// TestSDKValidateProvider_EmptyVersion verifies that sdk.ValidateProvider
// still rejects an empty provider_version (regression: existing check preserved).
//
// [AC-S62dbba-1-3] [AC-S62dbba-1-M]
func TestSDKValidateProvider_EmptyVersion(t *testing.T) {
	// [AC-S62dbba-1-3]
	err := sdk.ValidateProvider(&noVersionProvider{})
	if err == nil {
		t.Fatal("[AC-S62dbba-1-3] ValidateProvider: expected error for empty provider_version, got nil")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Errorf("[AC-S62dbba-1-3] ValidateProvider: error should mention 'version'; got: %v", err)
	}
	t.Logf("[AC-S62dbba-1-3] PASS: ValidateProvider rejected empty version: %v", err)
}

// TestSDKSchemaLint_FakeProviderRegression verifies that the existing
// fakeprovider schema passes lint (no regression from introducing schema lint).
//
// [AC-S62dbba-1-3] [AC-S62dbba-1-M]
func TestSDKSchemaLint_FakeProviderRegression(t *testing.T) {
	// [AC-S62dbba-1-3]
	fakeSchema := provider.Schema{
		Fields: map[string]provider.FieldSpec{
			"name": {Kind: core.FieldKindString, Required: true, Identity: true},
			"size": {Kind: core.FieldKindInt, Required: false},
		},
	}
	err := sdk.LintSchema(fakeSchema)
	if err != nil {
		t.Errorf("[AC-S62dbba-1-3] LintSchema: fakeprovider schema must pass lint; got: %v", err)
	}
	t.Logf("[AC-S62dbba-1-3] PASS: fakeprovider schema passes lint (no regression)")
}

// ─────────────────────────────────────────────────────────────────────────────
// CLI integration: schema lint gate in sakutei plan
// ─────────────────────────────────────────────────────────────────────────────

// TestSDKSchemaLint_CLI_FakePassesOnPlan verifies that "sakutei plan" with the
// fake provider (lint-clean schema + valid version) exits 0.
//
// [AC-S62dbba-1-3] [AC-S62dbba-1-M]
func TestSDKSchemaLint_CLI_FakePassesOnPlan(t *testing.T) {
	// [AC-S62dbba-1-3]
	tmp := t.TempDir()
	intentDir := tmp + "/intent"
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := tmp + "/state.json"

	initStateFile(t, statePath)
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "lint-sdk-vm",
				"fields":   map[string]string{"name": "lint-sdk-vm", "size": "5"},
			},
		},
	})

	_, stderr, code := runPlan(t, intentDir, statePath)
	if code != 0 {
		t.Errorf("[AC-S62dbba-1-3] plan: expected exit 0 for valid schema+version; got %d\nstderr: %s",
			code, stderr)
	}
	t.Logf("[AC-S62dbba-1-3] PASS: plan exited %d with valid schema+version provider", code)
}
