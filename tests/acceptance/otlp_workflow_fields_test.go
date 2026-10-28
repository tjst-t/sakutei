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

// otlp_workflow_fields_test.go — #189（OTLP のトレースの出口、§26.8）:
//
//	[AC-189-5] Workflow の手順が失敗しても（wait の時間切れ・apply の手順の plan の
//	           誤り・Apply の誤り）、手順の欄の値は span に入らない。手順の span は決まった
//	           短い理由（timed out・plan error・apply error）で、Workflow の span は
//	           「workflow run halted」で ERROR になる。halt の理由（S2）には今までどおり
//	           欄の値が残る
package acceptance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/fakeprovider"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/internal/tracing"
	"github.com/tjst-t/sakutei/v2/internal/workflow"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// spanSink keeps every span the daemon ends.
type spanSink struct {
	mu    sync.Mutex
	spans []tracing.SpanData
}

func (s *spanSink) Enqueue(d tracing.SpanData) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.spans = append(s.spans, d)
}

func (s *spanSink) all() []tracing.SpanData {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]tracing.SpanData(nil), s.spans...)
}

// refusingProvider is the fake provider whose plan refines fail for one
// desired value, with the value in the error (as a provider's may be).
type refusingProvider struct {
	*fakeprovider.Provider
}

func (p refusingProvider) RefinePlan(_ string, _ core.State, desired map[string]string, ops []core.Op) ([]core.Op, error) {
	if desired["size"] == "189002" {
		return nil, fmt.Errorf("cannot set size %s", desired["size"])
	}
	return ops, nil
}

// The step values all start with 1890: none of them may reach a span.
const otlpFailingWorkflows = `{
  "resources": [
    { "provider": "fake", "id": "t1", "fields": { "name": "t1", "size": "5" } },
    { "provider": "fake", "id": "t2", "fields": { "name": "t2", "size": "5" } },
    { "provider": "fake", "id": "t3", "fields": { "name": "t3", "size": "5" } }
  ],
  "workflows": [
    { "workflow": "wf-timeout", "target": { "provider": "fake" },
      "stages": [ { "name": "s1", "targets": ["t1"] } ],
      "steps": [ { "wait": { "fields": { "size": "189001" }, "timeout": "1ms" } } ],
      "on_failure": { "policy": "halt" } },
    { "workflow": "wf-plan", "target": { "provider": "fake" },
      "stages": [ { "name": "s1", "targets": ["t2"] } ],
      "steps": [ { "apply": { "size": "189002" } } ],
      "on_failure": { "policy": "halt" } },
    { "workflow": "wf-apply", "target": { "provider": "fake" },
      "stages": [ { "name": "s1", "targets": ["t3"] } ],
      "steps": [ { "apply": { "size": "189003" } } ],
      "on_failure": { "policy": "halt" } }
  ]
}`

func TestOTLP_WorkflowFailureKeepsFieldsOut(t *testing.T) {
	root := t.TempDir()
	intentDir := filepath.Join(root, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(intentDir, "intent.json"), []byte(otlpFailingWorkflows), 0o644); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "state.json")
	initStateFile(t, statePath)
	fp := fakeprovider.New(statePath)
	for _, n := range []string{"t1", "t2", "t3"} {
		if err := fp.SeedResource(n, map[string]any{"name": n, "size": 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := fp.SetFailInject(map[string]string{"t3": "refused by the device"}); err != nil {
		t.Fatal(err)
	}
	out := &syncBuf{}
	store := workflow.NewFileStore(filepath.Join(root, "workflows.json"))
	d := reconcile.NewDaemon(reconcile.Config{
		IntentDir: intentDir, GeneratedDir: filepath.Join(root, "generated"),
		Provider: refusingProvider{fp}, Out: out, ErrOut: out, WorkflowStore: store,
	})
	sink := &spanSink{}
	ctx := tracing.WithTracer(context.Background(), tracing.NewTracer(sink))

	halted := func() bool {
		for _, name := range []string{"wf-timeout", "wf-plan", "wf-apply"} {
			run, ok, err := store.Get(context.Background(), name)
			if err != nil || !ok || run.Phase != workflow.PhaseHalted {
				return false
			}
		}
		return true
	}
	for i := 0; i < 10 && !halted(); i++ {
		cctx, span := tracing.Start(ctx, tracing.SpanCycle)
		if _, err := d.RunCycle(cctx); err != nil {
			t.Fatalf("RunCycle: %v", err)
		}
		span.End()
		d.Wait(5 * time.Second)
		time.Sleep(5 * time.Millisecond) // past the wait step's 1ms timeout
	}
	if !halted() {
		t.Fatalf("the Workflows did not halt:\n%s", out.String())
	}
	// The halt reasons (S2, the log) still name the values: only the spans leave them out.
	for name, want := range map[string]string{"wf-timeout": "189001", "wf-plan": "189002", "wf-apply": "189003"} {
		run, _, _ := store.Get(context.Background(), name)
		if !strings.Contains(run.Reason, want) {
			t.Errorf("%s: halt reason %q does not name the step's value", name, run.Reason)
		}
	}

	stepWhy := map[string]string{}
	wfFailed := map[string]string{}
	for _, s := range sink.all() {
		texts := []string{s.ErrMsg}
		for _, a := range s.Attrs {
			texts = append(texts, fmt.Sprint(a.Value))
		}
		for _, x := range texts {
			if strings.Contains(x, "1890") {
				t.Errorf("[AC-189-5] a step's value is in the %s span: %q", s.Name, x)
			}
		}
		wf := ""
		for _, a := range s.Attrs {
			if a.Key == tracing.AttrWorkflow {
				wf = fmt.Sprint(a.Value)
			}
		}
		switch {
		case s.Name == tracing.SpanWorkflowStep && s.Failed:
			stepWhy[wf] = s.ErrMsg
		case s.Name == tracing.SpanWorkflow && s.Failed:
			wfFailed[wf] = s.ErrMsg
		}
	}
	for wf, want := range map[string]string{
		"wf-timeout": "wait step failed: timed out",
		"wf-plan":    "apply step failed: plan error",
		"wf-apply":   "apply step failed: apply error",
	} {
		if stepWhy[wf] != want {
			t.Errorf("[AC-189-5] %s: step span error %q, want %q", wf, stepWhy[wf], want)
		}
		if wfFailed[wf] != "workflow run halted" {
			t.Errorf("[AC-189-5] %s: workflow span error %q", wf, wfFailed[wf])
		}
	}
}
