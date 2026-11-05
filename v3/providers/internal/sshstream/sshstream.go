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

// Package sshstream は、コアの SDK の sdk/hostexec の接続方式（ssh・local）の上に
// 載る 2 つの層：1 本の stream を開く層（netconf の ssh サブシステム・機器の CLI の
// 対話）と、機器の CLI の 1 行をそのまま流す層（引用符で包まない）。E-E1・E-B7・E-B11。
//
// 決まり：
//
//   - ssh の接続設定（known_hosts でホスト鍵を確かめる・BatchMode・鍵）は
//     hostexec.SSH と同じ。接続は使い回さない（ControlPath=none。機器は 1 本の接続に
//     1 つのセッションしか受け付けないことが多い）。sudo は付けない（機器に sudo は無い）。
//   - ssh で到達できない（終了コード 255・起動できない）ときは *hostexec.UnreachableError。
//     stream の読み込み中に相手が切れたとき、ssh が 255 で終わっていればそのエラーを返す
//     （provider は到達できないとき Absent でなく Unknown を返す）。
package sshstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v3/sdk/hostexec"
)

// sshArgs は ssh の共通の引数（宛先の前まで）。
func sshArgs(s hostexec.SSH, extra ...string) []string {
	bin := s.Binary
	if bin == "" {
		bin = "ssh"
	}
	out := []string{bin, "-F", "/dev/null", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + s.KnownHosts, "-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "IdentitiesOnly=yes", "-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=10",
		"-o", "ControlPath=none", "-i", s.Key}
	if s.Port != 0 {
		out = append(out, "-p", strconv.Itoa(s.Port))
	}
	if s.User != "" {
		out = append(out, "-l", s.User)
	}
	return append(out, extra...)
}

// SubsystemCommand は ssh のサブシステム（netconf）を開く argv（テストで確かめる）。
func SubsystemCommand(s hostexec.SSH, name string) []string {
	return append(sshArgs(s, "-T", "-s", "--", s.Address), name)
}

// ShellCommand は pty つきのシェルを開く argv（機器の CLI の対話）。
func ShellCommand(s hostexec.SSH) []string {
	return sshArgs(s, "-tt", "--", s.Address)
}

// LineCommand は機器の CLI の 1 行をそのまま流す argv（exec。引用符で包まない：
// 機器の CLI はシェルでなく、行を自分で解釈する）。
func LineCommand(s hostexec.SSH, line string) []string {
	return append(sshArgs(s, "-T", "--", s.Address), line)
}

// Subsystem は r のホストのサブシステム name を開く（ssh だけ）。
func Subsystem(ctx context.Context, r hostexec.Runner, name string) (io.ReadWriteCloser, error) {
	s, ok := r.(hostexec.SSH)
	if !ok {
		return nil, fmt.Errorf("ホスト %s: サブシステム %s は ssh 接続でだけ開ける", r.Name(), name)
	}
	return Start(ctx, s.HostName, SubsystemCommand(s, name), true)
}

// Shell は r のホストに pty つきのシェルを開く（ssh だけ）。
func Shell(ctx context.Context, r hostexec.Runner) (io.ReadWriteCloser, error) {
	s, ok := r.(hostexec.SSH)
	if !ok {
		return nil, fmt.Errorf("ホスト %s: 対話のセッションは ssh 接続でだけ開ける", r.Name())
	}
	return Start(ctx, s.HostName, ShellCommand(s), true)
}

// RunLine は機器の CLI の 1 行を流す。ssh なら行をそのまま渡し、local なら
// `sh -c` で実行する（手元の模擬）。0 以外の終了は *hostexec.ExitError。
func RunLine(ctx context.Context, r hostexec.Runner, line string) ([]byte, error) {
	switch s := r.(type) {
	case hostexec.SSH:
		return run(ctx, s.HostName, LineCommand(s, line), true)
	case hostexec.Local:
		argv := append(append([]string(nil), s.Prefix...), "sh", "-c", line)
		return run(ctx, s.HostName, argv, false)
	}
	return nil, fmt.Errorf("ホスト %s: 知らない接続方式", r.Name())
}

func run(ctx context.Context, host string, argv []string, viaSSH bool) ([]byte, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, hostexec.DefaultTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return stdout.Bytes(), nil
	case ctx.Err() != nil:
		return nil, &hostexec.UnreachableError{Host: host, Err: fmt.Errorf("時間切れ: %w", ctx.Err())}
	case errors.As(err, &ee):
		if viaSSH && ee.ExitCode() == 255 {
			return nil, &hostexec.UnreachableError{Host: host, Err: errors.New(strings.TrimSpace(stderr.String()))}
		}
		return stdout.Bytes(), &hostexec.ExitError{Code: ee.ExitCode(), Stderr: stderr.String()}
	default:
		return nil, &hostexec.UnreachableError{Host: host, Err: err}
	}
}

// Start は argv を実行し、その標準入出力を 1 本の stream にする。viaSSH なら終了
// コード 255 を到達できないエラーにする。開く前に ctx が終われば stream を閉じる
// （ctx は開くまでの期限。開いたあとは Close まで使える）。
func Start(ctx context.Context, host string, argv []string, viaSSH bool) (io.ReadWriteCloser, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// 標準出力は自分の pipe にする（StdoutPipe は Wait が読み終える前に閉じる）。
	out, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = pw
	st := &stream{host: host, cmd: cmd, in: in, out: out, viaSSH: viaSSH, done: make(chan struct{})}
	cmd.Stderr = &st.stderr
	err = cmd.Start()
	pw.Close()
	if err != nil {
		out.Close()
		return nil, &hostexec.UnreachableError{Host: host, Err: err}
	}
	go func() {
		st.waitErr = cmd.Wait()
		close(st.done)
	}()
	if err := ctx.Err(); err != nil {
		st.Close()
		return nil, &hostexec.UnreachableError{Host: host, Err: err}
	}
	return st, nil
}

type stream struct {
	host   string
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *os.File
	viaSSH bool
	stderr lockedBuffer

	done    chan struct{}
	waitErr error
	once    sync.Once
}

// Read は相手の出力を読む。相手が切れて ssh が 255 で終わっていれば到達できないエラーを返す。
func (s *stream) Read(b []byte) (int, error) {
	n, err := s.out.Read(b)
	if err != nil && n == 0 && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe)) {
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
			return 0, err
		}
		var ee *exec.ExitError
		if s.viaSSH && errors.As(s.waitErr, &ee) && ee.ExitCode() == 255 {
			return 0, &hostexec.UnreachableError{Host: s.host, Err: errors.New(strings.TrimSpace(s.stderr.String()))}
		}
	}
	return n, err
}

func (s *stream) Write(b []byte) (int, error) { return s.in.Write(b) }

// Close は入力を閉じ、相手を止めて待つ。
func (s *stream) Close() error {
	s.once.Do(func() {
		_ = s.in.Close()
		select {
		case <-s.done:
		case <-time.After(500 * time.Millisecond):
			_ = s.cmd.Process.Kill()
			<-s.done
		}
		_ = s.out.Close()
	})
	return nil
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.b.Len() > 64<<10 {
		return len(p), nil
	}
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
