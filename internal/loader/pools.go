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

	"github.com/tjst-t/sakutei/internal/diff"
)

// Pool is an allocation pool declared in intent (§19.8):
//
//	pools:
//	  - { name: site-a-vlans, range: "100-199", claim: {site: site-a} }
//
// Range is an inclusive integer range "lo-hi".  Claim is the scope selector
// the pool belongs to (pools split by claim stay disjoint — 1 pool, 1
// authority, §19.7).  v1b uses integer pools only (VLAN IDs and the like).
type Pool struct {
	Name  string
	Range string
	Lo    int
	Hi    int
	Claim map[string]string
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
		out = append(out, Pool{Name: strings.TrimSpace(name), Range: rng, Lo: lo, Hi: hi, Claim: stringifyStringMap(m["claim"])})
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
// name an undeclared pool.  Overlapping ranges are NOT rejected: pools of
// different kinds (VLAN IDs, ASNs) legitimately share numbers; keeping
// same-kind pools disjoint across sites is the operator's declaration (§19.8).
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
	for _, r := range resources {
		for field, pool := range r.Allocations {
			if _, ok := byName[pool]; !ok {
				return fmt.Errorf("resource %s: field %q requests pool %q, which is not declared under pools:", r.ID, field, pool)
			}
		}
	}
	return nil
}
