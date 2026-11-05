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
	"math/rand"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/condition"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/sdk/fake"
)

// txnHarness は two-phase Apply の筋書き：chain を変えると管理の経路（probe）が
// 塞がるかもしれない。sleep は時計を進める（E-R4）。
func txnHarness(t *testing.T) *harness {
	h := newHarness(t)
	h.in.Ranges = nil
	h.in.Sleep = func(_ context.Context, d time.Duration) { h.clock.t = h.clock.t.Add(d) }
	h.p.Set(rid("rec", "probe"), present(core.Fields{"value": "up"}))
	return h
}

func txnDecl(name, value, group string, confirms ...core.Confirm) core.Resource {
	r := decl(name, value)
	r.Apply = &core.ApplyOptions{Transaction: &core.TxnSpec{Group: group, Timeout: 10 * time.Second, Confirm: confirms}}
	return r
}

func after(expr string) core.Confirm {
	return core.Confirm{Phase: core.ConfirmAfter, When: condition.MustParse(expr)}
}

func before(expr string) core.Confirm {
	return core.Confirm{Phase: core.ConfirmBefore, When: condition.MustParse(expr)}
}

const probeUp = `observe(p/rec/probe).value == "up"`

// lockout は Prepare で Apply した chain が管理の経路を塞ぐ（probe を down にする）注入。
func lockout(h *harness, only core.ID) {
	h.p.OnPrepare(func(changes []core.Change) {
		for _, ch := range changes {
			if ch.ID == only {
				h.p.Set(rid("rec", "probe"), present(core.Fields{"value": "down"}))
			}
		}
	})
}

func value(h *harness, id core.ID) any {
	obs, _ := h.p.Observe(context.Background(), []core.ID{id})
	return obs[id].Fields["value"]
}

// check が通れば Commit する（E-B3）。
func TestTxnCommitsWhenConfirmed(t *testing.T) {
	h := txnHarness(t)
	h.p.Set(rid("rec", "chain"), present(core.Fields{"value": "old"}))
	h.in.Resources = []core.Resource{txnDecl("chain", "new", "", after(probeUp))}
	r := h.run()
	txns := h.p.Txns()
	if len(txns) != 1 || txns[0].State != fake.TxnCommitted || txns[0].Timeout != 10*time.Second {
		t.Fatalf("txns: %+v", txns)
	}
	c := find(r, rid("rec", "chain"))
	if c.Outcome != plan.OutcomeOK || c.After != plan.None || c.Txn != "resource p/rec/chain" || cli.ExitCode(r) != cli.ExitOK {
		t.Fatalf("%+v exit=%d", c, cli.ExitCode(r))
	}
	if h.p.Calls().Apply != 0 {
		t.Fatal("two-phase の宣言に Apply を呼んだ")
	}
}

// 不変条件 17：check が通らなければ Commit せずに戻し、同じ宣言のまま Apply し直さない。
func TestTxnRollsBackWhenNotConfirmed(t *testing.T) {
	h := txnHarness(t)
	h.p.Set(rid("rec", "chain"), present(core.Fields{"value": "old"}))
	h.in.Resources = []core.Resource{txnDecl("chain", "new", "", after(probeUp))}
	lockout(h, rid("rec", "chain"))
	start := h.clock.t
	r := h.run()
	txns := h.p.Txns()
	if len(txns) != 1 || txns[0].State != fake.TxnRolledBack {
		t.Fatalf("txns: %+v", txns)
	}
	if v := value(h, rid("rec", "chain")); v != "old" {
		t.Fatalf("chain が戻っていない: %v", v)
	}
	if v := value(h, rid("rec", "probe")); v != "up" {
		t.Fatalf("probe が戻っていない: %v", v)
	}
	// check は timeout の半分まで。
	if d := h.clock.t.Sub(start); d < 5*time.Second || d > 7*time.Second {
		t.Fatalf("check した長さ %s", d)
	}
	c := find(r, rid("rec", "chain"))
	if !c.RolledBack || c.Outcome != plan.OutcomeError || cli.ExitCode(r) != cli.ExitHalted {
		t.Fatalf("%+v exit=%d", c, cli.ExitCode(r))
	}
	// 次の cycle は Apply し直さない。
	r = h.run()
	if len(h.p.Txns()) != 1 {
		t.Fatalf("同じ宣言で Apply し直した: %+v", h.p.Txns())
	}
	if c := find(r, rid("rec", "chain")); c.Action != plan.ActHalt {
		t.Fatalf("2 cycle 目: %+v", c)
	}
	// 宣言を直せば Apply する。
	h.p.OnPrepare(nil)
	h.in.Resources = []core.Resource{txnDecl("chain", "new2", "", after(probeUp))}
	r = h.run()
	if got := h.p.Txns(); len(got) != 2 || got[1].State != fake.TxnCommitted || cli.ExitCode(r) != cli.ExitOK {
		t.Fatalf("直した cycle: %+v exit=%d", got, cli.ExitCode(r))
	}
}

// 不変条件 17：Commit を呼ばない限り相手の側で戻る（偽の相手の時間切れ）。
func TestTxnExpiresWithoutCommit(t *testing.T) {
	p := fake.New(schema)
	id := rid("rec", "chain")
	p.Set(id, present(core.Fields{"value": "old"}))
	txn, err := p.Prepare(context.Background(), core.WriteContext{}, []core.Change{{ID: id, Desired: core.Fields{"value": "new"}}}, core.TxnOptions{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := txn.(*fake.Txn).Expire(); err != nil {
		t.Fatal(err)
	}
	obs, _ := p.Observe(context.Background(), []core.ID{id})
	if obs[id].Fields["value"] != "old" {
		t.Fatalf("戻っていない: %+v", obs[id])
	}
}

// group は全部で check が通ったときだけ Commit し、1 つでも落ちればどれも Commit しない
// （E-B9・N-B3）。
func TestTxnGroupAllOrNothing(t *testing.T) {
	h := txnHarness(t)
	h.p.Set(rid("rec", "a"), present(core.Fields{"value": "old"}))
	h.p.Set(rid("rec", "b"), present(core.Fields{"value": "old"}))
	a := txnDecl("a", "new", "wave1")
	b := txnDecl("b", "new", "wave1", after(probeUp))
	h.in.Resources = []core.Resource{a, b}
	lockout(h, a.ID) // a が塞ぎ、check するのは b
	r := h.run()
	if got := h.p.Txns(); len(got) != 1 || got[0].State != fake.TxnRolledBack || len(got[0].Changes) != 2 {
		t.Fatalf("txns: %+v", got)
	}
	for _, id := range []core.ID{a.ID, b.ID} {
		if c := find(r, id); !c.RolledBack || c.Txn != "group wave1" {
			t.Fatalf("%s: %+v", id, c)
		}
		if v := value(h, id); v != "old" {
			t.Fatalf("%s が戻っていない: %v", id, v)
		}
	}

	// 1 つが Prepare できなければ、ほかも Commit しない。
	h2 := txnHarness(t)
	h2.in.Resources = []core.Resource{txnDecl("a", "new", "w"), txnDecl("b", "new", "w")}
	h2.p.FailPrepare(rid("rec", "a"), errors.New("nft -c に落ちた"))
	r = h2.run()
	if len(h2.p.Txns()) != 0 {
		t.Fatalf("txns: %+v", h2.p.Txns())
	}
	for _, n := range []string{"a", "b"} {
		if c := find(r, rid("rec", n)); c.Outcome != plan.OutcomeError || c.RolledBack {
			t.Fatalf("%s: %+v", n, c)
		}
	}

	// group の 1 つが待つなら、残りも Apply しない。
	h3 := txnHarness(t)
	w := txnDecl("w", "new", "g")
	w.DependsOn = []core.Dependency{{When: condition.MustParse(`observe(p/rec/probe).value == "never"`)}}
	h3.in.Resources = []core.Resource{w, txnDecl("x", "new", "g")}
	r = h3.run()
	if c := find(r, rid("rec", "x")); c.Action != plan.ActWait || len(h3.p.Txns()) != 0 {
		t.Fatalf("x: %+v txns=%v", c, h3.p.Txns())
	}
}

// before の check が通らなければ変えない（E-B3・N-B9）。
func TestTxnBeforeMustHold(t *testing.T) {
	h := txnHarness(t)
	h.p.Set(rid("rec", "probe"), present(core.Fields{"value": "down"}))
	h.in.Resources = []core.Resource{txnDecl("chain", "new", "", before(probeUp), after(probeUp))}
	r := h.run()
	if c := find(r, rid("rec", "chain")); c.Action != plan.ActWait || len(h.p.Txns()) != 0 || cli.ExitCode(r) != cli.ExitRemaining {
		t.Fatalf("%+v txns=%v exit=%d", c, h.p.Txns(), cli.ExitCode(r))
	}
	h.p.Set(rid("rec", "probe"), present(core.Fields{"value": "up"}))
	r = h.run()
	if got := h.p.Txns(); len(got) != 1 || got[0].State != fake.TxnCommitted || cli.ExitCode(r) != cli.ExitOK {
		t.Fatalf("%+v exit=%d", got, cli.ExitCode(r))
	}
}

// 不変条件 17（性質）：Prepare の失敗・締め出し・効かない変更をランダムに注入しても、
// check が通らない単位は Commit せず、どの単位も「全部宣言どおり」か「全部元のまま」
// で終わる。
func TestPropertyTxnNeverCommitsUnconfirmed(t *testing.T) {
	rng := rand.New(rand.NewSource(17))
	iters := 200
	if testing.Short() || raceEnabled {
		iters = 40
	}
	for it := 0; it < iters; it++ {
		h := txnHarness(t)
		n := 1 + rng.Intn(4)
		group := ""
		if rng.Intn(2) == 0 {
			group = "g"
		}
		var ids []core.ID
		breaks, fails, inert := false, false, false
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("r%d", i)
			id := rid("rec", name)
			ids = append(ids, id)
			h.p.Set(id, present(core.Fields{"value": "old"}))
			h.in.Resources = append(h.in.Resources, txnDecl(name, "new", group, after(probeUp)))
			switch rng.Intn(6) {
			case 0:
				lockout(h, id)
				breaks = true
			case 1:
				h.p.FailPrepare(id, errors.New("injected"))
				fails = true
			case 2:
				h.p.SetIneffective(id, true)
				inert = true
			}
		}
		h.run()
		for _, txn := range h.p.Txns() {
			if txn.State != fake.TxnCommitted {
				continue
			}
			for _, ch := range txn.Changes {
				if v := value(h, ch.ID); v != "new" {
					t.Fatalf("iter %d: Commit したのに %s が %v", it, ch.ID, v)
				}
			}
			if v := value(h, rid("rec", "probe")); v != "up" {
				t.Fatalf("iter %d: check が通らないのに Commit した", it)
			}
		}
		if group != "" {
			news := 0
			for _, id := range ids {
				if value(h, id) == "new" {
					news++
				}
			}
			if news != 0 && news != n {
				t.Fatalf("iter %d: group の一部だけが変わった（%d/%d）", it, news, n)
			}
			if (breaks || fails || inert) && news != 0 {
				t.Fatalf("iter %d: 落ちた group が変わった", it)
			}
		}
	}
}
