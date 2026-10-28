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

package netconf

// edit_test.go — 宣言を部分木の置き換えにする edit-config の中身（Backlog #175）。
//
// Tags:
//   [AC-175-1-1] content match で選ぶ部分木はそのエントリを replace（containment は
//                中に入り、キーを写す）。部分木の外には触れない
//   [AC-175-1-2] selection の部分木は宣言の要素を replace、機器にだけある要素を
//                キーだけで remove。ordered の道には yang:insert="last"
//   [AC-175-1-3] 値の中の接頭辞は宣言し直して送る。filter の選ばない宣言はエラー

import (
	"strings"
	"testing"
)

func mustParse(t *testing.T, x string) []*node {
	t.Helper()
	n, err := parseXML([]byte(x))
	if err != nil {
		t.Fatalf("parse %q: %v", x, err)
	}
	return n
}

const nacm = "urn:ietf:params:xml:ns:yang:ietf-netconf-acm"

func TestBuildEdit_ContentMatch(t *testing.T) {
	// [AC-175-1-1]
	filter := mustParse(t, `<nacm xmlns="`+nacm+`"><rule-list><name>it</name></rule-list></nacm>`)
	want := mustParse(t, `<nacm xmlns="`+nacm+`"><rule-list><name>it</name><rule><name>r1</name></rule></rule-list></nacm>`)
	cur := mustParse(t, `<nacm xmlns="`+nacm+`"><rule-list><name>it</name><rule><name>r2</name></rule></rule-list></nacm>`)
	got, err := buildEdit(filter, want, cur, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantEdit := `<nacm xmlns="` + nacm + `"><rule-list xmlns:nc="` + BaseNS + `" nc:operation="replace"><name>it</name><rule><name>r1</name></rule></rule-list></nacm>`
	if got != wantEdit {
		t.Fatalf("[AC-175-1-1] edit =\n%s\nwant\n%s", got, wantEdit)
	}

	// containment with a key: the entry is entered, the key copied, and only
	// the selected leaf replaced.
	filter = mustParse(t, `<interfaces xmlns="u:if"><interface><name>up</name><description/></interface></interfaces>`)
	want = mustParse(t, `<interfaces xmlns="u:if"><interface><description>new</description><name>up</name></interface></interfaces>`)
	cur = mustParse(t, `<interfaces xmlns="u:if"><interface><name>up</name><description>old</description></interface></interfaces>`)
	got, err = buildEdit(filter, want, cur, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantEdit = `<interfaces xmlns="u:if"><interface><name>up</name><description xmlns:nc="` + BaseNS + `" nc:operation="replace">new</description></interface></interfaces>`
	if got != wantEdit {
		t.Fatalf("[AC-175-1-1] edit =\n%s\nwant\n%s", got, wantEdit)
	}
}

func TestBuildEdit_SelectionRemoveAndOrder(t *testing.T) {
	// [AC-175-1-2]
	filter := mustParse(t, `<acl xmlns="u:acl"><ace/></acl>`)
	want := mustParse(t, `<acl xmlns="u:acl"><ace><name>b</name><act>deny</act></ace><ace><name>a</name><act>permit</act></ace></acl>`)
	cur := mustParse(t, `<acl xmlns="u:acl"><ace><name>a</name><act>permit</act></ace><ace><name>c</name><act>deny</act></ace></acl>`)
	got, err := buildEdit(filter, want, cur, orderedSet{"acl/ace": true})
	if err != nil {
		t.Fatal(err)
	}
	op := ` xmlns:nc="` + BaseNS + `" nc:operation="replace" xmlns:yang="` + YANGNS + `" yang:insert="last"`
	wantEdit := `<acl xmlns="u:acl">` +
		`<ace` + op + `><name>b</name><act>deny</act></ace>` +
		`<ace` + op + `><name>a</name><act>permit</act></ace>` +
		`<ace xmlns:nc="` + BaseNS + `" nc:operation="remove"><name>c</name></ace></acl>`
	if got != wantEdit {
		t.Fatalf("[AC-175-1-2] edit =\n%s\nwant\n%s", got, wantEdit)
	}
	// Without ordered: no insert.
	got, _ = buildEdit(filter, want, cur, nil)
	if strings.Contains(got, "insert") {
		t.Fatalf("[AC-175-1-2] insert without ordered: %s", got)
	}
}

func TestBuildEdit_QNameAndUnselected(t *testing.T) {
	// [AC-175-1-3]
	filter := mustParse(t, `<interfaces xmlns="u:if"><interface><name>up</name></interface></interfaces>`)
	want := mustParse(t, `<interfaces xmlns="u:if"><interface><name>up</name><type xmlns:t="u:types">t:eth</type></interface></interfaces>`)
	got, err := buildEdit(filter, want, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `<type xmlns:s0="u:types">s0:eth</type>`) {
		t.Fatalf("[AC-175-1-3] edit = %s", got)
	}
	// The edit reads back as the declaration.
	back := mustParse(t, got)
	if canonical(stripOps(back), true, nil) != canonical(want, true, nil) {
		t.Fatalf("[AC-175-1-3] round trip:\n%s\n%s", canonical(back, true, nil), canonical(want, true, nil))
	}

	unselected := mustParse(t, `<interfaces xmlns="u:if"><interface><name>up</name></interface><other/></interfaces>`)
	if _, err := buildEdit(filter, unselected, nil, nil); err == nil || !strings.Contains(err.Error(), "not selected") {
		t.Fatalf("[AC-175-1-3] unselected: %v", err)
	}
}

func stripOps(nodes []*node) []*node {
	for _, n := range nodes {
		var attrs = n.attrs[:0]
		for _, a := range n.attrs {
			if a.Name.Space != BaseNS && a.Name.Space != YANGNS {
				attrs = append(attrs, a)
			}
		}
		n.attrs = attrs
		stripOps(n.children)
	}
	return nodes
}
