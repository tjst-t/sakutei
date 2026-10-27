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

// ldap_test.go — Backlog #144: the ldap provider (entries of an LDAP directory,
// only the declared attributes) against the fake ldapsearch / ldapmodify
// (internal/ldap/ldaptest, built as an executable so the provider runs it
// through os/exec exactly as it runs the OpenLDAP clients).  A real 389
// Directory Server is exercised by tests/integration/ldap (make
// integration-ldap).
//
// AC tags:
//
//	AC-144-1  DefaultRegistry builds ldap from ldap_config (the server list; the
//	          bind password is a SecretRef resolved from the file
//	          SecretBackend); without it is an error; the provider passes
//	          sdk.ContractTester (incl. unreachable → unknown, never absent)
//	AC-144-2  sakutei reconcile / status with --provider-config: created from
//	          empty, then conformant (the superior object classes the server
//	          adds are not drift); an attribute changed by hand is drift and
//	          comes back; no password appears in the output
//	AC-144-3  an unreachable server: the declared entries are unobservable,
//	          prune halts and nothing is deleted
package acceptance

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/ldap"
	"github.com/tjst-t/sakutei/v2/internal/ldap/ldaptest"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
	"github.com/tjst-t/sakutei/v2/pkg/registry"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
)

const (
	ldapDMSecret    = "ldap-dm-secret-value"
	ldapAliceSecret = "ldap-alice-secret-value"
)

var (
	fakeLDAPOnce sync.Once
	fakeLDAPDir  string
	fakeLDAPErr  error
)

// fakeLDAP builds the fake ldap commands once (ldapsearch and, by a link,
// ldapmodify) and returns their directory.
func fakeLDAP(t *testing.T) string {
	t.Helper()
	fakeLDAPOnce.Do(func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			fakeLDAPErr = err
			return
		}
		fakeLDAPDir = filepath.Join(binaryDir, "fakeldap")
		search := filepath.Join(fakeLDAPDir, "ldapsearch")
		cmd := exec.Command("go", "build", "-o", search, "./internal/ldap/ldaptest/fakeldap")
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			fakeLDAPErr = errors.New(string(out))
			return
		}
		fakeLDAPErr = os.Symlink(search, filepath.Join(fakeLDAPDir, "ldapmodify"))
	})
	if fakeLDAPErr != nil {
		t.Fatalf("build fakeldap: %v", fakeLDAPErr)
	}
	return fakeLDAPDir
}

// ldapLab is a fake directory (its state file), a server list pointing at it
// and a --provider-config naming that list.
type ldapLab struct {
	dir, state, config, providerConfig string
}

func newLDAPLab(t *testing.T) ldapLab {
	t.Helper()
	dir := t.TempDir()
	bin := fakeLDAP(t)
	lab := ldapLab{dir: dir, state: filepath.Join(dir, "state.json"), config: filepath.Join(dir, "ldap.yaml"), providerConfig: filepath.Join(dir, "providers.yaml")}
	if err := ldaptest.SaveState(lab.state, ldaptest.NewState("cn=Directory Manager", ldapDMSecret, "dc=example,dc=com")); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"secrets.json": `{"secrets": {"ldap/dm": {"gen": 1, "value": "` + ldapDMSecret + `"}, "ldap/alice": {"gen": 1, "value": "` + ldapAliceSecret + `"}}}`,
		"ldap.yaml": "ldapsearch: " + filepath.Join(bin, "ldapsearch") + "\nldapmodify: " + filepath.Join(bin, "ldapmodify") +
			"\nsecret_store: secrets.json\nservers:\n  dir1:\n    uri: ldaps://ldap.test\n    bind_dn: cn=Directory Manager\n" +
			"    bind_password: secretref:file/ldap/dm\n    base_dn: dc=example,dc=com\n",
		"providers.yaml": "providers:\n  ldap:\n    ldap_config: " + lab.config + "\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("SAKUTEI_FAKE_LDAP_STATE", lab.state)
	return lab
}

func (l ldapLab) edit(t *testing.T, f func(*ldaptest.State)) *ldaptest.State {
	t.Helper()
	st, err := ldaptest.LoadState(l.state)
	if err != nil {
		t.Fatal(err)
	}
	f(st)
	if err := ldaptest.SaveState(l.state, st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestLDAP_RegistryAndContract(t *testing.T) {
	// [AC-144-1]
	if _, err := registry.DefaultRegistry().Get(ldap.Name, map[string]string{"state": "x.json"}); err == nil || !strings.Contains(err.Error(), "ldap_config") {
		t.Errorf("[AC-144-1] ldap without ldap_config: err = %v; want one naming ldap_config", err)
	}
	lab := newLDAPLab(t)
	p, err := registry.DefaultRegistry().Get(ldap.Name, map[string]string{"ldap_config": lab.config})
	if err != nil {
		t.Fatalf("[AC-144-1] DefaultRegistry().Get(ldap): %v", err)
	}
	if err := sdk.ValidateProvider(p); err != nil {
		t.Fatalf("[AC-144-1] ValidateProvider: %v", err)
	}
	ct := sdk.ContractTester{
		Provider: p,
		Lister:   p.(provider.Lister),
		Scope:    core.NewScope(nil),
		SampleID: "dir1/ou=contract,dc=example,dc=com",
		SampleDesired: map[string]string{
			"objectclass": `["organizationalUnit","top"]`,
			"ou":          "contract",
			"description": "made by the contract test",
		},
		MakeUnreachable: func() (func(), error) {
			lab.edit(t, func(st *ldaptest.State) { st.Unreachable = true })
			return func() { lab.edit(t, func(st *ldaptest.State) { st.Unreachable = false }) }, nil
		},
	}
	ct.RunAll(t)
}

func TestLDAP_CLIReconcileAndStatus(t *testing.T) {
	// [AC-144-2] [AC-144-3]
	lab := newLDAPLab(t)
	intentDir := filepath.Join(lab.dir, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	intent := `resources:
  - provider: ldap
    id: dir1/ou=people,dc=example,dc=com
    deletion_policy: PruneWithinScope
    fields:
      objectClass: '["top","organizationalUnit"]'
      ou: People
  - provider: ldap
    id: dir1/uid=alice,ou=people,dc=example,dc=com
    deletion_policy: PruneWithinScope
    depends_on:
      - dir1/ou=people,dc=example,dc=com
    fields:
      objectClass: '["top","inetOrgPerson"]'
      uid: alice
      cn: Alice Liddell
      sn: Liddell
      mail: '["alice@example.com","al@example.com"]'
      userPassword: secretref:file/ldap/alice#gen1
`
	if err := os.WriteFile(filepath.Join(intentDir, "ldap.yaml"), []byte(intent), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, int) {
		args = append(args, "--intent", intentDir, "--state", filepath.Join(lab.dir, "fake.json"), "--provider-config", lab.providerConfig)
		out, err := exec.Command(buildBinary(t), args...).CombinedOutput()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		for _, s := range []string{ldapDMSecret, ldapAliceSecret, "{FAKE}"} {
			if strings.Contains(string(out), s) {
				t.Fatalf("[AC-144-2] sakutei %s shows a secret (%q):\n%s", args[0], s, out)
			}
		}
		return string(out), code
	}
	type summary struct {
		Conformant, Drift, Missing, Unobservable int
	}
	status := func() (summary, int, string) {
		out, code := run("status", "--json")
		var doc struct {
			Summary summary `json:"summary"`
		}
		if i := strings.Index(out, "{"); i < 0 || json.Unmarshal([]byte(out[i:]), &doc) != nil {
			t.Fatalf("status --json (exit %d):\n%s", code, out)
		}
		return doc.Summary, code, out
	}
	for i := 0; i < 2; i++ {
		if out, code := run("reconcile"); code != 0 {
			t.Fatalf("[AC-144-2] reconcile #%d exited %d:\n%s", i+1, code, out)
		}
	}
	if s, code, out := status(); code != 0 || s.Conformant != 2 || s.Drift != 0 {
		t.Fatalf("[AC-144-2] status after reconcile (exit %d) = %+v\n%s", code, s, out)
	}
	st := lab.edit(t, func(*ldaptest.State) {})
	a := st.Find("uid=alice,ou=people,dc=example,dc=com")
	if a == nil || len(a.Get("objectClass")) != 4 || a.Get("userPassword")[0] != ldaptest.HashPassword(ldapAliceSecret) {
		t.Fatalf("[AC-144-2] alice = %+v", a)
	}
	// 手で変えると drift、reconcile で戻る。
	lab.edit(t, func(st *ldaptest.State) {
		st.Find("uid=alice,ou=people,dc=example,dc=com").Set("mail", []string{"evil@example.com"})
	})
	if s, _, out := status(); s.Drift != 1 {
		t.Fatalf("[AC-144-2] status after the change by hand = %+v\n%s", s, out)
	}
	if out, code := run("reconcile"); code != 0 {
		t.Fatalf("[AC-144-2] reconcile exited %d:\n%s", code, out)
	}
	if s, code, out := status(); code != 0 || s.Conformant != 2 {
		t.Fatalf("[AC-144-2] status after reconcile = %+v\n%s", s, out)
	}
	// [AC-144-3] 届かない：観測できない・prune は止まる・何も消さない。
	lab.edit(t, func(st *ldaptest.State) {
		st.Put("ou=orphan,dc=example,dc=com", map[string][]string{"objectClass": {"top", "organizationalUnit"}, "ou": {"orphan"}})
		st.Unreachable = true
	})
	if s, code, out := status(); code != 3 || s.Unobservable != 2 || s.Conformant != 0 {
		t.Errorf("[AC-144-3] status while unreachable (exit %d) = %+v\n%s", code, s, out)
	}
	out, _ := run("reconcile")
	if !strings.Contains(out, "prune halted") {
		t.Errorf("[AC-144-3] reconcile while unreachable did not halt prune:\n%s", out)
	}
	st = lab.edit(t, func(st *ldaptest.State) { st.Unreachable = false })
	if st.Find("ou=orphan,dc=example,dc=com") == nil {
		t.Error("[AC-144-3] an entry was deleted while the server was unreachable")
	}
}
