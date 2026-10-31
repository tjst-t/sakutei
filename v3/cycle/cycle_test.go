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
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/cycle"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/record"
	"github.com/tjst-t/sakutei/v3/sdk/fake"
)

var schema = core.Schema{Version: "fake/1", Kinds: map[string]core.KindSchema{
	"rec": {Fields: map[string]core.FieldSpec{
		"value": {Type: core.String},
		"zone":  {Type: core.String, Identity: true},
	}},
	"vol": {DestructiveDelete: true, Fields: map[string]core.FieldSpec{"size": {Type: core.Int}}},
	"ro":  {ObserveOnly: true, Fields: map[string]core.FieldSpec{"version": {Type: core.String}}},
}}

func rid(kind, name string) core.ID { return core.ID{Type: "p/" + kind, Name: name} }

func present(f core.Fields) core.Observation {
	return core.Observation{Availability: core.Present, Fields: f}
}

func decl(name, value string) core.Resource {
	return core.Resource{ID: rid("rec", name), Fields: core.Fields{"value": value}}
}

func absent(name string) core.Resource {
	return core.Resource{ID: rid("rec", name), Ensure: core.EnsureAbsent}
}

type clock struct{ t time.Time }

func (c *clock) Now() time.Time { return c.t }

type harness struct {
	t      *testing.T
	p      *fake.Provider
	rec    *record.Record
	clock  *clock
	in     cycle.Input
	ranges []core.Range
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	rec, err := record.Open(filepath.Join(t.TempDir(), "record"))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, p: fake.New(schema), rec: rec, clock: &clock{t: time.Date(2026, 11, 5, 22, 0, 0, 0, time.UTC)}}
	h.in = cycle.Input{
		Ranges:    []core.Range{{Provider: "p", Prune: true, MaxDrop: core.DefaultMaxDrop}},
		Providers: map[string]core.Provider{"p": h.p},
		Record:    rec,
		Clock:     h.clock,
	}
	return h
}

func (h *harness) run() plan.Result {
	h.t.Helper()
	r, err := cycle.Run(context.Background(), h.in)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

func (h *harness) deletes() []core.ID {
	var out []core.ID
	for _, w := range h.p.Writes() {
		if w.Op == "delete" {
			out = append(out, w.ID)
		}
	}
	return out
}

func find(r plan.Result, id core.ID) plan.Change {
	for _, c := range r.Changes {
		if c.Resource == id.String() {
			return c
		}
	}
	return plan.Change{}
}

// 1 周目で範囲の件数を覚え、2 周目に宣言外を消す。これが基準の筋書きで、下の
// テストは条件を 1 つずつ外す。
func TestPruneHappyPath(t *testing.T) {
	h := newHarness(t)
	h.in.Resources = []core.Resource{decl("keep", "a")}
	h.p.Set(rid("rec", "keep"), present(core.Fields{"value": "a"}))
	h.p.Set(rid("rec", "stray"), present(core.Fields{"value": "x"}))

	r := h.run()
	if c := find(r, rid("rec", "stray")); c.Op != plan.Prune || c.Action != plan.ActHold {
		t.Fatalf("1 周目: %+v", c)
	}
	if len(h.deletes()) != 0 || cli.ExitCode(r) != 3 {
		t.Fatalf("1 周目に消した: %v exit=%d", h.deletes(), cli.ExitCode(r))
	}
	r = h.run()
	if c := find(r, rid("rec", "stray")); c.Outcome != plan.OutcomeOK || c.After != plan.None {
		t.Fatalf("2 周目: %+v", c)
	}
	if got := h.deletes(); !reflect.DeepEqual(got, []core.ID{rid("rec", "stray")}) || cli.ExitCode(r) != 0 {
		t.Fatalf("deletes=%v exit=%d", got, cli.ExitCode(r))
	}
	// 自分が消した分を差し引いた件数が次の基準（E-K2）。
	m, _ := h.rec.Marks()
	if m.RangeCounts["p:"] != 1 || m.Cycle != 2 {
		t.Fatalf("marks: %+v", m)
	}
}

// 不変条件 6・7・9：消す条件のどれか 1 つを外すと消えない。
func TestPruneNeedsEveryCondition(t *testing.T) {
	stray := rid("rec", "stray")
	cases := []struct {
		name  string
		setup func(h *harness)
	}{
		{"Range が無い", func(h *harness) { h.in.Ranges = nil }},
		{"prune: false", func(h *harness) { h.in.Ranges[0].Prune = false }},
		{"List が不完全", func(h *harness) { h.p.SetListIncomplete(true) }},
		{"List が失敗", func(h *harness) { h.p.SetListError(errors.New("timeout")) }},
		{"List に出ない", func(h *harness) { h.p.HideFromList(stray) }},
		{"宣言にある", func(h *harness) { h.in.Resources = append(h.in.Resources, decl("stray", "x")) }},
		{"外の持ち主", func(h *harness) {
			h.in.Ranges[0].ExternalOwners = []core.Exclusion{{Owner: "dhcp", IDPrefix: "rec/str", Reason: "動的"}}
		}},
		{"観測が Unknown", func(h *harness) { h.p.Set(stray, core.Observation{Availability: core.Unknown, Note: "届かない"}) }},
		{"観測が Converging", func(h *harness) { h.p.Set(stray, core.Observation{Availability: core.Converging}) }},
		{"観測が答えない", func(h *harness) { h.p.Omit(stray) }},
		{"Observe が失敗", func(h *harness) { h.p.SetObserveError(errors.New("refused")) }},
		{"予算 0", func(h *harness) {
			h.in.Policies = []core.Policy{{Name: "b", Select: core.Selector{All: true},
				Budget: &core.Budget{PerCycle: core.Unlimited, DestructivePerCycle: 0, DestructivePerWindow: core.Unlimited}}}
		}},
		{"report", func(h *harness) {
			h.in.Policies = []core.Policy{{Name: "r", Select: core.Selector{Types: []string{"p/rec"}}, Mode: core.ModeReport}}
		}},
		{"急減", func(h *harness) {
			for _, n := range []string{"a", "b", "c"} {
				h.p.HideFromList(rid("rec", n))
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.in.Resources = []core.Resource{decl("keep", "a")}
			h.p.Set(rid("rec", "keep"), present(core.Fields{"value": "a"}))
			for _, n := range []string{"a", "b", "c"} {
				h.in.Resources = append(h.in.Resources, decl(n, "a"))
				h.p.Set(rid("rec", n), present(core.Fields{"value": "a"}))
			}
			h.p.Set(stray, present(core.Fields{"value": "x"}))
			h.run() // 件数を覚える
			tc.setup(h)
			h.run()
			if got := h.deletes(); len(got) != 0 {
				t.Fatalf("消した: %v", got)
			}
		})
	}
	// 前の周の件数が無ければ消さない（Record を失った・初めての周）。
	t.Run("前の周の件数が無い", func(t *testing.T) {
		h := newHarness(t)
		h.p.Set(stray, present(core.Fields{"value": "x"}))
		h.run()
		if got := h.deletes(); len(got) != 0 {
			t.Fatalf("消した: %v", got)
		}
	})
	// 中身を失う種類・観測だけの種類は宣言外でも消さない（不変条件 9、E-C3）。
	t.Run("中身を失う種類", func(t *testing.T) {
		h := newHarness(t)
		h.p.Set(rid("vol", "data"), present(core.Fields{"size": 10}))
		h.p.Set(rid("ro", "fw"), present(core.Fields{"version": "1"}))
		h.run()
		r := h.run()
		if got := h.deletes(); len(got) != 0 {
			t.Fatalf("消した: %v", got)
		}
		if c := find(r, rid("vol", "data")); c.Op != plan.Unmanaged || cli.ExitCode(r) != 0 {
			t.Fatalf("%+v exit=%d", c, cli.ExitCode(r))
		}
	})
}

// 不変条件 7：List が急減した範囲は消さず周を止め（終了コード 4）、止めた件数を
// 新しい基準にしない。自分が消した分は差し引く。
func TestDropHaltsAndSubtractsOwnDeletes(t *testing.T) {
	h := newHarness(t)
	h.in.Ranges[0].MaxDrop = 20
	for i := 0; i < 7; i++ {
		n := string(rune('a' + i))
		h.in.Resources = append(h.in.Resources, decl(n, "v"))
		h.p.Set(rid("rec", n), present(core.Fields{"value": "v"}))
	}
	for _, n := range []string{"x", "y", "z"} {
		h.p.Set(rid("rec", n), present(core.Fields{"value": "stray"}))
	}
	h.run() // 基準 10
	r := h.run()
	if n := len(h.deletes()); n != 3 || cli.ExitCode(r) != 0 {
		t.Fatalf("宣言外 3 件を消すはず: %v exit=%d", h.deletes(), cli.ExitCode(r))
	}
	// 自分が消した 3 件を差し引いて基準は 7。10 → 7 の 30% 減を急減と読まない。
	r = h.run()
	if r.Halted() || cli.ExitCode(r) != 0 {
		t.Fatalf("自分の削除を急減と読んだ: %+v", r.Halts)
	}

	// ほかが 3 件消した（7 → 4、43% 減）：宣言外が現れても消さずに止める。
	for _, n := range []string{"a", "b", "c"} {
		h.p.HideFromList(rid("rec", n))
	}
	h.p.Set(rid("rec", "new-stray"), present(core.Fields{"value": "s"}))
	for i := 0; i < 2; i++ {
		before := len(h.deletes())
		r = h.run()
		if !r.Halted() || cli.ExitCode(r) != 4 || len(h.deletes()) != before {
			t.Fatalf("%d 回目: halts=%v exit=%d deletes=%v", i, r.Halts, cli.ExitCode(r), h.deletes())
		}
		m, _ := h.rec.Marks()
		if m.RangeCounts["p:"] != 7 {
			t.Fatalf("止めた周に基準を変えた: %v", m.RangeCounts)
		}
	}
}

// 不変条件 20：予算を超えたら超過分の保留でなく周全体を止める。
func TestBudgetHaltsWholeCycle(t *testing.T) {
	for _, limit := range []int{0, 2, 4, 5, 9} {
		h := newHarness(t)
		h.in.Ranges = nil
		for i := 0; i < 5; i++ {
			h.in.Resources = append(h.in.Resources, decl(string(rune('a'+i)), "v"))
		}
		h.in.Policies = []core.Policy{
			{Name: "all", Select: core.Selector{All: true}},
			{Name: "small", Select: core.Selector{Types: []string{"p/rec"}},
				Budget: &core.Budget{PerCycle: limit, DestructivePerCycle: core.Unlimited, DestructivePerWindow: core.Unlimited}},
		}
		r := h.run()
		writes := len(h.p.Writes())
		if limit < 5 {
			if writes != 0 || !r.Halted() || cli.ExitCode(r) != 4 {
				t.Fatalf("limit=%d: writes=%d halts=%v exit=%d", limit, writes, r.Halts, cli.ExitCode(r))
			}
			for _, c := range r.Changes {
				if c.Action != plan.ActHalt {
					t.Fatalf("limit=%d: %+v", limit, c)
				}
			}
			// 止めた周は窓も監査も使わない。
			if n, err := record.Verify(h.rec.Dir()); n != 0 || err != nil {
				t.Fatalf("監査: %d %v", n, err)
			}
			continue
		}
		if writes != 5 || r.Halted() || cli.ExitCode(r) != 0 {
			t.Fatalf("limit=%d: writes=%d halts=%v exit=%d", limit, writes, r.Halts, cli.ExitCode(r))
		}
	}
}

// 不変条件 20：周をまたいだ壊す変更を窓で数える。窓が過ぎれば数え直す。
// 断られた削除は数えない（不変条件 10）。
func TestBudgetWindow(t *testing.T) {
	h := newHarness(t)
	h.in.Ranges = nil
	h.in.Policies = []core.Policy{{Name: "w", Select: core.Selector{All: true},
		Budget: &core.Budget{PerCycle: core.Unlimited, DestructivePerCycle: core.Unlimited, DestructivePerWindow: 3, Window: time.Hour}}}
	gone := func(names ...string) {
		h.in.Resources = nil
		for _, n := range names {
			h.in.Resources = append(h.in.Resources, absent(n))
			h.p.Set(rid("rec", n), present(core.Fields{"value": "v"}))
		}
	}
	gone("a", "b")
	if r := h.run(); cli.ExitCode(r) != 0 || len(h.deletes()) != 2 {
		t.Fatalf("1 周目: exit=%d %v", cli.ExitCode(r), h.deletes())
	}
	gone("c", "d")
	h.clock.t = h.clock.t.Add(30 * time.Minute)
	r := h.run()
	if cli.ExitCode(r) != 4 || len(h.deletes()) != 2 || r.Budgets[0].WindowUsed != 4 {
		t.Fatalf("2 周目は 2+2 > 3 で止まるはず: exit=%d %v %+v", cli.ExitCode(r), h.deletes(), r.Budgets)
	}
	// 断られた削除は窓に数えない。
	h.p.RefuseDelete(rid("rec", "c"), "子がある")
	gone("c")
	r = h.run()
	if c := find(r, rid("rec", "c")); c.Outcome != plan.OutcomeRefused || c.Error != "子がある" || cli.ExitCode(r) != 3 {
		t.Fatalf("断り: %+v exit=%d", c, cli.ExitCode(r))
	}
	if m, _ := h.rec.Marks(); m.BudgetWindows["w"].Destructive != 2 {
		t.Fatalf("断りを数えた: %+v", m.BudgetWindows)
	}
	// 窓が過ぎれば数え直す。
	h.clock.t = h.clock.t.Add(time.Hour)
	gone("d", "e")
	if r := h.run(); cli.ExitCode(r) != 0 {
		t.Fatalf("窓のあと: exit=%d %+v", cli.ExitCode(r), r.Halts)
	}
}

// 作り直し（identity の変更）は壊す変更として数える（E-A10・不変条件 9）。
func TestReplaceIsDestructive(t *testing.T) {
	h := newHarness(t)
	h.in.Ranges = nil
	h.in.Resources = []core.Resource{{ID: rid("rec", "a"), Fields: core.Fields{"zone": "b"}}}
	h.p.Set(rid("rec", "a"), present(core.Fields{"zone": "a"}))
	h.in.Policies = []core.Policy{{Name: "d", Select: core.Selector{All: true},
		Budget: &core.Budget{PerCycle: core.Unlimited, DestructivePerCycle: 0, DestructivePerWindow: core.Unlimited}}}
	r := h.run()
	if c := find(r, rid("rec", "a")); !c.Destructive || c.Action != plan.ActHalt || len(h.p.Writes()) != 0 {
		t.Fatalf("%+v", c)
	}
}

// 不変条件 3：Unknown の上流の下流を進めない。上流が済めば下流を当てる。
func TestUnknownUpstreamBlocksDownstream(t *testing.T) {
	h := newHarness(t)
	h.in.Ranges = nil
	up := decl("up", "a")
	down := decl("down", "a")
	down.DependsOn = []core.Dependency{{On: up.ID}}
	ext := decl("ext-down", "a")
	ext.DependsOn = []core.Dependency{{On: rid("rec", "external")}}
	h.in.Resources = []core.Resource{up, down, ext}
	h.p.Set(up.ID, core.Observation{Availability: core.Unknown, Note: "届かない"})
	h.p.Set(rid("rec", "external"), core.Observation{Availability: core.Unknown})
	r := h.run()
	if len(h.p.Writes()) != 0 || cli.ExitCode(r) != 3 {
		t.Fatalf("writes=%v exit=%d", h.p.Writes(), cli.ExitCode(r))
	}
	if c := find(r, down.ID); c.Action != plan.ActWait || !strings.Contains(c.Blocked, "p/rec/up") {
		t.Fatalf("%+v", c)
	}
	// 上流が無い（作る）周は、上流を当てて効いたら同じ周に下流を観測し直して
	// 当てる（E-J7）。
	h.p.Set(up.ID, core.Observation{Availability: core.Absent})
	h.p.Set(rid("rec", "external"), present(nil))
	r = h.run()
	if got := h.p.Writes(); len(got) != 3 || got[2].ID != down.ID {
		t.Fatalf("writes: %v", got)
	}
	if c := find(r, down.ID); c.Outcome != plan.OutcomeOK || c.Wave != 2 || cli.ExitCode(r) != 0 {
		t.Fatalf("%+v exit=%d", c, cli.ExitCode(r))
	}
}

// 当てて失敗したものは誤りとして出し（終了コード 1）、ほかは当てる。監査に残す。
func TestApplyFailure(t *testing.T) {
	h := newHarness(t)
	h.in.Ranges = nil
	h.in.Resources = []core.Resource{decl("a", "1"), decl("b", "2")}
	h.p.FailApply(rid("rec", "a"), errors.New("500"))
	r := h.run()
	if a, b := find(r, rid("rec", "a")), find(r, rid("rec", "b")); a.Outcome != plan.OutcomeError || b.Outcome != plan.OutcomeOK || cli.ExitCode(r) != 1 {
		t.Fatalf("a=%+v b=%+v exit=%d", a, b, cli.ExitCode(r))
	}
	if n, err := record.Verify(h.rec.Dir()); n != 2 || err != nil {
		t.Fatalf("監査: %d %v", n, err)
	}
}

// mode: report は観測して知らせるだけ（E-I3）。最も厳しいものが勝つ（E-J5）。
func TestReportMode(t *testing.T) {
	h := newHarness(t)
	h.in.Ranges = nil
	a := decl("a", "1")
	a.Labels = map[string]string{"site": "legacy"}
	h.in.Resources = []core.Resource{a, decl("b", "1")}
	h.in.Policies = []core.Policy{
		{Name: "default", Select: core.Selector{All: true}},
		{Name: "legacy", Select: core.Selector{Labels: map[string]string{"site": "legacy"}}, Mode: core.ModeReport},
	}
	r := h.run()
	if c := find(r, a.ID); c.Action != plan.ActReport || !reflect.DeepEqual(c.Policies, []string{"default", "legacy"}) {
		t.Fatalf("%+v", c)
	}
	if got := h.p.Writes(); len(got) != 1 || got[0].ID != rid("rec", "b") || cli.ExitCode(r) != 3 {
		t.Fatalf("writes=%v exit=%d", got, cli.ExitCode(r))
	}
}

// ロックがほかにあれば進めない周：観測して比べるだけで、provider にも Record にも
// 書かない（E-M5・N-M7）。
func TestLockHeldElsewhere(t *testing.T) {
	h := newHarness(t)
	h.in.Resources = []core.Resource{decl("a", "1")}
	h.p.Set(rid("rec", "stray"), present(nil))
	h.run()
	other, err := record.Open(h.rec.Dir())
	if err != nil {
		t.Fatal(err)
	}
	release, held, err := other.Lock()
	if err != nil || !held {
		t.Fatalf("held=%v err=%v", held, err)
	}
	defer release()
	h.in.Resources = []core.Resource{decl("a", "2")}
	before, _ := h.rec.Marks()
	writes := len(h.p.Writes())
	r := h.run()
	after, _ := h.rec.Marks()
	if !r.AdvancedElsewhere || len(h.p.Writes()) != writes || !reflect.DeepEqual(before, after) || cli.ExitCode(r) != 3 {
		t.Fatalf("elsewhere=%v writes=%v marks %v→%v exit=%d", r.AdvancedElsewhere, h.p.Writes(), before, after, cli.ExitCode(r))
	}
}

// Record の目印が読めなければ当てず、目印を上書きしない（不変条件 7・20）。
func TestUnreadableMarks(t *testing.T) {
	h := newHarness(t)
	h.in.Resources = []core.Resource{decl("a", "1")}
	path := filepath.Join(h.rec.Dir(), "marks.json")
	if err := os.WriteFile(path, []byte("{壊れた"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := h.run()
	if len(h.p.Writes()) != 0 || len(r.Errors) == 0 || cli.ExitCode(r) != 1 {
		t.Fatalf("writes=%v errors=%v exit=%d", h.p.Writes(), r.Errors, cli.ExitCode(r))
	}
	if data, _ := os.ReadFile(path); string(data) != "{壊れた" {
		t.Fatalf("上書きした: %s", data)
	}
}

// 監査の最後の行が切れていれば当てない（鎖を書き換えて直さない、不変条件 28）。
func TestBrokenAuditBlocksWrites(t *testing.T) {
	h := newHarness(t)
	h.in.Resources = []core.Resource{decl("a", "1")}
	if err := os.MkdirAll(filepath.Join(h.rec.Dir(), "audit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.rec.Dir(), "audit", "000001.jsonl"), []byte(`{"seq":1`), 0o644); err != nil {
		t.Fatal(err)
	}
	r := h.run()
	if len(h.p.Writes()) != 0 || cli.ExitCode(r) != 1 {
		t.Fatalf("writes=%v exit=%d", h.p.Writes(), cli.ExitCode(r))
	}
}

// 不変条件 30：plan は reconcile と同じ判定をし、何も消費しない。
func TestPlanDecidesLikeReconcile(t *testing.T) {
	h := newHarness(t)
	h.in.Resources = []core.Resource{decl("a", "1"), absent("old")}
	h.p.Set(rid("rec", "old"), present(nil))
	h.p.Set(rid("rec", "stray"), present(nil))
	h.run()
	h.p.Set(rid("rec", "stray2"), present(nil))
	h.in.Resources = append(h.in.Resources, decl("b", "1"))

	marks, _ := h.rec.Marks()
	p := plan.Decide(context.Background(), plan.Input{Resources: h.in.Resources, Ranges: h.in.Ranges,
		Readers: map[string]core.Reader{"p": h.p}, Marks: marks, Now: h.clock.t})
	if after, _ := h.rec.Marks(); !reflect.DeepEqual(marks, after) {
		t.Fatal("plan が目印を変えた")
	}
	r := h.run()
	if len(p.Changes) != len(r.Changes) {
		t.Fatalf("plan=%d reconcile=%d", len(p.Changes), len(r.Changes))
	}
	for i := range p.Changes {
		a, b := p.Changes[i], r.Changes[i]
		if a.Resource != b.Resource || a.Op != b.Op || a.Action != b.Action {
			t.Fatalf("plan %+v\nreconcile %+v", a, b)
		}
	}
}
