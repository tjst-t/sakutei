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
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/internal/importer"
	"github.com/tjst-t/sakutei/pkg/provider"
	"github.com/tjst-t/sakutei/pkg/registry"
	"github.com/tjst-t/sakutei/pkg/sdk"
)

// runImport implements `sakutei import` (reverse-render, §24.4): Observe the
// existing reality through List + Read and write it out as intent YAML, one
// file per provider (<out>/imported-<provider>.yaml).
//
// It only reads reality.  Fields that changed between observations are marked
// as managed candidates in the output (§24.4).
func runImport(args []string) {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	providersFlag := fs.String("provider", "", "provider(s) to import, comma-separated, e.g. dnszone,nftables (required)")
	statePath := fs.String("state", "sakutei-fake-state.json", "path to the fake provider state file")
	zonePath := fs.String("zone", "", "path to the dnszone provider zone file (default: falls back to --state)")
	zoneSOASerial := zoneSOASerialFlag(fs)
	rulesetPath := fs.String("ruleset", "", "path to the nftables provider ruleset file (default: falls back to --state)")
	scopeFlag := fs.String("scope", "", "import only resources whose scope matches these labels, e.g. site=tokyo,rack=r1 (default: everything)")
	outDir := fs.String("out", "", "directory to write imported intent files into (required)")
	samples := fs.Int("samples", 3, "how many times to observe reality; fields that change between observations become managed candidates")
	intervalStr := fs.String("sample-interval", "2s", "wait between observations")
	force := fs.Bool("force", false, "overwrite existing imported-<provider>.yaml files")

	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	soaSerial := checkZoneSOASerial("sakutei import", *zoneSOASerial)
	if *providersFlag == "" || *outDir == "" {
		fmt.Fprintln(os.Stderr, "sakutei import: --provider and --out are required")
		fs.Usage()
		os.Exit(1)
	}
	if *samples < 1 {
		fmt.Fprintln(os.Stderr, "sakutei import: --samples must be >= 1")
		os.Exit(1)
	}
	interval, err := time.ParseDuration(*intervalStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sakutei import: invalid --sample-interval %q: %v\n", *intervalStr, err)
		os.Exit(1)
	}
	selector, err := parseClaimSelector(*scopeFlag) // same key=value selector syntax as --claim
	if err != nil {
		fmt.Fprintf(os.Stderr, "sakutei import: invalid --scope: %v\n", err)
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
	var names []string
	for _, name := range strings.Split(*providersFlag, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		p, err := reg.Get(name, cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sakutei import: %v\n", err)
			os.Exit(1)
		}
		if err := sdk.ValidateProvider(p); err != nil {
			fmt.Fprintf(os.Stderr, "sakutei import: provider validation failed (%q): %v\n", name, err)
			os.Exit(1)
		}
		providers[name] = p
		names = append(names, name)
	}

	// Refuse to clobber earlier imports (they may already be hand-edited).
	if !*force {
		for _, name := range names {
			path := filepath.Join(*outDir, "imported-"+name+".yaml")
			if _, err := os.Stat(path); err == nil {
				fmt.Fprintf(os.Stderr, "sakutei import: %s already exists (it may contain your edits); use --force to overwrite\n", path)
				os.Exit(1)
			}
		}
	}

	res, err := importer.Run(context.Background(), importer.Options{
		Providers: providers,
		Selector:  selector,
		Samples:   *samples,
		Interval:  interval,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "sakutei import: %v\n", err)
		os.Exit(1)
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "sakutei import: create --out: %v\n", err)
		os.Exit(1)
	}

	byProvider := map[string][]importer.Resource{}
	for _, r := range res.Resources {
		byProvider[r.Provider] = append(byProvider[r.Provider], r)
	}
	candidates := 0
	for _, name := range names {
		rs := byProvider[name]
		if len(rs) == 0 {
			fmt.Fprintf(os.Stdout, "import: %s: no resources in scope; nothing written\n", name)
			continue
		}
		path := filepath.Join(*outDir, "imported-"+name+".yaml")
		if err := writeAtomic(path, importer.EmitYAML(rs, *samples), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "sakutei import: write %s: %v\n", path, err)
			os.Exit(1)
		}
		for _, r := range rs {
			candidates += len(r.ManagedCandidates)
		}
		fmt.Fprintf(os.Stdout, "import: %s: %d resource(s) → %s\n", name, len(rs), path)
	}
	for _, r := range res.Resources {
		for _, f := range sortedFieldNames(r.ManagedCandidates) {
			fmt.Fprintf(os.Stdout, "import: managed candidate: %s field %q changed across %d observations: %s\n",
				r.ID, f, *samples, strings.Join(r.ManagedCandidates[f], " -> "))
		}
	}
	for _, s := range res.Skipped {
		fmt.Fprintf(os.Stdout, "import: skipped %s: %s\n", s.ID, s.Reason)
	}
	fmt.Fprintf(os.Stdout, "import: done (%d resource(s), %d managed candidate(s), %d skipped). This is a snapshot, not an ideal: review it, then run with --mode alert-only first (§24.2).\n",
		len(res.Resources), candidates, len(res.Skipped))
}

func sortedFieldNames(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// writeAtomic writes data to a temp file in the same directory and renames it
// over path, so a crash never leaves a half-written intent file.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Chmod(name, perm); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}
