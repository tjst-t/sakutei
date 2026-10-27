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
	"errors"
	"fmt"
	"time"

	"github.com/tjst-t/sakutei/pkg/core"
)

// 多相 Task（§6.2 多相型、Backlog #51）。firmware 更新のような、中断できず・長く・
// リブートを跨ぐ手順を相（step）の並びとして走らせる。
//
//	Idle → Staging → Verifying → Applying → Rebooting → Confirming → Done
//	                                                  ↘ Failed → Rollback
//
// Idle は記録が無いこと、Done は完了印（稼働バージョンなど）を収束ループが
// Observe して記録を消すこと（単純 Task と同じ Settle）。間の相の並びは Task の
// 種類ごとに実行体（PhasedRunner.TaskSteps）が宣言する。
//
// 規則:
//   - S2 に置くのは進行中の相（Record.Step）とその入口の時刻・lease だけ。相が
//     済んだかは持たない。
//   - 各相の入口で CheckStep（現実の Observe）に「もう済んでいるか」を問い、
//     済んでいれば RunStep を呼ばずに次へ進む（べき等な再開。二重実行しない）。
//   - 対象が見えない（StepUnknown）ときは unknown ≠ absent: 済んでいないとも
//     消えたとも取らず、相のタイムアウトまで再出現を待つ。Await の相（Rebooting）は
//     RunStep の後も Done が見えるまで待つ。タイムアウトは StepStartedAt から数える
//     （起こし直しても延びない）。過ぎたら Failed（halt）。
//   - 1-outstanding・窓・失敗で halt は単純 Task と同じ。窓が問うのは起動だけで、
//     進行中の Task は窓の外でも続ける（リブート中の機器を置き去りにしない）。
//   - 引き継ぎ: デーモンが安全な区切り（相の間・待ちの間）で止まれば記録を
//     Released にし、次のデーモンがすぐ続きを引き継ぐ。落ちた（kill -9）なら
//     lease（HeartbeatAt）が LeaseTTL を過ぎてから引き継ぐ。どちらも引き継ぎは
//     Update の比較と書込で1つに決まる。desired が起動時から変わっていれば
//     引き継がない（人が確かめて cancel する）。
//
//   - 危険区間（Backlog #52、gate.go）: StepSpec.Critical の相（Applying）は
//     RunStep の前に記録へ印（Record.Critical）を立て、返った後に下ろす。印が
//     立ったまま持ち主が消えた（落ちた）記録は、相の入口の Observe で Done と
//     確かめられない限り自動で再開せず、人手ゲート（PhaseAwaitingApproval）に
//     する。Critical でない相は、RunStep の途中で落ちても入口の CheckStep が
//     「済んでいない」と言えばもう一度 RunStep を呼ぶ（べき等な相に限って使う）。

// StepState は CheckStep が見た、相の現実。
type StepState int

const (
	// StepPending はまだ済んでいない（対象は見えている）。
	StepPending StepState = iota
	// StepDone はもう済んでいる。
	StepDone
	// StepUnknown は対象が見えない（リブート中・到達不能）。absent ではない。
	StepUnknown
)

func (s StepState) String() string {
	switch s {
	case StepPending:
		return "pending"
	case StepDone:
		return "done"
	case StepUnknown:
		return "unknown"
	}
	return "invalid"
}

// 代表的な相の名前（firmware 更新、§6.2）。
const (
	StepStaging    = "staging"    // 転送
	StepVerifying  = "verifying"  // hash / 署名
	StepApplying   = "applying"   // 書込
	StepRebooting  = "rebooting"  // 再起動して再出現を待つ
	StepConfirming = "confirming" // 新しい版を確かめる
)

// DefaultStepTimeout は StepSpec.Timeout が 0 の相の上限。
const DefaultStepTimeout = 30 * time.Minute

// StepSpec は多相 Task の相の宣言。
type StepSpec struct {
	Name string
	// Action は、入口で済んでいなければ RunStep を呼ぶ相か。false の相
	// （Confirming）は確かめるだけで、済んでいなければ Task を失敗にする。
	Action bool
	// Await は RunStep の後、CheckStep が StepDone になるまで待つ相か（Rebooting）。
	// 待つ間の StepUnknown・StepPending はどちらも待ち続ける。
	Await bool
	// Timeout は相の入口から、対象の再出現（と Await の完了）を待つ上限。
	// 0 なら DefaultStepTimeout。
	Timeout time.Duration
	// Critical は RunStep の途中で止まると「書込の途中」と「済んだ」を区別できない
	// 相（Applying、#52）。RunStep の間は S2 に印を立て、その中で落ちた Task は
	// 人手ゲートにする（gate.go）。
	Critical bool
}

func (s StepSpec) timeout() time.Duration {
	if s.Timeout <= 0 {
		return DefaultStepTimeout
	}
	return s.Timeout
}

// FirmwareSteps は firmware 更新の相の並び（§6.2 の図）。reboot は Rebooting と
// Confirming の上限（0 なら DefaultStepTimeout）。
func FirmwareSteps(reboot time.Duration) []StepSpec {
	return []StepSpec{
		{Name: StepStaging, Action: true},
		{Name: StepVerifying, Action: true},
		{Name: StepApplying, Action: true, Critical: true},
		{Name: StepRebooting, Action: true, Await: true, Timeout: reboot},
		{Name: StepConfirming, Timeout: reboot},
	}
}

// PhasedRunner は多相 Task も走らせる実行体。TaskSteps が相を返した Task は
// RunTask ではなく相ごとに走らせる。nil を返した Task は単純 Task のまま。
type PhasedRunner interface {
	Runner
	// TaskSteps は req の Task の相の並び（Task の種類ごとの宣言）。
	TaskSteps(ctx context.Context, req Request) ([]StepSpec, error)
	// CheckStep は相 step がもう済んでいるかを現実に問う（Observe。書かない）。
	// エラーは見えなかったものとして StepUnknown と同じに扱う。
	CheckStep(ctx context.Context, req Request, step string) (StepState, error)
	// RunStep は相 step を1回実行する。
	RunStep(ctx context.Context, req Request, step string) error
}

const (
	// DefaultPollInterval は CheckStep で待つ間隔。
	DefaultPollInterval = 5 * time.Second
	// DefaultLeaseTTL は HeartbeatAt がこれより古い記録の Holder を落ちたものとみなす。
	DefaultLeaseTTL = 2 * time.Minute
)

// errStopped は Stop で安全な区切りに止まった。
var errStopped = errors.New("task manager stopped")

// Resumed は Trigger / Resume の結果: 前のデーモンの多相 Task を途中の相から引き継いだ。
const Resumed Outcome = OutsideWindow + 1

func (m *Manager) pollInterval() time.Duration {
	if m.PollInterval > 0 {
		return m.PollInterval
	}
	return DefaultPollInterval
}

func (m *Manager) leaseTTL() time.Duration {
	if m.LeaseTTL > 0 {
		return m.LeaseTTL
	}
	return DefaultLeaseTTL
}

// Stop は走っている多相 Task を次の安全な区切り（RunStep の前後・待ちの間）で
// 止め、記録を Released にして手放す。RunStep の途中は切らない。単純 Task は
// 止めない（終わるまで走る）。止めた後の Manager は新しい Task を起動しない。
func (m *Manager) Stop() {
	m.stopOnce.Do(func() { close(m.stop) })
}

func (m *Manager) stopped() bool {
	select {
	case <-m.stop:
		return true
	default:
		return false
	}
}

// phasedSteps は req が多相 Task ならその相を返す。
func (m *Manager) phasedSteps(ctx context.Context, req Request) (PhasedRunner, []StepSpec, error) {
	pr, ok := m.runner.(PhasedRunner)
	if !ok {
		return nil, nil, nil
	}
	steps, err := pr.TaskSteps(ctx, req)
	if err != nil {
		return nil, nil, fmt.Errorf("task steps of %s: %w", req.Resource, err)
	}
	if len(steps) == 0 {
		return nil, nil, nil
	}
	return pr, steps, nil
}

func stepIndex(steps []StepSpec, name string) int {
	for i, s := range steps {
		if s.Name == name {
			return i
		}
	}
	return -1
}

// Resume は対象が Observe できなかった（unknown）周に呼ぶ。前のデーモンが手放したか
// lease が切れた多相 Task の記録があれば、その相から引き継ぐ。新しい Task は起動
// しない（差分は分からない）。ok は引き継いだか、このプロセスで走っているか。
func (m *Manager) Resume(ctx context.Context, id core.ID, desired map[string]string, pol core.UpdatePolicy) (Decision, bool, error) {
	key := id.String()
	if m.isRunning(key) {
		return Decision{Outcome: Running, Reason: "task is running"}, true, nil
	}
	rec, ok, err := m.store.Get(ctx, key)
	if err != nil || !ok || rec.Step == "" {
		return Decision{}, false, err
	}
	switch rec.Phase {
	case PhaseRunning:
		return m.resume(ctx, id, desired, core.Plan{}, pol, rec)
	case PhaseAwaitingApproval:
		return m.gated(ctx, id, desired, core.Plan{}, pol, rec)
	}
	return Decision{}, false, nil
}

// resume は多相 Task の running の記録 rec を引き継げるなら引き継ぐ。
func (m *Manager) resume(ctx context.Context, id core.ID, desired map[string]string, plan core.Plan, pol core.UpdatePolicy, rec Record) (Decision, bool, error) {
	outstanding := func(reason string) (Decision, bool, error) {
		return Decision{Outcome: Outstanding, Record: rec, Reason: reason}, false, nil
	}
	if m.stopped() {
		return outstanding("task manager is stopping")
	}
	now := m.now()
	stale := now.Sub(rec.HeartbeatAt) > m.leaseTTL()
	if !rec.Released && !stale {
		return outstanding(fmt.Sprintf("task %s is in step %s, held by %q (heartbeat %s); resumed here once released or after the lease (%s) runs out",
			rec.TaskID, rec.Step, rec.Holder, rec.HeartbeatAt.UTC().Format(time.RFC3339), m.leaseTTL()))
	}
	if rec.DesiredHash != DesiredHash(desired) {
		return outstanding(fmt.Sprintf("task %s stopped in step %s, but the desired state changed since it was launched; not resuming — check reality and cancel it",
			rec.TaskID, rec.Step))
	}
	req := Request{TaskID: rec.TaskID, Resource: id, Desired: copyFields(desired), Plan: plan}
	pr, steps, err := m.phasedSteps(ctx, req)
	if err != nil {
		return Decision{}, false, err
	}
	idx := stepIndex(steps, rec.Step)
	if pr == nil || idx < 0 {
		return outstanding(fmt.Sprintf("task %s stopped in step %s, which the task runner no longer declares; not resuming — check reality and cancel it",
			rec.TaskID, rec.Step))
	}
	prev := rec
	sameAsPrev := func(r *Record) bool {
		return r.Phase == PhaseRunning && r.Step == prev.Step && r.Holder == prev.Holder &&
			r.Released == prev.Released && r.HeartbeatAt.Equal(prev.HeartbeatAt) && r.Critical == prev.Critical
	}
	if rec.Critical {
		// 危険区間の中で持ち主が消えた（#52）。書込が済んだと Observe で確かめられ
		// なければ、リブート中か壊れたかを区別できないので、人手ゲートにする。
		if st, _ := pr.CheckStep(ctx, req, rec.Step); st != StepDone {
			return m.openGate(ctx, id, rec, st, sameAsPrev)
		}
	}
	cur, took, err := m.store.Update(ctx, id.String(), rec.TaskID, func(r *Record) bool {
		if !sameAsPrev(r) {
			return false // 先に誰かが引き継いだ
		}
		// 危険区間の印は、相の完了を Observe で確かめた（上）ので下ろす。
		r.Holder, r.Released, r.HeartbeatAt, r.Critical = m.holder, false, now.UTC(), false
		return true
	})
	if err != nil {
		return Decision{}, false, err
	}
	if !took {
		rec = cur
		return outstanding(m.outstandingReason(cur))
	}
	why := "lease of " + fmt.Sprintf("%q", prev.Holder) + " ran out"
	if prev.Released {
		why = fmt.Sprintf("released by %q", prev.Holder)
	}
	m.start(id.String(), func() { m.runPhased(context.WithoutCancel(ctx), pr, req, steps, idx, cur, pol) })
	return Decision{Outcome: Resumed, Record: cur,
		Reason: fmt.Sprintf("task %s resumed at step %s (%s)", cur.TaskID, cur.Step, why)}, true, nil
}

// start は key の Task を非同期に走らせる。
func (m *Manager) start(key string, run func()) {
	m.mu.Lock()
	m.running[key] = true
	m.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() {
			m.mu.Lock()
			delete(m.running, key)
			m.mu.Unlock()
		}()
		run()
	}()
}

// mine は記録が今もこのデーモンの起動のものか（引き継がれていないか）。
func (m *Manager) mine(r *Record) bool {
	return r.Holder == m.holder && !r.Released && r.Phase == PhaseRunning
}

// runPhased は steps[idx:] を走らせる。rec は idx の相の記録（入口の時刻つき）。
func (m *Manager) runPhased(ctx context.Context, pr PhasedRunner, req Request, steps []StepSpec, idx int, rec Record, pol core.UpdatePolicy) {
	key := req.Resource.String()
	stopHB := m.heartbeat(ctx, key, req.TaskID)
	defer stopHB()

	write := func(fn func(r *Record)) bool {
		_, ok, err := m.store.Update(ctx, key, req.TaskID, func(r *Record) bool {
			if !m.mine(r) {
				return false
			}
			fn(r)
			return true
		})
		return ok && err == nil
	}
	release := func() {
		write(func(r *Record) { r.Released, r.HeartbeatAt = true, m.now().UTC() })
	}
	fail := func(step string, err error) {
		msg := fmt.Sprintf("step %s: %v", step, err)
		rolledBack := false
		if pol.RollbackOnFail {
			rolledBack = true
			rreq := req
			rreq.Step = step
			switch note, rbErr := pr.RollbackTask(ctx, rreq); {
			case rbErr != nil:
				msg += "; rollback failed: " + rbErr.Error()
			case note != "":
				msg += "; rolled back: " + note
			default:
				msg += "; rolled back"
			}
		}
		// 書けなければ running のまま残る（=中断扱い。安全側）。
		write(func(r *Record) { r.Phase, r.RolledBack, r.Error, r.Critical = PhaseFailed, rolledBack, msg, false })
	}

	for i := idx; i < len(steps); i++ {
		s := steps[i]
		started := rec.StepStartedAt
		if i != idx || rec.Step != s.Name {
			started = m.now().UTC()
			if !write(func(r *Record) { r.Step, r.StepStartedAt, r.HeartbeatAt = s.Name, started, started }) {
				return // cancel された・引き継がれた
			}
		}
		deadline := started.Add(s.timeout())

		// 入口: もう済んでいるか。見えなければ再出現を待つ。
		st, err := m.awaitStep(ctx, pr, req, s, deadline, func(st StepState) bool { return st != StepUnknown })
		if errors.Is(err, errStopped) {
			release()
			return
		}
		if err != nil {
			fail(s.Name, err)
			return
		}
		if st == StepDone {
			continue
		}
		if !s.Action {
			fail(s.Name, errors.New("not confirmed: reality does not show the step done"))
			return
		}
		if m.stopped() {
			release()
			return
		}
		if s.Critical && !write(func(r *Record) { r.Critical = true }) {
			return // 印を立てられなければ書込を始めない
		}
		if err := pr.RunStep(ctx, req, s.Name); err != nil {
			fail(s.Name, err)
			return
		}
		// 書けなければ印が立ったまま残る（引き継ぐデーモンは人手ゲートにする。安全側）。
		if s.Critical && !write(func(r *Record) { r.Critical = false }) {
			return
		}
		if !s.Await && m.stopped() {
			// 相の効果は済んだが、次の相へ進んだことはまだ書いていない。続きを
			// 引き継ぐデーモンは、この相の入口の Observe で済んだと分かって飛ばす。
			release()
			return
		}
		if s.Await {
			_, err := m.awaitStep(ctx, pr, req, s, deadline, func(st StepState) bool { return st == StepDone })
			if errors.Is(err, errStopped) {
				release()
				return
			}
			if err != nil {
				fail(s.Name, err)
				return
			}
		}
	}
	write(func(r *Record) { r.Phase, r.Step = PhaseSucceeded, "" })
}

// awaitStep は CheckStep を cond が真になるまで繰り返す。deadline（m.now で比べる）を
// 過ぎたらエラー。見えない間（StepUnknown・CheckStep のエラー）は absent と取らない。
func (m *Manager) awaitStep(ctx context.Context, pr PhasedRunner, req Request, s StepSpec, deadline time.Time, cond func(StepState) bool) (StepState, error) {
	var lastErr error
	for {
		st, err := pr.CheckStep(ctx, req, s.Name)
		if err != nil {
			st, lastErr = StepUnknown, err
		}
		if cond(st) {
			return st, nil
		}
		if !m.now().Before(deadline) {
			what := "the target was not observed again"
			if st != StepUnknown {
				what = "the step did not complete (" + st.String() + ")"
			}
			msg := fmt.Sprintf("%s within %s (unknown ≠ absent: waited, never treated as gone)", what, s.timeout())
			if lastErr != nil && st == StepUnknown {
				msg += "; last observe error: " + lastErr.Error()
			}
			return st, errors.New(msg)
		}
		select {
		case <-m.stop:
			return st, errStopped
		case <-time.After(m.pollInterval()):
		}
	}
}

// heartbeat は記録の lease を更新し続ける。返す関数で止める。
func (m *Manager) heartbeat(ctx context.Context, key, taskID string) func() {
	every := m.leaseTTL() / 3
	if every < 10*time.Millisecond {
		every = 10 * time.Millisecond
	}
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				_, _, _ = m.store.Update(ctx, key, taskID, func(r *Record) bool {
					if !m.mine(r) {
						return false
					}
					r.HeartbeatAt = m.now().UTC()
					return true
				})
			}
		}
	}()
	return func() {
		close(done)
		<-exited
	}
}
