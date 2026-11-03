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

package cycle_test

import (
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/record"
)

func window(t *testing.T, s string) core.Window {
	t.Helper()
	w, err := core.ParseWindow(s)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func labeled(name, value string, labels map[string]string) core.Resource {
	r := decl(name, value)
	r.Labels = labels
	return r
}

// E-J3：別枠でない Policy の窓の外は report（知らせるだけ）。窓に入れば当てる。
func TestWindowsReportOutside(t *testing.T) {
	h := newHarness(t) // 2026-11-05（木）22:00 UTC
	h.in.Ranges = nil
	h.in.Resources = []core.Resource{decl("www", "b")}
	h.p.Set(rid("rec", "www"), present(core.Fields{"value": "a"}))
	h.in.Policies = []core.Policy{{Name: "maint", Select: core.Selector{All: true}, Windows: []core.Window{window(t, "Fri 01:00-02:00 Asia/Tokyo")}}}
	r := h.run()
	if c := find(r, rid("rec", "www")); c.Action != plan.ActReport || len(h.p.Writes()) != 0 || cli.ExitCode(r) != cli.ExitRemaining {
		t.Fatalf("窓の外で当てた: %+v", c)
	}
	h.clock.t = time.Date(2026, 11, 5, 16, 30, 0, 0, time.UTC) // 金 01:30 Asia/Tokyo
	if r := h.run(); cli.ExitCode(r) != cli.ExitOK || len(h.p.Writes()) != 1 {
		t.Fatalf("窓の中で当てない: %+v", r.Changes)
	}
}

// E-J2：別枠は窓の中で承認が揃えば、合う変更を max_count まで通常の予算の外で
// 通す（予算の超過でも止めない）。宣言の外は通常の予算で止まる。使い切れば
// 通常の予算に戻る。承認は使っても消えない。
func TestLaneAdmitsDeclaredWork(t *testing.T) {
	h := newHarness(t)
	h.in.Ranges = nil
	zone, fw := map[string]string{"zone": "example"}, map[string]string{"fw": "edge"}
	for _, n := range []string{"a1", "a2", "a3"} {
		h.in.Resources = append(h.in.Resources, labeled(n, "new", zone))
	}
	for _, n := range []string{"f1", "f2"} {
		h.in.Resources = append(h.in.Resources, labeled(n, "new", fw))
	}
	drift := func() {
		for _, r := range h.in.Resources {
			h.p.Set(r.ID, present(core.Fields{"value": "old"}))
		}
	}
	drift()
	lane := core.Policy{Name: "renumber", Select: core.Selector{Labels: zone}, Allow: &core.Allow{Kind: core.AllowUpdate, MaxCount: 3},
		Windows:   []core.Window{window(t, "2026-11-05 21:00 .. 2026-11-05 23:00")},
		Approval:  &core.Approval{N: 1, Of: []string{"alice"}},
		Applicant: "carol"}
	h.in.Policies = []core.Policy{
		{Name: "default", Select: core.Selector{All: true}, Budget: &core.Budget{PerCycle: 1, DestructivePerCycle: core.Unlimited, DestructivePerWindow: core.Unlimited}},
		lane,
	}
	// 承認が無い間は開かない（通常の予算で 5 > 1 で止まる）。依頼を書く。
	r := h.run()
	if len(h.p.Writes()) != 0 || len(r.Lanes) != 1 || r.Lanes[0].Open || r.Lanes[0].Request == "" {
		t.Fatalf("承認の無い別枠が開いた: %+v %+v", r.Lanes, h.p.Writes())
	}
	req := r.Lanes[0].Request
	if a, ok, _ := record.ReadApproval(h.rec.Dir(), req); !ok || a.Request.Kind != "lane" || a.Request.Applicant != "carol" {
		t.Fatalf("依頼: %+v %v", a, ok)
	}
	for _, by := range []string{"carol", "alice"} { // 申請者は数えない
		if err := record.WriteDecision(h.rec.Dir(), req, record.Decision{By: by, Decision: record.Approve, At: h.clock.t}); err != nil {
			t.Fatal(err)
		}
	}
	r = h.run()
	for _, n := range []string{"a1", "a2", "a3"} {
		if c := find(r, rid("rec", n)); c.Lane != "renumber" || c.Outcome != plan.OutcomeOK {
			t.Fatalf("別枠で通らない: %+v", c)
		}
	}
	for _, n := range []string{"f1", "f2"} {
		if c := find(r, rid("rec", n)); c.Lane != "" || c.Action != plan.ActHalt {
			t.Fatalf("宣言の外が止まらない: %+v", c)
		}
	}
	if len(h.p.Writes()) != 3 || cli.ExitCode(r) != cli.ExitHalted {
		t.Fatalf("writes=%v exit=%d", h.p.Writes(), cli.ExitCode(r))
	}
	if m, _ := h.rec.Marks(); m.Lanes["renumber"].Used != 3 {
		t.Fatalf("使った数: %+v", m.Lanes)
	}
	// 使い切った：同じ宣言のまままたずれても通常の予算（止まる）。
	drift()
	r = h.run()
	if len(h.p.Writes()) != 3 || r.Lanes[0].Note == "" {
		t.Fatalf("使い切った別枠で通した: %+v %+v", r.Lanes, h.p.Writes())
	}
	// max_count を広げれば（承認し直して）残りを使う。使った数は保つ。
	h.in.Policies[1].Allow = &core.Allow{Kind: core.AllowUpdate, MaxCount: 4}
	r = h.run()
	if r.Lanes[0].Open || r.Lanes[0].Request == req {
		t.Fatalf("枠を広げたのに承認し直さない: %+v", r.Lanes)
	}
	if err := record.WriteDecision(h.rec.Dir(), r.Lanes[0].Request, record.Decision{By: "alice", Decision: record.Approve, At: h.clock.t}); err != nil {
		t.Fatal(err)
	}
	r = h.run()
	if r.Lanes[0].Used != 3 || r.Lanes[0].Admitted != 1 || len(h.p.Writes()) != 4 {
		t.Fatalf("広げた枠: %+v %v", r.Lanes, h.p.Writes())
	}
	// 窓が過ぎれば閉じる。
	drift()
	h.clock.t = h.clock.t.Add(2 * time.Hour)
	if r := h.run(); r.Lanes[0].Open || len(h.p.Writes()) != 4 {
		t.Fatalf("窓のあとに開いた: %+v", r.Lanes)
	}
}

// E-A9：reimage は同じ種類・同じラベルの削除と作成の対を 1 と数え、壊す予算に
// 数えない。対の無い削除は通常の予算。
func TestLaneReimagePairs(t *testing.T) {
	h := newHarness(t)
	slot := func(n string) map[string]string { return map[string]string{"pool": "hpc", "slot": n} }
	h.in.Ranges = nil
	for _, n := range []string{"1", "2", "4"} {
		h.in.Ranges = append(h.in.Ranges, core.Range{Provider: "p", Args: map[string]string{"prefix": "n" + n + "-"}, Labels: slot(n), Prune: true, MaxDrop: core.DefaultMaxDrop})
		h.p.Set(rid("rec", "n"+n+"-v1"), present(core.Fields{"value": "v1"}))
	}
	h.in.Resources = []core.Resource{labeled("n1-v2", "v2", slot("1")), labeled("n2-v2", "v2", slot("2"))}
	counts := map[string]int{}
	for _, rg := range h.in.Ranges {
		counts[rg.Key()] = 1
	}
	if err := h.rec.WriteMarks(record.Marks{Cycle: 1, RangeCounts: counts}); err != nil {
		t.Fatal(err)
	}
	h.in.Policies = []core.Policy{
		{Name: "default", Select: core.Selector{All: true}, Budget: &core.Budget{PerCycle: core.Unlimited, DestructivePerCycle: 1, DestructivePerWindow: core.Unlimited}},
		{Name: "reimage", Select: core.Selector{Labels: map[string]string{"pool": "hpc"}}, Allow: &core.Allow{Kind: core.AllowReimage, MaxCount: 2}},
	}
	r := h.run()
	if cli.ExitCode(r) != cli.ExitOK || len(h.p.Writes()) != 5 {
		t.Fatalf("exit=%d halts=%v writes=%v", cli.ExitCode(r), r.Halts, h.p.Writes())
	}
	for _, n := range []string{"n1-v1", "n1-v2", "n2-v1", "n2-v2"} {
		if c := find(r, rid("rec", n)); c.Lane != "reimage" {
			t.Fatalf("対が別枠に入らない: %+v", c)
		}
	}
	if c := find(r, rid("rec", "n4-v1")); c.Lane != "" || c.Outcome != plan.OutcomeOK {
		t.Fatalf("対の無い削除: %+v", c)
	}
	if m, _ := h.rec.Marks(); m.Lanes["reimage"].Used != 2 || m.BudgetWindows != nil && m.BudgetWindows["default"].Destructive > 1 {
		t.Fatalf("数え方: %+v %+v", m.Lanes, m.BudgetWindows)
	}
}

// E-J2・N-J2：承認者を置かない別枠は cooling-off（宣言を初めて見てから）が過ぎて
// から開く。時計は Record にあり、plan は進めない。
func TestLaneCoolingOff(t *testing.T) {
	h := newHarness(t)
	h.in.Ranges = nil
	l := map[string]string{"pool": "web"}
	h.in.Resources = []core.Resource{labeled("a", "new", l), labeled("b", "new", l)}
	for _, r := range h.in.Resources {
		h.p.Set(r.ID, present(core.Fields{"value": "old"}))
	}
	h.in.Policies = []core.Policy{
		{Name: "default", Select: core.Selector{All: true}, Budget: &core.Budget{PerCycle: 1, DestructivePerCycle: core.Unlimited, DestructivePerWindow: core.Unlimited}},
		{Name: "bulk", Select: core.Selector{Labels: l}, Allow: &core.Allow{Kind: core.AllowUpdate, MaxCount: 2}, Approval: &core.Approval{CoolingOff: time.Hour}},
	}
	if r := h.run(); r.Lanes[0].Open || len(h.p.Writes()) != 0 {
		t.Fatalf("cooling-off の間に開いた: %+v", r.Lanes)
	}
	h.clock.t = h.clock.t.Add(30 * time.Minute)
	if r := h.run(); r.Lanes[0].Open {
		t.Fatalf("起こし直しで時計が戻った: %+v", r.Lanes)
	}
	h.clock.t = h.clock.t.Add(31 * time.Minute)
	if r := h.run(); !r.Lanes[0].Open || len(h.p.Writes()) != 2 {
		t.Fatalf("cooling-off のあとに開かない: %+v %v", r.Lanes, h.p.Writes())
	}
}
