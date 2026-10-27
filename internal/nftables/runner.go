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

package nftables

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/internal/remote"
)

// Runner は nft を 1 回走らせる seam（Backlog #121）。live の Provider は nft を
// この interface 越しにだけ呼ぶので、通常のテストは偽の nft（nftfake）で回り、
// 本物の nft は統合テスト（build tag integration_nftables）だけが使う。
//
// args は nft に渡す引数（例 {"-j", "list", "table", "inet", "filter"}）、
// stdin は標準入力（"-f -" のスクリプト。無ければ nil）。nft が走って 0 以外で
// 終わったときは *ExitError を、走らせられなかったとき（nft が無い・ctx の期限
// 切れ等）はそれ以外の error を返す。
type Runner interface {
	Run(ctx context.Context, args []string, stdin []byte) (stdout []byte, err error)
}

// ExitError は nft が走って 0 以外の終了コードで終わったことを表す。
type ExitError struct {
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		return fmt.Sprintf("nft exited with code %d", e.Code)
	}
	return fmt.Sprintf("nft exited with code %d: %s", e.Code, msg)
}

// runTimeout は ctx に期限が無いときの nft 1 回の上限。
const runTimeout = 30 * time.Second

// ExecRunner は本物の nft バイナリ（Path、既定 "nft"）を走らせる Runner。
type ExecRunner struct {
	Path string
}

// Run implements Runner.
func (r ExecRunner) Run(ctx context.Context, args []string, stdin []byte) ([]byte, error) {
	path := r.Path
	if path == "" {
		path = "nft"
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, runTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, path, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ctx.Err() == nil {
		return stdout.Bytes(), &ExitError{Code: ee.ExitCode(), Stderr: stderr.String()}
	}
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", path, err)
	}
	return stdout.Bytes(), nil
}

// RemoteRunner は nft を実行の層（internal/remote）の Executor 越しに走らせる
// Runner（Backlog #130）: 手元なら remote.Local、離れたホストなら remote.SSH。
// コマンドが 0 以外で終われば *ExitError、ホストに届かない・認証の失敗・
// ホスト鍵の不一致は *remote.ConnError をそのまま返す。
type RemoteRunner struct {
	Exec remote.Executor
	// NFT はそのホストの nft のパス（既定 "nft"）。
	NFT string
}

// Run implements Runner.
func (r RemoteRunner) Run(ctx context.Context, args []string, stdin []byte) ([]byte, error) {
	nft := r.NFT
	if nft == "" {
		nft = "nft"
	}
	// The deadline is the Executor's (the host's timeout).
	out, err := r.Exec.Run(ctx, append([]string{nft}, args...), stdin)
	var ee *remote.ExitError
	if errors.As(err, &ee) {
		return out, &ExitError{Code: ee.Code, Stderr: ee.Stderr}
	}
	return out, err
}
