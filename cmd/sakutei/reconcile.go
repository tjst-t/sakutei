// Copyright 2026 Takumi Tsujishita
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
)

// runReconcile implements "sakutei reconcile".
//
// It performs a single convergence pass: Observe → Diff → Apply.
//
// ADR-0002 contract (観測 state レス): reconcile MUST observe reality itself.
// It MUST NOT reuse any observations produced by a prior "sakutei plan" run or
// any other invocation.  Each call to fp.Read below is a fresh, independent
// observation.
//
// Convergence scope (Story Sead944-3):
//   - Single pass (one Observe→Diff→Apply per resource)
//   - No daemon loop, no DAG/wave scheduling, no safety mechanisms
//   - Those are deferred to subsequent Stories
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
	desired, err := loader.LoadDir(*intentDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sakutei reconcile: load intent: %v\n", err)
		os.Exit(1)
	}

	// Create the fake provider backed by the state file.
	fp := fakeprovider.New(*statePath)
	engine := diff.New(fp.Schema())
	exec := executor.New()

	ctx := context.Background()
	appliedCount := 0

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

		plan := engine.Diff(obs, res)

		// Skip resources that are already converged (no Ops needed).
		// This is the idempotency path: if reality already matches desired,
		// Apply is not called at all for this resource.
		if len(plan.Ops) == 0 {
			continue
		}

		// Apply the plan through the Executor seam.
		if err := exec.Execute(ctx, fp, plan); err != nil {
			fmt.Fprintf(os.Stderr, "sakutei reconcile: apply %s: %v\n", res.ID, err)
			os.Exit(1)
		}

		// Report each applied Op.
		for _, op := range plan.Ops {
			printOp(op) // shared with plan.go (same package)
			appliedCount++
		}
	}

	if appliedCount == 0 {
		fmt.Println("収束済み（差分なし）")
	} else {
		fmt.Printf("Reconcile: %d change(s) applied\n", appliedCount)
	}
	os.Exit(0)
}
