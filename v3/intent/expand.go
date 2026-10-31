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

package intent

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/internal/yaml"
)

// ExpandStep は intent の expand: の 1 つ（§3.8、E-A11・N-A14・E-B5・E-A7）。
// Kind は "go:<名前>"（cli.WithExpander で組み込んだもの）か "exec:<名前>"
// （つなぎ方の設定の expanders.<名前>）。Keys は exec の Expand が読む intent の
// 最上位の項目（go の Expand は自分で言う）。
type ExpandStep struct {
	Kind   string
	Keys   []string
	Origin core.Origin
}

// Module は Kind の "go:"・"exec:" のあとの名前。
func (s ExpandStep) Module() (kind, name string) {
	k, n, _ := strings.Cut(s.Kind, ":")
	return k, n
}

// InputSpec は intent の inputs: の 1 つ（E-K3）。在りかはつなぎ方の設定の inputs。
type InputSpec struct {
	Name string
	// ExpectedMin はこれより件数が少なければ「届いたが欠けている」（0 は見ない）。
	ExpectedMin int
	// MaxDrop は前の周の件数からこれ以上（百分率）減れば欠けているとみなす
	// （-1 は見ない）。前の周の件数は Record の目印。
	MaxDrop int
	// Items は件数を数えるリストの項目（入力の最上位のマップの項目）。書かなければ
	// 最上位の項目の数。
	Items  string
	Origin core.Origin
}

// ScanExpand は全部の文書の expand: だけを先に読む（Expand のモジュールが読む
// 項目を知ってから残りを読むため）。形の誤りは Decode が断る。
func ScanExpand(docs []Document) []ExpandStep {
	var out []ExpandStep
	for _, doc := range docs {
		if doc.Root == nil || doc.Root.Kind != yaml.Map || doc.Root.Map["expand"] == nil {
			continue
		}
		d := &decoder{file: doc.Path}
		out = append(out, d.expand(doc.Root.Map["expand"])...)
	}
	return out
}

func (d *decoder) extra(top map[string]*yaml.Node, out *Decoded) {
	if n := top["pools"]; n != nil {
		for i, item := range d.list(n, "pools") {
			if p, ok := d.pool(item, fmt.Sprintf("pools[%d]", i)); ok {
				out.Pools = append(out.Pools, p)
			}
		}
	}
	if n := top["allocations"]; n != nil {
		if a, ok := d.allocation(n, "allocations"); ok {
			out.Allocations = append(out.Allocations, a)
		}
	}
	if n := top["expand"]; n != nil {
		out.Expand = d.expand(n)
	}
	if n := top["inputs"]; n != nil {
		for i, item := range d.list(n, "inputs") {
			path := fmt.Sprintf("inputs[%d]", i)
			obj := d.object(item, path, "name", "items", "expected_min", "max_drop")
			if obj == nil {
				continue
			}
			in := InputSpec{MaxDrop: -1, Origin: core.Origin{File: d.file, Line: item.Line}}
			if obj["name"] == nil {
				d.reject(item, path, "name を書く")
			} else {
				in.Name, _ = d.str(obj["name"], path+".name")
			}
			if v := obj["items"]; v != nil {
				in.Items, _ = d.str(v, path+".items")
			}
			if v := obj["expected_min"]; v != nil {
				n, ok := v.Scalar.(int)
				if !ok || n < 1 {
					d.reject(v, path+".expected_min", "1 以上の整数を書く")
				}
				in.ExpectedMin = n
			}
			if v := obj["max_drop"]; v != nil {
				if p, ok := percent(v); ok {
					in.MaxDrop = p
				} else {
					d.reject(v, path+".max_drop", "0%% から 100%% の百分率を書く（例 50%%）")
				}
			}
			out.Inputs = append(out.Inputs, in)
		}
	}
}

func (d *decoder) expand(n *yaml.Node) []ExpandStep {
	var out []ExpandStep
	for i, item := range d.list(n, "expand") {
		path := fmt.Sprintf("expand[%d]", i)
		obj := d.object(item, path, "kind", "keys")
		if obj == nil {
			continue
		}
		s := ExpandStep{Origin: core.Origin{File: d.file, Line: item.Line}}
		if obj["kind"] == nil {
			d.reject(item, path, "kind を書く（go:<名前> か exec:<名前>）")
			continue
		}
		s.Kind, _ = d.str(obj["kind"], path+".kind")
		kind, name := s.Module()
		if (kind != "go" && kind != "exec") || !validName(name) {
			d.reject(obj["kind"], path+".kind", "go:<名前> か exec:<名前> を書く（%q が書かれている）", s.Kind)
			continue
		}
		if v := obj["keys"]; v != nil {
			if kind != "exec" {
				d.reject(v, path+".keys", "keys は exec の Expand だけに書く（go の Expand は自分で読む項目を言う）")
			}
			for j, kn := range d.list(v, path+".keys") {
				if k, ok := d.str(kn, fmt.Sprintf("%s.keys[%d]", path, j)); ok {
					s.Keys = append(s.Keys, k)
				}
			}
		}
		out = append(out, s)
	}
	return out
}

// span は "100-199" を読む。
func span(s string) (lo, hi int64, ok bool) {
	a, b, cut := strings.Cut(s, "-")
	if !cut {
		return 0, 0, false
	}
	lo, err1 := strconv.ParseInt(strings.TrimSpace(a), 10, 64)
	hi, err2 := strconv.ParseInt(strings.TrimSpace(b), 10, 64)
	return lo, hi, err1 == nil && err2 == nil && lo >= 0 && lo <= hi
}

// pool は pools: の 1 つを読む（N-B13）。
func (d *decoder) pool(n *yaml.Node, path string) (core.Pool, bool) {
	obj := d.object(n, path, "name", "range", "blocks", "reuse")
	if obj == nil {
		return core.Pool{}, false
	}
	before := len(d.diags)
	p := core.Pool{Reuse: true, Origin: core.Origin{File: d.file, Line: n.Line}}
	if obj["name"] == nil {
		d.reject(n, path, "name を書く")
	} else if s, ok := d.str(obj["name"], path+".name"); ok {
		if !validName(s) {
			d.reject(obj["name"], path+".name", "名前は英数字と - _ . だけ（%q）", s)
		}
		p.Name = s
	}
	rangeOf := func(v *yaml.Node, at string) (int64, int64) {
		s, ok := d.str(v, at)
		if !ok {
			return 0, 0
		}
		lo, hi, good := span(s)
		if !good {
			d.reject(v, at, "\"<下>-<上>\" の整数の範囲を書く（%q が書かれている）", s)
		}
		return lo, hi
	}
	if obj["range"] == nil {
		d.reject(n, path, "range を書く（例 \"100-199\"）")
	} else {
		p.Lo, p.Hi = rangeOf(obj["range"], path+".range")
	}
	if v := obj["blocks"]; v != nil {
		for i, bn := range d.list(v, path+".blocks") {
			bp := fmt.Sprintf("%s.blocks[%d]", path, i)
			bobj := d.object(bn, bp, "name", "range")
			if bobj == nil {
				continue
			}
			var b core.Block
			if bobj["name"] == nil || bobj["range"] == nil {
				d.reject(bn, bp, "name と range を書く")
				continue
			}
			b.Name, _ = d.str(bobj["name"], bp+".name")
			b.Lo, b.Hi = rangeOf(bobj["range"], bp+".range")
			// block はプールの中で、ほかの block と重ならない（N-B13）。
			if b.Lo < p.Lo || b.Hi > p.Hi {
				d.reject(bn, bp, "block %s はプールの範囲 %d-%d の外", b.Name, p.Lo, p.Hi)
			}
			for _, o := range p.Blocks {
				if o.Name == b.Name {
					d.reject(bn, bp, "同じ名前の block %s が 2 つある", b.Name)
				} else if b.Lo <= o.Hi && o.Lo <= b.Hi {
					d.reject(bn, bp, "block %s が block %s と重なる", b.Name, o.Name)
				}
			}
			p.Blocks = append(p.Blocks, b)
		}
	}
	if v := obj["reuse"]; v != nil {
		p.Reuse, _ = d.boolean(v, path+".reuse")
	}
	return p, len(d.diags) == before
}

// allocation は採番の結果のファイルの allocations: を読む（N-B13）。sakutei が
// 書き足し、人は消さない（返すのは sakutei alloc release）。
func (d *decoder) allocation(n *yaml.Node, path string) (core.Allocation, bool) {
	obj := d.object(n, path, "pool", "values", "last")
	if obj == nil {
		return core.Allocation{}, false
	}
	before := len(d.diags)
	a := core.Allocation{Values: map[string]int64{}, Last: map[string]int64{}, Origin: core.Origin{File: d.file, Line: n.Line}}
	if obj["pool"] == nil {
		d.reject(n, path, "pool を書く")
	} else {
		a.Pool, _ = d.str(obj["pool"], path+".pool")
	}
	pair := func(v *yaml.Node, at, key string) (string, int64, bool) {
		o := d.object(v, at, key, "value")
		if o == nil {
			return "", 0, false
		}
		k, _ := d.str(o[key], at+"."+key)
		if o[key] == nil || o["value"] == nil {
			d.reject(v, at, "%s と value を書く", key)
			return "", 0, false
		}
		num, ok := o["value"].Scalar.(int)
		if !ok || num < 0 {
			d.reject(o["value"], at+".value", "0 以上の整数を書く")
			return "", 0, false
		}
		return k, int64(num), true
	}
	if v := obj["values"]; v != nil {
		for i, item := range d.list(v, path+".values") {
			at := fmt.Sprintf("%s.values[%d]", path, i)
			k, num, ok := pair(item, at, "key")
			if !ok {
				continue
			}
			if _, dup := a.Values[k]; dup {
				d.reject(item, at, "鍵 %q が 2 回ある", k)
			}
			a.Values[k] = num
		}
	}
	if v := obj["last"]; v != nil {
		for i, item := range d.list(v, path+".last") {
			at := fmt.Sprintf("%s.last[%d]", path, i)
			if k, num, ok := pair(item, at, "block"); ok {
				a.Last[k] = num
			}
		}
	}
	return a, len(d.diags) == before
}
