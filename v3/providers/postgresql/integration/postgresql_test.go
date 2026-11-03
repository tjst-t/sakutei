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

// サーバ（sakutei-v3pg-server）とクライアント（sakutei-v3pg-client、psql を動かす
// だけ）を同じ docker のネットワーク（sakutei-v3pg-net）に置く。サーバはホストに
// 公開しない。イメージは環境変数 SAKUTEI_IT_POSTGRES_IMAGE で差し替えられる。
// 「手で変える」は管理用のロールで直に流す psql（sakutei の外）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/providers/postgresql"
)

// 既定のイメージ。v2 の統合テストと同じ digest（2026-10-27 に docker pull で確かめた値）。
const postgresImage = "postgres:17.11-bookworm@sha256:858bb5e98eed02eb8cafbcdc007f9a65d1b613972028944a8ec2133ccaf80d27"

const (
	prefix     = "sakutei-v3pg-"
	network    = prefix + "net"
	serverName = prefix + "server"
	clientName = prefix + "client"
	adminUser  = "sakutei"
	adminPW    = "admin-pw:with\\colon"
	appPW      = "app-pw-gen1"
	appPW2     = "app-pw-gen2"
	weirdRole  = `sk it "o'q\`
	weirdDB    = `sk-db 'x"; --`
	roUser     = "sakutei_ro"
	roPW       = "ro-pw:1"
)

var lab struct {
	dir, wrapper, image, secrets string
}

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	if err := setup(); err != nil {
		fmt.Fprintf(os.Stderr, "integration-v3-postgresql: %v\n", err)
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

// wrapperScript は psql をクライアントのコンテナの中で動かす。接続の設定は環境変数
// から受け取り、passfile は docker cp でコンテナに渡して終われば消す。
const wrapperScript = `#!/bin/bash
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
		return fmt.Errorf("docker through sudo without a password is required: %v: %s", err, out)
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
	lab.secrets = filepath.Join(dir, "secrets")
	if err := os.MkdirAll(lab.secrets, 0o700); err != nil {
		return err
	}
	if _, err := docker("image", "inspect", lab.image); err != nil {
		if out, err := docker("pull", lab.image); err != nil {
			return fmt.Errorf("docker pull %s: %v: %s", lab.image, err, out)
		}
	}
	teardownContainers() // 前の回の残り
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
	for name, v := range map[string]string{"admin": adminPW, "ro": roPW} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v+"\n"), 0o600); err != nil {
			return err
		}
	}
	if err := writeSecret(appPW); err != nil {
		return err
	}
	if err := waitReady(2 * time.Minute); err != nil {
		out, _ := docker("logs", "--tail", "20", serverName)
		return fmt.Errorf("server: %v\n%s", err, out)
	}
	return nil
}

// writeSecret は秘密 f:app の値を書く（file の保管先の世代は変更時刻と大きさ）。
func writeSecret(v string) error {
	return os.WriteFile(filepath.Join(lab.secrets, "app"), []byte(v+"\n"), 0o600)
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

// ─────────────────────────────────────────────────────────────────────────────
// intent・つなぎ方の設定・CLI
// ─────────────────────────────────────────────────────────────────────────────

type resource struct {
	Type      string              `json:"type"`
	ID        string              `json:"id"`
	Ensure    string              `json:"ensure,omitempty"`
	Fields    map[string]any      `json:"fields,omitempty"`
	DependsOn []map[string]string `json:"depends_on,omitempty"`
}

func r(kind, name string, fields map[string]any, deps ...string) resource {
	x := resource{Type: "pg/" + kind, ID: "main/" + name, Fields: fields}
	for _, d := range deps {
		x.DependsOn = append(x.DependsOn, map[string]string{"id": "pg/" + d})
	}
	return x
}

func absent(kind, name string) resource {
	return resource{Type: "pg/" + kind, ID: "main/" + name, Ensure: "absent"}
}

func key(x resource) string { return x.Type + "/" + x.ID }

// declared は宣言（hstore の版は引数）。
func declared(hstore string) []resource {
	return []resource{
		r("role", "app", map[string]any{"login": true, "createdb": false, "connection_limit": 10,
			"password": map[string]any{"secret": "f:app"}, "member_of": []any{"pg_read_all_data"},
			"bypassrls": false, "replication": false}),
		r("role", "reader", map[string]any{"login": false, "member_of": []any{"app"}}, "role/main/app"),
		r("role", weirdRole, map[string]any{"login": false, "inherit": true}),
		r("database", "appdb", map[string]any{"owner": "app", "encoding": "UTF8", "lc_collate": "C", "lc_ctype": "C",
			"template": "template0", "connection_limit": 50}, "role/main/app"),
		r("database", weirdDB, map[string]any{"owner": weirdRole}, "role/main/"+weirdRole),
		r("grant", "appdb/reader", map[string]any{"privileges": []any{"CONNECT", "TEMP"}}, "database/main/appdb", "role/main/reader"),
		r("extension", "appdb/hstore", map[string]any{"version": hstore}, "database/main/appdb"),
		r("extension", "appdb/citext", nil, "database/main/appdb"),
		// PUBLIC から全部外す（新しい DB の既定の CONNECT・TEMPORARY も）。
		r("grant", "appdb/PUBLIC", map[string]any{"privileges": []any{}}, "database/main/appdb"),
	}
}

// intent は intent のディレクトリを書く（JSON。範囲は main のサーバ）。
func intent(t *testing.T, name string, prune bool, rs []resource) string {
	t.Helper()
	d := filepath.Join(lab.dir, name)
	os.RemoveAll(d)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(map[string]any{
		"ranges":    []any{map[string]any{"provider": "pg", "args": map[string]string{"server": "main"}, "prune": prune}},
		"resources": rs,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "pg.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return d
}

// config はつなぎ方の設定を書いてそのパスを返す。write が空なら observe だけ。
func config(t *testing.T, name, observe, write string, verify bool) string {
	t.Helper()
	cred := func(user string) string {
		pf := map[string]string{adminUser: "admin", roUser: "ro"}[user]
		return fmt.Sprintf("{user: %s, password_file: %s}", user, filepath.Join(lab.dir, pf))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "secret_backends:\n  f:\n    module: file\n    settings: {dir: %s}\n", lab.secrets)
	fmt.Fprintf(&b, "providers:\n  pg:\n    module: postgresql\n    settings:\n      psql: %s\n      servers:\n        main:\n", lab.wrapper)
	fmt.Fprintf(&b, "          host: %s\n          port: 5432\n          connect_timeout: 3s\n          verify_passwords: %v\n", serverName, verify)
	fmt.Fprintf(&b, "          observe: %s\n", cred(observe))
	if write != "" {
		fmt.Fprintf(&b, "          write: %s\n", cred(write))
	}
	p := filepath.Join(lab.dir, name)
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// run は v3 の CLI を回す。出力に秘密の値・検証値が出れば落とす（E-G2）。
func run(t *testing.T, verb, intentDir, cfg string) (plan.Result, int, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), []string{verb, "--intent", intentDir, "--config", cfg,
		"--record", filepath.Join(lab.dir, "record-"+filepath.Base(cfg)), "--json"},
		&stdout, &stderr, cli.WithModule("postgresql", postgresql.Module))
	all := stdout.String() + stderr.String()
	for _, s := range []string{adminPW, appPW, appPW2, roPW, "SCRAM-SHA-256$"} {
		if strings.Contains(all, s) {
			t.Fatalf("sakutei %s が秘密（%q）を出した:\n%s", verb, s, all)
		}
	}
	var res plan.Result
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("%s の出力を読めない: %v\n%s\n%s", verb, err, stdout.String(), stderr.String())
	}
	return res, code, stderr.String()
}

func dump(r plan.Result) string {
	b, _ := json.MarshalIndent(map[string]any{"changes": r.Changes, "ranges": r.Ranges, "halts": r.Halts, "errors": r.Errors}, "", "  ")
	return string(b)
}

func changeOf(r plan.Result, resource string) plan.Change {
	for _, c := range r.Changes {
		if c.Resource == resource {
			return c
		}
	}
	return plan.Change{}
}

// pending は宣言どおりでも宣言外の報告でもない変更（"<resource>=<op>"、名前の順）。
func pending(r plan.Result) []string {
	var out []string
	for _, c := range r.Changes {
		if !c.Op.Settled() {
			out = append(out, c.Resource+"="+string(c.Op))
		}
	}
	sort.Strings(out)
	return out
}

// converge は reconcile を回し、続く plan が宣言どおり（宣言外の報告だけ）なことを確かめる。
func converge(t *testing.T, intentDir, cfg string) plan.Result {
	t.Helper()
	r, code, stderr := run(t, "reconcile", intentDir, cfg)
	if code != cli.ExitOK {
		t.Fatalf("reconcile: exit=%d\n%s\n%s", code, dump(r), stderr)
	}
	p, code, stderr := run(t, "plan", intentDir, cfg)
	if code != cli.ExitOK || len(pending(p)) != 0 {
		t.Fatalf("reconcile のあとの plan: exit=%d %v\n%s\n%s", code, pending(p), dump(p), stderr)
	}
	return r
}

// publicPrivileges は PUBLIC が db に持つ権限（"," 区切り、名前の順。datacl が NULL
// なら既定の分）。
func publicPrivileges(t *testing.T, db string) string {
	t.Helper()
	lit := "E'" + strings.NewReplacer(`'`, `''`, `\`, `\\`).Replace(db) + "'"
	return admin(t, "postgres", `SELECT coalesce(string_agg(DISTINCT a.privilege_type, ',' ORDER BY a.privilege_type), '')
  FROM pg_database d, aclexplode(coalesce(d.datacl, acldefault('d', d.datdba))) a
 WHERE d.datname = `+lit+` AND a.grantee = 0;`)
}

func databases(t *testing.T) string {
	t.Helper()
	return admin(t, "postgres", `SELECT string_agg(datname, ',' ORDER BY datname) FROM pg_database;`)
}

// ─────────────────────────────────────────────────────────────────────────────
// 1 本の資格で観測も書き込みも（管理用のロール、verify_passwords）
// ─────────────────────────────────────────────────────────────────────────────

func TestPostgreSQL(t *testing.T) {
	cfg := config(t, "main.yaml", adminUser, "", true)
	decl := declared("1.7")
	in := intent(t, "intent-main", false, decl)

	t.Run("空から作る", func(t *testing.T) {
		p, code, stderr := run(t, "plan", in, cfg)
		if code != cli.ExitOK {
			t.Fatalf("plan: exit=%d\n%s\n%s", code, dump(p), stderr)
		}
		for _, x := range decl {
			if c := changeOf(p, key(x)); c.Op != plan.Create {
				t.Errorf("%s: %s（want create）", key(x), c.Op)
			}
		}
		r := converge(t, in, cfg)
		t.Logf("reconcile:\n%s", dump(r))
		if got := admin(t, "postgres", `SELECT rolcanlogin, rolcreatedb, rolconnlimit, rolbypassrls, rolreplication FROM pg_roles WHERE rolname = 'app';`); got != "t|f|10|f|f" {
			t.Errorf("role app = %q", got)
		}
		if got := admin(t, "postgres", `SELECT pg_encoding_to_char(encoding), datcollate, datctype, datconnlimit, pg_get_userbyid(datdba) FROM pg_database WHERE datname = 'appdb';`); got != "UTF8|C|C|50|app" {
			t.Errorf("database appdb = %q", got)
		}
		if got := admin(t, "appdb", `SELECT string_agg(extname || '=' || extversion, ',' ORDER BY extname) FROM pg_extension;`); got != "citext=1.6,hstore=1.7,plpgsql=1.0" {
			t.Errorf("extensions = %q", got)
		}
		if got := admin(t, "postgres", `SELECT pg_get_userbyid(datdba) = E'sk it "o''q\\' FROM pg_database WHERE datname = E'sk-db ''x"; --';`); got != "t" {
			t.Errorf("引用符を含む名前の DB と所有者: %q", got)
		}
		if got := publicPrivileges(t, "appdb"); got != "" {
			t.Errorf("appdb の PUBLIC = %q（want 無し）", got)
		}
		if got := publicPrivileges(t, weirdDB); got != "CONNECT,TEMPORARY" {
			t.Errorf("宣言していない DB の PUBLIC = %q（want 既定のまま）", got)
		}
		// app は宣言のパスワードでつなげる（SCRAM の検証値が本物と合う）。平文は送らない。
		if out, err := psqlAs("app", appPW, "appdb", "SELECT current_user;"); err != nil || strings.TrimSpace(out) != "app" {
			t.Errorf("宣言のパスワードで app につなげない: %v: %s", err, out)
		}
		if got := admin(t, "postgres", `SELECT rolpassword LIKE 'SCRAM-SHA-256$%' FROM pg_authid WHERE rolname = 'app';`); got != "t" {
			t.Errorf("pg_authid が SCRAM の検証値でない: %q", got)
		}
	})

	t.Run("手で変えるとずれ、戻る", func(t *testing.T) {
		admin(t, "postgres", `ALTER ROLE app CREATEDB CONNECTION LIMIT 3;
ALTER ROLE app PASSWORD 'changed-by-hand';
REVOKE app FROM reader;
REVOKE CONNECT ON DATABASE appdb FROM reader;
ALTER DATABASE appdb CONNECTION LIMIT 5;`)
		p, _, _ := run(t, "plan", in, cfg)
		want := []string{"pg/database/main/appdb=update", "pg/grant/main/appdb/reader=update", "pg/role/main/app=update", "pg/role/main/reader=update"}
		if got := pending(p); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("plan = %v（want %v）", got, want)
		}
		if c := changeOf(p, "pg/role/main/app"); !strings.Contains(fmt.Sprint(c.Fields), "password") {
			t.Errorf("手で変えたパスワードがずれにならない（verify_passwords）: %+v", c.Fields)
		}
		converge(t, in, cfg)
		if out, err := psqlAs("app", appPW, "appdb", "SELECT 1;"); err != nil {
			t.Errorf("パスワードが戻らない: %v: %s", err, out)
		}
		if got := admin(t, "postgres", `SELECT pg_has_role('reader', 'app', 'MEMBER');`); got != "t" {
			t.Errorf("reader が app に戻らない: %q", got)
		}
	})

	t.Run("PUBLIC と BYPASSRLS を手で変えると戻り、宣言しない属性は触らない", func(t *testing.T) {
		admin(t, "postgres", `GRANT CREATE, CONNECT ON DATABASE appdb TO PUBLIC;
ALTER ROLE app BYPASSRLS;
ALTER ROLE reader REPLICATION;`)
		p, _, _ := run(t, "plan", in, cfg)
		want := []string{"pg/grant/main/appdb/PUBLIC=update", "pg/role/main/app=update"}
		if got := pending(p); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("plan = %v（want %v）", got, want)
		}
		converge(t, in, cfg)
		if got := publicPrivileges(t, "appdb"); got != "" {
			t.Errorf("appdb の PUBLIC = %q", got)
		}
		if got := admin(t, "postgres", `SELECT a.rolbypassrls, b.rolreplication FROM pg_roles a, pg_roles b WHERE a.rolname = 'app' AND b.rolname = 'reader';`); got != "f|t" {
			t.Errorf("app の bypassrls | reader の replication = %q（want f|t）", got)
		}
		admin(t, "postgres", `ALTER ROLE reader NOREPLICATION;`)
	})

	t.Run("秘密の新しい世代と拡張の更新", func(t *testing.T) {
		if err := writeSecret(appPW2); err != nil {
			t.Fatal(err)
		}
		in = intent(t, "intent-main", false, declared("1.8"))
		p, _, _ := run(t, "plan", in, cfg)
		want := []string{"pg/extension/main/appdb/hstore=update", "pg/role/main/app=update"}
		if got := pending(p); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("plan = %v（want %v）", got, want)
		}
		converge(t, in, cfg)
		if _, err := psqlAs("app", appPW2, "appdb", "SELECT 1;"); err != nil {
			t.Errorf("新しいパスワードでつなげない: %v", err)
		}
		if _, err := psqlAs("app", appPW, "appdb", "SELECT 1;"); err == nil {
			t.Error("古いパスワードでまだつなげる")
		}
		if got := admin(t, "appdb", `SELECT extversion FROM pg_extension WHERE extname = 'hstore';`); got != "1.8" {
			t.Errorf("hstore = %q", got)
		}
		// 作った後の encoding の変更は Apply が断る（作り直さない）。
		bad := declared("1.8")
		bad[3].Fields["encoding"] = "LATIN1"
		badIn := intent(t, "intent-bad-encoding", false, bad)
		r, code, _ := run(t, "reconcile", badIn, cfg)
		if c := changeOf(r, "pg/database/main/appdb"); code != cli.ExitError || c.Outcome != plan.OutcomeError || !strings.Contains(c.Error, "変えられない") {
			t.Errorf("encoding の変更: exit=%d %+v", code, c)
		}
		if got := admin(t, "postgres", `SELECT pg_encoding_to_char(encoding) FROM pg_database WHERE datname = 'appdb';`); got != "UTF8" {
			t.Errorf("appdb の encoding = %q", got)
		}
	})

	t.Run("何周してもずれない", func(t *testing.T) {
		before := admin(t, "postgres", `SELECT rolpassword FROM pg_authid WHERE rolname = 'app';`)
		for i := 0; i < 5; i++ {
			r, code, stderr := run(t, "reconcile", in, cfg)
			if code != cli.ExitOK || len(pending(r)) != 0 {
				t.Fatalf("%d 周目: exit=%d %v\n%s\n%s", i, code, pending(r), dump(r), stderr)
			}
		}
		if after := admin(t, "postgres", `SELECT rolpassword FROM pg_authid WHERE rolname = 'app';`); after != before {
			t.Error("周のあいだにパスワードを設定し直した")
		}
	})

	t.Run("利用者の表がある DB は消さない・宣言外の DB は prune しない", func(t *testing.T) {
		admin(t, "postgres", `CREATE ROLE sk_orphan_role;`)
		for _, db := range []string{"sk_orphan_full", "sk_orphan_empty", "sk_orphan_kept"} {
			admin(t, "postgres", "CREATE DATABASE "+db+";")
		}
		admin(t, "sk_orphan_full", `CREATE TABLE important (id int); INSERT INTO important VALUES (1);`)
		admin(t, "sk_orphan_empty", `CREATE EXTENSION hstore;`) // 拡張の表は利用者の表に数えない
		rs := append(declared("1.8"), absent("database", "sk_orphan_full"), absent("database", "sk_orphan_empty"))
		pin := intent(t, "intent-main", true, rs)
		var out []string
		for i := 0; i < 2; i++ {
			r, code, _ := run(t, "reconcile", pin, cfg)
			out = append(out, fmt.Sprintf("exit=%d\n%s", code, dump(r)))
			if i == 0 {
				if c := changeOf(r, "pg/database/main/sk_orphan_full"); c.Outcome != plan.OutcomeRefused || !strings.Contains(c.Error+c.Note, "利用者の表") {
					t.Errorf("表のある DB の削除: %+v", c)
				}
				if c := changeOf(r, "pg/database/main/sk_orphan_kept"); c.Op != plan.Unmanaged {
					t.Errorf("宣言外の DB: %+v（want unmanaged）", c)
				}
			}
		}
		t.Logf("reconcile:\n%s", strings.Join(out, "\n"))
		if got := databases(t); got != "appdb,postgres,sakutei,sk-db 'x\"; --,sk_orphan_full,sk_orphan_kept,template0,template1" {
			t.Errorf("DB = %q", got)
		}
		if got := admin(t, "sk_orphan_full", `SELECT count(*) FROM important;`); got != "1" {
			t.Error("利用者の表の行が消えた")
		}
		if got := admin(t, "postgres", `SELECT count(*) FROM pg_roles WHERE rolname = 'sk_orphan_role';`); got != "0" {
			t.Error("宣言外のロールが prune されない")
		}
		if got := admin(t, "postgres", `SELECT count(*) FROM pg_roles WHERE rolname IN ('pg_monitor', 'sakutei');`); got != "2" {
			t.Error("PostgreSQL のロールか管理用のロールが消えた")
		}
		// 後の段のために手で片付ける（sakutei はデータを消さない）。
		admin(t, "postgres", "DROP DATABASE sk_orphan_full; DROP DATABASE sk_orphan_kept;")
	})

	t.Run("止めると Unknown で何もしない", func(t *testing.T) {
		if out, err := docker("stop", serverName); err != nil {
			t.Fatalf("docker stop: %v: %s", err, out)
		}
		started := false
		defer func() {
			if !started {
				_, _ = docker("start", serverName)
			}
		}()
		pin := intent(t, "intent-main", true, declared("1.8"))
		p, code, _ := run(t, "plan", pin, cfg)
		if code != cli.ExitOK || p.Summary[plan.Unknown] != len(declared("1.8")) || len(p.Ranges) != 1 || p.Ranges[0].Complete {
			t.Errorf("止めたあとの plan: exit=%d %v\n%s", code, p.Summary, dump(p))
		}
		r, code, _ := run(t, "reconcile", pin, cfg)
		for _, c := range r.Changes {
			if c.Outcome != "" {
				t.Errorf("止めたあとの reconcile が当てた: %+v", c)
			}
		}
		if code != cli.ExitRemaining {
			t.Errorf("止めたあとの reconcile: exit=%d\n%s", code, dump(r))
		}
		if out, err := docker("start", serverName); err != nil {
			t.Fatalf("docker start: %v: %s", err, out)
		}
		started = true
		if err := waitReady(time.Minute); err != nil {
			t.Fatal(err)
		}
		if p, code, _ := run(t, "plan", pin, cfg); code != cli.ExitOK || len(pending(p)) != 0 {
			t.Errorf("始めたあとの plan: exit=%d %v", code, pending(p))
		}
	})

	t.Run("片付け", func(t *testing.T) {
		pin := intent(t, "intent-main", true, []resource{
			r("role", "app", map[string]any{"login": true}),
			absent("database", "appdb"), absent("database", weirdDB),
		})
		for i := 0; i < 3; i++ {
			r, code, _ := run(t, "reconcile", pin, cfg)
			t.Logf("reconcile #%d: exit=%d %v", i+1, code, pending(r))
		}
		if got := databases(t); got != "postgres,sakutei,template0,template1" {
			t.Errorf("残った DB = %q", got)
		}
		if got := admin(t, "postgres", `SELECT string_agg(rolname, ',' ORDER BY rolname) FROM pg_roles WHERE rolname NOT LIKE 'pg\_%';`); got != "app,sakutei" {
			t.Errorf("残ったロール = %q", got)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// 観測用と書き込み用のロール（E-G5）・verify_passwords と pg_authid（E-E6）・
// connect_roles（E-F3・N-F7）
// ─────────────────────────────────────────────────────────────────────────────

// statements はサーバのログ（log_statement = all）から、user でつないで流れた文の
// 行を返す。
func statements(t *testing.T, user string) []string {
	t.Helper()
	out, err := docker("logs", serverName)
	if err != nil {
		t.Fatalf("docker logs: %v", err)
	}
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "user="+user+" ") && strings.Contains(l, "statement:") {
			lines = append(lines, l)
		}
	}
	return lines
}

func TestPostgreSQLCredentialSplit(t *testing.T) {
	// 読むだけのロール：LOGIN だけ（観測が読むカタログ pg_roles・pg_auth_members・
	// pg_database・pg_extension と shobj_description・aclexplode は PUBLIC が読める）。
	admin(t, "postgres", `CREATE ROLE `+roUser+` LOGIN PASSWORD 'ro-pw:1';`)
	admin(t, "postgres", `ALTER SYSTEM SET log_statement = 'all';`)
	admin(t, "postgres", `ALTER SYSTEM SET log_line_prefix = 'user=%u ';`)
	admin(t, "postgres", `SELECT pg_reload_conf();`)
	defer func() {
		admin(t, "postgres", `ALTER SYSTEM RESET log_statement;`)
		admin(t, "postgres", `ALTER SYSTEM RESET log_line_prefix;`)
		admin(t, "postgres", `SELECT pg_reload_conf();`)
	}()
	cfg := config(t, "split.yaml", roUser, adminUser, false)
	rs := []resource{
		r("role", "split_app", map[string]any{"login": true, "password": map[string]any{"secret": "f:app"}}),
		r("database", "splitdb", map[string]any{"owner": "split_app"}, "role/main/split_app"),
		r("grant", "splitdb/split_app", map[string]any{"privileges": []any{"CONNECT"}}, "database/main/splitdb"),
		r("extension", "splitdb/citext", nil, "database/main/splitdb"),
	}
	in := intent(t, "intent-split", false, rs)

	t.Run("読むだけのロールは何も変えられない", func(t *testing.T) {
		for _, sql := range []string{"CREATE ROLE sk_by_ro;", "CREATE DATABASE sk_by_ro;"} {
			if out, err := psqlAs(roUser, roPW, "postgres", sql); err == nil || !strings.Contains(out, "permission denied") {
				t.Fatalf("%s を %s で: %v: %s（want permission denied）", sql, roUser, err, out)
			}
		}
	})

	t.Run("書き込み用のパスワードが無くても plan は動き、reconcile は書けない", func(t *testing.T) {
		pf := filepath.Join(lab.dir, "admin")
		if err := os.Rename(pf, pf+".away"); err != nil {
			t.Fatal(err)
		}
		defer os.Rename(pf+".away", pf)
		p, code, stderr := run(t, "plan", in, cfg)
		if code != cli.ExitOK || p.Summary[plan.Create] != len(rs) || p.Summary[plan.Unknown] != 0 {
			t.Fatalf("書き込み用のパスワードが無い plan: exit=%d %v\n%s\n%s", code, p.Summary, dump(p), stderr)
		}
		r, code, _ := run(t, "reconcile", in, cfg)
		if c := changeOf(r, "pg/role/main/split_app"); code != cli.ExitError || !strings.Contains(c.Error, "パスワードのファイル") {
			t.Fatalf("書き込み用のパスワードが無い reconcile: exit=%d %+v", code, c)
		}
	})

	t.Run("観測は読むだけのロール、書き込みは管理用のロール", func(t *testing.T) {
		r := converge(t, in, cfg)
		t.Logf("reconcile:\n%s", dump(r))
		if got := admin(t, "postgres", `SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = 'splitdb';`); got != "split_app" {
			t.Errorf("splitdb の所有者 = %q", got)
		}
		ro := statements(t, roUser)
		if len(ro) == 0 {
			t.Fatalf("%s で流れた文が無い（観測）", roUser)
		}
		for _, l := range ro {
			if !strings.Contains(l, "/* sakutei:") && !strings.Contains(l, "sk_by_ro") { // sk_by_ro は上の段の手の文
				t.Errorf("%s が観測でない文を流した: %s", roUser, l)
			}
		}
		created := false
		for _, l := range statements(t, adminUser) {
			created = created || strings.Contains(l, "CREATE DATABASE")
		}
		if !created {
			t.Error("CREATE DATABASE が書き込み用のロールで流れていない")
		}
	})

	t.Run("つなぐ 2 つのロールは List に出ず prune しない", func(t *testing.T) {
		pin := intent(t, "intent-split", true, rs)
		for i := 0; i < 2; i++ {
			run(t, "reconcile", pin, cfg)
		}
		p, code, _ := run(t, "plan", pin, cfg)
		for _, c := range p.Changes {
			if strings.HasSuffix(c.Resource, "/"+roUser) || c.Resource == "pg/role/main/"+adminUser {
				t.Errorf("つなぐロールが出た: %s=%s", c.Resource, c.Op)
			}
		}
		if got := admin(t, "postgres", `SELECT count(*) FROM pg_roles WHERE rolname IN ('`+roUser+`', '`+adminUser+`');`); got != "2" {
			t.Errorf("つなぐロールが消えた: %s", got)
		}
		if code != cli.ExitOK || len(pending(p)) != 0 {
			t.Errorf("prune のあと: exit=%d %v", code, pending(p))
		}
	})

	t.Run("verify_passwords は pg_authid が要り、読めなければ Unknown", func(t *testing.T) {
		vcfg := config(t, "split-verify.yaml", roUser, adminUser, true)
		role := "pg/role/main/split_app"
		check := func(step string, wantUnknown bool) {
			t.Helper()
			p, _, _ := run(t, "plan", in, vcfg)
			c := changeOf(p, role)
			if wantUnknown && (c.Op != plan.Unknown || !strings.Contains(c.Note, "pg_authid")) {
				t.Errorf("%s: %+v（want unknown）", step, c)
			}
			if !wantUnknown && (c.Op != plan.None || len(pending(p)) != 0) {
				t.Errorf("%s: %+v %v（want none）", step, c, pending(p))
			}
		}
		check("読むだけのロール", true)
		// 列の権限は DB ごと（pg_attribute）なので、管理用の DB（postgres）で与えないと効かない。
		admin(t, "splitdb", `GRANT SELECT (rolname, rolpassword) ON pg_authid TO `+roUser+`;`)
		check("別の DB で与えた pg_authid の列", true)
		admin(t, "postgres", `GRANT SELECT (rolname, rolpassword) ON pg_authid TO `+roUser+`;`)
		check("管理用の DB で与えた pg_authid の列", false)
		admin(t, "postgres", `REVOKE SELECT (rolname, rolpassword) ON pg_authid FROM `+roUser+`;`)
		admin(t, "splitdb", `REVOKE SELECT (rolname, rolpassword) ON pg_authid FROM `+roUser+`;`)
		check("REVOKE のあと", true)
		admin(t, "postgres", `GRANT pg_read_all_data TO `+roUser+`;`)
		check("pg_read_all_data", false)
		admin(t, "postgres", `REVOKE pg_read_all_data FROM `+roUser+`;`)
	})

	t.Run("connect_roles で観測用のロールが PUBLIC の繋げない DB に繋がる", func(t *testing.T) {
		// 観測用のロールの CONNECT は connect_roles で宣言し、手では GRANT しない。
		locked := append(append([]resource(nil), rs...),
			r("grant", "lockeddb/PUBLIC", map[string]any{"privileges": []any{}}, "database/main/lockeddb"),
			r("database", "lockeddb", map[string]any{"owner": "split_app", "connect_roles": []any{roUser}}, "role/main/split_app"),
			r("extension", "lockeddb/hstore", nil, "database/main/lockeddb"),
		)
		lin := intent(t, "intent-split", true, locked)
		res := converge(t, lin, cfg)
		t.Logf("reconcile:\n%s", dump(res))
		p, _, _ := run(t, "plan", lin, cfg)
		if len(p.Ranges) != 1 || !p.Ranges[0].Complete {
			t.Errorf("範囲の List が不完全（観測用のロールが繋げない？）: %+v", p.Ranges)
		}
		if got := publicPrivileges(t, "lockeddb"); got != "" {
			t.Errorf("lockeddb の PUBLIC = %q", got)
		}
		if got := admin(t, "postgres", `SELECT has_database_privilege('`+roUser+`', 'lockeddb', 'CONNECT');`); got != "t" {
			t.Errorf("%s の lockeddb の CONNECT = %q", roUser, got)
		}
		read := false
		for _, l := range statements(t, roUser) {
			read = read || strings.Contains(l, "sakutei:extensions")
		}
		if !read {
			t.Error("観測用のロールが拡張を読んでいない")
		}
		// 手の REVOKE はずれとして見え（拡張は観測できず、List は不完全）、1 周で戻る。
		admin(t, "postgres", `REVOKE CONNECT ON DATABASE lockeddb FROM `+roUser+`;`)
		p, _, _ = run(t, "plan", lin, cfg)
		if c := changeOf(p, "pg/database/main/lockeddb"); c.Op != plan.Update {
			t.Errorf("手の REVOKE のあと lockeddb: %+v", c)
		}
		if c := changeOf(p, "pg/extension/main/lockeddb/hstore"); c.Op != plan.Unknown {
			t.Errorf("手の REVOKE のあと拡張: %+v", c)
		}
		if p.Ranges[0].Complete {
			t.Errorf("手の REVOKE のあと List が完全: %+v", p.Ranges)
		}
		// この周は周の初めの List が不完全なので prune を止め、残りとして 3 で終わる。
		r, code, _ := run(t, "reconcile", lin, cfg)
		if c := changeOf(r, "pg/database/main/lockeddb"); c.Outcome != plan.OutcomeOK || c.After != plan.None || code != cli.ExitRemaining {
			t.Errorf("手の REVOKE のあとの reconcile: exit=%d %+v\n%s", code, c, dump(r))
		}
		converge(t, lin, cfg)
		if p, _, _ := run(t, "plan", lin, cfg); !p.Ranges[0].Complete {
			t.Errorf("CONNECT が戻ったのに List が不完全: %+v", p.Ranges)
		}
		// 宣言から外しても REVOKE しない（足すだけ）。
		locked[len(locked)-2].Fields = map[string]any{"owner": "split_app"}
		converge(t, intent(t, "intent-split", true, locked), cfg)
		if got := admin(t, "postgres", `SELECT has_database_privilege('`+roUser+`', 'lockeddb', 'CONNECT');`); got != "t" {
			t.Errorf("宣言から外した connect_roles を REVOKE した: %q", got)
		}
	})

	t.Run("片付け", func(t *testing.T) {
		pin := intent(t, "intent-split", true, []resource{
			r("role", "app", map[string]any{"login": true}),
			absent("database", "splitdb"), absent("database", "lockeddb"),
		})
		for i := 0; i < 3; i++ {
			r, code, _ := run(t, "reconcile", pin, cfg)
			t.Logf("reconcile #%d: exit=%d %v", i+1, code, pending(r))
		}
		if got := databases(t); got != "postgres,sakutei,template0,template1" {
			t.Errorf("残った DB = %q", got)
		}
		admin(t, "postgres", `DROP ROLE `+roUser+`;`)
	})
}
