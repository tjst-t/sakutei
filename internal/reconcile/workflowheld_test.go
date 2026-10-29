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

package reconcile

import (
	"testing"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// #209: WorkflowTargetHeld は、持つ理由を決めた周に持たなくなれば resolved。
func TestLevelCoverage_WorkflowTargetHeld(t *testing.T) {
	n1 := core.NewID("fake", core.NewScope(nil), "n1")
	ev := core.Event{Kind: core.EventWorkflowTargetHeld, Resource: n1}
	if (levelCoverage{}).resolves(ev, CycleResult{}, nil) {
		t.Fatal("resolved on a cycle that did not decide the holds")
	}
	if (levelCoverage{wfJudged: true, wfFlagged: map[string]bool{n1.String(): true}}).resolves(ev, CycleResult{}, nil) {
		t.Fatal("resolved while still held")
	}
	if !(levelCoverage{wfJudged: true, wfFlagged: map[string]bool{}}).resolves(ev, CycleResult{}, nil) {
		t.Fatal("not resolved once no longer held")
	}
}
