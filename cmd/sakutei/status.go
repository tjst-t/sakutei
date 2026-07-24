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

package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/tjst-t/sakutei/internal/cause"
	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/lastseen"
	"github.com/tjst-t/sakutei/internal/loader"
	"github.com/tjst-t/sakutei/internal/render"
	"github.com/tjst-t/sakutei/internal/statusreport"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/registry"
)

// runStatus implements "sakutei status" (Story Sc7a647-3, §26.4).
//
// # 位置づけ
//
// status は「宣言どおり在るか」の適合レポート（conformance report / drifted 一覧）を
// read-only に返すデバッグ体験である。収束ループ内で常時判定されている conformance
// （§26.4）を、人向け・監査向けに切り出したもの。plan と同じく現実を Observe し
// desired と Diff するが、plan が「これから何を変えるか（計画）」を示すのに対し
// status は「今、宣言どおり在るか（適合）」を報告する。
//
// health/canary（稼働性・段階ロールアウトの健全性）とは別物であり混同しない
// —— status が答えるのは drift の有無だけである。
//
// # read-only 契約（構造保証）
//
// status は Apply（CREATE/UPDATE/DELETE）に一切分岐しない。Read/List（観測）と Diff
// と整形しか呼ばない。plan が持つ generated/ への書き出し（WriteGenerated）や
// cause-state の書き戻しも status では行わない —— 現実（state/ruleset/zone ファイル）
// も generated/ も cause-state も、status 実行前後で byte-for-byte 不変である。
// この read-only 性は tests/acceptance/status_test.go で byte 比較により証明される。
//
// # 再利用
//
// plan.go の read-only な観測+diff 配管（loader.LoadDir → render.ExpandAll →
// resolveProviders → provider.Read → diff.Engine.Diff → cause.Classify）を再利用する。
// plan と異なるのは (a) Apply/generated 書き出し/cause-state 書き戻しをしない、
// (b) 出力が「計画」ではなく「適合レポート」である点のみ。
//
// # 出力要素（§26.4, §10.2, §5.3）
//
//   - drifted リソース一覧 + 原因タグ A〜E（diff エンジンが付した Op.Cause を採録）
//   - provider 階級（Capabilities tier: TrueObserve/List/BulkObserve/Pruneable/
//     ProviderVersion）
//   - last-seen リング由来の「前周は X だった」（internal/lastseen; 空なら
//     「前周スナップショットなし」・S3 非永続）
//   - 下流波及 D（依存変更由来。cause D が付いた項目として一覧に反映）
//
// Exit codes:
//
//	0 — status completed (適合でも drift ありでも 0; 観測は成功した)
//	1 — error (flag parse error, intent load failure, observe error, etc.)
func runStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	intentDir := fs.String("intent", "", "directory containing intent JSON files (required)")
	statePath := fs.String("state", "sakutei-fake-state.json",
		"path to the fake provider state file (default: sakutei-fake-state.json)")
	zonePath := fs.String("zone", "",
		"path to the dnszone provider zone file (default: falls back to --state)")
	// last-seen リングは S3・非永続（in-memory）。単一の status 呼び出しでは通常
	// 「前周」は存在しない（初回扱い）。前周比較の機構は statusreport の report-builder
	// 層で単体テストする。--cause-state と同様、リングの永続化は行わない。
	rulesetPath := fs.String("ruleset", "",
		"path to the nftables provider ruleset file (default: falls back to --state)")

	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	if *intentDir == "" {
		fmt.Fprintln(os.Stderr, "sakutei status: --intent <dir> is required")
		fs.Usage()
		os.Exit(1)
	}

	// desired をロード（plan と同一の loader）。
	intentResources, rev, err := loader.LoadDir(*intentDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sakutei status: load intent: %v\n", err)
		os.Exit(1)
	}

	// render フェーズ2（合成リソースの展開）を in-memory で実施。
	// status は read-only なので generated/ には一切書き出さない（plan の
	// WriteGenerated は呼ばない）。
	expRegistry := render.DefaultRegistry()
	expanded, expandErr := render.ExpandAll(expRegistry, intentResources)
	if expandErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei status: render phase 2: %v\n", expandErr)
		os.Exit(1)
	}

	// 非合成リソースのみが Diff 対象（合成親は子で代表される）。
	desired := make([]diff.DesiredResource, 0, len(expanded))
	for _, r := range expanded {
		if !r.IsComposite {
			desired = append(desired, r.DesiredResource)
		}
	}

	// provider を registry 経由で解決（plan と同一の resolveProviders 配管）。
	reg := registry.DefaultRegistry()
	providerConfig := map[string]string{"state": *statePath}
	if *zonePath != "" {
		providerConfig["zone"] = *zonePath
	}
	if *rulesetPath != "" {
		providerConfig["ruleset"] = *rulesetPath
	}
	providers, resolveErr := resolveProviders(reg, desired, providerConfig, "sakutei status")
	if resolveErr != nil {
		fmt.Fprintln(os.Stderr, resolveErr)
		os.Exit(1)
	}

	// provider ごとの diff エンジンを用意（各 provider は固有 Schema を持つ）。
	engines := make(map[string]*diff.Engine, len(providers))
	for name, p := range providers {
		engines[name] = diff.New(p.Schema())
	}

	// cause 分類のための版・バージョン信号を組み立てる。status は cause-state を
	// 読み書きしない（read-only）ため、前周版は「今周と同一」として扱う。
	//
	// これは意味的に正しい: status は「今コミットされている（安定した）intent に対し、
	// 現実が宣言どおり在るか」を報告する適合レポートである。intent は安定
	// （prev==cur）とみなすので、diff がある = 現実がドリフトした ＝ 原因 B が既定になる
	// （§10.2 の消去法: intent/rendered/provider_version がすべて安定なのに diff が
	// あれば B）。合成親由来の子は下流波及 D（UpstreamChanged）が B より優先される。
	// これにより「新規 apply（A）」ではなく「宣言どおり在るか（B/D）」という
	// conformance の問いに沿ったタグ付けになる。原因タグは diff エンジン
	// （cause パッケージ）が付す core.CauseTag をそのまま採録する（分類ロジックを fork
	// しない）。
	curProviderVersion := joinedProviderVersions(providers)

	// 下流波及 D: 合成親 → 子の対応から、親 desired が変わった子を特定する。
	// status は cause-state を持たないため親の「前周」は不明だが、合成展開が
	// 起きた（親が子を生んだ）ケースでは子は upstream 由来として cause D 対象になる。
	// これにより「依存変更由来の波及」を一覧に反映する。
	parentToChildren := render.BuildParentChildMap(expanded)
	childrenWithUpstreamChange := make(map[string]bool)
	for _, childIDs := range parentToChildren {
		for _, childID := range childIDs {
			childrenWithUpstreamChange[childID] = true
		}
	}

	baseCauseCtx := cause.CauseContext{
		// prev == cur: intent は安定とみなす（上記コメント参照）。diff があれば
		// 消去法で B（ドリフト）。合成親由来の子は UpstreamChanged=true で D が優先。
		IntentPrevRevision:   rev.IntentRevision,
		IntentCurRevision:    rev.IntentRevision,
		RenderedPrevRevision: rev.RenderedRevision,
		RenderedCurRevision:  rev.RenderedRevision,
		ProviderPrevVersion:  curProviderVersion,
		ProviderCurVersion:   curProviderVersion,
	}

	// last-seen リング（S3・非永続・in-memory）。単一 status 呼び出しでは前周は
	// 通常存在しない。前周比較の機構は statusreport の report-builder 層でテストする。
	ring := lastseen.NewResourceRing(lastseen.DefaultRingSize)

	builder := statusreport.NewBuilder(rev.IntentRevision, rev.RenderedRevision, ring)

	ctx := context.Background()

	for _, res := range desired {
		fp := providers[res.ID.Provider]
		engine := engines[res.ID.Provider]

		// ADR-0002: リソースごとに独立して Observe（read-only Read）。
		obs, readErr := fp.Read(ctx, res.ID.Local)
		if readErr != nil {
			fmt.Fprintf(os.Stderr, "sakutei status: observe %s: %v\n", res.ID, readErr)
			os.Exit(1)
		}
		if obs.Availability != core.AvailabilityOk {
			// unknown ≠ absent（§3）: 観測不能は drift とも converged とも数えない。
			builder.AddUnobservable()
			continue
		}

		curFields := stateToStringMap(obs.State)

		// secret フィールドの平文混入は plan と同じく致命的（read-only でも報告前に検証）。
		if valErr := engine.ValidateDesired(res); valErr != nil {
			fmt.Fprintf(os.Stderr, "sakutei status: %v\n", valErr)
			os.Exit(1)
		}

		resCauseCtx := baseCauseCtx
		if childrenWithUpstreamChange[res.ID.Local] {
			resCauseCtx.UpstreamChanged = true
		}

		plan := engine.Diff(obs, res, resCauseCtx)

		// drift あり: 原因タグ・provider 階級・last-seen 注記を添えて一覧へ。
		// last-seen 注記は「この観測を記録する前の」リング状態と比較する。単一実行では
		// 前周が無いため「前周スナップショットなし」になる（AddDrifted 内で今のリングと
		// 比較 → その後に Record する順序でこれを保証する）。前周比較の機構自体は
		// report-builder 層でテストする（観測を 2 回与える）。
		if len(plan.Ops) > 0 {
			op := plan.Ops[0]
			tier := statusreport.TierFromCapabilities(fp.Capabilities())
			builder.AddDrifted(res.ID, op.Cause, tier, op.Diagnostic, curFields)
		} else {
			// 適合（drift なし）。
			builder.AddConverged()
		}

		// 今周の観測を last-seen リングへ記録（S3・非永続）。AddDrifted の後に記録する
		// ことで、当該項目の前周注記は「今周観測を含まない」正しい前周状態を反映する。
		ring.Record(res.ID.Local, lastseen.Snapshot{
			LocalID:    res.ID.Local,
			ObservedAt: obs.ObservedAt,
			Fields:     curFields,
		})
	}

	// 適合レポートを整形して出力。Apply（CREATE/UPDATE/DELETE/applied）系の行は
	// 一切含まれない —— drift を「一覧」するだけで実行はしない（read-only 契約）。
	report := builder.Build()
	fmt.Print(report.String())
	os.Exit(0)
}

// stateToStringMap は観測 State を last-seen リング用の文字列マップに変換する。
// diff エンジンと同じ %v 表現を用いるので、リング上の前周比較が diff の見え方と一致する。
func stateToStringMap(state core.State) map[string]string {
	m := make(map[string]string, len(state))
	for name, tv := range state {
		m[name] = fmt.Sprintf("%v", tv.Value)
	}
	return m
}
