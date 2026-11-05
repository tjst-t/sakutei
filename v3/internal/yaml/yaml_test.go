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

package yaml

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseBlockAndFlow(t *testing.T) {
	src := `---
# コメント
resources:
  - type: dns/record   # 行末のコメント
    id: "www.example.com./A"
    fields: {value: 192.0.2.10, ttl: 300, tags: [a, "b, c"], url: "https://x:8006/#frag"}
    depends_on:
    - id: bmc/system/fw1
  - type: pve/vm
    id: old
    ensure: absent
ranges:
  - provider: dns
    args: {zone: example.com.}
    max_drop: 50%
    prune: true
empty: {}
none:
str: 'it''s'
version: 1.10
code: 007
country: NO
`
	n, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"resources": []any{
			map[string]any{
				"type": "dns/record", "id": "www.example.com./A",
				"fields": map[string]any{
					"value": "192.0.2.10", "ttl": 300,
					"tags": []any{"a", "b, c"}, "url": "https://x:8006/#frag",
				},
				"depends_on": []any{map[string]any{"id": "bmc/system/fw1"}},
			},
			map[string]any{"type": "pve/vm", "id": "old", "ensure": "absent"},
		},
		"ranges": []any{map[string]any{
			"provider": "dns", "args": map[string]any{"zone": "example.com."},
			"max_drop": "50%", "prune": true,
		}},
		"empty":   map[string]any{},
		"none":    nil,
		"str":     "it's",
		"version": "1.10",
		"code":    "007",
		"country": "NO",
	}
	if got := n.Value(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %#v\nwant %#v", got, want)
	}
	// 値ごとに行番号を持つ（E-M3：エラーにするとき場所を示す）。
	res := n.Map["resources"].Seq
	if res[0].Line != 4 || res[1].Line != 9 || res[0].Map["fields"].Line != 6 {
		t.Fatalf("行番号: %d %d %d", res[0].Line, res[1].Line, res[0].Map["fields"].Line)
	}
	if got := n.Keys; !reflect.DeepEqual(got[:2], []string{"resources", "ranges"}) {
		t.Fatalf("キーの順: %v", got)
	}
}

// 黙って読み違えないよう、部分集合の外は行番号つきでエラーにする。
func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"tab":        "a:\n\tb: 1\n",
		"anchor":     "a: &x 1\n",
		"alias":      "a: *x\n",
		"tag":        "a: !!str 1\n",
		"block":      "a: |\n  x\n",
		"multiline":  "a: {b: 1,\n  c: 2}\n",
		"twodocs":    "a: 1\n---\nb: 2\n",
		"dupkey":     "a: 1\na: 2\n",
		"dupflow":    "a: {b: 1, b: 2}\n",
		"merge":      "<<: x\n",
		"toplist":    "- a\n",
		"badindent":  "a:\n    b: 1\n  c: 2\n",
		"unclosed":   "a: \"x\n",
		"flowtrail":  "a: {b: 1} x\n",
		"notkeyval":  "a\n",
		"listinmap":  "a: 1\n- b\n",
		"docend":     "a: 1\n...\n",
		"flowbadkey": "a: {b}\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(src))
			if err == nil {
				t.Fatalf("reject しなかった: %q", src)
			}
			if name != "toplist" && !strings.Contains(err.Error(), "行目") {
				t.Fatalf("行番号が無い: %v", err)
			}
		})
	}
}

func TestParseJSON(t *testing.T) {
	n, err := ParseJSON([]byte(`{"b": [1, 1.5, true, null], "a": {"x": "y"}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"a": map[string]any{"x": "y"}, "b": []any{1, "1.5", true, nil}}
	if got := n.Value(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
	if !reflect.DeepEqual(n.Keys, []string{"a", "b"}) {
		t.Fatalf("キー: %v", n.Keys)
	}
	if _, err := ParseJSON([]byte(`[1]`)); err == nil {
		t.Fatal("トップレベルのリストをエラーにしなかった")
	}
}
