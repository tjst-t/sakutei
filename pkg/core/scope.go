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

package core

import (
	"fmt"
	"sort"
	"strings"
)

// Scope is a provider-agnostic label set that acts as the routing key for
// claim-based sharding (§4.2, ADR-0004).
//
// The same vocabulary is shared across four uses:
//   - claim scope-selector (§17): "--claim {site: site-a}" cuts the boundary.
//   - prune boundary (§12): closed-world set operations are scoped here.
//   - intent/<scope>/ path convention (§17): Path() maps 1:1 to the git tree.
//   - rendered_revision computation unit (§9): Path() is the granularity.
//
// Using a typed struct per zone/cluster would require a core change for every
// new provider; a single label-map absorbs that without touching the core.
type Scope struct {
	// Labels is the label set, e.g. {"site": "tokyo", "cluster": "prod"}.
	Labels map[string]string
}

// NewScope constructs a Scope from a label map.
func NewScope(labels map[string]string) Scope {
	cp := make(map[string]string, len(labels))
	for k, v := range labels {
		cp[k] = v
	}
	return Scope{Labels: cp}
}

// sortedParts returns the label set as a deterministic "k=v" slice, with keys
// sorted lexicographically so the same label set always yields the same order.
// It is the single source of the ordering shared by Path and String.
func (s Scope) sortedParts() []string {
	keys := make([]string, 0, len(s.Labels))
	for k := range s.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, s.Labels[k]))
	}
	return parts
}

// Path returns a deterministic string derived from the label set, suitable
// for use as an intent/<scope>/... directory path and as the unit of
// rendered_revision computation.
//
// The path is stable: keys are sorted lexicographically so that the same
// label set always produces the same string.
func (s Scope) Path() string {
	if len(s.Labels) == 0 {
		return "intent/_root"
	}
	return "intent/" + strings.Join(s.sortedParts(), ",")
}

// String returns a compact human-readable representation of the scope,
// e.g. "cluster=prod,site=tokyo".
func (s Scope) String() string {
	if len(s.Labels) == 0 {
		return ""
	}
	return strings.Join(s.sortedParts(), ",")
}

// Matches reports whether this scope satisfies the given selector (§17.1).
//
// A selector is itself a Scope whose Labels express the claim constraint,
// e.g. --claim {site: tokyo} → NewScope({"site": "tokyo"}).  This scope
// matches when it carries EVERY label in the selector with the same value
// (subset / superset semantics): the selector is a filter, and a resource
// scope is in the claim iff it agrees with the filter on all selector keys.
// Labels present on this scope but absent from the selector are ignored —
// they are finer-grained routing keys the claim does not constrain.
//
// An empty selector (no labels) matches every scope: "--claim" was not set,
// so the whole world is in scope (the daemon's default single-shard behaviour).
//
// This is the one matcher shared by the claim boundary; the sparse-checkout /
// git-distribution machinery that computes WHICH selector a shard holds is
// v1b (§17.4).  Here Matches is the seam that decides in-scope vs out-of-scope.
func (s Scope) Matches(selector Scope) bool {
	for k, v := range selector.Labels {
		if s.Labels[k] != v {
			return false
		}
	}
	return true
}

// ParseScope parses the shared CLI/config scope syntax: comma-separated
// "key=value" pairs, e.g. "site=tokyo,cluster=prod" (used by --claim, §17,
// and by a provider's static scope config, Backlog #106, §27.5).  An empty
// string yields an empty Scope (the root scope: no labels).  Whitespace
// around keys/values is trimmed.
func ParseScope(s string) (Scope, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return NewScope(nil), nil
	}
	labels := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if !ok || k == "" {
			return Scope{}, fmt.Errorf("scope term %q is not a key=value pair", part)
		}
		labels[k] = v
	}
	return NewScope(labels), nil
}

// Equal reports whether two Scopes have identical label sets.
func (s Scope) Equal(other Scope) bool {
	if len(s.Labels) != len(other.Labels) {
		return false
	}
	for k, v := range s.Labels {
		if other.Labels[k] != v {
			return false
		}
	}
	return true
}
