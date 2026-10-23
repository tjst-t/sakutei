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

// sqlmigrate_cli_test.go — #94: sqlmigrate の provider を CLI（plan・reconcile・
// status・常駐の sakutei）につなぐ。データベースの一覧は --sqlmigrate-config。
// 本物の sqlite3 を使う（無ければ Skip）。
//
//	[AC-94-1]  plan は sqlmigrate と dnszone を混ぜた intent の差分を出し、
//	           sqlmigrate の差分は Task を起動するものだと分かる。DB に書かない。
//	[AC-94-2]  一回性の reconcile は strategy: task のリソースを Apply しない
//	           （AC-v1.5-task-9）。dnszone は収束する。
//	[AC-94-3]  常駐の sakutei（--task-store 付き）が Task を起動し、DB が宣言の版に
//	           収束する。続く plan は Converged、status は drifted 無し、
//	           未完了の Task は残らない。
//	[AC-94-4]  設定の未知のキーはエラー。設定に無い名前を intent に書くと
//	           plan の時点でエラー。
//	[AC-94-5]  --sqlmigrate-config 無しで sqlmigrate のリソースを書くとエラー。
package acceptance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/sqlmigrate"
)

type smCLIRig struct {
	dir    string
	intent string
	state  string
	zone   string
	config string
	db     string
}

func newSMCLIRig(t *testing.T) *smCLIRig {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 is not installed")
	}
	dir := t.TempDir()
	r := &smCLIRig{
		dir:    dir,
		intent: filepath.Join(dir, "intent"),
		state:  filepath.Join(dir, "fake-state.json"),
		zone:   filepath.Join(dir, "example.com.zone"),
		config: filepath.Join(dir, "databases.yaml"),
		db:     filepath.Join(dir, "data", "app.db"),
	}
	for _, d := range []string{r.intent, filepath.Join(dir, "data"), filepath.Join(dir, "migrations", "app")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeZoneFile(t, r.zone, multiZone)
	for name, sql := range map[string]string{
		"0001_create_users.up.sql":   "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL);",
		"0001_create_users.down.sql": "DROP TABLE users;",
		"0002_add_email.up.sql":      "ALTER TABLE users ADD COLUMN email TEXT;",
		"0002_add_email.down.sql":    "ALTER TABLE users DROP COLUMN email;",
	} {
		r.write(t, filepath.Join("migrations", "app", name), sql)
	}
	// 相対パスは設定ファイルのある場所から。
	r.write(t, "databases.yaml", `# データベースの一覧（#94）
databases:
  app:
    path: data/app.db
    migrations: migrations/app
`)
	return r
}

func (r *smCLIRig) write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (r *smCLIRig) writeIntent(t *testing.T, db, version string) {
	t.Helper()
	writeIntentFile(t, r.intent, map[string]any{
		"resources": []map[string]any{
			{
				"provider":      sqlmigrate.Name,
				"id":            db,
				"update_policy": map[string]any{"strategy": "task", "rollback_on_fail": true},
				"fields":        map[string]string{"version": version},
			},
			dnszoneResource("www.example.com./A", "www.example.com.", "A", "192.0.2.10", "300", ""),
		},
	})
}

func (r *smCLIRig) run(t *testing.T, verb string, extra ...string) (string, int) {
	t.Helper()
	args := append([]string{verb, "--intent", r.intent, "--state", r.state, "--zone", r.zone}, extra...)
	return runSakutei(t, args...)
}

// version は sqlite3 で schema_migrations の最大の版を読む（DB が無ければ "none"）。
func (r *smCLIRig) version(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat(r.db); err != nil {
		return "none"
	}
	out, err := exec.Command("sqlite3", r.db, "SELECT coalesce(max(version), 0) FROM schema_migrations;").CombinedOutput()
	if err != nil {
		return "error: " + strings.TrimSpace(string(out))
	}
	return strings.TrimSpace(string(out))
}

func TestSQLMigrateCLI_DaemonRunsTaskToDeclaredVersion(t *testing.T) {
	r := newSMCLIRig(t)
	r.writeIntent(t, "app", "2")
	cfg := []string{"--sqlmigrate-config", r.config}

	// [AC-94-1]
	out, code := r.run(t, "plan", cfg...)
	if code != 0 {
		t.Fatalf("[AC-94-1] plan exit %d:\n%s", code, out)
	}
	for _, want := range []string{"sqlmigrate//app", `"0" → "2"`, "launches a Task", "rollback_on_fail", "dnszone"} {
		if !strings.Contains(out, want) {
			t.Errorf("[AC-94-1] plan output does not mention %q:\n%s", want, out)
		}
	}
	if v := r.version(t); v != "none" {
		t.Fatalf("[AC-94-1] plan touched the database (version %s)", v)
	}

	// [AC-94-2]
	out, code = r.run(t, "reconcile", cfg...)
	if code != 0 {
		t.Fatalf("[AC-94-2] reconcile exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "RequiresTaskEvent") {
		t.Errorf("[AC-94-2] reconcile must report RequiresTaskEvent:\n%s", out)
	}
	if v := r.version(t); v != "none" {
		t.Fatalf("[AC-94-2] one-shot reconcile migrated the database (version %s):\n%s", v, out)
	}
	if z := readZoneFile(t, r.zone); !zoneHasA(z, "www.example.com.", "192.0.2.10") {
		t.Errorf("[AC-94-2] zone did not converge:\n%s", z)
	}

	// [AC-94-3]
	store := filepath.Join(r.dir, "tasks.json")
	cmd, stdout, stderr := startDaemonWithArgs(t, r.intent, r.state, "100ms",
		"--zone", r.zone, "--sqlmigrate-config", r.config, "--task-store", store)
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	end := time.Now().Add(20 * time.Second)
	for r.version(t) != "2" && time.Now().Before(end) {
		time.Sleep(100 * time.Millisecond)
	}
	if v := r.version(t); v != "2" {
		t.Fatalf("[AC-94-3] the daemon did not migrate to version 2 (got %s)\nstdout:\n%s\nstderr:\n%s", v, stdout.String(), stderr.String())
	}
	cols, err := exec.Command("sqlite3", r.db, "SELECT name FROM pragma_table_info('users') ORDER BY cid;").CombinedOutput()
	if err != nil || strings.Fields(string(cols))[len(strings.Fields(string(cols)))-1] != "email" {
		t.Errorf("[AC-94-3] users must have the email column after 0002: %v %q", err, cols)
	}
	// 完了印が観測されたら未完了の記録は消える。
	var list string
	for time.Now().Before(end) {
		list, _ = runSakutei(t, "task", "list", "--store", store)
		if strings.Contains(list, "no outstanding task") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(list, "no outstanding task") {
		t.Errorf("[AC-94-3] an outstanding task remains after convergence:\n%s", list)
	}
	if !strings.Contains(stdout.String(), "for sqlmigrate//app completed") {
		t.Errorf("[AC-94-3] the daemon log does not mention the Task of sqlmigrate//app\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	out, code = r.run(t, "plan", cfg...)
	if code != 0 || !strings.Contains(out, "Converged (no changes)") {
		t.Fatalf("[AC-94-3] plan after the Task (exit %d) is not converged:\n%s", code, out)
	}
	out, code = r.run(t, "status", cfg...)
	if code != 0 || strings.Contains(out, "DRIFTED sqlmigrate//app") || !strings.Contains(out, "drifted=0") {
		t.Fatalf("[AC-94-3] status after the Task (exit %d) still reports drift:\n%s", code, out)
	}
}

func TestSQLMigrateCLI_ConfigErrors(t *testing.T) {
	r := newSMCLIRig(t)

	// [AC-94-4] 設定に無い名前。
	r.writeIntent(t, "billing", "1")
	out, code := r.run(t, "plan", "--sqlmigrate-config", r.config)
	if code == 0 || !strings.Contains(out, `database "billing" is not configured`) {
		t.Fatalf("[AC-94-4] a database not in the config: exit %d, want an error naming it:\n%s", code, out)
	}

	// [AC-94-4] 未知のキー。
	r.writeIntent(t, "app", "1")
	r.write(t, "databases.yaml", `databases:
  app:
    path: data/app.db
    migrations: migrations/app
    migration: typo
`)
	out, code = r.run(t, "plan", "--sqlmigrate-config", r.config)
	if code == 0 || !strings.Contains(out, `unknown key "databases.app.migration"`) {
		t.Fatalf("[AC-94-4] an unknown key: exit %d, want an error naming it:\n%s", code, out)
	}

	// [AC-94-5]
	out, code = r.run(t, "plan")
	if code == 0 || !strings.Contains(out, "--sqlmigrate-config") {
		t.Fatalf("[AC-94-5] no --sqlmigrate-config: exit %d, want an error asking for it:\n%s", code, out)
	}
	if v := r.version(t); v != "none" {
		t.Fatalf("[AC-94-4/5] a rejected plan touched the database (version %s)", v)
	}
}
