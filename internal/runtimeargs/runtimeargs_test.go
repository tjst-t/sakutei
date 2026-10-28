// Copyright 2026 tjst-t
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package runtimeargs

import "testing"

func TestResolve_Directions(t *testing.T) {
	decl := &Declaration{File: "rt.yaml", Values: map[string]string{
		"max_changes_per_cycle": "20", "mode": "auto", "on_exceed": "halt-destructive-only",
		"max_ineffective_applies": "3", "max_ineffective_duration": "30m0s", "interval": "30s",
	}}
	cases := []struct {
		key, flag string
		want      string // "tight", "loose", "differ", "same"
	}{
		{"max_changes_per_cycle", "5", "tight"},
		{"max_changes_per_cycle", "50", "loose"},
		{"max_changes_per_cycle", "0", "loose"}, // 0 = unlimited
		{"max_changes_per_cycle", "20", "same"},
		{"mode", "alert-only", "tight"},
		{"on_exceed", "halt-all", "tight"},
		{"max_ineffective_applies", "4", "loose"},
		{"max_ineffective_duration", "10m0s", "tight"},
		{"max_ineffective_duration", "0s", "loose"},
		{"interval", "10s", "differ"},
		{"interval", "1m0s", "differ"},
	}
	for _, c := range cases {
		res := Resolve(decl, map[string]string{c.key: c.flag}, nil)
		got := "same"
		switch {
		case len(res.Tightened) == 1:
			got = "tight"
		case len(res.Conflicts) == 1 && res.Conflicts[0].Loosens:
			got = "loose"
		case len(res.Conflicts) == 1:
			got = "differ"
		}
		if got != c.want {
			t.Errorf("%s flag %s vs intent %s: got %s, want %s", c.key, c.flag, decl.Values[c.key], got, c.want)
		}
		if got != "loose" && got != "differ" && res.Values[c.key] != c.flag {
			t.Errorf("%s: value in effect %q, want the flag's %q", c.key, res.Values[c.key], c.flag)
		}
	}
}

func TestResolve_OnlyOneSide(t *testing.T) {
	decl := &Declaration{Values: map[string]string{"interval": "30s", "stuck_after": "5"}}
	res := Resolve(decl, map[string]string{"settle": "2s"}, []string{"settle", "stuck_after"})
	if res.Values["settle"] != "2s" || res.Values["stuck_after"] != "5" || res.Values["interval"] != "" {
		t.Errorf("values = %v (interval is not used by this verb)", res.Values)
	}
	if len(res.FromIntent) != 1 || res.FromIntent[0] != "stuck_after" {
		t.Errorf("from intent = %v", res.FromIntent)
	}
}

func TestParse(t *testing.T) {
	d, err := Parse(map[string]any{"interval": "1m", "max_changes_per_cycle": 3}, "f.yaml")
	if err != nil || d.Values["interval"] != "1m0s" || d.Values["max_changes_per_cycle"] != "3" {
		t.Fatalf("Parse = %+v, %v", d, err)
	}
	for _, bad := range []map[string]any{
		{"intervl": "1m"}, {"interval": 30}, {"stuck_after": 0}, {"mode": "never"}, {"settle": "-1s"},
	} {
		if _, err := Parse(bad, "f.yaml"); err == nil {
			t.Errorf("Parse(%v) accepted", bad)
		}
	}
}
