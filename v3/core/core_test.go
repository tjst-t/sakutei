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

package core

import (
	"math"
	"testing"
)

func TestNormalizeAndEqual(t *testing.T) {
	cases := []struct {
		t    FieldType
		a, b any
		eq   bool
	}{
		{Int, 300, 300.0, true},
		{Int, int64(1), 2, false},
		{String, "a", "a", true},
		{Duration, "1m", "60s", true},
		{List, []any{"a", "b"}, []any{"b", "a"}, false},
		{Set, []any{"a", "b"}, []any{"b", "a"}, true},
		{Set, []any{"a", "a"}, []any{"a", "b"}, false},
		{Map, map[string]any{"x": 1}, map[string]any{"x": 1.0}, true},
		{Float, 2, 2.0, true},
		{Float, 0.5, 0.25, false},
	}
	for _, c := range cases {
		a, err := Normalize(c.t, c.a)
		if err != nil {
			t.Fatal(err)
		}
		b, err := Normalize(c.t, c.b)
		if err != nil {
			t.Fatal(err)
		}
		if Equal(c.t, a, b) != c.eq {
			t.Errorf("%s %v %v: want eq=%v", c.t, c.a, c.b, c.eq)
		}
	}
	for _, c := range []struct {
		t FieldType
		v any
	}{{String, 1}, {Int, "1"}, {Int, 1.5}, {Bool, "true"}, {Duration, "soon"}, {List, "a"}, {Map, []any{}}, {Float, "1"}, {Float, math.NaN()}} {
		if _, err := Normalize(c.t, c.v); err == nil {
			t.Errorf("%s %v: エラーにしなかった", c.t, c.v)
		}
	}
}

func TestRefs(t *testing.T) {
	id, err := ParseRef("bmc/virtual-media/r3-n07/cd")
	if err != nil || id != (ID{Type: "bmc/virtual-media", Name: "r3-n07/cd"}) || id.Provider() != "bmc" || id.Kind() != "virtual-media" || id.Local() != "virtual-media/r3-n07/cd" {
		t.Fatalf("%+v %v", id, err)
	}
	for _, bad := range []string{"a/b", "a//c", "/b/c"} {
		if _, err := ParseRef(bad); err == nil {
			t.Errorf("%q をエラーにしなかった", bad)
		}
	}
	for _, bad := range []string{"a", "a/b/c", "/b", "a/"} {
		if ParseType(bad) == nil {
			t.Errorf("type %q をエラーにしなかった", bad)
		}
	}
	e := Exclusion{IDPrefix: "vm/1"}
	if !e.Matches(ID{Type: "pve/vm", Name: "100"}) || e.Matches(ID{Type: "pve/vm", Name: "200"}) {
		t.Fatal("exclusion")
	}
	// 観測のゼロ値は Unknown（埋め忘れを Absent と読まない、不変条件 3）。
	if (Observation{}).Availability != Unknown {
		t.Fatal("zero observation is not unknown")
	}
}
