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

// Package compat は Expand のモジュール go:compat（設計 §3.8・§7.2、E-A7）。intent の
// compatibility: に部品のバージョンの compatibility table を置き、望むバージョンの
// 組み合わせと、1 つずつ更新する途中の組み合わせが表に合うかを確かめる。v2 の
// internal/compat を、コアが表を知らない形に書き直した。
//
//	expand:
//	  - kind: go:compat
//	compatibility:
//	  - name: bmc-bios
//	    components:
//	      bmc:  {id: "fw/version/{node}-bmc"}             # field の既定は version
//	      bios: {id: "fw/version/{node}-bios", field: version}
//	    allow:
//	      - {bmc: "1.2", bios: "2.0"}
//	      - {bmc: "1.3", bios: "2.0"}
//	      - {bmc: "1.3", bios: "2.1"}
//
// id は "<provider>/<type>/<名前>" で、{名前} は "/" を含まない 1 つのセグメントに
// 一致する。変数の値が同じ宣言が 1 つの組（同じ装置）になる。表の行には組の全部の
// 部品のバージョンを書く（"*" はどのバージョンでもよい）。バージョンは文字列の
// 完全一致で比べる。
//
//   - 望む組み合わせ（宣言のバージョン。宣言の無い部品はどのバージョンでもよい）が
//     表に無ければ、Reject の診断で読み込み時にエラーにする（reject）。
//   - 途中の組み合わせは、バージョンを宣言した部品ごとに depends_on の when を足して
//     待たせる。「その部品だけを望むバージョンにし、バージョンを宣言したほかの部品は
//     今観測しているバージョンのまま」の組み合わせが表にあるときだけ Apply する
//     （`(observe(D).version == "2.0") or …`）。表に合わないあいだは待つ（Event
//     Waiting）。ほかの部品が観測できなければ Unknown として待つ。
//
// コアは表を知らない。同じ入力に同じ出力（N-P5）。
package compat

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/tjst-t/sakutei/v3/condition"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/expand"
)

// Key は intent の最上位の key。
const Key = "compatibility"

// DefaultField は component の field を書かないときのバージョンの field。
const DefaultField = "version"

// Any は表の行で「どのバージョンでもよい」。
const Any = "*"

// Component は表の部品 1 つ。
type Component struct {
	Name  string
	ID    string // "<provider>/<type>/<名前>"。名前に {変数} を書ける
	Field string
	re    *regexp.Regexp
	vars  []string
}

// Table は compatibility table 1 つ。
type Table struct {
	Name       string
	Components []Component
	Allow      []map[string]string
	Origin     core.Origin
}

var varRe = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Expander は go:compat。
type Expander struct{}

var _ expand.Expander = Expander{}

// Keys は compatibility。
func (Expander) Keys() []string { return []string{Key} }

// Expand は表を読み、前の出力の宣言と照らす。
func (Expander) Expand(_ context.Context, in expand.Input) (expand.Output, error) {
	var out expand.Output
	reject := func(o core.Origin, target, format string, a ...any) {
		out.Diagnostics = append(out.Diagnostics, core.Diagnostic{Severity: core.Reject, Origin: o, Target: target, Message: fmt.Sprintf(format, a...)})
	}
	var tables []Table
	names := map[string]bool{}
	for _, s := range expand.Sections(in.Intent, Key) {
		list, ok := s.Value.([]any)
		if !ok {
			reject(s.Origin(), Key, "compatibility: にリストを書く")
			continue
		}
		for i, item := range list {
			t, err := parseTable(item)
			if err != nil {
				reject(s.Origin(), Key, "compatibility[%d]: %v", i, err)
				continue
			}
			if names[t.Name] {
				reject(s.Origin(), Key, "compatibility[%d]: 表 %s が 2 つある", i, t.Name)
				continue
			}
			names[t.Name] = true
			t.Origin = s.Origin()
			tables = append(tables, t)
		}
	}
	if len(out.Diagnostics) > 0 {
		return out, nil
	}
	byID := map[string]core.Resource{}
	ids := make([]string, 0, len(in.Prior.Resources))
	for _, r := range in.Prior.Resources {
		byID[r.ID.String()] = r
		ids = append(ids, r.ID.String())
	}
	sort.Strings(ids)
	for _, t := range tables {
		for _, g := range groups(t, ids) {
			checkGroup(t, g, byID, &out)
		}
	}
	return out, nil
}

// group は 1 つの装置：変数の値と、部品 → 宣言の ID（宣言が無ければ組み立てた ID）。
type group struct {
	key   string
	ids   map[string]string
	bound map[string]bool // 部品 → 宣言がある
}

// groups は表の部品の id に合う宣言を、変数の値ごとにまとめる。
func groups(t Table, ids []string) []group {
	byKey := map[string]*group{}
	var keys []string
	for _, c := range t.Components {
		for _, id := range ids {
			m := c.re.FindStringSubmatch(id)
			if m == nil {
				continue
			}
			vals := map[string]string{}
			ok := true
			for i, v := range c.vars {
				if prev, seen := vals[v]; seen && prev != m[i+1] {
					ok = false
				}
				vals[v] = m[i+1]
			}
			if !ok {
				continue
			}
			key := groupKey(vals)
			g := byKey[key]
			if g == nil {
				g = &group{key: key, ids: map[string]string{}, bound: map[string]bool{}}
				for _, cc := range t.Components {
					g.ids[cc.Name] = expandID(cc.ID, vals)
				}
				byKey[key] = g
				keys = append(keys, key)
			}
			g.bound[c.Name] = true
		}
	}
	sort.Strings(keys)
	out := make([]group, len(keys))
	for i, k := range keys {
		out[i] = *byKey[k]
	}
	return out
}

func checkGroup(t Table, g group, byID map[string]core.Resource, out *expand.Output) {
	// 望むバージョン（宣言の無い部品・field は Any）。
	want := map[string]string{}
	for _, c := range t.Components {
		want[c.Name] = Any
		if r, ok := byID[g.ids[c.Name]]; ok && g.bound[c.Name] {
			if v, ok := r.Fields[c.Field]; ok {
				want[c.Name] = fmt.Sprint(v)
			}
		}
	}
	if !anyRowMatches(t, want) {
		out.Diagnostics = append(out.Diagnostics, core.Diagnostic{Severity: core.Reject, Origin: t.Origin, Target: Key,
			Message: fmt.Sprintf("表 %s: %s の望む組み合わせ %s が allow に無い", t.Name, g.key, combo(t, want))})
		return
	}
	// 途中の組み合わせ：バージョンを宣言した部品 C を変えるとき、ほかの部品は今のバージョンのまま。
	for _, c := range t.Components {
		v := want[c.Name]
		if v == Any {
			continue
		}
		r := byID[g.ids[c.Name]]
		var alts []string
		for _, row := range t.Allow {
			if !match(row[c.Name], v) {
				continue
			}
			var terms []string
			for _, d := range t.Components {
				// バージョンを宣言していない部品はどのバージョンでもよい（観測を待たない）。
				if d.Name == c.Name || row[d.Name] == Any || want[d.Name] == Any {
					continue
				}
				terms = append(terms, fmt.Sprintf("observe(%s).%s == %s", g.ids[d.Name], d.Field, strconv.Quote(row[d.Name])))
			}
			if len(terms) == 0 {
				alts = nil // ほかの部品はどのバージョンでもよい：条件は要らない
				break
			}
			alts = append(alts, "("+strings.Join(terms, " and ")+")")
		}
		if len(alts) == 0 {
			continue
		}
		src := strings.Join(alts, " or ")
		when, err := condition.Parse(src)
		if err != nil {
			out.Diagnostics = append(out.Diagnostics, core.Diagnostic{Severity: core.Reject, Origin: t.Origin, Target: r.ID.String(),
				Message: fmt.Sprintf("表 %s: 条件を作れない: %v", t.Name, err)})
			continue
		}
		if out.DependsOn == nil {
			out.DependsOn = map[core.ID][]core.Dependency{}
		}
		out.DependsOn[r.ID] = append(out.DependsOn[r.ID], core.Dependency{When: when})
	}
}

func anyRowMatches(t Table, want map[string]string) bool {
	for _, row := range t.Allow {
		ok := true
		for _, c := range t.Components {
			if want[c.Name] != Any && !match(row[c.Name], want[c.Name]) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func match(cell, v string) bool { return cell == Any || cell == v }

func combo(t Table, want map[string]string) string {
	parts := make([]string, len(t.Components))
	for i, c := range t.Components {
		parts[i] = c.Name + "=" + want[c.Name]
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func groupKey(vals map[string]string) string {
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + vals[k]
	}
	if len(parts) == 0 {
		return "（変数なし）"
	}
	return strings.Join(parts, ",")
}

func expandID(pattern string, vals map[string]string) string {
	return varRe.ReplaceAllStringFunc(pattern, func(m string) string { return vals[m[1:len(m)-1]] })
}

func parseTable(item any) (Table, error) {
	m, ok := item.(map[string]any)
	if !ok {
		return Table{}, fmt.Errorf("マップを書く")
	}
	for k := range m {
		if k != "name" && k != "components" && k != "allow" {
			return Table{}, fmt.Errorf("不明な field %q（書ける field: name, components, allow）", k)
		}
	}
	var t Table
	t.Name, _ = m["name"].(string)
	if strings.TrimSpace(t.Name) == "" {
		return Table{}, fmt.Errorf("name を書く")
	}
	comps, _ := m["components"].(map[string]any)
	if len(comps) == 0 {
		return Table{}, fmt.Errorf("%s: components（部品の名前 → {id, field}）を書く", t.Name)
	}
	cnames := make([]string, 0, len(comps))
	for n := range comps {
		cnames = append(cnames, n)
	}
	sort.Strings(cnames)
	var vars []string
	for i, n := range cnames {
		cm, ok := comps[n].(map[string]any)
		if !ok {
			return Table{}, fmt.Errorf("%s: 部品 %s: {id, field} を書く", t.Name, n)
		}
		c := Component{Name: n, Field: DefaultField}
		c.ID, _ = cm["id"].(string)
		if f, _ := cm["field"].(string); f != "" {
			c.Field = f
		}
		for k := range cm {
			if k != "id" && k != "field" {
				return Table{}, fmt.Errorf("%s: 部品 %s: 不明な field %q", t.Name, n, k)
			}
		}
		if _, err := core.ParseRef(varRe.ReplaceAllString(c.ID, "x")); err != nil {
			return Table{}, fmt.Errorf("%s: 部品 %s: id %q が <provider>/<type>/<名前> の形でない", t.Name, n, c.ID)
		}
		pat := "^"
		last := 0
		for _, loc := range varRe.FindAllStringSubmatchIndex(c.ID, -1) {
			pat += regexp.QuoteMeta(c.ID[last:loc[0]]) + "([^/]+)"
			c.vars = append(c.vars, c.ID[loc[2]:loc[3]])
			last = loc[1]
		}
		pat += regexp.QuoteMeta(c.ID[last:]) + "$"
		c.re = regexp.MustCompile(pat)
		vs := uniq(c.vars)
		if i == 0 {
			vars = vs
		} else if strings.Join(vs, ",") != strings.Join(vars, ",") {
			// 組を作るため、どの部品も同じ変数を使う。
			return Table{}, fmt.Errorf("%s: 部品 %s の変数 %v がほかの部品の %v と違う", t.Name, n, vs, vars)
		}
		t.Components = append(t.Components, c)
	}
	rows, _ := m["allow"].([]any)
	if len(rows) == 0 {
		return Table{}, fmt.Errorf("%s: allow（許すバージョンの組み合わせ）を書く", t.Name)
	}
	for i, raw := range rows {
		rm, ok := raw.(map[string]any)
		if !ok {
			return Table{}, fmt.Errorf("%s: allow[%d]: マップを書く", t.Name, i)
		}
		row := map[string]string{}
		for k, v := range rm {
			if _, ok := comps[k]; !ok {
				return Table{}, fmt.Errorf("%s: allow[%d]: 部品 %q が components に無い", t.Name, i, k)
			}
			switch x := v.(type) {
			case string:
				row[k] = x
			case int, int64, float64:
				row[k] = fmt.Sprint(x)
			default:
				return Table{}, fmt.Errorf("%s: allow[%d].%s: バージョンを文字列で書く", t.Name, i, k)
			}
		}
		for _, c := range cnames {
			if _, ok := row[c]; !ok {
				return Table{}, fmt.Errorf("%s: allow[%d]: 部品 %s のバージョンを書く（どれでもよければ \"*\"）", t.Name, i, c)
			}
		}
		t.Allow = append(t.Allow, row)
	}
	return t, nil
}

func uniq(l []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range l {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
