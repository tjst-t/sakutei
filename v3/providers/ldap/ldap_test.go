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
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/providers/ldap"
	"github.com/tjst-t/sakutei/v3/providers/ldap/ldapfake"
	"github.com/tjst-t/sakutei/v3/sdk/contract"
)

func TestMain(m *testing.M) {
	if os.Getenv("SAKUTEI_FAKE_LDAP_MAIN") == "1" {
		ldapfake.Main()
	}
	os.Exit(m.Run())
}

const (
	dmDN     = "cn=Directory Manager"
	dmPW     = "dm-pw:with space\\"
	readerDN = "uid=sakutei-read,dc=example,dc=com"
	readerPW = "reader-pw-1"
	alicePW  = "alice-secret-1"
	alice2PW = "alice-secret-2"
	suffix   = "dc=example,dc=com"
	peopleDN = "ou=people," + suffix
	aliceDN  = "uid=alice,ou=people," + suffix
	adminsDN = "cn=admins,ou=people," + suffix
)

// secrets は secret の参照先 → 値（WriteContext の取り出し先）。
type secrets map[string]string

func (s secrets) Resolve(_ context.Context, ref core.SecretRef) (string, error) {
	v, ok := s[ref.Path]
	if !ok {
		return "", fmt.Errorf("secret %s が無い", ref.Ref())
	}
	return v, nil
}

type env struct {
	srv *ldapfake.Server
	p   *ldap.Provider
	dir string
	sec secrets
}

// newEnv は偽のサーバと provider を作る。観測は readerDN、書き込みは Directory
// Manager で bind する（E-G5）。mod で settings.servers.dir1 を変える。
func newEnv(t *testing.T, mod func(srv map[string]any)) *env {
	t.Helper()
	st := ldapfake.NewState(dmDN, dmPW, suffix)
	st.Readers = map[string]string{readerDN: readerPW}
	st.Put(readerDN, map[string][]string{"objectClass": {"top", "account"}, "uid": {"sakutei-read"}})
	srv := ldapfake.New(st)
	dir := t.TempDir()
	for name, pw := range map[string]string{"dm": dmPW, "reader": readerPW} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(pw+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := map[string]any{
		"uri":     "ldap://ldap.test",
		"timeout": "5s",
		"observe": map[string]any{"bind_dn": readerDN, "password_file": filepath.Join(dir, "reader")},
		"write":   map[string]any{"bind_dn": dmDN, "password_file": filepath.Join(dir, "dm")},
	}
	if mod != nil {
		mod(s)
	}
	p, err := ldap.New(map[string]any{"servers": map[string]any{"dir1": s}}, ldap.Options{Runner: ldap.RunnerFunc(srv.Run)})
	if err != nil {
		t.Fatal(err)
	}
	return &env{srv: srv, p: p, dir: dir, sec: secrets{"ldap/alice": alicePW, "ldap/alice2": alice2PW}}
}

func id(dn string) core.ID { return core.ID{Type: "dir/" + ldap.KindEntry, Name: "dir1/" + dn} }

func l(s ...string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// rng は suffix の下の Range（E-I6）。
func rng(extra ...string) core.Range {
	args := map[string]string{"server": "dir1", "base_dn": suffix}
	for i := 0; i+1 < len(extra); i += 2 {
		args[extra[i]] = extra[i+1]
	}
	return core.Range{Provider: "dir", Args: args}
}

func res(dn string, attrs map[string]any, pw string) core.Resource {
	f := core.Fields{ldap.FieldAttributes: attrs}
	if pw != "" {
		f[ldap.FieldUserPassword] = pw
	}
	return core.Resource{ID: id(dn), Fields: f}
}

func people() core.Resource {
	return res(peopleDN, map[string]any{"objectClass": l("top", "organizationalUnit"), "ou": "People", "description": "the people"}, "")
}

func alice(pw string) core.Resource {
	return res(aliceDN, map[string]any{
		"objectClass": l("top", "inetOrgPerson"), "uid": "Alice", "cn": "Alice Liddell", "sn": "Liddell",
		"mail": l("alice@example.com", "al@example.com"),
	}, pw)
}

func admins() core.Resource {
	return res(adminsDN, map[string]any{"objectClass": "groupOfNames", "cn": "admins", "member": "UID=Alice, OU=People,dc=example,dc=com"}, "")
}

// writes は ldapmodify の起動の数。
func (e *env) writes() int {
	n := 0
	for _, c := range e.srv.Calls() {
		if c.Command == "ldapmodify" {
			n++
		}
	}
	return n
}

// boundAs は bind の DN（正規形）ごとの起動の数。
func (e *env) boundAs(dn string) int {
	c, _ := ldap.CanonicalDN(dn)
	n := 0
	for _, m := range e.srv.BindCalls()[c] {
		n += m
	}
	return n
}

func (e *env) validate(t *testing.T, rs []core.Resource, ranges ...core.Range) {
	t.Helper()
	if len(ranges) == 0 {
		ranges = []core.Range{rng()}
	}
	for _, d := range e.p.Validate(rs, ranges) {
		t.Fatalf("Validate: %s", d)
	}
}

func (e *env) observe(t *testing.T, i core.ID) core.Observation {
	t.Helper()
	obs, err := e.p.Observe(context.Background(), []core.ID{i})
	if err != nil {
		t.Fatal(err)
	}
	return obs[i]
}

// compare は観測して宣言と比べる（コアの plan と同じ比べ方）。
func (e *env) compare(t *testing.T, r core.Resource) (plan.Change, core.Observation) {
	t.Helper()
	o := e.observe(t, r.ID)
	return plan.Compare(r, e.p.Schema().Kinds[ldap.KindEntry], o), o
}

// converge は宣言を（無ければ作り、ずれていれば）Apply し、読み直して宣言どおりかを
// 確かめる。Apply したときの op を返す。
func (e *env) converge(t *testing.T, all []core.Resource, r core.Resource) plan.Op {
	t.Helper()
	e.validate(t, all)
	c, o := e.compare(t, r)
	if c.Op == plan.Create || c.Op == plan.Update {
		if err := e.p.Apply(context.Background(), core.NewWriteContext(e.sec), r.ID, r.Fields, o); err != nil {
			t.Fatalf("Apply(%s): %v", r.ID, err)
		}
	}
	if again, _ := e.compare(t, r); again.Op != plan.None {
		t.Fatalf("%s: Apply したあとも宣言どおりでない: %s %+v %s", r.ID, again.Op, again.Fields, again.Note)
	}
	return c.Op
}

func TestContract(t *testing.T) {
	contract.Run(t, func(t *testing.T) contract.Target {
		e := newEnv(t, nil)
		e.srv.Do(func(st *ldapfake.State) {
			st.Put(peopleDN, map[string][]string{"objectClass": {"top", "organizationalUnit"}, "ou": {"people"}})
		})
		return contract.Target{
			Provider: e.p,
			Ranges:   []core.Range{rng()},
			Absent:   []core.ID{id("uid=nobody,ou=people," + suffix)},
			Create:   []core.Resource{alice("test:ldap/alice@gen=1")},
			Observed: []core.ID{id(peopleDN)},
			Faults: map[string]func(){
				"届かない":                func() { e.srv.Do(func(st *ldapfake.State) { st.Unreachable = true }) },
				"bind の失敗":            func() { e.srv.Do(func(st *ldapfake.State) { st.Readers[readerDN] = "changed" }) },
				"TLS を求められる":          func() { e.srv.Do(func(st *ldapfake.State) { st.RequireTLS = true }) },
				"読めない出力":              func() { e.srv.Do(func(st *ldapfake.State) { st.Broken = true }) },
				"コマンドが無い":             func() { e.srv.Missing = true },
				"credential のファイルが無い": func() { os.Remove(filepath.Join(e.dir, "reader")) },
			},
			Truncate:             func() { e.srv.Do(func(st *ldapfake.State) { st.SizeLimit = 1 }) },
			Writes:               e.writes,
			Secrets:              []string{dmPW, readerPW, alicePW},
			WriteCredentialReads: func() int { return e.boundAs(dmDN) },
			Refuses: []contract.Refuse{{ID: id(peopleDN), Prepare: func() {
				e.srv.Do(func(st *ldapfake.State) {
					st.Put("uid=child,"+peopleDN, map[string][]string{"objectClass": {"top", "account"}, "uid": {"child"}})
				})
			}}},
			Secret: e.sec,
		}
	})
}

// 空から作り、何 cycle 回しても書かない（E-G1・E-B12）。
func TestCreateFromEmptyAndNoDrift(t *testing.T) {
	e := newEnv(t, nil)
	all := []core.Resource{people(), alice("test:ldap/alice@gen=1"), admins()}
	for _, r := range all {
		if op := e.converge(t, all, r); op != plan.Create {
			t.Fatalf("%s: %s（want create）", r.ID, op)
		}
	}
	e.srv.Do(func(st *ldapfake.State) {
		a := st.Find(aliceDN)
		if a == nil || a.DN != "uid=Alice,ou=people,dc=example,dc=com" {
			t.Fatalf("葉の RDN は宣言の綴り: %+v", a)
		}
		if got := strings.Join(sortedLower(a.Get("objectClass")), ","); got != "inetorgperson,organizationalperson,person,top" {
			t.Errorf("objectClass = %s", got)
		}
		if pw := a.Get("userPassword"); len(pw) != 1 || pw[0] != ldapfake.HashPassword(alicePW) {
			t.Errorf("userPassword = %q", pw)
		}
		if m := st.Find(adminsDN).Get("member"); len(m) != 1 || m[0] != "uid=Alice,ou=People,dc=example,dc=com" {
			t.Errorf("member = %q", m)
		}
	})
	n := e.writes()
	for i := 0; i < 3; i++ {
		for _, r := range all {
			if op := e.converge(t, all, r); op != plan.None {
				t.Fatalf("%d cycle 目 %s: %s", i, r.ID, op)
			}
		}
	}
	if e.writes() != n {
		t.Errorf("宣言どおりなのに %d 回書いた", e.writes()-n)
	}
	// secret の値は ldapmodify の標準入力（base64）にだけ渡り、引数・環境変数に出ない。
	for _, c := range e.srv.Calls() {
		joined := strings.Join(c.Args, " ") + fmt.Sprint(c.Env)
		for _, s := range []string{dmPW, readerPW, alicePW} {
			if strings.Contains(joined, s) || strings.Contains(c.Stdin, s) {
				t.Fatalf("secret が引数・環境変数・平文の LDIF に出た: %v", c.Args)
			}
		}
	}
}

func sortedLower(v []string) []string {
	out := make([]string, len(v))
	for i, s := range v {
		out[i] = strings.ToLower(s)
	}
	sort.Strings(out)
	return out
}

// 手で変えた属性はずれになり、宣言した属性だけを戻す（E-G1・E-D7）。
func TestDriftComesBack(t *testing.T) {
	e := newEnv(t, nil)
	all := []core.Resource{people(), alice("test:ldap/alice@gen=1"), admins()}
	for _, r := range all {
		e.converge(t, all, r)
	}
	e.srv.Do(func(st *ldapfake.State) {
		a := st.Find(aliceDN)
		a.Set("mail", []string{"evil@example.com"})
		a.Set("telephoneNumber", []string{"123"})
		st.Find(adminsDN).Set("member", []string{"uid=bob,ou=people,dc=example,dc=com"})
		p := st.Find(peopleDN)
		p.Set("objectClass", append(p.Get("objectClass"), "extensibleObject"))
	})
	e.validate(t, all)
	c, _ := e.compare(t, alice("test:ldap/alice@gen=1"))
	if c.Op != plan.Update || len(c.Fields) != 1 {
		t.Fatalf("alice = %+v", c)
	}
	got := c.Fields[0].Observed.(map[string]any)
	if got["mail"] != "evil@example.com" || got["cn"] != "Alice Liddell" || got["telephoneNumber"] != nil {
		t.Errorf("観測（宣言した属性だけ。同じものは宣言の書き方）= %v", got)
	}
	for _, r := range all {
		e.converge(t, all, r)
	}
	e.srv.Do(func(st *ldapfake.State) {
		a := st.Find(aliceDN)
		if got := strings.Join(sortedLower(a.Get("mail")), ","); got != "al@example.com,alice@example.com" {
			t.Errorf("mail = %s", got)
		}
		if got := a.Get("telephoneNumber"); len(got) != 1 {
			t.Errorf("宣言に無い属性を触った: %q", got)
		}
		if got := sortedLower(st.Find(peopleDN).Get("objectClass")); strings.Contains(strings.Join(got, ","), "extensibleobject") {
			t.Errorf("objectClass = %q", got)
		}
	})
	// 宣言と違う属性だけを書く。
	var last string
	dn := base64.StdEncoding.EncodeToString([]byte(aliceDN))
	for _, c := range e.srv.Calls() {
		if c.Command == "ldapmodify" && strings.Contains(c.Stdin, dn) {
			last = c.Stdin
		}
	}
	if strings.Count(last, "replace:") != 1 || !strings.Contains(last, "replace: mail") {
		t.Errorf("alice の最後の変更 = %q", last)
	}
}

// userPassword は書き込みだけ：在れば適合、世代を上げても書き直さず、消せば書く
// （E-G1・E-G2）。
func TestUserPasswordWriteOnly(t *testing.T) {
	e := newEnv(t, nil)
	all := []core.Resource{people(), alice("test:ldap/alice@gen=1")}
	for _, r := range all {
		e.converge(t, all, r)
	}
	gen2 := []core.Resource{people(), alice("test:ldap/alice2@gen=2")}
	e.validate(t, gen2)
	if c, o := e.compare(t, gen2[1]); c.Op != plan.None || o.Fields[ldap.FieldUserPassword] != "test:ldap/alice2@gen=2" || o.Fields[ldap.FieldUserPasswordSet] != true {
		t.Fatalf("新しい世代: %+v %v", c, o.Fields)
	}
	e.srv.Do(func(st *ldapfake.State) { st.Find(aliceDN).Set("userPassword", nil) })
	c, o := e.compare(t, gen2[1])
	if c.Op != plan.Update || len(c.Fields) != 1 || c.Fields[0].Name != ldap.FieldUserPassword || !c.Fields[0].Missing || o.Fields[ldap.FieldUserPasswordSet] != false {
		t.Fatalf("消したあと: %+v", c)
	}
	n := e.writes()
	e.converge(t, gen2, gen2[1])
	if e.writes() != n+1 {
		t.Errorf("書いた数 %d（want 1）", e.writes()-n)
	}
	e.srv.Do(func(st *ldapfake.State) {
		if pw := st.Find(aliceDN).Get("userPassword"); len(pw) != 1 || pw[0] != ldapfake.HashPassword(alice2PW) {
			t.Errorf("userPassword = %q", pw)
		}
	})
	// 取り出せない secret はエラーで、値を出さない。
	e.srv.Do(func(st *ldapfake.State) { st.Find(aliceDN).Set("userPassword", nil) })
	missing := alice("test:ldap/none@gen=1")
	e.validate(t, []core.Resource{people(), missing})
	_, o = e.compare(t, missing)
	if err := e.p.Apply(context.Background(), core.NewWriteContext(e.sec), missing.ID, missing.Fields, o); err == nil || !strings.Contains(err.Error(), "user_password") {
		t.Errorf("取り出せない secret: %v", err)
	}
}

// 属性の名前は大文字小文字を問わず、既に在る entry を作る Apply は宣言にそろえる。
func TestCaseInsensitiveNamesAndCreateExisting(t *testing.T) {
	e := newEnv(t, nil)
	e.srv.Do(func(st *ldapfake.State) {
		st.Put(peopleDN, map[string][]string{"objectclass": {"top", "organizationalUnit"}, "OU": {"People"}, "Description": {"old"}})
	})
	r := people()
	e.validate(t, []core.Resource{r})
	c, o := e.compare(t, r)
	if c.Op != plan.Update || len(c.Fields) != 1 {
		t.Fatalf("%+v", c)
	}
	if got := c.Fields[0].Observed.(map[string]any); got["description"] != "old" || got["ou"] != "People" || len(got) != 3 {
		t.Errorf("観測（宣言の綴りの名前）= %v", got)
	}
	// 作る Apply（観測が古く Absent のまま）でも既に在ればそろえる。
	if err := e.p.Apply(context.Background(), core.NewWriteContext(e.sec), r.ID, r.Fields, core.Observation{Availability: core.Absent}); err != nil {
		t.Fatal(err)
	}
	if again, _ := e.compare(t, r); again.Op != plan.None {
		t.Fatalf("%+v %v", again, o)
	}
	e.srv.Do(func(st *ldapfake.State) {
		if got := st.Find(peopleDN).Get("description"); len(got) != 1 || got[0] != "the people" {
			t.Errorf("description = %q", got)
		}
	})
}

// List は Range の中だけ：base_dn そのもの・bind の DN・サブエントリ・filter の外・
// scope の外を出さない（E-I6・E-G1・E-K2）。
func TestList(t *testing.T) {
	e := newEnv(t, nil)
	e.srv.Do(func(st *ldapfake.State) {
		for dn, oc := range map[string]string{
			peopleDN:                 "organizationalUnit",
			"uid=bob," + peopleDN:    "account",
			"ou=outside," + suffix:   "organizationalUnit",
			"cn=sub," + suffix:       "ldapSubEntry",
			"cn=other,dc=other,dc=c": "organizationalUnit",
		} {
			k := strings.SplitN(strings.SplitN(dn, ",", 2)[0], "=", 2)
			st.Put(dn, map[string][]string{"objectClass": {"top", oc}, k[0]: {k[1]}})
		}
	})
	list := func(r core.Range) []string {
		t.Helper()
		lst, err := e.p.List(context.Background(), r)
		if err != nil || !lst.Complete {
			t.Fatalf("List: %v %v", lst, err)
		}
		var out []string
		for _, i := range lst.IDs {
			if i.Type != "dir/entry" {
				t.Errorf("type = %s", i.Type)
			}
			out = append(out, strings.TrimPrefix(i.Name, "dir1/"))
		}
		return out
	}
	if got := strings.Join(list(rng("filter", "(!(ou=outside))")), " "); got != "ou=people,dc=example,dc=com uid=bob,ou=people,dc=example,dc=com" {
		t.Errorf("sub = %s", got)
	}
	if got := strings.Join(list(rng("scope", "one")), " "); got != "ou=outside,dc=example,dc=com ou=people,dc=example,dc=com" {
		t.Errorf("one = %s", got)
	}
	// 書き込み用の DN も List に出ない。
	e2 := newEnv(t, func(s map[string]any) {
		s["write"] = map[string]any{"bind_dn": "UID=Admin," + suffix, "password_file": "/nonexistent"}
	})
	e2.srv.Do(func(st *ldapfake.State) {
		st.Put("uid=admin,"+suffix, map[string][]string{"objectClass": {"top", "account"}, "uid": {"admin"}})
	})
	lst, err := e2.p.List(context.Background(), rng())
	if err != nil || len(lst.IDs) != 0 {
		t.Errorf("bind の DN: %v %v", lst.IDs, err)
	}
	if l, err := e.p.List(context.Background(), rng("server", "nope")); err == nil {
		t.Errorf("知らないサーバ: %v", l)
	}
}

// 子のある entry は消さず、葉は消す（E-G1・E-F1）。
func TestDelete(t *testing.T) {
	e := newEnv(t, nil)
	e.srv.Do(func(st *ldapfake.State) {
		st.Put(peopleDN, map[string][]string{"objectClass": {"top", "organizationalUnit"}, "ou": {"people"}})
		st.Put("uid=bob,"+peopleDN, map[string][]string{"objectClass": {"top", "account"}, "uid": {"bob"}})
	})
	w := core.NewWriteContext(nil)
	var re *core.RefuseError
	if err := e.p.Delete(context.Background(), w, id(peopleDN), core.Observation{}); !errors.As(err, &re) {
		t.Fatalf("子のある entry: %v", err)
	}
	// 宣言した entry の上は、子がまだ無くても消さない（同じ cycle の prune と作成の順に依らない）。
	e.validate(t, []core.Resource{alice("")})
	if err := e.p.Delete(context.Background(), w, id(peopleDN), core.Observation{}); !errors.As(err, &re) || !strings.Contains(re.Reason, "uid=alice") {
		t.Fatalf("宣言した entry の上: %v", err)
	}
	e.validate(t, nil)
	if err := e.p.Delete(context.Background(), w, id("uid=bob,"+peopleDN), core.Observation{}); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Delete(context.Background(), w, id(peopleDN), core.Observation{}); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Delete(context.Background(), w, id(peopleDN), core.Observation{}); err != nil {
		t.Errorf("無い entry: %v", err)
	}
	if o := e.observe(t, id(peopleDN)); o.Availability != core.Absent {
		t.Errorf("消したあと %s", o.Availability)
	}
	// 削除は書き込み用の credential だけで行う（E-G5）。
	for _, c := range e.srv.Calls() {
		if c.Command == "ldapmodify" && c.BindDN != dmDN {
			t.Errorf("書き込みを %s で bind した", c.BindDN)
		}
	}
}

// 観測と List は観測用の credential だけ、Apply・Delete は書き込み用の credential だけ（E-G5・N-G3）。
func TestCredentialSplit(t *testing.T) {
	e := newEnv(t, nil)
	all := []core.Resource{people()}
	e.validate(t, all)
	e.observe(t, id(peopleDN))
	if _, err := e.p.List(context.Background(), rng()); err != nil {
		t.Fatal(err)
	}
	if n := e.boundAs(dmDN); n != 0 {
		t.Fatalf("観測で書き込み用の credential を %d 回使った", n)
	}
	e.converge(t, all, all[0])
	for _, c := range e.srv.Calls() {
		switch {
		case c.Command == "ldapmodify" && c.BindDN != dmDN:
			t.Errorf("書き込みを %s で bind した", c.BindDN)
		case c.Command == "ldapsearch" && c.BindDN != readerDN:
			t.Errorf("観測を %s で bind した", c.BindDN)
		}
	}
	// 書き込み用のパスワードのファイルが無くても観測はできる。
	os.Remove(filepath.Join(e.dir, "dm"))
	if o := e.observe(t, id(peopleDN)); o.Availability != core.Present {
		t.Errorf("書き込み用のファイルが無いときの観測: %s %s", o.Availability, o.Note)
	}
	// write を省けば observe を使う（読むだけの DN ならサーバが拒否する）。
	e2 := newEnv(t, func(s map[string]any) { delete(s, "write") })
	e2.validate(t, all)
	err := e2.p.Apply(context.Background(), core.NewWriteContext(nil), all[0].ID, all[0].Fields, core.Observation{Availability: core.Absent})
	if err == nil || !strings.Contains(err.Error(), "50") {
		t.Errorf("読むだけの DN の書き込み: %v", err)
	}
	if n := e2.boundAs(readerDN); n == 0 || e2.boundAs(dmDN) != 0 {
		t.Errorf("bind: reader %d dm %d", n, e2.boundAs(dmDN))
	}
}

// 届かない・壊れた出力は Unknown で、何も書かない（E-K1）。List はエラー（E-K2）。
func TestUnknownNotAbsent(t *testing.T) {
	for name, fault := range map[string]func(e *env){
		"届かない":     func(e *env) { e.srv.Do(func(st *ldapfake.State) { st.Unreachable = true }) },
		"bind の失敗": func(e *env) { e.srv.Do(func(st *ldapfake.State) { st.Readers[readerDN] = "x" }) },
		"読めない出力":   func(e *env) { e.srv.Do(func(st *ldapfake.State) { st.Broken = true }) },
		"件数の上限":    func(e *env) { e.srv.Do(func(st *ldapfake.State) { st.SizeLimit = 1 }) },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, nil)
			e.srv.Do(func(st *ldapfake.State) {
				st.Put(peopleDN, map[string][]string{"objectClass": {"top", "organizationalUnit"}, "ou": {"people"}})
			})
			fault(e)
			if _, err := e.p.List(context.Background(), rng()); err == nil {
				t.Error("List がエラーにならない")
			}
			if name == "件数の上限" {
				return
			}
			obs, err := e.p.Observe(context.Background(), []core.ID{id(peopleDN), id(aliceDN)})
			if err != nil {
				t.Fatal(err)
			}
			for i, o := range obs {
				if o.Availability != core.Unknown || o.Note == "" {
					t.Errorf("%s: %s %q", i, o.Availability, o.Note)
				}
			}
		})
	}
}

func TestValidate(t *testing.T) {
	e := newEnv(t, nil)
	good := alice("test:ldap/alice@gen=1")
	cases := map[string]struct {
		r      core.Resource
		ranges []core.Range
		want   string
	}{
		"正規形でない":         {res("UID=Alice, ou=people,dc=example,dc=com", good.Fields[ldap.FieldAttributes].(map[string]any), ""), nil, "正規形"},
		"Range の外":       {res("uid=x,dc=other,dc=com", map[string]any{"objectClass": "account", "uid": "x"}, ""), nil, "Range"},
		"scope one の外":   {good, []core.Range{rng("scope", "one")}, "Range"},
		"base そのもの":      {res(suffix, map[string]any{"objectClass": "domain"}, ""), nil, "Range"},
		"bind の DN":      {res(readerDN, map[string]any{"objectClass": "account", "uid": "sakutei-read"}, ""), nil, "bind"},
		"知らないサーバ":        {core.Resource{ID: core.ID{Type: "dir/entry", Name: "dir9/" + aliceDN}}, nil, "dir9"},
		"知らない type":      {core.Resource{ID: core.ID{Type: "dir/group", Name: "dir1/" + aliceDN}}, nil, "entry"},
		"objectClass 無し": {res(aliceDN, map[string]any{"uid": "alice"}, ""), nil, "objectClass"},
		"同じ属性":           {res(aliceDN, map[string]any{"objectClass": "account", "uid": "alice", "UID": "alice"}, ""), nil, "同じ属性"},
		"操作属性":           {res(aliceDN, map[string]any{"objectClass": "account", "modifyTimestamp": "x"}, ""), nil, "操作属性"},
		"userPassword":   {res(aliceDN, map[string]any{"objectClass": "account", "userPassword": "hunter2"}, ""), nil, "user_password"},
		"数":              {res(aliceDN, map[string]any{"objectClass": "account", "uidNumber": int64(1000)}, ""), nil, "文字列"},
		"空の値":            {res(aliceDN, map[string]any{"objectClass": "account", "mail": l("a@x", "")}, ""), nil, "空の値"},
		"RDN の値":         {res(aliceDN, map[string]any{"objectClass": "account", "uid": "bob"}, ""), nil, "RDN"},
		"名前でない":          {res(aliceDN, map[string]any{"objectClass": "account", "cn;lang-ja": "x"}, ""), nil, "属性の名前"},
		"観測だけ":           {core.Resource{ID: id(aliceDN), Fields: core.Fields{ldap.FieldAttributes: map[string]any{"objectClass": "account"}, ldap.FieldUserPasswordSet: true}}, nil, "観測だけ"},
		"Range の引数":      {good, []core.Range{rng("base", "x")}, "引数"},
		"Range の重なり":     {good, []core.Range{rng(), {Provider: "dir", Args: map[string]string{"server": "dir1", "base_dn": "OU=People," + suffix}}}, "重なる"},
		"Range の filter": {good, []core.Range{rng("filter", "objectClass=*")}, "filter"},
		"Range の scope":  {good, []core.Range{rng("scope", "base")}, "scope"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			ranges := c.ranges
			if ranges == nil {
				ranges = []core.Range{rng()}
			}
			diags := e.p.Validate([]core.Resource{c.r}, ranges)
			var msgs []string
			for _, d := range diags {
				msgs = append(msgs, d.String())
			}
			all := strings.Join(msgs, "\n")
			if len(diags) == 0 || !strings.Contains(all, c.want) {
				t.Errorf("診断 = %q（want %q を含む）", all, c.want)
			}
			if strings.Contains(all, "hunter2") {
				t.Errorf("値を出した: %s", all)
			}
		})
	}
	if d := e.p.Validate([]core.Resource{good, {ID: id("uid=gone,ou=people," + suffix), Ensure: core.EnsureAbsent}}, []core.Range{rng()}); len(d) != 0 {
		t.Errorf("正しい宣言をエラーにした: %v", d)
	}
}

func TestSettings(t *testing.T) {
	ok := func() map[string]any {
		return map[string]any{"uri": "ldaps://ldap.test", "observe": map[string]any{"bind_dn": dmDN, "password_file": "/x"}}
	}
	if _, err := ldap.Module(map[string]any{"servers": map[string]any{"dir1": ok()}}); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		mod  func(m map[string]any, s map[string]any)
		want string
	}{
		"scheme":     {func(_, s map[string]any) { s["uri"] = "http://x" }, "ldap://"},
		"no uri":     {func(_, s map[string]any) { delete(s, "uri") }, "uri"},
		"starttls":   {func(_, s map[string]any) { s["starttls"] = true }, "starttls"},
		"no observe": {func(_, s map[string]any) { delete(s, "observe") }, "observe"},
		"password":   {func(_, s map[string]any) { s["observe"] = map[string]any{"bind_dn": dmDN, "password": "hunter2"} }, "password"},
		"bind_dn":    {func(_, s map[string]any) { s["bind_dn"] = dmDN }, "bind_dn"},
		"base_dn":    {func(_, s map[string]any) { s["base_dn"] = suffix }, "intent"},
		"top base":   {func(m, _ map[string]any) { m["base_dn"] = suffix }, "intent"},
		"timeout":    {func(_, s map[string]any) { s["timeout"] = "0s" }, "timeout"},
		"unknown":    {func(_, s map[string]any) { s["tiemout"] = "3s" }, "知らない field"},
		"name":       {func(m, s map[string]any) { m["servers"] = map[string]any{"Dir/1": s} }, "名前"},
	} {
		t.Run(name, func(t *testing.T) {
			s := ok()
			m := map[string]any{"servers": map[string]any{"dir1": s}}
			c.mod(m, s)
			_, err := ldap.Module(m)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v（want %q を含む）", err, c.want)
			}
			if err != nil && strings.Contains(err.Error(), "hunter2") {
				t.Error("エラーの文に値を出した")
			}
		})
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
	if err := ldapfake.SaveState(state, ldapfake.NewState(dmDN, dmPW, suffix)); err != nil {
		t.Fatal(err)
	}
	pwFile := filepath.Join(dir, "dm")
	if err := os.WriteFile(pwFile, []byte(dmPW+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SAKUTEI_FAKE_LDAP_MAIN", "1")
	t.Setenv("SAKUTEI_FAKE_LDAP_STATE", state)
	t.Setenv("LDAPTLS_REQCERT", "never") // 親の設定は引き継がない。
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	settings := func(search string) map[string]any {
		return map[string]any{"ldapsearch": search, "ldapmodify": modify, "servers": map[string]any{"dir1": map[string]any{
			"uri": "ldap://ldap.test", "starttls": true, "observe": map[string]any{"bind_dn": dmDN, "password_file": pwFile}}}}
	}
	p, err := ldap.Module(settings(search))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{p: p.(*ldap.Provider), sec: secrets{"ldap/alice": alicePW}}
	all := []core.Resource{people(), alice("test:ldap/alice@gen=1")}
	e.converge(t, all, all[0])
	e.converge(t, all, all[1])
	st, err := ldapfake.LoadState(state)
	if err != nil {
		t.Fatal(err)
	}
	if a := st.Find(aliceDN); a == nil || a.Get("userPassword")[0] != ldapfake.HashPassword(alicePW) {
		t.Fatalf("alice = %+v", a)
	}
	left, _ := filepath.Glob(filepath.Join(tmp, "sakutei-ldap-*"))
	for _, f := range left {
		t.Errorf("一時のパスワードのファイルが残った: %s", f)
	}
	// コマンドが無い。
	p2, _ := ldap.Module(settings(filepath.Join(dir, "nope")))
	obs, err := p2.Observe(context.Background(), []core.ID{id(peopleDN)})
	if o := obs[id(peopleDN)]; err != nil || o.Availability != core.Unknown || !strings.Contains(o.Note, "起動できない") {
		t.Errorf("コマンドが無い: %+v, %v", o, err)
	}
}
