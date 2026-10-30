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

// Package reconcile — scoped.go implements the scoped reconcile (早期収束) path
// for Story Sf6a62f-2 (§8.6 早期収束パス / ADR-0009).
//
// # 位置づけ（"edge-triggered な起床、level-triggered な真実"）
//
// 定期周期（loop.go の Run/runCycleAsync）は全 DAG を毎周期 Observe→Diff→Apply
// する level-triggered ループであり、正しさの最終責任を負う。scoped reconcile は
// その上に載る「早起き」経路である: DesiredSource.Watch が届けた core.ChangeHint
// （revision+scope のみ・truth を運ばない）を入力に、影響を受けたサブ DAG だけを
// 新鮮に Observe→Diff→Apply する。目的は制御プレーンの追加レイテンシを秒オーダーに
// 抑えることであり、正しさそのものは常に定期周期へ委ねられる。
//
// # 4 つの非交渉の安全柵（§8.6 / ADR-0009）
//
// scoped reconcile が「汎用修復チャネル」に堕ちないために、以下は型/ガードで強制する:
//
//  1. cause A/E 限定 — 対象は「新旧 rendered desired の決定的 diff」から入るリソース
//     とその下流のみ。道中で見つけた drift（cause B）は scoped 経路では修復しない
//     （定期周期の仕事）。C/D をグローバルに追いかけることもしない。ScopedPlan が
//     Observe を一切挟まずに影響集合を決めるので、B/C/D は構造的に混入しない
//     （B は「desired 不変なのに現実がずれた」＝ desired diff に現れない）。
//
//  2. 集合差 prune 禁止 — scoped 経路は full-scope List をしないので orphan を検出
//     できず、してはならない。集合差 prune（PruneWithinScope）は経路上で決して呼ば
//     れない（prune.Engine を import すらしない）。desired で明示的に absent へ
//     移った個別リソース（`ensure: absent` の tombstone 個別削除、§12.2・#163）だけを
//     許容する。desired から消えただけのリソースは消さない（open-world）。
//
//  3. 予算共有（バイパスではない）— scoped run は定期経路と同一の budget.Engine を
//     消費する。独立カウンタは作らない。予算超過時は halt して当該作業を次の定期
//     周期へ縮退させる（バイパスしない・§13）。
//
//  4. scoped-only Observe — ヒントは truth を運ばないので、起床後に影響サブ DAG の
//     リソースだけを新鮮に Observe する（level-triggered）。全 fleet は Observe しない。
//     plan（ScopedPlan）の観測を reconcile に持ち越さない（観測 state レス・§3 原則1）。
//
// # v1b との境界（seam サイズ）
//
// 本番の edge-triggered 常駐結線（webhook/watch デーモンからヒントを吸い出し続ける
// フル経路）は v1b。ここでは seam（ScopedPlan / ScopedReconcile）を切り、fake 駆動の
// 受け入れテストで「影響サブ DAG だけが収束する」ことを証明する最小結線に留める。
// loop.go の Run には、DesiredSource が Watcher を実装するときにヒントを吸い出して
// ScopedReconcile を呼ぶ薄い結線を behind-type-assertion で足す（非 watch source は
// 一切影響を受けず、既存テストは決定的なまま）。
package reconcile

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/tjst-t/sakutei/v2/internal/budget"
	"github.com/tjst-t/sakutei/v2/internal/cause"
	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/eventlog"
	"github.com/tjst-t/sakutei/v2/internal/executor"
	"github.com/tjst-t/sakutei/v2/internal/planreport"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// ScopedPlan は影響サブ DAG 算出 seam である（AC-Sf6a62f-2-1, task Sf6a62f-2-1）。
//
// 入力は core.ChangeHint と「新旧 rendered desired」であり、リアルには一切触れない
// （Observe を挟まない決定的 diff）。出力は「desired が変わったリソース + その下流」
// に限定した部分集合であり、これがそのまま scoped Observe→Diff→Apply の対象となる。
//
// # 影響集合の決め方（決定的・観測非接触）
//
//	seed（起点） = 次のいずれかを満たす newDesired 内リソース:
//	  (a) hint.TouchedScopes のいずれかに scope が一致する、または
//	  (b) 新旧 desired の間で desired が変化した（新規追加・フィールド変更・
//	      depends_on 変化）。
//	closure = seed から depends_on エッジを下流方向へ推移閉包した集合。
//	          （下流 = "この seed に依存するリソース"。上流の変化は下流に波及しうる。）
//
// 上流（seed が依存する側）は「変わっていない前提」で閉包に入れない。もし上流も
// 変わっていれば、その上流自身が seed に入る（新旧 diff で拾われる）ので取りこぼさない。
//
// # 個別削除（tombstone）の扱い（安全柵 2）
//
// 新 desired で `ensure: absent`（明示の absent、§12.2・#163）になった影響集合の
// リソースだけを RemovedTombstones として返す（その Delete は Diff が計画し、
// planned_change のレーンが開いているときだけ走る）。v1a は「desired から消えた＝
// 個別削除意図」を tombstone とみなしていたが、それでは prune しない型
// （Pruneable:false の VM など）も宣言から外しただけで消えてしまうので、#163 で
// 明示の absent だけに改めた。desired から消えただけのリソースは scoped 経路でも
// 定期周期でも消さない（PruneWithinScope の orphan は full-List の定期周期だけが扱う）。
//
// 返り値の Impacted は newDesired の出現順を保った決定的なスライスである。
func ScopedPlan(hint core.ChangeHint, oldDesired, newDesired []diff.DesiredResource) ScopedPlanResult {
	// old をキー引きできるようにする。
	oldByLocal := make(map[string]diff.DesiredResource, len(oldDesired))
	for _, r := range oldDesired {
		oldByLocal[r.ID.Local] = r
	}
	newByLocal := make(map[string]diff.DesiredResource, len(newDesired))
	for _, r := range newDesired {
		newByLocal[r.ID.Local] = r
	}

	// seed 集合を決める（scope 一致 or desired 変化）。
	seed := make(map[string]bool)
	for _, r := range newDesired {
		local := r.ID.Local
		if scopeTouched(r.ID.Scope, hint.TouchedScopes) {
			seed[local] = true
			continue
		}
		old, existed := oldByLocal[local]
		if !existed || !desiredEqual(old, r) {
			// 新規追加 or desired 変化 → cause A/E 起点。
			seed[local] = true
		}
	}

	// depends_on の下流隣接（dep → r）を作る。adj[depLocal] = [depLocal に依存する local...]。
	adj := make(map[string][]string, len(newDesired))
	for _, r := range newDesired {
		for _, dep := range r.DependsOn {
			adj[dep] = append(adj[dep], r.ID.Local)
		}
	}

	// seed から下流方向へ推移閉包（BFS）。
	impactedSet := make(map[string]bool, len(seed))
	queue := make([]string, 0, len(seed))
	// 決定的にするため newDesired 出現順で seed を積む。
	for _, r := range newDesired {
		if seed[r.ID.Local] && !impactedSet[r.ID.Local] {
			impactedSet[r.ID.Local] = true
			queue = append(queue, r.ID.Local)
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, down := range adj[cur] {
			if !impactedSet[down] {
				impactedSet[down] = true
				queue = append(queue, down)
			}
		}
	}

	// Impacted を newDesired 出現順で確定（決定的）。
	impacted := make([]diff.DesiredResource, 0, len(impactedSet))
	for _, r := range newDesired {
		if impactedSet[r.ID.Local] {
			impacted = append(impacted, r)
		}
	}

	// 個別削除 tombstone: 影響集合のうち、新 desired で ensure: absent のもの（#163）。
	// desired から消えただけのものは含めない（安全柵 2・open-world）。
	var tombstones []core.ID
	for _, r := range impacted {
		if r.Absent {
			tombstones = append(tombstones, r.ID)
		}
	}
	// 決定的順序。
	sort.Slice(tombstones, func(i, j int) bool {
		return tombstones[i].String() < tombstones[j].String()
	})

	return ScopedPlanResult{
		Hint:              hint,
		Impacted:          impacted,
		RemovedTombstones: tombstones,
	}
}

// ScopedPlanResult は ScopedPlan の出力（影響サブ DAG）である。
type ScopedPlanResult struct {
	// Hint は起点となった ChangeHint（revision+scope のみ）。
	Hint core.ChangeHint
	// Impacted は影響を受けたリソース（seed + 下流閉包）を newDesired 出現順で
	// 並べたスライス。これが scoped Observe→Diff→Apply の対象。
	Impacted []diff.DesiredResource
	// RemovedTombstones は影響集合のうち新 desired で ensure: absent のリソースの ID
	// （§12.2・#163）。scoped 経路で許容される唯一の削除（個別・明示 absent）で、
	// 集合差 prune ではない。Delete は Impacted の Diff が計画する。
	RemovedTombstones []core.ID
}

// ScopedReconcileResult は ScopedReconcile の結果サマリである。
type ScopedReconcileResult struct {
	// Impacted は影響サブ DAG に入ったリソース数。
	Impacted int
	// Observed は実際に新鮮な Observe を行ったリソース数（scoped-only）。
	Observed int
	// Applied はこの run で適用した Op 数（同期経路）。
	Applied int
	// Skipped は上流失敗/観測不能でスキップしたリソースの Local ID 群。
	Skipped []string
	// DeferredCauseB は「scoped 経路で見つかったが cause B と分類され、修復せず
	// 定期周期へ委ねた」リソースの Local ID 群（安全柵 1）。
	DeferredCauseB []string
	// Tombstoned は個別削除（ensure: absent）した ID 群。
	Tombstoned []core.ID
	// AwaitingApproval は ensure: absent で現実に在るのに、planned_change のレーンが
	// 開いていないので消さなかった ID 群（§13.3・§14.4、#163）と、provider が承認の
	// 要る変更とした op をレーンが無いので流さなかった ID 群（#179）。
	AwaitingApproval []core.ID
	// BudgetHalted は予算超過で halt し、当該作業を次の定期周期へ縮退させたとき true。
	BudgetHalted bool
	// BudgetReason は halt 理由（BudgetHalted のとき非空）。
	BudgetReason string
	// Deferred は予算 halt などで「今回は適用せず定期周期へ委ねた」とき true。
	Deferred bool
}

// ScopedReconcile は早期収束の到達可能な最小エントリである（AC-Sf6a62f-2-2/3/4）。
//
// hint（revision+scope のみ）と新旧 rendered desired を受け取り、ScopedPlan で
// 影響サブ DAG を算出し、そのサブ DAG だけを新鮮に Observe→Diff→Apply する。
// 通常の wave/skip セマンティクス（ApplyWaves）を再利用する。
//
// 引数:
//
//	oldDesired/newDesired — 決定的 diff の材料（リアル非接触）。呼び出し側が
//	                        DesiredSource.Load で得た新旧を渡す（観測ではない）。
//	fp/exec               — 定期経路と同じ Provider/Executor。
//	budgetEngine          — 定期経路と "同一" の budget.Engine（安全柵 3・共有）。
//	ifl                   — 定期経路と同一の in-flight 集合（per-resource 排他共有）。
//	baseCauseCtx          — この run の revision/version 文脈。scoped 経路は
//	                        A/E のみ扱う前提。
//
// 4 つの安全柵の実装点は本関数のコメントで各所に明示する。
func ScopedReconcile(
	ctx context.Context,
	spr ScopedPlanResult,
	fp provider.Provider,
	exec executor.Executor,
	engine *diff.Engine,
	budgetEngine *budget.Engine,
	ifl *InFlightSet,
	baseCauseCtx cause.CauseContext,
	errOut io.Writer,
) ScopedReconcileResult {
	res := ScopedReconcileResult{Impacted: len(spr.Impacted)}
	if errOut == nil {
		errOut = io.Discard
	}

	desired := spr.Impacted

	// ---- 安全柵 4: scoped-only Observe -------------------------------------
	//
	// 影響サブ DAG のリソースだけを新鮮に Observe する。全 fleet は Observe しない
	// （そもそも desired スライスに影響外リソースが入っていない）。ヒントは truth を
	// 運ばないので、ここで初めて現実を再導出する（level-triggered）。plan（ScopedPlan）
	// は観測を一切していないので、観測 state を reconcile に持ち越さない。
	plans := make(map[string]core.Plan, len(desired))
	unobservable := make(map[string]bool)
	// ready は今回の観測で得た ready condition（§8.3）。前回値は持ち越さない。
	ready := make(Readiness)
	observed := 0
	var deferredCauseB []string

	for _, r := range desired {
		local := r.ID.Local

		// per-resource in-flight 排他共有（安全柵 3 の一部）: 定期経路と同じ ifl を
		// 参照するので、定期経路が Apply 中のリソースは scoped 経路でも二重起動しない。
		if ifl.IsInFlight(local) {
			unobservable[local] = true // 下流へ skip を伝播（収束中扱い）。
			fmt.Fprintf(errOut, "sakutei: scoped: skip %s: in-flight (定期経路と排他共有)\n", r.ID)
			continue
		}
		// commit_group（#199）の member は、group の全部を見る定期経路に任せる
		// （影響サブ DAG に group の一部しか入らないことがあり、一部だけ確定しない）。
		if g := provider.CommitGroupOf(fp, local, r.Fields); g != "" {
			unobservable[local] = true
			fmt.Fprintf(errOut, "sakutei: scoped: skip %s: in commit_group %q (left to the periodic cycle, which sees the whole group)\n", r.ID, g)
			continue
		}

		obs, readErr := provider.ReadFor(ctx, fp, local, r.Fields)
		if readErr != nil {
			fmt.Fprintf(errOut, "sakutei: scoped: observe %s: %v\n", r.ID, readErr)
			unobservable[local] = true
			continue
		}
		observed++
		if obs.Availability != core.AvailabilityOk {
			fmt.Fprintf(errOut, "sakutei: scoped: observe %s: availability=%v (not Ok)\n", r.ID, obs.Availability)
			unobservable[local] = true
			continue
		}
		ready.Record(local, obs)

		// S33266e-1 (§20): reject a bare plaintext in a secret-flagged field
		// before diffing (names the field only, never the value).
		if err := engine.ValidateDesired(r); err != nil {
			fmt.Fprintf(errOut, "sakutei: scoped: %v\n", err)
			unobservable[local] = true
			continue
		}

		// Fix Sc7a647 cause-consistency: pass Create-vs-Update to Classify.
		// The scoped (early-convergence) path is entered on a revision change
		// (cause A/E) so IntentPrevRevision is normally known; this flag only
		// changes the cold-start decision, kept here for uniformity.
		resCauseCtx := baseCauseCtx
		resCauseCtx.ResourceObservedAbsent = obs.State == nil
		plan, planErr := engine.Plan(obs, r, resCauseCtx)
		if planErr != nil {
			fmt.Fprintf(errOut, "sakutei: scoped: plan: %v\n", planErr)
			unobservable[local] = true
			continue
		}
		if len(plan.Ops) == 0 {
			continue // 既に収束。
		}

		// #41 (§6.5): strategy: task のリソースは早期収束パスで Apply しない。Task の
		// 起動（1-outstanding・窓）は定期周期だけが行う。下流は収束中扱いで待たせる。
		if r.UpdatePolicy.IsTask() {
			unobservable[local] = true
			fmt.Fprintf(errOut, "sakutei: scoped: skip %s: strategy: task は定期周期が Task を起動する（直接 Apply しない）\n", r.ID)
			continue
		}

		// §23.4: 観測だけの provider（Capabilities.ObserveOnly）は早期収束パスでも
		// Apply しない。ずれの知らせは定期周期が出す。下流は収束中扱いで待たせる。
		if providerObservesOnly(fp, r.ID.Provider) {
			unobservable[local] = true
			fmt.Fprintf(errOut, "sakutei: scoped: skip %s: %s\n", r.ID, planreport.ObserveOnlyReason)
			continue
		}

		// ---- 安全柵 1: cause A/E 限定 --------------------------------------
		//
		// scoped 経路は「新旧 desired diff から入った」cause A/E のみを扱う。道中で
		// 見つけた cause B（drift）は scoped 経路では修復せず、定期周期へ委ねる。
		// C（ghost diff）/ D（cascade）も scoped 経路では適用しない。
		//
		// baseCauseCtx は「この run 全体の revision/version 文脈」なので、Diff が
		// 返す Op の Cause で個別に判定する。影響集合は desired diff 由来なので通常は
		// A/E になるが、seed が scope 一致だけで入って実際には desired 不変（現実だけ
		// ずれていた＝B）というケースを取りこぼさないよう、ここで明示的にフィルタする。
		var acceptedOps []core.Op
		hasNonAE := false
		for _, op := range plan.Ops {
			switch op.Cause {
			case core.CauseTagA, core.CauseTagE:
				acceptedOps = append(acceptedOps, op)
			default:
				// B/C/D は scoped 経路で修復しない。
				hasNonAE = true
			}
		}
		if hasNonAE {
			deferredCauseB = append(deferredCauseB, local)
			fmt.Fprintf(errOut,
				"sakutei: scoped: defer %s: 非 A/E cause を含む diff は定期周期へ委ねる（安全柵: cause A/E 限定）\n",
				r.ID)
		}
		if len(acceptedOps) == 0 {
			// A/E の op が無い（全て B/C/D だった）→ この run では何もしない。
			continue
		}
		plans[local] = core.Plan{Ops: acceptedOps}
	}

	// 影響サブ DAG の外にある ready_condition 上流も観測して gate に使う（§8.3）。
	// 観測できなければ下流は保留のまま（次の定期周期が拾う）。
	ObserveExternalUpstreams(ctx, func(diff.DesiredResource) provider.Provider { return fp },
		desired, ready, errOut)

	res.Observed = observed
	res.DeferredCauseB = deferredCauseB

	// ---- 安全柵 2: 集合差 prune 禁止（アサーション） ------------------------
	//
	// scoped 経路は full-scope List を一切しないので orphan を検出できない。よって
	// 集合差 prune（PruneWithinScope）はこの経路から決して起動されない。ScopedPlan は
	// PruneWithinScope の消失リソースを RemovedTombstones から除外済みだが、防御的に
	// ここでも「tombstone は全て個別・非集合差である」ことを保証する。prune.Engine を
	// import すらしていない（このファイルの import 群を参照）ことが最終的な保証。
	assertNoSetDifferencePrune(spr)

	// ---- 安全柵 3: 予算共有（バイパスではない） ----------------------------
	//
	// 定期経路と同一の budget.Engine を消費する。独立カウンタは作らない。scoped run の
	// 全 op（Apply plans。ensure: absent の tombstone delete を含む）を CheckCycle に
	// 通し、halt したら適用せず次の定期周期へ縮退する（バイパスしない）。
	var allPlannedOps []core.Op
	for _, r := range desired {
		if p, ok := plans[r.ID.Local]; ok {
			allPlannedOps = append(allPlannedOps, p.Ops...)
		}
	}

	// in-flight（定期経路 or 先の scoped run 由来）も cycle 予算に数える（loop.go と同じ
	// 数え方）。ここでは影響サブ DAG 内の in-flight を数える。破壊的上限には破壊的な
	// in-flight だけを数える（§13.1）。
	var inFlight budget.InFlight
	for _, r := range desired {
		if ifl.IsInFlight(r.ID.Local) && !ifl.IsLane(r.ID.Local) {
			inFlight.Total++
			if ifl.IsDestructive(r.ID.Local) {
				inFlight.Destructive++
			}
		}
	}
	decision := budgetEngine.CheckCycle(allPlannedOps, inFlight, len(desired))
	if decision.HaltAll || decision.HaltDestructive {
		// 予算超過 → scoped run は halt して定期周期へ縮退（バイパス禁止・§13）。
		// halt-destructive-only の場合でも scoped 経路は保守的に「今回は何もせず縮退」
		// させる: 速い経路が予算枠を先食いして定期経路の判断を歪めないため。
		res.BudgetHalted = true
		res.BudgetReason = decision.Reason
		res.Deferred = true
		eventlog.Emit(errOut, core.Event{
			Kind: core.EventBudgetHalted, Severity: core.SeverityWarning, Message: decision.Reason,
			Attrs: map[string]string{"path": "scoped"},
			Text: fmt.Sprintf("sakutei: scoped: BUDGET HALTED: %s\n  → scoped run を縮退し、次の定期周期へ委ねる（バイパスしない）",
				decision.Reason),
		})
		return res
	}

	for _, ev := range decision.Events {
		eventlog.Emit(errOut, core.Event{
			Kind: core.EventPlannedChange, Severity: core.SeverityInfo, Message: ev,
			Attrs: map[string]string{"path": "scoped"},
			Text:  "sakutei: scoped: " + ev,
		})
	}

	// ensure: absent の Delete（§12.2・#163）は planned_change のレーンに入ったときだけ
	// 走る。レーンの外のものは消さずに知らせ、この run の plans から落とす。
	for _, op := range UnapprovedOps(plans, decision) {
		res.AwaitingApproval = append(res.AwaitingApproval, op.Target)
		EmitAwaitingApproval(errOut, "sakutei: scoped: ", op)
		delete(plans, op.Target.Local)
	}
	allPlannedOps = allPlannedOps[:0]
	for _, r := range desired {
		if p, ok := plans[r.ID.Local]; ok {
			allPlannedOps = append(allPlannedOps, p.Ops...)
		}
	}

	// planned_change のレーン（§13.3）に入った op は、発射前にレーンの枠を消費する
	// （同期 wave なので、ここで全 op 分を先に取る。skip された op の枠は戻さない＝保守側）。
	budgetEngine.ReserveLanes(decision, allPlannedOps)
	if err := budgetEngine.FlushLaneAudit(); err != nil {
		fmt.Fprintf(errOut, "sakutei: scoped: warn: planned_change: cannot record lane executions in S4: %v\n", err)
	}

	// ---- Apply（同期 wave） -------------------------------------------------
	//
	// 影響サブ DAG のみに対して通常の wave/skip セマンティクス（ApplyWaves）を再利用。
	// A/E の plans だけが渡るので B/C/D は適用されない（安全柵 1）。
	// #187: the S4 record of each Apply names the lane that admitted it.
	applyCtx := executor.WithApproval(ctx, func(op core.Op) (string, string) {
		return budgetEngine.ApprovalOf(decision, op)
	})
	applied, appliedIDs, skipped, applyErr := applyWaves(
		applyCtx, fp, exec, desired, plans, unobservable, nil, ready, errOut)
	if applyErr != nil {
		fmt.Fprintf(errOut, "sakutei: scoped: wave error: %v\n", applyErr)
	}
	res.Applied = applied
	res.Skipped = skipped

	// 適用に成功した op を予算 window に記録（安全柵 3: 定期経路と同一 counter を進める）。
	var committed []core.Op
	for _, r := range desired {
		if !appliedIDs[r.ID.Local] {
			continue
		}
		if p, ok := plans[r.ID.Local]; ok {
			committed = append(committed, p.Ops...)
			if r.Absent {
				// 個別削除 tombstone（ensure: absent）を適用した。
				fmt.Fprintf(errOut, "sakutei: scoped: deleted %s (ensure: absent; 個別削除)\n", r.ID)
				res.Tombstoned = append(res.Tombstoned, r.ID)
			}
		}
	}

	// レーンの op は通常予算の window に記録しない（§13.3: 別枠）。
	if committed = decision.OutsideLanes(committed); len(committed) > 0 {
		budgetEngine.CommitOps(committed)
	}

	return res
}

// ScopedRunner は早期収束の共有状態を束ねた、デーモンから到達可能な seam である。
//
// 定期経路（loop.go の Run）と "同一" の budget.Engine と in-flight 集合を持つことが
// 安全柵 3（予算/排他の共有）の結線点である。Run 側でこれらを構築し、同じポインタを
// ScopedRunner に渡せば、scoped 経路と定期経路は同一カウンタ/同一排他集合を共有する。
type ScopedRunner struct {
	Provider     provider.Provider
	Executor     executor.Executor
	Engine       *diff.Engine
	BudgetEngine *budget.Engine
	InFlight     *InFlightSet
	ErrOut       io.Writer
}

// DrainAndReconcile は Watcher が届けたヒントを（ブロックせず）吸い出し、影響サブ DAG
// だけを scoped reconcile する、デーモン結線用の薄いエントリである。
//
// 決定論のための規律（§8.6 / このパッケージ doc の "v1b との境界"）:
//   - チャネルに今ある分だけを非ブロッキングに吸い出して 1 回の CoalesceHints に畳む
//     （level-triggered: 途中ヒントを落としても最終現実を Observe するので安全）。
//   - ヒントが 1 件も無ければ即座に false を返す（定期経路を一切妨げない）。
//   - oldDesired/newDesired は呼び出し側が Load で与える（本関数は Observe を挟まない
//     決定的 diff から出発する）。newDesired の Observe は ScopedReconcile 内で scoped に
//     行われる（scoped-only Observe）。
//
// 本番の常駐 edge-triggered 経路（watch を回し続ける）は v1b。ここでは「Watcher を
// 実装する DesiredSource からヒントを吸い出して ScopedReconcile を呼べる」ことを示す
// 最小結線に留め、非 watch source は Watcher 型アサーションで弾かれて一切影響を受けない。
//
// 戻り値 ran は scoped run を実行したか（ヒントがあったか）。
func (sr ScopedRunner) DrainAndReconcile(
	ctx context.Context,
	hintCh <-chan core.ChangeHint,
	oldDesired, newDesired []diff.DesiredResource,
	baseCauseCtx cause.CauseContext,
) (ScopedReconcileResult, bool) {
	// 非ブロッキングに、今ある分だけ吸い出す。
	var hints []core.ChangeHint
drain:
	for {
		select {
		case h, ok := <-hintCh:
			if !ok {
				break drain // チャネル close = 購読終了。
			}
			hints = append(hints, h)
		default:
			break drain
		}
	}
	if len(hints) == 0 {
		return ScopedReconcileResult{}, false
	}

	// level-triggered: n 件のヒントを 1 件に畳む（scope 和集合 + last-wins な revision タグ）。
	// revision は content-address で単調でないため「最新」を辞書比較で選ばない（FIX #1）。
	coalesced := core.CoalesceHints(hints...)
	spr := ScopedPlan(coalesced, oldDesired, newDesired)
	res := ScopedReconcile(
		ctx, spr, sr.Provider, sr.Executor, sr.Engine,
		sr.BudgetEngine, sr.InFlight, baseCauseCtx, sr.ErrOut)
	return res, true
}

// assertNoSetDifferencePrune は安全柵 2 の防御的アサーションである。
//
// scoped 経路で許容される削除は ensure: absent の個別 tombstone のみ。ここに
// 到達する RemovedTombstones は ScopedPlan が明示の absent だけから作っているが、
// 将来の改変で集合差 prune が紛れ込む回帰を型/実行時で塞ぐため二重に保証する。
//
// このパッケージは internal/prune を import しない（scoped.go の import 群を参照）。
// つまり集合差 prune エンジンは scoped 経路から到達不能である。
func assertNoSetDifferencePrune(spr ScopedPlanResult) {
	// RemovedTombstones は ID のみ。ScopedPlan は ensure: absent の影響集合だけから
	// 作るので、ここでは経路上に List 由来 orphan が
	// 一切入り込まないことを設計不変条件として明示するだけでよい。ノーオペ。
	_ = spr
}

// scopeTouched は scope が hint.TouchedScopes のいずれかに一致するかを返す。
// 空の TouchedScopes は「どの scope とも一致しない」（scope 由来の seed 無し）とする。
// desired diff 由来の seed は別途拾われるので、これは routing key の絞り込みに徹する。
func scopeTouched(sc core.Scope, touched []core.Scope) bool {
	for _, t := range touched {
		if sc.Equal(t) {
			return true
		}
	}
	return false
}

// desiredEqual は 2 つの DesiredResource が desired として同値かを決定的に判定する。
// ID・Fields・DependsOn・ReadyConditions・DeletionPolicy を比較する（観測には触れない）。
func desiredEqual(a, b diff.DesiredResource) bool {
	if !a.ID.Equal(b.ID) {
		return false
	}
	if a.DeletionPolicy != b.DeletionPolicy || a.Absent != b.Absent {
		return false
	}
	if len(a.Fields) != len(b.Fields) {
		return false
	}
	for k, v := range a.Fields {
		if bv, ok := b.Fields[k]; !ok || bv != v {
			return false
		}
	}
	if len(a.DependsOn) != len(b.DependsOn) {
		return false
	}
	// ready_condition の付け外し・変更も desired 変化とみなす。
	if len(a.ReadyConditions) != len(b.ReadyConditions) {
		return false
	}
	for k, v := range a.ReadyConditions {
		if bv, ok := b.ReadyConditions[k]; !ok || bv != v {
			return false
		}
	}
	// depends_on は順序も含めて比較（順序変化も desired 変化とみなす保守側）。
	for i := range a.DependsOn {
		if a.DependsOn[i] != b.DependsOn[i] {
			return false
		}
	}
	return true
}
