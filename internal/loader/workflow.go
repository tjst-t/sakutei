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

	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/workflow"
	"github.com/tjst-t/sakutei/pkg/core"
)

// parseWorkflows reads the intent's top-level `workflows:` (§6.3, §6.4).  See
// package workflow for the format.  What v1.5 does not implement is rejected
// rather than ignored: strategy.select (dynamic selection is v2) and on_failure
// other than halt.
func parseWorkflows(v any) ([]workflow.Definition, error) {
	raw, _ := v.([]any)
	out := make([]workflow.Definition, 0, len(raw))
	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("workflows[%d]: オブジェクトでなければなりません", i)
		}
		def, err := parseWorkflow(m)
		if err != nil {
			name, _ := m["workflow"].(string)
			return nil, fmt.Errorf("workflows[%d] (%s): %w", i, name, err)
		}
		out = append(out, def)
	}
	return out, nil
}

func parseWorkflow(m map[string]any) (workflow.Definition, error) {
	var def workflow.Definition
	def.Name, _ = m["workflow"].(string)
	def.Name = strings.TrimSpace(def.Name)
	def.Applicant, _ = m["applicant"].(string)
	def.Applicant = strings.TrimSpace(def.Applicant)

	providerName, scope := "", core.NewScope(nil)
	if t, ok := m["target"].(map[string]any); ok {
		providerName, _ = t["provider"].(string)
		scope = core.NewScope(stringifyStringMap(t["scope"]))
	}
	if strings.TrimSpace(providerName) == "" {
		return def, fmt.Errorf("target.provider is required")
	}

	if inv, ok := m["invariant"].(map[string]any); ok {
		for k := range inv {
			if k != "min_active" && k != "active_when" {
				return def, fmt.Errorf("invariant.%s is not supported in v1.5 (min_active only)", k)
			}
		}
		if v, ok := inv["min_active"]; ok {
			n, isInt := toInt(v)
			if !isInt {
				return def, fmt.Errorf("invariant.min_active must be an integer")
			}
			def.Invariant.MinActive = n
		}
		def.Invariant.ActiveWhen = stringifyStringMap(inv["active_when"])
	}

	if st, ok := m["strategy"].(map[string]any); ok {
		if order, _ := st["order"].(string); order != "" && order != "one-at-a-time" {
			return def, fmt.Errorf("strategy.order %q is not supported (one-at-a-time only)", order)
		}
		if _, ok := st["select"]; ok {
			return def, fmt.Errorf("strategy.select is v2 (dynamic selection); v1.5 processes targets in the declared order")
		}
	}

	if of, ok := m["on_failure"].(map[string]any); ok {
		if policy, _ := of["policy"].(string); policy != "" && policy != "halt" {
			return def, fmt.Errorf("on_failure.policy %q is not supported (halt only)", policy)
		}
	}

	stages, _ := m["stages"].([]any)
	for i, s := range stages {
		sm, _ := s.(map[string]any)
		var st workflow.Stage
		st.Name, _ = sm["name"].(string)
		st.Name = strings.TrimSpace(st.Name)
		targets, _ := sm["targets"].([]any)
		for _, t := range targets {
			st.Targets = append(st.Targets, core.NewID(providerName, scope, strings.TrimSpace(scalarToString(t))))
		}
		if g, ok := sm["gate"].(map[string]any); ok {
			gate, err := parseGate(g)
			if err != nil {
				return def, fmt.Errorf("stages[%d] (%s): %w", i, st.Name, err)
			}
			st.Gate = gate
		}
		def.Stages = append(def.Stages, st)
	}

	steps, _ := m["steps"].([]any)
	for i, s := range steps {
		step, err := parseStep(s)
		if err != nil {
			return def, fmt.Errorf("steps[%d]: %w", i, err)
		}
		def.Steps = append(def.Steps, step)
	}
	return def, def.Validate()
}

// parseGate reads a stage gate.  manual_approval: required is a single
// approval; manual_approval: { requirements: {...} } adds the approver
// requirements of §14.4 (quorum / roles / separation).
func parseGate(g map[string]any) (*workflow.Gate, error) {
	gate := &workflow.Gate{Timeout: workflow.DefaultGateTimeout}
	switch ma := g["manual_approval"].(type) {
	case string:
		if ma != "required" {
			return nil, fmt.Errorf("gate.manual_approval must be \"required\"")
		}
	case map[string]any:
		for k := range ma {
			if k != "requirements" {
				return nil, fmt.Errorf("gate.manual_approval.%s is not supported (requirements only)", k)
			}
		}
		if raw, ok := ma["requirements"]; ok {
			req, err := parseRequirements(raw)
			if err != nil {
				return nil, fmt.Errorf("gate.manual_approval.requirements: %w", err)
			}
			gate.Requirements = req
		}
	case nil:
		return nil, fmt.Errorf("gate.manual_approval is required (health_check gates are v2)")
	}
	if _, ok := g["health_check"]; ok {
		return nil, fmt.Errorf("gate.health_check is v2")
	}
	if ts, ok := g["timeout"].(string); ok {
		d, err := time.ParseDuration(strings.TrimSpace(ts))
		if err != nil {
			return nil, fmt.Errorf("gate.timeout: %w", err)
		}
		gate.Timeout = d
	}
	return gate, nil
}

// parseRequirements reads { quorum: { n, of }, roles: [...], separation:
// applicant_ne_approver } (§14.4).  Unknown keys are rejected: a misspelled
// requirement would silently weaken the gate.
func parseRequirements(v any) (*workflow.Requirements, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("must be an object { quorum, roles, separation }")
	}
	req := &workflow.Requirements{}
	for k, val := range m {
		switch k {
		case "quorum":
			q, ok := val.(map[string]any)
			if !ok || len(q) != 2 {
				return nil, fmt.Errorf("quorum must be { n: <int>, of: <int> }")
			}
			n, nok := toInt(q["n"])
			of, ofok := toInt(q["of"])
			if !nok || !ofok {
				return nil, fmt.Errorf("quorum must be { n: <int>, of: <int> }")
			}
			req.Quorum = &workflow.Quorum{N: n, Of: of}
		case "roles":
			list, ok := val.([]any)
			if !ok || len(list) == 0 {
				return nil, fmt.Errorf("roles must be a non-empty list of role names")
			}
			for _, r := range list {
				role, _ := r.(string)
				req.Roles = append(req.Roles, strings.TrimSpace(role))
			}
		case "separation":
			req.Separation, _ = val.(string)
			req.Separation = strings.TrimSpace(req.Separation)
			if req.Separation == "" {
				return nil, fmt.Errorf("separation must be %q", workflow.SeparationApplicantNeApprover)
			}
		default:
			return nil, fmt.Errorf("unknown requirement %q (quorum | roles | separation)", k)
		}
	}
	return req, nil
}

// parseStep reads one step: an object with exactly one of apply / task /
// wait / require / verify.
//
// apply takes either the fields of the target directly (apply: { active:
// "false" }) or, to name the resource it touches (#87), { resource:
// <provider>/<id>, fields: {...} } — recognized by a `fields` key holding an
// object.  wait and require take { resource, fields, timeout } (resource
// optional; require's timeout is mandatory).
func parseStep(v any) (workflow.Step, error) {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		return workflow.Step{}, fmt.Errorf("a step is an object with exactly one of apply / task / wait / require / verify")
	}
	for k, val := range m {
		step := workflow.Step{Kind: workflow.StepKind(k)}
		switch step.Kind {
		case workflow.StepApply, workflow.StepTask:
			am, _ := val.(map[string]any)
			if _, nested := am["fields"].(map[string]any); !nested {
				step.Fields = stringifyStringMap(val)
				break
			}
			if step.Kind == workflow.StepTask {
				return workflow.Step{}, fmt.Errorf("task takes the target's fields directly (a task runs on the target; resource is v2's polymorphic Task)")
			}
			for key := range am {
				if key != "resource" && key != "fields" {
					return workflow.Step{}, fmt.Errorf("apply.%s is not supported (resource | fields)", key)
				}
			}
			if err := parseStepResource(&step, am); err != nil {
				return workflow.Step{}, err
			}
			step.Fields = stringifyStringMap(am["fields"])
		case workflow.StepWait, workflow.StepRequire:
			wm, _ := val.(map[string]any)
			for key := range wm {
				if key != "resource" && key != "fields" && key != "timeout" {
					return workflow.Step{}, fmt.Errorf("%s.%s is not supported (resource | fields | timeout)", k, key)
				}
			}
			if err := parseStepResource(&step, wm); err != nil {
				return workflow.Step{}, err
			}
			step.Fields = stringifyStringMap(wm["fields"])
			if ts, ok := wm["timeout"].(string); ok {
				d, err := time.ParseDuration(strings.TrimSpace(ts))
				if err != nil {
					return workflow.Step{}, fmt.Errorf("%s.timeout: %w", k, err)
				}
				step.Timeout = d
			}
		case workflow.StepVerify:
			// verify: invariant  /  verify: { invariant: min_active }
			ok := false
			switch t := val.(type) {
			case string:
				ok = t == "invariant"
			case map[string]any:
				inv, _ := t["invariant"].(string)
				ok = len(t) == 1 && inv == "min_active"
			}
			if !ok {
				return workflow.Step{}, fmt.Errorf("verify must be \"invariant\" or { invariant: min_active }")
			}
		default:
			return workflow.Step{}, fmt.Errorf("unknown step %q (apply | task | wait | require | verify)", k)
		}
		return step, nil
	}
	return workflow.Step{}, nil
}

// parseStepResource reads the step's optional `resource: <provider>/<id>`.
func parseStepResource(step *workflow.Step, m map[string]any) error {
	raw, ok := m["resource"]
	if !ok {
		return nil
	}
	ref, isString := raw.(string)
	if !isString || strings.TrimSpace(ref) == "" {
		return fmt.Errorf("%s.resource must be \"<provider>/<id>\"", step.Kind)
	}
	step.Resource = strings.TrimSpace(ref)
	return nil
}

// validateWorkflows rejects two workflows with the same name (the name is the
// API's {id}) and a resource touched by two workflows (as a target or by a
// step's resource, #87) or also declared as a strategy: task resource (two
// procedures would drive the same resource).
func validateWorkflows(defs []workflow.Definition, resources []diff.DesiredResource) error {
	names := map[string]bool{}
	owner := map[string]string{}
	for _, def := range defs {
		if names[def.Name] {
			return fmt.Errorf("workflow %s is declared twice", def.Name)
		}
		names[def.Name] = true
		for _, id := range def.Resources() {
			if other, ok := owner[id.Local]; ok {
				return fmt.Errorf("target %s is in both workflow %s and %s", id.Local, other, def.Name)
			}
			owner[id.Local] = def.Name
		}
	}
	for _, r := range resources {
		if name, ok := owner[r.ID.Local]; ok && r.UpdatePolicy.IsTask() {
			return fmt.Errorf("resource %s is a target of workflow %s and also declares strategy: task; drive it with one of them", r.ID.Local, name)
		}
	}
	return nil
}
