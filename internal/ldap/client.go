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

package ldap

// client.go — OpenLDAP のクライアント（ldapsearch・ldapmodify）を 1 回起動する
// （Go の依存を増やさないため、ライブラリでなく CLI を exec で呼ぶ。postgresql が
// psql を呼ぶのと同じ形）。
//
//   - シェルは通さない。引数は固定のオプションと DN・フィルタ・属性の名前だけ。
//     変更（LDIF）は標準入力で渡す。
//   - bind は simple bind（-x -D）。パスワードは呼ぶたびに secret store から解決し
//     （JIT、§20.2）、0600 の一時ファイルに書いて -y で渡し、終われば消す。
//     引数・環境変数・ログ・エラーには出さない。
//   - LDAPS（ldaps://）と StartTLS（-ZZ：TLS にできなければ失敗）。CA は
//     LDAPTLS_CACERT、証明書の確かめは LDAPTLS_REQCERT=demand（利用者の ldaprc の
//     TLS_REQCERT never などで緩めさせない）。親の LDAP で始まる環境変数は引き継が
//     ない。
//   - 終了コードは LDAP の結果コード（32 noSuchObject、49 invalidCredentials、
//     負のコードは 256 を足した値：255 はサーバに届かない）。
//
// Runner はコマンドを起動する seam。既定は os/exec、テストは偽の ldap コマンド
// （internal/ldap/ldaptest）を差す。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// Invocation はコマンドの 1 回の起動。
type Invocation struct {
	Path  string   // ldapsearch・ldapmodify のパス（"/" を含まなければ PATH から探す）
	Args  []string // 引数
	Env   []string // 子プロセスの環境変数（全部）
	Stdin string   // LDIF（ldapmodify）
}

// Result はコマンドが終わったときの結果。
type Result struct {
	Stdout, Stderr string
	ExitCode       int
}

// Runner はコマンドを起動する。error は起動できなかった（見つからない・実行
// できない）ときだけで、0 以外の終了コードは Result で返す。
type Runner func(ctx context.Context, inv Invocation) (Result, error)

// ExecRunner は os/exec で起動する Runner。
func ExecRunner(ctx context.Context, inv Invocation) (Result, error) {
	cmd := exec.CommandContext(ctx, inv.Path, inv.Args...)
	cmd.Env = inv.Env
	cmd.Stdin = strings.NewReader(inv.Stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return res, nil
	case ctx.Err() != nil:
		// 時間切れ・取り消しで殺した。届かないのと同じに扱う。
		res.ExitCode = 255
		res.Stderr += "\n" + ctx.Err().Error()
		return res, nil
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}
	return res, err
}

// RunnerFunc は「パス・引数・環境変数・標準入力 → 標準出力・標準エラー・終了
// コード」の関数を Runner にする（偽の ldap コマンド、ldaptest.Server.Run を差すため）。
func RunnerFunc(f func(ctx context.Context, path string, args, env []string, stdin string) (string, string, int, error)) Runner {
	return func(ctx context.Context, inv Invocation) (Result, error) {
		out, errOut, code, err := f(ctx, inv.Path, inv.Args, inv.Env, inv.Stdin)
		return Result{Stdout: out, Stderr: errOut, ExitCode: code}, err
	}
}

// LDAP の結果コード（使うものだけ）。
const (
	CodeSizeLimitExceeded   = 4
	CodeNoSuchObject        = 32
	CodeInvalidCredentials  = 49
	CodeNotAllowedOnNonLeaf = 66
	CodeAlreadyExists       = 68
)

// ErrorKind は失敗の種類。
type ErrorKind int

const (
	// KindNoCommand はコマンドを起動できない（無い・実行できない）。
	KindNoCommand ErrorKind = iota + 1
	// KindUnreachable はサーバに届かない・時間切れ・サーバが忙しい。
	KindUnreachable
	// KindAuth は bind の失敗・TLS の失敗（証明書を確かめられない）。
	KindAuth
	// KindSecret は bind のパスワードを secret store から解決できない。
	KindSecret
	// KindLDAP はサーバが返したエラー（Code に結果コード）。
	KindLDAP
	// KindOutput はコマンドの出力が読めない。
	KindOutput
)

// Error はコマンドの 1 回の失敗。文言にパスワードは入れない。
type Error struct {
	Kind    ErrorKind
	Server  string
	Code    int
	Message string
	Err     error
}

func (e *Error) Error() string {
	var what string
	switch e.Kind {
	case KindNoCommand:
		what = "the ldap command cannot be run"
	case KindUnreachable:
		what = "unreachable"
	case KindAuth:
		what = "bind or TLS failed"
	case KindSecret:
		what = "the bind password cannot be resolved"
	case KindLDAP:
		what = "LDAP error " + strconv.Itoa(e.Code)
	case KindOutput:
		what = "unreadable ldap command output"
	}
	s := fmt.Sprintf("server %s: %s", e.Server, what)
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// KindOf は err の中の *Error の Kind（無ければ 0）。
func KindOf(err error) ErrorKind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return 0
}

// CodeOf は err の中の *Error の LDAP の結果コード（KindLDAP でなければ 0）。
func CodeOf(err error) int {
	var e *Error
	if errors.As(err, &e) && e.Kind == KindLDAP {
		return e.Code
	}
	return 0
}

// oneLine は標準エラーを 1 行にそろえて短くする。
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 600 {
		s = s[:600] + "…"
	}
	return s
}

// Client は ldap コマンドを呼ぶ口。
type Client struct {
	search, modify string
	runner         Runner
	secrets        core.SecretResolver
}

// NewClient は opts でコマンドを呼ぶ Client を返す。
func NewClient(opts Options) *Client {
	c := &Client{search: opts.LDAPSearch, modify: opts.LDAPModify, runner: opts.Runner, secrets: opts.Secrets}
	if c.search == "" {
		c.search = "ldapsearch"
	}
	if c.modify == "" {
		c.modify = "ldapmodify"
	}
	if c.runner == nil {
		c.runner = ExecRunner
	}
	return c
}

// SearchScope は検索の範囲。
type SearchScope string

const (
	ScopeBase SearchScope = "base"
	ScopeOne  SearchScope = "one"
	ScopeSub  SearchScope = "sub"
)

// SearchRequest は 1 回の検索。
type SearchRequest struct {
	Base   string
	Scope  SearchScope
	Filter string
	Attrs  []string
	// Paged は結果をページに分けて受け取る（サーバの件数の上限に当たらないため。List）。
	Paged bool
	// SizeLimit は返す項目の数の上限（0 は付けない）。超えたら結果コード 4。
	SizeLimit int
}

// Search は検索して項目を返す。失敗は *Error（結果コード 4 で切れたときも、
// 読めた項目と一緒に *Error を返す）。
func (c *Client) Search(ctx context.Context, name string, srv *Server, req SearchRequest) ([]Entry, error) {
	args := []string{"-LLL", "-o", "ldif-wrap=no"}
	if srv.Timeout > 0 {
		args = append(args, "-l", strconv.Itoa(seconds(srv.Timeout)))
	}
	if req.Paged {
		args = append(args, "-E", "pr=500/noprompt")
	}
	if req.SizeLimit > 0 {
		args = append(args, "-z", strconv.Itoa(req.SizeLimit))
	}
	args = append(args, "-b", req.Base, "-s", string(req.Scope), req.Filter)
	args = append(args, req.Attrs...)
	out, err := c.run(ctx, c.search, name, srv, args, "")
	if err != nil && CodeOf(err) != CodeSizeLimitExceeded {
		return nil, err
	}
	recs, perr := ParseLDIF(out)
	if perr != nil {
		return nil, &Error{Kind: KindOutput, Server: name, Err: perr}
	}
	es, perr := entries(recs)
	if perr != nil {
		return nil, &Error{Kind: KindOutput, Server: name, Err: perr}
	}
	return es, err
}

// Modify は LDIF の変更を流す（最初のエラーで止まる）。
func (c *Client) Modify(ctx context.Context, name string, srv *Server, ldif string) error {
	_, err := c.run(ctx, c.modify, name, srv, nil, ldif)
	return err
}

func seconds(d time.Duration) int {
	return max(int((d+time.Second-1)/time.Second), 1)
}

// run はコマンドを 1 回起動し、標準出力を返す。
func (c *Client) run(ctx context.Context, path, name string, srv *Server, extra []string, stdin string) (string, error) {
	mk := func(kind ErrorKind, code int, msg string, err error) error {
		return &Error{Kind: kind, Server: name, Code: code, Message: msg, Err: err}
	}
	args := []string{"-x", "-H", srv.URI}
	if srv.StartTLS {
		args = append(args, "-ZZ")
	}
	if srv.Timeout > 0 {
		args = append(args, "-o", "nettimeout="+strconv.Itoa(seconds(srv.Timeout)))
	}
	args = append(args, "-D", srv.BindDN)
	ref := srv.BindPassword
	if c.secrets == nil {
		return "", mk(KindSecret, 0, fmt.Sprintf("%s: no secret store is configured", ref), nil)
	}
	pw, err := c.secrets.ResolveValue(ctx, ref.Backend(), ref.Path(), ref.Generation())
	if err != nil {
		return "", mk(KindSecret, 0, ref.String(), err)
	}
	file, err := writeSecretFile(pw)
	pw = ""
	if err != nil {
		return "", mk(KindSecret, 0, "cannot write the temporary password file", err)
	}
	defer os.Remove(file)
	args = append(args, "-y", file)
	args = append(args, extra...)

	env := childEnv(os.Environ())
	env = append(env, "LDAPTLS_REQCERT=demand", "LC_ALL=C")
	if srv.CACert != "" {
		env = append(env, "LDAPTLS_CACERT="+srv.CACert)
	}
	if srv.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 3*srv.Timeout+5*time.Second)
		defer cancel()
	}
	res, err := c.runner(ctx, Invocation{Path: path, Args: args, Env: env, Stdin: stdin})
	if err != nil {
		return "", mk(KindNoCommand, 0, fmt.Sprintf("%s (set ldapsearch / ldapmodify in the server list to its path)", path), err)
	}
	msg := oneLine(res.Stderr)
	switch code := res.ExitCode; {
	case code == 0:
		return res.Stdout, nil
	case code == CodeInvalidCredentials || code == 48 || code == 8 || code == 13:
		// invalidCredentials・inappropriateAuthentication・strongerAuthRequired・
		// confidentialityRequired。
		return res.Stdout, mk(KindAuth, code, msg, nil)
	case code == 3 || code == 51 || code == 52 || code >= 128:
		// timeLimitExceeded・busy・unavailable・クライアントの側の負のコード
		// （届かない・時間切れ・接続のエラー）。
		return res.Stdout, mk(KindUnreachable, code, msg, nil)
	case code == 1 && (strings.Contains(res.Stderr, "start_tls") || strings.Contains(res.Stderr, "TLS") || strings.Contains(res.Stderr, "certificate")):
		return res.Stdout, mk(KindAuth, code, msg, nil)
	}
	return res.Stdout, mk(KindLDAP, res.ExitCode, msg, nil)
}

// childEnv は親の環境変数から LDAP で始まるもの（OpenLDAP の設定）と言語の
// 設定を除いたもの。
func childEnv(parent []string) []string {
	out := make([]string, 0, len(parent)+4)
	for _, kv := range parent {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "LDAP") || strings.HasPrefix(k, "LC_") || k == "LANG" {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// writeSecretFile は値（末尾に改行を付けない。-y はファイルの中身をそのまま
// パスワードにする）を 0600 の一時ファイルに書き、そのパスを返す。
func writeSecretFile(value string) (string, error) {
	f, err := os.CreateTemp("", "sakutei-ldap-")
	if err != nil {
		return "", err
	}
	name := f.Name()
	fail := func(err error) (string, error) {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if err := f.Chmod(0o600); err != nil {
		return fail(err)
	}
	if _, err := f.WriteString(value); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}
