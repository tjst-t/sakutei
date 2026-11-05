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

package secretbackend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
)

// CommandName はモジュールの名前。
const CommandName = "command"

// Command は外部コマンド（パスワード管理の CLI・現場のスクリプト）に問い合わせる secret backend。
//
//	<command> generation <パス>         → 世代を正の整数で出す（値を世代と取り違えて出さないよう、数だけ）
//	<command> value <パス> <世代>       → 値を出す（末尾の改行 1 つは落とす）
//
// 終了コードが 0 でなければエラー。stdout はエラーメッセージに引用しない。stderr は
// generation のときだけ 1 行に縮めて引用する（value の出力はどのメッセージにも出さない）。
type Command struct {
	path    string
	timeout time.Duration
}

// CommandModule は secret_backends の module: command。settings：
//
//	command   実行ファイルのパス（必須）
//	timeout   呼び出しごとのタイムアウト（既定 10s）
func CommandModule(settings map[string]any) (core.SecretBackend, error) {
	const kind = CommandName
	if err := checkKeys(kind, settings, "command", "timeout"); err != nil {
		return nil, err
	}
	p, err := str(kind, settings, "command")
	if err != nil {
		return nil, err
	}
	if p == "" {
		return nil, fmt.Errorf("%s: command を書く", kind)
	}
	if _, err := exec.LookPath(p); err != nil {
		return nil, fmt.Errorf("%s: command %s: %w", kind, p, err)
	}
	c := &Command{path: p}
	if c.timeout, err = timeoutSetting(kind, settings, 10*time.Second); err != nil {
		return nil, err
	}
	return c, nil
}

// Generation は "<command> generation <パス>" を実行する。
func (c *Command) Generation(ctx context.Context, path string) (string, error) {
	out, stderr, err := c.run(ctx, "generation", path)
	if err != nil {
		if s := clip(stderr); s != "" {
			err = fmt.Errorf("%w: %s", err, s)
		}
		return "", fmt.Errorf("%s: %s の世代: %w", CommandName, path, err)
	}
	n, perr := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if perr != nil || n <= 0 {
		return "", fmt.Errorf("%s: %s の世代: コマンドが正の整数を出さなかった", CommandName, path)
	}
	return strconv.FormatInt(n, 10), nil
}

// Resolve は "<command> value <パス> <世代>" を実行する。
func (c *Command) Resolve(ctx context.Context, path, gen string) (string, error) {
	out, _, err := c.run(ctx, "value", path, gen)
	if err != nil {
		return "", fmt.Errorf("%s: %s（世代 %s）の値: %w", CommandName, path, gen, err)
	}
	out = strings.TrimSuffix(out, "\n")
	return strings.TrimSuffix(out, "\r"), nil
}

func (c *Command) run(ctx context.Context, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.path, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limited{w: &stdout, n: 1 << 20}
	cmd.Stderr = &limited{w: &stderr, n: 4 << 10}
	// stdout を握ったままの子プロセスがタイムアウトのあとまで残らないようにする。
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "", "", fmt.Errorf("%s でタイムアウト", c.timeout)
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", stderr.String(), fmt.Errorf("終了コード %d", ee.ExitCode())
		}
		return "", "", err
	}
	return stdout.String(), stderr.String(), nil
}

// limited は n バイトまでを残し、残りを捨てる。
type limited struct {
	w io.Writer
	n int
}

func (l *limited) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}
