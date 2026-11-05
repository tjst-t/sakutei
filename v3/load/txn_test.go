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
	"time"

	"github.com/tjst-t/sakutei/v3/core"
)

// apply.transaction を読む（§3.1、E-B3・E-B9・N-B9）。
func TestLoadTransaction(t *testing.T) {
	l, err := run(t, baseConfig, map[string]string{"a.yaml": `
resources:
  - type: dns/record
    id: probe
  - type: dns/record
    id: a
    apply:
      transaction:
        group: edge-wave-1
        timeout: 20s
        confirm:
          - {phase: before, when: 'observe(dns/record/probe).value == "up"'}
          - {phase: after, when: 'present(dns/record/probe)'}
  - type: dns/record
    id: b
    apply: {transaction: {}}
`})
	if err != nil {
		t.Fatal(err)
	}
	a := l.Resources[1].Txn()
	if a == nil || a.Group != "edge-wave-1" || a.Timeout != 20*time.Second || len(a.Confirm) != 2 ||
		a.Confirm[0].Phase != core.ConfirmBefore || a.Confirm[1].Phase != core.ConfirmAfter {
		t.Fatalf("%+v", a)
	}
	if b := l.Resources[2].Txn(); b == nil || b.Timeout != core.DefaultTxnTimeout || l.Resources[2].TxnKey() != "resource dns/record/b" {
		t.Fatalf("%+v", b)
	}
}

// 不変条件 25：confirm の式の誤り・知らない phase・短い timeout・group の中の依存は
// 読み込み時にエラーにする。
func TestLoadRejectsTransaction(t *testing.T) {
	cases := []struct {
		name  string
		yaml  string
		wants []string
	}{
		{"phase", "resources:\n  - {type: dns/record, id: a, apply: {transaction: {confirm: [{phase: during, when: 'present(dns/record/b)'}]}}}\n",
			[]string{"before か after"}},
		{"phase が無い", "resources:\n  - {type: dns/record, id: a, apply: {transaction: {confirm: [{when: 'present(dns/record/b)'}]}}}\n",
			[]string{"phase を書く"}},
		{"知らない field", "resources:\n  - {type: dns/record, id: a, apply: {transaction: {confirm: [{phase: after, when: 'observe(dns/record/b).vale == 1'}]}}}\n",
			[]string{"confirm（after）", `"vale" が無い`}},
		{"登録の無い check", "resources:\n  - {type: dns/record, id: a, apply: {transaction: {confirm: [{phase: after, when: 'check(bgp-up)'}]}}}\n",
			[]string{"check(bgp-up)"}},
		{"短い timeout", "resources:\n  - {type: dns/record, id: a, apply: {transaction: {timeout: 1s}}}\n",
			[]string{"2s 以上"}},
		{"transaction が無い", "resources:\n  - {type: dns/record, id: a, apply: {}}\n",
			[]string{"transaction を書く"}},
		{"group の中の依存", `resources:
  - {type: dns/record, id: a, apply: {transaction: {group: g}}}
  - {type: dns/record, id: b, depends_on: [{id: dns/record/a}], apply: {transaction: {group: g}}}
`, []string{"dns/record/b", `transaction.group "g"`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := run(t, baseConfig, map[string]string{"a.yaml": c.yaml})
			expectReject(t, err, c.wants...)
		})
	}
}
