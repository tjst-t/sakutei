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

// Package diff implements the Schema-driven diff engine for Sakutei (§10).
//
// The engine compares desired state (from intent) against observed actual state
// (from Provider.Read) and produces a core.Plan containing zero or more Ops.
//
// Sprint 1 (Sead944-2) scope:
//   - Create: resource absent in reality, present in desired → OpKindCreate
//   - Update: resource present in both; one or more field values differ → OpKindUpdate
//   - Delete: not implemented in this story (no prune scope yet)
//
// Managed-field exclusion, cause-tag attribution, and ghost-diff filtering
// (CauseTagC) are deferred to subsequent Sprints.
package diff

import (
	"fmt"
	"sort"

	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// DesiredResource represents a single resource as declared in intent.
// It carries the composite ID and the desired field values.
type DesiredResource struct {
	// ID is the composite identifier (Provider, Scope, Local).
	ID core.ID
	// Fields is the map of field name → desired value (as string).
	// String representation is used at this layer; the diff engine converts
	// both sides to strings for comparison in this Sprint.
	Fields map[string]string
	// DependsOn is the list of provider-local IDs (same intent scope) that
	// this resource depends on.  Populated from the "depends_on" field in
	// the intent file (Story S94f1f9-2, provisional; formal Loader is S33266e).
	DependsOn []string
	// DeletionPolicy controls how the engine handles resources in reality that
	// are absent from the intent (§12.2, ADR-0005).  Defaults to
	// ExplicitAbsentOnly (open-world) when not declared.
	DeletionPolicy core.DeletionPolicy
	// Pruneable declares whether this resource may be automatically deleted by
	// the prune engine.  Defaults to false (§12.3, ADR-0005): destructive
	// operations require explicit opt-in.  Only meaningful when DeletionPolicy
	// is PruneWithinScope.
	Pruneable bool
}

// Engine holds the provider schema used to drive the diff.
type Engine struct {
	schema provider.Schema
}

// New creates a diff Engine for the given provider schema.
func New(schema provider.Schema) *Engine {
	return &Engine{schema: schema}
}

// Diff computes the core.Plan needed to reconcile a single resource.
//
// obs is the output of Provider.Read for this resource.
// desired is the intent declaration for this resource.
//
// Diff returns a Plan containing zero or one Op:
//   - If obs.Availability != Ok: no Op (unknown ≠ absent; do not create/delete).
//   - If obs.State is nil (resource absent in reality): OpKindCreate.
//   - If obs.State is non-nil and all fields match: no Op (already converged).
//   - If obs.State is non-nil and fields differ: OpKindUpdate.
//
// CauseTag is set to CauseTagA for all Ops in this Sprint (cause attribution
// is a follow-on Story; A is the safest placeholder for "operator intent").
func (e *Engine) Diff(obs core.Observation, desired DesiredResource) core.Plan {
	// If the target is unreachable or mid-convergence, skip this resource.
	// unknown ≠ absent (ADR-0002; §3 asymmetry principle).
	if obs.Availability != core.AvailabilityOk {
		return core.Plan{}
	}

	// Resource absent in reality → Create.
	if obs.State == nil {
		fields := desiredFieldDiffs(desired.Fields)
		diagnostic := fmt.Sprintf("resource %s does not exist in reality; will create", desired.ID)
		return core.Plan{
			Ops: []core.Op{
				{
					Kind:        core.OpKindCreate,
					Target:      desired.ID,
					Destructive: false,
					Cause:       core.CauseTagA,
					Diagnostic:  diagnostic,
					Fields:      fields,
				},
			},
		}
	}

	// Resource exists in reality → compare field by field.
	fieldDiffs := diffFields(obs.State, desired.Fields, e.schema)
	if len(fieldDiffs) == 0 {
		// Already converged.
		return core.Plan{}
	}

	diagnostic := fmt.Sprintf("resource %s has %d field(s) out of sync", desired.ID, len(fieldDiffs))
	return core.Plan{
		Ops: []core.Op{
			{
				Kind:        core.OpKindUpdate,
				Target:      desired.ID,
				Destructive: false,
				Cause:       core.CauseTagA,
				Diagnostic:  diagnostic,
				Fields:      fieldDiffs,
			},
		},
	}
}

// diffFields compares observed state against desired fields and returns the
// list of FieldDiff entries where the values differ.
//
// Fields marked Computed in the schema are excluded from diff (they are
// observed-only; desired never specifies them).
// Fields marked Managed in the schema are excluded from diff (external system
// owns them; Sakutei must not fight the external system).
//
// In this Sprint, values are compared as strings via their fmt representation.
func diffFields(actual core.State, desired map[string]string, schema provider.Schema) []core.FieldDiff {
	var diffs []core.FieldDiff

	// Collect all field names from desired, sorted for deterministic output.
	names := make([]string, 0, len(desired))
	for name := range desired {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		spec, declared := schema.Fields[name]
		if declared {
			// Skip computed and managed fields.
			if spec.Computed || spec.Managed {
				continue
			}
		}

		desiredVal := desired[name]
		actualTV, exists := actual[name]

		if !exists {
			// Field absent in reality → differs.
			diffs = append(diffs, core.FieldDiff{
				Field: name,
				From:  "",
				To:    desiredVal,
			})
			continue
		}

		actualStr := typedValueToString(actualTV)
		if actualStr != desiredVal {
			diffs = append(diffs, core.FieldDiff{
				Field: name,
				From:  actualStr,
				To:    desiredVal,
			})
		}
	}

	return diffs
}

// desiredFieldDiffs creates FieldDiff entries for all desired fields in a
// Create operation (From is always empty because nothing existed before).
func desiredFieldDiffs(desired map[string]string) []core.FieldDiff {
	names := make([]string, 0, len(desired))
	for name := range desired {
		names = append(names, name)
	}
	sort.Strings(names)

	diffs := make([]core.FieldDiff, 0, len(desired))
	for _, name := range names {
		diffs = append(diffs, core.FieldDiff{
			Field: name,
			From:  "",
			To:    desired[name],
		})
	}
	return diffs
}

// typedValueToString converts a TypedValue to its string representation for
// comparison purposes.  Precision/formatting is intentionally simple in Sprint
// 1; a full normalisation layer comes in a later Story.
func typedValueToString(tv core.TypedValue) string {
	return fmt.Sprintf("%v", tv.Value)
}
