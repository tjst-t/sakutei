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
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// stepRunner は相ごとの現実を map で持つ PhasedRunner。
type stepRunner struct {
	mu       sync.Mutex
	steps    []StepSpec
	done     map[string]bool
	runs     map[string]int
	rollback []string // RollbackTask に渡った Request.Step
}

func newStepRunner(steps []StepSpec) *stepRunner {
	return &stepRunner{steps: steps, done: map[string]bool{}, runs: map[string]int{}}
}

func (r *stepRunner) RunTask(context.Context, Request) error { panic("simple RunTask on a phased task") }

func (r *stepRunner) RollbackTask(_ context.Context, req Request) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rollback = append(r.rollback, req.Step)
	return "", nil
}

func (r *stepRunner) TaskSteps(context.Context, Request) ([]StepSpec, error) { return r.steps, nil }

func (r *stepRunner) CheckStep(_ context.Context, _ Request, step string) (StepState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done[step] {
		return StepDone, nil
	}
	return StepPending, nil
}

func (r *stepRunner) RunStep(_ context.Context, _ Request, step string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs[step]++
	r.done[step] = true
	return nil
}

var phasedID = core.ID{Provider: "fw", Local: "sw1"}

func seedPhased(t *testing.T, st Store, step string, desired map[string]string, heartbeat time.Time, released bool) {
	t.Helper()
	if _, ok, err := st.Begin(context.Background(), Record{Resource: phasedID.String(), TaskID: "old-1", Phase: PhaseRunning,
		Holder: "old", DesiredHash: DesiredHash(desired), Step: step, StepStartedAt: heartbeat,
		HeartbeatAt: heartbeat, Released: released}); err != nil || !ok {
		t.Fatalf("seed: ok=%v err=%v", ok, err)
	}
}

func waitRecordPhase(t *testing.T, st Store, phase Phase) Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec, ok, err := st.Get(context.Background(), phasedID.String())
		if err != nil {
			t.Fatal(err)
		}
		if ok && rec.Phase == phase {
			return rec
		}
		if time.Now().After(deadline) {
			t.Fatalf("record %+v (present=%v), want phase %s", rec, ok, phase)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// 確かめるだけの相（Confirming）で済んでいなければ、Task は失敗（halt）し、
// rollback には失敗した相が渡る。
func TestPhased_ConfirmNotDoneFails(t *testing.T) {
	ctx := context.Background()
	st := NewFileStore(filepath.Join(t.TempDir(), "tasks.json"))
	r := newStepRunner([]StepSpec{{Name: StepApplying, Action: true}, {Name: StepConfirming}})
	m := NewManager(st, r, "me", nil)
	m.PollInterval = time.Millisecond
	dec, err := m.Trigger(ctx, phasedID, map[string]string{"v": "2"}, core.Plan{}, core.UpdatePolicy{RollbackOnFail: true})
	if err != nil || dec.Outcome != Launched {
		t.Fatalf("trigger: %v %v", dec, err)
	}
	m.Wait(5 * time.Second)
	rec := waitRecordPhase(t, st, PhaseFailed)
	if rec.Step != StepConfirming || !strings.Contains(rec.Error, "not confirmed") || !rec.RolledBack {
		t.Fatalf("record %+v", rec)
	}
	if len(r.rollback) != 1 || r.rollback[0] != StepConfirming {
		t.Fatalf("rollback got steps %v", r.rollback)
	}
}

// desired が起動の後で変わった記録は引き継がない。
func TestPhased_DesiredChangedNotResumed(t *testing.T) {
	ctx := context.Background()
	st := NewFileStore(filepath.Join(t.TempDir(), "tasks.json"))
	seedPhased(t, st, StepApplying, map[string]string{"v": "2"}, time.Now(), true)
	r := newStepRunner(FirmwareSteps(0))
	m := NewManager(st, r, "me", nil)
	dec, err := m.Trigger(ctx, phasedID, map[string]string{"v": "3"}, core.Plan{}, core.UpdatePolicy{})
	if err != nil || dec.Outcome != Outstanding || !strings.Contains(dec.Reason, "desired state changed") {
		t.Fatalf("decision %+v err=%v", dec, err)
	}
	if len(r.runs) != 0 {
		t.Fatalf("steps ran: %v", r.runs)
	}
}

// 手放された記録を2つのデーモンが同時に引き継ごうとしても、引き継ぐのは1つ。
func TestPhased_TakeoverIsExclusive(t *testing.T) {
	ctx := context.Background()
	st := NewFileStore(filepath.Join(t.TempDir(), "tasks.json"))
	desired := map[string]string{"v": "2"}
	seedPhased(t, st, StepVerifying, desired, time.Now(), true)
	r := newStepRunner(FirmwareSteps(0))
	r.done[StepStaging] = true
	hold := make(chan struct{})
	var managers []*Manager
	for _, h := range []string{"a", "b", "c", "d"} {
		m := NewManager(st, &blockingRunner{stepRunner: r, hold: hold}, h, nil)
		m.PollInterval = time.Millisecond
		managers = append(managers, m)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	resumed := 0
	for _, m := range managers {
		wg.Add(1)
		go func(m *Manager) {
			defer wg.Done()
			dec, err := m.Trigger(ctx, phasedID, desired, core.Plan{}, core.UpdatePolicy{})
			if err != nil {
				t.Error(err)
				return
			}
			if dec.Outcome == Resumed {
				mu.Lock()
				resumed++
				mu.Unlock()
			}
		}(m)
	}
	wg.Wait()
	close(hold)
	for _, m := range managers {
		m.Wait(5 * time.Second)
	}
	if resumed != 1 {
		t.Fatalf("%d managers resumed, want 1", resumed)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runs[StepStaging] != 0 || r.runs[StepVerifying] != 1 {
		t.Fatalf("runs %v", r.runs)
	}
}

// blockingRunner は RunStep を hold が閉じるまで止める。
type blockingRunner struct {
	*stepRunner
	hold chan struct{}
}

func (b *blockingRunner) RunStep(ctx context.Context, req Request, step string) error {
	<-b.hold
	return b.stepRunner.RunStep(ctx, req, step)
}
