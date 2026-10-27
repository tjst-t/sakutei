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

package render

import (
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

func expandTwoParents(t *testing.T, p1Extra, p2Extra string) []ExpandedResource {
	t.Helper()
	reg := &ExpanderRegistry{}
	reg.Register("fake", CompositeKind, FakeCompositeExpander{})
	scope := core.Scope{}
	intent := []diff.DesiredResource{
		{ID: core.NewID("fake", scope, "p1"), Fields: map[string]string{"kind": CompositeKind, "extra": p1Extra}},
		{ID: core.NewID("fake", scope, "p2"), Fields: map[string]string{"kind": CompositeKind, "extra": p2Extra}},
		{ID: core.NewID("fake", scope, "plain"), Fields: map[string]string{"name": "plain"}},
	}
	out, err := ExpandAll(reg, intent)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestUpstreamChangedChildren_OnlyChangedParent — #14: with two composite
// parents, changing one tags only that parent's children D.  [AC-14-1]
func TestUpstreamChangedChildren_OnlyChangedParent(t *testing.T) {
	before := expandTwoParents(t, "v1", "v1")
	after := expandTwoParents(t, "v2", "v1")

	prev := ParentRevisions(before)
	cur := ParentRevisions(after)
	if prev["p2"] != cur["p2"] {
		t.Fatalf("p2 unchanged but its revision moved: %q → %q", prev["p2"], cur["p2"])
	}
	if prev["p1"] == cur["p1"] {
		t.Fatalf("p1 changed but its revision did not move")
	}

	got := UpstreamChangedChildren(prev, cur, BuildParentChildMap(after))
	want := map[string]bool{"p1-child-a": true, "p1-child-b": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for id := range want {
		if !got[id] {
			t.Errorf("%s not tagged; got %v", id, got)
		}
	}
}

// TestUpstreamChangedChildren_NoBaseline — nil prev (cold start / a cause-state
// written before #14) tags nothing; a new parent against a known baseline does.
func TestUpstreamChangedChildren_NoBaseline(t *testing.T) {
	exp := expandTwoParents(t, "v1", "v1")
	cur := ParentRevisions(exp)
	ptc := BuildParentChildMap(exp)

	if got := UpstreamChangedChildren(nil, cur, ptc); len(got) != 0 {
		t.Errorf("nil prev: got %v, want none", got)
	}
	if got := UpstreamChangedChildren(cur, cur, ptc); len(got) != 0 {
		t.Errorf("stable: got %v, want none", got)
	}
	onlyP1 := map[string]string{"p1": cur["p1"]}
	got := UpstreamChangedChildren(onlyP1, cur, ptc)
	if len(got) != 2 || !got["p2-child-a"] || !got["p2-child-b"] {
		t.Errorf("new parent p2: got %v, want p2's children", got)
	}
}

// TestParentRevisions_Deterministic — the same expansion hashes the same.
func TestParentRevisions_Deterministic(t *testing.T) {
	a := ParentRevisions(expandTwoParents(t, "x", "y"))
	b := ParentRevisions(expandTwoParents(t, "x", "y"))
	if len(a) != 2 || a["p1"] != b["p1"] || a["p2"] != b["p2"] {
		t.Errorf("not deterministic: %v vs %v", a, b)
	}
	if _, ok := a["plain"]; ok {
		t.Errorf("plain resource must not have a parent revision")
	}
}
