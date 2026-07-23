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

	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/executor"
	"github.com/tjst-t/sakutei/internal/fakeprovider"
	"github.com/tjst-t/sakutei/internal/loader"
	"github.com/tjst-t/sakutei/internal/prune"
	"github.com/tjst-t/sakutei/internal/reconcile"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
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
// D-1 (unobservable-upstream safety, ADR-0002 / §3): a resource whose Observe
// returns an error OR Availability != Ok is treated as failed/unknown, NOT
// converged.  It and its transitive downstream are skipped this cycle
// (unknown ≠ absent; halt-default).  Policy, unified with the loop path:
// the one-shot reconcile still processes every observable resource, but if any
// resource was skipped it exits non-zero to signal an incomplete convergence.
// (The loop path instead retries on the next cycle — same skip semantics,
// different follow-up because a daemon has a "next cycle" and a one-shot does not.)
//
// Exit codes:
//
//	0 — reconcile completed, nothing skipped (with or without changes applied)
//	1 — error (flag parse, intent load, apply error) OR one or more resources
//	    were skipped this cycle (unobservable upstream / upstream Apply failure)
func runReconcile(args []string) {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	intentDir := fs.String("intent", "", "directory containing intent JSON files (required)")
	statePath := fs.String("state", "sakutei-fake-state.json",
		"path to the fake provider state file (default: sakutei-fake-state.json)")

	if err := fs.Parse(args); err != nil {
		// flag package already printed the error.
		os.Exit(1)
	}

	if *intentDir == "" {
		fmt.Fprintln(os.Stderr, "sakutei reconcile: --intent <dir> is required")
		fs.Usage()
		os.Exit(1)
	}

	// Load desired resources from intent directory.
	// loader.LoadDir also performs cycle detection (S94f1f9-2): cyclic
	// depends_on is rejected here with a non-zero exit before any Apply.
	desired, err := loader.LoadDir(*intentDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: load intent: %v\n", err)
		os.Exit(1)
	}

	// Create the fake provider backed by the state file.
	fp := fakeprovider.New(*statePath)
	engine := diff.New(fp.Schema())
	// Depend on the Executor seam (interface), not the concrete LocalExecutor,
	// so later Stories can swap in other implementations without touching this
	// call site (§6 seam principle).
	var exec executor.Executor = executor.New()

	ctx := context.Background()

	// Phase 1: Observe + Diff (fresh per resource — ADR-0002).
	// Collect plans for all resources before applying any of them.
	plans := make(map[string]core.Plan, len(desired))
	// unobservable holds Local IDs whose Observe failed or returned Availability
	// != Ok (D-1).  They are NOT treated as converged: ApplyWaves marks them
	// failed and skips their transitive downstream this cycle.
	unobservable := make(map[string]bool)
	for _, res := range desired {
		// ADR-0002: Observe independently for each resource.
		//
		// This is a fresh observation: reconcile does NOT carry over any
		// observation result from a prior "sakutei plan" invocation or from
		// a previous iteration of this loop.  Every call to fp.Read reaches
		// the state file directly, re-reading reality at this moment in time.
		obs, err := fp.Read(ctx, res.ID.Local)
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
		plans[res.ID.Local] = engine.Diff(obs, res)
	}

	// Phase 2: Apply in dependency (wave) order via Kahn decomposition.
	// reconcile.ApplyWaves ensures that:
	//   - Resources with depends_on are applied after their dependencies.
	//   - If a dependency fails OR is unobservable (D-1), its transitive
	//     dependents are skipped.
	//   - appliedIDs contains the Local IDs of resources that were successfully applied.
	_, appliedIDs, skipped, waveErr := reconcile.ApplyWaves(ctx, fp, exec, desired, plans, unobservable, os.Stderr)
	if waveErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: wave apply: %v\n", waveErr)
		os.Exit(1)
	}

	// Report skipped resources (upstream failure or unobservable) to stderr.
	for _, sk := range skipped {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: skip %s (unobservable or upstream failure; not converged this cycle)\n", sk)
	}

	// Report applied ops: only for resources whose Apply succeeded.
	appliedOps := 0
	for _, res := range desired {
		if !appliedIDs[res.ID.Local] {
			continue
		}
		plan := plans[res.ID.Local]
		for _, op := range plan.Ops {
			printOp(op)
			appliedOps++
		}
	}

	// Phase 3: Prune (§12, ADR-0005, Scf7762-1).
	//
	// For any scope that declares PruneWithinScope, compute the set-difference
	// (reality − desired) and apply approved Delete ops.
	//
	// Safety guards in the prune engine:
	//   - DeletionPolicy gate: only PruneWithinScope resources participate.
	//   - Pruneable gate: pruneable:false (default) suppresses deletion.
	//   - ListStatus gate: only ListComplete results allow prune.
	//   - Drop-rate gate: >50% list-count drop halts prune for this cycle.
	//   - unknown ≠ absent: resources with non-Ok Observe are skipped.
	pruneOps, pruneHalted := runPrune(ctx, fp, desired, unobservable, exec)
	for _, op := range pruneOps {
		printOp(op)
		appliedOps++
	}
	if pruneHalted {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: prune halted this cycle (safety guard triggered); see above for reason\n")
	}

	switch {
	case appliedOps > 0:
		fmt.Printf("Reconcile: %d change(s) applied\n", appliedOps)
	case len(skipped) > 0:
		// Do not claim convergence: some resources were skipped this cycle
		// (unobservable upstream or upstream Apply failure).
		fmt.Printf("Reconcile: no changes applied; %d resource(s) skipped\n", len(skipped))
	default:
		fmt.Println("Converged (no changes)")
	}

	// D-1 exit policy: an *unobservable* upstream is a safety failure — the
	// one-shot reconcile could not establish that reality is what we think, so
	// it exits non-zero to signal an incomplete/unsafe convergence.  Skips that
	// stem only from an upstream *Apply* failure remain exit 0 (level-triggered:
	// the resource was observable, the failure is transient and self-healing on
	// a subsequent reconcile) — this preserves AC-S94f1f9-2-2's exit-0 contract.
	if len(unobservable) > 0 {
		fmt.Fprintf(os.Stderr,
			"sakutei reconcile: %d resource(s) were unobservable this cycle; convergence incomplete\n",
			len(unobservable))
		os.Exit(1)
	}
	os.Exit(0)
}

// runPrune executes the prune phase for the given desired resources and
// returns the list of Delete ops that were applied and whether prune was halted.
//
// Only providers that implement provider.Lister can be pruned.  If the provider
// does not implement Lister, runPrune returns immediately with no ops.
func runPrune(
	ctx context.Context,
	fp provider.Provider,
	desired []diff.DesiredResource,
	unobservable map[string]bool,
	exec executor.Executor,
) (appliedOps []core.Op, halted bool) {
	lister, ok := fp.(provider.Lister)
	if !ok {
		return nil, false
	}

	// Validate PruneWithinScope declarations against Lister capability.
	if err := prune.ValidatePrunePolicy(desired, fp); err != nil {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: prune policy error: %v\n", err)
		return nil, false
	}

	// Use empty scope for the fake provider (Sprint 1 simplification).
	scope := core.NewScope(nil)

	engine := prune.New()
	result, err := engine.ComputePrune(ctx, lister, fp, scope, desired, unobservable, os.Stderr)
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

	// Apply each approved Delete op individually.
	for _, op := range result.DeleteOps {
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
