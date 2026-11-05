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
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/condition"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/cycle"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/record"
	"github.com/tjst-t/sakutei/v3/sdk/fake"
)

func events(r plan.Result, kind string) []core.Event {
	var out []core.Event
	for _, e := range r.Events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// E-J7：空から組む構成（FW の許可 → その奥のホスト → ホストの上のもの）が 1 回の
// reconcile で収束する。奥のものは上流が無い間は観測できない（Unknown）が、上流を
// Apply して効いたら同じ cycle に再観測して Apply する。
func TestEmptyBuildConvergesInOneRun(t *testing.T) {
	h := newHarness(t)
	h.in.Ranges = nil
	fw := decl("fw-allow", "open")
	host := decl("host", "up")
	host.DependsOn = []core.Dependency{{On: fw.ID}}
	app := decl("app", "v1")
	app.DependsOn = []core.Dependency{{On: host.ID}}
	cfg := decl("cfg", "c1")
	cfg.DependsOn = []core.Dependency{{On: app.ID, When: condition.MustParse(`observe(p/rec/app).value == "v1"`)}}
	h.in.Resources = []core.Resource{cfg, app, host, fw} // 宣言の順は関係ない
	h.p.Behind(host.ID, fw.ID)
	h.p.Behind(app.ID, host.ID)
	h.p.Behind(cfg.ID, app.ID)

	// plan は再観測しない：最初の wave だけを出し、下流は待つか Unknown。
	pl := plan.Decide(context.Background(), plan.Input{Resources: h.in.Resources, Readers: map[string]core.Reader{"p": h.p}})
	if c := find(pl, fw.ID); c.Action != plan.ActApply || strings.Join(c.Downstream, ",") != "p/rec/app,p/rec/cfg,p/rec/host" {
		t.Fatalf("plan の fw: %+v", c)
	}
	if c := find(pl, host.ID); c.Op != plan.Unknown {
		t.Fatalf("plan の host: %+v", c)
	}

	r := h.run()
	if code := cli.ExitCode(r); code != cli.ExitOK {
		t.Fatalf("終了コード %d\n%+v", code, r)
	}
	want := []core.ID{fw.ID, host.ID, app.ID, cfg.ID}
	got := h.p.Writes()
	if len(got) != len(want) {
		t.Fatalf("writes: %v", got)
	}
	for i, w := range got {
		if w.ID != want[i] {
			t.Fatalf("Apply した順 %v、want %v", got, want)
		}
		if c := find(r, w.ID); c.Wave != i+1 || c.After != plan.None {
			t.Fatalf("%s: %+v", w.ID, c)
		}
	}
	if len(events(r, "Waiting")) != 0 || len(events(r, "Unknown")) != 0 {
		t.Fatalf("収束したのに待ち・Unknown の Event がある: %+v", r.Events)
	}
	// 次の cycle は何もしない。
	r = h.run()
	if len(h.p.Writes()) != 4 || cli.ExitCode(r) != cli.ExitOK {
		t.Fatalf("2 cycle 目: writes=%v exit=%d", h.p.Writes(), cli.ExitCode(r))
	}
}

// 再観測は回数 3 まで。5 つ連なる依存は 1 cycle で 4 つまで進み、残りは次の cycle。
func TestReobserveRoundLimit(t *testing.T) {
	h := newHarness(t)
	h.in.Ranges = nil
	var prev *core.Resource
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		r := decl(n, "x")
		if prev != nil {
			r.DependsOn = []core.Dependency{{On: prev.ID}}
		}
		h.in.Resources = append(h.in.Resources, r)
		prev = &h.in.Resources[len(h.in.Resources)-1]
	}
	r := h.run()
	if n := len(h.p.Writes()); n != 1+cycle.ReobserveRounds {
		t.Fatalf("1 cycle 目に %d 件 Apply した: %v", n, h.p.Writes())
	}
	if c := find(r, rid("rec", "e")); c.Action != plan.ActWait || cli.ExitCode(r) != cli.ExitRemaining {
		t.Fatalf("%+v exit=%d", c, cli.ExitCode(r))
	}
	r = h.run()
	if len(h.p.Writes()) != 5 || cli.ExitCode(r) != cli.ExitOK {
		t.Fatalf("2 cycle 目: %v exit=%d", h.p.Writes(), cli.ExitCode(r))
	}
}

// 上流の Apply が失敗・ineffective なら、下流は同じ cycle に進めない（E-D6）。
func TestReobserveNeedsEffectiveUpstream(t *testing.T) {
	h := newHarness(t)
	h.in.Ranges = nil
	up := decl("up", "a")
	down := decl("down", "a")
	down.DependsOn = []core.Dependency{{On: up.ID}}
	h.in.Resources = []core.Resource{up, down}
	h.p.SetIneffective(up.ID, true)
	r := h.run()
	if got := h.p.Writes(); len(got) != 1 || got[0].ID != up.ID {
		t.Fatalf("writes: %v", got)
	}
	if c := find(r, down.ID); c.Action != plan.ActWait {
		t.Fatalf("%+v", c)
	}
}

// 消す宣言は下流から消す（§3.5）。下流が残る間は上流を消さず、下流が消えたら
// 同じ cycle に上流を消す。
func TestDeleteDownstreamFirst(t *testing.T) {
	h := newHarness(t)
	h.in.Ranges = nil
	up := absent("up")
	down := absent("down")
	down.DependsOn = []core.Dependency{{On: up.ID}}
	h.in.Resources = []core.Resource{up, down}
	h.p.Set(up.ID, present(core.Fields{"value": "a"}))
	h.p.Set(down.ID, present(core.Fields{"value": "a"}))

	pl := plan.Decide(context.Background(), plan.Input{Resources: h.in.Resources, Readers: map[string]core.Reader{"p": h.p}})
	if c := find(pl, up.ID); c.Action != plan.ActWait || !strings.Contains(c.Blocked, "p/rec/down") {
		t.Fatalf("plan の up: %+v", c)
	}
	r := h.run()
	got := h.p.Writes()
	if len(got) != 2 || got[0].ID != down.ID || got[1].ID != up.ID || cli.ExitCode(r) != cli.ExitOK {
		t.Fatalf("writes=%v exit=%d", got, cli.ExitCode(r))
	}
}

// 不変条件 14：成功を返すが現実を変えない provider（人とツールの取り合い、E-J6・
// E-K10）。ineffective Apply を cycle をまたいで数え、上限（既定 3）で halt する。halt した
// あとは Apply しない。one-shot 実行でも終了コード 4 で分かる。
func TestIneffectiveApplyHalts(t *testing.T) {
	h := newHarness(t)
	h.in.Ranges = nil
	a := decl("a", "want")
	h.in.Resources = []core.Resource{a}
	h.p.Set(a.ID, present(core.Fields{"value": "theirs"}))
	h.p.SetIneffective(a.ID, true)
	for i := 1; i <= core.DefaultMaxIneffectiveApplies; i++ {
		r := h.run()
		c := find(r, a.ID)
		if c.Outcome != plan.OutcomeOK || c.After != plan.Update || c.Ineffective != i {
			t.Fatalf("%d cycle 目: %+v", i, c)
		}
		want := cli.ExitRemaining
		if i == core.DefaultMaxIneffectiveApplies {
			want = cli.ExitHalted
			if hs := events(r, "Halt"); len(hs) != 1 || hs[0].Scope != "resource p/rec/a" || hs[0].State != core.Firing {
				t.Fatalf("Halt: %+v", hs)
			}
		}
		if code := cli.ExitCode(r); code != want {
			t.Fatalf("%d cycle 目: 終了コード %d、want %d", i, code, want)
		}
	}
	m, err := h.rec.Marks()
	if err != nil || m.Ineffective[a.ID.String()].Count != 3 {
		t.Fatalf("marks: %+v %v", m.Ineffective, err)
	}
	r := h.run()
	if c := find(r, a.ID); c.Action != plan.ActHalt || len(h.p.Writes()) != 3 || cli.ExitCode(r) != cli.ExitHalted {
		t.Fatalf("上限のあとも Apply した: %+v writes=%d", c, len(h.p.Writes()))
	}

	// 宣言を直せば数え直す。
	h.in.Resources[0].Fields["value"] = "want2"
	r = h.run()
	if c := find(r, a.ID); c.Outcome != plan.OutcomeOK || c.Ineffective != 1 {
		t.Fatalf("宣言を直したあと: %+v", c)
	}
	// 現実が宣言どおりになれば消す。
	h.p.SetIneffective(a.ID, false)
	r = h.run()
	if c := find(r, a.ID); c.After != plan.None {
		t.Fatalf("%+v", c)
	}
	if m, _ := h.rec.Marks(); len(m.Ineffective) != 0 {
		t.Fatalf("宣言どおりなのに marks が残る: %+v", m.Ineffective)
	}
}

// 不変条件 14：再観測が Unknown・Converging なら ineffective の回数に数えない（不変条件 4）。
// Policy の max_ineffective_applies は最も小さいものが勝つ（E-J5）。
func TestIneffectiveCountsOnlyCertainRereads(t *testing.T) {
	h := newHarness(t)
	h.in.Ranges = nil
	a := decl("a", "want")
	h.in.Resources = []core.Resource{a}
	h.in.Policies = []core.Policy{
		{Name: "loose", Select: core.Selector{All: true}, Budget: &core.Budget{PerCycle: core.Unlimited, DestructivePerCycle: core.Unlimited,
			DestructivePerWindow: core.Unlimited, MaxIneffectiveApplies: 5}},
		{Name: "tight", Select: core.Selector{All: true}, Budget: &core.Budget{PerCycle: core.Unlimited, DestructivePerCycle: core.Unlimited,
			DestructivePerWindow: core.Unlimited, MaxIneffectiveApplies: 2}},
	}
	// 1 cycle 目：Apply したあと再観測すると Converging（Apply の中で Converging にする provider）。
	p := &convergingAfterApply{Provider: h.p}
	h.in.Providers["p"] = p
	r := h.run()
	if c := find(r, a.ID); c.After != plan.Converging || c.Ineffective != 0 {
		t.Fatalf("%+v", c)
	}
	// 2・3 cycle 目：ineffective Apply。2 回で halt する。
	h.in.Providers["p"] = h.p
	h.p.Set(a.ID, present(core.Fields{"value": "theirs"}))
	h.p.SetIneffective(a.ID, true)
	h.run()
	r = h.run()
	if c := find(r, a.ID); c.Ineffective != 2 || cli.ExitCode(r) != cli.ExitHalted {
		t.Fatalf("%+v exit=%d", c, cli.ExitCode(r))
	}
}

type convergingAfterApply struct{ *fake.Provider }

func (p *convergingAfterApply) Apply(ctx context.Context, w core.WriteContext, id core.ID, desired core.Fields, cur core.Observation) error {
	if err := p.Provider.Apply(ctx, w, id, desired, cur); err != nil {
		return err
	}
	p.Provider.Set(id, core.Observation{Availability: core.Converging, Note: "再起動中"})
	return nil
}

// E-C4：容量の門。プールの使用率が閾値を超えたら新しい VM を作らない（待つ）。
// 観測できないプールは容量内と断じない（on_unknown: block で halt して通知する）。
func TestCapacityGate(t *testing.T) {
	h := newHarness(t)
	h.in.Ranges = nil
	pool := core.ID{Type: "p/pool", Name: "ceph"}
	vm := decl("vm1", "x")
	vm.DependsOn = []core.Dependency{{When: condition.MustParse(`observe(p/pool/ceph).usage < 80`), OnUnknown: core.OnUnknownBlock}}
	h.in.Resources = []core.Resource{vm}

	// 超えている：待つ（終了コード 3、Waiting）。
	h.p.Set(pool, present(core.Fields{"usage": int64(91)}))
	r := h.run()
	c := find(r, vm.ID)
	if c.Action != plan.ActWait || !strings.Contains(c.Blocked, "が偽") || len(h.p.Writes()) != 0 || cli.ExitCode(r) != cli.ExitRemaining {
		t.Fatalf("%+v exit=%d", c, cli.ExitCode(r))
	}
	if len(events(r, "Waiting")) != 1 {
		t.Fatalf("Waiting が無い: %+v", r.Events)
	}

	// 観測できない：halt する（終了コード 4、Halt）。
	h.p.Set(pool, core.Observation{Availability: core.Unknown, Note: "届かない"})
	r = h.run()
	c = find(r, vm.ID)
	if c.Action != plan.ActHalt || !strings.Contains(c.Blocked, "on_unknown: block") || len(h.p.Writes()) != 0 || cli.ExitCode(r) != cli.ExitHalted {
		t.Fatalf("%+v exit=%d", c, cli.ExitCode(r))
	}
	if hs := events(r, "Halt"); len(hs) != 1 || hs[0].Scope != "resource p/rec/vm1" {
		t.Fatalf("Halt: %+v", r.Events)
	}

	// 既定の on_unknown（wait）なら待つだけ。
	h.in.Resources[0].DependsOn[0].OnUnknown = core.OnUnknownWait
	r = h.run()
	if c := find(r, vm.ID); c.Action != plan.ActWait || cli.ExitCode(r) != cli.ExitRemaining {
		t.Fatalf("%+v", c)
	}

	// 容量内：作る。
	h.in.Resources[0].DependsOn[0].OnUnknown = core.OnUnknownBlock
	h.p.Set(pool, present(core.Fields{"usage": int64(40)}))
	r = h.run()
	if c := find(r, vm.ID); c.Outcome != plan.OutcomeOK || cli.ExitCode(r) != cli.ExitOK {
		t.Fatalf("%+v exit=%d", c, cli.ExitCode(r))
	}
}

// on_unknown: ignore は不明を真とみなす（報告だけの条件、N-H6）。ただし名指しの
// 上流が Unknown なら待つ（不変条件 3）。
func TestOnUnknownIgnore(t *testing.T) {
	h := newHarness(t)
	h.in.Ranges = nil
	mon := core.ID{Type: "p/pool", Name: "mon"}
	a := decl("a", "x")
	a.DependsOn = []core.Dependency{{When: condition.MustParse(`observe(p/pool/mon).usage < 80`), OnUnknown: core.OnUnknownIgnore}}
	b := decl("b", "x")
	b.DependsOn = []core.Dependency{{On: mon, When: condition.MustParse(`observe(p/pool/mon).usage < 80`), OnUnknown: core.OnUnknownIgnore}}
	h.in.Resources = []core.Resource{a, b}
	h.p.Set(mon, core.Observation{Availability: core.Unknown})
	r := h.run()
	if c := find(r, a.ID); c.Outcome != plan.OutcomeOK {
		t.Fatalf("a: %+v", c)
	}
	if c := find(r, b.ID); c.Action != plan.ActWait || !strings.Contains(c.Blocked, "上流 p/pool/mon") {
		t.Fatalf("b: %+v", c)
	}
}

// quantifier（N-B18・N-J3）と check（E-B9）：95% 以上が healthy で、登録した check が通るまで
// 待つ。check の理由を待ちの理由に出す。
func TestQuantifiedConditionAndCheck(t *testing.T) {
	h := newHarness(t)
	h.in.Ranges = nil
	var nodes []core.Resource
	for _, n := range []string{"n1", "n2", "n3"} {
		r := decl(n, "ok")
		r.Labels = map[string]string{"role": "node"}
		nodes = append(nodes, r)
		h.p.Set(r.ID, present(core.Fields{"value": "ok"}))
	}
	lb := decl("lb", "on")
	lb.DependsOn = []core.Dependency{{When: condition.MustParse(`count(p/rec[role=node], it.value == "ok") >= 3 and check(bgp-up)`)}}
	h.in.Resources = append(nodes, lb)
	bgp := core.TriFalse
	h.in.Checks = map[string]core.CheckFunc{"bgp-up": func(_ context.Context, obs core.ObservationView) (core.Tri, string) {
		if _, ok := obs.Observation(rid("rec", "n1")); !ok {
			return core.TriUnknown, "n1 を観測していない"
		}
		return bgp, "peer 192.0.2.1 が Idle"
	}}
	r := h.run()
	if c := find(r, lb.ID); c.Action != plan.ActWait || !strings.Contains(c.Blocked, "peer 192.0.2.1 が Idle") {
		t.Fatalf("%+v", c)
	}
	bgp = core.TriTrue
	h.p.Set(rid("rec", "n3"), core.Observation{Availability: core.Unknown})
	r = h.run()
	if c := find(r, lb.ID); c.Action != plan.ActWait || !strings.Contains(c.Blocked, "分からない") {
		t.Fatalf("%+v", c)
	}
	h.p.Set(rid("rec", "n3"), present(core.Fields{"value": "ok"}))
	r = h.run()
	if c := find(r, lb.ID); c.Outcome != plan.OutcomeOK {
		t.Fatalf("%+v", c)
	}
}

// settle は Apply した type の最も長いものを 1 wave に 1 回待ってから再観測する（E-J6・E-R3）。
func TestSettleBeforeReread(t *testing.T) {
	h := newHarness(t)
	h.in.Ranges = nil
	slow := core.Schema{Version: "fake/1", Kinds: map[string]core.KindSchema{
		"rec":  schema.Kinds["rec"],
		"slow": {Settle: 3 * time.Second, Fields: map[string]core.FieldSpec{"value": {Type: core.String}}},
	}}
	h.p = fake.New(slow)
	h.in.Providers["p"] = h.p
	h.in.Resources = []core.Resource{decl("a", "x"), {ID: rid("slow", "b"), Fields: core.Fields{"value": "x"}}}
	var slept []time.Duration
	h.in.Sleep = func(_ context.Context, d time.Duration) { slept = append(slept, d) }
	h.run()
	if len(slept) != 1 || slept[0] != 3*time.Second {
		t.Fatalf("待った: %v", slept)
	}
}

// budget は cycle の全部の wave で数える（不変条件 20）。後の wave で超えたらその wave を止める。
func TestBudgetAcrossWaves(t *testing.T) {
	h := newHarness(t)
	h.in.Ranges = nil
	up := decl("up", "a")
	d1 := decl("d1", "a")
	d1.DependsOn = []core.Dependency{{On: up.ID}}
	d2 := decl("d2", "a")
	d2.DependsOn = []core.Dependency{{On: up.ID}}
	h.in.Resources = []core.Resource{up, d1, d2}
	h.in.Policies = []core.Policy{{Name: "b", Select: core.Selector{All: true}, Budget: &core.Budget{PerCycle: 2,
		DestructivePerCycle: core.Unlimited, DestructivePerWindow: core.Unlimited, MaxIneffectiveApplies: core.Unlimited}}}
	r := h.run()
	if len(h.p.Writes()) != 1 || cli.ExitCode(r) != cli.ExitHalted {
		t.Fatalf("writes=%v exit=%d", h.p.Writes(), cli.ExitCode(r))
	}
	if c := find(r, d1.ID); c.Action != plan.ActHalt {
		t.Fatalf("%+v", c)
	}
	if _, err := record.Verify(h.rec.Dir()); err != nil {
		t.Fatal(err)
	}
}
