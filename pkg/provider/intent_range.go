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

package provider

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// RangeSetting is one range setting of a provider instance (§7.7, Backlog
// #181): what it manages — the scope it tags its ids with, the tables it
// manages — as opposed to how it is reached (endpoints, keys, timeouts),
// which stays in the provider config (§1.7).
type RangeSetting struct {
	// Key is the setting's path under the intent's providers.<name>:, dotted
	// (e.g. "scope", "tables", "clusters.lab.scope").
	Key string
	// Setting names where the provider config has it, for messages (e.g.
	// "zone_scope", "proxmox_config clusters.lab.scope").
	Setting string
	// Value is the canonical form: two settings meaning the same have the
	// same Value (e.g. a scope as core.Scope.String()).
	Value string
	// Raw is the value as written, for Apply, when the canonical Value is
	// only for comparing (e.g. a netconf subtree filter, Backlog #182).
	// Empty: Apply uses Value.
	Raw string
}

// IntentRanges is how a provider takes its range settings from the intent's
// providers.<name>: section (§7.7, Backlog #181).  It is registered next to
// the provider's factory (registry.RegisterIntentRanges) since the settings
// are needed before the provider is built.
//
// MergeRanges holds the one rule all providers share: a setting only in the
// config is read with a warning, one only in the intent is taken, and one in
// both must be the same — they disagree otherwise, and the run is refused
// (never settled toward the weaker side).
type IntentRanges interface {
	// FromIntent parses the provider's providers.<name>: entry.  An unknown
	// key or a bad value is an error.  config is the provider config, for a
	// setting that names something of it (e.g. a proxmox-vm cluster).
	FromIntent(entry map[string]any, config map[string]string) ([]RangeSetting, error)
	// FromConfig reports the range settings the provider config sets
	// (an unset one is left out).
	FromConfig(config map[string]string) ([]RangeSetting, error)
	// Apply writes the settings taken from the intent into config (a copy
	// the provider is then built from).  take may be empty.
	Apply(config map[string]string, take []RangeSetting) error
}

// MergeRanges merges the intent's providers.<name>: entry (nil: the intent
// has none) into config by the rule of IntentRanges and returns the config to
// build the provider from.  config is not modified.
func MergeRanges(name string, spec IntentRanges, entry map[string]any, config map[string]string) (map[string]string, []string, error) {
	inIntent := map[string]RangeSetting{}
	if entry != nil {
		settings, err := spec.FromIntent(entry, config)
		if err != nil {
			return nil, nil, fmt.Errorf("intent providers.%s: %w", name, err)
		}
		for _, s := range settings {
			if _, dup := inIntent[s.Key]; dup {
				return nil, nil, fmt.Errorf("intent providers.%s.%s: declared twice", name, s.Key)
			}
			inIntent[s.Key] = s
		}
	}
	fromConfig, err := spec.FromConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("provider %s config: %w", name, err)
	}
	var warnings []string
	inConfig := map[string]bool{}
	for _, c := range fromConfig {
		inConfig[c.Key] = true
		s, ok := inIntent[c.Key]
		if !ok {
			warnings = append(warnings, fmt.Sprintf("provider %s: %s=%q is only in the provider config: write it in the intent too (providers.%s.%s, §7.7); the provider config keeps it as the safety net until the next major version", name, c.Setting, c.Value, name, c.Key))
			continue
		}
		if s.Value != c.Value {
			return nil, nil, fmt.Errorf("provider %s: the intent's providers.%s.%s is %q but the provider config's %s is %q; the two disagree — make them the same, or take it out of the provider config (§7.7, §29.1)", name, name, c.Key, s.Value, c.Setting, c.Value)
		}
	}
	var take []RangeSetting
	for _, s := range inIntent {
		if !inConfig[s.Key] {
			take = append(take, s)
		}
	}
	sort.Slice(take, func(i, j int) bool { return take[i].Key < take[j].Key })
	out := make(map[string]string, len(config)+len(take))
	for k, v := range config {
		out[k] = v
	}
	if err := spec.Apply(out, take); err != nil {
		return nil, nil, fmt.Errorf("intent providers.%s: %w", name, err)
	}
	return out, warnings, nil
}

// IntentKeys refuses a key of an intent entry that is not in allowed (a
// misspelt setting must not be silently ignored).
func IntentKeys(entry map[string]any, allowed ...string) error {
	ok := map[string]bool{}
	for _, k := range allowed {
		ok[k] = true
	}
	keys := make([]string, 0, len(entry))
	for k := range entry {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !ok[k] {
			return fmt.Errorf("unknown key %q (want %s)", k, strings.Join(allowed, ", "))
		}
	}
	return nil
}

// IntentScope reads a scope written in the intent as a label map
// (site: tokyo), the same form as a resource's scope.
func IntentScope(v any) (core.Scope, error) {
	if v == nil {
		return core.NewScope(nil), nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return core.Scope{}, fmt.Errorf("want a label map of names to values (e.g. site: tokyo)")
	}
	labels := make(map[string]string, len(m))
	for k, x := range m {
		if strings.TrimSpace(k) == "" {
			return core.Scope{}, fmt.Errorf("an empty label name")
		}
		switch x := x.(type) {
		case string:
			labels[k] = x
		case float64, int, int64, bool:
			labels[k] = fmt.Sprintf("%v", x)
		default:
			return core.Scope{}, fmt.Errorf("label %q: want a string", k)
		}
	}
	return core.NewScope(labels), nil
}
