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

// Package compat は部品の版の互換の表（intent のトップレベルの
// compatibility:、§29.1・Backlog #151）を持ち、望む版の組み合わせと、
// 1 つずつ更新する間の途中の組み合わせが表に合うかを確かめる。
//
//	compatibility:
//	  - name: bmc-bios
//	    components:
//	      bmc:  { id: "{node}/firmware/BMC" }               # field の既定は version
//	      bios: { id: "{node}/firmware/BIOS", field: version }
//	    allow:
//	      - { bmc: "1.2", bios: "2.0" }
//	      - { bmc: "1.3", bios: "2.0" }
//	      - { bmc: "1.3", bios: "2.1" }
//
// id の {名前} は "/" を含まない 1 区切りに合い、同じ値に合ったものが 1 つの
// 組（同じ装置）になる。表の行は組のすべての部品の版を書く（"*" はどの版でも
// よい）。版は文字列の完全一致で比べる（redfish-firmware と同じ、§29.1 #148）。
//
// 表に無い部品は対象外。表の部品のうち intent に宣言の無いもの（またはその
// フィールドを宣言していないもの）は、その組では「どの版でもよい」として扱う。
package compat

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// DefaultField は component の field を書かないときの版のフィールド。
const DefaultField = "version"

// Any は表の行で「どの版でもよい」を表す。
const Any = "*"

// MaxChanging は 1 つの組で途中の組み合わせを数え上げる、変わる部品の数の
// 上限。超える組は確かめられないので変わる部品を止める（unknown ≠ ok）。
const MaxChanging = 12

// Component は表の部品の 1 つ。
type Component struct {
	// Name は表の中の部品の名前（allow の行のキー）。
	Name string
	// ID は Local ID、または {名前} を含む型。
	ID string
	// Field は版のフィールド（既定 version）。
	Field string

	re   *regexp.Regexp
	vars []string
}

// Table は互換の表の 1 つ。
type Table struct {
	Name       string
	Components []Component
	// Allow は許す版の組み合わせ（部品の名前 → 版、"*" はどれでも）。
	Allow []map[string]string
}

// Set は intent の compatibility: のすべて。
type Set []Table

var varRe = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Parse は intent のトップレベルの compatibility: を読む。
func Parse(v any) (Set, error) {
	if v == nil {
		return nil, nil
	}
	raw, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("compatibility: 配列でなければなりません")
	}
	out := make(Set, 0, len(raw))
	names := map[string]bool{}
	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("compatibility[%d]: オブジェクトでなければなりません", i)
		}
		t, err := parseTable(m)
		if err != nil {
			return nil, fmt.Errorf("compatibility[%d]: %w", i, err)
		}
		if names[t.Name] {
			return nil, fmt.Errorf("compatibility[%d]: name %q is declared twice", i, t.Name)
		}
		names[t.Name] = true
		out = append(out, t)
	}
	return out, nil
}

func parseTable(m map[string]any) (Table, error) {
	var t Table
	t.Name, _ = m["name"].(string)
	t.Name = strings.TrimSpace(t.Name)
	if t.Name == "" {
		return Table{}, fmt.Errorf("name is required")
	}
	comps, _ := m["components"].(map[string]any)
	if len(comps) == 0 {
		return Table{}, fmt.Errorf("%s: components is required (component name → { id, field })", t.Name)
	}
	compNames := make([]string, 0, len(comps))
	for name := range comps {
		compNames = append(compNames, name)
	}
	sort.Strings(compNames)
	var vars []string
	for i, name := range compNames {
		cm, ok := comps[name].(map[string]any)
		if !ok {
			return Table{}, fmt.Errorf("%s: component %q: want { id, field }", t.Name, name)
		}
		c := Component{Name: name}
		c.ID, _ = cm["id"].(string)
		c.ID = strings.TrimSpace(c.ID)
		c.Field, _ = cm["field"].(string)
		c.Field = strings.TrimSpace(c.Field)
		if c.Field == "" {
			c.Field = DefaultField
		}
		if c.ID == "" {
			return Table{}, fmt.Errorf("%s: component %q: id is required", t.Name, name)
		}
		if err := c.compile(); err != nil {
			return Table{}, fmt.Errorf("%s: component %q: %w", t.Name, name, err)
		}
		if i == 0 {
			vars = c.vars
		} else if strings.Join(c.vars, ",") != strings.Join(vars, ",") {
			return Table{}, fmt.Errorf("%s: component %q: id names {%s}, component %q names {%s}; every component of a table must name the same {…} (they decide which resources form one group)",
				t.Name, name, strings.Join(c.vars, "}{"), compNames[0], strings.Join(vars, "}{"))
		}
		t.Components = append(t.Components, c)
	}
	rows, _ := m["allow"].([]any)
	if len(rows) == 0 {
		return Table{}, fmt.Errorf("%s: allow is required (the version combinations that work together)", t.Name)
	}
	for j, r := range rows {
		rm, ok := r.(map[string]any)
		if !ok {
			return Table{}, fmt.Errorf("%s: allow[%d]: want { component: version, … }", t.Name, j)
		}
		row := make(map[string]string, len(rm))
		for k, v := range rm {
			if _, ok := comps[k]; !ok {
				return Table{}, fmt.Errorf("%s: allow[%d]: %q is not a component of the table", t.Name, j, k)
			}
			// 整数はそのまま文字列に（小数は読み込みが文字列のまま渡す: 1.10 を
			// 1.1 に潰さない）。それ以外（真偽など）は断る。
			switch x := v.(type) {
			case string:
				row[k] = strings.TrimSpace(x)
			case int:
				row[k] = strconv.Itoa(x)
			default:
				return Table{}, fmt.Errorf("%s: allow[%d]: %s: the version must be a string (quote it: \"%v\")", t.Name, j, k, v)
			}
		}
		for _, c := range t.Components {
			if _, ok := row[c.Name]; !ok {
				return Table{}, fmt.Errorf("%s: allow[%d]: no version for component %q (write \"*\" for any)", t.Name, j, c.Name)
			}
		}
		t.Allow = append(t.Allow, row)
	}
	return t, nil
}

func (c *Component) compile() error {
	var b strings.Builder
	b.WriteString("^")
	last := 0
	seen := map[string]bool{}
	for _, loc := range varRe.FindAllStringSubmatchIndex(c.ID, -1) {
		b.WriteString(regexp.QuoteMeta(c.ID[last:loc[0]]))
		name := c.ID[loc[2]:loc[3]]
		if seen[name] {
			return fmt.Errorf("id %q names {%s} twice", c.ID, name)
		}
		seen[name] = true
		c.vars = append(c.vars, name)
		b.WriteString("([^/]+)")
		last = loc[1]
	}
	rest := c.ID[last:]
	if strings.ContainsAny(rest, "{}") {
		return fmt.Errorf("id %q: a placeholder is {name} (letters, digits, _)", c.ID)
	}
	b.WriteString(regexp.QuoteMeta(rest))
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return err
	}
	c.re = re
	sort.Strings(c.vars)
	return nil
}

// match は local がこの部品に合えば、組を決める {名前}=値 の並びを返す。
func (c Component) match(local string) (string, bool) {
	sub := c.re.FindStringSubmatch(local)
	if sub == nil {
		return "", false
	}
	// vars は名前の順に並べ直してあるので、id の中の出現順で値を取り直す。
	byName := map[string]string{}
	for i, loc := range varRe.FindAllStringSubmatch(c.ID, -1) {
		byName[loc[1]] = sub[i+1]
	}
	parts := make([]string, 0, len(c.vars))
	for _, v := range c.vars {
		parts = append(parts, v+"="+byName[v])
	}
	return strings.Join(parts, ","), true
}

// member は組の中の宣言された部品の 1 つ。
type member struct {
	comp    string
	id      core.ID
	desired string
}

// Group は 1 つの表の、同じ {名前} の値に合った宣言の組。
type Group struct {
	Table   *Table
	Key     string // {名前}=値 の並び（無い表は ""）
	members map[string]member
}

// Label は知らせに出す組の名前。
func (g Group) Label() string {
	if g.Key == "" {
		return fmt.Sprintf("compatibility %q", g.Table.Name)
	}
	return fmt.Sprintf("compatibility %q (%s)", g.Table.Name, g.Key)
}

// Locals は組の宣言された部品の Local ID（並べたもの）。
func (g Group) Locals() []string {
	out := make([]string, 0, len(g.members))
	for _, m := range g.members {
		out = append(out, m.id.Local)
	}
	sort.Strings(out)
	return out
}

// Groups は desired（render の後）を表ごと・組ごとに分ける。1 つの宣言が
// 同じ表の 2 つの部品に合うのはエラー。absent の宣言と、版のフィールドを
// 宣言していないものは組に入らない（どの版でもよい）。
func (s Set) Groups(desired []diff.DesiredResource) ([]Group, error) {
	var out []Group
	for ti := range s {
		t := &s[ti]
		byKey := map[string]*Group{}
		var keys []string
		owner := map[string]string{}
		for _, r := range desired {
			if r.Absent {
				continue
			}
			for _, c := range t.Components {
				key, ok := c.match(r.ID.Local)
				if !ok {
					continue
				}
				if prev, dup := owner[r.ID.Local]; dup && prev != c.Name {
					return nil, fmt.Errorf("compatibility %q: resource %s matches both component %q and %q", t.Name, r.ID, prev, c.Name)
				}
				owner[r.ID.Local] = c.Name
				v, has := r.Fields[c.Field]
				if !has {
					continue
				}
				g, ok := byKey[key]
				if !ok {
					g = &Group{Table: t, Key: key, members: map[string]member{}}
					byKey[key] = g
					keys = append(keys, key)
				}
				if prev, dup := g.members[c.Name]; dup {
					return nil, fmt.Errorf("%s: component %q matches both %s and %s", g.Label(), c.Name, prev.id, r.ID)
				}
				g.members[c.Name] = member{comp: c.Name, id: r.ID, desired: v}
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = append(out, *byKey[k])
		}
	}
	return out, nil
}

// allowed は state（部品の名前 → 版。無い部品はどの版でもよい）が表の行の
// どれかに合うか。
func (t *Table) allowed(state map[string]string) bool {
	for _, row := range t.Allow {
		ok := true
		for comp, v := range state {
			if w := row[comp]; w != Any && w != v {
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

// describe は state を「部品=版（Local ID）」の並びにする。
func (g Group) describe(state map[string]string) string {
	names := make([]string, 0, len(state))
	for n := range state {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s=%s (%s)", n, state[n], g.members[n].id.Local))
	}
	return strings.Join(parts, ", ")
}

// CheckDesired は各組の望む版の組み合わせが表に合うかを確かめる。合わない
// 組を 1 つにまとめたエラー（部品と版を出す）を返す。観測は要らない。
func (s Set) CheckDesired(desired []diff.DesiredResource) error {
	groups, err := s.Groups(desired)
	if err != nil {
		return err
	}
	var bad []string
	for _, g := range groups {
		state := make(map[string]string, len(g.members))
		for n, m := range g.members {
			state[n] = m.desired
		}
		if !g.Table.allowed(state) {
			bad = append(bad, fmt.Sprintf("%s: the desired versions %s are not an allowed combination", g.Label(), g.describe(state)))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("%s (§29.1 compatibility; fix the versions or the table)", strings.Join(bad, "; "))
	}
	return nil
}

// Finding は途中の組み合わせを確かめた結果のうち、止める組の 1 つ。
type Finding struct {
	Group Group
	// Hold は止める（Apply も Task も起こさない）部品。組の変わる部品すべて。
	Hold []core.ID
	// Unverified は確かめられなかった（観測できない部品がある・変わる部品が
	// 多すぎる）。false なら表に合わない途中の組み合わせがある。
	Unverified bool
	// Reason は部品と版を入れた理由。
	Reason string
	// Hint は depends_on の案内（無ければ ""）。
	Hint string
}

// Message は Finding の 1 行の文。
func (f Finding) Message() string {
	if f.Hint == "" {
		return f.Reason
	}
	return f.Reason + "; " + f.Hint
}

// Current は Local ID のいまの版のフィールドの値を返す。観測できなければ
// ok=false（unknown）。観測できて現実に無ければ ok=true で fields が nil。
type Current func(local string) (fields map[string]string, ok bool)

// CheckPath は、各組で宣言と違う部品を 1 つずつ更新する間の途中の組み合わせ
// （depends_on の順序で起こりうるもの）がすべて表に合うかを確かめる。望む版の
// 組み合わせそのものは CheckDesired が確かめる。
//
//   - 変わる部品は、観測した版が望む版と違うもの。
//   - 順序は depends_on（desired の中を推移的にたどる）で、決まらない部品の
//     組はどちらが先にもなりうる（同じ周に両方起きうる）として数える。
//   - 組の部品で観測できないものがあれば、変わる部品を止める（確かめられない）。
func (s Set) CheckPath(desired []diff.DesiredResource, cur Current) ([]Finding, error) {
	groups, err := s.Groups(desired)
	if err != nil {
		return nil, err
	}
	before := dependsClosure(desired)
	var out []Finding
	for _, g := range groups {
		if f, ok := checkGroup(g, cur, before); ok {
			out = append(out, f)
		}
	}
	return out, nil
}

func checkGroup(g Group, cur Current, before func(a, b string) bool) (Finding, bool) {
	names := make([]string, 0, len(g.members))
	for n := range g.members {
		names = append(names, n)
	}
	sort.Strings(names)
	now := map[string]string{}
	var changing, unknown []string
	for _, n := range names {
		m := g.members[n]
		fields, ok := cur(m.id.Local)
		if !ok {
			unknown = append(unknown, m.id.Local)
			continue
		}
		v, has := fields[g.Table.componentField(n)]
		if !has {
			// 現実に無い（または版を観測しない）: 作られるまでは版を問わない。
			changing = append(changing, n)
			continue
		}
		now[n] = v
		if v != m.desired {
			changing = append(changing, n)
		}
	}
	if len(changing) == 0 {
		return Finding{}, false
	}
	hold := make([]core.ID, 0, len(changing))
	var holdNames []string
	for _, n := range changing {
		hold = append(hold, g.members[n].id)
		holdNames = append(holdNames, g.members[n].id.Local)
	}
	if len(unknown) > 0 {
		return Finding{Group: g, Hold: hold, Unverified: true,
			Reason: fmt.Sprintf("%s: cannot verify the update order of %s: %s not observed this cycle (not updating, unknown ≠ ok)",
				g.Label(), strings.Join(holdNames, ", "), strings.Join(unknown, ", "))}, true
	}
	if len(changing) > MaxChanging {
		return Finding{Group: g, Hold: hold, Unverified: true,
			Reason: fmt.Sprintf("%s: %d components change at once (more than %d); cannot verify the update order (not updating)", g.Label(), len(changing), MaxChanging)}, true
	}
	// mustBefore[i] は changing[i] より先に済む changing の印。
	n := len(changing)
	mustBefore := make([]uint32, n)
	for i, a := range changing {
		for j, b := range changing {
			if i != j && before(g.members[b].id.Local, g.members[a].id.Local) {
				mustBefore[i] |= 1 << j
			}
		}
	}
	state := func(done uint32) map[string]string {
		st := make(map[string]string, len(g.members))
		for k, v := range now {
			st[k] = v
		}
		for i, c := range changing {
			if done&(1<<i) != 0 {
				st[c] = g.members[c].desired
			} else if _, has := now[c]; !has {
				delete(st, c)
			}
		}
		return st
	}
	full := uint32(1)<<n - 1
	for done := uint32(1); done < full; done++ {
		if !closed(done, mustBefore) || g.Table.allowed(state(done)) {
			continue
		}
		var updated []string
		for i, c := range changing {
			if done&(1<<i) != 0 {
				updated = append(updated, g.members[c].id.Local)
			}
		}
		return Finding{Group: g, Hold: hold,
			Reason: fmt.Sprintf("%s: updating %s first gives %s, which is not an allowed combination (not updating %s)",
				g.Label(), strings.Join(updated, ", "), g.describe(state(done)), strings.Join(holdNames, ", ")),
			Hint: hint(g, changing, mustBefore, state)}, true
	}
	return Finding{}, false
}

func (t *Table) componentField(name string) string {
	for _, c := range t.Components {
		if c.Name == name {
			return c.Field
		}
	}
	return DefaultField
}

// closed は done が順序の制約に閉じているか（済んだものより先に要るものが
// すべて済んでいる）。
func closed(done uint32, mustBefore []uint32) bool {
	for i, mb := range mustBefore {
		if done&(1<<i) != 0 && done&mb != mb {
			return false
		}
	}
	return true
}

// hint は 1 つずつ更新して途中がすべて表に合う順序を探し、depends_on の案内に
// する。いまの depends_on に沿う順序が無ければ、沿わない順序を探す。
func hint(g Group, changing []string, mustBefore []uint32, state func(uint32) map[string]string) string {
	order := findOrder(len(changing), mustBefore, func(done uint32) bool { return g.Table.allowed(state(done)) })
	if order == nil {
		if order = findOrder(len(changing), make([]uint32, len(changing)), func(done uint32) bool { return g.Table.allowed(state(done)) }); order == nil {
			return "no one-at-a-time order of these updates stays within the table: declare an intermediate version first, or fix the table"
		}
		return "the update order the current depends_on allows does not stay within the table; " + orderText(g, changing, order) + " (and drop the depends_on that forces the other order)"
	}
	return orderText(g, changing, order)
}

func orderText(g Group, changing []string, order []int) string {
	var seq, edges []string
	for k, i := range order {
		seq = append(seq, g.members[changing[i]].id.Local)
		if k > 0 {
			edges = append(edges, fmt.Sprintf("%s depends_on %s", g.members[changing[i]].id.Local, g.members[changing[order[k-1]]].id.Local))
		}
	}
	return fmt.Sprintf("the order %s stays within the table: declare %s", strings.Join(seq, " → "), strings.Join(edges, ", "))
}

// findOrder は制約に沿い、途中（済んだ集合）がすべて ok な順序を深さ優先で探す。
func findOrder(n int, mustBefore []uint32, ok func(uint32) bool) []int {
	full := uint32(1)<<n - 1
	seen := map[uint32]bool{}
	var walk func(done uint32, path []int) []int
	walk = func(done uint32, path []int) []int {
		if done == full {
			return path
		}
		if seen[done] {
			return nil
		}
		seen[done] = true
		for i := 0; i < n; i++ {
			bit := uint32(1) << i
			if done&bit != 0 || done&mustBefore[i] != mustBefore[i] {
				continue
			}
			next := done | bit
			if next != full && !ok(next) {
				continue
			}
			if p := walk(next, append(path, i)); p != nil {
				return p
			}
		}
		return nil
	}
	return walk(0, nil)
}

// dependsClosure は before(a, b)（b が推移的に a に depends_on する）を返す。
func dependsClosure(desired []diff.DesiredResource) func(a, b string) bool {
	deps := make(map[string][]string, len(desired))
	for _, r := range desired {
		deps[r.ID.Local] = r.DependsOn
	}
	memo := map[string]map[string]bool{}
	var reach func(b string) map[string]bool
	reach = func(b string) map[string]bool {
		if m, ok := memo[b]; ok {
			return m
		}
		m := map[string]bool{}
		memo[b] = m // depends_on は読み込みで循環を断っている
		for _, a := range deps[b] {
			m[a] = true
			for x := range reach(a) {
				m[x] = true
			}
		}
		return m
	}
	return func(a, b string) bool { return reach(b)[a] }
}
