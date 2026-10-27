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

package loader

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// Pool is an allocation pool declared in intent (§19.8):
//
//	pools:
//	  - { name: site-a-vlans, range: "100-199", claim: {site: site-a} }
//
// Range is an inclusive integer range "lo-hi".  Claim is the scope selector
// the pool belongs to: the shard of that claim alone declares and allocates
// from it (pool split by claim, §19.8).  Space names the numbering space
// ("vlan", "asn"; default: one common space) within which pools of different
// claims must be disjoint — 1 value, 1 authority (§19.7).  v1b uses integer
// pools only (VLAN IDs and the like).
type Pool struct {
	Name  string
	Range string
	Lo    int
	Hi    int
	Claim map[string]string
	Space string
}

func parsePools(v any) ([]Pool, error) {
	raw, _ := v.([]any)
	out := make([]Pool, 0, len(raw))
	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("pools[%d]: オブジェクトでなければなりません", i)
		}
		name, _ := m["name"].(string)
		rng := scalarToString(m["range"])
		lo, hi, err := parseRange(rng)
		if err != nil {
			return nil, fmt.Errorf("pools[%d] (%s): %w", i, name, err)
		}
		space, _ := m["space"].(string)
		out = append(out, Pool{Name: strings.TrimSpace(name), Range: rng, Lo: lo, Hi: hi,
			Claim: stringifyStringMap(m["claim"]), Space: strings.TrimSpace(space)})
	}
	return out, nil
}

// parseRange parses "lo-hi" (inclusive, non-negative integers, lo <= hi).
func parseRange(s string) (int, int, error) {
	a, b, ok := strings.Cut(strings.TrimSpace(s), "-")
	if !ok {
		return 0, 0, fmt.Errorf("range %q must be \"lo-hi\"", s)
	}
	lo, err1 := strconv.Atoi(strings.TrimSpace(a))
	hi, err2 := strconv.Atoi(strings.TrimSpace(b))
	if err1 != nil || err2 != nil || lo < 0 || lo > hi {
		return 0, 0, fmt.Errorf("range %q must be \"lo-hi\" with integers 0 <= lo <= hi", s)
	}
	return lo, hi, nil
}

// validatePools rejects duplicate pool names and allocation requests that
// name an undeclared pool.
//
// Pool split by claim (§19.8): two pools of the same space with different
// claims, at least one of them claimed, must not overlap — otherwise two
// shards' authorities could hand out the same value (§19.7).  And a resource
// may request a claimed pool only from inside that claim.  Pools without any
// claim may overlap: pools of different kinds (VLAN IDs, ASNs) legitimately
// share numbers.
func validatePools(pools []Pool, resources []diff.DesiredResource) error {
	byName := map[string]Pool{}
	for _, p := range pools {
		if p.Name == "" {
			return fmt.Errorf("pool with empty name")
		}
		if _, dup := byName[p.Name]; dup {
			return fmt.Errorf("pool %q declared twice", p.Name)
		}
		byName[p.Name] = p
	}
	for i, a := range pools {
		for _, b := range pools[i+1:] {
			if a.Space != b.Space || (len(a.Claim) == 0 && len(b.Claim) == 0) {
				continue
			}
			if core.NewScope(a.Claim).Equal(core.NewScope(b.Claim)) {
				continue
			}
			if a.Lo <= b.Hi && b.Lo <= a.Hi {
				return fmt.Errorf("pools %q (%s, claim %v) and %q (%s, claim %v) overlap: pools split by claim must be disjoint (§19.8); declare a different space: if they number different things",
					a.Name, a.Range, a.Claim, b.Name, b.Range, b.Claim)
			}
		}
	}
	for _, r := range resources {
		for field, pool := range r.Allocations {
			p, ok := byName[pool]
			if !ok {
				return fmt.Errorf("resource %s: field %q requests pool %q, which is not declared under pools:", r.ID, field, pool)
			}
			if !r.ID.Scope.Matches(core.NewScope(p.Claim)) {
				return fmt.Errorf("resource %s: field %q requests pool %q of claim %v from outside that claim (pools split by claim, §19.8)", r.ID, field, pool, p.Claim)
			}
		}
	}
	return nil
}
