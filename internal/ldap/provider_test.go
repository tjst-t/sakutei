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

package ldap_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/cause"
	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/ldap"
	"github.com/tjst-t/sakutei/v2/internal/ldap/ldaptest"
	"github.com/tjst-t/sakutei/v2/internal/prune"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
)

// TestMain は、環境変数 SAKUTEI_FAKE_LDAP_MAIN があればこのテストのバイナリを
// 偽の ldap コマンドとして動かす（ExecRunner の経路を本物の exec で確かめるため）。
func TestMain(m *testing.M) {
	if os.Getenv("SAKUTEI_FAKE_LDAP_MAIN") == "1" {
		ldaptest.Main()
	}
	os.Exit(m.Run())
}

const (
	dmPW     = "dm-pw:with space\\"
	alicePW  = "alice-secret-1"
	alice2PW = "alice-secret-2"
	suffix   = "dc=example,dc=com"
	bindDN   = "cn=Directory Manager"
)

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

type env struct {
	srv *ldaptest.Server
	p   *ldap.Provider
	sec secrets
}

func server(mod func(*ldap.Server)) ldap.Server {
	s := ldap.Server{URI: "ldap://ldap.test", StartTLS: true, BindDN: bindDN, BindPassword: core.NewSecretRef("file", "ldap/dm", 1),
		BaseDN: suffix, Timeout: 5 * time.Second, Scope: core.NewScope(nil)}
	if mod != nil {
		mod(&s)
	}
	return s
}

func newEnv(t *testing.T, mod func(*ldap.Server)) *env {
	t.Helper()
	srv := ldaptest.New(ldaptest.NewState(bindDN, dmPW, suffix))
	sec := secrets{"ldap/dm": dmPW, "ldap/alice": alicePW, "ldap/alice2": alice2PW}
	p, err := ldap.New(map[string]ldap.Server{"dir1": server(mod)}, ldap.Options{Runner: ldap.RunnerFunc(srv.Run), Secrets: sec})
	if err != nil {
		t.Fatal(err)
	}
	return &env{srv: srv, p: p, sec: sec}
}

func id(local string) core.ID { return core.NewID(ldap.Name, core.NewScope(nil), local) }

const (
	peopleID = "dir1/ou=people,dc=example,dc=com"
	aliceID  = "dir1/uid=alice,ou=people,dc=example,dc=com"
	groupID  = "dir1/cn=admins,ou=people,dc=example,dc=com"
)

func people() map[string]string {
	return map[string]string{"objectClass": `["top","organizationalUnit"]`, "ou": "People", "description": "the people"}
}

func alice() map[string]string {
	return map[string]string{
		"objectClass":  `["top","inetOrgPerson"]`,
		"uid":          "Alice",
		"cn":           "Alice Liddell",
		"sn":           "Liddell",
		"mail":         `["alice@example.com","al@example.com"]`,
		"userPassword": "secretref:file/ldap/alice#gen1",
	}
}

func group() map[string]string {
	return map[string]string{"objectClass": "groupOfNames", "cn": "admins", "member": "UID=Alice, OU=People,dc=example,dc=com"}
}

// plan は Read → Plan（RefinePlan まで）。
func (e *env) plan(t *testing.T, local string, fields map[string]string) core.Plan {
	t.Helper()
	if err := e.p.ValidateIntent(local, fields); err != nil {
		t.Fatalf("ValidateIntent(%s): %v", local, err)
	}
	obs, err := e.p.Read(context.Background(), local)
	if err != nil {
		t.Fatalf("Read(%s): %v", local, err)
	}
	pl, err := diff.ForProvider(e.p).Plan(obs, diff.DesiredResource{ID: id(local), Fields: fields}, cause.CauseContext{ResourceObservedAbsent: obs.State == nil})
	if err != nil {
		t.Fatalf("Plan(%s): %v", local, err)
	}
	return pl
}

// converge は plan を Apply し、もう一度 plan して何も無いことを確かめる。
func (e *env) converge(t *testing.T, local string, fields map[string]string) core.Plan {
	t.Helper()
	pl := e.plan(t, local, fields)
	if len(pl.Ops) > 0 {
		if err := e.p.Apply(context.Background(), pl); err != nil {
			t.Fatalf("Apply(%s): %v", local, err)
		}
	}
	if again := e.plan(t, local, fields); len(again.Ops) != 0 {
		t.Fatalf("%s: not converged after Apply: %+v", local, again.Ops)
	}
	return pl
}

func TestContract(t *testing.T) {
	e := newEnv(t, nil)
	e.srv.Do(func(st *ldaptest.State) {
		st.Put("ou=people,"+suffix, map[string][]string{"objectClass": {"top", "organizationalUnit"}, "ou": {"people"}})
	})
	ct := sdk.ContractTester{
		Provider: e.p,
		Lister:   e.p,
		Scope:    core.NewScope(nil),
		SampleID: "dir1/uid=contract,ou=people,dc=example,dc=com",
		SampleDesired: map[string]string{
			"objectclass": `["inetOrgPerson","organizationalPerson","person","top"]`,
			"uid":         "contract",
			"cn":          "Contract",
			"sn":          "Tester",
		},
		MakeUnreachable: func() (func(), error) {
			e.srv.Do(func(st *ldaptest.State) { st.Unreachable = true })
			return func() { e.srv.Do(func(st *ldaptest.State) { st.Unreachable = false }) }, nil
		},
	}
	if err := sdk.ValidateProvider(e.p); err != nil {
		t.Fatalf("ValidateProvider: %v", err)
	}
	ct.RunAll(t)
}

func TestCreateFromEmptyAndNoDrift(t *testing.T) {
	e := newEnv(t, nil)
	pl := e.converge(t, peopleID, people())
	if len(pl.Ops) != 1 || pl.Ops[0].Kind != core.OpKindCreate {
		t.Fatalf("people plan = %+v", pl.Ops)
	}
	e.converge(t, aliceID, alice())
	e.converge(t, groupID, group())
	e.srv.Do(func(st *ldaptest.State) {
		p := st.Find("ou=people," + suffix)
		if p.DN != "ou=People,dc=example,dc=com" {
			t.Errorf("the entry was created as %q; want the leaf RDN spelt as declared (ou=People)", p.DN)
		}
		a := st.Find("uid=alice,ou=people," + suffix)
		if got := strings.Join(a.Get("objectClass"), ","); got != "inetOrgPerson,top,organizationalperson,person" {
			t.Errorf("alice objectClass = %q (the fake adds the superior classes)", got)
		}
		pw := a.Get("userPassword")
		if len(pw) != 1 || pw[0] != ldaptest.HashPassword(alicePW) {
			t.Errorf("alice userPassword = %q", pw)
		}
		if !strings.HasPrefix(a.DN, "uid=Alice,") {
			t.Errorf("alice DN = %q", a.DN)
		}
	})
	// 何周しても drift が出ない（上位のクラス・DN の値の空白・userPassword）。
	for i := 0; i < 3; i++ {
		for local, f := range map[string]map[string]string{peopleID: people(), aliceID: alice(), groupID: group()} {
			if pl := e.plan(t, local, f); len(pl.Ops) != 0 {
				t.Fatalf("cycle %d: %s planned %+v", i, local, pl.Ops)
			}
		}
	}
	// 観測にパスワード（ハッシュも）を入れない。
	obs, _ := e.p.Read(context.Background(), aliceID)
	if got := fmt.Sprint(obs.State[ldap.FieldUserPassword].Value); got != ldap.PasswordSet {
		t.Errorf("observed userpassword = %q", got)
	}
	if strings.Contains(fmt.Sprint(obs.State), "FAKE") {
		t.Errorf("the observation shows the stored password: %v", obs.State)
	}
}

func TestDriftComesBack(t *testing.T) {
	e := newEnv(t, nil)
	e.converge(t, peopleID, people())
	e.converge(t, aliceID, alice())
	e.srv.Do(func(st *ldaptest.State) {
		a := st.Find("uid=alice,ou=people," + suffix)
		a.Set("mail", []string{"evil@example.com"})
		a.Set("sn", nil)
		a.Set("objectClass", append(a.Get("objectClass"), "posixAccount"))
		a.Set("telephoneNumber", []string{"123"}) // 宣言していない属性は触らない。
	})
	pl := e.plan(t, aliceID, alice())
	if len(pl.Ops) != 1 || pl.Ops[0].Kind != core.OpKindUpdate {
		t.Fatalf("plan = %+v", pl.Ops)
	}
	var fields []string
	for _, f := range pl.Ops[0].Fields {
		fields = append(fields, f.Field)
	}
	if strings.Join(fields, ",") != "mail,objectClass,sn" {
		t.Errorf("drifted fields = %v", fields)
	}
	if !strings.Contains(pl.Ops[0].Diagnostic, "evil@example.com") || !strings.Contains(pl.Ops[0].Diagnostic, "sn: on the server (none)") {
		t.Errorf("diagnostic = %q", pl.Ops[0].Diagnostic)
	}
	e.converge(t, aliceID, alice())
	e.srv.Do(func(st *ldaptest.State) {
		a := st.Find("uid=alice,ou=people," + suffix)
		if got := a.Get("telephoneNumber"); len(got) != 1 {
			t.Errorf("an undeclared attribute was touched: %q", got)
		}
		for _, c := range a.Get("objectClass") {
			if strings.EqualFold(c, "posixAccount") {
				t.Error("the class added by hand is still there")
			}
		}
	})
}

func TestUserPasswordWriteOnly(t *testing.T) {
	e := newEnv(t, nil)
	e.converge(t, peopleID, people())
	e.converge(t, aliceID, alice())
	// 新しい世代を宣言しても書き直さない（観測からは値が分からない）。
	f := alice()
	f["userPassword"] = "secretref:file/ldap/alice2#gen2"
	if pl := e.plan(t, aliceID, f); len(pl.Ops) != 0 {
		t.Fatalf("a new generation planned %+v", pl.Ops)
	}
	// 手で消すと、次の周に今の値を書く。
	e.srv.Do(func(st *ldaptest.State) { st.Find("uid=alice,ou=people,"+suffix).Set("userPassword", nil) })
	pl := e.converge(t, aliceID, f)
	if len(pl.Ops) != 1 || len(pl.Ops[0].Fields) != 1 || pl.Ops[0].Fields[0].Field != "userPassword" {
		t.Fatalf("plan = %+v", pl.Ops)
	}
	if strings.Contains(fmt.Sprint(pl.Ops), alice2PW) {
		t.Error("the plan shows the password")
	}
	e.srv.Do(func(st *ldaptest.State) {
		if got := st.Find("uid=alice,ou=people," + suffix).Get("userPassword"); len(got) != 1 || got[0] != ldaptest.HashPassword(alice2PW) {
			t.Errorf("userPassword = %q", got)
		}
	})
	// パスワード（bind の・userPassword の）は引数・環境変数に出さない。-y で渡す。
	for _, c := range e.srv.Calls() {
		all := strings.Join(c.Args, " ") + fmt.Sprint(c.Env)
		for _, s := range []string{dmPW, alicePW, alice2PW} {
			if strings.Contains(all, s) {
				t.Fatalf("a password in the arguments or the environment: %v", c.Args)
			}
		}
		if c.Password != dmPW {
			t.Errorf("-y file = %q; want the bind password", c.Password)
		}
		if c.Env["LDAPTLS_REQCERT"] != "demand" || !strings.Contains(strings.Join(c.Args, " "), "-ZZ") {
			t.Errorf("TLS is not demanded: %v %v", c.Args, c.Env)
		}
	}
}

func TestCaseInsensitiveNamesAndCreateExisting(t *testing.T) {
	e := newEnv(t, nil)
	e.srv.Do(func(st *ldaptest.State) {
		st.Put("OU=People, dc=example,dc=com", map[string][]string{"OBJECTCLASS": {"organizationalUnit", "top"}, "OU": {"People"}, "Description": {"old"}})
	})
	f := map[string]string{"objectclass": `["organizationalUnit"]`, "ou": "People", "description": "new"}
	pl := e.converge(t, peopleID, f)
	if len(pl.Ops) != 1 || len(pl.Ops[0].Fields) != 1 || pl.Ops[0].Fields[0].Field != "description" {
		t.Fatalf("plan = %+v", pl.Ops)
	}
	// Create の時に在っても宣言にそろえる（別の誰かが先に作った）。
	op := core.Op{Kind: core.OpKindCreate, Target: id(peopleID), Fields: []core.FieldDiff{{Field: "objectClass", To: "organizationalUnit"}, {Field: "ou", To: "People"}, {Field: "description", To: "again"}}}
	if err := e.p.Apply(context.Background(), core.Plan{Ops: []core.Op{op}}); err != nil {
		t.Fatalf("Create over an existing entry: %v", err)
	}
	e.srv.Do(func(st *ldaptest.State) {
		if got := st.Find("ou=people," + suffix).Get("description"); len(got) != 1 || got[0] != "again" {
			t.Errorf("description = %q", got)
		}
	})
	// 親が無い項目の Create は一時的な失敗（親を先に作れば作れる）。
	op = core.Op{Kind: core.OpKindCreate, Target: id("dir1/uid=x,ou=nowhere,dc=example,dc=com"), Fields: []core.FieldDiff{{Field: "objectClass", To: "account"}}}
	err := e.p.Apply(context.Background(), core.Plan{Ops: []core.Op{op}})
	var te *provider.TransientError
	if !errors.As(err, &te) || !strings.Contains(err.Error(), "parent") {
		t.Errorf("Create without the parent: %v; want a TransientError", err)
	}
}

func TestListBoundaryAndPrune(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, func(s *ldap.Server) { s.Filter = "(!(objectClass=posixAccount))" })
	e.srv.Do(func(st *ldaptest.State) {
		st.Put("ou=People,"+suffix, map[string][]string{"objectClass": {"top", "organizationalUnit"}, "ou": {"People"}})
		st.Put("uid=bob,ou=People,"+suffix, map[string][]string{"objectClass": {"top", "account"}, "uid": {"bob"}})
		st.Put("ou=Leaf,"+suffix, map[string][]string{"objectClass": {"top", "organizationalUnit"}, "ou": {"Leaf"}})
		st.Put("uid=posix,"+suffix, map[string][]string{"objectClass": {"top", "posixAccount"}, "uid": {"posix"}})
		st.Put("cn=sub,"+suffix, map[string][]string{"objectClass": {"top", "ldapSubEntry"}, "cn": {"sub"}})
		st.Put("ou=other,dc=example,dc=org", map[string][]string{"objectClass": {"top", "organizationalUnit"}})
		st.Put(bindDN+","+suffix, map[string][]string{"objectClass": {"top", "person"}})
	})
	e2, err := ldap.New(map[string]ldap.Server{"dir1": server(func(s *ldap.Server) {
		s.Filter = "(!(objectClass=posixAccount))"
		s.BindDN = bindDN + "," + suffix
	})}, ldap.Options{Runner: ldap.RunnerFunc(e.srv.Run), Secrets: e.sec})
	if err != nil {
		t.Fatal(err)
	}
	e.srv.Do(func(st *ldaptest.State) { st.BindDN = bindDN + "," + suffix })
	res, err := e2.List(ctx, core.NewScope(nil))
	if err != nil || res.Status != core.ListComplete {
		t.Fatalf("List = %+v, %v", res, err)
	}
	var got []string
	for _, x := range res.IDs {
		got = append(got, x.Local)
	}
	want := "dir1/ou=leaf,dc=example,dc=com dir1/ou=people,dc=example,dc=com dir1/uid=bob,ou=people,dc=example,dc=com"
	if strings.Join(got, " ") != want {
		t.Fatalf("List = %v; want %s (no base, no bind DN, no subentry, only the filter)", got, want)
	}
	// prune：子のある項目は消さない（PermanentError）、葉は消す（破壊的）。
	desired := []diff.DesiredResource{{ID: id("dir1/uid=bob,ou=people,dc=example,dc=com"), Fields: map[string]string{"objectClass": "account"}, DeletionPolicy: core.PruneWithinScope}}
	pr, err := prune.New().ComputePrune(ctx, e2, e2, core.NewScope(nil), core.NewScope(nil), desired, nil, nil)
	if err != nil || pr.Halted {
		t.Fatalf("ComputePrune = %+v, %v", pr, err)
	}
	var targets []string
	for _, op := range pr.DeleteOps {
		targets = append(targets, op.Target.Local)
		if !op.Destructive {
			t.Errorf("Delete of %s is not destructive", op.Target.Local)
		}
		err := e2.Apply(ctx, core.Plan{Ops: []core.Op{op}})
		var pe *provider.PermanentError
		switch op.Target.Local {
		case "dir1/ou=people,dc=example,dc=com":
			if !errors.As(err, &pe) || !strings.Contains(err.Error(), "child entries") {
				t.Errorf("Delete of the entry with a child: %v; want a PermanentError", err)
			}
		default:
			if err != nil {
				t.Errorf("Delete of %s: %v", op.Target.Local, err)
			}
		}
	}
	if strings.Join(targets, " ") != "dir1/ou=leaf,dc=example,dc=com dir1/ou=people,dc=example,dc=com" {
		t.Errorf("DeleteOps = %v", targets)
	}
	e.srv.Do(func(st *ldaptest.State) {
		if st.Find("ou=people,"+suffix) == nil || st.Find("uid=bob,ou=people,"+suffix) == nil {
			t.Error("the entry with a child (or its child) was deleted")
		}
		if st.Find("ou=leaf,"+suffix) != nil {
			t.Error("the leaf orphan is still there")
		}
		if st.Find(suffix) == nil || st.Find("uid=posix,"+suffix) == nil || st.Find("cn=sub,"+suffix) == nil {
			t.Error("something outside the boundary was deleted")
		}
	})
	// 消えた項目の Delete は何もしない。
	if err := e2.Apply(ctx, core.Plan{Ops: []core.Op{{Kind: core.OpKindDelete, Target: id("dir1/ou=leaf,dc=example,dc=com")}}}); err != nil {
		t.Errorf("Delete of a gone entry: %v", err)
	}
}

func TestUnreachableAndBindFailureAreUnknown(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.converge(t, peopleID, people())
	for name, f := range map[string]func(st *ldaptest.State){
		"unreachable": func(st *ldaptest.State) { st.Unreachable = true },
		"bind":        func(st *ldaptest.State) { st.BindPassword = "changed" },
		"tls":         func(st *ldaptest.State) { st.RequireTLS = true },
	} {
		e2 := e
		if name == "tls" {
			e2 = &env{srv: e.srv, sec: e.sec}
			var err error
			e2.p, err = ldap.New(map[string]ldap.Server{"dir1": server(func(s *ldap.Server) { s.StartTLS = false })}, ldap.Options{Runner: ldap.RunnerFunc(e.srv.Run), Secrets: e.sec})
			if err != nil {
				t.Fatal(err)
			}
		}
		var saved ldaptest.State
		e.srv.Do(func(st *ldaptest.State) { saved = *st; f(st) })
		obs, err := e2.p.Read(ctx, peopleID)
		if err == nil || obs.Availability != core.AvailabilityUnknown {
			t.Errorf("%s: Read = %+v, %v; want Unknown", name, obs, err)
		}
		if strings.Contains(fmt.Sprint(err), dmPW) {
			t.Errorf("%s: the error shows the password", name)
		}
		if _, err := e2.p.List(ctx, core.NewScope(nil)); err == nil {
			t.Errorf("%s: List succeeded", name)
		}
		if _, err := e2.p.BulkObserve(ctx, core.NewScope(nil)); err == nil {
			t.Errorf("%s: BulkObserve succeeded", name)
		}
		e.srv.Do(func(st *ldaptest.State) { *st = saved })
	}
	// 届かないのは一時的、bind の失敗は恒久的。
	e.srv.Do(func(st *ldaptest.State) { st.Unreachable = true })
	_, err := e.p.Read(ctx, peopleID)
	var te *provider.TransientError
	if !errors.As(err, &te) {
		t.Errorf("unreachable: %v; want a TransientError", err)
	}
	e.srv.Do(func(st *ldaptest.State) { st.Unreachable = false })
	// コマンドが無い。
	e.srv.Missing = true
	if obs, err := e.p.Read(ctx, peopleID); err == nil || obs.Availability != core.AvailabilityUnknown {
		t.Errorf("no command: Read = %+v, %v", obs, err)
	}
	e.srv.Missing = false
	// 秘密が読めない。
	delete(e.sec, "ldap/dm")
	if obs, err := e.p.Read(ctx, peopleID); err == nil || obs.Availability != core.AvailabilityUnknown {
		t.Errorf("no secret: Read = %+v, %v", obs, err)
	}
}

func TestBulkObserveEqualsRead(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.converge(t, peopleID, people())
	e.converge(t, aliceID, alice())
	e.converge(t, groupID, group())
	e.srv.Do(func(st *ldaptest.State) { st.Fold = true }) // 折り返した出力も読める。
	bulk, err := e.p.BulkObserve(ctx, core.NewScope(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(bulk.Observations) != 3 {
		t.Fatalf("BulkObserve = %d entries", len(bulk.Observations))
	}
	for _, local := range []string{peopleID, aliceID, groupID} {
		r, err := e.p.Read(ctx, local)
		if err != nil {
			t.Fatal(err)
		}
		b, ok := bulk.Observations[id(local).Key()]
		if !ok || fmt.Sprint(b.State) != fmt.Sprint(r.State) {
			t.Errorf("%s: BulkObserve %v != Read %v", local, b.State, r.State)
		}
	}
}

func TestValidateIntent(t *testing.T) {
	e := newEnv(t, nil)
	with := func(k, v string) map[string]string {
		f := alice()
		if v == "\x00drop" {
			delete(f, k)
		} else {
			f[k] = v
		}
		return f
	}
	cases := []struct {
		id     string
		fields map[string]string
		want   string
	}{
		{"dir1/uid=Alice,ou=people,dc=example,dc=com", alice(), `canonical form "dir1/uid=alice,ou=people,dc=example,dc=com"`},
		{"dir1/uid=alice, ou=people,dc=example,dc=com", alice(), "canonical form"},
		{"dir1/dc=example,dc=com", alice(), "not below base_dn"},
		{"dir1/ou=x,dc=other,dc=com", alice(), "not below base_dn"},
		{"dir2/uid=alice,ou=people,dc=example,dc=com", alice(), "no server"},
		{"dir1", alice(), "want <server>/<dn>"},
		{aliceID, with("userPassword", "hunter2"), "must be a SecretRef"},
		{aliceID, with("objectClass", "\x00drop"), "objectClass is required"},
		{aliceID, with("objectClass", "[]"), "must not be empty"},
		{aliceID, with("uid", "bob"), "must contain the value of the RDN"},
		{aliceID, with("UID", "alice"), "name the same attribute"},
		{aliceID, with("createTimestamp", "x"), "operational"},
		{aliceID, with("cn;lang-ja", "x"), "not an attribute name"},
		{aliceID, with("mail", "[bad"), "JSON array"},
		{aliceID, with("mail", `["a",""]`), "empty value"},
		{aliceID, with("implied_objectclass", "top"), "observed only"},
		{aliceID, with("dn", "uid=bob,ou=people,dc=example,dc=com"), "dn must be the DN"},
		{aliceID, with("server", "dir2"), "must equal the server"},
		{aliceID, with("Server", "dir1"), "lower case"},
	}
	for _, c := range cases {
		err := e.p.ValidateIntent(c.id, c.fields)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("ValidateIntent(%q) = %v; want %q", c.id, err, c.want)
		}
		if err != nil && strings.Contains(err.Error(), "hunter2") {
			t.Errorf("the error shows the password: %v", err)
		}
	}
	if err := e.p.ValidateIntent(aliceID, with("dn", "UID=Alice, ou=People,dc=example,dc=com")); err != nil {
		t.Errorf("dn written differently: %v", err)
	}
	e2, err := ldap.New(map[string]ldap.Server{"dir1": server(func(s *ldap.Server) { s.BindDN = "uid=admin,dc=example,dc=com" })}, ldap.Options{Runner: ldap.RunnerFunc(e.srv.Run), Secrets: e.sec})
	if err != nil {
		t.Fatal(err)
	}
	if err := e2.ValidateIntent("dir1/uid=admin,dc=example,dc=com", map[string]string{"objectClass": "account"}); err == nil || !strings.Contains(err.Error(), "bind_dn") {
		t.Errorf("the bind DN: %v", err)
	}
}

// TestExecRunner は本物の exec の経路（-y の一時ファイル・終了コード）を、この
// テストのバイナリを偽の ldap コマンドとして起動して確かめる。
func TestExecRunner(t *testing.T) {
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	search, modify := filepath.Join(dir, "ldapsearch"), filepath.Join(dir, "ldapmodify")
	for _, p := range []string{search, modify} {
		if err := os.Symlink(exe, p); err != nil {
			t.Fatal(err)
		}
	}
	state := filepath.Join(dir, "state.json")
	if err := ldaptest.SaveState(state, ldaptest.NewState(bindDN, dmPW, suffix)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SAKUTEI_FAKE_LDAP_MAIN", "1")
	t.Setenv("SAKUTEI_FAKE_LDAP_STATE", state)
	t.Setenv("LDAPTLS_REQCERT", "never") // 親の設定は引き継がない。
	p, err := ldap.New(map[string]ldap.Server{"dir1": server(nil)}, ldap.Options{LDAPSearch: search, LDAPModify: modify, Secrets: secrets{"ldap/dm": dmPW, "ldap/alice": alicePW}})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{p: p}
	e.converge(t, peopleID, people())
	e.converge(t, aliceID, alice())
	st, err := ldaptest.LoadState(state)
	if err != nil {
		t.Fatal(err)
	}
	if a := st.Find("uid=alice,ou=people," + suffix); a == nil || a.Get("userPassword")[0] != ldaptest.HashPassword(alicePW) {
		t.Fatalf("alice = %+v", a)
	}
	left, _ := filepath.Glob(filepath.Join(os.TempDir(), "sakutei-ldap-*"))
	for _, f := range left {
		if info, err := os.Stat(f); err == nil && time.Since(info.ModTime()) < time.Minute {
			t.Errorf("a temporary password file was left: %s", f)
		}
	}
	// コマンドが無い。
	p2, _ := ldap.New(map[string]ldap.Server{"dir1": server(nil)}, ldap.Options{LDAPSearch: filepath.Join(dir, "nope"), Secrets: secrets{"ldap/dm": dmPW}})
	if obs, err := p2.Read(context.Background(), peopleID); err == nil || obs.Availability != core.AvailabilityUnknown || ldap.KindOf(err) != ldap.KindNoCommand {
		t.Errorf("no command: %+v, %v", obs, err)
	}
}
