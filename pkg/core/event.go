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

package core

import (
	"encoding/json"
	"time"
)

// EventKind は Event の種別（§22.3 の kind enum、§27.2）。
//
// 収束失敗・drift・halt・Workflow の出来事を個別の通知にせず、単一の Event 型の
// kind で区別する。通知は Event の consumer（sink）が kind を見て分ける。
type EventKind string

const (
	// EventDrift は cause B のずれ（現実が desired から外れた）。
	EventDrift EventKind = "Drift"
	// EventDriftWouldApply は alert-only（freeze・schedule・scope 宣言）で
	// 直さずに持ち越したずれ。直すなら行う操作を知らせる（§14.1, §14.3）。
	EventDriftWouldApply EventKind = "DriftWouldApply"
	// EventBudgetHalted は変更予算が操作を止めた周（§13）。
	EventBudgetHalted EventKind = "BudgetHalted"
	// EventCapacityHalted は容量の関門が変更を止めた周（§13.4）。
	EventCapacityHalted EventKind = "CapacityHalted"
	// EventPruneHalted は prune の安全装置（List 不完全・急減）が削除を止めた周（§12.4）。
	EventPruneHalted EventKind = "PruneHalted"
	// EventPruned は prune が orphan を消した（reality − desired、§12）。
	EventPruned EventKind = "Pruned"
	// EventApplyFailed は Apply（prune の Delete を含む）の失敗。
	EventApplyFailed EventKind = "ApplyFailed"
	// EventCycleSkipped は外部入力の不達で周を飛ばした（halt-safe、§9.2）。
	EventCycleSkipped EventKind = "CycleSkipped"
	// EventExternalInputUnreachableProlonged は外部入力不達の連続スキップが上限に達した（§9.2）。
	EventExternalInputUnreachableProlonged EventKind = "ExternalInputUnreachableProlonged"
	// EventInventoryHalted は inventory が不完全で周を止めた（incomplete ≠ empty、§9.2）。
	EventInventoryHalted EventKind = "InventoryHalted"
	// EventBreakGlassWriteUsed は break-glass の緊急書込が使われた（§25.1）。
	EventBreakGlassWriteUsed EventKind = "BreakGlassWriteUsed"
	// EventDRPlanActive は DR プランが有効になり claim がそれに従う（§25）。
	EventDRPlanActive EventKind = "DRPlanActive"
	// EventFenced は新しい fencing 世代を他シャードが持つ scope を収束しない（§15）。
	EventFenced EventKind = "Fenced"
	// EventProviderUpgradeDiff は cause C の ghost diff を適用せずに止めた（§10.4）。
	EventProviderUpgradeDiff EventKind = "ProviderUpgradeDiff"
	// EventRequiresTask は strategy: task の資源を Apply せず Task に回した（§6.5）。
	EventRequiresTask EventKind = "RequiresTask"
	// EventPlannedChange は planned_change のレーンの知らせ（§13.3）。
	EventPlannedChange EventKind = "PlannedChange"
	// EventAbsentAwaitingApproval は ensure: absent の資源が現実に在るのに、
	// planned_change（kind: delete）のレーンが開いていないので消さずに待っている
	// （§12.2・§13.3・§14.4、Backlog #163）。
	EventAbsentAwaitingApproval EventKind = "AbsentAwaitingApproval"
	// EventChangeAwaitingApproval は provider が承認の要る変更とした op
	// （provider.ApprovalRequirer。nftables の base chain の作成・type/hook/
	// priority/policy の変更、Backlog #179）を、その種類の planned_change の
	// レーンが開いていないので流さずに待っている（§13.3・§14.4）。
	EventChangeAwaitingApproval EventKind = "ChangeAwaitingApproval"
	// EventAllocationHeld は採番の書き戻しのファイル（§19.7、#157）がその要求を
	// 止めた（ファイルと store の食い違い・store に無い・書き戻しの取り込み待ち）。
	// そのリソースは Apply しない。
	EventAllocationHeld EventKind = "AllocationHeld"
	// EventApplyIneffective は Apply が成功を返したのに、settle を過ぎても現実が
	// 宣言どおりにならなかった（Diff が同じまま残った）。上限に達したらそのリソース
	// への Apply を止め（oscillating、§11.1 Tier3・§15.3）、attrs の stopped=true
	// で知らせる（Backlog #166）。
	EventApplyIneffective EventKind = "ApplyIneffective"
	// EventVersionIncompatible は部品の版の互換の表（§29.1、Backlog #151）に
	// 合わない途中の組み合わせを通る更新の順序、または確かめられない組（部品が
	// 観測できない）があって、その組の変わる部品を止めた（Apply も Task も
	// 起こさない）。
	EventVersionIncompatible EventKind = "VersionIncompatible"
	// EventDisruption は全体を止める型の更新（Workflow の disruption、§6.3、
	// Backlog #152）が対象を止めようとしている・止めている間の知らせ。Resource は
	// 止める対象、attrs に止める時間の見積もり（estimate）・影響を受ける下流
	// （affects）・時間帯（window）・知らせた時刻（notified_at）・workflow・run
	// （止めてから見積もりを過ぎたら overrun）。上流の観測値はここには載せず、
	// 止める前の承認ゲートの Event に載せる。止める前の周に firing で出し、その対象の手順を終えた周、または止める直前の確かめ（時間帯・
	// 上流）が外れて知らせを取り下げた周に resolved になる。
	EventDisruption EventKind = "Disruption"
	// EventRuntimeTightened は起動の旗が intent の runtime:（§26.7、Backlog
	// #183）より締めている（予算が小さい・mode が弱い）一時的な締め。attrs の
	// keys に締めている引数、<key>.flag と <key>.intent に旗の値と宣言の値。
	// 締めは運用者が選んだものなので、続いても stuck に上げない。
	EventRuntimeTightened EventKind = "RuntimeTightened"
	// EventRuntimeDeclarationChanged は常駐が起動したあとに intent の runtime:
	// が変わった（起動の引数は起動のときに決まり、この常駐は起動したときの値で
	// 回り続ける。起こし直すと宣言を取り込む）。attrs の <key>.started と
	// <key>.intent に起動したときの値といまの宣言の値。
	EventRuntimeDeclarationChanged EventKind = "RuntimeDeclarationChanged"
	// EventFrozen は global freeze（§14.3、Backlog #184）が効いている間の level
	// Event。attrs の source（flag・command）・by・reason・since・until（期限が
	// あるとき）。freeze を解いた周（sakutei freeze end・期限切れ）に resolved。
	// freeze は運用者が選んだものなので、続いても stuck に上げない。
	EventFrozen EventKind = "Frozen"
	// EventCommitGroupApplied は commit_group（Backlog #199）の Apply の台ごとの
	// 結果（edge、その Apply の周に 1 回）。Resource は group の 1 つ、attrs の
	// group と outcome（committed・not_committed・unknown）。unknown は確定の
	// rpc の途中で切れて成否が分からない台（次の周の観測に任せる）。
	EventCommitGroupApplied EventKind = "CommitGroupApplied"

	// Workflow の Event（edge、§6.3, §6.4）。internal/workflow はこの kind を使う。
	EventWorkflowStarted       EventKind = "WorkflowStarted"
	EventWorkflowStepDone      EventKind = "WorkflowStepDone"
	EventWorkflowWaiting       EventKind = "WorkflowWaiting"
	EventInvariantBlocked      EventKind = "InvariantBlocked"
	EventStageAwaitingApproval EventKind = "StageAwaitingApproval"
	EventGateApproved          EventKind = "GateApproved"
	EventWorkflowHalted        EventKind = "WorkflowHalted"
	EventWorkflowCompleted     EventKind = "WorkflowCompleted"
	EventWorkflowError         EventKind = "WorkflowError"
	// EventWorkflowTargetHeld は Workflow に任せて収束させない対象のうち、halt した
	// run の対象と、進み具合が分からない（store が無い・store に run が無く
	// done_when が満たされない）対象（level、§6.3、Backlog #209）。resource に対象、
	// attrs の workflow と stage（止まった理由・分からない理由）。持たなくなった周
	// （cancel・resume・done_when が満たされた）に resolved。
	EventWorkflowTargetHeld EventKind = "WorkflowTargetHeld"
	// EventWorkflowAdvancedElsewhere はこの常駐が Workflow を進めず、別の処理（store の
	// 進めるロックを取った常駐）に任せていること（level、§6.3、Backlog #210）: store を
	// 渡されていない・読むだけ（--workflow-observe-only）・alert-only・ほかの処理が
	// ロックを持つ。誤りではない（info）。attrs の workflow に Workflow の名前（, で
	// 区切る）、reason に理由。続く間は最初の周に 1 回だけ出し、進めるようになった周
	// （か Workflow が無くなった周）に resolved。
	EventWorkflowAdvancedElsewhere EventKind = "WorkflowAdvancedElsewhere"
)

// IsLevel は k が level（継続状態）の Event かを返す（§22.3）。収束失敗・ずれ・
// halt は level で、firing / resolved で出し、続く間は毎周は出さない。それ以外
// （Workflow の出来事・prune・break-glass など）は edge（起きた周に 1 回）。
func (k EventKind) IsLevel() bool {
	switch k {
	case EventDrift, EventDriftWouldApply, EventApplyFailed,
		EventBudgetHalted, EventCapacityHalted, EventPruneHalted, EventInventoryHalted,
		EventCycleSkipped, EventExternalInputUnreachableProlonged, EventProviderUpgradeDiff,
		// v3.18 (Backlog #112): a strategy: task resource's "no Task launched"
		// reason (outside the window, outstanding, no task runner, fenced) and
		// its "task launched" notice are a continuing condition about the same
		// resource; level-reporting them stops the edge spam of one warning
		// per cycle while waiting.
		EventRequiresTask,
		// Backlog #163: an absent resource waiting for its lane is a
		// continuing condition, resolved when it is observed gone.
		EventAbsentAwaitingApproval,
		// Backlog #179: the same for a provider's change waiting for its lane.
		EventChangeAwaitingApproval,
		// #157: a write-back hold continues until the file and the store agree
		// or the review is taken in.
		EventAllocationHeld,
		// Backlog #166: an Apply that does not take, and the stop of a
		// resource's Apply, is a continuing condition, resolved when the
		// resource is no longer stopped.
		EventApplyIneffective,
		// Backlog #151: a group held by the compatibility table stays held
		// until its update order (or the table) is fixed.
		EventVersionIncompatible,
		// Backlog #152: a disruptive update's outage notice fires before the
		// target is stopped and resolves when its steps are done.
		EventDisruption,
		// Backlog #183: a flag tightening the intent's runtime:, and a
		// runtime: changed since the daemon started, last as long as the
		// process (or the declaration) does.
		EventRuntimeTightened, EventRuntimeDeclarationChanged,
		// Backlog #184: a global freeze lasts until it is ended or expires.
		EventFrozen,
		// Backlog #209: a Workflow target held for a halt (or unknown
		// progress) stays held until a person cancels or resumes the run.
		EventWorkflowTargetHeld,
		// Backlog #210: a daemon that does not advance the Workflows lasts as
		// long as its role (its flags, its remediation mode).
		EventWorkflowAdvancedElsewhere:
		return true
	}
	return false
}

// 次の 2 つは Event ではない。--log-format json のとき、周の要約やその他のログ行を
// Event と同じ形の 1 行にするための kind。
const (
	// EventCycleSummary は周の要約の行（cycle N: observed=…）。
	EventCycleSummary EventKind = "CycleSummary"
	// EventLog は Event でないログ行（診断・進捗）。
	EventLog EventKind = "Log"
)

// Severity は Event の重さ。
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// EventState は level の Event の状態（§22.3）。edge の Event では空。
type EventState string

const (
	// EventFiring は続いている状態（初めて出た周と、内容が変わった周・stuck に
	// 上がった周に出る）。
	EventFiring EventState = "firing"
	// EventResolved は消えた状態（消えた周に 1 回出る）。
	EventResolved EventState = "resolved"
)

// EventClass は level の Event の分類（§22.3）。
type EventClass string

const (
	// EventTransient は自己回復の見込みがある（続いた周数が stuck のしきい値未満）。
	EventTransient EventClass = "transient"
	// EventStuck は自己回復の見込みがない（しきい値を超えて続いた、または
	// 権限・欠損・容量など permanent な失敗）。
	EventStuck EventClass = "stuck"
)

// Event は Sakutei の知らせの単一型（§22.3, §27.2）。
//
// secret の値は載せない（§20.2）。secret の欄は SecretRef の文字列（マスク済みの
// ref）のまま Message・Attrs に入れる。
type Event struct {
	Kind     EventKind
	Severity Severity
	// Time は発生時刻。ゼロなら出すときに埋める。
	Time time.Time
	// Cycle は常駐ループの周の番号（1 回収束・plan では 0）。
	Cycle int
	// Resource は対象の資源。資源に紐づかない Event ではゼロ値。
	Resource ID
	// Scope は対象の scope。空で Resource があれば Resource の scope を出す。
	Scope Scope
	// IntentRevision / RenderedRevision はその周の revision（§9.4）。
	IntentRevision   string
	RenderedRevision string
	// Cause は cause tag（A〜E、あれば）。
	Cause CauseTag
	// Message は人が読む文。
	Message string
	// Attrs は付属情報（キーと値）。
	Attrs map[string]string
	// State・Since・Count・Class は level の Event（収束失敗・ずれ・halt）にだけ
	// 付く（§22.3）。Since は始まった周の時刻、Count は続いた周数。edge の Event
	// では空。
	State EventState
	Since time.Time
	Count int
	Class EventClass
	// Text は --log-format text での 1 行（"sakutei: " などの接頭辞を含む）。
	// 空なら "<Kind>: <Message>"。JSON には出さない。
	Text string
}

// HasResource は Event が資源に紐づくかを返す。
func (e Event) HasResource() bool {
	return e.Resource.Provider != "" || e.Resource.Local != ""
}

// TextLine は text 形式の 1 行（改行なし）。
func (e Event) TextLine() string {
	if e.Text != "" {
		return e.Text
	}
	return string(e.Kind) + ": " + e.Message
}

// eventJSON は Event の JSON lines の 1 行の形。
type eventJSON struct {
	Time             string            `json:"time"`
	Kind             EventKind         `json:"kind"`
	Severity         Severity          `json:"severity"`
	Cycle            int               `json:"cycle,omitempty"`
	Resource         string            `json:"resource,omitempty"`
	Scope            string            `json:"scope,omitempty"`
	IntentRevision   string            `json:"intent_revision,omitempty"`
	RenderedRevision string            `json:"rendered_revision,omitempty"`
	Cause            CauseTag          `json:"cause,omitempty"`
	Message          string            `json:"message"`
	Attrs            map[string]string `json:"attrs,omitempty"`
	State            EventState        `json:"state,omitempty"`
	Since            string            `json:"since,omitempty"`
	Count            int               `json:"count,omitempty"`
	Class            EventClass        `json:"class,omitempty"`
}

// MarshalJSON は Event を JSON lines の 1 行の形にする（Resource は ID.String()）。
func (e Event) MarshalJSON() ([]byte, error) {
	j := eventJSON{
		Time:             e.Time.UTC().Format(time.RFC3339Nano),
		Kind:             e.Kind,
		Severity:         e.Severity,
		Cycle:            e.Cycle,
		IntentRevision:   e.IntentRevision,
		RenderedRevision: e.RenderedRevision,
		Cause:            e.Cause,
		Message:          e.Message,
		Attrs:            e.Attrs,
		State:            e.State,
		Count:            e.Count,
		Class:            e.Class,
	}
	if !e.Since.IsZero() {
		j.Since = e.Since.UTC().Format(time.RFC3339Nano)
	}
	if j.Severity == "" {
		j.Severity = SeverityInfo
	}
	scope := e.Scope
	if e.HasResource() {
		j.Resource = e.Resource.String()
		if len(scope.Labels) == 0 {
			scope = e.Resource.Scope
		}
	}
	if len(scope.Labels) > 0 {
		j.Scope = scope.String()
	}
	return json.Marshal(j)
}
