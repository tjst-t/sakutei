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

package ldap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanonicalDN(t *testing.T) {
	cases := []struct{ in, want string }{
		{"uid=alice,ou=people,dc=example,dc=com", "uid=alice,ou=people,dc=example,dc=com"},
		{"UID=Alice, OU=People , DC=Example;dc=com", "uid=alice,ou=people,dc=example,dc=com"},
		{"OID.0.9.2342.19200300.100.1.1=Bob,dc=x", "0.9.2342.19200300.100.1.1=bob,dc=x"},
		{`cn=Smith\, John,dc=x`, `cn=smith\, john,dc=x`},
		{`cn=a\2cb,dc=x`, `cn=a\,b,dc=x`},
		{`cn="Smith, J",dc=x`, `cn=smith\, j,dc=x`},
		{`cn=\ lead\ ,dc=x`, `cn=\ lead\ ,dc=x`},
		{`cn=\#hash,dc=x`, `cn=\#hash,dc=x`},
		{`uid=b+cn=A,dc=x`, `cn=a+uid=b,dc=x`},
		{`cn=Ärger,dc=x`, `cn=ärger,dc=x`},
		{`cn=a/b,dc=x`, `cn=a/b,dc=x`},
		{"", ""},
	}
	for _, c := range cases {
		got, err := CanonicalDN(c.in)
		if err != nil || got != c.want {
			t.Errorf("CanonicalDN(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
		// 正規形は動かない。
		if again, err := CanonicalDN(got); err != nil || again != got {
			t.Errorf("CanonicalDN(%q) is not stable: %q, %v", got, again, err)
		}
	}
	for _, bad := range []string{"cn", "cn=#0401,dc=x", `cn=a\`, `cn="open`, "1x=a", `cn=a\qb`, "cn=a,,dc=x"} {
		if _, err := CanonicalDN(bad); err == nil {
			t.Errorf("CanonicalDN(%q): want an error", bad)
		}
	}
}

func TestDNStringKeepsCase(t *testing.T) {
	d, err := ParseDN("UID=Alice, OU=People,dc=example,dc=com")
	if err != nil {
		t.Fatal(err)
	}
	if got := d.String(); got != "uid=Alice,ou=People,dc=example,dc=com" {
		t.Errorf("String() = %q", got)
	}
}

func TestUnder(t *testing.T) {
	base, _ := ParseDN("dc=example,dc=com")
	for in, want := range map[string]bool{
		"ou=people,dc=example,dc=com":          true,
		"uid=a,ou=people,dc=example,dc=com":    true,
		"dc=example,dc=com":                    false,
		"dc=com":                               false,
		"ou=people,dc=other,dc=com":            false,
		"ou=people,dc=example,dc=com,dc=extra": false,
		"ou=people,dc=xexample,dc=com":         false,
	} {
		d, _ := ParseDN(in)
		if got := d.Canonical().Under(base.Canonical()); got != want {
			t.Errorf("%q under %q = %v; want %v", in, "dc=example,dc=com", got, want)
		}
	}
}

func TestParseLDIF(t *testing.T) {
	out := "# extended LDIF\n" +
		"dn: uid=alice,ou=people,dc=example,dc=com\n" +
		"objectClass: top\n" +
		"objectClass: inetOrgPerson\n" +
		"description: a long line that the\n" +
		"  server folded\n" +
		"cn:: w4RyZ2Vy\n" +
		"userPassword:: e1NTSEF9eHh4\n" +
		"\n" +
		"# pagedresults: estimate=3 cookie=MA==\n" +
		"#  folded comment\n" +
		"dn: ou=people,dc=example,dc=com\n" +
		"ou: people\n" +
		"\n" +
		"# pagedresults: cookie=\n"
	recs, err := ParseLDIF(out)
	if err != nil {
		t.Fatal(err)
	}
	es, err := entries(recs)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 2 {
		t.Fatalf("entries = %+v", es)
	}
	a := es[0]
	if a.DN != "uid=alice,ou=people,dc=example,dc=com" || strings.Join(a.Attrs["objectclass"], ",") != "top,inetOrgPerson" {
		t.Errorf("entry = %+v", a)
	}
	if got := a.Attrs["description"][0]; got != "a long line that the server folded" {
		t.Errorf("folded = %q", got)
	}
	if a.Attrs["cn"][0] != "Ärger" || a.Attrs["userpassword"][0] != "{SSHA}xxx" {
		t.Errorf("base64 = %q %q", a.Attrs["cn"], a.Attrs["userpassword"])
	}
	if _, err := ParseLDIF("dn: x\nattr:< file:///etc/passwd\n"); err == nil {
		t.Error("a value by URL: want an error")
	}
	if _, err := ParseLDIF(" lead\n"); err == nil {
		t.Error("a continuation at the start: want an error")
	}
}

func TestLDIFRoundTrip(t *testing.T) {
	attrs := []Attr{{Name: "objectClass", Values: []string{"top", "person"}}, {Name: "description", Values: []string{"line1\nline2", " lead", "日本"}}}
	recs, err := ParseLDIF(AddLDIF("cn=a\\,b,dc=x", attrs) + ReplaceLDIF("cn=z,dc=x", []Attr{{Name: "mail"}, {Name: "cn", Values: []string{"z"}}}) + DeleteLDIF("cn=y,dc=x"))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("records = %d", len(recs))
	}
	add := recs[0]
	if add[0].Value != "cn=a\\,b,dc=x" || add[1].Value != "add" || add[4].Value != "line1\nline2" || add[5].Value != " lead" || add[6].Value != "日本" {
		t.Errorf("add = %+v", add)
	}
	rep := recs[1]
	want := []string{"dn", "changetype", "replace", "-", "replace", "cn", "-"}
	if len(rep) != len(want) {
		t.Fatalf("replace = %+v", rep)
	}
	for i, l := range rep {
		if l.Name != want[i] {
			t.Errorf("replace line %d = %+v; want %s", i, l, want[i])
		}
	}
	if recs[2][1].Value != "delete" {
		t.Errorf("delete = %+v", recs[2])
	}
}

func TestEncodeDecodeValues(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"x"}, "x"},
		{[]string{"b", "a", "b"}, `["a","b"]`},
		{[]string{"[x"}, `["[x"]`},
	} {
		got := EncodeValues(c.in)
		if got != c.want {
			t.Errorf("EncodeValues(%q) = %q; want %q", c.in, got, c.want)
		}
		back, err := DecodeValues(got)
		if err != nil || EncodeValues(back) != got {
			t.Errorf("DecodeValues(%q) = %q, %v", got, back, err)
		}
	}
	if v, err := DecodeValues("[]"); err != nil || len(v) != 0 {
		t.Errorf("[] = %q, %v", v, err)
	}
	if _, err := DecodeValues("[not json"); err == nil {
		t.Error("bad JSON: want an error")
	}
}

func TestImpliedClasses(t *testing.T) {
	sup := parseObjectClasses([]string{
		"( 2.5.6.0 NAME 'top' ABSTRACT MUST objectClass X-ORIGIN 'RFC 4512' )",
		"( 2.5.6.6 NAME 'person' SUP top STRUCTURAL MUST ( sn $ cn ) )",
		"( 2.5.6.7 NAME 'organizationalPerson' SUP person STRUCTURAL )",
		"( 2.16.840.1.113730.3.2.2 NAME ( 'inetOrgPerson' 'iop' ) SUP organizationalPerson STRUCTURAL )",
		"( 1.1 NAME 'multi' SUP ( person $ domain ) AUXILIARY )",
	})
	if got := impliedClasses([]string{"inetOrgPerson", "top"}, sup); strings.Join(got, ",") != "organizationalperson,person,top" {
		t.Errorf("implied = %v", got)
	}
	if got := impliedClasses([]string{"IOP"}, sup); strings.Join(got, ",") != "organizationalperson,person,top" {
		t.Errorf("implied by alias = %v", got)
	}
	if got := impliedClasses([]string{"multi"}, sup); strings.Join(got, ",") != "domain,person,top" {
		t.Errorf("implied of multi = %v", got)
	}
}

func TestSameAttr(t *testing.T) {
	imp := `["organizationalperson","person","top"]`
	cases := []struct {
		name, d, o string
		want       bool
	}{
		{"objectclass", `["top","inetOrgPerson"]`, `["inetOrgPerson","organizationalPerson","person","top"]`, true},
		{"objectclass", `inetorgperson`, `["inetOrgPerson","organizationalPerson","person","top"]`, true},
		{"objectclass", `["top","inetOrgPerson"]`, `["inetOrgPerson","organizationalPerson","person","posixAccount","top"]`, false},
		{"objectclass", `["top","inetOrgPerson","posixAccount"]`, `["inetOrgPerson","top"]`, false},
		{"mail", `["b@x","a@x"]`, `["a@x","b@x"]`, true},
		{"mail", `a@x`, `["a@x","b@x"]`, false},
		{"mail", `A@x`, `a@x`, false},
		{"mail", ``, ``, true},
		{"mail", `[]`, ``, true},
		{"member", `UID=Alice, OU=People,dc=x`, `uid=alice,ou=people,dc=x`, true},
		{"description", `UID=Alice, OU=People,dc=x`, `uid=alice,ou=people,dc=x`, false},
	}
	for _, c := range cases {
		if got := sameAttr(c.name, c.d, c.o, imp); got != c.want {
			t.Errorf("sameAttr(%s, %q, %q) = %v; want %v", c.name, c.d, c.o, got, c.want)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("secrets.json", `{"secrets": {"ldap/dm": {"gen": 1, "value": "x"}}}`)
	good := "secret_store: secrets.json\nldapsearch: bin/ldapsearch\nservers:\n  dir1:\n    uri: ldaps://ldap.test\n    ca_cert: ca.pem\n" +
		"    bind_dn: cn=Directory Manager\n    bind_password: secretref:file/ldap/dm\n    base_dn: dc=Example,dc=com\n    timeout: 3s\n"
	cfg, err := LoadConfig(write("good.yaml", good))
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Servers["dir1"]
	if s.Filter != DefaultFilter || s.Timeout.String() != "3s" || s.CACert != filepath.Join(dir, "ca.pem") || cfg.LDAPSearch != filepath.Join(dir, "bin/ldapsearch") || cfg.LDAPModify != "" {
		t.Errorf("config = %+v %+v", cfg, s)
	}
	if s.base.String() != "dc=example,dc=com" {
		t.Errorf("base = %q", s.base)
	}
	for name, c := range map[string]struct{ body, want string }{
		"raw":      {strings.Replace(good, "secretref:file/ldap/dm", "hunter2", 1), "must be a SecretRef"},
		"missing":  {strings.Replace(good, "ldap/dm", "ldap/none", 1), "is not in the secret store"},
		"nostore":  {strings.Replace(good, "secret_store: secrets.json\n", "", 1), "secret_store"},
		"scheme":   {strings.Replace(good, "ldaps://", "http://", 1), "want ldap://"},
		"starttls": {strings.Replace(good, "    timeout: 3s\n", "    starttls: true\n", 1), "already TLS"},
		"nobase":   {strings.Replace(good, "    base_dn: dc=Example,dc=com\n", "", 1), "base_dn is required"},
		"filter":   {strings.Replace(good, "    timeout: 3s\n", "    filter: objectClass=*\n", 1), "in parentheses"},
		"unknown":  {strings.Replace(good, "    timeout: 3s\n", "    tiemout: 3s\n", 1), "unknown key"},
		"nopw":     {strings.Replace(good, "    bind_password: secretref:file/ldap/dm\n", "", 1), "bind_password is required"},
	} {
		_, err := LoadConfig(write(name+".yaml", c.body))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v; want %q", name, err, c.want)
		}
		if err != nil && strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: the error shows the password", name)
		}
	}
}
