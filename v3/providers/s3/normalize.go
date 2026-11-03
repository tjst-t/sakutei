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

package s3

// normalize.go — policy・lifecycle・tags を比べるための正規形（v2 の
// internal/s3bucket から移した。永久のずれを出さない、E-C1・E-I7）。宣言も観測も
// 同じ関数で正規形にし、正規形どうしを比べる（FieldSpec.Match）。
//
//   - policy：JSON のオブジェクト（intent では YAML のマップ）。キーの順・空白を
//     捨て、IAM の決まりで意味が同じ書き方をそろえる：Statement が 1 つの
//     オブジェクトなら配列に、Action・NotAction・Resource・NotResource と、
//     Principal・NotPrincipal・Condition の値の文字列は 1 要素の配列にして並べる
//     （重なりは落とす）。Principal の "*" は {"AWS": ["*"]} と同じとみなす。
//     Statement の順は意味を持つことがあるので並べ替えない。
//   - lifecycle：ルールの一覧（要素は S3 の XML の Rule をそのままマップにした形）。
//     正規形では値をすべて文字列にし（30 と "30" は同じ）、日時は RFC 3339 の UTC
//     に、空の値（空の文字列・空のマップ）を落とし、1 要素の配列は中身だけに、
//     配列（ルールの並びも）は正規形の JSON の順に並べる。ルールの順は S3 では
//     意味を持たない。
//   - tags：キー → 値のマップ（値は文字列）。
//
// どれも空（ポリシーが無い・ルールが無い・タグが無い）は空のマップ・空の一覧。

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
)

// ---- JSON ----

// canonJSON は v をキーの順・空白なしで書く（HTML の文字をエスケープしない）。
func canonJSON(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimSuffix(b.String(), "\n")
}

// decodeJSON は数を json.Number のまま読む。
func decodeJSON(s string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("JSON の値のあとに余りがある")
	}
	return v, nil
}

// ---- policy ----

// CanonPolicy は policy の JSON の正規形を返す。空なら ""。
func CanonPolicy(s string) (string, error) {
	if strings.TrimSpace(s) == "" {
		return "", nil
	}
	v, err := decodeJSON(s)
	if err != nil {
		return "", fmt.Errorf("policy が JSON として読めない: %v", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return "", errors.New("policy は JSON のオブジェクトで書く")
	}
	if len(m) == 0 {
		return "", nil
	}
	return canonJSON(canonPolicyValue("", m)), nil
}

// canonPolicyOf は宣言・観測の policy（マップ）の正規形を返す。空のマップは ""。
func canonPolicyOf(v any) (string, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return "", errors.New("policy はマップで書く（JSON のポリシーの文書）")
	}
	if len(m) == 0 {
		return "", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return CanonPolicy(string(b))
}

var policySetKeys = map[string]bool{"Action": true, "NotAction": true, "Resource": true, "NotResource": true}

func canonPolicyValue(key string, v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			switch {
			case k == "Statement":
				if obj, ok := x.(map[string]any); ok {
					x = []any{obj}
				}
				out[k] = canonPolicyValue(k, x)
			case policySetKeys[k]:
				out[k] = stringSet(x)
			case k == "Principal" || k == "NotPrincipal":
				if x == "*" {
					// "*" は {"AWS": "*"} と同じ（IAM の決まり。endpoint によっては後者で持つ）。
					x = map[string]any{"AWS": "*"}
				}
				if pm, ok := x.(map[string]any); ok {
					pp := make(map[string]any, len(pm))
					for pk, pv := range pm {
						pp[pk] = stringSet(pv)
					}
					out[k] = pp
				} else {
					out[k] = x
				}
			case k == "Condition":
				if cm, ok := x.(map[string]any); ok {
					cc := make(map[string]any, len(cm))
					for op, ov := range cm {
						if om, ok := ov.(map[string]any); ok {
							oo := make(map[string]any, len(om))
							for ck, cv := range om {
								oo[ck] = stringSet(cv)
							}
							cc[op] = oo
						} else {
							cc[op] = ov
						}
					}
					out[k] = cc
				} else {
					out[k] = x
				}
			default:
				out[k] = canonPolicyValue(k, x)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = canonPolicyValue(key, x)
		}
		return out
	}
	return v
}

// stringSet は文字列か文字列の配列を、並べて重なりを落とした配列にする。それ
// 以外（オブジェクトなど）はそのまま。
func stringSet(v any) any {
	var xs []string
	switch t := v.(type) {
	case string:
		xs = []string{t}
	case json.Number:
		xs = []string{t.String()}
	case bool:
		xs = []string{fmt.Sprint(t)}
	case []any:
		for _, x := range t {
			switch s := x.(type) {
			case string:
				xs = append(xs, s)
			case json.Number:
				xs = append(xs, s.String())
			case bool:
				xs = append(xs, fmt.Sprint(s))
			default:
				return v
			}
		}
	default:
		return v
	}
	sort.Strings(xs)
	out := make([]any, 0, len(xs))
	for i, x := range xs {
		if i > 0 && xs[i-1] == x {
			continue
		}
		out = append(out, x)
	}
	return out
}

// policyValue は endpoint が返したポリシー（JSON）を観測の値（正規形のマップ）に
// する。空なら空のマップ。
func policyValue(raw string) (map[string]any, error) {
	c, err := CanonPolicy(raw)
	if err != nil {
		return nil, err
	}
	if c == "" {
		return map[string]any{}, nil
	}
	v, err := decodeJSON(c)
	if err != nil {
		return nil, err
	}
	n, err := core.Normalize(core.Map, v) // 観測の値は正規形の不動点にする
	if err != nil {
		return nil, err
	}
	return n.(map[string]any), nil
}

// ---- tags ----

// ParseTags は tags の宣言（キー → 文字列のマップ）を読む。
func ParseTags(v any) (map[string]string, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("tags はキー → 値のマップで書く（{env: prod} など）")
	}
	out := make(map[string]string, len(m))
	for k, x := range m {
		sv, ok := x.(string)
		if !ok {
			return nil, fmt.Errorf("tags の %q の値は文字列で書く", k)
		}
		if k == "" {
			return nil, errors.New("tags に空のキーがある")
		}
		out[k] = sv
	}
	return out, nil
}

// tagsValue はタグを観測の値にする。
func tagsValue(tags map[string]string) map[string]any {
	out := make(map[string]any, len(tags))
	for k, v := range tags {
		out[k] = v
	}
	return out
}

// ---- lifecycle ----

// ParseLifecycle は lifecycle の宣言（ルールの一覧）を読み、確かめる。各ルールは
// ID（空でない文字列）と Status（Enabled・Disabled）を持つ。
func ParseLifecycle(v any) ([]map[string]any, error) {
	arr, ok := v.([]any)
	if !ok {
		return nil, errors.New(`lifecycle はルールの一覧で書く（[{ID: expire-logs, Status: Enabled, Filter: {Prefix: logs/}, Expiration: {Days: 30}}] など）`)
	}
	rules := make([]map[string]any, 0, len(arr))
	ids := map[string]bool{}
	for i, x := range arr {
		r, ok := x.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("lifecycle[%d]: ルールはマップで書く", i)
		}
		id, _ := r["ID"].(string)
		if strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("lifecycle[%d]: ID を書く（無いと endpoint が作った ID と比べて永久にずれる）", i)
		}
		if ids[id] {
			return nil, fmt.Errorf("lifecycle[%d]: ID %q が重なる", i, id)
		}
		ids[id] = true
		if st, _ := r["Status"].(string); st != "Enabled" && st != "Disabled" {
			return nil, fmt.Errorf("lifecycle[%d]（%s）: Status は Enabled か Disabled", i, id)
		}
		if err := checkXMLNames(r, fmt.Sprintf("lifecycle[%d]", i)); err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, nil
}

// checkXMLNames は XML の要素の名前にできないキーと、配列の中の配列を断る。
func checkXMLNames(v any, at string) error {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if !xmlName(k) {
				return fmt.Errorf("%s: %q は XML の要素の名前にできない", at, k)
			}
			if arr, ok := x.([]any); ok {
				for i, y := range arr {
					if _, nested := y.([]any); nested {
						return fmt.Errorf("%s.%s[%d]: 配列の中の配列は XML にできない", at, k, i)
					}
					if err := checkXMLNames(y, fmt.Sprintf("%s.%s[%d]", at, k, i)); err != nil {
						return err
					}
				}
				continue
			}
			if err := checkXMLNames(x, at+"."+k); err != nil {
				return err
			}
		}
	case nil:
		return fmt.Errorf("%s: null は XML にできない", at)
	}
	return nil
}

func xmlName(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		ok := c == '_' || ('A' <= c && c <= 'Z') || ('a' <= c && c <= 'z') || (i > 0 && (('0' <= c && c <= '9') || c == '-' || c == '.'))
		if !ok {
			return false
		}
	}
	return true
}

// rulesOf は lifecycle の値（宣言か観測の一覧）を確かめずにルールの一覧にする。
func rulesOf(v any) ([]map[string]any, bool) {
	arr, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]map[string]any, 0, len(arr))
	for _, x := range arr {
		m, ok := x.(map[string]any)
		if !ok {
			return nil, false
		}
		out = append(out, m)
	}
	return out, true
}

// canonRules はルールの一覧の正規形（ルールの正規形を正規形の JSON の順に並べた
// 一覧。ルールが無ければ空の一覧）。
func canonRules(rules []map[string]any) []any {
	type item struct {
		v   any
		key string
	}
	items := make([]item, 0, len(rules))
	for _, r := range rules {
		c := canonLeaf(r)
		if c == nil {
			continue
		}
		items = append(items, item{c, canonJSON(c)})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].key < items[j].key })
	out := make([]any, len(items))
	for i, it := range items {
		out[i] = it.v
	}
	return out
}

// canonLeaf は lifecycle の値の正規形（空なら nil）。
func canonLeaf(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, x := range t {
			if c := canonLeaf(x); c != nil {
				out[k] = c
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case []any:
		type item struct {
			v   any
			key string
		}
		var items []item
		for _, x := range t {
			if c := canonLeaf(x); c != nil {
				items = append(items, item{c, canonJSON(c)})
			}
		}
		switch len(items) {
		case 0:
			return nil
		case 1:
			return items[0].v
		}
		sort.Slice(items, func(i, j int) bool { return items[i].key < items[j].key })
		out := make([]any, len(items))
		for i, it := range items {
			out[i] = it.v
		}
		return out
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return nil
		}
		if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return ts.UTC().Format(time.RFC3339)
		}
		return s
	case json.Number:
		return t.String()
	case bool:
		return fmt.Sprint(t)
	case float64:
		if t == math.Trunc(t) && !math.IsInf(t, 0) {
			return fmt.Sprint(int64(t))
		}
		return fmt.Sprint(t)
	case nil:
		return nil
	}
	return fmt.Sprint(v)
}

// lifecycleXML はルールの一覧を PutBucketLifecycleConfiguration の本文にする。
func lifecycleXML(rules []map[string]any) []byte {
	var b strings.Builder
	b.WriteString(`<LifecycleConfiguration xmlns="` + s3NS + `">`)
	for _, r := range rules {
		b.WriteString("<Rule>")
		writeXMLElems(&b, r, true)
		b.WriteString("</Rule>")
	}
	b.WriteString("</LifecycleConfiguration>")
	return []byte(b.String())
}

// ruleKeyOrder は Rule の子の先頭に置く順（S3 の文書の順）。残りは名前の順。
var ruleKeyOrder = []string{"ID", "Filter", "Prefix", "Status"}

func writeXMLElems(b *strings.Builder, m map[string]any, rule bool) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if rule {
		var head, rest []string
		for _, k := range ruleKeyOrder {
			if _, ok := m[k]; ok {
				head = append(head, k)
			}
		}
		for _, k := range keys {
			if !contains(ruleKeyOrder, k) {
				rest = append(rest, k)
			}
		}
		keys = append(head, rest...)
	}
	for _, k := range keys {
		writeXMLValue(b, k, m[k])
	}
}

func writeXMLValue(b *strings.Builder, name string, v any) {
	switch t := v.(type) {
	case []any:
		for _, x := range t {
			writeXMLValue(b, name, x)
		}
	case map[string]any:
		b.WriteString("<" + name + ">")
		writeXMLElems(b, t, false)
		b.WriteString("</" + name + ">")
	default:
		var s string
		switch x := t.(type) {
		case string:
			s = x
		case json.Number:
			s = x.String()
		default:
			s = fmt.Sprint(x)
		}
		b.WriteString("<" + name + ">" + xmlEscape(s) + "</" + name + ">")
	}
}

// xmlNode は XML の汎用の木。
type xmlNode struct {
	name     string
	text     string
	children []*xmlNode
}

func parseXMLTree(data []byte) (*xmlNode, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var stack []*xmlNode
	var root *xmlNode
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &xmlNode{name: t.Name.Local}
			if len(stack) > 0 {
				p := stack[len(stack)-1]
				p.children = append(p.children, n)
			} else if root == nil {
				root = n
			}
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) == 0 {
				return nil, errors.New("XML の要素の対が合わない")
			}
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text += string(t)
			}
		}
	}
	if root == nil {
		return nil, errors.New("XML が空")
	}
	if len(stack) != 0 {
		return nil, errors.New("XML が途中で切れている")
	}
	return root, nil
}

// toJSONValue は要素を値にする（子があればマップ、同じ名前の子が複数なら配列、
// 子が無ければ文字列）。
func (n *xmlNode) toJSONValue() any {
	if len(n.children) == 0 {
		return strings.TrimSpace(n.text)
	}
	out := map[string]any{}
	for _, c := range n.children {
		v := c.toJSONValue()
		if prev, ok := out[c.name]; ok {
			if arr, isArr := prev.([]any); isArr {
				out[c.name] = append(arr, v)
			} else {
				out[c.name] = []any{prev, v}
			}
			continue
		}
		out[c.name] = v
	}
	return out
}

// lifecycleFromXML は GetBucketLifecycleConfiguration の本文をルールの一覧にする。
func lifecycleFromXML(data []byte) ([]map[string]any, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	root, err := parseXMLTree(data)
	if err != nil {
		return nil, err
	}
	if root.name != "LifecycleConfiguration" {
		return nil, fmt.Errorf("根の要素が %q", root.name)
	}
	var rules []map[string]any
	for _, c := range root.children {
		if c.name != "Rule" {
			continue
		}
		if m, ok := c.toJSONValue().(map[string]any); ok {
			rules = append(rules, m)
		}
	}
	return rules, nil
}

// aliasStorageClass は rules の StorageClass（Transition・
// NoncurrentVersionTransition の中など、どの深さでも）を aliases で標準の名前に
// 読み替える。rules をその場で書き換える（E-C1）。
func aliasStorageClass(rules []map[string]any, aliases map[string]string) {
	if len(aliases) == 0 {
		return
	}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, x := range t {
				if s, ok := x.(string); ok && k == "StorageClass" {
					if to, ok := aliases[strings.TrimSpace(s)]; ok {
						t[k] = to
					}
					continue
				}
				walk(x)
			}
		case []any:
			for _, x := range t {
				walk(x)
			}
		}
	}
	for _, r := range rules {
		walk(r)
	}
}
