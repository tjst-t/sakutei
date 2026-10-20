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

// Package render implements render phase 2 (§9.1, §9.5): deterministic
// expansion of composite resources into concrete child resources.
//
// # Story S072a14-1 scope
//
// This package implements render phase 2 only (§9.1 フェーズ2: 決定的展開,
// 副作用なし).  Phase 1 (external input resolution, §9.1 フェーズ1) is
// implemented in S072a14-2.
//
// # Composite resource intent format (PROVISIONAL)
//
// IMPORTANT: The intent format for composite resources is PROVISIONAL.  The
// production Loader with full JSON Schema validation is Story S33266e, which
// will formalise the schema and may change the format.  Do not treat the
// current "kind" / "composite" / "children" fields as stable API.
//
// Composite resources are recognised in intent files by the field
// "kind": "composite".  The ExpanderRegistry maps provider+kind to an Expander
// that produces the concrete child DesiredResource values.
//
// # Determinism
//
// All Expanders MUST be deterministic: the same parent input always produces
// the same child set in the same order.  This is the render phase 2 contract
// (§9.1) and is required for rendered_revision stability (§9.3).
//
// # Child ownership (§7.5)
//
// Each expanded child carries ManagedBy set to the parent's ID.Local so the
// convergence loop can track parent→child ownership, propagate prune scope,
// and report aggregated status (§9.7).
package render

import (
	"fmt"

	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/pkg/core"
)

// CompositeKind is the "kind" field value that marks a resource as a composite
// (to be expanded in phase 2).  PROVISIONAL — see package doc.
const CompositeKind = "composite"

// ManagedByField is the field name used to record the parent composite ID in
// each expanded child resource (§7.5).
const ManagedByField = "managed_by"

// ExpandedResource extends DesiredResource with composite-expansion metadata
// that the convergence loop uses for DAG scheduling and status aggregation.
type ExpandedResource struct {
	diff.DesiredResource

	// ManagedBy holds the Local ID of the composite parent that produced this
	// child.  Empty for non-composite (directly declared) resources.
	ManagedBy string

	// IsComposite is true for the original composite resource.  Composites are
	// NOT passed to Observe/Diff/Apply — only their children are.
	IsComposite bool
}

// Expander expands a single composite DesiredResource into one or more concrete
// child DesiredResources.
//
// Contract:
//   - Must be deterministic: same input → same output, always.
//   - Must not call any external service (no side effects; §9.1 フェーズ2).
//   - Each returned child must have ManagedBy set to parent.ID.Local (§7.5).
type Expander interface {
	// Expand returns the child resources for the given composite parent.
	// The returned slice must be stable across identical calls.
	Expand(parent diff.DesiredResource) ([]ExpandedResource, error)
}

// ExpanderRegistry maps (provider, kind) to the Expander responsible for that
// composite type.  The zero value is an empty, usable registry.
type ExpanderRegistry struct {
	expanders map[expanderKey]Expander
}

type expanderKey struct {
	provider string
	kind     string
}

// Register adds an Expander for the given provider and kind.
// Panics if an Expander for the same (provider, kind) is already registered
// (programming error — registrations happen at init time).
func (r *ExpanderRegistry) Register(providerName, kind string, exp Expander) {
	if r.expanders == nil {
		r.expanders = make(map[expanderKey]Expander)
	}
	key := expanderKey{provider: providerName, kind: kind}
	if _, exists := r.expanders[key]; exists {
		panic(fmt.Sprintf("render: duplicate Expander for provider=%q kind=%q", providerName, kind))
	}
	r.expanders[key] = exp
}

// Lookup returns the Expander for (providerName, kind), or nil if none is
// registered.
func (r *ExpanderRegistry) Lookup(providerName, kind string) Expander {
	if r.expanders == nil {
		return nil
	}
	return r.expanders[expanderKey{provider: providerName, kind: kind}]
}

// ExpandAll processes the intent resources and:
//  1. Passes non-composite resources through unchanged (IsComposite=false,
//     ManagedBy="").
//  2. Expands composite resources (kind==CompositeKind) using the registered
//     Expander, marking each child with ManagedBy = parent.ID.Local (§7.5).
//  3. Composite parents themselves are included in the returned slice with
//     IsComposite=true but are NOT intended for Observe/Diff/Apply — callers
//     must filter on IsComposite before driving the convergence loop.
//
// The order of the output slice is deterministic: non-composite resources come
// first in their original order, then composite parents followed immediately
// by their children in Expander-defined order.  This ordering stability is
// required for rendered_revision determinism (§9.3).
//
// Returns an error if a composite resource has no registered Expander.
func ExpandAll(registry *ExpanderRegistry, intent []diff.DesiredResource) ([]ExpandedResource, error) {
	var out []ExpandedResource

	for _, res := range intent {
		kind := res.Fields["kind"]
		if kind != CompositeKind {
			// Plain resource: pass through unchanged.
			out = append(out, ExpandedResource{DesiredResource: res})
			continue
		}

		// Composite resource: find the Expander.
		exp := registry.Lookup(res.ID.Provider, kind)
		if exp == nil {
			return nil, fmt.Errorf("render: no Expander registered for provider=%q kind=%q (resource %s)",
				res.ID.Provider, kind, res.ID)
		}

		// Include the composite parent (IsComposite=true) in the output so
		// callers can trace parent→child relationships.
		out = append(out, ExpandedResource{DesiredResource: res, IsComposite: true})

		// Expand into children.
		children, err := exp.Expand(res)
		if err != nil {
			return nil, fmt.Errorf("render: expand %s: %w", res.ID, err)
		}
		for _, child := range children {
			// Enforce §7.5: child must carry the parent's Local ID.
			if child.ManagedBy == "" {
				child.ManagedBy = res.ID.Local
			}
			// #30 (§16.5): a child is observed at its parent's tier unless
			// the expander chose one.
			if child.Tier == "" {
				child.Tier = res.Tier
			}
			out = append(out, child)
		}
	}

	return out, nil
}

// FakeCompositeExpander is the built-in fake Expander used for acceptance
// tests (S072a14-1).  It expands one "fake" composite into exactly two
// children: <parent-id>-child-a and <parent-id>-child-b.
//
// The children inherit the parent's Scope and use provider "fake".  Each child
// gets a "name" field equal to its own ID and a "managed_by" field set to the
// parent's Local ID (§7.5).  A parent "vlan" field is copied to each child.
//
// FakeCompositeExpander is deterministic by construction (no time/random calls).
type FakeCompositeExpander struct{}

// Expand returns two deterministic child resources for the given composite parent.
func (FakeCompositeExpander) Expand(parent diff.DesiredResource) ([]ExpandedResource, error) {
	suffixes := []string{"child-a", "child-b"}
	children := make([]ExpandedResource, 0, len(suffixes))

	for _, suffix := range suffixes {
		childLocal := parent.ID.Local + "-" + suffix
		childID := core.NewID(parent.ID.Provider, parent.ID.Scope, childLocal)
		childFields := map[string]string{
			"name":        childLocal,
			ManagedByField: parent.ID.Local,
		}
		// #24: a parent "vlan" (typically allocated from a pool in render
		// phase 1) reaches every child, the way a virtual_network's VLAN
		// reaches its vds_portgroup (§9.5).  Only when present, so existing
		// composites expand exactly as before.
		if v, ok := parent.Fields["vlan"]; ok {
			childFields["vlan"] = v
		}
		child := ExpandedResource{
			DesiredResource: diff.DesiredResource{
				ID:     childID,
				Fields: childFields,
			},
			ManagedBy: parent.ID.Local,
		}
		children = append(children, child)
	}

	return children, nil
}

// DefaultRegistry returns a new ExpanderRegistry pre-loaded with the fake
// composite expander for testing.
func DefaultRegistry() *ExpanderRegistry {
	r := &ExpanderRegistry{}
	r.Register("fake", CompositeKind, FakeCompositeExpander{})
	return r
}
