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

// イメージは環境変数 SAKUTEI_IT_POSTGRES_IMAGE で差し替えられる（既定は tag と
// digest で固定）。サーバ（sakutei-pgit-server）とクライアント
// （sakutei-pgit-client、psql を動かすだけ）を同じ docker のネットワーク
// （sakutei-pgit-net）に置く。サーバはホストに公開しない。

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// 既定のイメージ。digest は docker pull で確かめた値（2026-10-27）。
const postgresImage = "postgres:17.11-bookworm@sha256:858bb5e98eed02eb8cafbcdc007f9a65d1b613972028944a8ec2133ccaf80d27"

const (
	prefix     = "sakutei-pgit-"
	network    = prefix + "net"
	serverName = prefix + "server"
	clientName = prefix + "client"
	adminUser  = "sakutei"
	adminPW    = "admin-pw:with\\colon"
	appPW      = "app-pw-gen1"
	appPW2     = "app-pw-gen2"
	weirdRole  = `sk it "o'q\`
	weirdDB    = `sk-db 'x"; --`
)

var lab struct {
	dir     string
	bin     string
	wrapper string
	config  string
	image   string
}

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	if err := setup(); err != nil {
		fmt.Fprintf(os.Stderr, "integration-postgresql: %v\n", err)
		teardown()
		return 1
	}
	defer teardown()
	return m.Run()
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

func setup() error {
	if out, err := docker("version", "--format", "{{.Server.Version}}"); err != nil {
		return fmt.Errorf("docker (through sudo -n) is required: %v: %s", err, out)
	}
	lab.image = postgresImage
	if v, ok := os.LookupEnv("SAKUTEI_IT_POSTGRES_IMAGE"); ok && v != "" {
		lab.image = v
	}
	dir, err := os.MkdirTemp("", "sakutei-pg-it-")
	if err != nil {
		return err
	}
	lab.dir = dir
	if out, err := docker("pull", lab.image); err != nil {
		return fmt.Errorf("docker pull %s: %v: %s", lab.image, err, out)
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
	store := map[string]any{"secrets": map[string]any{
		"pg/admin": map[string]any{"gen": 1, "value": adminPW},
		"pg/app":   map[string]any{"gen": 1, "value": appPW},
	}}
	if err := writeJSON(filepath.Join(dir, "secrets.json"), store); err != nil {
		return err
	}
	cfg := "psql: psql\nsecret_store: secrets.json\nservers:\n  main:\n    host: " + serverName +
		"\n    port: 5432\n    user: " + adminUser + "\n    password: secretref:file/pg/admin\n    connect_timeout: 3s\n    verify_passwords: true\n"
	lab.config = filepath.Join(dir, "pg.yaml")
	if err := os.WriteFile(lab.config, []byte(strings.Replace(cfg, "psql: psql", "psql: "+lab.wrapper, 1)), 0o600); err != nil {
		return err
	}
	if err := waitReady(2 * time.Minute); err != nil {
		out, _ := docker("logs", "--tail", "20", serverName)
		return fmt.Errorf("server: %v\n%s", err, out)
	}
	lab.bin = filepath.Join(dir, "sakutei")
	build := exec.Command("go", "build", "-o", lab.bin, "./cmd/sakutei")
	build.Dir = "../../.."
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("go build: %v\n%s", err, out)
	}
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
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
	return fmt.Errorf("not ready after %s: %v", d, last)
}

func teardownContainers() {
	for _, c := range []string{serverName, clientName} {
		_, _ = docker("rm", "-f", "-v", c)
	}
	_, _ = docker("network", "rm", network)
}

func teardown() {
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
		t.Fatalf("psql (%s): %v\n%s\n%s", db, err, sql, out)
	}
	return strings.TrimSpace(out)
}

// ---- intent と CLI ----

type resource struct {
	Provider       string            `json:"provider"`
	ID             string            `json:"id"`
	Fields         map[string]string `json:"fields"`
	DeletionPolicy string            `json:"deletion_policy,omitempty"`
}

func r(id string, fields map[string]string) resource {
	return resource{Provider: "postgresql", ID: "main/" + id, Fields: fields}
}

// declared は宣言（hstore の版は引数）。
func declared(hstore string) []resource {
	return []resource{
		r("role/app", map[string]string{"login": "true", "createdb": "false", "connection_limit": "10",
			"password": "secretref:file/pg/app#gen1", "member_of": "pg_read_all_data",
			"bypassrls": "false", "replication": "false"}),
		r("role/reader", map[string]string{"login": "false", "member_of": "app"}),
		r("role/"+weirdRole, map[string]string{"login": "false", "inherit": "true"}),
		r("database/appdb", map[string]string{"owner": "app", "encoding": "UTF8", "lc_collate": "C", "lc_ctype": "C",
			"template": "template0", "connection_limit": "50"}),
		r("database/"+weirdDB, map[string]string{"owner": weirdRole}),
		r("database/appdb/grant/reader", map[string]string{"privileges": "CONNECT,TEMP"}),
		r("database/appdb/extension/hstore", map[string]string{"version": hstore}),
		r("database/appdb/extension/citext", map[string]string{}),
		// PUBLIC から全部外す（新しいデータベースの既定の CONNECT・TEMPORARY も）。
		r("database/appdb/grant/PUBLIC", map[string]string{"privileges": ""}),
	}
}

func withPrune(rs []resource) []resource {
	out := append([]resource(nil), rs...)
	for i := range out {
		out[i].DeletionPolicy = "PruneWithinScope"
	}
	return out
}

func writeIntent(t *testing.T, rs []resource) {
	t.Helper()
	dir := filepath.Join(lab.dir, "intent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(map[string]any{"resources": rs}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pg.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func cliArgs(verb string, extra ...string) []string {
	return append([]string{verb,
		"--intent", filepath.Join(lab.dir, "intent"),
		"--state", filepath.Join(lab.dir, "fake-state.json"),
		"--cause-state", filepath.Join(lab.dir, "cause-state.json"),
		"--postgresql-config", lab.config,
	}, extra...)
}

func sakutei(t *testing.T, verb string, extra ...string) (string, int) {
	t.Helper()
	out, err := exec.Command(lab.bin, cliArgs(verb, extra...)...).CombinedOutput()
	for _, s := range []string{adminPW, appPW, appPW2, "SCRAM-SHA-256$"} {
		if strings.Contains(string(out), s) {
			t.Fatalf("sakutei %s shows a secret (%q):\n%s", verb, s, out)
		}
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		t.Fatalf("sakutei %s: %v", verb, err)
	}
	return string(out), 0
}

type statusDoc struct {
	Complete bool `json:"complete"`
	Summary  struct {
		Conformant   int `json:"conformant"`
		Drift        int `json:"drift"`
		Missing      int `json:"missing"`
		Unobservable int `json:"unobservable"`
	} `json:"summary"`
	Resources []struct {
		LocalID string `json:"local_id"`
		State   string `json:"state"`
	} `json:"resources"`
}

func status(t *testing.T) (statusDoc, int, string) {
	t.Helper()
	cmd := exec.Command(lab.bin, cliArgs("status", "--json")...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("sakutei status: %v", err)
	}
	var doc statusDoc
	if jerr := json.Unmarshal(out, &doc); jerr != nil {
		t.Fatalf("status --json: %v (%v)\n%s%s", jerr, err, out, stderr.String())
	}
	return doc, code, stderr.String()
}

// converge は reconcile を（同じ周にできなかったものがあれば）3 周まで回し、
// 全部が適合になったことを確かめる。
func converge(t *testing.T, n int) string {
	t.Helper()
	var logs []string
	for i := 0; i < 3; i++ {
		out, code := sakutei(t, "reconcile")
		logs = append(logs, out)
		if code != 0 {
			t.Fatalf("reconcile exited %d:\n%s", code, out)
		}
		if s, _, _ := status(t); s.Summary.Conformant == n && s.Summary.Drift == 0 {
			return strings.Join(logs, "\n")
		}
	}
	s, _, errOut := status(t)
	t.Fatalf("not conformant after 3 reconciles: %+v\n%s\n%s", s.Summary, strings.Join(logs, "\n"), errOut)
	return ""
}

func drifted(s statusDoc) []string {
	var out []string
	for _, r := range s.Resources {
		if r.State != "conformant" && r.State != "unmanaged" { // 公式のイメージは POSTGRES_USER の名前の DB を作る（宣言外）。
			out = append(out, r.LocalID+"="+r.State)
		}
	}
	sort.Strings(out)
	return out
}

// publicPrivileges は PUBLIC が db に持つ権限（"," 区切り、名前の順。datacl が
// NULL なら既定の分）。
func publicPrivileges(t *testing.T, db string) string {
	t.Helper()
	lit := "E'" + strings.NewReplacer(`'`, `''`, `\`, `\\`).Replace(db) + "'"
	return admin(t, "postgres", `SELECT coalesce(string_agg(DISTINCT a.privilege_type, ',' ORDER BY a.privilege_type), '')
  FROM pg_database d, aclexplode(coalesce(d.datacl, acldefault('d', d.datdba))) a
 WHERE d.datname = `+lit+` AND a.grantee = 0;`)
}

func TestPostgreSQLIntegration(t *testing.T) {
	n := len(declared("1.7"))

	t.Run("create from empty", func(t *testing.T) {
		writeIntent(t, declared("1.7"))
		s, _, _ := status(t)
		if s.Summary.Missing != n {
			t.Fatalf("before: %+v; want %d missing", s.Summary, n)
		}
		out := converge(t, n)
		t.Logf("reconcile:\n%s", out)
		got := admin(t, "postgres", `SELECT rolcanlogin, rolcreatedb, rolconnlimit FROM pg_roles WHERE rolname = 'app';`)
		if got != "t|f|10" {
			t.Errorf("role app = %q", got)
		}
		got = admin(t, "postgres", `SELECT pg_encoding_to_char(encoding), datcollate, datctype, datconnlimit, pg_get_userbyid(datdba) FROM pg_database WHERE datname = 'appdb';`)
		if got != "UTF8|C|C|50|app" {
			t.Errorf("database appdb = %q", got)
		}
		if got := admin(t, "appdb", `SELECT string_agg(extname || '=' || extversion, ',' ORDER BY extname) FROM pg_extension;`); got != "citext=1.6,hstore=1.7,plpgsql=1.0" {
			t.Errorf("extensions = %q", got)
		}
		if got := admin(t, "postgres", `SELECT count(*) FROM pg_roles WHERE rolname = E'sk it "o''q\\';`); got != "1" {
			t.Errorf("the role with quotes in its name: %q", got)
		}
		if got := admin(t, "postgres", `SELECT pg_get_userbyid(datdba) = E'sk it "o''q\\' FROM pg_database WHERE datname = E'sk-db ''x"; --';`); got != "t" {
			t.Errorf("the database with quotes in its name: %q", got)
		}
		if got := publicPrivileges(t, "appdb"); got != "" {
			t.Errorf("PUBLIC on appdb = %q; want nothing", got)
		}
		if got := publicPrivileges(t, weirdDB); got != "CONNECT,TEMPORARY" {
			t.Errorf("PUBLIC on the undeclared database = %q; want the default CONNECT,TEMPORARY untouched", got)
		}
		if got := admin(t, "postgres", `SELECT rolbypassrls, rolreplication FROM pg_roles WHERE rolname = 'app';`); got != "f|f" {
			t.Errorf("app bypassrls|replication = %q", got)
		}
		// app は宣言のパスワードでつなげる（SCRAM の検証値が本物と合う）。
		if out, err := psqlAs("app", appPW, "appdb", "SELECT current_user;"); err != nil || strings.TrimSpace(out) != "app" {
			t.Errorf("login as app with the declared password: %v: %s", err, out)
		}
		if got := admin(t, "postgres", `SELECT rolpassword LIKE 'SCRAM-SHA-256$%' FROM pg_authid WHERE rolname = 'app';`); got != "t" {
			t.Errorf("the stored password is not a SCRAM verifier: %q", got)
		}
	})

	t.Run("changed by hand is drift and comes back", func(t *testing.T) {
		admin(t, "postgres", `ALTER ROLE app CREATEDB CONNECTION LIMIT 3;
ALTER ROLE app PASSWORD 'changed-by-hand';
REVOKE app FROM reader;
REVOKE CONNECT ON DATABASE appdb FROM reader;
ALTER DATABASE appdb CONNECTION LIMIT 5;`)
		s, code, _ := status(t)
		want := []string{"main/database/appdb/grant/reader=drift", "main/database/appdb=drift", "main/role/app=drift", "main/role/reader=drift"}
		if got := drifted(s); strings.Join(got, " ") != strings.Join(want, " ") || code != 0 {
			t.Errorf("status (exit %d) drifted = %v; want %v", code, got, want)
		}
		out := converge(t, n)
		t.Logf("reconcile:\n%s", out)
		if out, err := psqlAs("app", appPW, "appdb", "SELECT 1;"); err != nil {
			t.Errorf("the password did not come back: %v: %s", err, out)
		}
		if got := admin(t, "postgres", `SELECT pg_has_role('reader', 'app', 'MEMBER');`); got != "t" {
			t.Errorf("reader is not a member of app again: %q", got)
		}
	})

	t.Run("PUBLIC and BYPASSRLS changed by hand come back", func(t *testing.T) {
		admin(t, "postgres", `GRANT CREATE, CONNECT ON DATABASE appdb TO PUBLIC;
ALTER ROLE app BYPASSRLS;
ALTER ROLE reader REPLICATION;`)
		s, code, _ := status(t)
		// reader の replication は宣言していないので drift にならない。
		want := []string{"main/database/appdb/grant/PUBLIC=drift", "main/role/app=drift"}
		if got := drifted(s); strings.Join(got, " ") != strings.Join(want, " ") || code != 0 {
			t.Errorf("status (exit %d) drifted = %v; want %v", code, got, want)
		}
		out := converge(t, n)
		t.Logf("reconcile:\n%s", out)
		if got := publicPrivileges(t, "appdb"); got != "" {
			t.Errorf("PUBLIC on appdb = %q; want nothing", got)
		}
		if got := admin(t, "postgres", `SELECT rolbypassrls FROM pg_roles WHERE rolname = 'app';`); got != "f" {
			t.Errorf("app bypassrls = %q; want f", got)
		}
		if got := admin(t, "postgres", `SELECT rolreplication FROM pg_roles WHERE rolname = 'reader';`); got != "t" {
			t.Errorf("reader replication = %q; want t (not declared, so untouched)", got)
		}
		admin(t, "postgres", `ALTER ROLE reader NOREPLICATION;`)
	})

	t.Run("a new generation of the password and an extension update", func(t *testing.T) {
		store := map[string]any{"secrets": map[string]any{
			"pg/admin": map[string]any{"gen": 1, "value": adminPW},
			"pg/app":   map[string]any{"gen": 2, "value": appPW2},
		}}
		if err := writeJSON(filepath.Join(lab.dir, "secrets.json"), store); err != nil {
			t.Fatal(err)
		}
		rs := declared("1.8")
		rs[0].Fields["password"] = "secretref:file/pg/app#gen2"
		writeIntent(t, rs)
		converge(t, n)
		if _, err := psqlAs("app", appPW2, "appdb", "SELECT 1;"); err != nil {
			t.Errorf("login with the new password: %v", err)
		}
		if _, err := psqlAs("app", appPW, "appdb", "SELECT 1;"); err == nil {
			t.Error("the old password still works")
		}
		if got := admin(t, "appdb", `SELECT extversion FROM pg_extension WHERE extname = 'hstore';`); got != "1.8" {
			t.Errorf("hstore = %q; want 1.8", got)
		}
		// encoding を変える宣言は計画のエラー（status は観測できないとして報告）。
		rs2 := declared("1.8")
		rs2[0].Fields["password"] = "secretref:file/pg/app#gen2"
		rs2[3].Fields["encoding"] = "LATIN1"
		writeIntent(t, rs2)
		s, code, errOut := status(t)
		if code != 3 || s.Summary.Unobservable != 1 || !strings.Contains(errOut, "cannot be changed after the database is created") {
			t.Errorf("status with a changed encoding: exit %d %+v\n%s", code, s.Summary, errOut)
		}
		writeIntent(t, rs)
	})

	t.Run("no drift however many cycles", func(t *testing.T) {
		before := admin(t, "postgres", `SELECT rolpassword FROM pg_authid WHERE rolname = 'app';`)
		for i := 0; i < 5; i++ {
			out, code := sakutei(t, "reconcile")
			if code != 0 || strings.Contains(out, "will create") || strings.Contains(out, "out of sync") {
				t.Fatalf("cycle %d (exit %d) planned a change:\n%s", i, code, out)
			}
			if s, _, _ := status(t); s.Summary.Drift != 0 || s.Summary.Conformant != n || !s.Complete {
				t.Fatalf("cycle %d: %+v", i, s.Summary)
			}
		}
		if after := admin(t, "postgres", `SELECT rolpassword FROM pg_authid WHERE rolname = 'app';`); after != before {
			t.Error("the password was set again over the cycles")
		}
	})

	t.Run("a database with user tables is not pruned", func(t *testing.T) {
		admin(t, "postgres", `CREATE ROLE sk_orphan_role;`)
		for _, db := range []string{"sk_orphan_full", "sk_orphan_empty"} {
			admin(t, "postgres", "CREATE DATABASE "+db+";")
		}
		admin(t, "sk_orphan_full", `CREATE TABLE important (id int); INSERT INTO important VALUES (1);`)
		admin(t, "sk_orphan_empty", `CREATE EXTENSION hstore;`) // 拡張の表（なし）は利用者の表に数えない。
		rs := declared("1.8")
		rs[0].Fields["password"] = "secretref:file/pg/app#gen2"
		writeIntent(t, withPrune(rs))
		out, _ := sakutei(t, "reconcile")
		t.Logf("reconcile with PruneWithinScope:\n%s", out)
		out2, _ := sakutei(t, "reconcile")
		t.Logf("reconcile with PruneWithinScope (2):\n%s", out2)
		exists := func(db string) bool {
			return admin(t, "postgres", "SELECT count(*) FROM pg_database WHERE datname = '"+db+"';") == "1"
		}
		if !exists("sk_orphan_full") {
			t.Error("the database with a user table was dropped")
		}
		if exists("sk_orphan_empty") {
			t.Error("the empty orphan database is still there")
		}
		if got := admin(t, "postgres", `SELECT count(*) FROM pg_roles WHERE rolname = 'sk_orphan_role';`); got != "0" {
			t.Error("the orphan role is still there")
		}
		for _, db := range []string{"postgres", "template0", "template1"} {
			if !exists(db) {
				t.Errorf("%s was dropped", db)
			}
		}
		if got := admin(t, "postgres", `SELECT count(*) FROM pg_roles WHERE rolname IN ('pg_monitor', 'sakutei');`); got != "2" {
			t.Error("a system role or the admin role was dropped")
		}
		if got := admin(t, "sk_orphan_full", `SELECT count(*) FROM important;`); got != "1" {
			t.Error("the user table lost its row")
		}
		if !strings.Contains(out+out2, "user table") {
			t.Errorf("reconcile did not report the database with tables:\n%s", out+out2)
		}
		// 後の段のために、中身のある orphan は手で片付ける（sakutei は消さない）。
		admin(t, "postgres", "DROP DATABASE sk_orphan_full;")
	})

	t.Run("a stopped server is unobservable and nothing is done", func(t *testing.T) {
		if out, err := docker("stop", serverName); err != nil {
			t.Fatalf("docker stop: %v: %s", err, out)
		}
		started := false
		defer func() {
			if !started {
				_, _ = docker("start", serverName)
			}
		}()
		s, code, errOut := status(t)
		if code != 3 || s.Summary.Unobservable != n || s.Summary.Conformant != 0 || s.Summary.Drift != 0 {
			t.Errorf("status while stopped: exit %d %+v\n%s", code, s.Summary, errOut)
		}
		out, code := sakutei(t, "reconcile")
		t.Logf("reconcile while stopped (exit %d):\n%s", code, out)
		if strings.Contains(out, "DELETE") || strings.Contains(out, "CREATE ") || !strings.Contains(out, "prune halted") {
			t.Errorf("reconcile while stopped did something or did not halt prune:\n%s", out)
		}
		if out, err := docker("start", serverName); err != nil {
			t.Fatalf("docker start: %v: %s", err, out)
		}
		started = true
		if err := waitReady(time.Minute); err != nil {
			t.Fatal(err)
		}
		if s, _, _ := status(t); s.Summary.Conformant != n {
			t.Errorf("after start: %+v", s.Summary)
		}
	})

	t.Run("clean up through prune", func(t *testing.T) {
		// 宣言を app だけにして prune：作ったものを消す（app は残す）。
		writeIntent(t, withPrune([]resource{r("role/app", map[string]string{"login": "true"})}))
		for i := 0; i < 3; i++ {
			out, _ := sakutei(t, "reconcile")
			t.Logf("reconcile #%d:\n%s", i+1, out)
		}
		if got := admin(t, "postgres", `SELECT string_agg(datname, ',' ORDER BY datname) FROM pg_database;`); got != "postgres,template0,template1" {
			t.Errorf("databases left = %q", got)
		}
		if got := admin(t, "postgres", `SELECT string_agg(rolname, ',' ORDER BY rolname) FROM pg_roles WHERE rolname NOT LIKE 'pg\_%';`); got != "app,sakutei" {
			t.Errorf("roles left = %q", got)
		}
	})
}
