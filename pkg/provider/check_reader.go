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

import (
	"context"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// CheckReader is an OPTIONAL capability of the minimal tier (§5.6, §28.7),
// discovered by type assertion like Lister and PlanRefiner.  A provider that
// cannot return current State (Capabilities.TrueObserve false) but can answer
// "would applying this desired change anything?" by a dry run against the
// target (Ansible: the module in check mode) implements it.
//
// ReadCheck is the one place where the desired reaches an observation, and
// only as the question asked of the target: the target, not the provider,
// compares it with reality, and nothing is changed.  It returns:
//
//   - Availability Ok with Check set (State nil): Check.Changed false means
//     converged, true means applying the desired would change reality;
//   - Availability Unknown (with an error) when the target cannot answer: it
//     is unreachable, the dry run failed, or the target has no dry run for
//     this desired.  unknown ≠ "no change" — the core does nothing with it.
//
// Read of a CheckReader has no desired to ask about, so it MUST NOT return
// Ok; the core observes a CheckReader through ReadFor wherever it has the
// desired.  A provider declaring TrueObserve MUST NOT implement CheckReader
// (ValidateProvider): a provider that can read State has no business seeing
// the desired (ADR-0002).
type CheckReader interface {
	ReadCheck(ctx context.Context, localID string, desired map[string]string) (core.Observation, error)
}

// ReadFor observes localID of p: through ReadCheck with desired when p is a
// CheckReader, otherwise through Read (which never sees desired).  The core's
// convergence paths observe a desired resource through it.
func ReadFor(ctx context.Context, p Provider, localID string, desired map[string]string) (core.Observation, error) {
	if cr, ok := p.(CheckReader); ok {
		return cr.ReadCheck(ctx, localID, desired)
	}
	return p.Read(ctx, localID)
}
