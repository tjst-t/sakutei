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

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/providers/postgresql/pgfake"
	"github.com/tjst-t/sakutei/v3/sdk/contract"
)

const (
	adminUser = "sakutei"
	adminPW   = "admin:pw\\x"
	roUser    = "sakutei_ro"
	roPW      = "ro-pw:1"
	appPW     = "app-secret-gen1"
	app2PW    = "app-secret-gen2"
)

// secrets は秘密の参照（世代を除く）→ 値。
type secrets map[string]string

func (s secrets) Resolve(_ context.Context, ref core.SecretRef) (string, error) {
	v, ok := s[ref.Ref()]
	if !ok {
		return "", fmt.Errorf("秘密 %s が無い", ref.Ref())
	}
	return v, nil
}

type env struct {
	srv  *pgfake.Server
	p    *Provider
	dir  string
	sec  secrets
	opts map[string]any // settings.servers.main
}

// newEnv は偽のサーバと provider を作る。観測は読むだけのロール（sakutei_ro）、
// 書き込みは管理用のロール（sakutei、超ユーザ）。初めの現実：ロール old（olddb の
// 所有者）・user1（olddb に CONNECT）・lonely（どこかの表を持つ）、olddb（拡張 citext）。
func newEnv(t *testing.T, mod func(server map[string]any)) *env {
	t.Helper()
	st := pgfake.NewState(adminUser, adminPW)
	st.ReadOnly = map[string]string{roUser: roPW}
	st.Roles[roUser] = &pgfake.Role{Login: true, Inherit: true, ConnLimit: -1}
	st.Roles["old"] = &pgfake.Role{Inherit: true, ConnLimit: -1}
	st.Roles["user1"] = &pgfake.Role{Login: true, Inherit: true, ConnLimit: -1}
	st.Roles["lonely"] = &pgfake.Role{Inherit: true, ConnLimit: -1, OwnsObjects: true}
	st.Databases["olddb"] = &pgfake.Database{Owner: "old", Encoding: "UTF8", Collate: "en_US.utf8", Ctype: "en_US.utf8", ConnLimit: -1, AllowConn: true,
		ACL:        map[string][]string{"old": {"CONNECT", "CREATE", "TEMPORARY"}, "user1": {"CONNECT"}, "": {"CONNECT", "TEMPORARY"}},
		Extensions: map[string]string{"plpgsql": "1.0", "citext": "1.6"}}
	e := &env{srv: pgfake.New(st), dir: t.TempDir(), sec: secrets{"t:app": appPW}}
	for name, pw := range map[string]string{"admin": adminPW, "ro": roPW} {
		if err := os.WriteFile(filepath.Join(e.dir, name), []byte(pw+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	e.opts = map[string]any{
		"host":    "db.example",
		"port":    5432,
		"observe": map[string]any{"user": roUser, "password_file": filepath.Join(e.dir, "ro")},
		"write":   map[string]any{"user": adminUser, "password_file": filepath.Join(e.dir, "admin")},
	}
	if mod != nil {
		mod(e.opts)
	}
	p, err := New(map[string]any{"psql": "psql", "servers": map[string]any{"main": e.opts}}, RunnerFunc(e.srv.Run))
	if err != nil {
		t.Fatal(err)
	}
	e.p = p
	return e
}

func (e *env) state() *pgfake.State {
	var out *pgfake.State
	e.srv.Do(func(st *pgfake.State) { out = st })
	return out
}

func id(kind, name string) core.ID { return core.ID{Type: "pg/" + kind, Name: name} }

func res(kind, name string, f core.Fields) core.Resource {
	return core.Resource{ID: id(kind, name), Fields: f}
}

var mainRange = core.Range{Provider: "pg", Args: map[string]string{"server": "main"}}

func (e *env) observe(t *testing.T, i core.ID) core.Observation {
	t.Helper()
	obs, err := e.p.Observe(context.Background(), []core.ID{i})
	if err != nil {
		t.Fatal(err)
	}
	return obs[i]
}

func (e *env) compare(t *testing.T, r core.Resource) plan.Change {
	t.Helper()
	return plan.Compare(r, e.p.Schema().Kinds[r.ID.Kind()], e.observe(t, r.ID))
}

// converge は宣言を Validate に渡し、ずれていれば当て、宣言どおりになったことを確かめる。
func (e *env) converge(t *testing.T, rs ...core.Resource) {
	t.Helper()
	for _, d := range e.p.Validate(rs, nil) {
		t.Fatalf("Validate: %s", d)
	}
	for _, r := range rs {
		c := e.compare(t, r)
		if c.Op == plan.None {
			continue
		}
		if c.Op != plan.Create && c.Op != plan.Update {
			t.Fatalf("%s: %s %s", r.ID, c.Op, c.Note)
		}
		if err := e.p.Apply(context.Background(), core.NewWriteContext(e.sec), r.ID, r.Fields, c.Current); err != nil {
			t.Fatalf("%s: Apply: %v", r.ID, err)
		}
		if c := e.compare(t, r); c.Op != plan.None {
			t.Fatalf("%s: Apply のあとも %s %+v %s", r.ID, c.Op, c.Fields, c.Note)
		}
	}
}

func TestContract(t *testing.T) {
	contract.Run(t, func(t *testing.T) contract.Target {
		e := newEnv(t, nil)
		return contract.Target{
			Provider: e.p,
			Ranges:   []core.Range{mainRange},
			Absent: []core.ID{id(KindRole, "main/nobody"), id(KindDatabase, "main/nodb"),
				id(KindGrant, "main/olddb/nobody"), id(KindExtension, "main/olddb/hstore")},
			Create: []core.Resource{
				res(KindRole, "main/app", core.Fields{FieldLogin: true, FieldConnectionLimit: int64(10),
					FieldMemberOf: []any{"pg_read_all_data"}, FieldPassword: "t:app@gen=1"}),
				res(KindRole, "main/reader", core.Fields{FieldLogin: false, FieldMemberOf: []any{"app"}}),
				res(KindDatabase, "main/appdb", core.Fields{FieldOwner: "app", FieldEncoding: "UTF-8", FieldLcCollate: "C",
					FieldTemplate: "template0", FieldConnectionLimit: int64(50), FieldConnectRoles: []any{roUser}}),
				res(KindGrant, "main/appdb/reader", core.Fields{FieldPrivileges: []any{"connect", "TEMP"}}),
				res(KindGrant, "main/appdb/PUBLIC", core.Fields{FieldPrivileges: []any{}}),
				res(KindExtension, "main/appdb/hstore", core.Fields{FieldVersion: "1.7"}),
			},
			Converge: []core.Resource{res(KindRole, "main/old", core.Fields{FieldConnectionLimit: int64(5), FieldLogin: true})},
			Observed: []core.ID{id(KindRole, "main/old"), id(KindRole, "main/user1"), id(KindDatabase, "main/olddb"),
				id(KindGrant, "main/olddb/user1"), id(KindGrant, "main/olddb/PUBLIC"), id(KindExtension, "main/olddb/citext")},
			Faults: map[string]func(){
				"接続を切る":         func() { e.srv.Do(func(st *pgfake.State) { st.Unreachable = true }) },
				"認証の失敗":         func() { e.srv.Do(func(st *pgfake.State) { st.ReadOnly[roUser] = "other" }) },
				"psql が無い":      func() { e.srv.Missing = true },
				"壊れた出力":         func() { e.srv.Broken = true },
				"パスワードのファイルが無い": func() { os.Remove(filepath.Join(e.dir, "ro")) },
			},
			Writes:               e.srv.Writes,
			Secrets:              []string{adminPW, roPW, appPW},
			WriteCredentialReads: func() int { return e.srv.Users()[adminUser] },
			Refuses: []contract.Refuse{
				{ID: id(KindDatabase, "main/olddb"), Prepare: func() { e.srv.Do(func(st *pgfake.State) { st.Databases["olddb"].Tables = 1 }) }},
				{ID: id(KindRole, "main/old")},    // DB を持つ
				{ID: id(KindRole, "main/lonely")}, // 表を持つ（PostgreSQL が 2BP01 で断る）
				{ID: id(KindExtension, "main/olddb/citext"), Prepare: func() {
					e.srv.Do(func(st *pgfake.State) { st.Databases["olddb"].Depended = map[string]bool{"citext": true} })
				}},
			},
			Secret: e.sec,
		}
	})
}

// 名前はいつも quote する（E-F1）。
func TestNamesAreQuoted(t *testing.T) {
	e := newEnv(t, nil)
	evilRole := `x"; DROP ROLE sakutei; --`
	evilDB := `d'b"; DROP DATABASE postgres; --`
	parent := `Parent Role\`
	e.converge(t,
		res(KindRole, "main/"+parent, core.Fields{}),
		res(KindRole, "main/"+evilRole, core.Fields{FieldMemberOf: []any{parent}, FieldConnectionLimit: int64(3)}),
		res(KindDatabase, "main/"+evilDB, core.Fields{FieldOwner: evilRole, FieldLcCollate: "en_US.utf8"}),
		res(KindGrant, "main/"+evilDB+"/"+parent, core.Fields{FieldPrivileges: []any{"CREATE"}}),
		res(KindExtension, "main/"+evilDB+"/citext", core.Fields{}),
	)
	st := e.state()
	if _, ok := st.Roles[adminUser]; !ok {
		t.Fatal("ロールの名前から管理用のロールが消された")
	}
	if _, ok := st.Databases["postgres"]; !ok {
		t.Fatal("DB の名前から postgres が消された")
	}
	if r, ok := st.Roles[evilRole]; !ok || len(r.MemberOf) != 1 || r.MemberOf[0] != parent || r.ConnLimit != 3 {
		t.Errorf("ロール %q = %+v", evilRole, r)
	}
	if d, ok := st.Databases[evilDB]; !ok || d.Owner != evilRole || d.Extensions["citext"] == "" {
		t.Errorf("DB %q = %+v", evilDB, d)
	}
}

// パスワードは平文で送らず、参照と世代だけで比べる。verify_passwords なら手の変更を
// 見つける（E-F1・E-G2・N-F1）。
func TestPassword(t *testing.T) {
	e := newEnv(t, nil)
	r := res(KindRole, "main/app", core.Fields{FieldLogin: true, FieldPassword: "t:app@gen=1"})
	e.converge(t, r)
	st := e.state()
	role := st.Roles["app"]
	if !strings.HasPrefix(role.Password, "SCRAM-SHA-256$") || st.PlaintextPasswords != 0 {
		t.Fatalf("pg_authid = %q（平文 %d 回）", role.Password, st.PlaintextPasswords)
	}
	if role.Comment == nil || *role.Comment != PasswordMarker+"t:app@gen=1"+verifierMarker+verifierDigest(role.Password) {
		t.Fatalf("コメント = %v", role.Comment)
	}
	for _, c := range e.srv.Calls() {
		blob := strings.Join(c.Args, " ") + fmt.Sprint(c.Env) + c.Stdin
		for _, s := range []string{appPW, adminPW, roPW} {
			if strings.Contains(blob, s) {
				t.Fatalf("psql の引数・環境変数・SQL に秘密 %q が出た", s)
			}
		}
		if pf := c.Env["PGPASSFILE"]; pf == "" {
			t.Error("PGPASSFILE が無い")
		} else if _, err := os.Stat(pf); !os.IsNotExist(err) {
			t.Errorf("passfile %s が残った", pf)
		}
	}
	first := role.Password
	// 同じ宣言なら何周しても書かない。
	n := e.srv.Writes()
	for i := 0; i < 3; i++ {
		e.converge(t, r)
	}
	if e.srv.Writes() != n {
		t.Error("同じ宣言で書いた")
	}
	// 世代が変われば設定し直す。
	e.sec["t:app"] = app2PW
	r2 := res(KindRole, "main/app", core.Fields{FieldLogin: true, FieldPassword: "t:app@gen=2"})
	if c := e.compare(t, r2); c.Op != plan.Update || fmt.Sprint(c.Fields) == "" || strings.Contains(fmt.Sprint(c), app2PW) {
		t.Fatalf("世代の変更: %+v", c)
	}
	e.converge(t, r2)
	if e.state().Roles["app"].Password == first {
		t.Error("新しい世代のパスワードを設定しなかった")
	}

	// verify_passwords：観測用のロールが pg_authid を読めなければ Unknown（E-E6）。
	v := newEnv(t, func(s map[string]any) { s["verify_passwords"] = true })
	for _, d := range v.p.Validate([]core.Resource{r}, nil) {
		t.Fatal(d)
	}
	if err := v.p.Apply(context.Background(), core.NewWriteContext(v.sec), r.ID, r.Fields, core.Observation{}); err != nil {
		t.Fatal(err)
	}
	if o := v.observe(t, r.ID); o.Availability != core.Unknown || !strings.Contains(o.Note, "pg_authid") {
		t.Fatalf("読むだけのロールで verify_passwords: %+v", o)
	}
	// 超ユーザで観測すれば、手で変えたパスワードがずれになり Apply が戻す。
	s := newEnv(t, func(s map[string]any) {
		s["verify_passwords"] = true
		s["observe"] = s["write"]
	})
	s.converge(t, r)
	verifier, _ := ScramVerifier("changed-by-hand")
	s.srv.Do(func(st *pgfake.State) { st.Roles["app"].Password = verifier })
	if c := s.compare(t, r); c.Op != plan.Update || len(c.Fields) != 1 || c.Fields[0].Name != FieldPassword {
		t.Fatalf("手で変えたパスワード: %+v", c)
	}
	s.converge(t, r)
	if s.state().Roles["app"].Password == verifier {
		t.Error("パスワードが戻らない")
	}
}

// member_of は集合ちょうど。属性は宣言したものだけ（E-F1・E-D7）。
func TestRole(t *testing.T) {
	e := newEnv(t, nil)
	e.converge(t, res(KindRole, "main/a", core.Fields{}), res(KindRole, "main/b", core.Fields{}),
		res(KindRole, "main/x", core.Fields{FieldMemberOf: []any{"a", "b"}, FieldBypassRLS: false, FieldReplication: false}))
	e.srv.Do(func(st *pgfake.State) {
		st.Roles["x"].BypassRLS = true
		st.Roles["x"].Replication = true
		st.Roles["x"].CreateDB = true // 宣言していない
	})
	x := res(KindRole, "main/x", core.Fields{FieldMemberOf: []any{"b"}, FieldBypassRLS: false, FieldReplication: false})
	e.converge(t, x)
	r := e.state().Roles["x"]
	if strings.Join(r.MemberOf, ",") != "b" || r.BypassRLS || r.Replication || !r.CreateDB {
		t.Errorf("x = %+v", r)
	}
}

// encoding・ロケールは宣言の書き方で比べ、作った後は変えない。template は作るとき
// だけ（E-F1・E-I7）。
func TestDatabaseCreateOnly(t *testing.T) {
	e := newEnv(t, nil)
	db := res(KindDatabase, "main/appdb", core.Fields{FieldEncoding: "utf-8", FieldLcCtype: "en_US.UTF-8", FieldTemplate: "template0"})
	e.converge(t, db)
	if d := e.state().Databases["appdb"]; d.Encoding != "UTF8" {
		t.Fatalf("appdb = %+v", d)
	}
	bad := res(KindDatabase, "main/appdb", core.Fields{FieldEncoding: "LATIN1"})
	e.p.Validate([]core.Resource{bad}, nil)
	c := e.compare(t, bad)
	if c.Op != plan.Update {
		t.Fatalf("%+v", c)
	}
	err := e.p.Apply(context.Background(), core.NewWriteContext(nil), bad.ID, bad.Fields, c.Current)
	if err == nil || !strings.Contains(err.Error(), "変えられない") {
		t.Fatalf("encoding の変更: %v", err)
	}
}

// PUBLIC の権限は宣言どおり（空の集合も）、宣言しなければ触らない。ロールの権限は
// 集合ちょうど（E-F1）。
func TestGrant(t *testing.T) {
	e := newEnv(t, nil)
	e.converge(t,
		res(KindGrant, "main/olddb/user1", core.Fields{FieldPrivileges: []any{"CREATE", "temporary"}}),
		res(KindGrant, "main/olddb/PUBLIC", core.Fields{FieldPrivileges: []any{"CONNECT"}}),
	)
	acl := e.state().Databases["olddb"].ACL
	sort.Strings(acl["user1"])
	if strings.Join(acl["user1"], ",") != "CREATE,TEMPORARY" || strings.Join(acl[""], ",") != "CONNECT" {
		t.Fatalf("acl = %v", acl)
	}
	e.converge(t, res(KindGrant, "main/olddb/PUBLIC", core.Fields{FieldPrivileges: []any{}}))
	if p := e.state().Databases["olddb"].ACL[""]; len(p) != 0 {
		t.Fatalf("PUBLIC = %v", p)
	}
	if err := e.p.Delete(context.Background(), core.NewWriteContext(nil), id(KindGrant, "main/olddb/user1"), core.Observation{}); err != nil {
		t.Fatal(err)
	}
	if o := e.observe(t, id(KindGrant, "main/olddb/user1")); o.Availability != core.Absent {
		t.Fatalf("REVOKE ALL のあと %+v", o)
	}
	var re *core.RefuseError
	if err := e.p.Delete(context.Background(), core.NewWriteContext(nil), id(KindGrant, "main/olddb/PUBLIC"), core.Observation{}); !errors.As(err, &re) {
		t.Fatalf("PUBLIC の Delete: %v", err)
	}
}

// 拡張の版は宣言したときだけ合わせる（E-F1）。
func TestExtensionVersion(t *testing.T) {
	e := newEnv(t, nil)
	e.converge(t, res(KindExtension, "main/olddb/hstore", core.Fields{FieldVersion: "1.7"}))
	e.converge(t, res(KindExtension, "main/olddb/hstore", core.Fields{FieldVersion: "1.8"}))
	if v := e.state().Databases["olddb"].Extensions["hstore"]; v != "1.8" {
		t.Fatalf("hstore = %q", v)
	}
}

// connect_roles は作る Apply の中で付け、宣言の集合が観測に含まれていれば同じ。
// 足すだけで外さない（E-F3・N-F7）。
func TestConnectRoles(t *testing.T) {
	e := newEnv(t, nil)
	db := res(KindDatabase, "main/locked", core.Fields{FieldOwner: "old", FieldConnectRoles: []any{roUser}})
	pub := res(KindGrant, "main/locked/PUBLIC", core.Fields{FieldPrivileges: []any{}})
	e.converge(t, db, pub)
	// 観測用のロールは PUBLIC の CONNECT が無くても繋げ、List は完全。
	if l, err := e.p.List(context.Background(), mainRange); err != nil || !l.Complete {
		t.Fatalf("List: %v %+v", err, l)
	}
	// 宣言していない境界の外のロールの CONNECT は残す（観測は宣言の値のまま）。
	e.srv.Do(func(st *pgfake.State) { st.Databases["locked"].ACL["pg_monitor"] = []string{"CONNECT"} })
	if c := e.compare(t, db); c.Op != plan.None {
		t.Fatalf("宣言外の境界の外のロール: %+v", c)
	}
	// 手で外せばずれになり、足す（pg_monitor は外さない）。
	e.srv.Do(func(st *pgfake.State) { delete(st.Databases["locked"].ACL, roUser) })
	if _, err := e.p.List(context.Background(), mainRange); err == nil {
		t.Fatal("繋げない DB があるのに List が完全")
	}
	if c := e.compare(t, db); c.Op != plan.Update {
		t.Fatalf("手の REVOKE: %+v", c)
	}
	e.converge(t, db, pub)
	acl := e.state().Databases["locked"].ACL
	if len(acl[roUser]) == 0 || len(acl["pg_monitor"]) == 0 {
		t.Fatalf("acl = %v", acl)
	}
}

// List は管理の境界の内のものだけ（E-F1・E-K2）。
func TestListBoundary(t *testing.T) {
	e := newEnv(t, nil)
	l, err := e.p.List(context.Background(), mainRange)
	if err != nil || !l.Complete {
		t.Fatal(err)
	}
	var got []string
	for _, i := range l.IDs {
		got = append(got, i.String())
	}
	sort.Strings(got)
	want := "pg/database/main/olddb pg/extension/main/olddb/citext pg/grant/main/olddb/user1 pg/role/main/lonely pg/role/main/old pg/role/main/user1"
	if strings.Join(got, " ") != want {
		t.Errorf("List = %v\nwant %s", got, want)
	}
}

func TestValidate(t *testing.T) {
	e := newEnv(t, nil)
	cases := map[string]core.Resource{
		"管理用のロール":         res(KindRole, "main/"+adminUser, core.Fields{}),
		"観測用のロール":         res(KindRole, "main/"+roUser, core.Fields{}),
		"pg_":             res(KindRole, "main/pg_monitor", core.Fields{}),
		"public":          res(KindRole, "main/Public", core.Fields{}),
		"知らないサーバ":         res(KindRole, "other/app", core.Fields{}),
		"id の形":           res(KindGrant, "main/appdb", core.Fields{}),
		"システムの DB":        res(KindDatabase, "main/template1", core.Fields{}),
		"DB の =":          res(KindDatabase, "main/a=b", core.Fields{}),
		"長い名前":            res(KindRole, "main/"+strings.Repeat("r", 64), core.Fields{}),
		"自分に属す":           res(KindRole, "main/app", core.Fields{FieldMemberOf: []any{"app"}}),
		"上限":              res(KindRole, "main/app", core.Fields{FieldConnectionLimit: int64(-2)}),
		"権限が無い":           res(KindGrant, "main/appdb/app", core.Fields{}),
		"権限が空":            res(KindGrant, "main/appdb/app", core.Fields{FieldPrivileges: []any{}}),
		"知らない権限":          res(KindGrant, "main/appdb/app", core.Fields{FieldPrivileges: []any{"SELECT"}}),
		"connect_roles":   res(KindDatabase, "main/appdb", core.Fields{FieldConnectRoles: []any{"app"}}),
		"encoding":        res(KindDatabase, "main/appdb", core.Fields{FieldEncoding: "x y"}),
		"PUBLIC の absent": {ID: id(KindGrant, "main/appdb/PUBLIC"), Ensure: core.EnsureAbsent},
	}
	for name, r := range cases {
		if d := e.p.Validate([]core.Resource{r}, nil); len(d) == 0 || d[0].Severity != core.Reject {
			t.Errorf("%s: 断らない %v", name, d)
		}
	}
	for name, rg := range map[string]core.Range{
		"server が無い": {Provider: "pg"},
		"知らないサーバ":    {Provider: "pg", Args: map[string]string{"server": "other"}},
		"知らない引数":     {Provider: "pg", Args: map[string]string{"server": "main", "scope": "x"}},
	} {
		if d := e.p.Validate(nil, []core.Range{rg}); len(d) == 0 {
			t.Errorf("範囲 %s: 断らない", name)
		}
	}
	ok := []core.Resource{
		res(KindRole, "main/app", core.Fields{FieldMemberOf: []any{"pg_read_all_data"}}),
		res(KindDatabase, "main/appdb", core.Fields{FieldConnectRoles: []any{roUser, "pg_monitor"}}),
		res(KindGrant, "main/appdb/PUBLIC", core.Fields{FieldPrivileges: []any{}}),
		res(KindExtension, "main/appdb/plpgsql", core.Fields{}),
		{ID: id(KindDatabase, "main/gone"), Ensure: core.EnsureAbsent},
	}
	if d := e.p.Validate(ok, []core.Range{mainRange}); len(d) != 0 {
		t.Errorf("書ける宣言を断った: %v", d)
	}
}

func TestModuleSettings(t *testing.T) {
	obs := map[string]any{"user": "u"}
	for name, s := range map[string]map[string]any{
		"servers が無い":     {},
		"知らない項目":          {"servers": map[string]any{"main": map[string]any{"observe": obs}}, "x": 1},
		"observe が無い":     {"servers": map[string]any{"main": map[string]any{"host": "h"}}},
		"パスワードそのもの":       {"servers": map[string]any{"main": map[string]any{"observe": map[string]any{"user": "u", "password": "p"}}}},
		"サーバの名前":          {"servers": map[string]any{"Main/x": map[string]any{"observe": obs}}},
		"sslmode":         {"servers": map[string]any{"main": map[string]any{"observe": obs, "sslmode": "maybe"}}},
		"port":            {"servers": map[string]any{"main": map[string]any{"observe": obs, "port": 70000}}},
		"connect_timeout": {"servers": map[string]any{"main": map[string]any{"observe": obs, "connect_timeout": "0s"}}},
	} {
		if _, err := Module(s); err == nil {
			t.Errorf("%s: 断らない", name)
		}
	}
	p, err := New(map[string]any{"servers": map[string]any{"main": map[string]any{"observe": obs}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s := p.servers["main"]; s.Write != s.Observe || s.DBName != "postgres" || s.ConnectTimeout != DefaultConnectTimeout {
		t.Errorf("既定 = %+v", s)
	}
}

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{
		`app`: `"app"`, `App User`: `"App User"`, `x"; DROP ROLE admin; --`: `"x""; DROP ROLE admin; --"`, `o'q\`: `"o'q\"`,
	} {
		if got, err := QuoteIdent(in); err != nil || got != want {
			t.Errorf("QuoteIdent(%q) = %q, %v", in, got, err)
		}
	}
	for in, want := range map[string]string{
		`it's`: `E'it''s'`, `back\slash`: `E'back\\slash'`, `\'; SELECT 1; --`: `E'\\''; SELECT 1; --'`,
	} {
		if got, err := QuoteLiteral(in); err != nil || got != want {
			t.Errorf("QuoteLiteral(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := QuoteIdent("a\x00b"); err == nil {
		t.Error("NUL を通した")
	}
}

func TestScramKnownVector(t *testing.T) {
	// RFC 7677 §3：salt=W22ZaJ0SNY7soEsUEjb6gQ==、i=4096、password=pencil。
	salt := []byte{0x5b, 0x6d, 0x99, 0x68, 0x9d, 0x12, 0x35, 0x8e, 0xec, 0xa0, 0x4b, 0x14, 0x12, 0x36, 0xfa, 0x81}
	want := "SCRAM-SHA-256$4096:W22ZaJ0SNY7soEsUEjb6gQ==$WG5d8oPm3OtcPnkdi4Uo7BkeZkBFzpcXkuLmtbsT4qY=:wfPLwcE6nTWhTAmQ7tl2KeoiWGPlZqQxSrmfPwDl2dU="
	if got := scramVerifierWithSalt("pencil", salt, 4096); got != want {
		t.Errorf("verifier = %s", got)
	}
	if _, err := ScramVerifier("pässword"); err == nil {
		t.Error("ASCII でないパスワードを通した")
	}
}

func TestPsqlHelpers(t *testing.T) {
	if got := childEnv([]string{"PATH=/bin", "PGOPTIONS=-c x", "PGSERVICE=prod", "PGPASSWORD=leak", "HOME=/h", "LC_ALL=ja_JP.UTF-8"}); strings.Join(got, " ") != "PATH=/bin HOME=/h" {
		t.Errorf("childEnv = %v", got)
	}
	path, err := writePassfile(`pa:ss\word`)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	fi, _ := os.Stat(path)
	data, _ := os.ReadFile(path)
	if fi.Mode().Perm() != 0o600 || string(data) != "*:*:*:*:pa\\:ss\\\\word\n" {
		t.Errorf("passfile = %v %q", fi.Mode().Perm(), data)
	}
	if got := redact("ERROR:  42601: x PASSWORD E'SCRAM-SHA-256$4096:c2FsdA==$AAAA:BBBB' md50123456789abcdef0123456789abcdef"); strings.Contains(got, "c2FsdA") || strings.Contains(got, "0123456789abcdef0123") {
		t.Errorf("redact = %s", got)
	}
	if canonLocale("en_US.UTF-8") != canonLocale("en_US.utf8") || canonEncoding("UTF-8") != canonEncoding("utf8") {
		t.Error("正規形")
	}
}
