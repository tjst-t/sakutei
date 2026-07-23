// Copyright 2026 Takumi Tsujishita
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

package core

// CauseTag classifies the root cause of a single diff (§4.4, §10).
// Per-resource attribution is required: a single cycle-level tag would
// mask A/E co-occurrence within the same cycle.
//
// The letters A–E are normative; do not change them—they appear in
// audit logs and operator-facing output.
type CauseTag string

const (
	// CauseTagA is an intentional change: the operator committed a new desired
	// state (intent_revision changed).
	CauseTagA CauseTag = "A"
	// CauseTagB is drift: the real world drifted away from the desired state
	// without an intent commit (remediation candidate).
	CauseTagB CauseTag = "B"
	// CauseTagC is a ghost diff caused by a provider upgrade changing how the
	// provider normalizes or computes fields (provider_version changed).
	CauseTagC CauseTag = "C"
	// CauseTagD is cascading: a dependency changed and this resource must follow.
	CauseTagD CauseTag = "D"
	// CauseTagE is an external-input change: Vault secret rotation, inventory
	// update, or other render-phase external inputs changed (rendered_revision
	// changed while intent_revision is stable).
	CauseTagE CauseTag = "E"
)

// OpKind enumerates the three fundamental convergence operations.
type OpKind int

const (
	// OpKindCreate means the resource does not exist and must be created.
	OpKindCreate OpKind = iota
	// OpKindUpdate means the resource exists but one or more fields differ.
	OpKindUpdate
	// OpKindDelete means the resource exists in reality but is absent from
	// the desired state and is within a prune scope.
	OpKindDelete
)

// FieldDiff records which field changed and in what direction (used for
// display in sakutei status / plan output).  The diff values are intentionally
// untyped strings here so that the display layer can format them without
// depending on the full type system; the Schema-driven diff engine populates
// these.
type FieldDiff struct {
	// Field is the Schema field name.
	Field string
	// From is the string representation of the observed (actual) value.
	// Empty when the resource did not exist before.
	From string
	// To is the string representation of the desired value.
	// Empty when the operation is a deletion.
	To string
}

// Op is a single convergence operation within a Plan (§4.4).
//
// Destructive is the safety-mechanism drive axis (§13): rate-limiting,
// destruction budget, and global freeze all key off this flag.  When
// TrueObserve is false for the provider, the flag must be set to true
// (assume destructive; non-symmetric principle).
type Op struct {
	// Kind is the operation type: Create, Update, or Delete.
	Kind OpKind
	// Target is the composite ID of the resource to operate on.
	Target ID
	// Destructive indicates that this operation may destroy data or
	// reduce capacity.  Safety mechanisms (rate limit, budget, halt) are
	// driven by this flag.  When in doubt, set to true.
	Destructive bool
	// Cause is the per-resource root-cause classification.
	Cause CauseTag
	// Diagnostic is a human-readable description of the diff, shown in
	// sakutei plan / sakutei status output.
	Diagnostic string
	// Fields lists the individual field-level diffs for Update operations.
	// May be nil for Create and Delete.
	Fields []FieldDiff
}

// Plan is the output of the Diff phase (§4.4).  It is an ephemeral,
// in-memory value (S3) produced fresh for each reconciliation cycle.
//
// The Plan is what Provider.Apply receives; it encodes everything the
// safety layer and the executor need to decide whether and how to proceed.
type Plan struct {
	// Ops is the ordered list of convergence operations.
	Ops []Op
}
