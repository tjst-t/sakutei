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

package clidevice

// session.go — 装置の CLI とのやりとり。
//
//   - exec：コマンド 1 つを ssh の exec の 1 行として流す（remote.ShellOpener の
//     RunLine）。0 以外の終了は失敗。
//   - interactive：pty つきの殻を開き（OpenShell）、プロンプトの行が出るまで読む。
//     ページ送りの印（pager）が最後の行に出たらキーを打ち、印は出力から消す。
//     打ったコマンドのこだま（pty の echo）は出力の先頭から外す。
//
// 出力は端末として読む: "\r\n" は改行、行の途中の "\r" は行の頭に戻る（後ろの
// 文字で上書きされる）、"\b" は 1 文字消す、ESC の制御列は捨てる。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/remote"
)

// Conn は 1 つの装置へのつなぎ方（*remote.SSH、テストでは偽の装置）。
type Conn interface {
	// Target は装置の名前（エラーの文言）。
	Target() string
	remote.ShellOpener
}

// session は開いたやりとり。
type session interface {
	// do は cmd を打ち、出力（行は "\n" 区切り、こだまとプロンプトを除く）を返す。
	do(ctx context.Context, cmd string) (string, error)
	close()
}

// cmdFailed はコマンドが失敗として終わった（exec の 0 以外の終了）。
type cmdFailed struct {
	out    string
	reason string
}

func (e *cmdFailed) Error() string { return e.reason }

// open は d の型でやりとりを開く。
func (d *Definition) open(ctx context.Context, c Conn) (session, error) {
	if d.Mode == ModeExec {
		return &execSession{d: d, c: c}, nil
	}
	rw, err := c.OpenShell(ctx)
	if err != nil {
		return nil, err
	}
	s := &shellSession{d: d, rw: rw, chunks: make(chan []byte, 16)}
	go s.pump()
	if _, err := s.wait(ctx); err != nil {
		s.abort()
		return nil, fmt.Errorf("waiting for the first prompt: %w", err)
	}
	for _, cmd := range d.Setup {
		out, err := s.do(ctx, cmd)
		if err != nil {
			s.abort()
			return nil, fmt.Errorf("setup %q: %w", cmd, err)
		}
		if n, line, bad := d.errorLine(out); bad {
			s.abort()
			return nil, fmt.Errorf("setup %q: error marker at line %d: %q", cmd, n, clip(line))
		}
	}
	return s, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// exec
// ─────────────────────────────────────────────────────────────────────────────

type execSession struct {
	d *Definition
	c Conn
}

func (s *execSession) do(ctx context.Context, cmd string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.d.Timeout)
	defer cancel()
	raw, err := s.c.RunLine(ctx, cmd)
	lines, partial := screen(raw)
	if partial != "" {
		lines = append(lines, partial)
	}
	out := strings.Join(lines, "\n")
	var ee *remote.ExitError
	if errors.As(err, &ee) {
		return out, &cmdFailed{out: out, reason: fmt.Sprintf("exited with code %d", ee.Code)}
	}
	return out, err
}

func (s *execSession) close() {}

// ─────────────────────────────────────────────────────────────────────────────
// interactive
// ─────────────────────────────────────────────────────────────────────────────

type shellSession struct {
	d      *Definition
	rw     io.ReadWriteCloser
	chunks chan []byte
	// readErr は chunks が閉じた訳（pump が閉じる前に書く）。
	readErr error
	raw     []byte
}

// pump は読んだ塊を chunks に送る。終われば閉じる。
func (s *shellSession) pump() {
	buf := make([]byte, 4096)
	for {
		n, err := s.rw.Read(buf)
		if n > 0 {
			s.chunks <- append([]byte(nil), buf[:n]...)
		}
		if err != nil {
			s.readErr = err
			close(s.chunks)
			return
		}
	}
}

// pagerMark は「ここで行の頭に戻る」の印（ページ送りの印を消すのに使う。
// 端末の出力に NUL は来ない）。
const pagerMark = 0

// wait はプロンプトの行が出るまで読み、そこまでの行を返す（プロンプトの行は
// 含まない）。
func (s *shellSession) wait(ctx context.Context) ([]string, error) {
	timer := time.NewTimer(s.d.Timeout)
	defer timer.Stop()
	for {
		lines, partial := screen(s.raw)
		if s.d.Pager != nil && partial != "" && s.d.Pager.MatchString(partial) {
			if _, err := io.WriteString(s.rw, s.d.PagerKey); err != nil {
				return nil, err
			}
			s.raw = append(s.raw, pagerMark)
			continue
		}
		if partial != "" && s.d.Prompt.MatchString(partial) {
			s.raw = nil
			return lines, nil
		}
		select {
		case b, ok := <-s.chunks:
			if !ok {
				err := s.readErr
				if err == nil || errors.Is(err, io.EOF) {
					err = errors.New("the session ended before the prompt")
				}
				return nil, err
			}
			s.raw = append(s.raw, b...)
		case <-timer.C:
			return nil, fmt.Errorf("no prompt within %s (the last line is %q)", s.d.Timeout, clip(partial))
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (s *shellSession) do(ctx context.Context, cmd string) (string, error) {
	s.raw = nil
	if _, err := io.WriteString(s.rw, cmd+s.d.Newline); err != nil {
		return "", err
	}
	lines, err := s.wait(ctx)
	if err != nil {
		return "", err
	}
	// こだま（打った行。前にプロンプトが付くこともある）を外す。
	if len(lines) > 0 && strings.HasSuffix(strings.TrimRight(lines[0], " "), strings.TrimRight(cmd, " ")) {
		lines = lines[1:]
	}
	return strings.Join(lines, "\n"), nil
}

func (s *shellSession) close() {
	for _, cmd := range s.d.Logout {
		_, _ = io.WriteString(s.rw, cmd+s.d.Newline)
	}
	s.abort()
}

func (s *shellSession) abort() {
	_ = s.rw.Close()
	// pump が送りかけの塊で止まらないよう、閉じるまで読み捨てる。
	go func() {
		for range s.chunks {
		}
	}()
}

// ─────────────────────────────────────────────────────────────────────────────
// 端末の出力
// ─────────────────────────────────────────────────────────────────────────────

// screen は端末の出力を行に直す。partial は改行で終わっていない最後の行。
func screen(raw []byte) (lines []string, partial string) {
	var cur []byte
	for i := 0; i < len(raw); i++ {
		b := raw[i]
		switch {
		case b == '\n':
			lines = append(lines, string(cur))
			cur = cur[:0]
		case b == '\r':
			j := i + 1
			for j < len(raw) && raw[j] == '\r' {
				j++
			}
			if j == len(raw) || raw[j] == '\n' {
				// "\r\n"（"\r\r\n"）は改行。最後の "\r" は続きを待つ。
				i = j - 1
				continue
			}
			cur = cur[:0]
			i = j - 1
		case b == pagerMark:
			cur = cur[:0]
		case b == '\b':
			if len(cur) > 0 {
				cur = cur[:len(cur)-1]
			}
		case b == 0x1b:
			// ESC [ … 終わりの文字（0x40–0x7e）、または ESC と次の 1 文字。
			if i+1 < len(raw) && raw[i+1] == '[' {
				j := i + 2
				for j < len(raw) && (raw[j] < 0x40 || raw[j] > 0x7e) {
					j++
				}
				i = j
			} else {
				i++
			}
		case b < 0x20 && b != '\t':
			// ほかの制御文字（ベルなど）は捨てる。
		default:
			cur = append(cur, b)
		}
	}
	return lines, string(cur)
}

// errorLine は out の中でエラーの印に当たる最初の行（1 から）。
func (d *Definition) errorLine(out string) (int, string, bool) {
	for i, l := range strings.Split(out, "\n") {
		for _, re := range d.Errors {
			if re.MatchString(l) {
				return i + 1, l, true
			}
		}
	}
	return 0, "", false
}

// clip は文言に載せる行の頭（200 文字まで）。
func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
