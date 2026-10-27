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

// engine: postgresql（#126）の単体テスト。psql は偽物（fakePG、postgresql.Runner）で、
// 本物の PostgreSQL に向けたものは tests/integration/postgresql（make
// integration-postgresql）。

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
	"time"

	"github.com/tjst-t/sakutei/v2/internal/postgresql"
	"github.com/tjst-t/sakutei/v2/internal/task"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

const pgAdminPW = "pg-admin:pw"

// fakePG は偽の psql：sqlmigrate が流す台本（版の問い合わせと 1 本ごとの
// BEGIN … COMMIT）だけを解釈し、schema_migrations の版をメモリに持つ。本文に
// FAIL_HERE があれば SQL のエラー、CONCURRENTLY があれば 25001 で失敗する
// （トランザクションは巻き戻る＝版は変わらない）。
type fakePG struct {
	mu          sync.Mutex
	version     int64
	hasTable    bool
	unreachable bool
	missing     bool
	committed   []string // 通った本文
	calls       []postgresql.Invocation
	beforeStep  func(f *fakePG) // 1 本の台本を解釈する前に呼ぶ（ほかのデーモンの真似）
	passwords   []string        // passfile に書かれていたパスワード
}

var (
	fakeGuard = regexp.MustCompile(`<> ([0-9]+) THEN`)
	fakeBody  = regexp.MustCompile(`(?s)END \$sakutei\$; (.*)\n;\n(INSERT INTO schema_migrations \(version\) VALUES \(([0-9]+)\)|DELETE FROM schema_migrations WHERE version = ([0-9]+));\nCOMMIT;\n$`)
)

func (f *fakePG) run(_ context.Context, inv postgresql.Invocation) (postgresql.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, inv)
	if f.missing {
		return postgresql.Result{}, exec.ErrNotFound
	}
	if f.unreachable {
		return postgresql.Result{Stderr: "psql: error: connection to server at \"db\" failed: Connection refused", ExitCode: 2}, nil
	}
	for _, kv := range inv.Env {
		if k, v, _ := strings.Cut(kv, "="); k == "PGPASSFILE" {
			b, err := os.ReadFile(v)
			if err != nil {
				return postgresql.Result{ExitCode: 2, Stderr: "no passfile"}, nil
			}
			pw := strings.TrimSuffix(strings.TrimPrefix(string(b), "*:*:*:*:"), "\n")
			f.passwords = append(f.passwords, strings.NewReplacer(`\:`, `:`, `\\`, `\`).Replace(pw))
		}
	}
	sqlErr := func(state, msg string) (postgresql.Result, error) {
		return postgresql.Result{Stderr: "psql:<stdin>:1: ERROR:  " + state + ": " + msg, ExitCode: 3}, nil
	}
	switch {
	case inv.Stdin == pgVersionQuery:
		if !f.hasTable {
			return postgresql.Result{Stdout: "0\n"}, nil
		}
		return postgresql.Result{Stdout: strconv.FormatInt(f.version, 10) + "\n"}, nil
	case strings.HasPrefix(inv.Stdin, "BEGIN; SELECT pg_advisory_xact_lock("+strconv.FormatInt(pgLockKey, 10)+"); "):
		if f.beforeStep != nil {
			f.beforeStep(f)
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
		return postgresql.Result{Stdout: "\n"}, nil
	}
	return sqlErr("42601", "unexpected script")
}

type staticSecrets map[string]string

func (s staticSecrets) ResolveValue(_ context.Context, backend, path string, _ int64) (string, error) {
	if v, ok := s[backend+"/"+path]; ok {
		return v, nil
	}
	return "", errors.New("no such secret")
}

type pgFixture struct {
	p    *Provider
	db   Database
	fake *fakePG
	dir  string
}

var pgMigrations = map[string]string{
	"0001_users.up.sql":    "CREATE TABLE users (id bigint PRIMARY KEY, name text);\n",
	"0001_users.down.sql":  "DROP TABLE users;\n",
	"0002_email.up.sql":    "ALTER TABLE users ADD COLUMN email text;\nCOMMENT ON COLUMN users.email IS E'mail\\naddress'", // 最後の ";" 無し・E'' の \
	"0002_email.down.sql":  "ALTER TABLE users DROP COLUMN email;\n",
	"0003_orders.up.sql":   "CREATE TABLE orders (id bigint PRIMARY KEY, body text DEFAULT $$a\\b$$);\n",
	"0003_orders.down.sql": "DROP TABLE orders;\n",
}

func newPGFixture(t *testing.T, files map[string]string) pgFixture {
	t.Helper()
	dir := t.TempDir()
	mdir := filepath.Join(dir, "migrations")
	if err := os.Mkdir(mdir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(mdir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fake := &fakePG{}
	db := Database{Engine: EnginePostgreSQL, Server: "main", DBName: "orders", Migrations: mdir}
	p := New(map[string]Database{"app": db}, Options{
		PostgreSQLServers: map[string]postgresql.Server{"main": {Host: "db.example.net", Port: 5433, DBName: "postgres", User: "migrator",
			HasPassword: true, Password: core.NewSecretRef("file", "pg/migrator", 1), ConnectTimeout: 3 * time.Second}},
		PostgreSQL: postgresql.Options{Psql: "/opt/pg/bin/psql", Runner: fake.run, Secrets: staticSecrets{"file/pg/migrator": pgAdminPW}},
	})
	return pgFixture{p: p, db: db, fake: fake, dir: dir}
}

func (f pgFixture) version(t *testing.T) int64 {
	t.Helper()
	obs, err := f.p.Read(context.Background(), "app")
	if err != nil || obs.Availability != core.AvailabilityOk {
		t.Fatalf("Read = %+v, %v", obs, err)
	}
	return obs.State[FieldVersion].Value.(int64)
}

func TestPG_RunTask_UpAndDown(t *testing.T) {
	f := newPGFixture(t, pgMigrations)
	ctx := context.Background()
	if v := f.version(t); v != 0 {
		t.Fatalf("version before = %d, want 0 (no schema_migrations yet)", v)
	}
	if err := f.p.RunTask(ctx, req("t1", "3")); err != nil {
		t.Fatalf("RunTask up: %v", err)
	}
	if v := f.version(t); v != 3 {
		t.Fatalf("version = %d, want 3", v)
	}
	if len(f.fake.committed) != 3 {
		t.Fatalf("committed %d migrations, want 3 (one transaction each)", len(f.fake.committed))
	}
	if err := f.p.RunTask(ctx, req("t2", "1")); err != nil {
		t.Fatalf("RunTask down: %v", err)
	}
	if v := f.version(t); v != 1 {
		t.Fatalf("version = %d, want 1", v)
	}
	if got := f.fake.committed[3:]; len(got) != 2 || !strings.Contains(got[0], "DROP TABLE orders") || !strings.Contains(got[1], "DROP COLUMN email") {
		t.Errorf("down ran %q, want 0003 then 0002", got)
	}

	// 接続の設定は postgresql provider と同じ口（環境変数・passfile）で渡る。
	for _, c := range f.fake.calls {
		if c.Path != "/opt/pg/bin/psql" {
			t.Fatalf("psql path = %q", c.Path)
		}
		env := strings.Join(c.Env, "\n")
		for _, want := range []string{"PGHOST=db.example.net", "PGPORT=5433", "PGDATABASE=orders", "PGUSER=migrator", "PGPASSFILE=", "PGCONNECT_TIMEOUT=3"} {
			if !strings.Contains(env, want) {
				t.Errorf("env does not have %q:\n%s", want, env)
			}
		}
		if strings.Contains(env+strings.Join(c.Args, " ")+c.Stdin, pgAdminPW) {
			t.Errorf("the password appears in the env, args or SQL")
		}
		if !strings.Contains(strings.Join(c.Args, " "), "ON_ERROR_STOP=1") {
			t.Errorf("args = %q, want ON_ERROR_STOP=1", c.Args)
		}
	}
	for _, pw := range f.fake.passwords {
		if pw != pgAdminPW {
			t.Errorf("passfile password = %q", pw)
		}
	}
	if len(f.fake.passwords) != len(f.fake.calls) {
		t.Errorf("passfile on %d of %d calls", len(f.fake.passwords), len(f.fake.calls))
	}
}

func TestPG_Script(t *testing.T) {
	s := pgUpScript(2, "ALTER TABLE users ADD COLUMN email text")
	for _, want := range []string{"BEGIN; SELECT pg_advisory_xact_lock(", "CREATE TABLE IF NOT EXISTS schema_migrations", "<> 1 THEN",
		"ALTER TABLE users ADD COLUMN email text\n;\nINSERT INTO schema_migrations (version) VALUES (2);\nCOMMIT;\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("up script does not have %q:\n%s", want, s)
		}
	}
	if strings.Count(strings.SplitN(s, "ALTER", 2)[0], "\n") != 0 {
		t.Errorf("the migration must start on the first line (psql line numbers = file line numbers):\n%s", s)
	}
	d := pgDownScript(2, "ALTER TABLE users DROP COLUMN email;")
	if !strings.Contains(d, "<> 2 THEN") || !strings.Contains(d, "DELETE FROM schema_migrations WHERE version = 2;\nCOMMIT;\n") || strings.Contains(d, "CREATE TABLE") {
		t.Errorf("down script:\n%s", d)
	}
}

func TestPG_FailedMigrationLeavesNoTrace(t *testing.T) {
	for _, c := range []struct{ body, state string }{
		{"CREATE TABLE half (x int);\nINSERT INTO nope VALUES (1); -- FAIL_HERE\n", "42P01"},
		// トランザクションに入れられない文は特別扱いせず、失敗として知らせる。
		{"CREATE INDEX CONCURRENTLY users_name ON users (name);\n", "25001"},
	} {
		files := copyFiles(pgMigrations)
		files["0002_email.up.sql"] = c.body
		f := newPGFixture(t, files)
		err := f.p.RunTask(context.Background(), req("t1", "3"))
		if err == nil {
			t.Fatalf("%s: RunTask succeeded", c.state)
		}
		if !strings.Contains(err.Error(), "0002_email.up.sql") || !strings.Contains(err.Error(), "version 1") || !strings.Contains(err.Error(), c.state) {
			t.Errorf("%s: error = %v, want the file, the version left and the SQLSTATE", c.state, err)
		}
		if v := f.version(t); v != 1 {
			t.Errorf("%s: version = %d, want 1", c.state, v)
		}
	}
}

func TestPG_ManagerRollsBackAndHalts(t *testing.T) {
	files := copyFiles(pgMigrations)
	files["0003_orders.up.sql"] = "CREATE TABLE orders (id int); SELECT FAIL_HERE;\n"
	f := newPGFixture(t, files)
	ctx := context.Background()
	if err := f.p.RunTask(ctx, req("setup", "1")); err != nil {
		t.Fatal(err)
	}
	store := task.NewFileStore(filepath.Join(f.dir, "tasks.json"))
	m := task.NewManager(store, f.p, "test", nil)
	pol := core.UpdatePolicy{Strategy: core.UpdateStrategyTask, RollbackOnFail: true}
	if dec, err := m.Trigger(ctx, appID, map[string]string{FieldVersion: "3"}, core.Plan{}, pol); err != nil || dec.Outcome != task.Launched {
		t.Fatalf("Trigger = %+v, %v", dec, err)
	}
	m.Wait(30 * time.Second)
	rec, ok, err := store.Get(ctx, appID.String())
	if err != nil || !ok || rec.Phase != task.PhaseFailed || !rec.RolledBack || !strings.Contains(rec.Error, "rolled back: the database is at version 1") {
		t.Fatalf("record = %+v, %v, %v", rec, ok, err)
	}
	if v := f.version(t); v != 1 {
		t.Errorf("version = %d, want 1", v)
	}
}

// ほかのデーモンが先に進めた版は、鍵を取った後の確かめで分かり、何も流さない。
func TestPG_AnotherRunnerMovedTheVersion(t *testing.T) {
	f := newPGFixture(t, pgMigrations)
	f.fake.beforeStep = func(fk *fakePG) {
		fk.version, fk.hasTable, fk.beforeStep = 1, true, nil
	}
	err := f.p.RunTask(context.Background(), req("t1", "3"))
	if err == nil || !strings.Contains(err.Error(), "55000") || !strings.Contains(err.Error(), "another runner") {
		t.Fatalf("RunTask = %v, want the guard to fail", err)
	}
	if len(f.fake.committed) != 0 {
		t.Errorf("committed %q, want nothing", f.fake.committed)
	}
}

func TestPG_UnobservableIsUnknown(t *testing.T) {
	for name, breakIt := range map[string]func(f pgFixture){
		"unreachable": func(f pgFixture) { f.fake.unreachable = true },
		"no psql":     func(f pgFixture) { f.fake.missing = true },
		"no secret": func(f pgFixture) {
			f.p.psql = postgresql.NewClient(postgresql.Options{Runner: f.fake.run, Secrets: staticSecrets{}})
		},
	} {
		f := newPGFixture(t, pgMigrations)
		breakIt(f)
		obs, err := f.p.Read(context.Background(), "app")
		if err == nil || obs.Availability != core.AvailabilityUnknown || obs.State != nil {
			t.Errorf("%s: Read = %+v, %v; want Unknown", name, obs, err)
		}
		if err := f.p.RunTask(context.Background(), req("t", "1")); err == nil {
			t.Errorf("%s: RunTask succeeded", name)
		}
	}
}

func TestPG_MetaCommandRefused(t *testing.T) {
	files := copyFiles(pgMigrations)
	files["0002_email.up.sql"] = "ALTER TABLE users ADD COLUMN email text;\n\\! touch /tmp/pwned\n"
	f := newPGFixture(t, files)
	err := f.p.RunTask(context.Background(), req("t1", "3"))
	if err == nil || !strings.Contains(err.Error(), "0002_email.up.sql:2") || !strings.Contains(err.Error(), "meta-command") {
		t.Fatalf("RunTask = %v, want the meta-command refused", err)
	}
	if len(f.fake.committed) != 0 {
		t.Errorf("committed %q before refusing, want nothing (all SQL is read first)", f.fake.committed)
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
		{"SELECT 1; -- \\! rm\n", 1},     // コメントの中も断る
		{"/* x\n\\! rm */ SELECT 1;", 2}, // 入れ子のコメントの中も断る
		{"SELECT 'never closed \\", 0},
		{"SELECT xE'\\';\n\\q", 2}, // xE は識別子なので E'' ではない：'\' で閉じ、次の行の \q はメタコマンド
	} {
		line, ok := psqlMetaCommand(c.sql)
		if (c.line != 0) != ok || line != c.line {
			t.Errorf("%q: line %d %v, want %d", c.sql, line, ok, c.line)
		}
	}
}

func TestPG_ConfigAndValidate(t *testing.T) {
	dir := t.TempDir()
	write := func(name, s string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("secrets.json", `{"secrets": {"pg/migrator": {"gen": 1, "value": "x"}}}`)
	write("pg.yaml", "psql: bin/psql\nsecret_store: secrets.json\nservers:\n  main:\n    host: db\n    user: migrator\n    password: secretref:file/pg/migrator\n")
	if err := os.Mkdir(filepath.Join(dir, "m"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("m/0001_a.up.sql", "SELECT 1;")
	path := write("databases.yaml", "postgresql_config: pg.yaml\ndatabases:\n  app:\n    path: a.db\n    migrations: m\n  orders:\n    engine: postgresql\n    server: main\n    database: orders\n    migrations: m\n")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Databases["orders"]; got.Engine != EnginePostgreSQL || got.Server != "main" || got.DBName != "orders" || got.Path != "" || got.Migrations != filepath.Join(dir, "m") {
		t.Errorf("orders = %+v", got)
	}
	if got := cfg.Databases["app"]; got.Engine != EngineSQLite || got.Path != filepath.Join(dir, "a.db") {
		t.Errorf("app = %+v", got)
	}
	if s := cfg.Options.PostgreSQLServers["main"]; s.User != "migrator" || !s.HasPassword || cfg.Options.PostgreSQL.Psql != filepath.Join(dir, "bin/psql") || cfg.Options.PostgreSQL.Secrets == nil {
		t.Errorf("postgresql options = %+v %+v", s, cfg.Options.PostgreSQL)
	}
	p, err := NewFromConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.ValidateIntent("orders", map[string]string{FieldVersion: "1"}); err != nil {
		t.Errorf("ValidateIntent: %v", err)
	}
	if err := p.ValidateIntent("orders", map[string]string{FieldVersion: "2"}); err == nil {
		t.Errorf("ValidateIntent of a version not in the directory succeeded")
	}

	for _, c := range []struct{ content, want string }{
		{"databases:\n  o:\n    engine: postgresql\n    server: main\n    database: o\n    migrations: m\n", `"postgresql_config"`},
		{"postgresql_config: pg.yaml\ndatabases:\n  o:\n    engine: postgresql\n    server: other\n    database: o\n    migrations: m\n", `"other" is not in`},
		{"postgresql_config: pg.yaml\ndatabases:\n  o:\n    engine: postgresql\n    database: o\n    migrations: m\n", "databases.o.server is required"},
		{"postgresql_config: pg.yaml\ndatabases:\n  o:\n    engine: postgresql\n    server: main\n    migrations: m\n", "databases.o.database is required"},
		{"postgresql_config: pg.yaml\ndatabases:\n  o:\n    engine: postgresql\n    server: main\n    database: o\n    path: x.db\n    migrations: m\n", "path is for engine: sqlite"},
		{"databases:\n  o:\n    path: x.db\n    server: main\n    migrations: m\n", "server and database are for engine: postgresql"},
		{"databases:\n  o:\n    engine: mysql\n    path: x.db\n    migrations: m\n", `engine "mysql"`},
		{"postgresql_config: nope.yaml\ndatabases:\n  o:\n    path: x.db\n    migrations: m\n", "postgresql_config"},
	} {
		_, err := LoadConfig(write("bad.yaml", c.content))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: err = %v, want it to mention %q", c.content, err, c.want)
		}
	}
}
