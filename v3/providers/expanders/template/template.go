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

// Package template は Expand のモジュール go:template（設計 §3.8、E-A11・N-A14・
// N-E7・N-B15）。intent の templates: に、ホストなどの一覧（外部入力か intent に
// 書いた items）の 1 件ごとに宣言を作るテンプレートを書く。全ホストに共通の値
// （バージョンなど）は vars に 1 か所で書き、ホストごとの差は一覧の値で埋める。
//
//	expand:
//	  - kind: go:template
//	inputs:
//	  - {name: inventory, items: hosts, expected_min: 2}
//	templates:
//	  - name: chrony
//	    for_each: inventory.hosts         # 外部入力の名前と、その中のホストのリストの field。または items: [...]
//	    vars: {ntp: 192.0.2.123, version: "4.5"}
//	    resources:
//	      - type: host/file
//	        id: "{item.name}:/etc/chrony.conf"
//	        labels: {site: "{item.site}"}
//	        fields:
//	          content: "server {vars.ntp} iburst\n"
//	          mode: "0644"
//	      - type: pve/vm
//	        id: "{item.name}"
//	        fields:
//	          cores: "{item.cores}"          # 文字列全体が 1 つの差し込みなら値の型のまま
//	          vmid: {from_pool: vmid}        # 採番の要求もそのまま書ける
//
// 差し込みは {item}（一覧の 1 件が文字列・数のとき）・{item.<field>}（入れ子は
// {item.a.b}）・{vars.<field>}・{index}（0 から）。無い field を指せば Reject。
// テンプレートは intent の resources: と同じ形で、差し込んだあと intent と同じ
// 読み方で宣言にする。
//
// 外部入力が届かない・欠けているとき（expand.ExternalInput.Reachable が false）は
// 宣言を作らず、テンプレートの type ごとに Uncertain の診断を出す（その provider の
// Range のリソースを消さない。空の desired で消さない、E-K3・不変条件 26）。同じ入力に同じ出力（N-P5）。
package template

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/expand"
)

// Key は intent の最上位の key。
const Key = "templates"

// Expander は go:template。
type Expander struct{}

var _ expand.Expander = Expander{}

// Keys は templates。
func (Expander) Keys() []string { return []string{Key} }

var refRe = regexp.MustCompile(`\{(item|vars|index)((?:\.[A-Za-z0-9_-]+)*)\}`)

// Expand はテンプレートごとに、一覧の 1 件ずつ宣言を作る。
func (Expander) Expand(_ context.Context, in expand.Input) (expand.Output, error) {
	var out expand.Output
	for _, s := range expand.Sections(in.Intent, Key) {
		list, ok := s.Value.([]any)
		if !ok {
			out.Diagnostics = append(out.Diagnostics, diag(core.Reject, s.Origin(), Key, "templates: にリストを書く"))
			continue
		}
		for i, raw := range list {
			got := expandOne(s, i, raw, in.Inputs)
			out.Resources = append(out.Resources, got.Resources...)
			out.Diagnostics = append(out.Diagnostics, got.Diagnostics...)
		}
	}
	return out, nil
}

func diag(sev core.Severity, o core.Origin, target, format string, a ...any) core.Diagnostic {
	return core.Diagnostic{Severity: sev, Origin: o, Target: target, Message: fmt.Sprintf(format, a...)}
}

func expandOne(s expand.Section, i int, raw any, inputs map[string]expand.ExternalInput) expand.Output {
	var out expand.Output
	o := s.Origin()
	at := fmt.Sprintf("templates[%d]", i)
	m, ok := raw.(map[string]any)
	if !ok {
		out.Diagnostics = append(out.Diagnostics, diag(core.Reject, o, Key, "%s: マップを書く", at))
		return out
	}
	for k := range m {
		switch k {
		case "name", "for_each", "items", "vars", "resources":
		default:
			out.Diagnostics = append(out.Diagnostics, diag(core.Reject, o, Key, "%s: 不明な field %q（書ける field: name, for_each, items, vars, resources）", at, k))
		}
	}
	if name, _ := m["name"].(string); name != "" {
		at = "template " + name
	}
	vars, _ := m["vars"].(map[string]any)
	if m["vars"] != nil && vars == nil {
		out.Diagnostics = append(out.Diagnostics, diag(core.Reject, o, Key, "%s: vars にマップを書く", at))
	}
	tmpl, _ := m["resources"].([]any)
	if len(tmpl) == 0 {
		out.Diagnostics = append(out.Diagnostics, diag(core.Reject, o, Key, "%s: resources（テンプレート）を書く", at))
	}
	var items []any
	src, hasInput := m["for_each"].(string)
	inline, hasItems := m["items"].([]any)
	switch {
	case hasInput == hasItems:
		out.Diagnostics = append(out.Diagnostics, diag(core.Reject, o, Key, "%s: for_each（外部入力の名前）か items のどちらか 1 つを書く", at))
	case hasItems:
		items = inline
	default:
		name, path, _ := strings.Cut(src, ".")
		ext, declared := inputs[name]
		switch {
		case !declared:
			out.Diagnostics = append(out.Diagnostics, diag(core.Reject, o, Key, "%s: for_each の外部入力 %q が intent の inputs: に無い", at, name))
		case !ext.Reachable:
			// 宣言を作らず、作るはずだった type の Range のリソースを消させない（E-K3）。
			for _, typ := range types(tmpl) {
				out.Diagnostics = append(out.Diagnostics, diag(core.Uncertain, o, typ, "%s: 外部入力 %s: %s。この type の宣言が欠けているかもしれない", at, src, ext.Note))
			}
			return out
		default:
			v := ext.Data
			if path != "" {
				v, _ = lookup("input", "."+path, map[string]any{"input": v})
			}
			l, ok := v.([]any)
			if !ok {
				out.Diagnostics = append(out.Diagnostics, diag(core.Reject, o, Key, "%s: 外部入力 %s はリストでない", at, src))
			}
			items = l
		}
	}
	if len(out.Diagnostics) > 0 {
		return out
	}
	var made []any
	for idx, item := range items {
		env := map[string]any{"item": item, "vars": vars, "index": int64(idx)}
		for _, t := range tmpl {
			v, err := subst(t, env)
			if err != nil {
				out.Diagnostics = append(out.Diagnostics, diag(core.Reject, o, Key, "%s: 一覧の %d 件目: %v", at, idx, err))
				return out
			}
			made = append(made, v)
		}
	}
	if len(made) == 0 {
		return out
	}
	data, err := json.Marshal(map[string]any{"resources": made})
	if err != nil {
		out.Diagnostics = append(out.Diagnostics, diag(core.Reject, o, Key, "%s: %v", at, err))
		return out
	}
	d, diags, err := expand.DecodeJSON(o.File+"（"+at+"）", data)
	if err != nil {
		out.Diagnostics = append(out.Diagnostics, diag(core.Reject, o, Key, "%s: %v", at, err))
		return out
	}
	out.Resources = d.Resources
	out.Diagnostics = append(out.Diagnostics, diags...)
	return out
}

// types はテンプレートの type（差し込みの無いもの）。
func types(tmpl []any) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range tmpl {
		m, _ := t.(map[string]any)
		typ, _ := m["type"].(string)
		if typ != "" && !strings.Contains(typ, "{") && !seen[typ] {
			seen[typ] = true
			out = append(out, typ)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		out = []string{Key} // type が分からなければ全部の Range のリソースを消させない
	}
	return out
}

// subst は木の中の文字列の差し込みを埋める。
func subst(v any, env map[string]any) (any, error) {
	switch x := v.(type) {
	case string:
		return substString(x, env)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			s, err := subst(e, env)
			if err != nil {
				return nil, err
			}
			out[k] = s
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			s, err := subst(e, env)
			if err != nil {
				return nil, err
			}
			out[i] = s
		}
		return out, nil
	}
	return v, nil
}

func substString(s string, env map[string]any) (any, error) {
	locs := refRe.FindAllStringSubmatchIndex(s, -1)
	if len(locs) == 0 {
		return s, nil
	}
	// 文字列全体が 1 つの差し込みなら値の型のまま（数・真偽・リスト）。
	if len(locs) == 1 && locs[0][0] == 0 && locs[0][1] == len(s) {
		return lookup(s[locs[0][2]:locs[0][3]], s[locs[0][4]:locs[0][5]], env)
	}
	var b strings.Builder
	last := 0
	for _, l := range locs {
		b.WriteString(s[last:l[0]])
		v, err := lookup(s[l[2]:l[3]], s[l[4]:l[5]], env)
		if err != nil {
			return nil, err
		}
		switch x := v.(type) {
		case string:
			b.WriteString(x)
		case int64:
			b.WriteString(strconv.FormatInt(x, 10))
		case int:
			b.WriteString(strconv.Itoa(x))
		case float64:
			b.WriteString(strconv.FormatFloat(x, 'f', -1, 64))
		case bool:
			b.WriteString(strconv.FormatBool(x))
		default:
			return nil, fmt.Errorf("%s を文字列の中に埋め込めない（文字列・数・真偽だけ）", s[l[0]:l[1]])
		}
		last = l[1]
	}
	b.WriteString(s[last:])
	return b.String(), nil
}

func lookup(root, path string, env map[string]any) (any, error) {
	v := env[root]
	name := root
	for _, p := range strings.Split(strings.TrimPrefix(path, "."), ".") {
		if p == "" {
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("{%s.%s}: %s はマップでない", name, p, name)
		}
		if v, ok = m[p]; !ok {
			return nil, fmt.Errorf("{%s%s}: field %q が無い", root, path, p)
		}
		name += "." + p
	}
	if v == nil {
		return nil, fmt.Errorf("{%s%s} が無い", root, path)
	}
	return v, nil
}
