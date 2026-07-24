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

	"github.com/tjst-t/sakutei/internal/cause"
	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/fakeprovider"
	"github.com/tjst-t/sakutei/internal/loader"
	"github.com/tjst-t/sakutei/internal/prune"
	"github.com/tjst-t/sakutei/internal/render"
	"github.com/tjst-t/sakutei/internal/revision"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// runPlan implements "sakutei plan".
//
// It loads desired resources from --intent <dir>, observes actual state via
// the fake provider backed by --state <file>, computes the diff, and prints
// the result.
//
// ADR-0002 contract: Apply is NEVER called.  This function stops at Diff.
//
// Scb0ecd-2: cause-tag classification (A/B/C active, D/E seam).
//   - --cause-state <file>: optional JSON file holding previous-cycle revision
//     and provider_version values.  Written at end of invocation so the next
//     run can compare.  Absent = first-run (all ops tagged A, correct).
//
// Exit codes:
//
//	0 — plan completed (with or without diffs; no-diff prints "Converged (no changes)")
//	1 — error (flag parse error, intent load failure, observe error, etc.)
func runPlan(args []string) {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	intentDir := fs.String("intent", "", "directory containing intent JSON files (required)")
	statePath := fs.String("state", "sakutei-fake-state.json",
		"path to the fake provider state file (default: sakutei-fake-state.json)")
	// Scb0ecd-2: cause-state file for cross-invocation revision tracking.
	causeStatePath := fs.String("cause-state", "",
		"path to cause-state JSON file for cross-invocation revision tracking (Scb0ecd-2); optional")
	// S072a14-1: generated/ output directory for render phase 2 (§9.6).
	// Default: sibling of --intent named "generated" (e.g. ./generated when intent=./intent).
	generatedDir := fs.String("generated", "",
		"directory to write render phase 2 output (default: sibling of --intent named 'generated')")
	// S072a14-2: render phase 1 — external input resolver flags.
	// --inventory-hash injects a deterministic hash into the rendered_revision
	// computation (simulating a FakeInventoryResolver for acceptance tests).
	// --inventory-unreachable simulates an unreachable external authority to test
	// the §9.2 last-value-on-unreachable safety path.
	inventoryHash := fs.String("inventory-hash", "",
		"S072a14-2: inject deterministic inventory hash into rendered_revision (fake phase 1; empty=no external input)")
	inventoryUnreachable := fs.Bool("inventory-unreachable", false,
		"S072a14-2: simulate an unreachable ExternalInputResolver (triggers last-value-on-unreachable path §9.2)")

	if err := fs.Parse(args); err != nil {
		// flag package already printed the error.
		os.Exit(1)
	}

	if *intentDir == "" {
		fmt.Fprintln(os.Stderr, "sakutei plan: --intent <dir> is required")
		fs.Usage()
		os.Exit(1)
	}

	// Resolve generated/ directory (S072a14-1: §9.6).
	genDir := *generatedDir
	if genDir == "" {
		genDir = filepath.Join(filepath.Dir(*intentDir), "generated")
	}

	// Load desired resources from intent directory.
	// Scb0ecd-1: LoadDir now also returns RevisionInfo.
	intentResources, rev, err := loader.LoadDir(*intentDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sakutei plan: load intent: %v\n", err)
		os.Exit(1)
	}

	// S072a14-2: render phase 1 — external input resolution (§9.1 フェーズ1).
	// Build a FakeInventoryResolver from the --inventory-hash and
	// --inventory-unreachable flags, then resolve through a zero-TTL cache.
	//
	// When --inventory-hash is empty AND --inventory-unreachable is false, no
	// resolver is configured and phase 1 is a no-op (v1a default: rendered ==
	// intent).  This preserves backward compatibility with existing tests.
	if *inventoryHash != "" || *inventoryUnreachable {
		fakeResolver := &render.FakeInventoryResolver{
			Hash: *inventoryHash,
			Fail: *inventoryUnreachable,
		}
		// Use a zero-TTL cache: every plan invocation resolves fresh.
		cache := render.NewResolverCache(fakeResolver, 0)
		ctx := context.Background()
		phase1Result := render.ResolveExternalInputs(ctx, cache)
		if phase1Result.UnreachableFallback {
			fmt.Fprintf(os.Stderr,
				"sakutei plan: ExternalInputUnreachable: resolver returned error; using last cached value (§9.2 last-value-on-unreachable)\n")
		}
		if phase1Result.FirstCallFailed {
			// No prior value — halt-safe: do not converge (§9.2).
			fmt.Fprintf(os.Stderr,
				"sakutei plan: ExternalInputUnreachable (first call): %v\n"+
					"  No prior cached value available; convergence not advanced (halt-safe, §9.2)\n",
				phase1Result.Err)
			os.Exit(1)
		}
		// Recompute rendered_revision with the external inputs so that a change
		// in inventory hash causes rendered_revision to diverge from
		// intent_revision (cause tag E activation basis, §9.4, ADR-0008).
		//
		// IMPORTANT — rendered_revision 算出定義は §26 カテゴリ A.
		// The v1a canonical definition is provisional: only inventory_hash is
		// included.  Vault secret generations and allocator epochs are v1b/S8.
		rootScope := core.NewScope(nil)
		rev.RenderedRevision = revision.ComputeRenderedRevision(
			rootScope, rev.IntentRevision, phase1Result.ExternalInputHashes)
	}

	// S072a14-1: render phase 2 — expand composite resources into concrete
	// children (§9.5).  Composites themselves are NOT passed to Diff/Apply;
	// only their expanded children enter the convergence loop.
	registry := render.DefaultRegistry()
	expanded, expandErr := render.ExpandAll(registry, intentResources)
	if expandErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei plan: render phase 2: %v\n", expandErr)
		os.Exit(1)
	}

	// §9.6: validate the existing generated/ file for direct human edits.
	// A missing file is fine; a file without _generated:true triggers a warning.
	genFilePath := filepath.Join(genDir, render.GeneratedFileName)
	if valErr := render.ValidateGeneratedFile(genFilePath); valErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei plan: WARNING: %v\n", valErr)
	}

	// §9.6: write expanded children to generated/expanded.json.
	if writeErr := render.WriteGenerated(genDir, expanded); writeErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei plan: write generated/: %v\n", writeErr)
		os.Exit(1)
	}

	// Build the desired slice: only non-composite resources enter Diff/Apply.
	// Composite parents are excluded (children represent them in the DAG).
	desired := make([]diff.DesiredResource, 0, len(expanded))
	for _, r := range expanded {
		if !r.IsComposite {
			desired = append(desired, r.DesiredResource)
		}
	}

	// Scb0ecd-1: print revision header so the caller can observe revision values.
	// In v1a rendered_revision = intent_revision (no external inputs yet; seam
	// for S072a14 where external inputs enter the rendered hash).
	fmt.Printf("intent_revision=%s rendered_revision=%s\n", rev.IntentRevision, rev.RenderedRevision)

	// Create the fake provider backed by the state file.
	fp := fakeprovider.New(*statePath)

	// Scb0ecd-3: fail-closed provider_version validation (§10.4, ADR-0008).
	// plan also validates so that operators get an immediate error rather than
	// silently producing plans that cannot be cause-C–attributed.
	if err := provider.ValidateProvider(fp); err != nil {
		fmt.Fprintf(os.Stderr, "sakutei plan: provider validation failed: %v\n", err)
		os.Exit(1)
	}

	// Scb0ecd-2: load cause state (previous-cycle revision + provider_version).
	prevState, csErr := loadCauseState(*causeStatePath)
	if csErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei plan: warn: load cause-state %q: %v (treating as first-run)\n",
			*causeStatePath, csErr)
		prevState = causeStateFile{}
	}
	curProviderVersion := fp.ProviderVersion()

	// S072a14-3: compute RenderedRevisionChanged for cause E.
	// E fires when rendered_revision changed but intent_revision is stable.
	renderedRevisionChanged := prevState.PrevRenderedRevision != "" &&
		rev.RenderedRevision != prevState.PrevRenderedRevision &&
		rev.IntentRevision == prevState.PrevIntentRevision

	// S072a14-3: build the parent→children map for cause D propagation.
	// A parent "changed" this cycle if intent or rendered revision changed.
	parentToChildren := render.BuildParentChildMap(expanded)
	intentChanged := prevState.PrevIntentRevision != "" && rev.IntentRevision != prevState.PrevIntentRevision
	renderedChanged := prevState.PrevRenderedRevision != "" && rev.RenderedRevision != prevState.PrevRenderedRevision
	parentDesiredChanged := intentChanged || renderedChanged
	childrenWithUpstreamChange := make(map[string]bool)
	if parentDesiredChanged {
		for _, childIDs := range parentToChildren {
			for _, childID := range childIDs {
				childrenWithUpstreamChange[childID] = true
			}
		}
	}

	// Build the base per-invocation CauseContext.  All resources in this invocation
	// share the same revision signals (intent_revision, rendered_revision,
	// provider_version); per-resource differentiation (§10.3) is structural:
	// the same intent_revision change affects ALL resources in this intent
	// directory (which is the correct scope unit, ADR-0008).
	//
	// S072a14-3: RenderedRevisionChanged is now activated (E), and UpstreamChanged
	// is set per-resource below for children of composite parents that changed.
	baseCauseCtx := cause.CauseContext{
		IntentPrevRevision:      prevState.PrevIntentRevision,
		IntentCurRevision:       rev.IntentRevision,
		RenderedPrevRevision:    prevState.PrevRenderedRevision,
		RenderedCurRevision:     rev.RenderedRevision,
		ProviderPrevVersion:     prevState.PrevProviderVersion,
		ProviderCurVersion:      curProviderVersion,
		RenderedRevisionChanged: renderedRevisionChanged,
		// UpstreamChanged is set per-resource below.
	}

	engine := diff.New(fp.Schema())

	ctx := context.Background()
	var allOps []core.Op

	// Track unobservable resources for the prune phase (unknown ≠ absent).
	unobservable := make(map[string]bool)

	for _, res := range desired {
		// ADR-0002: Observe independently for each resource.  Each call to
		// Read is fresh; no observation is carried across resources or cycles.
		obs, err := fp.Read(ctx, res.ID.Local)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sakutei plan: observe %s: %v\n", res.ID, err)
			os.Exit(1)
		}
		if obs.Availability != core.AvailabilityOk {
			unobservable[res.ID.Local] = true
			continue
		}

		// S072a14-3: set UpstreamChanged=true for children of composite parents
		// whose desired state changed this cycle (cause D).
		resCauseCtx := baseCauseCtx
		if childrenWithUpstreamChange[res.ID.Local] {
			resCauseCtx.UpstreamChanged = true
		}

		plan := engine.Diff(obs, res, resCauseCtx)
		// Scb0ecd-2: log ProviderUpgradeDiff event for cause-C ops (ghost diff).
		for _, op := range plan.Ops {
			if op.Cause == core.CauseTagC {
				fmt.Fprintf(os.Stderr,
					"sakutei plan: ProviderUpgradeDiff: %s [cause: C] provider_version changed (%s → %s); review before applying\n",
					op.Target, prevState.PrevProviderVersion, curProviderVersion)
			}
		}
		allOps = append(allOps, plan.Ops...)
	}

	// Scb0ecd-2: persist current revision + provider_version as "previous" for
	// the next invocation.  Non-fatal on failure (S3 semantics).
	if saveErr := saveCauseState(*causeStatePath, causeStateFile{
		PrevIntentRevision:   rev.IntentRevision,
		PrevRenderedRevision: rev.RenderedRevision,
		PrevProviderVersion:  curProviderVersion,
	}); saveErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei plan: warn: save cause-state %q: %v\n", *causeStatePath, saveErr)
	}

	// Prune phase (dry-run): show what would be deleted (§12, ADR-0005).
	// plan does NOT call Apply; it only computes and displays the Delete ops.
	allOps = append(allOps, planPruneOps(ctx, fp, desired, unobservable)...)

	// S072a14-3: compute composite status aggregation (§9.7) for reporting.
	// Build per-resource ConvergenceStatus from diff results and unobservable set.
	resourceStatus := make(map[string]core.ConvergenceStatus)
	for _, res := range desired {
		local := res.ID.Local
		if unobservable[local] {
			resourceStatus[local] = core.StatusUnknown
			continue
		}
		// Check if this resource has any planned ops (= drifted).
		hasPlan := false
		for _, op := range allOps {
			if op.Target.Local == local {
				hasPlan = true
				break
			}
		}
		if hasPlan {
			resourceStatus[local] = core.StatusDrifted
		} else {
			resourceStatus[local] = core.StatusConverged
		}
	}
	// Compute composite status summaries.
	compositeSummaries := render.AggregateCompositeStatuses(parentToChildren, resourceStatus)

	// Print plan output.
	if len(allOps) == 0 {
		// Even if no ops, print composite status if there are composites.
		if len(compositeSummaries) > 0 {
			fmt.Println("Composite status:")
			for parentID, summary := range compositeSummaries {
				fmt.Printf("  composite %s: %s\n", parentID, summary.AggregatedStatus)
			}
		}
		fmt.Println("Converged (no changes)")
		os.Exit(0)
	}

	fmt.Printf("Plan: %d change(s)\n\n", len(allOps))
	for _, op := range allOps {
		printOp(op)
	}

	// S072a14-3: print composite status aggregation (§9.7).
	if len(compositeSummaries) > 0 {
		fmt.Println("Composite status (§9.7 worst-first):")
		for parentID, summary := range compositeSummaries {
			fmt.Printf("  composite %s: %s\n", parentID, summary.AggregatedStatus)
		}
	}
	os.Exit(0)
}

// planPruneOps computes the Delete ops that would be emitted by the prune engine
// for the given desired resources.  No Apply is called (dry-run).
func planPruneOps(
	ctx context.Context,
	fp provider.Provider,
	desired []diff.DesiredResource,
	unobservable map[string]bool,
) []core.Op {
	lister, ok := fp.(provider.Lister)
	if !ok {
		return nil
	}

	scope := core.NewScope(nil)
	engine := prune.New()
	result, err := engine.ComputePrune(ctx, lister, fp, scope, desired, unobservable, os.Stderr)
	if err != nil || result.Halted {
		return nil
	}
	return result.DeleteOps
}

// printOp formats a single Op for human-readable plan output.
func printOp(op core.Op) {
	var kindStr string
	var marker string
	switch op.Kind {
	case core.OpKindCreate:
		kindStr = "CREATE"
		marker = "+"
	case core.OpKindUpdate:
		kindStr = "UPDATE"
		marker = "~"
	case core.OpKindDelete:
		kindStr = "DELETE"
		marker = "-"
	default:
		kindStr = "UNKNOWN"
		marker = "?"
	}

	destructiveNote := ""
	if op.Destructive {
		destructiveNote = " [DESTRUCTIVE]"
	}

	fmt.Printf("%s %s %s%s\n", marker, kindStr, op.Target, destructiveNote)
	if op.Diagnostic != "" {
		fmt.Printf("  reason: %s\n", op.Diagnostic)
	}
	if op.Cause != "" {
		fmt.Printf("  cause:  %s\n", string(op.Cause))
	}
	for _, fd := range op.Fields {
		if fd.From == "" {
			fmt.Printf("  + %-20s  (new) → %q\n", fd.Field, fd.To)
		} else if fd.To == "" {
			fmt.Printf("  - %-20s  %q → (removed)\n", fd.Field, fd.From)
		} else {
			fmt.Printf("  ~ %-20s  %q → %q\n", fd.Field, fd.From, fd.To)
		}
	}
	fmt.Println()
}
