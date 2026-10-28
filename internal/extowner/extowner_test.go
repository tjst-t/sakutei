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

package extowner

import (
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// excluding is a provider whose config excludes vmid 100 under "lab/" and
// the VM "lab/ctl", and offers vmid to select by.
type excluding struct{ provider.Provider }

func (excluding) OwnerSelectFields() []string { return []string{"vmid"} }
func (excluding) ConfigExclusions() []provider.ConfigExclusion {
	return []provider.ConfigExclusion{
		{Setting: "protected_vmids", IDPrefix: "lab/", Fields: map[string]string{"vmid": "100"}},
		{Setting: "protected_names", ID: "lab/ctl"},
	}
}

func vm(local string, fields map[string]string) diff.DesiredResource {
	return diff.DesiredResource{ID: core.NewID("vm", core.NewScope(nil), local), Fields: fields}
}

func TestCheck(t *testing.T) {
	ps := map[string]provider.Provider{"vm": excluding{}}
	stage0 := Set{
		{Owner: "stage0", Provider: "vm", ID: "lab/ctl"},
		{Owner: "stage0", Provider: "vm", IDPrefix: "lab/", Fields: map[string]string{"vmid": "100"}},
	}

	// Only in the provider config: a warning for each exclusion.
	w, err := Check(nil, []diff.DesiredResource{vm("lab/app", nil)}, ps)
	if err != nil || len(w) != 2 {
		t.Fatalf("config only: warnings %v, err %v; want 2 warnings", w, err)
	}
	// Written in the intent (or covered by a wider entry): no warning.
	if w, err := Check(stage0, nil, ps); err != nil || len(w) != 0 {
		t.Fatalf("in the intent: warnings %v, err %v; want none", w, err)
	}
	if w, _ := Check(Set{{Owner: "x", Provider: "vm", IDPrefix: "lab/"}}, nil, ps); len(w) != 0 {
		t.Fatalf("a wider prefix covers both: warnings %v", w)
	}
	// A narrower entry does not cover: still warned.
	if w, _ := Check(Set{{Owner: "x", Provider: "vm", ID: "lab/other"}}, nil, ps); len(w) != 2 {
		t.Fatalf("an unrelated entry covers nothing: warnings %v", w)
	}

	// The intent manages what the config excludes: refused, even when the
	// intent also writes the exclusion.
	for _, r := range []diff.DesiredResource{vm("lab/ctl", nil), vm("lab/x", map[string]string{"vmid": "100"})} {
		for _, set := range []Set{nil, stage0[:0]} {
			if _, err := Check(set, []diff.DesiredResource{r}, ps); err == nil || !strings.Contains(err.Error(), "provider config excludes") {
				t.Errorf("%s managed: err %v; want the disagreement refused", r.ID, err)
			}
		}
		if _, err := Check(stage0, []diff.DesiredResource{r}, ps); err == nil {
			t.Errorf("%s managed with the exclusion in the intent: want refused", r.ID)
		}
	}

	// A field the provider does not offer.
	if _, err := Check(Set{{Owner: "x", Provider: "vm", Fields: map[string]string{"node": "pve1"}}}, nil, ps); err == nil || !strings.Contains(err.Error(), `does not offer field "node"`) {
		t.Errorf("unoffered field: err %v", err)
	}
}

func TestByState(t *testing.T) {
	set := Set{{Owner: "drs", Provider: "vm", IDPrefix: "lab/", Fields: map[string]string{"vmid": "100"}}}
	id := core.NewID("vm", core.NewScope(nil), "lab/a")
	st := core.State{"vmid": {Kind: core.FieldKindInt, Value: int64(100)}}
	if _, ok := set.ByState(id, st); !ok {
		t.Error("vmid 100 not selected")
	}
	if _, ok := set.ByID(id); ok {
		t.Error("a field selector selected by id alone")
	}
	st["vmid"] = core.TypedValue{Kind: core.FieldKindInt, Value: int64(101)}
	if _, ok := set.ByState(id, st); ok {
		t.Error("vmid 101 selected")
	}
	if _, ok := set.ByState(id, nil); ok {
		t.Error("a missing state selected by a field selector")
	}
}
