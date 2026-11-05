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

// Package yamlout は sakutei-migrate と sakutei-import が書き出す YAML。コアの YAML の部分集合（v3/internal/yaml）が
// そのまま読める形だけを出す：ブロック形式のマップとシーケンス、文字列・整数・真偽値。
// キーの順は書いた順（Obj）で、読み返すと同じ値になる。
package yamlout

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Obj はキーの順を保つマップ。
type Obj struct {
	Keys []string
	Vals map[string]any
}

// New は空の Obj。
func New() *Obj { return &Obj{Vals: map[string]any{}} }

// Set はキーを足す（あれば値を置き換える）。nil は書かない。
func (o *Obj) Set(k string, v any) *Obj {
	if v == nil {
		return o
	}
	if _, ok := o.Vals[k]; !ok {
		o.Keys = append(o.Keys, k)
	}
	o.Vals[k] = v
	return o
}

// Get はキーの値（無ければ nil）。
func (o *Obj) Get(k string) any { return o.Vals[k] }

// Len はキーの数。
func (o *Obj) Len() int { return len(o.Keys) }

// Sorted は Go のマップをキーの順に並べた Obj にする。
func Sorted(m map[string]any) *Obj {
	o := New()
	for _, k := range sortedKeys(m) {
		o.Set(k, m[k])
	}
	return o
}

// Value は観測の値（map[string]any・[]any・[]string・整数・真偽値・文字列）を書き
// 出せる形にする。マップはキーの順にそろえる。
func Value(v any) any {
	switch x := v.(type) {
	case map[string]any:
		o := New()
		for _, k := range sortedKeys(x) {
			o.Set(k, Value(x[k]))
		}
		return o
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = Value(e)
		}
		return out
	case []string:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = e
		}
		return out
	case int64:
		return int(x)
	case int32:
		return int(x)
	case float64:
		if x == float64(int(x)) {
			return int(x)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return v
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Emit は v を YAML の文書にする（header は先頭に置くコメントの行）。
func Emit(v *Obj, header ...string) string {
	var b strings.Builder
	for _, h := range header {
		b.WriteString("# " + h + "\n")
	}
	writeMap(&b, v, 0)
	return b.String()
}

func writeMap(b *strings.Builder, o *Obj, indent int) {
	pad := strings.Repeat(" ", indent)
	for _, k := range o.Keys {
		writeEntry(b, pad+key(k)+":", o.Vals[k], indent)
	}
}

// writeEntry は「キー:」に値を続ける。マップとシーケンスは次の行から 2 字下げる。
func writeEntry(b *strings.Builder, head string, v any, indent int) {
	switch x := v.(type) {
	case *Obj:
		if x.Len() == 0 {
			b.WriteString(head + " {}\n")
			return
		}
		b.WriteString(head + "\n")
		writeMap(b, x, indent+2)
	case []any:
		if len(x) == 0 {
			b.WriteString(head + " []\n")
			return
		}
		b.WriteString(head + "\n")
		writeSeq(b, x, indent+2)
	default:
		b.WriteString(head + " " + scalar(x) + "\n")
	}
}

func writeSeq(b *strings.Builder, items []any, indent int) {
	pad := strings.Repeat(" ", indent)
	for _, it := range items {
		switch x := it.(type) {
		case *Obj:
			if x.Len() == 0 {
				b.WriteString(pad + "- {}\n")
				continue
			}
			// 最初のキーを「- 」の行に置き、残りを 2 字下げで続ける。
			for i, k := range x.Keys {
				head := pad + "  " + key(k) + ":"
				if i == 0 {
					head = pad + "- " + key(k) + ":"
				}
				writeEntry(b, head, x.Vals[k], indent+2)
			}
		case []any:
			b.WriteString(pad + "-\n")
			writeSeq(b, x, indent+2)
		default:
			b.WriteString(pad + "- " + scalar(x) + "\n")
		}
	}
}

var plainKey = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@/-]*$`)

func key(k string) string {
	if plainKey.MatchString(k) {
		return k
	}
	return quote(k)
}

// plain は引用符なしで書いても文字列として読める字句（数値・真偽値・null と読まれる
// もの、記号で始まるもの、: や # を含むものは除く）。
var plain = regexp.MustCompile(`^[A-Za-z_/.][A-Za-z0-9_./@+=-]*$`)

func scalar(v any) string {
	switch x := v.(type) {
	case string:
		switch x {
		case "true", "false", "null", "Null", "NULL", "~":
			return quote(x)
		}
		if plain.MatchString(x) {
			return x
		}
		return quote(x)
	case int:
		return strconv.Itoa(x)
	case bool:
		return strconv.FormatBool(x)
	case nil:
		return "null"
	default:
		panic(fmt.Sprintf("yamlout: 書き出せない値 %T", v))
	}
}

func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
