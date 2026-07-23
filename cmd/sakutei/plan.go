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
	"github.com/tjst-t/sakutei/internal/fakeprovider"
	"github.com/tjst-t/sakutei/internal/loader"
	"github.com/tjst-t/sakutei/internal/prune"
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

	if err := fs.Parse(args); err != nil {
		// flag package already printed the error.
		os.Exit(1)
	}

	if *intentDir == "" {
		fmt.Fprintln(os.Stderr, "sakutei plan: --intent <dir> is required")
		fs.Usage()
		os.Exit(1)
	}

	// Load desired resources from intent directory.
	// Scb0ecd-1: LoadDir now also returns RevisionInfo.
	desired, rev, err := loader.LoadDir(*intentDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sakutei plan: load intent: %v\n", err)
		os.Exit(1)
	}

	// Scb0ecd-1: print revision header so the caller can observe revision values.
	// In v1a rendered_revision = intent_revision (no external inputs yet; seam
	// for S072a14 where external inputs enter the rendered hash).
	fmt.Printf("intent_revision=%s rendered_revision=%s\n", rev.IntentRevision, rev.RenderedRevision)

	// Create the fake provider backed by the state file.
	fp := fakeprovider.New(*statePath)

	// Scb0ecd-2: load cause state (previous-cycle revision + provider_version).
	prevState, csErr := loadCauseState(*causeStatePath)
	if csErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei plan: warn: load cause-state %q: %v (treating as first-run)\n",
			*causeStatePath, csErr)
		prevState = causeStateFile{}
	}
	curProviderVersion := fp.ProviderVersion()

	// Build the per-invocation CauseContext.  All resources in this invocation
	// share the same revision signals (intent_revision, rendered_revision,
	// provider_version); per-resource differentiation (§10.3) is structural:
	// the same intent_revision change affects ALL resources in this intent
	// directory (which is the correct scope unit, ADR-0008).
	//
	// In v1a UpstreamChanged and RenderedRevisionChanged are always false
	// (seams for S072a14).
	ctx_cause := cause.CauseContext{
		IntentPrevRevision:   prevState.PrevIntentRevision,
		IntentCurRevision:    rev.IntentRevision,
		RenderedPrevRevision: prevState.PrevRenderedRevision,
		RenderedCurRevision:  rev.RenderedRevision,
		ProviderPrevVersion:  prevState.PrevProviderVersion,
		ProviderCurVersion:   curProviderVersion,
		// UpstreamChanged:          false (seam, v1a)
		// RenderedRevisionChanged:  false (seam, v1a; rendered == intent)
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

		plan := engine.Diff(obs, res, ctx_cause)
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

	// Print plan output.
	if len(allOps) == 0 {
		fmt.Println("Converged (no changes)")
		os.Exit(0)
	}

	fmt.Printf("Plan: %d change(s)\n\n", len(allOps))
	for _, op := range allOps {
		printOp(op)
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
