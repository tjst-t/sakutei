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

package condition

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/tjst-t/sakutei/v3/core"
)

type parser struct {
	src   string
	pos   int
	quant int // 量化の中の深さ（it を書けるか）
}

func (p *parser) errorf(format string, a ...any) error {
	return fmt.Errorf("%d 文字目: %s", p.pos+1, fmt.Sprintf(format, a...))
}

func (p *parser) eof() bool { return p.pos >= len(p.src) }

func (p *parser) rest() string { return p.src[p.pos:] }

func (p *parser) skip() {
	for !p.eof() && strings.ContainsRune(" \t\r\n", rune(p.src[p.pos])) {
		p.pos++
	}
}

func isIdent(c byte, first bool) bool {
	return c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || (!first && ('0' <= c && c <= '9'))
}

// peekWord は次の語（識別子）を返す。進めない。
func (p *parser) peekWord() string {
	p.skip()
	i := p.pos
	for i < len(p.src) && isIdent(p.src[i], i == p.pos) {
		i++
	}
	return p.src[p.pos:i]
}

func (p *parser) word() string {
	w := p.peekWord()
	p.pos += len(w)
	return w
}

func (p *parser) accept(s string) bool {
	p.skip()
	if strings.HasPrefix(p.rest(), s) {
		p.pos += len(s)
		return true
	}
	return false
}

func (p *parser) expect(s string) error {
	if !p.accept(s) {
		if p.eof() {
			return p.errorf("%q が要るが式が終わった", s)
		}
		return p.errorf("%q が要る（%q がある）", s, head(p.rest()))
	}
	return nil
}

func head(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}

func (p *parser) expr() (node, error) {
	first, err := p.and()
	if err != nil {
		return nil, err
	}
	xs := []node{first}
	for p.peekWord() == "or" {
		p.word()
		x, err := p.and()
		if err != nil {
			return nil, err
		}
		xs = append(xs, x)
	}
	if len(xs) == 1 {
		return first, nil
	}
	return orN{xs: xs}, nil
}

func (p *parser) and() (node, error) {
	first, err := p.not()
	if err != nil {
		return nil, err
	}
	xs := []node{first}
	for p.peekWord() == "and" {
		p.word()
		x, err := p.not()
		if err != nil {
			return nil, err
		}
		xs = append(xs, x)
	}
	if len(xs) == 1 {
		return first, nil
	}
	return andN{xs: xs}, nil
}

func (p *parser) not() (node, error) {
	if p.peekWord() == "not" {
		p.word()
		x, err := p.not()
		if err != nil {
			return nil, err
		}
		if _, ok := x.(countN); ok {
			return nil, p.errorf("count(...) は比較の中で使う")
		}
		return notN{x: x}, nil
	}
	return p.comparison()
}

var symbolOps = []string{"==", "!=", "<=", ">=", "<", ">"}

func (p *parser) comparison() (node, error) {
	l, err := p.term()
	if err != nil {
		return nil, err
	}
	op := ""
	p.skip()
	for _, s := range symbolOps {
		if strings.HasPrefix(p.rest(), s) {
			op = s
			p.pos += len(s)
			break
		}
	}
	if op == "" {
		if w := p.peekWord(); w == "in" || w == "contains" {
			op = p.word()
		}
	}
	if op == "" {
		return l, nil
	}
	r, err := p.term()
	if err != nil {
		return nil, err
	}
	_, lc := l.(countN)
	_, rc := r.(countN)
	switch {
	case lc && rc:
		return nil, p.errorf("count(...) どうしは比べられない")
	case (lc || rc) && (op == "in" || op == "contains"):
		return nil, p.errorf("count(...) は %s で比べられない", op)
	}
	return cmpN{op: op, l: l, r: r}, nil
}

func (p *parser) term() (node, error) {
	p.skip()
	if p.eof() {
		return nil, p.errorf("項が要るが式が終わった")
	}
	switch c := p.src[p.pos]; {
	case c == '(':
		p.pos++
		x, err := p.expr()
		if err != nil {
			return nil, err
		}
		if err := p.expect(")"); err != nil {
			return nil, err
		}
		return x, nil
	case c == '"' || c == '\'' || c == '-' || ('0' <= c && c <= '9') || c == '[':
		v, err := p.constant()
		if err != nil {
			return nil, err
		}
		return constN{v: v}, nil
	}
	start := p.pos
	switch w := p.word(); w {
	case "":
		return nil, p.errorf("項が要る（%q がある）", head(p.rest()))
	case "true":
		return constN{v: true}, nil
	case "false":
		return constN{v: false}, nil
	case "observe":
		id, err := p.ref()
		if err != nil {
			return nil, err
		}
		path, err := p.path("observe(" + id.String() + ")")
		if err != nil {
			return nil, err
		}
		return obsN{id: id, path: path}, nil
	case "present", "absent":
		id, err := p.ref()
		if err != nil {
			return nil, err
		}
		return presN{id: id, absent: w == "absent"}, nil
	case "desired":
		path, err := p.path("desired")
		if err != nil {
			return nil, err
		}
		return desN{path: path}, nil
	case "it":
		if p.quant == 0 {
			p.pos = start
			return nil, p.errorf("it は all・any・count の中でだけ書ける")
		}
		path, err := p.path("it")
		if err != nil {
			return nil, err
		}
		return itN{path: path}, nil
	case "check":
		if err := p.expect("("); err != nil {
			return nil, err
		}
		name := strings.TrimSpace(p.until(")"))
		if err := p.expect(")"); err != nil {
			return nil, err
		}
		if name == "" {
			return nil, p.errorf("check() に名前を書く")
		}
		return checkN{name: name}, nil
	case "all", "any", "count":
		if err := p.expect("("); err != nil {
			return nil, err
		}
		sel, err := p.selection()
		if err != nil {
			return nil, err
		}
		if err := p.expect(","); err != nil {
			return nil, err
		}
		p.quant++
		body, err := p.expr()
		p.quant--
		if err != nil {
			return nil, err
		}
		if _, ok := body.(countN); ok {
			return nil, p.errorf("count(...) は比較の中で使う")
		}
		if err := p.expect(")"); err != nil {
			return nil, err
		}
		if w == "count" {
			return countN{sel: sel, body: body}, nil
		}
		return quantN{kind: w, sel: sel, body: body}, nil
	default:
		p.pos = start
		return nil, p.errorf("知らない語 %q（書ける参照: observe・present・absent・desired・it・check・all・any・count）", w)
	}
}

// until は stop のどれかの前までの文字列を読む（ID には . / : が入るので語で切らない）。
func (p *parser) until(stops string) string {
	i := p.pos
	for i < len(p.src) && !strings.ContainsRune(stops, rune(p.src[i])) {
		i++
	}
	s := p.src[p.pos:i]
	p.pos = i
	return s
}

func (p *parser) ref() (core.ID, error) {
	if err := p.expect("("); err != nil {
		return core.ID{}, err
	}
	at := p.pos
	s := strings.TrimSpace(p.until(")"))
	if err := p.expect(")"); err != nil {
		return core.ID{}, err
	}
	id, err := core.ParseRef(s)
	if err != nil {
		p.pos = at
		return core.ID{}, p.errorf("%v", err)
	}
	return id, nil
}

func (p *parser) path(what string) ([]string, error) {
	var path []string
	for {
		if p.eof() || p.src[p.pos] != '.' {
			break
		}
		p.pos++
		i := p.pos
		for i < len(p.src) && (isIdent(p.src[i], false) || p.src[i] == '-') {
			i++
		}
		if i == p.pos {
			return nil, p.errorf("%s. のあとに項目の名前を書く", what)
		}
		path = append(path, p.src[p.pos:i])
		p.pos = i
	}
	if len(path) == 0 {
		return nil, p.errorf("%s のあとに .項目 を書く", what)
	}
	return path, nil
}

func (p *parser) selection() (core.Selection, error) {
	p.skip()
	at := p.pos
	typ := strings.TrimSpace(p.until("[,)"))
	if err := core.ParseType(typ); err != nil {
		p.pos = at
		return core.Selection{}, p.errorf("選び方: %v", err)
	}
	sel := core.Selection{Type: typ}
	if !p.accept("[") {
		return sel, nil
	}
	sel.Labels = map[string]string{}
	p.skip()
	if p.accept("]") {
		return sel, nil
	}
	for {
		p.skip()
		key := p.until("=,]")
		key = strings.TrimSpace(key)
		if key == "" {
			return sel, p.errorf("選び方のラベルの名前を書く")
		}
		if err := p.expect("="); err != nil {
			return sel, err
		}
		p.skip()
		var value string
		if !p.eof() && (p.src[p.pos] == '"' || p.src[p.pos] == '\'') {
			s, err := p.str()
			if err != nil {
				return sel, err
			}
			value = s
		} else {
			value = strings.TrimSpace(p.until(",]"))
		}
		if _, dup := sel.Labels[key]; dup {
			return sel, p.errorf("ラベル %q を 2 回書いた", key)
		}
		sel.Labels[key] = value
		if p.accept("]") {
			return sel, nil
		}
		if err := p.expect(","); err != nil {
			return sel, err
		}
	}
}

func (p *parser) str() (string, error) {
	q := p.src[p.pos]
	var b strings.Builder
	i := p.pos + 1
	for i < len(p.src) {
		c := p.src[i]
		switch {
		case c == q:
			p.pos = i + 1
			return b.String(), nil
		case c == '\\' && i+1 < len(p.src):
			i++
			switch e := p.src[i]; e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				b.WriteByte(e)
			}
		default:
			b.WriteByte(c)
		}
		i++
	}
	return "", p.errorf("文字列が閉じていない")
}

func (p *parser) constant() (any, error) {
	p.skip()
	if p.eof() {
		return nil, p.errorf("定数が要るが式が終わった")
	}
	switch c := p.src[p.pos]; {
	case c == '"' || c == '\'':
		return p.str()
	case c == '[':
		p.pos++
		out := []any{}
		if p.accept("]") {
			return out, nil
		}
		for {
			v, err := p.constant()
			if err != nil {
				return nil, err
			}
			out = append(out, v)
			if p.accept("]") {
				return out, nil
			}
			if err := p.expect(","); err != nil {
				return nil, err
			}
		}
	case c == '-' || ('0' <= c && c <= '9'):
		i := p.pos + 1
		for i < len(p.src) && (('0' <= p.src[i] && p.src[i] <= '9') || p.src[i] == '.') {
			i++
		}
		s := p.src[p.pos:i]
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			p.pos = i
			return n, nil
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, p.errorf("数として読めない %q", s)
		}
		p.pos = i
		return f, nil
	}
	switch w := p.word(); w {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return nil, p.errorf("定数（文字列・数・true・false・[...]）が要る")
}
