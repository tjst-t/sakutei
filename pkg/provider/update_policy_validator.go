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

package provider

import "github.com/tjst-t/sakutei/pkg/core"

// UpdatePolicyValidator is an OPTIONAL Provider capability, discovered by type
// assertion in the same style as IntentValidator.  It lets a provider reject,
// at load time, a desired resource whose update_policy it cannot honour (§6.2,
// §29.1): e.g. a provider whose changes are non-idempotent (a migration) and
// that refuses Apply requires strategy: task, so a resource declared without it
// is an error before the first cycle instead of an Apply failing every cycle.
//
// ValidateUpdatePolicy MUST NOT consult reality (it runs before any Read).
type UpdatePolicyValidator interface {
	ValidateUpdatePolicy(localID string, pol core.UpdatePolicy) error
}

// ValidateUpdatePolicy calls p.ValidateUpdatePolicy when p implements
// UpdatePolicyValidator and returns nil otherwise.
func ValidateUpdatePolicy(p Provider, localID string, pol core.UpdatePolicy) error {
	if v, ok := p.(UpdatePolicyValidator); ok {
		return v.ValidateUpdatePolicy(localID, pol)
	}
	return nil
}
