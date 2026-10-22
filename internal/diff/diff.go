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
// Scb0ecd-2 additions:
//   - CauseContext is accepted by Diff(); the cause package classifies each Op.
//   - A/B/C are fully active; D/E are seams (always false in v1a).
//   - Managed-field exclusion is handled by diffFields().
package diff

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/internal/cause"
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
	// ReadyConditions maps a dependency's Local ID (an entry of DependsOn) to
	// the named ready condition it must satisfy before this resource may be
	// applied (§8.3, §11.2).  The upstream's Observation.Ready[condition] must
	// be true in the CURRENT cycle's observation; until then this resource is
	// skipped every cycle (multi-cycle dependency release).  A dependency with
	// no entry here only needs to be processed (the plain depends_on order).
	// Nil when no dependency declares a ready_condition.
	ReadyConditions map[string]string
	// DeletionPolicy controls how the engine handles resources in reality that
	// are absent from the intent (§12.2, ADR-0005).  Defaults to
	// ExplicitAbsentOnly (open-world) when not declared.
	//
	// NOTE: pruneability is deliberately NOT a field here.  As of Scf7762-1 it
	// is a TYPE-level property declared by the provider via
	// Capabilities().Pruneable (ADR-0005: "破壊的型は pruneable:false 既定"), not
	// a per-instance intent flag.  See internal/prune.
	DeletionPolicy core.DeletionPolicy
	// Allocations maps a field name to the pool it must be allocated from
	// (intent: `field: { from_pool: <pool> }`, §19.1).  Render phase 1 asks the
	// AllocationAuthority for the value and writes it into Fields; until then
	// the field is absent from Fields.  Nil when the resource requests nothing.
	Allocations map[string]string
	// ObserveTier names the Observe-frequency tier the resource belongs to
	// (intent: `observe_tier: <name>`, declared under top-level
	// `observe_tiers:`, §16.5).  Not the §8 DAG priority band (`tier:`).  Empty means
	// the default: Observed every cycle (§8).
	ObserveTier string
	// UpdatePolicy is the resource's update_policy (§6.2).  With strategy: task
	// a diff never reaches Apply: the daemon launches a Task instead (§6.5).
	// The zero value is the default Apply strategy.
	UpdatePolicy core.UpdatePolicy
}

// Engine holds the provider schema used to drive the diff.
type Engine struct {
	schema provider.Schema
	// refiner is the provider's optional plan-time refinement (Plan); nil for
	// an Engine built from a Schema alone.
	refiner provider.PlanRefiner
}

// New creates a diff Engine for the given provider schema.
func New(schema provider.Schema) *Engine {
	return &Engine{schema: schema}
}

// ForProvider creates a diff Engine for p: its Schema drives Diff, and Plan
// also runs p's PlanRefiner when p implements it.
func ForProvider(p provider.Provider) *Engine {
	e := New(p.Schema())
	if r, ok := p.(provider.PlanRefiner); ok {
		e.refiner = r
	}
	return e
}

// Plan is Diff followed by the provider's plan-time refinement
// (provider.PlanRefiner, §28.4).  The convergence paths (daemon, scoped,
// plan, reconcile, status) plan through it; Diff alone is the pure
// Schema-driven diff.
//
// The refinement runs only for an Ok observation with a State (a target that
// is unknown, converging or absent gives the provider nothing to check
// against).  The core keeps it safe whatever the provider returns: an op can
// only lose FieldDiffs (one left without any is dropped), Destructive can
// only be raised, and Kind / Target / Cause stay the core's.  An error from
// the provider is returned with an empty Plan: the resource is not changed
// this cycle.
func (e *Engine) Plan(obs core.Observation, desired DesiredResource, ctx cause.CauseContext) (core.Plan, error) {
	plan := e.Diff(obs, desired, ctx)
	if e.refiner == nil || obs.Availability != core.AvailabilityOk || obs.State == nil {
		return plan, nil
	}
	refined, err := e.refiner.RefinePlan(desired.ID.Local, obs.State, desired.Fields, plan.Ops)
	if err != nil {
		return core.Plan{}, fmt.Errorf("resource %s: %w", desired.ID, err)
	}
	return core.Plan{Ops: mergeRefined(plan.Ops, refined)}, nil
}

// mergeRefined applies the provider's refinement to the core's ops under the
// rules of Plan.  A refined op is matched to the core's op by Target and Kind;
// a core op with no match is dropped (the provider found nothing to change).
func mergeRefined(orig, refined []core.Op) []core.Op {
	var out []core.Op
	for _, op := range orig {
		var r *core.Op
		for i := range refined {
			if refined[i].Target.Key() == op.Target.Key() && refined[i].Kind == op.Kind {
				r = &refined[i]
				break
			}
		}
		if r == nil {
			continue
		}
		keep := make(map[string]bool, len(r.Fields))
		for _, f := range r.Fields {
			keep[f.Field] = true
		}
		var fields []core.FieldDiff
		for _, f := range op.Fields {
			if keep[f.Field] {
				fields = append(fields, f)
			}
		}
		if len(fields) == 0 && op.Kind != core.OpKindDelete {
			continue
		}
		op.Fields = fields
		op.Destructive = op.Destructive || r.Destructive
		if r.Diagnostic != "" {
			op.Diagnostic = r.Diagnostic
		}
		out = append(out, op)
	}
	return out
}

// ValidateDesired checks a desired resource's intent fields against the Schema
// BEFORE any diff runs (S33266e-1, §20).  It rejects a bare plaintext value in a
// field the Schema flags `secret`: such a field MUST carry a SecretRef reference
// form — either the "secretref:<backend>/<path>#genN" string or the
// {"$secret":"<path>",...} JSON object — never a raw scalar.
//
// Why fail here rather than silently mask: a raw plaintext in a secret field is
// (a) a plaintext secret physically sitting in an intent file, and (b) a
// permanent non-converging diff — the desired side would mask to "***" while the
// observed side is "secretref:...#genN", so they never compare equal.  Failing
// loud at validate time surfaces the operator mistake before the first diff and
// before the secret can spread.
//
// The returned error names the offending field and instructs the operator to use
// a SecretRef.  It NEVER echoes the field value (it may be a real secret): the
// value is discarded, only the field name appears.
func (e *Engine) ValidateDesired(desired DesiredResource) error {
	names := make([]string, 0, len(desired.Fields))
	for name := range desired.Fields {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		spec, declared := e.schema.Fields[name]
		if !declared || !spec.Secret {
			continue
		}
		if _, ok := secretRefForm(desired.Fields[name]); !ok {
			// Do NOT include the value: it may be a real plaintext secret.
			return fmt.Errorf(
				"resource %s: secret-flagged field %q must be a SecretRef "+
					"(secretref:<backend>/<path>#genN or {\"$secret\":\"<path>\",...}), got a bare scalar",
				desired.ID, name)
		}
	}
	return nil
}

// Diff computes the core.Plan needed to reconcile a single resource.
//
// obs is the output of Provider.Read for this resource.
// desired is the intent declaration for this resource.
// ctx is the per-cycle cause context used to classify the root cause of any
// diff (§10.2, ADR-0008, Scb0ecd-2).  The caller SHOULD set
// ctx.ResourceObservedAbsent = (obs.State == nil) so Classify can distinguish a
// cold-start Create (tag A) from a cold-start drift-repair Update (tag B) when
// there is no prior revision history (fix Sc7a647 cause-consistency).  A
// zero-value CauseContext (empty prev revision, ResourceObservedAbsent=false)
// now classifies a first-run UPDATE as B, not A — an empty prev revision means
// "no history / cold start", not "intent changed".  A first-run CREATE is A only
// when the caller sets ResourceObservedAbsent (which Diff's own obs.State == nil
// branch already implies for the op it emits).
//
// Diff returns a Plan containing zero or one Op:
//   - If obs.Availability != Ok: no Op (unknown ≠ absent; do not create/delete).
//   - If obs.State is nil (resource absent in reality): OpKindCreate.
//   - If obs.State is non-nil and all fields match: no Op (already converged).
//   - If obs.State is non-nil and fields differ: OpKindUpdate.
//
// CauseTag is determined by cause.Classify(ctx) per-resource (§10.3).
func (e *Engine) Diff(obs core.Observation, desired DesiredResource, ctx cause.CauseContext) core.Plan {
	// If the target is unreachable or mid-convergence, skip this resource.
	// unknown ≠ absent (ADR-0002; §3 asymmetry principle).
	if obs.Availability != core.AvailabilityOk {
		return core.Plan{}
	}

	// Resource absent in reality → Create.
	if obs.State == nil {
		tag := cause.Classify(true, ctx)
		fields := desiredFieldDiffs(desired.Fields, e.schema)
		diagnostic := fmt.Sprintf("resource %s does not exist in reality; will create", desired.ID)
		return core.Plan{
			Ops: []core.Op{
				{
					Kind:        core.OpKindCreate,
					Target:      desired.ID,
					Destructive: false,
					Cause:       tag,
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

	tag := cause.Classify(true, ctx)
	diagnostic := fmt.Sprintf("resource %s has %d field(s) out of sync", desired.ID, len(fieldDiffs))
	return core.Plan{
		Ops: []core.Op{
			{
				Kind:        core.OpKindUpdate,
				Target:      desired.ID,
				Destructive: false,
				Cause:       tag,
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
		secret := false
		if declared {
			// Skip computed and managed fields.
			if spec.Computed || spec.Managed {
				continue
			}
			secret = spec.Secret
		}

		// §20 / S33266e-1: for a secret-flagged field, both sides are normalised
		// to the SecretRef *reference* form (secretref:<backend>/<path>#genN)
		// BEFORE comparison.  This is the structural guarantee that the plaintext
		// value never enters Diff / convergence: only the ref (path + generation)
		// is compared and stored in the FieldDiff.  A generation bump (gen3→gen4)
		// changes the ref string and therefore IS a diff (cause E); a change of
		// the underlying value with the SAME ref+gen is invisible here (there is
		// no value to compare).
		desiredVal := desired[name]
		actualTV, exists := actual[name]

		if secret {
			desiredVal = secretMaskField(desiredVal)
		}

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
		if secret {
			actualStr = secretMaskField(actualStr)
		}
		// §10 / S c7a647a: unordered (Ordered:false) string-list フィールドは
		// 集合として比較する。両辺を ListSep で split→sort→join して正規化し、
		// 純粋な並べ替えを diff にしない。Ordered:true（や非 list、ListSep 未
		// 宣言）は verbatim 比較（現行動作）。
		if declared {
			actualStr = normalizeUnorderedList(actualStr, spec)
			desiredVal = normalizeUnorderedList(desiredVal, spec)
		}
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
//
// §20 / S33266e-1: secret-flagged fields are normalised to the SecretRef
// reference form so a Create op never carries a plaintext secret value into its
// FieldDiff (which is printed by printOp and could otherwise leak).
func desiredFieldDiffs(desired map[string]string, schema provider.Schema) []core.FieldDiff {
	names := make([]string, 0, len(desired))
	for name := range desired {
		names = append(names, name)
	}
	sort.Strings(names)

	diffs := make([]core.FieldDiff, 0, len(desired))
	for _, name := range names {
		to := desired[name]
		if spec, ok := schema.Fields[name]; ok {
			if spec.Secret {
				to = secretMaskField(to)
			}
			// unordered set フィールドは canonical order で記録する
			// （並べ替えが Create op の見た目に影響しないように）。
			to = normalizeUnorderedList(to, spec)
		}
		diffs = append(diffs, core.FieldDiff{
			Field: name,
			From:  "",
			To:    to,
		})
	}
	return diffs
}

// SnapshotFields renders an observed State as the field → string map kept by
// the last-seen ring and quoted in drift Events (§20.2).  A field the Schema
// flags secret, or one observed as a SecretRef, is reduced to its ref form
// ("***" if it is not a recognised ref): the plaintext never enters an Event.
func (e *Engine) SnapshotFields(state core.State) map[string]string {
	out := make(map[string]string, len(state))
	for name, tv := range state {
		v := typedValueToString(tv)
		if e.schema.Fields[name].Secret || tv.Kind == core.FieldKindSecretRef {
			v = secretMaskField(v)
		}
		out[name] = v
	}
	return out
}

// typedValueToString converts a TypedValue to its string representation for
// comparison purposes.  Precision/formatting is intentionally simple in Sprint
// 1; a full normalisation layer comes in a later Story.
//
// §20 / S33266e-1: if the value is a core.SecretRef, fmt routes through
// SecretRef.String() (the masked ref form) — the plaintext is unreachable via
// fmt by construction.  secretMaskField below is applied on top for the
// secret-flagged path so that even a value that did NOT arrive as a SecretRef
// (a raw string that slipped through) is masked before it can enter a FieldDiff.
func typedValueToString(tv core.TypedValue) string {
	return fmt.Sprintf("%v", tv.Value)
}

// normalizeUnorderedList canonicalises the encoded value of an UNORDERED
// (Ordered:false) FieldKindStringList field so that a pure reorder of the set
// is not a diff (§10, S c7a647a).
//
// これが `Ordered` flag を diff エンジンで実際に load-bearing にする箇所である。
// 正規化の条件（すべて満たすとき split→sort→join する）:
//
//   - spec.Kind == FieldKindStringList（list フィールドのみ）
//   - !spec.Ordered（順序が意味を持たない = set 的）
//   - spec.ListSep != ""（provider 非依存の diff が split するために必須）
//
// 上記のいずれかを満たさない場合は値をそのまま返す（verbatim 比較）。とくに
// Ordered:true（nftables rules 等）は verbatim なので並べ替えが diff になる
// （full-replacement semantics）。ListSep が空の unordered list は、区切りを
// 知らないため安全側に verbatim へフォールバックする（並べ替えが diff になる）。
//
// 決定的にするため空要素は落とし、各要素を TrimSpace してから sort する。
func normalizeUnorderedList(v string, spec provider.FieldSpec) string {
	if spec.Kind != core.FieldKindStringList || spec.Ordered || spec.ListSep == "" {
		return v
	}
	if strings.TrimSpace(v) == "" {
		return v
	}
	parts := strings.Split(v, spec.ListSep)
	elems := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t == "" {
			continue
		}
		elems = append(elems, t)
	}
	sort.Strings(elems)
	return strings.Join(elems, spec.ListSep)
}

// secretMaskField normalises a secret-flagged field's string value to the
// canonical SecretRef reference form so that only the ref (path + generation)
// participates in Diff — never the plaintext value (§20).
//
// Accepted inputs, all producing the ref form "secretref:<backend>/<path>#genN":
//   - the ref form itself: "secretref:file/db/root#gen3" (round-trips unchanged)
//   - the JSON form:        {"$secret":"db/root","backend":"file","gen":3}
//
// Any other value (a raw plaintext that should never reach a secret field) is
// collapsed to "***".  This is defense-in-depth: the diff engine refuses to let
// an unrecognised secret-field value survive into a FieldDiff.  By the time a
// value reaches here it has already passed ValidateDesired (which rejects a bare
// plaintext at load/validate time, S33266e-1), so the "***" branch is only ever
// hit for the *observed* (actual) side — never for a desired intent value.
func secretMaskField(v string) string {
	if ref, ok := secretRefForm(v); ok {
		return ref
	}
	// Unrecognised: never let a raw value through.
	return "***"
}

// secretRefForm returns the canonical SecretRef reference form of v and true if
// v IS a recognised SecretRef (the "secretref:...#genN" string form or the
// {"$secret":...} JSON form).  It returns ("", false) for any bare scalar — a
// value that is NOT a SecretRef.  The plaintext is never returned in either
// branch (the false branch discards v entirely).
func secretRefForm(v string) (string, bool) {
	trimmed := strings.TrimSpace(v)
	if strings.HasPrefix(trimmed, "secretref:") {
		// Already the ref form: keep it verbatim (it carries no value).
		return trimmed, true
	}
	if strings.HasPrefix(trimmed, "{") {
		var ref core.SecretRef
		if err := json.Unmarshal([]byte(trimmed), &ref); err == nil && ref.Path() != "" {
			return ref.String(), true
		}
	}
	return "", false
}

// ObservedIntentValue returns the intent (desired) value that makes an observed
// field compare equal in Diff — the inverse of the comparison in diffFields.
// It is what `sakutei import` (reverse-render, §24.4) writes for each field, so
// that a plan run right after an import reports no changes.
//
// include is false when the field must NOT be written into intent:
//   - Computed or Managed fields: Diff never compares them, and writing an
//     externally-owned value into intent would invite a tug-of-war (§7.2).
//   - Secret fields whose observed value is not a recognised SecretRef: the
//     plaintext is never exported (§20).  A recognised SecretRef is written in
//     its reference form, which carries no value.
//
// reason explains an exclusion in one short phrase (empty when included).
func ObservedIntentValue(name string, tv core.TypedValue, schema provider.Schema) (value string, include bool, reason string) {
	spec, declared := schema.Fields[name]
	if declared {
		if spec.Computed {
			return "", false, "computed (observed-only)"
		}
		if spec.Managed {
			return "", false, "managed (owned by an external system)"
		}
		if spec.Secret {
			ref, ok := secretRefForm(typedValueToString(tv))
			if !ok {
				return "", false, "secret value is not a SecretRef; plaintext is never exported, write a SecretRef by hand"
			}
			return ref, true, ""
		}
	}
	return typedValueToString(tv), true, ""
}
