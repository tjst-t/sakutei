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

// Package remote は provider が外部コマンドを「手元」か「ssh 越し」で流す
// 実行の層（Backlog #130、§29.1）。
//
// 設計判断:
//
//   - provider は Executor 越しにだけコマンドを走らせる。手元は Local（os/exec
//     で直に）、離れたホストは SSH（OpenSSH の ssh CLI を os/exec で呼ぶ。Go の
//     依存は増やさない）。通常のテストは Local か偽の ssh で回り、本物の sshd は
//     統合テスト（build tag integration_remote）だけが使う。
//   - コマンドは引数の列（argv）で渡す。SSH はリモートのシェルが解釈する 1 本の
//     文字列に、各要素を Quote で引用して組む（注入させない）。中身（ファイル・
//     nft のスクリプト）は標準入力で渡し、引数には載せない。
//   - 失敗は 3 つに分ける: コマンドが走って 0 以外で終わった（*ExitError）、
//     ホストに届かない・認証の失敗・ホスト鍵の不一致（*ConnError）、それ以外
//     （走らせられない・期限切れ）。上の provider はどれも「観測できない」
//     （unknown。absent にしない、§3・§5.4）にし、ホスト鍵の不一致だけは
//     PermanentError で知らせる（ProviderError）。
//   - 鍵・パスワードは引数・ログ・エラーに出さない（§20）。エラーに載せるのは
//     ssh の標準エラーの頭だけ（ssh は鍵の中身を書かない）。
package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/pkg/provider"
)

// Executor は外部コマンドを 1 つのホストで走らせる。
type Executor interface {
	// Run は argv（argv[0] がコマンド）を走らせ、標準出力を返す。stdin は
	// 標準入力（無ければ nil）。0 以外で終われば *ExitError、ホストに届かない
	// などは *ConnError、それ以外は error。
	Run(ctx context.Context, argv []string, stdin []byte) ([]byte, error)
	// Hold は argv を走らせ、標準出力の最初の行が "ok" になるまで待つ（例:
	// ロックを取ったら ok を書き、標準入力が閉じるまで待つコマンド）。release は
	// 標準入力を閉じてコマンドの終わりを待つ。ok の前に終われば error。
	Hold(ctx context.Context, argv []string) (release func() error, err error)
	// Target は対象の名前（手元なら ""）。エラーの文言に使う。
	Target() string
}

// ExitError はコマンドが走って 0 以外で終わったことを表す。
type ExitError struct {
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		return fmt.Sprintf("exited with code %d", e.Code)
	}
	return fmt.Sprintf("exited with code %d: %s", e.Code, msg)
}

// ConnKind は ConnError の種類。
type ConnKind int

const (
	// Unreachable はホストに届かない（接続の拒否・期限切れ・名前を引けない等）。
	Unreachable ConnKind = iota
	// AuthFailed は認証の失敗（鍵が受け入れられない）。
	AuthFailed
	// HostKeyMismatch はホスト鍵を確かめられない（known_hosts に無い・違う）。
	HostKeyMismatch
)

func (k ConnKind) String() string {
	switch k {
	case AuthFailed:
		return "authentication failed"
	case HostKeyMismatch:
		return "host key verification failed"
	}
	return "unreachable"
}

// ConnError はホストとのつながりの失敗（コマンドは走っていない）。
type ConnError struct {
	Target string
	Kind   ConnKind
	// Detail は ssh の標準エラーの頭（鍵の中身は含まない）。
	Detail string
}

func (e *ConnError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("remote %s: %s", e.Target, e.Kind)
	}
	return fmt.Sprintf("remote %s: %s: %s", e.Target, e.Kind, e.Detail)
}

// IsHostKeyMismatch は err がホスト鍵の不一致かどうか。
func IsHostKeyMismatch(err error) bool {
	var ce *ConnError
	return errors.As(err, &ce) && ce.Kind == HostKeyMismatch
}

// ProviderError は観測・適用の失敗を provider の error にする: ホスト鍵の
// 不一致は設定（known_hosts）を直すまで治らないので PermanentError、それ
// 以外は TransientError。
func ProviderError(err error) error {
	if IsHostKeyMismatch(err) {
		return provider.NewPermanentError(err)
	}
	return provider.NewTransientError(err)
}

// ─────────────────────────────────────────────────────────────────────────────
// 引用
// ─────────────────────────────────────────────────────────────────────────────

// safeWord はそのままでもシェルが書き換えない語（先頭の '=' は zsh が展開する
// ので別に断る）。
var safeWord = regexp.MustCompile(`^[A-Za-z0-9_@+:,./-]+$`)

// Quote は s を POSIX シェル（sh・bash・zsh）がそのまま 1 つの語として読む
// 形にする: 安全な文字だけならそのまま、そうでなければ ' で囲み、中の ' は
// '"'"' にする（' の中ではシェルは何も解釈しない）。
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	if safeWord.MatchString(s) && s[0] != '=' {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// ShellCommand は argv をリモートのシェルに渡す 1 本の文字列にする。NUL を
// 含む引数はシェルに渡せないので error。
func ShellCommand(argv []string) (string, error) {
	if len(argv) == 0 {
		return "", errors.New("no command")
	}
	words := make([]string, len(argv))
	for i, a := range argv {
		if strings.ContainsRune(a, 0) {
			return "", fmt.Errorf("argument %d contains a NUL byte", i)
		}
		words[i] = Quote(a)
	}
	return strings.Join(words, " "), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// 手元
// ─────────────────────────────────────────────────────────────────────────────

// DefaultTimeout は ctx に期限が無いときのコマンド 1 回の上限。
const DefaultTimeout = 60 * time.Second

// Local は手元でコマンドを走らせる Executor。
type Local struct {
	// Timeout はコマンド 1 回の上限（0 なら DefaultTimeout）。
	Timeout time.Duration
}

// Target implements Executor.
func (Local) Target() string { return "" }

// Run implements Executor.
func (l Local) Run(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("no command")
	}
	ctx, cancel := withTimeout(ctx, l.Timeout)
	defer cancel()
	return runCmd(ctx, exec.CommandContext(ctx, argv[0], argv[1:]...), stdin)
}

// Hold implements Executor.
func (l Local) Hold(ctx context.Context, argv []string) (func() error, error) {
	if len(argv) == 0 {
		return nil, errors.New("no command")
	}
	return holdCmd(ctx, exec.Command(argv[0], argv[1:]...))
}

func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	if d <= 0 {
		d = DefaultTimeout
	}
	return context.WithTimeout(ctx, d)
}

// runCmd は cmd を走らせ、0 以外の終了を *ExitError にする。
func runCmd(ctx context.Context, cmd *exec.Cmd, stdin []byte) ([]byte, error) {
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
		if ctx.Err() != nil {
			return nil, fmt.Errorf("run %s: %w", cmd.Args[0], ctx.Err())
		}
		return nil, fmt.Errorf("run %s: %w", cmd.Args[0], err)
	}
	return stdout.Bytes(), nil
}

// holdTimeout は Hold が "ok" を待つ上限。
const holdTimeout = 60 * time.Second

// holdCmd は cmd を走らせ、標準出力の最初の行が "ok" になるのを待つ。
// cmd に Stdin・Stdout・Stderr を設定してはいけない。ok の前に終われば、
// 0 以外の終了は *ExitError（標準エラーつき）。
func holdCmd(ctx context.Context, cmd *exec.Cmd) (func() error, error) {
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr lockedBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("run %s: %w", cmd.Args[0], err)
	}
	ended := func(cause error) error {
		_ = in.Close()
		werr := cmd.Wait()
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			return &ExitError{Code: ee.ExitCode(), Stderr: stderr.String()}
		}
		return fmt.Errorf("run %s: %w", cmd.Args[0], cause)
	}
	got := make(chan error, 1)
	go func() {
		var line []byte
		buf := make([]byte, 1)
		for {
			if _, err := out.Read(buf); err != nil {
				got <- fmt.Errorf("ended before it was ready: %w", err)
				return
			}
			if buf[0] == '\n' {
				break
			}
			if len(line) < 16 {
				line = append(line, buf[0])
			}
		}
		if string(line) != "ok" {
			got <- fmt.Errorf("unexpected first line %q", line)
			return
		}
		got <- nil
		_, _ = io.Copy(io.Discard, out)
	}()
	timer := time.NewTimer(holdTimeout)
	defer timer.Stop()
	select {
	case err := <-got:
		if err != nil {
			return nil, ended(err)
		}
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		return nil, ended(ctx.Err())
	case <-timer.C:
		_ = cmd.Process.Kill()
		return nil, ended(errors.New("timed out waiting for it to be ready"))
	}
	return func() error {
		_ = in.Close()
		if err := cmd.Wait(); err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return &ExitError{Code: ee.ExitCode(), Stderr: stderr.String()}
			}
			return err
		}
		return nil
	}, nil
}

// lockedBuffer は並行に書かれてもよい bytes.Buffer。
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
