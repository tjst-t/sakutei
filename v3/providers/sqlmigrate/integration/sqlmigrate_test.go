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

//go:build integration_v3_postgresql

package integration

// sqlmigrate_test.go — v2 の統合テスト（tests/integration/postgresql の
// sqlmigrate_integration_test.go）の場面を、新しいコアの sqlmigrate と v3 の CLI で
// 回す。前の step が作った現実を次の step が使うので、1 つ落ちたら残りは回さない。
//
//	1 one-shot 実行の reconcile で Procedure（on_drift → migrate → done_when）が 0 から 3 へ流し、
//	  次の plan は何もしない（E-F2・N-F2）
//	2 daemon が宣言の 1 へ down を逆順に流す
//	3 失敗する 0004：Procedure は halt し、0004 は巻き戻り（バージョン 3 のまま）、次の cycle も送り直さない
//	  （不変条件 16・E-A9）
//	4 rollback_on_fail：失敗したら呼んだときのバージョン（1）に戻して止まる
//	5 トランザクションに入れられない文（CREATE INDEX CONCURRENTLY）は 25001 で止まる
//	6 2 つの daemon（別々の Record）が同じデータベースを 1 から 3 へ：テストが advisory lock を
//	  持つ間はどちらも送れていない（55P03、ErrNotSent で次の cycle に送り直す）。外すと同じバージョンを
//	  二度流さずに 3 になり、両方の Procedure が済む（E-F2）
//	7 書き込み用の credential（secret の参照の在りか）が無い：plan は観測用の credential で読めて差分を出し、
//	  reconcile は何も流さず送れていない（E-G5・N-G3・E-G2）
//	8 サーバを止める（docker pause）と Unknown で何もしない（E-K1）
//
// 観測は sm_observe（読むだけ。schema_migrations は既定の権限で SELECT だけ）、
// invoke は sm_migrate（データベースの持ち主）でつなぐ。sm_migrate のパスワードは
// secret_backends の file から password_secret で取り出す。

import (
	"bytes"
	"context"
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

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/daemon"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/providers/sqlmigrate"
)

// 既定のイメージ。v2 の統合テストと同じ digest（2026-10-27 に docker pull で確かめた値）。
const postgresImage = "postgres:17.11-bookworm@sha256:858bb5e98eed02eb8cafbcdc007f9a65d1b613972028944a8ec2133ccaf80d27"

const (
	prefix     = "sakutei-v3sm-"
	network    = prefix + "net"
	serverName = prefix + "server"
	clientName = prefix + "client"
	adminUser  = "sakutei"
	adminPW    = "admin-pw:with\\colon"
	observePW  = "observe-pw"
	migratePW  = "migrate-pw:x"
	smDB       = "migdb"
	target     = "db/database/" + smDB
)

var lab struct {
	dir, image, wrapper, config, intent, migrations, secretDir string
}

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	start := time.Now()
	if err := setup(); err != nil {
		fmt.Fprintf(os.Stderr, "integration-v3-postgresql (sqlmigrate): %v\n", err)
		teardown()
		return 1
	}
	defer teardown()
	ready := time.Now()
	code := m.Run()
	fmt.Printf("sqlmigrate の統合テスト: 用意 %s・step %s\n", ready.Sub(start).Round(time.Second), time.Since(ready).Round(time.Second))
	return code
}

func docker(args ...string) (string, error) {
	out, err := exec.Command("sudo", append([]string{"-n", "docker"}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

const wrapperScript = `#!/bin/bash
# psql をクライアントのコンテナの中で動かす（統合テスト用）。接続の設定は
# 環境変数から受け取り、passfile は docker cp でコンテナに渡して終われば消す。
set -u
c=` + clientName + `
pf=""
if [ -n "${PGPASSFILE:-}" ]; then
  pf=/tmp/pgpass-$$-$RANDOM
  sudo -n docker cp "$PGPASSFILE" "$c:$pf" >/dev/null || exit 2
fi
envs=()
for k in PGHOST PGPORT PGDATABASE PGUSER PGSSLMODE PGSSLROOTCERT PGCONNECT_TIMEOUT PGAPPNAME PGCLIENTENCODING LC_MESSAGES; do
  if [ -n "${!k:-}" ]; then envs+=(-e "$k=${!k}"); fi
done
if [ -n "$pf" ]; then envs+=(-e "PGPASSFILE=$pf"); fi
sudo -n docker exec -i "${envs[@]}" "$c" psql "$@"
rc=$?
if [ -n "$pf" ]; then sudo -n docker exec "$c" rm -f "$pf" >/dev/null 2>&1; fi
exit $rc
`

var migrations = map[string]string{
	"0001_users.up.sql":    "CREATE TABLE users (id bigint PRIMARY KEY, name text NOT NULL);\n",
	"0001_users.down.sql":  "DROP TABLE users;\n",
	"0002_email.up.sql":    "ALTER TABLE users ADD COLUMN email text;\nCOMMENT ON COLUMN users.email IS E'mail\\naddress';\nCREATE INDEX users_email ON users (email)", // 最後の ";" 無し
	"0002_email.down.sql":  "DROP INDEX users_email;\nALTER TABLE users DROP COLUMN email;\n",
	"0003_orders.up.sql":   "CREATE TABLE orders (id bigint PRIMARY KEY, user_id bigint REFERENCES users (id), note text DEFAULT $$a'b$$);\n",
	"0003_orders.down.sql": "DROP TABLE orders;\n",
	"0004_bad.up.sql":      "CREATE TABLE half_done (x int);\nINSERT INTO no_such_table VALUES (1);\n",
	"0004_bad.down.sql":    "DROP TABLE half_done;\n",
}

func setup() error {
	if out, err := docker("version", "--format", "{{.Server.Version}}"); err != nil {
		return fmt.Errorf("docker（sudo -n で）が要る: %v: %s", err, out)
	}
	lab.image = postgresImage
	if v := os.Getenv("SAKUTEI_IT_POSTGRES_IMAGE"); v != "" {
		lab.image = v
	}
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		return err
	}
	lab.dir = dir
	if _, err := docker("image", "inspect", lab.image); err != nil {
		if out, err := docker("pull", lab.image); err != nil {
			return fmt.Errorf("docker pull %s: %v: %s", lab.image, err, out)
		}
	}
	teardownContainers() // 前の回の残り。
	if out, err := docker("network", "create", network); err != nil {
		return fmt.Errorf("docker network create: %v: %s", err, out)
	}
	if out, err := docker("run", "-d", "--name", serverName, "--network", network,
		"-e", "POSTGRES_USER="+adminUser, "-e", "POSTGRES_PASSWORD="+adminPW, lab.image); err != nil {
		return fmt.Errorf("docker run server: %v: %s", err, out)
	}
	if out, err := docker("run", "-d", "--name", clientName, "--network", network, "--entrypoint", "sleep", lab.image, "infinity"); err != nil {
		return fmt.Errorf("docker run client: %v: %s", err, out)
	}
	lab.wrapper = filepath.Join(dir, "psql")
	if err := os.WriteFile(lab.wrapper, []byte(wrapperScript), 0o755); err != nil {
		return err
	}
	if err := waitReady(2 * time.Minute); err != nil {
		out, _ := docker("logs", "--tail", "20", serverName)
		return fmt.Errorf("server: %v\n%s", err, out)
	}
	for db, sql := range map[string]string{
		"postgres": "CREATE ROLE sm_observe LOGIN PASSWORD '" + observePW + "';\nCREATE ROLE sm_migrate LOGIN PASSWORD '" + migratePW + "';\n" +
			"CREATE DATABASE " + smDB + " OWNER sm_migrate;\n",
		smDB: "REVOKE CREATE ON SCHEMA public FROM PUBLIC;\n" +
			"ALTER DEFAULT PRIVILEGES FOR ROLE sm_migrate IN SCHEMA public GRANT SELECT ON TABLES TO sm_observe;\n",
	} {
		if out, err := psqlAs(adminUser, adminPW, db, sql); err != nil {
			return fmt.Errorf("用意の SQL（%s）: %v\n%s", db, err, out)
		}
	}
	lab.migrations = filepath.Join(dir, "migrations")
	lab.intent = filepath.Join(dir, "intent")
	lab.secretDir = filepath.Join(dir, "secrets")
	for _, d := range []string{lab.migrations, lab.intent, lab.secretDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	for name, sql := range migrations {
		if err := os.WriteFile(filepath.Join(lab.migrations, name), []byte(sql), 0o644); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "observe-password"), []byte(observePW+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(lab.secretDir, "migrate"), []byte(migratePW+"\n"), 0o600); err != nil {
		return err
	}
	lab.config = filepath.Join(dir, "config.yaml")
	cfg := `secret_backends:
  files:
    module: file
    settings: {dir: ` + lab.secretDir + `}
providers:
  db:
    module: sqlmigrate
    settings:
      psql: ` + lab.wrapper + `
      lock_timeout: 1s
      databases:
        ` + smDB + `:
          engine: postgresql
          host: ` + serverName + `
          port: 5432
          connect_timeout: 2s
          migrations: ` + lab.migrations + `
          observe: {user: sm_observe, password_file: ` + filepath.Join(dir, "observe-password") + `}
          write: {user: sm_migrate, password_secret: "files:migrate"}
`
	return os.WriteFile(lab.config, []byte(cfg), 0o600)
}

// waitReady は管理用のロールで SELECT 1 が通るまで待つ。
func waitReady(d time.Duration) error {
	deadline := time.Now().Add(d)
	var last error
	for time.Now().Before(deadline) {
		out, err := psqlAs(adminUser, adminPW, "postgres", "SELECT 1;")
		if err == nil && strings.TrimSpace(out) == "1" {
			return nil
		}
		last = fmt.Errorf("%v: %s", err, out)
		time.Sleep(time.Second)
	}
	return fmt.Errorf("%s 待っても用意ができない: %v", d, last)
}

func teardownContainers() {
	for _, c := range []string{serverName, clientName} {
		_, _ = docker("rm", "-f", "-v", c)
	}
	_, _ = docker("network", "rm", network)
}

func teardown() {
	_, _ = docker("unpause", serverName)
	teardownContainers()
	if lab.dir != "" {
		os.RemoveAll(lab.dir)
	}
}

// psqlAs は user で db につないで sql を流す（sakutei の外の操作）。
func psqlAs(user, password, db, sql string) (string, error) {
	pf := filepath.Join(lab.dir, fmt.Sprintf("pgpass-%d", time.Now().UnixNano()))
	esc := strings.NewReplacer(`\`, `\\`, `:`, `\:`).Replace(password)
	if err := os.WriteFile(pf, []byte("*:*:*:*:"+esc+"\n"), 0o600); err != nil {
		return "", err
	}
	defer os.Remove(pf)
	cmd := exec.Command(lab.wrapper, "-X", "-w", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-f", "-")
	cmd.Env = append(os.Environ(), "PGHOST="+serverName, "PGUSER="+user, "PGDATABASE="+db, "PGPASSFILE="+pf, "PGCONNECT_TIMEOUT=3")
	cmd.Stdin = strings.NewReader(sql)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func admin(t *testing.T, db, sql string) string {
	t.Helper()
	out, err := psqlAs(adminUser, adminPW, db, sql)
	if err != nil {
		t.Fatalf("psql（%s）: %v\n%s\n%s", db, err, sql, out)
	}
	return strings.TrimSpace(out)
}

// version は schema_migrations のバージョン（sakutei の外から読む。表が無ければ "0"）。
func version(t *testing.T) string {
	t.Helper()
	if admin(t, smDB, `SELECT to_regclass('schema_migrations') IS NOT NULL;`) != "t" {
		return "0"
	}
	return admin(t, smDB, `SELECT coalesce(max(version), 0) FROM schema_migrations;`)
}

func tables(t *testing.T) string {
	t.Helper()
	return admin(t, smDB, `SELECT coalesce(string_agg(tablename, ',' ORDER BY tablename), '') FROM pg_tables WHERE schemaname = 'public';`)
}

func writeMigration(t *testing.T, name, sql string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(lab.migrations, name), []byte(sql), 0o644); err != nil {
		t.Fatal(err)
	}
}

// declare はバージョン v を宣言し、Procedure migrate で合わせる intent を書く。
func declare(t *testing.T, v int, rollback bool) {
	t.Helper()
	intent := fmt.Sprintf(`resources:
  - {type: db/database, id: %s, fields: {version: %d}}
procedures:
  - name: migrate
    target: {type: db/database, id: %s}
    trigger: on_drift
    drift_fields: [version]
    steps:
      - name: migrate
        invoke: {op: migrate, args: {version: "{desired.version}", rollback_on_fail: %t}}
    done_when: 'observe(db/database/{target}).version == desired.version'
`, smDB, v, smDB, rollback)
	if err := os.WriteFile(filepath.Join(lab.intent, "db.yaml"), []byte(intent), 0o644); err != nil {
		t.Fatal(err)
	}
}

func recordDir(name string) string { return filepath.Join(lab.dir, "record-"+name) }

// sakutei は v3 の CLI（plan か reconcile）を 1 回動かす。
func sakutei(t *testing.T, verb, record string) (plan.Result, int, string) {
	t.Helper()
	args := []string{verb, "--intent", lab.intent, "--config", lab.config, "--json"}
	if record != "" {
		args = append(args, "--record", record)
	}
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), args, &stdout, &stderr, cli.WithModule("sqlmigrate", sqlmigrate.Module))
	var r plan.Result
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("%s の出力を読めない: %v\n%s\n%s", verb, err, stdout.String(), stderr.String())
	}
	return r, code, stdout.String() + stderr.String()
}

func dump(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

func runOf(r *plan.Result) plan.RunStatus {
	if r == nil {
		return plan.RunStatus{}
	}
	for _, s := range r.Runs {
		if s.Target == target {
			return s
		}
	}
	return plan.RunStatus{}
}

// converge は Procedure が済むまで one-shot 実行の reconcile を回す。
func converge(t *testing.T, record, want string) {
	t.Helper()
	var out string
	for i := 0; i < 5; i++ {
		r, code, o := sakutei(t, "reconcile", record)
		out = o
		if code == cli.ExitOK && version(t) == want && !runOf(&r).Active() {
			return
		}
	}
	t.Fatalf("バージョン %s に収束しない（今 %s）\n%s", want, version(t), out)
}

// halted は Procedure が halt するまで one-shot 実行の reconcile を回し、halt した Procedure を返す。
func halted(t *testing.T, record string) plan.RunStatus {
	t.Helper()
	var s plan.RunStatus
	var out string
	for i := 0; i < 5 && s.State != "halted"; i++ {
		var r plan.Result
		r, _, out = sakutei(t, "reconcile", record)
		s = runOf(&r)
	}
	if s.State != "halted" {
		t.Fatalf("Procedure が halt しない\n%s", out)
	}
	return s
}

// daemonRun は daemon の sakutei（cli.Run、cycle の間隔は 300ms）。
type daemonRun struct {
	mu     sync.Mutex
	last   *plan.Result
	seen   []plan.RunStatus
	cancel context.CancelFunc
	done   chan struct{}
	out    bytes.Buffer
}

func (d *daemonRun) latest() (*plan.Result, []plan.RunStatus) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.last, append([]plan.RunStatus(nil), d.seen...)
}

func (d *daemonRun) stop() {
	d.cancel()
	<-d.done
}

func startDaemon(t *testing.T, record string) *daemonRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	d := &daemonRun{cancel: cancel, done: make(chan struct{})}
	wait := func(ctx context.Context, _ time.Duration, wake <-chan struct{}) bool {
		select {
		case <-ctx.Done():
			return false
		case <-wake:
		case <-time.After(300 * time.Millisecond):
		}
		return true
	}
	onCycle := func(c daemon.Cycle) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if c.Result != nil {
			d.last = c.Result
			if s := runOf(c.Result); s.Procedure != "" {
				d.seen = append(d.seen, s)
			}
		}
	}
	go func() {
		defer close(d.done)
		var stdout, stderr bytes.Buffer
		cli.Run(ctx, []string{"--intent", lab.intent, "--config", lab.config, "--record", record},
			&stdout, &stderr, cli.WithModule("sqlmigrate", sqlmigrate.Module), cli.WithDaemonWait(wait), cli.WithDaemonCycle(onCycle))
		d.mu.Lock()
		d.out.Write(stdout.Bytes())
		d.out.Write(stderr.Bytes())
		d.mu.Unlock()
	}()
	t.Cleanup(d.stop)
	return d
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		ok, s := cond()
		if ok {
			return
		}
		last = s
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("%s を %s 待っても来ない: %s", what, timeout, last)
}

func TestSQLMigrate(t *testing.T) {
	steps := []struct {
		name string
		f    func(*testing.T)
	}{
		{"01_up_oneshot", stepUp},
		{"02_down_daemon", stepDown},
		{"03_failure_halts", stepFailure},
		{"04_rollback_on_fail", stepRollbackOnFail},
		{"05_not_in_transaction", stepConcurrently},
		{"06_two_daemons", stepTwoDaemons},
		{"07_write_credentials", stepWriteCredentials},
		{"08_unreachable", stepUnreachable},
	}
	for _, s := range steps {
		if !t.Run(s.name, s.f) {
			t.Fatalf("step %s が落ちたので残りは回さない", s.name)
		}
	}
}

func stepUp(t *testing.T) {
	declare(t, 3, false)
	r, code, out := sakutei(t, "plan", "")
	if code == cli.ExitError || code == cli.ExitRejected || runOf(&r).Procedure == "" || version(t) != "0" {
		t.Fatalf("plan: exit=%d バージョン %s\n%s", code, version(t), out)
	}
	converge(t, recordDir("up"), "3")
	if got := tables(t); got != "orders,schema_migrations,users" {
		t.Errorf("表 %q", got)
	}
	if got := admin(t, smDB, `SELECT string_agg(version::text, ',' ORDER BY version) FROM schema_migrations WHERE applied_at IS NOT NULL;`); got != "1,2,3" {
		t.Errorf("schema_migrations %q", got)
	}
	if got := admin(t, smDB, `SELECT col_description('users'::regclass, 3) || '|' || (SELECT column_default FROM information_schema.columns WHERE table_name = 'orders' AND column_name = 'note');`); got != "mail\naddress|'a''b'::text" {
		t.Errorf("E'' と $$ の引用 %q", got)
	}
	// 書いたのは sm_migrate（観測用の sm_observe は表を持たない）。
	if got := admin(t, smDB, `SELECT string_agg(DISTINCT tableowner, ',') FROM pg_tables WHERE schemaname = 'public';`); got != "sm_migrate" {
		t.Errorf("表の持ち主 %q（want sm_migrate）", got)
	}
	if r, code, out := sakutei(t, "plan", ""); code != cli.ExitOK || len(r.Runs) != 0 {
		t.Fatalf("収束したあとの plan: exit=%d\n%s", code, out)
	}
}

func stepDown(t *testing.T) {
	declare(t, 1, false)
	d := startDaemon(t, recordDir("down"))
	waitFor(t, "バージョン 1 と Procedure が済む", time.Minute, func() (bool, string) {
		r, _ := d.latest()
		v := version(t)
		return v == "1" && r != nil && !runOf(r).Active(), "バージョン " + v
	})
	d.stop()
	if got := tables(t); got != "schema_migrations,users" {
		t.Errorf("表 %q", got)
	}
	if got := admin(t, smDB, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'email';`); got != "0" {
		t.Errorf("down のあと users.email が残る")
	}
}

func stepFailure(t *testing.T) {
	rec := recordDir("fail")
	declare(t, 3, false)
	converge(t, rec, "3")
	declare(t, 4, false)
	s := halted(t, rec)
	if !strings.Contains(s.Reason, "0004_bad.up.sql") || !strings.Contains(s.Reason, "42P01") || !strings.Contains(s.Reason, "バージョン 3") {
		t.Errorf("止めた理由 %q（want 0004 が 42P01 で失敗しバージョン 3）", s.Reason)
	}
	if v := version(t); v != "3" {
		t.Errorf("バージョン %s（want 3：0004 は巻き戻る）", v)
	}
	if got := tables(t); strings.Contains(got, "half_done") {
		t.Errorf("表 %q：失敗した 1 本が half_done を残した", got)
	}
	// halt した Procedure は次の cycle も送り直さない。
	r, code, out := sakutei(t, "reconcile", rec)
	if code == cli.ExitOK || runOf(&r).State != "halted" || version(t) != "3" {
		t.Fatalf("halt した Procedure: exit=%d\n%s", code, out)
	}
}

func stepRollbackOnFail(t *testing.T) {
	rec := recordDir("rollback")
	declare(t, 1, false)
	converge(t, rec, "1")
	declare(t, 4, true)
	s := halted(t, rec)
	if !strings.Contains(s.Reason, "バージョン 1 に戻した") || !strings.Contains(s.Reason, "0004_bad.up.sql") {
		t.Errorf("止めた理由 %q（want バージョン 1 に戻した）", s.Reason)
	}
	if v := version(t); v != "1" {
		t.Errorf("バージョン %s（want 1）", v)
	}
	if got := tables(t); got != "schema_migrations,users" {
		t.Errorf("戻したあとの表 %q", got)
	}
}

func stepConcurrently(t *testing.T) {
	writeMigration(t, "0002_email.up.sql", "ALTER TABLE users ADD COLUMN email text;\nCREATE INDEX CONCURRENTLY users_email ON users (email);\n")
	defer writeMigration(t, "0002_email.up.sql", migrations["0002_email.up.sql"])
	declare(t, 2, false)
	s := halted(t, recordDir("concurrently"))
	if !strings.Contains(s.Reason, "25001") || !strings.Contains(s.Reason, "0002_email.up.sql") {
		t.Errorf("止めた理由 %q（want 0002 が 25001）", s.Reason)
	}
	if v := version(t); v != "1" {
		t.Errorf("バージョン %s（want 1）", v)
	}
	if got := admin(t, smDB, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'email';`); got != "0" {
		t.Errorf("失敗した 1 本が users.email を残した")
	}
}

// lockKey は sqlmigrate の pg_advisory_xact_lock のキー（"sakutei:sqlmigrate" の FNV-1a）。
func lockKey() string {
	h := fnv.New64a()
	h.Write([]byte("sakutei:sqlmigrate"))
	return strconv.FormatInt(int64(h.Sum64()), 10)
}

func stepTwoDaemons(t *testing.T) {
	if v := version(t); v != "1" {
		t.Fatalf("バージョン %s（want 1）", v)
	}
	declare(t, 3, false)
	held := make(chan error, 1)
	go func() {
		out, err := psqlAs(adminUser, adminPW, smDB, "SELECT pg_advisory_lock("+lockKey()+");\nSELECT pg_sleep(6);\n")
		if err != nil {
			err = fmt.Errorf("%v: %s", err, out)
		}
		held <- err
	}()
	waitFor(t, "テストがロックを持つ", 30*time.Second, func() (bool, string) {
		return admin(t, smDB, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted;`) == "1", ""
	})
	a := startDaemon(t, recordDir("race-a"))
	b := startDaemon(t, recordDir("race-b"))
	// ロックを持たれている間はどちらも送れていない（55P03 → ErrNotSent、次の cycle に送り直す）。
	waitFor(t, "両方の daemon がロックを待ちきれず送れていない", 30*time.Second, func() (bool, string) {
		ok := true
		var why []string
		for _, d := range []*daemonRun{a, b} {
			r, _ := d.latest()
			s := runOf(r)
			if !strings.Contains(s.Waiting, "送れていない") || !strings.Contains(s.Waiting, "55P03") {
				ok = false
			}
			why = append(why, s.Waiting)
		}
		return ok, strings.Join(why, " / ")
	})
	if v := version(t); v != "1" {
		t.Fatalf("ロックの間にバージョン %s（want 1）", v)
	}
	if err := <-held; err != nil {
		t.Fatalf("ロックを持つ: %v", err)
	}
	waitFor(t, "バージョン 3 と両方の Procedure が済む", time.Minute, func() (bool, string) {
		v := version(t)
		ok := v == "3"
		for _, d := range []*daemonRun{a, b} {
			r, _ := d.latest()
			if r == nil || runOf(r).Active() || runOf(r).State == "halted" {
				ok = false
			}
		}
		return ok, "バージョン " + v
	})
	a.stop()
	b.stop()
	for i, d := range []*daemonRun{a, b} {
		_, seen := d.latest()
		for _, s := range seen {
			if s.State == "halted" || s.Error != "" {
				t.Errorf("daemon %d の Procedure が halt した: %+v", i, s)
			}
		}
	}
	if got := admin(t, smDB, `SELECT string_agg(version::text, ',' ORDER BY version) FROM schema_migrations;`); got != "1,2,3" {
		t.Errorf("schema_migrations %q（want 1,2,3：同じバージョンを二度流さない）", got)
	}
	if got := admin(t, smDB, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'email';`); got != "1" {
		t.Errorf("users.email の列 %s（want 1）", got)
	}
	if got := tables(t); got != "orders,schema_migrations,users" {
		t.Errorf("表 %q", got)
	}
}

func stepWriteCredentials(t *testing.T) {
	declare(t, 1, false)
	secret := filepath.Join(lab.secretDir, "migrate")
	if err := os.Rename(secret, secret+".away"); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(secret+".away", secret)
	r, code, out := sakutei(t, "plan", "")
	if code == cli.ExitError || code == cli.ExitRejected || runOf(&r).Procedure == "" {
		t.Fatalf("書き込み用の credential が無いと plan が観測できない: exit=%d\n%s", code, out)
	}
	for i := 0; i < 2; i++ {
		r, _, out = sakutei(t, "reconcile", recordDir("nowrite"))
	}
	if s := runOf(&r); s.State == "halted" || !strings.Contains(s.Waiting, "送れていない") {
		t.Fatalf("credential が無いのに送れていないでない: %+v\n%s", s, out)
	}
	if v := version(t); v != "3" {
		t.Fatalf("credential が無いのに流した（バージョン %s）", v)
	}
	if err := os.Rename(secret+".away", secret); err != nil {
		t.Fatal(err)
	}
	converge(t, recordDir("nowrite"), "1")
}

func stepUnreachable(t *testing.T) {
	declare(t, 2, false)
	if out, err := docker("pause", serverName); err != nil {
		t.Fatalf("docker pause: %v: %s", err, out)
	}
	defer docker("unpause", serverName)
	r, code, out := sakutei(t, "reconcile", recordDir("unreachable"))
	if _, err := docker("unpause", serverName); err != nil {
		t.Fatal(err)
	}
	var c plan.Change
	for _, ch := range r.Changes {
		if ch.Resource == target {
			c = ch
		}
	}
	if code != cli.ExitRemaining || c.Op != plan.Unknown || runOf(&r).State == "halted" {
		t.Fatalf("止めたサーバ: exit=%d %+v\n%s", code, c, out)
	}
	if v := version(t); v != "1" {
		t.Fatalf("届かないのに流した（バージョン %s）", v)
	}
	t.Logf("届かないときの観測: %s", c.Note)
}
