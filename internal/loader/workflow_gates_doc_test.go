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
	"path/filepath"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/workflow"
)

// #101: 試用の手順書（docs/trial/workflow-gates）の intent は、そのまま loader で読め、
// run.sh が前提にするゲート（s1 単一・s2 2-of-3 + roles + separation・s3 なし）になっている。
func TestWorkflowGatesTrialIntentLoads(t *testing.T) {
	in, err := LoadIntent(filepath.Join("../../docs/trial/workflow-gates", "intent"))
	if err != nil {
		t.Fatalf("手順書の intent が読み込めません: %v", err)
	}
	if len(in.Resources) != 3 || len(in.Workflows) != 1 {
		t.Fatalf("resources = %d, workflows = %d", len(in.Resources), len(in.Workflows))
	}
	def := in.Workflows[0]
	if def.Name != "rolling" || def.Applicant != "alice" || def.Invariant.MinActive != 2 || len(def.Stages) != 3 {
		t.Fatalf("workflow = %+v", def)
	}
	s1, s2, s3 := def.Stages[0].Gate, def.Stages[1].Gate, def.Stages[2].Gate
	if s1 == nil || s1.Requirements != nil || s1.Timeout != 24*time.Hour {
		t.Errorf("gate s1 = %+v, want a single approval with timeout 24h", s1)
	}
	if s2 == nil || s2.Requirements == nil || s2.Requirements.Needed() != 2 || s2.Requirements.Quorum.Of != 3 ||
		len(s2.Requirements.Roles) != 1 || s2.Requirements.Roles[0] != "platform-admin" ||
		s2.Requirements.Separation != workflow.SeparationApplicantNeApprover {
		t.Errorf("gate s2 = %+v", s2)
	}
	if s3 != nil {
		t.Errorf("stage s3 has a gate %+v", s3)
	}
}
