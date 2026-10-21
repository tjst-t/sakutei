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

	"github.com/tjst-t/sakutei/internal/lastseen"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
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
	// UnobservableCount は観測不能（Availability != Ok）だったリソース数。
	// unknown ≠ absent（§3）: これらは drifted にも converged にも数えない。
	UnobservableCount int
	// Unmanaged は「宣言外・実在」リソース一覧（現実に在るが intent に無い）。
	// FIX #2 (Sc7a647-3): provider の List でリアルを列挙し、宣言集合との差分から
	// 求める。既定の DeletionPolicy（ExplicitAbsentOnly）では prune 対象外＝削除され
	// ない（「確信なき減算はしない」）。ID 文字列昇順で整列済み。
	Unmanaged []core.ID
	// UnmanagedNotes は unmanaged 検出が「できなかった」provider の注記
	// （List 非対応 / List エラー）。検出不能を「宣言外ゼロ」と混同しないための
	// graceful degradation の可視化。
	UnmanagedNotes []string
	// Daemon は常駐デーモンの健全性（#15）。status に --daemon-status が無ければ nil。
	Daemon *DaemonHealth
	// DaemonNote はデーモン健全性を読めなかったときの注記（読めないことを隠さない）。
	DaemonNote string
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
	unobservable     int
	unmanaged        []core.ID
	unmanagedNotes   []string
	daemon           *DaemonHealth
	daemonNote       string
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
func (b *Builder) AddDrifted(id core.ID, cause core.CauseTag, tier Tier, diagnostic string, curFields map[string]string) {
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
	})
}

// AddConverged は適合（drift なし）だったリソースを 1 つ数える。
func (b *Builder) AddConverged() { b.converged++ }

// AddUnobservable は観測不能だったリソースを 1 つ数える（unknown ≠ absent）。
func (b *Builder) AddUnobservable() { b.unobservable++ }

// AddUnmanaged は「宣言外・実在」リソース（現実に在るが intent に無い）を 1 つ
// 記録する（FIX #2, Sc7a647-3）。既定 DeletionPolicy では prune 対象外＝削除されない。
func (b *Builder) AddUnmanaged(id core.ID) {
	b.unmanaged = append(b.unmanaged, id)
}

// NoteUnmanagedUndetectable は unmanaged 検出が「できなかった」provider の注記を
// 追加する（List 非対応、または List エラー）。graceful degradation：検出不能を
// 「宣言外ゼロ」と混同させず、現実を過少報告しないための可視化。
func (b *Builder) NoteUnmanagedUndetectable(note string) {
	b.unmanagedNotes = append(b.unmanagedNotes, note)
}

// SetDaemonHealth は常駐デーモンが書いた健全性（#15）をレポートに載せる。
func (b *Builder) SetDaemonHealth(h DaemonHealth) { b.daemon = &h }

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
	notes := make([]string, len(b.unmanagedNotes))
	copy(notes, b.unmanagedNotes)
	sort.Strings(notes)
	return Report{
		IntentRevision:    b.intentRevision,
		RenderedRevision:  b.renderedRevision,
		Drifted:           sorted,
		ConvergedCount:    b.converged,
		UnobservableCount: b.unobservable,
		Unmanaged:         unmanaged,
		UnmanagedNotes:    notes,
		Daemon:            b.daemon,
		DaemonNote:        b.daemonNote,
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
			sb.WriteString("\n")
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
	} else if r.DaemonNote != "" {
		sb.WriteString("\n")
		fmt.Fprintf(&sb, "  %s\n", r.DaemonNote)
	}

	fmt.Fprintf(&sb, "\n要約: drifted=%d converged=%d unobservable=%d unmanaged=%d\n",
		len(r.Drifted), r.ConvergedCount, r.UnobservableCount, len(r.Unmanaged))
	return sb.String()
}

// 静的な型アサーション: *lastseen.ResourceRing が DriftSummaryProvider を満たすこと
// をコンパイル時に保証する（リング API の使用が正しいことの担保）。
var _ DriftSummaryProvider = (*lastseen.ResourceRing)(nil)
