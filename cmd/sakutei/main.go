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
	"time"

	"github.com/tjst-t/sakutei/internal/reconcile"
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
		notYetImplemented("status")
	case "import":
		notYetImplemented("import")
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
func runDaemon() {
	fs := flag.NewFlagSet("sakutei", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	intentDir := fs.String("intent", "", "directory containing intent JSON files (required)")
	statePath := fs.String("state", "sakutei-fake-state.json",
		"path to the fake provider state file (default: sakutei-fake-state.json)")
	intervalStr := fs.String("interval", "",
		"convergence cycle interval, e.g. 200ms or 30s (default: 10s)")

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

	cfg := reconcile.Config{
		IntentDir: *intentDir,
		StatePath: *statePath,
		Interval:  interval,
	}

	if err := reconcile.Run(context.Background(), cfg); err != nil {
		fmt.Fprintf(os.Stderr, "sakutei: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// notYetImplemented prints a placeholder message and exits with a non-zero
// code.  Subcommand stubs use this until their Stories are implemented.
func notYetImplemented(sub string) {
	fmt.Fprintf(os.Stderr, "sakutei %s: not yet implemented\n", sub)
	os.Exit(1)
}

func printUsage() {
	fmt.Fprintf(os.Stdout, `sakutei — declarative infrastructure convergence engine

Usage:
  sakutei              run continuously, converging on every cycle
  sakutei plan         dry-run: show what would change
  sakutei reconcile    run one convergence cycle manually
  sakutei status       report drifted resources
  sakutei import       export existing reality as intent YAML
  sakutei version      print version information
  sakutei help         show this help message
`)
}
