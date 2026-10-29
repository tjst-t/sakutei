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

// Package statusreport builds the human-facing 適合レポート（conformance report）
// printed by "sakutei status"（Story Sc7a647-3, §26.4）。
//
// # 位置づけ
//
// status は read-only の「チェック」であり、収束ループの conformance 判定
// （§26.4）を人向け・監査向けに切り出したデバッグ体験である。現実を Observe し
// desired と Diff するだけで、Apply（CREATE/UPDATE/DELETE）は一切行わない。
// health/canary（稼働性・段階ロールアウトの健全性判定）とは別物であり、混同しない
// —— status が答えるのは「宣言どおり在るか（drift の有無）」だけである。
//
// # レポートの構成要素
//
//   - drifted リソース一覧（適合レポート）—— desired と現実が食い違うリソース。
//   - 原因タグ A〜E（§10.2）—— なぜ drift したか。diff エンジンが Op に付した
//     core.CauseTag をそのまま採録する（cause 分類ロジックを fork しない）。
//   - provider 階級（Capabilities tier）—— TrueObserve/List/BulkObserve/Pruneable/
//     ProviderVersion。各 drifted 項目の provider の正直な実装階級を示す。
//   - last-seen リング由来の「前周は X だった」（S3・非永続）—— internal/lastseen の
//     ResourceRing.DriftSummary。リングが空（初回/再起動後）は「前周スナップショット
//     なし」。
//   - 下流波及 D —— drift が依存先（depends_on）に及ぶ場合、その依存先を cause D として
//     一覧に反映する。cause D の判定は diff エンジン（cause パッケージ）が付した
//     core.CauseTagD を採録する。
//
// このパッケージは純粋な組み立て・整形のみを行い、Observe も Apply もしない。
// 呼び出し側（cmd/sakutei/status.go）が read-only に観測した結果を渡す。これにより
// 「前周は X だった」機構をリング層で単体テストできる（観測を 2 回与えて、レポートが
// 前周スナップショットを表示することを assert する）。
package statusreport

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/freeze"
	"github.com/tjst-t/sakutei/v2/internal/lastseen"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// Tier は provider の Capabilities 階級を人間可読に整形した表示用の値である。
type Tier struct {
	TrueObserve     bool
	List            bool
	BulkObserve     bool
	Pruneable       bool
	ProviderVersion string
}

// TierFromCapabilities は provider.Capabilities から表示用 Tier を作る。
func TierFromCapabilities(c provider.Capabilities) Tier {
	return Tier{
		TrueObserve:     c.TrueObserve,
		List:            c.List,
		BulkObserve:     c.BulkObserve,
		Pruneable:       c.Pruneable,
		ProviderVersion: c.ProviderVersion,
	}
}

// String は Tier を "TrueObserve=true List=true BulkObserve=false Pruneable=false
// ProviderVersion=v1" 形式の 1 行に整形する。
func (t Tier) String() string {
	return fmt.Sprintf(
		"TrueObserve=%t List=%t BulkObserve=%t Pruneable=%t ProviderVersion=%s",
		t.TrueObserve, t.List, t.BulkObserve, t.Pruneable, t.ProviderVersion)
}

// DriftedItem は 1 つの drifted リソースのレポート行を構成する要素である。
type DriftedItem struct {
	// ID はリソースの合成 ID（Provider/Scope/Local）。
	ID core.ID
	// Cause は diff エンジンが付した原因タグ（A〜E）。Op.Cause をそのまま採録する。
	Cause core.CauseTag
	// Tier は当該リソースの provider の Capabilities 階級。
	Tier Tier
	// LastSeen は last-seen リング由来の「前周は X だった」注記
	// （空リングなら「前周スナップショットなし」相当のメッセージ）。
	LastSeen string
	// Diagnostic は diff エンジンが付した人間可読の理由（任意）。
	Diagnostic string
	// Fields はずれているフィールドと、その現実の値（From）・宣言の値（To）（#111）。
	// diff エンジンの FieldDiff をそのまま採録する（secret は参照の形に正規化済み、§20.2）。
	Fields []core.FieldDiff
	// Missing は、宣言されているのに現実に無い（観測した State が nil）こと（#118）。
	// 文の出力では DRIFTED のまま。JSON の missing に出る。
	Missing bool
}

// UnobservableItem は観測できなかったリソース 1 つ（#117）。
type UnobservableItem struct {
	ID     core.ID
	Reason string
}

// UndetectableItem は宣言外（unmanaged）の検出ができなかった provider 1 つ（#118）。
type UndetectableItem struct {
	Provider string
	// Kind は検出できなかったわけの種類（UndetectableListUnsupported など）。
	Kind string
	// Reason は人向けの理由（List のエラー文、List の Status など。非対応なら空）。
	Reason string
	// Note は文の出力に出す注記の行。
	Note string
}

// 宣言外の検出ができなかったわけの種類（JSON の unmanaged_undetectable[].kind）。
const (
	UndetectableListUnsupported = "list_unsupported"
	UndetectableListError       = "list_error"
	UndetectableListIncomplete  = "list_incomplete"
	// #169: List の ID の scope が宣言の scope と食い違う（数えるのは list_error と同じ）。
	UndetectableScopeMismatch = "scope_mismatch"
	// #169: --claim のとき、scope が決まらず claim の内外を決められない ID があった。
	UndetectableScopeUndecided = "scope_undecided"
)

// WorkflowHeldItem は Workflow に任せている対象 1 つ（#89）。
type WorkflowHeldItem struct {
	ID       core.ID
	Workflow string
	// Stage は run の段階（実行中・承認待ち・halt・未起動。workflow store が無ければ不明）。
	Stage string
}

// OscillatingItem は Apply を止めたリソース 1 つ（§11.1 Tier3・§15.3、Backlog
// #166）: 同じ Diff の Apply が成功を返しても現実が変わらないまま上限に達した。
// 常駐の読み取り専用の口（GET /status）と --daemon-status に出る。
type OscillatingItem struct {
	ID core.ID
	// Diff は止めたときの Diff（`power: "Off" → "On"` など）。
	Diff string
	// IneffectiveApplies は効かなかった Apply の回数、MaxApplies はその上限、
	// Settle は 1 回ごとに待った時間。
	IneffectiveApplies int
	MaxApplies         int
	Settle             string
	// Reason は止めたわけ（max_ineffective_applies / max_ineffective_duration）、
	// Note はその文。
	Reason string
	Note   string
	// Since は止めた時刻。
	Since time.Time
}

// Report は status の 1 回分の適合レポートである。
type Report struct {
	// IntentRevision / RenderedRevision はヘッダに出す版情報。
	IntentRevision   string
	RenderedRevision string
	// Drifted は drifted リソース一覧（Local ID 昇順で整列済み）。
	Drifted []DriftedItem
	// ConvergedCount は適合（drift なし）だったリソース数。
	ConvergedCount int
	// Converged は適合だったリソース（ID 文字列昇順）（#118）。ID の分からない呼び出し
	// （AddConverged）は数だけ増やし、ここには載せない。
	Converged []core.ID
	// UnobservableCount は観測不能（Availability != Ok）だったリソース数。
	// unknown ≠ absent（§3）: これらは drifted にも converged にも数えない。
	UnobservableCount int
	// Unobservable は観測できなかったリソースと、その理由（Read のエラー文、または
	// Availability）（#117）。Local ID 昇順で整列済み。理由が分からない呼び出し
	// （AddUnobservable）は数だけ増やし、ここには載せない。
	Unobservable []UnobservableItem
	// ListErrorCount は List がエラーだった provider の数（#117）。宣言外の検出が
	// できなかった＝現実を見られなかったので、観測できないものとして終了コードに効く。
	ListErrorCount int
	// Unmanaged は「宣言外・実在」リソース一覧（現実に在るが intent に無い）。
	// FIX #2 (Sc7a647-3): provider の List でリアルを列挙し、宣言集合との差分から
	// 求める。既定の DeletionPolicy（ExplicitAbsentOnly）では prune 対象外＝削除され
	// ない（「確信なき減算はしない」）。ID 文字列昇順で整列済み。
	Unmanaged []core.ID
	// UnmanagedNotes は unmanaged 検出が「できなかった」provider の注記
	// （List 非対応 / List エラー）。検出不能を「宣言外ゼロ」と混同しないための
	// graceful degradation の可視化。
	UnmanagedNotes []string
	// Undetectable は unmanaged 検出ができなかった provider とその理由（#118）。
	// provider 名の昇順で整列済み。UnmanagedNotes の行と同じ情報の構造化した形。
	Undetectable []UndetectableItem
	// WorkflowHeld は done でない Workflow に任せている対象（#89、§6.3）。収束ループは
	// これを Observe も Apply もしないので、drifted にも converged にも数えない。
	// Local ID 昇順で整列済み。
	WorkflowHeld []WorkflowHeldItem
	// Oscillating は Apply を止めたリソース（#166、§11.1 Tier3）。drifted には
	// 数えない（ずれてはいるが、収束ループは Apply しない）。ID 文字列の昇順。
	Oscillating []OscillatingItem
	// Daemon は常駐デーモンの健全性（#15）。status に --daemon-status が無ければ nil。
	Daemon *DaemonHealth
	// DaemonNote はデーモン健全性を読めなかったときの注記（読めないことを隠さない）。
	DaemonNote string
	// DaemonStale は、status を出した時点でデーモンの健全性ファイルが周期の数倍
	// 更新されていないこと（デーモンが止まっているかもしれない）。
	DaemonStale bool
	// Freeze は --freeze-state のファイルに書かれ、効いている global freeze
	// （§14.3、#184）。status に --freeze-state が無い・効いていなければ nil。
	Freeze *freeze.Freeze
	// FreezeNote は --freeze-state を読めなかったときの注記。
	FreezeNote string
}

// DriftSummaryProvider は last-seen リングの「前周は X だった」注記を提供する
// 最小インタフェースである。*lastseen.ResourceRing が満たす。テストでは実際の
// リングに 2 回観測を Record して前周表示を検証する。
type DriftSummaryProvider interface {
	DriftSummary(localID string, curFields map[string]string) string
}

// Builder は 1 回の status 実行のレポートを組み立てる。呼び出し側が read-only に
// 観測・diff した結果を Add* で流し込み、最後に Build() でレポートを得る。
//
// Builder は Observe も Apply もしない —— 純粋な組み立てのみ。read-only 性は
// 構造的にこの型からは破れない（Apply への分岐が存在しない）。
type Builder struct {
	intentRevision   string
	renderedRevision string
	ring             DriftSummaryProvider
	drifted          []DriftedItem
	converged        int
	convergedIDs     []core.ID
	unobservable     int
	unobservableList []UnobservableItem
	listErrors       int
	unmanaged        []core.ID
	undetectable     []UndetectableItem
	workflowHeld     []WorkflowHeldItem
	oscillating      []OscillatingItem
	daemon           *DaemonHealth
	daemonNote       string
	daemonStale      bool
	freeze           *freeze.Freeze
	freezeNote       string
}

// NewBuilder は指定した版情報と last-seen リングでビルダを作る。
// ring が nil の場合、last-seen 注記は「前周スナップショットなし」固定になる。
func NewBuilder(intentRevision, renderedRevision string, ring DriftSummaryProvider) *Builder {
	return &Builder{
		intentRevision:   intentRevision,
		renderedRevision: renderedRevision,
		ring:             ring,
	}
}

// AddDrifted は 1 つの drifted リソースを記録する。curFields は当該リソースの
// 「今周」観測フィールド（last-seen リングとの比較に使う）。cause は diff エンジンが
// 付した Op.Cause。
func (b *Builder) AddDrifted(id core.ID, cause core.CauseTag, tier Tier, diagnostic string, curFields map[string]string, fields ...core.FieldDiff) {
	lastSeen := "前周スナップショットなし（初回または再起動後・S3 非永続）"
	if b.ring != nil {
		lastSeen = b.ring.DriftSummary(id.Local, curFields)
	}
	b.drifted = append(b.drifted, DriftedItem{
		ID:         id,
		Cause:      cause,
		Tier:       tier,
		LastSeen:   lastSeen,
		Diagnostic: diagnostic,
		Fields:     fields,
	})
}

// AddMissing は、宣言されているのに現実に無いリソース（drift の 1 種）を記録する
// （#118）。引数は AddDrifted と同じ。
func (b *Builder) AddMissing(id core.ID, cause core.CauseTag, tier Tier, diagnostic string, curFields map[string]string, fields ...core.FieldDiff) {
	b.AddDrifted(id, cause, tier, diagnostic, curFields, fields...)
	b.drifted[len(b.drifted)-1].Missing = true
}

// AddConverged は適合（drift なし）だったリソースを 1 つ数える。
func (b *Builder) AddConverged() { b.converged++ }

// AddConvergedID は適合だったリソースを ID つきで 1 つ数える（#118）。
func (b *Builder) AddConvergedID(id core.ID) {
	b.converged++
	b.convergedIDs = append(b.convergedIDs, id)
}

// AddUnobservable は観測不能だったリソースを 1 つ数える（unknown ≠ absent）。
func (b *Builder) AddUnobservable() { b.unobservable++ }

// AddUnobservableReason は観測できなかったリソースを理由つきで 1 つ数える（#117）。
// reason は Read のエラー文、または Availability。
func (b *Builder) AddUnobservableReason(id core.ID, reason string) {
	b.unobservable++
	b.unobservableList = append(b.unobservableList, UnobservableItem{ID: id, Reason: reason})
}

// NoteListError は provider の List がエラーだったことを注記し、観測できないものとして
// 数える（#117）。注記は unmanaged 検出不可の注記として出る。
func (b *Builder) NoteListError(providerName string, err error) {
	b.listErrors++
	b.undetectable = append(b.undetectable, UndetectableItem{
		Provider: providerName,
		Kind:     UndetectableListError,
		Reason:   err.Error(),
		Note:     fmt.Sprintf("unmanaged 検出不可（List エラー: %s: %v）", providerName, err),
	})
}

// AddUnmanaged は「宣言外・実在」リソース（現実に在るが intent に無い）を 1 つ
// 記録する（FIX #2, Sc7a647-3）。既定 DeletionPolicy では prune 対象外＝削除されない。
func (b *Builder) AddUnmanaged(id core.ID) {
	b.unmanaged = append(b.unmanaged, id)
}

// NoteListUnsupported は List 非対応で unmanaged 検出が「できなかった」provider を
// 注記する。graceful degradation：検出不能を「宣言外ゼロ」と混同させず、現実を
// 過少報告しないための可視化。
func (b *Builder) NoteListUnsupported(providerName string) {
	b.undetectable = append(b.undetectable, UndetectableItem{
		Provider: providerName,
		Kind:     UndetectableListUnsupported,
		Note:     fmt.Sprintf("unmanaged 検出不可（List 非対応 provider: %s）", providerName),
	})
}

// NoteListIncomplete は List が完全でなく（Partial/Unknown）unmanaged 判定を控えた
// provider を注記する。status は List の Status。
func (b *Builder) NoteListIncomplete(providerName, status string) {
	b.undetectable = append(b.undetectable, UndetectableItem{
		Provider: providerName,
		Kind:     UndetectableListIncomplete,
		Reason:   "status=" + status,
		Note:     fmt.Sprintf("unmanaged 検出不可（List 未完全: %s: status=%s）", providerName, status),
	})
}

// NoteScopeMismatch は List の ID の scope が宣言の scope と食い違い、その
// provider の unmanaged 判定を控えたことを注記する（#169、§5.4）。List エラーと
// 同じく観測できなかったものに数える（終了コード 3）。
func (b *Builder) NoteScopeMismatch(providerName, detail string) {
	b.listErrors++
	b.undetectable = append(b.undetectable, UndetectableItem{
		Provider: providerName,
		Kind:     UndetectableScopeMismatch,
		Reason:   detail,
		Note:     fmt.Sprintf("unmanaged 検出不可（scope の食い違い: %s: %s）", providerName, detail),
	})
}

// NoteScopeUndecided は --claim のとき、scope が決まらず claim の内外を決め
// られなかった宣言外の ID の件数を注記する（#169）。黙って隠さない。
func (b *Builder) NoteScopeUndecided(providerName string, n int) {
	b.undetectable = append(b.undetectable, UndetectableItem{
		Provider: providerName,
		Kind:     UndetectableScopeUndecided,
		Reason:   fmt.Sprintf("count=%d", n),
		Note: fmt.Sprintf("unmanaged 検出不可（scope が決まらない %d 件: %s。provider の scope の設定か宣言で決める）",
			n, providerName),
	})
}

// AddWorkflowHeld は Workflow に任せている対象を 1 つ記録する（#89）。
func (b *Builder) AddWorkflowHeld(id core.ID, workflow, stage string) {
	b.workflowHeld = append(b.workflowHeld, WorkflowHeldItem{ID: id, Workflow: workflow, Stage: stage})
}

// AddDriftedItem は組み立て済みの drifted の項目をそのまま記録する（デーモンの
// 読み取り専用の口が、前の周の項目を持ち越すのに使う、#136）。
func (b *Builder) AddDriftedItem(it DriftedItem) { b.drifted = append(b.drifted, it) }

// AddOscillating は Apply を止めたリソースを 1 つ記録する（#166）。
func (b *Builder) AddOscillating(it OscillatingItem) { b.oscillating = append(b.oscillating, it) }

// SetDaemonHealth は常駐デーモンが書いた健全性（#15）をレポートに載せる。
func (b *Builder) SetDaemonHealth(h DaemonHealth) { b.daemon = &h }

// SetFreeze は --freeze-state のファイルで効いている freeze（#184）を載せる。
func (b *Builder) SetFreeze(f *freeze.Freeze) { b.freeze = f }

// NoteFreezeUnavailable は --freeze-state を読めなかったことを注記する。
func (b *Builder) NoteFreezeUnavailable(note string) { b.freezeNote = note }

// MarkDaemonStale は、デーモンの健全性ファイルが古い（DaemonHealth.Stale）ことを記録する。
func (b *Builder) MarkDaemonStale() { b.daemonStale = true }

// NoteDaemonHealthUnavailable はデーモン健全性を読めなかったことを注記する。
func (b *Builder) NoteDaemonHealthUnavailable(note string) { b.daemonNote = note }

// Build は蓄積した項目からレポートを組み立てる。drifted 一覧は Local ID 昇順に
// 整列される（map 由来の非決定順を排除）。
func (b *Builder) Build() Report {
	sorted := make([]DriftedItem, len(b.drifted))
	copy(sorted, b.drifted)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].ID.Local < sorted[j].ID.Local
	})
	unmanaged := make([]core.ID, len(b.unmanaged))
	copy(unmanaged, b.unmanaged)
	sort.Slice(unmanaged, func(i, j int) bool {
		return unmanaged[i].String() < unmanaged[j].String()
	})
	undetectable := make([]UndetectableItem, len(b.undetectable))
	copy(undetectable, b.undetectable)
	sort.SliceStable(undetectable, func(i, j int) bool { return undetectable[i].Provider < undetectable[j].Provider })
	notes := make([]string, 0, len(undetectable))
	for _, u := range undetectable {
		notes = append(notes, u.Note)
	}
	sort.Strings(notes)
	converged := make([]core.ID, len(b.convergedIDs))
	copy(converged, b.convergedIDs)
	sort.Slice(converged, func(i, j int) bool { return converged[i].String() < converged[j].String() })
	held := make([]WorkflowHeldItem, len(b.workflowHeld))
	copy(held, b.workflowHeld)
	sort.Slice(held, func(i, j int) bool { return held[i].ID.Local < held[j].ID.Local })
	unobs := make([]UnobservableItem, len(b.unobservableList))
	copy(unobs, b.unobservableList)
	sort.Slice(unobs, func(i, j int) bool { return unobs[i].ID.String() < unobs[j].ID.String() })
	osc := make([]OscillatingItem, len(b.oscillating))
	copy(osc, b.oscillating)
	sort.Slice(osc, func(i, j int) bool { return osc[i].ID.String() < osc[j].ID.String() })
	return Report{
		Oscillating:       osc,
		Unobservable:      unobs,
		ListErrorCount:    b.listErrors,
		WorkflowHeld:      held,
		IntentRevision:    b.intentRevision,
		RenderedRevision:  b.renderedRevision,
		Drifted:           sorted,
		ConvergedCount:    b.converged,
		Converged:         converged,
		Undetectable:      undetectable,
		UnobservableCount: b.unobservable,
		Unmanaged:         unmanaged,
		UnmanagedNotes:    notes,
		Daemon:            b.daemon,
		DaemonNote:        b.daemonNote,
		DaemonStale:       b.daemonStale,
		Freeze:            b.freeze,
		FreezeNote:        b.freezeNote,
	}
}

// causeLabel は原因タグに日本語の短い説明を添える。
func causeLabel(c core.CauseTag) string {
	switch c {
	case core.CauseTagA:
		return "A（意図: intent 変更）"
	case core.CauseTagB:
		return "B（ドリフト）"
	case core.CauseTagC:
		return "C（ghost diff: provider 更新）"
	case core.CauseTagD:
		return "D（波及: 依存変更）"
	case core.CauseTagE:
		return "E（外部入力変更）"
	default:
		return string(c)
	}
}

// String は Report を "sakutei status" の標準出力向けに整形する。
//
// read-only 契約: この整形は CREATE/UPDATE/DELETE や "applied" といった Apply 系の
// 文言を一切含めない。drift を「一覧」するだけで、実行行は出さない。
func (r Report) String() string {
	var sb strings.Builder
	sb.WriteString("sakutei status — 適合レポート（conformance report; read-only, §26.4）\n")
	fmt.Fprintf(&sb, "intent_revision=%s rendered_revision=%s\n\n", r.IntentRevision, r.RenderedRevision)

	if len(r.Drifted) == 0 {
		sb.WriteString("適合（drift なし）: 宣言どおり在る。\n")
	} else {
		fmt.Fprintf(&sb, "drifted: %d リソース\n\n", len(r.Drifted))
		for _, it := range r.Drifted {
			fmt.Fprintf(&sb, "  DRIFTED %s\n", it.ID)
			fmt.Fprintf(&sb, "    原因タグ: %s\n", causeLabel(it.Cause))
			fmt.Fprintf(&sb, "    provider 階級: %s\n", it.Tier.String())
			fmt.Fprintf(&sb, "    前周: %s\n", it.LastSeen)
			if it.Diagnostic != "" {
				fmt.Fprintf(&sb, "    理由: %s\n", it.Diagnostic)
			}
			if len(it.Fields) > 0 {
				// #111: どのフィールドがどうずれているか（plan と同じ「現実 → 宣言」の向き）。
				sb.WriteString("    ずれ（現実 → 宣言）:\n")
				for _, fd := range it.Fields {
					switch {
					case fd.From == "":
						fmt.Fprintf(&sb, "      + %s: (無い) → %q\n", fd.Field, fd.To)
					case fd.To == "":
						fmt.Fprintf(&sb, "      - %s: %q → (無い)\n", fd.Field, fd.From)
					default:
						fmt.Fprintf(&sb, "      ~ %s: %q → %q\n", fd.Field, fd.From, fd.To)
					}
				}
			}
			sb.WriteString("\n")
		}
	}

	// #117 (§3): 観測できなかったリソース。drifted にも converged にも数えない
	// （unknown ≠ absent）。1 つの失敗で報告全体を止めず、理由をリソースごとに出す。
	if len(r.Unobservable) > 0 {
		sb.WriteString("\n")
		fmt.Fprintf(&sb, "観測できない（drift とも適合とも数えない）: %d リソース\n", len(r.Unobservable))
		for _, it := range r.Unobservable {
			fmt.Fprintf(&sb, "  UNOBSERVABLE %s\n", it.ID)
			fmt.Fprintf(&sb, "    理由: %s\n", it.Reason)
		}
	}

	// #89 (§6.3): Workflow に任せている対象。drifted としては出さない（収束ループは
	// 触らない）。
	if len(r.WorkflowHeld) > 0 {
		sb.WriteString("\n")
		fmt.Fprintf(&sb, "Workflow に任せる（収束ループは触らない）: %d リソース\n", len(r.WorkflowHeld))
		for _, it := range r.WorkflowHeld {
			fmt.Fprintf(&sb, "  WORKFLOW %s\n", it.ID)
			fmt.Fprintf(&sb, "    workflow: %s\n", it.Workflow)
			fmt.Fprintf(&sb, "    段階: %s\n", it.Stage)
		}
	}

	// #166 (§11.1 Tier3): Apply を止めたリソース。drifted としては出さない。
	if len(r.Oscillating) > 0 {
		sb.WriteString("\n")
		fmt.Fprintf(&sb, "Apply を止めた（oscillating: Apply は成功を返したが現実が変わらない）: %d リソース\n", len(r.Oscillating))
		for _, it := range r.Oscillating {
			writeOscillating(&sb, "  ", it)
		}
	}

	// FIX #2 (Sc7a647-3): unmanaged（宣言外・実在）セクション。現実に在るが intent に
	// 無いリソースを列挙する。既定 DeletionPolicy（ExplicitAbsentOnly）では prune 対象外
	// ＝ status では削除しない（read-only；「確信なき減算はしない」§12.2, ADR-0005）。
	if len(r.Unmanaged) > 0 {
		sb.WriteString("\n")
		fmt.Fprintf(&sb, "unmanaged（宣言外・実在・prune 対象外）: %d リソース\n", len(r.Unmanaged))
		sb.WriteString("  注記: これらは現実に在るが intent に宣言が無い。既定（ExplicitAbsentOnly）では\n")
		sb.WriteString("  自動削除されない（prune 対象外・read-only）。削除するなら intent 側で明示する。\n")
		for _, id := range r.Unmanaged {
			fmt.Fprintf(&sb, "  UNMANAGED %s\n", id)
		}
	}
	// graceful degradation: 検出不能 provider の注記（List 非対応 / List エラー）。
	// 検出不能を「宣言外ゼロ」と混同させないため、常に可視化する。
	if len(r.UnmanagedNotes) > 0 {
		sb.WriteString("\n")
		for _, note := range r.UnmanagedNotes {
			fmt.Fprintf(&sb, "  %s\n", note)
		}
	}

	// #15 (§9.2): 外部入力不達による連続スキップ。デーモンが静かに止まり続けて
	// いることを status でも見えるようにする。
	if r.Daemon != nil {
		sb.WriteString("\n")
		fmt.Fprintf(&sb, "デーモン（cycle %d, %s 時点）: 外部入力不達による連続スキップ consecutive_unreachable_skips=%d（上限 %d）\n",
			r.Daemon.Cycle, r.Daemon.UpdatedAt.Format(time.RFC3339), r.Daemon.ConsecutiveUnreachableSkips, r.Daemon.UnreachableSkipLimit)
		if r.Daemon.Prolonged() {
			sb.WriteString("  警告: ExternalInputUnreachableProlonged — 外部の権威が長く届かず、収束が止まっている（§9.2）。\n")
		}
		if r.Daemon.ConsecutiveErrorCycles > 0 {
			fmt.Fprintf(&sb, "  警告: 周がエラーで終わり続けている consecutive_error_cycles=%d — 収束が止まっている。最後のエラー: %s\n",
				r.Daemon.ConsecutiveErrorCycles, r.Daemon.LastError)
		}
		if r.DaemonStale {
			fmt.Fprintf(&sb, "  警告: このファイルは周期（%s）の数倍のあいだ更新されていない — デーモンが止まっているかもしれない。\n",
				r.Daemon.Interval)
		}
		writeDaemonPlacement(&sb, *r.Daemon)
	} else if r.DaemonNote != "" {
		sb.WriteString("\n")
		fmt.Fprintf(&sb, "  %s\n", r.DaemonNote)
	}

	// #184: the freeze state file's freeze.
	if r.Freeze != nil {
		sb.WriteString("\n")
		writeFreeze(&sb, *r.Freeze, "")
	} else if r.FreezeNote != "" {
		sb.WriteString("\n")
		fmt.Fprintf(&sb, "  %s\n", r.FreezeNote)
	}

	fmt.Fprintf(&sb, "\n要約: drifted=%d converged=%d unobservable=%d unmanaged=%d",
		len(r.Drifted), r.ConvergedCount, r.UnobservableCount, len(r.Unmanaged))
	if len(r.WorkflowHeld) > 0 {
		fmt.Fprintf(&sb, " workflow=%d", len(r.WorkflowHeld))
	}
	if len(r.Oscillating) > 0 {
		fmt.Fprintf(&sb, " oscillating=%d", len(r.Oscillating))
	}
	sb.WriteString("\n")
	return sb.String()
}

// writeOscillating は Apply を止めたリソース 1 つを書く（#166）。
func writeOscillating(sb *strings.Builder, indent string, it OscillatingItem) {
	fmt.Fprintf(sb, "%sOSCILLATING %s\n", indent, it.ID)
	fmt.Fprintf(sb, "%s  ずれ: %s\n", indent, it.Diff)
	fmt.Fprintf(sb, "%s  止めたわけ: %s\n", indent, it.Note)
	if !it.Since.IsZero() {
		fmt.Fprintf(sb, "%s  止めた時刻: %s\n", indent, it.Since.UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(sb, "%s  解く: desired か現実が変わる・現実が宣言どおりになる・sakutei apply-check release\n", indent)
}

// writeFreeze は効いている global freeze（#184）を 2 行で書く。
func writeFreeze(sb *strings.Builder, f freeze.Freeze, indent string) {
	until := "期限なし（sakutei freeze end で解く。--freeze なら実行が終わるまで）"
	if !f.Until.IsZero() {
		until = f.Until.UTC().Format(time.RFC3339) + " まで（過ぎたら解ける）"
	}
	fmt.Fprintf(sb, "%sFROZEN: global freeze（§14.3）— alert-only、drift の検知は続ける。誰が %s・入口 %s・%s から・%s\n%s  理由: %s\n",
		indent, f.By, f.Source, f.Since.UTC().Format(time.RFC3339), until, indent, f.Reason)
}

// writeDaemonPlacement はデーモンの担当（claim）・fencing の世代・git の配布の
// 状態を書く（#136。あるときだけ）。
func writeDaemonPlacement(sb *strings.Builder, h DaemonHealth) {
	switch {
	case h.Idle:
		sb.WriteString("  claim: なし（このシャードは今何も担当していない、§17.7）\n")
	case h.Claim != "":
		fmt.Fprintf(sb, "  claim: %s\n", h.Claim)
	}
	if h.Shard != "" {
		fmt.Fprintf(sb, "  shard: %s\n", h.Shard)
	}
	for _, f := range h.Fencing {
		if f.Fenced {
			fmt.Fprintf(sb, "  fencing: %s generation=%d holder=%s — fence されている（別のシャードが新しい世代を持つ。書かない）\n", f.Scope, f.Generation, f.Holder)
		} else {
			fmt.Fprintf(sb, "  fencing: %s generation=%d holder=%s\n", f.Scope, f.Generation, f.Holder)
		}
	}
	if m := h.Mirror; m != nil {
		fetched := "まだ取れていない"
		if m.FetchedAt != nil {
			fetched = m.FetchedAt.UTC().Format(time.RFC3339)
		}
		rev := m.Revision
		if rev == "" {
			rev = "(なし)"
		}
		fmt.Fprintf(sb, "  mirror: %s revision=%s 最後に取れた時刻=%s\n", m.Remote, rev, fetched)
		if !m.Reachable {
			fmt.Fprintf(sb, "  警告: intent の取得先に届いていない — 手元の写しで収束を続けている（§17.4）: %s\n", m.Error)
		}
	}
	if len(h.ApplyStopped) > 0 {
		fmt.Fprintf(sb, "  警告: Apply を止めている（oscillating: Apply は成功を返したが現実が変わらない、§11.1）: %d リソース\n", len(h.ApplyStopped))
		for _, a := range h.ApplyStopped {
			fmt.Fprintf(sb, "    OSCILLATING %s: %s — %s（%s から）\n", a.Resource, a.Diff, a.Note, a.Since.UTC().Format(time.RFC3339))
		}
	}
	if len(h.RuntimeTightened) > 0 {
		fmt.Fprintf(sb, "  注意: 起動の旗が intent の runtime: を一時的に締めている（§26.7）: %d 引数\n", len(h.RuntimeTightened))
		for _, t := range h.RuntimeTightened {
			fmt.Fprintf(sb, "    TIGHTENED %s: 旗 --%s=%s / intent %s=%s — 旗を外すと宣言に戻る\n", t.Key, t.Flag, t.FlagValue, t.IntentPath(), t.IntentValue)
		}
	}
	if h.Freeze != nil {
		writeFreeze(sb, *h.Freeze, "  ")
	}
	if h.RuntimeChanged != "" {
		fmt.Fprintf(sb, "  警告: 起動したあとに intent の runtime: が変わった — 起動したときの値で回っている。起こし直すと取り込む（§26.7）: %s\n", h.RuntimeChanged)
	}
	for _, n := range h.Notify {
		fmt.Fprintf(sb, "  notify: %s %s delivered=%d failed=%d rejected=%d dropped=%d pending=%d", n.Sink, n.URL,
			n.Delivered, n.Failed, n.Rejected, n.Dropped, n.Pending)
		if n.Sink == "alertmanager" {
			fmt.Fprintf(sb, " active=%d", n.Active)
		}
		sb.WriteString("\n")
		if n.Down() {
			fmt.Fprintf(sb, "  警告: 通知の宛先 %s に届いていない（続けて %d 回）— 送り直している。他の宛先には送り続ける（§22.3）: %s\n",
				n.URL, n.ConsecutiveFailures, n.LastError)
		}
	}
}

// Incomplete は、報告はできたが観測できないもの（観測不能なリソース、または List
// エラーの provider）があったこと（#117）。status はこのとき終了コード 3 を返す。
func (r Report) Incomplete() bool {
	return r.UnobservableCount > 0 || r.ListErrorCount > 0
}

// 静的な型アサーション: *lastseen.ResourceRing が DriftSummaryProvider を満たすこと
// をコンパイル時に保証する（リング API の使用が正しいことの担保）。
var _ DriftSummaryProvider = (*lastseen.ResourceRing)(nil)
