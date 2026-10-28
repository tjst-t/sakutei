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

package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/graphreport"
	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/render"
	"github.com/tjst-t/sakutei/v2/internal/revision"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// runGraph implements "sakutei graph" (#136): the depends_on DAG of the
// intent — resources, depends_on, ready_condition, whether each condition
// holds now — and the waves the convergence loop applies in.  The daemon's
// read-only API serves the same report on GET /graph (docs/read-api.md).
//
// It is read-only: it renders in memory (generated/ is not written), looks
// allocations up without allocating, and Observes only the upstreams named
// by a ready_condition (§8.3) to say whether the condition holds.  An
// upstream it cannot observe counts as not ready (unknown ≠ ready), as in the
// loop.
//
// Exit codes: 0 — the graph was made; 1 — it was not (flag error, intent
// load failure, render failure).  With --json the failure is a JSON object
// with "error", except for a flag parse error.
func runGraph(args []string) {
	fs := flag.NewFlagSet("graph", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: sakutei graph --intent <dir> [--json] [flags]")
		fs.PrintDefaults()
	}
	intentDir := fs.String("intent", "", "directory containing intent JSON files (required)")
	providerConfigFile := providerConfigFlag(fs)
	statePath := fs.String("state", "sakutei-fake-state.json",
		"path to the fake provider state file (default: sakutei-fake-state.json)")
	zonePath := fs.String("zone", "",
		"path to the dnszone provider zone file (default: falls back to --state)")
	zoneSOASerial := zoneSOASerialFlag(fs)
	zoneScope := zoneScopeFlag(fs)
	redfishConfig := redfishConfigFlag(fs)
	sqlmigrateConfig := sqlmigrateConfigFlag(fs)
	s3Config := s3ConfigFlag(fs)
	postgresqlConfig := postgresqlConfigFlag(fs)
	ansibleConfig := ansibleConfigFlag(fs)
	rulesetPath := fs.String("ruleset", "",
		"path to the nftables provider ruleset file (default: falls back to --state)")
	rulesetScope := rulesetScopeFlag(fs)
	rulesetTarget := rulesetTargetFlag(fs)
	allocStore := fs.String("alloc-store", "",
		"path to the allocation store file; graph only looks allocations up, never allocates")
	allocURL := fs.String("alloc-url", "", allocURLUsage)
	allocWriteBack := fs.String("alloc-write-back", "", allocWriteBackUsage)
	claim := fs.String("claim", "",
		"scope-selector restricting the graph to a claim, e.g. site=tokyo,cluster=prod (§17.1); empty=whole world")
	invFlags := addInventoryFlags(fs, true)
	jsonOut := fs.Bool("json", false,
		"print the graph as one JSON object on stdout (schema_version 1, see docs/read-api.md)")

	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	report := graphreport.New(time.Now())
	fail := func(format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		fmt.Fprintln(os.Stderr, msg)
		if *jsonOut {
			report.Error = strings.TrimPrefix(msg, "sakutei graph: ")
			writeGraphJSON(report)
		}
		os.Exit(1)
	}
	soaSerial := checkZoneSOASerial("sakutei graph", *zoneSOASerial)
	zScope := checkScope("sakutei graph", "zone-scope", *zoneScope)
	rScope := checkScope("sakutei graph", "ruleset-scope", *rulesetScope)
	rulesetTarget.check("sakutei graph")
	claimSelector, claimErr := parseClaimSelector(*claim)
	if claimErr != nil {
		fail("sakutei graph: invalid --claim %q: %v", *claim, claimErr)
	}
	if *intentDir == "" {
		fs.Usage()
		fail("sakutei graph: --intent <dir> is required")
	}

	in, err := loader.LoadIntent(*intentDir)
	if err != nil {
		fail("sakutei graph: load intent: %v", err)
	}
	var externalInputs []string
	if input := composeOneShotInventory("sakutei graph", invFlags, &in, os.Stderr); input != "" {
		externalInputs = append(externalInputs, input)
	}
	rev := in.Revision
	allocAuth, allocErr := openAllocStore(*allocStore, *allocURL)
	if allocErr == nil {
		allocAuth, allocErr = wrapWriteBack(allocAuth, *intentDir, *allocWriteBack)
	}
	if allocErr != nil {
		fail("sakutei graph: %v", allocErr)
	}
	allocRes, allocErr := render.RenderAllocations(context.Background(), allocAuth, render.AllocLookupOnly, in, claimSelector)
	if allocErr != nil {
		fail("sakutei graph: %v", allocErr)
	}
	externalInputs = append(externalInputs, allocRes.Inputs...)
	if len(externalInputs) > 0 {
		rev.RenderedRevision = revision.ComputeRenderedRevision(core.NewScope(nil), rev.IntentRevision, externalInputs)
	}
	expanded, expandErr := render.ExpandAll(render.DefaultRegistry(), allocRes.Resources)
	if expandErr != nil {
		fail("sakutei graph: render phase 2: %v", expandErr)
	}
	desired := make([]diff.DesiredResource, 0, len(expanded))
	for _, r := range expanded {
		if !r.IsComposite && r.DesiredResource.ID.Scope.Matches(claimSelector) {
			desired = append(desired, r.DesiredResource)
		}
	}
	report.IntentRevision, report.RenderedRevision = rev.IntentRevision, rev.RenderedRevision
	report.Claim = claimSelector.String()

	reg := openProviderSet("sakutei graph", *providerConfigFile)
	providerConfig := map[string]string{"state": *statePath}
	if *zonePath != "" {
		providerConfig["zone"] = *zonePath
	}
	providerConfig["soa_serial"] = soaSerial
	providerConfig["zone_scope"] = zScope
	if *redfishConfig != "" {
		providerConfig["redfish_config"] = *redfishConfig
	}
	if *sqlmigrateConfig != "" {
		providerConfig["sqlmigrate_config"] = *sqlmigrateConfig
	}
	if *s3Config != "" {
		providerConfig["s3_config"] = *s3Config
	}
	if *postgresqlConfig != "" {
		providerConfig["postgresql_config"] = *postgresqlConfig
	}
	if *ansibleConfig != "" {
		providerConfig["ansible_config"] = *ansibleConfig
	}
	if *rulesetPath != "" {
		providerConfig["ruleset"] = *rulesetPath
	}
	providerConfig["ruleset_scope"] = rScope
	rulesetTarget.put(providerConfig)

	ready := observeReadyUpstreams(context.Background(), desired, func(name string) (provider.Provider, error) {
		return reg.Get(name, providerConfig)
	})
	report.Build(desired, ready)

	if *jsonOut {
		writeGraphJSON(report)
		os.Exit(0)
	}
	printGraph(report)
	os.Exit(0)
}

// observeReadyUpstreams Observes, read-only, each upstream named by a
// ready_condition (§8.3): a desired upstream through its own provider, one
// outside the desired set (external owner, §7) through its dependent's, as
// the loop does.  Only an Availability ok observation is recorded.
func observeReadyUpstreams(ctx context.Context, desired []diff.DesiredResource,
	providerFor func(string) (provider.Provider, error)) map[string]map[string]bool {
	byLocal := make(map[string]diff.DesiredResource, len(desired))
	for _, r := range desired {
		byLocal[r.ID.Local] = r
	}
	ready := map[string]map[string]bool{}
	tried := map[string]bool{}
	for _, r := range desired {
		for _, dep := range r.DependsOn {
			if _, ok := r.ReadyConditions[dep]; !ok || tried[dep] {
				continue
			}
			tried[dep] = true
			name, fields := r.ID.Provider, map[string]string(nil)
			if up, ok := byLocal[dep]; ok {
				name, fields = up.ID.Provider, up.Fields
			}
			p, err := providerFor(name)
			if err != nil {
				fmt.Fprintf(os.Stderr, "sakutei graph: provider %q for upstream %s: %v (not ready: unknown ≠ ready)\n", name, dep, err)
				continue
			}
			obs, err := provider.ReadFor(ctx, p, dep, fields)
			if err != nil {
				fmt.Fprintf(os.Stderr, "sakutei graph: observe upstream %s: %v (not ready: unknown ≠ ready)\n", dep, err)
				continue
			}
			if obs.Availability != core.AvailabilityOk {
				fmt.Fprintf(os.Stderr, "sakutei graph: observe upstream %s: availability=%v (not ready: unknown ≠ ready)\n", dep, obs.Availability)
				continue
			}
			conds := map[string]bool{}
			for k, v := range obs.Ready {
				conds[k] = v
			}
			ready[dep] = conds
		}
	}
	return ready
}

func writeGraphJSON(r *graphreport.Report) {
	data, err := r.JSON()
	if err != nil {
		fmt.Fprintf(os.Stderr, "sakutei graph: encode json: %v\n", err)
		os.Exit(1)
	}
	os.Stdout.Write(append(data, '\n'))
}

// printGraph is the text form: the waves in Apply order, then each edge with
// its ready_condition and whether it holds now.
func printGraph(r *graphreport.Report) {
	fmt.Printf("intent_revision=%s rendered_revision=%s\n", r.IntentRevision, r.RenderedRevision)
	fmt.Printf("Graph: %d resource(s), %d edge(s), %d wave(s)\n\n", r.Summary.Nodes-r.Summary.External, r.Summary.Edges, r.Summary.Waves)
	for i, w := range r.Waves {
		fmt.Printf("wave %d: %s\n", i, strings.Join(w, ", "))
	}
	if len(r.Edges) > 0 {
		fmt.Println()
		for _, e := range r.Edges {
			switch {
			case e.ReadyCondition == "":
				fmt.Printf("  %s -> %s\n", e.From, e.To)
			case *e.Ready:
				fmt.Printf("  %s -> %s  ready_condition %s: ready\n", e.From, e.To, e.ReadyCondition)
			default:
				fmt.Printf("  %s -> %s  ready_condition %s: NOT ready (%s waits)\n", e.From, e.To, e.ReadyCondition, e.To)
			}
		}
	}
	for _, n := range r.Nodes {
		if n.External {
			fmt.Printf("\nexternal upstream (not in the intent, observed for its ready_condition, §7): %s\n", n.LocalID)
		}
	}
}
