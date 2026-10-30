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

package executor

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/tjst-t/sakutei/v2/internal/audit"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// Audited は Inner を通した Apply を、Op ごとに S4 監査へ 1 件ずつ記録する（#187、
// §26.3「Apply 実行記録は追記専用ストアへ書く」）。記録は Apply のあと（結果を
// 書くため）。記録できなかったときは ErrOut に知らせるが、Apply の結果は変えない
// （もう起きた変更を失敗と偽ると、同じ変更を繰り返すだけになる）。
type Audited struct {
	Inner  Executor
	Log    *audit.Log
	ErrOut io.Writer
}

var _ Executor = (*Audited)(nil)

type ctxKey int

const (
	pruneKey ctxKey = iota
	approvalKey
)

// WithPrune は ctx の Execute が prune（reality − desired の Delete）であることを示す。
func WithPrune(ctx context.Context) context.Context {
	return context.WithValue(ctx, pruneKey, true)
}

// Approval は Op を通した承認：planned_change の宣言の Key と、その承認者（空は
// 承認が要らない・分からない）。
type Approval func(op core.Op) (plannedChange, approvers string)

// WithApproval は ctx の Execute の Op を誰が承認したかを引く口を付ける。
func WithApproval(ctx context.Context, fn Approval) context.Context {
	return context.WithValue(ctx, approvalKey, fn)
}

// Execute は Inner で Apply し、Op ごとに記録する。
func (a *Audited) Execute(ctx context.Context, p provider.Provider, plan core.Plan) error {
	err := a.Inner.Execute(ctx, p, plan)
	if a.Log == nil {
		return err
	}
	kind := audit.KindApply
	if v, _ := ctx.Value(pruneKey).(bool); v {
		kind = audit.KindPrune
	}
	approval, _ := ctx.Value(approvalKey).(Approval)
	for _, op := range plan.Ops {
		rec := audit.Record{
			Kind:     kind,
			Resource: op.Target.String(),
			Op:       strings.ToLower(op.Kind.String()),
			Cause:    string(op.Cause),
			Result:   audit.ResultOK,
		}
		for _, f := range op.Fields {
			rec.Fields = append(rec.Fields, f.Field)
		}
		// #199: a commit group's Apply has a result per member.
		if merr := provider.MemberError(err, op.Target); merr != nil {
			rec.Result, rec.Error = audit.ResultFailed, merr.Error()
		}
		if approval != nil {
			rec.PlannedChange, rec.Actor = approval(op)
		}
		if aerr := a.Log.Append(rec); aerr != nil && a.ErrOut != nil {
			fmt.Fprintf(a.ErrOut, "sakutei: error: the %s of %s cannot be recorded in the S4 audit log: %v\n", kind, op.Target, aerr)
		}
	}
	return err
}
