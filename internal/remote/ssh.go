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

// ssh.go — OpenSSH の ssh CLI でコマンドを流す Executor（Backlog #130）。
//
//   - ssh には利用者の設定（~/.ssh/config）を読ませない（-F /dev/null）。
//     BatchMode=yes（パスワード・確認を尋ねない）、ホスト鍵は設定の known_hosts
//     だけで確かめ（StrictHostKeyChecking=yes。初回だけ信じるのは
//     trust_on_first_use を明示したときだけ accept-new）、ConnectTimeout を付ける。
//   - 接続の使い回し: 対象ごとに 1 本の master（ControlMaster）を -f -N で立て、
//     各コマンドは ControlMaster=no でそこに相乗りする（ControlPersist の間、
//     使われなければ master は自分で終わる）。master が無い・死んでいれば立て直し、
//     それでも相乗りできなければ ssh は自分で直につなぐ。ソケットは 0700 の
//     ディレクトリ（自分の uid のもの）に置く。
//   - 秘密鍵は SecretRef から使う間だけ 0600 の一時ファイルに出して -i で渡し、
//     終わったら消す。鍵を設定しなければ ssh-agent（SSH_AUTH_SOCK）を使う。
//   - ホストに届かない・認証の失敗・ホスト鍵の不一致は *ConnError。失敗した
//     対象には ConnectTimeout の間は新しくつながず、同じ失敗をすぐ返す（届かない
//     ホストの資源ごとに ConnectTimeout を待たない）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SSH の既定。
const (
	DefaultConnectTimeout = 10 * time.Second
	DefaultControlPersist = 60 * time.Second
)

// SSHHost は 1 つの対象の設定。
type SSHHost struct {
	// Name は対象の名前（ID の先頭・エラーの文言）。
	Name string
	// Address・Port・User はつなぐ先（Port 0 は 22、User "" は ssh の既定）。
	Address string
	Port    int
	User    string
	// KnownHosts はホスト鍵を確かめる known_hosts のパス（必須）。
	KnownHosts string
	// TrustOnFirstUse なら known_hosts に無いホストの鍵を初回だけ信じて書き足す
	// （StrictHostKeyChecking=accept-new）。違う鍵は常に拒む。
	TrustOnFirstUse bool
	// Key は秘密鍵（OpenSSH の形式）を返す。nil なら ssh-agent を使う。値は
	// 使う間だけ一時ファイルに出す。
	Key func(ctx context.Context) ([]byte, error)
	// KeyID は鍵の参照（SecretRef の文字列。値ではない）。接続の使い回しの
	// 区別に使う。
	KeyID string
	// Sudo なら argv の前に "sudo -n --" を付ける。
	Sudo bool
	// ConnectTimeout（0 は DefaultConnectTimeout）・Timeout（コマンド 1 回の
	// 上限、0 は DefaultTimeout）・ControlPersist（0 は DefaultControlPersist、
	// 負なら使い回さない）。
	ConnectTimeout time.Duration
	Timeout        time.Duration
	ControlPersist time.Duration
	// SSH は ssh のパス（既定 "ssh"）。
	SSH string
	// ControlDir は master のソケットと一時の鍵を置くディレクトリ（既定は
	// DefaultControlDir()）。
	ControlDir string
}

// SSH は ssh 越しにコマンドを流す Executor。
type SSH struct {
	h SSHHost

	failMu   sync.Mutex
	failErr  error
	failedAt time.Time
}

// NewSSH は h の Executor を作る。設定の形が違えば error。
func NewSSH(h SSHHost) (*SSH, error) {
	if h.Name == "" {
		return nil, errors.New("remote: a host needs a name")
	}
	if h.Address == "" || strings.HasPrefix(h.Address, "-") || strings.ContainsAny(h.Address, " \t\r\n@") {
		return nil, fmt.Errorf("remote %s: address %q: want a host name or an IP address", h.Name, h.Address)
	}
	if strings.HasPrefix(h.User, "-") || strings.ContainsAny(h.User, " \t\r\n@") {
		return nil, fmt.Errorf("remote %s: user %q is not a user name", h.Name, h.User)
	}
	if h.Port < 0 || h.Port > 65535 {
		return nil, fmt.Errorf("remote %s: port %d", h.Name, h.Port)
	}
	if h.KnownHosts == "" {
		return nil, fmt.Errorf("remote %s: known_hosts is required (host keys are never accepted unchecked)", h.Name)
	}
	if h.ConnectTimeout <= 0 {
		h.ConnectTimeout = DefaultConnectTimeout
	}
	if h.Timeout <= 0 {
		h.Timeout = DefaultTimeout
	}
	if h.ControlPersist == 0 {
		h.ControlPersist = DefaultControlPersist
	}
	if h.SSH == "" {
		h.SSH = "ssh"
	}
	return &SSH{h: h}, nil
}

// Target implements Executor.
func (s *SSH) Target() string { return s.h.Name }

// Run implements Executor.
func (s *SSH) Run(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
	line, err := s.remoteCommand(argv)
	if err != nil {
		return nil, err
	}
	ctx, cancel := withTimeout(ctx, s.h.Timeout)
	defer cancel()
	if err := s.recentFailure(); err != nil {
		return nil, err
	}
	args, done, err := s.prepare(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	cmd := exec.CommandContext(ctx, s.h.SSH, append(args, line)...)
	out, err := runCmd(ctx, cmd, stdin)
	return out, s.classify(err)
}

// Hold implements Executor.
func (s *SSH) Hold(ctx context.Context, argv []string) (func() error, error) {
	line, err := s.remoteCommand(argv)
	if err != nil {
		return nil, err
	}
	if err := s.recentFailure(); err != nil {
		return nil, err
	}
	args, done, err := s.prepare(ctx)
	if err != nil {
		return nil, err
	}
	release, err := holdCmd(ctx, exec.Command(s.h.SSH, append(args, line)...))
	done()
	if err != nil {
		return nil, s.classify(err)
	}
	return func() error { return s.classify(release()) }, nil
}

// Probe は master に相乗りせず新しい ssh の接続を張り、相手で true を走らせる
// （Backlog #173：FW を変えた後に、新しい接続で届くかを確かめる）。sudo は
// 付けない。前の失敗で新しい接続を控えることはせず、この失敗も記録しない
// （確かめは何度も張り直す。ほかの呼び出しを止めない）。届かない・認証の失敗・
// ホスト鍵の不一致は *ConnError。
func (s *SSH) Probe(ctx context.Context) error {
	return s.probe(ctx, s)
}

// ProbeVia は Probe と同じ確かめを、別の経路（address・port・user。空・0 は
// この対象の設定のまま）で行う（Backlog #179：宣言した別の経路の ssh で到達を
// 確かめる）。鍵・known_hosts・タイムアウトはこの対象の設定を使う（ホスト鍵は
// その経路の名前で known_hosts と照らす）。
func (s *SSH) ProbeVia(ctx context.Context, user, address string, port int) error {
	h := s.h
	if address != "" {
		h.Address = address
	}
	if port != 0 {
		h.Port = port
	}
	if user != "" {
		h.User = user
	}
	via, err := NewSSH(h)
	if err != nil {
		return err
	}
	return s.probe(ctx, via)
}

// probe は via の設定で master に相乗りしない新しい接続を張り、true を走らせる。
func (s *SSH) probe(ctx context.Context, via *SSH) error {
	ctx, cancel := withTimeout(ctx, via.h.Timeout)
	defer cancel()
	keyPath, done, err := via.writeKey(ctx)
	if err != nil {
		return err
	}
	defer done()
	args, err := via.baseArgs(keyPath)
	if err != nil {
		return err
	}
	args = append(args, "-o", "ControlMaster=no", "-o", "ControlPath=none", "--", via.h.Address, "true")
	_, err = runCmd(ctx, exec.CommandContext(ctx, via.h.SSH, args...), nil)
	return connError(s.h.Name, err)
}

// remoteCommand は argv（sudo なら前に sudo -n --）を引用した 1 本の文字列にする。
func (s *SSH) remoteCommand(argv []string) (string, error) {
	if s.h.Sudo {
		argv = append([]string{"sudo", "-n", "--"}, argv...)
	}
	line, err := ShellCommand(argv)
	if err != nil {
		return "", fmt.Errorf("remote %s: %w", s.h.Name, err)
	}
	return line, nil
}

// prepare は ssh の引数（宛先まで。後ろにコマンドを足す）を作り、要れば master
// を立てる。done は一時の鍵を消す。
func (s *SSH) prepare(ctx context.Context) ([]string, func(), error) {
	keyPath, done, err := s.writeKey(ctx)
	if err != nil {
		return nil, nil, err
	}
	args, err := s.baseArgs(keyPath)
	if err != nil {
		done()
		return nil, nil, err
	}
	if s.h.ControlPersist > 0 {
		sock, err := s.controlPath()
		if err != nil {
			done()
			return nil, nil, err
		}
		if err := s.ensureMaster(ctx, args, sock); err != nil {
			done()
			return nil, nil, err
		}
		args = append(args, "-o", "ControlMaster=no", "-o", "ControlPath="+sock)
	}
	return append(args, "--", s.h.Address), done, nil
}

// baseArgs は全部の呼び出しに付ける ssh の引数。
func (s *SSH) baseArgs(keyPath string) ([]string, error) {
	strict := "yes"
	if s.h.TrustOnFirstUse {
		strict = "accept-new"
	}
	secs := int((s.h.ConnectTimeout + time.Second - 1) / time.Second)
	args := []string{
		"-F", os.DevNull,
		"-T",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=" + strict,
		"-o", "UserKnownHostsFile=" + s.h.KnownHosts,
		"-o", "GlobalKnownHostsFile=" + os.DevNull,
		"-o", "UpdateHostKeys=no",
		"-o", "ConnectTimeout=" + strconv.Itoa(secs),
		"-o", "ServerAliveInterval=" + strconv.Itoa(secs),
		"-o", "ServerAliveCountMax=3",
		"-o", "PasswordAuthentication=no",
		"-o", "KbdInteractiveAuthentication=no",
		"-o", "LogLevel=ERROR",
	}
	if s.h.Port != 0 {
		args = append(args, "-p", strconv.Itoa(s.h.Port))
	}
	if s.h.User != "" {
		args = append(args, "-l", s.h.User)
	}
	if keyPath != "" {
		args = append(args, "-i", keyPath, "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none")
	}
	return args, nil
}

// writeKey は秘密鍵を 0600 の一時ファイルに出す。done はそれを消す。鍵を
// 設定していなければ "" を返す（ssh-agent）。
func (s *SSH) writeKey(ctx context.Context) (string, func(), error) {
	if s.h.Key == nil {
		return "", func() {}, nil
	}
	key, err := s.h.Key(ctx)
	if err != nil {
		// The resolver's error names the ref, never the value.
		return "", nil, fmt.Errorf("remote %s: private key: %w", s.h.Name, err)
	}
	if len(key) > 0 && key[len(key)-1] != '\n' {
		key = append(key, '\n')
	}
	dir, err := s.controlDir()
	if err != nil {
		return "", nil, err
	}
	f, err := os.CreateTemp(dir, "key-*")
	if err != nil {
		return "", nil, fmt.Errorf("remote %s: private key: %w", s.h.Name, err)
	}
	path := f.Name()
	done := func() { _ = os.Remove(path) }
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		done()
		return "", nil, fmt.Errorf("remote %s: private key: %w", s.h.Name, err)
	}
	_, werr := f.Write(key)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		done()
		return "", nil, fmt.Errorf("remote %s: private key: could not write the temporary key file", s.h.Name)
	}
	return path, done, nil
}

func (s *SSH) controlDir() (string, error) {
	dir := s.h.ControlDir
	if dir == "" {
		dir = DefaultControlDir()
	}
	if err := ensurePrivateDir(dir); err != nil {
		return "", fmt.Errorf("remote %s: control dir: %w", s.h.Name, err)
	}
	return dir, nil
}

// controlPath は対象の master のソケット。つなぐ先・ホスト鍵の確かめ方・鍵の
// 参照が同じなら（プロセスをまたいでも）同じソケットを使う。
func (s *SSH) controlPath() (string, error) {
	dir, err := s.controlDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		s.h.User, s.h.Address, strconv.Itoa(s.h.Port), s.h.KnownHosts,
		strconv.FormatBool(s.h.TrustOnFirstUse), s.h.KeyID,
	}, "\x00")))
	sock := filepath.Join(dir, "cm-"+hex.EncodeToString(sum[:8]))
	// ssh binds "<ControlPath>.<16 random chars>" first; a unix socket path
	// has room for about 104 bytes.
	if len(sock)+17 > maxSocketPath {
		return "", fmt.Errorf("remote %s: the control socket %s is too long for a unix socket: set control_dir to a shorter path (or control_persist: 0s)", s.h.Name, sock)
	}
	return sock, nil
}

// maxSocketPath は unix ソケットのパスの上限（sun_path。小さい方の 104 に
// 合わせる）。
const maxSocketPath = 103

// ensureMaster は sock に生きた master が無ければ立てる。
func (s *SSH) ensureMaster(ctx context.Context, args []string, sock string) error {
	mu := masterLock(sock)
	mu.Lock()
	defer mu.Unlock()
	if alive(sock) {
		return nil
	}
	_ = os.Remove(sock) // stale
	dir, err := s.controlDir()
	if err != nil {
		return err
	}
	// The backgrounded master keeps the stderr it was started with: give it a
	// file (not a pipe) so that the start returns as soon as ssh -f does.
	errf, err := os.CreateTemp(dir, "master-*.err")
	if err != nil {
		return fmt.Errorf("remote %s: %w", s.h.Name, err)
	}
	defer os.Remove(errf.Name())
	defer errf.Close()
	persist := int((s.h.ControlPersist + time.Second - 1) / time.Second)
	margs := append(append([]string(nil), args...),
		"-o", "ControlMaster=yes",
		"-o", "ControlPath="+sock,
		"-o", "ControlPersist="+strconv.Itoa(persist),
		"-f", "-N", "--", s.h.Address)
	cctx, cancel := context.WithTimeout(ctx, s.h.ConnectTimeout+5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, s.h.SSH, margs...)
	cmd.Stdout = nil
	cmd.Stderr = errf
	runErr := cmd.Run()
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		msg, _ := os.ReadFile(errf.Name())
		return s.classify(&ExitError{Code: 255, Stderr: string(msg)})
	}
	if runErr != nil {
		err := &ConnError{Target: s.h.Name, Kind: Unreachable, Detail: runErr.Error()}
		s.noteFailure(err)
		return err
	}
	return nil
}

// masterLocks はソケットごとの master の立ち上げを直列化する（同じ対象の
// Executor が 2 つあっても master は 1 本）。
var masterLocks sync.Map // string → *sync.Mutex

func masterLock(sock string) *sync.Mutex {
	mu, _ := masterLocks.LoadOrStore(sock, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// alive は sock に master が応えるかどうか。
func alive(sock string) bool {
	c, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// classify は ssh の終わり方を分ける: 255 で終わったのは ssh 自身の失敗
// （*ConnError）、それ以外はそのまま（リモートのコマンドの終了）。
func (s *SSH) classify(err error) error {
	err = connError(s.h.Name, err)
	var ce *ConnError
	if errors.As(err, &ce) {
		s.noteFailure(ce)
	}
	return err
}

// connError は ssh が 255 で終わったのを *ConnError にする（それ以外はそのまま）。
func connError(target string, err error) error {
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 255 {
		return err
	}
	ce := &ConnError{Target: target, Kind: Unreachable, Detail: firstLines(ee.Stderr)}
	switch msg := ee.Stderr; {
	case strings.Contains(msg, "Host key verification failed"),
		strings.Contains(msg, "REMOTE HOST IDENTIFICATION HAS CHANGED"),
		strings.Contains(msg, "host key is known"),
		strings.Contains(msg, "Host key for"):
		ce.Kind = HostKeyMismatch
	case strings.Contains(msg, "Permission denied"),
		strings.Contains(msg, "Too many authentication failures"),
		strings.Contains(msg, "Load key"):
		ce.Kind = AuthFailed
	}
	return ce
}

func (s *SSH) noteFailure(err error) {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	s.failErr, s.failedAt = err, time.Now()
}

// recentFailure は ConnectTimeout より前でない失敗を返す（その間は新しく
// つながない）。
func (s *SSH) recentFailure() error {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	if s.failErr == nil || time.Since(s.failedAt) >= s.h.ConnectTimeout {
		return nil
	}
	return s.failErr
}

// firstLines は ssh の標準エラーの頭（空行を除く 3 行、300 文字まで）。
func firstLines(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.Trim(l, "@ ") == "" {
			continue
		}
		out = append(out, l)
		if len(out) == 3 {
			break
		}
	}
	msg := strings.Join(out, "; ")
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return msg
}

// Close は master を止める（立っていなければ何もしない）。
func (s *SSH) Close() error {
	if s.h.ControlPersist <= 0 {
		return nil
	}
	sock, err := s.controlPath()
	if err != nil || !alive(sock) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.h.SSH, "-F", os.DevNull, "-o", "ControlPath="+sock, "-O", "exit", "--", s.h.Address)
	return cmd.Run()
}

// DefaultControlDir は master のソケットと一時の鍵を置く既定のディレクトリ:
// $XDG_RUNTIME_DIR/sakutei-ssh、無ければ <一時ディレクトリ>/sakutei-ssh-<uid>。
func DefaultControlDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "sakutei-ssh")
	}
	return filepath.Join(os.TempDir(), "sakutei-ssh-"+strconv.Itoa(os.Getuid()))
}

// ensurePrivateDir は dir を 0700 で作る（あれば、自分の uid の 0700 の
// ディレクトリであることを確かめる。ほかの利用者が鍵やソケットを覗ける・
// 差し替えられる場所は使わない）。
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if fi.Mode().Perm() != 0o700 {
		return fmt.Errorf("%s must be mode 0700 (it is %04o)", dir, fi.Mode().Perm())
	}
	return checkOwner(dir, fi)
}
