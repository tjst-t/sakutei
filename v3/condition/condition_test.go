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

package condition_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v3/condition"
	"github.com/tjst-t/sakutei/v3/core"
)

type env struct {
	obs     map[core.ID]core.Observation
	labels  map[core.ID]map[string]string
	desired core.Fields
	checks  map[string]core.Tri
}

func (e env) Observation(id core.ID) (core.Observation, bool) {
	o, ok := e.obs[id]
	return o, ok
}

func (e env) Select(s core.Selection) []core.ID {
	var out []core.ID
	for id, l := range e.labels {
		if id.Type != s.Type {
			continue
		}
		hit := true
		for k, v := range s.Labels {
			hit = hit && l[k] == v
		}
		if hit {
			out = append(out, id)
		}
	}
	return out
}

func (e env) Desired() core.Fields { return e.desired }

func (e env) Check(name string) core.Tri { return e.checks[name] }

func ref(s string) core.ID {
	id, err := core.ParseRef(s)
	if err != nil {
		panic(err)
	}
	return id
}

func pres(f core.Fields) core.Observation {
	return core.Observation{Availability: core.Present, Fields: f}
}

var (
	T = core.TriTrue
	F = core.TriFalse
	U = core.TriUnknown
)

func world() env {
	e := env{obs: map[core.ID]core.Observation{}, labels: map[core.ID]map[string]string{},
		desired: core.Fields{"firmware": "1.2"}, checks: map[string]core.Tri{"bgp-up": T, "slow": U}}
	e.obs[ref("bmc/system/fw1")] = pres(core.Fields{"power": "On", "firmware": "1.2", "temp": int64(40), "load": 0.5,
		"tags": []any{"a", "b"}, "nested": map[string]any{"x": int64(1)}})
	e.obs[ref("bmc/system/fw2")] = core.Observation{Availability: core.Unknown, Note: "届かない"}
	e.obs[ref("bmc/system/fw3")] = core.Observation{Availability: core.Absent}
	e.obs[ref("bmc/system/fw4")] = core.Observation{Availability: core.Converging}
	e.obs[ref("pki/cert/www")] = pres(core.Fields{"remaining": "500h0m0s"})
	e.obs[ref("metrics/pool/ceph")] = pres(core.Fields{"usage": float64(72)})
	// gpu の台：3 台 On、1 台 Off、1 台 Unknown。
	for i, st := range []string{"On", "On", "On", "Off", "?"} {
		id := core.ID{Type: "bmc/system", Name: "gpu" + string(rune('0'+i))}
		e.labels[id] = map[string]string{"role": "gpu"}
		if st == "?" {
			e.obs[id] = core.Observation{Availability: core.Unknown}
			continue
		}
		e.obs[id] = pres(core.Fields{"power": st, "mtu": int64(9000)})
	}
	return e
}

func TestEval(t *testing.T) {
	cases := []struct {
		src  string
		want core.Tri
	}{
		{`observe(bmc/system/fw1).power == "On"`, T},
		{`observe(bmc/system/fw1).power != "On"`, F},
		{`observe(bmc/system/fw1).temp < 50`, T},
		{`observe(bmc/system/fw1).temp >= 40.0`, T},
		{`observe(bmc/system/fw1).load <= 0.5`, T},
		{`observe(bmc/system/fw1).nested.x == 1`, T},
		{`observe(bmc/system/fw1).tags contains "b"`, T},
		{`"On" in ["Off", "On"]`, T},
		{`observe(bmc/system/fw1).power in ["Off"]`, F},
		{`observe(bmc/system/fw1).firmware == desired.firmware`, T},
		// Unknown・Converging・観測していない対象は不明（E-K1）。
		{`observe(bmc/system/fw2).power == "On"`, U},
		{`observe(bmc/system/fw4).power == "On"`, U},
		{`observe(bmc/system/nowhere).power == "On"`, U},
		// Absent の対象と観測に無い field は「無い」：== は偽、!= は真、大小は偽。
		{`observe(bmc/system/fw3).power == "On"`, F},
		{`observe(bmc/system/fw3).power != "On"`, T},
		{`observe(bmc/system/fw1).missing < 3`, F},
		{`present(bmc/system/fw1)`, T},
		{`present(bmc/system/fw3)`, F},
		{`absent(bmc/system/fw3)`, T},
		{`present(bmc/system/fw2)`, U},
		{`absent(bmc/system/fw4)`, U},
		// 三値の論理。
		{`present(bmc/system/fw3) and present(bmc/system/fw2)`, F},
		{`present(bmc/system/fw1) and present(bmc/system/fw2)`, U},
		{`present(bmc/system/fw1) or present(bmc/system/fw2)`, T},
		{`present(bmc/system/fw3) or present(bmc/system/fw2)`, U},
		{`not present(bmc/system/fw2)`, U},
		{`not (present(bmc/system/fw3) or false)`, T},
		// 長さ（時刻を読まない、残り時間を比べる、N-G4）。
		{`observe(pki/cert/www).remaining < "720h"`, T},
		{`observe(pki/cert/www).remaining == "500h"`, T},
		// 比べられない組は不明。
		{`observe(bmc/system/fw1).power < 3`, U},
		{`observe(bmc/system/fw1).power`, U},
		{`check(bgp-up)`, T},
		{`check(slow) or false`, U},
		// 容量の門（E-C4）。
		{`observe(metrics/pool/ceph).usage < 80`, T},
		// quantifier（N-B16・N-F4・N-B18・N-J3）。
		{`all(bmc/system[role=gpu], it.mtu == 9000)`, U}, // 1 台は不明、偽は無い
		{`all(bmc/system[role=gpu], it.power == "On")`, F},
		{`any(bmc/system[role=gpu], it.power == "On")`, T},
		{`any(bmc/system[role=gpu], it.power == "Halt")`, U},
		{`any(bmc/system[role="none"], it.power == "On")`, F},
		{`all(bmc/system[role="none"], it.power == "On")`, T},
		{`count(bmc/system[role=gpu], it.power == "On") >= 3`, T}, // k=3, u=1
		{`count(bmc/system[role=gpu], it.power == "On") >= 4`, U}, // 3〜4 で変わる
		{`count(bmc/system[role=gpu], it.power == "On") >= 5`, F},
		{`count(bmc/system[role=gpu], it.power == "On") == 1`, F},
		{`2 < count(bmc/system[role=gpu], it.power == "On")`, T},
		{`count(bmc/system[role=gpu], it.power == "Off") <= 2`, T},
	}
	e := world()
	for _, c := range cases {
		x, err := condition.Parse(c.src)
		if err != nil {
			t.Errorf("%s: %v", c.src, err)
			continue
		}
		if got := x.Eval(e); got != c.want {
			t.Errorf("%s = %s, want %s", c.src, got, c.want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		``:                                     "空",
		`observe(bmc/system/fw1)`:              ".<field>",
		`observe(bmc/fw1).x == 1`:              "bmc/fw1",
		`it.power == "On"`:                     "it は",
		`count(bmc/system, it.x == 1)`:         "比較の中",
		`count(a/b, true) == count(a/b, true)`: "どうし",
		`present(a/b/c) and`:                   "項が要る",
		`now() > 3`:                            "知らない語",
		`"abc`:                                 "閉じていない",
		`present(a/b/c) present(a/b/c)`:        "終わりのあと",
		`all(nope, true)`:                      "選び方",
		`check()`:                              "名前",
	}
	for src, want := range cases {
		_, err := condition.Parse(src)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: 誤り %v に %q が無い", src, err, want)
		}
	}
}

func TestRefs(t *testing.T) {
	x := condition.MustParse(`observe(a/b/c).x == 1 and absent(a/b/d) and count(a/e[k=v], it.y == 2) > 0 and check(z) and observe(a/b/c).w == 2`)
	sel := func(s core.Selection) []core.ID {
		if s.Type != "a/e" || s.Labels["k"] != "v" {
			t.Fatalf("選び方 %+v", s)
		}
		return []core.ID{ref("a/e/1"), ref("a/e/2")}
	}
	want := []core.ID{ref("a/b/c"), ref("a/b/d"), ref("a/e/1"), ref("a/e/2")}
	if got := x.Refs(sel); !reflect.DeepEqual(got, want) {
		t.Errorf("Refs = %v, want %v", got, want)
	}
	if got := x.Direct(); !reflect.DeepEqual(got, []core.ID{ref("a/b/c"), ref("a/b/d")}) {
		t.Errorf("Direct = %v", got)
	}
	if got := x.Checks(); !reflect.DeepEqual(got, []string{"z"}) {
		t.Errorf("Checks = %v", got)
	}
}

func TestLint(t *testing.T) {
	kinds := func(typ string) (core.KindSchema, string) {
		if typ == "bmc/system" {
			return core.KindSchema{Fields: map[string]core.FieldSpec{"power": {Type: core.String}}}, ""
		}
		return core.KindSchema{}, "知らない type"
	}
	x := condition.MustParse(`observe(bmc/system/a).volt > 1 and present(x/y/z) and all(bmc/system, it.pwr == "On") and check(nope) and desired.power == "On"`)
	got := strings.Join(condition.Lint(x, kinds, nil, func(string) bool { return false }), "\n")
	for _, want := range []string{`field "volt"`, "present(x/y/z): 知らない type", `field "pwr"`, "check(nope)", "desired は"} {
		if !strings.Contains(got, want) {
			t.Errorf("Lint に %q が無い:\n%s", want, got)
		}
	}
	ok := condition.MustParse(`observe(bmc/system/a).power == "On" and count(bmc/system[r=1], it.power == "On") > 1`)
	if got := condition.Lint(ok, kinds, nil, nil); len(got) != 0 {
		t.Errorf("Lint = %v", got)
	}
}
