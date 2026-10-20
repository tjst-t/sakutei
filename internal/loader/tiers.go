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
	"strings"

	"github.com/tjst-t/sakutei/internal/diff"
)

// Tier is an Observe-frequency tier declared in intent (§16.5):
//
//	tiers:
//	  - { name: ansible, observe_every: 5 }   # 5 周に1回
//
// A resource joins a tier with `tier: <name>`.  A resource with no tier is
// Observed every cycle (the §8 default).  ObserveEvery is the number of
// cycles between two Observes of a converged resource; 1 means every cycle.
type Tier struct {
	Name         string
	ObserveEvery int
}

func parseTiers(v any) ([]Tier, error) {
	raw, _ := v.([]any)
	out := make([]Tier, 0, len(raw))
	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tiers[%d]: オブジェクトでなければなりません", i)
		}
		name, _ := m["name"].(string)
		every, _ := m["observe_every"].(int)
		out = append(out, Tier{Name: strings.TrimSpace(name), ObserveEvery: every})
	}
	return out, nil
}

// validateTiers rejects empty or duplicate tier names, observe_every < 1, and
// resources that name an undeclared tier.  An undeclared tier is an error, not
// a silent "every cycle": a typo would otherwise hide the declaration.
func validateTiers(tiers []Tier, resources []diff.DesiredResource) error {
	byName := map[string]bool{}
	for _, t := range tiers {
		if t.Name == "" {
			return fmt.Errorf("tiers: a tier has an empty name")
		}
		if byName[t.Name] {
			return fmt.Errorf("tiers: tier %q declared twice", t.Name)
		}
		if t.ObserveEvery < 1 {
			return fmt.Errorf("tiers: tier %q: observe_every must be >= 1 (got %d)", t.Name, t.ObserveEvery)
		}
		byName[t.Name] = true
	}
	for _, r := range resources {
		if r.Tier != "" && !byName[r.Tier] {
			return fmt.Errorf("resource %s: tier %q is not declared under tiers:", r.ID, r.Tier)
		}
	}
	return nil
}

// TierIntervals returns tier name → observe_every for the declared tiers.
func (in Intent) TierIntervals() map[string]int {
	out := make(map[string]int, len(in.Tiers))
	for _, t := range in.Tiers {
		out[t.Name] = t.ObserveEvery
	}
	return out
}
