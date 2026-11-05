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

package events_test

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/events"
	"github.com/tjst-t/sakutei/v3/plan"
)

var t0 = time.Date(2026, 11, 5, 22, 0, 0, 0, time.UTC)

// run は結果を順に Derive に通し、cycle ごとに積んだ Event を返す。
func run(stuckAfter int, results ...plan.Result) [][]core.Event {
	var prev []core.Event
	var seq uint64
	var out [][]core.Event
	for i, r := range results {
		o := events.Derive(events.Input{Result: r, Prev: prev, Seq: seq, Now: t0.Add(time.Duration(i) * time.Minute), StuckAfter: stuckAfter})
		prev, seq = o.Firing, o.Seq
		out = append(out, o.Emitted)
	}
	return out
}

func res(changes ...plan.Change) plan.Result { return plan.Result{Changes: changes} }

func ch(name string, op plan.Op) plan.Change { return plan.Change{Resource: "p/rec/" + name, Op: op} }

// states は level Event を "Kind 対象 state" の列にする。
func states(evs []core.Event) []string {
	var out []string
	for _, e := range evs {
		if e.Type == core.StateEvent {
			s := e.Key() + " " + e.State
			if e.Stuck {
				s += " stuck"
			}
			out = append(out, s)
		}
	}
	return out
}

func eq(t *testing.T, cyc int, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("cycle %d: %q（期待 %q）", cyc, got, want)
	}
}

// E-H1：始まりと終わりで 1 回ずつ。続いた cycle は数え、stuck_after で 1 回 stuck。
func TestFiringResolvedOnce(t *testing.T) {
	drift := ch("a", plan.Update)
	fixed := ch("a", plan.Update)
	fixed.Outcome, fixed.After = plan.OutcomeOK, plan.None
	got := run(3, res(drift), res(drift), res(drift), res(drift), res(fixed), res(ch("a", plan.None)))
	eq(t, 1, states(got[0]), "Drift p/rec/a firing")
	eq(t, 2, states(got[1]))
	eq(t, 3, states(got[2]), "Drift p/rec/a firing stuck")
	eq(t, 4, states(got[3]))
	eq(t, 5, states(got[4]), "Drift p/rec/a resolved")
	eq(t, 6, states(got[5]))
	// edge Event：Apply した cycle に Applied、cycle ごとに CycleDone。
	var kinds []string
	for _, e := range got[4] {
		kinds = append(kinds, e.Kind)
	}
	if strings.Join(kinds, ",") != "Drift,Applied,CycleDone" {
		t.Errorf("cycle 5: %v", kinds)
	}
	// 番号は cycle をまたいで続く。
	var last uint64
	for _, evs := range got {
		for _, e := range evs {
			if e.Seq != last+1 {
				t.Fatalf("番号が飛んだ: %d の次が %d", last, e.Seq)
			}
			last = e.Seq
		}
	}
	if r := got[4][0]; r.Count != 4 || r.Since == nil || !r.Since.Equal(t0) {
		t.Errorf("resolved の count・since: %+v", r)
	}
}

// 不変条件 29：確かめられなかった cycle（Unknown・Converging・List が不完全）に resolved に
// しない。count も進めない。
func TestNoResolveWithoutConfirmation(t *testing.T) {
	drift := ch("a", plan.Update)
	unk := ch("a", plan.Unknown)
	conv := ch("a", plan.Converging)
	got := run(0, res(drift), res(unk), res(conv), res(drift), res(ch("a", plan.None)))
	eq(t, 1, states(got[0]), "Drift p/rec/a firing")
	eq(t, 2, states(got[1]), "Unknown p/rec/a firing")
	eq(t, 3, states(got[2]), "Unknown p/rec/a resolved", "Converging p/rec/a firing")
	eq(t, 4, states(got[3]), "Converging p/rec/a resolved")
	eq(t, 5, states(got[4]), "Drift p/rec/a resolved")
	if c := got[4][0].Count; c != 2 {
		t.Errorf("確かめられなかった cycle を数えた: count=%d", c)
	}

	// unmanaged：Range の List が不完全な cycle では消えたと見ない。
	stray := plan.Change{Resource: "p/rec/s", Op: plan.Unmanaged, Range: "p:"}
	ok := plan.RangeStatus{Range: "p:", Complete: true}
	broken := plan.RangeStatus{Range: "p:", Note: "List が不完全"}
	got = run(0,
		plan.Result{Changes: []plan.Change{stray}, Ranges: []plan.RangeStatus{ok}},
		plan.Result{Ranges: []plan.RangeStatus{broken}},
		plan.Result{Ranges: []plan.RangeStatus{ok}},
	)
	eq(t, 1, states(got[0]), "Unmanaged p/rec/s firing")
	eq(t, 2, states(got[1]), "Unknown range p: firing")
	eq(t, 3, states(got[2]), "Unknown range p: resolved", "Unmanaged p/rec/s resolved")

	// 急減で halt した Range は、List が完全で急減しない cycle に消える。
	drop := plan.Result{Ranges: []plan.RangeStatus{{Range: "p:", Complete: true, Dropped: true, Prune: true}},
		Halts: []plan.Halt{{Scope: "range p:", Reason: "急減"}}}
	got = run(0, drop, plan.Result{Ranges: []plan.RangeStatus{broken}}, plan.Result{Ranges: []plan.RangeStatus{ok}})
	eq(t, 1, states(got[0]), "Halt range p: firing")
	eq(t, 2, states(got[1]), "Unknown range p: firing")
	eq(t, 3, states(got[2]), "Halt range p: resolved", "Unknown range p: resolved")
}

// reject された削除は、消す操作を試さなかった cycle には消えたと見ない。
func TestDeleteRefused(t *testing.T) {
	refused := ch("a", plan.Delete)
	refused.Outcome, refused.Error, refused.After = plan.OutcomeRefused, "保護", plan.Delete
	held := ch("a", plan.Delete)
	held.Action, held.Blocked = plan.ActHalt, "budget"
	got := run(0, res(refused), res(held), res(ch("a", plan.None)))
	eq(t, 1, states(got[0]), "DeleteRefused p/rec/a firing", "Drift p/rec/a firing")
	eq(t, 2, states(got[1]))
	eq(t, 3, states(got[2]), "DeleteRefused p/rec/a resolved", "Drift p/rec/a resolved")
}

// 不変条件 29 の性質：ランダムな結果の並びで、対象が Unknown・Converging の cycle、
// Range の List が不完全な cycle に、その対象の（Unknown・Converging 以外の）Event を resolved に
// しない。また、同じキーの firing は resolved を挟まずに 2 回積まない。
func TestPropertyNoResolveUnconfirmed(t *testing.T) {
	ops := []plan.Op{plan.None, plan.Update, plan.Create, plan.Unknown, plan.Converging, plan.Drift}
	for seed := int64(1); seed <= 300; seed++ {
		rng := rand.New(rand.NewSource(seed))
		var prev []core.Event
		var seq uint64
		open := map[string]bool{}
		for cyc := 0; cyc < 12; cyc++ {
			var r plan.Result
			unconfirmed := map[string]bool{}
			for i := 0; i < 4; i++ {
				if rng.Intn(6) == 0 {
					continue // 宣言から外した
				}
				c := ch(fmt.Sprint(i), ops[rng.Intn(len(ops))])
				if rng.Intn(4) == 0 {
					c.Action, c.Blocked = plan.ActWait, "上流"
				}
				if c.Op == plan.Unknown || c.Op == plan.Converging {
					unconfirmed[c.Resource] = true
				}
				r.Changes = append(r.Changes, c)
			}
			complete := rng.Intn(3) > 0
			r.Ranges = []plan.RangeStatus{{Range: "p:", Complete: complete}}
			if complete && rng.Intn(2) == 0 {
				r.Changes = append(r.Changes, plan.Change{Resource: "p/rec/stray", Op: plan.Unmanaged, Range: "p:"})
			}
			o := events.Derive(events.Input{Result: r, Prev: prev, Seq: seq, Now: t0, StuckAfter: 3})
			for _, e := range o.Emitted {
				if e.Type != core.StateEvent {
					continue
				}
				switch e.State {
				case core.Resolved:
					if unconfirmed[e.Resource] && e.Kind != events.KindUnknown && e.Kind != events.KindConverging {
						t.Fatalf("seed %d cycle %d: 確かめられない %s を resolved にした", seed, cyc, e.Key())
					}
					if !complete && (e.Resource == "p/rec/stray" || e.Scope == "range p:") {
						t.Fatalf("seed %d cycle %d: List が不完全な cycle に %s を resolved にした", seed, cyc, e.Key())
					}
					if !open[e.Key()] {
						t.Fatalf("seed %d cycle %d: firing でない %s を resolved にした", seed, cyc, e.Key())
					}
					delete(open, e.Key())
				case core.Firing:
					if open[e.Key()] && !e.Stuck {
						t.Fatalf("seed %d cycle %d: %s の firing を 2 回積んだ", seed, cyc, e.Key())
					}
					open[e.Key()] = true
				}
			}
			for _, f := range o.Firing {
				if !open[f.Key()] {
					t.Fatalf("seed %d cycle %d: 積んでいない %s が firing の一覧にある", seed, cyc, f.Key())
				}
			}
			if len(o.Firing) != len(open) {
				t.Fatalf("seed %d cycle %d: firing の一覧 %d と積んだ firing %d が合わない", seed, cyc, len(o.Firing), len(open))
			}
			prev, seq = o.Firing, o.Seq
		}
	}
}
