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
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/record"
	"github.com/tjst-t/sakutei/v3/sdk/fake"
)

// 観測の答えの分類。
type obsKind int

const (
	obsSame obsKind = iota
	obsDiff
	obsAbsent
	obsUnknown
	obsConverging
	obsOmit
	obsKinds
)

// world は 1 cycle ぶんの偽の現実と、そこで消してよいかを外から判定する材料。
type world struct {
	obs         map[core.ID]obsKind
	listBroken  bool // List のエラーか不完全
	hidden      map[core.ID]bool
	observeErr  bool
	ineffective []core.ID
}

// 性質のテスト（§4 の「性質」、E-P1）：偽の provider にランダムな観測の並び・List の
// 欠け・budget・mode を与えて cycle を重ね、どの cycle でも次が成り立つことを確かめる。
//
//   - 不変条件 3：Unknown・Converging・答えなし・Observe の失敗の対象に書かない。
//     Unknown の上流の下流に書かない。
//   - 不変条件 6・7・9：unmanaged を消すのは、prune の Range で、List が完全で、List に
//     出て、external owner でなく、観測が Present で、消せる type で、前の cycle の件数があり、
//     急減していないときだけ。
//   - 不変条件 20：書いた数が budget を超えない。budget で halt した cycle は 1 件も書かない
//     （超過分だけを保留しない）。
//   - 不変条件 1：Record のどのファイルにも観測の値が現れない（通知の状態・outbox を
//     含む）。
//   - 不変条件 29：観測が Unknown・Converging・答えなし・Observe の失敗の対象と、List が
//     壊れた cycle の Range の Event を resolved にしない。
//   - 不変条件 28：送れない sink があっても cycle のエラーにならない。
func TestCycleProperties(t *testing.T) {
	seeds := 400
	if testing.Short() || raceEnabled {
		seeds = 40
	}
	for seed := 1; seed <= seeds; seed++ {
		seed := seed
		t.Run(fmt.Sprint(seed), func(t *testing.T) { property(t, int64(seed)) })
	}
}

func property(t *testing.T, seed int64) {
	rng := rand.New(rand.NewSource(seed))
	h := newHarness(t)
	rg := &h.in.Ranges[0]
	rg.Prune = rng.Intn(4) != 0
	rg.MaxDrop = []int{0, 20, 50, 100}[rng.Intn(4)]
	if rng.Intn(2) == 0 {
		rg.ExternalOwners = []core.Exclusion{{Owner: "other", IDPrefix: "rec/ext-", Reason: "外"}}
	}

	// 宣言：作る・変える・消す・作り直し・依存。
	nDecl := rng.Intn(7)
	var declared []core.Resource
	for i := 0; i < nDecl; i++ {
		r := decl(fmt.Sprintf("d%d", i), "want")
		switch rng.Intn(5) {
		case 0:
			r = absent(r.ID.Name)
		case 1:
			r.Fields["zone"] = "z1"
		}
		if i > 0 && rng.Intn(3) == 0 {
			r.DependsOn = []core.Dependency{{On: declared[rng.Intn(i)].ID}}
		}
		declared = append(declared, r)
	}
	h.in.Resources = declared
	declaredSet := map[core.ID]*core.Resource{}
	for i := range declared {
		declaredSet[declared[i].ID] = &declared[i]
	}

	// unmanaged：消せる type・中身を失う type・観測だけ・external owner。
	var strays []core.ID
	for i := 0; i < rng.Intn(8); i++ {
		kind := []string{"rec", "rec", "rec", "vol", "ro"}[rng.Intn(5)]
		name := fmt.Sprintf("s%d", i)
		if rng.Intn(5) == 0 {
			name = fmt.Sprintf("ext-%d", i)
		}
		strays = append(strays, rid(kind, name))
	}

	// budget と mode。
	switch rng.Intn(4) {
	case 0:
		h.in.Policies = []core.Policy{{Name: "b", Select: core.Selector{All: true}, Budget: &core.Budget{
			PerCycle: rng.Intn(6), DestructivePerCycle: rng.Intn(4) - 1, DestructivePerWindow: rng.Intn(6) - 1, Window: time.Hour}}}
		if b := h.in.Policies[0].Budget; b.DestructivePerWindow == core.Unlimited {
			b.Window = 0
		}
	case 1:
		h.in.Policies = []core.Policy{{Name: "r", Select: core.Selector{Types: []string{"p/rec"}}, Mode: core.ModeReport}}
	}

	marker := fmt.Sprintf("obs-%d-", seed)
	ineffective := map[core.ID]int{} // 続いた ineffective Apply（不変条件 14）
	for cyc := 0; cyc < 6; cyc++ {
		h.p = fake.New(schema)
		h.in.Providers["p"] = h.p
		w := world{obs: map[core.ID]obsKind{}, hidden: map[core.ID]bool{}}
		all := append([]core.ID{}, strays...)
		for _, r := range declared {
			all = append(all, r.ID)
		}
		for i, id := range all {
			k := obsKind(rng.Intn(int(obsKinds)))
			if _, ok := declaredSet[id]; !ok && rng.Intn(2) == 0 {
				k = obsSame // unmanaged は Present を多めに
			}
			w.obs[id] = k
			val := fmt.Sprintf("%s%d", marker, i) // Record に出てはいけない観測の値
			switch k {
			case obsSame:
				f := core.Fields{"value": "want", "version": val, "size": 1}
				if r := declaredSet[id]; r != nil && r.Fields["zone"] != nil {
					f["zone"] = "z1"
				}
				h.p.Set(id, present(f))
			case obsDiff:
				h.p.Set(id, present(core.Fields{"value": val, "zone": "z0"}))
			case obsAbsent:
				h.p.Set(id, core.Observation{Availability: core.Absent})
			case obsUnknown:
				h.p.Set(id, core.Observation{Availability: core.Unknown, Note: "届かない"})
			case obsConverging:
				h.p.Set(id, core.Observation{Availability: core.Converging})
			case obsOmit:
				h.p.Set(id, present(core.Fields{"value": val}))
				h.p.Omit(id)
			}
			if rng.Intn(6) == 0 {
				h.p.HideFromList(id)
				w.hidden[id] = true
			}
			if rng.Intn(2) == 0 {
				h.p.SetIneffective(id, true) // 成功を返すが現実を変えない
				w.ineffective = append(w.ineffective, id)
			}
		}
		switch rng.Intn(8) {
		case 0:
			h.p.SetListIncomplete(true)
			w.listBroken = true
		case 1:
			h.p.SetListError(errors.New("timeout"))
			w.listBroken = true
		case 2:
			h.p.SetObserveError(errors.New("refused"))
			w.observeErr = true
		}

		prev, err := h.rec.Marks()
		if err != nil {
			t.Fatal(err)
		}
		h.clock.t = h.clock.t.Add(time.Duration(rng.Intn(50)) * time.Minute)
		// 送れない sink を混ぜても cycle は変わらない（不変条件 28）。
		h.in.Sinks = map[string]core.Sink{"flaky": failSink{fail: rng.Intn(2) == 0}}
		r := h.run()
		check(t, cyc, h, w, declaredSet, prev, r)
		checkEvents(t, cyc, w, r)
		checkIneffective(t, cyc, h, w, declaredSet, ineffective)
	}

	// 不変条件 1：Record に観測の値を書かない。
	err := filepath.WalkDir(h.rec.Dir(), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), marker) {
			t.Errorf("%s に観測の値がある:\n%s", p, data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := record.Verify(h.rec.Dir()); err != nil {
		t.Fatalf("audit のハッシュチェーン: %v", err)
	}
}

func check(t *testing.T, cyc int, h *harness, w world, declared map[core.ID]*core.Resource, prev record.Marks, r plan.Result) {
	t.Helper()
	fail := func(format string, a ...any) {
		t.Helper()
		t.Fatalf("cycle %d: %s\nresult: %+v", cyc, fmt.Sprintf(format, a...), r)
	}
	rg := h.in.Ranges[0]
	writes := h.p.Writes()
	destructive := 0
	listed := 0
	for id := range w.obs {
		if (w.obs[id] == obsSame || w.obs[id] == obsDiff || w.obs[id] == obsOmit) && !w.hidden[id] {
			listed++
		}
	}
	base, hasBase := prev.RangeCounts[rg.Key()]
	dropped := hasBase && base > 0 && (base-listed)*100 > base*rg.MaxDrop

	for _, wr := range writes {
		k := w.obs[wr.ID]
		// 不変条件 3：確かでない対象には書かない。
		if w.observeErr || k == obsUnknown || k == obsConverging || k == obsOmit {
			fail("観測が確かでない %s に %s した（%d）", wr.ID, wr.Op, k)
		}
		res := declared[wr.ID]
		if res != nil {
			for _, d := range res.DependsOn {
				if dk := w.obs[d.On]; wr.Op == "apply" && (dk == obsUnknown || dk == obsConverging || dk == obsOmit) {
					fail("上流 %s が確かでないのに下流 %s を Apply した", d.On, wr.ID)
				}
			}
			if wr.Op == "delete" {
				destructive++
				if res.Ensure != core.EnsureAbsent {
					fail("宣言にある %s を消した", wr.ID)
				}
			} else if k == obsDiff && res.Fields["zone"] != nil {
				destructive++ // 作り直し
			}
			continue
		}
		// unmanaged を消してよい条件（不変条件 6・7・9）。
		destructive++
		switch {
		case wr.Op != "delete":
			fail("unmanaged %s に apply した", wr.ID)
		case !rg.Prune:
			fail("prune: false の Range で %s を消した", wr.ID)
		case w.listBroken:
			fail("List が不完全なのに %s を消した", wr.ID)
		case w.hidden[wr.ID]:
			fail("List に出ない %s を消した", wr.ID)
		case strings.HasPrefix(wr.ID.Name, "ext-") && len(rg.ExternalOwners) > 0:
			fail("external owner の %s を消した", wr.ID)
		case k != obsSame && k != obsDiff:
			fail("観測が Present でない %s を消した", wr.ID)
		case wr.ID.Kind() != "rec":
			fail("消せない type の %s を消した", wr.ID)
		case !hasBase:
			fail("前の cycle の件数が無いのに %s を消した", wr.ID)
		case dropped:
			fail("急減（%d → %d、max_drop %d%%）なのに %s を消した", base, listed, rg.MaxDrop, wr.ID)
		}
	}

	// 不変条件 20：budget を超えない。budget で halt した wave は 1 件も書かない（超過分だけを
	// 保留しない）。Apply すると決めたものは全部 Apply する。前の wave で Apply したものは、同じ cycle の
	// 再観測（E-J7）の wave が budget で halt しても残る。
	budgetHalt := false
	for _, hl := range r.Halts {
		budgetHalt = budgetHalt || strings.HasPrefix(hl.Scope, "policy ")
	}
	for _, wr := range writes {
		if c := find(r, wr.ID); c.Action != plan.ActApply {
			fail("%s（%s）に書いた", wr.ID, c.Action)
		}
	}
	if budgetHalt {
		for _, c := range r.Changes {
			if c.Action == plan.ActApply && c.Wave == 0 {
				fail("budget で halt した cycle に、Apply せずに残した %s がある", c.Resource)
			}
		}
	}
	applied := 0
	for _, c := range r.Changes {
		if c.Action == plan.ActApply {
			applied++
		}
	}
	if applied != len(writes) {
		fail("Apply すると決めた %d 件のうち %d 件だけ書いた", applied, len(writes))
	}
	if len(h.in.Policies) == 1 && h.in.Policies[0].Budget != nil {
		b := h.in.Policies[0].Budget
		if b.PerCycle != core.Unlimited && len(writes) > b.PerCycle {
			fail("per_cycle %d を超えて %d 件書いた", b.PerCycle, len(writes))
		}
		if b.DestructivePerCycle != core.Unlimited && destructive > b.DestructivePerCycle {
			fail("destructive_per_cycle %d を超えて %d 件壊した", b.DestructivePerCycle, destructive)
		}
		if b.DestructivePerWindow != core.Unlimited {
			used := prev.Window("b", b.Window, h.clock.t).Destructive
			if used+destructive > b.DestructivePerWindow {
				fail("window の destructive_per_window %d を超えた（%d+%d）", b.DestructivePerWindow, used, destructive)
			}
		}
	}
	// report の Policy に該当する書き込みは無い。
	if len(h.in.Policies) == 1 && h.in.Policies[0].Mode == core.ModeReport && len(writes) > 0 {
		for _, wr := range writes {
			if wr.ID.Kind() == "rec" {
				fail("report なのに %s に書いた", wr.ID)
			}
		}
	}
}

// checkIneffective は不変条件 14 を確かめる：同じ宣言の Resource に、ineffective
// Apply を上限（既定 3）を超えて続けない。効いた・宣言どおりだった cycle に数え直す。
// 観測が確かでない cycle は数えも戻しもしない。
func checkIneffective(t *testing.T, cyc int, h *harness, w world, declared map[core.ID]*core.Resource, count map[core.ID]int) {
	t.Helper()
	applied := map[core.ID]bool{}
	for _, wr := range h.p.Writes() {
		if wr.Op == "apply" {
			applied[wr.ID] = true
		}
	}
	ineff := map[core.ID]bool{}
	for _, id := range w.ineffective {
		ineff[id] = true
	}
	for id, r := range declared {
		k := w.obs[id]
		switch {
		case applied[id] && ineff[id]:
			count[id]++
			if count[id] > core.DefaultMaxIneffectiveApplies {
				t.Fatalf("cycle %d: %s に ineffective Apply を %d 回続けた", cyc, id, count[id])
			}
		case applied[id]:
			count[id] = 0
		case w.observeErr || k == obsUnknown || k == obsConverging || k == obsOmit:
		case k == obsSame && r.Ensure == core.EnsurePresent:
			count[id] = 0
		case k == obsAbsent && r.Ensure == core.EnsureAbsent:
			count[id] = 0
		}
	}
}

type failSink struct{ fail bool }

func (s failSink) Deliver(context.Context, []core.Event) error {
	if s.fail {
		return errors.New("届かない")
	}
	return nil
}

// checkEvents は不変条件 28・29 を確かめる。
func checkEvents(t *testing.T, cyc int, w world, r plan.Result) {
	t.Helper()
	if len(r.Errors) > 0 {
		t.Fatalf("cycle %d: cycle のエラー %v", cyc, r.Errors)
	}
	if len(r.Events) == 0 || r.Events[len(r.Events)-1].Kind != "CycleDone" {
		t.Fatalf("cycle %d: CycleDone が無い: %+v", cyc, r.Events)
	}
	for _, e := range r.Events {
		if e.Type != core.StateEvent || e.State != core.Resolved {
			continue
		}
		if e.Resource == "" {
			if w.listBroken && strings.HasPrefix(e.Scope, "range ") {
				t.Fatalf("cycle %d: List が壊れた cycle に %s を resolved にした", cyc, e.Key())
			}
			continue
		}
		id, err := core.ParseRef(e.Resource)
		if err != nil {
			t.Fatal(err)
		}
		k, seen := w.obs[id]
		blind := w.observeErr || k == obsUnknown || k == obsConverging || k == obsOmit
		// unmanaged は完全な List に出なければ無いとみなす（List が集合の正、不変条件 8）。
		// 偽の provider の List は Present だけを返す。
		if e.Kind == "Unmanaged" {
			listed := !w.hidden[id] && (k == obsSame || k == obsDiff || k == obsOmit)
			blind = listed && (w.observeErr || k == obsOmit)
		}
		if seen && blind && e.Kind != "Unknown" && e.Kind != "Converging" {
			t.Fatalf("cycle %d: 観測できなかった %s を resolved にした", cyc, e.Key())
		}
		if w.listBroken && e.Kind == "Unmanaged" {
			t.Fatalf("cycle %d: List が壊れた cycle に unmanaged %s を resolved にした", cyc, e.Key())
		}
	}
}
