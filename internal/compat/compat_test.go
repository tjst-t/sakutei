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

package compat

import (
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

func table(t *testing.T, rows ...map[string]any) Set {
	t.Helper()
	allow := make([]any, 0, len(rows))
	for _, r := range rows {
		allow = append(allow, r)
	}
	s, err := Parse([]any{map[string]any{
		"name": "bmc-bios",
		"components": map[string]any{
			"bmc":  map[string]any{"id": "{node}/firmware/BMC"},
			"bios": map[string]any{"id": "{node}/firmware/BIOS"},
		},
		"allow": allow,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func res(local, version string, deps ...string) diff.DesiredResource {
	return diff.DesiredResource{ID: core.NewID("redfish-firmware", core.NewScope(nil), local),
		Fields: map[string]string{"version": version}, DependsOn: deps}
}

func cur(m map[string]string) Current {
	return func(local string) (map[string]string, bool) {
		v, ok := m[local]
		if !ok {
			return nil, false
		}
		return map[string]string{"version": v}, true
	}
}

var rows = []map[string]any{
	{"bmc": "1", "bios": "1"},
	{"bmc": "2", "bios": "1"},
	{"bmc": "2", "bios": "2"},
}

func TestCheckDesired(t *testing.T) {
	s := table(t, rows...)
	ok := []diff.DesiredResource{res("n1/firmware/BMC", "2"), res("n1/firmware/BIOS", "2"), res("n2/firmware/BMC", "1"), res("other", "9")}
	if err := s.CheckDesired(ok); err != nil {
		t.Fatalf("allowed: %v", err)
	}
	bad := []diff.DesiredResource{res("n1/firmware/BMC", "1"), res("n1/firmware/BIOS", "2")}
	err := s.CheckDesired(bad)
	if err == nil || !strings.Contains(err.Error(), "bios=2 (n1/firmware/BIOS)") || !strings.Contains(err.Error(), "node=n1") {
		t.Fatalf("want refusal naming component and version, got %v", err)
	}
}

func TestCheckPath(t *testing.T) {
	s := table(t, rows...)
	now := cur(map[string]string{"n1/firmware/BMC": "1", "n1/firmware/BIOS": "1"})
	// 順序の無い両方の更新: BIOS が先になりうる（bmc=1, bios=2 は表に無い）。
	f, err := s.CheckPath([]diff.DesiredResource{res("n1/firmware/BMC", "2"), res("n1/firmware/BIOS", "2")}, now)
	if err != nil || len(f) != 1 || f[0].Unverified || len(f[0].Hold) != 2 {
		t.Fatalf("want one violation holding both, got %+v %v", f, err)
	}
	if !strings.Contains(f[0].Hint, "n1/firmware/BIOS depends_on n1/firmware/BMC") {
		t.Fatalf("want the depends_on hint, got %q", f[0].Hint)
	}
	// depends_on で BMC を先にすれば通る。
	f, _ = s.CheckPath([]diff.DesiredResource{res("n1/firmware/BMC", "2"), res("n1/firmware/BIOS", "2", "n1/firmware/BMC")}, now)
	if len(f) != 0 {
		t.Fatalf("ordered: want no finding, got %+v", f)
	}
	// 逆の順序は止まり、depends_on を見直す案内。
	f, _ = s.CheckPath([]diff.DesiredResource{res("n1/firmware/BMC", "2", "n1/firmware/BIOS"), res("n1/firmware/BIOS", "2")}, now)
	if len(f) != 1 || !strings.Contains(f[0].Hint, "drop the depends_on") {
		t.Fatalf("reverse order: got %+v", f)
	}
	// 観測できない部品があれば確かめられない。
	f, _ = s.CheckPath([]diff.DesiredResource{res("n1/firmware/BMC", "2"), res("n1/firmware/BIOS", "1")},
		cur(map[string]string{"n1/firmware/BMC": "1"}))
	if len(f) != 1 || !f[0].Unverified || len(f[0].Hold) != 1 {
		t.Fatalf("unknown: got %+v", f)
	}
	// どの順でも合わない。
	s2 := table(t, map[string]any{"bmc": "1", "bios": "1"}, map[string]any{"bmc": "2", "bios": "2"})
	f, _ = s2.CheckPath([]diff.DesiredResource{res("n1/firmware/BMC", "2"), res("n1/firmware/BIOS", "2")}, now)
	if len(f) != 1 || !strings.Contains(f[0].Hint, "no one-at-a-time order") {
		t.Fatalf("no order: got %+v", f)
	}
}

func TestParseRefusals(t *testing.T) {
	for name, v := range map[string]any{
		"no allow": []any{map[string]any{"name": "x", "components": map[string]any{"a": map[string]any{"id": "a"}}}},
		"missing component in row": []any{map[string]any{"name": "x", "components": map[string]any{"a": map[string]any{"id": "a"}, "b": map[string]any{"id": "b"}},
			"allow": []any{map[string]any{"a": "1"}}}},
		"unknown component": []any{map[string]any{"name": "x", "components": map[string]any{"a": map[string]any{"id": "a"}},
			"allow": []any{map[string]any{"a": "1", "z": "1"}}}},
		"number version": []any{map[string]any{"name": "x", "components": map[string]any{"a": map[string]any{"id": "a"}},
			"allow": []any{map[string]any{"a": 1.10}}}},
		"different vars": []any{map[string]any{"name": "x", "components": map[string]any{"a": map[string]any{"id": "{n}/a"}, "b": map[string]any{"id": "b"}},
			"allow": []any{map[string]any{"a": "1", "b": "1"}}}},
	} {
		if _, err := Parse(v); err == nil {
			t.Errorf("%s: want a refusal", name)
		}
	}
}
