//go:build integration_postgresql

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

// sqlmigrate の engine: postgresql（Backlog #126）を、同じコンテナのサーバに向けて
// 常駐の sakutei（--task-store）で回す：0→3、3→1 の down、失敗する 1 本
// （halt）、rollback_on_fail、トランザクションに入れられない文（25001）、2 つの
// デーモンが同時に同じ DB を動かす（pg_advisory_xact_lock）。接続の設定は
// postgresql provider と同じサーバの一覧（lab.config）を postgresql_config で
// 指す。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	smDB       = "migdb"
	smResource = "sqlmigrate//" + smDB
)

type smLab struct {
	dir, intent, config, migrations string
}

func newSMLab(t *testing.T) smLab {
	t.Helper()
	d := filepath.Join(lab.dir, "sqlmigrate")
	l := smLab{dir: d, intent: filepath.Join(d, "intent"), config: filepath.Join(d, "databases.yaml"), migrations: filepath.Join(d, "migrations")}
	for _, p := range []string{l.intent, l.migrations} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, sql := range map[string]string{
		"0001_users.up.sql":    "CREATE TABLE users (id bigint PRIMARY KEY, name text NOT NULL);\n",
		"0001_users.down.sql":  "DROP TABLE users;\n",
		"0002_email.up.sql":    "ALTER TABLE users ADD COLUMN email text;\nCOMMENT ON COLUMN users.email IS E'mail\\naddress';\nCREATE INDEX users_email ON users (email)", // 最後の ";" 無し
		"0002_email.down.sql":  "DROP INDEX users_email;\nALTER TABLE users DROP COLUMN email;\n",
		"0003_orders.up.sql":   "CREATE TABLE orders (id bigint PRIMARY KEY, user_id bigint REFERENCES users (id), note text DEFAULT $$a'b$$);\n",
		"0003_orders.down.sql": "DROP TABLE orders;\n",
		"0004_bad.up.sql":      "CREATE TABLE half_done (x int);\nINSERT INTO no_such_table VALUES (1);\n",
		"0004_bad.down.sql":    "DROP TABLE half_done;\n",
	} {
		l.writeMigration(t, name, sql)
	}
	cfg := "postgresql_config: " + lab.config + "\ndatabases:\n  " + smDB + ":\n    engine: postgresql\n    server: main\n    database: " + smDB + "\n    migrations: migrations\n"
	if err := os.WriteFile(l.config, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return l
}

func (l smLab) writeMigration(t *testing.T, name, sql string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(l.migrations, name), []byte(sql), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (l smLab) declare(t *testing.T, version string, rollback bool) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"resources": []map[string]any{{
		"provider":      "sqlmigrate",
		"id":            smDB,
		"update_policy": map[string]any{"strategy": "task", "rollback_on_fail": rollback},
		"fields":        map[string]string{"version": version},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.intent, "sm.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// version は schema_migrations の版（sakutei の外から読む。表が無ければ "0"）。
func (l smLab) version(t *testing.T) string {
	t.Helper()
	if admin(t, smDB, `SELECT to_regclass('schema_migrations') IS NOT NULL;`) != "t" {
		return "0"
	}
	return admin(t, smDB, `SELECT coalesce(max(version), 0) FROM schema_migrations;`)
}

func (l smLab) tables(t *testing.T) string {
	t.Helper()
	return admin(t, smDB, `SELECT coalesce(string_agg(tablename, ',' ORDER BY tablename), '') FROM pg_tables WHERE schemaname = 'public';`)
}

type smRecord struct {
	Phase      string `json:"phase"`
	RolledBack bool   `json:"rolled_back"`
	Error      string `json:"error"`
}

// record はタスクの記録（無ければ ok が false）。
func (l smLab) record(t *testing.T, store string) (smRecord, bool) {
	t.Helper()
	b, err := os.ReadFile(store)
	if os.IsNotExist(err) {
		return smRecord{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Tasks map[string]smRecord `json:"tasks"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return smRecord{}, false // 書きかけ
	}
	r, ok := doc.Tasks[smResource]
	return r, ok
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// daemon は常駐の sakutei を起動し、止める関数とログを返す。
func (l smLab) daemon(t *testing.T, name string) (stop func(), log *lockedBuffer) {
	t.Helper()
	log = &lockedBuffer{}
	cmd := exec.Command(lab.bin,
		"--intent", l.intent,
		"--state", filepath.Join(l.dir, name+"-fake-state.json"),
		"--interval", "300ms",
		"--sqlmigrate-config", l.config,
		"--task-store", filepath.Join(l.dir, name+"-tasks.json"))
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := false
	stop = func() {
		if !done {
			done = true
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}
	t.Cleanup(stop)
	return stop, log
}

func (l smLab) store(name string) string { return filepath.Join(l.dir, name+"-tasks.json") }

func waitFor(t *testing.T, what string, d time.Duration, log *lockedBuffer, cond func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s\ndaemon log:\n%s", what, log.String())
}

// converged は宣言の版になり、未完了の記録が消えるまで常駐の sakutei を回す。
func (l smLab) converged(t *testing.T, name, want string) {
	t.Helper()
	stop, log := l.daemon(t, name)
	defer stop()
	waitFor(t, "version "+want, time.Minute, log, func() bool { return l.version(t) == want })
	waitFor(t, "the task record to clear", 30*time.Second, log, func() bool {
		_, ok := l.record(t, l.store(name))
		return !ok
	})
}

// failed は Task が失敗＝halt するまで常駐の sakutei を回し、記録を返して消す。
func (l smLab) failed(t *testing.T, name string) smRecord {
	t.Helper()
	stop, log := l.daemon(t, name)
	var rec smRecord
	waitFor(t, "the task to fail", time.Minute, log, func() bool {
		r, ok := l.record(t, l.store(name))
		rec = r
		return ok && r.Phase == "failed"
	})
	stop()
	out, err := exec.Command(lab.bin, "task", "cancel", "--store", l.store(name), smResource).CombinedOutput()
	if err != nil {
		t.Fatalf("task cancel: %v\n%s", err, out)
	}
	return rec
}

func TestSQLMigratePostgreSQLIntegration(t *testing.T) {
	admin(t, "postgres", "CREATE DATABASE "+smDB+";")
	t.Cleanup(func() {
		_, _ = psqlAs(adminUser, adminPW, "postgres", "DROP DATABASE IF EXISTS "+smDB+" WITH (FORCE);")
	})
	l := newSMLab(t)

	t.Run("0 to 3", func(t *testing.T) {
		l.declare(t, "3", false)
		l.converged(t, "up", "3")
		if got := l.tables(t); got != "orders,schema_migrations,users" {
			t.Errorf("tables = %q", got)
		}
		if got := admin(t, smDB, `SELECT string_agg(version::text, ',' ORDER BY version) FROM schema_migrations WHERE applied_at IS NOT NULL;`); got != "1,2,3" {
			t.Errorf("schema_migrations = %q", got)
		}
		if got := admin(t, smDB, `SELECT col_description('users'::regclass, 3) || '|' || (SELECT column_default FROM information_schema.columns WHERE table_name = 'orders' AND column_name = 'note');`); got != "mail\naddress|'a''b'::text" {
			t.Errorf("E'' and $$ quoting = %q", got)
		}
	})

	t.Run("3 to 1 runs down in reverse", func(t *testing.T) {
		l.declare(t, "1", false)
		l.converged(t, "down", "1")
		if got := l.tables(t); got != "schema_migrations,users" {
			t.Errorf("tables = %q", got)
		}
		if got := admin(t, smDB, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'email';`); got != "0" {
			t.Errorf("users.email remains after down")
		}
	})

	t.Run("a failing migration halts and leaves nothing behind", func(t *testing.T) {
		l.declare(t, "4", false)
		rec := l.failed(t, "fail")
		if rec.RolledBack || !strings.Contains(rec.Error, "0004_bad.up.sql") || !strings.Contains(rec.Error, "42P01") || !strings.Contains(rec.Error, "version 3") {
			t.Errorf("record = %+v, want 0004 failed with 42P01 at version 3", rec)
		}
		if v := l.version(t); v != "3" {
			t.Errorf("version = %s, want 3 (2 and 3 stay, 4 is rolled back)", v)
		}
		if got := l.tables(t); strings.Contains(got, "half_done") {
			t.Errorf("tables = %q: the failed migration left half_done", got)
		}
	})

	t.Run("rollback_on_fail goes back to the version at launch", func(t *testing.T) {
		l.declare(t, "1", false)
		l.converged(t, "down2", "1")
		l.declare(t, "4", true)
		rec := l.failed(t, "rollback")
		if !rec.RolledBack || !strings.Contains(rec.Error, "rolled back: the database is at version 1") {
			t.Errorf("record = %+v, want rolled back to 1", rec)
		}
		if v := l.version(t); v != "1" {
			t.Errorf("version = %s, want 1", v)
		}
		if got := l.tables(t); got != "schema_migrations,users" {
			t.Errorf("tables after rollback = %q", got)
		}
	})

	t.Run("a statement that cannot run in a transaction is a failure", func(t *testing.T) {
		l.writeMigration(t, "0002_email.up.sql", "ALTER TABLE users ADD COLUMN email text;\nCREATE INDEX CONCURRENTLY users_email ON users (email);\n")
		l.declare(t, "2", false)
		rec := l.failed(t, "concurrently")
		if !strings.Contains(rec.Error, "25001") || !strings.Contains(rec.Error, "0002_email.up.sql") {
			t.Errorf("record = %+v, want 0002 failed with 25001", rec)
		}
		if v := l.version(t); v != "1" {
			t.Errorf("version = %s, want 1", v)
		}
		if got := admin(t, smDB, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'email';`); got != "0" {
			t.Errorf("the failed migration left users.email")
		}
		l.writeMigration(t, "0002_email.up.sql", "ALTER TABLE users ADD COLUMN email text;\nCREATE INDEX users_email ON users (email);\n")
	})

	t.Run("two daemons on the same database", func(t *testing.T) {
		// 別々の task store の 2 つのデーモンが同時に版 1 から 3 へ流す。テストが先に
		// 同じ advisory lock を持っておくと、2 つとも最初の 1 本（0002）の頭で待たされ
		// （版は 1 のまま）、外すと片方（勝った側）が 0002 を流し、もう片方（負けた側）は
		// 版の確かめ（55000）で何も流さずに止まる（列の二重の追加 42701 などには
		// ならない）。0003 はわざと失敗させ、版を 2 で止める（宣言の 3 にならないので、
		// どちらの失敗の記録も現実を見て消されず、記録で確かめられる）。
		l.declare(t, "1", false)
		l.converged(t, "down3", "1")
		l.writeMigration(t, "0003_orders.up.sql", "CREATE TABLE orders (id bigint PRIMARY KEY);\nSELECT no_such_function();\n")
		defer l.writeMigration(t, "0003_orders.up.sql", "CREATE TABLE orders (id bigint PRIMARY KEY);\n")
		l.declare(t, "3", false)
		held := make(chan error, 1)
		go func() {
			out, err := psqlAs(adminUser, adminPW, smDB, "SELECT pg_advisory_lock("+lockKey()+");\nSELECT pg_sleep(6);\n")
			if err != nil {
				err = fmt.Errorf("%v: %s", err, out)
			}
			held <- err
		}()
		waitFor(t, "the test to hold the lock", 30*time.Second, &lockedBuffer{}, func() bool {
			return admin(t, smDB, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted;`) == "1"
		})
		stopA, logA := l.daemon(t, "race-a")
		stopB, logB := l.daemon(t, "race-b")
		defer stopA()
		defer stopB()
		defer func() {
			if t.Failed() {
				t.Logf("daemon A:\n%s\ndaemon B:\n%s", logA.String(), logB.String())
			}
		}()
		waitFor(t, "both daemons to wait on the lock", 30*time.Second, logA, func() bool {
			return admin(t, smDB, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted;`) == "2"
		})
		if v := l.version(t); v != "1" {
			t.Fatalf("version = %s while the lock is held, want 1", v)
		}
		if err := <-held; err != nil {
			t.Fatalf("holding the lock: %v", err)
		}
		var recs [2]smRecord
		waitFor(t, "both tasks to fail", time.Minute, logA, func() bool {
			for i, name := range []string{"race-a", "race-b"} {
				r, ok := l.record(t, l.store(name))
				if !ok || r.Phase != "failed" {
					return false
				}
				recs[i] = r
			}
			return true
		})
		var guard, winner int
		for _, r := range recs {
			t.Logf("task: %+v", r)
			switch {
			case strings.Contains(r.Error, "0002_email.up.sql") && strings.Contains(r.Error, "55000") && strings.Contains(r.Error, "another runner"):
				guard++
			case strings.Contains(r.Error, "0003_orders.up.sql") && strings.Contains(r.Error, "42883"):
				winner++
			}
		}
		if guard != 1 || winner != 1 {
			t.Errorf("tasks = %+v, want one stopped by the 55000 guard on 0002 and one failing on 0003", recs)
		}
		if got := admin(t, smDB, `SELECT string_agg(version::text, ',' ORDER BY version) FROM schema_migrations;`); got != "1,2" {
			t.Errorf("schema_migrations = %q, want 1,2 (0002 once)", got)
		}
		if got := admin(t, smDB, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'email';`); got != "1" {
			t.Errorf("users.email columns = %s, want 1", got)
		}
		if got := l.tables(t); got != "schema_migrations,users" {
			t.Errorf("tables = %q", got)
		}
	})
}

// lockKey は sqlmigrate の pg_advisory_xact_lock の鍵（"sakutei:sqlmigrate" の FNV-1a）。
func lockKey() string {
	h := fnv.New64a()
	h.Write([]byte("sakutei:sqlmigrate"))
	return strconv.FormatInt(int64(h.Sum64()), 10)
}
