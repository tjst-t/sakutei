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

package task

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// countingRunner は呼ばれた回数だけ数える provider の実行体。
type countingRunner struct{ calls int }

func (c *countingRunner) RunTask(context.Context, Request) error { c.calls++; return nil }
func (c *countingRunner) RollbackTask(context.Context, Request) (string, error) {
	c.calls++
	return "", nil
}

// [AC-150-5] manual の Task は相を 1 つ（承認・待つ）宣言し、provider の実行体を
// 呼ばない。観測は ManualObserver だけ。manual でない Task は inner に回す。
func TestManualRunner_NeverCallsProviderRunner(t *testing.T) {
	inner := &countingRunner{}
	observed := 0
	r := WithManual(inner, func(context.Context, Request) (StepState, error) {
		observed++
		return StepPending, nil
	})
	ctx := context.Background()
	req := Request{
		Resource: core.NewID("fake", core.NewScope(nil), "sw1"),
		Policy:   core.UpdatePolicy{Strategy: core.UpdateStrategyManual, Procedure: "use the GUI", ProcedureURL: "https://x.example"},
		Plan:     core.Plan{Ops: []core.Op{{Fields: []core.FieldDiff{{Field: "version", From: "1.0", To: "2.0"}}}}},
	}
	steps, err := r.TaskSteps(ctx, req)
	if err != nil || len(steps) != 1 {
		t.Fatalf("[AC-150-5] steps %+v, %v", steps, err)
	}
	s := steps[0]
	if s.Name != StepManual || !s.Approval || !s.Await || !s.Action || s.Critical || s.Timeout != DefaultConfirmTimeout {
		t.Fatalf("[AC-150-5] step: %+v", s)
	}
	for _, want := range []string{"use the GUI", "https://x.example", "version: 1.0 → 2.0"} {
		if !strings.Contains(s.Notice, want) {
			t.Fatalf("[AC-150-5] notice %q lacks %q", s.Notice, want)
		}
	}
	if !strings.HasPrefix(s.Stuck, "stuck:") {
		t.Fatalf("[AC-150-5] stuck: %q", s.Stuck)
	}
	req.Policy.ConfirmTimeout = 2 * time.Hour
	if steps, _ := r.TaskSteps(ctx, req); steps[0].Timeout != 2*time.Hour {
		t.Fatalf("[AC-150-5] confirm_timeout not used: %+v", steps[0])
	}
	if st, err := r.CheckStep(ctx, req, StepManual); st != StepPending || err != nil || observed != 1 {
		t.Fatalf("[AC-150-5] CheckStep: %s %v (observed %d)", st, err, observed)
	}
	if err := r.RunStep(ctx, req, StepManual); err != nil {
		t.Fatalf("[AC-150-5] RunStep: %v", err)
	}
	if r.RunTask(ctx, req) == nil {
		t.Fatalf("[AC-150-5] RunTask of a manual update must refuse")
	}
	if inner.calls != 0 {
		t.Fatalf("[AC-150-5] the provider's runner was called %d time(s)", inner.calls)
	}

	req.Policy = core.UpdatePolicy{Strategy: core.UpdateStrategyTask}
	if steps, _ := r.TaskSteps(ctx, req); steps != nil {
		t.Fatalf("[AC-150-5] a simple Task got steps: %+v", steps)
	}
	if err := r.RunTask(ctx, req); err != nil || inner.calls != 1 {
		t.Fatalf("[AC-150-5] strategy: task must go to the provider's runner: %v, calls %d", err, inner.calls)
	}
	if WithManual(nil, nil).RunTask(ctx, req) == nil {
		t.Fatalf("[AC-150-5] no runner: RunTask must fail")
	}
}
