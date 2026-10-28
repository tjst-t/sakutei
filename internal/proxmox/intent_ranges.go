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

package proxmox

// intent_ranges.go — intent の providers.proxmox-vm:（§7.7、Backlog #181）。
// クラスタごとの scope を intent に書く:
//
//	providers:
//	  proxmox-vm:
//	    clusters:
//	      lab:
//	        scope:
//	          site: tokyo
//
// 触らない VM（protected_vmids・protected_names）は external_owners:（§7.3、
// #158）に書く。設定の分は最後の安全の網として残る（ConfigExclusions）。

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// intentScopesKey は intent から取ったクラスタの scope を factory に渡す設定の
// キー（Apply が毎回書き直す。--provider-config に書いても上書きされる）。
const intentScopesKey = "proxmox_intent_scopes"

// IntentRanges は proxmox-vm の provider.IntentRanges。
type IntentRanges struct{}

func (IntentRanges) FromIntent(entry map[string]any, config map[string]string) ([]provider.RangeSetting, error) {
	if err := provider.IntentKeys(entry, "clusters"); err != nil {
		return nil, err
	}
	raw, ok := entry["clusters"]
	if !ok {
		return nil, nil
	}
	clusters, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("clusters: want a map of cluster name → its ranges")
	}
	var known map[string]ClusterConfig
	if path := config["proxmox_config"]; path != "" {
		cfg, err := loadFor(config, path)
		if err != nil {
			return nil, err
		}
		known = cfg.Clusters
	}
	names := make([]string, 0, len(clusters))
	for n := range clusters {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []provider.RangeSetting
	for _, n := range names {
		if known != nil {
			if _, ok := known[n]; !ok {
				return nil, fmt.Errorf("clusters.%s: no such cluster in proxmox_config (the provider config says how to reach it)", n)
			}
		}
		m, ok := clusters[n].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("clusters.%s: want a map", n)
		}
		if err := provider.IntentKeys(m, "scope"); err != nil {
			return nil, fmt.Errorf("clusters.%s: %w", n, err)
		}
		v, ok := m["scope"]
		if !ok {
			continue
		}
		sc, err := provider.IntentScope(v)
		if err != nil {
			return nil, fmt.Errorf("clusters.%s.scope: %w", n, err)
		}
		out = append(out, provider.RangeSetting{Key: "clusters." + n + ".scope", Setting: "proxmox_config clusters." + n + ".scope", Value: sc.String()})
	}
	return out, nil
}

func (IntentRanges) FromConfig(config map[string]string) ([]provider.RangeSetting, error) {
	path := config["proxmox_config"]
	if path == "" {
		return nil, nil // factory が断る
	}
	cfg, err := loadFor(config, path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(cfg.Clusters))
	for n := range cfg.Clusters {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []provider.RangeSetting
	for _, n := range names {
		if sc := cfg.Clusters[n].Scope; len(sc.Labels) > 0 {
			out = append(out, provider.RangeSetting{Key: "clusters." + n + ".scope", Setting: "proxmox_config clusters." + n + ".scope", Value: sc.String()})
		}
	}
	return out, nil
}

func (IntentRanges) Apply(config map[string]string, take []provider.RangeSetting) error {
	delete(config, intentScopesKey)
	scopes := map[string]string{}
	for _, t := range take {
		name := strings.TrimSuffix(strings.TrimPrefix(t.Key, "clusters."), ".scope")
		scopes[name] = t.Value
	}
	if len(scopes) == 0 {
		return nil
	}
	b, err := json.Marshal(scopes)
	if err != nil {
		return err
	}
	config[intentScopesKey] = string(b)
	return nil
}

// intentScopes は Apply が書いたクラスタの scope を読む。
func intentScopes(config map[string]string) (map[string]core.Scope, error) {
	v := config[intentScopesKey]
	if v == "" {
		return nil, nil
	}
	var raw map[string]string
	if err := json.Unmarshal([]byte(v), &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", intentScopesKey, err)
	}
	out := make(map[string]core.Scope, len(raw))
	for n, s := range raw {
		sc, err := core.ParseScope(s)
		if err != nil {
			return nil, fmt.Errorf("clusters.%s.scope: %w", n, err)
		}
		out[n] = sc
	}
	return out, nil
}

// NewFromProviderConfig は registry の factory 用：config["proxmox_config"] の
// クラスタの一覧に、intent から取ったクラスタの scope（§7.7）を重ねて Provider を作る。
func NewFromProviderConfig(config map[string]string) (*Provider, error) {
	path := config["proxmox_config"]
	if path == "" {
		return nil, fmt.Errorf("proxmox-vm: the cluster list is not set: proxmox_config in --provider-config")
	}
	scopes, err := intentScopes(config)
	if err != nil {
		return nil, fmt.Errorf("proxmox-vm: %w", err)
	}
	cfg, err := loadFor(config, path)
	if err != nil {
		return nil, err
	}
	for n, sc := range scopes {
		cc, ok := cfg.Clusters[n]
		if !ok {
			return nil, fmt.Errorf("proxmox-vm: intent providers.proxmox-vm.clusters.%s: no such cluster in %s", n, path)
		}
		cc.Scope = sc
		cfg.Clusters[n] = cc
	}
	cls, err := cfg.Build()
	if err != nil {
		return nil, fmt.Errorf("proxmox config %s: %w", path, err)
	}
	return New(cls, Options{}), nil
}

// loadFor は path の設定を、config の回の access（provider.ConfigAccess、§20.7、
// #186）の読み方で読む。書かない回は書き込み用の secret を確かめない。
func loadFor(config map[string]string, path string) (Config, error) {
	if err := provider.CheckAccess(config); err != nil {
		return Config{}, fmt.Errorf("proxmox-vm: %w", err)
	}
	return LoadConfigWith(path, LoadOptions{ObserveOnly: provider.ObserveOnlyRun(config)})
}
