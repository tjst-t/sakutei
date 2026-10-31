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

// Package condition は観測についての三値の式（設計 §3.4）を読み、評価する。
//
//	式      := 論理和
//	論理和  := 論理積 ("or" 論理積)*
//	論理積  := 否定 ("and" 否定)*
//	否定    := "not" 否定 | 比較
//	比較    := 項 (演算子 項)?            演算子: == != < <= > >= in contains
//	項      := 定数 | 参照 | 量化 | "(" 式 ")"
//	参照    := observe(<type>/<id>).<項目>(.<項目>)*
//	         | present(<type>/<id>) | absent(<type>/<id>)
//	         | desired.<項目> | it.<項目> | check(<名前>)
//	量化    := all(<選び方>, 式) | any(<選び方>, 式) | count(<選び方>, 式)
//	選び方  := <type>[<ラベル>=<値>, ...]
//	定数    := "文字列" | '文字列' | 数 | true | false | [定数, ...]
//
// 算術・文字列の加工・現在時刻は持たない（N-G4・N-R2・E-I4：期限は provider が
// 残り時間を観測の値として返す）。
//
// 三値の決まり：
//   - 参照が Unknown・Converging・観測していない対象を指すと不明。Absent の対象と
//     観測に無い項目は「無い」（null）で、不明ではない。
//   - 不明を含む比較は不明。null との == は偽、!= は真、大小・in・contains は偽。
//   - and・or・not は三値の論理（偽 and 不明 = 偽、真 or 不明 = 真）。
//   - all は 1 件でも偽なら偽、偽が無く不明があれば不明。any は 1 件でも真なら真。
//   - count(...) op N は、真の件数 k と不明の件数 u から、k〜k+u のどれでも同じ
//     結果なら真偽、変わるなら不明。
//   - 大小は数どうしと、長さ（"720h"）どうしだけ。比べられない組は不明。
package condition

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
)

// Expr は読んだ式。core.Condition を満たす。
type Expr struct {
	src  string
	root node
}

var _ core.Condition = (*Expr)(nil)

// Parse は式を読む。形の誤りは読み込みで断る（不変条件 25、E-M3）。
func Parse(src string) (*Expr, error) {
	p := &parser{src: src}
	p.skip()
	if p.eof() {
		return nil, fmt.Errorf("式が空")
	}
	n, err := p.expr()
	if err != nil {
		return nil, err
	}
	p.skip()
	if !p.eof() {
		return nil, p.errorf("式の終わりのあとに %q がある", p.rest())
	}
	if _, isCount := n.(countN); isCount {
		return nil, fmt.Errorf("count(...) は比較の中で使う（例 count(...) >= 3）")
	}
	return &Expr{src: src, root: n}, nil
}

// MustParse は Parse して、誤りなら panic する（テスト用）。
func MustParse(src string) *Expr {
	e, err := Parse(src)
	if err != nil {
		panic(err)
	}
	return e
}

// String は書いたままの式。
func (e *Expr) String() string { return e.src }

// Eval は env の観測だけで評価する（E-J9）。
func (e *Expr) Eval(env core.ConditionEnv) core.Tri {
	return truth(e.root.eval(&scope{env: env}))
}

// Refs は観測すべき対象。
func (e *Expr) Refs(sel func(core.Selection) []core.ID) []core.ID {
	var out []core.ID
	walk(e.root, func(n node) {
		switch x := n.(type) {
		case obsN:
			out = append(out, x.id)
		case presN:
			out = append(out, x.id)
		case quantN:
			if sel != nil {
				out = append(out, sel(x.sel)...)
			}
		case countN:
			if sel != nil {
				out = append(out, sel(x.sel)...)
			}
		}
	})
	return dedup(out)
}

// Direct は名指しで参照する対象（順序の辺、E-J7）。
func (e *Expr) Direct() []core.ID {
	var out []core.ID
	walk(e.root, func(n node) {
		switch x := n.(type) {
		case obsN:
			out = append(out, x.id)
		case presN:
			out = append(out, x.id)
		}
	})
	return dedup(out)
}

// Checks は使う確かめの名前。
func (e *Expr) Checks() []string {
	seen := map[string]bool{}
	var out []string
	walk(e.root, func(n node) {
		if c, ok := n.(checkN); ok && !seen[c.name] {
			seen[c.name] = true
			out = append(out, c.name)
		}
	})
	sort.Strings(out)
	return out
}

// KindLookup は "<provider>/<種類>" の Schema を引く。無ければ理由を返す。
type KindLookup func(typ string) (core.KindSchema, string)

// Lint は式の参照を Schema と突き合わせる（E-M3：知らない provider・種類・項目・
// 確かめは読み込みで断る）。desired は desired.項目 を引く宣言の種類（無ければ nil
// で、desired を書けない）。
func Lint(c core.Condition, kinds KindLookup, desired *core.KindSchema, hasCheck func(string) bool) []string {
	e, ok := c.(*Expr)
	if !ok {
		return nil
	}
	var out []string
	field := func(what string, ks core.KindSchema, path []string) {
		if _, ok := ks.Fields[path[0]]; !ok {
			out = append(out, fmt.Sprintf("%s に項目 %q が無い（ある項目: %s）", what, path[0], strings.Join(fieldNames(ks), ", ")))
		}
	}
	var visit func(n node, it *core.Selection)
	visit = func(n node, it *core.Selection) {
		switch x := n.(type) {
		case obsN:
			if ks, why := kinds(x.id.Type); why != "" {
				out = append(out, fmt.Sprintf("observe(%s): %s", x.id, why))
			} else {
				field(fmt.Sprintf("observe(%s)", x.id), ks, x.path)
			}
		case presN:
			if _, why := kinds(x.id.Type); why != "" {
				out = append(out, fmt.Sprintf("%s(%s): %s", x.name(), x.id, why))
			}
		case desN:
			if desired == nil {
				out = append(out, "desired は手順の対象か宣言の中でだけ書ける")
			} else {
				field("desired", *desired, x.path)
			}
		case itN:
			if it != nil {
				if ks, why := kinds(it.Type); why == "" {
					field("it（"+it.Type+"）", ks, x.path)
				}
			}
		case checkN:
			if hasCheck == nil || !hasCheck(x.name) {
				out = append(out, fmt.Sprintf("check(%s): 登録した確かめに無い", x.name))
			}
		case quantN:
			if _, why := kinds(x.sel.Type); why != "" {
				out = append(out, fmt.Sprintf("%s(%s): %s", x.kind, x.sel.Type, why))
			}
			visit(x.body, &x.sel)
			return
		case countN:
			if _, why := kinds(x.sel.Type); why != "" {
				out = append(out, fmt.Sprintf("count(%s): %s", x.sel.Type, why))
			}
			visit(x.body, &x.sel)
			return
		}
		for _, c := range children(n) {
			visit(c, it)
		}
	}
	visit(e.root, nil)
	return out
}

func fieldNames(ks core.KindSchema) []string {
	names := make([]string, 0, len(ks.Fields))
	for n := range ks.Fields {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func dedup(ids []core.ID) []core.ID {
	seen := map[core.ID]bool{}
	out := ids[:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// ---- 値 ----

type vkind int

const (
	vUnknown vkind = iota // 不明
	vNull                 // 無い（Absent の対象・観測に無い項目）
	vVal                  // 値
	vCount                // count の結果（真の件数 lo、不明を全部真とみた件数 hi）
)

type val struct {
	k      vkind
	v      any
	lo, hi int
}

var unknown = val{}

func boolVal(t core.Tri) val {
	switch t {
	case core.TriTrue:
		return val{k: vVal, v: true}
	case core.TriFalse:
		return val{k: vVal, v: false}
	}
	return unknown
}

// truth は値を三値にする。無い（null）は偽、真偽でない値は不明。
func truth(v val) core.Tri {
	switch v.k {
	case vNull:
		return core.TriFalse
	case vVal:
		if b, ok := v.v.(bool); ok {
			if b {
				return core.TriTrue
			}
			return core.TriFalse
		}
	}
	return core.TriUnknown
}

type scope struct {
	env core.ConditionEnv
	it  *core.ID
}

// ---- 構文木 ----

type node interface {
	eval(s *scope) val
}

type orN struct{ xs []node }
type andN struct{ xs []node }
type notN struct{ x node }
type cmpN struct {
	op   string
	l, r node
}
type constN struct{ v any }
type obsN struct {
	id   core.ID
	path []string
}
type presN struct {
	id     core.ID
	absent bool
}
type desN struct{ path []string }
type itN struct{ path []string }
type checkN struct{ name string }
type quantN struct {
	kind string // all | any
	sel  core.Selection
	body node
}
type countN struct {
	sel  core.Selection
	body node
}

func (p presN) name() string {
	if p.absent {
		return "absent"
	}
	return "present"
}

func children(n node) []node {
	switch x := n.(type) {
	case orN:
		return x.xs
	case andN:
		return x.xs
	case notN:
		return []node{x.x}
	case cmpN:
		return []node{x.l, x.r}
	case quantN:
		return []node{x.body}
	case countN:
		return []node{x.body}
	}
	return nil
}

func walk(n node, f func(node)) {
	f(n)
	for _, c := range children(n) {
		walk(c, f)
	}
}

func (n orN) eval(s *scope) val {
	out := core.TriFalse
	for _, x := range n.xs {
		switch truth(x.eval(s)) {
		case core.TriTrue:
			return boolVal(core.TriTrue)
		case core.TriUnknown:
			out = core.TriUnknown
		}
	}
	return boolVal(out)
}

func (n andN) eval(s *scope) val {
	out := core.TriTrue
	for _, x := range n.xs {
		switch truth(x.eval(s)) {
		case core.TriFalse:
			return boolVal(core.TriFalse)
		case core.TriUnknown:
			out = core.TriUnknown
		}
	}
	return boolVal(out)
}

func (n notN) eval(s *scope) val {
	switch truth(n.x.eval(s)) {
	case core.TriTrue:
		return boolVal(core.TriFalse)
	case core.TriFalse:
		return boolVal(core.TriTrue)
	}
	return unknown
}

func (n constN) eval(*scope) val { return val{k: vVal, v: n.v} }

func observed(s *scope, id core.ID, path []string) val {
	o, ok := s.env.Observation(id)
	if !ok {
		return unknown
	}
	switch o.Availability {
	case core.Present:
		return lookup(map[string]any(o.Fields), path)
	case core.Absent:
		return val{k: vNull}
	}
	return unknown
}

func lookup(m map[string]any, path []string) val {
	var cur any = m
	for _, seg := range path {
		var mm map[string]any
		switch x := cur.(type) {
		case map[string]any:
			mm = x
		case core.Fields:
			mm = x
		default:
			return val{k: vNull}
		}
		v, ok := mm[seg]
		if !ok || v == nil {
			return val{k: vNull}
		}
		cur = v
	}
	return val{k: vVal, v: cur}
}

func (n obsN) eval(s *scope) val { return observed(s, n.id, n.path) }

func (n itN) eval(s *scope) val {
	if s.it == nil {
		return unknown
	}
	return observed(s, *s.it, n.path)
}

func (n desN) eval(s *scope) val {
	d := s.env.Desired()
	if d == nil {
		return val{k: vNull}
	}
	return lookup(map[string]any(d), n.path)
}

func (n presN) eval(s *scope) val {
	o, ok := s.env.Observation(n.id)
	if !ok {
		return unknown
	}
	var t core.Tri
	switch o.Availability {
	case core.Present:
		t = core.TriTrue
	case core.Absent:
		t = core.TriFalse
	default:
		return unknown
	}
	if n.absent {
		if t == core.TriTrue {
			t = core.TriFalse
		} else {
			t = core.TriTrue
		}
	}
	return boolVal(t)
}

func (n checkN) eval(s *scope) val { return boolVal(s.env.Check(n.name)) }

func (n quantN) items(s *scope) []core.Tri {
	ids := s.env.Select(n.sel)
	out := make([]core.Tri, len(ids))
	for i := range ids {
		out[i] = truth(n.body.eval(&scope{env: s.env, it: &ids[i]}))
	}
	return out
}

func (n quantN) eval(s *scope) val {
	ts := n.items(s)
	hasUnknown := false
	for _, t := range ts {
		switch {
		case n.kind == "all" && t == core.TriFalse:
			return boolVal(core.TriFalse)
		case n.kind == "any" && t == core.TriTrue:
			return boolVal(core.TriTrue)
		case t == core.TriUnknown:
			hasUnknown = true
		}
	}
	switch {
	case hasUnknown:
		return unknown
	case n.kind == "all":
		return boolVal(core.TriTrue)
	default:
		return boolVal(core.TriFalse)
	}
}

func (n countN) eval(s *scope) val {
	k, u := 0, 0
	ids := s.env.Select(n.sel)
	for i := range ids {
		switch truth(n.body.eval(&scope{env: s.env, it: &ids[i]})) {
		case core.TriTrue:
			k++
		case core.TriUnknown:
			u++
		}
	}
	return val{k: vCount, lo: k, hi: k + u}
}

func (n cmpN) eval(s *scope) val {
	l, r := n.l.eval(s), n.r.eval(s)
	if l.k == vUnknown || r.k == vUnknown {
		return unknown
	}
	// count は取りうる件数のどれでも同じ結果のときだけ真偽。
	if l.k == vCount || r.k == vCount {
		c, other, swapped := l, r, false
		if r.k == vCount {
			c, other, swapped = r, l, true
		}
		var out core.Tri
		for k := c.lo; k <= c.hi; k++ {
			cv := val{k: vVal, v: int64(k)}
			var t core.Tri
			if swapped {
				t = compare(n.op, other, cv)
			} else {
				t = compare(n.op, cv, other)
			}
			switch {
			case t == core.TriUnknown:
				return unknown
			case k == c.lo:
				out = t
			case t != out:
				return unknown
			}
		}
		return boolVal(out)
	}
	return boolVal(compare(n.op, l, r))
}

// compare は値（vVal か vNull）どうしを比べる。
func compare(op string, l, r val) core.Tri {
	tf := func(b bool) core.Tri {
		if b {
			return core.TriTrue
		}
		return core.TriFalse
	}
	switch op {
	case "==":
		return tf(equal(l, r))
	case "!=":
		return tf(!equal(l, r))
	case "in":
		if l.k == vNull || r.k == vNull {
			return core.TriFalse
		}
		list, ok := r.v.([]any)
		if !ok {
			return core.TriUnknown
		}
		for _, e := range list {
			if equal(l, val{k: vVal, v: e}) {
				return core.TriTrue
			}
		}
		return core.TriFalse
	case "contains":
		if l.k == vNull || r.k == vNull {
			return core.TriFalse
		}
		switch x := l.v.(type) {
		case []any:
			for _, e := range x {
				if equal(val{k: vVal, v: e}, r) {
					return core.TriTrue
				}
			}
			return core.TriFalse
		case string:
			if sub, ok := r.v.(string); ok {
				return tf(strings.Contains(x, sub))
			}
		case map[string]any:
			if key, ok := r.v.(string); ok {
				_, has := x[key]
				return tf(has)
			}
		}
		return core.TriUnknown
	}
	// 大小。
	if l.k == vNull || r.k == vNull {
		return core.TriFalse
	}
	var c int
	if a, ok := number(l.v); ok {
		b, ok := number(r.v)
		if !ok {
			return core.TriUnknown
		}
		c = cmp3(a, b)
	} else if a, ok := duration(l.v); ok {
		b, ok := duration(r.v)
		if !ok {
			return core.TriUnknown
		}
		c = cmp3(float64(a), float64(b))
	} else {
		return core.TriUnknown
	}
	switch op {
	case "<":
		return tf(c < 0)
	case "<=":
		return tf(c <= 0)
	case ">":
		return tf(c > 0)
	case ">=":
		return tf(c >= 0)
	}
	return core.TriUnknown
}

func cmp3(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func equal(l, r val) bool {
	if l.k == vNull || r.k == vNull {
		return l.k == r.k
	}
	if a, ok := number(l.v); ok {
		b, ok := number(r.v)
		return ok && a == b
	}
	if a, ok := l.v.(string); ok {
		b, ok := r.v.(string)
		if !ok {
			return false
		}
		if a == b {
			return true
		}
		da, aok := duration(a)
		db, bok := duration(b)
		return aok && bok && da == db
	}
	return reflect.DeepEqual(numbersToFloat(l.v), numbersToFloat(r.v))
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, !math.IsNaN(n)
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func duration(v any) (time.Duration, bool) {
	s, ok := v.(string)
	if !ok {
		return 0, false
	}
	d, err := time.ParseDuration(s)
	return d, err == nil
}

func numbersToFloat(v any) any {
	switch x := v.(type) {
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = numbersToFloat(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = numbersToFloat(e)
		}
		return out
	}
	if f, ok := number(v); ok {
		return f
	}
	return v
}
