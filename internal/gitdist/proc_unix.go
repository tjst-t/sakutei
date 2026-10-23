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
//go:build unix

package gitdist

import (
	"os/exec"
	"syscall"
)

// killGroupOnCancel は git を自分のプロセスグループで動かし、context の取消でグループ
// ごと止める。git remote update・clone は子の git（fetch など）を起こすので、親だけを
// 止めると子が標準出力を握ったまま応答しない中央で待ち続け、呼び手も戻れない（試用
// docs/trial/dr-mode で見つけた）。
func killGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
