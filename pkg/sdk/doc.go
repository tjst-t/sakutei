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

// Package sdk provides cross-cutting helpers for Sakutei provider authors
// (§28, ADR-0022).
//
// The SDK is the mechanism that lets provider authors write only ~4 functions
// (Schema / Capabilities / Read / Apply) while the SDK handles the rest.
//
// # Golden path for provider authors
//
// Step 1 — Write Schema (hardest: §28.4):
//
//	schema := provider.Schema{Fields: map[string]provider.FieldSpec{
//	    "name": {Kind: core.FieldKindString, Required: true, Identity: true},
//	    "ttl":  {Kind: core.FieldKindInt},
//	}}
//
// Step 2 — Validate at startup with ValidateProvider:
//
//	if err := sdk.ValidateProvider(myProvider); err != nil {
//	    log.Fatal(err)
//	}
//
// ValidateProvider runs both the original Capabilities check
// (provider_version non-empty) and Schema lint (§28.4 rules).
//
// Step 3 — Classify errors returned from Read / Apply:
//
//	if sdk.IsTransient(err) { /* back off and retry */ }
//	if sdk.IsPermanent(err) { /* move resource to stuck */ }
//
// Step 4 — Run the contract test harness in provider unit tests:
//
//	func TestMyProviderContracts(t *testing.T) {
//	    ct := sdk.ContractTester{ ... }
//	    ct.RunAll(t)
//	}
//
// # Schema lint rules (§28.4)
//
// The following Schema declarations are rejected by LintSchema:
//
//   - No identity field declared at all — core cannot resolve resource identity.
//   - A field that is both secret and identity — secrets used as identity keys
//     would appear in logs, defeating the masking guarantee.
//   - A field that is both computed and required — computed fields are
//     observed-only; they cannot be required in desired state.
//   - A field that is both secret and managed — conflicting intent: secrets
//     are Sakutei-managed by definition, yet managed means externally owned.
//
// # Separation of production and test code
//
// ContractTester lives in contract_tester.go (this package, for non-test
// code) and takes a *testing.T parameter.  It uses only stdlib/testing; no
// test-only build tags are needed.  The production binary never instantiates
// ContractTester so no dead code is linked in; Go's linker drops unreferenced
// functions automatically.
package sdk
