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

// PlanRefiner is an OPTIONAL Provider capability, discovered by type assertion
// in the same style as Lister and IntentValidator.  It lets a provider refine,
// at plan time, the ops the core's Schema-driven diff produced for one
// resource with knowledge the Schema cannot express (§28.4, §29.1 Redfish):
//
//   - a field compared only under a condition on another desired field
//     (Redfish: boot_target is not compared while boot_enabled is Disabled);
//   - a desired-only parameter of HOW to converge that reality never reports
//     (Redfish: power_off / force_after);
//   - an op that is destructive only in some situations (Redfish: the stop
//     that has waited force_after is a ForceOff, §13);
//   - a desired value the target itself rejects, checked against what it
//     advertises in the observation (Redfish: @Redfish.AllowableValues).
//
// The core calls RefinePlan once per resource and cycle whenever the resource
// was observed Ok, also when the diff is empty (so the provider can drop
// in-memory S3 bookkeeping that the desired no longer needs).  The core keeps
// the refinement safe by construction (diff.Engine.Plan):
//
//   - an op can only lose FieldDiffs, never gain or change one; an op left
//     with no FieldDiff is dropped (converged);
//   - Destructive can only be raised, never lowered (non-symmetric principle);
//   - Kind, Target and Cause stay the core's; a non-empty Diagnostic replaces
//     the core's.
//
// An error means the desired state cannot be planned against this reality:
// the resource gets no op this cycle and the error is reported.  RefinePlan
// MUST NOT do I/O (it sees only the observation of this cycle) and MUST NOT
// echo secret field values in its error.
type PlanRefiner interface {
	RefinePlan(localID string, observed core.State, desired map[string]string, ops []core.Op) ([]core.Op, error)
}
