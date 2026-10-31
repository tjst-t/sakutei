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

// Decoded は intent から読んだ Resource と Range。
type Decoded struct {
	Resources []core.Resource
	Ranges    []core.Range
}

// Decode は 1 つの Document の resources: と ranges: を読む。形の誤り・知らない
// 項目は診断（Reject）にして返す。
func Decode(doc Document) (Decoded, []core.Diagnostic) {
	d := &decoder{file: doc.Path}
	var out Decoded
	top := d.object(doc.Root, "", "resources", "ranges")
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
	return out, d.diags
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
			dp := fmt.Sprintf("%s.depends_on[%d]", target, i)
			dobj := d.object(dn, dp, "id")
			if dobj == nil {
				continue
			}
			if dobj["id"] == nil {
				d.reject(dn, dp, "id を書く")
				continue
			}
			s, ok := d.str(dobj["id"], dp+".id")
			if !ok {
				continue
			}
			ref, err := core.ParseRef(s)
			if err != nil {
				d.reject(dobj["id"], dp, "%v", err)
				continue
			}
			r.DependsOn = append(r.DependsOn, core.Dependency{On: ref})
		}
	}
	if v := obj["reason"]; v != nil {
		r.Reason, _ = d.str(v, target+".reason")
	}
	return r, len(d.diags) == before
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
