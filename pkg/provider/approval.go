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

import "github.com/tjst-t/sakutei/v2/pkg/core"

// ApprovalRequirer is an OPTIONAL Provider capability, discovered by type
// assertion like PlanRefiner (Backlog #179).  It lets a provider say that an
// op of its own is a change that must pass the approval gate — the same gate
// as the explicit absent's Delete (§12.2, core.Op.RequiresApproval): the op
// runs only when admitted to an open planned_change lane of its kind (§13.3;
// the lane's approval, cooling-off and window are the gate, §14.4 / §14.5),
// and it is destructive, so the normal change budget still counts — and
// halts — it (§13.1 / §13.2).  Outside every lane it is held and the
// ChangeAwaitingApproval Event says why.
//
// Example: nftables (live) creating a base chain or changing its type, hook,
// priority or policy — a change that can shut the management path out.
//
// The core calls ApprovalRequired once per op the plan holds for an Ok
// observation, after the PlanRefiner, also for a Create (observed is nil
// then); never for an explicit absent (whose Delete already requires
// approval).  A non-empty reason marks the op Destructive and
// RequiresApproval, with the reason as its core.Op.ApprovalReason.  The
// core only ever raises the marks.  It MUST NOT do I/O and MUST NOT echo
// secret field values.
type ApprovalRequirer interface {
	ApprovalRequired(localID string, observed core.State, desired map[string]string, op core.Op) (reason string)
}
