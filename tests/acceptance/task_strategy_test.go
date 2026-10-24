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

// task_strategy_test.go: 単純 Task と strategy: task（#41、§6.2, §6.5）。
//
// 常駐ループを reconcile.Daemon で1周ずつ回す。Task の実行体は fake provider
// （RunTask の実行回数を state file に数える）。
//
//	AC-v1.5-task-1 — 差分がある間も Task は 1 つしか起動されず、Apply もされない（1-outstanding）
//	AC-v1.5-task-2 — 完了印を Observe できたら記録が消え、以後 Task は起動されない
//	AC-v1.5-task-3 — 常駐ループを再起動しても二重実行しない（実行中の中断・完了印あり・別インスタンス同時）
//	AC-v1.5-task-4 — メンテ窓の外では起動せず、窓に入った周で起動する
//	AC-v1.5-task-5 — 失敗は halt（rollback_on_fail なら rollback）。cancel 後に差分が残れば改めて起動する
//	AC-v1.5-task-6 — 成功を返しても完了印が Observe できなければ再実行しない
//	AC-v1.5-task-7 — task store が無ければ Task も Apply もしない／下流は Task の完了を待つ
//	AC-v1.5-task-8 — update_policy の宣言の誤りはロード時に拒否される
//	AC-v1.5-task-9 — 1 回収束（sakutei reconcile）も strategy: task のリソースを Apply しない
//	AC-112-1 — 自分が起動した Task が走っている間は、周を重ねても DriftEvent が stuck（critical）に上がらない（§22.3、v3.18）
//	AC-112-2 — RequiresTask の起動しない理由（窓の外）が続く間、同じ理由は最初の周だけ出て毎周は出ない（level、§22.3、v3.18）
package acceptance

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/fakeprovider"
	"github.com/tjst-t/sakutei/internal/loader"
	"github.com/tjst-t/sakutei/internal/reconcile"
	"github.com/tjst-t/sakutei/internal/task"
	"github.com/tjst-t/sakutei/pkg/core"
)

// taskIntent は migration 役の db（strategy: task、size = スキーマ版）と、それに依存する app。
func taskIntent(version, policy string) string {
	return `{
  "resources": [
    { "provider": "fake", "id": "db", "update_policy": ` + policy + `,
      "fields": { "name": "db", "size": "` + version + `" } },
    { "provider": "fake", "id": "app", "depends_on": ["db"], "fields": { "name": "app", "size": "1" } }
  ]
}`
}

const taskPolicy = `{ "strategy": "task" }`

// dbKey は store の記録のキー（core.ID.String()）。
var dbKey = core.NewID("fake", core.NewScope(nil), "db").String()

type taskClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *taskClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *taskClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

type taskRig struct {
	t         *testing.T
	root      string
	intentDir string
	storePath string
	fp        *fakeprovider.Provider
	clock     *taskClock
	d         *reconcile.Daemon
	errOut    *syncBuf
}

// syncBuf は複数 goroutine から書かれる ErrOut。
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newTaskRig(t *testing.T, intent string) *taskRig {
	t.Helper()
	root := t.TempDir()
	r := &taskRig{
		t:         t,
		root:      root,
		intentDir: filepath.Join(root, "intent"),
		storePath: filepath.Join(root, "tasks.json"),
		fp:        fakeprovider.New(filepath.Join(root, "state.json")),
		clock:     &taskClock{now: time.Date(2026, 10, 24, 3, 0, 0, 0, time.UTC)}, // 土曜 03:00
	}
	if err := os.MkdirAll(r.intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.intentDir, "intent.json"), []byte(intent), 0o644); err != nil {
		t.Fatal(err)
	}
	mustSeed(t, r.fp, "db", 41)
	mustSeed(t, r.fp, "app", 1)
	r.d = r.newDaemon(true)
	return r
}

// newDaemon は同じ intent・現実・task store（S2 ファイル）で新しいデーモンを作る
// （＝常駐ループの再起動）。store が false なら task store を渡さない。
func (r *taskRig) newDaemon(store bool) *reconcile.Daemon {
	r.errOut = &syncBuf{}
	cfg := reconcile.Config{
		IntentDir:    r.intentDir,
		GeneratedDir: filepath.Join(r.root, "generated"),
		Provider:     r.fp,
		Out:          r.errOut,
		ErrOut:       r.errOut,
		Now:          r.clock.Now,
	}
	if store {
		cfg.TaskStore = task.NewFileStore(r.storePath)
	}
	return reconcile.NewDaemon(cfg)
}

func (r *taskRig) cycleOn(d *reconcile.Daemon) reconcile.CycleResult {
	r.t.Helper()
	res, err := d.RunCycle(context.Background())
	if err != nil {
		r.t.Fatalf("RunCycle: %v", err)
	}
	return res
}

// cycle は1周回し、Apply と（止められていない）Task が終わるまで待つ。
func (r *taskRig) cycle() reconcile.CycleResult {
	r.t.Helper()
	res := r.cycleOn(r.d)
	r.d.Wait(5 * time.Second)
	return res
}

func (r *taskRig) runs() int {
	r.t.Helper()
	runs, _, err := r.fp.TaskCounts()
	if err != nil {
		r.t.Fatal(err)
	}
	return runs["db"]
}

func (r *taskRig) rollbacks() int {
	r.t.Helper()
	_, rb, err := r.fp.TaskCounts()
	if err != nil {
		r.t.Fatal(err)
	}
	return rb["db"]
}

func (r *taskRig) applies(local string) int {
	r.t.Helper()
	c, err := r.fp.GetApplyCount()
	if err != nil {
		r.t.Fatal(err)
	}
	return c[local]
}

func (r *taskRig) record() (task.Record, bool) {
	r.t.Helper()
	rec, ok, err := task.NewFileStore(r.storePath).Get(context.Background(), dbKey)
	if err != nil {
		r.t.Fatal(err)
	}
	return rec, ok
}

// waitRuns は実行体が n 回呼ばれるまで待つ（起動は非同期）。
func (r *taskRig) waitRuns(n int) {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for r.runs() < n {
		if time.Now().After(deadline) {
			r.t.Fatalf("task ran %d time(s), want %d", r.runs(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitPhase は記録が phase になるまで待つ。
func (r *taskRig) waitPhase(phase task.Phase) {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if rec, ok := r.record(); ok && rec.Phase == phase {
			return
		}
		if time.Now().After(deadline) {
			rec, ok := r.record()
			r.t.Fatalf("task record = %+v (present=%v), want phase %s", rec, ok, phase)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// [AC-v1.5-task-1]
func TestTaskStrategy_OneOutstandingWhileDiffPersists(t *testing.T) {
	r := newTaskRig(t, taskIntent("42", taskPolicy))
	release := r.fp.HoldTask("db") // 長い migration: 実行中は差分が残り続ける
	defer release()

	for c := 1; c <= 5; c++ {
		res := r.cycleOn(r.d)
		if c == 1 && !contains(res.TasksLaunched, "db") {
			t.Fatalf("[AC-v1.5-task-1] cycle 1: Task not launched for db: %+v\n%s", res, r.errOut)
		}
		if c > 1 && (len(res.TasksLaunched) != 0 || !contains(res.TasksHeld, "db")) {
			t.Fatalf("[AC-v1.5-task-1] cycle %d: want db held with its Task outstanding, got launched=%v held=%v",
				c, res.TasksLaunched, res.TasksHeld)
		}
	}
	r.waitRuns(1)
	if got := r.runs(); got != 1 {
		t.Fatalf("[AC-v1.5-task-1] Task ran %d times over 5 cycles with the diff present, want 1", got)
	}
	if got := r.applies("db"); got != 0 {
		t.Fatalf("[AC-v1.5-task-1] strategy: task resource was Applied %d time(s); must never be Applied", got)
	}
	if rec, ok := r.record(); !ok || rec.Phase != task.PhaseRunning {
		t.Fatalf("[AC-v1.5-task-1] S2 record = %+v (present=%v), want phase running", rec, ok)
	}
	release()
	r.d.Wait(5 * time.Second)
}

// [AC-112-1] Backlog #112, v3.18, §22.3: 自分が起動した Task が走っている間は、
// stuck-after（既定 5）を超えて周を重ねても DriftEvent が stuck（critical）に上がらない。
func TestTaskStrategy_RunningDoesNotEscalateDriftToStuck(t *testing.T) {
	r := newTaskRig(t, taskIntent("42", taskPolicy))
	release := r.fp.HoldTask("db") // 長い migration: 実行中は差分が残り続ける
	defer release()

	// 既定の stuck-after（5）を超えて回す。Task は持ったままなので、
	// 毎周 DriftEvent を出していたらここで stuck へ昇格してしまう。
	for c := 1; c <= 7; c++ {
		r.cycleOn(r.d)
	}
	r.waitRuns(1)
	if strings.Contains(r.errOut.String(), "stuck") {
		t.Fatalf("[AC-112-1] Task 実行中に DriftEvent が stuck へ上がった（7 周実行後）:\n%s", r.errOut)
	}
	release()
	r.d.Wait(5 * time.Second)
}

// [AC-112-2] Backlog #112, v3.18, §22.3: RequiresTask の起動しない理由（窓の外）が
// 連続していても、同じ理由は最初の周だけ出て毎周は出ない（level）。
func TestTaskStrategy_RequiresTaskNoLaunchReasonIsLevel(t *testing.T) {
	r := newTaskRig(t, taskIntent("42", `{ "strategy": "task", "window": "Sat 02:00-04:00 UTC" }`))
	r.clock.Set(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)) // 水曜 12:00、窓の外

	// stuck-after（既定 5）より少ない回数だけ回す（stuck への昇格とは別の話）。
	for c := 1; c <= 4; c++ {
		res := r.cycle()
		if len(res.TasksLaunched) != 0 || !contains(res.TasksHeld, "db") {
			t.Fatalf("[AC-112-2] cycle %d outside the window: launched=%v held=%v", c, res.TasksLaunched, res.TasksHeld)
		}
	}
	const reason = "no Task launched (outside-window)"
	if got := strings.Count(r.errOut.String(), reason); got != 1 {
		t.Fatalf("[AC-112-2] 同じ理由が %d 回出た（edge、毎周）、want 1（level、最初の周だけ）:\n%s", got, r.errOut)
	}
}

// [AC-v1.5-task-2]
func TestTaskStrategy_CompletionMarkObservedStopsLaunching(t *testing.T) {
	r := newTaskRig(t, taskIntent("42", taskPolicy))

	if res := r.cycle(); !contains(res.TasksLaunched, "db") {
		t.Fatalf("[AC-v1.5-task-2] cycle 1: Task not launched: %+v", res)
	}
	r.waitPhase(task.PhaseSucceeded) // 実行体は成功を返した。まだ完了印は Observe していない

	res := r.cycle()
	if !contains(res.TasksCompleted, "db") {
		t.Fatalf("[AC-v1.5-task-2] cycle 2: completion mark (size=42) observed but record not settled: %+v\n%s", res, r.errOut)
	}
	if _, ok := r.record(); ok {
		t.Fatal("[AC-v1.5-task-2] S2 record must be gone once the completion mark is observed")
	}
	for c := 3; c <= 6; c++ {
		res := r.cycle()
		if len(res.TasksLaunched) != 0 || len(res.TasksHeld) != 0 {
			t.Fatalf("[AC-v1.5-task-2] cycle %d: converged resource got a Task decision: %+v", c, res)
		}
	}
	if got := r.runs(); got != 1 {
		t.Fatalf("[AC-v1.5-task-2] Task ran %d times, want exactly 1", got)
	}
	if mustSize(t, r.fp, "db") != 42 {
		t.Fatal("[AC-v1.5-task-2] completion mark (size=42) not in reality")
	}
	// 下流の app は db の完了後に通常どおり収束する（ここでは元から収束済み）。
	if got := r.applies("db"); got != 0 {
		t.Fatalf("[AC-v1.5-task-2] db was Applied %d time(s)", got)
	}
}

// [AC-v1.5-task-3]
func TestTaskStrategy_RestartDoesNotRunTwice(t *testing.T) {
	t.Run("interrupted while running", func(t *testing.T) {
		r := newTaskRig(t, taskIntent("42", taskPolicy))
		release := r.fp.HoldTask("db")
		defer release()
		r.cycleOn(r.d)
		r.waitRuns(1)

		// デーモンが Task の途中で落ちて、新しいデーモンが同じ S2 で立ち上がる。
		d2 := r.newDaemon(true)
		for c := 1; c <= 4; c++ {
			res := r.cycleOn(d2)
			if len(res.TasksLaunched) != 0 || !contains(res.TasksHeld, "db") {
				t.Fatalf("[AC-v1.5-task-3] restarted cycle %d: want db held, got launched=%v held=%v",
					c, res.TasksLaunched, res.TasksHeld)
			}
		}
		if got := r.runs(); got != 1 {
			t.Fatalf("[AC-v1.5-task-3] Task ran %d times across a restart, want 1", got)
		}
		if !strings.Contains(r.errOut.String(), "outstanding") {
			t.Fatalf("[AC-v1.5-task-3] restarted daemon must report the outstanding Task:\n%s", r.errOut)
		}

		// 中断された Task が実は現実に完了印を残していた → Observe で完了とみなす。
		release()
		r.d.Wait(5 * time.Second)
		if res := r.cycleOn(d2); !contains(res.TasksCompleted, "db") {
			t.Fatalf("[AC-v1.5-task-3] restarted daemon must settle once the mark is observed: %+v", res)
		}
		for c := 0; c < 3; c++ {
			r.cycleOn(d2)
			d2.Wait(5 * time.Second)
		}
		if got := r.runs(); got != 1 {
			t.Fatalf("[AC-v1.5-task-3] Task ran %d times, want 1", got)
		}
	})

	t.Run("mark already in reality after restart", func(t *testing.T) {
		r := newTaskRig(t, taskIntent("42", taskPolicy))
		// 前のデーモンが起動の記録を残し、Task は完了印を書いたところで落ちた。
		if _, started, err := task.NewFileStore(r.storePath).Begin(context.Background(), task.Record{
			Resource: dbKey, TaskID: "old-1", Phase: task.PhaseRunning, Holder: "old"}); err != nil || !started {
			t.Fatalf("seed record: started=%v err=%v", started, err)
		}
		mustSeed(t, r.fp, "db", 42)

		res := r.cycle()
		if !contains(res.TasksCompleted, "db") || len(res.TasksLaunched) != 0 {
			t.Fatalf("[AC-v1.5-task-3] want the old record settled without a new Task: %+v", res)
		}
		if got := r.runs(); got != 0 {
			t.Fatalf("[AC-v1.5-task-3] Task ran %d times; reality already had the mark", got)
		}
	})

	t.Run("two daemons on one store", func(t *testing.T) {
		r := newTaskRig(t, taskIntent("42", taskPolicy))
		release := r.fp.HoldTask("db")
		defer release()
		d2 := r.newDaemon(true)
		var wg sync.WaitGroup
		for _, d := range []*reconcile.Daemon{r.d, d2} {
			wg.Add(1)
			go func(d *reconcile.Daemon) {
				defer wg.Done()
				for c := 0; c < 3; c++ {
					if _, err := d.RunCycle(context.Background()); err != nil {
						t.Errorf("RunCycle: %v", err)
					}
				}
			}(d)
		}
		wg.Wait()
		r.waitRuns(1)
		time.Sleep(20 * time.Millisecond)
		if got := r.runs(); got != 1 {
			t.Fatalf("[AC-v1.5-task-3] two daemons on one S2 store ran the Task %d times, want 1", got)
		}
		release()
		r.d.Wait(5 * time.Second)
		d2.Wait(5 * time.Second)
	})
}

// [AC-v1.5-task-4]
func TestTaskStrategy_NotLaunchedOutsideWindow(t *testing.T) {
	r := newTaskRig(t, taskIntent("42", `{ "strategy": "task", "window": "Sat 02:00-04:00 UTC" }`))
	r.clock.Set(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)) // 水曜 12:00

	for c := 1; c <= 3; c++ {
		res := r.cycle()
		if len(res.TasksLaunched) != 0 || !contains(res.TasksHeld, "db") {
			t.Fatalf("[AC-v1.5-task-4] cycle %d outside the window: launched=%v held=%v", c, res.TasksLaunched, res.TasksHeld)
		}
	}
	r.clock.Set(time.Date(2026, 10, 24, 4, 0, 0, 0, time.UTC)) // 土曜 04:00（終わりは含まない）
	if res := r.cycle(); len(res.TasksLaunched) != 0 {
		t.Fatalf("[AC-v1.5-task-4] launched at the window end: %+v", res)
	}
	if got := r.runs(); got != 0 {
		t.Fatalf("[AC-v1.5-task-4] Task ran %d time(s) outside the window", got)
	}
	if _, ok := r.record(); ok {
		t.Fatal("[AC-v1.5-task-4] no S2 record may be written outside the window")
	}
	if !strings.Contains(r.errOut.String(), "outside maintenance window") {
		t.Fatalf("[AC-v1.5-task-4] the hold must be reported:\n%s", r.errOut)
	}

	r.clock.Set(time.Date(2026, 10, 24, 2, 30, 0, 0, time.UTC)) // 土曜 02:30
	if res := r.cycle(); !contains(res.TasksLaunched, "db") {
		t.Fatalf("[AC-v1.5-task-4] inside the window the Task must launch: %+v", res)
	}
	if got := r.runs(); got != 1 {
		t.Fatalf("[AC-v1.5-task-4] Task ran %d times, want 1", got)
	}
	if got := r.applies("db"); got != 0 {
		t.Fatalf("[AC-v1.5-task-4] db was Applied %d time(s)", got)
	}
}

// [AC-v1.5-task-5]
func TestTaskStrategy_FailureHaltsUntilCancelled(t *testing.T) {
	r := newTaskRig(t, taskIntent("42", `{ "strategy": "task", "rollback_on_fail": true }`))
	if err := r.fp.SetTaskFailInject(map[string]string{"db": "migration error"}); err != nil {
		t.Fatal(err)
	}
	r.cycle()
	r.waitPhase(task.PhaseFailed)
	for c := 2; c <= 4; c++ {
		if res := r.cycle(); len(res.TasksLaunched) != 0 {
			t.Fatalf("[AC-v1.5-task-5] cycle %d: a failed Task must halt, not retry: %+v", c, res)
		}
	}
	if r.runs() != 1 || r.rollbacks() != 1 {
		t.Fatalf("[AC-v1.5-task-5] runs=%d rollbacks=%d, want 1 and 1", r.runs(), r.rollbacks())
	}
	if rec, _ := r.record(); !rec.RolledBack || !strings.Contains(rec.Error, "migration error") {
		t.Fatalf("[AC-v1.5-task-5] record must carry the failure and the rollback: %+v", rec)
	}

	// 人が現実を確かめて直し、cancel する → 差分が残っているので改めて起動する。
	if err := r.fp.SetTaskFailInject(nil); err != nil {
		t.Fatal(err)
	}
	out, code := runSakutei(t, "task", "cancel", "--store", r.storePath, dbKey)
	if code != 0 || !strings.Contains(out, "cancelled") {
		t.Fatalf("[AC-v1.5-task-5] sakutei task cancel (exit %d):\n%s", code, out)
	}
	if res := r.cycle(); !contains(res.TasksLaunched, "db") {
		t.Fatalf("[AC-v1.5-task-5] after cancel the remaining diff must launch a new Task: %+v", res)
	}
	r.cycle()
	if r.runs() != 2 || mustSize(t, r.fp, "db") != 42 {
		t.Fatalf("[AC-v1.5-task-5] runs=%d size=%d, want 2 and 42", r.runs(), mustSize(t, r.fp, "db"))
	}
	if _, ok := r.record(); ok {
		t.Fatal("[AC-v1.5-task-5] record must be settled after the mark is observed")
	}
}

// [AC-v1.5-task-6]
func TestTaskStrategy_SuccessWithoutMarkIsNotRerun(t *testing.T) {
	r := newTaskRig(t, taskIntent("42", taskPolicy))
	if err := r.fp.SetTaskSkipMark(map[string]bool{"db": true}); err != nil {
		t.Fatal(err)
	}
	r.cycle()
	r.waitPhase(task.PhaseSucceeded)
	for c := 2; c <= 4; c++ {
		if res := r.cycle(); len(res.TasksLaunched) != 0 || !contains(res.TasksHeld, "db") {
			t.Fatalf("[AC-v1.5-task-6] cycle %d: want held, got %+v", c, res)
		}
	}
	if got := r.runs(); got != 1 {
		t.Fatalf("[AC-v1.5-task-6] Task ran %d times, want 1 (completion is asked of reality, not re-run)", got)
	}

	// desired が変わった（次の migration）なら、成功済みの前の起動は完了しているので起動してよい。
	if err := r.fp.SetTaskSkipMark(nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.intentDir, "intent.json"), []byte(taskIntent("43", taskPolicy)), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := r.cycle(); !contains(res.TasksLaunched, "db") {
		t.Fatalf("[AC-v1.5-task-6] a new desired after a succeeded Task must launch: %+v", res)
	}
	if r.runs() != 2 || mustSize(t, r.fp, "db") != 43 {
		t.Fatalf("[AC-v1.5-task-6] runs=%d size=%d, want 2 and 43", r.runs(), mustSize(t, r.fp, "db"))
	}
}

// [AC-v1.5-task-7]
func TestTaskStrategy_NoStoreHoldsAndDownstreamWaits(t *testing.T) {
	r := newTaskRig(t, taskIntent("42", taskPolicy))
	// app も drift させておく: db（上流）が収束するまで Apply されてはならない。
	mustSeed(t, r.fp, "app", 9)

	r.d = r.newDaemon(false)
	for c := 1; c <= 3; c++ {
		res := r.cycle()
		if len(res.TasksLaunched) != 0 || !contains(res.TasksHeld, "db") {
			t.Fatalf("[AC-v1.5-task-7] cycle %d without a task store: %+v", c, res)
		}
	}
	if r.runs() != 0 || r.applies("db") != 0 {
		t.Fatalf("[AC-v1.5-task-7] without S2 neither a Task (%d) nor an Apply (%d) may run", r.runs(), r.applies("db"))
	}
	if r.applies("app") != 0 {
		t.Fatal("[AC-v1.5-task-7] downstream app was Applied while its strategy: task upstream is not converged")
	}
	if !strings.Contains(r.errOut.String(), "no task store") {
		t.Fatalf("[AC-v1.5-task-7] the reason must be reported:\n%s", r.errOut)
	}

	// store ありで回すと Task → 完了印 → 次の周で app が収束する。
	r.d = r.newDaemon(true)
	r.cycle()
	r.cycle()
	if mustSize(t, r.fp, "app") != 1 {
		t.Fatalf("[AC-v1.5-task-7] app must converge after db's Task completes (size=%d)", mustSize(t, r.fp, "app"))
	}
	if r.runs() != 1 || r.applies("db") != 0 {
		t.Fatalf("[AC-v1.5-task-7] runs=%d applies(db)=%d, want 1 and 0", r.runs(), r.applies("db"))
	}
}

// [AC-v1.5-task-8]
func TestTaskStrategy_LoaderValidatesUpdatePolicy(t *testing.T) {
	load := func(policy string) (loader.Intent, error) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "intent.json"), []byte(taskIntent("42", policy)), 0o644); err != nil {
			t.Fatal(err)
		}
		return loader.LoadIntent(dir)
	}
	in, err := load(`{ "strategy": "task", "window": "Mon-Fri 22:00-06:00", "rollback_on_fail": true }`)
	if err != nil {
		t.Fatalf("[AC-v1.5-task-8] valid update_policy rejected: %v", err)
	}
	want := core.UpdatePolicy{Strategy: core.UpdateStrategyTask, Window: "Mon-Fri 22:00-06:00", RollbackOnFail: true}
	if got := in.Resources[0].UpdatePolicy; got != want {
		t.Fatalf("[AC-v1.5-task-8] UpdatePolicy = %+v, want %+v", got, want)
	}
	if in.Resources[1].UpdatePolicy.IsTask() {
		t.Fatal("[AC-v1.5-task-8] a resource without update_policy must keep the Apply strategy")
	}
	for _, bad := range []string{
		`{ "strategy": "taks" }`,
		`{ "strategy": "task", "window": "Sat 25:00-04:00" }`,
		`{ "strategy": "task", "window": "Caturday 02:00-04:00" }`,
		`{ "window": "Sat 02:00-04:00" }`,
		`{ "strategy": "task", "retry": true }`,
	} {
		if _, err := load(bad); err == nil {
			t.Errorf("[AC-v1.5-task-8] update_policy %s must be rejected at load", bad)
		}
	}
}

// [AC-v1.5-task-9]
func TestTaskStrategy_OneShotReconcileDoesNotApply(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	initStateFile(t, statePath)
	fp := fakeprovider.New(statePath)
	mustSeed(t, fp, "db", 41)
	mustSeed(t, fp, "app", 9)
	if err := os.WriteFile(filepath.Join(intentDir, "intent.json"), []byte(taskIntent("42", taskPolicy)), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runReconcileFull(t, intentDir, statePath)
	if code != 0 {
		t.Fatalf("[AC-v1.5-task-9] reconcile exit %d:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "RequiresTaskEvent") {
		t.Fatalf("[AC-v1.5-task-9] reconcile must report RequiresTaskEvent:\n%s", stderr)
	}
	if !strings.Contains(stdout, "await their Task") {
		t.Fatalf("[AC-v1.5-task-9] reconcile must not claim convergence:\n%s", stdout)
	}
	if mustSize(t, fp, "db") != 41 || mustSize(t, fp, "app") != 9 {
		t.Fatalf("[AC-v1.5-task-9] one-shot reconcile changed db (%d) or its downstream app (%d)",
			mustSize(t, fp, "db"), mustSize(t, fp, "app"))
	}
	if c, _ := fp.GetApplyCount(); c["db"] != 0 {
		t.Fatalf("[AC-v1.5-task-9] db was Applied %d time(s)", c["db"])
	}
}
