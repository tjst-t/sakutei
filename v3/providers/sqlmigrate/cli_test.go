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

package sqlmigrate_test

// v3 の CLI（cli.Run の plan・reconcile）で、v2 の strategy: task を書き直した Procedure
// （trigger: on_drift・invoke: migrate・done_when）を本物の sqlite3 に向けて回す
// （§3.7・§7.1、E-F2・N-F2・不変条件 15・16・18）。

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/providers/sqlmigrate"
)

type cliLab struct {
	t                                   *testing.T
	bin, db, migrations, intent, config string
	record                              string
}

func newCLILab(t *testing.T) *cliLab {
	t.Helper()
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 が無い")
	}
	dir := t.TempDir()
	l := &cliLab{t: t, bin: bin, db: filepath.Join(dir, "app.db"), migrations: filepath.Join(dir, "migrations"),
		intent: filepath.Join(dir, "intent"), config: filepath.Join(dir, "config.yaml"), record: filepath.Join(dir, "record")}
	for _, d := range []string{l.migrations, l.intent} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{
		"0001_users.up.sql":   "CREATE TABLE users (id INTEGER PRIMARY KEY);\n",
		"0001_users.down.sql": "DROP TABLE users;\n",
		"0002_email.up.sql":   "ALTER TABLE users ADD COLUMN email TEXT;\n",
		"0002_email.down.sql": "ALTER TABLE users DROP COLUMN email;\n",
		"0003_bad.up.sql":     "CREATE TABLE half_done (x INTEGER);\nINSERT INTO nope VALUES (1);\n",
	} {
		l.write(filepath.Join(l.migrations, name), body)
	}
	l.write(l.config, `providers:
  db:
    module: sqlmigrate
    settings:
      sqlite3: `+bin+`
      databases:
        app: {path: `+l.db+`, migrations: `+l.migrations+`}
`)
	return l
}

func (l *cliLab) write(path, body string) {
	l.t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		l.t.Fatal(err)
	}
}

// declare はバージョン version を宣言し、Procedure migrate-app で合わせる intent を書く。
func (l *cliLab) declare(version string, rollback bool) {
	l.t.Helper()
	rb := "false"
	if rollback {
		rb = "true"
	}
	l.write(filepath.Join(l.intent, "db.yaml"), `resources:
  - {type: db/database, id: app, fields: {version: `+version+`}}
procedures:
  - name: migrate-app
    target: {type: db/database, id: app}
    trigger: on_drift
    drift_fields: [version]
    steps:
      - name: migrate
        invoke: {op: migrate, args: {version: "{desired.version}", rollback_on_fail: `+rb+`}}
    done_when: 'observe(db/database/{target}).version == desired.version'
`)
}

func (l *cliLab) run(verb string) (plan.Result, int, string) {
	l.t.Helper()
	args := []string{verb, "--intent", l.intent, "--config", l.config, "--json"}
	if verb == "reconcile" {
		args = append(args, "--record", l.record)
	}
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), args, &stdout, &stderr, cli.WithModule("sqlmigrate", sqlmigrate.Module))
	var r plan.Result
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		l.t.Fatalf("%s の出力を読めない: %v\n%s\n%s", verb, err, stdout.String(), stderr.String())
	}
	return r, code, stdout.String() + stderr.String()
}

func (l *cliLab) version() string {
	l.t.Helper()
	if _, err := os.Stat(l.db); os.IsNotExist(err) {
		return "0"
	}
	out, err := exec.Command(l.bin, "-batch", "-noheader", l.db, "SELECT coalesce(max(version), 0) FROM schema_migrations;").CombinedOutput()
	if err != nil {
		l.t.Fatalf("sqlite3: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func runOf(r plan.Result) plan.RunStatus {
	for _, s := range r.Runs {
		if s.Procedure == "migrate-app" {
			return s
		}
	}
	return plan.RunStatus{}
}

// converge は Procedure が済む（done）まで reconcile を回す（one-shot 実行。migrate の step は
// critical でないので one-shot 実行でも進む）。
func (l *cliLab) converge(want string) {
	l.t.Helper()
	var out string
	for i := 0; i < 4; i++ {
		r, code, o := l.run("reconcile")
		out = o
		if code == cli.ExitOK && l.version() == want && !runOf(r).Active() {
			return
		}
	}
	l.t.Fatalf("バージョン %s に収束しない（今 %s）\n%s", want, l.version(), out)
}

func TestProcedureThroughCLI(t *testing.T) {
	l := newCLILab(t)

	// plan は何も流さず、Procedure を起こすと言う（不変条件 30）。
	l.declare("2", false)
	r, code, out := l.run("plan")
	if code == cli.ExitError || code == cli.ExitRejected || l.version() != "0" {
		t.Fatalf("plan: exit=%d バージョン %s\n%s", code, l.version(), out)
	}
	if s := runOf(r); s.Procedure == "" {
		t.Fatalf("plan が Procedure を起こすと言わない\n%s", out)
	}
	if _, err := os.Stat(l.db); !os.IsNotExist(err) {
		t.Fatal("plan が DB のファイルを作った")
	}

	// on_drift → migrate → done_when で 0 から 2 へ。
	l.converge("2")
	if r, code, out := l.run("plan"); code != cli.ExitOK || len(r.Runs) != 0 {
		t.Fatalf("収束したあとの plan: exit=%d\n%s", code, out)
	}

	// 宣言を下げると down で 1 へ。
	l.declare("1", false)
	l.converge("1")

	// 失敗する 0003：Procedure は halt し、バージョンは 2 のまま（0002 は残り 0003 は巻き戻る）。
	// halt した Procedure は次の cycle も送り直さない（不変条件 16・E-A9）。
	l.declare("3", false)
	var halted plan.RunStatus
	for i := 0; i < 3 && halted.State != "halted"; i++ {
		r, _, _ := l.run("reconcile")
		halted = runOf(r)
	}
	if halted.State != "halted" || !strings.Contains(halted.Reason, "0003_bad.up.sql") || l.version() != "2" {
		t.Fatalf("失敗で止まらない: %+v バージョン %s", halted, l.version())
	}
	before := l.version()
	r, code, out = l.run("reconcile")
	if code == cli.ExitOK || runOf(r).State != "halted" || l.version() != before {
		t.Fatalf("halt した Procedure を送り直した: exit=%d %+v\n%s", code, runOf(r), out)
	}
}

// rollback_on_fail：失敗したら呼んだときのバージョンに戻して止まる（E-F2・N-F2）。
func TestProcedureRollbackOnFailThroughCLI(t *testing.T) {
	l := newCLILab(t)
	l.declare("1", false)
	l.converge("1")
	l.declare("3", true)
	var s plan.RunStatus
	for i := 0; i < 3 && s.State != "halted"; i++ {
		r, _, _ := l.run("reconcile")
		s = runOf(r)
	}
	if s.State != "halted" || !strings.Contains(s.Reason, "バージョン 1 に戻した") || l.version() != "1" {
		t.Fatalf("戻して止まらない: %+v バージョン %s", s, l.version())
	}
}
