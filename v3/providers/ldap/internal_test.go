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
	"strings"
	"testing"
)

// DN の正規形は 1 つに決まり、動かない（E-G1・E-M3）。
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
		if again, err := CanonicalDN(got); err != nil || again != got {
			t.Errorf("CanonicalDN(%q) が動く: %q, %v", got, again, err)
		}
	}
	for _, bad := range []string{"cn", "cn=#0401,dc=x", `cn=a\`, `cn="open`, "1x=a", `cn=a\qb`, "cn=a,,dc=x"} {
		if _, err := CanonicalDN(bad); err == nil {
			t.Errorf("CanonicalDN(%q): エラーにならない", bad)
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

func TestUnderAndRange(t *testing.T) {
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
			t.Errorf("%q under dc=example,dc=com = %v; want %v", in, got, want)
		}
	}
	// scope one は base_dn の直下だけ（E-G1）。
	one := rangeSpec{server: "s", base: base.Canonical(), scope: ScopeOne}
	child, _ := ParseDN("ou=people,dc=example,dc=com")
	grand, _ := ParseDN("uid=a,ou=people,dc=example,dc=com")
	if !one.contains(child) || one.contains(grand) {
		t.Errorf("scope one: child %v grandchild %v", one.contains(child), one.contains(grand))
	}
	// Range の重なり（E-K8）。
	sub := rangeSpec{server: "s", base: base.Canonical(), scope: ScopeSub}
	people := rangeSpec{server: "s", base: child.Canonical(), scope: ScopeSub}
	other := rangeSpec{server: "t", base: base.Canonical(), scope: ScopeSub}
	if !sub.overlaps(people) || !people.overlaps(sub) || one.overlaps(people) || sub.overlaps(other) {
		t.Errorf("overlaps: %v %v %v %v", sub.overlaps(people), people.overlaps(sub), one.overlaps(people), sub.overlaps(other))
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
		t.Errorf("折り返し = %q", got)
	}
	if a.Attrs["cn"][0] != "Ärger" || a.Attrs["userpassword"][0] != "{SSHA}xxx" {
		t.Errorf("base64 = %q %q", a.Attrs["cn"], a.Attrs["userpassword"])
	}
	if _, err := ParseLDIF("dn: x\nattr:< file:///etc/passwd\n"); err == nil {
		t.Error("URL の値: エラーにならない")
	}
	if _, err := ParseLDIF(" lead\n"); err == nil {
		t.Error("先頭の継続の行: エラーにならない")
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
			t.Errorf("replace の %d 行目 = %+v; want %s", i, l, want[i])
		}
	}
	if recs[2][1].Value != "delete" {
		t.Errorf("delete = %+v", recs[2])
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
		t.Errorf("別名の implied = %v", got)
	}
	if got := impliedClasses([]string{"multi"}, sup); strings.Join(got, ",") != "domain,person,top" {
		t.Errorf("multi の implied = %v", got)
	}
}

// 値の集合として比べる（E-B12・E-G1）。
func TestEquivalent(t *testing.T) {
	sup := map[string][]string{"inetorgperson": {"organizationalperson"}, "organizationalperson": {"person"}, "person": {"top"}}
	schema := func() (map[string][]string, error) { return sup, nil }
	l := func(s ...string) []string { return s }
	cases := []struct {
		name       string
		want, have []string
		same       bool
	}{
		{"objectclass", l("top", "inetOrgPerson"), l("inetOrgPerson", "organizationalPerson", "person", "top"), true},
		{"objectclass", l("inetorgperson"), l("inetOrgPerson", "organizationalPerson", "person", "top"), true},
		{"objectclass", l("top", "inetOrgPerson"), l("inetOrgPerson", "organizationalPerson", "person", "posixAccount", "top"), false},
		{"objectclass", l("top", "inetOrgPerson", "posixAccount"), l("inetOrgPerson", "top"), false},
		{"mail", l("b@x", "a@x"), l("a@x", "b@x"), true},
		{"mail", l("a@x"), l("a@x", "b@x"), false},
		{"mail", l("A@x"), l("a@x"), false},
		{"mail", nil, nil, true},
		{"member", l("UID=Alice, OU=People,dc=x"), l("uid=alice,ou=people,dc=x"), true},
		{"description", l("UID=Alice, OU=People,dc=x"), l("uid=alice,ou=people,dc=x"), false},
	}
	for _, c := range cases {
		if got, err := equivalent(c.name, c.want, c.have, schema); err != nil || got != c.same {
			t.Errorf("equivalent(%s, %q, %q) = %v, %v; want %v", c.name, c.want, c.have, got, err, c.same)
		}
	}
	// スキーマが無ければ上位のクラスを許さない（Apply の判定は書く側に倒す）。
	if got, _ := equivalent("objectclass", l("inetOrgPerson"), l("inetOrgPerson", "top"), nil); got {
		t.Error("スキーマ無しで上位のクラスを許した")
	}
}
