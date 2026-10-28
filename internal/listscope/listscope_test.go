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

package listscope

import (
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

func id(prov string, labels map[string]string, local string) core.ID {
	return core.NewID(prov, core.NewScope(labels), local)
}

var mgmt = map[string]string{"node": "mgmt"}

func TestAlign_RootTakesTheDeclaredID(t *testing.T) {
	declared := []core.ID{id("p", mgmt, "a")}
	res := Align([]core.ID{id("p", nil, "a"), id("p", nil, "x")}, declared)
	if len(res.Mismatches) != 0 {
		t.Fatalf("mismatches = %v", res.Mismatches)
	}
	if !res.IDs[0].Equal(declared[0]) {
		t.Errorf("declared a: got %s, want %s", res.IDs[0], declared[0])
	}
	if !res.IDs[1].Equal(id("p", nil, "x")) {
		t.Errorf("undeclared x must stay as listed: got %s", res.IDs[1])
	}
}

func TestAlign_SameScopeAndOtherProviderUntouched(t *testing.T) {
	declared := []core.ID{id("p", mgmt, "a"), id("q", mgmt, "b")}
	listed := []core.ID{id("p", mgmt, "a"), id("p", nil, "b")}
	res := Align(listed, declared)
	if len(res.Mismatches) != 0 {
		t.Fatalf("mismatches = %v", res.Mismatches)
	}
	if !res.IDs[0].Equal(listed[0]) || !res.IDs[1].Equal(listed[1]) {
		t.Errorf("got %v, want %v (q's b is not p's b)", res.IDs, listed)
	}
}

func TestAlign_ProviderScopeContradictsDeclaration(t *testing.T) {
	declared := []core.ID{id("p", mgmt, "a")}
	res := Align([]core.ID{id("p", map[string]string{"node": "edge"}, "a")}, declared)
	if len(res.Mismatches) != 1 {
		t.Fatalf("mismatches = %v, want 1", res.Mismatches)
	}
	if m := res.Mismatch(); !strings.Contains(m, "provider は node=edge") || !strings.Contains(m, "宣言は node=mgmt") {
		t.Errorf("mismatch = %q", m)
	}
}

func TestAlign_RootWithTwoDeclaredScopesIsAMismatch(t *testing.T) {
	declared := []core.ID{id("p", mgmt, "a"), id("p", map[string]string{"node": "edge"}, "a")}
	res := Align([]core.ID{id("p", nil, "a")}, declared)
	if len(res.Mismatches) != 1 {
		t.Fatalf("mismatches = %v, want 1", res.Mismatches)
	}
	if m := res.Mismatch(); !strings.Contains(m, "宣言は node=edge / node=mgmt") {
		t.Errorf("mismatch = %q", m)
	}
}

func TestAlign_DuplicateDeclarationIsNotAmbiguous(t *testing.T) {
	declared := []core.ID{id("p", mgmt, "a"), id("p", mgmt, "a")}
	res := Align([]core.ID{id("p", nil, "a")}, declared)
	if len(res.Mismatches) != 0 || !res.IDs[0].Equal(declared[0]) {
		t.Fatalf("got %v / %v", res.IDs, res.Mismatches)
	}
}

func TestInferScope(t *testing.T) {
	edge := map[string]string{"node": "edge"}
	cases := []struct {
		name     string
		declared []core.ID
		want     map[string]string
		ok       bool
	}{
		{"one scope", []core.ID{id("p", mgmt, "a"), id("p", mgmt, "b"), id("q", edge, "c")}, mgmt, true},
		{"no declaration", []core.ID{id("q", mgmt, "c")}, nil, false},
		{"root", []core.ID{id("p", nil, "a")}, nil, false},
		{"two scopes", []core.ID{id("p", mgmt, "a"), id("p", edge, "b")}, nil, false},
		{"root and scope", []core.ID{id("p", mgmt, "a"), id("p", nil, "b")}, nil, false},
	}
	for _, c := range cases {
		got, ok := InferScope("p", c.declared)
		if ok != c.ok {
			t.Errorf("%s: ok = %v, want %v", c.name, ok, c.ok)
			continue
		}
		if ok && !got.Equal(core.NewScope(c.want)) {
			t.Errorf("%s: scope = %s, want %v", c.name, got, c.want)
		}
	}
}
