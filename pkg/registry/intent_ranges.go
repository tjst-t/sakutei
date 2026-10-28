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

package registry

// intent_ranges.go — the range settings the built-in providers take from the
// intent's providers: section (§7.7, Backlog #181):
//
//	providers:
//	  dnszone:
//	    scope:                          # was zone_scope (--zone-scope)
//	      site: tokyo
//	  nftables:
//	    scope:                          # was ruleset_scope (--ruleset-scope)
//	      site: tokyo
//	    tables:                         # was ruleset_tables (--ruleset-tables); live only
//	      - inet/filter
//	      - ip/nat
//	  proxmox-vm:
//	    clusters:
//	      lab:
//	        scope:                      # was proxmox_config clusters.lab.scope
//	          site: tokyo
//
// The exclusions (proxmox-vm's protected_vmids / protected_names) go to the
// intent's external_owners: (§7.3, #158), not here.

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/v2/internal/nftables"
	"github.com/tjst-t/sakutei/v2/internal/proxmox"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

func registerIntentRanges(r *ProviderRegistry) {
	r.RegisterIntentRanges("dnszone", scopeRanges{setting: "zone_scope"})
	r.RegisterIntentRanges("nftables", nftablesRanges{})
	r.RegisterIntentRanges(proxmox.Name, proxmox.IntentRanges{})
}

// scopeRanges is a provider whose only range setting is its fixed scope,
// held in config[setting] (dnszone's zone_scope).
type scopeRanges struct{ setting string }

func (s scopeRanges) FromIntent(entry map[string]any, _ map[string]string) ([]provider.RangeSetting, error) {
	if err := provider.IntentKeys(entry, "scope"); err != nil {
		return nil, err
	}
	return intentScope(entry, s.setting)
}

func (s scopeRanges) FromConfig(config map[string]string) ([]provider.RangeSetting, error) {
	return configScope(config, s.setting)
}

func (s scopeRanges) Apply(config map[string]string, take []provider.RangeSetting) error {
	for _, t := range take {
		config[s.setting] = t.Value
	}
	return nil
}

func intentScope(entry map[string]any, setting string) ([]provider.RangeSetting, error) {
	v, ok := entry["scope"]
	if !ok {
		return nil, nil
	}
	sc, err := provider.IntentScope(v)
	if err != nil {
		return nil, fmt.Errorf("scope: %w", err)
	}
	return []provider.RangeSetting{{Key: "scope", Setting: setting, Value: sc.String()}}, nil
}

func configScope(config map[string]string, setting string) ([]provider.RangeSetting, error) {
	if strings.TrimSpace(config[setting]) == "" {
		return nil, nil
	}
	sc, err := core.ParseScope(config[setting])
	if err != nil {
		return nil, fmt.Errorf("%s: %w", setting, err)
	}
	return []provider.RangeSetting{{Key: "scope", Setting: setting, Value: sc.String()}}, nil
}

// nftablesRanges: the scope (ruleset_scope) and, for the live target, the
// tables to manage (ruleset_tables).
type nftablesRanges struct{}

func (nftablesRanges) FromIntent(entry map[string]any, _ map[string]string) ([]provider.RangeSetting, error) {
	if err := provider.IntentKeys(entry, "scope", "tables"); err != nil {
		return nil, err
	}
	out, err := intentScope(entry, "ruleset_scope")
	if err != nil {
		return nil, err
	}
	v, ok := entry["tables"]
	if !ok {
		return out, nil
	}
	var parts []string
	switch v := v.(type) {
	case []any:
		for i, x := range v {
			s, ok := x.(string)
			if !ok {
				return nil, fmt.Errorf("tables[%d]: want <family>/<name>", i)
			}
			parts = append(parts, s)
		}
	case string:
		parts = []string{v}
	default:
		return nil, errors.New("tables: want a list of <family>/<name>")
	}
	canon, err := canonicalTables(strings.Join(parts, ","))
	if err != nil {
		return nil, fmt.Errorf("tables: %w", err)
	}
	return append(out, provider.RangeSetting{Key: "tables", Setting: "ruleset_tables", Value: canon}), nil
}

func (nftablesRanges) FromConfig(config map[string]string) ([]provider.RangeSetting, error) {
	out, err := configScope(config, "ruleset_scope")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(config["ruleset_tables"]) == "" {
		return out, nil
	}
	canon, err := canonicalTables(config["ruleset_tables"])
	if err != nil {
		return nil, fmt.Errorf("ruleset_tables: %w", err)
	}
	return append(out, provider.RangeSetting{Key: "tables", Setting: "ruleset_tables", Value: canon}), nil
}

func (nftablesRanges) Apply(config map[string]string, take []provider.RangeSetting) error {
	for _, t := range take {
		switch t.Key {
		case "scope":
			config["ruleset_scope"] = t.Value
		case "tables":
			// The file target converges the whole file: tables the intent
			// names would be ignored there, so a narrower range is refused
			// rather than silently widened.
			if config["ruleset_target"] != nftables.TargetLive {
				return errors.New("tables: the tables to manage are for ruleset_target live (the file target converges the whole --ruleset file)")
			}
			config["ruleset_tables"] = t.Value
		}
	}
	return nil
}

// canonicalTables is the table list in a fixed order (the order does not
// change what is managed).
func canonicalTables(s string) (string, error) {
	tables, err := nftables.ParseTables(s)
	if err != nil {
		return "", err
	}
	parts := make([]string, 0, len(tables))
	for _, t := range tables {
		parts = append(parts, t.Family+"/"+t.Name)
	}
	sort.Strings(parts)
	return strings.Join(parts, ","), nil
}
