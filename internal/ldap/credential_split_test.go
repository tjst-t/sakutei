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

// credential_split_test.go — 観測用と書き込み用の bind の資格（§20.7、#186）。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/cause"
	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/ldap"
	"github.com/tjst-t/sakutei/v2/internal/ldap/ldaptest"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

const (
	readerDN = "cn=sakutei-read,ou=system,dc=example,dc=com"
	readerPW = "reader-pw"
	adminDN  = "cn=sakutei,ou=system,dc=example,dc=com"
	adminPW  = "admin-pw"
)

// splitRig は読むだけの DN（Readers）と書き込みのできる DN を受ける偽の ldap
// コマンドと、その 2 本を observe:・write: に書いたサーバの一覧。2 本の DN の
// 項目はどちらも base_dn の下にある（List に出さないことを確かめるため）。
func splitRig(t *testing.T, store string) (*ldaptest.Server, string) {
	t.Helper()
	st := ldaptest.NewState(adminDN, adminPW, suffix)
	st.Readers = map[string]string{readerDN: readerPW}
	st.Put("ou=system,"+suffix, map[string][]string{"objectClass": {"top", "organizationalUnit"}, "ou": {"system"}})
	st.Put(readerDN, map[string][]string{"objectClass": {"top", "person"}, "cn": {"sakutei-read"}})
	st.Put(adminDN, map[string][]string{"objectClass": {"top", "person"}, "cn": {"sakutei"}})
	srv := ldaptest.New(st)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "secrets.json"), []byte(store), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := "secret_store: secrets.json\nservers:\n  dir1:\n    uri: ldap://ldap.test\n    starttls: true\n    base_dn: " + suffix + "\n" +
		"    observe:\n      bind_dn: " + readerDN + "\n      bind_password: secretref:file/ldap/read\n" +
		"    write:\n      bind_dn: " + adminDN + "\n      bind_password: secretref:file/ldap/write\n"
	path := filepath.Join(dir, "ldap.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return srv, path
}

// splitProvider は設定を opts で読み、偽の ldap コマンドを差した Provider を作る。
func splitProvider(t *testing.T, srv *ldaptest.Server, path string, opts ldap.LoadOptions) (*ldap.Provider, error) {
	t.Helper()
	cfg, err := ldap.LoadConfigWith(path, opts)
	if err != nil {
		return nil, err
	}
	o := cfg.Options()
	o.Runner = ldap.RunnerFunc(srv.Run)
	return ldap.New(cfg.Servers, o)
}

func splitPlan(t *testing.T, p *ldap.Provider, local string, fields map[string]string) core.Plan {
	t.Helper()
	obs, err := p.Read(context.Background(), local)
	if err != nil || obs.Availability != core.AvailabilityOk {
		t.Fatalf("Read(%s) with the observe DN: %v %+v", local, err, obs)
	}
	pl, err := diff.ForProvider(p).Plan(obs, diff.DesiredResource{ID: id(local), Fields: fields}, cause.CauseContext{ResourceObservedAbsent: obs.State == nil})
	if err != nil {
		t.Fatal(err)
	}
	return pl
}

func TestCredentialSplit_ObserveWithReaderWriteWithAdmin(t *testing.T) {
	// [AC-186-4-1] the observe bind DN only searches, the write bind DN makes
	// the changes, the loop converges, and neither bind DN is listed.
	srv, path := splitRig(t, `{"secrets": {"ldap/read": {"value": "`+readerPW+`"}, "ldap/write": {"value": "`+adminPW+`"}}}`)
	p, err := splitProvider(t, srv, path, ldap.LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct {
		local  string
		fields map[string]string
	}{{peopleID, people()}, {groupID, map[string]string{"objectClass": "groupOfNames", "cn": "admins", "member": adminDN}}} {
		for i := 0; i < 3; i++ {
			pl := splitPlan(t, p, r.local, r.fields)
			if len(pl.Ops) == 0 {
				break
			}
			if err := p.Apply(provider.WithWrite(context.Background()), pl); err != nil {
				t.Fatalf("Apply(%s) with the write DN: %v", r.local, err)
			}
		}
		if pl := splitPlan(t, p, r.local, r.fields); len(pl.Ops) != 0 {
			t.Fatalf("%s: not converged: %+v", r.local, pl.Ops)
		}
	}
	res, err := p.List(context.Background(), core.NewScope(nil))
	if err != nil {
		t.Fatalf("List with the observe DN: %v", err)
	}
	var got []string
	for _, x := range res.IDs {
		got = append(got, x.Local)
	}
	if want := "dir1/cn=admins,ou=people,dc=example,dc=com dir1/ou=people,dc=example,dc=com dir1/ou=system,dc=example,dc=com"; strings.Join(got, " ") != want {
		t.Errorf("List = %v; want %s (neither bind DN)", got, want)
	}
	if _, err := p.BulkObserve(context.Background(), core.NewScope(nil)); err != nil {
		t.Fatalf("BulkObserve with the observe DN: %v", err)
	}
	calls := srv.BindCalls()
	reader, admin := calls[readerDN], calls[adminDN]
	if reader["ldapsearch"] == 0 || reader["ldapmodify"] != 0 {
		t.Errorf("the observe DN: %v; want searches only", reader)
	}
	if admin["ldapmodify"] == 0 {
		t.Errorf("the write DN: %v; want the modifies", admin)
	}
	if len(calls) != 2 {
		t.Errorf("bind DNs used: %v; want the two configured", calls)
	}
	// Both bind DNs are outside what the provider manages.
	for _, dn := range []string{readerDN, adminDN} {
		if err := p.ValidateIntent("dir1/"+dn, map[string]string{"objectClass": "person"}); err == nil || !strings.Contains(err.Error(), "bind_dn") {
			t.Errorf("the bind DN %s as an id: %v", dn, err)
		}
	}
}

func TestCredentialSplit_ObserveOnlyRunNeedsNoWritePassword(t *testing.T) {
	// [AC-186-4-2] an observe-only run starts without the write password,
	// observes, and refuses a write without sending it; a run that may write
	// needs the write password.
	srv, path := splitRig(t, `{"secrets": {"ldap/read": {"value": "`+readerPW+`"}}}`)
	if _, err := splitProvider(t, srv, path, ldap.LoadOptions{}); err == nil || !strings.Contains(err.Error(), "servers.dir1.write.bind_password") {
		t.Fatalf("a writing run without the write password: %v", err)
	}
	p, err := splitProvider(t, srv, path, ldap.LoadOptions{ObserveOnly: true})
	if err != nil {
		t.Fatalf("an observe-only run: %v", err)
	}
	pl := splitPlan(t, p, peopleID, people())
	if len(pl.Ops) == 0 {
		t.Fatal("want a plan to create the entry")
	}
	if _, err := p.List(context.Background(), core.NewScope(nil)); err != nil {
		t.Fatalf("List in an observe-only run: %v", err)
	}
	for _, ctx := range []context.Context{provider.WithWrite(context.Background()), context.Background()} {
		if err := p.Apply(ctx, pl); !errors.Is(err, provider.ErrNoWriteAccess) {
			t.Fatalf("Apply in an observe-only run: %v", err)
		}
	}
	// A write through the provider's Read in a write context is refused too.
	if _, err := p.Read(provider.WithWrite(context.Background()), peopleID); !errors.Is(err, provider.ErrNoWriteAccess) {
		t.Errorf("Read in a write context of an observe-only run: %v", err)
	}
	calls := srv.BindCalls()
	if len(calls[adminDN]) != 0 {
		t.Errorf("the write DN was used: %v", calls[adminDN])
	}
	if calls[readerDN]["ldapmodify"] != 0 {
		t.Errorf("the observe DN tried to modify: %v", calls[readerDN])
	}
	srv.Do(func(st *ldaptest.State) {
		if st.Find("ou=people,"+suffix) != nil {
			t.Error("the entry was created in an observe-only run")
		}
	})
}

func TestCredentialSplit_ConfigErrors(t *testing.T) {
	// [AC-186-4-3]
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "secrets.json"), []byte(`{"secrets": {"ldap/a": {"value": "a"}, "ldap/b": {"value": "b"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	head := "secret_store: secrets.json\nservers:\n  dir1:\n    uri: ldap://ldap.test\n    base_dn: " + suffix + "\n"
	load := func(body string) (ldap.Config, error) {
		path := filepath.Join(dir, "ldap.yaml")
		if err := os.WriteFile(path, []byte(head+body), 0o600); err != nil {
			t.Fatal(err)
		}
		return ldap.LoadConfig(path)
	}
	for name, c := range map[string]struct{ body, want string }{
		"no credential":       {"", "servers.dir1.bind_dn is required"},
		"observe only, no dn": {"    bind_password: secretref:file/ldap/a\n    observe:\n      bind_password: secretref:file/ldap/b\n", "servers.dir1.observe.bind_dn is required"},
		"write lacks password": {"    write:\n      bind_dn: cn=w\n    observe:\n      bind_dn: cn=r\n      bind_password: secretref:file/ldap/a\n",
			"servers.dir1.write.bind_password is required"},
		"unknown key":      {"    bind_dn: cn=a\n    bind_password: secretref:file/ldap/a\n    write:\n      uri: ldap://other\n", `unknown key "servers.dir1.write.uri"`},
		"not a map":        {"    bind_dn: cn=a\n    bind_password: secretref:file/ldap/a\n    observe: cn=r\n", "servers.dir1.observe: must be a map"},
		"raw password":     {"    bind_dn: cn=a\n    bind_password: secretref:file/ldap/a\n    write:\n      bind_password: hunter2\n", "servers.dir1.write.bind_password: must be a SecretRef"},
		"bad dn":           {"    bind_password: secretref:file/ldap/a\n    observe:\n      bind_dn: \"cn\"\n    write:\n      bind_dn: cn=w\n", "servers.dir1.observe.bind_dn"},
		"write not stored": {"    bind_dn: cn=a\n    bind_password: secretref:file/ldap/a\n    write:\n      bind_password: secretref:file/ldap/zz\n", "servers.dir1.write.bind_password: secretref:file/ldap/zz#gen0 is not in the secret store"},
	} {
		_, err := load(c.body)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v; want %q", name, err, c.want)
		}
		if err != nil && strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: the error shows the password: %v", name, err)
		}
	}
	// Both resolved credentials complete: the server-level keys are optional,
	// and an unwritten key is inherited from the server.
	cfg, err := load("    bind_password: secretref:file/ldap/a\n    observe:\n      bind_dn: cn=r\n    write:\n      bind_dn: cn=w\n      bind_password: secretref:file/ldap/b\n")
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Servers["dir1"]
	if s.BindDN != "cn=r" || s.BindPassword.Path() != "ldap/a" || s.WriteBindDN != "cn=w" || s.WriteBindPassword.Path() != "ldap/b" {
		t.Errorf("server = %+v", s)
	}
	// Neither written: one credential as before.
	cfg, err = load("    bind_dn: cn=a\n    bind_password: secretref:file/ldap/a\n")
	if err != nil {
		t.Fatal(err)
	}
	if s := cfg.Servers["dir1"]; s.BindDN != "cn=a" || s.WriteBindDN != "cn=a" || s.WriteBindPassword.Path() != "ldap/a" {
		t.Errorf("one credential: %+v", s)
	}
}
