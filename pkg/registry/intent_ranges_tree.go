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

// intent_ranges_tree.go — the range settings of a provider whose config is a
// file of its own (ldap_config, postgresql_config, s3_config) (§7.7, Backlog
// #182).  The intent's providers.<name>: entry has the file's shape, with only
// the range settings:
//
//	providers:
//	  ldap:
//	    scope:                    # the file's scope (a label map in the intent)
//	      site: tokyo
//	    servers:
//	      dir1:                   # a server of the file
//	        base_dn: dc=example,dc=com
//	        filter: (!(ou=outside))
//
// The settings taken from the intent are laid over the file's tree before it
// is parsed (treeRanges.Tree), so the provider reads them as if the file had
// them: a setting the file requires (ldap's base_dn) may then be left out of
// the file.

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/v2/internal/ldap"
	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// treeField is one range setting of the file (at the top or in a member of
// the group).
type treeField struct {
	key string
	// intent reads the intent's value: the canonical form and the form to
	// lay over the file's tree.
	intent func(v any) (canon, raw string, err error)
	// config reads the file's value (as the file has it) in canonical form.
	config func(v any) (string, error)
}

// treeRanges is the provider.IntentRanges of a provider configured by a file.
type treeRanges struct {
	// file is the config key of the file's path ("ldap_config").
	file string
	// overlay is the config key Apply writes the intent's settings to (JSON
	// of the dotted key → value; Apply rewrites it every time, so one written
	// in --provider-config is overwritten).
	overlay string
	top     []treeField
	// group is the file's map of members ("servers"); member its fields.
	group  string
	member []treeField
}

func (t treeRanges) FromIntent(entry map[string]any, config map[string]string) ([]provider.RangeSetting, error) {
	allowed := make([]string, 0, len(t.top)+1)
	for _, f := range t.top {
		allowed = append(allowed, f.key)
	}
	if t.group != "" {
		allowed = append(allowed, t.group)
	}
	if err := provider.IntentKeys(entry, allowed...); err != nil {
		return nil, err
	}
	out, err := intentFields(entry, t.top, "", t.file+" ")
	if err != nil {
		return nil, err
	}
	if t.group == "" {
		return out, nil
	}
	raw, ok := entry[t.group]
	if !ok {
		return out, nil
	}
	members, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: want a map of name → its ranges", t.group)
	}
	var known map[string]any
	if path := config[t.file]; path != "" {
		tree, err := loader.ReadTree(path)
		if err != nil {
			return nil, err
		}
		known, _ = tree[t.group].(map[string]any)
	}
	for _, n := range sortedKeys(members) {
		p := t.group + "." + n
		if config[t.file] != "" {
			if _, ok := known[n]; !ok {
				return nil, fmt.Errorf("%s: no such %s in %s (the provider config says how to reach it)", p, strings.TrimSuffix(t.group, "s"), t.file)
			}
		}
		m, ok := members[n].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: want a map", p)
		}
		keys := make([]string, 0, len(t.member))
		for _, f := range t.member {
			keys = append(keys, f.key)
		}
		if err := provider.IntentKeys(m, keys...); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		settings, err := intentFields(m, t.member, p+".", t.file+" "+p+".")
		if err != nil {
			return nil, err
		}
		out = append(out, settings...)
	}
	return out, nil
}

func intentFields(m map[string]any, fields []treeField, prefix, setting string) ([]provider.RangeSetting, error) {
	var out []provider.RangeSetting
	for _, f := range fields {
		v, ok := m[f.key]
		if !ok {
			continue
		}
		canon, raw, err := f.intent(v)
		if err != nil {
			return nil, fmt.Errorf("%s%s: %w", prefix, f.key, err)
		}
		out = append(out, provider.RangeSetting{Key: prefix + f.key, Setting: setting + f.key, Value: canon, Raw: raw})
	}
	return out, nil
}

func (t treeRanges) FromConfig(config map[string]string) ([]provider.RangeSetting, error) {
	path := config[t.file]
	if path == "" {
		return nil, nil // the factory refuses
	}
	tree, err := loader.ReadTree(path)
	if err != nil {
		return nil, err
	}
	out, err := configFields(tree, t.top, "", t.file+" ")
	if err != nil {
		return nil, err
	}
	if t.group == "" {
		return out, nil
	}
	members, _ := tree[t.group].(map[string]any)
	for _, n := range sortedKeys(members) {
		m, ok := members[n].(map[string]any)
		if !ok {
			continue // the provider refuses
		}
		p := t.group + "." + n + "."
		settings, err := configFields(m, t.member, p, t.file+" "+p)
		if err != nil {
			return nil, err
		}
		out = append(out, settings...)
	}
	return out, nil
}

func configFields(m map[string]any, fields []treeField, prefix, setting string) ([]provider.RangeSetting, error) {
	var out []provider.RangeSetting
	for _, f := range fields {
		v, ok := m[f.key]
		if !ok {
			continue
		}
		if s, isStr := v.(string); isStr && strings.TrimSpace(s) == "" {
			continue
		}
		canon, err := f.config(v)
		if err != nil {
			return nil, fmt.Errorf("%s%s: %w", setting, f.key, err)
		}
		out = append(out, provider.RangeSetting{Key: prefix + f.key, Setting: setting + f.key, Value: canon})
	}
	return out, nil
}

func (t treeRanges) Apply(config map[string]string, take []provider.RangeSetting) error {
	delete(config, t.overlay)
	if len(take) == 0 {
		return nil
	}
	over := map[string]string{}
	for _, s := range take {
		v := s.Raw
		if v == "" {
			v = s.Value
		}
		over[s.Key] = v
	}
	b, err := json.Marshal(over)
	if err != nil {
		return err
	}
	config[t.overlay] = string(b)
	return nil
}

// Tree reads the provider's file with the intent's settings Apply wrote laid
// over it, and returns the file's path and the tree.
func (t treeRanges) Tree(config map[string]string) (string, map[string]any, error) {
	path := config[t.file]
	tree, err := loader.ReadTree(path)
	if err != nil {
		return "", nil, err
	}
	if config[t.overlay] == "" {
		return path, tree, nil
	}
	var over map[string]string
	if err := json.Unmarshal([]byte(config[t.overlay]), &over); err != nil {
		return "", nil, fmt.Errorf("%s: %w", t.overlay, err)
	}
	for _, k := range sortedKeys(over) {
		v := over[k]
		parts := strings.Split(k, ".")
		switch len(parts) {
		case 1:
			tree[k] = v
		case 3:
			members, _ := tree[parts[0]].(map[string]any)
			m, ok := members[parts[1]].(map[string]any)
			if !ok {
				return "", nil, fmt.Errorf("intent providers: %s.%s: no such %s in %s", parts[0], parts[1], strings.TrimSuffix(parts[0], "s"), path)
			}
			m[parts[2]] = v
		default:
			return "", nil, fmt.Errorf("%s: bad key %q", t.overlay, k)
		}
	}
	return path, tree, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// The fields.

// scopeField is a scope: a label map in the intent, a "k=v,..." string in the
// file.
func scopeField() treeField {
	return treeField{
		key: "scope",
		intent: func(v any) (string, string, error) {
			sc, err := provider.IntentScope(v)
			if err != nil {
				return "", "", err
			}
			return sc.String(), "", nil
		},
		config: func(v any) (string, error) {
			s, ok := v.(string)
			if !ok {
				return "", errors.New(`want a string such as "site=tokyo"`)
			}
			sc, err := core.ParseScope(s)
			if err != nil {
				return "", err
			}
			return sc.String(), nil
		},
	}
}

// stringField is a string compared as written (e.g. s3's bucket_prefix).
func stringField(key string, check func(string) error) treeField {
	read := func(v any) (string, error) {
		s, ok := v.(string)
		if !ok {
			return "", errors.New("want a string")
		}
		if check != nil {
			if err := check(s); err != nil {
				return "", err
			}
		}
		return s, nil
	}
	return treeField{
		key: key,
		intent: func(v any) (string, string, error) {
			s, err := read(v)
			return s, "", err
		},
		config: read,
	}
}

// ldapFilter is an LDAP filter in parentheses (ldap checks it again).
func ldapFilter(s string) error {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "(") || !strings.HasSuffix(s, ")") || strings.IndexByte(s, 0) >= 0 {
		return fmt.Errorf("%q: want an LDAP filter in parentheses such as (objectClass=inetOrgPerson)", s)
	}
	return nil
}

// ldapDNField is a DN compared in its canonical form (cn=X,DC=Example and
// cn=x,dc=example are the same base); the intent's DN is laid over the file
// as written.
func ldapDNField(key string) treeField {
	canon := func(v any) (string, string, error) {
		s, ok := v.(string)
		if !ok {
			return "", "", errors.New("want a DN")
		}
		dn, err := ldap.ParseDN(s)
		if err != nil {
			return "", "", err
		}
		if len(dn) == 0 {
			return "", "", errors.New("the root DSE cannot be the base")
		}
		return dn.Canonical().String(), s, nil
	}
	return treeField{
		key:    key,
		intent: canon,
		config: func(v any) (string, error) {
			c, _, err := canon(v)
			return c, err
		},
	}
}

var (
	// ldapRanges: the scope (top and per server), and each server's base_dn
	// and filter (what is under management, §7.7).
	ldapRanges = treeRanges{
		file: "ldap_config", overlay: "ldap_intent_ranges",
		top:   []treeField{scopeField()},
		group: "servers",
		member: []treeField{
			ldapDNField("base_dn"),
			stringField("filter", ldapFilter),
			scopeField(),
		},
	}
	// postgresqlRanges: the scope (top and per server).
	postgresqlRanges = treeRanges{
		file: "postgresql_config", overlay: "postgresql_intent_ranges",
		top:    []treeField{scopeField()},
		group:  "servers",
		member: []treeField{scopeField()},
	}
	// s3Ranges: the scope, and each endpoint's bucket_prefix (the buckets it
	// manages).  The endpoint's fields (the settings the implementation
	// supports) say what the endpoint can do and stay in the file.
	s3Ranges = treeRanges{
		file: "s3_config", overlay: "s3_intent_ranges",
		top:    []treeField{scopeField()},
		group:  "endpoints",
		member: []treeField{stringField("bucket_prefix", nil)},
	}
)
