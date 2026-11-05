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

package core

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Procedure は一回きりの作業（設計 §3.7）。非冪等な操作・複数 step の作業・人の作業を、
// cycle をまたいで 1 step ずつ進める。進みは Record に置き、「済んだ」は観測で決める。
//
// 式（Precondition・DoneWhen・Invariant・step の When・SkipIf）は書いたままの文字列で
// 持ち、{target} を対象の名前に置き換えてから Condition として読む（E-A5・N-J3）。
type Procedure struct {
	Name    string
	Target  ProcTarget // E-A5・N-J3
	Trigger Trigger    // E-F2・E-A6・N-F2・E-G8・N-F4・N-K8・N-D4
	// Precondition は始める前の条件（E-A8）。
	Precondition string
	// DoneWhen は済んだかを観測で決める。Record を失っても二度走らない
	// （不変条件 18、E-A9・E-A6・E-F2）。
	DoneWhen string
	// Invariant は step を進める前に毎回 check する集合の条件（N-B16・N-F4・N-B18・N-J3）。
	Invariant   string
	Steps       []Step
	Concurrency int           // 同時に進める対象の数。既定 1（N-J3・N-E2・N-A4）
	Timeout     time.Duration // Procedure 全体の上限。過ぎたら halt する（E-A6・E-R2）。0 は無し
	Origin      Origin
}

// ProcTarget は Procedure の対象の選び方。Type は必ず書き、ID か Labels で絞る。
type ProcTarget struct {
	Type   string
	ID     string            // 名前 1 つ（ID.Name）
	Labels map[string]string // 宣言した Resource のラベルで選ぶ
}

// TriggerKind は Procedure を起こすきっかけ。
type TriggerKind int

const (
	// TriggerOnDrift は Fields の field がずれたら対象ごとに 1 回起こす（E-F2・E-A6・
	// N-F2・E-G8）。その field は収束ループが Apply しない（E-F2・E-A5・N-A4）。
	TriggerOnDrift TriggerKind = iota
	// TriggerManual は人が sakutei start で起こす（N-F4・N-K8・N-D4）。
	TriggerManual
)

func (k TriggerKind) String() string {
	if k == TriggerManual {
		return "manual"
	}
	return "on_drift"
}

// Trigger は Procedure を起こすきっかけ。
type Trigger struct {
	Kind   TriggerKind
	Fields []string // on_drift の drift_fields
}

// StepKind は step の kind（§3.7）。
type StepKind int

const (
	StepApply  StepKind = iota // Resource を Apply して進む（E-A3・E-A8・N-B10）
	StepInvoke                 // 一回きりの操作（E-A2・N-A2・N-A11・N-D4・N-E2・N-D9）
	StepWait                   // 条件か時間を待つ（E-A8・N-B11・N-F6・N-G5・N-R4）
	StepManual                 // 人の作業。観測で閉じる（E-A6・E-Q3・N-A5・N-Q4・N-B19）
	StepGate                   // 承認を待つ（E-A5・E-L2・E-J8）
	StepPolicy                 // Procedure の間だけ効く一時的な Policy（E-Q3・N-D4）
)

var stepKindNames = map[StepKind]string{StepApply: "apply", StepInvoke: "invoke", StepWait: "wait",
	StepManual: "manual", StepGate: "gate", StepPolicy: "policy"}

func (k StepKind) String() string { return stepKindNames[k] }

// Step は Procedure の step 1 つ。
type Step struct {
	Name string
	Kind StepKind
	// Apply は Apply する Resource（StepApply）。ID.Name と文字列の field の {target}・
	// {desired.<field>} は対象ごとに置き換える。
	Apply *Resource
	// Op・Args は送る操作（StepInvoke）。Args の文字列の {target}・{desired.<field>} は
	// 置き換える。
	Op   string
	Args Fields
	// Instruction・URL は人への頼み（StepManual）。
	Instruction, URL string
	// When は待つ条件、For は待つ時間（StepWait）。
	When string
	For  time.Duration
	// Timeout は wait・gate・manual の step の上限。過ぎたら Procedure を halt する（E-A6）。
	Timeout time.Duration
	// SkipIf が真なら済んでいる step として飛ばす（再開のとき、E-A5）。
	SkipIf string
	// Critical は書き込み中に止まると壊れる step。止まったら自動で再開せず承認に
	// 回す。one-shot 実行では進めない（不変条件 16、E-A5・E-K5・N-K5）。
	Critical bool
	Gate     *Approval // StepGate
	Policy   *Policy   // StepPolicy
}

// Substitute は s の {target} を対象の名前に、{desired.<field>} を対象の宣言の値に
// 置き換える（E-A5・N-J3）。
func Substitute(s, target string, desired Fields) string {
	s = strings.ReplaceAll(s, "{target}", target)
	for {
		i := strings.Index(s, "{desired.")
		if i < 0 {
			return s
		}
		j := strings.Index(s[i:], "}")
		if j < 0 {
			return s
		}
		key := s[i+len("{desired.") : i+j]
		v := ""
		if x, ok := desired[key]; ok {
			v = describeScalar(x)
		}
		s = s[:i] + v + s[i+j+1:]
	}
}

func describeScalar(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return describe(v)
}

// OpSpec は一回きりの操作の性質。
type OpSpec struct {
	// Destructive は壊す操作（budget の壊す変更に数える、E-C1・不変条件 9）。
	Destructive bool
}

// ErrNotSent は Invoke が相手に何も送れていないこと。コアは送信中の記録を消して
// 次の cycle に送り直してよい（不変条件 16、E-A2）。ほかのエラーは「送れたか分からない」
// で、二度送らない。
var ErrNotSent = errors.New("送れていない")

// Invoker は観測に残らない一回きりの操作（boot_once・reset・migrate・drain・
// upgrade）を送る任意のインターフェース。Procedure からだけ呼ぶ（不変条件 15、E-A2・E-B11・
// N-D4・N-E2・N-A11）。
type Invoker interface {
	// Ops は kind の操作の一覧。
	Ops(kind string) map[string]OpSpec
	// Invoke は操作を送る。送れていなければ ErrNotSent を（包んで）返す。
	Invoke(ctx context.Context, w WriteContext, id ID, op string, args Fields) error
}
