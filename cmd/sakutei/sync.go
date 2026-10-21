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
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/internal/loader"
	"github.com/tjst-t/sakutei/internal/syncmode"
	"github.com/tjst-t/sakutei/pkg/provider"
	"github.com/tjst-t/sakutei/pkg/registry"
	"github.com/tjst-t/sakutei/pkg/sdk"
)

// runSync implements `sakutei sync` (Sync Mode, §24.5): record the reality of
// every `mode: sync` scope into its generated/ target.  It never Applies.
// With --interval it keeps doing so (a loop separate from convergence);
// without it, one pass.
func runSync(args []string) {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	intentDir := fs.String("intent", "", "directory containing intent files (required); sync scopes are read from its scopes:")
	providersFlag := fs.String("provider", "", "provider(s) to record, comma-separated (required)")
	statePath := fs.String("state", "sakutei-fake-state.json", "path to the fake provider state file")
	zonePath := fs.String("zone", "", "path to the dnszone provider zone file (default: falls back to --state)")
	zoneSOASerial := zoneSOASerialFlag(fs)
	rulesetPath := fs.String("ruleset", "", "path to the nftables provider ruleset file (default: falls back to --state)")
	intervalStr := fs.String("interval", "", "repeat every interval, e.g. 30s (default: one pass)")
	samples := fs.Int("samples", 3, "how many times to observe reality per pass; a field that changes between observations is a managed candidate and its changes alone are not committed")
	sampleInterval := fs.Duration("sample-interval", 2*time.Second, "wait between observations (with --samples > 1)")
	committerName := fs.String("committer-name", syncmode.DefaultCommitter.Name, "name on Sync's git commits (the system, §26.3)")
	committerEmail := fs.String("committer-email", syncmode.DefaultCommitter.Email, "email on Sync's git commits")

	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	soaSerial := checkZoneSOASerial("sakutei sync", *zoneSOASerial)
	if *intentDir == "" || *providersFlag == "" {
		fmt.Fprintln(os.Stderr, "sakutei sync: --intent and --provider are required")
		fs.Usage()
		os.Exit(1)
	}
	var interval time.Duration
	if *intervalStr != "" {
		d, err := time.ParseDuration(*intervalStr)
		if err != nil || d <= 0 {
			fmt.Fprintf(os.Stderr, "sakutei sync: invalid --interval %q\n", *intervalStr)
			os.Exit(1)
		}
		interval = d
	}
	if *samples < 1 {
		fmt.Fprintln(os.Stderr, "sakutei sync: --samples must be >= 1")
		os.Exit(1)
	}
	if strings.TrimSpace(*committerName) == "" || strings.TrimSpace(*committerEmail) == "" {
		fmt.Fprintln(os.Stderr, "sakutei sync: --committer-name and --committer-email must not be empty")
		os.Exit(1)
	}

	reg := registry.DefaultRegistry()
	cfg := map[string]string{"state": *statePath}
	if *zonePath != "" {
		cfg["zone"] = *zonePath
	}
	cfg["soa_serial"] = soaSerial
	if *rulesetPath != "" {
		cfg["ruleset"] = *rulesetPath
	}
	providers := map[string]provider.Provider{}
	for _, name := range strings.Split(*providersFlag, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		p, err := reg.Get(name, cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sakutei sync: %v\n", err)
			os.Exit(1)
		}
		if err := sdk.ValidateProvider(p); err != nil {
			fmt.Fprintf(os.Stderr, "sakutei sync: provider validation failed (%q): %v\n", name, err)
			os.Exit(1)
		}
		providers[name] = p
	}

	root := filepath.Dir(filepath.Clean(*intentDir))
	base := syncmode.Options{
		Root: root, Providers: providers, Samples: *samples, Interval: *sampleInterval,
		Committer: syncmode.Committer{Name: *committerName, Email: *committerEmail},
	}
	for {
		failed := syncPass(*intentDir, base)
		if interval == 0 {
			if failed {
				os.Exit(1)
			}
			return
		}
		time.Sleep(interval)
	}
}

// syncPass loads the intent fresh (scopes may change between passes), records
// every sync scope and commits what changed.  It returns true if anything
// failed.
func syncPass(intentDir string, opts syncmode.Options) bool {
	in, err := loader.LoadIntent(intentDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sakutei sync: load intent: %v\n", err)
		return true
	}
	if len(in.SyncScopes()) == 0 {
		fmt.Fprintln(os.Stdout, "sync: no scope is declared mode: sync; nothing to record")
		return false
	}
	opts.Intent = in
	rep := syncmode.RunOnce(context.Background(), opts)
	failed := false
	for _, o := range rep.Outcomes {
		switch {
		case o.Err != nil:
			failed = true
			fmt.Fprintf(os.Stderr, "sync: scope %s provider %s: skipped, previous snapshot kept: %v\n", o.Scope, o.Provider, o.Err)
		case o.Changed:
			fmt.Fprintf(os.Stdout, "sync: scope %s provider %s: %d resource(s) → %s (updated)\n", o.Scope, o.Provider, o.Resources, o.Path)
		default:
			fmt.Fprintf(os.Stdout, "sync: scope %s provider %s: %d resource(s), unchanged\n", o.Scope, o.Provider, o.Resources)
		}
	}
	switch {
	case rep.Repo == "":
		fmt.Fprintln(os.Stdout, "sync: not in a git work tree; snapshots written, not committed")
	case rep.CommitErr != nil:
		failed = true
		fmt.Fprintf(os.Stderr, "sync: commit failed (snapshots stay in the work tree; the next pass commits them): %v\n", rep.CommitErr)
	case rep.Commit != "":
		fmt.Fprintf(os.Stdout, "sync: committed %s as %s <%s>\n", rep.Commit, opts.Committer.Name, opts.Committer.Email)
	default:
		fmt.Fprintln(os.Stdout, "sync: nothing to commit")
	}
	return failed
}
