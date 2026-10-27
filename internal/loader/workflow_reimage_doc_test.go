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

	"github.com/tjst-t/sakutei/internal/budget"
	"github.com/tjst-t/sakutei/internal/workflow"
)

// #88: 設計書 §6.3 の trigger: reimage の例（Workflow と planned_change）は、そのまま
// intent に写して読み込める。
func TestWorkflowReimageSpecExampleLoads(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "docs", "architecture", "spec", "part2-core-model.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(src), "**入れ直しを宣言で起こす `trigger: reimage`")
	if !ok {
		t.Fatal("§6.3 に trigger: reimage の節が見つかりません")
	}
	_, rest, ok = strings.Cut(rest, "```yaml\n")
	block, _, ok2 := strings.Cut(rest, "```")
	if !ok || !ok2 {
		t.Fatal("trigger: reimage の yaml の例が見つかりません")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "reimage.yaml"), []byte(block), 0o644); err != nil {
		t.Fatal(err)
	}
	in, err := LoadIntent(dir)
	if err != nil {
		t.Fatalf("例が読み込めません: %v\n%s", err, block)
	}
	if len(in.Workflows) != 1 || in.Workflows[0].Trigger != workflow.TriggerReimage {
		t.Fatalf("workflows = %+v, want one with trigger reimage", in.Workflows)
	}
	if len(in.PlannedChanges) != 1 || in.PlannedChanges[0].Kind != budget.PlannedReimage ||
		!in.Workflows[0].Scope().Matches(in.PlannedChanges[0].Scope) {
		t.Fatalf("planned_change = %+v, want a reimage covering the workflow's scope", in.PlannedChanges)
	}
}
