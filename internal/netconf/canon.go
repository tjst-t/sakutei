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

// canon.go — 設定の XML の正規形（Backlog #141）。
//
// 比べるのは「意味」で、書き方の違いはずれにしない:
//
//   - 名前空間: 接頭辞は捨て、名前空間の URI で比べる（xmlns:if="…" と
//     xmlns="…" は同じ）。要素の名前空間は親と違うときだけ xmlns="…" で書く。
//   - 値の中の接頭辞（identityref など。"ianaift:ethernetCsmacd"）は、その
//     接頭辞がその場で宣言されていれば {URI}名前 に書き換える（機器は自分の
//     接頭辞で返す）。宣言されていない接頭辞の値（時刻・IPv6 など）はそのまま。
//   - 空白: 子要素を持つ要素の間の空白は捨てる。値（葉の文字）は前後の空白を
//     落とす。<a/> と <a></a> は同じ。
//   - 属性: xmlns の宣言は捨て、残りを（名前空間, 名前）の順に並べる。名前空間
//     つきの属性は、その要素で a0・a1… の接頭辞を宣言して書く（正規形も XML
//     として読み直せる）。比べるのは宣言に書いた属性だけ（Backlog #208）:
//     機器の側の属性は、宣言の同じ道（ordered と同じ書き方）の要素に同じ名前
//     （名前空間, 名前）の属性があるときだけ残す（onlyDeclaredAttrs）。機器が
//     付ける印（commit の時刻・利用者など）でずれにしない。
//   - 中身の無い最上位の要素（子も値も無い。名前空間の有無も属性も問わない）は
//     部分木が無いことを表す（contentNodes）。部分木が無いときに空の
//     <configuration/> を返す機器がある。
//   - 兄弟の順: 既定では順を問わない（比べるときに並べ替える）。順に意味の
//     ある列（ordered-by user のリストなど）は ordered で名指しした道
//     （"acl/aces/ace" のように、部分木の頭からの要素の名前を / でつなぐ）の
//     要素だけ、同じ名前の兄弟の間で元の順を保つ。
//   - コメント・処理命令・DOCTYPE は捨てる。
//
// 正規形は比べるためと見せるための形で、機器に送らない（送る形は edit.go）。

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// node は XML の要素 1 つ。
type node struct {
	name     xml.Name // Space は名前空間の URI
	attrs    []xml.Attr
	children []*node
	text     string // 前後の空白を落とした文字（子を持つ要素では空白だけなら空）
}

// maxDepth は XML の入れ子の深さの上限（壊れた・悪意のある入力で止まる）。
const maxDepth = 256

// qnameValue は接頭辞つきの値（prefix:name）。
var qnameValue = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9._-]*):([A-Za-z_][A-Za-z0-9._-]*)$`)

// parseXML は data の最上位の要素の列を読む（いくつあってもよい。文書の
// 外の文字は空白だけ）。
func parseXML(data []byte) ([]*node, error) {
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = true
	var top []*node
	var stack []*node
	var texts []*strings.Builder
	// scopes は接頭辞 → URI の宣言（要素ごとに積む）。
	scopes := []map[string]string{{"xml": "http://www.w3.org/XML/1998/namespace"}}
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if len(stack) >= maxDepth {
				return nil, fmt.Errorf("the XML is nested deeper than %d", maxDepth)
			}
			scope := map[string]string{}
			for k, v := range scopes[len(scopes)-1] {
				scope[k] = v
			}
			n := &node{name: t.Name}
			for _, a := range t.Attr {
				switch {
				case a.Name.Space == "xmlns":
					scope[a.Name.Local] = a.Value
				case a.Name.Space == "" && a.Name.Local == "xmlns":
				default:
					n.attrs = append(n.attrs, a)
				}
			}
			for i := range n.attrs {
				n.attrs[i].Value = resolveQName(n.attrs[i].Value, scope)
			}
			scopes = append(scopes, scope)
			if len(stack) == 0 {
				top = append(top, n)
			} else {
				p := stack[len(stack)-1]
				p.children = append(p.children, n)
			}
			stack = append(stack, n)
			texts = append(texts, &strings.Builder{})
		case xml.EndElement:
			n := stack[len(stack)-1]
			text := strings.TrimSpace(texts[len(texts)-1].String())
			n.text = resolveQName(text, scopes[len(scopes)-1])
			stack, texts, scopes = stack[:len(stack)-1], texts[:len(texts)-1], scopes[:len(scopes)-1]
		case xml.CharData:
			if len(stack) == 0 {
				if len(bytes.TrimSpace(t)) > 0 {
					return nil, errors.New("text outside of an element")
				}
				continue
			}
			texts[len(texts)-1].Write(t)
		}
	}
	if len(stack) > 0 {
		return nil, errors.New("the XML ends inside an element")
	}
	return top, nil
}

// resolveQName は v が prefix:name で prefix が宣言されていれば {URI}name にする。
func resolveQName(v string, scope map[string]string) string {
	m := qnameValue.FindStringSubmatch(v)
	if m == nil {
		return v
	}
	uri, ok := scope[m[1]]
	if !ok || uri == "" {
		return v
	}
	return "{" + uri + "}" + m[2]
}

// orderedSet は順を保つ道の集合（"a/b/c"）。
type orderedSet map[string]bool

// ParseOrdered は "acl/aces/ace, x/y" を読む。道は要素の名前（接頭辞なし）を
// "/" でつないだもの。
func ParseOrdered(s string) (orderedSet, error) {
	out := orderedSet{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		for _, seg := range strings.Split(p, "/") {
			if !xmlName.MatchString(seg) {
				return nil, fmt.Errorf("ordered: %q: want element names joined by \"/\" (e.g. acl/aces/ace)", p)
			}
		}
		out[p] = true
	}
	return out, nil
}

var xmlName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9._-]*$`)

// canonical は top の正規形の文字列。sorted なら順を問わない兄弟を並べ替える
// （ordered の道の要素は同じ名前の兄弟の間で元の順のまま）。sorted でなければ
// 元の順のまま書く（観測の値として見せる形）。
func canonical(top []*node, sorted bool, ordered orderedSet) string {
	var b strings.Builder
	for _, c := range arrange(top, "", "", sorted, ordered) {
		b.WriteString(c.canon)
	}
	return b.String()
}

// placed は並べた兄弟の 1 つ。
type placed struct {
	n     *node
	path  string
	canon string
}

// arrange は兄弟の列を書く順に並べ、それぞれの正規形を作る。sorted なら
// （名前空間, 名前）の順、同じ名前の中は正規形の順（ordered の道は元の順）。
func arrange(nodes []*node, parentPath, parentNS string, sorted bool, ordered orderedSet) []placed {
	type item struct {
		placed
		key string // 名前空間と名前
		seq int    // 同じ名前の兄弟の中の元の順
		ord bool
	}
	items := make([]item, len(nodes))
	seqByName := map[string]int{}
	for i, n := range nodes {
		path := n.name.Local
		if parentPath != "" {
			path = parentPath + "/" + n.name.Local
		}
		key := n.name.Space + "\x00" + n.name.Local
		items[i] = item{
			placed: placed{n: n, path: path, canon: canonicalNode(n, path, parentNS, sorted, ordered)},
			key:    key, seq: seqByName[key], ord: ordered[path],
		}
		seqByName[key]++
	}
	if sorted {
		sort.SliceStable(items, func(i, j int) bool {
			a, b := items[i], items[j]
			if a.key != b.key {
				return a.key < b.key
			}
			if a.ord {
				return a.seq < b.seq
			}
			return a.canon < b.canon
		})
	}
	out := make([]placed, len(items))
	for i, it := range items {
		out[i] = it.placed
	}
	return out
}

func canonicalNode(n *node, path, parentNS string, sorted bool, ordered orderedSet) string {
	var b strings.Builder
	b.WriteByte('<')
	b.WriteString(n.name.Local)
	if n.name.Space != parentNS {
		b.WriteString(` xmlns="`)
		escapeAttr(&b, n.name.Space)
		b.WriteByte('"')
	}
	attrs := append([]xml.Attr(nil), n.attrs...)
	sort.Slice(attrs, func(i, j int) bool {
		if attrs[i].Name.Space != attrs[j].Name.Space {
			return attrs[i].Name.Space < attrs[j].Name.Space
		}
		return attrs[i].Name.Local < attrs[j].Name.Local
	})
	prefixes := map[string]string{}
	for _, a := range attrs {
		if a.Name.Space != "" && prefixes[a.Name.Space] == "" {
			prefixes[a.Name.Space] = fmt.Sprintf("a%d", len(prefixes))
			b.WriteString(" xmlns:" + prefixes[a.Name.Space] + `="`)
			escapeAttr(&b, a.Name.Space)
			b.WriteByte('"')
		}
	}
	for _, a := range attrs {
		b.WriteByte(' ')
		if a.Name.Space != "" {
			b.WriteString(prefixes[a.Name.Space] + ":")
		}
		b.WriteString(a.Name.Local)
		b.WriteString(`="`)
		escapeAttr(&b, a.Value)
		b.WriteByte('"')
	}
	children := arrange(n.children, path, n.name.Space, sorted, ordered)
	if len(children) == 0 && n.text == "" {
		b.WriteString("/>")
		return b.String()
	}
	b.WriteByte('>')
	if n.text != "" {
		_ = xml.EscapeText(&b, []byte(n.text))
	}
	for _, c := range children {
		b.WriteString(c.canon)
	}
	b.WriteString("</")
	b.WriteString(n.name.Local)
	b.WriteByte('>')
	return b.String()
}

func escapeAttr(b *strings.Builder, s string) {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	b.Write(buf.Bytes())
}

// Canonical は XML の data（最上位の要素がいくつあってもよい）の正規形。
// sorted なら順を問わない兄弟を並べ替える（比べる形）、そうでなければ元の順
// （見せる形）。ordered は ParseOrdered の書式。
func Canonical(data string, sorted bool, ordered string) (string, error) {
	set, err := ParseOrdered(ordered)
	if err != nil {
		return "", err
	}
	top, err := parseXML([]byte(data))
	if err != nil {
		return "", err
	}
	return canonical(top, sorted, set), nil
}

// leafLines は top を「道 = 値」の行の列にする（ずれの説明に使う）。sorted
// の正規形と同じ順。
func leafLines(top []*node, ordered orderedSet) []string {
	var out []string
	var walk func(nodes []*node, parentPath, parentNS string)
	walk = func(nodes []*node, parentPath, parentNS string) {
		for _, c := range arrange(nodes, parentPath, parentNS, true, ordered) {
			if len(c.n.children) == 0 {
				out = append(out, c.path+" = "+c.n.text)
				continue
			}
			walk(c.n.children, c.path, c.n.name.Space)
		}
	}
	walk(top, "", "")
	return out
}

// hasContent は n が子か値を持つか（属性だけの要素は中身が無い）。
func hasContent(n *node) bool { return len(n.children) > 0 || n.text != "" }

// contentNodes は最上位の要素のうち中身のあるものだけを返す。部分木が機器に
// 無いとき、名前空間の無い（rpc-reply の base:1.0 を受け継ぐ）空の
// <configuration/> を返す機器がある（#208）。中身の無い要素は、名前空間の
// 有無を問わず「無い」と見る。
func contentNodes(top []*node) []*node {
	var out []*node
	for _, n := range top {
		if hasContent(n) {
			out = append(out, n)
		}
	}
	return out
}

// attrSet は宣言に書いた属性（要素の道と属性の名前空間・名前）の集合。
type attrSet map[string]bool

func attrKey(path string, name xml.Name) string {
	return path + "\x00" + name.Space + "\x00" + name.Local
}

// declaredAttrs は宣言 declared に書いた属性の集合。
func declaredAttrs(declared []*node) attrSet {
	set := attrSet{}
	var walk func(nodes []*node, parentPath string)
	walk = func(nodes []*node, parentPath string) {
		for _, n := range nodes {
			path := joinPath(parentPath, n.name.Local)
			for _, a := range n.attrs {
				set[attrKey(path, a.Name)] = true
			}
			walk(n.children, path)
		}
	}
	walk(declared, "")
	return set
}

// onlyDeclaredAttrs は nodes の写しで、属性を set にあるものだけに絞る
// （宣言に無い属性は比べない、#208）。nodes は変えない。
func onlyDeclaredAttrs(nodes []*node, set attrSet) []*node {
	var cp func(nodes []*node, parentPath string) []*node
	cp = func(nodes []*node, parentPath string) []*node {
		if nodes == nil {
			return nil
		}
		out := make([]*node, len(nodes))
		for i, n := range nodes {
			path := joinPath(parentPath, n.name.Local)
			c := &node{name: n.name, text: n.text}
			for _, a := range n.attrs {
				if set[attrKey(path, a.Name)] {
					c.attrs = append(c.attrs, a)
				}
			}
			c.children = cp(n.children, path)
			out[i] = c
		}
		return out
	}
	return cp(nodes, "")
}

// attrLines は top の属性を「道 @名前 = 値」の行の列にする（属性だけのずれの
// 説明に使う）。名前空間つきの属性の名前は {URI}名前。
func attrLines(top []*node, ordered orderedSet) []string {
	var out []string
	var walk func(nodes []*node, parentPath, parentNS string)
	walk = func(nodes []*node, parentPath, parentNS string) {
		for _, c := range arrange(nodes, parentPath, parentNS, true, ordered) {
			attrs := append([]xml.Attr(nil), c.n.attrs...)
			sort.Slice(attrs, func(i, j int) bool {
				if attrs[i].Name.Space != attrs[j].Name.Space {
					return attrs[i].Name.Space < attrs[j].Name.Space
				}
				return attrs[i].Name.Local < attrs[j].Name.Local
			})
			for _, a := range attrs {
				name := a.Name.Local
				if a.Name.Space != "" {
					name = "{" + a.Name.Space + "}" + name
				}
				out = append(out, c.path+" @"+name+" = "+a.Value)
			}
			walk(c.n.children, c.path, c.n.name.Space)
		}
	}
	walk(top, "", "")
	return out
}
