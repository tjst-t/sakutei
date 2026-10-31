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

// Package intent は intent のディレクトリを読み、resources: と ranges: を
// Resource・Range にする（設計 §3.1・§3.2）。知らない項目は読み込みで断る
// （不変条件 25、E-M3・N-M3）。
package intent

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/condition"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/internal/yaml"
)

// Document は intent のファイル 1 つ。
type Document struct {
	Path string // intent のディレクトリからの相対のパス（診断に出す）
	Root *yaml.Node
}

// ReadFile はファイル 1 つを拡張子で YAML か JSON として読む。
func ReadFile(path string) (*yaml.Node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var n *yaml.Node
	if strings.EqualFold(filepath.Ext(path), ".json") {
		n, err = yaml.ParseJSON(data)
	} else {
		n, err = yaml.Parse(data)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return n, nil
}

// LoadDir は dir の下の *.yaml・*.yml・*.json を名前順に読む。"." で始まる
// ディレクトリ（.git など）は見ない。
func LoadDir(dir string) ([]Document, error) {
	st, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%s はディレクトリでない", dir)
	}
	var paths []string
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		switch strings.ToLower(filepath.Ext(p)) {
		case ".yaml", ".yml", ".json":
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	docs := make([]Document, 0, len(paths))
	for _, p := range paths {
		n, err := ReadFile(p)
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			rel = p
		}
		docs = append(docs, Document{Path: filepath.ToSlash(rel), Root: n})
	}
	return docs, nil
}

// Decoded は intent から読んだ Resource・Range・Policy。
type Decoded struct {
	Resources []core.Resource
	Ranges    []core.Range
	Policies  []core.Policy
	Cycle     *core.CycleSettings // 書かなければ nil
}

// Decode は 1 つの Document の resources:・ranges:・policies:・cycle: を読む。形の誤り・知らない
// 項目は診断（Reject）にして返す。
func Decode(doc Document) (Decoded, []core.Diagnostic) {
	d := &decoder{file: doc.Path}
	var out Decoded
	top := d.object(doc.Root, "", "resources", "ranges", "policies", "cycle")
	if top == nil {
		return out, d.diags
	}
	if n := top["resources"]; n != nil {
		for i, item := range d.list(n, "resources") {
			if r, ok := d.resource(item, fmt.Sprintf("resources[%d]", i)); ok {
				out.Resources = append(out.Resources, r)
			}
		}
	}
	if n := top["ranges"]; n != nil {
		for i, item := range d.list(n, "ranges") {
			if r, ok := d.rangeOf(item, fmt.Sprintf("ranges[%d]", i)); ok {
				out.Ranges = append(out.Ranges, r)
			}
		}
	}
	if n := top["policies"]; n != nil {
		for i, item := range d.list(n, "policies") {
			if p, ok := d.policy(item, fmt.Sprintf("policies[%d]", i)); ok {
				out.Policies = append(out.Policies, p)
			}
		}
	}
	if n := top["cycle"]; n != nil {
		out.Cycle = d.cycle(n, "cycle")
	}
	return out, d.diags
}

// cycle は常駐の周の設定を読む（E-J11・E-H1）。
func (d *decoder) cycle(n *yaml.Node, path string) *core.CycleSettings {
	obj := d.object(n, path, "interval", "stuck_after")
	if obj == nil {
		return nil
	}
	c := &core.CycleSettings{Interval: core.DefaultInterval, Origin: core.Origin{File: d.file, Line: n.Line}}
	if v := obj["interval"]; v != nil {
		if s, ok := d.str(v, path+".interval"); ok {
			iv, err := time.ParseDuration(s)
			if err != nil || iv < time.Second {
				d.reject(v, path+".interval", "1s 以上の長さを書く（例 60s）")
			} else {
				c.Interval = iv
			}
		}
	}
	if v := obj["stuck_after"]; v != nil {
		i, ok := v.Scalar.(int)
		if v.Kind != yaml.Scalar || !ok || i < 1 {
			d.reject(v, path+".stuck_after", "1 以上の整数を書く")
		} else {
			c.StuckAfter = i
		}
	}
	return c
}

type decoder struct {
	file  string
	diags []core.Diagnostic
}

func (d *decoder) reject(n *yaml.Node, target, format string, a ...any) {
	line := 0
	if n != nil {
		line = n.Line
	}
	d.diags = append(d.diags, core.Diagnostic{
		Severity: core.Reject,
		Origin:   core.Origin{File: d.file, Line: line},
		Target:   target,
		Message:  fmt.Sprintf(format, a...),
	})
}

// object はマップを読み、allowed に無いキーを断る（不変条件 25 の「未知の項目」、
// E-M3）。マップでなければ nil。
func (d *decoder) object(n *yaml.Node, path string, allowed ...string) map[string]*yaml.Node {
	if n.Kind != yaml.Map {
		d.reject(n, path, "マップを書く（%s が書かれている）", n.KindName())
		return nil
	}
	ok := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		ok[a] = true
	}
	out := make(map[string]*yaml.Node, len(n.Keys))
	for _, k := range n.Keys {
		v := n.Map[k]
		if !ok[k] {
			d.reject(&yaml.Node{Line: n.LineOf(k)}, path, "知らない項目 %q（書ける項目: %s）", k, strings.Join(allowed, ", "))
			continue
		}
		out[k] = v
	}
	return out
}

func (d *decoder) list(n *yaml.Node, path string) []*yaml.Node {
	if n.Kind != yaml.Seq {
		d.reject(n, path, "リストを書く（%s が書かれている）", n.KindName())
		return nil
	}
	return n.Seq
}

func (d *decoder) str(n *yaml.Node, path string) (string, bool) {
	if n == nil {
		return "", false
	}
	s, ok := n.Scalar.(string)
	if n.Kind != yaml.Scalar || !ok {
		d.reject(n, path, "文字列を書く")
		return "", false
	}
	return s, true
}

func (d *decoder) boolean(n *yaml.Node, path string) (bool, bool) {
	b, ok := n.Scalar.(bool)
	if n.Kind != yaml.Scalar || !ok {
		d.reject(n, path, "true か false を書く")
		return false, false
	}
	return b, true
}

// stringMap はラベル・List の引数のマップを読む。値は文字列・整数・真偽を
// 文字列にする。
func (d *decoder) stringMap(n *yaml.Node, path string) (map[string]string, bool) {
	if n.Kind != yaml.Map {
		d.reject(n, path, "マップを書く（%s が書かれている）", n.KindName())
		return nil, false
	}
	out := make(map[string]string, len(n.Keys))
	good := true
	for _, k := range n.Keys {
		v := n.Map[k]
		switch s := v.Scalar.(type) {
		case string:
			out[k] = s
		case int, bool:
			out[k] = fmt.Sprint(s)
		default:
			d.reject(v, path+"."+k, "文字列を書く（%s が書かれている）", v.KindName())
			good = false
		}
	}
	return out, good
}

func (d *decoder) resource(n *yaml.Node, path string) (core.Resource, bool) {
	obj := d.object(n, path, "type", "id", "ensure", "labels", "fields", "depends_on", "reason")
	if obj == nil {
		return core.Resource{}, false
	}
	before := len(d.diags)
	r := core.Resource{Origin: core.Origin{File: d.file, Line: n.Line}}
	typ, ok := d.str(obj["type"], path+".type")
	if obj["type"] == nil {
		d.reject(n, path, "type を書く")
	} else if ok {
		if err := core.ParseType(typ); err != nil {
			d.reject(obj["type"], path+".type", "%v", err)
		}
	}
	name, _ := d.str(obj["id"], path+".id")
	if obj["id"] == nil {
		d.reject(n, path, "id を書く")
	}
	r.ID = core.ID{Type: typ, Name: name}
	target := path
	if typ != "" && name != "" {
		target = r.ID.String()
	}
	if v := obj["ensure"]; v != nil {
		s, ok := d.str(v, target+".ensure")
		switch {
		case !ok:
		case s == "present":
		case s == "absent":
			r.Ensure = core.EnsureAbsent
		default:
			d.reject(v, target, "ensure は present か absent（%q が書かれている）", s)
		}
	}
	if v := obj["labels"]; v != nil {
		r.Labels, _ = d.stringMap(v, target+".labels")
	}
	if v := obj["fields"]; v != nil {
		if v.Kind != yaml.Map {
			d.reject(v, target+".fields", "マップを書く（%s が書かれている）", v.KindName())
		} else {
			r.Fields = core.Fields(v.Value().(map[string]any))
		}
	}
	if v := obj["depends_on"]; v != nil {
		for i, dn := range d.list(v, target+".depends_on") {
			if dep, ok := d.dependency(dn, fmt.Sprintf("%s.depends_on[%d]", target, i)); ok {
				r.DependsOn = append(r.DependsOn, dep)
			}
		}
	}
	if v := obj["reason"]; v != nil {
		r.Reason, _ = d.str(v, target+".reason")
	}
	return r, len(d.diags) == before
}

// dependency は depends_on の 1 件を読む（§3.5）。id（名指しの上流、E-J7）と
// when（観測で待つ条件、E-C4・N-C8・N-J5）の少なくとも一方を書く。on_unknown は
// when と一緒に書く（N-H6）。
func (d *decoder) dependency(n *yaml.Node, path string) (core.Dependency, bool) {
	var dep core.Dependency
	obj := d.object(n, path, "id", "when", "on_unknown")
	if obj == nil {
		return dep, false
	}
	before := len(d.diags)
	if x := obj["id"]; x != nil {
		if s, ok := d.str(x, path+".id"); ok {
			ref, err := core.ParseRef(s)
			if err != nil {
				d.reject(x, path, "%v", err)
			}
			dep.On = ref
		}
	}
	if x := obj["when"]; x != nil {
		if s, ok := d.str(x, path+".when"); ok {
			c, err := condition.Parse(s)
			if err != nil {
				d.reject(x, path+".when", "%v", err)
			} else {
				dep.When = c
			}
		}
	}
	if x := obj["on_unknown"]; x != nil {
		if s, ok := d.str(x, path+".on_unknown"); ok {
			disp, err := core.ParseDisposition(s)
			if err != nil {
				d.reject(x, path+".on_unknown", "%v", err)
			}
			dep.OnUnknown = disp
		}
		if obj["when"] == nil {
			d.reject(x, path+".on_unknown", "on_unknown は when と一緒に書く")
		}
	}
	if obj["id"] == nil && obj["when"] == nil {
		d.reject(n, path, "id か when を書く")
	}
	return dep, len(d.diags) == before
}

func (d *decoder) rangeOf(n *yaml.Node, path string) (core.Range, bool) {
	obj := d.object(n, path, "provider", "args", "labels", "prune", "max_drop", "external_owners")
	if obj == nil {
		return core.Range{}, false
	}
	before := len(d.diags)
	r := core.Range{MaxDrop: core.DefaultMaxDrop, Origin: core.Origin{File: d.file, Line: n.Line}}
	if obj["provider"] == nil {
		d.reject(n, path, "provider を書く")
	} else {
		r.Provider, _ = d.str(obj["provider"], path+".provider")
	}
	if v := obj["args"]; v != nil {
		r.Args, _ = d.stringMap(v, path+".args")
	}
	if v := obj["labels"]; v != nil {
		r.Labels, _ = d.stringMap(v, path+".labels")
	}
	if v := obj["prune"]; v != nil {
		r.Prune, _ = d.boolean(v, path+".prune")
	}
	if v := obj["max_drop"]; v != nil {
		if p, ok := percent(v); ok {
			r.MaxDrop = p
		} else {
			d.reject(v, path+".max_drop", "0%% から 100%% の百分率を書く（例 50%%）")
		}
	}
	if v := obj["external_owners"]; v != nil {
		for i, en := range d.list(v, path+".external_owners") {
			ep := fmt.Sprintf("%s.external_owners[%d]", path, i)
			eobj := d.object(en, ep, "owner", "id", "id_prefix", "reason")
			if eobj == nil {
				continue
			}
			var e core.Exclusion
			if x := eobj["owner"]; x != nil {
				e.Owner, _ = d.str(x, ep+".owner")
			}
			if x := eobj["id"]; x != nil {
				e.ID, _ = d.str(x, ep+".id")
			}
			if x := eobj["id_prefix"]; x != nil {
				e.IDPrefix, _ = d.str(x, ep+".id_prefix")
			}
			if x := eobj["reason"]; x != nil {
				e.Reason, _ = d.str(x, ep+".reason")
			}
			// 誰のものか・なぜ触らないかを残す（E-D2・E-T2）。
			if e.Owner == "" {
				d.reject(en, ep, "owner を書く")
			}
			if e.Reason == "" {
				d.reject(en, ep, "reason を書く")
			}
			// 全部を外す除外は断る（不変条件 25、E-D2）。
			switch {
			case e.ID == "" && e.IDPrefix == "":
				d.reject(en, ep, "id か id_prefix を書く（空の除外は範囲の全部を外す）")
			case e.ID != "" && e.IDPrefix != "":
				d.reject(en, ep, "id と id_prefix はどちらか 1 つ")
			}
			r.ExternalOwners = append(r.ExternalOwners, e)
		}
	}
	return r, len(d.diags) == before
}

// policy は Policy を読む（§3.6）。段 2 は select・mode・budget だけ。
func (d *decoder) policy(n *yaml.Node, path string) (core.Policy, bool) {
	obj := d.object(n, path, "name", "select", "mode", "budget")
	if obj == nil {
		return core.Policy{}, false
	}
	before := len(d.diags)
	p := core.Policy{Origin: core.Origin{File: d.file, Line: n.Line}}
	if obj["name"] == nil {
		d.reject(n, path, "name を書く")
	} else if s, ok := d.str(obj["name"], path+".name"); ok {
		if s == "" {
			d.reject(obj["name"], path+".name", "空でない名前を書く")
		}
		p.Name = s
		path = "policy " + s
	}
	if v := obj["select"]; v == nil {
		d.reject(n, path, "select を書く（全部に当てるなら {all: true}）")
	} else {
		p.Select = d.selector(v, path+".select")
	}
	if v := obj["mode"]; v != nil {
		switch s, _ := d.str(v, path+".mode"); s {
		case "apply":
		case "report":
			p.Mode = core.ModeReport
		default:
			d.reject(v, path+".mode", "mode は apply か report（%q が書かれている）", s)
		}
	}
	if v := obj["budget"]; v != nil {
		p.Budget = d.budget(v, path+".budget")
	}
	return p, len(d.diags) == before
}

// selector は select を読む。何にも当たらない select は断る（E-M2）。
func (d *decoder) selector(n *yaml.Node, path string) core.Selector {
	var s core.Selector
	obj := d.object(n, path, "all", "type", "types", "labels")
	if obj == nil {
		return s
	}
	if v := obj["all"]; v != nil {
		if b, ok := d.boolean(v, path+".all"); ok {
			if !b {
				d.reject(v, path+".all", "all は true だけを書く（絞るなら type・labels）")
			}
			s.All = b
		}
	}
	addType := func(v *yaml.Node, p string) {
		t, ok := d.str(v, p)
		if !ok {
			return
		}
		if err := core.ParseType(t); err != nil {
			d.reject(v, p, "%v", err)
			return
		}
		s.Types = append(s.Types, t)
	}
	if v := obj["type"]; v != nil {
		addType(v, path+".type")
	}
	if v := obj["types"]; v != nil {
		for i, t := range d.list(v, path+".types") {
			addType(t, fmt.Sprintf("%s.types[%d]", path, i))
		}
	}
	if v := obj["labels"]; v != nil {
		s.Labels, _ = d.stringMap(v, path+".labels")
	}
	switch {
	case s.All && (len(s.Types) > 0 || len(s.Labels) > 0):
		d.reject(n, path, "all: true と type・labels は一緒に書けない")
	case !s.All && len(s.Types) == 0 && len(s.Labels) == 0 && obj["all"] == nil:
		d.reject(n, path, "当たり先を書く（all・type・types・labels）")
	}
	return s
}

// budget は予算を読む（E-J1）。書かない上限は core.Unlimited。
func (d *decoder) budget(n *yaml.Node, path string) *core.Budget {
	obj := d.object(n, path, "per_cycle", "destructive_per_cycle", "destructive_per_window", "window", "max_ineffective_applies")
	if obj == nil {
		return nil
	}
	b := &core.Budget{PerCycle: core.Unlimited, DestructivePerCycle: core.Unlimited, DestructivePerWindow: core.Unlimited,
		MaxIneffectiveApplies: core.Unlimited}
	count := func(key string, dst *int) {
		v := obj[key]
		if v == nil {
			return
		}
		i, ok := v.Scalar.(int)
		if v.Kind != yaml.Scalar || !ok || i < 0 {
			d.reject(v, path+"."+key, "0 以上の整数を書く")
			return
		}
		*dst = i
	}
	count("per_cycle", &b.PerCycle)
	count("destructive_per_cycle", &b.DestructivePerCycle)
	count("destructive_per_window", &b.DestructivePerWindow)
	// 効かない Apply の上限（E-J6）。0 は 1 回目で止まるので書けない。
	if v := obj["max_ineffective_applies"]; v != nil {
		i, ok := v.Scalar.(int)
		if v.Kind != yaml.Scalar || !ok || i < 1 {
			d.reject(v, path+".max_ineffective_applies", "1 以上の整数を書く")
		} else {
			b.MaxIneffectiveApplies = i
		}
	}
	if v := obj["window"]; v != nil {
		if s, ok := d.str(v, path+".window"); ok {
			w, err := time.ParseDuration(s)
			if err != nil || w <= 0 {
				d.reject(v, path+".window", "正の長さを書く（例 24h）")
			}
			b.Window = w
		}
	}
	// 窓と累積の上限は対で書く（片方だけでは数えられない）。
	switch {
	case obj["destructive_per_window"] != nil && obj["window"] == nil:
		d.reject(n, path, "destructive_per_window には window を書く")
	case obj["window"] != nil && obj["destructive_per_window"] == nil:
		d.reject(n, path, "window は destructive_per_window と一緒に書く")
	case len(obj) == 0:
		d.reject(n, path, "上限を 1 つ以上書く")
	}
	return b
}

// percent は "50%" か 50 を 0〜100 の整数にする。
func percent(n *yaml.Node) (int, bool) {
	var v int
	switch s := n.Scalar.(type) {
	case int:
		v = s
	case string:
		if _, err := fmt.Sscanf(s, "%d%%", &v); err != nil || fmt.Sprintf("%d%%", v) != s {
			return 0, false
		}
	default:
		return 0, false
	}
	return v, v >= 0 && v <= 100
}
