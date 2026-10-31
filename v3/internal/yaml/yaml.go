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

// Package yaml は intent とつなぎ方の設定を読む、標準ライブラリだけの YAML の
// 部分集合のパーサ。値ごとに行番号を残し、読み込みで断るときに宣言の場所を
// 示す（E-M3）。JSON も同じ Node にする（E-O4：設定は 1 つのファイルで、形は
// YAML か JSON）。
//
// 受けるもの：ブロックのマップとリスト、1 行に収まるフローの {…}・[…]、
// 引用符の文字列、整数・true/false・null、"#" のコメント、先頭の "---" 1 つ。
// 黙って読み違えないよう、それ以外（アンカー・エイリアス・タグ・ブロック
// スカラー・複数行のフロー・複数ドキュメント・タブのインデント・マージキー）は
// 行番号つきで断る。
//
// 型は保守的に読む：bool は true/false だけ（yes/no/on/off は文字列）、数は
// 整数だけ（1.10 や 1e3、先頭が 0 の 007 は文字列）。
package yaml

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Kind は Node の形。
type Kind int

const (
	Scalar Kind = iota
	Map
	Seq
)

// Node は読んだ値 1 つ。Line は 1 始まりの行番号（JSON から読んだものは 0）。
type Node struct {
	Kind   Kind
	Line   int
	Scalar any // string・int・bool・nil
	Keys   []string
	Map    map[string]*Node
	Seq    []*Node
	// KeyLine はマップのキーの行（値が次の行から始まるときに値の行と違う）。
	KeyLine map[string]int
}

// LineOf はキー k の行。無ければマップの行。
func (n *Node) LineOf(k string) int {
	if l, ok := n.KeyLine[k]; ok {
		return l
	}
	return n.Line
}

func (n *Node) set(k string, line int, v *Node) {
	if n.KeyLine == nil {
		n.KeyLine = map[string]int{}
	}
	n.Keys = append(n.Keys, k)
	n.Map[k] = v
	n.KeyLine[k] = line
}

// Value は Node を map[string]any・[]any・スカラーにする。
func (n *Node) Value() any {
	switch n.Kind {
	case Map:
		m := make(map[string]any, len(n.Map))
		for k, v := range n.Map {
			m[k] = v.Value()
		}
		return m
	case Seq:
		l := make([]any, len(n.Seq))
		for i, v := range n.Seq {
			l[i] = v.Value()
		}
		return l
	}
	return n.Scalar
}

// KindName は誤りの文に使う形の名前。
func (n *Node) KindName() string {
	switch n.Kind {
	case Map:
		return "マップ"
	case Seq:
		return "リスト"
	}
	if n.Scalar == nil {
		return "null"
	}
	return "スカラー"
}

// Parse は YAML の部分集合を読む。トップレベルはマップでなければならない。
// 空のファイルは空のマップ。
func Parse(data []byte) (*Node, error) {
	p := &parser{}
	if err := p.tokenize(string(data)); err != nil {
		return nil, err
	}
	if len(p.lines) == 0 {
		return &Node{Kind: Map, Line: 1, Map: map[string]*Node{}}, nil
	}
	if p.lines[0].indent != 0 {
		return nil, fmt.Errorf("%d 行目: 先頭の行がインデントされている", p.lines[0].num)
	}
	n, err := p.block(0)
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.lines) {
		return nil, fmt.Errorf("%d 行目: インデントがそろっていない", p.lines[p.pos].num)
	}
	if n.Kind != Map {
		return nil, fmt.Errorf("トップレベルはマップでなければならない")
	}
	return n, nil
}

// ParseJSON は JSON を Node にする。マップのキーの順は名前順。
func ParseJSON(data []byte) (*Node, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("JSON として読めない: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("JSON の値が 2 つ以上ある")
	}
	n := fromJSON(v)
	if n.Kind != Map {
		return nil, fmt.Errorf("トップレベルはマップでなければならない")
	}
	return n, nil
}

func fromJSON(v any) *Node {
	switch x := v.(type) {
	case map[string]any:
		n := &Node{Kind: Map, Map: make(map[string]*Node, len(x))}
		for k, e := range x {
			n.Keys = append(n.Keys, k)
			n.Map[k] = fromJSON(e)
		}
		sort.Strings(n.Keys)
		return n
	case []any:
		n := &Node{Kind: Seq}
		for _, e := range x {
			n.Seq = append(n.Seq, fromJSON(e))
		}
		return n
	case json.Number:
		// 整数だけを数にし、小数は文字列のまま（YAML と同じ読み方）。
		if i, err := strconv.Atoi(x.String()); err == nil {
			return &Node{Kind: Scalar, Scalar: i}
		}
		return &Node{Kind: Scalar, Scalar: x.String()}
	}
	return &Node{Kind: Scalar, Scalar: v}
}

type line struct {
	num    int
	indent int
	body   string
}

type parser struct {
	lines []line
	pos   int
}

func (p *parser) tokenize(text string) error {
	docSep := false
	for i, raw := range strings.Split(text, "\n") {
		num := i + 1
		raw = strings.TrimRight(raw, "\r")
		indent := 0
		for indent < len(raw) && raw[indent] == ' ' {
			indent++
		}
		content := raw[indent:]
		if strings.HasPrefix(content, "\t") || (strings.Contains(content, "\t") && !strings.HasPrefix(strings.TrimSpace(content), "#")) {
			return fmt.Errorf("%d 行目: タブは使えない（半角の空白にする）", num)
		}
		trimmed := strings.TrimSpace(content)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if trimmed == "---" && indent == 0 {
			if docSep || len(p.lines) > 0 {
				return fmt.Errorf("%d 行目: ドキュメントは 1 つだけ（2 つ目の '---'）", num)
			}
			docSep = true
			continue
		}
		if trimmed == "..." {
			return fmt.Errorf("%d 行目: ドキュメントの終わりの '...' は使えない", num)
		}
		if strings.HasPrefix(trimmed, "<<") {
			return fmt.Errorf("%d 行目: マージキー（<<）は使えない", num)
		}
		body := strings.TrimRight(stripComment(content), " ")
		p.lines = append(p.lines, line{num: num, indent: indent, body: body})
	}
	return nil
}

// stripComment は引用符の外の「行頭か空白のあとの #」から後ろを落とす。
func stripComment(s string) string {
	inS, inD := false, false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			if !inD {
				inS = !inS
			}
		case '"':
			if !inS {
				inD = !inD
			} else {
				continue
			}
		case '\\':
			if inD {
				i++
			}
		case '#':
			if !inS && !inD && (i == 0 || s[i-1] == ' ') {
				return s[:i]
			}
		}
	}
	return s
}

func isSeqItem(body string) bool { return body == "-" || strings.HasPrefix(body, "- ") }

func (p *parser) block(indent int) (*Node, error) {
	if isSeqItem(p.lines[p.pos].body) {
		return p.seq(indent)
	}
	return p.mapping(indent)
}

func (p *parser) mapping(indent int) (*Node, error) {
	n := &Node{Kind: Map, Line: p.lines[p.pos].num, Map: map[string]*Node{}}
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if l.indent < indent {
			break
		}
		if l.indent > indent {
			return nil, fmt.Errorf("%d 行目: 思わぬインデント", l.num)
		}
		if isSeqItem(l.body) {
			return nil, fmt.Errorf("%d 行目: マップの途中にリストの項目がある", l.num)
		}
		key, rest, ok := splitKey(l.body)
		if !ok {
			return nil, fmt.Errorf("%d 行目: 'キー: 値' の形でない: %q", l.num, l.body)
		}
		if _, dup := n.Map[key]; dup {
			return nil, fmt.Errorf("%d 行目: キー %q が 2 回ある", l.num, key)
		}
		p.pos++
		var v *Node
		var err error
		switch {
		case rest != "":
			v, err = inline(rest, l.num)
		case p.pos < len(p.lines) && p.lines[p.pos].indent > indent:
			v, err = p.block(p.lines[p.pos].indent)
		case p.pos < len(p.lines) && p.lines[p.pos].indent == indent && isSeqItem(p.lines[p.pos].body):
			// "key:" の下に同じ桁で "- " が並ぶ書き方。
			v, err = p.seq(indent)
		default:
			v = &Node{Kind: Scalar, Line: l.num}
		}
		if err != nil {
			return nil, err
		}
		n.set(key, l.num, v)
	}
	return n, nil
}

func (p *parser) seq(indent int) (*Node, error) {
	n := &Node{Kind: Seq, Line: p.lines[p.pos].num}
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if l.indent < indent {
			break
		}
		if l.indent > indent {
			return nil, fmt.Errorf("%d 行目: 思わぬインデント", l.num)
		}
		if !isSeqItem(l.body) {
			break
		}
		item := strings.TrimLeft(strings.TrimPrefix(l.body, "-"), " ")
		var v *Node
		var err error
		switch {
		case item == "":
			p.pos++
			if p.pos < len(p.lines) && p.lines[p.pos].indent > indent {
				v, err = p.block(p.lines[p.pos].indent)
			} else {
				v = &Node{Kind: Scalar, Line: l.num}
			}
		case startsMapEntry(item):
			// "- key: value" はその桁から始まるマップ。行を書き換えて読み直す。
			p.lines[p.pos] = line{num: l.num, indent: l.indent + len(l.body) - len(item), body: item}
			v, err = p.mapping(p.lines[p.pos].indent)
		default:
			p.pos++
			v, err = inline(item, l.num)
		}
		if err != nil {
			return nil, err
		}
		n.Seq = append(n.Seq, v)
	}
	return n, nil
}

func startsMapEntry(s string) bool {
	if s[0] == '{' || s[0] == '[' {
		return false
	}
	_, _, ok := splitKey(s)
	return ok
}

// splitKey は引用符を見て最初の「": " か行末の ":"」でキーと値に分ける。
func splitKey(body string) (key, rest string, ok bool) {
	inS, inD := false, false
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '\'':
			if !inD {
				inS = !inS
			}
		case '"':
			if !inS {
				inD = !inD
			}
		case ':':
			if inS || inD {
				continue
			}
			if i+1 == len(body) || body[i+1] == ' ' {
				key = unquoteKey(body[:i])
				return key, strings.TrimSpace(body[i+1:]), key != ""
			}
		}
	}
	return "", "", false
}

func unquoteKey(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		return s[1 : len(s)-1]
	}
	return s
}

// inline は 1 行の値（スカラーかフロー）を読む。
func inline(s string, num int) (*Node, error) {
	if s[0] == '{' || s[0] == '[' {
		f := &flow{s: s, num: num}
		n, err := f.value()
		if err != nil {
			return nil, err
		}
		f.skip()
		if f.i != len(f.s) {
			return nil, fmt.Errorf("%d 行目: フローの値のあとに余分なものがある: %q", num, f.s[f.i:])
		}
		return n, nil
	}
	v, err := scalar(s, num)
	if err != nil {
		return nil, err
	}
	return &Node{Kind: Scalar, Line: num, Scalar: v}, nil
}

// flow は 1 行に収まる {…}・[…] を読む。
type flow struct {
	s   string
	i   int
	num int
}

func (f *flow) skip() {
	for f.i < len(f.s) && f.s[f.i] == ' ' {
		f.i++
	}
}

func (f *flow) errorf(format string, a ...any) error {
	return fmt.Errorf("%d 行目: "+format, append([]any{f.num}, a...)...)
}

func (f *flow) value() (*Node, error) {
	f.skip()
	if f.i >= len(f.s) {
		return nil, f.errorf("フローの値が途中で終わっている（フローは 1 行に書く）")
	}
	switch f.s[f.i] {
	case '{':
		return f.mapping()
	case '[':
		return f.seq()
	}
	tok, err := f.token()
	if err != nil {
		return nil, err
	}
	v, err := scalar(tok, f.num)
	if err != nil {
		return nil, err
	}
	return &Node{Kind: Scalar, Line: f.num, Scalar: v}, nil
}

// token は引用符の文字列か、",]}" の手前までの裸の文字列を返す（引用符は残す）。
func (f *flow) token() (string, error) {
	start := f.i
	if q := f.s[f.i]; q == '"' || q == '\'' {
		f.i++
		for f.i < len(f.s) {
			c := f.s[f.i]
			if c == '\\' && q == '"' {
				f.i += 2
				continue
			}
			f.i++
			if c == q {
				if q == '\'' && f.i < len(f.s) && f.s[f.i] == '\'' {
					f.i++
					continue
				}
				return f.s[start:f.i], nil
			}
		}
		return "", f.errorf("引用符が閉じていない")
	}
	for f.i < len(f.s) && !strings.ContainsRune(",]}", rune(f.s[f.i])) {
		f.i++
	}
	return strings.TrimSpace(f.s[start:f.i]), nil
}

func (f *flow) mapping() (*Node, error) {
	n := &Node{Kind: Map, Line: f.num, Map: map[string]*Node{}}
	f.i++ // {
	f.skip()
	if f.i < len(f.s) && f.s[f.i] == '}' {
		f.i++
		return n, nil
	}
	for {
		f.skip()
		if f.i >= len(f.s) {
			return nil, f.errorf("'}' が無い（フローは 1 行に書く）")
		}
		var key string
		if q := f.s[f.i]; q == '"' || q == '\'' {
			tok, err := f.token()
			if err != nil {
				return nil, err
			}
			key = unquoteKey(tok)
		} else {
			start := f.i
			for f.i < len(f.s) && f.s[f.i] != ':' && !strings.ContainsRune(",]}", rune(f.s[f.i])) {
				f.i++
			}
			key = strings.TrimSpace(f.s[start:f.i])
		}
		if key == "" || f.i >= len(f.s) || f.s[f.i] != ':' {
			return nil, f.errorf("フローのマップの項目が 'キー: 値' の形でない")
		}
		f.i++ // :
		if _, dup := n.Map[key]; dup {
			return nil, f.errorf("キー %q が 2 回ある", key)
		}
		v, err := f.value()
		if err != nil {
			return nil, err
		}
		n.set(key, f.num, v)
		f.skip()
		if f.i >= len(f.s) {
			return nil, f.errorf("'}' が無い（フローは 1 行に書く）")
		}
		switch f.s[f.i] {
		case ',':
			f.i++
		case '}':
			f.i++
			return n, nil
		default:
			return nil, f.errorf("フローのマップに思わぬ文字 %q", f.s[f.i])
		}
	}
}

func (f *flow) seq() (*Node, error) {
	n := &Node{Kind: Seq, Line: f.num}
	f.i++ // [
	f.skip()
	if f.i < len(f.s) && f.s[f.i] == ']' {
		f.i++
		return n, nil
	}
	for {
		v, err := f.value()
		if err != nil {
			return nil, err
		}
		n.Seq = append(n.Seq, v)
		f.skip()
		if f.i >= len(f.s) {
			return nil, f.errorf("']' が無い（フローは 1 行に書く）")
		}
		switch f.s[f.i] {
		case ',':
			f.i++
		case ']':
			f.i++
			return n, nil
		default:
			return nil, f.errorf("フローのリストに思わぬ文字 %q", f.s[f.i])
		}
	}
}

// scalar はスカラーのトークンを値にする。
func scalar(tok string, num int) (any, error) {
	tok = strings.TrimSpace(tok)
	if len(tok) >= 2 && tok[0] == '"' && tok[len(tok)-1] == '"' {
		return unquoteDouble(tok[1:len(tok)-1], num)
	}
	if len(tok) >= 2 && tok[0] == '\'' && tok[len(tok)-1] == '\'' {
		return strings.ReplaceAll(tok[1:len(tok)-1], "''", "'"), nil
	}
	if tok != "" {
		switch tok[0] {
		case '&':
			return nil, fmt.Errorf("%d 行目: アンカー（&）は使えない", num)
		case '*':
			return nil, fmt.Errorf("%d 行目: エイリアス（*）は使えない", num)
		case '!':
			return nil, fmt.Errorf("%d 行目: タグ（!）は使えない", num)
		case '|', '>':
			return nil, fmt.Errorf("%d 行目: ブロックスカラー（|・>）は使えない", num)
		case '"', '\'':
			return nil, fmt.Errorf("%d 行目: 引用符が閉じていない", num)
		case '{', '[':
			return nil, fmt.Errorf("%d 行目: フローの値が途中から始まっている", num)
		}
	}
	switch tok {
	case "", "null", "~", "Null", "NULL":
		return nil, nil
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	if isInt(tok) {
		if n, err := strconv.Atoi(tok); err == nil {
			return n, nil
		}
	}
	return tok, nil
}

// isInt は整数の字句か。先頭が 0 の 2 桁以上（007）は文字列のまま残す。
func isInt(tok string) bool {
	i := 0
	if tok[0] == '+' || tok[0] == '-' {
		i = 1
	}
	if i >= len(tok) || (tok[i] == '0' && i+1 < len(tok)) {
		return false
	}
	for ; i < len(tok); i++ {
		if tok[i] < '0' || tok[i] > '9' {
			return false
		}
	}
	return true
}

func unquoteDouble(s string, num int) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(s) {
			return "", fmt.Errorf("%d 行目: 文字列の終わりのエスケープが誤り", num)
		}
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		default:
			return "", fmt.Errorf("%d 行目: 知らないエスケープ \\%c", num, s[i])
		}
	}
	return b.String(), nil
}
