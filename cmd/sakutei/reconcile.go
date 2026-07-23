// Copyright 2026 The Sakutei Authors
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
	"github.com/tjst-t/sakutei/internal/reconcile"
	"github.com/tjst-t/sakutei/pkg/core"
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
// Exit codes:
//
//	0 — reconcile completed (with or without changes applied)
//	1 — error (flag parse, intent load, observe error, apply error, etc.)
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
	for _, res := range desired {
		// ADR-0002: Observe independently for each resource.
		//
		// This is a fresh observation: reconcile does NOT carry over any
		// observation result from a prior "sakutei plan" invocation or from
		// a previous iteration of this loop.  Every call to fp.Read reaches
		// the state file directly, re-reading reality at this moment in time.
		obs, err := fp.Read(ctx, res.ID.Local)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sakutei reconcile: observe %s: %v\n", res.ID, err)
			os.Exit(1)
		}
		plans[res.ID.Local] = engine.Diff(obs, res)
	}

	// Phase 2: Apply in dependency (wave) order via Kahn decomposition.
	// reconcile.ApplyWaves ensures that:
	//   - Resources with depends_on are applied after their dependencies.
	//   - If a dependency fails, its transitive dependents are skipped.
	//   - appliedIDs contains the Local IDs of resources that were successfully applied.
	_, appliedIDs, skipped, waveErr := reconcile.ApplyWaves(ctx, fp, exec, desired, plans, os.Stderr)
	if waveErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: wave apply: %v\n", waveErr)
		os.Exit(1)
	}

	// Report skipped resources (upstream failures) to stderr.
	for _, sk := range skipped {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: skip %s (upstream failure; will retry next cycle)\n", sk)
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

	if appliedOps == 0 {
		fmt.Println("Converged (no changes)")
	} else {
		fmt.Printf("Reconcile: %d change(s) applied\n", appliedOps)
	}
	os.Exit(0)
}
