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

package remote

// shell.go — 装置の CLI（POSIX のシェルでない殻）にコマンドを打つ口
// （Backlog #159、cli-device）。
//
//   - RunLine は 1 行を ssh の exec のコマンドとしてそのまま渡す（引用しない）。
//     装置の CLI はシェルでないので Quote の引用は通じない。行に入る値は呼ぶ側
//     （cli-device の機種の定義の値の形）が読み込みの時点で絞る。改行・NUL・
//     制御文字は断る。sudo は付けない（装置の CLI に sudo は無い）。接続は
//     使い回さない（装置の ssh は 1 本の接続に 1 つのセッションしか受けない
//     ことが多い）。
//   - OpenShell は相手に pty を取らせて（ssh -t -t）対話の殻を開く。プロンプトを
//     待ってコマンドを打つのは呼ぶ側。接続は使い回さない（サブシステムと同じ）。

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"unicode"
)

// ShellOpener は装置の CLI に exec の 1 行を流し、対話の殻を開ける Executor（*SSH）。
type ShellOpener interface {
	// RunLine は line を exec のコマンドとしてそのまま流し、標準出力を返す。
	RunLine(ctx context.Context, line string) ([]byte, error)
	// OpenShell は pty つきの対話の殻を開く。
	OpenShell(ctx context.Context) (io.ReadWriteCloser, error)
}

var _ ShellOpener = (*SSH)(nil)

// CheckLine は line が 1 行のコマンドとして渡せるか（空でない・改行・NUL・
// 制御文字を含まない）。
func CheckLine(line string) error {
	if strings.TrimSpace(line) == "" {
		return fmt.Errorf("empty command line")
	}
	for _, r := range line {
		if r == '\t' {
			continue
		}
		if unicode.IsControl(r) {
			return fmt.Errorf("the command line contains a control character (%U)", r)
		}
	}
	return nil
}

// RunLine implements ShellOpener.
func (s *SSH) RunLine(ctx context.Context, line string) ([]byte, error) {
	if err := CheckLine(line); err != nil {
		return nil, fmt.Errorf("remote %s: %w", s.h.Name, err)
	}
	ctx, cancel := withTimeout(ctx, s.h.Timeout)
	defer cancel()
	if err := s.recentFailure(); err != nil {
		return nil, err
	}
	keyPath, done, err := s.writeKey(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	args, err := s.baseArgs(keyPath)
	if err != nil {
		return nil, err
	}
	args = append(args, "-o", "ControlMaster=no", "-o", "ControlPath=none", "--", s.h.Address, line)
	out, err := runCmd(ctx, exec.CommandContext(ctx, s.h.SSH, args...), nil)
	return out, s.classify(err)
}

// OpenShell implements ShellOpener.
func (s *SSH) OpenShell(ctx context.Context) (io.ReadWriteCloser, error) {
	return s.openStream(ctx, true, "--", s.h.Address)
}
