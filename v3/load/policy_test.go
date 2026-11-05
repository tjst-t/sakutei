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
      max_ineffective_applies: 2
  - name: legacy
    select: {type: dns/record, labels: {site: legacy}}
    mode: report
`})
	if err != nil {
		t.Fatal(err)
	}
	want := []core.Policy{
		{Name: "default", Select: core.Selector{All: true}, Origin: core.Origin{File: "p.yaml", Line: 3},
			Budget: &core.Budget{PerCycle: 20, DestructivePerCycle: 3, DestructivePerWindow: 10, Window: 24 * time.Hour,
				MaxIneffectiveApplies: 2}},
		{Name: "legacy", Select: core.Selector{Types: []string{"dns/record"}, Labels: map[string]string{"site": "legacy"}},
			Mode: core.ModeReport, Origin: core.Origin{File: "p.yaml", Line: 11}},
	}
	if !reflect.DeepEqual(l.Policies, want) {
		t.Fatalf("got  %+v\nwant %+v", l.Policies, want)
	}
	// 書かない上限は Unlimited。
	l, err = run(t, baseConfig, map[string]string{"p.yaml": "policies:\n  - {name: d, select: {types: [dns/record, dns/serial]}, budget: {destructive_per_cycle: 0}}\n"})
	if err != nil {
		t.Fatal(err)
	}
	if b := l.Policies[0].Budget; *b != (core.Budget{PerCycle: core.Unlimited, DestructivePerCycle: 0, DestructivePerWindow: core.Unlimited,
		MaxIneffectiveApplies: core.Unlimited}) {
		t.Fatalf("%+v", b)
	}
}

// window と allow を intent から読む（§3.6、E-J2・E-J3）。
func TestLoadWindowsAndAllow(t *testing.T) {
	l, err := run(t, baseConfig, map[string]string{"p.yaml": `
policies:
  - name: maint
    select: {all: true}
    windows: ["Sat,Sun 02:00-04:00 Asia/Tokyo", "2026-11-05 22:00 .. 2026-11-06 06:00 Asia/Tokyo"]
  - name: decom
    select: {labels: {rack: "3"}}
    allow: {kind: reimage, max_count: 48}
    windows: ["2026-11-05T22:00:00+09:00 .. 2026-11-06T06:00:00+09:00"]
    approval: {quorum: {n: 2, of: [alice, bob]}, separation: true, cooling_off: 1h}
    applicant: carol
    reason: ラック 3 の入れ直し
  - name: solo
    select: {labels: {pool: web}}
    allow: {kind: update, max_count: 3}
    approval: {cooling_off: 2h}
`})
	if err != nil {
		t.Fatal(err)
	}
	m, d, s := l.Policies[0], l.Policies[1], l.Policies[2]
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	if len(m.Windows) != 2 || !m.Windows[0].Contains(time.Date(2026, 11, 7, 3, 0, 0, 0, tokyo)) || m.Windows[0].Contains(time.Date(2026, 11, 6, 3, 0, 0, 0, tokyo)) ||
		!m.Windows[1].Contains(time.Date(2026, 11, 5, 13, 0, 0, 0, time.UTC)) {
		t.Fatalf("%+v", m.Windows)
	}
	if *d.Allow != (core.Allow{Kind: core.AllowReimage, MaxCount: 48}) || d.Approval.CoolingOff != time.Hour || d.Reason == "" ||
		!d.Windows[0].Contains(time.Date(2026, 11, 5, 14, 0, 0, 0, time.UTC)) {
		t.Fatalf("%+v %+v", d, d.Approval)
	}
	if s.Approval.N != 0 || s.Approval.CoolingOff != 2*time.Hour {
		t.Fatalf("%+v", s.Approval)
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
		{"空の select", "policies:\n  - {name: a, select: {}}\n", []string{"対象を書く"}},
		{"all: false", "policies:\n  - {name: a, select: {all: false}}\n", []string{"all は true だけ"}},
		{"all と labels", "policies:\n  - {name: a, select: {all: true, labels: {a: b}}}\n", []string{"一緒に書けない"}},
		{"知らない type", "policies:\n  - {name: a, select: {type: dns/recrd}}\n", []string{`select の type dns/recrd`, `type "recrd" が無い`}},
		{"知らない mode", "policies:\n  - {name: a, select: {all: true}, mode: dry}\n", []string{"mode は apply か report"}},
		{"負の budget", "policies:\n  - {name: a, select: {all: true}, budget: {per_cycle: -1}}\n", []string{"0 以上の整数"}},
		{"window だけ", "policies:\n  - {name: a, select: {all: true}, budget: {window: 1h}}\n", []string{"window は destructive_per_window と一緒に"}},
		{"window が無い", "policies:\n  - {name: a, select: {all: true}, budget: {destructive_per_window: 3}}\n", []string{"destructive_per_window には window を書く"}},
		{"空の budget", "policies:\n  - {name: a, select: {all: true}, budget: {}}\n", []string{"上限を 1 つ以上"}},
		{"Stage 9 に無い field", "policies:\n  - {name: a, select: {all: true}, observe_every: 2}\n", []string{`知らない field "observe_every"`}},
		{"読めない window", "policies:\n  - {name: a, select: {all: true}, windows: [\"Satu 02:00-04:00\"]}\n", []string{`曜日 "Satu"`}},
		{"空の window", "policies:\n  - {name: a, select: {all: true}, windows: [\"02:00-02:00\"]}\n", []string{"空"}},
		{"知らない tz", "policies:\n  - {name: a, select: {all: true}, windows: [\"02:00-04:00 Asia/Tokio\"]}\n", []string{`tz "Asia/Tokio"`}},
		{"allow の kind", "policies:\n  - {name: a, select: {labels: {a: b}}, allow: {kind: purge, max_count: 1}}\n", []string{"kind は delete・update"}},
		{"allow の max_count", "policies:\n  - {name: a, select: {labels: {a: b}}, allow: {kind: delete, max_count: 0}}\n", []string{"1 以上の整数"}},
		{"allow と mode", "policies:\n  - {name: a, select: {labels: {a: b}}, mode: report, allow: {kind: delete, max_count: 1}}\n", []string{"allow の Policy に mode は書けない"}},
		{"allow と all", "policies:\n  - {name: a, select: {all: true}, allow: {kind: delete, max_count: 1}}\n", []string{"select に all は書けない"}},
		{"allow でない cooling_off", "policies:\n  - {name: a, select: {all: true}, approval: {quorum: {n: 1, of: [x]}, cooling_off: 1h}}\n", []string{"cooling_off は allow"}},
		{"quorum の無い承認", "policies:\n  - {name: a, select: {all: true}, approval: {timeout: 1h}}\n", []string{"quorum"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := run(t, baseConfig, map[string]string{"p.yaml": tc.body})
			expectReject(t, err, tc.wants...)
		})
	}
}
