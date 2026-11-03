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

// engine: sqlite の単体テスト。sqlite3 は本物（/usr/bin/sqlite3 か PATH の sqlite3。
// 無ければ飛ばす）。

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/sdk/contract"
)

var sqliteMigrations = map[string]string{
	"0001_users.up.sql":    "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT);\n",
	"0001_users.down.sql":  "DROP TABLE users;\n",
	"0002_email.up.sql":    "ALTER TABLE users ADD COLUMN email TEXT", // 最後の ";" 無し
	"0002_email.down.sql":  "ALTER TABLE users DROP COLUMN email;\n",
	"0003_orders.up.sql":   "CREATE TABLE orders (id INTEGER PRIMARY KEY, note TEXT DEFAULT 'a''b');\n",
	"0003_orders.down.sql": "DROP TABLE orders;\n",
	"README.md":            "not a migration",
}

func sqlite3Path(t *testing.T) string {
	t.Helper()
	for _, p := range []string{"/usr/bin/sqlite3"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	p, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 が無い")
	}
	return p
}

func copyFiles(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

type sqliteFixture struct {
	p          *Provider
	dir, db    string
	migrations string
	bin        string
}

var appID = core.ID{Type: "db/database", Name: "app"}

func newSQLite(t *testing.T, files map[string]string) sqliteFixture {
	t.Helper()
	bin := sqlite3Path(t)
	dir := t.TempDir()
	f := sqliteFixture{dir: dir, db: filepath.Join(dir, "data", "app.db"), migrations: filepath.Join(dir, "migrations"), bin: bin}
	writeFiles(t, f.migrations, files)
	if err := os.MkdirAll(filepath.Dir(f.db), 0o755); err != nil {
		t.Fatal(err)
	}
	pr, err := Module(map[string]any{"sqlite3": bin, "lock_timeout": "200ms", "databases": map[string]any{
		"app": map[string]any{"path": f.db, "migrations": f.migrations},
	}})
	if err != nil {
		t.Fatal(err)
	}
	f.p = pr.(*Provider)
	return f
}

func (f sqliteFixture) observe(t *testing.T) core.Observation {
	t.Helper()
	o, err := f.p.Observe(context.Background(), []core.ID{appID})
	if err != nil {
		t.Fatal(err)
	}
	return o[appID]
}

func (f sqliteFixture) version(t *testing.T) int64 {
	t.Helper()
	o := f.observe(t)
	if o.Availability != core.Present {
		t.Fatalf("観測が %s: %s", o.Availability, o.Note)
	}
	return o.Fields[FieldVersion].(int64)
}

func (f sqliteFixture) query(t *testing.T, sql string) string {
	t.Helper()
	out, err := exec.Command(f.bin, "-batch", "-noheader", "-list", f.db, sql).CombinedOutput()
	if err != nil {
		t.Fatalf("sqlite3: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f sqliteFixture) invoke(op string, args core.Fields) error {
	return f.p.Invoke(context.Background(), core.NewWriteContext(nil), appID, op, args)
}

func TestSQLiteContract(t *testing.T) {
	contract.Run(t, func(t *testing.T) contract.Target {
		f := newSQLite(t, sqliteMigrations)
		return contract.Target{
			Provider:  f.p,
			Resources: []core.Resource{{ID: appID, Fields: core.Fields{FieldVersion: int64(2)}}},
			Observed:  []core.ID{appID},
			Faults: map[string]func(){
				"sqlite3 が無い": func() { f.p.sqlite3 = filepath.Join(f.dir, "no-sqlite3") },
				"壊れた DB": func() {
					if err := os.WriteFile(f.db, []byte(strings.Repeat("not a database ", 100)), 0o644); err != nil {
						t.Fatal(err)
					}
				},
				"DB の置き場所が無い":   func() { os.RemoveAll(filepath.Dir(f.db)) },
				"マイグレーションが読めない": func() { os.RemoveAll(f.migrations) },
			},
			Invokes: []contract.Invoke{
				{ID: appID, Op: OpMigrate, Args: core.Fields{"version": "3"},
					NotSent: func() { f.p.sqlite3 = filepath.Join(f.dir, "no-sqlite3") },
					Failed: func() {
						writeFiles(t, f.migrations, map[string]string{"0002_email.up.sql": "INSERT INTO nope VALUES (1);\n"})
					}},
				{ID: appID, Op: OpRollback, Args: core.Fields{"version": "0"},
					NotSent: func() { f.p.sqlite3 = filepath.Join(f.dir, "no-sqlite3") }},
			},
			Refuses: []contract.Refuse{{ID: appID}},
		}
	})
}

// まだ何も流していない（ファイルが無い）は版 0。流すと 1 本ずつ版が上がり、
// 下げると down を逆順に流す（E-F2・N-F2）。
func TestSQLiteMigrateUpAndDown(t *testing.T) {
	f := newSQLite(t, sqliteMigrations)
	if o := f.observe(t); o.Availability != core.Present || o.Fields[FieldVersion] != int64(0) || o.Fields[FieldLatest] != int64(3) || o.Fields[FieldPending] != int64(3) {
		t.Fatalf("初めの観測 %+v", o)
	}
	if err := f.invoke(OpMigrate, core.Fields{"version": "3"}); err != nil {
		t.Fatal(err)
	}
	if v := f.version(t); v != 3 {
		t.Fatalf("版 %d（want 3）", v)
	}
	if got := f.query(t, "SELECT group_concat(version) FROM (SELECT version FROM schema_migrations ORDER BY version);"); got != "1,2,3" {
		t.Fatalf("schema_migrations %q", got)
	}
	if got := f.query(t, "SELECT count(*) FROM schema_migrations WHERE applied_at LIKE '____-__-__T__:__:__Z';"); got != "3" {
		t.Fatalf("applied_at が埋まっていない: %s", got)
	}
	// 宣言どおりなら何も流さない。
	if err := f.invoke(OpMigrate, core.Fields{"version": int64(3)}); err != nil {
		t.Fatal(err)
	}
	if err := f.invoke(OpMigrate, core.Fields{"version": "1"}); err != nil {
		t.Fatal(err)
	}
	if v := f.version(t); v != 1 {
		t.Fatalf("版 %d（want 1）", v)
	}
	if got := f.query(t, "SELECT group_concat(name) FROM (SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name);"); got != "schema_migrations,users" {
		t.Fatalf("表 %q", got)
	}
	if err := f.invoke(OpRollback, core.Fields{"version": "3"}); err == nil || errors.Is(err, core.ErrNotSent) || !strings.Contains(err.Error(), "下げるだけ") {
		t.Fatalf("rollback で上げた: %v", err)
	}
	if err := f.invoke(OpRollback, core.Fields{"version": "0"}); err != nil {
		t.Fatal(err)
	}
	if v := f.version(t); v != 0 {
		t.Fatalf("版 %d（want 0）", v)
	}
}

// 失敗した 1 本は巻き戻り、前に通した版は残る。手順を止める誤り（ErrNotSent で
// ない）。rollback_on_fail なら呼んだときの版に戻す（E-F2・N-F2・不変条件 16）。
func TestSQLiteFailureAndRollbackOnFail(t *testing.T) {
	files := copyFiles(sqliteMigrations)
	files["0003_orders.up.sql"] = "CREATE TABLE half_done (x INTEGER);\nINSERT INTO nope VALUES (1);\n"
	f := newSQLite(t, files)
	err := f.invoke(OpMigrate, core.Fields{"version": "3"})
	if err == nil || errors.Is(err, core.ErrNotSent) || !strings.Contains(err.Error(), "0003_orders.up.sql") || !strings.Contains(err.Error(), "版 2") {
		t.Fatalf("失敗の誤り %v", err)
	}
	if v := f.version(t); v != 2 {
		t.Fatalf("版 %d（want 2：1 と 2 は残り、3 は巻き戻る）", v)
	}
	if got := f.query(t, "SELECT count(*) FROM sqlite_master WHERE name = 'half_done';"); got != "0" {
		t.Fatal("失敗した 1 本が half_done を残した")
	}

	if err := f.invoke(OpMigrate, core.Fields{"version": "1"}); err != nil {
		t.Fatal(err)
	}
	err = f.invoke(OpMigrate, core.Fields{"version": "3", "rollback_on_fail": true})
	if err == nil || errors.Is(err, core.ErrNotSent) || !strings.Contains(err.Error(), "版 1 に戻した") {
		t.Fatalf("rollback_on_fail の誤り %v", err)
	}
	if v := f.version(t); v != 1 {
		t.Fatalf("版 %d（want 1）", v)
	}
	if got := f.query(t, "SELECT count(*) FROM pragma_table_info('users') WHERE name = 'email';"); got != "0" {
		t.Fatal("戻したのに users.email が残る")
	}
}

// 流す前に全部の SQL を読み、流せない書き方（ドットコマンド）・足りない down が
// あれば何も流さない。
func TestSQLiteRefusedBeforeSending(t *testing.T) {
	files := copyFiles(sqliteMigrations)
	files["0002_email.up.sql"] = "ALTER TABLE users ADD COLUMN email TEXT;\n  .shell touch /tmp/pwned\n"
	f := newSQLite(t, files)
	err := f.invoke(OpMigrate, core.Fields{"version": "3"})
	if err == nil || errors.Is(err, core.ErrNotSent) || !strings.Contains(err.Error(), "0002_email.up.sql:2") {
		t.Fatalf("ドットコマンドを断らない: %v", err)
	}
	if v := f.version(t); v != 0 {
		t.Fatalf("断る前に流した（版 %d）", v)
	}

	files = copyFiles(sqliteMigrations)
	delete(files, "0002_email.down.sql")
	f = newSQLite(t, files)
	if err := f.invoke(OpMigrate, core.Fields{"version": "3"}); err != nil {
		t.Fatal(err)
	}
	if err := f.invoke(OpMigrate, core.Fields{"version": "0"}); err == nil || !strings.Contains(err.Error(), "版 2 の .down.sql が無い") {
		t.Fatalf("down の無い版を下げた: %v", err)
	}
	if v := f.version(t); v != 3 {
		t.Fatalf("断る前に流した（版 %d）", v)
	}
}

// ファイルの中で COMMIT を書くと、どこまで確定したか分からない誤りで止める
// （送れていないにしない、不変条件 16）。BEGIN を書けばその 1 本は巻き戻る。
func TestSQLiteFileControlsTransaction(t *testing.T) {
	files := copyFiles(sqliteMigrations)
	files["0001_users.up.sql"] = "CREATE TABLE users (id INTEGER PRIMARY KEY);\nCOMMIT;\n"
	f := newSQLite(t, files)
	err := f.invoke(OpMigrate, core.Fields{"version": "1"})
	if err == nil || errors.Is(err, core.ErrNotSent) || !strings.Contains(err.Error(), "トランザクション") {
		t.Fatalf("%v", err)
	}
	files["0001_users.up.sql"] = "BEGIN;\nCREATE TABLE users (id INTEGER PRIMARY KEY);\n"
	f = newSQLite(t, files)
	if err := f.invoke(OpMigrate, core.Fields{"version": "1"}); err == nil || errors.Is(err, core.ErrNotSent) {
		t.Fatalf("%v", err)
	}
	if v := f.version(t); v != 0 {
		t.Fatalf("版 %d（want 0）", v)
	}
}

// 別のプロセスが書き込みの鍵を持っていると、観測は Unknown、invoke は送れていない
// （ErrNotSent）。1 本でも確定したあとなら止める誤り（不変条件 16、E-K1）。
func TestSQLiteLocked(t *testing.T) {
	f := newSQLite(t, sqliteMigrations)
	if err := f.invoke(OpMigrate, core.Fields{"version": "1"}); err != nil {
		t.Fatal(err)
	}
	release := holdLock(t, f.bin, f.db)
	if o := f.observe(t); o.Availability != core.Unknown {
		// 読むだけなら rollback journal でも鍵の RESERVED の間は読める。読めたなら
		// 版は確かに 1。
		if o.Fields[FieldVersion] != int64(1) {
			t.Fatalf("鍵の間の観測 %+v", o)
		}
	}
	err := f.invoke(OpMigrate, core.Fields{"version": "3"})
	if !errors.Is(err, core.ErrNotSent) || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("鍵を待ちきれないのに %v（want ErrNotSent）", err)
	}
	release()
	if v := f.version(t); v != 1 {
		t.Fatalf("版 %d（want 1）", v)
	}
	if err := f.invoke(OpMigrate, core.Fields{"version": "3"}); err != nil {
		t.Fatal(err)
	}
}

// holdLock は別の sqlite3 で BEGIN EXCLUSIVE を取り、外す関数を返す。
func holdLock(t *testing.T, bin, db string) func() {
	t.Helper()
	cmd := exec.Command(bin, "-batch", db)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	io.WriteString(in, "BEGIN EXCLUSIVE;\nSELECT 'held';\n")
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "held" {
		t.Fatalf("鍵を取れない: %q %v", line, err)
	}
	done := false
	release := func() {
		if !done {
			done = true
			io.WriteString(in, "ROLLBACK;\n")
			in.Close()
			_ = cmd.Wait()
		}
	}
	t.Cleanup(release)
	return release
}

// 鍵を取ったあと版が流す前と違えば（ほかの処理が先に動かした）、CHECK 制約で
// 止まり何も流さない（E-F2）。
func TestSQLiteGuard(t *testing.T) {
	f := newSQLite(t, sqliteMigrations)
	if err := f.invoke(OpMigrate, core.Fields{"version": "2"}); err != nil {
		t.Fatal(err)
	}
	// 版 1 のつもりの 0002 をもう一度流す。
	_, err := f.p.sqlite(context.Background(), f.db, sqliteUpScript(2, "ALTER TABLE users ADD COLUMN email2 TEXT;"))
	if kindOf(err) != kindConflict {
		t.Fatalf("版の確かめが効かない: %v", err)
	}
	_, err = f.p.sqlite(context.Background(), f.db, sqliteDownScript(1, "DROP TABLE users;"))
	if kindOf(err) != kindConflict {
		t.Fatalf("down の版の確かめが効かない: %v", err)
	}
	if v := f.version(t); v != 2 {
		t.Fatalf("版 %d（want 2）", v)
	}
	if got := f.query(t, "SELECT count(*) FROM pragma_table_info('users') WHERE name = 'email2';"); got != "0" {
		t.Fatal("確かめで止まったのに流した")
	}
}

// 2 つの処理（別々の Provider＝別々のプロセスの真似）が同時に同じファイルを 0 から
// 3 へ動かしても、同じ版を二度流さない（E-F2）。
func TestSQLiteTwoRunners(t *testing.T) {
	f := newSQLite(t, sqliteMigrations)
	pr, err := Module(map[string]any{"sqlite3": f.bin, "lock_timeout": "5s", "databases": map[string]any{
		"app": map[string]any{"path": f.db, "migrations": f.migrations},
	}})
	if err != nil {
		t.Fatal(err)
	}
	f.p.lockTimeout = 5 * time.Second
	errs := make(chan error, 2)
	for _, p := range []*Provider{f.p, pr.(*Provider)} {
		p := p
		go func() {
			errs <- p.Invoke(context.Background(), core.NewWriteContext(nil), appID, OpMigrate, core.Fields{"version": "3"})
		}()
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil && !errors.Is(err, core.ErrNotSent) {
			t.Errorf("二重の実行で止める誤り: %v", err)
		}
	}
	if got := f.query(t, "SELECT group_concat(version) FROM (SELECT version FROM schema_migrations ORDER BY version);"); got != "1,2,3" {
		t.Fatalf("schema_migrations %q", got)
	}
}

func TestSQLiteValidate(t *testing.T) {
	f := newSQLite(t, sqliteMigrations)
	ok := []core.Resource{{ID: appID, Fields: core.Fields{FieldVersion: int64(3)}}}
	if d := f.p.Validate(ok, nil); len(d) != 0 {
		t.Fatalf("書ける宣言を断った: %v", d)
	}
	for _, c := range []struct {
		r      core.Resource
		ranges []core.Range
		want   string
	}{
		{core.Resource{ID: appID, Fields: core.Fields{FieldVersion: int64(4)}}, nil, "最新は 3"},
		{core.Resource{ID: appID, Fields: core.Fields{FieldVersion: int64(-1)}}, nil, "0 以上"},
		{core.Resource{ID: appID, Fields: core.Fields{FieldPending: int64(0)}}, nil, "観測だけ"},
		{core.Resource{ID: core.ID{Type: "db/database", Name: "other"}}, nil, "settings.databases に無い"},
		{core.Resource{ID: core.ID{Type: "db/table", Name: "app"}}, nil, "知らない"},
		{core.Resource{ID: appID, Ensure: core.EnsureAbsent}, nil, "消せない"},
		{core.Resource{ID: appID}, []core.Range{{Provider: "db"}}, "範囲を持たない"},
	} {
		d := f.p.Validate([]core.Resource{c.r}, c.ranges)
		if len(d) == 0 || !strings.Contains(d[0].Message, c.want) {
			t.Errorf("%+v: %v（want %q）", c.r, d, c.want)
		}
	}
	if err := f.p.Apply(context.Background(), core.NewWriteContext(nil), appID, core.Fields{FieldVersion: int64(1)}, core.Observation{}); err == nil {
		t.Fatal("Apply が通った（観測だけ）")
	}
	for _, c := range []struct {
		args core.Fields
		want string
	}{
		{core.Fields{}, "version を書く"},
		{core.Fields{"version": "02"}, "0 以上の整数"},
		{core.Fields{"version": "1", "force": true}, "知らない引数"},
		{core.Fields{"version": "1", "rollback_on_fail": "yes"}, "true か false"},
	} {
		if err := f.invoke(OpMigrate, c.args); err == nil || errors.Is(err, core.ErrNotSent) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v（want %q）", c.args, err, c.want)
		}
	}
	if v := f.version(t); v != 0 {
		t.Fatalf("引数の誤りで流した（版 %d）", v)
	}
}

func TestMigrationsDirectory(t *testing.T) {
	for _, c := range []struct {
		files map[string]string
		want  string
	}{
		{map[string]string{"0001_a.up.sql": "", "0003_c.up.sql": ""}, "版 2 が無い"},
		{map[string]string{"0001_a.up.sql": "", "0001_b.up.sql": ""}, "二重"},
		{map[string]string{"0001_a.down.sql": ""}, ".up.sql が無い"},
		{map[string]string{"1-a.sql": ""}, "NNNN_"},
		{map[string]string{"0000_a.up.sql": ""}, "1 以上"},
	} {
		dir := t.TempDir()
		writeFiles(t, dir, c.files)
		if _, err := loadMigrations(dir); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v（want %q）", c.files, err, c.want)
		}
	}
}

func TestModuleSettings(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		settings map[string]any
		want     string
	}{
		{map[string]any{}, "databases を書く"},
		{map[string]any{"databases": map[string]any{"a": map[string]any{"path": "rel.db", "migrations": dir}}}, "絶対パス"},
		{map[string]any{"databases": map[string]any{"a": map[string]any{"path": dir + "/a.db"}}}, "migrations"},
		{map[string]any{"databases": map[string]any{"a": map[string]any{"engine": "mysql", "migrations": dir}}}, "mysql"},
		{map[string]any{"databases": map[string]any{"a": map[string]any{"path": dir + "/a.db", "migrations": dir, "host": "x"}}}, "知らない項目"},
		{map[string]any{"databases": map[string]any{"a": map[string]any{"engine": "postgresql", "host": "db", "migrations": dir}}}, "observe"},
		{map[string]any{"databases": map[string]any{"a": map[string]any{"engine": "postgresql", "host": "db", "migrations": dir,
			"observe": map[string]any{"user": "ro", "password": "x"}}}}, "パスワードそのものは書けない"},
		{map[string]any{"databases": map[string]any{"a": map[string]any{"engine": "postgresql", "host": "db", "migrations": dir,
			"observe": map[string]any{"user": "ro", "password_secret": "file:x"}}}}, "知らない項目"},
		{map[string]any{"databases": map[string]any{"a": map[string]any{"engine": "postgresql", "host": "db", "migrations": dir, "port": 70000,
			"observe": map[string]any{"user": "ro"}}}}, "port"},
		{map[string]any{"lock_timeout": "soon"}, "lock_timeout"},
		{map[string]any{"sqlite3": "bin/sqlite3"}, "絶対パス"},
		{map[string]any{"nope": 1}, "知らない項目"},
	} {
		if _, err := Module(c.settings); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v（want %q）", c.settings, err, c.want)
		}
	}
}
