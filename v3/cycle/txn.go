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

package cycle

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/graph"
	"github.com/tjst-t/sakutei/v3/plan"
)

// TxnPoll は 2 段の Apply の確かめを繰り返す間隔（E-B3）。
const TxnPoll = time.Second

// runTxn は 2 段の Apply の単位（group か宣言 1 つ）を当てる（§3.3 の Transactor、
// E-B3・E-B8・E-B9・N-B2・N-B3・N-D5）。
//
//  1. provider ごとに Prepare する（相手の側に timeout の後に戻す仕掛けを置いて
//     当てる）。1 つでも準備できなければ、準備したものを戻して確定しない。
//  2. settle を待ち、timeout の半分まで、全部の宣言を読み直して宣言どおりで、
//     全部の after の確かめが真になるのを待つ。
//  3. 全部が通ったときだけ Commit する。通らなければ Commit を呼ばず（Rollback は
//     急ぐためだけ）、相手の時間切れでも戻る（不変条件 17）。
func runTxn(ctx context.Context, in Input, clock core.Clock, r *plan.Result, unit []int, byID map[core.ID]*core.Resource, g *graph.Graph) {
	key := byID[r.Changes[unit[0]].ID].TxnKey()
	timeout := time.Duration(0)
	byProvider := map[string][]core.Change{}
	members := map[string][]int{}
	for _, i := range unit {
		c := &r.Changes[i]
		c.Txn = key
		res := byID[c.ID]
		if t := res.Txn().Timeout; timeout == 0 || t < timeout {
			timeout = t
		}
		ch := core.Change{ID: c.ID, Current: c.Current}
		if c.Op == plan.Delete || c.Op == plan.Prune {
			ch.Delete = true
		} else {
			ch.Desired = res.Fields
		}
		name := c.ID.Provider()
		byProvider[name] = append(byProvider[name], ch)
		members[name] = append(members[name], i)
	}
	if timeout <= 0 {
		timeout = core.DefaultTxnTimeout
	}
	names := make([]string, 0, len(byProvider))
	for n := range byProvider {
		names = append(names, n)
	}
	sort.Strings(names)

	type prepared struct {
		name string
		txn  core.Txn
	}
	var txns []prepared
	rollbackAll := func() []string {
		var notes []string
		for _, t := range txns {
			if err := t.txn.Rollback(ctx); err != nil {
				notes = append(notes, t.name+" の Rollback: "+err.Error()+"（相手の時間切れで戻る）")
			}
		}
		return notes
	}
	mark := func(idx []int, outcome plan.Outcome, msg string, rolled bool) {
		for _, i := range idx {
			c := &r.Changes[i]
			c.Outcome, c.Error, c.RolledBack = outcome, msg, rolled
		}
	}

	// 1. 準備。
	for _, name := range names {
		tr, ok := in.Providers[name].(core.Transactor)
		var t core.Txn
		var err error
		if ok {
			t, err = tr.Prepare(ctx, core.WriteContext{}, byProvider[name], core.TxnOptions{Timeout: timeout})
		}
		if !ok || err != nil {
			why := "provider " + name + " は 2 段の Apply を持たない"
			if err != nil {
				why = "準備に失敗: " + err.Error()
			}
			notes := rollbackAll()
			mark(unit, plan.OutcomeError, joinNotes(key+" を確定しない（"+name+" を準備できない）", notes), false)
			mark(members[name], plan.OutcomeError, why, false)
			return
		}
		txns = append(txns, prepared{name, t})
	}

	// 2. 確かめ。全部が宣言どおりで、全部の after の確かめが真のときだけ通る。
	start := clock.Now()
	half := timeout / 2
	wait(ctx, in.Sleep, unitSettle(r, unit, in.Providers))
	rounds := int(half / TxnPoll)
	var why string
	for round := 0; ; round++ {
		var ok bool
		ok, why = confirmUnit(ctx, in, r, unit, byID, g)
		if ok {
			break
		}
		if round >= rounds || ctx.Err() != nil || clock.Now().Sub(start) >= half {
			// 3. 確かめられなかった：確定しない。
			notes := rollbackAll()
			mark(unit, plan.OutcomeError, joinNotes(key+" を "+half.String()+" のうちに確かめられなかった: "+why, notes), true)
			return
		}
		wait(ctx, in.Sleep, TxnPoll)
	}

	// 3. 確定。途中で切れた provider の分は分からない（相手の時間切れで戻るかも
	// しれない）ので失敗にする。
	for _, t := range txns {
		if err := t.txn.Commit(ctx); err != nil {
			mark(members[t.name], plan.OutcomeError, "確定に失敗（相手の時間切れで戻るかもしれない）: "+err.Error(), false)
			continue
		}
		mark(members[t.name], plan.OutcomeOK, "", false)
	}
}

// confirmUnit は単位の全部の宣言を読み直し、宣言どおりかと after の確かめを
// 見る（E-B3・E-B9・N-B9）。
func confirmUnit(ctx context.Context, in Input, r *plan.Result, unit []int, byID map[core.ID]*core.Resource, g *graph.Graph) (bool, string) {
	var ids []core.ID
	for _, i := range unit {
		id := r.Changes[i].ID
		ids = append(ids, id)
		ids = append(ids, plan.ConfirmRefs(*byID[id], core.ConfirmAfter, g)...)
	}
	readers := make(map[string]core.Reader, len(in.Providers))
	for n, p := range in.Providers {
		readers[n] = p
	}
	obs := plan.Observe(ctx, ids, readers)
	var failed []string
	for _, i := range unit {
		id := r.Changes[i].ID
		res := byID[id]
		ks := readers[id.Provider()].Schema().Kinds[id.Kind()]
		if a := plan.Compare(*res, ks, obs[id]); a.Op != plan.None {
			msg := id.String() + " が宣言どおりでない（" + string(a.Op)
			if a.Note != "" {
				msg += ": " + a.Note
			}
			failed = append(failed, msg+"）")
			continue
		}
		if ok, why := plan.Confirmed(ctx, *res, core.ConfirmAfter, g, obs, in.Checks); !ok {
			failed = append(failed, id.String()+" の confirm（after）: "+why)
		}
	}
	return len(failed) == 0, strings.Join(failed, "・")
}

// unitSettle は単位の種類の Settle の最も長いもの（E-J6・E-R3）。
func unitSettle(r *plan.Result, unit []int, providers map[string]core.Provider) time.Duration {
	var d time.Duration
	for _, i := range unit {
		id := r.Changes[i].ID
		if ks := providers[id.Provider()].Schema().Kinds[id.Kind()]; ks.Settle > d {
			d = ks.Settle
		}
	}
	return d
}

func joinNotes(msg string, notes []string) string {
	if len(notes) == 0 {
		return msg
	}
	return msg + "（" + strings.Join(notes, "・") + "）"
}

// wait は d だけ待つ（sleep があればそれで、E-R4）。
func wait(ctx context.Context, sleep func(context.Context, time.Duration), d time.Duration) {
	if d <= 0 {
		return
	}
	if sleep != nil {
		sleep(ctx, d)
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
