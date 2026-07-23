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
	"github.com/tjst-t/sakutei/internal/fakeprovider"
	"github.com/tjst-t/sakutei/internal/loader"
	"github.com/tjst-t/sakutei/pkg/core"
)

// runPlan implements "sakutei plan".
//
// It loads desired resources from --intent <dir>, observes actual state via
// the fake provider backed by --state <file>, computes the diff, and prints
// the result.
//
// ADR-0002 contract: Apply is NEVER called.  This function stops at Diff.
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
	desired, err := loader.LoadDir(*intentDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sakutei plan: load intent: %v\n", err)
		os.Exit(1)
	}

	// Create the fake provider backed by the state file.
	fp := fakeprovider.New(*statePath)
	engine := diff.New(fp.Schema())

	ctx := context.Background()
	var allOps []core.Op

	for _, res := range desired {
		// ADR-0002: Observe independently for each resource.  Each call to
		// Read is fresh; no observation is carried across resources or cycles.
		obs, err := fp.Read(ctx, res.ID.Local)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sakutei plan: observe %s: %v\n", res.ID, err)
			os.Exit(1)
		}

		plan := engine.Diff(obs, res)
		allOps = append(allOps, plan.Ops...)
	}

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
