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

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/task"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
)

// 本物の sqlite3 を使うテストは、PATH に無ければ飛ばす（CI に無いかもしれない）。
func needSQLite(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(DefaultSQLite3); err != nil {
		t.Skip("sqlite3 is not in PATH")
	}
}

// 既定のマイグレーション：1 users、2 email の列、3 orders。
var defaultMigrations = map[string]string{
	"0001_users.up.sql":    "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT);\n",
	"0001_users.down.sql":  "DROP TABLE users;\n",
	"0002_email.up.sql":    "ALTER TABLE users ADD COLUMN email TEXT;\nCREATE INDEX users_email ON users (email)", // 最後の ";" 無し
	"0002_email.down.sql":  "DROP INDEX users_email;\nALTER TABLE users DROP COLUMN email;\n",
	"0003_orders.up.sql":   "CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INTEGER);\n",
	"0003_orders.down.sql": "DROP TABLE orders;\n",
	"README.md":            "not a migration\n",
}

type fixture struct {
	p   *Provider
	db  Database
	dir string
}

func newFixture(t *testing.T, files map[string]string) fixture {
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
	db := Database{Path: filepath.Join(dir, "app.db"), Migrations: mdir}
	return fixture{p: New(map[string]Database{"app": db}, Options{}), db: db, dir: dir}
}

func (f fixture) write(t *testing.T, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.db.Migrations, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

var appID = core.NewID(Name, core.NewScope(nil), "app")

func req(taskID, version string) task.Request {
	return task.Request{TaskID: taskID, Resource: appID, Desired: map[string]string{FieldVersion: version}}
}

func (f fixture) version(t *testing.T) string {
	t.Helper()
	obs, err := f.p.Read(context.Background(), "app")
	if err != nil || obs.Availability != core.AvailabilityOk {
		t.Fatalf("Read = %+v, %v", obs, err)
	}
	return fmt.Sprint(obs.State[FieldVersion].Value)
}

// query は本物の sqlite3 で DB を覗く（テスト用）。
func (f fixture) query(t *testing.T, sql string) string {
	t.Helper()
	out, err := f.p.sqlite(context.Background(), f.db.Path, sql)
	if err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return out
}

func (f fixture) tables(t *testing.T) string {
	t.Helper()
	return f.query(t, "SELECT group_concat(name, ',') FROM (SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name);")
}

// taskDriven は契約テスト用のアダプタ：収束ループの Apply の代わりに Task の
// 実行体を呼ぶ（strategy: task のもとで「Apply 相当」が契約を満たすかを見る）。
type taskDriven struct{ *Provider }

func (d taskDriven) Apply(ctx context.Context, plan core.Plan) error {
	for _, op := range plan.Ops {
		desired := map[string]string{}
		for _, fd := range op.Fields {
			desired[fd.Field] = fd.To
		}
		if err := d.RunTask(ctx, task.Request{TaskID: "contract", Resource: op.Target, Desired: desired, Plan: plan}); err != nil {
			return err
		}
	}
	return nil
}

func TestValidateProvider(t *testing.T) {
	if err := sdk.ValidateProvider(New(nil, Options{})); err != nil {
		t.Fatalf("ValidateProvider: %v", err)
	}
}

func TestContract(t *testing.T) {
	needSQLite(t)
	f := newFixture(t, defaultMigrations)
	ct := sdk.ContractTester{
		Provider: taskDriven{f.p},
		Scope:    core.NewScope(nil),
		SampleID: "app",
		// データベースは作ったり消したりしない：Apply の前は版 0 として今ある。
		Preexisting:   true,
		SampleDesired: map[string]string{FieldName: "app", FieldVersion: "2"},
		MakeUnreachable: func() (func(), error) {
			f.p.opts.SQLite3 = filepath.Join(f.dir, "no-such-sqlite3")
			return func() { f.p.opts.SQLite3 = DefaultSQLite3 }, nil
		},
	}
	ct.RunAll(t)
}

func TestRunTask_UpAndDown(t *testing.T) {
	needSQLite(t)
	f := newFixture(t, defaultMigrations)
	ctx := context.Background()
	if v := f.version(t); v != "0" {
		t.Fatalf("version before = %s, want 0 (no database file yet)", v)
	}

	if err := f.p.RunTask(ctx, req("t1", "3")); err != nil {
		t.Fatalf("RunTask up: %v", err)
	}
	if v := f.version(t); v != "3" {
		t.Fatalf("version = %s, want 3", v)
	}
	if got := f.tables(t); got != "orders,schema_migrations,users" {
		t.Errorf("tables = %q", got)
	}
	if got := f.query(t, "SELECT group_concat(version, ',') FROM schema_migrations WHERE applied_at LIKE '____-__-__T__:__:__Z';"); got != "1,2,3" {
		t.Errorf("schema_migrations = %q, want 1,2,3 with applied_at", got)
	}
	obs, _ := f.p.Read(ctx, "app")
	if p := obs.State[FieldPending].Value; p != int64(0) {
		t.Errorf("pending = %v, want 0", p)
	}

	// 同じ版をもう一度：何も流さない。
	if err := f.p.RunTask(ctx, req("t2", "3")); err != nil {
		t.Fatalf("RunTask same version: %v", err)
	}

	if err := f.p.RunTask(ctx, req("t3", "1")); err != nil {
		t.Fatalf("RunTask down: %v", err)
	}
	if v := f.version(t); v != "1" {
		t.Fatalf("version = %s, want 1", v)
	}
	if got := f.tables(t); got != "schema_migrations,users" {
		t.Errorf("tables after down = %q", got)
	}
	if got := f.query(t, "SELECT count(*) FROM pragma_table_info('users') WHERE name = 'email';"); got != "0" {
		t.Errorf("users.email still exists after down")
	}
	obs, _ = f.p.Read(ctx, "app")
	if p := obs.State[FieldPending].Value; p != int64(2) {
		t.Errorf("pending = %v, want 2", p)
	}

	if err := f.p.RunTask(ctx, req("t4", "0")); err != nil {
		t.Fatalf("RunTask down to 0: %v", err)
	}
	if v := f.version(t); v != "0" {
		t.Fatalf("version = %s, want 0", v)
	}
}

func TestRunTask_FailedMigrationLeavesNoTrace(t *testing.T) {
	needSQLite(t)
	files := copyFiles(defaultMigrations)
	// 2 本目：1 文目は通るが 2 文目で失敗する。
	files["0002_email.up.sql"] = "CREATE TABLE half_done (x);\nINSERT INTO no_such_table VALUES (1);\n"
	f := newFixture(t, files)

	err := f.p.RunTask(context.Background(), req("t1", "3"))
	if err == nil {
		t.Fatal("RunTask succeeded, want the failure of 0002")
	}
	if !strings.Contains(err.Error(), "0002_email.up.sql") || !strings.Contains(err.Error(), "version 1") {
		t.Errorf("error = %v, want it to name the file and the version left", err)
	}
	if v := f.version(t); v != "1" {
		t.Fatalf("version = %s, want 1 (the version before the failed one stays)", v)
	}
	if got := f.tables(t); got != "schema_migrations,users" {
		t.Errorf("tables = %q: the failed migration must leave nothing behind", got)
	}
}

// sqlite3 のエラーの「near line N」はマイグレーションのファイルの行（トランザクションで
// 包む行や busy_timeout の行でずれない。#95 の試用で、ファイルの 4 行目が line 7 と出ていた）。
func TestRunTask_ErrorLineIsTheFileLine(t *testing.T) {
	needSQLite(t)
	files := copyFiles(defaultMigrations)
	files["0002_email.up.sql"] = "-- 3 行目で失敗する\nCREATE TABLE half_done (x);\nINSERT INTO no_such_table VALUES (1);\n"
	for _, timeout := range []time.Duration{0, time.Second} {
		f := newFixture(t, files)
		f.p = New(map[string]Database{"app": f.db}, Options{BusyTimeout: timeout})
		err := f.p.RunTask(context.Background(), req("t1", "3"))
		if err == nil || !strings.Contains(err.Error(), "near line 3:") {
			t.Errorf("busy_timeout %s: error = %v, want it to point at line 3 of the file", timeout, err)
		}
		if v := f.version(t); v != "1" {
			t.Errorf("busy_timeout %s: version = %s, want 1", timeout, v)
		}
	}
}

func TestRunTask_DownWithoutDownFileFailsBeforeRunning(t *testing.T) {
	needSQLite(t)
	files := copyFiles(defaultMigrations)
	delete(files, "0002_email.down.sql")
	f := newFixture(t, files)
	ctx := context.Background()
	if err := f.p.RunTask(ctx, req("t1", "3")); err != nil {
		t.Fatal(err)
	}
	err := f.p.RunTask(ctx, req("t2", "1"))
	if err == nil || !strings.Contains(err.Error(), "no .down.sql for version 2") {
		t.Fatalf("RunTask = %v, want missing down for version 2", err)
	}
	if v := f.version(t); v != "3" {
		t.Errorf("version = %s, want 3 (nothing runs when a down is missing)", v)
	}
}

func TestRollbackTask(t *testing.T) {
	needSQLite(t)
	files := copyFiles(defaultMigrations)
	files["0003_orders.up.sql"] = "CREATE TABLE orders (id INTEGER PRIMARY KEY);\nSELECT no_such_function();\n"

	for _, withPlan := range []bool{true, false} {
		f := newFixture(t, files)
		ctx := context.Background()
		if err := f.p.RunTask(ctx, req("setup", "1")); err != nil {
			t.Fatal(err)
		}
		r := req("t1", "3")
		if withPlan {
			// 計画の古い値は 1。RunTask の頭の記録より計画を先に見る。
			r.Plan = core.Plan{Ops: []core.Op{{Kind: core.OpKindUpdate, Target: appID,
				Fields: []core.FieldDiff{{Field: FieldVersion, From: "1", To: "3"}}}}}
		}
		if err := f.p.RunTask(ctx, r); err == nil {
			t.Fatal("RunTask succeeded, want the failure of 0003")
		}
		if v := f.version(t); v != "2" {
			t.Fatalf("withPlan=%v: version after failure = %s, want 2", withPlan, v)
		}
		note, err := f.p.RollbackTask(ctx, r)
		if err != nil {
			t.Fatalf("withPlan=%v: RollbackTask: %v", withPlan, err)
		}
		if note != "the database is at version 1" {
			t.Errorf("withPlan=%v: RollbackTask note = %q", withPlan, note)
		}
		if v := f.version(t); v != "1" {
			t.Fatalf("withPlan=%v: version after rollback = %s, want 1 (the version at launch)", withPlan, v)
		}
		if got := f.tables(t); got != "schema_migrations,users" {
			t.Errorf("withPlan=%v: tables after rollback = %q", withPlan, got)
		}
	}
}

func TestRollbackTask_UnknownStart(t *testing.T) {
	needSQLite(t)
	f := newFixture(t, defaultMigrations)
	if _, err := f.p.RollbackTask(context.Background(), req("never-ran", "3")); err == nil {
		t.Fatal("RollbackTask without a plan or a RunTask succeeded, want an error")
	}
}

// Task の仕組み（task.Manager）に載せて、失敗 → rollback_on_fail → halt の流れを通す。
func TestManager_FailureRollsBackAndHalts(t *testing.T) {
	needSQLite(t)
	files := copyFiles(defaultMigrations)
	files["0003_orders.up.sql"] = "CREATE TABLE orders (id);\nINSERT INTO nope VALUES (1);\n"
	f := newFixture(t, files)
	ctx := context.Background()
	if err := f.p.RunTask(ctx, req("setup", "1")); err != nil {
		t.Fatal(err)
	}
	store := task.NewFileStore(filepath.Join(f.dir, "tasks.json"))
	m := task.NewManager(store, f.p, "test", nil)
	pol := core.UpdatePolicy{Strategy: core.UpdateStrategyTask, RollbackOnFail: true}
	dec, err := m.Trigger(ctx, appID, map[string]string{FieldVersion: "3"}, core.Plan{}, pol)
	if err != nil || dec.Outcome != task.Launched {
		t.Fatalf("Trigger = %+v, %v", dec, err)
	}
	m.Wait(30 * time.Second)
	rec, ok, err := store.Get(ctx, appID.String())
	if err != nil || !ok {
		t.Fatalf("record = %+v, %v, %v", rec, ok, err)
	}
	if rec.Phase != task.PhaseFailed || !rec.RolledBack || !strings.Contains(rec.Error, "0003_orders.up.sql") {
		t.Errorf("record = %+v, want failed + rolled back naming 0003", rec)
	}
	// #113: 記録のエラーに rollback の後の版が出る。
	if !strings.Contains(rec.Error, "rolled back: the database is at version 1") {
		t.Errorf("record error = %q, want the version after the rollback", rec.Error)
	}
	if strings.Contains(rec.Error, "rollback failed") {
		t.Errorf("rollback failed: %s", rec.Error)
	}
	if v := f.version(t); v != "1" {
		t.Errorf("version = %s, want 1 (rolled back to the version at launch)", v)
	}
	dec, err = m.Trigger(ctx, appID, map[string]string{FieldVersion: "3"}, core.Plan{}, pol)
	if err != nil || dec.Outcome != task.Outstanding {
		t.Errorf("second Trigger = %+v, %v, want outstanding (halted until cancelled)", dec, err)
	}
}

func TestApplyRefused(t *testing.T) {
	f := newFixture(t, defaultMigrations)
	err := f.p.Apply(context.Background(), core.Plan{Ops: []core.Op{{Kind: core.OpKindUpdate, Target: appID,
		Fields: []core.FieldDiff{{Field: FieldVersion, From: "0", To: "1"}}}}})
	var perm *provider.PermanentError
	if !errors.As(err, &perm) || !strings.Contains(err.Error(), "strategy: task") {
		t.Fatalf("Apply = %v, want a permanent refusal", err)
	}
	if _, serr := os.Stat(f.db.Path); !os.IsNotExist(serr) {
		t.Errorf("Apply touched the database file")
	}
}

func TestValidateUpdatePolicy(t *testing.T) {
	p := New(nil, Options{})
	if err := provider.ValidateUpdatePolicy(p, "app", core.UpdatePolicy{}); err == nil {
		t.Error("strategy apply (default) accepted, want an error")
	}
	if err := provider.ValidateUpdatePolicy(p, "app", core.UpdatePolicy{Strategy: core.UpdateStrategyTask}); err != nil {
		t.Errorf("strategy task: %v", err)
	}
}

func TestValidateIntent(t *testing.T) {
	f := newFixture(t, defaultMigrations)
	if err := f.p.ValidateIntent("app", map[string]string{FieldName: "app", FieldVersion: "2"}); err != nil {
		t.Errorf("with name: %v", err)
	}
	for _, ok := range []string{"0", "1", "3"} {
		if err := f.p.ValidateIntent("app", map[string]string{FieldVersion: ok}); err != nil {
			t.Errorf("version %s: %v", ok, err)
		}
	}
	cases := []struct {
		id     string
		fields map[string]string
		want   string
	}{
		{"app", map[string]string{FieldVersion: "4"}, "version 4 is not in"},
		{"app", map[string]string{FieldVersion: "-1"}, "non-negative integer"},
		{"app", map[string]string{FieldVersion: "02"}, "non-negative integer"},
		{"app", map[string]string{FieldVersion: "two"}, "non-negative integer"},
		{"app", map[string]string{}, "version is required"},
		{"app", map[string]string{FieldVersion: "1", "path": "/tmp/x"}, `unknown field "path"`},
		{"app", map[string]string{FieldVersion: "1", FieldPending: "0"}, "observed only"},
		{"other", map[string]string{FieldVersion: "1"}, "not configured"},
		{"app", map[string]string{FieldName: "other", FieldVersion: "1"}, "must equal the resource id"},
	}
	for _, c := range cases {
		err := f.p.ValidateIntent(c.id, c.fields)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("ValidateIntent(%s, %v) = %v, want %q", c.id, c.fields, err, c.want)
		}
	}
}

func TestLoadMigrations_Errors(t *testing.T) {
	cases := map[string]struct {
		files map[string]string
		want  string
	}{
		"gap":            {map[string]string{"0001_a.up.sql": "", "0003_c.up.sql": ""}, "version 2 is missing"},
		"duplicate":      {map[string]string{"0001_a.up.sql": "", "1_b.up.sql": ""}, "defined twice"},
		"duplicate same": {map[string]string{"0001_a.up.sql": "", "01_a.up.sql": ""}, "defined twice"},
		"down only":      {map[string]string{"0001_a.up.sql": "", "0002_b.down.sql": ""}, "version 2 has no .up.sql"},
		"version zero":   {map[string]string{"0000_a.up.sql": ""}, "1 or more"},
		"bad name":       {map[string]string{"0001_a.sql": ""}, "want NNNN_<name>.up.sql"},
	}
	for name, c := range cases {
		dir := t.TempDir()
		for fn, body := range c.files {
			if err := os.WriteFile(filepath.Join(dir, fn), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		_, err := loadMigrations(dir)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: loadMigrations = %v, want %q", name, err, c.want)
		}
	}
}

func TestRunTask_DotCommandRefused(t *testing.T) {
	needSQLite(t)
	files := copyFiles(defaultMigrations)
	marker := filepath.Join(t.TempDir(), "shell-ran")
	files["0001_users.up.sql"] = "CREATE TABLE users (id);\n.shell touch " + marker + "\n"
	f := newFixture(t, files)
	err := f.p.RunTask(context.Background(), req("t1", "1"))
	if err == nil || !strings.Contains(err.Error(), "dot-commands are not allowed") {
		t.Fatalf("RunTask = %v, want the dot-command refused", err)
	}
	if _, serr := os.Stat(marker); serr == nil {
		t.Error("the .shell command ran")
	}
	if _, serr := os.Stat(f.db.Path); !os.IsNotExist(serr) {
		t.Error("the database was touched")
	}
}

func TestRead_NoSQLite3IsUnknown(t *testing.T) {
	needSQLite(t)
	for _, withFile := range []bool{false, true} {
		f := newFixture(t, defaultMigrations)
		if withFile {
			if err := f.p.RunTask(context.Background(), req("t1", "1")); err != nil {
				t.Fatal(err)
			}
		}
		f.p.opts.SQLite3 = filepath.Join(f.dir, "no-such-sqlite3")
		obs, err := f.p.Read(context.Background(), "app")
		if err == nil || obs.Availability != core.AvailabilityUnknown || obs.State != nil {
			t.Errorf("withFile=%v: Read = %+v, %v; want Unknown with an error", withFile, obs, err)
		}
		if err := f.p.RunTask(context.Background(), req("t2", "2")); err == nil {
			t.Errorf("withFile=%v: RunTask without sqlite3 succeeded", withFile)
		}
	}
}

func TestRead_UnknownIsNotAbsent(t *testing.T) {
	needSQLite(t)
	ctx := context.Background()

	// DB を置くディレクトリが無い（マウント忘れなど）：版 0 と言わない。
	f := newFixture(t, defaultMigrations)
	f.p.dbs["app"] = Database{Path: filepath.Join(f.dir, "missing-dir", "app.db"), Migrations: f.db.Migrations}
	if obs, err := f.p.Read(ctx, "app"); err == nil || obs.Availability != core.AvailabilityUnknown {
		t.Errorf("missing db dir: Read = %+v, %v; want Unknown", obs, err)
	}

	// マイグレーションのディレクトリが無い。
	f = newFixture(t, defaultMigrations)
	f.p.dbs["app"] = Database{Path: f.db.Path, Migrations: filepath.Join(f.dir, "no-migrations")}
	if obs, err := f.p.Read(ctx, "app"); err == nil || obs.Availability != core.AvailabilityUnknown {
		t.Errorf("missing migrations dir: Read = %+v, %v; want Unknown", obs, err)
	}

	// DB のファイルがデータベースでない。
	f = newFixture(t, defaultMigrations)
	if err := os.WriteFile(f.db.Path, []byte(strings.Repeat("not a database ", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	if obs, err := f.p.Read(ctx, "app"); err == nil || obs.Availability != core.AvailabilityUnknown {
		t.Errorf("corrupt db: Read = %+v, %v; want Unknown", obs, err)
	}

	// ファイルが無く、置き場所がある：まだ何も流していない＝版 0。
	f = newFixture(t, defaultMigrations)
	obs, err := f.p.Read(ctx, "app")
	if err != nil || obs.Availability != core.AvailabilityOk || obs.State[FieldVersion].Value != int64(0) || obs.State[FieldPending].Value != int64(3) {
		t.Errorf("fresh db: Read = %+v, %v; want Ok version 0 pending 3", obs, err)
	}
	if _, serr := os.Stat(f.db.Path); !os.IsNotExist(serr) {
		t.Error("Read created the database file")
	}
}

func TestRead_LockedIsUnknown(t *testing.T) {
	needSQLite(t)
	f := newFixture(t, defaultMigrations)
	ctx := context.Background()
	if err := f.p.RunTask(ctx, req("t1", "1")); err != nil {
		t.Fatal(err)
	}
	// 別の sqlite3 が排他ロックを持ったまま止まっている。
	holder := exec.Command(DefaultSQLite3, "-batch", f.db.Path)
	stdin, err := holder.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stdin.Close()
		_ = holder.Wait()
	}()
	// ロックを取ったことを holder 自身に言わせてから確かめる（CI で、holder が
	// まだ BEGIN EXCLUSIVE を流していないうちに観測しないように）。
	if _, err := io.WriteString(stdin, "BEGIN EXCLUSIVE;\nINSERT INTO users (id) VALUES (1);\nSELECT 'holding-the-lock';\n"); err != nil {
		t.Fatal(err)
	}
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err == nil && !strings.Contains(line, "holding-the-lock") {
			err = fmt.Errorf("unexpected output from the lock holder: %q", line)
		}
		ready <- err
		_, _ = io.Copy(io.Discard, stdout)
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("the lock holder did not take the lock: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the lock holder did not take the lock within 10s")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		obs, err := f.p.Read(ctx, "app")
		if obs.Availability == core.AvailabilityUnknown && err != nil {
			if !strings.Contains(err.Error(), "locked") {
				t.Errorf("Read error = %v, want database is locked", err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Read stayed %+v while the database is locked", obs)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := f.p.RunTask(ctx, req("t2", "2")); err == nil {
		t.Error("RunTask succeeded while the database is locked")
	}
	stdin.Close()
	_ = holder.Wait()
	if v := f.version(t); v != "1" {
		t.Errorf("version = %s, want 1", v)
	}
}

func copyFiles(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
