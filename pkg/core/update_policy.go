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

import "time"

// UpdateStrategy は差分を検知したときに収束が取る手（§6.2, §6.5）。
type UpdateStrategy string

const (
	// UpdateStrategyApply は既定。差分を冪等な Apply で直す。
	UpdateStrategyApply UpdateStrategy = ""
	// UpdateStrategyTask は差分を検知しても直接 Apply せず Task を起動するだけ
	// （RequiresTaskEvent、§6.5）。非冪等な操作（migration・firmware 書込など）を
	// 毎周 Apply する収束ループから排除する。
	UpdateStrategyTask UpdateStrategy = "task"
	// UpdateStrategyManual は管理 API に更新の口が無い装置（GUI だけで更新する
	// もの）を人の手順で合わせる（§6.2「人の手順で合わせる Task」、Backlog #150）。
	// strategy: task と同じく Apply せず Task を起こすが、Task は provider の
	// 実行体を呼ばない: 承認ゲートで人に作業を促し、承認の後は版が宣言どおりに
	// なるのを観測で待って閉じる。
	UpdateStrategyManual UpdateStrategy = "manual"
)

// UpdatePolicy は Resource の update_policy（§6.2）。
//
//	update_policy:
//	  strategy: task
//	  window: "Sat 02:00-04:00"
//	  rollback_on_fail: true
type UpdatePolicy struct {
	Strategy UpdateStrategy
	// Window はメンテ窓。窓の外では Task を起動しない。空なら常時。
	// 書式は internal/task.ParseWindow。
	Window string
	// RollbackOnFail は Task が失敗したら rollback 手順を走らせる。
	RollbackOnFail bool

	// 以下は strategy: manual だけが使う（#150）。
	//
	//	update_policy:
	//	  strategy: manual
	//	  procedure: "管理画面の System > Firmware から版のイメージを上げる"
	//	  procedure_url: "https://wiki.example/switch-fw"
	//	  confirm_timeout: 2h
	//
	// Procedure は人がする手順の説明、ProcedureURL はその手順書の在りか。
	// 少なくともどちらかを宣言する。承認ゲート・Event にそのまま出す。
	Procedure    string
	ProcedureURL string
	// ConfirmTimeout は人が「作業した」と承認してから、観測で宣言どおりに
	// なるのを待つ上限。過ぎたら Task は stuck（halt）として知らせ、勝手に
	// 閉じない。0 なら task.DefaultConfirmTimeout。
	ConfirmTimeout time.Duration
}

// IsTask は差分を Apply せず Task に回すか（strategy: task か strategy: manual）。
func (p UpdatePolicy) IsTask() bool {
	return p.Strategy == UpdateStrategyTask || p.Strategy == UpdateStrategyManual
}

// IsManual は strategy: manual（人の手順で合わせる、#150）か。
func (p UpdatePolicy) IsManual() bool { return p.Strategy == UpdateStrategyManual }
