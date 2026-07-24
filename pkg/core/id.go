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

// Package core defines the canonical types shared across all of Sakutei.
// These types are the "always-wave" foundation: changing them ripples to
// every provider, so their structure is frozen here (§4).
package core

import (
	"fmt"
	"sort"
	"strings"
)

// ID is the canonical resource identifier. It is a composite key of
// (Provider, Scope, Local) as specified in ADR-0004 (§4.1).
//
// A global-unique flat string ID is intentionally avoided: when a scope
// moves during re-sharding, a flat ID would break resource attribution and
// depends_on references.  With a composite key, the Local part stays stable
// while the routing key (Scope) moves.
type ID struct {
	// Provider is the name of the provider that owns this resource.
	Provider string
	// Scope is the routing key used by claim-based sharding (§4.2, §17).
	Scope Scope
	// Local is the provider-local identifier string. The core treats it as
	// opaque; its semantics belong to the provider.
	Local string
}

// NewID constructs an ID from its three components.
func NewID(provider string, scope Scope, local string) ID {
	return ID{Provider: provider, Scope: scope, Local: local}
}

// Equal reports whether two IDs identify the same resource.
// All three components must match.
func (id ID) Equal(other ID) bool {
	return id.Provider == other.Provider &&
		id.Scope.Equal(other.Scope) &&
		id.Local == other.Local
}

// String returns a human-readable representation that includes all three
// components, e.g. "proxmox_vm/site=tokyo,cluster=prod/orders-db".
func (id ID) String() string {
	return fmt.Sprintf("%s/%s/%s", id.Provider, id.Scope.String(), id.Local)
}

// IDKey is a comparable, canonical key derived from an ID, suitable for use as
// a Go map key (§4.1).
//
// ID itself is NOT comparable — its Scope embeds a label map — so it cannot be
// a map key directly.  IDKey is the canonical, collision-free stand-in: it is
// derived from the same (Provider, Scope, Local) components with the scope
// labels in sorted order, so two IDs are Equal iff their Keys are equal — in
// BOTH directions (Equal(a,b) ⇔ a.Key()==b.Key()).
type IDKey string

// Key returns the canonical, collision-free comparable key for this ID (IDKey).
//
// Collision-freedom (FIX #8):
//
// String() is for humans and is NOT injective: it joins Provider, Scope, and
// Local with '/', and joins scope labels with ','/'='.  A value containing one
// of those separators makes distinct IDs share the same String() — e.g.
// scope {site:"x/a"}+local "b" vs scope {site:"x"}+local "a/b", or a label value
// containing ',' or '='.  Using String() as the key therefore ALIASES distinct
// IDs (the "Equal iff Keys equal" claim fails in the reverse direction).
//
// Key() instead builds a length-delimited encoding — mirroring
// internal/revision's "<name>:<len>\n<value>" technique — so no separator in any
// component can be mistaken for a delimiter.  Every component (and every scope
// label key/value) is emitted as "<len>:<bytes>", with scope labels canonicalised
// in sorted key order (the same order as Scope.String()/Path()).  Because the
// byte length precedes the bytes, the decoder boundary is unambiguous regardless
// of the bytes' content, so distinct IDs always yield distinct keys and Equal IDs
// always yield identical keys.
func (id ID) Key() IDKey {
	var b strings.Builder
	writeLenPrefixed(&b, id.Provider)
	// Scope labels in canonical (sorted key) order — same order as
	// Scope.String()/Path(), so equal label sets always serialise identically.
	keys := make([]string, 0, len(id.Scope.Labels))
	for k := range id.Scope.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// Prefix the label count so that a scope with N labels cannot be confused
	// with one that has a different number of labels but the same byte tail.
	writeLenPrefixed(&b, fmt.Sprintf("%d", len(keys)))
	for _, k := range keys {
		writeLenPrefixed(&b, k)
		writeLenPrefixed(&b, id.Scope.Labels[k])
	}
	writeLenPrefixed(&b, id.Local)
	return IDKey(b.String())
}

// writeLenPrefixed appends a length-delimited encoding of s ("<len>:<bytes>") to
// b.  The explicit byte length makes the component boundary unambiguous no matter
// what bytes s contains, which is what makes IDKey collision-free (see Key()).
func writeLenPrefixed(b *strings.Builder, s string) {
	b.WriteString(fmt.Sprintf("%d:", len(s)))
	b.WriteString(s)
}
