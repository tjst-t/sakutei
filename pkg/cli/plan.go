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
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/cause"
	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/eventlog"
	"github.com/tjst-t/sakutei/v2/internal/extowner"
	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/planreport"
	"github.com/tjst-t/sakutei/v2/internal/prune"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/internal/remediation"
	"github.com/tjst-t/sakutei/v2/internal/render"
	"github.com/tjst-t/sakutei/v2/internal/revision"
	"github.com/tjst-t/sakutei/v2/internal/task"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
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
	providerConfigFile := providerConfigFlag(fs)
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
	zoneScope := zoneScopeFlag(fs)
	rulesetScope := rulesetScopeFlag(fs)
	rulesetTarget := rulesetTargetFlag(fs)
	redfishConfig := redfishConfigFlag(fs)
	sqlmigrateConfig := sqlmigrateConfigFlag(fs)
	s3Config := s3ConfigFlag(fs)
	postgresqlConfig := postgresqlConfigFlag(fs)
	ansibleConfig := ansibleConfigFlag(fs)
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
	// #115: the daemon's inventory composition (§9.1, §9.2), so plan shows
	// the same desired the daemon converges.
	invFlags := addInventoryFlags(fs, true)
	allocStore := fs.String("alloc-store", "",
		"#24: path to the allocation store file (file AllocationAuthority, §19.9); required when the intent uses from_pool")
	allocURL := fs.String("alloc-url", "", allocURLUsage)
	allocWriteBack := fs.String("alloc-write-back", "", allocWriteBackUsage)
	devenv := fs.Bool("devenv", false,
		"S33266e-3: devenv（坪庭）mode — render external inputs (inventory/allocation/secret) are substituted with fakes; no production pool/secret is touched (§26.2)")

	// #89 (§6.3): the daemon's workflow store, read only to show the stage of
	// each Workflow whose targets the loop leaves to it.
	workflowStore := fs.String("workflow-store", "",
		"OPTIONAL path to the daemon's workflow store (§6.3): shows the stage of each Workflow whose targets are left to it; plan only reads it")

	// #111: --claim, the same scope-selector as reconcile (§17.1).  A shard's
	// plan shows only its claim: the other shards' resources are not observed,
	// not pruned, and their allocations are not reported as pending (§19.8).
	claim := fs.String("claim", "",
		"scope-selector restricting the plan to a claim, e.g. site=tokyo,cluster=prod (§17.1); empty=whole world")

	// #135: the change budget, planned_change lanes and capacity gates of
	// reconcile, evaluated (never consumed) to show what a reconcile now
	// would halt.  --audit-log is only read: plan writes nothing to S4.
	budgetFl := addBudgetFlags(fs)
	auditLog := fs.String("audit-log", "",
		"S4 audit log file read (never written) to evaluate the planned_change lanes as a reconcile with the same --audit-log would (§13.3, §14.5)")
	var capacityGates capacityGateFlag
	fs.Var(&capacityGates, "capacity-gate", capacityGateUsage)

	// #135: one JSON object on stdout (schema_version 1, docs/plan-json.md).
	jsonOut := fs.Bool("json", false,
		"print the plan as one JSON object on stdout (schema_version 1, see docs/plan-json.md); exit codes are unchanged")

	logFormat := addLogFormatFlag(fs)

	if err := fs.Parse(args); err != nil {
		// flag package already printed the error.
		os.Exit(1)
	}
	setLogFormat("sakutei plan", *logFormat)

	// #135: the text and the JSON are made from one report.  With --json the
	// text lines of stdout are dropped and the report is printed instead; the
	// notices on stderr are unchanged.
	report := planreport.New(time.Now())
	if *jsonOut {
		logOut = io.Discard
	}
	// fail ends the plan with exit code 1: the reason goes to stderr and, with
	// --json, into the report's error.
	fail := func(format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		fmt.Fprintln(logErr, msg)
		if *jsonOut {
			report.Error = strings.TrimPrefix(msg, "sakutei plan: ")
			report.Summarize()
			writePlanJSON(report)
		}
		os.Exit(1)
	}
	claimSelector, claimErr := parseClaimSelector(*claim)
	if claimErr != nil {
		fail("sakutei plan: invalid --claim %q: %v", *claim, claimErr)
	}
	soaSerial := checkZoneSOASerial("sakutei plan", *zoneSOASerial)
	zScope := checkScope("sakutei plan", "zone-scope", *zoneScope)
	rScope := checkScope("sakutei plan", "ruleset-scope", *rulesetScope)
	rulesetTarget.check("sakutei plan")

	if *intentDir == "" {
		fs.Usage()
		fail("sakutei plan: --intent <dir> is required")
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
		fail("sakutei plan: load intent: %v", err)
	}
	// #183 (§26.7): the budget flags are checked against the intent's
	// runtime: (a reconcile with the same flags would be refused, or run
	// tightened).
	runtimeTightened, rtErr := checkRuntime("sakutei plan", fs, in.Runtime, runtimeBudgetKeys)
	if rtErr != nil {
		fail("%v", rtErr)
	}
	for _, t := range runtimeTightened {
		fmt.Fprintf(logErr, "sakutei plan: %s\n", t)
	}
	budgetCfg, budgetCfgErr := budgetFl.config()
	if budgetCfgErr != nil {
		fail("sakutei plan: %v", budgetCfgErr)
	}
	var externalInputs []string
	recomputeRendered := false
	// #115: render phase 1 — inventory composition, as in the daemon.
	if input := composeOneShotInventory("sakutei plan", invFlags, &in, logErr); input != "" {
		externalInputs = append(externalInputs, input)
		recomputeRendered = true
	}
	intentResources, rev := in.Resources, in.Revision

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
			fmt.Fprintln(logErr,
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
			fmt.Fprintf(logErr,
				"sakutei plan: ExternalInputUnreachable: resolver returned error; using last cached value (§9.2 last-value-on-unreachable)\n")
		}
		if phase1Result.FirstCallFailed {
			// No prior value — halt-safe: do not converge (§9.2).
			fail("sakutei plan: ExternalInputUnreachable (first call): %v\n"+
				"  No prior cached value available; convergence not advanced (halt-safe, §9.2)",
				phase1Result.Err)
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
	if allocErr == nil {
		allocAuth, allocErr = wrapWriteBack(allocAuth, *intentDir, *allocWriteBack)
	}
	if allocErr != nil {
		fail("sakutei plan: %v", allocErr)
	}
	allocRes, allocErr := render.RenderAllocations(context.Background(), allocAuth, render.AllocLookupOnly, in, claimSelector)
	if allocErr != nil {
		fail("sakutei plan: %v", allocErr)
	}
	intentResources = allocRes.Resources
	if len(allocRes.Inputs) > 0 {
		externalInputs = append(externalInputs, allocRes.Inputs...)
		recomputeRendered = true
	}
	for _, pa := range allocRes.Pending {
		line := pendingLine("plan", pa)
		fmt.Fprintln(logErr, line)
		report.PendingAllocations = append(report.PendingAllocations, pendingAllocationJSON(pa, line))
	}
	if recomputeRendered {
		rootScope := core.NewScope(nil)
		rev.RenderedRevision = revision.ComputeRenderedRevision(
			rootScope, rev.IntentRevision, externalInputs)
	}
	report.IntentRevision, report.RenderedRevision = rev.IntentRevision, rev.RenderedRevision

	// S072a14-1: render phase 2 — expand composite resources into concrete
	// children (§9.5).  Composites themselves are NOT passed to Diff/Apply;
	// only their expanded children enter the convergence loop.
	expRegistry := render.DefaultRegistry()
	expanded, expandErr := render.ExpandAll(expRegistry, intentResources)
	if expandErr != nil {
		fail("sakutei plan: render phase 2: %v", expandErr)
	}

	// §9.6: validate the existing generated/ file for direct human edits.
	// A missing file is fine; a file without _generated:true triggers a warning.
	genFilePath := filepath.Join(genDir, render.GeneratedFileName)
	if valErr := render.ValidateGeneratedFile(genFilePath); valErr != nil {
		fmt.Fprintf(logErr, "sakutei plan: WARNING: %v\n", valErr)
	}

	// §9.6: write expanded children to generated/expanded.json.
	if writeErr := render.WriteGenerated(genDir, expanded); writeErr != nil {
		fail("sakutei plan: write generated/: %v", writeErr)
	}

	// Build the desired slice: only non-composite resources enter Diff/Apply.
	// Composite parents are excluded (children represent them in the DAG).
	// #111: with --claim, an out-of-claim resource is excluded like reconcile.
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
		fmt.Fprintf(logErr,
			"sakutei plan: --claim %s: %d resource(s) in claim, %d excluded (out of scope)\n",
			claimSelector.String(), len(desired), excludedByClaim)
		report.Claim = &planreport.Claim{Selector: claimSelector.String(), InClaim: len(desired), Excluded: excludedByClaim}
	}

	// Scb0ecd-1: print revision header so the caller can observe revision values.
	// In v1a rendered_revision = intent_revision (no external inputs yet; seam
	// for S072a14 where external inputs enter the rendered hash).
	fmt.Fprintf(logOut, "intent_revision=%s rendered_revision=%s\n", rev.IntentRevision, rev.RenderedRevision)
	// #111 (§14.2): with a remediation schedule in intent, say whether a
	// reconcile run now would fix the diffs below or only report them.
	if in.Remediation != nil {
		rem := remediationNow(in.Remediation, time.Now())
		fmt.Fprintln(logOut, rem.Line)
		report.Remediation = &rem
	}
	// §22.3: the Events of this run carry its revision.
	logOut = eventlog.Stamp(logOut, 0, rev)
	logErr = eventlog.Stamp(logErr, 0, rev)
	// §7.6 (#174): what the network allows expanded into, with their reasons
	// and tags, and the warnings of the check (its errors refused the load).
	report.NetworkPolicy = netPolicyReport(in.NetworkPolicy)
	printNetPolicy(logOut, report.NetworkPolicy)

	// S62dbba-2: resolve providers via registry based on intent resource declarations.
	// The --state flag is passed as the "state" config key for the "fake" provider;
	// --zone / --ruleset (when set) are passed as "zone" / "ruleset" for the
	// dnszone / nftables providers so a mixed intent can keep each provider's
	// backing file separate.
	reg := openProviderSet("sakutei plan", *providerConfigFile)
	providerConfig := map[string]string{"state": *statePath}
	if *zonePath != "" {
		providerConfig["zone"] = *zonePath
	}
	if *rulesetPath != "" {
		providerConfig["ruleset"] = *rulesetPath
	}
	providerConfig["soa_serial"] = soaSerial
	providerConfig["zone_scope"] = zScope
	providerConfig["ruleset_scope"] = rScope
	rulesetTarget.put(providerConfig)
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
	providers, resolveErr := resolveIntentProviders(reg, in.Providers, desired, providerConfig, "sakutei plan", logErr)
	if resolveErr == nil {
		resolveErr = checkExternalOwners(in.ExternalOwners, desired, providers, "sakutei plan", logErr)
	}
	if resolveErr == nil {
		resolveErr = checkCompatibility(in.Compatibility, desired, "sakutei plan")
	}
	if resolveErr != nil {
		fail("%v", resolveErr)
	}

	// Build per-provider diff engines (each provider has its own Schema).
	engines := make(map[string]*diff.Engine, len(providers))
	for name, p := range providers {
		engines[name] = diff.ForProvider(p)
	}

	// Scb0ecd-2: compute curProviderVersion for cause-state tracking.
	// With multiple providers, concatenate all "name:version" pairs in sorted
	// order so the combined version string is deterministic.
	// For v1a (single "fake" provider) this reduces to the fake's version.
	curProviderVersion := joinedProviderVersions(providers)

	// Scb0ecd-2: load cause state (previous-cycle revision + provider_version).
	prevState, csErr := loadCauseState(*causeStatePath)
	if csErr != nil {
		fmt.Fprintf(logErr, "sakutei plan: warn: load cause-state %q: %v (treating as first-run)\n",
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
	// §29.1 (#151): the observed fields, for the compatibility check of the
	// combinations met while updating one at a time.
	observedFields := make(map[string]map[string]string)

	// #89 (§6.3): the targets of a Workflow that is not done are driven by the
	// Workflow; the daemon's loop neither Observes nor Applies them, so plan
	// shows them as such instead of as a diff.
	wfHeld := workflowHeld(in.Workflows, *workflowStore)
	var heldShown []workflowHeldTarget

	// #94 (§6.2): a diff of a strategy: task resource is never Applied; the
	// daemon launches its Task instead, so plan says so next to the op.
	taskPolicies := map[string]core.UpdatePolicy{}
	for _, res := range desired {
		if res.UpdatePolicy.IsTask() {
			taskPolicies[res.ID.String()] = res.UpdatePolicy
		}
	}

	for _, res := range desired {
		if h, ok := wfHeld[res.ID.Local]; ok {
			h.ID = res.ID
			heldShown = append(heldShown, h)
			unobservable[res.ID.Local] = true
			continue
		}
		// S62dbba-2: route Read and Diff to the provider for this resource.
		fp, ok := providers[res.ID.Provider]
		if !ok {
			// Should not happen: resolveProviders already validated all names.
			fail("sakutei plan: internal error: no provider for %q", res.ID.Provider)
		}
		engine := engines[res.ID.Provider]

		// ADR-0002: Observe independently for each resource.  Each call to
		// Read is fresh; no observation is carried across resources or cycles.
		obs, err := provider.ReadFor(ctx, fp, res.ID.Local, res.Fields)
		if err != nil {
			fail("sakutei plan: observe %s: %v", res.ID, err)
		}
		if obs.Availability == core.AvailabilityOk {
			observedFields[res.ID.Local] = engine.SnapshotFields(obs.State)
			// §7.3 (#158): selected by an external_owners entry by an observed
			// value the intent did not declare: observed, never applied.
			if sel, ok := in.ExternalOwners.ByState(res.ID, obs.State); ok {
				unobservable[res.ID.Local] = true
				fmt.Fprintf(logErr, "sakutei plan: %s: %s: observed, not planned (§7.3)\n", res.ID, sel.Describe())
				continue
			}
		}
		if obs.Availability != core.AvailabilityOk {
			unobservable[res.ID.Local] = true
			// #135: say so, instead of leaving it out of the plan silently.
			fmt.Fprintf(logErr, "sakutei plan: %s: unobservable (availability=%s) — not planned (unknown ≠ absent, §3)\n", res.ID, obs.Availability)
			report.Unobservable = append(report.Unobservable, planreport.Unobservable{
				Target: planreport.NewTarget(res.ID), Availability: obs.Availability.String()})
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
			fail("sakutei plan: %v", err)
		}

		plan, planErr := engine.Plan(obs, res, resCauseCtx)
		if planErr != nil {
			fail("sakutei plan: %v", planErr)
		}
		// Scb0ecd-2: log ProviderUpgradeDiff event for cause-C ops (ghost diff).
		for _, op := range plan.Ops {
			if op.Cause == core.CauseTagC {
				reconcile.EmitProviderUpgradeDiff(logErr, "sakutei plan: ", op,
					prevState.PrevProviderVersion, curProviderVersion, "review before applying")
			}
		}
		allOps = append(allOps, plan.Ops...)
	}

	// §29.1 (#151): an update order that passes through a combination the
	// compatibility table does not allow stops the plan; a group that cannot
	// be verified (a member not observed) is held by reconcile and the daemon.
	findings, compatErr := compatFindings(in.Compatibility, desired, observedFields)
	if compatErr != nil {
		fail("sakutei plan: %v", compatErr)
	}
	var incompatible []string
	for _, f := range findings {
		if f.Unverified {
			fmt.Fprintf(logErr, "sakutei plan: %s\n", f.Message())
			continue
		}
		incompatible = append(incompatible, f.Message())
	}
	if len(incompatible) > 0 {
		fail("sakutei plan: %s (§29.1 compatibility); nothing was done", strings.Join(incompatible, "; "))
	}

	// Scb0ecd-2: persist current revision + provider_version as "previous" for
	// the next invocation.  Non-fatal on failure (S3 semantics).
	if saveErr := saveCauseState(*causeStatePath, causeStateFile{
		PrevIntentRevision:   rev.IntentRevision,
		PrevRenderedRevision: rev.RenderedRevision,
		PrevProviderVersion:  curProviderVersion,
		PrevParentRevisions:  curParentRevs,
	}); saveErr != nil {
		fmt.Fprintf(logErr, "sakutei plan: warn: save cause-state %q: %v\n", *causeStatePath, saveErr)
	}

	// Prune phase (dry-run): show what would be deleted (§12, ADR-0005).
	// plan does NOT call Apply; it only computes and displays the Delete ops.
	// S62dbba-2: prune is computed per provider so each provider's Lister is
	// used for the resources it manages.
	diffOpCount := len(allOps)
	pruneOps := planPruneOpsMulti(ctx, providers, claimSelector, desired, unobservable, syncScopeList(in), in.ExternalOwners, report)
	allOps = append(allOps, pruneOps...)

	// S072a14-3: compute composite status aggregation (§9.7) for reporting.
	// Build per-resource ConvergenceStatus from diff results and unobservable set.
	resourceStatus := make(map[string]core.ConvergenceStatus)
	for _, res := range desired {
		local := res.ID.Local
		if _, ok := wfHeld[local]; ok {
			// Driven by its Workflow: converging for the loop.
			resourceStatus[local] = core.StatusConverging
			continue
		}
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
	for _, parentID := range sortedCompositeParentIDs(compositeSummaries) {
		report.Composites = append(report.Composites, planreport.Composite{
			ID: parentID, Status: compositeSummaries[parentID].AggregatedStatus.String()})
	}
	for _, h := range heldShown {
		report.Workflow = append(report.Workflow, planreport.WorkflowTarget{
			Target: planreport.NewTarget(h.ID), Workflow: h.Workflow, Stage: h.Stage})
	}

	// #135: the gates a reconcile now would apply to these ops — the scopes
	// declared remediation: alert-only (§14.3), the change budget with its
	// planned_change lanes (§13) and the capacity gates (§13.4).  Like
	// reconcile, the budget sees only the ops a reconcile would Apply: not a
	// ghost diff (cause C), not the op of a strategy: task resource.
	for i, op := range allOps {
		o := planreport.NewOperation(op, i >= diffOpCount)
		if pol, ok := taskPolicies[op.Target.String()]; ok && op.Kind != core.OpKindDelete {
			o.Task = planreport.NewTask(pol)
		}
		report.Operations = append(report.Operations, o)
	}
	gatePlanOps(ctx, gatePlanInput{
		ops:          allOps,
		report:       report,
		alertScopes:  in.AlertOnlyScopes(),
		planned:      in.PlannedChanges,
		budgetCfg:    budgetCfg,
		auditLog:     *auditLog,
		capacity:     capacityGates,
		totalDesired: len(desired),
		providerFor: func(name string) provider.Provider {
			if p, ok := providers[name]; ok {
				return p
			}
			p, err := reg.Get(name, providerConfig)
			if err != nil {
				return nil
			}
			return p
		},
	}, fail)
	report.Summarize()

	// Print plan output.
	printWorkflowHeld(heldShown)
	if len(allOps) == 0 {
		// Even if no ops, print composite status if there are composites.
		if len(report.Composites) > 0 {
			fmt.Fprintln(logOut, "Composite status:")
			for _, c := range report.Composites {
				fmt.Fprintf(logOut, "  composite %s: %s\n", c.ID, c.Status)
			}
		}
		if len(heldShown) > 0 {
			fmt.Fprintf(logOut, "No changes by the convergence loop (%d Workflow target(s) left to their Workflow)\n", len(heldShown))
		} else {
			fmt.Fprintln(logOut, "Converged (no changes)")
		}
		finishPlan(report, *jsonOut)
	}

	printGateLines(report)
	fmt.Fprintf(logOut, "Plan: %d change(s)\n\n", len(allOps))
	for i, op := range allOps {
		printOpLines(op)
		o := report.Operations[i]
		if o.Task != nil {
			printTaskNote(taskPolicies[op.Target.String()])
		}
		printOpGate(o)
		fmt.Fprintln(logOut)
	}

	// S072a14-3: print composite status aggregation (§9.7).
	if len(report.Composites) > 0 {
		fmt.Fprintln(logOut, "Composite status (§9.7 worst-first):")
		for _, c := range report.Composites {
			fmt.Fprintf(logOut, "  composite %s: %s\n", c.ID, c.Status)
		}
	}
	finishPlan(report, *jsonOut)
}

// finishPlan ends a plan that completed (exit 0): with --json the report is
// printed on stdout.
func finishPlan(report *planreport.Report, jsonOut bool) {
	if jsonOut {
		writePlanJSON(report)
	}
	os.Exit(0)
}

// writePlanJSON prints the report as one JSON object on stdout (#135).
func writePlanJSON(report *planreport.Report) {
	data, err := report.JSON()
	if err != nil {
		fmt.Fprintf(os.Stderr, "sakutei plan: encode json: %v\n", err)
		os.Exit(1)
	}
	os.Stdout.Write(append(data, '\n'))
}

// printTaskNote follows the op of a strategy: task resource (§6.2, #94): the
// change is made by a Task the daemon launches (1-outstanding, inside the
// window), never by Apply — a one-shot reconcile leaves it alone.
func printTaskNote(pol core.UpdatePolicy) {
	window := "any time"
	if pol.Window != "" {
		window = pol.Window
	}
	if pol.IsManual() {
		// #150 (§6.2 人の手順で合わせる Task): nothing is written; a person is asked.
		fmt.Fprintf(logOut, "  task:   asks a person (update_policy.strategy: manual; window: %s) — %s; confirmed by observation, not Applied\n",
			window, task.ManualNotice(task.Request{Policy: pol}))
		return
	}
	rollback := ""
	if pol.RollbackOnFail {
		rollback = ", rollback_on_fail"
	}
	fmt.Fprintf(logOut, "  task:   launches a Task (update_policy.strategy: task; window: %s%s) — run by the daemon, not Applied\n", window, rollback)
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
// No Apply is called (dry-run).  #135: a provider whose prune a safety guard
// halted, and the candidates it deferred, are said on stderr and put in report.
func planPruneOpsMulti(
	ctx context.Context,
	providers map[string]provider.Provider,
	claim core.Scope,
	desired []diff.DesiredResource,
	unobservable map[string]bool,
	syncScopes []core.Scope,
	owners extowner.Set,
	report *planreport.Report,
) []core.Op {
	var allDeleteOps []core.Op

	// Group desired resources by provider.
	byProvider := make(map[string][]diff.DesiredResource)
	for _, r := range desired {
		byProvider[r.ID.Provider] = append(byProvider[r.ID.Provider], r)
	}

	// #135: in name order, so the plan's ops come out in the same order.
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fp := providers[name]
		lister, ok := fp.(provider.Lister)
		if !ok {
			continue
		}
		scope := core.NewScope(nil)
		engine := prune.New()
		engine.SetExcludedScopes(syncScopes) // #23: sync scopes are never pruned (§24.5)
		engine.SetExternalOwners(owners)     // §7.3 (#158): never pruned
		provDesired := byProvider[name]
		// FIX #6 / #111: the --claim selector scopes the prune preview to the
		// claim (empty = whole world), as in reconcile.
		result, err := engine.ComputePrune(ctx, lister, fp, scope, claim, provDesired, unobservable, logErr)
		if err != nil {
			result = prune.PruneResult{Halted: true, HaltReason: fmt.Sprintf("prune compute error: %v", err)}
		}
		for _, id := range result.Deferred {
			report.PruneDeferred = append(report.PruneDeferred, planreport.NewTarget(id))
		}
		if result.Halted {
			reconcile.EmitPruneHalted(logErr, result, name, "sakutei plan: prune halted: "+result.HaltReason)
			report.PruneHalts = append(report.PruneHalts, planreport.PruneHalt{
				Provider: name, Reason: result.HaltReason, CountDrop: result.CountDrop, HeldOrphans: result.HeldOrphans})
			continue
		}
		// #135: by ID, so two plans of one reality list the same ops in the same
		// order (a List need not be ordered).
		ops := append([]core.Op(nil), result.DeleteOps...)
		sort.SliceStable(ops, func(i, j int) bool { return ops[i].Target.String() < ops[j].Target.String() })
		allDeleteOps = append(allDeleteOps, ops...)
	}
	return allDeleteOps
}

// printWorkflowHeld prints the Workflow targets the loop leaves to their
// Workflow (#89): never as a diff, since the loop does not Apply them.
func printWorkflowHeld(held []workflowHeldTarget) {
	if len(held) == 0 {
		return
	}
	fmt.Fprintf(logOut, "Workflow に任せる（収束ループは触らない）: %d target(s)\n\n", len(held))
	for _, h := range held {
		fmt.Fprintf(logOut, "= WORKFLOW %s\n", h.ID)
		fmt.Fprintf(logOut, "  workflow: %s\n", h.Workflow)
		fmt.Fprintf(logOut, "  stage:    %s\n\n", h.Stage)
	}
}

// printOp formats a single Op for human-readable plan output.
func printOp(op core.Op) {
	printOpLines(op)
	fmt.Fprintln(logOut)
}

// printOpLines prints one op without the blank line that separates ops.
func printOpLines(op core.Op) {
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

	fmt.Fprintf(logOut, "%s %s %s%s\n", marker, kindStr, op.Target, destructiveNote)
	if op.Diagnostic != "" {
		fmt.Fprintf(logOut, "  reason: %s\n", op.Diagnostic)
	}
	if op.ApprovalReason != "" {
		// #179: a change the provider requires approval for (destructive).
		fmt.Fprintf(logOut, "  approval: required — %s (planned_change kind %s)\n", op.ApprovalReason, planreport.LaneKindFor(op.Kind))
	}
	if op.Cause != "" {
		fmt.Fprintf(logOut, "  cause:  %s\n", string(op.Cause))
	}
	for _, fd := range op.Fields {
		if fd.From == "" {
			fmt.Fprintf(logOut, "  + %-20s  (new) → %q\n", fd.Field, fd.To)
		} else if fd.To == "" {
			fmt.Fprintf(logOut, "  - %-20s  %q → (removed)\n", fd.Field, fd.From)
		} else {
			fmt.Fprintf(logOut, "  ~ %-20s  %q → %q\n", fd.Field, fd.From, fd.To)
		}
	}
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

// remediationNow is remediationNowLine with the mode it is about (#135).
func remediationNow(s *remediation.Schedule, now time.Time) planreport.Remediation {
	m, why := s.ModeAt(now)
	alertOnly := m == remediation.ModeAlertOnly
	mode := "auto"
	if alertOnly {
		mode = "alert-only"
	}
	return planreport.Remediation{Mode: mode, Reason: why, OutsideWindow: alertOnly, Line: remediationNowLine(s, now)}
}

// remediationNowLine is plan's account of the intent's remediation schedule at
// now (#111): whether the next reconcile / daemon cycle would remediate.  plan
// takes no --mode or --freeze, so only the schedule is evaluated; either flag
// on the reconcile run makes it alert-only as well (most-restrictive-wins,
// §14.3).
func remediationNowLine(s *remediation.Schedule, now time.Time) string {
	m, why := s.ModeAt(now)
	if m == remediation.ModeAlertOnly {
		return fmt.Sprintf("Remediation mode now: alert-only (%s) — outside the remediation window: a reconcile now reports these changes but does not apply them (§14.2)", why)
	}
	return fmt.Sprintf("Remediation mode now: auto (%s) — a reconcile now applies these changes (unless run with --mode alert-only or --freeze, §14.3)", why)
}
