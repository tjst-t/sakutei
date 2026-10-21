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
	"path/filepath"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/internal/audit"
	"github.com/tjst-t/sakutei/internal/budget"
	"github.com/tjst-t/sakutei/internal/capacity"
	"github.com/tjst-t/sakutei/internal/cause"
	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/executor"
	"github.com/tjst-t/sakutei/internal/lastseen"
	"github.com/tjst-t/sakutei/internal/loader"
	"github.com/tjst-t/sakutei/internal/prune"
	"github.com/tjst-t/sakutei/internal/reconcile"
	"github.com/tjst-t/sakutei/internal/remediation"
	"github.com/tjst-t/sakutei/internal/render"
	"github.com/tjst-t/sakutei/internal/revision"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
	"github.com/tjst-t/sakutei/pkg/registry"
	"github.com/tjst-t/sakutei/pkg/sdk"
)

// runReconcile implements "sakutei reconcile".
//
// It performs a single convergence pass: Observe → Diff → Apply (wave order).
//
// ADR-0002 contract (観測 state レス): reconcile MUST observe reality itself.
// It MUST NOT reuse any observations produced by a prior "sakutei plan" run or
// any other invocation.  Each call to fp.Read below is a fresh, independent
// observation.
//
// S94f1f9-2: Apply is now ordered by the depends_on DAG using Kahn waves.
// Upstream failure skips downstream resources for this cycle.
// Cyclic depends_on is rejected at intent load time.
//
// Scb0ecd-2: cause-tag classification (A/B/C active, D/E seam).
//   - --cause-state <file>: optional JSON file holding previous-cycle revision
//     and provider_version values.  Written at end of invocation so the next
//     run can compare.  Absent = first-run (all ops tagged A, correct).
//   - Ops with cause: C are NOT Applied (ProviderUpgradeDiff gate, §10.4):
//     they are logged as ProviderUpgradeDiff Events and filtered from the Apply
//     set for this cycle.  The operator must review and re-run with the new
//     provider version locked in (or revert).
//
// D-1 (unobservable-upstream safety, ADR-0002 / §3): a resource whose Observe
// returns an error OR Availability != Ok is treated as failed/unknown, NOT
// converged.  It and its transitive downstream are skipped this cycle
// (unknown ≠ absent; halt-default).  Policy, unified with the loop path:
// the one-shot reconcile still processes every observable resource, but if any
// resource was skipped it exits non-zero to signal an incomplete convergence.
// (The loop path instead retries on the next cycle — same skip semantics,
// different follow-up because a daemon has a "next cycle" and a one-shot does not.)
//
// S62dbba-2: provider is no longer hardcoded to fakeprovider.  The
// ProviderRegistry resolves the correct provider for each resource based on
// the "provider" field in the intent file.
//
// Exit codes:
//
//	0 — reconcile completed, nothing skipped (with or without changes applied)
//	1 — error (flag parse, intent load, apply error) OR one or more resources
//	    were skipped this cycle (unobservable upstream / upstream Apply failure)
//	    OR --expect-revision did not match rendered_revision (nothing applied)
func runReconcile(args []string) {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	intentDir := fs.String("intent", "", "directory containing intent JSON files (required)")
	statePath := fs.String("state", "sakutei-fake-state.json",
		"path to the fake provider state file (default: sakutei-fake-state.json)")
	// S62dbba verify: distinct file for the dnszone provider so a mixed
	// fake+dnszone intent does not force both providers onto one path.  When
	// empty, dnszone falls back to --state (registry alias) as before.
	zonePath := fs.String("zone", "",
		"path to the dnszone provider zone file (default: falls back to --state)")
	// #67: distinct file for the nftables provider, same meaning as in status.
	rulesetPath := fs.String("ruleset", "",
		"path to the nftables provider ruleset file (default: falls back to --state)")
	zoneSOASerial := zoneSOASerialFlag(fs)
	// Scb0ecd-2: cause-state file for cross-invocation revision tracking.
	causeStatePath := fs.String("cause-state", "",
		"path to cause-state JSON file for cross-invocation revision tracking (Scb0ecd-2); optional")
	// S072a14-1: generated/ output directory for render phase 2 (§9.6).
	allocStore := fs.String("alloc-store", "",
		"#24: path to the allocation store file (file AllocationAuthority, §19.9); required when the intent uses from_pool")
	allocURL := fs.String("alloc-url", "", allocURLUsage)
	// #44 (§14.5, §23.7): S4 audit log; with it the planned_change lanes open.
	auditLog := fs.String("audit-log", "",
		"S4 audit log file (append-only JSON lines); with it planned_change lanes open, holding max_count and the cooling-off across runs (§14.5, §23.7)")
	generatedDir := fs.String("generated", "",
		"directory to write render phase 2 output (default: sibling of --intent named 'generated')")
	// S072a14-2: render phase 1 — external input resolver flags.
	inventoryHash := fs.String("inventory-hash", "",
		"S072a14-2: inject deterministic inventory hash into rendered_revision (fake phase 1; empty=no external input)")
	inventoryUnreachable := fs.Bool("inventory-unreachable", false,
		"S072a14-2: simulate an unreachable ExternalInputResolver (triggers last-value-on-unreachable path §9.2)")

	// Scf7762-2: change-budget flags (§13).
	maxDestructivePerCycle := fs.Int("max-destructive-per-cycle", 0,
		"maximum destructive (Delete/destructive-Update) ops per cycle; 0=unlimited (§13.1)")
	maxChangesPerCycleAbsolute := fs.Int("max-changes-per-cycle", 0,
		"maximum total (additive+destructive) ops per cycle; 0=unlimited (§13.1)")
	maxDestructivePerWindow := fs.Int("max-destructive-per-window", 0,
		"maximum destructive ops in the rolling time window; 0=unlimited (§13.2)")
	windowDurationStr := fs.String("window-duration", "",
		"rolling window duration for cumulative budget, e.g. 24h, 10m (default: 24h) (§13.2)")
	onExceedStr := fs.String("on-exceed", "halt-all",
		"action when budget is exceeded: halt-all | halt-destructive-only (§13.1)")
	approveLargeChange := fs.Int("approve-large-change", 0,
		"explicitly approve up to N destructive changes this cycle (overrides per-cycle rate limit)")

	// §13.4: capacity gate.
	var capacityGates capacityGateFlag
	fs.Var(&capacityGates, "capacity-gate", capacityGateUsage)

	// Scf7762-3: remediation mode flags (§14.1).
	modeStr := fs.String("mode", "auto",
		"remediation mode: auto | alert-only (§14.1); alert-only detects drift but does not Apply")
	freeze := fs.Bool("freeze", false,
		"global freeze: override to alert-only regardless of --mode (§14.3 escape hatch)")

	// Sf6a62f-3: --claim scope-selector (§17.1).  Restricts convergence to the
	// resources whose scope matches the selector; out-of-scope resources are
	// excluded from Observe/plan/Apply (a shard only converges its own claim).
	// Syntax: comma-separated key=value pairs, e.g. --claim site=tokyo,cluster=prod.
	// Empty (default) = no restriction (single-shard: the whole world is claimed).
	// Real sparse checkout / git distribution is v1b (§17.4); this is the seam.
	claim := fs.String("claim", "",
		"scope-selector restricting convergence to a claim, e.g. site=tokyo,cluster=prod (§17.1); empty=whole world")

	// #46 (§26.6): --expect-revision pins the rendered_revision confirmed by
	// plan.  If intent, allocations or inventory moved in between, this run's
	// rendered_revision differs and nothing is Applied (the approval gate's
	// "approve the intent, check identity by rendered_revision at run time", §14).
	expectRevision := fs.String("expect-revision", "",
		"rendered_revision confirmed by plan; if this run's rendered_revision differs, abort before any Apply (§26.6); empty=no check")

	if err := fs.Parse(args); err != nil {
		// flag package already printed the error.
		os.Exit(1)
	}
	soaSerial := checkZoneSOASerial("sakutei reconcile", *zoneSOASerial)

	if *intentDir == "" {
		fmt.Fprintln(os.Stderr, "sakutei reconcile: --intent <dir> is required")
		fs.Usage()
		os.Exit(1)
	}

	// Parse remediation configuration (Scf7762-3, §14.1).
	remMode, remModeErr := remediation.ParseMode(*modeStr)
	if remModeErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: invalid --mode %q: %v\n", *modeStr, remModeErr)
		os.Exit(1)
	}
	remCfg := remediation.Config{
		Mode:   remMode,
		Freeze: *freeze,
	}

	// Parse budget configuration.
	budgetCfg := budget.Config{
		MaxDestructivePerCycle:     *maxDestructivePerCycle,
		MaxChangesPerCycleAbsolute: *maxChangesPerCycleAbsolute,
		MaxDestructivePerWindow:    *maxDestructivePerWindow,
		ApproveLargeChange:         *approveLargeChange,
	}
	if *windowDurationStr != "" {
		d, err := time.ParseDuration(*windowDurationStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sakutei reconcile: invalid --window-duration %q: %v\n", *windowDurationStr, err)
			os.Exit(1)
		}
		budgetCfg.WindowDuration = d
	}
	switch *onExceedStr {
	case "halt-destructive-only":
		budgetCfg.OnExceed = budget.OnExceedHaltDestructiveOnly
	case "halt-all", "":
		budgetCfg.OnExceed = budget.OnExceedHaltAll
	default:
		fmt.Fprintf(os.Stderr, "sakutei reconcile: unknown --on-exceed value %q; use halt-all or halt-destructive-only\n", *onExceedStr)
		os.Exit(1)
	}
	budgetEngine := budget.New(budgetCfg)

	// Resolve generated/ directory (S072a14-1: §9.6).
	genDir := *generatedDir
	if genDir == "" {
		genDir = filepath.Join(filepath.Dir(*intentDir), "generated")
	}

	// Load desired resources from intent directory.
	// loader.LoadDir also performs cycle detection (S94f1f9-2): cyclic
	// depends_on is rejected here with a non-zero exit before any Apply.
	// Scb0ecd-1: LoadDir now also returns RevisionInfo.
	in, err := loader.LoadIntent(*intentDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: load intent: %v\n", err)
		os.Exit(1)
	}
	// §13.3 / §14.5: one-shot reconcile builds a fresh budget engine every
	// invocation, so it opens the planned_change lanes only with the S4 audit
	// log: S4 holds when each declaration was first seen (the cooling-off is a
	// gate evaluated on this run, §23.7) and how much of each lane earlier runs
	// used (max_count across runs).  Without it every change is evaluated by the
	// normal budget (conservative).
	if *auditLog != "" {
		if err := budgetEngine.AttachAudit(audit.NewLog(*auditLog)); err != nil {
			fmt.Fprintf(os.Stderr, "sakutei reconcile: read audit log: %v\n", err)
			os.Exit(1)
		}
		budgetEngine.SetPlannedChanges(in.PlannedChanges)
	} else if len(in.PlannedChanges) > 0 {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: note: %d planned_change declaration(s) need --audit-log to open their lanes in a one-shot run (max_count and the cooling-off are held in S4 across runs); this run evaluates every change by the normal budget\n", len(in.PlannedChanges))
	}
	// #42 (§6.3): a Workflow target is driven only by its Workflow in the
	// resident loop, where its progress (S2) is known.  The one-shot run cannot
	// tell whether the Workflow is done, so it never Applies a target with a
	// diff (conservative).
	workflowOf := map[string]string{}
	for _, def := range in.Workflows {
		for _, id := range def.Targets() {
			workflowOf[id.Local] = def.Name
		}
	}
	intentResources, rev := in.Resources, in.Revision
	var externalInputs []string
	recomputeRendered := false

	// S072a14-2: render phase 1 — external input resolution (§9.1 フェーズ1).
	if *inventoryHash != "" || *inventoryUnreachable {
		fakeResolver := &render.FakeInventoryResolver{
			Hash: *inventoryHash,
			Fail: *inventoryUnreachable,
		}
		cache := render.NewResolverCache(fakeResolver, 0)
		phase1Result := render.ResolveExternalInputs(context.Background(), cache)
		if phase1Result.UnreachableFallback {
			fmt.Fprintf(os.Stderr,
				"sakutei reconcile: ExternalInputUnreachable: resolver returned error; using last cached value (§9.2 last-value-on-unreachable)\n")
		}
		if phase1Result.FirstCallFailed {
			fmt.Fprintf(os.Stderr,
				"sakutei reconcile: ExternalInputUnreachable (first call): %v\n"+
					"  No prior cached value available; convergence not advanced (halt-safe, §9.2)\n",
				phase1Result.Err)
			os.Exit(1)
		}
		externalInputs = append(externalInputs, phase1Result.ExternalInputHashes...)
		recomputeRendered = true
	}

	// Sf6a62f-3: parse the --claim scope-selector into a core.Scope matcher.
	// #25: parsed before render so allocation follows the claim (§19.8).
	claimSelector, claimErr := parseClaimSelector(*claim)
	if claimErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: invalid --claim %q: %v\n", *claim, claimErr)
		os.Exit(1)
	}

	// #24: render phase 1 — allocation (§9.1, §19).  reconcile ALLOCATES
	// (idempotently: the same owner always gets the same value).  #25: a
	// request the authority cannot serve (allocator down, pool exhausted)
	// makes only its resource waiting-allocation; the rest converges (§19.9).
	allocAuth, allocErr := openAllocStore(*allocStore, *allocURL)
	if allocErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: %v\n", allocErr)
		os.Exit(1)
	}
	allocRes, allocErr := render.RenderAllocations(context.Background(), allocAuth, render.AllocAllocate, in, claimSelector)
	if allocErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: %v\n  convergence not advanced (halt-safe, §9.2)\n", allocErr)
		os.Exit(1)
	}
	for _, w := range allocRes.Warnings {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: warn: allocation: %s\n", w)
	}
	for _, w := range allocRes.Waiting {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: waiting-allocation: %s field %q from pool %q: %v (not converged until allocated; others continue, §19.9)\n",
			w.ID, w.Field, w.Pool, w.Err)
	}
	intentResources = allocRes.Resources
	if len(allocRes.Inputs) > 0 {
		externalInputs = append(externalInputs, allocRes.Inputs...)
		recomputeRendered = true
	}
	if recomputeRendered {
		rootScope := core.NewScope(nil)
		rev.RenderedRevision = revision.ComputeRenderedRevision(
			rootScope, rev.IntentRevision, externalInputs)
	}
	// #46 (§26.6): checked before render phase 2 writes generated/ and before
	// any Observe/Apply, so a mismatch leaves reality untouched.
	if *expectRevision != "" && *expectRevision != rev.RenderedRevision {
		fmt.Printf("intent_revision=%s rendered_revision=%s\n", rev.IntentRevision, rev.RenderedRevision)
		fmt.Fprintf(os.Stderr,
			"sakutei reconcile: RevisionMismatch: --expect-revision %s but rendered_revision is %s (intent, allocations or inventory changed since plan); nothing applied — re-run plan (§26.6)\n",
			*expectRevision, rev.RenderedRevision)
		os.Exit(1)
	}

	// S072a14-1: render phase 2 — expand composite resources (§9.5).
	expRegistry := render.DefaultRegistry()
	expanded, expandErr := render.ExpandAll(expRegistry, intentResources)
	if expandErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: render phase 2: %v\n", expandErr)
		os.Exit(1)
	}

	// §9.6: validate existing generated/ file and warn on direct human edits.
	genFilePath := filepath.Join(genDir, render.GeneratedFileName)
	if valErr := render.ValidateGeneratedFile(genFilePath); valErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: WARNING: %v\n", valErr)
	}

	// §9.6: write expanded children to generated/expanded.json (overwrites any
	// prior contents — render is deterministic so the result is identical each
	// time for the same intent, preventing two-management drift).
	if writeErr := render.WriteGenerated(genDir, expanded); writeErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: write generated/: %v\n", writeErr)
		os.Exit(1)
	}

	// Build the desired slice: only non-composite resources enter Diff/Apply.
	//
	// Sf6a62f-3: a resource whose scope does not match the claim selector is
	// EXCLUDED here — before Observe/Diff/Apply — so an out-of-claim resource is
	// never even observed (§17.1: "claim に含まれない scope は Observe すらしない").
	// With an empty selector Matches is always true, so this is a no-op filter
	// in the default single-shard case.
	desired := make([]diff.DesiredResource, 0, len(expanded))
	excludedByClaim := 0
	for _, r := range expanded {
		if r.IsComposite {
			continue
		}
		if !r.DesiredResource.ID.Scope.Matches(claimSelector) {
			excludedByClaim++
			continue
		}
		desired = append(desired, r.DesiredResource)
	}
	if len(claimSelector.Labels) > 0 {
		fmt.Fprintf(os.Stderr,
			"sakutei reconcile: --claim %s: %d resource(s) in claim, %d excluded (out of scope)\n",
			claimSelector.String(), len(desired), excludedByClaim)
	}

	// Scb0ecd-1: print revision header so the caller can observe revision values.
	// In v1a rendered_revision = intent_revision (seam for S072a14).
	fmt.Printf("intent_revision=%s rendered_revision=%s\n", rev.IntentRevision, rev.RenderedRevision)

	// S62dbba-2: resolve providers via registry based on intent resource declarations.
	// The --state flag is passed as the "state" config key for the "fake" provider;
	// --zone / --ruleset (when set) are passed as "zone" / "ruleset" for the
	// dnszone / nftables providers so a mixed intent can keep each provider's
	// backing file separate.
	reg := registry.DefaultRegistry()
	providerConfig := map[string]string{"state": *statePath}
	if *zonePath != "" {
		providerConfig["zone"] = *zonePath
	}
	if *rulesetPath != "" {
		providerConfig["ruleset"] = *rulesetPath
	}
	providerConfig["soa_serial"] = soaSerial
	providers, resolveErr := resolveProviders(reg, desired, providerConfig, "sakutei reconcile")
	if resolveErr != nil {
		fmt.Fprintln(os.Stderr, resolveErr)
		os.Exit(1)
	}

	// Build per-provider diff engines (each provider has its own Schema).
	engines := make(map[string]*diff.Engine, len(providers))
	for name, p := range providers {
		engines[name] = diff.New(p.Schema())
	}

	// Scb0ecd-2: compute curProviderVersion for cause-state tracking.
	curProviderVersion := joinedProviderVersions(providers)

	// Depend on the Executor seam (interface), not the concrete LocalExecutor,
	// so later Stories can swap in other implementations without touching this
	// call site (§6 seam principle).
	var exec executor.Executor = executor.New()

	// Scb0ecd-2: load cause state (previous-cycle revision + provider_version).
	prevState, csErr := loadCauseState(*causeStatePath)
	if csErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: warn: load cause-state %q: %v (treating as first-run)\n",
			*causeStatePath, csErr)
		prevState = causeStateFile{}
	}

	// S072a14-3: compute RenderedRevisionChanged for cause E.
	renderedRevisionChangedRec := prevState.PrevRenderedRevision != "" &&
		rev.RenderedRevision != prevState.PrevRenderedRevision &&
		rev.IntentRevision == prevState.PrevIntentRevision

	// S072a14-3: build parent→children map for cause D.
	// #14: only the children of a parent whose own rendered contribution
	// changed since the previous invocation get D.
	parentToChildrenRec := render.BuildParentChildMap(expanded)
	curParentRevsRec := render.ParentRevisions(expanded)
	childrenWithUpstreamChangeRec := render.UpstreamChangedChildren(prevState.PrevParentRevisions, curParentRevsRec, parentToChildrenRec)

	// Build the base per-cycle CauseContext.  All resources in this invocation
	// share the same revision signals (§10.3: global revision for candidate
	// narrowing; per-resource diff is the final arbiter).
	//
	// S072a14-3: RenderedRevisionChanged is now activated (E), and UpstreamChanged
	// is set per-resource below for children of composite parents that changed.
	baseCauseCtxRec := cause.CauseContext{
		IntentPrevRevision:      prevState.PrevIntentRevision,
		IntentCurRevision:       rev.IntentRevision,
		RenderedPrevRevision:    prevState.PrevRenderedRevision,
		RenderedCurRevision:     rev.RenderedRevision,
		ProviderPrevVersion:     prevState.PrevProviderVersion,
		ProviderCurVersion:      curProviderVersion,
		RenderedRevisionChanged: renderedRevisionChangedRec,
		// UpstreamChanged is set per-resource below.
	}

	ctx := context.Background()

	// Scb0ecd-3: last-seen ring for one-shot reconcile.
	//
	// The one-shot reconcile has no "previous cycle" by definition (it is a
	// single invocation, not a long-lived daemon).  We create the ring here
	// so that the code path is exercised and drift B events include a last-
	// seen note.  For one-shot, the ring always reports "no previous snapshot"
	// because Record is called after Diff — there is nothing in the ring yet
	// when DriftSummary is called for the same invocation.  This is correct:
	// the ring's value is in the daemon's multi-cycle context (§10.5).
	lastSeenRing := lastseen.NewResourceRing(lastseen.DefaultRingSize)

	// Phase 1: Observe + Diff (fresh per resource — ADR-0002).
	// Collect plans for all resources before applying any of them.
	plans := make(map[string]core.Plan, len(desired))
	// unobservable holds Local IDs whose Observe failed or returned Availability
	// != Ok (D-1).  They are NOT treated as converged: ApplyWaves marks them
	// failed and skips their transitive downstream this cycle.
	unobservable := make(map[string]bool)
	// #25: waiting-allocation resources (and the children they expand to) are
	// skipped like an unobservable one — not Observed, downstream skipped,
	// never pruned — but they are not a safety failure (§19.9).
	waitingAlloc := allocRes.WaitingLocals(parentToChildrenRec)
	// #41: strategy: task resources with a diff (held for the daemon's Task).
	taskHeld := map[string]bool{}
	// #42: Workflow targets with a diff (driven by the daemon's Workflow).
	workflowHeld := map[string]bool{}
	// ready holds the ready conditions observed this invocation (§8.3); a
	// downstream whose ready_condition upstream is not ready is skipped.
	ready := make(reconcile.Readiness)
	// obsFields records the observed field values per resource (as strings) so
	// we can populate the last-seen ring and report drift B diagnostics.
	obsFields := make(map[string]map[string]string, len(desired))
	// #27 (§5.7): one CycleObserver per provider for this invocation — a
	// provider that declares BulkObserve is observed with one bulk call, any
	// other with one Read per resource.
	cycleObs := make(map[string]*sdk.CycleObserver, len(providers))
	for _, res := range desired {
		// S62dbba-2: route Read and Diff to the provider for this resource.
		fp, ok := providers[res.ID.Provider]
		if !ok {
			// Should not happen: resolveProviders already validated all names.
			fmt.Fprintf(os.Stderr, "sakutei reconcile: internal error: no provider for %q\n", res.ID.Provider)
			os.Exit(1)
		}
		engine := engines[res.ID.Provider]

		if waitingAlloc[res.ID.Local] {
			unobservable[res.ID.Local] = true
			continue
		}

		// ADR-0002: Observe independently for each resource.
		//
		// This is a fresh observation: reconcile does NOT carry over any
		// observation result from a prior "sakutei plan" invocation or from
		// a previous iteration of this loop.  Every Observe reaches reality
		// at this moment in time — by Read, or by this invocation's single
		// bulk call when the provider declares BulkObserve (§5.7).
		obsv, ok := cycleObs[res.ID.Provider]
		if !ok {
			obsv = sdk.NewCycleObserver(fp, claimSelector)
			cycleObs[res.ID.Provider] = obsv
		}
		obs, err := obsv.Observe(ctx, res.ID)
		if err != nil {
			// D-1: Observe error → unobservable, not converged.  Do not exit
			// immediately: record it, keep observing the rest, and let ApplyWaves
			// skip this resource and its downstream.  A non-zero exit is emitted
			// below if anything was skipped (unified skip policy with the loop).
			fmt.Fprintf(os.Stderr, "sakutei reconcile: observe %s: %v\n", res.ID, err)
			unobservable[res.ID.Local] = true
			continue
		}
		if obs.Availability != core.AvailabilityOk {
			// D-1: reachable-but-unknown/converging is still not "converged".
			// unknown ≠ absent: do not proceed downstream on uncertainty.
			fmt.Fprintf(os.Stderr, "sakutei reconcile: observe %s: availability=%v (not Ok); treating as unobservable\n",
				res.ID, obs.Availability)
			unobservable[res.ID.Local] = true
			continue
		}
		ready.Record(res.ID.Local, obs)

		// Convert observed State to string map for last-seen ring storage.
		curFields := make(map[string]string, len(obs.State))
		for fname, tv := range obs.State {
			curFields[fname] = fmt.Sprintf("%v", tv.Value)
		}
		obsFields[res.ID.Local] = curFields

		// S072a14-3: set UpstreamChanged=true for children of composite parents
		// whose desired state changed this cycle (cause D).
		resCauseCtxRec := baseCauseCtxRec
		if childrenWithUpstreamChangeRec[res.ID.Local] {
			resCauseCtxRec.UpstreamChanged = true
		}
		// Fix Sc7a647 cause-consistency: mark whether this op is a Create
		// (resource absent in reality) so a cold-start (empty prev revision)
		// Create is A but a cold-start drift-repair UPDATE is B — mirroring the
		// diff engine's obs.State == nil → OpKindCreate test.
		resCauseCtxRec.ResourceObservedAbsent = obs.State == nil

		// S33266e-1 (§20): reject a bare plaintext in a secret-flagged field
		// before diffing (plaintext secret + perpetual non-converging diff).
		// The error names the field only, never the value.
		if err := engine.ValidateDesired(res); err != nil {
			fmt.Fprintf(os.Stderr, "sakutei reconcile: %v\n", err)
			os.Exit(1)
		}

		resourcePlan := engine.Diff(obs, res, resCauseCtxRec)
		// Scb0ecd-2: log ProviderUpgradeDiff event for cause-C ops (ghost diff),
		// and filter them out of the Apply set (§10.4, task Scb0ecd-2-2).
		// C ops are shown in plan output but NOT applied; the operator must
		// confirm the provider upgrade before applying.
		var nonCOps []core.Op
		for _, op := range resourcePlan.Ops {
			if op.Cause == core.CauseTagC {
				fmt.Fprintf(os.Stderr,
					"sakutei reconcile: ProviderUpgradeDiff: %s [cause: C] provider_version changed (%s → %s); NOT applying (review required)\n",
					op.Target, prevState.PrevProviderVersion, curProviderVersion)
			} else {
				// Scb0ecd-3: for cause B (drift), attach last-seen ring summary.
				if op.Cause == core.CauseTagB {
					summary := lastSeenRing.DriftSummary(res.ID.Local, curFields)
					fmt.Fprintf(os.Stderr,
						"sakutei reconcile: DriftEvent: %s [cause: B] %s\n",
						op.Target, summary)
				}
				nonCOps = append(nonCOps, op)
			}
		}
		if wf, ok := workflowOf[res.ID.Local]; ok && len(nonCOps) > 0 {
			fmt.Fprintf(os.Stderr,
				"sakutei reconcile: %s: target of workflow %s — not applying; the daemon drives it through the Workflow (§6.3)\n",
				res.ID, wf)
			unobservable[res.ID.Local] = true
			workflowHeld[res.ID.Local] = true
		} else if len(nonCOps) > 0 && res.UpdatePolicy.IsTask() {
			// #41 (§6.5 RequiresTaskEvent): a non-idempotent resource is never
			// Applied.  Only the daemon launches its Task (1-outstanding, S2);
			// here it is not converged, so its downstream waits.
			fmt.Fprintf(os.Stderr,
				"sakutei reconcile: RequiresTaskEvent: %s: strategy: task — not applying; the daemon launches its Task (§6.5)\n",
				res.ID)
			unobservable[res.ID.Local] = true
			taskHeld[res.ID.Local] = true
		} else if len(nonCOps) > 0 {
			plans[res.ID.Local] = core.Plan{Ops: nonCOps}
		} else if len(resourcePlan.Ops) > 0 && len(nonCOps) == 0 {
			// All ops were C (ghost diff); store an empty plan so that the
			// resource is not treated as "no diff" (it is drifted, just not
			// applied this cycle). In current flow no plan entry = converged,
			// which is acceptable: ghost diff resources will show in plan
			// output but not be applied. The cause: C flag in printed ops
			// already communicates this to the operator.
			// NOTE: we intentionally do NOT add to plans here so that
			// reconcile.ApplyWaves sees it as "already converged" for the wave
			// scheduler (do not attempt Apply, do not skip downstream).
		}
	}

	// §8.3: observe ready_condition upstreams that are outside the desired set
	// through the dependent's provider, so they can still gate it.
	reconcile.ObserveExternalUpstreams(ctx, func(r diff.DesiredResource) provider.Provider {
		return providers[r.ID.Provider]
	}, desired, ready, os.Stderr)

	// Scb0ecd-3: populate the last-seen ring AFTER Diff so the ring records
	// the current cycle's observations.  The ring is read on the NEXT cycle
	// (in the daemon) to produce the "prev cycle snapshot" message.
	// For the one-shot reconcile, populating here means the ring is only ever
	// consulted with zero prior entries (so DriftSummary returns "no previous
	// snapshot"), which is the correct S3-non-persistent behaviour.
	for localID, fields := range obsFields {
		lastSeenRing.Record(localID, lastseen.Snapshot{
			LocalID:    localID,
			ObservedAt: time.Now(),
			Fields:     fields,
		})
	}

	// Scb0ecd-2: persist current revision + provider_version as "previous" for
	// the next invocation.  Non-fatal on failure (S3 semantics).
	if saveErr := saveCauseState(*causeStatePath, causeStateFile{
		PrevIntentRevision:   rev.IntentRevision,
		PrevRenderedRevision: rev.RenderedRevision,
		PrevProviderVersion:  curProviderVersion,
		PrevParentRevisions:  curParentRevsRec,
	}); saveErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: warn: save cause-state %q: %v\n", *causeStatePath, saveErr)
	}

	// Phase 2a: Compute prune ops (dry-run, no Apply yet — Scf7762-2 budget gate).
	//
	// The budget gate needs to see ALL planned ops (Apply + prune) before any is
	// executed.  We therefore compute the prune set here (without executing it)
	// so the budget engine can count destructive ops across the whole cycle.
	//
	// S62dbba-2: prune is computed per provider.  For the one-shot path, each
	// provider that implements Lister gets its own prune computation over its
	// resources.
	var precomputedPruneOps []core.Op
	var pruneWasHaltedBySafetyGuard bool
	// Group desired resources by provider for per-provider prune.
	byProvider := make(map[string][]diff.DesiredResource, len(providers))
	for _, r := range desired {
		byProvider[r.ID.Provider] = append(byProvider[r.ID.Provider], r)
	}
	for name, fp := range providers {
		if _, isLister := fp.(provider.Lister); isLister {
			ops, halted := computePruneForProvider(ctx, fp, claimSelector, byProvider[name], unobservable, syncScopeList(in))
			precomputedPruneOps = append(precomputedPruneOps, ops...)
			if halted {
				pruneWasHaltedBySafetyGuard = true
			}
		}
		_ = name
	}

	// Phase 2b: Remediation mode gate (§14.1, Scf7762-3; #40 §14.2/§14.3).
	//
	// The mode is resolved from freeze, --mode and the intent's remediation
	// schedule at the current time (most-restrictive-wins).
	// alert-only / freeze: do NOT Apply or prune.  Report drift and exit 0.
	// The budget gate (Phase 2c) is skipped — nothing is applied.
	decision := remCfg.Decide(in.Remediation)
	if decision.IsAlertOnly() {
		held := reconcile.HoldAll(desired, plans, unobservable, precomputedPruneOps)
		modeLabel := "alert-only"
		if remCfg.Freeze {
			modeLabel = "alert-only [FROZEN]"
		}
		if in.Remediation != nil {
			fmt.Printf("Remediation mode: alert-only (%s)\n", decision.Reason)
		}
		if held.DriftCount == 0 {
			fmt.Printf("Reconcile (%s): no drift detected\n", modeLabel)
		} else {
			fmt.Printf("Reconcile (%s): %d drifted resource(s) detected — no changes applied\n", modeLabel, held.DriftCount)
			for _, ev := range held.Events {
				fmt.Printf("  DRIFT EVENT: %s\n", ev)
			}
		}
		// alert-only exits 0: detection without action is not a failure.
		os.Exit(0)
	}
	if in.Remediation != nil {
		fmt.Printf("Remediation mode: auto (%s)\n", decision.Reason)
	}
	// §14.3: scopes declared remediation: alert-only are held even in auto.
	scopeHeld := reconcile.HoldAlertOnly(desired, plans, unobservable, precomputedPruneOps, in.AlertOnlyScopes())
	precomputedPruneOps = scopeHeld.PruneOps
	if scopeHeld.DriftCount > 0 {
		fmt.Printf("Alert-only scope(s): %d drifted resource(s) held — not remediated\n", scopeHeld.DriftCount)
		for _, ev := range scopeHeld.Events {
			fmt.Printf("  DRIFT EVENT: %s\n", ev)
		}
	}

	// Phase 2c: Collect all planned Apply ops (from Diff phase).
	var allPlannedOps []core.Op
	for _, res := range desired {
		if unobservable[res.ID.Local] {
			continue
		}
		if plan, ok := plans[res.ID.Local]; ok {
			allPlannedOps = append(allPlannedOps, plan.Ops...)
		}
	}
	allPlannedOps = append(allPlannedOps, precomputedPruneOps...)

	// Phase 2d: Budget gate (§13, Scf7762-2).
	//
	// Evaluate rate-limit and cumulative time-window budget.  For the one-shot
	// reconcile, in-flight is always 0 (synchronous Apply, no prior goroutines).
	budgetDecision := budgetEngine.CheckCycle(allPlannedOps, budget.InFlight{}, len(desired))
	for _, ev := range budgetDecision.Events {
		fmt.Printf("%s\n", ev)
	}
	budgetHalted := budgetDecision.HaltAll || budgetDecision.HaltDestructive
	if budgetHalted {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: BUDGET HALTED: %s\n", budgetDecision.Reason)
	}

	// Phase 2d': Capacity gate (§11.4, §13.4).  Observe each pool's current
	// utilization; above the threshold (or unknown) halts new changes.  The
	// pool is read through the gate's provider, or the fake provider (the
	// --state default) when the gate names none.
	capacityDecision := capacity.CheckAll(ctx, capacityGates, func(g capacity.Gate) provider.Provider {
		name := g.Provider
		if name == "" {
			name = "fake"
		}
		if p, ok := providers[name]; ok {
			return p
		}
		p, err := reg.Get(name, providerConfig)
		if err != nil {
			return nil
		}
		return p
	})
	capacityHalted := capacityDecision.HaltAll || capacityDecision.HaltDestructive
	if capacityHalted {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: %s: CAPACITY HALTED: %s\n", capacity.EventName, capacityDecision.Reason)
	}
	budgetDecision = budget.Merge(budgetDecision, capacityDecision)

	// Filter per-resource plans through the budget decision.
	filteredPlans := make(map[string]core.Plan, len(plans))
	for local, plan := range plans {
		var allowed []core.Op
		for _, op := range plan.Ops {
			if !budgetDecision.IsHalted(op) {
				allowed = append(allowed, op)
			} else {
				fmt.Fprintf(os.Stderr, "sakutei reconcile: budget: blocked %v op for %s\n", op.Kind, op.Target)
			}
		}
		if len(allowed) > 0 {
			filteredPlans[local] = core.Plan{Ops: allowed}
		}
	}

	// §13.3 / §14.5: the lane ops that passed the gates consume their lanes
	// up front, and the usage is recorded in S4 before anything is dispatched:
	// a run that cannot record it executes nothing (a skipped op's slot is not
	// given back — conservative, as in the scoped path).
	if budgetDecision.LaneAdmitted() > 0 {
		var laneOps []core.Op
		for _, plan := range filteredPlans {
			laneOps = append(laneOps, plan.Ops...)
		}
		allowedPrune, _ := budgetDecision.FilterOps(precomputedPruneOps)
		laneOps = append(laneOps, allowedPrune...)
		budgetEngine.ReserveLanes(budgetDecision, laneOps)
		if err := budgetEngine.FlushLaneAudit(); err != nil {
			fmt.Fprintf(os.Stderr, "sakutei reconcile: planned_change: cannot record lane executions in S4: %v; nothing applied\n", err)
			os.Exit(1)
		}
	}

	// Phase 2: Apply in dependency (wave) order via Kahn decomposition.
	// reconcile.ApplyWaves ensures that:
	//   - Resources with depends_on are applied after their dependencies.
	//   - If a dependency fails OR is unobservable (D-1), its transitive
	//     dependents are skipped.
	//   - appliedIDs contains the Local IDs of resources that were successfully applied.
	//
	// S62dbba-2: ApplyWaves needs to route Apply to the right provider per resource.
	// We use a multiProvider adapter so the existing ApplyWaves interface (which
	// takes a single provider.Provider) continues to work without changes to
	// internal/reconcile.  The adapter routes each Apply call to the correct
	// per-resource provider based on the Op's target provider.
	applyProvider := newMultiProviderApply(providers, desired)
	_, appliedIDs, skipped, waveErr := reconcile.ApplyWaves(ctx, applyProvider, exec, desired, filteredPlans, unobservable, ready, os.Stderr)
	if waveErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: wave apply: %v\n", waveErr)
		os.Exit(1)
	}

	// Report skipped resources (upstream failure or unobservable) to stderr.
	for _, sk := range skipped {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: skip %s (unobservable, upstream failure, or upstream not ready; not converged this cycle)\n", sk)
	}

	// Advance the cumulative window counter for applied ops.
	for _, res := range desired {
		if !appliedIDs[res.ID.Local] {
			continue
		}
		if plan, ok := filteredPlans[res.ID.Local]; ok {
			budgetEngine.CommitOps(budgetDecision.OutsideLanes(plan.Ops))
		}
	}

	// Report applied ops: only for resources whose Apply succeeded.
	//
	// C (Scf7762-2): display/count MUST read from filteredPlans — the exact same
	// source ApplyWaves and CommitOps used — not the unfiltered plans map.  If we
	// read from plans here, an op that the budget gate blocked (removed from
	// filteredPlans, never applied, never committed) could still be printed and
	// counted as "applied".  With today's 1 resource = 1 op shape this is latent
	// (a blocked resource is absent from filteredPlans AND from appliedIDs, so it
	// is skipped by the guard above), but a future multi-op plan where only some
	// ops are destructive would show/count a blocked op as applied.  Reading from
	// filteredPlans keeps display, count, apply, and commit on one source.
	appliedOps := 0
	for _, res := range desired {
		if !appliedIDs[res.ID.Local] {
			continue
		}
		plan := filteredPlans[res.ID.Local]
		for _, op := range plan.Ops {
			printOp(op)
			appliedOps++
		}
	}

	// Phase 3: Prune (§12, ADR-0005, Scf7762-1 + Scf7762-2).
	//
	// Apply the subset of precomputed prune Delete ops that survived the budget gate.
	allowedPruneOps, blockedPruneOps := budgetDecision.FilterOps(precomputedPruneOps)
	for _, op := range blockedPruneOps {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: budget: blocked destructive op DELETE %s (budget limit)\n", op.Target)
	}

	pruneOps, pruneHalted := runPruneWithPrecomputed(ctx, applyProvider, exec, precomputedPruneOps, allowedPruneOps, pruneWasHaltedBySafetyGuard)
	for _, op := range pruneOps {
		printOp(op)
		budgetEngine.CommitOps(budgetDecision.OutsideLanes([]core.Op{op}))
		appliedOps++
	}
	if pruneHalted {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: prune halted this cycle (safety guard triggered); see above for reason\n")
	}

	switch {
	case capacityHalted && appliedOps == 0:
		fmt.Printf("Reconcile: CAPACITY HALTED (no changes applied): %s\n", capacityDecision.Reason)
		os.Exit(1)
	case budgetHalted && appliedOps == 0:
		// A (Scf7762-2): the BUDGET HALTED stderr line is already emitted once,
		// above, right after CheckCycle (the single stderr source of truth).  Do
		// NOT repeat it here — only the stdout summary belongs in this switch.
		fmt.Printf("Reconcile: BUDGET HALTED (no changes applied): %s\n", budgetDecision.Reason)
		os.Exit(1)
	case appliedOps > 0:
		if budgetHalted {
			fmt.Printf("Reconcile: %d change(s) applied (budget halted some ops)\n", appliedOps)
		} else {
			fmt.Printf("Reconcile: %d change(s) applied\n", appliedOps)
		}
	case len(skipped) > 0:
		// Do not claim convergence: some resources were skipped this cycle
		// (unobservable upstream or upstream Apply failure).
		fmt.Printf("Reconcile: no changes applied; %d resource(s) skipped\n", len(skipped))
	case scopeHeld.DriftCount > 0:
		fmt.Printf("Reconcile: no changes applied; %d drifted resource(s) held by alert-only scope(s)\n", scopeHeld.DriftCount)
	default:
		fmt.Println("Converged (no changes)")
	}

	// D-1 exit policy: an *unobservable* upstream is a safety failure — the
	// one-shot reconcile could not establish that reality is what we think, so
	// it exits non-zero to signal an incomplete/unsafe convergence.  Skips that
	// stem only from an upstream *Apply* failure remain exit 0 (level-triggered:
	// the resource was observable, the failure is transient and self-healing on
	// a subsequent reconcile) — this preserves AC-S94f1f9-2-2's exit-0 contract.
	// #25: waiting-allocation is not a safety failure: like an Apply-failure
	// skip it resolves itself on a later reconcile, once the allocation exists.
	// #40: a resource held by an alert-only scope (§14.3) was observed fine;
	// holding it is the declared policy, not a safety failure.
	// #41: a strategy: task resource held for its Task is not a safety failure
	// either — it was observed; only the daemon may launch the Task.  #42: the
	// same for a Workflow target.
	waitingCount, heldCount, taskCount, workflowCount := 0, 0, 0, 0
	for local := range unobservable {
		if waitingAlloc[local] {
			waitingCount++
		} else if scopeHeld.Locals[local] {
			heldCount++
		} else if taskHeld[local] {
			taskCount++
		} else if workflowHeld[local] {
			workflowCount++
		}
	}
	if waitingCount > 0 {
		fmt.Printf("Reconcile: %d resource(s) waiting-allocation (retry once the allocator can allocate)\n", waitingCount)
	}
	if taskCount > 0 {
		fmt.Printf("Reconcile: %d strategy: task resource(s) await their Task (launched by the daemon, §6.5)\n", taskCount)
	}
	if workflowCount > 0 {
		fmt.Printf("Reconcile: %d Workflow target(s) held for the daemon's Workflow (§6.3)\n", workflowCount)
	}
	if n := waitingCount + heldCount + taskCount + workflowCount; len(unobservable) > n {
		fmt.Fprintf(os.Stderr,
			"sakutei reconcile: %d resource(s) were unobservable this cycle; convergence incomplete\n",
			len(unobservable)-n)
		os.Exit(1)
	}
	if budgetHalted || capacityHalted {
		os.Exit(1)
	}
	os.Exit(0)
}

// parseClaimSelector parses a --claim scope-selector string into a core.Scope
// used as a claim matcher (§17.1, Sf6a62f-3).
//
// Syntax: comma-separated "key=value" pairs, e.g. "site=tokyo,cluster=prod".
// An empty string yields an empty selector (matches every scope: no claim
// restriction).  Whitespace around keys/values is trimmed.
//
// This is the seam parser: it turns the CLI flag into the same Scope vocabulary
// the loader assigns to each resource, so Scope.Matches can decide in/out of
// claim.  The real selector SOURCE (sparse checkout of intent/<scope>/) is v1b.
func parseClaimSelector(s string) (core.Scope, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return core.NewScope(nil), nil
	}
	labels := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if !ok || k == "" {
			return core.Scope{}, fmt.Errorf("selector term %q is not a key=value pair", part)
		}
		labels[k] = v
	}
	return core.NewScope(labels), nil
}

// computePruneForProvider runs the prune engine computation (§12, ADR-0005) for
// a single provider WITHOUT executing any Delete ops.  Returns the approved
// Delete ops and whether the prune safety guard halted prune.
//
// IMPORTANT (Scf7762-1): the one-shot "sakutei reconcile" creates a FRESH
// prune.Engine every invocation, so its List count-drop baseline (§12.4b) is
// reset to 0 on every run and the count-drop halt can NEVER fire here — a
// one-shot process has no "previous cycle" to compare against.  The count-drop
// guard only works in the long-lived daemon (internal/reconcile.Run).
func computePruneForProvider(
	ctx context.Context,
	fp provider.Provider,
	claim core.Scope,
	desired []diff.DesiredResource,
	unobservable map[string]bool,
	syncScopes []core.Scope,
) (approvedOps []core.Op, halted bool) {
	lister, ok := fp.(provider.Lister)
	if !ok {
		return nil, false
	}

	// Validate PruneWithinScope declarations against Lister capability.
	if err := prune.ValidatePrunePolicy(desired, fp); err != nil {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: prune policy error: %v\n", err)
		return nil, false
	}

	// List the whole provider; the engine narrows the set-difference to the
	// scopes that declare PruneWithinScope (#38).
	scope := core.NewScope(nil)

	// FIX #6: pass the --claim selector so the prune set-difference is scoped to
	// the claim.  Reality IDs outside the claim are dropped before set-difference,
	// so an out-of-claim resource is never treated as an orphan (§17.1, 原則3).
	// An empty selector matches everything → unchanged single-shard behaviour.
	engine := prune.New()
	engine.SetExcludedScopes(syncScopes) // #23: sync scopes are never pruned (§24.5)
	result, err := engine.ComputePrune(ctx, lister, fp, scope, claim, desired, unobservable, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: prune compute error: %v\n", err)
		return nil, false
	}

	if result.Halted {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: prune halted: %s\n", result.HaltReason)
		return nil, true
	}

	// Log deferred resources.
	for _, id := range result.Deferred {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: prune deferred for %s (safety guard: pruneable:false or unknown)\n", id)
	}

	return result.DeleteOps, false
}

// runPruneWithPrecomputed executes a pre-filtered set of prune Delete ops.
//
// precomputed is the full set returned by computePruneForProvider (for
// halted-by-guard detection); allowed is the budget-filtered subset that may
// actually execute.
func runPruneWithPrecomputed(
	ctx context.Context,
	fp provider.Provider,
	exec executor.Executor,
	precomputed []core.Op,
	allowed []core.Op,
	safetyGuardHalted bool,
) (appliedOps []core.Op, halted bool) {
	if safetyGuardHalted {
		return nil, true
	}
	_ = precomputed // reserved for future diagnostics
	_ = fp          // interface kept for call-site symmetry

	// Apply each budget-approved Delete op individually.
	for _, op := range allowed {
		plan := core.Plan{Ops: []core.Op{op}}
		if applyErr := exec.Execute(ctx, fp, plan); applyErr != nil {
			fmt.Fprintf(os.Stderr, "sakutei reconcile: prune apply %s: %v\n", op.Target, applyErr)
			// Continue with remaining ops (level-triggered: retry next cycle).
			continue
		}
		appliedOps = append(appliedOps, op)
	}

	return appliedOps, false
}

// multiProviderApply is a provider.Provider adapter that routes Read and Apply
// to the correct per-resource provider based on the resource's provider name.
//
// This allows the existing reconcile.ApplyWaves (which takes a single
// provider.Provider) to work with multiple providers without changes to the
// internal/reconcile package.
//
// S62dbba-2: this is intentionally an internal cmd-layer adapter, NOT a public
// pkg/ type.  It wires the multi-provider map to the single-provider interface
// expected by ApplyWaves.  If multi-provider becomes a first-class concept in
// the reconcile core, this adapter can be promoted or replaced.
type multiProviderApply struct {
	// providers maps provider name → provider instance.
	providers map[string]provider.Provider
	// localToProvider maps resource local ID → provider name.
	// Built once at construction from the desired resource list.
	localToProvider map[string]string
	// fallback is used when the local ID is not found in localToProvider
	// (should not happen in normal operation).
	fallback provider.Provider
}

// Compile-time assertion that multiProviderApply satisfies provider.Provider.
var _ provider.Provider = (*multiProviderApply)(nil)

// newMultiProviderApply creates a multiProviderApply from the given providers
// map and the desired resource list.
func newMultiProviderApply(providers map[string]provider.Provider, desired []diff.DesiredResource) *multiProviderApply {
	localToProvider := make(map[string]string, len(desired))
	for _, r := range desired {
		localToProvider[r.ID.Local] = r.ID.Provider
	}
	// Pick an arbitrary fallback (the first one in iteration order).
	var fallback provider.Provider
	for _, p := range providers {
		fallback = p
		break
	}
	return &multiProviderApply{
		providers:       providers,
		localToProvider: localToProvider,
		fallback:        fallback,
	}
}

// Schema returns the schema of the fallback provider.
//
// Note: in a multi-provider scenario each provider has its own schema.  The
// Schema method here is required by the provider.Provider interface but is not
// used by ApplyWaves (which routes Apply, not Schema/Diff).  The per-provider
// diff engines are built separately in runReconcile.
func (m *multiProviderApply) Schema() provider.Schema {
	if m.fallback != nil {
		return m.fallback.Schema()
	}
	return provider.Schema{}
}

// Capabilities returns the capabilities of the fallback provider.
// See Schema() for the same caveat about per-provider capabilities.
func (m *multiProviderApply) Capabilities() provider.Capabilities {
	if m.fallback != nil {
		return m.fallback.Capabilities()
	}
	return provider.Capabilities{}
}

// Read routes to the provider responsible for the given local ID.
func (m *multiProviderApply) Read(ctx context.Context, id string) (core.Observation, error) {
	p := m.providerFor(id)
	return p.Read(ctx, id)
}

// Apply routes each op in the plan to the provider responsible for its target.
//
// All ops in a plan MUST belong to the same provider (ApplyWaves sends one
// plan per resource, and each resource has a single provider).  If ops in a
// plan span providers, they are dispatched to their respective providers in
// separate Apply calls.
func (m *multiProviderApply) Apply(ctx context.Context, plan core.Plan) error {
	// Group ops by provider.
	byProvider := make(map[string][]core.Op)
	var providerOrder []string
	for _, op := range plan.Ops {
		// Resolve the owning provider for this op.
		//
		// AF-1 (S62dbba verify): localToProvider is built from the DESIRED
		// resource list, so it only knows resources the operator declared.  A
		// prune Delete op targets an ORPHAN — a resource present in reality but
		// absent from desired — so its Local ID is NOT in localToProvider and the
		// lookup returns "".  Falling back to an arbitrary provider here would
		// mis-route the delete: in a mixed fake+dnszone intent, a DNS orphan
		// delete could be dispatched to fakeprovider.Apply (the wrong provider
		// deletes the wrong thing).  The prune engine sets op.Target.Provider
		// from the Lister's own IDs, so prefer that; localToProvider is used only
		// as a corroborating source for desired-derived ops.
		name := m.localToProvider[op.Target.Local]
		if name == "" {
			// Orphan prune target (or otherwise not in desired): route by the
			// provider the op's target carries, which the prune engine populates.
			name = op.Target.Provider
		}
		if _, seen := byProvider[name]; !seen {
			providerOrder = append(providerOrder, name)
		}
		byProvider[name] = append(byProvider[name], op)
	}

	for _, name := range providerOrder {
		ops := byProvider[name]
		var p provider.Provider
		if name == "" {
			p = m.fallback
		} else {
			var ok bool
			p, ok = m.providers[name]
			if !ok {
				p = m.fallback
			}
		}
		if err := p.Apply(ctx, core.Plan{Ops: ops}); err != nil {
			return err
		}
	}
	return nil
}

// List implements provider.Lister by routing to the fallback provider's List.
//
// For multi-provider prune, the per-provider prune logic (computePruneForProvider)
// calls each provider's Lister directly.  This method exists so that the
// multiProviderApply can also satisfy provider.Lister when needed by ApplyWaves.
func (m *multiProviderApply) List(ctx context.Context, scope core.Scope) (core.ListResult, error) {
	lister, ok := m.fallback.(provider.Lister)
	if !ok {
		return core.ListResult{}, nil
	}
	return lister.List(ctx, scope)
}

// providerFor returns the provider responsible for the given local ID.
func (m *multiProviderApply) providerFor(localID string) provider.Provider {
	name, ok := m.localToProvider[localID]
	if !ok || name == "" {
		return m.fallback
	}
	p, ok := m.providers[name]
	if !ok {
		return m.fallback
	}
	return p
}
