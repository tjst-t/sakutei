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

package postgresql

// psql.go — psql を 1 回起動して SQL を流す（Go の依存を増やさないため、ドライバ
// ではなく CLI を exec で呼ぶ。sqlmigrate が sqlite3 を呼ぶのと同じ形）。
//
//   - シェルは通さない。SQL は標準入力で渡し、引数は固定のオプションだけ。
//   - 接続の設定（host・port・dbname・user・sslmode・sslrootcert・connect_timeout）は
//     子プロセスの環境変数（PGHOST など）で渡す。親の PG* の環境変数は引き継が
//     ない（PGSERVICE・PGOPTIONS などで黙って別の所へつながないように）。
//   - つなぐロールは観測用か書き込み用（ctx で選ぶ、#186）。
//   - 管理用のロールのパスワードは呼ぶたびに secret store から解決し（JIT、
//     §20.2）、0600 の一時的な passfile に書いて PGPASSFILE で渡し、終われば消す。
//     引数・環境変数の値・ログ・エラーには出さない。
//   - -X（psqlrc を読まない）-w（パスワードを聞かない）-q -A -t で、結果は
//     飾りの無い値だけ。読む問い合わせは結果を 1 つの JSON にして返す。
//   - ON_ERROR_STOP で最初のエラーで止まり（終了コード 3）、VERBOSITY verbose で
//     エラーに SQLSTATE を付けさせる。接続の失敗は終了コード 2。
//
// Runner は psql を起動する seam。既定は os/exec、テストは偽の psql
// （internal/postgresql/pgtest）を差す。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// Invocation は psql の 1 回の起動。
type Invocation struct {
	Path  string   // psql のパス（"/" を含まなければ PATH から探す）
	Args  []string // 引数（固定のオプションだけ）
	Env   []string // 子プロセスの環境変数（全部）
	Stdin string   // SQL
}

// Result は psql が終わったときの結果。
type Result struct {
	Stdout, Stderr string
	ExitCode       int
}

// Runner は psql を起動する。error は起動できなかった（見つからない・実行
// できない）ときだけで、0 以外の終了コードは Result で返す。
type Runner func(ctx context.Context, inv Invocation) (Result, error)

// ExecRunner は os/exec で psql を起動する Runner。
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
		// 時間切れ・取り消しで殺した。つながらないのと同じに扱う。
		res.ExitCode = 2
		res.Stderr += "\npsql: " + ctx.Err().Error()
		return res, nil
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}
	return res, err
}

// psqlArgs は psql に渡す引数（いつも同じ）。
var psqlArgs = []string{
	"-X", "-w", "-q", "-A", "-t",
	"-v", "ON_ERROR_STOP=1",
	"-v", "VERBOSITY=verbose",
	"-f", "-",
}

// ErrorKind は psql の失敗の種類。
type ErrorKind int

const (
	// KindNoPsql は psql を起動できない（無い・実行できない）。
	KindNoPsql ErrorKind = iota + 1
	// KindUnreachable はサーバにつながらない（終了コード 2・時間切れ）。
	KindUnreachable
	// KindAuth は認証の失敗・接続が許されない（pg_hba.conf など）。
	KindAuth
	// KindSecret は管理用のパスワードを secret store から解決できない。
	KindSecret
	// KindSQL は SQL のエラー（終了コード 3。SQLState に SQLSTATE）。
	KindSQL
	// KindOutput は psql の出力が読めない。
	KindOutput
	// KindNoWrite はこの回が書かない（provider.AccessObserve）のに書き込みの
	// psql を求められた（起動しない。Err は provider.ErrNoWriteAccess を包む）。
	KindNoWrite
)

// Error は psql の 1 回の失敗。文言にパスワードや SCRAM の検証値は入れない。
type Error struct {
	Kind     ErrorKind
	Server   string
	Database string
	SQLState string
	Message  string
	Err      error
}

func (e *Error) Error() string {
	var what string
	switch e.Kind {
	case KindNoPsql:
		what = "psql cannot be run"
	case KindUnreachable:
		what = "unreachable"
	case KindAuth:
		what = "authentication failed or connection not allowed"
	case KindSecret:
		what = "the password cannot be resolved"
	case KindSQL:
		what = "SQL error " + e.SQLState
	case KindOutput:
		what = "unreadable psql output"
	case KindNoWrite:
		what = "the write is refused"
	}
	s := fmt.Sprintf("server %s", e.Server)
	if e.Database != "" {
		s += fmt.Sprintf(" database %q", e.Database)
	}
	s += ": " + what
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

// SQLStateOf は err の中の *Error の SQLSTATE（無ければ ""）。
func SQLStateOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.SQLState
	}
	return ""
}

var (
	// sqlErrorLine は VERBOSITY verbose のエラーの行（"ERROR:  42710: role ..."）。
	sqlErrorLine = regexp.MustCompile(`(?m)(?:ERROR|FATAL|PANIC):\s+([0-9A-Z]{5}):\s*(.*)$`)
	// authFailure は接続の時点で断られたことを示す文。
	authFailure = regexp.MustCompile(`password authentication failed|no pg_hba\.conf entry|authentication failed|role "[^"]*" does not exist|pg_hba\.conf rejects|no password supplied|is not permitted to log in|SSL error|certificate`)
	// redactions はエラーの文から消すもの（SCRAM・md5 の検証値）。
	redactions = regexp.MustCompile(`SCRAM-SHA-256\$[^'\s]*|md5[0-9a-f]{32}`)
)

// redact はエラーの文から検証値を消し、1 行にそろえて短くする。
func redact(s string) string {
	s = redactions.ReplaceAllString(s, "***")
	lines := strings.Fields(strings.ReplaceAll(s, "\n", " "))
	s = strings.Join(lines, " ")
	if len(s) > 600 {
		s = s[:600] + "…"
	}
	return s
}

// connParams は 1 回の接続の設定。
type connParams struct {
	server   string
	srv      *Server
	database string
}

// run は psql を 1 回起動して script を流し、標準出力を返す。
func (p *Provider) run(ctx context.Context, cp connParams, script string) (string, error) {
	return p.psql.Run(ctx, cp.server, *cp.srv, cp.database, script)
}

// Client は psql を呼ぶ口（接続の設定の渡し方・パスワードの解決と passfile・
// エラーの分け方）。postgresql provider と sqlmigrate（engine: postgresql）が
// 共有する。
type Client struct {
	psqlPath string
	runner   Runner
	secrets  core.SecretResolver
	noWrite  bool
}

// NewClient は opts（Psql・Runner・Secrets）で psql を呼ぶ Client を返す。
func NewClient(opts Options) *Client {
	c := &Client{psqlPath: opts.Psql, runner: opts.Runner, secrets: opts.Secrets, noWrite: opts.NoWrite}
	if c.psqlPath == "" {
		c.psqlPath = "psql"
	}
	if c.runner == nil {
		c.runner = ExecRunner
	}
	return c
}

// Run は srv（名前は server）の database につなぐ psql を 1 回起動して script を
// 流し、標準出力を返す。失敗は *Error。つなぐロールは ctx で選ぶ: 書き込み
// （provider.Writing）なら書き込み用、ほかは観測用（§20.7、#186）。
func (c *Client) Run(ctx context.Context, server string, srv Server, database, script string) (string, error) {
	mk := func(kind ErrorKind, state, msg string, err error) error {
		return &Error{Kind: kind, Server: server, Database: database, SQLState: state, Message: msg, Err: err}
	}
	if database == "" {
		database = srv.DBName
	}
	cred, err := srv.credentials(c.noWrite).For(ctx, "psql")
	if err != nil {
		return "", mk(KindNoWrite, "", "", provider.ErrNoWriteAccess)
	}
	env := childEnv(os.Environ())
	add := func(k, v string) {
		if v != "" {
			env = append(env, k+"="+v)
		}
	}
	add("PGHOST", srv.Host)
	if srv.Port != 0 {
		add("PGPORT", strconv.Itoa(srv.Port))
	}
	add("PGDATABASE", database)
	add("PGUSER", cred.User)
	add("PGSSLMODE", srv.SSLMode)
	add("PGSSLROOTCERT", srv.SSLRootCert)
	if t := srv.ConnectTimeout; t > 0 {
		secs := int((t + time.Second - 1) / time.Second)
		add("PGCONNECT_TIMEOUT", strconv.Itoa(max(secs, 2)))
	}
	add("PGAPPNAME", "sakutei")
	add("PGCLIENTENCODING", "UTF8")
	add("LC_MESSAGES", "C")
	if cred.HasPassword {
		ref := cred.Password
		if c.secrets == nil {
			return "", mk(KindSecret, "", fmt.Sprintf("%s: no secret store is configured", ref), nil)
		}
		pw, err := c.secrets.ResolveValue(ctx, ref.Backend(), ref.Path(), ref.Generation())
		if err != nil {
			return "", mk(KindSecret, "", ref.String(), err)
		}
		file, err := writePassfile(pw)
		pw = ""
		if err != nil {
			return "", mk(KindSecret, "", "cannot write the temporary passfile", err)
		}
		defer os.Remove(file)
		add("PGPASSFILE", file)
	}

	res, err := c.runner(ctx, Invocation{Path: c.psqlPath, Args: append([]string(nil), psqlArgs...), Env: env, Stdin: script})
	if err != nil {
		return "", mk(KindNoPsql, "", fmt.Sprintf("%s (set psql in the server list to its path)", c.psqlPath), err)
	}
	stderr := redact(res.Stderr)
	switch res.ExitCode {
	case 0:
		return res.Stdout, nil
	case 2:
		if authFailure.MatchString(res.Stderr) {
			return "", mk(KindAuth, "", stderr, nil)
		}
		if m := sqlErrorLine.FindStringSubmatch(res.Stderr); m != nil && m[1] == "3D000" {
			// つなぐ先のデータベースが無い。
			return "", mk(KindSQL, m[1], redact(m[2]), nil)
		}
		if strings.Contains(res.Stderr, "does not exist") && strings.Contains(res.Stderr, "database") {
			return "", mk(KindSQL, "3D000", stderr, nil)
		}
		return "", mk(KindUnreachable, "", stderr, nil)
	case 3:
		if m := sqlErrorLine.FindStringSubmatch(res.Stderr); m != nil {
			return "", mk(KindSQL, m[1], redact(m[2]), nil)
		}
		return "", mk(KindSQL, "", stderr, nil)
	}
	return "", mk(KindNoPsql, "", fmt.Sprintf("psql exited with %d: %s", res.ExitCode, stderr), nil)
}

// childEnv は親の環境変数から PG で始まるもの（libpq の設定）と言語の設定を
// 除いたもの。
func childEnv(parent []string) []string {
	out := make([]string, 0, len(parent)+12)
	for _, kv := range parent {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "PG") || k == "LC_MESSAGES" || k == "LC_ALL" {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// writePassfile は「どのホスト・ポート・DB・利用者にもこのパスワード」の
// passfile を 0600 の一時ファイルに書き、そのパスを返す。
func writePassfile(password string) (string, error) {
	if strings.ContainsAny(password, "\n\r") {
		return "", errors.New("the password must not contain a line break")
	}
	f, err := os.CreateTemp("", "sakutei-pgpass-")
	if err != nil {
		return "", err
	}
	name := f.Name()
	esc := strings.NewReplacer(`\`, `\\`, `:`, `\:`).Replace(password)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if _, err := f.WriteString("*:*:*:*:" + esc + "\n"); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

// RunnerFunc は「パス・引数・環境変数・標準入力 → 標準出力・標準エラー・終了
// コード」の関数を Runner にする（偽の psql、pgtest.Server.Run を差すため）。
func RunnerFunc(f func(ctx context.Context, path string, args, env []string, stdin string) (string, string, int, error)) Runner {
	return func(ctx context.Context, inv Invocation) (Result, error) {
		out, errOut, code, err := f(ctx, inv.Path, inv.Args, inv.Env, inv.Stdin)
		return Result{Stdout: out, Stderr: errOut, ExitCode: code}, err
	}
}
