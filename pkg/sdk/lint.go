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
	"strings"

	"github.com/tjst-t/sakutei/pkg/provider"
)

// LintError is returned by LintSchema when one or more Schema declarations
// are found to be invalid.  It carries the full list of violations so the
// provider author can fix them all at once.
type LintError struct {
	// Violations is the list of human-readable error strings.
	Violations []string
}

func (e *LintError) Error() string {
	return "schema lint: " + strings.Join(e.Violations, "; ")
}

// LintSchema validates the Schema declarations for correctness (§28.4).
//
// Getting Schema declarations right is the hardest part of writing a provider:
// an incorrect declaration silently invalidates the safety guarantees that the
// core provides (secret masking, managed exclusion, identity resolution, etc.).
//
// Rules enforced (fail-closed — each violation is a hard error):
//
//  1. At least one identity field must be declared.
//     Without an identity field the core cannot resolve the resource's key,
//     which means deletes and references target the wrong resource.
//
//  2. A field MUST NOT be both secret and identity.
//     Secret fields are masked in logs and diff output.  If the identity key
//     is a secret, the masked value appears in logs and the secret is
//     effectively exposed in plaintext in the resource address.
//
//  3. A field MUST NOT be both computed and required.
//     Computed fields are observed-only; they never appear in desired state.
//     Requiring a computed field would make the desired-state schema impossible
//     to satisfy, as the field value only exists after the resource is created.
//
//  4. A field MUST NOT be both secret and managed.
//     Managed means the field is controlled by an external system; Sakutei
//     does not own its value.  Secret means Sakutei is responsible for
//     masking and JIT-resolving the value.  The two intents are contradictory:
//     if the field is externally owned, Sakutei should not be masking it.
//
// LintSchema returns nil if all declarations are valid.  It returns *LintError
// listing all violations found (not just the first) if any are invalid.
func LintSchema(schema provider.Schema) error {
	var violations []string

	identityCount := 0
	for name, spec := range schema.Fields {
		if spec.Identity {
			identityCount++
		}

		// Rule 2: secret + identity
		if spec.Secret && spec.Identity {
			violations = append(violations,
				fmt.Sprintf("field %q is both secret and identity: secret fields must not be used as identity keys because the secret value would appear in resource addresses and logs", name))
		}

		// Rule 3: computed + required
		if spec.Computed && spec.Required {
			violations = append(violations,
				fmt.Sprintf("field %q is both computed and required: computed fields are observed-only and cannot be required in desired state", name))
		}

		// Rule 4: secret + managed
		if spec.Secret && spec.Managed {
			violations = append(violations,
				fmt.Sprintf("field %q is both secret and managed: secret fields are Sakutei-owned (JIT-resolved), but managed fields are externally owned — these intents conflict", name))
		}
	}

	// Rule 1: at least one identity field
	if identityCount == 0 {
		violations = append(violations,
			"at least one identity field must be declared: without an identity field the core cannot resolve the resource key for deletes and references")
	}

	if len(violations) > 0 {
		return &LintError{Violations: violations}
	}
	return nil
}
