// Copyright 2026 tjst-t
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package loader

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/diff"
)

// ProviderApplyCheck is a top-level `apply_check:` entry (§11.1, Backlog
// #166): the settle and the stop limits of one provider type.
//
//	apply_check:
//	  - { provider: redfish, settle: 60s, max_ineffective_applies: 3, max_ineffective_duration: 30m }
//
// A resource may declare its own `apply_check:` (the same keys but provider),
// which wins over its type's.
type ProviderApplyCheck struct {
	Provider string
	diff.ApplyCheck
}

// applyCheckKeys are the keys of an apply_check declaration (a top-level
// entry also has provider).
var applyCheckKeys = map[string]bool{"settle": true, "max_ineffective_applies": true, "max_ineffective_duration": true}

// parseApplyCheck reads the settle / limit keys of an apply_check
// declaration.  where names it in errors.
func parseApplyCheck(m map[string]any, where string) (diff.ApplyCheck, error) {
	var ac diff.ApplyCheck
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := m[k]
		switch k {
		case "provider":
		case "settle", "max_ineffective_duration":
			s, _ := v.(string)
			d, err := time.ParseDuration(strings.TrimSpace(s))
			if err != nil || d < 0 {
				return ac, fmt.Errorf("%s: %s %v: want a duration >= 0 such as 30s or 10m", where, k, v)
			}
			if k == "settle" {
				ac.Settle = &d
			} else {
				ac.MaxIneffectiveDuration = &d
			}
		case "max_ineffective_applies":
			n, ok := v.(int)
			if !ok || n < 1 {
				return ac, fmt.Errorf("%s: max_ineffective_applies %v: want an integer >= 1", where, v)
			}
			ac.MaxIneffectiveApplies = &n
		default:
			return ac, fmt.Errorf("%s: unknown key %q (settle / max_ineffective_applies / max_ineffective_duration)", where, k)
		}
	}
	return ac, nil
}

func parseApplyChecks(v any) ([]ProviderApplyCheck, error) {
	raw, _ := v.([]any)
	out := make([]ProviderApplyCheck, 0, len(raw))
	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("apply_check[%d]: オブジェクトでなければなりません", i)
		}
		name, _ := m["provider"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("apply_check[%d]: provider is required (the provider type the settle and limits are for)", i)
		}
		ac, err := parseApplyCheck(m, fmt.Sprintf("apply_check[%d] (provider %s)", i, name))
		if err != nil {
			return nil, err
		}
		out = append(out, ProviderApplyCheck{Provider: name, ApplyCheck: ac})
	}
	return out, nil
}

// validateApplyChecks rejects a provider type declared twice (across files).
func validateApplyChecks(acs []ProviderApplyCheck) error {
	seen := map[string]bool{}
	for _, ac := range acs {
		if seen[ac.Provider] {
			return fmt.Errorf("apply_check: provider %q declared twice; declare a type's settle and limits once", ac.Provider)
		}
		seen[ac.Provider] = true
	}
	return nil
}

// ApplyCheckByProvider returns the top-level apply_check declarations by
// provider type.
func (in Intent) ApplyCheckByProvider() map[string]diff.ApplyCheck {
	out := make(map[string]diff.ApplyCheck, len(in.ApplyChecks))
	for _, ac := range in.ApplyChecks {
		out[ac.Provider] = ac.ApplyCheck
	}
	return out
}
