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

package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/cause"
	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/extowner"
	"github.com/tjst-t/sakutei/v2/internal/lastseen"
	"github.com/tjst-t/sakutei/v2/internal/listscope"
	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/render"
	"github.com/tjst-t/sakutei/v2/internal/revision"
	"github.com/tjst-t/sakutei/v2/internal/statusreport"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
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
//	0 — status completed and every resource was observed (適合でも drift ありでも 0)
//	1 — the report itself could not be made (flag parse error, intent load
//	    failure, provider resolution failure, etc.)
//	3 — the report was made, but some resource could not be observed (a Read
//	    error, Availability != ok, or a plan error such as a declared field
//	    whose API the target does not implement, #122) or a provider's List
//	    failed (#117).  1 is
//	    kept for "status itself failed" so existing callers are not broken.
func runStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: sakutei status --intent <dir> [flags]")
		fs.PrintDefaults()
		fmt.Fprintln(os.Stderr, `
Exit codes:
  0  every resource was observed and reported (drift does not change it)
  3  reported, but some resource could not be observed (Read error,
     availability != ok or a plan error) or a provider's List failed (#117)
  1  no report (flag error, intent load failure, provider resolution failure)`)
	}

	intentDir := fs.String("intent", "", "directory containing intent JSON files (required)")
	providerConfigFile := providerConfigFlag(fs)
	statePath := fs.String("state", "sakutei-fake-state.json",
		"path to the fake provider state file (default: sakutei-fake-state.json)")
	zonePath := fs.String("zone", "",
		"path to the dnszone provider zone file (default: falls back to --state)")
	zoneSOASerial := zoneSOASerialFlag(fs)
	zoneScope := zoneScopeFlag(fs)
	redfishConfig := redfishConfigFlag(fs)
	sqlmigrateConfig := sqlmigrateConfigFlag(fs)
	s3Config := s3ConfigFlag(fs)
	postgresqlConfig := postgresqlConfigFlag(fs)
	ansibleConfig := ansibleConfigFlag(fs)
	// last-seen リングは S3・非永続（in-memory）。単一の status 呼び出しでは通常
	// 「前周」は存在しない（初回扱い）。前周比較の機構は statusreport の report-builder
	// 層で単体テストする。--cause-state と同様、リングの永続化は行わない。
	rulesetPath := fs.String("ruleset", "",
		"path to the nftables provider ruleset file (default: falls back to --state)")
	rulesetScope := rulesetScopeFlag(fs)
	rulesetTarget := rulesetTargetFlag(fs)
	// Fix Sc7a647 cause-consistency: status may READ an optional cause-state file
	// to learn the previous cycle's revisions, so its cause tags AGREE with
	// plan/reconcile (which read the same file).  status MUST stay READ-ONLY: it
	// reads this file but NEVER writes it (no saveCauseState call).  Absent =
	// cold start (no history), the same convention plan/reconcile use.
	allocStore := fs.String("alloc-store", "",
		"#24: path to the allocation store file; status only looks allocations up, never allocates")
	allocURL := fs.String("alloc-url", "", allocURLUsage)
	allocWriteBack := fs.String("alloc-write-back", "", allocWriteBackUsage)
	causeStatePath := fs.String("cause-state", "",
		"OPTIONAL path to a cause-state JSON file to READ previous-cycle revisions for consistent cause tags (Sc7a647); status never writes it (read-only)")

	daemonStatus := fs.String("daemon-status", "",
		"OPTIONAL path to the daemon status file written by `sakutei --daemon-status` (#15): reports the consecutive cycles skipped because an external input was unreachable")

	workflowStore := fs.String("workflow-store", "",
		"OPTIONAL path to the daemon's workflow store (§6.3): shows the stage of each Workflow whose targets are left to it; status only reads it")

	// #111: --claim, the same scope-selector as reconcile (§17.1): a shard's
	// status reports only its claim (other shards' resources, unmanaged reality
	// and pending allocations are not its business, §19.8).
	claim := fs.String("claim", "",
		"scope-selector restricting the report to a claim, e.g. site=tokyo,cluster=prod (§17.1); empty=whole world")

	// #115: the daemon's inventory composition (§9.1, §9.2): without it the
	// inventory's resources would show as UNMANAGED.
	invFlags := addInventoryFlags(fs, true)

	// #118: machine-readable output for tools that collect status results.
	jsonOut := fs.Bool("json", false,
		"print the report as one JSON object on stdout (schema_version 1, see docs/status-json.md); exit codes are unchanged")

	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	soaSerial := checkZoneSOASerial("sakutei status", *zoneSOASerial)
	zScope := checkScope("sakutei status", "zone-scope", *zoneScope)
	rScope := checkScope("sakutei status", "ruleset-scope", *rulesetScope)
	rulesetTarget.check("sakutei status")
	claimSelector, claimErr := parseClaimSelector(*claim)
	if claimErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei status: invalid --claim %q: %v\n", *claim, claimErr)
		os.Exit(1)
	}

	if *intentDir == "" {
		fmt.Fprintln(os.Stderr, "sakutei status: --intent <dir> is required")
		fs.Usage()
		os.Exit(1)
	}

	// desired をロード（plan と同一の loader）。
	in, err := loader.LoadIntent(*intentDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sakutei status: load intent: %v\n", err)
		os.Exit(1)
	}
	// #115: render phase 1 — inventory composition, as in the daemon.
	var externalInputs []string
	if input := composeOneShotInventory("sakutei status", invFlags, &in, os.Stderr); input != "" {
		externalInputs = append(externalInputs, input)
	}
	intentResources, rev := in.Resources, in.Revision

	// #24: render phase 1 — allocation.  status is read-only: it only looks
	// up existing allocations (a request without one shows as pending).
	allocAuth, allocErr := openAllocStore(*allocStore, *allocURL)
	if allocErr == nil {
		allocAuth, allocErr = wrapWriteBack(allocAuth, *intentDir, *allocWriteBack)
	}
	if allocErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei status: %v\n", allocErr)
		os.Exit(1)
	}
	allocRes, allocErr := render.RenderAllocations(context.Background(), allocAuth, render.AllocLookupOnly, in, claimSelector)
	if allocErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei status: %v\n", allocErr)
		os.Exit(1)
	}
	intentResources = allocRes.Resources
	externalInputs = append(externalInputs, allocRes.Inputs...)
	if len(externalInputs) > 0 {
		rev.RenderedRevision = revision.ComputeRenderedRevision(core.NewScope(nil), rev.IntentRevision, externalInputs)
	}
	for _, pa := range allocRes.Pending {
		fmt.Fprintln(os.Stderr, pendingLine("status", pa))
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
	// #111: with --claim, an out-of-claim resource is not reported (like reconcile).
	desired := make([]diff.DesiredResource, 0, len(expanded))
	for _, r := range expanded {
		if !r.IsComposite && r.DesiredResource.ID.Scope.Matches(claimSelector) {
			desired = append(desired, r.DesiredResource)
		}
	}

	// provider を registry 経由で解決（plan と同一の resolveProviders 配管）。
	reg := openProviderSet("sakutei status", *providerConfigFile)
	providerConfig := map[string]string{"state": *statePath}
	if *zonePath != "" {
		providerConfig["zone"] = *zonePath
	}
	providerConfig["soa_serial"] = soaSerial
	providerConfig["zone_scope"] = zScope
	if *redfishConfig != "" {
		providerConfig["redfish_config"] = *redfishConfig
	}
	if *sqlmigrateConfig != "" {
		providerConfig["sqlmigrate_config"] = *sqlmigrateConfig
	}
	if *s3Config != "" {
		providerConfig["s3_config"] = *s3Config
	}
	if *postgresqlConfig != "" {
		providerConfig["postgresql_config"] = *postgresqlConfig
	}
	if *ansibleConfig != "" {
		providerConfig["ansible_config"] = *ansibleConfig
	}
	if *rulesetPath != "" {
		providerConfig["ruleset"] = *rulesetPath
	}
	providerConfig["ruleset_scope"] = rScope
	rulesetTarget.put(providerConfig)
	providers, resolveErr := resolveProviders(reg, desired, providerConfig, "sakutei status")
	if resolveErr == nil {
		resolveErr = checkExternalOwners(in.ExternalOwners, desired, providers, "sakutei status", os.Stderr)
	}
	if resolveErr != nil {
		fmt.Fprintln(os.Stderr, resolveErr)
		os.Exit(1)
	}

	// #164: 宣言が 1 つも無いが設定はされている provider も、宣言外の検出の
	// ためだけに組み立てる（List するだけで、Read・Diff・cause には加えない）。
	listOnly, listOnlyErrs := undeclaredListers(reg, providers, providerConfig)

	// provider ごとの diff エンジンを用意（各 provider は固有 Schema を持つ）。
	engines := make(map[string]*diff.Engine, len(providers))
	for name, p := range providers {
		engines[name] = diff.ForProvider(p)
	}

	// cause 分類のための版・バージョン信号を組み立てる。
	//
	// Fix Sc7a647 cause-consistency: 以前は「前周版 = 今周版（prev==cur）」を
	// ハードコードしていた。これだと cold-start CREATE（宣言されているのに実在
	// しない = drift/deletion）が prev==cur の安定扱いで B になる一方、plan/reconcile
	// は同じ状況で（--cause-state 無しの空 prev で）A を出し、両者が食い違っていた。
	//
	// 統一規約: status も plan/reconcile と同じ cold-start 規約を使う。
	//   - --cause-state ファイルが与えられ読めれば、その prev 版で比較する
	//     （plan/reconcile と同一の loadCauseState を READ するだけ・書き戻さない）。
	//   - 与えられなければ prev 版は空（"" = 履歴なし / cold start）。
	// これに ResourceObservedAbsent（リソースごとに obs.State==nil で設定）を
	// 組み合わせることで、cold-start CREATE=A・drift UPDATE=B が plan/reconcile と
	// 一致する（§10.2）。intent が安定（既知 prev == cur）なら消去法で B、合成親由来の
	// 子は下流波及 D（UpstreamChanged）が B より優先。原因タグは diff エンジン
	// （cause パッケージ）が付す core.CauseTag をそのまま採録する（分類ロジックを fork
	// しない）。
	curProviderVersion := joinedProviderVersions(providers)

	// Fix Sc7a647: READ (never write) the optional cause-state file so status's
	// prev revisions match plan/reconcile.  A load error is non-fatal: treat as
	// cold start (empty prev), the same as an absent file.
	prevState, csErr := loadCauseState(*causeStatePath)
	if csErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei status: warn: load cause-state %q: %v (treating as cold start)\n",
			*causeStatePath, csErr)
		prevState = causeStateFile{}
	}

	// 下流波及 D: 合成親 → 子の対応から、親 desired が変わった子を特定する。
	// cause-state に親ごとの rendered 寄与ハッシュ（#14）があれば、それと比べて
	// 変わった親の子だけを D 対象にする（plan/reconcile と同じ判定）。無ければ
	// 親の「前周」は不明なので、合成展開が起きた（親が子を生んだ）ケースでは
	// 子は upstream 由来として cause D 対象になる。
	parentToChildren := render.BuildParentChildMap(expanded)
	var childrenWithUpstreamChange map[string]bool
	if prevState.PrevParentRevisions != nil {
		childrenWithUpstreamChange = render.UpstreamChangedChildren(
			prevState.PrevParentRevisions, render.ParentRevisions(expanded), parentToChildren)
	} else {
		childrenWithUpstreamChange = make(map[string]bool)
		for _, childIDs := range parentToChildren {
			for _, childID := range childIDs {
				childrenWithUpstreamChange[childID] = true
			}
		}
	}

	// Fix Sc7a647: derive prev signals from the cause-state file (READ-only).
	// Absent/unreadable → empty prev (cold start), matching plan/reconcile.
	baseCauseCtx := cause.CauseContext{
		IntentPrevRevision:   prevState.PrevIntentRevision,
		IntentCurRevision:    rev.IntentRevision,
		RenderedPrevRevision: prevState.PrevRenderedRevision,
		RenderedCurRevision:  rev.RenderedRevision,
		ProviderPrevVersion:  prevState.PrevProviderVersion,
		ProviderCurVersion:   curProviderVersion,
	}

	// last-seen リング（S3・非永続・in-memory）。単一 status 呼び出しでは前周は
	// 通常存在しない。前周比較の機構は statusreport の report-builder 層でテストする。
	ring := lastseen.NewResourceRing(lastseen.DefaultRingSize)

	builder := statusreport.NewBuilder(rev.IntentRevision, rev.RenderedRevision, ring)

	ctx := context.Background()

	// #89 (§6.3): a target of a Workflow that is not done is driven by the
	// Workflow, not by the loop — reported as such, never as drifted.
	wfHeld := workflowHeld(in.Workflows, *workflowStore)

	for _, res := range desired {
		if h, ok := wfHeld[res.ID.Local]; ok {
			builder.AddWorkflowHeld(res.ID, h.Workflow, h.Stage)
			continue
		}
		fp := providers[res.ID.Provider]
		engine := engines[res.ID.Provider]

		// ADR-0002: リソースごとに独立して Observe（read-only Read）。
		// #117: Read のエラーは報告を止めず、そのリソースを観測できないものとして
		// 数える。1 台の BMC が落ちてもほかの適合は出す。
		obs, readErr := provider.ReadFor(ctx, fp, res.ID.Local, res.Fields)
		if readErr != nil {
			fmt.Fprintf(os.Stderr, "sakutei status: observe %s: %v\n", res.ID, readErr)
			builder.AddUnobservableReason(res.ID, fmt.Sprintf("observe error: %v", readErr))
			continue
		}
		if obs.Availability != core.AvailabilityOk {
			// unknown ≠ absent（§3）: 観測不能は drift とも converged とも数えない。
			builder.AddUnobservableReason(res.ID, fmt.Sprintf("availability=%v", obs.Availability))
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
		// Fix Sc7a647 cause-consistency: mark whether the resource is absent in
		// reality (obs.State == nil → OpKindCreate) so a cold-start CREATE is A
		// but a cold-start drift-repair UPDATE is B — identical to plan/reconcile.
		resCauseCtx.ResourceObservedAbsent = obs.State == nil

		// #122: a desired state the provider cannot plan against this
		// observation (e.g. a declared field whose API the target does not
		// implement) is reported as unobservable, like a Read error (#117),
		// instead of stopping the whole report.
		plan, planErr := engine.Plan(obs, res, resCauseCtx)
		if planErr != nil {
			fmt.Fprintf(os.Stderr, "sakutei status: %v\n", planErr)
			builder.AddUnobservableReason(res.ID, fmt.Sprintf("plan: %v", planErr))
			continue
		}

		// drift あり: 原因タグ・provider 階級・last-seen 注記を添えて一覧へ。
		// last-seen 注記は「この観測を記録する前の」リング状態と比較する。単一実行では
		// 前周が無いため「前周スナップショットなし」になる（AddDrifted 内で今のリングと
		// 比較 → その後に Record する順序でこれを保証する）。前周比較の機構自体は
		// report-builder 層でテストする（観測を 2 回与える）。
		if len(plan.Ops) > 0 {
			op := plan.Ops[0]
			tier := statusreport.TierFromCapabilities(fp.Capabilities())
			// #111: ずれたフィールドは全 op のぶんを載せる（op は 1 つが普通）。
			var fields []core.FieldDiff
			for _, o := range plan.Ops {
				fields = append(fields, o.Fields...)
			}
			if obs.State == nil {
				builder.AddMissing(res.ID, op.Cause, tier, op.Diagnostic, curFields, fields...)
			} else {
				builder.AddDrifted(res.ID, op.Cause, tier, op.Diagnostic, curFields, fields...)
			}
		} else {
			// 適合（drift なし）。
			builder.AddConvergedID(res.ID)
		}

		// 今周の観測を last-seen リングへ記録（S3・非永続）。AddDrifted の後に記録する
		// ことで、当該項目の前周注記は「今周観測を含まない」正しい前周状態を反映する。
		ring.Record(res.ID.Local, lastseen.Snapshot{
			LocalID:    res.ID.Local,
			ObservedAt: obs.ObservedAt,
			Fields:     curFields,
		})
	}

	// FIX #2 (Sc7a647-3, §12.2): unmanaged（宣言外・実在）検出。
	//
	// provider.Lister を実装する provider ごとに List でリアルを列挙し、宣言集合との
	// 差分（reality − declared）を取る。差分に残る ID＝「宣言していないのに現実に在る」
	// リソース。既定 DeletionPolicy（ExplicitAbsentOnly）では prune 対象外なので status は
	// これを削除せず、単に「在る」ことを可視化する（「確信なき減算はしない」）。List は
	// READ なので status の read-only 契約は保たれる（prune も apply もしない）。
	//
	// graceful degradation:
	//   - Lister 非対応 provider はリアルを列挙できない → 「検出不可（List 非対応）」注記。
	//   - List がエラー（complete-or-error）→ 「検出不可（List エラー）」注記。失敗を
	//     「宣言外ゼロ」と混同しない（現実の過少報告を避ける）。status は crash しない。
	//   - List が ListComplete でない（Partial/Unknown）→ 完全列挙を保証できないので
	//     unmanaged 判定を控え、注記に留める（安全側）。
	//
	// #111: --claim があれば claim の中の現実だけを宣言外の候補にする（prune と同じ境界）。
	//
	// #164: Pruneable かどうかに関わらない（unmanaged は見せるだけで消さない）。
	// 宣言が 0 件の provider も、設定されていれば List する（listOnly）。
	//
	// #169: 宣言済みかは claim で絞る前の宣言全体で決める（ほかの shard の宣言は
	// unmanaged ではない）。List の ID の scope は宣言に合わせてから突き合わせる。
	allDeclared := make([]core.ID, 0, len(expanded))
	for _, r := range expanded {
		if !r.IsComposite {
			allDeclared = append(allDeclared, r.DesiredResource.ID)
		}
	}
	detectUnmanaged(ctx, providers, listOnly, listOnlyErrs, claimSelector, allDeclared, in.ExternalOwners, builder)

	// #15 (§9.2): デーモンの連続スキップ回数（デーモンが書いたファイルを READ するだけ）。
	if *daemonStatus != "" {
		if h, err := statusreport.ReadDaemonHealth(*daemonStatus); err != nil {
			builder.NoteDaemonHealthUnavailable(fmt.Sprintf("デーモン健全性: 読めない（%v）", err))
		} else {
			builder.SetDaemonHealth(h)
			if h.Stale(time.Now()) {
				builder.MarkDaemonStale()
			}
		}
	}

	// 適合レポートを整形して出力。Apply（CREATE/UPDATE/DELETE/applied）系の行は
	// 一切含まれない —— drift を「一覧」するだけで実行はしない（read-only 契約）。
	report := builder.Build()
	if *jsonOut {
		// #118: 機械で読む形。情報源は文の出力と同じ Report。
		data, err := report.JSON(time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "sakutei status: encode json: %v\n", err)
			os.Exit(1)
		}
		os.Stdout.Write(append(data, '\n'))
	} else {
		fmt.Print(report.String())
	}
	if report.Incomplete() {
		os.Exit(3)
	}
	os.Exit(0)
}

// detectUnmanaged enumerates reality via each Lister-capable provider and adds
// any resource present in reality but absent from the declared intent set to the
// report as UNMANAGED (FIX #2, Sc7a647-3, §12.2).  It is READ-ONLY: it calls
// List (a read) only — never prune, apply, or write.
//
// Matching uses core.ID.Key() (collision-free) so a reality ID counts as
// declared only when Provider+Scope+Local all match a declared resource.
//
// Whether the provider's type is pruneable does not matter: unmanaged is only
// shown, never deleted (#164).  Besides the providers of the declared
// resources it Lists listOnly, the providers set up for this run with no
// declared resource (undeclaredListers); one that could not be built
// (listOnlyErrs) is noted like a List error.
//
// #169 (§5.4): a provider puts the scope of its own settings on the IDs it
// lists (root when it has none), independently of the declarations.  The IDs
// are aligned with the declarations first (listscope.Align): a declared one
// takes its declared ID; a scope the declarations contradict is noted for the
// provider (counted like a List error) and nothing is derived from its List.
// An undeclared root ID takes the one scope every declaration of the provider
// has (listscope.InferScope); with none decided and a --claim, the IDs whose
// side of the claim is unknown are counted in a note, never silently hidden.
// declared is the whole intent before the claim narrows it (another shard's
// declaration is not unmanaged); the candidates are narrowed by the claim.
//
// Graceful degradation (never crash, never under-report):
//   - provider without Lister → "検出不可（List 非対応 provider）" note, skipped.
//   - List returns error       → "検出不可（List エラー）" note (a failed List is
//     NOT treated as "no unmanaged" — that would hide reality).
//   - List not ListComplete    → "検出不可（List 未完全: partial/unknown）" note; do
//     not derive unmanaged from an incomplete enumeration (safe side).
func detectUnmanaged(
	ctx context.Context,
	providers map[string]provider.Provider,
	listOnly map[string]provider.Lister,
	listOnlyErrs map[string]error,
	claim core.Scope,
	declaredIDs []core.ID,
	owners extowner.Set,
	builder *statusreport.Builder,
) {
	// Declared set, keyed by collision-free ID key.
	declared := make(map[core.IDKey]bool, len(declaredIDs))
	for _, id := range declaredIDs {
		declared[id.Key()] = true
	}

	// Deterministic provider iteration order for stable notes/output.
	names := make([]string, 0, len(providers)+len(listOnly)+len(listOnlyErrs))
	for name := range providers {
		names = append(names, name)
	}
	for name := range listOnly {
		names = append(names, name)
	}
	for name := range listOnlyErrs {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if err, ok := listOnlyErrs[name]; ok {
			fmt.Fprintf(os.Stderr, "sakutei status: build %s: %v\n", name, err)
			builder.NoteListError(name, err)
			continue
		}
		lister, ok := listOnly[name]
		if !ok {
			lister, ok = providers[name].(provider.Lister)
		}
		if !ok {
			builder.NoteListUnsupported(name)
			continue
		}
		// The whole world is listed; --claim narrows the candidates below (#111).
		result, err := lister.List(ctx, core.NewScope(nil))
		if err != nil {
			// complete-or-error: a failed List must NOT read as "no unmanaged".
			// #117: counted as not observed (exit 3), not as a status failure.
			fmt.Fprintf(os.Stderr, "sakutei status: list %s: %v\n", name, err)
			builder.NoteListError(name, err)
			continue
		}
		if result.Status != core.ListComplete {
			builder.NoteListIncomplete(name, fmt.Sprintf("%v", result.Status))
			continue
		}
		// #169: the declaration is the source of truth for the scope of what
		// it declares; a contradiction is noted, never matched loosely.
		aligned := listscope.Align(result.IDs, declaredIDs)
		if len(aligned.Mismatches) > 0 {
			fmt.Fprintf(os.Stderr, "sakutei status: list %s: scope mismatch: %s\n", name, aligned.Mismatch())
			builder.NoteScopeMismatch(name, aligned.Mismatch())
			continue
		}
		inferred, inferOK := listscope.InferScope(name, declaredIDs)
		undecided := 0
		for _, realID := range aligned.IDs {
			if declared[realID.Key()] {
				continue
			}
			undecidedHere := false
			if listscope.IsRoot(realID.Scope) {
				if inferOK {
					realID.Scope = inferred
				} else {
					undecidedHere = !listscope.IsRoot(claim)
				}
			}
			// §7.3 (#158): what external_owners selects by its id is declared
			// (as someone else's), so it is not unmanaged.
			if _, ext := owners.ByID(realID); ext {
				continue
			}
			if undecidedHere {
				undecided++
				continue
			}
			if realID.Scope.Matches(claim) {
				builder.AddUnmanaged(realID)
			}
		}
		if undecided > 0 {
			builder.NoteScopeUndecided(name, undecided)
		}
	}
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
