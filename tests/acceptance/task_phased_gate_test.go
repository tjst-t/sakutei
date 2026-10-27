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


// task_phased_gate_test.go: 多相 Task の危険区間と人手ゲート（#52、§29.1）。
//
// 多相 Task の rig（task_phased_test.go）を使う。Applying（firmware の書込）が
// 危険区間。前のデーモンが書込の途中で落ちた（kill -9）ことは、危険区間の印の
// 立った記録を S2 に置き、lease を切らして表す。
//
//	AC-52-1 — Applying の RunStep の間だけ S2 に危険区間の印が立つ。デーモンの停止（安全な区切り）では印は下りていて、起こし直しても人手ゲートにならない
//	AC-52-2 — Applying の途中で落ちた Task は、書込が済んだと Observe で確かめられないので自動で再開せず人手ゲートで止まる（何周回しても・デーモンを替えても）。承認すると Applying から再開して Done になる
//	AC-52-3 — 落ちた時点で書込が済んでいたと Observe で確かめられれば、ゲートにせず（またはゲートを閉じて）承認なしで続ける。リブート中（unknown）はゲートで待ち、戻って完了印が見えれば Done
//	AC-52-4 — 却下とゲートのタイムアウトは halt（再開も rollback もしない）
//	AC-52-5 — sakutei task approve / reject（S4 監査に残してから S2 に書く。ゲートで待っていない Task は承認できない）
package acceptance

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/audit"
	"github.com/tjst-t/sakutei/internal/fakeprovider"
	"github.com/tjst-t/sakutei/internal/task"
)

var dbDesired = map[string]string{"name": "db", "size": "42"}

// seedCrashedInApplying は、前のデーモン（old-host-1）が Applying の書込の途中で
// 落ちた記録を置く。fw は落ちた時点の機器。lease は切れている。
func (r *taskRig) seedCrashedInApplying(fw *fakeprovider.Firmware) {
	r.t.Helper()
	if err := r.fp.SetFirmware("db", fw); err != nil {
		r.t.Fatal(err)
	}
	now := r.clock.Now()
	if _, started, err := task.NewFileStore(r.storePath).Begin(context.Background(), task.Record{
		Resource: dbKey, TaskID: "old-1", Phase: task.PhaseRunning, Holder: "old-host-1",
		DesiredHash: task.DesiredHash(dbDesired), StartedAt: now.Add(-20 * time.Minute),
		Step: task.StepApplying, StepStartedAt: now.Add(-10 * time.Minute), HeartbeatAt: now.Add(-5 * time.Minute),
		Critical: true,
	}); err != nil || !started {
		r.t.Fatalf("seed: started=%v err=%v", started, err)
	}
}

func (r *taskRig) auditLog() *audit.Log { return audit.NewLog(filepath.Join(r.root, "audit.log")) }

// wantGated は記録が人手ゲートで待っていることを確かめる。
func (r *taskRig) wantGated(tag string) task.Record {
	r.t.Helper()
	rec, ok := r.record()
	if !ok || rec.Phase != task.PhaseAwaitingApproval || rec.Step != task.StepApplying || !rec.Critical {
		r.t.Fatalf("[%s] record = %+v (present=%v), want awaiting_approval at applying with the critical mark\n%s", tag, rec, ok, r.errOut)
	}
	return rec
}

// [AC-52-1]
func TestPhasedTaskGate_CriticalMarkOnlyDuringApplying(t *testing.T) {
	r := newPhasedRig(t, taskPolicy)
	reached, release := r.fp.HoldStep("db", task.StepApplying)
	defer release()
	r.cycleOn(r.d)
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatalf("[AC-52-1] applying never ran\n%s", r.errOut)
	}
	// 書込の最中: S2 に印が立っている（ここで落ちれば人手ゲート）。
	if rec := r.waitStep(task.StepApplying); !rec.Critical {
		t.Fatalf("[AC-52-1] record during the applying RunStep must carry the critical mark: %+v", rec)
	}
	// 停止は RunStep を切らない。返ってから印を下ろして手放す。
	r.d.StopTasks()
	release()
	r.d.Wait(5 * time.Second)
	rec := r.waitStep(task.StepApplying)
	if rec.Critical || !rec.Released {
		t.Fatalf("[AC-52-1] stopped daemon must clear the mark and release the record: %+v", rec)
	}

	d2 := r.newDaemon(true)
	res := r.cycleOn(d2)
	if !contains(res.TasksResumed, "db") || len(res.TasksAwaitingApproval) != 0 {
		t.Fatalf("[AC-52-1] a graceful stop must resume without the gate: %+v\n%s", res, r.errOut)
	}
	r.waitStep(task.StepRebooting)
	if rec, _ := r.record(); rec.Critical {
		t.Fatalf("[AC-52-1] mark must be down outside applying: %+v", rec)
	}
	r.waitRecord("device rebooting", func(task.Record, bool) bool {
		fw, err := r.fp.GetFirmware("db")
		return err == nil && fw.Rebooting
	})
	r.finishReboot()
	r.settle("AC-52-1", func() bool {
		res := r.cycleOn(d2)
		d2.Wait(5 * time.Second)
		return contains(res.TasksCompleted, "db")
	})
	r.wantStepCounts("AC-52-1", map[string]int{task.StepStaging: 1, task.StepVerifying: 1, task.StepApplying: 1, task.StepRebooting: 1})
}

// [AC-52-2]
func TestPhasedTaskGate_CrashInApplyingWaitsForApproval(t *testing.T) {
	r := newPhasedRig(t, taskPolicy)
	// 書込の途中で落ちた: 検証済みの image はあるが、書けたとは見えない。
	r.seedCrashedInApplying(&fakeprovider.Firmware{Staged: "42", Verified: "42"})

	res := r.cycle()
	if !contains(res.TasksAwaitingApproval, "db") || !contains(res.TasksHeld, "db") ||
		len(res.TasksResumed) != 0 || len(res.TasksLaunched) != 0 {
		t.Fatalf("[AC-52-2] crashed inside applying: want the approval gate, not a resume: %+v\n%s", res, r.errOut)
	}
	rec := r.wantGated("AC-52-2")
	if !rec.GateDeadline.Equal(rec.GateOpenedAt.Add(task.DefaultGateTimeout)) {
		t.Fatalf("[AC-52-2] gate deadline = %s, want opened (%s) + %s", rec.GateDeadline, rec.GateOpenedAt, task.DefaultGateTimeout)
	}
	if !strings.Contains(rec.Error, "critical section") || !strings.Contains(rec.Error, "old-host-1") {
		t.Fatalf("[AC-52-2] record must say why it waits: %q", rec.Error)
	}
	if !strings.Contains(r.errOut.String(), "awaiting approval") || !strings.Contains(r.errOut.String(), "sakutei task approve") {
		t.Fatalf("[AC-52-2] RequiresTask must tell how to approve:\n%s", r.errOut)
	}

	// 何周回しても、時間が経っても（上限の手前）、デーモンを替えても自動で再開しない。
	r.clock.Set(r.clock.Now().Add(task.DefaultGateTimeout - time.Hour))
	for i := 0; i < 3; i++ {
		if i == 1 {
			r.d = r.newDaemon(true) // 別のデーモン（起こし直し）
		}
		res := r.cycle()
		if !contains(res.TasksAwaitingApproval, "db") || len(res.TasksResumed) != 0 || len(res.TasksLaunched) != 0 {
			t.Fatalf("[AC-52-2] cycle %d: must stay at the gate: %+v\n%s", i, res, r.errOut)
		}
		if got := r.wantGated("AC-52-2"); !got.GateOpenedAt.Equal(rec.GateOpenedAt) {
			t.Fatalf("[AC-52-2] gate re-opened (deadline extended): %+v", got)
		}
	}
	r.wantStepCounts("AC-52-2", map[string]int{})

	// 人が機器を確かめて承認: Applying の入口から再開する。
	got, err := task.Decide(context.Background(), task.NewFileStore(r.storePath), r.auditLog(), dbKey,
		task.Approval{Approver: "alice", Signature: "sig-a"}, r.clock.Now())
	if err != nil {
		t.Fatalf("[AC-52-2] approve: %v", err)
	}
	if got.Phase != task.PhaseRunning || !got.Released || got.Critical || got.ApprovedBy != "alice" || got.Step != task.StepApplying {
		t.Fatalf("[AC-52-2] approved record = %+v, want released running at applying", got)
	}
	res = r.cycleOn(r.d)
	if !contains(res.TasksResumed, "db") || len(res.TasksAwaitingApproval) != 0 {
		t.Fatalf("[AC-52-2] approved Task must be resumed: %+v\n%s", res, r.errOut)
	}
	r.waitStep(task.StepRebooting)
	r.waitRecord("device rebooting", func(task.Record, bool) bool {
		fw, err := r.fp.GetFirmware("db")
		return err == nil && fw.Rebooting
	})
	r.finishReboot()
	r.settle("AC-52-2", func() bool { return contains(r.cycle().TasksCompleted, "db") })
	// 済んでいた相（転送・検証）は飛ばし、書込は承認の後の 1 回だけ。
	r.wantStepCounts("AC-52-2", map[string]int{task.StepApplying: 1, task.StepRebooting: 1})
}

// [AC-52-3]
func TestPhasedTaskGate_ObservedDoneNeedsNoApproval(t *testing.T) {
	t.Run("flash observed at takeover", func(t *testing.T) {
		r := newPhasedRig(t, taskPolicy)
		r.seedCrashedInApplying(&fakeprovider.Firmware{Staged: "42", Verified: "42", Flashed: "42"})
		res := r.cycleOn(r.d)
		if !contains(res.TasksResumed, "db") || len(res.TasksAwaitingApproval) != 0 {
			t.Fatalf("[AC-52-3] flash observed done: want resumed without the gate: %+v\n%s", res, r.errOut)
		}
		r.waitStep(task.StepRebooting)
		r.finishReboot()
		r.settle("AC-52-3", func() bool { return contains(r.cycle().TasksCompleted, "db") })
		r.wantStepCounts("AC-52-3", map[string]int{task.StepRebooting: 1})
	})

	t.Run("rebooting, came back with the new version", func(t *testing.T) {
		r := newPhasedRig(t, taskPolicy)
		r.seedCrashedInApplying(&fakeprovider.Firmware{Staged: "42", Verified: "42", Flashed: "42", Rebooting: true})
		// 見えない: リブート中か壊れたか分からない。人手ゲート（unknown ≠ absent）。
		for c := 0; c < 3; c++ {
			res := r.cycle()
			if !contains(res.TasksAwaitingApproval, "db") || len(res.TasksResumed) != 0 || res.Pruned != 0 || len(res.TasksCompleted) != 0 {
				t.Fatalf("[AC-52-3] unknown device: want the gate: %+v\n%s", res, r.errOut)
			}
		}
		r.wantGated("AC-52-3")
		// 機器が新しい版で戻った: 完了印が見えたので承認なしで Done。
		r.finishReboot()
		if res := r.cycle(); !contains(res.TasksCompleted, "db") {
			t.Fatalf("[AC-52-3] completion mark observed: want Done: %+v\n%s", res, r.errOut)
		}
		if _, ok := r.record(); ok {
			t.Fatal("[AC-52-3] record must be gone once Done")
		}
		r.wantStepCounts("AC-52-3", map[string]int{})
	})

	t.Run("gate closes when the flash becomes observable", func(t *testing.T) {
		r := newPhasedRig(t, taskPolicy)
		// 落ちた時点では書けたと見えない（見えない機器）。
		r.seedCrashedInApplying(&fakeprovider.Firmware{Staged: "42", Verified: "42", Rebooting: true})
		if res := r.cycle(); !contains(res.TasksAwaitingApproval, "db") {
			t.Fatalf("[AC-52-3] want the gate: %+v\n%s", res, r.errOut)
		}
		// 後から書込済みと見えた（稼働版はまだ 41）: ゲートを閉じて続ける。
		if err := r.fp.SetFirmware("db", &fakeprovider.Firmware{Staged: "42", Verified: "42", Flashed: "42"}); err != nil {
			t.Fatal(err)
		}
		res := r.cycleOn(r.d)
		if !contains(res.TasksResumed, "db") || len(res.TasksAwaitingApproval) != 0 {
			t.Fatalf("[AC-52-3] flash observed while gated: want resumed without approval: %+v\n%s", res, r.errOut)
		}
		r.waitStep(task.StepRebooting)
		r.finishReboot()
		r.settle("AC-52-3", func() bool { return contains(r.cycle().TasksCompleted, "db") })
		r.wantStepCounts("AC-52-3", map[string]int{task.StepRebooting: 1})
		if rec, _ := r.record(); rec.ApprovedBy != "" {
			t.Fatalf("[AC-52-3] no approval was given: %+v", rec)
		}
	})
}

// [AC-52-4]
func TestPhasedTaskGate_RejectAndTimeoutHalt(t *testing.T) {
	for _, how := range []string{"reject", "timeout"} {
		t.Run(how, func(t *testing.T) {
			r := newPhasedRig(t, `{ "strategy": "task", "rollback_on_fail": true }`)
			r.seedCrashedInApplying(&fakeprovider.Firmware{Staged: "42", Verified: "42"})
			r.cycle()
			gate := r.wantGated("AC-52-4")
			switch how {
			case "reject":
				if _, err := task.Decide(context.Background(), task.NewFileStore(r.storePath), r.auditLog(), dbKey,
					task.Approval{Approver: "bob", Reject: true}, r.clock.Now()); err != nil {
					t.Fatalf("[AC-52-4] reject: %v", err)
				}
			case "timeout":
				r.clock.Set(gate.GateDeadline)
				// 上限を過ぎた承認は効かない。
				_, err := task.Decide(context.Background(), task.NewFileStore(r.storePath), r.auditLog(), dbKey,
					task.Approval{Approver: "alice"}, r.clock.Now())
				if err == nil || !strings.Contains(err.Error(), "timed out") {
					t.Fatalf("[AC-52-4] approval after the deadline: err = %v", err)
				}
			}
			for c := 0; c < 3; c++ {
				res := r.cycle()
				if len(res.TasksResumed) != 0 || len(res.TasksLaunched) != 0 || len(res.TasksAwaitingApproval) != 0 || !contains(res.TasksHeld, "db") {
					t.Fatalf("[AC-52-4] %s: must halt: %+v\n%s", how, res, r.errOut)
				}
			}
			rec, _ := r.record()
			if rec.Phase != task.PhaseFailed || rec.RolledBack {
				t.Fatalf("[AC-52-4] %s: record = %+v, want failed without rollback", how, rec)
			}
			want := map[string]string{"reject": "rejected by bob", "timeout": "approval gate timed out"}[how]
			if !strings.Contains(rec.Error, want) || !strings.Contains(rec.Error, "critical section") {
				t.Fatalf("[AC-52-4] %s: error %q must say %q", how, rec.Error, want)
			}
			if r.rollbacks() != 0 {
				t.Fatalf("[AC-52-4] %s: a device that may be bricked must not be rolled back automatically", how)
			}
			r.wantStepCounts("AC-52-4", map[string]int{})
		})
	}
}

// [AC-52-5]
func TestPhasedTaskGate_CLI(t *testing.T) {
	r := newPhasedRig(t, taskPolicy)
	auditPath := filepath.Join(r.root, "audit.log")

	// ゲートで待っていない Task は承認できない。
	out, code := runSakutei(t, "task", "approve", "--store", r.storePath, "--audit-log", auditPath, "--approver", "alice", dbKey)
	if code == 0 || !strings.Contains(out, "no task awaiting approval") {
		t.Fatalf("[AC-52-5] approve without a gate (exit %d):\n%s", code, out)
	}
	_, err := task.Decide(context.Background(), task.NewFileStore(r.storePath), r.auditLog(), dbKey, task.Approval{Approver: "alice"}, r.clock.Now())
	if !errors.Is(err, task.ErrNotAwaitingApproval) {
		t.Fatalf("[AC-52-5] Decide without a gate: err = %v", err)
	}

	// CLI は実時刻で上限を比べるので、ゲートは実時刻で開く。
	r.clock.Set(time.Now())
	r.seedCrashedInApplying(&fakeprovider.Firmware{Staged: "42", Verified: "42"})
	r.cycle()
	r.wantGated("AC-52-5")
	out, code = runSakutei(t, "task", "list", "--store", r.storePath)
	if code != 0 || !strings.Contains(out, "awaiting_approval:applying") {
		t.Fatalf("[AC-52-5] task list must show the gate (exit %d):\n%s", code, out)
	}
	out, code = runSakutei(t, "task", "approve", "--store", r.storePath, "--approver", "alice", dbKey)
	if code == 0 {
		t.Fatalf("[AC-52-5] approve without --audit-log must be refused:\n%s", out)
	}
	out, code = runSakutei(t, "task", "approve", "--store", r.storePath, "--audit-log", auditPath,
		"--approver", "alice", "--role", "platform-admin", "--signature", "sig-a", dbKey)
	if code != 0 || !strings.Contains(out, "approved task old-1") {
		t.Fatalf("[AC-52-5] sakutei task approve (exit %d):\n%s", code, out)
	}
	recs, err := audit.ReadAll(auditPath)
	if err != nil || len(recs) != 1 {
		t.Fatalf("[AC-52-5] audit log: %v %+v", err, recs)
	}
	if a := recs[0]; a.Kind != audit.KindTaskGateApproved || a.Actor != "alice" || a.Resource != dbKey ||
		a.RunID != "old-1" || a.Gate != task.StepApplying || a.Signature != "sig-a" || len(a.Roles) != 1 {
		t.Fatalf("[AC-52-5] audit record = %+v", a)
	}
	if rec, _ := r.record(); rec.Phase != task.PhaseRunning || !rec.Released || rec.ApprovedBy != "alice" {
		t.Fatalf("[AC-52-5] approved record = %+v", rec)
	}
	// 2 度目の承認は効かない（もう待っていない）。
	if out, code = runSakutei(t, "task", "reject", "--store", r.storePath, "--audit-log", auditPath, "--approver", "bob", dbKey); code == 0 {
		t.Fatalf("[AC-52-5] reject after the approval must be refused:\n%s", out)
	}
	if res := r.cycleOn(r.d); !contains(res.TasksResumed, "db") {
		t.Fatalf("[AC-52-5] approved Task must be resumed: %+v\n%s", res, r.errOut)
	}
	r.d.StopTasks()
	r.d.Wait(5 * time.Second)
}
