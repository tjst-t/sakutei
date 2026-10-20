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

// Command sakutei is the single binary for the Sakutei declarative
// infrastructure convergence engine.
//
// Usage:
//
//	sakutei              # run continuously, converging on every cycle
//	sakutei plan         # dry-run: show what would change (stops at Diff)
//	sakutei reconcile    # run one convergence cycle manually
//	sakutei status       # report drifted resources
//	sakutei import       # export existing reality as intent YAML
//	sakutei version      # print version information
//
// Subcommand implementations beyond the skeleton are added in subsequent
// Stories.  See CLAUDE.md for the full CLI specification.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/tjst-t/sakutei/internal/budget"
	"github.com/tjst-t/sakutei/internal/reconcile"
	"github.com/tjst-t/sakutei/internal/remediation"
	"github.com/tjst-t/sakutei/internal/render"
	"github.com/tjst-t/sakutei/pkg/registry"
	"github.com/tjst-t/sakutei/pkg/sdk"
)

// version is the build-time version string.  It is expected to be injected
// via -ldflags "-X main.version=<tag>" during release builds.
var version = "dev"

func main() {
	// Route to the daemon loop when:
	//   (a) no arguments at all, or
	//   (b) the first argument starts with '-' (a flag, not a subcommand).
	// This lets "sakutei --intent dir --interval 200ms" invoke the daemon.
	if len(os.Args) < 2 || (len(os.Args[1]) > 0 && os.Args[1][0] == '-') {
		runDaemon()
		return
	}

	switch os.Args[1] {
	case "plan":
		runPlan(os.Args[2:])
	case "reconcile":
		runReconcile(os.Args[2:])
	case "status":
		runStatus(os.Args[2:])
	case "import":
		runImport(os.Args[2:])
	case "sync":
		runSync(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Fprintf(os.Stdout, "sakutei %s\n", version)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "sakutei: unknown subcommand %q\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

// runDaemon is the "sakutei" entry point: run the convergence loop
// continuously (Story S94f1f9-1).
//
// Flags:
//
//	--intent <dir>       directory containing intent JSON files (required)
//	--state  <file>      path to the fake provider state file (default: sakutei-fake-state.json)
//	--interval <dur>     convergence cycle period (default: 10s; use short values in tests)
//
// The loop runs until SIGTERM or SIGINT is received, at which point it stops
// at the next cycle boundary and exits 0.
//
// S62dbba-2: the provider is now resolved via ProviderRegistry instead of being
// directly imported from internal/fakeprovider.  The daemon still uses a single
// provider (the "fake" provider) for the initial implementation; multi-provider
// daemon support is a future Story once the DNS provider (S62dbba-3) is ready.
func runDaemon() {
	fs := flag.NewFlagSet("sakutei", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	intentDir := fs.String("intent", "", "directory containing intent JSON files (required)")
	statePath := fs.String("state", "sakutei-fake-state.json",
		"path to the fake provider state file (default: sakutei-fake-state.json)")
	intervalStr := fs.String("interval", "",
		"convergence cycle interval, e.g. 200ms or 30s (default: 10s)")
	// S072a14-3: render pipeline flags for the daemon.
	generatedDir := fs.String("generated", "",
		"directory to write render phase 2 output (default: sibling of --intent named 'generated')")
	inventoryHash := fs.String("inventory-hash", "",
		"S072a14-3: inject deterministic inventory hash into rendered_revision (fake phase 1; empty=no external input)")
	allocStore := fs.String("alloc-store", "",
		"#24: path to the allocation store file (file AllocationAuthority, §19.9); required when the intent uses from_pool")
	inventoryUnreachable := fs.Bool("inventory-unreachable", false,
		"S072a14-3: simulate an unreachable ExternalInputResolver (triggers last-value-on-unreachable path §9.2)")
	inventoryFile := fs.String("inventory-file", "",
		"#31: inventory file (intent-format resources composed by render phase 1, §9.1); a sudden drop halts the cycle (incomplete≠empty, §9.2)")
	inventoryMinRatio := fs.Float64("inventory-min-ratio", render.DefaultInventoryMinRatio,
		"#31: an inventory at or below this fraction of the previous item count is incomplete (§9.2)")
	inventoryExpectedMin := fs.Int("inventory-expected-min", 0,
		"#31: expected inventory cardinality; fewer items is incomplete even on the first cycle (§9.2); 0=off")

	// Scf7762-2: change-budget flags (§13).
	maxDestructivePerCycle := fs.Int("max-destructive-per-cycle", 0,
		"maximum destructive ops per cycle; 0=unlimited (§13.1)")
	maxChangesPerCycleAbsolute := fs.Int("max-changes-per-cycle", 0,
		"maximum total ops per cycle; 0=unlimited (§13.1)")
	maxDestructivePerWindow := fs.Int("max-destructive-per-window", 0,
		"maximum destructive ops in rolling time window; 0=unlimited (§13.2)")
	windowDurationStr := fs.String("window-duration", "",
		"rolling window for cumulative budget, e.g. 24h, 10m (default: 24h) (§13.2)")
	onExceedStr := fs.String("on-exceed", "halt-all",
		"budget exceeded action: halt-all | halt-destructive-only (§13.1)")
	approveLargeChange := fs.Int("approve-large-change", 0,
		"explicitly approve up to N destructive changes this invocation")

	// Scf7762-3: remediation mode flags (§14.1).
	modeStr := fs.String("mode", "auto",
		"remediation mode: auto | alert-only (§14.1); alert-only detects drift but does not Apply")
	freeze := fs.Bool("freeze", false,
		"global freeze: override to alert-only regardless of --mode (§14.3 escape hatch)")

	if err := fs.Parse(os.Args[1:]); err != nil {
		// "-h"/"--help" makes flag.Parse return flag.ErrHelp after printing
		// usage; that is a successful, user-requested help display, not an
		// error, so exit 0.
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(1)
	}

	if *intentDir == "" {
		fmt.Fprintln(os.Stderr, "sakutei: --intent <dir> is required")
		fs.Usage()
		os.Exit(1)
	}

	interval := reconcile.DefaultInterval
	if *intervalStr != "" {
		d, err := time.ParseDuration(*intervalStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sakutei: invalid --interval %q: %v\n", *intervalStr, err)
			os.Exit(1)
		}
		interval = d
	}

	// Parse budget configuration (Scf7762-2).
	budgetCfg := budget.Config{
		MaxDestructivePerCycle:     *maxDestructivePerCycle,
		MaxChangesPerCycleAbsolute: *maxChangesPerCycleAbsolute,
		MaxDestructivePerWindow:    *maxDestructivePerWindow,
		ApproveLargeChange:         *approveLargeChange,
	}
	if *windowDurationStr != "" {
		d, err := time.ParseDuration(*windowDurationStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sakutei: invalid --window-duration %q: %v\n", *windowDurationStr, err)
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
		fmt.Fprintf(os.Stderr, "sakutei: unknown --on-exceed value %q; use halt-all or halt-destructive-only\n", *onExceedStr)
		os.Exit(1)
	}

	// Parse remediation configuration (Scf7762-3, §14.1).
	mode, modeErr := remediation.ParseMode(*modeStr)
	if modeErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei: invalid --mode %q: %v\n", *modeStr, modeErr)
		os.Exit(1)
	}
	remCfg := remediation.Config{
		Mode:   mode,
		Freeze: *freeze,
	}

	// S62dbba-2: resolve the "fake" provider via the ProviderRegistry.
	// The daemon currently supports a single provider (the state file path is
	// provided via --state).  Multi-provider daemon support is a future Story.
	reg := registry.DefaultRegistry()
	fp, err := reg.Get("fake", map[string]string{"state": *statePath})
	if err != nil {
		fmt.Fprintf(os.Stderr, "sakutei: provider resolution failed: %v\n", err)
		os.Exit(1)
	}

	// Scb0ecd-3: fail-closed provider_version validation (§10.4, ADR-0008).
	// A provider without a version cannot support ghost-diff (cause C) detection;
	// starting the loop would silently misclassify ghost diffs as drift (B) and
	// auto-apply them — the dangerous outcome this gate exists to prevent.
	if err := sdk.ValidateProvider(fp); err != nil {
		fmt.Fprintf(os.Stderr, "sakutei: provider validation failed: %v\n", err)
		os.Exit(1)
	}

	// S072a14-3: resolve generated/ directory.
	genDir := *generatedDir
	if genDir == "" {
		genDir = filepath.Join(filepath.Dir(*intentDir), "generated")
	}

	// S072a14-3: build render phase 1 resolver if inventory hash is configured.
	// Uses a reconcile.DefaultTTL cache (30s) for the daemon — callers that need
	// a different TTL can construct a ResolverCache directly and set it on Config.
	var resolverCache *render.ResolverCache
	if *inventoryHash != "" || *inventoryUnreachable {
		fakeResolver := &render.FakeInventoryResolver{
			Hash: *inventoryHash,
			Fail: *inventoryUnreachable,
		}
		resolverCache = render.NewResolverCache(fakeResolver, render.DefaultTTL)
	}

	// #31: render phase 1 inventory composition with the incomplete≠empty guard.
	var inventory *render.InventoryGuard
	if *inventoryFile != "" {
		if *inventoryMinRatio <= 0 || *inventoryMinRatio >= 1 {
			fmt.Fprintf(os.Stderr, "sakutei: invalid --inventory-min-ratio %v: must be in (0, 1)\n", *inventoryMinRatio)
			os.Exit(1)
		}
		inventory = render.NewInventoryGuard(&render.FileInventorySource{Path: *inventoryFile},
			render.WithInventoryMinRatio(*inventoryMinRatio),
			render.WithInventoryExpectedMin(*inventoryExpectedMin))
	}

	cfg := reconcile.Config{
		IntentDir:             *intentDir,
		StatePath:             *statePath,
		Provider:              fp,
		Interval:              interval,
		BudgetConfig:          budgetCfg,
		Remediation:           remCfg,
		GeneratedDir:          genDir,
		ExternalInputResolver: resolverCache,
		Allocation:            openAllocStore(*allocStore),
		Inventory:             inventory,
	}

	if err := reconcile.Run(context.Background(), cfg); err != nil {
		fmt.Fprintf(os.Stderr, "sakutei: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func printUsage() {
	fmt.Fprintf(os.Stdout, `sakutei — declarative infrastructure convergence engine

Usage:
  sakutei              run continuously, converging on every cycle
  sakutei plan         dry-run: show what would change
  sakutei reconcile    run one convergence cycle manually
  sakutei status       report drifted resources
  sakutei import       export existing reality as intent YAML
  sakutei sync         record the reality of mode: sync scopes into generated/
  sakutei version      print version information
  sakutei help         show this help message
`)
}
