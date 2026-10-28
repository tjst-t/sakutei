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

// subsystem.go — ssh のサブシステム（ssh -s、例 netconf。RFC 6242）を
// 双方向のバイト列として開く（Backlog #141）。
//
//   - コマンドと違い、やりとりが往復する（hello を読んでから rpc を書く）ので、
//     Run ではなく標準入出力をつないだままの Stream を返す。
//   - サブシステムはシェルを通らない（引用は要らない）。sudo は付かない。
//   - 接続は使い回さない（ControlMaster=no・ControlPath=none）。サブシステムの
//     セッションそのものを呼ぶ側が使い回す（netconf はホストごとに 1 本）。
//     ssh の多重化を受けない実装のサーバ（機器の ssh）もあるので、直につなぐ。
//   - 秘密鍵の一時ファイルは、最初のバイトが届いた（認証が済んだ）ときか
//     Close で消す。
//   - 標準出力が終わったとき、ssh が 255 で終わっていれば Read は EOF でなく
//     *ConnError（届かない・認証・ホスト鍵）を返す。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"time"
)

// SubsystemOpener は ssh のサブシステムを開ける Executor（*SSH）。
type SubsystemOpener interface {
	// OpenSubsystem はサブシステム name を開く。返す Stream の Read は
	// サーバからのバイト、Write はサーバへのバイト。Close で閉じる。
	OpenSubsystem(ctx context.Context, name string) (io.ReadWriteCloser, error)
}

var _ SubsystemOpener = (*SSH)(nil)

var subsystemName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._-]*$`)

// closeWait は Close が ssh の終わりを待つ上限（過ぎれば止める）。
const closeWait = 5 * time.Second

// OpenSubsystem implements SubsystemOpener.
func (s *SSH) OpenSubsystem(ctx context.Context, name string) (io.ReadWriteCloser, error) {
	if !subsystemName.MatchString(name) {
		return nil, fmt.Errorf("remote %s: subsystem %q: not a subsystem name", s.h.Name, name)
	}
	return s.openStream(ctx, false, "-s", "--", s.h.Address, name)
}

// openStream は ssh を直に（使い回さずに）起こし、標準入出力をつないだままの
// Stream を返す。tail は宛先とその後ろ（"-s -- <address> <name>" など）。
// tty なら -T を外して -t -t（相手に pty を取らせる、OpenShell）。
func (s *SSH) openStream(ctx context.Context, tty bool, tail ...string) (io.ReadWriteCloser, error) {
	if err := s.recentFailure(); err != nil {
		return nil, err
	}
	keyPath, done, err := s.writeKey(ctx)
	if err != nil {
		return nil, err
	}
	args, err := s.baseArgs(keyPath)
	if err != nil {
		done()
		return nil, err
	}
	if tty {
		kept := args[:0]
		for _, a := range args {
			if a != "-T" {
				kept = append(kept, a)
			}
		}
		args = append(kept, "-t", "-t")
	}
	args = append(args, "-o", "ControlMaster=no", "-o", "ControlPath=none")
	args = append(args, tail...)
	cmd := exec.Command(s.h.SSH, args...)
	in, err := cmd.StdinPipe()
	if err != nil {
		done()
		return nil, err
	}
	// An os.Pipe of our own (not StdoutPipe): Wait runs concurrently with the
	// reads, and must not close the read end under them.
	pr, pw, err := os.Pipe()
	if err != nil {
		done()
		return nil, err
	}
	cmd.Stdout = pw
	st := &stream{s: s, cmd: cmd, in: in, out: pr, waited: make(chan struct{})}
	cmd.Stderr = &st.stderr
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		done()
		return nil, fmt.Errorf("remote %s: run %s: %w", s.h.Name, s.h.SSH, err)
	}
	pw.Close()
	st.dropKey = sync.OnceFunc(done)
	go func() {
		st.waitErr = cmd.Wait()
		close(st.waited)
	}()
	return st, nil
}

// stream は開いたサブシステム。
type stream struct {
	s       *SSH
	cmd     *exec.Cmd
	in      io.WriteCloser
	out     *os.File
	stderr  lockedBuffer
	dropKey func()

	waited  chan struct{}
	waitErr error

	closeOnce sync.Once
}

// Read implements io.Reader.  At the end of the output it reports how ssh
// ended: a *ConnError when ssh itself failed (exit 255), else io.EOF.
func (st *stream) Read(p []byte) (int, error) {
	n, err := st.out.Read(p)
	if n > 0 {
		st.dropKey()
	}
	if err == nil || n > 0 {
		return n, err
	}
	if errors.Is(err, io.EOF) {
		select {
		case <-st.waited:
		case <-time.After(closeWait):
			return 0, io.EOF
		}
		var ee *exec.ExitError
		if errors.As(st.waitErr, &ee) && ee.ExitCode() == 255 {
			return 0, st.s.classify(&ExitError{Code: 255, Stderr: st.stderr.String()})
		}
		return 0, io.EOF
	}
	return 0, err
}

// Write implements io.Writer.
func (st *stream) Write(p []byte) (int, error) { return st.in.Write(p) }

// Close closes the input and waits (up to closeWait) for ssh to end; then it
// is stopped.
func (st *stream) Close() error {
	st.closeOnce.Do(func() {
		_ = st.in.Close()
		select {
		case <-st.waited:
		case <-time.After(closeWait):
			_ = st.cmd.Process.Kill()
			<-st.waited
		}
		_ = st.out.Close()
		st.dropKey()
	})
	return nil
}
