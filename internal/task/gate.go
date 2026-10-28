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

	"github.com/tjst-t/sakutei/v2/internal/audit"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// 多相 Task の危険区間と人手ゲート（§29.1、Backlog #52）と、承認の要る相の
// 入口のゲート（§6.4、Backlog #148）。
//
// 相の入口の Observe（phased.go）は相の境界で止まった Task を守るが、書込の途中
// （Applying の RunStep の中）で持ち主が消えた Task は、引き継いだデーモンから見て
// 「リブート中」と「壊れた（bricked）」の区別がつかない。そこで:
//
//   - StepSpec.Critical の相の RunStep の間、記録に印（Record.Critical）を立てる。
//     デーモンの停止（Stop）は RunStep を切らないので、印が立ったまま残るのは
//     持ち主が落ちた（lease 切れ）か、印を下ろす書込に失敗したときだけ。
//   - 印の立った記録を引き継ぐとき、その相の CheckStep を 1 回問う。StepDone なら
//     書込は済んでいるので印を下ろして普通に続ける（入口の Observe で飛ばす）。
//     StepPending・StepUnknown（見えない・Observe のエラー）なら自動で再開せず、
//     記録を PhaseAwaitingApproval にして人手ゲートを開く。rollback もしない
//     （壊れたかもしれない機器に自動で手を出さない）。
//   - ゲートの間も周ごとに同じ CheckStep を問い、StepDone が見えたら（リブートから
//     戻って書込が済んでいた）ゲートを閉じて続ける。Task 全体の完了印（稼働版）が
//     見えれば、収束ループの Settle が記録を消す（Done）。
//   - 人の承認（Decide、sakutei task approve）で、記録は危険区間の相の入口に戻って
//     手放された状態（Released）になり、次の周に引き継がれる。入口の Observe が
//     済んでいないと言えば RunStep をもう一度呼ぶ（書き直してよいと人が決めた）。
//     却下は PhaseFailed（halt、人が cancel するまで再起動しない）。
//   - ゲートの上限は GateTimeout（既定 24h、§6.4 のゲートの既定と同じ）。開いた
//     ときに GateDeadline として記録に置く。過ぎたら PhaseFailed（halt）。
//   - 承認・却下は S4 監査に append してから S2 に書く（同じロックの中）。コアは
//     認可を持たない（§7）: 承認者 ID・役割・署名は記録するだけ。要件は単一承認
//     （team の既定、§14.4・§14.5）。
//   - 承認の要る相（StepSpec.Approval、#148）: 入口の Observe で済んでいなければ
//     RunStep の前に同じゲート（Record.Gate が GateApproval）を開く。判定・承認・
//     却下・タイムアウトは上と同じで、承認はその相を RunStep してよいという判断
//     （Record.ApprovedStep）。待つ間に相が済んだと Observe で見えれば承認なしで
//     閉じる。

// AwaitingApproval は Trigger / Resume の結果: 危険区間の中で止まった多相 Task が
// 人手ゲートで承認を待っている。
const AwaitingApproval Outcome = Resumed + 1

// DefaultGateTimeout は Manager.GateTimeout が 0 のときの人手ゲートの上限。
const DefaultGateTimeout = 24 * time.Hour

func (m *Manager) gateTimeout() time.Duration {
	if m.GateTimeout > 0 {
		return m.GateTimeout
	}
	return DefaultGateTimeout
}

// openGate は危険区間の中で止まった記録 rec（same が今も同じ記録かを比べる）を
// 人手ゲートにする。st はその相の CheckStep が見たもの。
func (m *Manager) openGate(ctx context.Context, id core.ID, rec Record, st StepState, same func(*Record) bool) (Decision, bool, error) {
	now := m.now().UTC()
	why := fmt.Sprintf("lease of %q ran out", rec.Holder)
	if rec.Released {
		why = fmt.Sprintf("released by %q", rec.Holder)
	}
	cur, ok, err := m.store.Update(ctx, id.String(), rec.TaskID, func(r *Record) bool {
		if !same(r) {
			return false // 先に誰かが引き継いだ・ゲートにした
		}
		r.Phase, r.Gate, r.GateOpenedAt, r.GateDeadline = PhaseAwaitingApproval, "", now, now.Add(m.gateTimeout())
		r.Error = fmt.Sprintf("step %s: stopped inside the critical section (%s) and the step is not observed done (%s); cannot tell a reboot from a bricked device",
			rec.Step, why, st)
		return true
	})
	if err != nil {
		return Decision{}, false, err
	}
	if !ok {
		return Decision{Outcome: Outstanding, Record: cur, Reason: m.outstandingReason(cur)}, false, nil
	}
	return Decision{Outcome: AwaitingApproval, Record: cur, Reason: m.outstandingReason(cur)}, false, nil
}

// gated は人手ゲートで待つ記録 rec を見る: 上限を過ぎていれば halt、相の完了が
// Observe で見えればゲートを閉じて引き継ぐ。どちらでもなければ待ち続ける。
func (m *Manager) gated(ctx context.Context, id core.ID, desired map[string]string, plan core.Plan, pol core.UpdatePolicy, rec Record) (Decision, bool, error) {
	key := id.String()
	sameGate := func(r *Record) bool {
		return r.Phase == PhaseAwaitingApproval && r.GateOpenedAt.Equal(rec.GateOpenedAt)
	}
	outstanding := func(r Record) (Decision, bool, error) {
		return Decision{Outcome: Outstanding, Record: r, Reason: m.outstandingReason(r)}, false, nil
	}
	waiting := Decision{Outcome: AwaitingApproval, Record: rec, Reason: m.outstandingReason(rec)}
	now := m.now().UTC()
	if !now.Before(rec.GateDeadline) {
		cur, _, err := m.store.Update(ctx, key, rec.TaskID, func(r *Record) bool {
			if !sameGate(r) {
				return false
			}
			r.Phase, r.Gate = PhaseFailed, ""
			r.Error = fmt.Sprintf("%s; approval gate timed out after %s (opened %s)",
				r.Error, m.gateTimeout(), r.GateOpenedAt.UTC().Format(time.RFC3339))
			return true
		})
		if err != nil {
			return Decision{}, false, err
		}
		return outstanding(cur)
	}
	if m.stopped() || rec.DesiredHash != DesiredHash(desired) {
		return waiting, false, nil
	}
	req := Request{TaskID: rec.TaskID, Resource: id, Desired: copyFields(desired), Plan: plan, StartedAt: rec.StartedAt}
	pr, steps, err := m.phasedSteps(ctx, req)
	if err != nil {
		return Decision{}, false, err
	}
	if pr == nil || stepIndex(steps, rec.Step) < 0 {
		return waiting, false, nil
	}
	if st, _ := pr.CheckStep(ctx, req, rec.Step); st != StepDone {
		return waiting, false, nil
	}
	// 書込は済んでいた（例: リブートから戻った機器に新しい版が書かれている）。
	// 人の承認は要らない: ゲートを閉じ、手放された記録として引き継ぐ。
	cur, ok, err := m.store.Update(ctx, key, rec.TaskID, func(r *Record) bool {
		if !sameGate(r) {
			return false
		}
		reopen(r, now)
		return true
	})
	if err != nil {
		return Decision{}, false, err
	}
	if !ok {
		return outstanding(cur)
	}
	dec, resumed, err := m.resume(ctx, id, desired, plan, pol, cur)
	if resumed {
		dec.Reason = fmt.Sprintf("step %s observed done; approval gate closed without approval; %s", rec.Step, dec.Reason)
	}
	return dec, resumed, err
}

// reopen はゲートを閉じ、記録を危険区間の相の入口で手放された状態に戻す。
// 相のタイムアウトは今から数え直す。
func reopen(r *Record, now time.Time) {
	r.Phase, r.Released, r.Critical, r.Error, r.Gate, r.Awaiting = PhaseRunning, true, false, "", "", false
	r.StepStartedAt, r.HeartbeatAt = now, now
	r.GateOpenedAt, r.GateDeadline = time.Time{}, time.Time{}
}

// Approval は人手ゲートへの判断。
type Approval struct {
	// Approver は承認者 ID（必須）。
	Approver string
	// Roles は承認者が名乗る役割、Signature はその署名（記録するだけ、§7）。
	Roles     []string
	Signature string
	// Reject は却下（= halt）。
	Reject bool
}

// ErrNotAwaitingApproval は resource の記録が人手ゲートで待っていない。
var ErrNotAwaitingApproval = errors.New("no task awaiting approval")

// Decide は人手ゲートで待つ resource の多相 Task への判断（承認 / 却下）を受け付ける
// （sakutei task approve / reject）。判断は S4 監査 log に append してから S2 に書く
// （同じロックの中）。S4 に残せない判断は効かせない。承認した記録は危険区間の相の
// 入口で手放され、デーモンが次の周に引き継ぐ。
func Decide(ctx context.Context, store Store, log *audit.Log, resource string, a Approval, now time.Time) (Record, error) {
	if log == nil {
		return Record{}, errors.New("an S4 audit log is required to record approvals (--audit-log)")
	}
	if a.Approver == "" {
		return Record{}, errors.New("approver ID is required")
	}
	rec, ok, err := store.Get(ctx, resource)
	if err != nil {
		return Record{}, err
	}
	if !ok || rec.Phase != PhaseAwaitingApproval {
		if ok {
			return rec, fmt.Errorf("%w: task %s of %s is %s", ErrNotAwaitingApproval, rec.TaskID, resource, rec.Phase)
		}
		return Record{}, fmt.Errorf("%w: no outstanding task for %s", ErrNotAwaitingApproval, resource)
	}
	var decideErr error
	cur, applied, err := store.Update(ctx, resource, rec.TaskID, func(r *Record) bool {
		if r.Phase != PhaseAwaitingApproval || !r.GateOpenedAt.Equal(rec.GateOpenedAt) {
			decideErr = fmt.Errorf("%w: task %s of %s changed meanwhile (now %s)", ErrNotAwaitingApproval, r.TaskID, resource, r.Phase)
			return false
		}
		if !now.Before(r.GateDeadline) {
			decideErr = fmt.Errorf("approval gate of task %s timed out at %s (the daemon halts it)", r.TaskID, r.GateDeadline.UTC().Format(time.RFC3339))
			return false
		}
		kind, detail := audit.KindTaskGateApproved,
			fmt.Sprintf("task stopped inside the critical section of step %s approved; resuming at step %s", r.Step, r.Step)
		if a.Reject {
			kind, detail = audit.KindTaskGateRejected,
				fmt.Sprintf("task stopped inside the critical section of step %s rejected; halted", r.Step)
		}
		if r.Gate == GateApproval {
			detail = fmt.Sprintf("task approved before step %s; running step %s", r.Step, r.Step)
			if a.Reject {
				detail = fmt.Sprintf("task rejected before step %s; halted", r.Step)
			}
		}
		if decideErr = log.Append(audit.Record{
			Time: now, Kind: kind, Resource: resource, RunID: r.TaskID, Gate: r.Step,
			Actor: a.Approver, Roles: a.Roles, Signature: a.Signature, Detail: detail,
		}); decideErr != nil {
			return false
		}
		if a.Reject {
			r.Phase, r.Gate = PhaseFailed, ""
			r.Error = fmt.Sprintf("%s; rejected by %s", r.Error, a.Approver)
			r.GateOpenedAt, r.GateDeadline = time.Time{}, time.Time{}
			return true
		}
		approvalGate := r.Gate == GateApproval
		reopen(r, now.UTC())
		r.ApprovedBy = a.Approver
		if approvalGate {
			r.ApprovedStep = r.Step
		}
		return true
	})
	if err != nil {
		return Record{}, err
	}
	if decideErr != nil {
		return cur, decideErr
	}
	if !applied {
		return cur, fmt.Errorf("%w: task %s of %s was replaced", ErrNotAwaitingApproval, rec.TaskID, resource)
	}
	return cur, nil
}
