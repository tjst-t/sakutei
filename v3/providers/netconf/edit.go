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

// edit.go — 宣言を部分木の置き換えにする edit-config の中身（Backlog #175）。
//
// 置き換えるのは filter が選ぶ部分木だけで、その外（同じ親の下のほかの要素）
// には触らない。YANG を持たないので、どこで置き換えるかは filter の形で決める
// （RFC 6241 §6）:
//
//   - 子が content match（値つきの葉）だけの filter の要素・子の無い filter の
//     要素（selection）は「境目」: 宣言のその要素を operation="replace" で
//     送り、機器にだけあるその要素（同じ名前の兄弟）を operation="remove" で
//     消す。機器と宣言の要素の突き合わせは、content match があればその値、
//     無ければ機器の要素の最初の葉（YANG のリストのキーは最初に書かれる、
//     RFC 7950 §7.8.5）の名前と値で。葉の無い要素は名前だけで。
//   - それ以外（containment）の要素は、そのまま中に入る（content match の葉は
//     どのエントリかを示すために写す）。operation は付けない（既定の merge）。
//   - ordered で名指しした道の境目の要素には yang:insert="last" も付け、宣言の
//     順に並べ直す（ordered-by user の列）。
//   - filter が選ばない宣言の要素はエラー（送れば filter の外を書く）。
//
// 値の中の接頭辞（{URI}名前 に解決したもの）は、その要素で接頭辞を宣言して
// 書き戻す。

import (
	"encoding/xml"
	"fmt"
	"regexp"
	"strings"
)

// YANGNS は yang:insert の名前空間（RFC 7950 §7.8.6）。
const YANGNS = "urn:ietf:params:xml:ns:yang:1"

// resolvedQName は resolveQName が書き換えた値（{URI}名前）。
var resolvedQName = regexp.MustCompile(`^\{([^{}]+)\}([A-Za-z_][A-Za-z0-9._-]*)$`)

// buildEdit は filter の部分木を declared に置き換える edit-config の
// <config> の中身を作る。current は機器の今の部分木（filter で絞ったもの）。
func buildEdit(filter, declared, current []*node, ordered orderedSet) (string, error) {
	var b strings.Builder
	if err := editLevel(&b, filter, declared, current, "", "", ordered); err != nil {
		return "", err
	}
	return b.String(), nil
}

// contentMatch は filter の要素 f が content match（値つきの葉）か。
func contentMatch(f *node) bool { return len(f.children) == 0 && f.text != "" }

// boundary は f が置き換えの境目か（子が content match だけ、または子が無い）。
func boundary(f *node) bool {
	for _, c := range f.children {
		if !contentMatch(c) {
			return false
		}
	}
	return true
}

func sameName(f, n *node) bool {
	return f.name.Local == n.name.Local && (f.name.Space == "" || f.name.Space == n.name.Space)
}

// selects は filter の要素 f が n を選ぶか（名前と content match）。
func selects(f, n *node) bool {
	if !sameName(f, n) {
		return false
	}
	for _, cm := range f.children {
		if contentMatch(cm) && findChild(n, cm.name, cm.text) == nil {
			return false
		}
	}
	return true
}

// findChild は n の子で名前が name（text が "" でなければ値も）のもの。
func findChild(n *node, name xml.Name, text string) *node {
	for _, c := range n.children {
		if c.name == name && (text == "" || c.text == text) {
			return c
		}
	}
	return nil
}

// keyLeaves は境目の要素 c（機器の側）を指す葉: filter の content match、
// 無ければ c の最初の葉。葉が無ければ nil（名前だけで指す）。
func keyLeaves(f, c *node) []*node {
	var keys []*node
	for _, cm := range f.children {
		if contentMatch(cm) {
			if k := findChild(c, cm.name, cm.text); k != nil {
				keys = append(keys, k)
			}
		}
	}
	if len(keys) > 0 {
		return keys
	}
	if len(c.children) > 0 && len(c.children[0].children) == 0 {
		return []*node{c.children[0]}
	}
	return nil
}

// sameEntry は機器の要素 c と宣言の要素 d が同じエントリか。
func sameEntry(f, c, d *node) bool {
	if c.name != d.name {
		return false
	}
	for _, k := range keyLeaves(f, c) {
		if findChild(d, k.name, k.text) == nil {
			return false
		}
	}
	return true
}

func joinPath(parent, local string) string {
	if parent == "" {
		return local
	}
	return parent + "/" + local
}

func editLevel(b *strings.Builder, filter, declared, current []*node, parentPath, parentNS string, ordered orderedSet) error {
	used := map[*node]bool{}
	for _, f := range filter {
		if contentMatch(f) {
			continue // written by the parent as the entry's key
		}
		path := joinPath(parentPath, f.name.Local)
		var ds, cs []*node
		for _, d := range declared {
			if !used[d] && selects(f, d) {
				ds = append(ds, d)
				used[d] = true
			}
		}
		for _, c := range current {
			if selects(f, c) {
				cs = append(cs, c)
			}
		}
		if boundary(f) {
			for _, d := range ds {
				writeWire(b, d, parentNS, "replace", ordered[path])
			}
			for _, c := range cs {
				gone := true
				for _, d := range ds {
					if sameEntry(f, c, d) {
						gone = false
						break
					}
				}
				if gone {
					writeRemove(b, c, keyLeaves(f, c), parentNS)
				}
			}
			continue
		}
		// containment: pair the declared and the device's entries by name and
		// content match, and go in.
		type pair struct{ d, c *node }
		var pairs []pair
		for _, d := range ds {
			pairs = append(pairs, pair{d: d})
		}
		for _, c := range cs {
			matched := false
			for i := range pairs {
				if pairs[i].c == nil && pairs[i].d.name == c.name {
					pairs[i].c = c
					matched = true
					break
				}
			}
			if !matched {
				pairs = append(pairs, pair{c: c})
			}
		}
		for _, p := range pairs {
			n := p.d
			if n == nil {
				n = p.c
			}
			b.WriteString("<" + n.name.Local)
			if n.name.Space != parentNS {
				b.WriteString(` xmlns="`)
				escapeAttr(b, n.name.Space)
				b.WriteByte('"')
			}
			b.WriteByte('>')
			var keys []*node
			for _, cm := range f.children {
				if !contentMatch(cm) {
					continue
				}
				if k := findChild(n, cm.name, cm.text); k != nil {
					keys = append(keys, k)
				}
			}
			for _, k := range keys {
				writeWire(b, k, n.name.Space, "", false)
			}
			rest := func(x *node) []*node {
				if x == nil {
					return nil
				}
				var out []*node
				for _, c := range x.children {
					key := false
					for _, cm := range f.children {
						if contentMatch(cm) && c.name == cm.name && c.text == cm.text {
							key = true
						}
					}
					if key {
						continue
					}
					out = append(out, c)
				}
				return out
			}
			if err := editLevel(b, f.children, rest(p.d), rest(p.c), path, n.name.Space, ordered); err != nil {
				return err
			}
			b.WriteString("</" + n.name.Local + ">")
		}
	}
	for _, d := range declared {
		if !used[d] {
			return fmt.Errorf("config: <%s xmlns=%q> under %q is not selected by the subtree's filter", d.name.Local, d.name.Space, parentPath)
		}
	}
	return nil
}

// writeRemove は機器にだけある要素 c を、それを指す葉 keys だけで
// operation="remove" で書く。
func writeRemove(b *strings.Builder, c *node, keys []*node, parentNS string) {
	b.WriteString("<" + c.name.Local)
	if c.name.Space != parentNS {
		b.WriteString(` xmlns="`)
		escapeAttr(b, c.name.Space)
		b.WriteByte('"')
	}
	b.WriteString(` xmlns:nc="` + BaseNS + `" nc:operation="remove"`)
	if len(keys) == 0 {
		b.WriteString("/>")
		return
	}
	b.WriteByte('>')
	for _, k := range keys {
		writeWire(b, k, c.name.Space, "", false)
	}
	b.WriteString("</" + c.name.Local + ">")
}

// writeWire は n を機器に送る XML で書く。op が "" でなければ nc:operation、
// insertLast なら yang:insert="last" を付ける。
func writeWire(b *strings.Builder, n *node, parentNS, op string, insertLast bool) {
	b.WriteString("<" + n.name.Local)
	if n.name.Space != parentNS {
		b.WriteString(` xmlns="`)
		escapeAttr(b, n.name.Space)
		b.WriteByte('"')
	}
	if op != "" {
		b.WriteString(` xmlns:nc="` + BaseNS + `" nc:operation="` + op + `"`)
	}
	if insertLast {
		b.WriteString(` xmlns:yang="` + YANGNS + `" yang:insert="last"`)
	}
	// Prefixes for namespaced attributes and resolved values.
	prefixes := map[string]string{}
	declare := func(uri string) string {
		if p, ok := prefixes[uri]; ok {
			return p
		}
		p := fmt.Sprintf("s%d", len(prefixes))
		prefixes[uri] = p
		b.WriteString(" xmlns:" + p + `="`)
		escapeAttr(b, uri)
		b.WriteByte('"')
		return p
	}
	value := func(v string) string {
		if m := resolvedQName.FindStringSubmatch(v); m != nil {
			return declare(m[1]) + ":" + m[2]
		}
		return v
	}
	text := value(n.text)
	for _, a := range n.attrs {
		name := a.Name.Local
		if a.Name.Space != "" {
			name = declare(a.Name.Space) + ":" + name
		}
		v := value(a.Value)
		b.WriteString(" " + name + `="`)
		escapeAttr(b, v)
		b.WriteByte('"')
	}
	if len(n.children) == 0 && text == "" {
		b.WriteString("/>")
		return
	}
	b.WriteByte('>')
	_ = xml.EscapeText(b, []byte(text))
	for _, c := range n.children {
		writeWire(b, c, n.name.Space, "", false)
	}
	b.WriteString("</" + n.name.Local + ">")
}
