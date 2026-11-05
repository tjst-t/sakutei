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

package plan_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/sdk/fake"
)

var schema = core.Schema{Version: "fake/1", Kinds: map[string]core.KindSchema{
	"record": {Fields: map[string]core.FieldSpec{
		"value": {Type: core.String},
		"ttl":   {Type: core.Int},
		"tags":  {Type: core.Set},
		"rules": {Type: core.List},
		"wait":  {Type: core.Duration},
		"zone":  {Type: core.String, Identity: true},
	}},
	"serial": {ObserveOnly: true, Fields: map[string]core.FieldSpec{"version": {Type: core.String}}},
}}

func id(kind, name string) core.ID { return core.ID{Type: "dns/" + kind, Name: name} }

func res(kind, name string, f core.Fields) core.Resource {
	r := core.Resource{ID: id(kind, name), Fields: core.Fields{}}
	for k, v := range f {
		n, err := core.Normalize(schema.Kinds[kind].Fields[k].Type, v)
		if err != nil {
			panic(err)
		}
		r.Fields[k] = n
	}
	return r
}

func present(f core.Fields) core.Observation {
	return core.Observation{Availability: core.Present, Fields: f}
}

func TestCompute(t *testing.T) {
	p := fake.New(schema)
	p.Set(id("record", "same"), present(core.Fields{"value": "a", "ttl": 300.0, "tags": []any{"y", "x"}, "extra": "機器が付けた値"}))
	p.Set(id("record", "changed"), present(core.Fields{"value": "a", "rules": []any{"r2", "r1"}}))
	p.Set(id("record", "moved"), present(core.Fields{"zone": "a"}))
	p.Set(id("record", "missing-field"), present(core.Fields{}))
	p.Set(id("record", "gone"), present(core.Fields{"value": "a"}))
	p.Set(id("record", "busy"), core.Observation{Availability: core.Converging, Note: "再起動待ち"})
	p.Set(id("record", "dark"), core.Observation{Availability: core.Unknown, Note: "届かない"})
	p.Omit(id("record", "silent"))
	p.Set(id("record", "badtype"), present(core.Fields{"ttl": "三百"}))
	p.Set(id("record", "dur"), present(core.Fields{"wait": "60s"}))
	p.Set(id("serial", "s"), present(core.Fields{"version": "1"}))

	gone := res("record", "gone", nil)
	gone.Ensure = core.EnsureAbsent
	alreadyGone := res("record", "already-gone", nil)
	alreadyGone.Ensure = core.EnsureAbsent
	busyAbsent := res("record", "busy", nil)
	busyAbsent.Ensure = core.EnsureAbsent

	in := []core.Resource{
		res("record", "same", core.Fields{"value": "a", "ttl": 300, "tags": []any{"x", "y"}}),
		res("record", "changed", core.Fields{"value": "b", "rules": []any{"r1", "r2"}}),
		res("record", "moved", core.Fields{"zone": "b"}),
		res("record", "missing-field", core.Fields{"value": "a"}),
		res("record", "new", core.Fields{"value": "a"}),
		gone, alreadyGone, busyAbsent,
		res("record", "dark", core.Fields{"value": "a"}),
		res("record", "silent", core.Fields{"value": "a"}),
		res("record", "badtype", core.Fields{"ttl": 300}),
		res("record", "dur", core.Fields{"wait": "1m"}),
		res("serial", "s", core.Fields{"version": "2"}),
		res("serial", "t", core.Fields{"version": "2"}),
	}
	r := plan.Compute(context.Background(), in, map[string]core.Observer{"dns": p})
	got := map[string]plan.Change{}
	for _, c := range r.Changes {
		got[strings.TrimPrefix(c.Resource, "dns/")] = c
	}
	wantOps := map[string]plan.Op{
		"record/same":          plan.None,       // 宣言に無い field（extra）は比べない（E-D7）。数は int と float を同じに、Set は順不同（E-B12）
		"record/changed":       plan.Update,     // List は順序ごと比べる
		"record/moved":         plan.Update,     // identity の変更は作り直し（E-A10）
		"record/missing-field": plan.Update,     // 観測に field が無い
		"record/new":           plan.Create,     // 置いていない ID は Absent
		"record/gone":          plan.Delete,     // ensure: absent だが Present（E-D3）
		"record/already-gone":  plan.None,       // ensure: absent で Absent
		"record/busy":          plan.Converging, // Converging は消すも重ねもしない（E-A4）
		"record/dark":          plan.Unknown,    // 届かない（E-K1）
		"record/silent":        plan.Unknown,    // 答えなかった ID は Absent と読まない（不変条件 3）
		"record/badtype":       plan.Unknown,    // 定義に合わない出力（E-B11）
		"record/dur":           plan.None,       // duration は正規形で比べる
		"serial/s":             plan.Drift,      // 観測だけの type は通知するだけ（E-B7）
		"serial/t":             plan.Drift,
	}
	for name, want := range wantOps {
		if got[name].Op != want {
			t.Errorf("%s: op=%s want %s (%+v)", name, got[name].Op, want, got[name])
		}
	}
	if len(r.Changes) != len(wantOps) {
		t.Errorf("changes=%d want %d", len(r.Changes), len(wantOps))
	}
	if !got["record/moved"].Replace || got["record/changed"].Replace {
		t.Errorf("replace: moved=%v changed=%v", got["record/moved"].Replace, got["record/changed"].Replace)
	}
	if f := got["record/missing-field"].Fields; len(f) != 1 || !f[0].Missing || f[0].Observed != nil {
		t.Errorf("missing-field: %+v", f)
	}
	// Unknown のものに field の値を作らない（不変条件 29）。
	if c := got["record/dark"]; c.Fields != nil || c.Note != "届かない" {
		t.Errorf("dark: %+v", c)
	}
	if r.Summary[plan.Update] != 3 || r.Summary[plan.Unknown] != 3 {
		t.Errorf("summary: %v", r.Summary)
	}
	// provider ごとに 1 回だけ Observe を呼び（E-D8）、書き込みの API は呼ばない（不変条件 30）。
	if c := p.Calls(); c != (fake.Calls{Observe: 1}) {
		t.Errorf("calls: %+v", c)
	}
}

// Observe が失敗したらその provider の全部を Unknown にする（E-K1・N-K1）。
func TestComputeObserveError(t *testing.T) {
	p := fake.New(schema)
	p.SetObserveError(errors.New("connection refused"))
	r := plan.Compute(context.Background(), []core.Resource{res("record", "a", core.Fields{"value": "x"})}, map[string]core.Observer{"dns": p})
	if c := r.Changes[0]; c.Op != plan.Unknown || !strings.Contains(c.Note, "connection refused") {
		t.Fatalf("%+v", c)
	}
}

// 観測を cycle をまたいで持ち越さない：plan を 2 回すれば 2 回観測し、その間の現実の
// 変化を拾う（不変条件 1）。
func TestComputeObservesEveryTime(t *testing.T) {
	p := fake.New(schema)
	in := []core.Resource{res("record", "a", core.Fields{"value": "x"})}
	obs := map[string]core.Observer{"dns": p}
	if c := plan.Compute(context.Background(), in, obs).Changes[0]; c.Op != plan.Create {
		t.Fatalf("1 回目: %+v", c)
	}
	p.Set(id("record", "a"), present(core.Fields{"value": "x"}))
	if c := plan.Compute(context.Background(), in, obs).Changes[0]; c.Op != plan.None {
		t.Fatalf("2 回目: %+v", c)
	}
	if n := p.Calls().Observe; n != 2 {
		t.Fatalf("observe=%d", n)
	}
}

// 文と JSON は同じ構造から作る（不変条件 30）。
func TestWrite(t *testing.T) {
	p := fake.New(schema)
	p.Set(id("record", "b"), present(core.Fields{"value": "old", "zone": "a"}))
	a := res("record", "a", core.Fields{"value": "x"})
	a.Reason = "CHG-1"
	a.Origin = core.Origin{File: "a.yaml", Line: 3}
	r := plan.Compute(context.Background(), []core.Resource{a, res("record", "b", core.Fields{"value": "new", "zone": "b"})}, map[string]core.Observer{"dns": p})

	var text bytes.Buffer
	if err := r.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	want := `plan: 作る 1・変える 1
+ dns/record/a  [a.yaml:3]
    理由: CHG-1
    value: "x"
~ dns/record/b（作り直し）
    value: "old" → "new"
    zone（identity）: "a" → "b"
`
	if text.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", text.String(), want)
	}

	var buf bytes.Buffer
	if err := r.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var back struct {
		Changes []plan.Change  `json:"changes"`
		Summary map[string]int `json:"summary"`
	}
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Changes) != 2 || back.Changes[1].Op != plan.Update || !back.Changes[1].Replace ||
		!reflect.DeepEqual(back.Summary, map[string]int{"create": 1, "update": 1}) {
		t.Fatalf("json: %s", buf.String())
	}
}

// E-E2：Digest の field は宣言を sha256 にして比べ、平文を出さない。
func TestCompareDigest(t *testing.T) {
	ks := core.KindSchema{Fields: map[string]core.FieldSpec{"content": {Type: core.String, Digest: true}}}
	r := core.Resource{ID: core.ID{Type: "h/file", Name: "a"}, Fields: core.Fields{"content": "secret=1\n"}}
	same := plan.Compare(r, ks, core.Observation{Availability: core.Present, Fields: core.Fields{"content": core.Digest("secret=1\n")}})
	if same.Op != plan.None {
		t.Fatalf("%+v", same)
	}
	diff := plan.Compare(r, ks, core.Observation{Availability: core.Present, Fields: core.Fields{"content": core.Digest("x")}})
	if diff.Op != plan.Update || diff.Fields[0].Desired != core.Digest("secret=1\n") {
		t.Fatalf("%+v", diff)
	}
	created := plan.Compare(r, ks, core.Observation{Availability: core.Absent})
	if created.Op != plan.Create || created.Fields[0].Desired != core.Digest("secret=1\n") {
		t.Fatalf("%+v", created)
	}
}
