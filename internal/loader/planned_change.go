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

	"github.com/tjst-t/sakutei/internal/budget"
	"github.com/tjst-t/sakutei/pkg/core"
)

// parsePlannedChanges reads the intent's top-level `planned_change:` (§13.3):
// one declaration, or a list of them.
//
//	planned_change:
//	  scope: { site: hpc-a, cluster: gpu-pool }
//	  kind: reimage                    # delete | update | reimage | recreate
//	  max_count: 500
//	  window: "2026-08-01 00:00 .. 2026-08-01 06:00"   # UTC, or RFC 3339
//	  approval: { pr: 1423, approvers: [alice, bob] }
func parsePlannedChanges(v any) ([]budget.PlannedChange, error) {
	var raw []any
	switch t := v.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		raw = []any{t}
	case []any:
		raw = t
	}
	out := make([]budget.PlannedChange, 0, len(raw))
	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("planned_change[%d]: オブジェクトでなければなりません", i)
		}
		kind, _ := m["kind"].(string)
		pc := budget.PlannedChange{
			Scope: core.NewScope(stringifyStringMap(m["scope"])),
			Kind:  budget.PlannedKind(strings.TrimSpace(kind)),
		}
		pc.MaxCount, _ = m["max_count"].(int)
		window, _ := m["window"].(string)
		start, end, err := budget.ParseWindow(window)
		if err != nil {
			return nil, fmt.Errorf("planned_change[%d]: %w", i, err)
		}
		pc.Start, pc.End = start, end
		if ap, ok := m["approval"].(map[string]any); ok {
			pc.Approval.PR, _ = ap["pr"].(int)
			if names, ok := ap["approvers"].([]any); ok {
				for _, n := range names {
					pc.Approval.Approvers = append(pc.Approval.Approvers, scalarToString(n))
				}
			}
		}
		out = append(out, pc)
	}
	return out, nil
}

// validatePlannedChanges checks every declaration (budget.PlannedChange.Validate)
// and rejects the same declaration written twice, which would open its lane
// twice.
func validatePlannedChanges(pcs []budget.PlannedChange) error {
	seen := map[string]bool{}
	for _, pc := range pcs {
		if err := pc.Validate(); err != nil {
			return fmt.Errorf("planned_change (scope %s): %w", pc.Scope, err)
		}
		if seen[pc.Key()] {
			return fmt.Errorf("planned_change: %s is declared twice", pc)
		}
		seen[pc.Key()] = true
	}
	return nil
}
