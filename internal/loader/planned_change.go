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
	"time"

	"github.com/tjst-t/sakutei/v2/internal/budget"
	"github.com/tjst-t/sakutei/v2/internal/workflow"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// plannedDecl is one planned_change as written: the declaration plus what the
// loader needs to resolve its gate (§14.4 / §14.5).
type plannedDecl struct {
	pc budget.PlannedChange
	// requirements are the declaration's own approver requirements (nil: the
	// profile's default).
	requirements *workflow.Requirements
	// approvals are approval.approvers as approvals (with the roles each
	// asserts).
	approvals []workflow.Approval
	// coolingOff is the declaration's own cooling_off (nil: the profile's).
	coolingOff *time.Duration
}

// parsePlannedChanges reads the intent's top-level `planned_change:` (§13.3):
// one declaration, or a list of them.
//
//	planned_change:
//	  scope: { site: hpc-a, cluster: gpu-pool }
//	  kind: reimage                    # delete | update | reimage | recreate
//	  max_count: 500
//	  window: "2026-08-01 00:00 .. 2026-08-01 06:00"   # UTC, or RFC 3339
//	  applicant: carol                 # the PR author (separation, §14.4)
//	  approval: { pr: 1423, approvers: [alice, bob] }  # or { id, roles: [...] }
//	  requirements: { quorum: { n: 2, of: 3 } }        # overrides the profile
//	  cooling_off: 2h                                  # overrides the profile
func parsePlannedChanges(v any) ([]plannedDecl, error) {
	var raw []any
	switch t := v.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		raw = []any{t}
	case []any:
		raw = t
	}
	out := make([]plannedDecl, 0, len(raw))
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
		pc.Applicant, _ = m["applicant"].(string)
		pc.Applicant = strings.TrimSpace(pc.Applicant)
		d := plannedDecl{}
		if ap, ok := m["approval"].(map[string]any); ok {
			pc.Approval.PR, _ = ap["pr"].(int)
			if names, ok := ap["approvers"].([]any); ok {
				for _, n := range names {
					a, err := parsePlannedApprover(n)
					if err != nil {
						return nil, fmt.Errorf("planned_change[%d]: approval.approvers: %w", i, err)
					}
					pc.Approval.Approvers = append(pc.Approval.Approvers, a.Approver)
					d.approvals = append(d.approvals, a)
				}
			}
		}
		if rv, ok := m["requirements"]; ok {
			req, err := parseRequirements(rv)
			if err != nil {
				return nil, fmt.Errorf("planned_change[%d]: requirements: %w", i, err)
			}
			if err := req.Validate(); err != nil {
				return nil, fmt.Errorf("planned_change[%d]: %w", i, err)
			}
			d.requirements = req
		}
		if cv, ok := m["cooling_off"]; ok {
			c, err := parseNonNegDuration(cv)
			if err != nil {
				return nil, fmt.Errorf("planned_change[%d]: cooling_off: %w", i, err)
			}
			d.coolingOff = &c
		}
		d.pc = pc
		out = append(out, d)
	}
	return out, nil
}

// parsePlannedApprover reads one approval.approvers entry: an ID, or
// { id, roles: [...] } (the roles the approver asserts; their truth is the
// upstream IdP's, §7).
func parsePlannedApprover(v any) (workflow.Approval, error) {
	a := workflow.Approval{Decision: workflow.DecisionApprove}
	switch t := v.(type) {
	case map[string]any:
		for k := range t {
			if k != "id" && k != "roles" {
				return a, fmt.Errorf("unknown key %q (id | roles)", k)
			}
		}
		a.Approver = strings.TrimSpace(scalarToString(t["id"]))
		roles, _ := t["roles"].([]any)
		for _, r := range roles {
			a.Roles = append(a.Roles, strings.TrimSpace(scalarToString(r)))
		}
	default:
		a.Approver = strings.TrimSpace(scalarToString(v))
	}
	if a.Approver == "" {
		return a, fmt.Errorf("an approver needs an id")
	}
	return a, nil
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
