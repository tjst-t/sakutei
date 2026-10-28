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
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// 人の手順で合わせる Task（§6.2、Backlog #150）。管理 API に更新の口が無い装置
// （GUI だけで FW を上げるスイッチなど）の版を、update_policy.strategy: manual で
// 宣言する。Sakutei は版を観測し、人に作業を促し、終わったことを観測で確かめる。
// 書くのは人だけで、Sakutei は装置に何も送らない（provider の実行体も呼ばない）。
//
// 相は 1 つ（StepManual）。多相 Task の決まり（phased.go・gate.go）にそのまま乗る:
//
//   - 入口で観測する。宣言どおりなら何もせずに閉じる。
//   - 違えば承認ゲート（Approval）を開き、手順の文・URL・いまの版と望む版
//     （Notice）を記録と Event に出して、人を待つ。
//   - 待つ間に観測で宣言どおりになれば、承認なしで閉じる（人が答えなくてよい）。
//   - 人が「作業した」と承認したら（sakutei task approve）、RunStep は何もせず
//     （Await）、観測で宣言どおりになるのを ConfirmTimeout まで待つ。なれば閉じる。
//     ならなければ stuck として halt する（勝手に閉じない。後で観測が合えば
//     収束ループの Settle が閉じる）。
//   - 却下・ゲートのタイムアウトは halt（ほかの承認の要る相と同じ）。

// StepManual は人の手順で合わせる Task の相の名前。
const StepManual = "manual"

// DefaultConfirmTimeout は update_policy.confirm_timeout が無いときの、承認の後に
// 観測で宣言どおりになるのを待つ上限。
const DefaultConfirmTimeout = time.Hour

// ManualObserver は strategy: manual のリソースを観測し、宣言どおりか（StepDone）・
// 違うか（StepPending）・見えないか（StepUnknown）を返す。書かない。
type ManualObserver func(ctx context.Context, req Request) (StepState, error)

// ManualSteps は人の手順で合わせる Task の相の並び。
func ManualSteps(req Request) []StepSpec {
	timeout := req.Policy.ConfirmTimeout
	if timeout <= 0 {
		timeout = DefaultConfirmTimeout
	}
	return []StepSpec{{
		Name: StepManual, Action: true, Approval: true, Await: true, Timeout: timeout,
		Notice: ManualNotice(req),
		Stuck: fmt.Sprintf("stuck: the operator approved the manual update, but reality is not observed as declared within confirm_timeout %s; not closing it (it closes once observed as declared, or sakutei task cancel)",
			timeout),
	}}
}

// ManualNotice は人への案内: 手順の文・URL と、差分（いまの値 → 望む値）。
// 差分は起動を決めた Plan から取る（引き継いだときは Plan が空で、望む値だけ）。
func ManualNotice(req Request) string {
	var parts []string
	if p := req.Policy.Procedure; p != "" {
		parts = append(parts, "procedure: "+p)
	}
	if u := req.Policy.ProcedureURL; u != "" {
		parts = append(parts, "see "+u)
	}
	if d := ManualDiff(req.Plan); d != "" {
		parts = append(parts, d)
	}
	return "manual update — " + strings.Join(parts, "; ")
}

// ManualDiff は Plan の差分を「field: いま → 望む」の並びで返す。
func ManualDiff(plan core.Plan) string {
	var out []string
	for _, op := range plan.Ops {
		for _, f := range op.Fields {
			from := f.From
			if from == "" {
				from = "(not observed)"
			}
			out = append(out, fmt.Sprintf("%s: %s → %s", f.Field, from, f.To))
		}
	}
	return strings.Join(out, ", ")
}

// WithManual は inner（provider の実行体。nil でよい）に、strategy: manual の
// Task を observe で観測して人の手順で合わせる相を足した実行体を返す。manual の
// Task は inner を一度も呼ばない。
func WithManual(inner Runner, observe ManualObserver) PhasedRunner {
	return manualRunner{inner: inner, observe: observe}
}

type manualRunner struct {
	inner   Runner
	observe ManualObserver
}

var errNoRunner = errors.New("no task runner for this resource")

func (r manualRunner) RunTask(ctx context.Context, req Request) error {
	if req.Policy.IsManual() {
		// 相を宣言するので RunTask は呼ばれない。呼ばれても何も書かない。
		return errors.New("a manual update is a phased task; nothing to run")
	}
	if r.inner == nil {
		return errNoRunner
	}
	return r.inner.RunTask(ctx, req)
}

func (r manualRunner) RollbackTask(ctx context.Context, req Request) (string, error) {
	if req.Policy.IsManual() || r.inner == nil {
		return "", errNoRunner
	}
	return r.inner.RollbackTask(ctx, req)
}

func (r manualRunner) TaskSteps(ctx context.Context, req Request) ([]StepSpec, error) {
	if req.Policy.IsManual() {
		return ManualSteps(req), nil
	}
	if pr, ok := r.inner.(PhasedRunner); ok {
		return pr.TaskSteps(ctx, req)
	}
	return nil, nil
}

func (r manualRunner) CheckStep(ctx context.Context, req Request, step string) (StepState, error) {
	if req.Policy.IsManual() {
		if step != StepManual {
			return StepUnknown, fmt.Errorf("manual update has no step %q", step)
		}
		return r.observe(ctx, req)
	}
	pr, ok := r.inner.(PhasedRunner)
	if !ok {
		return StepUnknown, errNoRunner
	}
	return pr.CheckStep(ctx, req, step)
}

func (r manualRunner) RunStep(ctx context.Context, req Request, step string) error {
	if req.Policy.IsManual() {
		// 人が作業した（承認）。Sakutei は何も送らず、観測で確かめるだけ（Await）。
		return nil
	}
	pr, ok := r.inner.(PhasedRunner)
	if !ok {
		return errNoRunner
	}
	return pr.RunStep(ctx, req, step)
}
