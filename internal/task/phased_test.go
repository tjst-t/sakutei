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

	"github.com/tjst-t/sakutei/v2/internal/audit"
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

func (r *stepRunner) RunTask(context.Context, Request) error {
	panic("simple RunTask on a phased task")
}

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

// 承認の要る相（#148）: 入口で済んでいなければ RunStep の前に人手ゲートで止まり、
// 承認でその相から続ける。却下は halt。待つ間に済んだと見えれば承認なしで閉じる。
func TestPhased_ApprovalGate(t *testing.T) {
	ctx := context.Background()
	steps := []StepSpec{{Name: StepVerifying, Action: true, Approval: true}, {Name: StepStaging, Action: true}}
	desired := map[string]string{"v": "2"}
	log := audit.NewLog(filepath.Join(t.TempDir(), "audit.jsonl"))
	launch := func() (*FileStore, *stepRunner, *Manager) {
		st := NewFileStore(filepath.Join(t.TempDir(), "tasks.json"))
		r := newStepRunner(steps)
		m := NewManager(st, r, "me", nil)
		m.PollInterval = time.Millisecond
		if dec, err := m.Trigger(ctx, phasedID, desired, core.Plan{}, core.UpdatePolicy{}); err != nil || dec.Outcome != Launched {
			t.Fatalf("trigger: %v %v", dec, err)
		}
		m.Wait(5 * time.Second)
		rec := waitRecordPhase(t, st, PhaseAwaitingApproval)
		if rec.Gate != GateApproval || rec.Step != StepVerifying || len(r.runs) != 0 {
			t.Fatalf("record %+v runs %v", rec, r.runs)
		}
		return st, r, m
	}

	st, r, m := launch()
	dec, err := m.Trigger(ctx, phasedID, desired, core.Plan{}, core.UpdatePolicy{})
	if err != nil || dec.Outcome != AwaitingApproval || !strings.Contains(dec.Reason, "needs approval before step verifying") {
		t.Fatalf("decision %+v err=%v", dec, err)
	}
	if len(r.runs) != 0 {
		t.Fatalf("steps ran without approval: %v", r.runs)
	}
	if _, err := Decide(ctx, st, log, phasedID.String(), Approval{Approver: "alice"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if dec, err := m.Trigger(ctx, phasedID, desired, core.Plan{}, core.UpdatePolicy{}); err != nil || dec.Outcome != Resumed {
		t.Fatalf("after approval: %+v %v", dec, err)
	}
	m.Wait(5 * time.Second)
	rec := waitRecordPhase(t, st, PhaseSucceeded)
	if r.runs[StepVerifying] != 1 || r.runs[StepStaging] != 1 || rec.ApprovedBy != "alice" {
		t.Fatalf("runs %v record %+v", r.runs, rec)
	}

	st2, r2, _ := launch()
	if _, err := Decide(ctx, st2, log, phasedID.String(), Approval{Approver: "bob", Reject: true}, time.Now()); err != nil {
		t.Fatal(err)
	}
	rec = waitRecordPhase(t, st2, PhaseFailed)
	if !strings.Contains(rec.Error, "rejected by bob") || len(r2.runs) != 0 {
		t.Fatalf("record %+v runs %v", rec, r2.runs)
	}

	st3, r3, m3 := launch()
	r3.mu.Lock()
	r3.done[StepVerifying] = true
	r3.mu.Unlock()
	if dec, err := m3.Trigger(ctx, phasedID, desired, core.Plan{}, core.UpdatePolicy{}); err != nil || dec.Outcome != Resumed {
		t.Fatalf("closed gate: %+v %v", dec, err)
	}
	m3.Wait(5 * time.Second)
	waitRecordPhase(t, st3, PhaseSucceeded)
	if r3.runs[StepVerifying] != 0 || r3.runs[StepStaging] != 1 {
		t.Fatalf("runs %v", r3.runs)
	}
}

// Await の相（#148）: RunStep が返ると S2 に Awaiting の印が立ち、待ちの間に
// 持ち主が落ちても、引き継いだデーモンは RunStep をもう一度呼ばずに待つ。
// Critical の印は RunStep の間だけ。
func TestPhased_AwaitingIsNotRerun(t *testing.T) {
	ctx := context.Background()
	steps := []StepSpec{{Name: StepRebooting, Action: true, Await: true, Critical: true}}
	desired := map[string]string{"v": "2"}

	// 新しく起動した Task: RunStep の後、待つ間は Awaiting が立ち Critical は下りている。
	st := NewFileStore(filepath.Join(t.TempDir(), "tasks.json"))
	r := &awaitRunner{stepRunner: newStepRunner(steps)}
	m := NewManager(st, r, "me", nil)
	m.PollInterval = time.Millisecond
	if dec, err := m.Trigger(ctx, phasedID, desired, core.Plan{}, core.UpdatePolicy{}); err != nil || dec.Outcome != Launched {
		t.Fatalf("trigger: %v %v", dec, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec, _, _ := st.Get(ctx, phasedID.String())
		if rec.Awaiting && !rec.Critical {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("record while waiting = %+v, want awaiting without the critical mark", rec)
		}
		time.Sleep(time.Millisecond)
	}
	m.Stop()
	m.Wait(5 * time.Second)

	// 待ちの間に落ちた記録（lease 切れ）を別のデーモンが引き継ぐ: RunStep は呼ばない。
	st2 := NewFileStore(filepath.Join(t.TempDir(), "tasks.json"))
	if _, ok, err := st2.Begin(ctx, Record{Resource: phasedID.String(), TaskID: "old-1", Phase: PhaseRunning,
		Holder: "old", DesiredHash: DesiredHash(desired), Step: StepRebooting, StepStartedAt: time.Now(),
		HeartbeatAt: time.Now().Add(-time.Hour), Awaiting: true}); err != nil || !ok {
		t.Fatalf("seed: %v %v", ok, err)
	}
	r2 := &awaitRunner{stepRunner: newStepRunner(steps)}
	m2 := NewManager(st2, r2, "me2", nil)
	m2.PollInterval = time.Millisecond
	if dec, err := m2.Trigger(ctx, phasedID, desired, core.Plan{}, core.UpdatePolicy{}); err != nil || dec.Outcome != Resumed {
		t.Fatalf("resume: %+v %v", dec, err)
	}
	time.Sleep(20 * time.Millisecond)
	r2.mu.Lock()
	runs := r2.runs[StepRebooting]
	r2.done[StepRebooting] = true // 再起動から戻った
	r2.mu.Unlock()
	if runs != 0 {
		t.Fatalf("RunStep ran %d time(s) after the takeover of an awaiting step, want 0", runs)
	}
	m2.Wait(5 * time.Second)
	waitRecordPhase(t, st2, PhaseSucceeded)
}

// awaitRunner の RunStep は効果を起こすが、Await の完了（done）は人が置く。
type awaitRunner struct{ *stepRunner }

func (a *awaitRunner) RunStep(_ context.Context, _ Request, step string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.runs[step]++
	return nil
}
