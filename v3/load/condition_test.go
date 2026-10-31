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

package load_test

import (
	"testing"

	"github.com/tjst-t/sakutei/v3/core"
)

// depends_on の when・on_unknown を読む（§3.4・§3.5、E-C4・E-J7）。
func TestLoadConditions(t *testing.T) {
	l, err := run(t, baseConfig, map[string]string{"a.yaml": `
resources:
  - type: dns/record
    id: pool
  - type: dns/record
    id: vm
    depends_on:
      - when: 'observe(dns/record/pool).ttl < 80'
        on_unknown: block
      - id: dns/serial/s
        when: 'observe(dns/serial/s).version == desired.value and count(dns/record[site=a], it.value == "x") >= 1'
`})
	if err != nil {
		t.Fatal(err)
	}
	d := l.Resources[1].DependsOn
	if len(d) != 2 || d[0].HasOn() || d[0].OnUnknown != core.OnUnknownBlock || d[0].When.String() != "observe(dns/record/pool).ttl < 80" {
		t.Fatalf("%+v", d)
	}
	if d[1].On != (core.ID{Type: "dns/serial", Name: "s"}) || d[1].OnUnknown != core.OnUnknownWait || d[1].When == nil {
		t.Fatalf("%+v", d[1])
	}
}

// 不変条件 25：式の形の誤り・知らない参照・when の無い on_unknown・when が作る
// 循環は読み込みで断る（E-M3・E-J7）。
func TestLoadRejectsConditions(t *testing.T) {
	cases := []struct {
		name  string
		yaml  string
		wants []string
	}{
		{"式の形", "resources:\n  - {type: dns/record, id: a, depends_on: [{when: 'present(dns/record/b) and'}]}\n",
			[]string{"a.yaml:2", "when", "項が要る"}},
		{"it の外", "resources:\n  - {type: dns/record, id: a, depends_on: [{when: 'it.value == 1'}]}\n",
			[]string{"it は all・any・count の中"}},
		{"知らない種類", "resources:\n  - {type: dns/record, id: a, depends_on: [{when: 'observe(dns/zone/x).v == 1'}]}\n",
			[]string{"dns/record/a", `種類 "zone" が無い`}},
		{"知らない項目", "resources:\n  - {type: dns/record, id: a, depends_on: [{when: 'observe(dns/record/x).vale == 1'}]}\n",
			[]string{`項目 "vale" が無い`}},
		{"量化の知らない項目", "resources:\n  - {type: dns/record, id: a, depends_on: [{when: 'all(dns/record, it.vale == 1)'}]}\n",
			[]string{`項目 "vale" が無い`}},
		{"desired の知らない項目", "resources:\n  - {type: dns/record, id: a, depends_on: [{when: 'desired.vale == 1'}]}\n",
			[]string{`desired に項目 "vale" が無い`}},
		{"登録の無い確かめ", "resources:\n  - {type: dns/record, id: a, depends_on: [{when: 'check(bgp-up)'}]}\n",
			[]string{"check(bgp-up)", "登録した確かめに無い"}},
		{"on_unknown だけ", "resources:\n  - {type: dns/record, id: a, depends_on: [{id: dns/record/b, on_unknown: block}]}\n",
			[]string{"on_unknown は when と一緒に書く"}},
		{"on_unknown の値", "resources:\n  - {type: dns/record, id: a, depends_on: [{when: 'present(dns/record/b)', on_unknown: skip}]}\n",
			[]string{"wait・block・ignore"}},
		{"id も when も無い", "resources:\n  - {type: dns/record, id: a, depends_on: [{}]}\n",
			[]string{"id か when を書く"}},
		{"when が作る循環", `resources:
  - {type: dns/record, id: a, depends_on: [{when: 'present(dns/record/b)'}]}
  - {type: dns/record, id: b, depends_on: [{id: dns/record/a}]}
`, []string{"循環", "dns/record/a → dns/record/b → dns/record/a"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := run(t, baseConfig, map[string]string{"a.yaml": c.yaml})
			expectReject(t, err, c.wants...)
		})
	}
}

// 量化が自分を選んでも循環にしない（集合の条件は辺にしない）。
func TestLoadQuantifierIsNotEdge(t *testing.T) {
	_, err := run(t, baseConfig, map[string]string{"a.yaml": `
resources:
  - type: dns/record
    id: a
    labels: {role: node}
    depends_on: [{when: 'count(dns/record[role=node], it.value == "x") >= 0'}]
`})
	if err != nil {
		t.Fatal(err)
	}
}
