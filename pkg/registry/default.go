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

import (
	"errors"
	"fmt"

	"github.com/tjst-t/sakutei/internal/ansible"
	"github.com/tjst-t/sakutei/internal/dnszone"
	"github.com/tjst-t/sakutei/internal/fakeprovider"
	"github.com/tjst-t/sakutei/internal/nftables"
	"github.com/tjst-t/sakutei/internal/postgresql"
	"github.com/tjst-t/sakutei/internal/redfish"
	"github.com/tjst-t/sakutei/internal/redfish/bmcconfig"
	"github.com/tjst-t/sakutei/internal/redfish/system"
	"github.com/tjst-t/sakutei/internal/s3bucket"
	"github.com/tjst-t/sakutei/internal/sqlmigrate"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// DefaultRegistry returns a new ProviderRegistry pre-loaded with the built-in
// providers:
//
//   - "fake": in-memory, file-backed fake provider (fakeprovider.New).
//     config["state"] is the path to the state file.  Defaults to
//     "sakutei-fake-state.json" when the key is absent.
//
//   - "dnszone": RFC1035 zone-file provider (dnszone.New, S62dbba-3).
//     config["zone"] is the path to the zone file.  If absent, defaults to
//     "sakutei-zone.db".  Manages A/AAAA/CNAME/TXT/MX/… records; SOA and NS
//     are excluded (management boundary, decisions.json S62dbba).
//     config["soa_serial"] is "bump" (default: a changing Apply raises the
//     SOA serial to YYYYMMDDnn) or "keep" (§29.1, Backlog #70).
//
//   - "nftables": nftables ruleset-file provider (nftables.New, Sc7a647-1).
//     config["ruleset"] is the path to the managed ruleset file.  If absent,
//     defaults to "sakutei-nftables.rules".  Manages regular chains whose
//     `rules` field is an ORDERED list (reordering is a diff); base chains
//     (type/hook/policy) are excluded (management boundary, §27 FW〈順序付き〉).
//     config["ruleset_target"] is "file" (default: the ruleset file above) or
//     "live" (the kernel's ruleset through nft, nftables.NewLive, Backlog #121):
//     then config["ruleset_tables"] ("<family>/<name>,...", required) names the
//     tables to manage and config["ruleset_nft"] the nft binary (default "nft").
func DefaultRegistry() *ProviderRegistry {
	r := New()

	// "fake" — file-backed fake provider.
	// config["state"] is the path to the JSON state file.
	r.Register("fake", func(config map[string]string) (provider.Provider, error) {
		statePath := config["state"]
		if statePath == "" {
			statePath = "sakutei-fake-state.json"
		}
		return fakeprovider.New(statePath), nil
	})

	// "dnszone" — RFC1035 zone-file provider (S62dbba-3).
	// config["zone"] is the path to the zone file.
	// config["state"] is accepted as an alias for "zone" so that the common
	// --state flag can be used for both providers without extra CLI plumbing.
	r.Register("dnszone", func(config map[string]string) (provider.Provider, error) {
		zonePath := config["zone"]
		if zonePath == "" {
			// Accept --state as an alias (CLI passes --state for all providers).
			zonePath = config["state"]
		}
		if zonePath == "" {
			zonePath = "sakutei-zone.db"
		}
		mode, err := dnszone.ParseSOASerialMode(config["soa_serial"])
		if err != nil {
			return nil, err
		}
		// Backlog #106, Option A: a fixed scope for this provider instance
		// (default: root scope), set via config["zone_scope"] (CLI --zone-scope).
		scope, err := core.ParseScope(config["zone_scope"])
		if err != nil {
			return nil, err
		}
		return dnszone.New(zonePath, dnszone.WithSOASerial(mode), dnszone.WithScope(scope)), nil
	})

	// "nftables" — nftables ruleset-file provider (Sc7a647-1).
	// config["ruleset"] is the path to the managed ruleset file.
	// config["state"] is accepted as an alias for "ruleset" so the common
	// --state flag can be used for all providers without extra CLI plumbing.
	// Manages regular chains (identity = family/table/chain) whose `rules`
	// field is an ORDERED list; base chains (type/hook/policy) are excluded
	// as a management boundary (§27 FW〈順序付き〉).
	r.Register("nftables", func(config map[string]string) (provider.Provider, error) {
		rulesetPath := config["ruleset"]
		if rulesetPath == "" {
			// Accept --state as an alias (CLI passes --state for all providers).
			rulesetPath = config["state"]
		}
		if rulesetPath == "" {
			rulesetPath = "sakutei-nftables.rules"
		}
		// Backlog #106, Option A: a fixed scope for this provider instance
		// (default: root scope), set via config["ruleset_scope"] (CLI --ruleset-scope).
		scope, err := core.ParseScope(config["ruleset_scope"])
		if err != nil {
			return nil, err
		}
		switch config["ruleset_target"] {
		case "", nftables.TargetFile:
		case nftables.TargetLive:
			// Backlog #121: the kernel's ruleset.  --ruleset / --state are not
			// used; the tables to manage are never guessed.
			if config["ruleset_tables"] == "" {
				return nil, errors.New("nftables: ruleset_target live needs the tables to manage: pass --ruleset-tables <family>/<name>,... (or ruleset_tables in --provider-config)")
			}
			tables, err := nftables.ParseTables(config["ruleset_tables"])
			if err != nil {
				return nil, fmt.Errorf("nftables: ruleset_tables: %w", err)
			}
			return nftables.NewLive(nftables.LiveConfig{NFT: config["ruleset_nft"], Tables: tables}, nftables.WithScope(scope))
		default:
			return nil, fmt.Errorf("nftables: ruleset_target %q: want file or live", config["ruleset_target"])
		}
		return nftables.New(rulesetPath, nftables.WithScope(scope)), nil
	})

	// "redfish-system" / "redfish-virtual-media" — Redfish の BMC provider
	// （§29.1、#83）。config["redfish_config"] は BMC の一覧（--redfish-config）。
	// --state は代わりにならない（BMC のアドレスと資格は推測しない）。
	r.Register(system.Name, func(config map[string]string) (provider.Provider, error) {
		clients, err := redfishClients(config)
		if err != nil {
			return nil, err
		}
		return system.New(clients, system.Options{}), nil
	})
	r.Register(system.MediaName, func(config map[string]string) (provider.Provider, error) {
		clients, err := redfishClients(config)
		if err != nil {
			return nil, err
		}
		return system.NewMedia(clients), nil
	})

	// "sqlmigrate" — SQLite・PostgreSQL のマイグレーション（§29.1、#92・#94・#126）。
	// config["sqlmigrate_config"] はデータベースの一覧（--sqlmigrate-config）。
	// --state は代わりにならない（DB のファイルとマイグレーションの置き場所は
	// 推測しない）。strategy: task 専用：変更は Task の実行体だけが行う。
	r.Register(sqlmigrate.Name, func(config map[string]string) (provider.Provider, error) {
		path := config["sqlmigrate_config"]
		if path == "" {
			return nil, errors.New("the database list is not set: pass --sqlmigrate-config <file>")
		}
		return sqlmigrate.NewFromConfig(path)
	})

	// "s3-bucket" — S3 互換の API のバケット（§29.1、#122）。
	// config["s3_config"] は endpoint の一覧（--s3-config、または
	// --provider-config の s3-bucket の s3_config）。--state は代わりにならない
	// （endpoint と資格は推測しない）。
	r.Register(s3bucket.Name, func(config map[string]string) (provider.Provider, error) {
		path := config["s3_config"]
		if path == "" {
			return nil, errors.New("the endpoint list is not set: pass --s3-config <file> (or s3_config in --provider-config)")
		}
		return s3bucket.NewFromConfig(path)
	})

	// "postgresql" — PostgreSQL のロール・データベース・権限・拡張（§29.1、
	// #125）。config["postgresql_config"] はサーバの一覧（--postgresql-config、
	// または --provider-config の postgresql の postgresql_config）。--state は
	// 代わりにならない（接続先と資格は推測しない）。
	r.Register(postgresql.Name, func(config map[string]string) (provider.Provider, error) {
		path := config["postgresql_config"]
		if path == "" {
			return nil, errors.New("the server list is not set: pass --postgresql-config <file> (or postgresql_config in --provider-config)")
		}
		return postgresql.NewFromConfig(path)
	})

	// "ansible" — Ansible のモジュールを 1 つのホストに流す最小階級の provider
	// （§28.7、§29.1、#127）。config["ansible_config"] は設定（--ansible-config、
	// または --provider-config の ansible の ansible_config）。--state は代わりに
	// ならない（inventory は推測しない）。
	r.Register(ansible.Name, func(config map[string]string) (provider.Provider, error) {
		path := config["ansible_config"]
		if path == "" {
			return nil, errors.New("the ansible settings are not set: pass --ansible-config <file> (or ansible_config in --provider-config)")
		}
		return ansible.NewFromConfig(path)
	})

	return r
}

// redfishClients は config["redfish_config"] の BMC の一覧からノードごとの
// Client を作る。
func redfishClients(config map[string]string) (map[string]redfish.Client, error) {
	path := config["redfish_config"]
	if path == "" {
		return nil, errors.New("the BMC list is not set: pass --redfish-config <file>")
	}
	return bmcconfig.LoadClients(path)
}
