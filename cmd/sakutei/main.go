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
	"fmt"
	"os"
)

// version is the build-time version string.  It is expected to be injected
// via -ldflags "-X main.version=<tag>" during release builds.
var version = "dev"

func main() {
	if len(os.Args) < 2 {
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
// continuously.  Implementation is deferred to subsequent Stories.
func runDaemon() {
	fmt.Fprintln(os.Stderr, "sakutei: daemon mode — not yet implemented")
	os.Exit(1)
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
