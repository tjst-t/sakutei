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

import (
	"errors"
	"strings"
	"testing"
)

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

// runtime.daemon: / runtime.oneshot:（#193）: 種類の欄にあればそれ、無ければ
// 共通の値。旗と比べる相手は種類に効く値で、案内はその欄を指す。
func TestParse_Kinds(t *testing.T) {
	d, err := Parse(map[string]any{"mode": "auto", "max_changes_per_cycle": 20,
		"daemon":  map[string]any{"mode": "alert-only", "interval": "1m"},
		"oneshot": map[string]any{"max_changes_per_cycle": 5},
	}, "f.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dm, os := d.For(Daemon), d.For(Oneshot)
	if dm.Values["mode"] != "alert-only" || dm.Values["interval"] != "1m0s" || dm.Values["max_changes_per_cycle"] != "20" {
		t.Errorf("daemon = %v", dm.Values)
	}
	if os.Values["mode"] != "auto" || os.Values["max_changes_per_cycle"] != "5" || os.Values["interval"] != "" {
		t.Errorf("oneshot = %v", os.Values)
	}
	if dm.Path("mode") != "runtime.daemon.mode" || dm.Path("max_changes_per_cycle") != "runtime.max_changes_per_cycle" {
		t.Errorf("paths = %s, %s", dm.Path("mode"), dm.Path("max_changes_per_cycle"))
	}
	res := Resolve(os, map[string]string{"max_changes_per_cycle": "6"}, nil)
	if len(res.Conflicts) != 1 || !strings.Contains(res.Conflicts[0].Error(), "change runtime.oneshot.max_changes_per_cycle in f.yaml") {
		t.Errorf("conflicts = %+v", res.Conflicts)
	}
	res = Resolve(dm, map[string]string{"mode": "auto"}, nil)
	if len(res.Conflicts) != 1 || !res.Conflicts[0].Loosens {
		t.Errorf("--mode auto must loosen runtime.daemon.mode alert-only: %+v", res)
	}
	if (*Declaration)(nil).For(Daemon) != nil {
		t.Error("For of no declaration must be nil")
	}
	for _, bad := range []map[string]any{
		{"oneshot": map[string]any{"interval": "1m"}}, {"oneshot": map[string]any{"stuck_after": 3}},
		{"daemon": map[string]any{"intervl": "1m"}}, {"daemon": "auto"}, {"daemon": map[string]any{"daemon": map[string]any{}}},
	} {
		if _, err := Parse(bad, "f.yaml"); err == nil {
			t.Errorf("Parse(%v) accepted", bad)
		}
	}
}

// #213: a role's values win over runtime.daemon:, which win over runtime:.
func TestForRole_Precedence(t *testing.T) {
	d, err := Parse(map[string]any{
		"mode": "alert-only", "max_changes_per_cycle": 20, "settle": "1s",
		"daemon": map[string]any{"interval": "30s", "settle": "2s"},
		"roles": map[string]any{
			"os-install": map[string]any{"mode": "auto", "interval": "5s", "stuck_after": 3},
		},
	}, "rt.yaml")
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.ForRole("os-install")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"mode": "auto", "max_changes_per_cycle": "20", "settle": "2s", "interval": "5s", "stuck_after": "3"}
	for k, v := range want {
		if got.Values[k] != v {
			t.Errorf("%s = %q, want %q", k, got.Values[k], v)
		}
	}
	if got.Role != "os-install" || got.Path("mode") != "runtime.roles.os-install.mode" ||
		got.Path("settle") != "runtime.daemon.settle" || got.Path("max_changes_per_cycle") != "runtime.max_changes_per_cycle" {
		t.Errorf("paths: role %q mode %s settle %s budget %s", got.Role, got.Path("mode"), got.Path("settle"), got.Path("max_changes_per_cycle"))
	}
	if plain, err := d.ForRole(""); err != nil || plain.Values["mode"] != "alert-only" || plain.Role != "" {
		t.Errorf("no role: %+v %v", plain, err)
	}
	_, err = d.ForRole("canary")
	var ue *UnknownRoleError
	if !errors.As(err, &ue) || !strings.Contains(err.Error(), "declared: os-install") {
		t.Errorf("unknown role: %v", err)
	}
	if _, err := (*Declaration)(nil).ForRole("os-install"); err == nil || !strings.Contains(err.Error(), "none is declared") {
		t.Errorf("nil declaration with a role: %v", err)
	}
}
