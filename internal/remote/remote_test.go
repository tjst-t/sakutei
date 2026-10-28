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

package remote_test

// remote_test.go — 実行の層（Backlog #130）を、手元の sh と偽の ssh で確かめる。
// 本物の sshd は tests/integration/remote（make integration-remote）。
//
// Tags:
//   [AC-130-1-1] 引用: どんな引数もリモートのシェルでそのまま 1 語に戻る（注入させない）
//   [AC-130-1-2] ssh の引数: BatchMode・known_hosts・ConnectTimeout・使い回し・sudo
//   [AC-130-1-3] 秘密鍵: 0600 の一時ファイルで渡し、終われば消える。引数・エラーに値を出さない
//   [AC-130-1-4] 失敗の分け方: 届かない・認証・ホスト鍵（永続）・コマンドの終了
//   [AC-130-1-5] 対象の一覧の読み込み

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/remote"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// nasty は引用で守るべき引数。
var nasty = []string{
	"", " ", "plain", "a b", "$(touch /tmp/sakutei-pwned)", "`id`", "; rm -rf /", "x' ; echo pwned '",
	`"double"`, "back\\slash", "new\nline", "tab\there", "*", "?", "~", "~root", "=ls", "%1", "!!", "$HOME",
	"a&&b", "a|b", "a>b", "<x", "{a,b}", "#comment", "'", "''", "-n", "--", "é ü 日本", "\x01ctl",
}

// argvFromShell runs the quoted command line through `shell -c` with printf
// as the command and returns the words the shell handed to printf.
func argvFromShell(t *testing.T, shell string, args []string) []string {
	t.Helper()
	line, err := remote.ShellCommand(append([]string{"printf", `%s\0`}, args...))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(shell, "-c", line).Output()
	if err != nil {
		t.Fatalf("%s -c %q: %v", shell, line, err)
	}
	words := strings.Split(string(out), "\x00")
	return words[:len(words)-1]
}

func TestQuote_RoundTrip(t *testing.T) {
	// [AC-130-1-1]
	for _, shell := range []string{"sh", "bash", "zsh", "dash"} {
		if _, err := exec.LookPath(shell); err != nil {
			continue
		}
		t.Run(shell, func(t *testing.T) {
			got := argvFromShell(t, shell, nasty)
			if len(got) != len(nasty) {
				t.Fatalf("[AC-130-1-1] %d words back, want %d: %q", len(got), len(nasty), got)
			}
			for i := range nasty {
				if got[i] != nasty[i] {
					t.Errorf("[AC-130-1-1] word %d = %q, want %q", i, got[i], nasty[i])
				}
			}
		})
	}
	if _, err := os.Stat("/tmp/sakutei-pwned"); err == nil {
		t.Error("[AC-130-1-1] an argument was run as a command")
	}
	if _, err := remote.ShellCommand([]string{"a\x00b"}); err == nil {
		t.Error("[AC-130-1-1] a NUL byte must be refused")
	}
	if remote.Quote("inet/filter") != "inet/filter" || remote.Quote("=x") == "=x" {
		t.Error("[AC-130-1-1] plain words stay as they are; a leading '=' is quoted")
	}
}

func TestLocal_RunAndHold(t *testing.T) {
	ctx := context.Background()
	l := remote.Local{}
	out, err := l.Run(ctx, []string{"sh", "-c", "cat; echo \"$1\"", "sh", "a b"}, []byte("in\n"))
	if err != nil || string(out) != "in\na b\n" {
		t.Fatalf("Run = %q, %v", out, err)
	}
	_, err = l.Run(ctx, []string{"sh", "-c", "echo boom >&2; exit 3"}, nil)
	var ee *remote.ExitError
	if !errors.As(err, &ee) || ee.Code != 3 || !strings.Contains(ee.Stderr, "boom") {
		t.Fatalf("Run exit 3 = %v", err)
	}
	if _, err := l.Run(ctx, []string{"/nonexistent/cmd"}, nil); err == nil || errors.As(err, &ee) {
		t.Fatalf("a missing command must be a non-exit error: %v", err)
	}

	// Hold: ok after the first line, released by closing stdin.
	dir := t.TempDir()
	release, err := l.Hold(ctx, []string{"sh", "-c", `echo ok; cat >/dev/null; touch "$1"`, "sh", filepath.Join(dir, "released")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "released")); err == nil {
		t.Fatal("released before release()")
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "released")); err != nil {
		t.Fatal("release() did not end the command")
	}
	_, err = l.Hold(ctx, []string{"sh", "-c", "echo nope >&2; exit 4"})
	if !errors.As(err, &ee) || ee.Code != 4 {
		t.Fatalf("Hold that ends before ok = %v", err)
	}
}

// fakeSSH writes a fake ssh into dir: it records its arguments, the key file
// it was given (mode and content), and then runs the last argument with sh
// (as sshd would hand it to the user's shell).  FAKESSH_FAIL makes it fail
// like ssh does.
const fakeSSH = `#!/bin/sh
log=$FAKESSH_LOG
key=
prev=
for a; do
  printf '%s\0' "$a" >> "$log.args"
  [ "$prev" = "-i" ] && key=$a
  prev=$a
  last=$a
done
printf 'END\0' >> "$log.args"
if [ -n "$key" ]; then
  stat -c '%a' "$key" >> "$log.key"
  cat "$key" >> "$log.key"
fi
case "$FAKESSH_FAIL" in
  hostkey) echo "No ED25519 host key is known for [192.0.2.1]:22 and you have requested strict checking." >&2; echo "Host key verification failed." >&2; exit 255;;
  changed) printf '@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@\n@    WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!     @\n' >&2; echo "Host key verification failed." >&2; exit 255;;
  auth) echo "sakutei@192.0.2.1: Permission denied (publickey)." >&2; exit 255;;
  down) echo "ssh: connect to host 192.0.2.1 port 22: Connection refused" >&2; exit 255;;
esac
for a; do case "$a" in -N) exit 0;; esac; done
exec sh -c "$last"
`

type fake struct {
	dir, log string
	// ctrl is the control dir: short, as a unix socket's path is limited.
	ctrl string
}

func newFake(t *testing.T) fake {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(fakeSSH), 0o755); err != nil {
		t.Fatal(err)
	}
	ctrl, err := os.MkdirTemp("/tmp", "srt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(ctrl) })
	f := fake{dir: dir, log: filepath.Join(dir, "log"), ctrl: ctrl}
	t.Setenv("FAKESSH_LOG", f.log)
	t.Setenv("FAKESSH_FAIL", "")
	return f
}

func (f fake) host(t *testing.T, mut func(*remote.SSHHost)) *remote.SSH {
	t.Helper()
	h := remote.SSHHost{
		Name: "fw1", Address: "192.0.2.1", Port: 2222, User: "sakutei",
		KnownHosts: filepath.Join(f.dir, "known_hosts"), SSH: filepath.Join(f.dir, "ssh"),
		ControlDir: f.ctrl, ControlPersist: -1,
	}
	if mut != nil {
		mut(&h)
	}
	s, err := remote.NewSSH(h)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// calls returns the argument lists of the fake's calls.
func (f fake) calls(t *testing.T) [][]string {
	t.Helper()
	b, err := os.ReadFile(f.log + ".args")
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	var cur []string
	for _, a := range strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00") {
		if a == "END" {
			out = append(out, cur)
			cur = nil
			continue
		}
		cur = append(cur, a)
	}
	return out
}

func hasPair(args []string, k, v string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == k && args[i+1] == v {
			return true
		}
	}
	return false
}

func TestSSH_ArgsAndRun(t *testing.T) {
	// [AC-130-1-2]
	f := newFake(t)
	s := f.host(t, func(h *remote.SSHHost) { h.Sudo = false })
	out, err := s.Run(context.Background(), append([]string{"printf", `%s\0`}, nasty...), nil)
	if err != nil {
		t.Fatal(err)
	}
	words := strings.Split(string(out), "\x00")
	if got := words[:len(words)-1]; strings.Join(got, "|") != strings.Join(nasty, "|") {
		t.Fatalf("[AC-130-1-1] through ssh the words are %q", got)
	}
	// stdin carries the content.
	out, err = s.Run(context.Background(), []string{"cat"}, []byte("table inet filter\n"))
	if err != nil || string(out) != "table inet filter\n" {
		t.Fatalf("stdin: %q, %v", out, err)
	}
	calls := f.calls(t)
	if len(calls) != 2 {
		t.Fatalf("%d calls, want 2: %q", len(calls), calls)
	}
	args := calls[1]
	for _, want := range [][2]string{
		{"-F", os.DevNull}, {"-o", "BatchMode=yes"}, {"-o", "StrictHostKeyChecking=yes"},
		{"-o", "UserKnownHostsFile=" + filepath.Join(f.dir, "known_hosts")},
		{"-o", "GlobalKnownHostsFile=" + os.DevNull}, {"-o", "ConnectTimeout=10"},
		{"-p", "2222"}, {"-l", "sakutei"}, {"--", "192.0.2.1"},
	} {
		if !hasPair(args, want[0], want[1]) {
			t.Errorf("[AC-130-1-2] missing %s %s in %q", want[0], want[1], args)
		}
	}
	if args[len(args)-1] != "cat" {
		t.Errorf("[AC-130-1-2] the remote command is %q", args[len(args)-1])
	}
	for _, a := range args {
		if strings.HasPrefix(a, "ControlPath=") || a == "-i" {
			t.Errorf("[AC-130-1-2] unexpected %q (no reuse, no key configured)", a)
		}
	}
}

func TestSSH_SudoTOFUAndReuse(t *testing.T) {
	// [AC-130-1-2]
	f := newFake(t)
	s := f.host(t, func(h *remote.SSHHost) {
		h.Sudo = true
		h.TrustOnFirstUse = true
		h.ControlPersist = 30 * time.Second
	})
	// The fake has no sudo: check the command line it is given instead.
	if _, err := s.Run(context.Background(), []string{"true"}, nil); err == nil {
		// sudo may exist on the test machine; either way the line is checked below.
		_ = err
	}
	calls := f.calls(t)
	if len(calls) != 2 {
		t.Fatalf("%d calls, want the master and the command: %q", len(calls), calls)
	}
	master, run := calls[0], calls[1]
	if !hasPair(master, "-o", "ControlMaster=yes") || !hasPair(master, "-o", "ControlPersist=30") {
		t.Errorf("[AC-130-1-2] master: %q", master)
	}
	if !hasPair(run, "-o", "ControlMaster=no") || !hasPair(run, "-o", "StrictHostKeyChecking=accept-new") {
		t.Errorf("[AC-130-1-2] run: %q", run)
	}
	if got := run[len(run)-1]; got != "sudo -n -- true" {
		t.Errorf("[AC-130-1-2] sudo: the remote command is %q", got)
	}
	fi, err := os.Stat(f.ctrl)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("[AC-130-1-2] control dir: %v %v", fi, err)
	}
	// A control dir others can read is refused.
	open := filepath.Join(f.dir, "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	s2 := f.host(t, func(h *remote.SSHHost) { h.ControlDir = open; h.ControlPersist = time.Minute })
	if _, err := s2.Run(context.Background(), []string{"true"}, nil); err == nil || !strings.Contains(err.Error(), "0700") {
		t.Errorf("[AC-130-1-2] a 0755 control dir must be refused: %v", err)
	}
}

const secretKey = "-----BEGIN OPENSSH PRIVATE KEY-----\nc2VjcmV0LWtleS1ib2R5\n-----END OPENSSH PRIVATE KEY-----"

func TestSSH_KeyFile(t *testing.T) {
	// [AC-130-1-3]
	f := newFake(t)
	s := f.host(t, func(h *remote.SSHHost) {
		h.Key = func(context.Context) ([]byte, error) { return []byte(secretKey), nil }
		h.KeyID = "secretref:file/ssh/fw1#gen0"
	})
	if _, err := s.Run(context.Background(), []string{"true"}, nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(f.log + ".key")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), "600\n"+secretKey+"\n") {
		t.Errorf("[AC-130-1-3] the key file was %q; want mode 600 and the key with a final newline", b)
	}
	args := f.calls(t)[0]
	for _, a := range args {
		if strings.Contains(a, "c2VjcmV0") {
			t.Fatalf("[AC-130-1-3] the key is in the arguments: %q", a)
		}
	}
	if !hasPair(args, "-o", "IdentitiesOnly=yes") {
		t.Errorf("[AC-130-1-3] %q", args)
	}
	left, _ := filepath.Glob(filepath.Join(f.ctrl, "key-*"))
	if len(left) != 0 {
		t.Errorf("[AC-130-1-3] the key file is left behind: %v", left)
	}
	// A failing call does not leave it either, and the error does not quote it.
	t.Setenv("FAKESSH_FAIL", "auth")
	_, err = s.Run(context.Background(), []string{"true"}, nil)
	if err == nil || strings.Contains(err.Error(), "c2VjcmV0") {
		t.Errorf("[AC-130-1-3] %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(f.ctrl, "key-*")); len(left) != 0 {
		t.Errorf("[AC-130-1-3] the key file is left behind after a failure: %v", left)
	}
}

func TestSSH_Failures(t *testing.T) {
	// [AC-130-1-4]
	for mode, want := range map[string]remote.ConnKind{
		"hostkey": remote.HostKeyMismatch,
		"changed": remote.HostKeyMismatch,
		"auth":    remote.AuthFailed,
		"down":    remote.Unreachable,
	} {
		t.Run(mode, func(t *testing.T) {
			f := newFake(t)
			s := f.host(t, nil)
			t.Setenv("FAKESSH_FAIL", mode)
			_, err := s.Run(context.Background(), []string{"true"}, nil)
			var ce *remote.ConnError
			if !errors.As(err, &ce) || ce.Kind != want || ce.Target != "fw1" {
				t.Fatalf("[AC-130-1-4] %s: %v", mode, err)
			}
			perr := remote.ProviderError(err)
			var pe *provider.PermanentError
			if (want == remote.HostKeyMismatch) != errors.As(perr, &pe) {
				t.Errorf("[AC-130-1-4] %s: ProviderError = %T; host key mismatch (only) is permanent", mode, perr)
			}
			// Within ConnectTimeout a failed host is not dialled again.
			n := len(f.calls(t))
			t.Setenv("FAKESSH_FAIL", "")
			if _, err := s.Run(context.Background(), []string{"true"}, nil); !errors.As(err, &ce) {
				t.Errorf("[AC-130-1-4] the recent failure must be returned: %v", err)
			}
			if len(f.calls(t)) != n {
				t.Errorf("[AC-130-1-4] a failed host was dialled again at once")
			}
		})
	}
	// A command that runs and fails is an ExitError, not a ConnError.
	f := newFake(t)
	s := f.host(t, nil)
	_, err := s.Run(context.Background(), []string{"sh", "-c", "echo no >&2; exit 2"}, nil)
	var ee *remote.ExitError
	if !errors.As(err, &ee) || ee.Code != 2 || !strings.Contains(ee.Stderr, "no") {
		t.Fatalf("[AC-130-1-4] %v", err)
	}
	if _, ok := remote.ProviderError(err).(*provider.TransientError); !ok {
		t.Errorf("[AC-130-1-4] a failed command is transient")
	}
	// Hold through ssh.
	release, err := s.Hold(context.Background(), []string{"sh", "-c", "echo ok; cat >/dev/null"})
	if err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKESSH_FAIL", "down")
	s2 := f.host(t, nil)
	_, err = s2.Hold(context.Background(), []string{"sh", "-c", "echo ok; cat"})
	var ce *remote.ConnError
	if !errors.As(err, &ce) || ce.Kind != remote.Unreachable {
		t.Fatalf("[AC-130-1-4] Hold on a down host: %v", err)
	}
}

func TestNewSSH_Refuses(t *testing.T) {
	base := remote.SSHHost{Name: "a", Address: "h", KnownHosts: "/k"}
	for name, mut := range map[string]func(*remote.SSHHost){
		"no known_hosts": func(h *remote.SSHHost) { h.KnownHosts = "" },
		"option address": func(h *remote.SSHHost) { h.Address = "-oProxyCommand=x" },
		"user@ address":  func(h *remote.SSHHost) { h.Address = "root@h" },
		"option user":    func(h *remote.SSHHost) { h.User = "-x" },
	} {
		h := base
		mut(&h)
		if _, err := remote.NewSSH(h); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	// [AC-130-1-5]
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("secrets.json", `{"secrets": {"ssh/fw1": {"value": "`+strings.ReplaceAll(secretKey, "\n", `\n`)+`", "generation": 1}}}`)
	good := write("hosts.yaml", `
secret_store: secrets.json
known_hosts: known_hosts
hosts:
  fw1:
    address: 192.0.2.11
    port: 2222
    user: sakutei
    private_key: secretref:file/ssh/fw1
    sudo: true
    connect_timeout: 3s
    control_persist: 0s
  fw2:
    address: 192.0.2.12
    known_hosts: /etc/sakutei/known_hosts
    trust_on_first_use: true
`)
	cfg, err := remote.LoadConfig(good)
	if err != nil {
		t.Fatal(err)
	}
	h := cfg.Hosts["fw1"]
	if h.Address != "192.0.2.11" || h.Port != 2222 || !h.Sudo || h.ConnectTimeout != 3*time.Second || h.ControlPersist >= 0 ||
		h.KnownHosts != filepath.Join(dir, "known_hosts") || h.Key == nil {
		t.Errorf("[AC-130-1-5] fw1 = %+v", h)
	}
	key, err := h.Key(context.Background())
	if err != nil || !bytes.Equal(key, []byte(secretKey)) {
		t.Errorf("[AC-130-1-5] key: %v", err)
	}
	if h2 := cfg.Hosts["fw2"]; h2.KnownHosts != "/etc/sakutei/known_hosts" || !h2.TrustOnFirstUse || h2.Key != nil {
		t.Errorf("[AC-130-1-5] fw2 = %+v", h2)
	}
	ex, order, err := cfg.Executors("fw2, fw1")
	if err != nil || len(ex) != 2 || strings.Join(order, ",") != "fw2,fw1" || ex["fw1"].Target() != "fw1" {
		t.Errorf("[AC-130-1-5] Executors = %v %v %v", ex, order, err)
	}
	for _, names := range []string{"", "fw3", "fw1,fw1"} {
		if _, _, err := cfg.Executors(names); err == nil {
			t.Errorf("[AC-130-1-5] Executors(%q) must fail", names)
		}
	}

	for name, body := range map[string]string{
		"raw key":        "known_hosts: k\nsecret_store: secrets.json\nhosts:\n  a:\n    address: h\n    private_key: \"-----BEGIN OPENSSH PRIVATE KEY-----\"\n",
		"missing ref":    "known_hosts: k\nsecret_store: secrets.json\nhosts:\n  a:\n    address: h\n    private_key: secretref:file/ssh/none\n",
		"no store":       "known_hosts: k\nhosts:\n  a:\n    address: h\n    private_key: secretref:file/ssh/fw1\n",
		"no known_hosts": "hosts:\n  a:\n    address: h\n",
		"no address":     "known_hosts: k\nhosts:\n  a:\n    user: x\n",
		"unknown key":    "known_hosts: k\nhosts:\n  a:\n    address: h\n    passwd: x\n",
		"bad name":       "known_hosts: k\nhosts:\n  A/b:\n    address: h\n",
		"bad port":       "known_hosts: k\nhosts:\n  a:\n    address: h\n    port: 70000\n",
		"no hosts":       "known_hosts: k\n",
	} {
		_, err := remote.LoadConfig(write("bad.yaml", body))
		if err == nil {
			t.Errorf("[AC-130-1-5] %s must be an error", name)
			continue
		}
		if strings.Contains(err.Error(), "BEGIN OPENSSH") {
			t.Errorf("[AC-130-1-5] %s: the error quotes the key: %v", name, err)
		}
	}
}

func TestSSH_ControlPathTooLong(t *testing.T) {
	// [AC-130-1-2]
	f := newFake(t)
	long := filepath.Join(f.dir, strings.Repeat("d", 100))
	s := f.host(t, func(h *remote.SSHHost) { h.ControlDir = long; h.ControlPersist = time.Minute })
	if _, err := s.Run(context.Background(), []string{"true"}, nil); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Errorf("[AC-130-1-2] a control socket path too long for a unix socket must be a clear error: %v", err)
	}
}

func TestSSH_ProbeOpensANewConnection(t *testing.T) {
	// [AC-173-4-1] Probe は master に相乗りせず（ControlPath=none）新しい接続で
	// true を走らせ、sudo を付けない。届かないのは *ConnError で、その失敗は
	// ほかの呼び出しを控えさせない。
	f := newFake(t)
	s := f.host(t, func(h *remote.SSHHost) {
		h.Sudo = true
		h.ControlPersist = 30 * time.Second
	})
	if err := s.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := f.calls(t)
	if len(calls) != 1 {
		t.Fatalf("%d calls, want only the probe (no master): %q", len(calls), calls)
	}
	args := calls[0]
	if !hasPair(args, "-o", "ControlPath=none") || !hasPair(args, "-o", "ControlMaster=no") || args[len(args)-1] != "true" {
		t.Errorf("probe: %q", args)
	}
	t.Setenv("FAKESSH_FAIL", "down")
	var ce *remote.ConnError
	if err := s.Probe(context.Background()); !errors.As(err, &ce) || ce.Kind != remote.Unreachable {
		t.Fatalf("probe of a host that is down = %v", err)
	}
	t.Setenv("FAKESSH_FAIL", "")
	if err := s.Probe(context.Background()); err != nil {
		t.Errorf("a probe right after a failed one is not tried again: %v", err)
	}
}
