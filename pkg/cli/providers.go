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

// providers.go — multi-provider resolution helpers for the CLI.
//
// resolveProviders collects the unique set of provider names declared in
// the loaded intent resources, looks each up in the ProviderRegistry, and
// returns a name→Provider map ready for routing.
//
// Design rationale (S62dbba-2):
//   - A single intent directory may declare resources whose "provider" field
//     names differ (e.g. "fake" and "dnszone").
//   - The one-shot plan/reconcile commands need one provider.Provider instance
//     per name so they can route fp.Read and fp.Apply to the correct backend.
//   - The registry is the single place that knows about concrete provider
//     packages; this file depends only on the pkg/registry seam.

import (
	"flag"
	"fmt"
	"os"

	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/dnszone"
	"github.com/tjst-t/sakutei/internal/nftables"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
	"github.com/tjst-t/sakutei/pkg/sdk"
)

// resolveProviders builds a name→provider.Provider map for all provider names
// referenced in the given desired resources.
//
// reg is the provider set to look up factories in.
// baseConfig is the per-invocation configuration built from the flags (e.g.
// {"state": "/tmp/state.json"} for the fake provider); reg lays each
// provider's --provider-config values over it (#119).
//
// Returns an error if any provider name is not registered or if a factory call
// fails.  Also runs sdk.ValidateProvider on every resolved provider (fail-closed,
// §10.4, ADR-0008) and exits 1 via exitOnValidationFail when validation fails.
func resolveProviders(
	reg *providerSet,
	resources []diff.DesiredResource,
	baseConfig map[string]string,
	commandName string,
) (map[string]provider.Provider, error) {
	// Collect unique provider names from the loaded resources.
	names := make(map[string]struct{})
	for _, r := range resources {
		names[r.ID.Provider] = struct{}{}
	}

	result := make(map[string]provider.Provider, len(names))
	for name := range names {
		// Per-provider config: baseConfig overlaid with the provider's
		// --provider-config values (the factory picks what it needs from it).
		p, err := reg.Get(name, baseConfig)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", commandName, err)
		}
		// Fail-closed provider validation (§10.4, ADR-0008).
		if err := sdk.ValidateProvider(p); err != nil {
			fmt.Fprintf(os.Stderr, "sakutei: provider validation failed (%q): %v\n", name, err)
			os.Exit(1)
		}
		result[name] = p
	}
	// §28.4 (#69): an id the provider would never List (e.g. a relative DNS
	// name) is rejected before the first Read, so it can neither be Created a
	// second time nor turn the real resource into a prune orphan.
	for _, r := range resources {
		if err := provider.ValidateIntent(result[r.ID.Provider], r.ID.Local, r.Fields); err != nil {
			return nil, fmt.Errorf("%s: resource %s: %w", commandName, r.ID, err)
		}
		if err := provider.ValidateUpdatePolicy(result[r.ID.Provider], r.ID.Local, r.UpdatePolicy); err != nil {
			return nil, fmt.Errorf("%s: resource %s: %w", commandName, r.ID, err)
		}
	}
	return result, nil
}

// zoneSOASerialFlag registers --zone-soa-serial, the dnszone provider's
// "soa_serial" setting (§29.1, Backlog #70), on every command that takes
// --zone.
func zoneSOASerialFlag(fs *flag.FlagSet) *string {
	return fs.String("zone-soa-serial", string(dnszone.SOASerialBump),
		"dnszone: what an Apply that changes the zone does to the SOA serial: bump (raise it to YYYYMMDDnn) | keep (leave it to whoever else numbers it)")
}

// checkZoneSOASerial validates --zone-soa-serial at startup, before any
// provider is built (a provider the intent does not use is never built), and
// exits 1 on a bad value.  It returns the value for the provider config.
func checkZoneSOASerial(commandName, v string) string {
	m, err := dnszone.ParseSOASerialMode(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: --zone-soa-serial: %v\n", commandName, err)
		os.Exit(1)
	}
	return string(m)
}

// zoneScopeFlag registers --zone-scope, the dnszone provider's static scope
// setting (Backlog #106, Option A, §27.5): List/BulkObserve tag every
// returned id with this fixed scope instead of always the root scope.
// Format is "key=value,key=value" (same syntax as --claim).
func zoneScopeFlag(fs *flag.FlagSet) *string {
	return fs.String("zone-scope", "",
		"dnszone: fixed scope to tag every id this provider instance's List/BulkObserve return with (key=value,... ; default: root scope)")
}

// rulesetScopeFlag registers --ruleset-scope, the nftables provider's static
// scope setting (Backlog #106, Option A, §27.5): same idea as --zone-scope,
// for the nftables provider.
func rulesetScopeFlag(fs *flag.FlagSet) *string {
	return fs.String("ruleset-scope", "",
		"nftables: fixed scope to tag every id this provider instance's List/BulkObserve return with (key=value,... ; default: root scope)")
}

// rulesetTargetFlags are the nftables provider's target settings (Backlog
// #121): --ruleset-target file|live, and for live --ruleset-tables (the tables
// to manage) and --ruleset-nft (the nft binary).  The same keys can be given in
// --provider-config (ruleset_target, ruleset_tables, ruleset_nft).
type rulesetTargetFlags struct {
	target, tables, nft *string
}

func rulesetTargetFlag(fs *flag.FlagSet) rulesetTargetFlags {
	return rulesetTargetFlags{
		target: fs.String("ruleset-target", nftables.TargetFile,
			"nftables: what the provider converges: file (the --ruleset file) | live (the kernel's ruleset through nft; needs CAP_NET_ADMIN)"),
		tables: fs.String("ruleset-tables", "",
			"nftables (live): the tables to manage, <family>/<name>,... (required for live; other tables are never touched)"),
		nft: fs.String("ruleset-nft", "",
			"nftables (live): path to the nft binary (default: nft)"),
	}
}

// check validates the flags at startup and exits 1 on a bad value.
func (f rulesetTargetFlags) check(commandName string) {
	switch *f.target {
	case nftables.TargetFile, nftables.TargetLive:
	default:
		fmt.Fprintf(os.Stderr, "%s: --ruleset-target: %q: want file or live\n", commandName, *f.target)
		os.Exit(1)
	}
	if *f.tables != "" {
		if _, err := nftables.ParseTables(*f.tables); err != nil {
			fmt.Fprintf(os.Stderr, "%s: --ruleset-tables: %v\n", commandName, err)
			os.Exit(1)
		}
	}
}

// put adds the flags to the provider config.
func (f rulesetTargetFlags) put(cfg map[string]string) {
	cfg["ruleset_target"] = *f.target
	if *f.tables != "" {
		cfg["ruleset_tables"] = *f.tables
	}
	if *f.nft != "" {
		cfg["ruleset_nft"] = *f.nft
	}
}

// checkScope validates a --zone-scope/--ruleset-scope value at startup and
// exits 1 on a bad value.  It returns the value unchanged for the provider
// config (the registry re-parses it there with core.ParseScope; this call is
// just the fail-fast check so a typo is reported before any provider runs).
func checkScope(commandName, flagName, v string) string {
	if _, err := core.ParseScope(v); err != nil {
		fmt.Fprintf(os.Stderr, "%s: --%s: %v\n", commandName, flagName, err)
		os.Exit(1)
	}
	return v
}

// redfishConfigFlag registers --redfish-config, the BMC list of the Redfish
// providers (redfish-system / redfish-virtual-media, §29.1, #83): per node the
// BMC address, username and the password as a SecretRef.
func redfishConfigFlag(fs *flag.FlagSet) *string {
	return fs.String("redfish-config", "",
		"redfish: BMC list (JSON / YAML): per node address, system / manager path, username, password (a SecretRef resolved from secret_store), ca_file, insecure_skip_verify, timeout")
}

// sqlmigrateConfigFlag registers --sqlmigrate-config, the database list of the
// sqlmigrate provider (§29.1, #94): per database name the SQLite file and its
// migrations directory.
func sqlmigrateConfigFlag(fs *flag.FlagSet) *string {
	return fs.String("sqlmigrate-config", "",
		"sqlmigrate: database list (JSON / YAML): databases.<name>.path, databases.<name>.migrations (relative to the config file), sqlite3 (CLI path, optional), busy_timeout (optional)")
}
