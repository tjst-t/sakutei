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
	"reflect"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
)

// Policy の select・mode・budget を intent から読む（§3.6、E-M2・E-I3・E-J1）。
func TestLoadPolicies(t *testing.T) {
	l, err := run(t, baseConfig, map[string]string{"p.yaml": `
policies:
  - name: default
    select: {all: true}
    budget:
      per_cycle: 20
      destructive_per_cycle: 3
      destructive_per_window: 10
      window: 24h
  - name: legacy
    select: {type: dns/record, labels: {site: legacy}}
    mode: report
`})
	if err != nil {
		t.Fatal(err)
	}
	want := []core.Policy{
		{Name: "default", Select: core.Selector{All: true}, Origin: core.Origin{File: "p.yaml", Line: 3},
			Budget: &core.Budget{PerCycle: 20, DestructivePerCycle: 3, DestructivePerWindow: 10, Window: 24 * time.Hour}},
		{Name: "legacy", Select: core.Selector{Types: []string{"dns/record"}, Labels: map[string]string{"site": "legacy"}},
			Mode: core.ModeReport, Origin: core.Origin{File: "p.yaml", Line: 10}},
	}
	if !reflect.DeepEqual(l.Policies, want) {
		t.Fatalf("got  %+v\nwant %+v", l.Policies, want)
	}
	// 書かない上限は Unlimited。
	l, err = run(t, baseConfig, map[string]string{"p.yaml": "policies:\n  - {name: d, select: {types: [dns/record, dns/serial]}, budget: {destructive_per_cycle: 0}}\n"})
	if err != nil {
		t.Fatal(err)
	}
	if b := l.Policies[0].Budget; *b != (core.Budget{PerCycle: core.Unlimited, DestructivePerCycle: 0, DestructivePerWindow: core.Unlimited}) {
		t.Fatalf("%+v", b)
	}
}

func TestLoadRejectsPolicies(t *testing.T) {
	cases := []struct {
		name, body string
		wants      []string
	}{
		{"name が無い", "policies:\n  - {select: {all: true}}\n", []string{"p.yaml:2", "name を書く"}},
		{"同じ名前", "policies:\n  - {name: a, select: {all: true}}\n  - {name: a, select: {all: true}}\n", []string{"p.yaml:3", "同じ名前の Policy", "p.yaml:2"}},
		{"select が無い", "policies:\n  - {name: a}\n", []string{"select を書く"}},
		{"空の select", "policies:\n  - {name: a, select: {}}\n", []string{"当たり先を書く"}},
		{"all: false", "policies:\n  - {name: a, select: {all: false}}\n", []string{"all は true だけ"}},
		{"all と labels", "policies:\n  - {name: a, select: {all: true, labels: {a: b}}}\n", []string{"一緒に書けない"}},
		{"知らない種類", "policies:\n  - {name: a, select: {type: dns/recrd}}\n", []string{`select の type dns/recrd`, `種類 "recrd" が無い`}},
		{"知らない mode", "policies:\n  - {name: a, select: {all: true}, mode: dry}\n", []string{"mode は apply か report"}},
		{"負の予算", "policies:\n  - {name: a, select: {all: true}, budget: {per_cycle: -1}}\n", []string{"0 以上の整数"}},
		{"窓だけ", "policies:\n  - {name: a, select: {all: true}, budget: {window: 1h}}\n", []string{"window は destructive_per_window と一緒に"}},
		{"窓が無い", "policies:\n  - {name: a, select: {all: true}, budget: {destructive_per_window: 3}}\n", []string{"destructive_per_window には window を書く"}},
		{"空の予算", "policies:\n  - {name: a, select: {all: true}, budget: {}}\n", []string{"上限を 1 つ以上"}},
		{"段 2 に無い項目", "policies:\n  - {name: a, select: {all: true}, windows: [x]}\n", []string{`知らない項目 "windows"`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := run(t, baseConfig, map[string]string{"p.yaml": tc.body})
			expectReject(t, err, tc.wants...)
		})
	}
}
