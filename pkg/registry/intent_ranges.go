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
//
// Backlog #182 moves the rest (docs/providers/ranges.md has the list):
//
//	providers:
//	  netconf:
//	    hosts: [sw1]                    # was netconf_hosts
//	    subtrees:                       # was the netconf_subtrees list
//	      if-uplink:
//	        filter: '<interfaces xmlns="..."/>'
//	  guest-os:
//	    hosts: [node1]                  # was guest_hosts
//	    markers:                        # was guest_markers
//	      installed: /etc/sakutei-installed
//	  host-file:    {hosts: [web1]}     # was file_hosts
//	  systemd-unit: {hosts: [web1]}     # was unit_hosts
//	  cli-device:   {hosts: [sw1]}      # was cli_hosts
//
// and ldap / postgresql / s3-bucket, whose config is a file of its own, in
// intent_ranges_tree.go.

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/v2/internal/clidevice"
	"github.com/tjst-t/sakutei/v2/internal/guestos"
	"github.com/tjst-t/sakutei/v2/internal/hostconf"
	"github.com/tjst-t/sakutei/v2/internal/ldap"
	"github.com/tjst-t/sakutei/v2/internal/netconf"
	"github.com/tjst-t/sakutei/v2/internal/nftables"
	"github.com/tjst-t/sakutei/v2/internal/postgresql"
	"github.com/tjst-t/sakutei/v2/internal/proxmox"
	"github.com/tjst-t/sakutei/v2/internal/s3bucket"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

func registerIntentRanges(r *ProviderRegistry) {
	r.RegisterIntentRanges("dnszone", scopeRanges{setting: "zone_scope"})
	r.RegisterIntentRanges("nftables", nftablesRanges{})
	r.RegisterIntentRanges(proxmox.Name, proxmox.IntentRanges{})
	r.RegisterIntentRanges(proxmox.FWName, proxmox.IntentRanges{})
	// Backlog #182: the rest of the built-in providers.
	r.RegisterIntentRanges(netconf.Name, netconfRanges{})
	r.RegisterIntentRanges(guestos.Name, guestosRanges{})
	r.RegisterIntentRanges(hostconf.FileName, hostsRanges{setting: "file_hosts"})
	r.RegisterIntentRanges(hostconf.UnitName, hostsRanges{setting: "unit_hosts"})
	r.RegisterIntentRanges(clidevice.Name, hostsRanges{setting: "cli_hosts"})
	r.RegisterIntentRanges(ldap.Name, ldapRanges)
	r.RegisterIntentRanges(postgresql.Name, postgresqlRanges)
	r.RegisterIntentRanges(s3bucket.Name, s3Ranges)
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

// hostsRanges: the hosts (devices) a provider instance manages, held in
// config[setting] ("a,b,...", e.g. file_hosts) (§7.7, Backlog #182).  The
// host is the head of the provider's ids, so the list is what it manages;
// how each host is reached (address, user, key) stays in remote_config.
type hostsRanges struct{ setting string }

func (h hostsRanges) FromIntent(entry map[string]any, _ map[string]string) ([]provider.RangeSetting, error) {
	if err := provider.IntentKeys(entry, "hosts"); err != nil {
		return nil, err
	}
	return intentHosts(entry, h.setting)
}

func (h hostsRanges) FromConfig(config map[string]string) ([]provider.RangeSetting, error) {
	return configHosts(config, h.setting)
}

func (h hostsRanges) Apply(config map[string]string, take []provider.RangeSetting) error {
	for _, t := range take {
		config[h.setting] = t.Value
	}
	return nil
}

func intentHosts(entry map[string]any, setting string) ([]provider.RangeSetting, error) {
	v, ok := entry["hosts"]
	if !ok {
		return nil, nil
	}
	var names []string
	switch v := v.(type) {
	case []any:
		for i, x := range v {
			s, ok := x.(string)
			if !ok {
				return nil, fmt.Errorf("hosts[%d]: want a host name", i)
			}
			names = append(names, s)
		}
	case string:
		names = []string{v}
	default:
		return nil, errors.New("hosts: want a list of host names")
	}
	canon, err := canonicalHosts(names)
	if err != nil {
		return nil, fmt.Errorf("hosts: %w", err)
	}
	return []provider.RangeSetting{{Key: "hosts", Setting: setting, Value: canon}}, nil
}

func configHosts(config map[string]string, setting string) ([]provider.RangeSetting, error) {
	if strings.TrimSpace(config[setting]) == "" {
		return nil, nil
	}
	canon, err := canonicalHosts(strings.Split(config[setting], ","))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", setting, err)
	}
	return []provider.RangeSetting{{Key: "hosts", Setting: setting, Value: canon}}, nil
}

// canonicalHosts is the host list in a fixed order (the order does not
// change what is managed).
func canonicalHosts(names []string) (string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if strings.ContainsAny(n, ", \t/") {
			return "", fmt.Errorf("%q is not a host name", n)
		}
		if seen[n] {
			return "", fmt.Errorf("host %q is listed twice", n)
		}
		seen[n] = true
		out = append(out, n)
	}
	if len(out) == 0 {
		return "", errors.New("no host")
	}
	sort.Strings(out)
	return strings.Join(out, ","), nil
}

// guestosRanges: the hosts observed (guest_hosts) and the markers of an
// installation (guest_markers, name → absolute path).
type guestosRanges struct{}

func (guestosRanges) FromIntent(entry map[string]any, _ map[string]string) ([]provider.RangeSetting, error) {
	if err := provider.IntentKeys(entry, "hosts", "markers"); err != nil {
		return nil, err
	}
	out, err := intentHosts(entry, "guest_hosts")
	if err != nil {
		return nil, err
	}
	raw, ok := entry["markers"]
	if !ok {
		return out, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("markers: want a map of marker name → absolute path")
	}
	for _, n := range sortedKeys(m) {
		path, ok := m[n].(string)
		if !ok {
			return nil, fmt.Errorf("markers.%s: want an absolute path", n)
		}
		if _, err := guestos.ParseMarkers(n + "=" + path); err != nil || strings.Contains(n, "=") || strings.Contains(path, ",") {
			if err == nil {
				err = fmt.Errorf("marker %q: want <name>: <absolute path>", n)
			}
			return nil, fmt.Errorf("markers: %w", err)
		}
		out = append(out, provider.RangeSetting{Key: "markers." + n, Setting: "guest_markers " + n, Value: path})
	}
	return out, nil
}

func (guestosRanges) FromConfig(config map[string]string) ([]provider.RangeSetting, error) {
	out, err := configHosts(config, "guest_hosts")
	if err != nil {
		return nil, err
	}
	markers, err := guestos.ParseMarkers(config["guest_markers"])
	if err != nil {
		return nil, fmt.Errorf("guest_markers: %w", err)
	}
	for _, mk := range markers {
		out = append(out, provider.RangeSetting{Key: "markers." + mk.Name, Setting: "guest_markers " + mk.Name, Value: mk.Path})
	}
	return out, nil
}

func (guestosRanges) Apply(config map[string]string, take []provider.RangeSetting) error {
	var markers []string
	if strings.TrimSpace(config["guest_markers"]) != "" {
		markers = append(markers, config["guest_markers"])
	}
	for _, t := range take {
		if t.Key == "hosts" {
			config["guest_hosts"] = t.Value
			continue
		}
		markers = append(markers, strings.TrimPrefix(t.Key, "markers.")+"="+t.Value)
	}
	if len(markers) > 0 {
		config["guest_markers"] = strings.Join(markers, ",")
	}
	return nil
}

// netconfIntentSubtrees is the config key netconfRanges.Apply writes the
// intent's subtrees to (JSON of name → filter; rewritten every time, so one
// written in --provider-config is overwritten).
const netconfIntentSubtrees = "netconf_intent_subtrees"

// netconfRanges: the devices (netconf_hosts) and the subtrees observed on
// them (netconf_subtrees, name → subtree filter).  Filters are compared in
// netconf's canonical form (namespaces by URI, whitespace and sibling order
// ignored), so one copied from the subtree list into the intent agrees.
type netconfRanges struct{}

func (netconfRanges) FromIntent(entry map[string]any, _ map[string]string) ([]provider.RangeSetting, error) {
	if err := provider.IntentKeys(entry, "hosts", "subtrees"); err != nil {
		return nil, err
	}
	out, err := intentHosts(entry, "netconf_hosts")
	if err != nil {
		return nil, err
	}
	raw, ok := entry["subtrees"]
	if !ok {
		return out, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("subtrees: want a map of subtree name → {filter}")
	}
	for _, n := range sortedKeys(m) {
		st, ok := m[n].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("subtrees.%s: want a map with filter", n)
		}
		if err := provider.IntentKeys(st, "filter"); err != nil {
			return nil, fmt.Errorf("subtrees.%s: %w (a filter_file stays in the subtree list netconf_subtrees)", n, err)
		}
		filter, ok := st["filter"].(string)
		if !ok {
			return nil, fmt.Errorf("subtrees.%s.filter: want the subtree filter's XML", n)
		}
		s, err := netconfSubtreeSetting(n, filter)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func netconfSubtreeSetting(name, filter string) (provider.RangeSetting, error) {
	st, err := netconf.NewSubtree(name, filter)
	if err != nil {
		return provider.RangeSetting{}, err
	}
	canon, err := netconf.Canonical(st.Filter, true, "")
	if err != nil {
		return provider.RangeSetting{}, fmt.Errorf("subtree %s: filter: %w", name, err)
	}
	return provider.RangeSetting{Key: "subtrees." + name, Setting: "netconf_subtrees " + name, Value: canon, Raw: st.Filter}, nil
}

func (netconfRanges) FromConfig(config map[string]string) ([]provider.RangeSetting, error) {
	out, err := configHosts(config, "netconf_hosts")
	if err != nil {
		return nil, err
	}
	if config["netconf_subtrees"] == "" {
		return out, nil
	}
	subtrees, err := netconf.LoadSubtrees(config["netconf_subtrees"])
	if err != nil {
		return nil, err
	}
	for _, n := range sortedKeys(subtrees) {
		s, err := netconfSubtreeSetting(n, subtrees[n].Filter)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func (netconfRanges) Apply(config map[string]string, take []provider.RangeSetting) error {
	delete(config, netconfIntentSubtrees)
	subtrees := map[string]string{}
	for _, t := range take {
		if t.Key == "hosts" {
			config["netconf_hosts"] = t.Value
			continue
		}
		subtrees[strings.TrimPrefix(t.Key, "subtrees.")] = t.Raw
	}
	if len(subtrees) == 0 {
		return nil
	}
	b, err := json.Marshal(subtrees)
	if err != nil {
		return err
	}
	config[netconfIntentSubtrees] = string(b)
	return nil
}

// netconfSubtrees is the subtree list of the file netconf_subtrees and the
// intent's providers.netconf.subtrees together.
func netconfSubtrees(config map[string]string) (map[string]netconf.Subtree, error) {
	out := map[string]netconf.Subtree{}
	if config["netconf_subtrees"] != "" {
		var err error
		if out, err = netconf.LoadSubtrees(config["netconf_subtrees"]); err != nil {
			return nil, err
		}
	}
	if v := config[netconfIntentSubtrees]; v != "" {
		var raw map[string]string
		if err := json.Unmarshal([]byte(v), &raw); err != nil {
			return nil, fmt.Errorf("%s: %w", netconfIntentSubtrees, err)
		}
		for n, f := range raw {
			st, err := netconf.NewSubtree(n, f)
			if err != nil {
				return nil, fmt.Errorf("intent providers.netconf.subtrees: %w", err)
			}
			out[n] = st
		}
	}
	if len(out) == 0 {
		return nil, errors.New("netconf: the subtrees are not set: providers.netconf.subtrees in the intent (or netconf_subtrees in --provider-config)")
	}
	return out, nil
}
