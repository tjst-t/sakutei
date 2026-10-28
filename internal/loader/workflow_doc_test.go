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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/workflow"
)

// workflowDocExample returns the YAML example in package workflow's doc
// comment: the indented block ("//\t") that starts with "workflows:".
func workflowDocExample(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "workflow", "workflow.go"))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	in := false
	for _, line := range strings.Split(string(src), "\n") {
		body, isCode := strings.CutPrefix(line, "//\t")
		if !in {
			if isCode && body == "workflows:" {
				in = true
			} else {
				continue
			}
		}
		if !isCode {
			break
		}
		b.WriteString(body + "\n")
	}
	if b.Len() == 0 {
		t.Fatal("workflow.go の説明に workflows: の例が見つかりません")
	}
	return b.String()
}

// #90: package workflow の説明の例は、そのまま intent に写して読み込める
// （フロースタイルを使っていない）。
func TestWorkflowDocExampleLoads(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "workflow.yaml"), []byte(workflowDocExample(t)), 0o644); err != nil {
		t.Fatal(err)
	}
	in, err := LoadIntent(dir)
	if err != nil {
		t.Fatalf("説明の例が読み込めません: %v", err)
	}
	if len(in.Workflows) != 1 {
		t.Fatalf("workflows = %+v", in.Workflows)
	}
	def := in.Workflows[0]
	if def.Name != "rolling-upgrade" || def.Applicant != "alice" {
		t.Errorf("name/applicant = %q/%q", def.Name, def.Applicant)
	}
	if def.Invariant.MinActive != 2 || def.Invariant.ActiveWhen["active"] != "true" {
		t.Errorf("invariant = %+v", def.Invariant)
	}
	if len(def.Stages) != 2 {
		t.Fatalf("stages = %+v", def.Stages)
	}
	s1, s2 := def.Stages[0], def.Stages[1]
	if len(s1.Targets) != 1 || s1.Targets[0].Provider != "fake" || s1.Targets[0].Local != "n1" {
		t.Errorf("stages[0].targets = %+v", s1.Targets)
	}
	if s1.Gate == nil || s1.Gate.Timeout != 24*time.Hour || s1.Gate.Requirements != nil {
		t.Errorf("stages[0].gate = %+v", s1.Gate)
	}
	if s2.Gate == nil || s2.Gate.Requirements == nil {
		t.Fatalf("stages[1].gate = %+v", s2.Gate)
	}
	req := s2.Gate.Requirements
	if req.Quorum == nil || req.Quorum.N != 2 || req.Quorum.Of != 3 ||
		len(req.Roles) != 1 || req.Roles[0] != "platform-admin" ||
		req.Separation != workflow.SeparationApplicantNeApprover {
		t.Errorf("stages[1].gate.requirements = %+v", req)
	}
	kinds := []workflow.StepKind{workflow.StepApply, workflow.StepTask, workflow.StepWait, workflow.StepApply, workflow.StepVerify}
	if len(def.Steps) != len(kinds) {
		t.Fatalf("steps = %+v", def.Steps)
	}
	for i, k := range kinds {
		if def.Steps[i].Kind != k {
			t.Errorf("steps[%d].kind = %q, want %q", i, def.Steps[i].Kind, k)
		}
	}
	if def.Steps[0].Fields["active"] != "false" || def.Steps[2].Fields["version"] != "2" || def.Steps[2].Timeout != 5*time.Minute {
		t.Errorf("steps = %+v", def.Steps)
	}
}

// #152: Disruption の説明の例（"disruption:" で始まる "//\t" の塊）は、そのまま
// Workflow に書いて読み込める。
func TestDisruptionDocExampleLoads(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "workflow", "workflow.go"))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	in := false
	for _, line := range strings.Split(string(src), "\n") {
		body, isCode := strings.CutPrefix(line, "//\t")
		if !in {
			if isCode && body == "disruption:" {
				in = true
			} else {
				continue
			}
		}
		if !isCode {
			break
		}
		b.WriteString("    " + body + "\n")
	}
	if b.Len() == 0 {
		t.Fatal("workflow.go の説明に disruption: の例が見つかりません")
	}
	yaml := `workflows:
  - workflow: gw-update
    target:
      provider: fake
    stages:
      - name: s1
        targets:
          - gw1
    steps:
      - apply:
          active: "false"
` + b.String()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "workflow.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	in2, err := LoadIntent(dir)
	if err != nil {
		t.Fatalf("説明の例が読み込めません: %v\n%s", err, yaml)
	}
	d := in2.Workflows[0].Disruption
	if d == nil || d.Estimate != 20*time.Minute || len(d.Affects) != 2 || d.Window != "Sat 02:00-04:00 UTC" ||
		len(d.Upstream) != 1 || d.Upstream[0].Fields["ready.below_limit"] != "true" || d.MaxWait != time.Hour ||
		d.NotifyBefore != 10*time.Minute || d.Gate.Timeout != 4*time.Hour || d.Gate.Requirements.Needed() != 2 {
		t.Fatalf("disruption = %+v", d)
	}
	gw1 := in2.Workflows[0].Stages[0].Targets[0]
	if got := d.AffectsFor(gw1); got[1].Local != "gw1-clients" || got[1].Provider != "fake" {
		t.Errorf("affects for gw1 = %+v", got)
	}
	if g := workflow.DisruptionGateName("s1", 0); g != "s1-disruption-1" {
		t.Errorf("gate name = %q", g)
	}
}
