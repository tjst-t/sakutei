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
// 段 4 で、段 6 の波（上流が効いたら下流を同じ周に観測し直して当てる、E-J7）と
// 段 7 の settle・効かない Apply の回数（不変条件 14）を足した。
package cycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/alloc"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/events"
	"github.com/tjst-t/sakutei/v3/graph"
	"github.com/tjst-t/sakutei/v3/load"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/procedure"
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
	// Checks は Condition の check(名前) の確かめ（E-B9）。
	Checks map[string]core.CheckFunc
	// Sleep は Apply のあとの settle を待つ（E-J6・E-R3）。nil なら時計で待つ。
	// テストで差し替える（E-R4）。
	Sleep func(ctx context.Context, d time.Duration)
	// Procedures は一回きりの手順（§3.7）。
	Procedures []core.Procedure
	// Daemon は常駐の周か。一回実行は critical の段を進めない（§2.2、不変条件 16）。
	Daemon bool
	// Unknown は読み込み（Expand・秘密の世代・採番）が「分からない」にした宣言 →
	// 理由。観測を試みず Unknown として扱う（E-G4・E-K3・不変条件 5）。
	Unknown map[core.ID]string
	// Incomplete は宣言が欠けているかもしれない provider → 理由。その範囲は
	// 消さない（E-K3・不変条件 26）。
	Incomplete map[string]string
	// Secrets は書き込みの文脈で秘密の値を取り出す保管先（E-G2）。
	Secrets core.SecretResolver
	// IntentDir と Allocs は、ロックを持って当てる前に書く採番（N-B13）。
	IntentDir string
	Allocs    map[string]alloc.Write
	// InputCounts は届いた外部入力の件数（目印に書く、E-K3）。
	InputCounts map[string]int
}

// readersOf は provider の観測の口。Unknown の宣言は provider に問わずに Unknown を
// 返す（依存が使えない間は観測を試みない、不変条件 5）。
func readersOf(in Input) map[string]core.Reader {
	readers := make(map[string]core.Reader, len(in.Providers))
	for n, p := range in.Providers {
		readers[n] = p
	}
	if len(in.Unknown) == 0 {
		return readers
	}
	for n, p := range in.Providers {
		readers[n] = unknownReader{Reader: p, unknown: in.Unknown}
	}
	return readers
}

type unknownReader struct {
	core.Reader
	unknown map[core.ID]string
}

func (u unknownReader) Observe(ctx context.Context, ids []core.ID) (map[core.ID]core.Observation, error) {
	var ask []core.ID
	out := map[core.ID]core.Observation{}
	for _, id := range ids {
		if why, ok := u.unknown[id]; ok {
			out[id] = core.Observation{Availability: core.Unknown, Note: why}
		} else {
			ask = append(ask, id)
		}
	}
	if len(ask) == 0 {
		return out, nil
	}
	got, err := u.Reader.Observe(ctx, ask)
	if err != nil {
		return nil, err
	}
	for id, o := range got {
		out[id] = o
	}
	return out, nil
}

// 同じ周の観測し直しの上限（§3.5、E-J7）。上流の Apply が効いたら、待っていた・
// 観測できなかった下流をもう一度観測して当てる。
const (
	ReobserveRounds = 3
	ReobserveTime   = 2 * time.Minute
)

// Run は 1 周回す。誤りを返すのはロックを試すこともできないときだけで、ほかの
// 誤り（Record・監査に書けない）は Result.Errors に入れる。
func Run(ctx context.Context, in Input) (plan.Result, error) {
	clock := in.Clock
	if clock == nil {
		clock = core.SystemClock{}
	}
	now := clock.Now()
	readers := readersOf(in)
	pin := plan.Input{Resources: in.Resources, Ranges: in.Ranges, Policies: in.Policies, Readers: readers, Now: now, Checks: in.Checks,
		Incomplete: in.Incomplete}

	// 段 0：ロック。取れなければ進めない周：観測して比べるだけで、何も書かない
	// （E-M5・N-M7・N-S3）。
	release, held, err := in.Record.Lock()
	if err != nil {
		return plan.Result{}, fmt.Errorf("Record のロック: %w", err)
	}
	defer release()
	if !held {
		marks, _ := record.ReadMarks(in.Record.Dir())
		r := Plan(ctx, in, in.Record.Dir(), marks, "ほかの処理が Record のロックを持つ")
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
	if merr != nil || audit == nil {
		r := Plan(ctx, in, in.Record.Dir(), marks, pin.ReportOnly)
		r.Verb = "reconcile"
		if _, err := in.Record.RecordCycle(now, strings.Join(errs, "; ")); err != nil {
			errs = append(errs, "健全性を書けない: "+err.Error())
		}
		r.Errors = append(r.Errors, errs...)
		return r, nil
	}

	// 段 2 の続き：選んだ採番の値を、当てる前に intent のファイルと写しに書く。
	// 書けなかった値に依る宣言はこの周 Unknown（二度払わない、不変条件 11）。
	if len(in.Allocs) > 0 {
		failed := alloc.Persist(in.IntentDir, in.Record, func(pool, key string, v int64) error {
			res, field := splitAllocKey(key)
			_, err := audit.Append(record.Entry{At: now, Kind: "alloc.assign", Resource: res, Fields: []string{field}, Pool: pool, Value: &v, Result: "ok"})
			return err
		}, in.Allocs)
		if len(failed) > 0 {
			unknown := make(map[core.ID]string, len(in.Unknown)+len(failed))
			for id, why := range in.Unknown {
				unknown[id] = why
			}
			for key, why := range failed {
				res, field := splitAllocKey(key)
				if id, err := core.ParseRef(res); err == nil {
					unknown[id] = fmt.Sprintf("項目 %q の採番: %s", field, why)
				}
				errs = append(errs, fmt.Sprintf("採番 %s: %s", key, why))
			}
			in.Unknown = unknown
			readers = readersOf(in)
			pin.Readers = readers
		}
	}

	// 段 1：一時的な Policy・承認・手順の進みを読み、期限切れ・終えたもの・新しい
	// 判断を監査に残す（不変条件 21・22）。
	st := readState(in.Record.Dir(), now, in.Policies, &stateExec{rec: in.Record, audit: audit})
	errs = append(errs, st.errs...)
	handover, heldTargets := procedure.Prepare(in.Procedures, in.Resources, st.runs)
	pin.Policies, pin.Approvals, pin.Handover, pin.Held = st.policies, st.approvals, handover, heldTargets
	if st.reportOnly != "" {
		pin.ReportOnly = st.reportOnly
	}

	// 段 3〜5。
	r := plan.Decide(ctx, pin)
	r.Verb = "reconcile"
	r.Temporary = st.temporary

	// 段 6・7：波ごとに当てて読み直す。上流が効いたら、待っていた・観測できなかった
	// 下流を同じ周にもう一度観測して当てる（E-J7）。同じ Resource は 1 周に 1 回
	// だけ当てる（不変条件 12）。上流が失敗・未確認なら下流は次の周（E-D6）。
	converged := plan.Strip(in.Resources, handover)
	g := graph.New(converged)
	byID := make(map[core.ID]*core.Resource, len(converged))
	for i := range converged {
		byID[converged[i].ID] = &converged[i]
	}
	base := baseWindows(r)
	firedDestructive := map[string]int{} // Policy → 当てた壊す変更（断られた削除を除く）
	used := map[string]plan.BudgetUse{}  // Policy → この周に当てた数（次の波の予算）
	fired := map[core.ID]bool{}
	var allWrites []int
	for wave := 1; ; wave++ {
		var writes []int
		for i, c := range r.Changes {
			if c.Action == plan.ActApply && !fired[c.ID] {
				writes = append(writes, i)
			}
		}
		// 発射の前に予算の窓の使用量を書く。書けなければ当てない（途中で止まっても
		// 使った分を失わない、不変条件 20・21）。
		if len(writes) > 0 {
			pre := marks
			pre.BudgetWindows = windowsWith(base, firedDestructive, r, writes)
			if err := in.Record.WriteMarks(pre); err != nil {
				errs = append(errs, "Record に書けない: "+err.Error())
				block(&r, writes, "Record に書けないので当てない")
				writes = nil
			}
		}
		stop := false
		// 承認を使った印を発射の前に書く。書けなければ承認を使わない（不変条件 21）。
		useApprovals := func(idx []int) []int {
			var ok []int
			for _, i := range idx {
				c := &r.Changes[i]
				if c.Request != "" {
					if err := useApproval(in.Record, audit, st.approvals[c.Request], c, now); err != nil {
						errs = append(errs, "承認を使った印を書けない: "+err.Error())
						block(&r, []int{i}, "承認を使った印を Record に書けないので当てない")
						continue
					}
				}
				ok = append(ok, i)
			}
			return ok
		}
		// account は当てた 1 件を予算に数えて監査に残す。監査に書けなければ false。
		account := func(i int) bool {
			c := &r.Changes[i]
			c.Wave = wave
			fired[c.ID] = true
			for _, p := range c.Policies {
				u := used[p]
				u.Changes++
				if c.Destructive {
					u.Destructive++
					if c.Outcome != plan.OutcomeRefused {
						firedDestructive[p]++ // 断られた削除は窓に数えない（不変条件 10）
					}
				}
				used[p] = u
			}
			if _, err := audit.Append(entry(c, now)); err != nil {
				errs = append(errs, "監査に書けない: "+err.Error())
				return false
			}
			return true
		}
		plain, units := plan.TxnUnits(r, writes, byID)
		var done []int
		for n, i := range plain {
			if len(useApprovals([]int{i})) == 0 {
				continue
			}
			fire(ctx, &r.Changes[i], in.Providers[r.Changes[i].ID.Provider()], byID[r.Changes[i].ID], in.Secrets)
			done = append(done, i)
			if !account(i) {
				block(&r, plain[n+1:], "監査に書けないので当てない")
				for _, u := range units {
					block(&r, u, "監査に書けないので当てない")
				}
				units, stop = nil, true
				break
			}
		}
		// 2 段の Apply は単位（group か宣言）ごとに準備し、確かめが通ったときだけ
		// 確定する（不変条件 17）。
		for n, u := range units {
			u = useApprovals(u)
			if len(u) == 0 {
				continue
			}
			runTxn(ctx, in, clock, &r, u, byID, g)
			done = append(done, u...)
			for _, i := range u {
				if !account(i) {
					stop = true
				}
			}
			if stop {
				for _, rest := range units[n+1:] {
					block(&r, rest, "監査に書けないので当てない")
				}
				break
			}
		}
		writes = done
		// settle を待って読み直す（観測しなかった値を「ずれ無し」と出さない、
		// 不変条件 29。効いたかを見る、不変条件 14）。
		settle(ctx, in.Sleep, r, writes, readers)
		reread(ctx, &r, writes, readers, byID)
		allWrites = append(allWrites, writes...)

		if stop || ctx.Err() != nil || wave > ReobserveRounds || clock.Now().Sub(now) >= ReobserveTime {
			break
		}
		only := reobserve(r, g, writes, fired)
		if len(only) == 0 {
			break
		}
		sub := pin
		sub.Only, sub.Used = only, used
		merge(&r, plan.Decide(ctx, sub))
	}

	// 承認の依頼を書く（無い・使った・期限切れ・要件が変わった、E-L2）。
	for i := range r.Changes {
		c := &r.Changes[i]
		if c.NewRequest == nil {
			continue
		}
		if err := in.Record.WriteRequest(*c.NewRequest, c.Fresh); err != nil {
			errs = append(errs, "承認の依頼を書けない: "+err.Error())
			continue
		}
		if _, err := audit.Append(record.Entry{At: now, Kind: "approval.request", Request: c.Request, Resource: c.Resource,
			Policies: c.Policies, By: c.NewRequest.Applicant, Result: "ok"}); err != nil {
			errs = append(errs, "監査に書けない: "+err.Error())
		}
	}

	// 段 6：手順を進める（§3.7）。書く段は予算を周の変更と合わせて数える
	// （不変条件 20）。
	pin2 := procedure.Input{Procedures: in.Procedures, Resources: in.Resources, Readers: readers, Policies: st.policies,
		Runs: st.runs, Approvals: st.approvals, Changes: r.Changes, Now: now, Cycle: marks.Cycle + 1, Checks: in.Checks, Daemon: in.Daemon}
	if pin.ReportOnly == "" {
		cmds, err := in.Record.Commands()
		if err != nil {
			errs = append(errs, "頼み（commands/）が読めない: "+err.Error())
		}
		pin2.Exec = &procedure.Exec{Record: in.Record, Audit: audit, Providers: in.Providers, Commands: cmds,
			Reserve: func(names []string, destructive bool) string {
				return reserve(&r, st.policies, names, destructive, used, firedDestructive, marks, base, now, in.Record)
			}, Write: core.NewWriteContext(in.Secrets)}
	}
	pout := procedure.Run(ctx, pin2)
	r.Runs, r.Halts = pout.Runs, append(r.Halts, pout.Halts...)
	errs = append(errs, pout.Errors...)
	// 手順の段 policy はこの周に足したものも出す。
	if runs, err := in.Record.Runs(); err == nil {
		r.Temporary = append(r.Temporary, procedure.Temporary(runs)...)
	} else {
		errs = append(errs, "手順の進み（runs/）が読めない: "+err.Error())
	}

	// 段 8：目印を書く。
	gone := map[core.ID]bool{}
	for _, i := range allWrites {
		c := r.Changes[i]
		if c.Outcome == plan.OutcomeOK && (c.Op == plan.Delete || c.Op == plan.Prune) {
			gone[c.ID] = true
		}
	}
	windows := windowsWith(base, firedDestructive, r, nil)
	ineffective := countIneffective(&r, marks)
	next := record.Marks{Cycle: marks.Cycle + 1, At: now, RangeCounts: nextCounts(r, marks, gone), BudgetWindows: windows,
		Ineffective: ineffective, InputCounts: inputCounts(marks, in.InputCounts)}
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

// Plan は reconcile と同じ判定を何も書かずに出す（plan・ロックがほかにある周・
// Record が読めない周、不変条件 30）。dir が空なら Record なし。reportOnly が
// 空でなければ当てる操作を全部 report にする。
func Plan(ctx context.Context, in Input, dir string, marks record.Marks, reportOnly string) plan.Result {
	clock := in.Clock
	if clock == nil {
		clock = core.SystemClock{}
	}
	now := clock.Now()
	readers := readersOf(in)
	st := readState(dir, now, in.Policies, nil)
	handover, held := procedure.Prepare(in.Procedures, in.Resources, st.runs)
	if reportOnly == "" {
		reportOnly = st.reportOnly
	}
	r := plan.Decide(ctx, plan.Input{Resources: in.Resources, Ranges: in.Ranges, Policies: st.policies, Readers: readers,
		Marks: marks, Now: now, ReportOnly: reportOnly, Checks: in.Checks, Approvals: st.approvals, Handover: handover, Held: held,
		Incomplete: in.Incomplete})
	r.Temporary = st.temporary
	pout := procedure.Run(ctx, procedure.Input{Procedures: in.Procedures, Resources: in.Resources, Readers: readers,
		Policies: st.policies, Runs: st.runs, Approvals: st.approvals, Changes: r.Changes, Now: now, Checks: in.Checks, Daemon: in.Daemon})
	r.Runs, r.Halts = pout.Runs, append(r.Halts, pout.Halts...)
	r.Temporary = append(r.Temporary, procedure.Temporary(st.runs)...)
	r.Errors = append(r.Errors, st.errs...)
	return r
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

// baseWindows は Policy ごとの予算の窓の、この周より前の使用量（最初の判定の
// Budgets から）。
func baseWindows(r plan.Result) map[string]record.WindowUse {
	out := map[string]record.WindowUse{}
	for _, u := range r.Budgets {
		if u.DestructivePerWindow != core.Unlimited {
			out[u.Policy] = record.WindowUse{Start: u.WindowStart, Destructive: u.WindowUsed - u.Destructive}
		}
	}
	return out
}

// windowsWith は窓の使用量に、この周に当てた分と、これから当てる writes の壊す
// 変更を足す。
func windowsWith(base map[string]record.WindowUse, fired map[string]int, r plan.Result, writes []int) map[string]record.WindowUse {
	out := make(map[string]record.WindowUse, len(base))
	for p, w := range base {
		w.Destructive += fired[p]
		for _, i := range writes {
			if c := r.Changes[i]; c.Destructive && contains(c.Policies, p) {
				w.Destructive++
			}
		}
		out[p] = w
	}
	return out
}

// reobserve は、この波で効いた（当てて読み直すと宣言どおり）ものに依る・それが
// 依る宣言のうち、待っていたか観測できなかったものを返す（E-J7）。この周に当てた
// ものは入れない（不変条件 12）。
func reobserve(r plan.Result, g *graph.Graph, writes []int, fired map[core.ID]bool) map[core.ID]bool {
	effective := map[core.ID]bool{}
	for _, i := range writes {
		if c := r.Changes[i]; c.Outcome == plan.OutcomeOK && c.After == plan.None {
			effective[c.ID] = true
		}
	}
	if len(effective) == 0 {
		return nil
	}
	only := map[core.ID]bool{}
	for _, c := range r.Changes {
		if fired[c.ID] || g.Resource(c.ID) == nil || (c.Action != plan.ActWait && c.Op != plan.Unknown) {
			continue
		}
		for _, n := range append(g.Refs(c.ID), g.Dependents(c.ID)...) {
			if effective[n] {
				only[c.ID] = true
				break
			}
		}
	}
	return only
}

// merge は観測し直した判定を周の結果に入れる。予算はその判定が前の波の分と
// 合わせて数え直したもので置き換える。
func merge(r *plan.Result, sub plan.Result) {
	index := make(map[string]int, len(r.Changes))
	for i, c := range r.Changes {
		index[c.Resource] = i
	}
	for _, c := range sub.Changes {
		if i, ok := index[c.Resource]; ok {
			r.Changes[i] = c
		}
	}
	r.Halts = append(r.Halts, sub.Halts...)
	if len(sub.Budgets) > 0 {
		r.Budgets = sub.Budgets
	}
	r.Summary = map[plan.Op]int{}
	for _, c := range r.Changes {
		r.Summary[c.Op]++
	}
}

// settle は当てて成功した種類の Settle の最も長いものを待つ（E-J6・E-R3）。
func settle(ctx context.Context, sleep func(context.Context, time.Duration), r plan.Result, writes []int, readers map[string]core.Reader) {
	var d time.Duration
	for _, i := range writes {
		c := r.Changes[i]
		if c.Outcome != plan.OutcomeOK || c.Txn != "" {
			continue // 2 段の Apply は確かめの前に待った
		}
		if ks := readers[c.ID.Provider()].Schema().Kinds[c.ID.Kind()]; ks.Settle > d {
			d = ks.Settle
		}
	}
	wait(ctx, sleep, d)
}

// countIneffective は効かない Apply を数える（不変条件 14、E-J6・E-K10）。当てて
// 成功したのに読み直すとずれたままなら 1 つ足し、宣言どおりなら消す。読み直しが
// Unknown・作業中なら数えない（不変条件 4）。宣言が変われば数え直す。上限に
// 達したら止める。
func countIneffective(r *plan.Result, marks record.Marks) map[string]record.IneffectiveMark {
	out := map[string]record.IneffectiveMark{}
	for i := range r.Changes {
		c := &r.Changes[i]
		prev, had := marks.Ineffective[c.Resource]
		if had && prev.Desired != c.DesiredHash {
			had = false
		}
		switch {
		case c.RolledBack:
			// 確かめられず戻した 2 段の Apply は、同じ宣言のまま当て直さない
			// （不変条件 13・17。毎周締め出して戻すのを繰り返さない）。
			n := max(c.IneffectiveLimit, 1)
			if had && prev.Count+1 > n {
				n = prev.Count + 1
			}
			out[c.Resource] = record.IneffectiveMark{Count: n, Desired: c.DesiredHash}
			c.Ineffective = n
			r.Halts = append(r.Halts, plan.Halt{Scope: "resource " + c.Resource,
				Reason: "2 段の Apply を確かめられず確定しなかった（相手の側で元に戻る）。宣言を直すか現実を宣言どおりにするまで当て直さない"})
		case c.Outcome == plan.OutcomeOK:
			switch c.After {
			case plan.None:
			case plan.Unknown, plan.Converging:
				if had {
					out[c.Resource] = prev
				}
			default:
				n := 1
				if had {
					n = prev.Count + 1
				}
				out[c.Resource] = record.IneffectiveMark{Count: n, Desired: c.DesiredHash}
				c.Ineffective = n
				if c.IneffectiveLimit > 0 && n >= c.IneffectiveLimit {
					r.Halts = append(r.Halts, plan.Halt{Scope: "resource " + c.Resource,
						Reason: fmt.Sprintf("効かない Apply が %d 回続いた（max_ineffective_applies %d）。次の周から当てない", n, c.IneffectiveLimit)})
				}
			}
		case c.Final() == plan.None:
		default:
			if had {
				out[c.Resource] = prev
			}
		}
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
func fire(ctx context.Context, c *plan.Change, p core.Provider, res *core.Resource, secrets core.SecretResolver) {
	var err error
	w := core.NewWriteContext(secrets)
	switch c.Op {
	case plan.Create, plan.Update:
		err = p.Apply(ctx, w, c.ID, res.Fields, c.Current)
	case plan.Delete, plan.Prune:
		err = p.Delete(ctx, w, c.ID, c.Current)
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

// splitAllocKey は採番の鍵（"<type>/<id>#<項目>"）を宣言と項目に分ける。
func splitAllocKey(key string) (res, field string) {
	i := strings.LastIndex(key, "#")
	if i < 0 {
		return key, ""
	}
	return key[:i], key[i+1:]
}

// inputCounts は外部入力の件数の目印。届いたものは今の件数、欠けた・届かない
// ものは前の値を保つ（E-K3）。
func inputCounts(marks record.Marks, now map[string]int) map[string]int {
	if len(marks.InputCounts) == 0 && len(now) == 0 {
		return nil
	}
	out := make(map[string]int, len(marks.InputCounts)+len(now))
	for k, v := range marks.InputCounts {
		out[k] = v
	}
	for k, v := range now {
		out[k] = v
	}
	return out
}

// FromLoaded は読み込んだ結果を 1 周の入力にする（一回実行・plan・常駐で同じ、
// 不変条件 30）。
func FromLoaded(loaded *load.Loaded, rec *record.Record, clock core.Clock) Input {
	var secrets core.SecretResolver
	if len(loaded.Secrets) > 0 {
		secrets = loaded.Secrets
	}
	return Input{Resources: loaded.Resources, Ranges: loaded.Ranges, Policies: loaded.Policies,
		Providers: loaded.Providers, Record: rec, Clock: clock, Sinks: loaded.Sinks, StuckAfter: loaded.Cycle.StuckAfter,
		Checks: loaded.Checks, Procedures: loaded.Procedures, Unknown: loaded.Unknown, Incomplete: loaded.Incomplete,
		Secrets: secrets, IntentDir: loaded.IntentDir, Allocs: loaded.Allocs, InputCounts: loaded.InputCounts}
}
