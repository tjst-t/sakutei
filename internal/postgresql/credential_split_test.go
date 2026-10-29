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

package postgresql_test

// credential_split_test.go — 観測用と書き込み用のロール（§20.7、#186）。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/cause"
	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/postgresql"
	"github.com/tjst-t/sakutei/v2/internal/postgresql/pgtest"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

const (
	roUser = "sakutei_ro"
	roPW   = "ro-secret-value"
)

// splitConfig は読むだけのロールを observe:、管理用のロールを write: に書いた
// サーバの一覧。
const splitConfig = `secret_store: secrets.json
servers:
  main:
    host: db.test
    observe:
      user: sakutei_ro
      password: secretref:file/pg/ro
    write:
      user: sakutei
      password: secretref:file/pg/admin
`

// splitRig は読むだけのロール sakutei_ro を持つ偽のサーバと、設定（splitConfig と
// extra）・secret store（secrets）を書いたファイルを用意し、opts で読んだ
// Provider を返す。
func splitRig(t *testing.T, secrets, extra string, opts postgresql.LoadOptions) (*pgtest.Server, *postgresql.Provider, error) {
	t.Helper()
	st := pgtest.NewState("sakutei", adminPW)
	st.ReadOnly = map[string]string{roUser: roPW}
	st.Roles[roUser] = &pgtest.Role{Login: true, Inherit: true, ConnLimit: -1}
	srv := pgtest.New(st)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "secrets.json"), []byte(secrets), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "pg.yaml")
	if err := os.WriteFile(path, []byte(splitConfig+extra), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := postgresql.LoadConfigWith(path, opts)
	if err != nil {
		return srv, nil, err
	}
	o := cfg.Options()
	o.Runner = postgresql.RunnerFunc(srv.Run)
	return srv, postgresql.New(cfg.Servers, o), nil
}

const allSecrets = `{"secrets": {"pg/ro": {"gen": 1, "value": "` + roPW + `"}, "pg/admin": {"gen": 1, "value": "admin:pw\\x"}, "pg/app": {"gen": 1, "value": "app-secret-value"}}}`

var splitWant = []struct {
	local  string
	fields map[string]string
}{
	{"main/role/app", map[string]string{postgresql.FieldLogin: "true", postgresql.FieldPassword: "secretref:file/pg/app#gen1"}},
	{"main/database/appdb", map[string]string{postgresql.FieldOwner: "app"}},
	{"main/database/appdb/grant/app", map[string]string{postgresql.FieldPrivileges: "CONNECT"}},
}

// splitPlan は local を観測（書き込みでない ctx）して fields との差を計画する。
func splitPlan(t *testing.T, p *postgresql.Provider, local string, fields map[string]string) core.Plan {
	t.Helper()
	obs, err := p.Read(context.Background(), local)
	if err != nil || obs.Availability != core.AvailabilityOk {
		t.Fatalf("Read %s with the observe role: %v %+v", local, err, obs)
	}
	pl, err := diff.ForProvider(p).Plan(obs, diff.DesiredResource{ID: id(local), Fields: fields},
		cause.CauseContext{ResourceObservedAbsent: obs.State == nil})
	if err != nil {
		t.Fatalf("Plan %s: %v", local, err)
	}
	return pl
}

func TestCredentialSplit_ObserveWithReadOnlyRoleWriteWithAdmin(t *testing.T) {
	// [AC-186-5-1] observe は読むだけのロール、write は管理用のロール: 観測は
	// 読むだけのロールで、変更は管理用のロールで流れて収束し、2 つのロールは
	// List に出ない（prune しない）・宣言できない。
	srv, p, err := splitRig(t, allSecrets, "", postgresql.LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; ; round++ {
		changed := false
		for _, w := range splitWant {
			pl := splitPlan(t, p, w.local, w.fields)
			if len(pl.Ops) == 0 {
				continue
			}
			changed = true
			if err := p.Apply(provider.WithWrite(context.Background()), pl); err != nil {
				t.Fatalf("Apply %s with the write role: %v", w.local, err)
			}
		}
		if !changed {
			break
		}
		if round == 4 {
			t.Fatal("not converged")
		}
	}
	res, err := p.List(context.Background(), core.NewScope(nil))
	if err != nil || res.Status != core.ListComplete {
		t.Fatalf("List with the observe role: %v %+v", err, res)
	}
	for _, i := range res.IDs {
		if strings.Contains(i.Local, roUser) || i.Local == "main/role/sakutei" || strings.HasSuffix(i.Local, "/grant/sakutei") {
			t.Errorf("List shows a role Sakutei connects as: %s", i.Local)
		}
	}
	if len(res.IDs) == 0 {
		t.Error("List shows nothing")
	}
	ro, admin := srv.CountsOf(roUser), srv.CountsOf("sakutei")
	if ro.Reads == 0 || ro.Writes != 0 || ro.Denied != 0 {
		t.Errorf("the read-only role: %+v; want reads only", ro)
	}
	if admin.Writes == 0 {
		t.Errorf("the write role: %+v; want the changes", admin)
	}
	for _, local := range []string{"main/role/" + roUser, "main/role/sakutei", "main/database/appdb/grant/" + roUser} {
		if err := p.ValidateIntent(local, map[string]string{postgresql.FieldPrivileges: "CONNECT"}); err == nil || !strings.Contains(err.Error(), "Sakutei connects as") {
			t.Errorf("declaring %s: %v; want refused", local, err)
		}
	}
}

func TestCredentialSplit_ObserveOnlyRunNeedsNoWritePassword(t *testing.T) {
	// [AC-186-5-2] 書かない回は write の password が store に無くても読み込めて
	// 観測し、書き込みは provider.ErrNoWriteAccess で断って psql を起動しない。
	// 書く回は write の password が無ければ読み込みで断る。
	onlyRO := `{"secrets": {"pg/ro": {"gen": 1, "value": "` + roPW + `"}}}`
	if _, _, err := splitRig(t, onlyRO, "", postgresql.LoadOptions{}); err == nil ||
		!strings.Contains(err.Error(), "servers.main.write.password") {
		t.Fatalf("a writing run without the write password: %v", err)
	}
	srv, p, err := splitRig(t, onlyRO, "", postgresql.LoadOptions{ObserveOnly: true})
	if err != nil {
		t.Fatalf("an observe-only run: %v", err)
	}
	pl := splitPlan(t, p, splitWant[1].local, splitWant[1].fields)
	if len(pl.Ops) == 0 {
		t.Fatal("want a plan to create the database")
	}
	before := len(srv.Calls())
	err = p.Apply(provider.WithWrite(context.Background()), pl)
	if !errors.Is(err, provider.ErrNoWriteAccess) {
		t.Fatalf("Apply in an observe-only run: %v", err)
	}
	// Apply は書き込みの ctx を自分でも付ける。
	if err := p.Apply(context.Background(), pl); !errors.Is(err, provider.ErrNoWriteAccess) {
		t.Fatalf("Apply without WithWrite in an observe-only run: %v", err)
	}
	if n := len(srv.Calls()); n != before {
		t.Errorf("psql ran %d times for a refused write", n-before)
	}
	if c := srv.CountsOf("sakutei"); c != (pgtest.Counts{}) {
		t.Errorf("the write role was used: %+v", c)
	}
	if c := srv.CountsOf(roUser); c.Writes != 0 || c.Denied != 0 || c.Reads == 0 {
		t.Errorf("the read-only role: %+v", c)
	}
	if _, err := p.List(context.Background(), core.NewScope(nil)); err != nil {
		t.Errorf("List in an observe-only run: %v", err)
	}
}

func TestCredentialSplit_VerifyPasswordsWithReadOnlyRoleIsUnknown(t *testing.T) {
	// [AC-186-5-3] verify_passwords の pg_authid を観測のロールが読めなければ、
	// パスワードを宣言したロールは観測できない（Unknown）。「違う」（""）にして
	// 書き直しはしない（§3 第3原則）。
	srv, p, err := splitRig(t, allSecrets, "    verify_passwords: true\n", postgresql.LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	w := splitWant[0]
	if err := p.Apply(provider.WithWrite(context.Background()), splitPlan(t, p, w.local, w.fields)); err != nil {
		t.Fatalf("Apply (the write role reads pg_authid back): %v", err)
	}
	obs, err := p.Read(context.Background(), w.local)
	if err == nil || obs.Availability != core.AvailabilityUnknown || obs.State != nil {
		t.Fatalf("Read = %+v, %v; want Unknown", obs, err)
	}
	if !strings.Contains(err.Error(), "pg_authid") {
		t.Errorf("err = %v; want the reason", err)
	}
	if c := srv.CountsOf(roUser); c.Denied == 0 || c.Writes != 0 {
		t.Errorf("the read-only role: %+v", c)
	}
}

func TestCredentialSplit_ConfigErrors(t *testing.T) {
	// [AC-186-5-4] observe:・write: の書き誤り。
	store := `{"secrets": {"pg/ro": {"gen": 1, "value": "x"}, "pg/admin": {"gen": 1, "value": "y"}}}`
	for name, c := range map[string]struct{ body, want string }{
		"not a map":     {"servers:\n  main:\n    user: a\n    observe: ro\n", "servers.main.observe: must be a map"},
		"unknown key":   {"servers:\n  main:\n    user: a\n    write:\n      host: h\n", `unknown key "servers.main.write.host"`},
		"no write user": {"servers:\n  main:\n    observe:\n      user: ro\n", "servers.main.write.user is required"},
		"raw password":  {"secret_store: secrets.json\nservers:\n  main:\n    user: a\n    write:\n      password: hunter2\n", "servers.main.write.password: must be a SecretRef"},
		"missing write": {"secret_store: secrets.json\nservers:\n  main:\n    user: a\n    write:\n      password: secretref:file/pg/none\n", "servers.main.write.password: secretref:file/pg/none"},
		"no store":      {"servers:\n  main:\n    user: a\n    observe:\n      password: secretref:file/pg/ro\n", "servers.main.observe.password: \"secret_store\" is required"},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "secrets.json"), []byte(store), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "pg.yaml")
		if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := postgresql.LoadConfig(path)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v; want %q", name, err, c.want)
		}
		if err != nil && strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: the error shows a password: %v", name, err)
		}
	}
	// 書かない回は write の password を store と突き合わせない。
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "secrets.json"), []byte(store), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "pg.yaml")
	body := "secret_store: secrets.json\nservers:\n  main:\n    user: a\n    password: secretref:file/pg/ro\n    write:\n      password: secretref:file/pg/none\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := postgresql.LoadConfigWith(path, postgresql.LoadOptions{ObserveOnly: true})
	if err != nil {
		t.Fatalf("observe-only: %v", err)
	}
	if s := cfg.Servers["main"]; s.User != "a" || s.WriteUser != "a" || s.Password.Path() != "pg/ro" || s.WritePassword.Path() != "pg/none" {
		t.Errorf("inherited keys: %+v", s)
	}
}

// splitCycle は 1 周を真似る：全部を観測用のロールで観測して計画し、宣言の順に
// 書き込み用のロールで Apply する（観測できないものは飛ばす。同じ周にできないもの（まだ無いデータベースの
// 権限）は Transient で次の周に回る）。
func splitCycle(t *testing.T, p *postgresql.Provider, want []struct {
	local  string
	fields map[string]string
}) (applied int) {
	t.Helper()
	plans := make([]core.Plan, len(want))
	for i, w := range want {
		obs, err := p.Read(context.Background(), w.local)
		if err != nil || obs.Availability != core.AvailabilityOk {
			continue // 観測できないものはその周は飛ばす（unknown ≠ absent）。
		}
		plans[i] = splitPlan(t, p, w.local, w.fields)
	}
	for i, pl := range plans {
		if len(pl.Ops) == 0 {
			continue
		}
		if err := p.Apply(provider.WithWrite(context.Background()), pl); err != nil {
			var te *provider.TransientError
			if !errors.As(err, &te) {
				t.Fatalf("Apply %s: %v", want[i].local, err)
			}
			continue
		}
		applied++
	}
	return applied
}

func TestCredentialSplit_ConnectRolesKeepsTheObserveRoleConnected(t *testing.T) {
	// [AC-203-1-1] PUBLIC の CONNECT を外したデータベースを空から宣言しても、
	// connect_roles に観測用のロールを書けば、データベースを作る Apply の中で
	// CONNECT が付き、次の周の List（拡張の観測）が通る。宣言の順に依らない。
	// 書かなければ List が落ちる（prune が止まる）。
	for _, c := range []struct {
		name    string
		connect bool
		reverse bool
	}{{"declared", true, false}, {"declared, PUBLIC first", true, true}, {"not declared", false, false}} {
		t.Run(c.name, func(t *testing.T) {
			_, p, err := splitRig(t, allSecrets, "", postgresql.LoadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			db := map[string]string{postgresql.FieldOwner: "app"}
			if c.connect {
				db[postgresql.FieldConnectRoles] = roUser
			}
			want := []struct {
				local  string
				fields map[string]string
			}{
				{"main/role/app", map[string]string{postgresql.FieldLogin: "true"}},
				{"main/database/appdb", db},
				{"main/database/appdb/extension/hstore", map[string]string{}},
				{"main/database/appdb/grant/PUBLIC", map[string]string{postgresql.FieldPrivileges: ""}},
			}
			if c.reverse {
				for i, j := 0, len(want)-1; i < j; i, j = i+1, j-1 {
					want[i], want[j] = want[j], want[i]
				}
			}
			listOK := true
			for cycle := 0; cycle < 4; cycle++ {
				applied := splitCycle(t, p, want)
				if _, err := p.List(context.Background(), core.NewScope(nil)); err != nil {
					listOK = false
					if c.connect {
						t.Fatalf("List after cycle %d: %v", cycle+1, err)
					}
					if !strings.Contains(err.Error(), "CONNECT") {
						t.Errorf("List: %v; want the reason", err)
					}
				}
				if applied == 0 {
					break
				}
			}
			if !c.connect {
				if listOK {
					t.Fatal("without connect_roles, List passed; want it to fail (the observe role cannot connect)")
				}
				return
			}
			for _, w := range want {
				if pl := splitPlan(t, p, w.local, w.fields); len(pl.Ops) != 0 {
					t.Errorf("%s not converged: %+v", w.local, pl.Ops)
				}
			}
		})
	}
}

func TestCredentialSplit_ConnectRolesDriftAndBoundary(t *testing.T) {
	// [AC-203-1-2] 手の REVOKE はずれとして見え、Apply で戻る。宣言していない
	// 境界の外のロールの CONNECT は触らない（ずれにもしない）。境界の内のロール・
	// PUBLIC は connect_roles に書けない。
	srv, p, err := splitRig(t, allSecrets, "", postgresql.LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	const local = "main/database/appdb"
	fields := map[string]string{postgresql.FieldConnectRoles: roUser}
	if err := p.ValidateIntent(local, fields); err != nil {
		t.Fatalf("ValidateIntent: %v", err)
	}
	if err := p.Apply(context.Background(), splitPlan(t, p, local, fields)); err != nil {
		t.Fatal(err)
	}
	srv.Do(func(st *pgtest.State) {
		acl := st.Databases["appdb"].ACL
		acl[roUser] = nil
		delete(acl, roUser)
		acl["pg_monitor"] = []string{"CONNECT"}
	})
	pl := splitPlan(t, p, local, fields)
	if len(pl.Ops) != 1 || len(pl.Ops[0].Fields) != 1 || pl.Ops[0].Fields[0].Field != postgresql.FieldConnectRoles {
		t.Fatalf("after a REVOKE by hand: %+v; want connect_roles out of sync", pl.Ops)
	}
	if err := p.Apply(context.Background(), pl); err != nil {
		t.Fatal(err)
	}
	if pl := splitPlan(t, p, local, fields); len(pl.Ops) != 0 {
		t.Fatalf("not back: %+v", pl.Ops)
	}
	srv.Do(func(st *pgtest.State) {
		acl := st.Databases["appdb"].ACL
		if strings.Join(acl[roUser], ",") != "CONNECT" || strings.Join(acl["pg_monitor"], ",") != "CONNECT" {
			t.Errorf("ACL = %v; want CONNECT for %s and pg_monitor kept", acl, roUser)
		}
	})
	// 宣言から外しても REVOKE しない。
	if pl := splitPlan(t, p, local, map[string]string{}); len(pl.Ops) != 0 {
		t.Errorf("without connect_roles: %+v; want nothing to do", pl.Ops)
	}
	res, err := p.List(context.Background(), core.NewScope(nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range res.IDs {
		if strings.Contains(i.Local, "/grant/") {
			t.Errorf("List shows %s; the CONNECT of a role outside the boundary is not pruned", i.Local)
		}
	}
	for _, bad := range []string{"app", "PUBLIC", "public"} {
		if err := p.ValidateIntent(local, map[string]string{postgresql.FieldConnectRoles: bad}); err == nil || !strings.Contains(err.Error(), "connect_roles") {
			t.Errorf("connect_roles: %s: %v; want refused", bad, err)
		}
	}
	for _, ok := range []string{"sakutei", "pg_monitor", roUser + ",sakutei"} {
		if err := p.ValidateIntent(local, map[string]string{postgresql.FieldConnectRoles: ok}); err != nil {
			t.Errorf("connect_roles: %s: %v", ok, err)
		}
	}
}
