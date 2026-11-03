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

// psql.go — psql を 1 回起動して SQL を流す（E-F1：ドライバでなく CLI を exec で
// 呼ぶ）。
//
//   - シェルは通さない。SQL は標準入力で渡し、引数は固定のオプションだけ。
//   - 接続の設定（host・port・dbname・user・sslmode・sslrootcert・connect_timeout）は
//     子プロセスの環境変数（PGHOST など）で渡す。親の PG* の環境変数は引き継が
//     ない（PGSERVICE・PGOPTIONS などで黙って別の所へつながないように）。
//   - つなぐロールのパスワードは呼ぶたびにファイルから読み（値を持たない、
//     E-G2）、0600 の一時的な passfile に書いて PGPASSFILE で渡し、終われば消す。
//     引数・環境変数の値・誤りの文には出さない。
//   - -X（psqlrc を読まない）-w（パスワードを聞かない）-q -A -t で、結果は
//     飾りの無い値だけ。読む問い合わせは結果を 1 つの JSON にして返す。
//   - ON_ERROR_STOP で最初の誤りで止まり（終了コード 3）、VERBOSITY verbose で
//     誤りに SQLSTATE を付けさせる。接続の失敗は終了コード 2。

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
)

// Server は 1 つのサーバへのつなぎ方（settings.servers.<名前>）。
type Server struct {
	Name           string
	Host           string
	Port           int
	DBName         string // 管理用につなぐ DB（既定 postgres）
	SSLMode        string
	SSLRootCert    string
	ConnectTimeout time.Duration // PGCONNECT_TIMEOUT（秒に切り上げ、最小 2 秒）
	// VerifyPasswords は pg_authid でパスワードが手で変わったかを確かめる（E-F1・N-F1）。
	VerifyPasswords bool
	// Observe は観測用の資格、Write は書き込み用の資格（write を書かなければ
	// Observe と同じ、E-G5）。
	Observe, Write Credential
}

// Credential はつなぐロールとそのパスワードのファイル（無ければ trust・peer の認証）。
type Credential struct {
	User         string
	PasswordFile string
}

// outsideRole は role が管理の境界の外なら理由を返す（E-F1）。
func (s *Server) outsideRole(role string) string {
	switch {
	case strings.HasPrefix(role, "pg_"):
		return fmt.Sprintf("ロール %q は PostgreSQL のロール（pg_*）", role)
	case role == s.Observe.User || role == s.Write.User:
		return fmt.Sprintf("ロール %q は Sakutei がつなぐロール", role)
	case strings.EqualFold(role, "public"):
		return fmt.Sprintf("%q はロールでない（PUBLIC の権限は grant の id の最後に %s と書く）", role, PublicGrantee)
	}
	return ""
}

// outsideGrantee は role が connect_roles に書ける（管理の境界の外のロール）か
// （E-F3・N-F7）。PUBLIC と取り違える名前は除く。
func (s *Server) outsideGrantee(role string) bool {
	return s.outsideRole(role) != "" && !strings.EqualFold(role, "public")
}

// outsideDatabase は db が管理の境界の外なら理由を返す（E-F1）。
func (s *Server) outsideDatabase(db string) string {
	if systemDatabases[db] || db == s.DBName {
		return fmt.Sprintf("データベース %q は PostgreSQL のものか Sakutei がつなぐもの", db)
	}
	return ""
}

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

// RunnerFunc は「パス・引数・環境変数・標準入力 → 標準出力・標準エラー・終了
// コード」の関数を Runner にする（偽の psql、pgfake.Server.Run を差すため）。
func RunnerFunc(f func(ctx context.Context, path string, args, env []string, stdin string) (string, string, int, error)) Runner {
	return func(ctx context.Context, inv Invocation) (Result, error) {
		out, errOut, code, err := f(ctx, inv.Path, inv.Args, inv.Env, inv.Stdin)
		return Result{Stdout: out, Stderr: errOut, ExitCode: code}, err
	}
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
	// KindPassword はつなぐロールのパスワードのファイルを読めない。
	KindPassword
	// KindSQL は SQL の誤り（終了コード 3。SQLState に SQLSTATE）。
	KindSQL
	// KindOutput は psql の出力が読めない。
	KindOutput
)

// Error は psql の 1 回の失敗。文にパスワードや SCRAM の検証値は入れない（E-G2）。
type Error struct {
	Kind     ErrorKind
	Server   string
	Database string
	SQLState string
	Message  string
}

func (e *Error) Error() string {
	what := map[ErrorKind]string{
		KindNoPsql: "psql を起動できない", KindUnreachable: "届かない", KindAuth: "認証の失敗か接続が許されない",
		KindPassword: "パスワードのファイルを読めない", KindSQL: "SQL の誤り " + e.SQLState, KindOutput: "psql の出力が読めない",
	}[e.Kind]
	s := "サーバ " + e.Server
	if e.Database != "" {
		s += fmt.Sprintf(" の DB %q", e.Database)
	}
	s += ": " + what
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
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
	// sqlErrorLine は VERBOSITY verbose の誤りの行（"ERROR:  42710: role ..."）。
	sqlErrorLine = regexp.MustCompile(`(?m)(?:ERROR|FATAL|PANIC):\s+([0-9A-Z]{5}):\s*(.*)$`)
	// authFailure は接続の時点で断られたことを示す文。
	authFailure = regexp.MustCompile(`password authentication failed|no pg_hba\.conf entry|authentication failed|role "[^"]*" does not exist|pg_hba\.conf rejects|no password supplied|is not permitted to log in|SSL error|certificate|permission denied for database`)
	// redactions は誤りの文から消すもの（SCRAM・md5 の検証値、E-G2）。
	redactions = regexp.MustCompile(`SCRAM-SHA-256\$[^'\s]*|md5[0-9a-f]{32}`)
)

// redact は誤りの文から検証値を消し、1 行にそろえて短くする。
func redact(s string) string {
	s = redactions.ReplaceAllString(s, "***")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 600 {
		s = s[:600] + "…"
	}
	return s
}

// Client は psql を呼ぶ口（接続の設定の渡し方・パスワードの passfile・誤りの
// 分け方）。
type Client struct {
	psqlPath string
	runner   Runner
}

// NewClient は psql（空なら PATH の psql）を runner（nil なら ExecRunner）で呼ぶ
// Client を返す。
func NewClient(psql string, runner Runner) *Client {
	if psql == "" {
		psql = "psql"
	}
	if runner == nil {
		runner = ExecRunner
	}
	return &Client{psqlPath: psql, runner: runner}
}

// Run は srv の database（空なら管理用の DB）に cred のロールでつなぐ psql を
// 1 回起動して script を流し、標準出力を返す。失敗は *Error。
func (c *Client) Run(ctx context.Context, srv *Server, cred Credential, database, script string) (string, error) {
	if database == "" {
		database = srv.DBName
	}
	mk := func(kind ErrorKind, state, msg string) error {
		return &Error{Kind: kind, Server: srv.Name, Database: database, SQLState: state, Message: msg}
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
	if cred.PasswordFile != "" {
		b, err := os.ReadFile(cred.PasswordFile)
		if err != nil {
			return "", mk(KindPassword, "", fmt.Sprintf("ロール %s のパスワードのファイル %s", cred.User, cred.PasswordFile))
		}
		file, err := writePassfile(strings.TrimRight(string(b), "\r\n"))
		if err != nil {
			return "", mk(KindPassword, "", fmt.Sprintf("一時的な passfile を書けない: %v", err))
		}
		defer os.Remove(file)
		add("PGPASSFILE", file)
	}
	res, err := c.runner(ctx, Invocation{Path: c.psqlPath, Args: append([]string(nil), psqlArgs...), Env: env, Stdin: script})
	if err != nil {
		return "", mk(KindNoPsql, "", fmt.Sprintf("%s（settings.psql にパスを書く）: %v", c.psqlPath, err))
	}
	stderr := redact(res.Stderr)
	switch res.ExitCode {
	case 0:
		return res.Stdout, nil
	case 2:
		if authFailure.MatchString(res.Stderr) {
			return "", mk(KindAuth, "", stderr)
		}
		if m := sqlErrorLine.FindStringSubmatch(res.Stderr); m != nil && m[1] == "3D000" {
			return "", mk(KindSQL, m[1], redact(m[2])) // つなぐ先の DB が無い
		}
		return "", mk(KindUnreachable, "", stderr)
	case 3:
		if m := sqlErrorLine.FindStringSubmatch(res.Stderr); m != nil {
			return "", mk(KindSQL, m[1], redact(m[2]))
		}
		return "", mk(KindSQL, "", stderr)
	}
	return "", mk(KindNoPsql, "", fmt.Sprintf("psql が %d で終わった: %s", res.ExitCode, stderr))
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
		return "", errors.New("パスワードに改行がある")
	}
	f, err := os.CreateTemp("", "sakutei-pgpass-")
	if err != nil {
		return "", err
	}
	name := f.Name()
	esc := strings.NewReplacer(`\`, `\\`, `:`, `\:`).Replace(password)
	err = f.Chmod(0o600)
	if err == nil {
		_, err = f.WriteString("*:*:*:*:" + esc + "\n")
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}
