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

// task_phased_test.go: 多相 Task（#51、§6.2 多相型）。
//
// fake provider の db を firmware 更新の対象にする（size = 稼働中の firmware 版、
// 41 → 42）。相は staging → verifying → applying → rebooting → confirming。
// rebooting の間 db は Observe できない（unknown）。
//
//	AC-51-1 — 多相 Task が全相を通り、完了印（稼働版 42）を Observe して Done になる。リブート中の unknown は absent と取らず待つ
//	AC-51-2 — 相ごとにデーモンを止めて起こし直しても（相の効果は済んだ後）、どの相も二重実行しない
//	AC-51-3 — 相ごとにデーモンが落ちても（kill -9、lease 切れ）、相の入口の Observe で済んだ相を飛ばして続きを引き継ぐ。lease が生きている間は引き継がない
//	AC-51-4 — リブート中の unknown はタイムアウトまで待ち、過ぎたら Failed（halt、rollback_on_fail なら rollback）
//	AC-51-5 — 窓と 1-outstanding は単純 Task と同じ（窓の外では起動しない・進行中の Task は窓の外でも引き継ぐ）
package acceptance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/fakeprovider"
	"github.com/tjst-t/sakutei/v2/internal/task"
)

var firmwareActionSteps = []string{task.StepStaging, task.StepVerifying, task.StepApplying, task.StepRebooting}

// newPhasedRig は db を firmware 更新（多相 Task）の対象にした rig。
func newPhasedRig(t *testing.T, policy string) *taskRig {
	t.Helper()
	r := newTaskRig(t, taskIntent("42", policy))
	if err := r.fp.SetTaskKind("db", fakeprovider.TaskKindFirmware); err != nil {
		t.Fatal(err)
	}
	r.poll = 2 * time.Millisecond
	r.d = r.newDaemon(true)
	return r
}

func (r *taskRig) stepCounts() map[string]int {
	r.t.Helper()
	c, err := r.fp.TaskStepCounts("db")
	if err != nil {
		r.t.Fatal(err)
	}
	return c
}

// wantStepCounts は各相の RunStep の回数を確かめる。
func (r *taskRig) wantStepCounts(tag string, want map[string]int) {
	r.t.Helper()
	got := r.stepCounts()
	for _, s := range firmwareActionSteps {
		if got[s] != want[s] {
			r.t.Fatalf("[%s] step %s ran %d time(s), want %d (all: %v)", tag, s, got[s], want[s], got)
		}
	}
}

// waitRecord は記録が cond を満たすまで待つ。
func (r *taskRig) waitRecord(what string, cond func(task.Record, bool) bool) task.Record {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec, ok := r.record()
		if cond(rec, ok) {
			return rec
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("task record = %+v (present=%v), want %s\n%s", rec, ok, what, r.errOut)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (r *taskRig) waitStep(step string) task.Record {
	r.t.Helper()
	return r.waitRecord("step "+step, func(rec task.Record, ok bool) bool {
		return ok && rec.Phase == task.PhaseRunning && rec.Step == step
	})
}

func (r *taskRig) finishReboot() {
	r.t.Helper()
	if err := r.fp.FinishReboot("db"); err != nil {
		r.t.Fatal(err)
	}
}

// settle は Task の成功を待ち、次の周で完了印を Observe して記録が消えることを確かめる。
func (r *taskRig) settle(tag string, cycle func() bool) {
	r.t.Helper()
	r.waitPhase(task.PhaseSucceeded)
	if !cycle() {
		r.t.Fatalf("[%s] completion mark (running version 42) observed but the record was not settled\n%s", tag, r.errOut)
	}
	if _, ok := r.record(); ok {
		r.t.Fatalf("[%s] S2 record must be gone once Done", tag)
	}
	if mustSize(r.t, r.fp, "db") != 42 {
		r.t.Fatalf("[%s] running version is not 42", tag)
	}
	if got := r.applies("db"); got != 0 {
		r.t.Fatalf("[%s] strategy: task resource was Applied %d time(s)", tag, got)
	}
}

// [AC-51-1]
func TestPhasedTask_AllStepsToDone(t *testing.T) {
	r := newPhasedRig(t, taskPolicy)

	if res := r.cycleOn(r.d); !contains(res.TasksLaunched, "db") {
		t.Fatalf("[AC-51-1] cycle 1: phased Task not launched: %+v\n%s", res, r.errOut)
	}
	// Applying の後、Rebooting で db は見えなくなる。
	r.waitStep(task.StepRebooting)
	r.waitRecord("device rebooting", func(task.Record, bool) bool {
		fw, err := r.fp.GetFirmware("db")
		return err == nil && fw.Rebooting
	})
	for c := 1; c <= 4; c++ {
		res := r.cycleOn(r.d)
		if len(res.TasksLaunched) != 0 || len(res.TasksCompleted) != 0 || res.Pruned != 0 {
			t.Fatalf("[AC-51-1] rebooting cycle %d: unknown must be neither a new Task nor Done nor absent: %+v", c, res)
		}
		if res.Observe.Unknown == 0 {
			t.Fatalf("[AC-51-1] rebooting cycle %d: db must be reported unknown: %+v", c, res.Observe)
		}
	}
	if rec := r.waitStep(task.StepRebooting); rec.Holder == "" || rec.Released {
		t.Fatalf("[AC-51-1] rebooting record must stay held by the daemon: %+v", rec)
	}

	r.finishReboot()
	r.settle("AC-51-1", func() bool { return contains(r.cycle().TasksCompleted, "db") })
	r.wantStepCounts("AC-51-1", map[string]int{task.StepStaging: 1, task.StepVerifying: 1, task.StepApplying: 1, task.StepRebooting: 1})
	for c := 0; c < 3; c++ {
		if res := r.cycle(); len(res.TasksLaunched) != 0 || len(res.TasksHeld) != 0 {
			t.Fatalf("[AC-51-1] converged resource got a Task decision: %+v", res)
		}
	}
	r.wantStepCounts("AC-51-1", map[string]int{task.StepStaging: 1, task.StepVerifying: 1, task.StepApplying: 1, task.StepRebooting: 1})
}

// [AC-51-2]
func TestPhasedTask_RestartAtEachStepDoesNotRunTwice(t *testing.T) {
	for _, step := range firmwareActionSteps {
		t.Run(step, func(t *testing.T) {
			r := newPhasedRig(t, taskPolicy)
			reached, release := r.fp.HoldStep("db", step)
			defer release()
			if res := r.cycleOn(r.d); !contains(res.TasksLaunched, "db") {
				t.Fatalf("[AC-51-2] phased Task not launched: %+v\n%s", res, r.errOut)
			}
			select {
			case <-reached:
			case <-time.After(5 * time.Second):
				t.Fatalf("[AC-51-2] step %s never ran\n%s", step, r.errOut)
			}
			// 相の効果は現実に済んだ。その結果を S2 に書く前にデーモンが止まる。
			r.d.StopTasks()
			release()
			r.d.Wait(5 * time.Second)
			if rec := r.waitStep(step); !rec.Released {
				t.Fatalf("[AC-51-2] stopped daemon must release the record at step %s: %+v", step, rec)
			}

			// 起こし直したデーモンは止まった相から引き継ぎ、入口の Observe で済んだと分かる。
			d2 := r.newDaemon(true)
			res := r.cycleOn(d2)
			if !contains(res.TasksResumed, "db") || len(res.TasksLaunched) != 0 {
				t.Fatalf("[AC-51-2] restarted daemon must resume (not relaunch) at step %s: resumed=%v launched=%v\n%s",
					step, res.TasksResumed, res.TasksLaunched, r.errOut)
			}
			// 残りの相を進めてリブートに入る（step が rebooting なら、db は unknown の
			// まま引き継いで再出現を待っている）。
			r.waitStep(task.StepRebooting)
			r.waitRecord("device rebooting", func(task.Record, bool) bool {
				fw, err := r.fp.GetFirmware("db")
				return err == nil && fw.Rebooting
			})
			r.finishReboot()
			r.settle("AC-51-2", func() bool {
				res := r.cycleOn(d2)
				d2.Wait(5 * time.Second)
				return contains(res.TasksCompleted, "db")
			})
			r.wantStepCounts("AC-51-2", map[string]int{task.StepStaging: 1, task.StepVerifying: 1, task.StepApplying: 1, task.StepRebooting: 1})
		})
	}
}

// [AC-51-3]
func TestPhasedTask_CrashAtEachStepResumesWithoutRerun(t *testing.T) {
	desired := map[string]string{"name": "db", "size": "42"}
	cases := []struct {
		step string
		// fw は落ちた時点の現実（その相の効果は済んでいる）。
		fw *fakeprovider.Firmware
		// runningNew は稼働版がもう 42 か（confirming で落ちた）。
		runningNew bool
		want       map[string]int
	}{
		{task.StepStaging, &fakeprovider.Firmware{Staged: "42"}, false,
			map[string]int{task.StepVerifying: 1, task.StepApplying: 1, task.StepRebooting: 1}},
		{task.StepVerifying, &fakeprovider.Firmware{Staged: "42", Verified: "42"}, false,
			map[string]int{task.StepApplying: 1, task.StepRebooting: 1}},
		{task.StepApplying, &fakeprovider.Firmware{Staged: "42", Verified: "42", Flashed: "42"}, false,
			map[string]int{task.StepRebooting: 1}},
		{task.StepRebooting, &fakeprovider.Firmware{Staged: "42", Verified: "42", Flashed: "42", Rebooting: true}, false,
			map[string]int{}},
		{task.StepConfirming, &fakeprovider.Firmware{Staged: "42", Verified: "42", Flashed: "42", Rebooting: true}, false,
			map[string]int{}},
		{task.StepConfirming + "/back", &fakeprovider.Firmware{Staged: "42", Verified: "42", Flashed: "42"}, true,
			map[string]int{}},
	}
	for _, tc := range cases {
		t.Run(tc.step, func(t *testing.T) {
			step, _, _ := strings.Cut(tc.step, "/")
			r := newPhasedRig(t, taskPolicy)
			r.lease = time.Minute
			r.d = r.newDaemon(true)
			if err := r.fp.SetFirmware("db", tc.fw); err != nil {
				t.Fatal(err)
			}
			if tc.runningNew {
				mustSeed(t, r.fp, "db", 42)
			}
			// 前のデーモンは step の途中で落ちた（kill -9）: 記録は running のまま、
			// lease はまだ生きている。
			now := r.clock.Now()
			if _, started, err := task.NewFileStore(r.storePath).Begin(context.Background(), task.Record{
				Resource: dbKey, TaskID: "old-1", Phase: task.PhaseRunning, Holder: "old-host-1",
				DesiredHash: task.DesiredHash(desired), StartedAt: now.Add(-10 * time.Minute),
				Step: step, StepStartedAt: now.Add(-time.Minute), HeartbeatAt: now,
			}); err != nil || !started {
				t.Fatalf("seed: started=%v err=%v", started, err)
			}

			if tc.runningNew {
				// 稼働版はもう 42: Observe に差分が無いので、そのまま Done。
				if res := r.cycle(); !contains(res.TasksCompleted, "db") || len(res.TasksResumed) != 0 {
					t.Fatalf("[AC-51-3] running version already 42: want settled without resuming: %+v\n%s", res, r.errOut)
				}
				r.wantStepCounts("AC-51-3", tc.want)
				return
			}

			// lease が生きている間は引き継がない（別のデーモンで走っているかもしれない）。
			for c := 0; c < 2; c++ {
				res := r.cycle()
				if len(res.TasksResumed) != 0 || len(res.TasksLaunched) != 0 {
					t.Fatalf("[AC-51-3] live lease: must not take over: %+v\n%s", res, r.errOut)
				}
			}
			if got := r.stepCounts(); len(got) != 0 {
				t.Fatalf("[AC-51-3] live lease: steps ran: %v", got)
			}

			// lease が切れた: 落ちたものとして引き継ぐ。
			r.clock.Set(now.Add(2 * time.Minute))
			res := r.cycleOn(r.d)
			if !contains(res.TasksResumed, "db") {
				t.Fatalf("[AC-51-3] stale lease: want resumed at %s: %+v\n%s", step, res, r.errOut)
			}
			if step != task.StepConfirming {
				r.waitStep(task.StepRebooting)
			}
			r.waitRecord("device rebooting", func(task.Record, bool) bool {
				fw, err := r.fp.GetFirmware("db")
				return err == nil && fw.Rebooting
			})
			r.finishReboot()
			r.settle("AC-51-3", func() bool { return contains(r.cycle().TasksCompleted, "db") })
			r.wantStepCounts("AC-51-3", tc.want)
		})
	}
}

// [AC-51-4]
func TestPhasedTask_RebootTimeout(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		name := "halt"
		policy := taskPolicy
		if rollback {
			name, policy = "rollback", `{ "strategy": "task", "rollback_on_fail": true }`
		}
		t.Run(name, func(t *testing.T) {
			r := newPhasedRig(t, policy)
			start := r.clock.Now()
			r.cycleOn(r.d)
			rec := r.waitStep(task.StepRebooting)

			// タイムアウトの手前: 見えないまま待ち続ける（absent と取らない）。
			r.clock.Set(rec.StepStartedAt.Add(fakeprovider.FirmwareRebootTimeout - time.Minute))
			for c := 0; c < 3; c++ {
				res := r.cycleOn(r.d)
				if len(res.TasksLaunched) != 0 || len(res.TasksCompleted) != 0 || res.Pruned != 0 {
					t.Fatalf("[AC-51-4] before the timeout: %+v", res)
				}
			}
			time.Sleep(20 * time.Millisecond) // 何度か CheckStep が回る
			if rec := r.waitStep(task.StepRebooting); rec.Phase != task.PhaseRunning {
				t.Fatalf("[AC-51-4] before the timeout the Task must still wait: %+v", rec)
			}

			// タイムアウトを過ぎた: Failed（halt）。
			r.clock.Set(start.Add(fakeprovider.FirmwareRebootTimeout + time.Minute))
			r.d.Wait(5 * time.Second)
			rec = r.waitRecord("phase failed", func(rec task.Record, ok bool) bool { return ok && rec.Phase == task.PhaseFailed })
			if !strings.Contains(rec.Error, "rebooting") || !strings.Contains(rec.Error, "unknown ≠ absent") {
				t.Fatalf("[AC-51-4] failure must name the step and the unknown wait: %q", rec.Error)
			}
			if rec.RolledBack != rollback || r.rollbacks() != btoi(rollback) {
				t.Fatalf("[AC-51-4] rolled_back=%v rollbacks=%d, want %v", rec.RolledBack, r.rollbacks(), rollback)
			}
			// halt: 機器が戻って差分が見えても、人が cancel するまで起動・引き継ぎしない。
			r.finishReboot()
			mustSeed(t, r.fp, "db", 41)
			for c := 0; c < 3; c++ {
				res := r.cycle()
				if len(res.TasksLaunched) != 0 || len(res.TasksResumed) != 0 || !contains(res.TasksHeld, "db") {
					t.Fatalf("[AC-51-4] failed Task must halt: %+v", res)
				}
			}
			r.wantStepCounts("AC-51-4", map[string]int{task.StepStaging: 1, task.StepVerifying: 1, task.StepApplying: 1, task.StepRebooting: 1})
		})
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// [AC-51-5]
func TestPhasedTask_WindowAndOneOutstanding(t *testing.T) {
	const windowed = `{ "strategy": "task", "window": "Sat 02:00-04:00 UTC" }`
	t.Run("not launched outside the window", func(t *testing.T) {
		r := newPhasedRig(t, windowed)
		r.clock.Set(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)) // 水曜
		for c := 0; c < 3; c++ {
			if res := r.cycle(); len(res.TasksLaunched) != 0 || !contains(res.TasksHeld, "db") {
				t.Fatalf("[AC-51-5] outside the window: %+v", res)
			}
		}
		if got := r.stepCounts(); len(got) != 0 {
			t.Fatalf("[AC-51-5] steps ran outside the window: %v", got)
		}
	})

	t.Run("one outstanding while in progress", func(t *testing.T) {
		r := newPhasedRig(t, taskPolicy)
		reached, release := r.fp.HoldStep("db", task.StepStaging)
		defer release()
		r.cycleOn(r.d)
		<-reached
		d2 := r.newDaemon(true) // 同じ S2 の別のデーモン
		for c := 0; c < 3; c++ {
			res := r.cycleOn(r.d)
			res2 := r.cycleOn(d2)
			if len(res.TasksLaunched)+len(res2.TasksLaunched)+len(res2.TasksResumed) != 0 {
				t.Fatalf("[AC-51-5] in progress: no second Task: %+v / %+v", res, res2)
			}
		}
		release()
		r.waitStep(task.StepRebooting)
		r.finishReboot()
		r.settle("AC-51-5", func() bool { return contains(r.cycle().TasksCompleted, "db") })
		r.wantStepCounts("AC-51-5", map[string]int{task.StepStaging: 1, task.StepVerifying: 1, task.StepApplying: 1, task.StepRebooting: 1})
	})

	t.Run("resumed outside the window", func(t *testing.T) {
		r := newPhasedRig(t, `{ "strategy": "task", "window": "Sat 03:00-03:05 UTC" }`)
		reached, release := r.fp.HoldStep("db", task.StepRebooting)
		defer release()
		r.cycleOn(r.d) // 土曜 03:00、窓の中で起動
		<-reached
		r.d.StopTasks()
		release()
		r.d.Wait(5 * time.Second)

		// 窓を過ぎてから（リブートのタイムアウトの前に）起こし直した: リブート中の
		// 機器を置き去りにせず引き継ぐ。
		r.clock.Set(time.Date(2026, 10, 24, 3, 6, 0, 0, time.UTC))
		d2 := r.newDaemon(true)
		if res := r.cycleOn(d2); !contains(res.TasksResumed, "db") {
			t.Fatalf("[AC-51-5] in-progress Task must be resumed outside the window: %+v\n%s", res, r.errOut)
		}
		r.finishReboot()
		r.settle("AC-51-5", func() bool {
			res := r.cycleOn(d2)
			d2.Wait(5 * time.Second)
			return contains(res.TasksCompleted, "db")
		})
	})
}
