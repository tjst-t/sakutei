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

// Package cycle は 1 周だけ回す処理（設計 §2 の RunCycle）。周をまたいで覚える
// ものは全部 Record に置き、メモリには持たない（E-O3・N-O4）。だから一回実行
// （reconcile --once）でも常駐でも、同じ Record から同じ結果になる。
//
// 段 2 は §2.1 の段 0（ロック）・3〜6（観測・比べる・方針・当てる）・8（目印と
// 監査）と、当てたものの読み直しを行う。判定は plan.Decide と同じ（不変条件 30）。
// 段 3 で、段 8 の通知の状態・送り出しの列・sink への送り出し・健全性を足した。
package cycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/events"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/record"
)

// Input は 1 周の入力（読み込みと検証を通ったもの）。
type Input struct {
	Resources []core.Resource
	Ranges    []core.Range
	Policies  []core.Policy
	Providers map[string]core.Provider
	Record    *record.Record
	Clock     core.Clock // nil なら計算機の時計（E-R4）
	// Sinks は Event の送り先（つなぎ方の設定の sinks、E-H2）。
	Sinks map[string]core.Sink
	// StuckAfter は intent の cycle.stuck_after（E-H1）。
	StuckAfter int
}

// Run は 1 周回す。誤りを返すのはロックを試すこともできないときだけで、ほかの
// 誤り（Record・監査に書けない）は Result.Errors に入れる。
func Run(ctx context.Context, in Input) (plan.Result, error) {
	clock := in.Clock
	if clock == nil {
		clock = core.SystemClock{}
	}
	now := clock.Now()
	readers := make(map[string]core.Reader, len(in.Providers))
	for n, p := range in.Providers {
		readers[n] = p
	}
	pin := plan.Input{Resources: in.Resources, Ranges: in.Ranges, Policies: in.Policies, Readers: readers, Now: now}

	// 段 0：ロック。取れなければ進めない周：観測して比べるだけで、何も書かない
	// （E-M5・N-M7・N-S3）。
	release, held, err := in.Record.Lock()
	if err != nil {
		return plan.Result{}, fmt.Errorf("Record のロック: %w", err)
	}
	defer release()
	if !held {
		pin.Marks, _ = record.ReadMarks(in.Record.Dir())
		pin.ReportOnly = "ほかの処理が Record のロックを持つ"
		r := plan.Decide(ctx, pin)
		r.Verb, r.AdvancedElsewhere = "reconcile", true
		return r, nil
	}

	// 段 1：Record を読む。読めなければ全体を report に寄せ、目印を上書きしない
	// （前の周の件数・予算の窓を失わない、不変条件 7・20）。
	var errs []string
	marks, merr := in.Record.Marks()
	var audit *record.Audit
	if merr != nil {
		errs = append(errs, "Record の目印が読めない: "+merr.Error())
		pin.ReportOnly = "Record の目印が読めない"
	} else if audit, err = in.Record.Audit(); err != nil {
		errs = append(errs, "監査を開けない: "+err.Error())
		pin.ReportOnly = "監査に書けない"
	}
	pin.Marks = marks

	// 段 3〜5。
	r := plan.Decide(ctx, pin)
	r.Verb = "reconcile"
	if merr != nil {
		if _, err := in.Record.RecordCycle(now, strings.Join(errs, "; ")); err != nil {
			errs = append(errs, "健全性を書けない: "+err.Error())
		}
		r.Errors = errs
		return r, nil
	}

	var writes []int
	for i, c := range r.Changes {
		if c.Action == plan.ActApply {
			writes = append(writes, i)
		}
	}
	windows := nextWindows(r, marks)

	// 発射の前に予算の窓の使用量を書く。書けなければ当てない（途中で止まっても
	// 使った分を失わない、不変条件 20・21）。
	if len(writes) > 0 {
		pre := marks
		pre.BudgetWindows = windows
		if err := in.Record.WriteMarks(pre); err != nil {
			errs = append(errs, "Record に書けない: "+err.Error())
			block(&r, writes, "Record に書けないので当てない")
			writes = nil
		}
	}

	// 段 6：当てる。上流が済んでいない下流は Decide が wait にしたので、ここに
	// 来るものはどの順で当ててもよい。
	byID := make(map[core.ID]*core.Resource, len(in.Resources))
	for i := range in.Resources {
		byID[in.Resources[i].ID] = &in.Resources[i]
	}
	for n, i := range writes {
		c := &r.Changes[i]
		fire(ctx, c, in.Providers[c.ID.Provider()], byID[c.ID])
		if _, err := audit.Append(entry(c, now)); err != nil {
			errs = append(errs, "監査に書けない: "+err.Error())
			block(&r, writes[n+1:], "監査に書けないので当てない")
			writes = writes[:n+1]
			break
		}
	}

	// 当てたものを読み直す（観測しなかった値を「ずれ無し」と出さない、不変条件 29）。
	reread(ctx, &r, writes, readers, byID)

	// 段 8：目印を書く。
	gone := map[core.ID]bool{}
	for _, i := range writes {
		c := r.Changes[i]
		if c.Outcome == plan.OutcomeOK && (c.Op == plan.Delete || c.Op == plan.Prune) {
			gone[c.ID] = true
		}
		// 断られた削除は予算に数えない（不変条件 10）。
		if c.Outcome == plan.OutcomeRefused {
			for _, p := range c.Policies {
				if w, ok := windows[p]; ok && c.Destructive && w.Destructive > 0 {
					w.Destructive--
					windows[p] = w
				}
			}
		}
	}
	next := record.Marks{Cycle: marks.Cycle + 1, At: now, RangeCounts: nextCounts(r, marks, gone), BudgetWindows: windows}
	if err := in.Record.WriteMarks(next); err != nil {
		errs = append(errs, "Record に書けない: "+err.Error())
	}

	// 通知の状態を進めて送り出しの列に積み、sink へ送る。送れなくても周を
	// 止めない（不変条件 28・29）。
	if err := emit(&r, in, now, clock.Now().Sub(now)); err != nil {
		errs = append(errs, "通知の状態を書けない: "+err.Error())
	}
	r.Deliveries = events.Deliver(ctx, in.Record, in.Sinks)

	// 健全性（E-K11・N-H4）。
	if _, err := in.Record.RecordCycle(now, strings.Join(errs, "; ")); err != nil {
		errs = append(errs, "健全性を書けない: "+err.Error())
	}
	r.Errors = errs
	return r, nil
}

// emit は通知の状態を進める。番号は送り出しの列の最後と state.json の大きいほう
// の次から（state.json を書く前に止まっても番号を二度使わない）。
func emit(r *plan.Result, in Input, now time.Time, d time.Duration) error {
	st, err := in.Record.EventState()
	if err != nil {
		return err
	}
	last, err := in.Record.LastOutboxSeq()
	if err != nil {
		return err
	}
	out := events.Derive(events.Input{Result: *r, Prev: st.Firing, Seq: max(st.Seq, last), Now: now, Duration: d, StuckAfter: in.StuckAfter})
	if err := in.Record.AppendOutbox(out.Emitted); err != nil {
		return err
	}
	if err := in.Record.WriteEventState(record.EventState{Seq: out.Seq, Firing: out.Firing}); err != nil {
		return err
	}
	r.Events = out.Emitted
	return nil
}

// RecordLoadError は intent・設定を読めなかった周を健全性に残す（E-K11）。一回
// 実行と常駐が同じものを書く（不変条件 30）。ロックが取れなければ何もしない。
func RecordLoadError(rec *record.Record, clock core.Clock, loadErr error) error {
	if clock == nil {
		clock = core.SystemClock{}
	}
	release, held, err := rec.Lock()
	if err != nil {
		return err
	}
	defer release()
	if !held {
		return nil
	}
	_, err = rec.RecordCycle(clock.Now(), "読み込み: "+loadErr.Error())
	return err
}

// nextWindows は Policy ごとの予算の窓の、この周に当てる分を足した使用量。
func nextWindows(r plan.Result, marks record.Marks) map[string]record.WindowUse {
	out := map[string]record.WindowUse{}
	for _, u := range r.Budgets {
		if u.DestructivePerWindow == core.Unlimited {
			continue
		}
		fired := 0
		for _, c := range r.Changes {
			if c.Action == plan.ActApply && c.Destructive && contains(c.Policies, u.Policy) {
				fired++
			}
		}
		// u.WindowUsed は止める前に数えた分を含むので、それを引いて実際に当てる分を足す。
		out[u.Policy] = record.WindowUse{Start: u.WindowStart, Destructive: u.WindowUsed - u.Destructive + fired}
	}
	return out
}

// nextCounts は範囲ごとの件数の目印。List が完全な範囲は、List の件数から
// 自分が消した分を差し引く（E-K2・N-K2）。不完全な範囲・急減で止めた範囲は前の
// 値を保つ（止めた件数を新しい基準にしない）。
func nextCounts(r plan.Result, marks record.Marks, gone map[core.ID]bool) map[string]int {
	out := map[string]int{}
	for k, v := range marks.RangeCounts {
		out[k] = v
	}
	for _, st := range r.Ranges {
		if !st.Complete || (st.Dropped && st.Prune) {
			continue
		}
		n := st.Listed
		for _, id := range st.IDs {
			if gone[id] {
				n--
			}
		}
		out[st.Range] = n
	}
	return out
}

func block(r *plan.Result, idx []int, why string) {
	for _, i := range idx {
		c := &r.Changes[i]
		c.Action, c.Blocked = plan.ActReport, why
	}
}

// fire は 1 件を当てる。書き込みの文脈は Apply・Delete のときだけ渡す（E-G5）。
func fire(ctx context.Context, c *plan.Change, p core.Provider, res *core.Resource) {
	var err error
	switch c.Op {
	case plan.Create, plan.Update:
		err = p.Apply(ctx, core.WriteContext{}, c.ID, res.Fields, c.Current)
	case plan.Delete, plan.Prune:
		err = p.Delete(ctx, core.WriteContext{}, c.ID, c.Current)
	}
	var refuse *core.RefuseError
	switch {
	case err == nil:
		c.Outcome = plan.OutcomeOK
	case errors.As(err, &refuse):
		c.Outcome, c.Error = plan.OutcomeRefused, refuse.Reason
	default:
		c.Outcome, c.Error = plan.OutcomeError, err.Error()
	}
}

// entry は監査の 1 行。値は書かず、項目の名前だけ（E-L1・N-L1）。
func entry(c *plan.Change, now time.Time) record.Entry {
	e := record.Entry{At: now, Resource: c.Resource, Result: string(c.Outcome), Error: c.Error, Policies: c.Policies, Reason: c.Reason}
	switch c.Op {
	case plan.Create, plan.Update:
		e.Kind = "resource.apply"
		for _, f := range c.Fields {
			e.Fields = append(e.Fields, f.Name)
		}
	case plan.Delete:
		e.Kind = "resource.delete"
	case plan.Prune:
		e.Kind = "resource.prune"
	}
	return e
}

// reread は当てて成功したものを provider ごとに 1 回で読み直し、After を置く。
func reread(ctx context.Context, r *plan.Result, writes []int, readers map[string]core.Reader, byID map[core.ID]*core.Resource) {
	var ids []core.ID
	for _, i := range writes {
		if r.Changes[i].Outcome == plan.OutcomeOK {
			ids = append(ids, r.Changes[i].ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	obs := plan.Observe(ctx, ids, readers)
	for _, i := range writes {
		c := &r.Changes[i]
		if c.Outcome != plan.OutcomeOK {
			continue
		}
		o := obs[c.ID]
		if res := byID[c.ID]; res != nil {
			ks := readers[c.ID.Provider()].Schema().Kinds[c.ID.Kind()]
			a := plan.Compare(*res, ks, o)
			c.After, c.AfterNote = a.Op, a.Note
			continue
		}
		switch o.Availability {
		case core.Absent:
			c.After = plan.None
		case core.Present:
			c.After = plan.Prune
		case core.Converging:
			c.After, c.AfterNote = plan.Converging, o.Note
		default:
			c.After, c.AfterNote = plan.Unknown, o.Note
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
