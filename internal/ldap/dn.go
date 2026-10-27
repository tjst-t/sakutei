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

// dn.go — DN（識別名、RFC 4514）を読み、正規形にする。
//
// 正規形（ID に書く形・List が返す形）:
//
//   - 属性の型は小文字（"OID." の接頭辞は外す）。
//   - 値は小文字（Unicode の小文字）。DN に使う標準の属性（dc・ou・cn・uid・o など）は
//     大文字小文字を区別しないので、サーバに "ou=People" と書かれた項目も
//     "ou=people" で同じ項目を指す。ID の綴りが 1 つに決まるので、List の ID と
//     intent の ID が大文字小文字だけで食い違って二重に作る・prune で消すことがない。
//   - 区切り（","・"+"・"="）の前後の空白は落とし、区切りは ","（";" は ","）。
//   - 値のエスケープは RFC 4514 の最小（"\" "," "+" """ ";" "<" ">"、先頭の空白と
//     "#"、末尾の空白、制御文字は \XX）。
//   - 複数の値を持つ RDN（"cn=a+uid=b"）は (型, 値) の順に並べる。
//
// "#" で始まる BER の値（"cn=#04024869"）は扱わない（エラー）。

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// AVA は RDN の 1 つの「型=値」。Type は小文字、Value はエスケープを解いた値。
type AVA struct {
	Type  string
	Value string
}

// RDN は 1 つの相対識別名（"+" でつないだ AVA の並び）。
type RDN []AVA

// DN は識別名。先頭が葉（いちばん左の RDN）。
type DN []RDN

var (
	descrType = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*$`)
	oidType   = regexp.MustCompile(`^[0-9]+(\.[0-9]+)+$`)
)

// ParseDN は s を読む。空の文字列はルート（RDN が 0 個）。
func ParseDN(s string) (DN, error) {
	p := dnParser{s: s}
	return p.parse()
}

type dnParser struct {
	s   string
	pos int
}

func (p *dnParser) skipSpaces() {
	for p.pos < len(p.s) && p.s[p.pos] == ' ' {
		p.pos++
	}
}

func (p *dnParser) parse() (DN, error) {
	if !utf8.ValidString(p.s) {
		return nil, errors.New("the DN is not valid UTF-8")
	}
	p.skipSpaces()
	if p.pos == len(p.s) {
		return DN{}, nil
	}
	var dn DN
	var rdn RDN
	for {
		a, err := p.ava()
		if err != nil {
			return nil, err
		}
		rdn = append(rdn, a)
		p.skipSpaces()
		if p.pos == len(p.s) {
			dn = append(dn, rdn)
			return dn, nil
		}
		switch p.s[p.pos] {
		case '+':
			p.pos++
		case ',', ';':
			p.pos++
			dn = append(dn, rdn)
			rdn = nil
		default:
			return nil, fmt.Errorf("unexpected %q at %d", p.s[p.pos], p.pos)
		}
	}
}

func (p *dnParser) ava() (AVA, error) {
	p.skipSpaces()
	eq := strings.IndexByte(p.s[p.pos:], '=')
	if eq < 0 {
		return AVA{}, fmt.Errorf("missing \"=\" after %q", p.s[p.pos:])
	}
	typ := strings.TrimSpace(p.s[p.pos : p.pos+eq])
	if len(typ) > 4 && strings.EqualFold(typ[:4], "oid.") {
		typ = typ[4:]
	}
	if !descrType.MatchString(typ) && !oidType.MatchString(typ) {
		return AVA{}, fmt.Errorf("attribute type %q is not a name or an OID", typ)
	}
	p.pos += eq + 1
	p.skipSpaces()
	if p.pos < len(p.s) && p.s[p.pos] == '#' {
		return AVA{}, fmt.Errorf("the BER value of %s (\"#...\") is not supported", typ)
	}
	var v string
	var err error
	if p.pos < len(p.s) && p.s[p.pos] == '"' {
		v, err = p.quoted()
	} else {
		v, err = p.plain()
	}
	if err != nil {
		return AVA{}, err
	}
	return AVA{Type: strings.ToLower(typ), Value: v}, nil
}

// quoted は RFC 2253 より前の引用符つきの値。
func (p *dnParser) quoted() (string, error) {
	p.pos++ // "
	var b []byte
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		switch c {
		case '"':
			p.pos++
			return string(b), nil
		case '\\':
			r, err := p.escape()
			if err != nil {
				return "", err
			}
			b = append(b, r...)
			continue
		}
		b = append(b, c)
		p.pos++
	}
	return "", errors.New("unterminated quoted value")
}

// plain は引用符の無い値。区切りの前の、エスケープしていない空白は落とす。
func (p *dnParser) plain() (string, error) {
	var b []byte
	keep := 0 // 末尾のエスケープした空白を落とさないための長さ。
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		if c == ',' || c == ';' || c == '+' {
			break
		}
		if c == '\\' {
			r, err := p.escape()
			if err != nil {
				return "", err
			}
			b = append(b, r...)
			keep = len(b)
			continue
		}
		b = append(b, c)
		if c != ' ' {
			keep = len(b)
		}
		p.pos++
	}
	v := b[:keep]
	if !utf8.Valid(v) {
		return "", errors.New("a value is not valid UTF-8")
	}
	return string(v), nil
}

// escape は "\" に続く 1 つのエスケープ（"\X" か "\XX" の 16 進）を解く。
func (p *dnParser) escape() ([]byte, error) {
	p.pos++ // \
	if p.pos >= len(p.s) {
		return nil, errors.New("a value ends with \"\\\"")
	}
	if p.pos+1 < len(p.s) && isHex(p.s[p.pos]) && isHex(p.s[p.pos+1]) {
		v := unhex(p.s[p.pos])<<4 | unhex(p.s[p.pos+1])
		p.pos += 2
		return []byte{v}, nil
	}
	c := p.s[p.pos]
	if strings.IndexByte(` "#+,;<=>\`, c) < 0 {
		return nil, fmt.Errorf("bad escape \"\\%c\"", c)
	}
	p.pos++
	return []byte{c}, nil
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

// Canonical は正規形（値を小文字にし、RDN の中の AVA を並べた DN）を返す。
func (d DN) Canonical() DN {
	out := make(DN, len(d))
	for i, r := range d {
		nr := make(RDN, len(r))
		for j, a := range r {
			nr[j] = AVA{Type: a.Type, Value: strings.ToLower(a.Value)}
		}
		sort.Slice(nr, func(a, b int) bool {
			if nr[a].Type != nr[b].Type {
				return nr[a].Type < nr[b].Type
			}
			return nr[a].Value < nr[b].Value
		})
		out[i] = nr
	}
	return out
}

// String は d を RFC 4514 の形で書く（値の大文字小文字はそのまま）。
func (d DN) String() string {
	var b strings.Builder
	for i, r := range d {
		if i > 0 {
			b.WriteByte(',')
		}
		for j, a := range r {
			if j > 0 {
				b.WriteByte('+')
			}
			b.WriteString(a.Type)
			b.WriteByte('=')
			b.WriteString(EscapeValue(a.Value))
		}
	}
	return b.String()
}

// EscapeValue は DN の値を RFC 4514 の最小のエスケープで書く。
func EscapeValue(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c == '\\' || c == '"' || c == '+' || c == ',' || c == ';' || c == '<' || c == '>':
			b.WriteByte('\\')
			b.WriteByte(c)
		case i == 0 && (c == ' ' || c == '#'), i == len(v)-1 && c == ' ':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, "\\%02x", c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// CanonicalDN は s の正規形の文字列。
func CanonicalDN(s string) (string, error) {
	d, err := ParseDN(s)
	if err != nil {
		return "", err
	}
	return d.Canonical().String(), nil
}

// Under は d が base のちょうど下（base そのものは含まない）にあるか。どちらも
// 正規形で渡す。
func (d DN) Under(base DN) bool {
	if len(d) <= len(base) {
		return false
	}
	off := len(d) - len(base)
	for i := range base {
		if !rdnEqual(d[off+i], base[i]) {
			return false
		}
	}
	return true
}

func rdnEqual(a, b RDN) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
