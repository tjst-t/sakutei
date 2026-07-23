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
	keys := make([]string, 0, len(s.Labels))
	for k := range s.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, s.Labels[k]))
	}
	return "intent/" + strings.Join(parts, ",")
}

// String returns a compact human-readable representation of the scope,
// e.g. "cluster=prod,site=tokyo".
func (s Scope) String() string {
	if len(s.Labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(s.Labels))
	for k := range s.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, s.Labels[k]))
	}
	return strings.Join(parts, ",")
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
