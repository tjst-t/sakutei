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

// manual_update_test.go — #150: 管理 API に更新の口が無い装置（§6.2「人の手順で
// 合わせる Task」）。fake provider の db の size を装置の版とみなし（41 → 42）、
// update_policy.strategy: manual を宣言する。Sakutei は版を観測し、承認ゲートで
// 人に作業を促し、観測で閉じる。装置には何も書かない（Apply も provider の
// 実行体も呼ばない）。
//
//	[AC-150-1]  版が違う → 知らせ（手順の文・URL・いまの版と望む版を承認ゲートと
//	            Event に出す）と承認待ち → 承認 → 観測の版を変える → 閉じる。閉じた
//	            あとで版が戻れば DriftEvent と新しい承認待ち。
//	[AC-150-2]  承認しても confirm_timeout までに版が変わらなければ閉じずに stuck
//	            として知らせる。後で観測の版が合えば閉じる。
//	[AC-150-3]  承認しなくても観測で版が合えば閉じる。
//	[AC-150-4]  宣言の誤り（手順の無い manual・manual 以外の procedure・manual の
//	            rollback_on_fail・読めない confirm_timeout）は読み込みで断る。
package acceptance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/audit"
	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/internal/task"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

const (
	manualProcedure = "upload the image from the switch GUI: System > Firmware"
	manualURL       = "https://wiki.example/switch-fw"
	manualPolicy    = `{ "strategy": "manual", "procedure": "` + manualProcedure + `", "procedure_url": "` + manualURL + `", "confirm_timeout": "1h" }`
)

// evBuf は Out / ErrOut を兼ね、Event をそのまま記録する（eventlog.Emitter）。
type evBuf struct {
	syncBuf
	mu  sync.Mutex
	evs []core.Event
}

func (b *evBuf) Emit(ev core.Event) {
	b.mu.Lock()
	b.evs = append(b.evs, ev)
	b.mu.Unlock()
	_, _ = b.Write([]byte(ev.TextLine() + "\n"))
}

// manual は db の strategy: manual の RequiresTask Event のうち、state のもの。
func (b *evBuf) manual(state string) []core.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []core.Event
	for _, ev := range b.evs {
		if ev.Kind == core.EventRequiresTask && ev.Resource.Local == "db" && ev.Attrs["update"] == "manual" && ev.Attrs["state"] == state {
			out = append(out, ev)
		}
	}
	return out
}

func (b *evBuf) drifts(local string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, ev := range b.evs {
		if ev.Kind == core.EventDrift && ev.Resource.Local == local {
			n++
		}
	}
	return n
}

type manualRig struct {
	*taskRig
	ev *evBuf
}

func newManualRig(t *testing.T) *manualRig {
	t.Helper()
	r := &manualRig{taskRig: newTaskRig(t, taskIntent("42", manualPolicy))}
	r.poll = 2 * time.Millisecond
	r.d = r.newManualDaemon()
	return r
}

func (r *manualRig) newManualDaemon() *reconcile.Daemon {
	r.ev = &evBuf{}
	r.errOut = &r.ev.syncBuf
	return reconcile.NewDaemon(reconcile.Config{
		IntentDir:        r.intentDir,
		GeneratedDir:     filepath.Join(r.root, "generated"),
		Provider:         r.fp,
		Out:              r.ev,
		ErrOut:           r.ev,
		Now:              r.clock.Now,
		TaskStore:        task.NewFileStore(r.storePath),
		TaskPollInterval: r.poll,
	})
}

// cycleOn は 1 周回し、level の報告を閉じる（§22.3。続かなかった Event を resolved に）。
func (r *manualRig) cycleOn(d *reconcile.Daemon) reconcile.CycleResult {
	r.t.Helper()
	res, err := d.RunCycle(context.Background())
	if err != nil {
		r.t.Fatalf("RunCycle: %v", err)
	}
	d.EndCycle(res, err)
	return res
}

func (r *manualRig) cycle() reconcile.CycleResult {
	r.t.Helper()
	res := r.cycleOn(r.d)
	r.d.Wait(5 * time.Second)
	return res
}

func (r *manualRig) approve() {
	r.t.Helper()
	log := audit.NewLog(filepath.Join(r.root, "audit.log"))
	if _, err := task.Decide(context.Background(), task.NewFileStore(r.storePath), log, dbKey,
		task.Approval{Approver: "alice"}, r.clock.Now()); err != nil {
		r.t.Fatal(err)
	}
}

// operatorSets は人が GUI で版を上げた（観測の版を変える）。
func (r *manualRig) operatorSets(version int) {
	r.t.Helper()
	mustSeed(r.t, r.fp, "db", version)
}

// nothingWritten は装置に何も書いていないこと（Apply も実行体も呼ばない）。
func (r *manualRig) nothingWritten(tag string) {
	r.t.Helper()
	if got := r.applies("db"); got != 0 {
		r.t.Fatalf("[%s] strategy: manual resource was Applied %d time(s)", tag, got)
	}
	if got := r.runs(); got != 0 {
		r.t.Fatalf("[%s] the provider's task runner ran %d time(s)", tag, got)
	}
	if c := r.stepCounts(); len(c) != 0 {
		r.t.Fatalf("[%s] the provider's phased steps ran: %v", tag, c)
	}
}

// openGate は 1 周目で Task が起き、承認ゲートで止まるまでを確かめる。
func (r *manualRig) openGate(tag string) task.Record {
	r.t.Helper()
	if res := r.cycle(); !contains(res.TasksLaunched, "db") {
		r.t.Fatalf("[%s] cycle 1: no Task launched: %+v\n%s", tag, res, r.errOut)
	}
	rec := r.waitRecord("the approval gate", func(rec task.Record, ok bool) bool {
		return ok && rec.Phase == task.PhaseAwaitingApproval
	})
	if rec.Gate != task.GateApproval || rec.Step != task.StepManual {
		r.t.Fatalf("[%s] gate: %+v", tag, rec)
	}
	for _, want := range []string{manualProcedure, manualURL, "size: 41 → 42"} {
		if !strings.Contains(rec.Notice, want) {
			r.t.Fatalf("[%s] the gate's notice %q lacks %q", tag, rec.Notice, want)
		}
	}
	return rec
}

// [AC-150-1]
func TestManualUpdate_NotifyApproveObserveClose(t *testing.T) {
	r := newManualRig(t)
	r.openGate("AC-150-1")
	if len(r.ev.manual("launched")) != 1 {
		t.Fatalf("[AC-150-1] the launch was not notified\n%s", r.errOut)
	}

	// 承認を待つ間、知らせ続ける（level の Event、§22.3: 続く間は 1 つの firing）。
	// 手順の文・URL・いまの版と望む版つき。
	for c := 0; c < 3; c++ {
		res := r.cycle()
		if !contains(res.TasksAwaitingApproval, "db") || len(res.TasksLaunched) != 0 {
			t.Fatalf("[AC-150-1] waiting cycle %d: %+v\n%s", c, res, r.errOut)
		}
	}
	evs := r.ev.manual("awaiting_approval")
	if len(evs) == 0 {
		t.Fatalf("[AC-150-1] no awaiting_approval Event\n%s", r.errOut)
	}
	ev := evs[0]
	if ev.Severity != core.SeverityWarning || ev.Attrs["procedure"] != manualProcedure || ev.Attrs["procedure_url"] != manualURL ||
		ev.Attrs["current"] != "size=41" || ev.Attrs["desired"] != "size=42" || !strings.Contains(ev.Message, manualProcedure) {
		t.Fatalf("[AC-150-1] awaiting_approval Event: %+v", ev)
	}
	r.nothingWritten("AC-150-1 before approval")

	// 人が「作業した」と承認する。版はまだ変わっていない: 観測で待つ。
	r.approve()
	if res := r.cycleOn(r.d); !contains(res.TasksResumed, "db") {
		t.Fatalf("[AC-150-1] after approval: not resumed: %+v\n%s", res, r.errOut)
	}
	rec := r.waitRecord("confirming", func(rec task.Record, ok bool) bool {
		return ok && rec.Phase == task.PhaseRunning && rec.Awaiting && rec.ApprovedStep == task.StepManual
	})
	if rec.ApprovedBy != "alice" {
		t.Fatalf("[AC-150-1] approved record: %+v", rec)
	}
	if res := r.cycleOn(r.d); len(res.TasksCompleted) != 0 || len(r.ev.manual("confirming")) == 0 {
		t.Fatalf("[AC-150-1] confirming must be reported and not closed: %+v\n%s", res, r.errOut)
	}

	// 人の作業が観測に現れる → 閉じる。
	r.operatorSets(42)
	r.waitPhase(task.PhaseSucceeded)
	r.d.Wait(5 * time.Second)
	if res := r.cycle(); !contains(res.TasksCompleted, "db") {
		t.Fatalf("[AC-150-1] the version is observed but the Task is not closed: %+v\n%s", res, r.errOut)
	}
	if _, ok := r.record(); ok {
		t.Fatalf("[AC-150-1] S2 record must be gone once closed")
	}
	r.nothingWritten("AC-150-1 closed")
	for c := 0; c < 2; c++ {
		if res := r.cycle(); len(res.TasksLaunched) != 0 || len(res.TasksHeld) != 0 {
			t.Fatalf("[AC-150-1] converged resource got a Task decision: %+v", res)
		}
	}

	// 閉じたあとで版が戻った（人が戻した）: DriftEvent で知らせ、また人に頼む。
	drifts := r.ev.drifts("db")
	r.operatorSets(41)
	if res := r.cycle(); !contains(res.TasksLaunched, "db") {
		t.Fatalf("[AC-150-1] drift back: no Task launched: %+v\n%s", res, r.errOut)
	}
	if r.ev.drifts("db") <= drifts {
		t.Fatalf("[AC-150-1] drift back: no DriftEvent\n%s", r.errOut)
	}
	r.waitPhase(task.PhaseAwaitingApproval)
	r.nothingWritten("AC-150-1 drift back")
}

// [AC-150-2]
func TestManualUpdate_ApprovedButNotObservedIsStuck(t *testing.T) {
	r := newManualRig(t)
	r.openGate("AC-150-2")
	r.approve()
	if res := r.cycleOn(r.d); !contains(res.TasksResumed, "db") {
		t.Fatalf("[AC-150-2] after approval: not resumed: %+v\n%s", res, r.errOut)
	}
	r.waitRecord("confirming", func(rec task.Record, ok bool) bool { return ok && rec.Awaiting })

	// confirm_timeout（1h）を過ぎても版は 41 のまま。
	r.clock.Set(r.clock.Now().Add(2 * time.Hour))
	r.waitPhase(task.PhaseFailed)
	r.d.Wait(5 * time.Second)
	rec, _ := r.record()
	if !strings.Contains(rec.Error, "stuck:") || !strings.Contains(rec.Error, "confirm_timeout 1h0m0s") {
		t.Fatalf("[AC-150-2] stuck record: %+v", rec)
	}
	for c := 0; c < 3; c++ {
		res := r.cycle()
		if len(res.TasksCompleted) != 0 || len(res.TasksLaunched) != 0 || !contains(res.TasksHeld, "db") {
			t.Fatalf("[AC-150-2] stuck cycle %d: must neither close nor relaunch: %+v\n%s", c, res, r.errOut)
		}
	}
	if rec, ok := r.record(); !ok || rec.Phase != task.PhaseFailed {
		t.Fatalf("[AC-150-2] stuck Task was closed: %+v (present=%v)", rec, ok)
	}
	stuck := r.ev.manual("stuck")
	if len(stuck) == 0 || stuck[0].Severity != core.SeverityCritical || stuck[0].Attrs["current"] != "size=41" {
		t.Fatalf("[AC-150-2] stuck Events: %+v\n%s", stuck, r.errOut)
	}
	r.nothingWritten("AC-150-2 stuck")

	// 後で観測の版が合えば閉じる。
	r.operatorSets(42)
	if res := r.cycle(); !contains(res.TasksCompleted, "db") {
		t.Fatalf("[AC-150-2] observed as declared later: not closed: %+v\n%s", res, r.errOut)
	}
	if _, ok := r.record(); ok {
		t.Fatalf("[AC-150-2] S2 record must be gone once observed")
	}
}

// [AC-150-3]
func TestManualUpdate_ObservedWithoutApprovalCloses(t *testing.T) {
	r := newManualRig(t)
	r.openGate("AC-150-3")
	r.cycle()
	r.operatorSets(42)
	if res := r.cycle(); !contains(res.TasksCompleted, "db") {
		t.Fatalf("[AC-150-3] observed as declared without approval: not closed: %+v\n%s", res, r.errOut)
	}
	if _, ok := r.record(); ok {
		t.Fatalf("[AC-150-3] S2 record must be gone")
	}
	if _, err := os.Stat(filepath.Join(r.root, "audit.log")); !os.IsNotExist(err) {
		t.Fatalf("[AC-150-3] nobody approved, but there is an audit log (%v)", err)
	}
	r.nothingWritten("AC-150-3")
	if res := r.cycle(); len(res.TasksLaunched) != 0 || len(res.TasksHeld) != 0 {
		t.Fatalf("[AC-150-3] converged resource got a Task decision: %+v", res)
	}
}

// [AC-150-4]
func TestManualUpdate_LoaderRefusesMistakes(t *testing.T) {
	for name, policy := range map[string]string{
		"no procedure":            `{ "strategy": "manual" }`,
		"procedure on task":       `{ "strategy": "task", "procedure": "x" }`,
		"procedure on apply":      `{ "procedure_url": "https://x.example" }`,
		"rollback_on_fail":        `{ "strategy": "manual", "procedure": "x", "rollback_on_fail": true }`,
		"bad confirm_timeout":     `{ "strategy": "manual", "procedure": "x", "confirm_timeout": "soon" }`,
		"zero confirm_timeout":    `{ "strategy": "manual", "procedure": "x", "confirm_timeout": "0s" }`,
		"confirm_timeout on task": `{ "strategy": "task", "confirm_timeout": "1h" }`,
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "intent.json"), []byte(taskIntent("42", policy)), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := loader.LoadIntent(dir); err == nil {
			t.Errorf("[AC-150-4] %s: loaded, want refused", name)
		}
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "intent.json"), []byte(taskIntent("42", manualPolicy)), 0o644); err != nil {
		t.Fatal(err)
	}
	in, err := loader.LoadIntent(dir)
	if err != nil {
		t.Fatalf("[AC-150-4] valid manual policy refused: %v", err)
	}
	for _, res := range in.Resources {
		if res.ID.Local != "db" {
			continue
		}
		pol := res.UpdatePolicy
		if !pol.IsManual() || !pol.IsTask() || pol.Procedure != manualProcedure || pol.ProcedureURL != manualURL || pol.ConfirmTimeout != time.Hour {
			t.Fatalf("[AC-150-4] parsed policy: %+v", pol)
		}
	}
}
