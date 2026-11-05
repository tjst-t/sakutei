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

// engine: postgresql の単体テスト。psql は偽物（fakePG、runner）で、本物の
// PostgreSQL に向けたものは integration（build tag integration_v3_postgresql）。

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/sdk/contract"
)

const (
	pgObservePW = "ro-pw:with\\colon"
	pgWritePW   = "migrate-pw-secret"
)

var ordersID = core.ID{Type: "db/database", Name: "orders"}

// fakePG は偽の psql：sqlmigrate が流すスクリプト（バージョンの問い合わせと 1 本ごとの
// BEGIN … COMMIT）だけを解釈し、schema_migrations のバージョンをメモリに持つ。本文に
// FAIL_HERE があれば SQL のエラー、CONCURRENTLY があれば 25001 で失敗する
// （トランザクションは巻き戻る＝バージョンは変わらない）。
type fakePG struct {
	mu          sync.Mutex
	version     int64
	hasTable    bool
	unreachable bool
	authFail    bool
	missing     bool
	broken      bool
	locked      bool // advisory lock を待ちきれない（55P03）
	lostAfter   bool // 1 本を確定したあと接続が切れたと答える
	committed   []string
	calls       []invocation
	users       []string      // 呼び出しごとの PGUSER
	passwords   []string      // passfile に書かれていたパスワード
	beforeStep  func(*fakePG) // 1 本のスクリプトを解釈する前に呼ぶ（ほかの処理の真似）
}

var (
	fakePrologue = regexp.MustCompile(`^BEGIN; SET LOCAL lock_timeout = '[0-9]+ms'; SELECT pg_advisory_xact_lock\(` + strconv.FormatInt(pgLockKey, 10) + `\); RESET lock_timeout; `)
	fakeGuard    = regexp.MustCompile(`<> ([0-9]+) THEN`)
	fakeBody     = regexp.MustCompile(`(?s)END \$sakutei\$; (.*)\n;\n(INSERT INTO schema_migrations \(version\) VALUES \(([0-9]+)\)|DELETE FROM schema_migrations WHERE version = ([0-9]+));\nSELECT 'sakutei-version:' [^\n]*\nCOMMIT;\n$`)
)

func (f *fakePG) run(_ context.Context, inv invocation) (result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, inv)
	if f.missing {
		return result{}, exec.ErrNotFound
	}
	for _, kv := range inv.Env {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "PGUSER":
			f.users = append(f.users, v)
		case "PGPASSFILE":
			b, err := os.ReadFile(v)
			if err != nil {
				return result{ExitCode: 2, Stderr: "no passfile"}, nil
			}
			pw := strings.TrimSuffix(strings.TrimPrefix(string(b), "*:*:*:*:"), "\n")
			f.passwords = append(f.passwords, strings.NewReplacer(`\:`, `:`, `\\`, `\`).Replace(pw))
		}
	}
	switch {
	case f.unreachable:
		return result{Stderr: `psql: error: connection to server at "db" failed: Connection refused`, ExitCode: 2}, nil
	case f.authFail:
		return result{Stderr: `psql: error: connection to server at "db" failed: FATAL:  28P01: password authentication failed for user "x"`, ExitCode: 2}, nil
	case f.broken:
		return result{Stdout: "garbage\n"}, nil
	}
	sqlErr := func(state, msg string) (result, error) {
		return result{Stderr: "psql:<stdin>:1: ERROR:  " + state + ": " + msg, ExitCode: 3}, nil
	}
	switch {
	case inv.Stdin == pgVersionQuery:
		if !f.hasTable {
			return result{Stdout: "0\n"}, nil
		}
		return result{Stdout: strconv.FormatInt(f.version, 10) + "\n"}, nil
	case fakePrologue.MatchString(inv.Stdin):
		if f.beforeStep != nil {
			f.beforeStep(f)
		}
		if f.locked {
			return sqlErr("55P03", "canceling statement due to lock timeout")
		}
		g := fakeGuard.FindStringSubmatch(inv.Stdin)
		m := fakeBody.FindStringSubmatch(inv.Stdin)
		if g == nil || m == nil {
			return sqlErr("42601", "the fake psql does not understand the script")
		}
		if want, _ := strconv.ParseInt(g[1], 10, 64); (f.hasTable && f.version != want) || (!f.hasTable && want != 0) {
			return sqlErr("55000", "sakutei: schema_migrations is no longer at version "+g[1]+" (another runner migrated this database)")
		}
		body := m[1]
		switch {
		case strings.Contains(body, "FAIL_HERE"):
			return sqlErr("42P01", `relation "nope" does not exist`)
		case strings.Contains(body, "CONCURRENTLY"):
			return sqlErr("25001", "CREATE INDEX CONCURRENTLY cannot run inside a transaction block")
		}
		if m[3] != "" {
			f.version, _ = strconv.ParseInt(m[3], 10, 64)
			f.hasTable = true
		} else {
			v, _ := strconv.ParseInt(m[4], 10, 64)
			f.version = v - 1
		}
		f.committed = append(f.committed, body)
		if f.lostAfter {
			return result{Stderr: "psql:<stdin>:3: server closed the connection unexpectedly", ExitCode: 2}, nil
		}
		return result{Stdout: "\nsakutei-version:" + strconv.FormatInt(f.version, 10) + "\n"}, nil
	}
	return sqlErr("42601", "unexpected script")
}

// staticBackend は secret backend の偽物。取り出した回数を数える（E-G5）。
type staticBackend struct {
	mu       sync.Mutex
	values   map[string]string
	resolved int
}

func (b *staticBackend) Generation(context.Context, string) (string, error) { return "1", nil }

func (b *staticBackend) Resolve(_ context.Context, path, _ string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resolved++
	if v, ok := b.values[path]; ok {
		return v, nil
	}
	return "", errors.New("no such secret")
}

var pgMigrations = map[string]string{
	"0001_users.up.sql":    "CREATE TABLE users (id bigint PRIMARY KEY, name text);\n",
	"0001_users.down.sql":  "DROP TABLE users;\n",
	"0002_email.up.sql":    "ALTER TABLE users ADD COLUMN email text;\nCOMMENT ON COLUMN users.email IS E'mail\\naddress'", // 最後の ";" 無し・E'' の \
	"0002_email.down.sql":  "ALTER TABLE users DROP COLUMN email;\n",
	"0003_orders.up.sql":   "CREATE TABLE orders (id bigint PRIMARY KEY, body text DEFAULT $$a\\b$$);\n",
	"0003_orders.down.sql": "DROP TABLE orders;\n",
}

type pgFixture struct {
	p       *Provider
	fake    *fakePG
	secrets *staticBackend
	dir     string
}

func (f pgFixture) w() core.WriteContext {
	return core.NewWriteContext(core.SecretBackends{"file": f.secrets})
}

func (f pgFixture) invoke(op string, args core.Fields) error {
	return f.p.Invoke(context.Background(), f.w(), ordersID, op, args)
}

func (f pgFixture) version(t *testing.T) int64 {
	t.Helper()
	o, _ := f.p.Observe(context.Background(), []core.ID{ordersID})
	if o[ordersID].Availability != core.Present {
		t.Fatalf("観測が %s: %s", o[ordersID].Availability, o[ordersID].Note)
	}
	return o[ordersID].Fields[FieldVersion].(int64)
}

// newPG は observe を password_file、write を password_secret（書き込みの文脈でだけ
// 取り出す）にした PostgreSQL のデータベース orders を作る。
func newPG(t *testing.T, files map[string]string) pgFixture {
	t.Helper()
	dir := t.TempDir()
	mdir := filepath.Join(dir, "migrations")
	writeFiles(t, mdir, files)
	pwFile := filepath.Join(dir, "ro-password")
	if err := os.WriteFile(pwFile, []byte(pgObservePW+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pr, err := Module(map[string]any{"psql": "/opt/pg/bin/psql", "databases": map[string]any{
		"orders": map[string]any{"engine": "postgresql", "host": "db.example.net", "port": 5433, "database": "orders_db",
			"sslmode": "verify-full", "connect_timeout": "3s", "migrations": mdir,
			"observe": map[string]any{"user": "sakutei_ro", "password_file": pwFile},
			"write":   map[string]any{"user": "sakutei_migrate", "password_secret": "file:pg/migrate"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	f := pgFixture{p: pr.(*Provider), fake: &fakePG{}, secrets: &staticBackend{values: map[string]string{"pg/migrate": pgWritePW}},
		dir: dir}
	f.p.run = f.fake.run
	return f
}

func TestPGContract(t *testing.T) {
	contract.Run(t, func(t *testing.T) contract.Target {
		f := newPG(t, pgMigrations)
		return contract.Target{
			Provider:  f.p,
			Resources: []core.Resource{{ID: ordersID, Fields: core.Fields{FieldVersion: int64(3)}}},
			Observed:  []core.ID{ordersID},
			Faults: map[string]func(){
				"接続を切る":         func() { f.fake.unreachable = true },
				"認証の失敗":         func() { f.fake.authFail = true },
				"psql が無い":      func() { f.fake.missing = true },
				"壊れた出力":         func() { f.fake.broken = true },
				"パスワードのファイルが無い": func() { os.Remove(filepath.Join(f.dir, "ro-password")) },
			},
			Secrets:              []string{pgObservePW, pgWritePW},
			WriteCredentialReads: func() int { return f.secrets.resolved },
			Secret:               core.SecretBackends{"file": f.secrets},
			Invokes: []contract.Invoke{
				{ID: ordersID, Op: OpMigrate, Args: core.Fields{"version": "3"},
					NotSent: func() { f.fake.locked = true },
					Failed: func() {
						writeFiles(t, filepath.Join(f.dir, "migrations"), map[string]string{"0001_users.up.sql": "SELECT FAIL_HERE;\n"})
					}},
				{ID: ordersID, Op: OpMigrate, Args: core.Fields{"version": "2"},
					NotSent: func() { f.fake.unreachable = true },
					Failed:  func() { f.fake.lostAfter = true }},
			},
			Refuses: []contract.Refuse{{ID: ordersID}},
		}
	})
}

// 観測は observe の credential（ファイル）、invoke は write の credential（secret の参照、書き込みの
// 文脈でだけ取り出す）。接続の設定は環境変数で、パスワードは passfile だけで渡る
// （E-G2・E-G5・N-G3）。
func TestPGUpAndDownWithCredentials(t *testing.T) {
	f := newPG(t, pgMigrations)
	if v := f.version(t); v != 0 {
		t.Fatalf("初めのバージョン %d", v)
	}
	if f.secrets.resolved != 0 {
		t.Fatal("観測で書き込み用の secret を取り出した")
	}
	if err := f.invoke(OpMigrate, core.Fields{"version": "3"}); err != nil {
		t.Fatal(err)
	}
	if v := f.version(t); v != 3 || len(f.fake.committed) != 3 {
		t.Fatalf("バージョン %d・確定 %d（want 3・3）", v, len(f.fake.committed))
	}
	if err := f.invoke(OpMigrate, core.Fields{"version": "1"}); err != nil {
		t.Fatal(err)
	}
	if got := f.fake.committed[3:]; len(got) != 2 || !strings.Contains(got[0], "DROP TABLE orders") || !strings.Contains(got[1], "DROP COLUMN email") {
		t.Fatalf("down の順 %q", got)
	}
	// 観測（初めと最後）は sakutei_ro、invoke の中（読み直しを含む）は sakutei_migrate。
	f.version(t)
	if f.fake.users[0] != "sakutei_ro" || f.fake.users[len(f.fake.users)-1] != "sakutei_ro" {
		t.Fatalf("観測の利用者 %v", f.fake.users)
	}
	for i, c := range f.fake.calls {
		env := strings.Join(c.Env, "\n")
		if c.Path != "/opt/pg/bin/psql" {
			t.Fatalf("psql のパス %q", c.Path)
		}
		for _, want := range []string{"PGHOST=db.example.net", "PGPORT=5433", "PGDATABASE=orders_db", "PGSSLMODE=verify-full", "PGPASSFILE=", "PGCONNECT_TIMEOUT=3"} {
			if !strings.Contains(env, want) {
				t.Errorf("環境変数に %q が無い:\n%s", want, env)
			}
		}
		all := env + strings.Join(c.Args, " ") + c.Stdin
		if strings.Contains(all, pgObservePW) || strings.Contains(all, pgWritePW) {
			t.Error("パスワードが環境変数・引数・SQL に出た")
		}
		want := pgObservePW
		if f.fake.users[i] == "sakutei_migrate" {
			want = pgWritePW
		}
		if f.fake.passwords[i] != want {
			t.Errorf("%d 回目（%s）の passfile のパスワードが違う", i, f.fake.users[i])
		}
		if c.Stdin != pgVersionQuery && f.fake.users[i] != "sakutei_migrate" {
			t.Errorf("書くスクリプトを %s で流した", f.fake.users[i])
		}
	}
	if f.secrets.resolved == 0 {
		t.Fatal("invoke で書き込み用の secret を取り出していない")
	}
	ents, _ := os.ReadDir(os.TempDir())
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "sakutei-sqlmigrate-pgpass-") {
			t.Errorf("passfile が残った: %s", e.Name())
		}
	}
}

// 書き込み用の credential を取り出せない（secret が無い）なら、何も送らず ErrNotSent（E-G5）。
// 観測は書き込み用の credential が無くても読める。
func TestPGWriteCredentialMissing(t *testing.T) {
	f := newPG(t, pgMigrations)
	f.secrets.values = map[string]string{}
	if v := f.version(t); v != 0 {
		t.Fatal(v)
	}
	if err := f.invoke(OpMigrate, core.Fields{"version": "3"}); !errors.Is(err, core.ErrNotSent) {
		t.Fatalf("%v（want ErrNotSent）", err)
	}
	if err := f.p.Invoke(context.Background(), core.NewWriteContext(nil), ordersID, OpMigrate, core.Fields{"version": "3"}); !errors.Is(err, core.ErrNotSent) {
		t.Fatalf("secret の取り出し先が無いのに %v", err)
	}
	if len(f.fake.committed) != 0 {
		t.Fatal("流した")
	}
}

func TestPGScript(t *testing.T) {
	s := pgUpScript(2, "ALTER TABLE users ADD COLUMN email text", DefaultLockTimeout)
	for _, want := range []string{"BEGIN; SET LOCAL lock_timeout = '5000ms'; SELECT pg_advisory_xact_lock(", "RESET lock_timeout;",
		"CREATE TABLE IF NOT EXISTS schema_migrations", "<> 1 THEN",
		"ALTER TABLE users ADD COLUMN email text\n;\nINSERT INTO schema_migrations (version) VALUES (2);\n" + versionMark + "COMMIT;\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("up のスクリプトに %q が無い:\n%s", want, s)
		}
	}
	if strings.Count(strings.SplitN(s, "ALTER", 2)[0], "\n") != 0 {
		t.Errorf("マイグレーションは 1 行目から始める（psql の行番号＝ファイルの行番号）:\n%s", s)
	}
	d := pgDownScript(2, "ALTER TABLE users DROP COLUMN email;", 0)
	if !strings.Contains(d, "lock_timeout = '1ms'") || !strings.Contains(d, "<> 2 THEN") || !strings.Contains(d, "DELETE FROM schema_migrations WHERE version = 2;\n"+versionMark+"COMMIT;\n") || strings.Contains(d, "CREATE TABLE") {
		t.Errorf("down のスクリプト:\n%s", d)
	}
}

func TestPGFailedMigration(t *testing.T) {
	for _, c := range []struct{ body, state string }{
		{"CREATE TABLE half (x int);\nINSERT INTO nope VALUES (1); -- FAIL_HERE\n", "42P01"},
		// トランザクションに入れられない文は特別扱いせず、失敗として報告する。
		{"CREATE INDEX CONCURRENTLY users_name ON users (name);\n", "25001"},
	} {
		files := copyFiles(pgMigrations)
		files["0002_email.up.sql"] = c.body
		f := newPG(t, files)
		err := f.invoke(OpMigrate, core.Fields{"version": "3"})
		if err == nil || errors.Is(err, core.ErrNotSent) || !strings.Contains(err.Error(), "0002_email.up.sql") || !strings.Contains(err.Error(), "バージョン 1") || !strings.Contains(err.Error(), c.state) {
			t.Errorf("%s: %v（ファイル・残ったバージョン・SQLSTATE が要る）", c.state, err)
		}
		if v := f.version(t); v != 1 {
			t.Errorf("%s: バージョン %d（want 1）", c.state, v)
		}
	}
}

func TestPGRollbackOnFail(t *testing.T) {
	files := copyFiles(pgMigrations)
	files["0003_orders.up.sql"] = "CREATE TABLE orders (id int); SELECT FAIL_HERE;\n"
	f := newPG(t, files)
	if err := f.invoke(OpMigrate, core.Fields{"version": "1"}); err != nil {
		t.Fatal(err)
	}
	err := f.invoke(OpMigrate, core.Fields{"version": "3", "rollback_on_fail": "true"})
	if err == nil || errors.Is(err, core.ErrNotSent) || !strings.Contains(err.Error(), "バージョン 1 に戻した") || !strings.Contains(err.Error(), "42P01") {
		t.Fatalf("%v", err)
	}
	if v := f.version(t); v != 1 {
		t.Fatalf("バージョン %d（want 1）", v)
	}
	if got := f.fake.committed; len(got) != 3 || !strings.Contains(got[2], "DROP COLUMN email") {
		t.Fatalf("確定 %q（want 0001・0002・0002 の down）", got)
	}
}

// ほかの処理が先にバージョンを動かしていたら、ロックのあとの確かめで分かり何も流さない。
// 読み直して残りを流す（同じバージョンを二度流さない、E-F2）。
func TestPGAnotherRunnerMovedTheVersion(t *testing.T) {
	f := newPG(t, pgMigrations)
	f.fake.beforeStep = func(fk *fakePG) { fk.version, fk.hasTable, fk.beforeStep = 1, true, nil }
	if err := f.invoke(OpMigrate, core.Fields{"version": "3"}); err != nil {
		t.Fatal(err)
	}
	if got := f.fake.committed; len(got) != 2 || !strings.Contains(got[0], "ADD COLUMN email") {
		t.Fatalf("確定 %q（want 0002 と 0003 だけ）", got)
	}
	// ほかの処理が宣言のバージョンまで済ませていたら、何も流さずに済む。
	f = newPG(t, pgMigrations)
	f.fake.beforeStep = func(fk *fakePG) { fk.version, fk.hasTable, fk.beforeStep = 3, true, nil }
	if err := f.invoke(OpMigrate, core.Fields{"version": "3"}); err != nil || len(f.fake.committed) != 0 {
		t.Fatalf("%v・確定 %q", err, f.fake.committed)
	}
	// 動かし続けるなら止める。
	f = newPG(t, pgMigrations)
	f.fake.beforeStep = func(fk *fakePG) { fk.hasTable, fk.version = true, fk.version+7 }
	if err := f.invoke(OpMigrate, core.Fields{"version": "3"}); err == nil || errors.Is(err, core.ErrNotSent) {
		t.Fatalf("%v", err)
	}
}

// ロックを待ちきれない・つながらないのは、まだ 1 本も確定していなければ ErrNotSent。
// 1 本でも確定したあと・接続が途中で切れた（確定したか分からない）は止めるエラー
// （不変条件 16、E-A2）。
func TestPGNotSent(t *testing.T) {
	f := newPG(t, pgMigrations)
	f.fake.locked = true
	if err := f.invoke(OpMigrate, core.Fields{"version": "3"}); !errors.Is(err, core.ErrNotSent) || !strings.Contains(err.Error(), "55P03") {
		t.Fatalf("ロック: %v", err)
	}
	f.fake.locked = false
	n := 0
	f.fake.beforeStep = func(fk *fakePG) {
		n++
		if n == 2 {
			fk.locked = true
		}
	}
	if err := f.invoke(OpMigrate, core.Fields{"version": "3"}); err == nil || errors.Is(err, core.ErrNotSent) || !strings.Contains(err.Error(), "バージョン 1 まで流した") {
		t.Fatalf("1 本確定したあとのロック: %v", err)
	}
	f = newPG(t, pgMigrations)
	f.fake.lostAfter = true
	if err := f.invoke(OpMigrate, core.Fields{"version": "3"}); err == nil || errors.Is(err, core.ErrNotSent) || !strings.Contains(err.Error(), "確定したか分からない") {
		t.Fatalf("接続が切れた: %v", err)
	}
	f = newPG(t, pgMigrations)
	f.fake.missing = true
	if err := f.invoke(OpMigrate, core.Fields{"version": "3"}); !errors.Is(err, core.ErrNotSent) {
		t.Fatalf("psql が無い: %v", err)
	}
}

func TestPGMetaCommandRefused(t *testing.T) {
	files := copyFiles(pgMigrations)
	files["0002_email.up.sql"] = "ALTER TABLE users ADD COLUMN email text;\n\\! touch /tmp/pwned\n"
	f := newPG(t, files)
	err := f.invoke(OpMigrate, core.Fields{"version": "3"})
	if err == nil || errors.Is(err, core.ErrNotSent) || !strings.Contains(err.Error(), "0002_email.up.sql:2") || !strings.Contains(err.Error(), "メタコマンド") {
		t.Fatalf("%v", err)
	}
	if len(f.fake.committed) != 0 {
		t.Fatalf("拒否する前に流した %q（流す前に全部の SQL を読む）", f.fake.committed)
	}
}

func TestPsqlMetaCommand(t *testing.T) {
	for _, c := range []struct {
		sql  string
		line int // 0 = 無い
	}{
		{"SELECT 1;", 0},
		{"SELECT 'a\\b';", 0},              // 標準の文字列の中
		{"SELECT E'it\\'s \\\\';", 0},      // E'' の escape
		{"SELECT e'\\n', 'x''y';", 0},      // 小文字の e と '' の escape
		{`SELECT "we\ird";`, 0},            // 引用符の識別子
		{"SELECT $$\\x$$, $f$\\$f$;", 0},   // dollar quote
		{"SELECT $1, a$b FROM t;\n\\q", 2}, // $1 は引数、a$b は識別子
		{"SELECT 1;\n\\i other.sql", 2},
		{"SELECT 1; \\gexec", 1},
		{"SELECT 1; -- \\! rm\n", 1},     // コメントの中も拒否する
		{"/* x\n\\! rm */ SELECT 1;", 2}, // 入れ子のコメントの中も拒否する
		{"SELECT 'never closed \\", 0},
		{"SELECT xE'\\';\n\\q", 2}, // xE は識別子なので E'' ではない：'\' で閉じ、次の行の \q はメタコマンド
	} {
		line, ok := psqlMetaCommand(c.sql)
		if (c.line != 0) != ok || line != c.line {
			t.Errorf("%q: 行 %d %v（want %d）", c.sql, line, ok, c.line)
		}
	}
}

func TestPGObserve(t *testing.T) {
	f := newPG(t, pgMigrations)
	f.fake.hasTable, f.fake.version = true, 2
	o, _ := f.p.Observe(context.Background(), []core.ID{ordersID, {Type: "db/database", Name: "nope"}})
	if got := o[ordersID]; got.Availability != core.Present || got.Fields[FieldVersion] != int64(2) || got.Fields[FieldPending] != int64(1) || got.Fields[FieldLatest] != int64(3) {
		t.Fatalf("%+v", got)
	}
	if o[core.ID{Type: "db/database", Name: "nope"}].Availability != core.Unknown {
		t.Fatal("設定に無いデータベースが Unknown でない")
	}
	// observe の password_secret は書けない（観測は書き込みの文脈を持たない）。
	if _, err := Module(map[string]any{"databases": map[string]any{"o": map[string]any{"engine": "postgresql", "host": "h",
		"migrations": f.dir, "observe": map[string]any{"user": "u", "password_secret": "file:x"}}}}); err == nil {
		t.Fatal("observe の password_secret を受けた")
	}
}
