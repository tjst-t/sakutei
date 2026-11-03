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

// ldif.go — LDIF（RFC 2849）の読み書きとスキーマの objectClasses（v2 の
// internal/ldap から移した、E-G1）。
//
//   - 読む：ldapsearch -LLL の出力（"#" の行は飛ばす。折り返しの行は前の行に
//     つなぐ。"名前:: base64" を解く）。
//   - 書く：ldapmodify に渡す変更（add・modify の replace・delete）。DN と値は
//     いつも base64（"名前:: ..."）で書くので、どんな値でもエスケープの誤りが無い。

import (
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// Line は LDIF の 1 行（"名前: 値"）。Value は base64 を解いた値。
type Line struct {
	Name  string
	Value string
}

// Record は空行で区切った 1 つのまとまり（先頭は dn の行）。
type Record []Line

// ParseLDIF は LDIF を読む。"名前:< URL" は扱わない（エラー）。
func ParseLDIF(text string) ([]Record, error) {
	var lines []string
	comment := false
	for _, l := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, " "):
			if comment {
				continue
			}
			if len(lines) == 0 || lines[len(lines)-1] == "" {
				return nil, errors.New("LDIF: 前の行の無い継続の行")
			}
			lines[len(lines)-1] += l[1:]
			continue
		case strings.HasPrefix(l, "#"):
			comment = true
			continue
		}
		comment = false
		lines = append(lines, l)
	}
	var out []Record
	var cur Record
	flush := func() {
		if len(cur) > 0 {
			out = append(out, cur)
			cur = nil
		}
	}
	for _, l := range lines {
		if l == "" {
			flush()
			continue
		}
		if l == "-" {
			cur = append(cur, Line{Name: "-"})
			continue
		}
		name, rest, ok := strings.Cut(l, ":")
		if !ok || name == "" {
			return nil, fmt.Errorf("LDIF: \":\" の無い行（%.40q）", l)
		}
		var value string
		switch {
		case strings.HasPrefix(rest, ":"):
			b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rest[1:]))
			if err != nil {
				return nil, fmt.Errorf("LDIF: %s: base64 として読めない", name)
			}
			value = string(b)
		case strings.HasPrefix(rest, "<"):
			return nil, fmt.Errorf("LDIF: %s: URL の値は扱わない", name)
		default:
			value = strings.TrimLeft(rest, " ")
		}
		cur = append(cur, Line{Name: name, Value: value})
	}
	flush()
	// 先頭の "version: 1" だけのまとまりは飛ばす。
	if len(out) > 0 && len(out[0]) == 1 && strings.EqualFold(out[0][0].Name, "version") {
		out = out[1:]
	}
	return out, nil
}

// Entry は検索の結果の 1 つの項目。
type Entry struct {
	// DN はサーバが返した DN（書き方はサーバのまま）。
	DN string
	// Attrs は属性（名前は小文字。オプションつきなら "cn;lang-ja"）→ 値（サーバの順）。
	Attrs map[string][]string
	// Names は属性の名前のサーバの綴り（小文字 → 綴り）。
	Names map[string]string
}

// entries は検索の結果のまとまりを Entry にする。
func entries(recs []Record) ([]Entry, error) {
	out := make([]Entry, 0, len(recs))
	for _, r := range recs {
		if !strings.EqualFold(r[0].Name, "dn") {
			return nil, fmt.Errorf("LDIF: dn で始まらない項目（%q）", r[0].Name)
		}
		e := Entry{DN: r[0].Value, Attrs: map[string][]string{}, Names: map[string]string{}}
		for _, l := range r[1:] {
			k := strings.ToLower(l.Name)
			e.Attrs[k] = append(e.Attrs[k], l.Value)
			if _, ok := e.Names[k]; !ok {
				e.Names[k] = l.Name
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// ---- 書く ----

// Attr は書く 1 つの属性（名前は宣言の綴り）。
type Attr struct {
	Name   string
	Values []string
}

func writeB64(b *strings.Builder, name, value string) {
	b.WriteString(name)
	b.WriteString(":: ")
	b.WriteString(base64.StdEncoding.EncodeToString([]byte(value)))
	b.WriteByte('\n')
}

// AddLDIF は項目を作る変更。
func AddLDIF(dn string, attrs []Attr) string {
	var b strings.Builder
	writeB64(&b, "dn", dn)
	b.WriteString("changetype: add\n")
	for _, a := range attrs {
		for _, v := range a.Values {
			writeB64(&b, a.Name, v)
		}
	}
	b.WriteByte('\n')
	return b.String()
}

// ReplaceLDIF は属性ごとに値をその一覧ちょうどにする変更（値が空なら属性を消す）。
// 1 つの modify なので、サーバは全部を一度に当てるか、何もしない。
func ReplaceLDIF(dn string, attrs []Attr) string {
	var b strings.Builder
	writeB64(&b, "dn", dn)
	b.WriteString("changetype: modify\n")
	for _, a := range attrs {
		b.WriteString("replace: " + a.Name + "\n")
		for _, v := range a.Values {
			writeB64(&b, a.Name, v)
		}
		b.WriteString("-\n")
	}
	b.WriteByte('\n')
	return b.String()
}

// DeleteLDIF は項目を消す変更。
func DeleteLDIF(dn string) string {
	var b strings.Builder
	writeB64(&b, "dn", dn)
	b.WriteString("changetype: delete\n\n")
	return b.String()
}

// ---- 値 ----

// uniqueSorted は値を並べ替え、重複を落とす（値の集合の正規形、E-B12）。
func uniqueSorted(vals []string) []string {
	s := append([]string(nil), vals...)
	sort.Strings(s)
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// validValue は宣言の値として書けるか（理由を返す。書ければ ""）。
func validValue(v string) string {
	switch {
	case v == "":
		return "空の値（属性が無いことを宣言するときは値の全体を \"\" か [] にする）"
	case !utf8.ValidString(v):
		return "UTF-8 でない値"
	case strings.IndexByte(v, 0) >= 0:
		return "NUL を含む値"
	}
	return ""
}

// ---- スキーマ（objectClasses） ----

// parseObjectClasses は subschema の objectClasses の値（RFC 4512 の
// ObjectClassDescription）から、クラスの名前（小文字）→ 上位のクラス（小文字）を作る。
func parseObjectClasses(values []string) map[string][]string {
	out := map[string][]string{}
	for _, v := range values {
		toks := schemaTokens(v)
		var names, sups []string
		for i := 0; i < len(toks); i++ {
			switch strings.ToUpper(toks[i]) {
			case "NAME":
				var n []string
				n, i = schemaList(toks, i+1)
				names = n
			case "SUP":
				var s []string
				s, i = schemaList(toks, i+1)
				sups = s
			}
		}
		for i := range sups {
			sups[i] = strings.ToLower(sups[i])
		}
		for _, n := range names {
			out[strings.ToLower(n)] = sups
		}
	}
	return out
}

// schemaTokens は "(" ")" "$" '引用' と語に分ける。
func schemaTokens(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			i++
		case c == '(' || c == ')' || c == '$':
			out = append(out, string(c))
			i++
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				out = append(out, s[i+1:])
				return out
			}
			out = append(out, s[i+1:i+1+j])
			i += j + 2
		default:
			j := i
			for j < len(s) && strings.IndexByte(" \t\n()$'", s[j]) < 0 {
				j++
			}
			out = append(out, s[i:j])
			i = j
		}
	}
	return out
}

// schemaList は toks[i] からの 1 つの値か "( a $ b )" の一覧を読み、最後に読んだ
// 位置を返す。
func schemaList(toks []string, i int) ([]string, int) {
	if i >= len(toks) {
		return nil, i
	}
	if toks[i] != "(" {
		return []string{toks[i]}, i
	}
	var out []string
	for i++; i < len(toks) && toks[i] != ")"; i++ {
		if toks[i] != "$" {
			out = append(out, toks[i])
		}
	}
	return out, i
}

// impliedClasses は classes（小文字）の上位のクラスを全部たどった集合と "top"。
// サーバ（389 Directory Server など）は項目に上位のクラスを足すので、宣言に
// 書かなくても在ってよいクラス。
func impliedClasses(classes []string, sup map[string][]string) []string {
	seen := map[string]bool{"top": true}
	var walk func(c string, depth int)
	walk = func(c string, depth int) {
		if depth > 32 {
			return
		}
		for _, s := range sup[c] {
			if !seen[s] {
				seen[s] = true
			}
			walk(s, depth+1)
		}
	}
	for _, c := range classes {
		walk(strings.ToLower(c), 0)
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
