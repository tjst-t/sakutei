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

package sqlmigrate

// postgres.go は engine: postgresql（v2 の #126 と同じ決まり、E-F2・N-F2）。Go の
// 依存を増やさないため、ドライバでなく psql を exec で呼ぶ。
//
//   - シェルは通さない。SQL は標準入力で渡し、引数は固定のオプションだけ。
//   - 接続の設定（host・port・dbname・user・sslmode・sslrootcert・connect_timeout）は
//     子プロセスの環境変数（PGHOST など）で渡す。親の PG* の環境変数は引き継が
//     ない（PGSERVICE・PGOPTIONS などで黙って別の所へつながないように）。
//   - 観測は observe の credential、invoke は write の credential（E-G5・N-G3）。パスワードは
//     呼ぶたびにファイルか secret の参照（write だけ、WriteContext.Secret）から読み、
//     0600 の一時的な passfile に書いて PGPASSFILE で渡し、終われば消す。引数・
//     環境変数の値・エラーメッセージには出さない（E-G2）。
//   - -X（psqlrc を読まない）-w（パスワードを聞かない）-q -A -t で、結果は飾りの
//     無い値だけ。ON_ERROR_STOP で最初のエラーで止まり（終了コード 3）、VERBOSITY
//     verbose でエラーに SQLSTATE を付けさせる。接続の失敗は終了コード 2。
//
// 観測は schema_migrations（version bigint PRIMARY KEY, applied_at timestamptz）の
// 最大のバージョン（表が無ければ 0）。up は 1 本を 1 回の psql の呼び出しで BEGIN … その
// SQL … INSERT INTO schema_migrations … COMMIT として流す（失敗した 1 本は残らない）。
// down は同じ形で DELETE。データベースは sqlmigrate が作らない（無ければ Unknown）。
//
// 同じデータベースを複数の daemon が動かさないよう、1 本ごとのトランザクションの頭で
// pg_advisory_xact_lock（固定のキー。advisory lock はデータベースごと）を
// lock_timeout まで待って取り、取れたら schema_migrations が流す前のバージョンのままかを
// 確かめる（違えば 55000 のエラーで、何も流さない、E-F2）。ロックを待ちきれない（55P03）
// ときは何も流していない。トランザクションに入れられない文（CREATE INDEX
// CONCURRENTLY・VACUUM など）は PostgreSQL が 25001 にし、その 1 本は失敗として
// 知らせる（特別扱いしない）。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
)

// invocation は psql の 1 回の起動。
type invocation struct {
	Path  string   // psql のパス（"/" を含まなければ PATH から探す）
	Args  []string // 引数（固定のオプションだけ）
	Env   []string // 子プロセスの環境変数（全部）
	Stdin string   // SQL
}

// result は psql が終わったときの結果。
type result struct {
	Stdout, Stderr string
	ExitCode       int
}

// runner は psql を起動する。error は起動できなかった（見つからない・実行できない）
// ときだけで、0 以外の終了コードは result で返す。
type runner func(ctx context.Context, inv invocation) (result, error)

// execRunner は os/exec で psql を起動する。時間切れ・取り消しで殺したときは
// 終了コード -1（送れたか分からない）。
func execRunner(ctx context.Context, inv invocation) (result, error) {
	cmd := exec.CommandContext(ctx, inv.Path, inv.Args...)
	cmd.Env = inv.Env
	cmd.Stdin = strings.NewReader(inv.Stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := result{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return res, nil
	case ctx.Err() != nil:
		res.ExitCode = -1
		res.Stderr += "\npsql: " + ctx.Err().Error()
		return res, nil
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}
	return res, err
}

// psqlArgs は psql に渡す引数（いつも同じ）。
var psqlArgs = []string{"-X", "-w", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-v", "VERBOSITY=verbose", "-f", "-"}

// pgLockKey は pg_advisory_xact_lock のキー（"sakutei:sqlmigrate" の FNV-1a。v2 と
// 同じ値なので、v2 と v3 の daemon が同じデータベースに向いても二重に流さない）。
var pgLockKey = func() int64 {
	h := fnv.New64a()
	h.Write([]byte("sakutei:sqlmigrate"))
	return int64(h.Sum64())
}()

var (
	// sqlErrorLine は VERBOSITY verbose のエラーの行（"ERROR:  42710: role ..."）。
	sqlErrorLine = regexp.MustCompile(`(?m)(?:ERROR|FATAL|PANIC):\s+([0-9A-Z]{5}):\s*(.*)$`)
	// connectionLost は接続が途中で切れたことを示す文（送れたか分からない）。
	connectionLost = regexp.MustCompile(`server closed the connection unexpectedly|connection to server was lost|terminating connection|no connection to the server|SSL SYSCALL error|could not receive data from server|could not send data to server`)
	// redactions はエラーメッセージから消すもの（SCRAM・md5 の検証値）。
	redactions = regexp.MustCompile(`SCRAM-SHA-256\$[^'\s]*|md5[0-9a-f]{32}`)
)

// redact はエラーメッセージから検証値を消し、1 行にそろえて短くする。
func redact(s string) string {
	s = redactions.ReplaceAllString(s, "***")
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
	if len(s) > 600 {
		s = s[:600] + "…"
	}
	return s
}

// psqlRun は db につなぐ psql を 1 回起動して script を流し、標準出力を返す。w が
// nil なら観測用の credential、nil でなければ書き込み用の credential でつなぐ（E-G5）。失敗は
// *execError。
func (p *Provider) psqlRun(ctx context.Context, db *database, w *core.WriteContext, script string) (string, error) {
	c := db.pg
	cred := c.observe
	if w != nil {
		cred = c.write
	}
	env := childEnv(os.Environ())
	add := func(k, v string) {
		if v != "" {
			env = append(env, k+"="+v)
		}
	}
	add("PGHOST", c.host)
	if c.port != 0 {
		add("PGPORT", strconv.Itoa(c.port))
	}
	add("PGDATABASE", c.dbname)
	add("PGUSER", cred.user)
	add("PGSSLMODE", c.sslmode)
	add("PGSSLROOTCERT", c.sslrootcert)
	if t := c.connectTimeout; t > 0 {
		add("PGCONNECT_TIMEOUT", strconv.Itoa(max(int((t+time.Second-1)/time.Second), 2)))
	}
	add("PGAPPNAME", "sakutei-sqlmigrate")
	add("PGCLIENTENCODING", "UTF8")
	add("LC_MESSAGES", "C")
	pw, hasPW, err := p.password(ctx, cred, w)
	if err != nil {
		return "", &execError{kind: kindNotConnected, msg: err.Error()}
	}
	if hasPW {
		file, err := writePassfile(pw)
		pw = ""
		if err != nil {
			return "", &execError{kind: kindNotConnected, msg: "一時的な passfile を書けない: " + err.Error()}
		}
		defer os.Remove(file)
		add("PGPASSFILE", file)
	}
	res, err := p.run(ctx, invocation{Path: p.psql, Args: append([]string(nil), psqlArgs...), Env: env, Stdin: script})
	if err != nil {
		return "", &execError{kind: kindNotConnected, msg: fmt.Sprintf("psql（%s）を起動できない: %v", p.psql, err)}
	}
	stderr := redact(res.Stderr)
	where := fmt.Sprintf("psql（%s の %s、利用者 %s）", c.host, c.dbname, cred.user)
	switch res.ExitCode {
	case 0:
		if strings.Contains(res.Stderr, "25P01") {
			// COMMIT がトランザクションの外で流れた：ファイルの中で COMMIT・ROLLBACK を
			// 書いたので、どこまで確定したか分からない。
			return "", &execError{kind: kindUnknown, msg: where + ": トランザクションの外で COMMIT が流れた（ファイルの中でトランザクションを書いていないか）: " + stderr}
		}
		return strings.TrimSpace(res.Stdout), nil
	case 2:
		if connectionLost.MatchString(res.Stderr) {
			return "", &execError{kind: kindUnknown, msg: where + ": 接続が途中で切れた: " + stderr}
		}
		// つながらない・認証の失敗・つなぐ先のデータベースが無い：何も送っていない。
		return "", &execError{kind: kindNotConnected, msg: where + ": つながらない: " + stderr}
	case 3:
		m := sqlErrorLine.FindStringSubmatch(res.Stderr)
		if m == nil {
			return "", &execError{kind: kindSQL, msg: where + ": " + stderr}
		}
		e := &execError{kind: kindSQL, state: m[1], msg: fmt.Sprintf("%s: SQL のエラー %s: %s", where, m[1], redact(m[2]))}
		switch {
		case m[1] == "55P03":
			e.kind = kindLocked
		case m[1] == "55000" && strings.Contains(m[2], "sakutei:"):
			e.kind = kindConflict
		}
		return "", e
	}
	return "", &execError{kind: kindUnknown, msg: fmt.Sprintf("%s: 終了コード %d: %s", where, res.ExitCode, stderr)}
}

// password はパスワード（無ければ hasPW が false）。password_secret は書き込みの
// 文脈でだけ取り出せる（E-G2・E-G5）。エラーメッセージに値を入れない。
func (p *Provider) password(ctx context.Context, c credential, w *core.WriteContext) (pw string, hasPW bool, err error) {
	switch {
	case c.passwordSecret != "":
		if w == nil {
			return "", false, errors.New("password_secret は書き込みの文脈でだけ取り出せる")
		}
		v, err := w.Secret(ctx, c.passwordSecret)
		if err != nil {
			return "", false, err
		}
		return v, true, nil
	case c.passwordFile != "":
		b, err := p.readFile(c.passwordFile)
		if err != nil {
			return "", false, fmt.Errorf("パスワードのファイル %s を読めない", c.passwordFile)
		}
		return strings.TrimRight(string(b), "\r\n"), true, nil
	}
	return "", false, nil
}

// childEnv は親の環境変数から PG で始まるもの（libpq の設定）と言語の設定を除く。
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

// writePassfile は「どのホスト・ポート・DB・利用者にもこのパスワード」の passfile を
// 0600 の一時ファイルに書き、そのパスを返す。
func writePassfile(password string) (string, error) {
	if strings.ContainsAny(password, "\n\r") {
		return "", errors.New("パスワードに改行は使えない")
	}
	f, err := os.CreateTemp("", "sakutei-sqlmigrate-pgpass-")
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

// pgVersionQuery は schema_migrations の最大のバージョン（表が無ければ 0）を返す。無い表は
// 文の中で参照できないので、psql の \gset と \if で分ける。
const pgVersionQuery = `SELECT to_regclass('schema_migrations') IS NOT NULL AS sakutei_has_table \gset
\if :sakutei_has_table
SELECT coalesce(max(version), 0) FROM schema_migrations;
\else
SELECT 0;
\endif
`

func (p *Provider) pgCurrentVersion(ctx context.Context, db *database, w *core.WriteContext) (int64, error) {
	out, err := p.psqlRun(ctx, db, w, pgVersionQuery)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseInt(out, 10, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("schema_migrations の max(version) %q が読めない", out)
	}
	return v, nil
}

const pgCreateTable = "CREATE TABLE IF NOT EXISTS schema_migrations (version bigint PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());"

// pgPrologue はトランザクションを始め、ロックを lock（待つ長さ）まで待って取り、バージョンが want のままかを
// 確かめる（E-F2）。ロックを取ったあとは lock_timeout を戻す（マイグレーションの文の
// 表のロックの待ちには効かせない）。
func pgPrologue(want int64, create bool, lock time.Duration) string {
	ms := max(lock.Milliseconds(), 1)
	s := "BEGIN; SET LOCAL lock_timeout = '" + strconv.FormatInt(ms, 10) + "ms'; SELECT pg_advisory_xact_lock(" +
		strconv.FormatInt(pgLockKey, 10) + "); RESET lock_timeout; "
	if create {
		s += pgCreateTable + " "
	}
	w := strconv.FormatInt(want, 10)
	return s + "DO $sakutei$ BEGIN IF (SELECT coalesce(max(version), 0) FROM schema_migrations) <> " + w +
		" THEN RAISE EXCEPTION 'sakutei: schema_migrations is no longer at version " + w +
		" (another runner migrated this database)' USING ERRCODE = '55000'; END IF; END $sakutei$; "
}

// pgUpScript は 1 本の up をトランザクションで包み、同じトランザクションで完了の記録を
// 書く。SQL の 1 行目は BEGIN と同じ行に続ける（psql のエラーの「psql:<stdin>:N」が
// ファイルの行と一致するように）。
func pgUpScript(v int64, sql string, lock time.Duration) string {
	return pgPrologue(v-1, true, lock) + sql + "\n;\n" +
		"INSERT INTO schema_migrations (version) VALUES (" + strconv.FormatInt(v, 10) + ");\n" + versionMark + "COMMIT;\n"
}

// pgDownScript は 1 本の down をトランザクションで包み、同じトランザクションで完了の記録を消す。
func pgDownScript(v int64, sql string, lock time.Duration) string {
	return pgPrologue(v, false, lock) + sql + "\n;\n" +
		"DELETE FROM schema_migrations WHERE version = " + strconv.FormatInt(v, 10) + ";\n" + versionMark + "COMMIT;\n"
}
