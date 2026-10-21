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
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/internal/cause"
	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/loader"
	"github.com/tjst-t/sakutei/internal/prune"
	"github.com/tjst-t/sakutei/internal/render"
	"github.com/tjst-t/sakutei/internal/revision"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
	"github.com/tjst-t/sakutei/pkg/registry"
)

// runPlan implements "sakutei plan".
//
// It loads desired resources from --intent <dir>, observes actual state via
// the provider resolved from each resource's "provider" field (via the
// ProviderRegistry), computes the diff, and prints the result.
//
// ADR-0002 contract: Apply is NEVER called.  This function stops at Diff.
//
// Scb0ecd-2: cause-tag classification (A/B/C active, D/E seam).
//   - --cause-state <file>: optional JSON file holding previous-cycle revision
//     and provider_version values.  Written at end of invocation so the next
//     run can compare.  Absent = first-run (all ops tagged A, correct).
//
// S62dbba-2: provider is no longer hardcoded to fakeprovider.  The
// ProviderRegistry resolves the correct provider for each resource based on
// the "provider" field in the intent file.
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
	// S33266e-3: devenv（坪庭）— substitute ALL render phase-1 external inputs
	// (inventory/allocation/secret) with fakes so no production pool/secret is
	// contacted (§26.2).  When set, phase 1 resolves through the DevenvProfile's
	// substituted inventory cache instead of the plain --inventory-hash resolver.
	allocStore := fs.String("alloc-store", "",
		"#24: path to the allocation store file (file AllocationAuthority, §19.9); required when the intent uses from_pool")
	allocURL := fs.String("alloc-url", "", allocURLUsage)
	devenv := fs.Bool("devenv", false,
		"S33266e-3: devenv（坪庭）mode — render external inputs (inventory/allocation/secret) are substituted with fakes; no production pool/secret is touched (§26.2)")

	if err := fs.Parse(args); err != nil {
		// flag package already printed the error.
		os.Exit(1)
	}
	soaSerial := checkZoneSOASerial("sakutei plan", *zoneSOASerial)

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
	in, err := loader.LoadIntent(*intentDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sakutei plan: load intent: %v\n", err)
		os.Exit(1)
	}
	intentResources, rev := in.Resources, in.Revision
	var externalInputs []string
	recomputeRendered := false

	// S072a14-2: render phase 1 — external input resolution (§9.1 フェーズ1).
	// Build a FakeInventoryResolver from the --inventory-hash and
	// --inventory-unreachable flags, then resolve through a zero-TTL cache.
	//
	// When --inventory-hash is empty AND --inventory-unreachable is false, no
	// resolver is configured and phase 1 is a no-op (v1a default: rendered ==
	// intent).  This preserves backward compatibility with existing tests.
	if *devenv || *inventoryHash != "" || *inventoryUnreachable {
		var cache *render.ResolverCache
		if *devenv {
			// S33266e-3: devenv（坪庭）— ALL external inputs substituted with
			// fakes.  Phase 1 runs through the DevenvProfile's substituted
			// inventory cache (built on the same FakeInventoryResolver +
			// ResolverCache path); no production pool/secret is contacted (§26.2).
			// --inventory-hash, when also given, seeds the fake inventory hash.
			opts := []render.DevenvOption{}
			if *inventoryHash != "" {
				opts = append(opts, render.WithDevenvInventoryHash(*inventoryHash))
			}
			cache = render.NewDevenvProfile(opts...).ExternalInputCache()
			fmt.Fprintln(os.Stderr,
				"sakutei plan: devenv（坪庭）mode — external inputs substituted with fakes; no production pool/secret contacted (§26.2)")
		} else {
			fakeResolver := &render.FakeInventoryResolver{
				Hash: *inventoryHash,
				Fail: *inventoryUnreachable,
			}
			// Use a zero-TTL cache: every plan invocation resolves fresh.
			cache = render.NewResolverCache(fakeResolver, 0)
		}
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
		externalInputs = append(externalInputs, phase1Result.ExternalInputHashes...)
		recomputeRendered = true
	}

	// #24: render phase 1 — allocation (§9.1, §19).  plan is read-only, so it
	// only LOOKS UP existing allocations; a request with none yet is shown as
	// pending and is allocated by reconcile.
	allocAuth, allocErr := openAllocStore(*allocStore, *allocURL)
	if allocErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei plan: %v\n", allocErr)
		os.Exit(1)
	}
	allocRes, allocErr := render.RenderAllocations(context.Background(), allocAuth, render.AllocLookupOnly, in, core.Scope{})
	if allocErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei plan: %v\n", allocErr)
		os.Exit(1)
	}
	intentResources = allocRes.Resources
	if len(allocRes.Inputs) > 0 {
		externalInputs = append(externalInputs, allocRes.Inputs...)
		recomputeRendered = true
	}
	for _, pa := range allocRes.Pending {
		fmt.Fprintf(os.Stderr, "sakutei plan: pending allocation: %s field %q from pool %q (allocated by reconcile)%s\n", pa.ID, pa.Field, pa.Pool, pendingReason(pa))
	}
	if recomputeRendered {
		rootScope := core.NewScope(nil)
		rev.RenderedRevision = revision.ComputeRenderedRevision(
			rootScope, rev.IntentRevision, externalInputs)
	}

	// S072a14-1: render phase 2 — expand composite resources into concrete
	// children (§9.5).  Composites themselves are NOT passed to Diff/Apply;
	// only their expanded children enter the convergence loop.
	expRegistry := render.DefaultRegistry()
	expanded, expandErr := render.ExpandAll(expRegistry, intentResources)
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
	providers, resolveErr := resolveProviders(reg, desired, providerConfig, "sakutei plan")
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
	// With multiple providers, concatenate all "name:version" pairs in sorted
	// order so the combined version string is deterministic.
	// For v1a (single "fake" provider) this reduces to the fake's version.
	curProviderVersion := joinedProviderVersions(providers)

	// Scb0ecd-2: load cause state (previous-cycle revision + provider_version).
	prevState, csErr := loadCauseState(*causeStatePath)
	if csErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei plan: warn: load cause-state %q: %v (treating as first-run)\n",
			*causeStatePath, csErr)
		prevState = causeStateFile{}
	}

	// S072a14-3: compute RenderedRevisionChanged for cause E.
	// E fires when rendered_revision changed but intent_revision is stable.
	renderedRevisionChanged := prevState.PrevRenderedRevision != "" &&
		rev.RenderedRevision != prevState.PrevRenderedRevision &&
		rev.IntentRevision == prevState.PrevIntentRevision

	// S072a14-3: build the parent→children map for cause D propagation.
	// #14: a parent "changed" this cycle if its own rendered contribution
	// differs from the previous invocation's, so only its children get D.
	parentToChildren := render.BuildParentChildMap(expanded)
	curParentRevs := render.ParentRevisions(expanded)
	childrenWithUpstreamChange := render.UpstreamChangedChildren(prevState.PrevParentRevisions, curParentRevs, parentToChildren)

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

	ctx := context.Background()
	var allOps []core.Op

	// Track unobservable resources for the prune phase (unknown ≠ absent).
	unobservable := make(map[string]bool)

	for _, res := range desired {
		// S62dbba-2: route Read and Diff to the provider for this resource.
		fp, ok := providers[res.ID.Provider]
		if !ok {
			// Should not happen: resolveProviders already validated all names.
			fmt.Fprintf(os.Stderr, "sakutei plan: internal error: no provider for %q\n", res.ID.Provider)
			os.Exit(1)
		}
		engine := engines[res.ID.Provider]

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
		// Fix Sc7a647 cause-consistency: tell Classify whether this op will be a
		// Create (resource absent in reality) so a cold-start (empty prev
		// revision) Create is A but a cold-start drift-repair UPDATE is B — the
		// same test the diff engine uses (obs.State == nil → OpKindCreate).
		resCauseCtx.ResourceObservedAbsent = obs.State == nil

		// S33266e-1 (§20): a bare plaintext in a secret-flagged field is a fatal
		// intent error (plaintext secret + perpetual non-converging diff).  Fail
		// loud; the error names the field only, never the value.
		if err := engine.ValidateDesired(res); err != nil {
			fmt.Fprintf(os.Stderr, "sakutei plan: %v\n", err)
			os.Exit(1)
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
		PrevParentRevisions:  curParentRevs,
	}); saveErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei plan: warn: save cause-state %q: %v\n", *causeStatePath, saveErr)
	}

	// Prune phase (dry-run): show what would be deleted (§12, ADR-0005).
	// plan does NOT call Apply; it only computes and displays the Delete ops.
	// S62dbba-2: prune is computed per provider so each provider's Lister is
	// used for the resources it manages.
	allOps = append(allOps, planPruneOpsMulti(ctx, providers, desired, unobservable, syncScopeList(in))...)

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
			// F-3: sort parent IDs so the output order is deterministic
			// (map iteration order is randomised in Go).
			parentIDs := sortedCompositeParentIDs(compositeSummaries)
			for _, parentID := range parentIDs {
				fmt.Printf("  composite %s: %s\n", parentID, compositeSummaries[parentID].AggregatedStatus)
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
		// F-3: sort parent IDs so the output order is deterministic
		// (map iteration order is randomised in Go).
		parentIDs := sortedCompositeParentIDs(compositeSummaries)
		for _, parentID := range parentIDs {
			fmt.Printf("  composite %s: %s\n", parentID, compositeSummaries[parentID].AggregatedStatus)
		}
	}
	os.Exit(0)
}

// sortedCompositeParentIDs returns the parent Local IDs of the given composite
// status summaries in ascending (sort.Strings) order.
//
// F-3: composite status is printed by iterating this slice rather than ranging
// over the map directly, so the CLI output order is deterministic across runs
// (Go randomises map iteration order).
func sortedCompositeParentIDs(summaries map[string]render.CompositeStatusSummary) []string {
	ids := make([]string, 0, len(summaries))
	for parentID := range summaries {
		ids = append(ids, parentID)
	}
	sort.Strings(ids)
	return ids
}

// planPruneOpsMulti computes Delete ops for the prune engine across all
// providers.  It calls the per-provider Lister (if the provider implements
// provider.Lister) with only the resources managed by that provider.
//
// No Apply is called (dry-run).
func planPruneOpsMulti(
	ctx context.Context,
	providers map[string]provider.Provider,
	desired []diff.DesiredResource,
	unobservable map[string]bool,
	syncScopes []core.Scope,
) []core.Op {
	var allDeleteOps []core.Op

	// Group desired resources by provider.
	byProvider := make(map[string][]diff.DesiredResource)
	for _, r := range desired {
		byProvider[r.ID.Provider] = append(byProvider[r.ID.Provider], r)
	}

	for name, fp := range providers {
		lister, ok := fp.(provider.Lister)
		if !ok {
			continue
		}
		scope := core.NewScope(nil)
		engine := prune.New()
		engine.SetExcludedScopes(syncScopes) // #23: sync scopes are never pruned (§24.5)
		provDesired := byProvider[name]
		// FIX #6: plan does not (yet) accept --claim, so the claim selector is
		// empty here — an empty selector matches every scope, keeping plan's
		// whole-world prune preview behaviour unchanged.
		result, err := engine.ComputePrune(ctx, lister, fp, scope, core.NewScope(nil), provDesired, unobservable, os.Stderr)
		if err != nil || result.Halted {
			continue
		}
		allDeleteOps = append(allDeleteOps, result.DeleteOps...)
	}
	return allDeleteOps
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

// joinedProviderVersions returns a combined provider version string for all
// providers in the map.  Versions are joined in sorted key order so the result
// is deterministic.
//
// For a single provider "fake" this returns the fake's ProviderVersion string
// unchanged (no separator prefix), preserving backward compatibility with
// existing cause-state files written before S62dbba-2.
// For multiple providers the format is "name1:v1,name2:v2".
func joinedProviderVersions(providers map[string]provider.Provider) string {
	if len(providers) == 0 {
		return ""
	}
	// Collect sorted names for deterministic output.
	names := make([]string, 0, len(providers))
	for n := range providers {
		names = append(names, n)
	}
	sort.Strings(names)

	if len(names) == 1 {
		// Single provider: return its version as-is for backward compatibility.
		return providers[names[0]].Capabilities().ProviderVersion
	}

	// Multiple providers: "name1:v1,name2:v2".
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, n+":"+providers[n].Capabilities().ProviderVersion)
	}
	return strings.Join(parts, ",")
}
