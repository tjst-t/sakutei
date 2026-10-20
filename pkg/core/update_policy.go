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

// UpdateStrategy は差分を検知したときに収束が取る手（§6.2, §6.5）。
type UpdateStrategy string

const (
	// UpdateStrategyApply は既定。差分を冪等な Apply で直す。
	UpdateStrategyApply UpdateStrategy = ""
	// UpdateStrategyTask は差分を検知しても直接 Apply せず Task を起動するだけ
	// （RequiresTaskEvent、§6.5）。非冪等な操作（migration・firmware 書込など）を
	// 毎周 Apply する収束ループから排除する。
	UpdateStrategyTask UpdateStrategy = "task"
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
}

// IsTask は strategy: task か。
func (p UpdatePolicy) IsTask() bool { return p.Strategy == UpdateStrategyTask }
