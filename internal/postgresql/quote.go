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

package postgresql

// quote.go — SQL に埋め込む識別子と文字列の quote（PostgreSQL の quote_ident・
// quote_literal に当たるもの）。provider は SQL を文字列で組み立てて psql の
// 標準入力に流すので、名前や値はすべてここを通す。
//
//   - 識別子は常に二重引用符で囲み、中の " は "" にする（予約語・大文字・空白・
//     引用符を含む名前も、書いたとおりの 1 つの識別子になる）。
//   - 文字列は E'...' で囲み、中の ' は ''、\ は \\ にする。standard_conforming_strings
//     の設定に関わらず同じ意味になる。
//   - NUL（0 バイト）は PostgreSQL の名前・文字列に入らないので、エラーにする。

import (
	"errors"
	"strings"
)

// errNUL は名前・値に NUL が含まれるときのエラー（値は文言に出さない）。
var errNUL = errors.New("a name or value must not contain a NUL byte")

// QuoteIdent は s を SQL の識別子として quote する。
func QuoteIdent(s string) (string, error) {
	if strings.IndexByte(s, 0) >= 0 {
		return "", errNUL
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`, nil
}

// QuoteLiteral は s を SQL の文字列として quote する。
func QuoteLiteral(s string) (string, error) {
	if strings.IndexByte(s, 0) >= 0 {
		return "", errNUL
	}
	r := strings.NewReplacer(`'`, `''`, `\`, `\\`)
	return `E'` + r.Replace(s) + `'`, nil
}

// sqlBuilder は文を組み立てる。quote のエラーは最初の 1 つだけを持ち、Err で
// 返す（呼ぶ側が毎回確かめなくてよいように）。
type sqlBuilder struct {
	b   strings.Builder
	err error
}

func (s *sqlBuilder) raw(parts ...string) *sqlBuilder {
	for _, p := range parts {
		s.b.WriteString(p)
	}
	return s
}

func (s *sqlBuilder) ident(name string) *sqlBuilder {
	q, err := QuoteIdent(name)
	if err != nil && s.err == nil {
		s.err = err
	}
	s.b.WriteString(q)
	return s
}

func (s *sqlBuilder) literal(v string) *sqlBuilder {
	q, err := QuoteLiteral(v)
	if err != nil && s.err == nil {
		s.err = err
	}
	s.b.WriteString(q)
	return s
}

// end は文を ";\n" で閉じる。
func (s *sqlBuilder) end() *sqlBuilder { return s.raw(";\n") }

func (s *sqlBuilder) String() string { return s.b.String() }
