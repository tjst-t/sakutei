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

package s3bucket

// normalize.go — policy・lifecycle・tags を比べるための正規形（永久 drift を
// 出さない）。宣言も観測も同じ関数で正規形にし、正規形どうしを比べる。
//
//   - policy: JSON を読み、キー順・空白を捨てて書き直す。加えて IAM の決まりで
//     意味が同じ書き方をそろえる：Statement が 1 つのオブジェクトなら配列に、
//     Action・NotAction・Resource・NotResource と、Principal・NotPrincipal・
//     Condition の値の文字列は 1 要素の配列にして並べ（重複は落とす）。Principal
//     の "*" は {"AWS": ["*"]} と同じとみなす。
//     Statement の順は意味を持つことがあるので並べ替えない。
//   - lifecycle: ルールの一覧（JSON の配列。要素は S3 の XML の Rule をそのまま
//     JSON にした形）。正規形では値をすべて文字列にし（30 と "30" は同じ）、
//     日時は RFC 3339 の UTC に、空の値（空の文字列・空のオブジェクト）を落とし、
//     1 要素の配列は中身だけに、配列（ルールの並びも）は正規形の JSON の順に
//     並べる。ルールの順は S3 では意味を持たない。
//   - tags: キー → 値の JSON のオブジェクト。キー順に並べる。
//
// どれも空（ポリシーが無い・ルールが無い・タグが無い）の正規形は ""。

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// ---- JSON の書き出し ----

// canonJSON は v をキー順・空白なしで書く（HTML の文字をエスケープしない）。
func canonJSON(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimSuffix(b.String(), "\n")
}

func decodeJSON(s string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the JSON value")
	}
	return v, nil
}

// ---- policy ----

// CanonPolicy は policy の正規形を返す。空なら ""。
func CanonPolicy(s string) (string, error) {
	if strings.TrimSpace(s) == "" {
		return "", nil
	}
	v, err := decodeJSON(s)
	if err != nil {
		return "", fmt.Errorf("policy is not valid JSON: %v", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return "", errors.New("policy must be a JSON object")
	}
	return canonJSON(canonPolicyValue("", m)), nil
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

// stringSet は文字列か文字列の配列を、並べて重複を落とした配列にする。それ
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

// ---- tags ----

// ParseTags は tags の宣言（JSON のオブジェクト、値は文字列）を読む。
func ParseTags(s string) (map[string]string, error) {
	if strings.TrimSpace(s) == "" {
		return map[string]string{}, nil
	}
	v, err := decodeJSON(s)
	if err != nil {
		return nil, fmt.Errorf("tags is not valid JSON: %v", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New(`tags must be a JSON object such as {"env": "prod"}`)
	}
	out := make(map[string]string, len(m))
	for k, x := range m {
		sv, ok := x.(string)
		if !ok {
			return nil, fmt.Errorf("tags: the value of %q must be a string", k)
		}
		if k == "" {
			return nil, errors.New("tags: empty key")
		}
		out[k] = sv
	}
	return out, nil
}

// CanonTagMap はタグの正規形（空なら ""）。
func CanonTagMap(tags map[string]string) string {
	if len(tags) == 0 {
		return ""
	}
	return canonJSON(tags)
}

// CanonTags は tags の宣言の正規形を返す。
func CanonTags(s string) (string, error) {
	m, err := ParseTags(s)
	if err != nil {
		return "", err
	}
	return CanonTagMap(m), nil
}

// ---- lifecycle ----

// ParseLifecycle は lifecycle の宣言（ルールの JSON の配列）を読み、検査する。
// 各ルールは ID（空でない文字列）と Status（Enabled / Disabled）を持つ。
func ParseLifecycle(s string) ([]map[string]any, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	v, err := decodeJSON(s)
	if err != nil {
		return nil, fmt.Errorf("lifecycle is not valid JSON: %v", err)
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, errors.New(`lifecycle must be a JSON array of rules such as [{"ID": "expire-logs", "Status": "Enabled", "Filter": {"Prefix": "logs/"}, "Expiration": {"Days": 30}}]`)
	}
	rules := make([]map[string]any, 0, len(arr))
	ids := map[string]bool{}
	for i, x := range arr {
		r, ok := x.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("lifecycle[%d]: a rule must be a JSON object", i)
		}
		id, _ := r["ID"].(string)
		if strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("lifecycle[%d]: ID is required (without it the endpoint may make one up and the rule never compares equal)", i)
		}
		if ids[id] {
			return nil, fmt.Errorf("lifecycle[%d]: duplicate ID %q", i, id)
		}
		ids[id] = true
		if st, _ := r["Status"].(string); st != "Enabled" && st != "Disabled" {
			return nil, fmt.Errorf("lifecycle[%d] (%s): Status must be Enabled or Disabled", i, id)
		}
		if err := checkXMLNames(r, fmt.Sprintf("lifecycle[%d]", i)); err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, nil
}

// checkXMLNames は XML の要素名にできないキーと、配列の中の配列を断る。
func checkXMLNames(v any, at string) error {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if !xmlName(k) {
				return fmt.Errorf("%s: %q is not an element name", at, k)
			}
			if arr, ok := x.([]any); ok {
				for i, y := range arr {
					if _, nested := y.([]any); nested {
						return fmt.Errorf("%s.%s[%d]: an array inside an array has no XML form", at, k, i)
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
		return fmt.Errorf("%s: null has no XML form", at)
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

// CanonLifecycle は lifecycle の宣言の正規形を返す。
func CanonLifecycle(s string) (string, error) {
	rules, err := ParseLifecycle(s)
	if err != nil {
		return "", err
	}
	return canonRules(rules), nil
}

func canonRules(rules []map[string]any) string {
	if len(rules) == 0 {
		return ""
	}
	type item struct {
		v   any
		key string
	}
	items := make([]item, 0, len(rules))
	for _, r := range rules {
		c := canonLeaf(r)
		items = append(items, item{c, canonJSON(c)})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].key < items[j].key })
	out := make([]any, len(items))
	for i, it := range items {
		out[i] = it.v
	}
	return canonJSON(out)
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
				return nil, errors.New("unbalanced XML")
			}
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text += string(t)
			}
		}
	}
	if root == nil {
		return nil, errors.New("empty XML")
	}
	return root, nil
}

// toJSONValue は要素を JSON の値にする（子があればオブジェクト、同じ名前の子が
// 複数なら配列、子が無ければ文字）。
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
