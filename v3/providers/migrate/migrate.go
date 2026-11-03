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

// Package migrate は v2 の intent と provider の設定を、新しいコアの intent と
// つなぎ方の設定に書き直す（設計 §7.2 の「v2 の intent → sakutei-migrate」、§8 の
// 段 9）。範囲（ゾーン・扱う table）と除外は v2 の設定・起動の引数から intent の
// ranges へ動かす（不変条件 24）。
//
// 書き直せないもの（新しいコアに受け皿がまだ無い・捨てたもの・形の分からない
// もの）は黙って落とさず、全部を Refusal にして返す。捨てたことが意味を変えない
// もの（observe_tier・scopes の sync・version）は Notes に残す。
//
// v2 の ID（<provider>/<scope>/<local>）と新しい ID（<provider>/<種類>/<名前>）の
// 対応は Output.IDs に出す。v2 の plan と新しいコアの plan を突き合わせる
// （段 9 の完了条件）のに使う。
package migrate

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/tjst-t/sakutei/v3/internal/yaml"
	"github.com/tjst-t/sakutei/v3/providers/internal/yamlout"
)

// Input は書き直す v2 の intent と設定。
type Input struct {
	// IntentDir は v2 の intent のディレクトリ（その直下の .yaml・.yml・.json。
	// v2 と同じく下のディレクトリは読まない）。
	IntentDir string
	// ProviderConfig は v2 の --provider-config の中身（{"providers": {<名前>: {…}}}）
	// のファイル。空なら無い。
	ProviderConfig string
	// Flags は v2 の起動の引数のうち provider の設定になるもの（zone・ruleset・
	// state・redfish-config・sqlmigrate-config）。ProviderConfig より弱い（v2 と同じ）。
	Flags map[string]string
	// OutDir は書き出す先（写した fake の state の置き場をつなぎ方の設定に書く）。
	OutDir string
	// NftHost は nftables の file の対象に付けるホストの名前（既定 local）。
	NftHost string
	// Roles は v2 の承認のロールの持ち主（ロール → 名前）。新しいコアの承認は
	// 名前の並びで数えるので、roles を書いた gate にはこれが要る。
	Roles map[string][]string
	// Approvers は roles を書かない gate の承認者（v2 は誰でもよかった）。
	Approvers []string
	// SecretFiles は v2 の秘密の参照（secretref:…）→ 中身を置いたファイル。新しい
	// コアの provider は資格をファイルで受け取る。
	SecretFiles map[string]string
}

// Output は書き直した結果。
type Output struct {
	// Intent は intent のディレクトリからの相対パス → 中身。
	Intent map[string]string
	// Config はつなぎ方の設定（config.yaml）の中身。
	Config string
	// Files は OutDir からの相対パス → 中身（fake の state の写し）。
	Files map[string][]byte
	// IDs は v2 の ID → 新しい ID（<provider>/<種類>/<名前>）。
	IDs map[string]string
	// Notes は落としたもの・意味の変わるところの知らせ。
	Notes []string
}

// Refusal は書き直せなかったもの全部。
type Refusal struct{ Problems []string }

func (r *Refusal) Error() string {
	return "書き直せないものがある:\n  - " + strings.Join(r.Problems, "\n  - ")
}

// conv は書き直しの途中の状態。
type conv struct {
	in       Input
	settings map[string]map[string]any // v2 の provider の名前 → 設定
	problems []string
	notes    []string

	ids       map[string]string // v2 の ID → 新しい ID
	instances map[string]*instance
	resources []*v2res
	files     map[string][]byte
}

// v2res は v2 の宣言 1 件とその書き直し。
type v2res struct {
	file     string
	provider string
	scope    map[string]string
	local    string
	v2id     string
	node     *yaml.Node
	out      *obj   // 新しい宣言
	typ      string // 新しい種類（<provider>/<種類>）
	name     string // 新しい名前
	prune    bool
	labels   map[string]string
}

func (r *v2res) ref() string { return r.typ + "/" + r.name }

func (c *conv) refuse(format string, a ...any) {
	c.problems = append(c.problems, fmt.Sprintf(format, a...))
}
func (c *conv) note(format string, a ...any) { c.notes = append(c.notes, fmt.Sprintf(format, a...)) }

// v2ID は v2 の ID の文字列（<provider>/<k=v,…>/<local>、鍵の順）。
func v2ID(provider string, scope map[string]string, local string) string {
	parts := make([]string, 0, len(scope))
	for k, v := range scope {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return provider + "/" + strings.Join(parts, ",") + "/" + local
}

// Convert は v2 の intent と設定を書き直す。書き直せないものがあれば *Refusal。
func Convert(in Input) (*Output, error) {
	if in.NftHost == "" {
		in.NftHost = "local"
	}
	c := &conv{in: in, ids: map[string]string{}, instances: map[string]*instance{}, files: map[string][]byte{}}
	if err := c.loadSettings(); err != nil {
		return nil, err
	}
	docs, err := readDir(in.IntentDir)
	if err != nil {
		return nil, err
	}
	type pending struct {
		file string
		out  *obj
		wfs  []*yaml.Node
		pols []any
	}
	var files []pending
	for _, d := range docs {
		p := pending{file: d.name, out: newObj()}
		top := d.root
		if top.Kind != yaml.Map {
			c.refuse("%s: 最上位はマップ", d.name)
			continue
		}
		var res []*v2res
		for _, k := range top.Keys {
			v := top.Map[k]
			switch k {
			case "version":
			case "resources":
				for i, item := range v.Seq {
					if r := c.resource(d.name, i, item); r != nil {
						res = append(res, r)
					}
				}
			case "pools":
				p.out.Set("pools", value(v))
			case "workflows":
				p.wfs = v.Seq
			case "observe_tiers":
				c.note("%s: observe_tiers は捨てた（新しいコアは毎周観測する。§7.2）", d.name)
			case "scopes":
				c.scopes(d.name, v)
			case "remediation":
				if pol := c.remediation(d.name, v); pol != nil {
					p.pols = append(p.pols, pol)
				}
			case "planned_change":
				c.refuse("%s: planned_change は新しいコアの Policy の allow・windows に当たるが、まだ受け皿が無い", d.name)
			default:
				c.refuse("%s: 最上位の %s は書き直せない（受け皿が無いか、この道具がまだ知らない）", d.name, k)
			}
		}
		c.resources = append(c.resources, res...)
		files = append(files, p)
	}
	// 宣言の ID がそろってから、依存・手順・設定を書き直す（別のファイルの ID を
	// 参照する）。
	for _, r := range c.resources {
		c.dependsOn(r)
	}
	for i := range files {
		for j, wf := range files[i].wfs {
			for _, p := range c.workflow(files[i].file, j, wf) {
				files[i].out.Set("procedures", appendSeq(files[i].out.Get("procedures"), p))
			}
		}
	}
	for _, r := range c.resources {
		for i := range files {
			if files[i].file == r.file {
				c.taskProcedure(r, files[i].out)
			}
		}
	}
	if len(files) > 0 {
		if pol := c.runtimePolicy(); pol != nil {
			files[0].pols = append(files[0].pols, pol)
		}
	}
	extra := c.extraResources()
	ranges := c.ranges()
	out := &Output{Intent: map[string]string{}, IDs: c.ids, Files: c.files}
	for i, f := range files {
		o := newObj()
		if i == 0 && len(ranges) > 0 {
			o.Set("ranges", ranges)
		}
		var rs []any
		for _, r := range c.resources {
			if r.file == f.file && r.out != nil {
				rs = append(rs, r.out)
			}
		}
		if i == 0 {
			rs = append(rs, extra...)
		}
		if len(rs) > 0 {
			o.Set("resources", rs)
		}
		for _, k := range []string{"pools", "procedures"} {
			if v := f.out.Get(k); v != nil {
				o.Set(k, v)
			}
		}
		if len(f.pols) > 0 {
			o.Set("policies", f.pols)
		}
		out.Intent[f.file] = yamlout.Emit(o, "sakutei-migrate が v2 の "+f.file+" から書き直した intent（新しいコア、docs/core/README.md）。")
	}
	c.allocations(out.Intent)
	cfg, err := c.config()
	if err != nil {
		return nil, err
	}
	out.Config = cfg
	out.Notes = c.notes
	if len(c.problems) > 0 {
		return out, &Refusal{Problems: unique(c.problems)}
	}
	return out, nil
}

func unique(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range s {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// obj・newObj は書き出す YAML のマップ（yamlout）。
type obj = yamlout.Obj

func newObj() *obj { return yamlout.New() }

func appendSeq(v any, x any) []any {
	s, _ := v.([]any)
	return append(s, x)
}

type doc struct {
	name string
	root *yaml.Node
}

// readDir は v2 の intent のディレクトリの直下のファイルを名前の順に読む。
func readDir(dir string) ([]doc, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var docs []doc
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext != ".yaml" && ext != ".yml" && ext != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var n *yaml.Node
		if ext == ".json" {
			n, err = yaml.ParseJSON(data)
		} else {
			n, err = yaml.Parse(data)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if n == nil {
			continue
		}
		name := e.Name()
		if ext == ".json" {
			name = strings.TrimSuffix(name, ext) + ".yaml"
		}
		docs = append(docs, doc{name: name, root: n})
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("%s に intent のファイル（.yaml・.yml・.json）が無い", dir)
	}
	return docs, nil
}

// str は v2 の値を文字列にする（v2 の項目は文字列）。
func str(n *yaml.Node) (string, bool) {
	if n == nil || n.Kind != yaml.Scalar {
		return "", false
	}
	switch x := n.Scalar.(type) {
	case string:
		return x, true
	case int:
		return strconv.Itoa(x), true
	case bool:
		return strconv.FormatBool(x), true
	case nil:
		return "", true
	}
	return "", false
}

// strMap は v2 の文字列のマップ。
func strMap(n *yaml.Node) (map[string]string, bool) {
	if n == nil {
		return nil, true
	}
	if n.Kind != yaml.Map {
		return nil, false
	}
	m := map[string]string{}
	for _, k := range n.Keys {
		s, ok := str(n.Map[k])
		if !ok {
			return nil, false
		}
		m[k] = s
	}
	return m, true
}

// value は YAML の値を書き出す形（*obj・[]any・スカラー）にする。
func value(n *yaml.Node) any {
	switch n.Kind {
	case yaml.Map:
		o := newObj()
		for _, k := range n.Keys {
			o.Set(k, value(n.Map[k]))
		}
		return o
	case yaml.Seq:
		s := []any{}
		for _, x := range n.Seq {
			s = append(s, value(x))
		}
		return s
	}
	return n.Scalar
}

func strObj(m map[string]string) *obj {
	o := newObj()
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		o.Set(k, m[k])
	}
	return o
}

// resource は v2 の宣言 1 件を読み、provider ごとの書き直しに渡す。
func (c *conv) resource(file string, i int, n *yaml.Node) *v2res {
	at := fmt.Sprintf("%s: resources[%d]", file, i)
	if n.Kind != yaml.Map {
		c.refuse("%s: マップでない", at)
		return nil
	}
	provider, _ := str(n.Map["provider"])
	local, _ := str(n.Map["id"])
	if provider == "" || local == "" {
		c.refuse("%s: provider と id が要る", at)
		return nil
	}
	scope, ok := strMap(n.Map["scope"])
	if !ok {
		c.refuse("%s: scope は文字列のマップ", at)
		return nil
	}
	r := &v2res{file: file, provider: provider, scope: scope, local: local, v2id: v2ID(provider, scope, local), node: n}
	at = r.v2id
	m, ok := mapping[provider]
	if !ok {
		c.refuse("%s: provider %s は書き直せない（この道具が知っている provider: %s）", at, provider, strings.Join(known(), ", "))
		return nil
	}
	ensureAbsent := false
	for _, k := range n.Keys {
		v := n.Map[k]
		switch k {
		case "provider", "id", "scope", "fields", "depends_on":
		case "deletion_policy":
			switch s, _ := str(v); s {
			case "PruneWithinScope":
				r.prune = true
			case "", "ExplicitAbsentOnly":
			default:
				c.refuse("%s: deletion_policy %q は書き直せない", at, s)
			}
		case "observe_tier":
			c.note("%s: observe_tier は捨てた（新しいコアは毎周観測する）", at)
		case "ensure":
			switch s, _ := str(v); s {
			case "absent":
				ensureAbsent = true
			case "", "present":
			default:
				c.refuse("%s: ensure %q は書き直せない", at, s)
			}
		case "update_policy":
			if provider != "sqlmigrate" {
				c.refuse("%s: update_policy は sqlmigrate の strategy: task だけを書き直せる", at)
			}
		default:
			c.refuse("%s: 項目 %s は書き直せない", at, k)
		}
	}
	fields := map[string]*yaml.Node{}
	var fieldKeys []string
	if f := n.Map["fields"]; f != nil {
		if f.Kind != yaml.Map {
			c.refuse("%s: fields はマップ", at)
			return nil
		}
		for _, k := range f.Keys {
			fields[k] = f.Map[k]
			fieldKeys = append(fieldKeys, k)
		}
	}
	r.labels = scope
	o := newObj()
	r.out = o
	if !m.convert(c, r, fieldKeys, fields) {
		r.out = nil
		return r
	}
	inst := c.instance(m)
	inst.res = append(inst.res, r)
	built := newObj().Set("type", r.typ).Set("id", r.name)
	if ensureAbsent {
		built.Set("ensure", "absent")
	}
	if len(r.labels) > 0 {
		built.Set("labels", strObj(r.labels))
	}
	if f, ok := o.Get("fields").(*obj); ok && (f.Len() > 0 || !ensureAbsent) {
		built.Set("fields", f)
	}
	r.out = built
	if prev, dup := c.ids[r.v2id]; dup {
		c.refuse("%s: 2 回宣言されている（%s）", at, prev)
	}
	c.ids[r.v2id] = r.ref()
	return r
}

// dependsOn は v2 の depends_on（同じ provider の local の ID か v2 の ID）を
// 新しい ID にする。
func (c *conv) dependsOn(r *v2res) {
	if r.out == nil {
		return
	}
	n := r.node.Map["depends_on"]
	if n == nil {
		return
	}
	var deps []any
	for _, d := range n.Seq {
		id, ok := str(d)
		if d.Kind == yaml.Map {
			id, ok = str(d.Map["id"])
			if d.Map["ready_condition"] != nil {
				c.refuse("%s: depends_on の ready_condition は Condition（when）に書き直す（この道具はまだ書き直せない）", r.v2id)
			}
		}
		if !ok || id == "" {
			c.refuse("%s: depends_on の形が分からない", r.v2id)
			continue
		}
		to, found := c.ids[id]
		if !found {
			to, found = c.ids[v2ID(r.provider, r.scope, id)]
		}
		if !found {
			c.refuse("%s: depends_on の %s が宣言に無い", r.v2id, id)
			continue
		}
		deps = append(deps, newObj().Set("id", to))
	}
	if len(deps) > 0 {
		r.out.Set("depends_on", deps)
	}
}

// scopes は v2 の scopes（Sync Mode の宣言）を読む。sync の scope は収束させない
// ので、新しいコアの intent には入れず、sakutei-import で現実を書き出す
// （§7.2：同期は道具へ）。
func (c *conv) scopes(file string, n *yaml.Node) {
	for i, s := range n.Seq {
		mode, _ := str(s.Map["mode"])
		scope, _ := strMap(s.Map["scope"])
		if mode != "sync" {
			c.refuse("%s: scopes[%d] の mode %q は書き直せない（sync だけ）", file, i, mode)
			continue
		}
		c.note("%s: scope %s は Sync Mode（収束させない）。新しいコアの intent には入れない。現実の記録は sakutei-import で書き出す", file, scopeString(scope))
	}
}

func scopeString(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// remediation は v2 の remediation のうち、時間帯の無いもの（default だけ）を
// Policy にする（alert-only → report、Auto → 何も足さない）。v2 は default を
// 省くと alert-only だった。時間帯（schedule）は Policy の windows に当たるが、
// まだ受け皿が無い。
func (c *conv) remediation(file string, n *yaml.Node) any {
	if n.Kind != yaml.Map {
		c.refuse("%s: remediation はマップ", file)
		return nil
	}
	mode := "alert-only"
	for _, k := range n.Keys {
		switch k {
		case "default":
			mode, _ = str(n.Map[k])
		case "tz":
		case "schedule":
			c.refuse("%s: remediation.schedule（時間帯）は新しいコアの Policy の windows に当たるが、まだ受け皿が無い", file)
		default:
			c.refuse("%s: remediation.%s は書き直せない", file, k)
		}
	}
	switch strings.ToLower(mode) {
	case "alert-only", "alertonly":
		return newObj().Set("name", "remediation").Set("select", newObj().Set("all", true)).Set("mode", "report")
	case "auto":
		return nil
	}
	c.refuse("%s: remediation.default %q は書き直せない", file, mode)
	return nil
}

// runtimePolicy は v2 の起動の引数の予算と mode を、全部に当たる Policy にする
// （新しいコアは方針を起動の引数に持たない、§5.5）。
func (c *conv) runtimePolicy() *obj {
	budget := newObj()
	for _, f := range []struct{ flag, key string }{
		{"max-changes-per-cycle", "per_cycle"},
		{"max-destructive-per-cycle", "destructive_per_cycle"},
		{"max-destructive-per-window", "destructive_per_window"},
	} {
		v := c.in.Flags[f.flag]
		if v == "" || v == "0" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			c.refuse("--%s %q は 0 以上の整数", f.flag, v)
			continue
		}
		budget.Set(f.key, n)
	}
	if w := c.in.Flags["window-duration"]; w != "" && budget.Get("destructive_per_window") != nil {
		budget.Set("window", w)
	}
	if e := c.in.Flags["on-exceed"]; e != "" && e != "halt-all" {
		c.refuse("--on-exceed %s は書き直せない（新しいコアは予算を超えたら止める）", e)
	}
	p := newObj().Set("name", "default").Set("select", newObj().Set("all", true))
	switch m := strings.ToLower(c.in.Flags["mode"]); m {
	case "", "auto":
	case "alert-only", "alertonly":
		p.Set("mode", "report")
	default:
		c.refuse("--mode %q は書き直せない", m)
	}
	if budget.Len() > 0 {
		p.Set("budget", budget)
	}
	if p.Len() == 2 {
		return nil
	}
	return p
}
