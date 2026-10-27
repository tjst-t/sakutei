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

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/internal/cause"
	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/postgresql"
	"github.com/tjst-t/sakutei/internal/postgresql/pgtest"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
	"github.com/tjst-t/sakutei/pkg/sdk"
)

// TestMain は、環境変数 SAKUTEI_FAKE_PSQL_MAIN があればこのテストのバイナリを
// 偽の psql として動かす（ExecRunner の経路を本物の exec で確かめるため）。
func TestMain(m *testing.M) {
	if os.Getenv("SAKUTEI_FAKE_PSQL_MAIN") == "1" {
		pgtest.Main()
	}
	os.Exit(m.Run())
}

const (
	adminPW = "admin:pw\\x"
	appPW   = "app-secret-value"
	app2PW  = "app-secret-value-2"
)

// secrets は SecretResolver（path → 値）。
type secrets map[string]string

func (s secrets) ResolveValue(_ context.Context, backend, path string, _ int64) (string, error) {
	if backend != "file" {
		return "", fmt.Errorf("backend %s", backend)
	}
	v, ok := s[path]
	if !ok {
		return "", fmt.Errorf("secret %s not found", path)
	}
	return v, nil
}

func newSecrets() secrets {
	return secrets{"pg/admin": adminPW, "pg/app": appPW, "pg/app2": app2PW}
}

type env struct {
	srv *pgtest.Server
	p   *postgresql.Provider
	sec secrets
}

func newEnv(t *testing.T, mod func(*postgresql.Server)) *env {
	t.Helper()
	srv := pgtest.New(pgtest.NewState("sakutei", adminPW))
	s := postgresql.Server{Host: "db.test", Port: 5432, User: "sakutei", HasPassword: true,
		Password: core.NewSecretRef("file", "pg/admin", 1)}
	if mod != nil {
		mod(&s)
	}
	sec := newSecrets()
	p := postgresql.New(map[string]postgresql.Server{"main": s}, postgresql.Options{
		Runner: postgresql.RunnerFunc(srv.Run), Secrets: sec,
	})
	return &env{srv: srv, p: p, sec: sec}
}

func id(local string) core.ID { return core.NewID(postgresql.Name, core.NewScope(nil), local) }

func (e *env) plan(t *testing.T, local string, fields map[string]string) (core.Plan, error) {
	t.Helper()
	obs, err := e.p.Read(context.Background(), local)
	if err != nil {
		t.Fatalf("Read %s: %v", local, err)
	}
	return diff.ForProvider(e.p).Plan(obs, diff.DesiredResource{ID: id(local), Fields: fields},
		cause.CauseContext{ResourceObservedAbsent: obs.State == nil})
}

func (e *env) converge(t *testing.T, local string, fields map[string]string) {
	t.Helper()
	pl, err := e.plan(t, local, fields)
	if err != nil {
		t.Fatalf("Plan %s: %v", local, err)
	}
	if err := e.p.Apply(context.Background(), pl); err != nil {
		t.Fatalf("Apply %s: %v", local, err)
	}
	pl, err = e.plan(t, local, fields)
	if err != nil {
		t.Fatalf("Plan %s (2): %v", local, err)
	}
	if len(pl.Ops) != 0 {
		t.Fatalf("after Apply %s still plans %+v", local, pl.Ops)
	}
}

func (e *env) state() *pgtest.State {
	var st *pgtest.State
	e.srv.Do(func(s *pgtest.State) { st = s })
	return st
}

func TestValidateProvider(t *testing.T) {
	if err := sdk.ValidateProvider(newEnv(t, nil).p); err != nil {
		t.Fatal(err)
	}
}

func TestContract(t *testing.T) {
	cases := []struct {
		name, id string
		desired  map[string]string
		setup    func(*pgtest.State)
	}{
		{"role", "main/role/sk_contract", map[string]string{
			postgresql.FieldLogin: "true", postgresql.FieldCreateDB: "false", postgresql.FieldConnectionLimit: "5",
			postgresql.FieldMemberOf: "pg_monitor", postgresql.FieldPassword: "secretref:file/pg/app#gen1",
		}, nil},
		{"database", "main/database/sk_contract", map[string]string{
			postgresql.FieldOwner: "app", postgresql.FieldEncoding: "UTF8", postgresql.FieldLcCollate: "en_US.utf8",
			postgresql.FieldConnectionLimit: "10",
		}, func(st *pgtest.State) { st.Roles["app"] = &pgtest.Role{Login: true, ConnLimit: -1} }},
		{"grant", "main/database/appdb/grant/app", map[string]string{
			postgresql.FieldPrivileges: "CONNECT,TEMPORARY",
		}, func(st *pgtest.State) {
			st.Roles["app"] = &pgtest.Role{Login: true, ConnLimit: -1}
			st.Databases["appdb"] = &pgtest.Database{Owner: "sakutei", Encoding: "UTF8", AllowConn: true, ConnLimit: -1, Extensions: map[string]string{"plpgsql": "1.0"}}
		}},
		{"extension", "main/database/appdb/extension/hstore", map[string]string{
			postgresql.FieldVersion: "1.7",
		}, func(st *pgtest.State) {
			st.Databases["appdb"] = &pgtest.Database{Owner: "sakutei", Encoding: "UTF8", AllowConn: true, ConnLimit: -1, Extensions: map[string]string{"plpgsql": "1.0"}}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, nil)
			if c.setup != nil {
				e.srv.Do(c.setup)
			}
			ct := sdk.ContractTester{
				Provider: e.p, Lister: e.p, Scope: core.NewScope(nil),
				SampleID: c.id, SampleDesired: c.desired,
				MakeUnreachable: func() (func(), error) {
					e.srv.Do(func(st *pgtest.State) { st.Unreachable = true })
					return func() { e.srv.Do(func(st *pgtest.State) { st.Unreachable = false }) }, nil
				},
			}
			ct.RunAll(t)
		})
	}
}

func TestBulkEquivalence(t *testing.T) {
	e := newEnv(t, func(s *postgresql.Server) { s.VerifyPasswords = true })
	e.converge(t, "main/role/app", map[string]string{postgresql.FieldLogin: "true", postgresql.FieldPassword: "secretref:file/pg/app#gen1"})
	e.converge(t, "main/role/reader", map[string]string{postgresql.FieldMemberOf: "app,pg_read_all_data"})
	e.converge(t, "main/database/appdb", map[string]string{postgresql.FieldOwner: "app"})
	e.converge(t, "main/database/appdb/grant/reader", map[string]string{postgresql.FieldPrivileges: "CONNECT"})
	e.converge(t, "main/database/appdb/extension/citext", map[string]string{})
	bt := sdk.BulkEquivalenceTester{Provider: e.p, Lister: e.p, Scope: core.NewScope(nil)}
	bt.Run(t)
}

func TestList_Boundary(t *testing.T) {
	e := newEnv(t, func(s *postgresql.Server) { s.Scope = core.NewScope(map[string]string{"site": "a"}) })
	e.converge(t, "main/role/app", map[string]string{postgresql.FieldLogin: "true"})
	e.converge(t, "main/role/reader", map[string]string{})
	e.converge(t, "main/database/appdb", map[string]string{postgresql.FieldOwner: "app"})
	e.converge(t, "main/database/appdb/grant/reader", map[string]string{postgresql.FieldPrivileges: "CONNECT"})
	e.converge(t, "main/database/appdb/extension/hstore", map[string]string{})
	e.srv.Do(func(st *pgtest.State) {
		st.Databases["postgres"].ACL = map[string][]string{"sakutei": {"CONNECT"}, "app": {"CONNECT"}}
	})
	res, err := e.p.List(context.Background(), core.NewScope(nil))
	if err != nil || res.Status != core.ListComplete {
		t.Fatalf("List = %+v, %v", res, err)
	}
	var got []string
	for _, i := range res.IDs {
		if i.Scope.Labels["site"] != "a" {
			t.Errorf("List id %s: scope %v; want site=a", i.Local, i.Scope)
		}
		got = append(got, i.Local)
	}
	want := "main/role/app main/role/reader main/database/appdb main/database/appdb/grant/reader main/database/appdb/extension/hstore"
	if strings.Join(got, " ") != want {
		t.Errorf("List = %v\nwant %s (no pg_*, no admin role, no system databases, no owner grant, no plpgsql)", got, want)
	}
}

func TestInjection_NamesAreQuoted(t *testing.T) {
	e := newEnv(t, nil)
	evilRole := `x"; DROP ROLE sakutei; --`
	evilDB := `d'b"; DROP DATABASE postgres; --`
	parent := `Parent Role\`
	e.converge(t, postgresql.RoleID("main", parent), map[string]string{})
	e.converge(t, postgresql.RoleID("main", evilRole), map[string]string{
		postgresql.FieldMemberOf: parent, postgresql.FieldConnectionLimit: "3",
	})
	e.converge(t, postgresql.DatabaseID("main", evilDB), map[string]string{
		postgresql.FieldOwner: evilRole, postgresql.FieldLcCollate: `en_US.utf8`,
	})
	e.converge(t, postgresql.GrantID("main", evilDB, parent), map[string]string{postgresql.FieldPrivileges: "CREATE"})
	e.converge(t, postgresql.ExtensionID("main", evilDB, "citext"), map[string]string{})
	st := e.state()
	if _, ok := st.Roles["sakutei"]; !ok {
		t.Fatal("the admin role was dropped through a role name")
	}
	if _, ok := st.Databases["postgres"]; !ok {
		t.Fatal("postgres was dropped through a database name")
	}
	r, ok := st.Roles[evilRole]
	if !ok || len(r.MemberOf) != 1 || r.MemberOf[0] != parent || r.ConnLimit != 3 {
		t.Errorf("role %q = %+v; want exactly that name, member of %q", evilRole, r, parent)
	}
	d, ok := st.Databases[evilDB]
	if !ok || d.Owner != evilRole || d.Extensions["citext"] == "" {
		t.Errorf("database %q = %+v", evilDB, d)
	}
	if len(st.Roles) != 5 || len(st.Databases) != 4 {
		t.Errorf("roles=%d databases=%d; want 5 and 4 (nothing else created)", len(st.Roles), len(st.Databases))
	}
}

func TestPassword_NotSentNorShown(t *testing.T) {
	e := newEnv(t, nil)
	const rid = "main/role/app"
	e.converge(t, rid, map[string]string{postgresql.FieldLogin: "true", postgresql.FieldPassword: "secretref:file/pg/app#gen1"})
	st := e.state()
	r := st.Roles["app"]
	if !strings.HasPrefix(r.Password, "SCRAM-SHA-256$") || !postgresql.PasswordMatches(r.Password, "app", appPW) {
		t.Errorf("stored password = %q; want a SCRAM verifier of the secret", r.Password)
	}
	if st.PlaintextPasswords != 0 {
		t.Errorf("a plaintext password reached the server %d time(s)", st.PlaintextPasswords)
	}
	if r.Comment == nil || *r.Comment != postgresql.PasswordMarker+"secretref:file/pg/app#gen1" {
		t.Errorf("comment = %v; want the marker with the reference", r.Comment)
	}
	for _, c := range e.srv.Calls() {
		blob := strings.Join(c.Args, " ") + fmt.Sprint(c.Env) + c.Stdin
		for _, secret := range []string{appPW, adminPW, "admin:pw"} {
			if strings.Contains(blob, secret) {
				t.Fatalf("a secret appears in the psql arguments, environment or SQL: %q", secret)
			}
		}
		if pf := c.Env["PGPASSFILE"]; pf == "" {
			t.Error("PGPASSFILE is not set")
		} else if _, err := os.Stat(pf); !os.IsNotExist(err) {
			t.Errorf("the passfile %s is left behind", pf)
		}
	}

	// 同じ宣言なら何周しても drift にならない。
	for i := 0; i < 3; i++ {
		if pl, err := e.plan(t, rid, map[string]string{postgresql.FieldPassword: "secretref:file/pg/app#gen1"}); err != nil || len(pl.Ops) != 0 {
			t.Fatalf("cycle %d: plan = %+v, %v; want nothing", i, pl.Ops, err)
		}
	}
	// 世代を上げれば設定し直す。
	e.sec["pg/app"] = app2PW
	pl, err := e.plan(t, rid, map[string]string{postgresql.FieldPassword: "secretref:file/pg/app#gen2"})
	if err != nil || len(pl.Ops) != 1 {
		t.Fatalf("plan after the generation bump = %+v, %v", pl.Ops, err)
	}
	if s := fmt.Sprint(pl.Ops); strings.Contains(s, app2PW) || strings.Contains(s, appPW) {
		t.Fatal("the plan shows a password value")
	}
	if err := e.p.Apply(context.Background(), pl); err != nil {
		t.Fatal(err)
	}
	if !postgresql.PasswordMatches(e.state().Roles["app"].Password, "app", app2PW) {
		t.Error("the new password was not set")
	}
}

func TestPassword_VerifyFindsAChangeByHand(t *testing.T) {
	e := newEnv(t, func(s *postgresql.Server) { s.VerifyPasswords = true })
	const rid = "main/role/app"
	want := map[string]string{postgresql.FieldLogin: "true", postgresql.FieldPassword: "secretref:file/pg/app#gen1"}
	e.converge(t, rid, want)
	v, _ := postgresql.ScramVerifier("changed-by-hand")
	e.srv.Do(func(st *pgtest.State) { st.Roles["app"].Password = v })
	pl, err := e.plan(t, rid, want)
	if err != nil || len(pl.Ops) != 1 || pl.Ops[0].Fields[0].Field != postgresql.FieldPassword {
		t.Fatalf("plan = %+v, %v; want a password drift", pl.Ops, err)
	}
	e.converge(t, rid, want)
	if !postgresql.PasswordMatches(e.state().Roles["app"].Password, "app", appPW) {
		t.Error("the password did not come back")
	}

	// 管理用のロールが超ユーザでなければ pg_authid を読めない：観測できない。
	e.srv.Do(func(st *pgtest.State) { st.Roles["sakutei"].Super = false })
	obs, err := e.p.Read(context.Background(), rid)
	if err == nil || obs.Availability != core.AvailabilityUnknown || !strings.Contains(err.Error(), "pg_authid") {
		t.Errorf("Read without a superuser = %+v, %v; want Unknown naming pg_authid", obs, err)
	}
}

func TestRole_MemberOfIsTheExactSet(t *testing.T) {
	e := newEnv(t, nil)
	for _, r := range []string{"a", "b", "c"} {
		e.converge(t, "main/role/"+r, map[string]string{})
	}
	e.converge(t, "main/role/x", map[string]string{postgresql.FieldMemberOf: "a,b"})
	e.converge(t, "main/role/x", map[string]string{postgresql.FieldMemberOf: "c,b"})
	if got := fmt.Sprint(e.state().Roles["x"].MemberOf); got != "[b c]" && got != "[c b]" {
		t.Errorf("member_of = %s; want b and c", got)
	}
	// member_of を宣言しなければ触らない。
	e.converge(t, "main/role/x", map[string]string{postgresql.FieldLogin: "true"})
	if n := len(e.state().Roles["x"].MemberOf); n != 2 {
		t.Errorf("member_of was touched without being declared (%d)", n)
	}
}

func TestDatabase_CreateOnlyFields(t *testing.T) {
	e := newEnv(t, nil)
	const did = "main/database/appdb"
	e.converge(t, did, map[string]string{postgresql.FieldEncoding: "UTF8", postgresql.FieldTemplate: "template0", postgresql.FieldLcCtype: "en_US.UTF-8"})
	// 同じ意味の書き方・template は差にならない。
	if pl, err := e.plan(t, did, map[string]string{postgresql.FieldEncoding: "utf-8", postgresql.FieldTemplate: "template1", postgresql.FieldLcCtype: "en_US.utf8"}); err != nil || len(pl.Ops) != 0 {
		t.Errorf("plan = %+v, %v; want nothing", pl.Ops, err)
	}
	_, err := e.plan(t, did, map[string]string{postgresql.FieldEncoding: "LATIN1"})
	if err == nil || !strings.Contains(err.Error(), "cannot be changed after the database is created") {
		t.Errorf("changing encoding: err = %v; want a plan error", err)
	}
	// 所有者・接続数の上限は変えられる。
	e.converge(t, "main/role/app", map[string]string{})
	e.converge(t, did, map[string]string{postgresql.FieldOwner: "app", postgresql.FieldConnectionLimit: "7"})
}

func TestDatabase_CreateInsideATransactionIsAvoided(t *testing.T) {
	e := newEnv(t, nil)
	e.converge(t, "main/database/appdb", map[string]string{postgresql.FieldConnectionLimit: "4"})
	for _, c := range e.srv.Calls() {
		if strings.Contains(c.Stdin, "CREATE DATABASE") && strings.Contains(c.Stdin, "BEGIN") {
			t.Errorf("CREATE DATABASE was sent inside a transaction:\n%s", c.Stdin)
		}
	}
}

func TestGrant(t *testing.T) {
	e := newEnv(t, nil)
	e.converge(t, "main/role/app", map[string]string{})
	e.converge(t, "main/database/appdb", map[string]string{})
	const gid = "main/database/appdb/grant/app"
	e.converge(t, gid, map[string]string{postgresql.FieldPrivileges: "connect, temp"})
	e.converge(t, gid, map[string]string{postgresql.FieldPrivileges: "CREATE"})
	if got := fmt.Sprint(e.state().Databases["appdb"].ACL["app"]); got != "[CREATE]" {
		t.Errorf("acl = %s; want exactly CREATE", got)
	}
	if err := e.p.Apply(context.Background(), core.Plan{Ops: []core.Op{{Kind: core.OpKindDelete, Target: id(gid)}}}); err != nil {
		t.Fatal(err)
	}
	if obs, err := e.p.Read(context.Background(), gid); err != nil || obs.State != nil {
		t.Errorf("after Delete: %+v, %v; want absent", obs, err)
	}
	// 権限の無いデータベース・ロールは Transient（同じ周で後から作られうる）。
	err := e.p.Apply(context.Background(), core.Plan{Ops: []core.Op{{Kind: core.OpKindCreate, Target: id("main/database/nodb/grant/app"),
		Fields: []core.FieldDiff{{Field: postgresql.FieldPrivileges, To: "CONNECT"}}}}})
	var te *provider.TransientError
	if !errors.As(err, &te) {
		t.Errorf("grant on a missing database: %v; want a TransientError", err)
	}
}

func TestExtension_Version(t *testing.T) {
	e := newEnv(t, nil)
	e.converge(t, "main/database/appdb", map[string]string{})
	const xid = "main/database/appdb/extension/hstore"
	e.converge(t, xid, map[string]string{postgresql.FieldVersion: "1.7"})
	e.converge(t, xid, map[string]string{postgresql.FieldVersion: "1.8"})
	// 版を宣言しなければ触らない。
	e.converge(t, xid, map[string]string{})
	if v := e.state().Databases["appdb"].Extensions["hstore"]; v != "1.8" {
		t.Errorf("hstore = %s; want 1.8", v)
	}
	err := e.p.Apply(context.Background(), core.Plan{Ops: []core.Op{{Kind: core.OpKindCreate, Target: id("main/database/appdb/extension/nosuch"), Fields: []core.FieldDiff{}}}})
	var pe *provider.PermanentError
	if !errors.As(err, &pe) {
		t.Errorf("an unavailable extension: %v; want a PermanentError", err)
	}
}

func del(t *testing.T, e *env, local string) error {
	t.Helper()
	return e.p.Apply(context.Background(), core.Plan{Ops: []core.Op{{Kind: core.OpKindDelete, Target: id(local), Destructive: true}}})
}

func TestDelete_KeepsDataAndDependencies(t *testing.T) {
	e := newEnv(t, nil)
	e.converge(t, "main/role/owner", map[string]string{})
	e.converge(t, "main/role/lonely", map[string]string{})
	e.converge(t, "main/database/full", map[string]string{postgresql.FieldOwner: "owner"})
	e.converge(t, "main/database/empty", map[string]string{})
	e.converge(t, "main/database/empty/extension/hstore", map[string]string{})
	e.srv.Do(func(st *pgtest.State) {
		st.Databases["full"].Tables = 2
		st.Databases["empty"].Depended = map[string]bool{"hstore": true}
	})
	var pe *provider.PermanentError
	if err := del(t, e, "main/database/full"); !errors.As(err, &pe) || !strings.Contains(err.Error(), "2 user table(s)") {
		t.Errorf("Delete of a database with tables: %v; want a PermanentError", err)
	}
	if err := del(t, e, "main/role/owner"); !errors.As(err, &pe) || !strings.Contains(err.Error(), "owns objects") {
		t.Errorf("Delete of a role that owns a database: %v; want a PermanentError", err)
	}
	if err := del(t, e, "main/database/empty/extension/hstore"); !errors.As(err, &pe) || !strings.Contains(err.Error(), "depend") {
		t.Errorf("Delete of a depended-on extension: %v; want a PermanentError", err)
	}
	e.srv.Do(func(st *pgtest.State) { st.Databases["empty"].Depended = nil })
	for _, local := range []string{"main/database/empty/extension/hstore", "main/database/empty", "main/role/lonely"} {
		if err := del(t, e, local); err != nil {
			t.Errorf("Delete %s: %v", local, err)
		}
		if err := del(t, e, local); err != nil {
			t.Errorf("Delete %s again (idempotent): %v", local, err)
		}
	}
	st := e.state()
	if _, ok := st.Databases["full"]; !ok {
		t.Error("the database with tables was dropped")
	}
	if _, ok := st.Databases["empty"]; ok {
		t.Error("the empty database was not dropped")
	}
	if _, ok := st.Roles["lonely"]; ok {
		t.Error("the role without objects was not dropped")
	}
}

func TestUnobservable(t *testing.T) {
	cases := map[string]struct {
		mod   func(*postgresql.Server)
		srv   func(*pgtest.Server)
		check string
	}{
		"unreachable":    {srv: func(s *pgtest.Server) { s.Do(func(st *pgtest.State) { st.Unreachable = true }) }, check: "unreachable"},
		"wrong password": {mod: func(s *postgresql.Server) { s.Password = core.NewSecretRef("file", "pg/app", 1) }, check: "authentication failed"},
		"no secret":      {mod: func(s *postgresql.Server) { s.Password = core.NewSecretRef("file", "pg/none", 1) }, check: "cannot be resolved"},
		"no psql":        {srv: func(s *pgtest.Server) { s.Missing = true }, check: "psql cannot be run"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, c.mod)
			if c.srv != nil {
				c.srv(e.srv)
			}
			for _, local := range []string{"main/role/app", "main/database/appdb", "main/database/appdb/grant/app", "main/database/appdb/extension/hstore"} {
				obs, err := e.p.Read(context.Background(), local)
				if err == nil || obs.Availability != core.AvailabilityUnknown || !strings.Contains(err.Error(), c.check) {
					t.Errorf("Read %s = %+v, %v; want Unknown with %q", local, obs, err, c.check)
				}
				if strings.Contains(fmt.Sprint(err), adminPW) || strings.Contains(fmt.Sprint(err), appPW) {
					t.Fatalf("the error shows a password: %v", err)
				}
			}
			if _, err := e.p.List(context.Background(), core.NewScope(nil)); err == nil {
				t.Error("List: want an error")
			}
			if _, err := e.p.BulkObserve(context.Background(), core.NewScope(nil)); err == nil {
				t.Error("BulkObserve: want an error (the only server failed)")
			}
		})
	}
}

func TestBulkObserve_OneServerDownKeepsTheOther(t *testing.T) {
	up := pgtest.New(pgtest.NewState("sakutei", adminPW))
	down := pgtest.New(pgtest.NewState("sakutei", adminPW))
	down.Do(func(st *pgtest.State) { st.Unreachable = true })
	mk := func(srv *pgtest.Server) postgresql.Runner { return postgresql.RunnerFunc(srv.Run) }
	byHost := map[string]postgresql.Runner{"up": mk(up), "down": mk(down)}
	runner := func(ctx context.Context, inv postgresql.Invocation) (postgresql.Result, error) {
		for _, kv := range inv.Env {
			if h, ok := strings.CutPrefix(kv, "PGHOST="); ok {
				return byHost[h](ctx, inv)
			}
		}
		return postgresql.Result{}, errors.New("no host")
	}
	srv := func(host string) postgresql.Server {
		return postgresql.Server{Host: host, User: "sakutei", HasPassword: true, Password: core.NewSecretRef("file", "pg/admin", 1)}
	}
	up.Do(func(st *pgtest.State) { st.Roles["app"] = &pgtest.Role{ConnLimit: -1, Inherit: true} })
	p := postgresql.New(map[string]postgresql.Server{"up": srv("up"), "down": srv("down")}, postgresql.Options{Runner: runner, Secrets: newSecrets()})
	bulk, err := p.BulkObserve(context.Background(), core.NewScope(nil))
	if err != nil {
		t.Fatal(err)
	}
	if o, ok := bulk.Observations[id("up/role/app").Key()]; !ok || o.Availability != core.AvailabilityOk {
		t.Errorf("up/role/app = %+v, %v; want observed", o, ok)
	}
	if _, err := p.List(context.Background(), core.NewScope(nil)); err == nil {
		t.Error("List with a server down: want an error (prune must halt)")
	}
}

func TestValidateIntent(t *testing.T) {
	p := newEnv(t, nil).p
	ok := map[string]map[string]string{
		"main/role/app":                         {postgresql.FieldLogin: "true", postgresql.FieldConnectionLimit: "-1", postgresql.FieldPassword: "secretref:file/pg/app#gen1", postgresql.FieldRole: "app", postgresql.FieldServer: "main"},
		"main/database/appdb":                   {postgresql.FieldOwner: "app", postgresql.FieldEncoding: "UTF8", postgresql.FieldTemplate: "template0"},
		"main/database/appdb/grant/app":         {postgresql.FieldPrivileges: "CONNECT,temp"},
		"main/database/appdb/extension/plpgsql": {postgresql.FieldVersion: "1.0"},
		"main/role/With Space \"and quote'":     {},
	}
	for local, f := range ok {
		if err := p.ValidateIntent(local, f); err != nil {
			t.Errorf("ValidateIntent(%s): %v", local, err)
		}
	}
	bad := map[string]struct {
		id     string
		fields map[string]string
		want   string
	}{
		"system role":       {"main/role/pg_monitor", nil, "system role"},
		"admin role":        {"main/role/sakutei", nil, "connects as"},
		"system database":   {"main/database/template1", nil, "system database"},
		"admin database":    {"main/database/postgres/extension/hstore", nil, "system database"},
		"unknown server":    {"other/role/app", nil, "no server"},
		"bad shape":         {"main/table/x", nil, "want <server>"},
		"slash":             {"main/role/a/b", nil, "want <server>"},
		"too long":          {"main/role/" + strings.Repeat("r", 64), nil, "63 bytes"},
		"equals in db":      {"main/database/a=b", nil, "connection string"},
		"field on kind":     {"main/role/app", map[string]string{postgresql.FieldOwner: "x"}, "cannot be declared on a role"},
		"bool":              {"main/role/app", map[string]string{postgresql.FieldLogin: "yes"}, "true or false"},
		"limit":             {"main/role/app", map[string]string{postgresql.FieldConnectionLimit: "-2"}, "connection_limit"},
		"raw password":      {"main/role/app", map[string]string{postgresql.FieldPassword: "hunter2"}, "SecretRef"},
		"self member":       {"main/role/app", map[string]string{postgresql.FieldMemberOf: "x,app"}, "itself"},
		"no privileges":     {"main/database/appdb/grant/app", nil, "privileges is required"},
		"bad privilege":     {"main/database/appdb/grant/app", map[string]string{postgresql.FieldPrivileges: "SELECT"}, "CONNECT"},
		"identity mismatch": {"main/role/app", map[string]string{postgresql.FieldRole: "other"}, "must equal"},
		"grant to pg role":  {"main/database/appdb/grant/pg_monitor", map[string]string{postgresql.FieldPrivileges: "CONNECT"}, "system role"},
	}
	for name, c := range bad {
		err := p.ValidateIntent(c.id, c.fields)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: ValidateIntent(%s) = %v; want an error with %q", name, c.id, err, c.want)
		}
		if err != nil && strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: the error shows the password", name)
		}
	}
}

func TestReadBack_MismatchIsPermanent(t *testing.T) {
	// サーバが受け付けても別の値で持つ（ここでは接続数の上限を 5 にしてしまう）。
	srv := pgtest.New(pgtest.NewState("sakutei", adminPW))
	inner := postgresql.RunnerFunc(srv.Run)
	runner := func(ctx context.Context, inv postgresql.Invocation) (postgresql.Result, error) {
		inv.Stdin = strings.ReplaceAll(inv.Stdin, "CONNECTION LIMIT 7", "CONNECTION LIMIT 5")
		return inner(ctx, inv)
	}
	s := postgresql.Server{User: "sakutei", HasPassword: true, Password: core.NewSecretRef("file", "pg/admin", 1)}
	p := postgresql.New(map[string]postgresql.Server{"main": s}, postgresql.Options{Runner: runner, Secrets: newSecrets()})
	err := p.Apply(context.Background(), core.Plan{Ops: []core.Op{{Kind: core.OpKindCreate, Target: id("main/role/app"),
		Fields: []core.FieldDiff{{Field: postgresql.FieldConnectionLimit, To: "7"}}}}})
	var pe *provider.PermanentError
	if !errors.As(err, &pe) || !strings.Contains(err.Error(), `keeps connection_limit as "5"`) {
		t.Errorf("Apply = %v; want a PermanentError naming the value the server keeps", err)
	}
}

// TestExecRunner は本物の exec の経路（引数・環境変数・passfile・終了コード）を、
// このテストのバイナリを偽の psql として起動して確かめる。
func TestExecRunner(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	if err := pgtest.SaveState(statePath, pgtest.NewState("sakutei", adminPW)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SAKUTEI_FAKE_PSQL_MAIN", "1")
	t.Setenv("SAKUTEI_FAKE_PSQL_STATE", statePath)
	t.Setenv("PGPASSWORD", "must-not-be-used")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	srv := postgresql.Server{Host: "db.test", User: "sakutei", HasPassword: true, Password: core.NewSecretRef("file", "pg/admin", 1)}
	p := postgresql.New(map[string]postgresql.Server{"main": srv}, postgresql.Options{Psql: self, Secrets: newSecrets()})
	ctx := context.Background()
	local := postgresql.RoleID("main", `o'q"x`)
	obs, err := p.Read(ctx, local)
	if err != nil || obs.State != nil {
		t.Fatalf("Read = %+v, %v; want absent", obs, err)
	}
	pl, err := diff.ForProvider(p).Plan(obs, diff.DesiredResource{ID: id(local), Fields: map[string]string{postgresql.FieldLogin: "true"}}, cause.CauseContext{ResourceObservedAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(ctx, pl); err != nil {
		t.Fatal(err)
	}
	st, err := pgtest.LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := st.Roles[`o'q"x`]; !ok || !r.Login {
		t.Errorf("role after Apply through exec = %+v", r)
	}
	// psql が無い：観測できない。
	p2 := postgresql.New(map[string]postgresql.Server{"main": srv}, postgresql.Options{Psql: filepath.Join(dir, "no-psql"), Secrets: newSecrets()})
	if obs, err := p2.Read(ctx, local); err == nil || obs.Availability != core.AvailabilityUnknown {
		t.Errorf("Read without psql = %+v, %v; want Unknown", obs, err)
	}
}
