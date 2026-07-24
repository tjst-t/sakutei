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

package sdk

import (
	"fmt"

	"github.com/tjst-t/sakutei/pkg/provider"
)

// ValidateProvider runs all startup invariants for a provider (fail-closed).
//
// This extends the existing pkg/provider.ValidateProvider with Schema lint
// (§28.4, ADR-0022).  Callers that previously called provider.ValidateProvider
// should switch to sdk.ValidateProvider to get the full validation suite.
//
// Checks performed (in order):
//
//  1. provider_version non-empty (§10.4, ADR-0008): ghost diffs (cause C) cannot
//     be detected for an unversioned provider.  An empty version means diffs would
//     be silently misclassified as drift B and auto-applied.
//
//  2. Schema lint (§28.4, ADR-0022): validates that the Schema's field
//     declarations are internally consistent.  Incorrect declarations silently
//     invalidate the safety guarantees that the core provides (secret masking,
//     managed exclusion, identity resolution).  See LintSchema for the full list
//     of rules.
//
// If either check fails, ValidateProvider returns a non-nil error and the
// reconcile loop MUST NOT start (fail-closed).
func ValidateProvider(p provider.Provider) error {
	// Check 1: provider_version non-empty (existing check from pkg/provider).
	if err := provider.ValidateProvider(p); err != nil {
		return err
	}

	// Check 2: Schema lint.
	if err := LintSchema(p.Schema()); err != nil {
		return fmt.Errorf("schema lint failed for provider: %w", err)
	}

	return nil
}
