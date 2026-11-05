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

// Package hostexec は provider SDK の「ホストでコマンドを実行する層」（設計 §6.1、
// E-E1・N-E1）。手元で実行する Local と、agent を置かずに ssh で離れたホストに
// 入る SSH を、同じ interface Runner で使う。
//
// ルール：
//
//   - 引数は argv で渡し、離れたホストではシェルの引用符で 1 つずつ囲む（呼び出し側
//     の値がシェルのコマンドとして解釈されない）。
//   - ssh はホスト鍵を known_hosts で検証し、知らない鍵は拒否する
//     （StrictHostKeyChecking yes、BatchMode）。known_hosts と鍵のファイルは必須（E-E1）。
//   - sudo は `sudo -n` で、パスワードを聞かない。実行するのは provider が決めた
//     コマンドだけ（nftables は専用ツール sakutei-nft-guard だけ）。
//   - 終了コードが 0 でなければ *ExitError（コマンドは実行された）。実行できない・
//     ssh が届かない（ssh の終了コード 255）は *UnreachableError。provider は
//     届かないとき Absent でなく Unknown を返す（不変条件 3）。
package hostexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Runner はホストでコマンドを 1 つ実行する。
type Runner interface {
	Run(ctx context.Context, argv []string, stdin []byte) ([]byte, error)
	// Name はホストの名前（設定の hosts のキー）。
	Name() string
}

// ExitError はコマンドが 0 以外の終了コードで終わったことを表す。
type ExitError struct {
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if len(msg) > 400 {
		msg = msg[:400] + "…"
	}
	return fmt.Sprintf("終了コード %d: %s", e.Code, msg)
}

// UnreachableError はコマンドを実行できなかったことを表す（届かない・ssh が落ちた）。
type UnreachableError struct {
	Host string
	Err  error
}

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("ホスト %s に届かない: %v", e.Host, e.Err)
}

func (e *UnreachableError) Unwrap() error { return e.Err }

// IsUnreachable は err が「届かない」を表すかを返す。
func IsUnreachable(err error) bool {
	var u *UnreachableError
	return errors.As(err, &u)
}

// DefaultTimeout は 1 つのコマンドのタイムアウト。
const DefaultTimeout = 60 * time.Second

// Local は手元で実行する。Prefix は argv の前に足す（例 sudo -n、ip netns exec）。
type Local struct {
	HostName string
	Prefix   []string
	Sudo     bool
}

// Name はホストの名前。
func (l Local) Name() string { return l.HostName }

// Run は argv を実行する。
func (l Local) Run(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
	full := append(append([]string(nil), l.Prefix...), argv...)
	if l.Sudo {
		full = append([]string{"sudo", "-n"}, full...)
	}
	return run(ctx, l.HostName, full, stdin, false)
}

// SSH は ssh で離れたホストに入って実行する。
type SSH struct {
	HostName   string
	Address    string
	User       string
	Port       int
	Key        string // 秘密鍵のファイル
	KnownHosts string // known_hosts のファイル
	Sudo       bool
	Binary     string // 既定 ssh
	// Fresh は接続を使い回さない（締め出しの確認で新しい接続を張る、E-B3）。
	Fresh bool
}

// Name はホストの名前。
func (s SSH) Name() string { return s.HostName }

// Command は ssh の argv（テストで確認する）。
func (s SSH) Command(argv []string) []string {
	bin := s.Binary
	if bin == "" {
		bin = "ssh"
	}
	out := []string{bin, "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + s.KnownHosts, "-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "IdentitiesOnly=yes", "-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=10", "-i", s.Key}
	if s.Fresh {
		out = append(out, "-o", "ControlPath=none")
	}
	if s.Port != 0 {
		out = append(out, "-p", strconv.Itoa(s.Port))
	}
	if s.User != "" {
		out = append(out, "-l", s.User)
	}
	out = append(out, "--", s.Address)
	remote := argv
	if s.Sudo {
		remote = append([]string{"sudo", "-n"}, argv...)
	}
	quoted := make([]string, len(remote))
	for i, a := range remote {
		quoted[i] = Quote(a)
	}
	return append(out, strings.Join(quoted, " "))
}

// Run は argv を ssh 越しに実行する。ssh の終了コード 255 は「届かない」として扱う。
func (s SSH) Run(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
	return run(ctx, s.HostName, s.Command(argv), stdin, true)
}

// Quote は POSIX シェルの単一引用符で囲む。
func Quote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./=:,@+%") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func run(ctx context.Context, host string, argv []string, stdin []byte, viaSSH bool) ([]byte, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return stdout.Bytes(), nil
	case ctx.Err() != nil:
		return nil, &UnreachableError{Host: host, Err: fmt.Errorf("タイムアウト: %w", ctx.Err())}
	case errors.As(err, &ee):
		if viaSSH && ee.ExitCode() == 255 {
			return nil, &UnreachableError{Host: host, Err: errors.New(strings.TrimSpace(stderr.String()))}
		}
		return stdout.Bytes(), &ExitError{Code: ee.ExitCode(), Stderr: stderr.String()}
	default:
		return nil, &UnreachableError{Host: host, Err: err}
	}
}

// ParseHosts は provider の settings の hosts を読む（接続設定、§5.6）。
//
//	hosts:
//	  local: {local: true, sudo: false, prefix: [ip, netns, exec, lab]}
//	  fw1:
//	    ssh: {address: 192.0.2.1, user: sakutei, port: 22, key: /etc/sakutei/id_ed25519, known_hosts: /etc/sakutei/known_hosts}
//	    sudo: true
//
// extra はホストごとに provider が読む field の名前（それ以外の field はエラーに
// する）。読んだ値は Host.Extra に入る。
func ParseHosts(v any, extra ...string) (map[string]Host, error) {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return nil, errors.New("settings.hosts にホストのマップを書く")
	}
	out := map[string]Host{}
	for _, name := range sortedKeys(m) {
		if name == "" || strings.ContainsAny(name, "/: ") {
			return nil, fmt.Errorf("settings.hosts: ホストの名前 %q に / : 空白は使えない", name)
		}
		hm, ok := m[name].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("settings.hosts.%s にマップを書く", name)
		}
		h, err := parseHost(name, hm, extra)
		if err != nil {
			return nil, fmt.Errorf("settings.hosts.%s: %w", name, err)
		}
		out[name] = h
	}
	return out, nil
}

// Host はホスト 1 つの接続設定。
type Host struct {
	Runner Runner
	// Fresh は管理の経路を新しい接続で確認する Runner（ssh なら ControlPath=none）。
	Fresh Runner
	Extra map[string]any
}

func parseHost(name string, m map[string]any, extra []string) (Host, error) {
	h := Host{Extra: map[string]any{}}
	var local, sudo bool
	var prefix []string
	var ssh *SSH
	for _, k := range sortedKeys(m) {
		v := m[k]
		switch k {
		case "local":
			b, ok := v.(bool)
			if !ok || !b {
				return h, errors.New("local は true だけを書く")
			}
			local = true
		case "sudo":
			b, ok := v.(bool)
			if !ok {
				return h, errors.New("sudo に true か false を書く")
			}
			sudo = b
		case "prefix":
			l, err := stringList(v)
			if err != nil {
				return h, fmt.Errorf("prefix: %w", err)
			}
			prefix = l
		case "ssh":
			s, err := parseSSH(v)
			if err != nil {
				return h, fmt.Errorf("ssh: %w", err)
			}
			s.HostName = name
			ssh = s
		default:
			if !contains(extra, k) {
				all := append([]string{"local", "prefix", "ssh", "sudo"}, extra...)
				sort.Strings(all)
				return h, fmt.Errorf("不明な field %q（使える field: %s）", k, strings.Join(all, ", "))
			}
			h.Extra[k] = v
		}
	}
	switch {
	case local && ssh != nil:
		return h, errors.New("local と ssh はどちらか 1 つ")
	case local:
		h.Runner = Local{HostName: name, Prefix: prefix, Sudo: sudo}
		h.Fresh = h.Runner
	case ssh != nil:
		if len(prefix) > 0 {
			return h, errors.New("prefix は local のときだけ")
		}
		ssh.Sudo = sudo
		h.Runner = *ssh
		fresh := *ssh
		fresh.Fresh = true
		h.Fresh = fresh
	default:
		if len(h.Extra) == 0 {
			return h, errors.New("local: true か ssh を書く")
		}
	}
	return h, nil
}

func parseSSH(v any) (*SSH, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("マップを書く")
	}
	s := &SSH{}
	for _, k := range sortedKeys(m) {
		switch k {
		case "address", "user", "key", "known_hosts", "binary":
			str, ok := m[k].(string)
			if !ok || str == "" {
				return nil, fmt.Errorf("%s に文字列を書く", k)
			}
			switch k {
			case "address":
				s.Address = str
			case "user":
				s.User = str
			case "key":
				s.Key = str
			case "known_hosts":
				s.KnownHosts = str
			case "binary":
				s.Binary = str
			}
		case "port":
			n, ok := m[k].(int64)
			if f, isF := m[k].(float64); isF {
				n, ok = int64(f), f == float64(int64(f))
			}
			if i, isI := m[k].(int); isI {
				n, ok = int64(i), true
			}
			if !ok || n < 1 || n > 65535 {
				return nil, errors.New("port に 1〜65535 を書く")
			}
			s.Port = int(n)
		default:
			return nil, fmt.Errorf("不明な field %q（使える field: address, binary, key, known_hosts, port, user）", k)
		}
	}
	// ホスト鍵を検証せずに接続しない（E-E1）。
	if s.Address == "" || s.Key == "" || s.KnownHosts == "" {
		return nil, errors.New("address・key・known_hosts を書く（ホスト鍵を検証せずに接続しない）")
	}
	return s, nil
}

func stringList(v any) ([]string, error) {
	l, ok := v.([]any)
	if !ok {
		return nil, errors.New("文字列のリストを書く")
	}
	out := make([]string, len(l))
	for i, e := range l {
		s, ok := e.(string)
		if !ok || s == "" {
			return nil, errors.New("文字列のリストを書く")
		}
		out[i] = s
	}
	return out, nil
}

// StringList は settings の文字列のリストを読む（provider が使う）。
func StringList(v any) ([]string, error) { return stringList(v) }

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
