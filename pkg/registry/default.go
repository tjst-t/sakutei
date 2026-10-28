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
	"net"
	"path"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/ansible"
	"github.com/tjst-t/sakutei/v2/internal/clidevice"
	"github.com/tjst-t/sakutei/v2/internal/dnszone"
	"github.com/tjst-t/sakutei/v2/internal/fakeprovider"
	"github.com/tjst-t/sakutei/v2/internal/guestos"
	"github.com/tjst-t/sakutei/v2/internal/hostconf"
	"github.com/tjst-t/sakutei/v2/internal/ldap"
	"github.com/tjst-t/sakutei/v2/internal/metricsprobe"
	"github.com/tjst-t/sakutei/v2/internal/netconf"
	"github.com/tjst-t/sakutei/v2/internal/nftables"
	"github.com/tjst-t/sakutei/v2/internal/postgresql"
	"github.com/tjst-t/sakutei/v2/internal/proxmox"
	"github.com/tjst-t/sakutei/v2/internal/redfish"
	"github.com/tjst-t/sakutei/v2/internal/redfish/bmcconfig"
	"github.com/tjst-t/sakutei/v2/internal/redfish/system"
	"github.com/tjst-t/sakutei/v2/internal/remote"
	"github.com/tjst-t/sakutei/v2/internal/render"
	"github.com/tjst-t/sakutei/v2/internal/s3bucket"
	"github.com/tjst-t/sakutei/v2/internal/sqlmigrate"
	"github.com/tjst-t/sakutei/v2/internal/templatefile"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
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
//
//   - Remote hosts (Backlog #130): config["remote_config"] is the host list
//     (remote.LoadConfig: address, user, private_key SecretRef, known_hosts,
//     sudo, timeouts).  nftables live with config["ruleset_hosts"]
//     ("fw1,fw2") runs nft on those hosts over ssh; dnszone with
//     config["zone_hosts"] manages config["zone"] (absolute) on those hosts.
//     The local ids then start with "<host>/".  config["zone_reload"] is the
//     command run after an Apply changed the zone (here, or on the host).
//
//   - "guest-os": observes the guest OS of the hosts config["guest_hosts"]
//     ("node1,node2", from config["remote_config"]) over ssh: reachability,
//     /etc/os-release and the markers config["guest_markers"]
//     ("installed=/etc/sakutei-installed,...").  Observe only (Backlog #86):
//     a Workflow's wait / require uses it (resource: guest-os/{target}).
//
//   - "netconf": observes subtrees of the running configuration of NETCONF
//     devices (Backlog #141): config["netconf_hosts"] ("sw1,sw2", from
//     config["remote_config"]) over the ssh "netconf" subsystem, the subtrees
//     named in config["netconf_subtrees"] (name → subtree filter).  Observe
//     only (Capabilities.ObserveOnly, §23.4) unless config["netconf_apply"] is
//     "true" (Backlog #175): then it declares CommitConfirm and applies in two
//     steps with commit confirmed (config["netconf_confirm_timeout"], default
//     2m; config["netconf_commit_flow"] "1.0" (default) or "1.1" for persist).
//
//   - "ldap": entries of LDAP directories (Backlog #144), only the declared
//     attributes, through the OpenLDAP clients ldapsearch / ldapmodify.
//     config["ldap_config"] is the server list (ldap.LoadConfig: uri, StartTLS,
//     CA, bind DN and its SecretRef, base DN, filter).
//
//   - "template-file": writes files rendered from the user's text/template
//     templates (config["template_dir"]) into config["output_dir"], e.g. the
//     answer files of an automated OS install (Backlog #139).  Minimal tier:
//     observed by comparing the file with the template rendered from the
//     desired.
//
//   - "host-file" / "systemd-unit": files and systemd units (incl. podman
//     quadlet) on the hosts config["file_hosts"] / config["unit_hosts"]
//     ("web1,web2" from config["remote_config"]; "local" is this machine)
//     (Backlog #155).  The local ids are "<host>/<absolute path>" and
//     "<host>/<unit file name>".  config["stamp_dir"] is where the stamps live
//     on the hosts (default /var/lib/sakutei); config["unit_dir"] /
//     config["quadlet_dir"] where the unit files are; config["secret_store"]
//     (or the remote config's secret_store) resolves content_secret.
//
//   - "proxmox-vm": the VMs of Proxmox VE clusters through the REST API
//     (Backlog #143).  config["proxmox_config"] is the cluster list
//     (proxmox.LoadConfig: endpoint, API token id and its secret as a
//     SecretRef, default node, scope, the VMs never to touch).
//
//   - "metrics-probe": observes values of /metrics (the Prometheus text
//     format) over HTTP(S) (Backlog #149).  config["metrics_probe_config"] is
//     the endpoint and probe list (metricsprobe.LoadConfig: url, TLS, the
//     credential as a SecretRef, metric name, label match, sum / max and the
//     comparisons such as "< 1000").  Observe only (Capabilities.ObserveOnly):
//     a Workflow's require and ready_condition read it.
//
//   - "cli-device": devices that can only be configured through their CLI
//     (Backlog #159).  config["cli_hosts"] names the devices ("sw1,sw2" from
//     config["remote_config"], over ssh); config["cli_definition"] is the
//     device definition (clidevice.LoadDefinition: how to observe each item,
//     how to read the output, the commands that change it, the error
//     markers) for every device, and config["cli_definitions"]
//     ("sw1=a.yaml,sw2=b.yaml") overrides it per device.  The local ids are
//     "<device>/<item>".  config["secret_store"] (or the remote config's
//     secret_store) resolves the secret fields.
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
		opts := []dnszone.Option{dnszone.WithSOASerial(mode), dnszone.WithScope(scope)}
		// Backlog #130: the reload command (zone_reload, words split on
		// spaces, no shell) and the remote hosts (zone_hosts + remote_config).
		if r := strings.Fields(config["zone_reload"]); len(r) > 0 {
			opts = append(opts, dnszone.WithReload(r))
		}
		if config["zone_hosts"] != "" {
			if config["zone"] == "" || !path.IsAbs(config["zone"]) {
				return nil, errors.New("dnszone: zone_hosts needs zone: the absolute path of the zone file on the hosts")
			}
			execs, order, err := remoteHosts(config, "zone_hosts")
			if err != nil {
				return nil, fmt.Errorf("dnszone: %w", err)
			}
			opts = append(opts, dnszone.WithHosts(order, execs))
			zonePath = config["zone"]
		}
		return dnszone.New(zonePath, opts...), nil
	})

	// "nftables" — nftables ruleset-file provider (Sc7a647-1).
	// config["ruleset"] is the path to the managed ruleset file.
	// config["state"] is accepted as an alias for "ruleset" so the common
	// --state flag can be used for all providers without extra CLI plumbing.
	// Manages regular chains (identity = family/table/chain) whose `rules`
	// field is an ORDERED list; base chains (type/hook/policy) are excluded
	// as a management boundary (§27 FW〈順序付き〉) unless the intent declares
	// one (type/hook/priority/policy, Backlog #173; a live change to one goes
	// through the lockout guard: ruleset_rollback_timeout, and for the local
	// nft ruleset_confirm_address).
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
		opts := []nftables.Option{nftables.WithScope(scope)}
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
			cfg := nftables.LiveConfig{NFT: config["ruleset_nft"], Tables: tables}
			// Backlog #173: the lockout guard of a change to a base chain.
			if v := config["ruleset_rollback_timeout"]; v != "" {
				d, err := time.ParseDuration(v)
				if err != nil || d < time.Second {
					return nil, fmt.Errorf("nftables: ruleset_rollback_timeout %q: want a duration of 1s or more (e.g. 30s)", v)
				}
				cfg.RollbackAfter = d
			}
			// The local nft's guard: "self" is confirmed by a new TCP
			// connection to ruleset_confirm_address (none: only the confirm
			// targets the intent declares, tcp://host:port, Backlog #179).
			local := nftables.ExecGuard{Exec: remote.Local{}, NFT: config["ruleset_nft"]}
			if addr := config["ruleset_confirm_address"]; addr != "" {
				if _, _, err := net.SplitHostPort(addr); err != nil {
					return nil, fmt.Errorf("nftables: ruleset_confirm_address %q: want host:port", addr)
				}
				if config["ruleset_hosts"] != "" {
					return nil, errors.New("nftables: ruleset_confirm_address is for the local nft; a remote host is confirmed over a new ssh connection")
				}
				local.Probe = nftables.TCPProbe(addr)
			}
			cfg.Guard = local
			// Backlog #130: the remote hosts (ruleset_hosts + remote_config).
			if config["ruleset_hosts"] != "" {
				execs, order, err := remoteHosts(config, "ruleset_hosts")
				if err != nil {
					return nil, fmt.Errorf("nftables: %w", err)
				}
				for _, name := range order {
					probe, err := nftables.SSHProbe(execs[name])
					if err != nil {
						return nil, fmt.Errorf("nftables: %w", err)
					}
					cfg.Hosts = append(cfg.Hosts, nftables.LiveHost{
						Name:   name,
						Runner: nftables.RemoteRunner{Exec: execs[name], NFT: config["ruleset_nft"]},
						Guard:  nftables.ExecGuard{Exec: execs[name], NFT: config["ruleset_nft"], Probe: probe},
					})
				}
			}
			return nftables.NewLive(cfg, opts...)
		default:
			return nil, fmt.Errorf("nftables: ruleset_target %q: want file or live", config["ruleset_target"])
		}
		if config["ruleset_hosts"] != "" {
			return nil, errors.New("nftables: ruleset_hosts needs ruleset_target live")
		}
		return nftables.New(rulesetPath, opts...), nil
	})

	// "redfish-system" / "redfish-virtual-media" / "redfish-bios" /
	// "redfish-firmware" — Redfish の BMC provider（§29.1、#83・#140・#148）。config["redfish_config"] は BMC の一覧（--redfish-config）。
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
	r.Register(system.BiosName, func(config map[string]string) (provider.Provider, error) {
		clients, err := redfishClients(config)
		if err != nil {
			return nil, err
		}
		return system.NewBios(clients), nil
	})
	// "redfish-firmware" — FW の版の観測と、多相 Task での更新（§29.1、#148）。
	// イメージの資格（image_password の SecretRef）は BMC の一覧の secret_store から。
	r.Register(system.FirmwareName, func(config map[string]string) (provider.Provider, error) {
		path := config["redfish_config"]
		if path == "" {
			return nil, errors.New("the BMC list is not set: pass --redfish-config <file>")
		}
		clients, secrets, err := bmcconfig.LoadClientsAndSecrets(path)
		if err != nil {
			return nil, err
		}
		return system.NewFirmware(clients, system.FirmwareOptions{Secrets: secrets}), nil
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
		if config["s3_config"] == "" {
			return nil, errors.New("the endpoint list is not set: pass --s3-config <file> (or s3_config in --provider-config)")
		}
		path, tree, err := s3Ranges.Tree(config)
		if err != nil {
			return nil, err
		}
		return s3bucket.NewFromConfigTree(path, tree)
	})

	// "postgresql" — PostgreSQL のロール・データベース・権限・拡張（§29.1、
	// #125）。config["postgresql_config"] はサーバの一覧（--postgresql-config、
	// または --provider-config の postgresql の postgresql_config）。--state は
	// 代わりにならない（接続先と資格は推測しない）。
	r.Register(postgresql.Name, func(config map[string]string) (provider.Provider, error) {
		if config["postgresql_config"] == "" {
			return nil, errors.New("the server list is not set: pass --postgresql-config <file> (or postgresql_config in --provider-config)")
		}
		path, tree, err := postgresqlRanges.Tree(config)
		if err != nil {
			return nil, err
		}
		return postgresql.NewFromConfigTree(path, tree)
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

	// "guest-os" — ゲスト OS の観測だけ（§29.1、#86）。config["guest_hosts"] の
	// ホストを config["remote_config"] の対象の一覧から ssh 越しに読む。
	// config["guest_markers"] は導入の目印（"<名前>=<パス>,..."）。
	r.Register(guestos.Name, func(config map[string]string) (provider.Provider, error) {
		if config["guest_hosts"] == "" {
			return nil, errors.New("guest-os: the hosts to observe are not set: providers.guest-os.hosts in the intent (or guest_hosts in --provider-config), and remote_config in --provider-config")
		}
		markers, err := guestos.ParseMarkers(config["guest_markers"])
		if err != nil {
			return nil, fmt.Errorf("guest-os: guest_markers: %w", err)
		}
		execs, _, err := remoteHosts(config, "guest_hosts")
		if err != nil {
			return nil, fmt.Errorf("guest-os: %w", err)
		}
		return guestos.New(execs, markers)
	})

	// "netconf" — NETCONF の機器の設定の部分木の観測だけ（§23.4、§29.1、#141）。
	// config["netconf_hosts"] の機器に config["remote_config"] の ssh で netconf
	// サブシステムを開き、config["netconf_subtrees"]（部分木の一覧）の filter で
	// running を読む。既定は Apply しない（Capabilities.ObserveOnly）。
	// config["netconf_apply"] が "true" のときだけ commit confirmed の 2 段で当てる
	// （#175）: netconf_confirm_timeout（既定 2m）・netconf_commit_flow（"1.0" が
	// 既定、"1.1" で persist の流れ）。
	r.Register(netconf.Name, func(config map[string]string) (provider.Provider, error) {
		if config["netconf_hosts"] == "" {
			return nil, errors.New("netconf: the devices are not set: providers.netconf.hosts in the intent (or netconf_hosts in --provider-config), and remote_config in --provider-config")
		}
		subtrees, err := netconfSubtrees(config)
		if err != nil {
			return nil, err
		}
		execs, _, err := remoteHosts(config, "netconf_hosts")
		if err != nil {
			return nil, fmt.Errorf("netconf: %w", err)
		}
		dialers, err := netconf.SSHDialers(execs)
		if err != nil {
			return nil, err
		}
		p, err := netconf.New(dialers, subtrees)
		if err != nil {
			return nil, err
		}
		ac, allow, err := netconfApplyConfig(config)
		if err != nil {
			return nil, err
		}
		if allow {
			if err := p.AllowApply(ac); err != nil {
				return nil, err
			}
		}
		return p, nil
	})

	// "ldap" — LDAP のディレクトリの項目（§29.1、#144）。config["ldap_config"] は
	// サーバの一覧（--provider-config の ldap の ldap_config）。--state は代わりに
	// ならない（接続先と資格は推測しない）。
	r.Register(ldap.Name, func(config map[string]string) (provider.Provider, error) {
		if config["ldap_config"] == "" {
			return nil, errors.New("ldap: the server list is not set: ldap_config in --provider-config")
		}
		path, tree, err := ldapRanges.Tree(config)
		if err != nil {
			return nil, err
		}
		return ldap.NewFromConfigTree(path, tree)
	})

	// "template-file" — 雛形（text/template）に対象ごとの値を入れたファイルを
	// 書き出す（§29.1、#139。OS の自動導入の応答ファイルなど）。
	// config["template_dir"] は雛形の置き場、config["output_dir"] は書き出す先。
	r.Register(templatefile.Name, func(config map[string]string) (provider.Provider, error) {
		return templatefile.New(config["template_dir"], config["output_dir"])
	})

	// "host-file" — 離れたホスト（または手元）のファイル（§29.1、#155）。
	// config["file_hosts"] のホストを config["remote_config"] の対象の一覧から
	// ssh 越しに扱う（"local" は手元）。
	r.Register(hostconf.FileName, func(config map[string]string) (provider.Provider, error) {
		execs, cfg, err := hostExecutors(config, "file_hosts")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", hostconf.FileName, err)
		}
		opts := hostconf.FileOptions{StampDir: config["stamp_dir"]}
		store := config["secret_store"]
		if store == "" {
			store = cfg.SecretStore
		}
		if store != "" {
			opts.Secrets = render.NewFileSecretBackend(store)
		}
		return hostconf.NewFile(execs, opts)
	})

	// "systemd-unit" — 離れたホスト（または手元）の systemd のユニットと podman の
	// quadlet（§29.1、#155）。config["unit_hosts"] のホストを同じく扱う。
	r.Register(hostconf.UnitName, func(config map[string]string) (provider.Provider, error) {
		execs, _, err := hostExecutors(config, "unit_hosts")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", hostconf.UnitName, err)
		}
		return hostconf.NewUnit(execs, hostconf.UnitOptions{
			StampDir: config["stamp_dir"], UnitDir: config["unit_dir"], QuadletDir: config["quadlet_dir"],
		})
	})

	// "cli-device" — CLI でしか設定できない装置（§29.1、#159）。config["cli_hosts"]
	// の装置に config["remote_config"] の ssh でつなぎ、機種の定義
	// （config["cli_definition"]、装置ごとの上書きは config["cli_definitions"]）に
	// 従って CLI を打つ。
	r.Register(clidevice.Name, func(config map[string]string) (provider.Provider, error) {
		if config["cli_hosts"] == "" {
			return nil, errors.New("cli-device: the devices are not set: providers.cli-device.hosts in the intent (or cli_hosts in --provider-config), and remote_config in --provider-config")
		}
		defs, err := cliDefinitions(config)
		if err != nil {
			return nil, err
		}
		execs, order, err := remoteHosts(config, "cli_hosts")
		if err != nil {
			return nil, fmt.Errorf("cli-device: %w", err)
		}
		hosts := map[string]clidevice.Host{}
		for _, n := range order {
			conn, ok := execs[n].(clidevice.Conn)
			if !ok {
				return nil, fmt.Errorf("cli-device: device %q cannot be reached through ssh", n)
			}
			d := defs[n]
			if d == nil {
				d = defs[""]
			}
			if d == nil {
				return nil, fmt.Errorf("cli-device: device %q has no device definition: cli_definition (or cli_definitions %s=<file>)", n, n)
			}
			hosts[n] = clidevice.Host{Conn: conn, Def: d}
		}
		for n := range defs {
			if _, ok := hosts[n]; n != "" && !ok {
				return nil, fmt.Errorf("cli-device: cli_definitions names %q, which is not in cli_hosts", n)
			}
		}
		opts := clidevice.Options{}
		store := config["secret_store"]
		if store == "" && config["remote_config"] != "" {
			cfg, err := remote.LoadConfig(config["remote_config"])
			if err != nil {
				return nil, err
			}
			store = cfg.SecretStore
		}
		if store != "" {
			opts.Secrets = render.NewFileSecretBackend(store)
		}
		return clidevice.New(hosts, opts)
	})

	// "proxmox-vm" — Proxmox VE の VM（§29.1、#143）。config["proxmox_config"] は
	// クラスタの一覧（--provider-config の proxmox-vm の proxmox_config）。--state
	// は代わりにならない（endpoint と資格は推測しない）。
	// クラスタの scope は intent の providers.proxmox-vm.clusters.<名前>.scope
	// にも書ける（§7.7、#181、registerIntentRanges）。
	r.Register(proxmox.Name, func(config map[string]string) (provider.Provider, error) {
		return proxmox.NewFromProviderConfig(config)
	})

	// "metrics-probe" — /metrics（Prometheus の text 形式）の値の観測だけ（§29.1、
	// #149）。config["metrics_probe_config"] は endpoint と probe の一覧
	// （--provider-config の metrics-probe の metrics_probe_config）。
	r.Register(metricsprobe.Name, func(config map[string]string) (provider.Provider, error) {
		path := config["metrics_probe_config"]
		if path == "" {
			return nil, errors.New("metrics-probe: the probe list is not set: metrics_probe_config in --provider-config")
		}
		return metricsprobe.NewFromConfig(path)
	})

	registerIntentRanges(r)
	return r
}

// remoteHosts は config[key]（"a,b,..."）の対象の Executor を
// config["remote_config"]（対象の一覧、Backlog #130）から作る。
func remoteHosts(config map[string]string, key string) (map[string]remote.Executor, []string, error) {
	path := config["remote_config"]
	if path == "" {
		return nil, nil, fmt.Errorf("%s needs remote_config: the host list (address, user, private_key, known_hosts, ...)", key)
	}
	cfg, err := remote.LoadConfig(path)
	if err != nil {
		return nil, nil, err
	}
	execs, order, err := cfg.Executors(config[key])
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", key, err)
	}
	return execs, order, nil
}

// hostExecutors は config[key]（"a,b,..."）の対象の Executor を作る。名前
// "local"（hostconf.LocalHost）は手元で、remote_config に無くてよい（remote_config に
// 同じ名前があればそちらを使う）。ほかの名前は remote_config から（#130）。
func hostExecutors(config map[string]string, key string) (hostconf.Hosts, remote.Config, error) {
	var names []string
	for _, n := range strings.Split(config[key], ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return nil, remote.Config{}, fmt.Errorf("the hosts are not set: %s (and remote_config) in --provider-config; %q is this machine", key, hostconf.LocalHost)
	}
	var cfg remote.Config
	if path := config["remote_config"]; path != "" {
		var err error
		if cfg, err = remote.LoadConfig(path); err != nil {
			return nil, cfg, err
		}
	}
	out := hostconf.Hosts{}
	var remoteNames []string
	for _, n := range names {
		if _, dup := out[n]; dup {
			return nil, cfg, fmt.Errorf("%s: host %q is listed twice", key, n)
		}
		if _, ok := cfg.Hosts[n]; n == hostconf.LocalHost && !ok {
			out[n] = remote.Local{}
			continue
		}
		out[n] = nil
		remoteNames = append(remoteNames, n)
	}
	if len(remoteNames) > 0 {
		if config["remote_config"] == "" {
			return nil, cfg, fmt.Errorf("%s needs remote_config: the host list (address, user, private_key, known_hosts, ...)", key)
		}
		execs, _, err := cfg.Executors(strings.Join(remoteNames, ","))
		if err != nil {
			return nil, cfg, fmt.Errorf("%s: %w", key, err)
		}
		for n, e := range execs {
			out[n] = e
		}
	}
	return out, cfg, nil
}

// cliDefinitions は cli-device の機種の定義を読む: "" は cli_definition（全部の
// 装置の既定）、ほかは cli_definitions（"<装置>=<ファイル>,..."）の装置ごと。
// 同じファイルは 1 回だけ読む。
func cliDefinitions(config map[string]string) (map[string]*clidevice.Definition, error) {
	out := map[string]*clidevice.Definition{}
	loaded := map[string]*clidevice.Definition{}
	load := func(path string) (*clidevice.Definition, error) {
		if d, ok := loaded[path]; ok {
			return d, nil
		}
		d, err := clidevice.LoadDefinition(path)
		if err != nil {
			return nil, err
		}
		loaded[path] = d
		return d, nil
	}
	if path := strings.TrimSpace(config["cli_definition"]); path != "" {
		d, err := load(path)
		if err != nil {
			return nil, err
		}
		out[""] = d
	}
	for _, pair := range strings.Split(config["cli_definitions"], ",") {
		if pair = strings.TrimSpace(pair); pair == "" {
			continue
		}
		name, path, ok := strings.Cut(pair, "=")
		name, path = strings.TrimSpace(name), strings.TrimSpace(path)
		if !ok || name == "" || path == "" {
			return nil, fmt.Errorf("cli-device: cli_definitions: %q: want <device>=<file>", pair)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("cli-device: cli_definitions: device %q is listed twice", name)
		}
		d, err := load(path)
		if err != nil {
			return nil, err
		}
		out[name] = d
	}
	if len(out) == 0 {
		return nil, errors.New("cli-device: the device definition is not set: cli_definition (or cli_definitions) in --provider-config")
	}
	return out, nil
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

// netconfApplyConfig は netconf の 2 段の Apply の設定を読む（#175）。
// netconf_apply が "true" でなければ許さない（"false"・無いは観測だけ。ほかの
// 値はエラー）。Apply を許さないのに Apply の設定だけがあるのもエラー。
func netconfApplyConfig(config map[string]string) (netconf.ApplyConfig, bool, error) {
	var ac netconf.ApplyConfig
	allow := false
	switch v := config["netconf_apply"]; v {
	case "true":
		allow = true
	case "", "false":
	default:
		return ac, false, fmt.Errorf("netconf: netconf_apply %q: want \"true\" or \"false\"", v)
	}
	if v := config["netconf_confirm_timeout"]; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return ac, false, fmt.Errorf("netconf: netconf_confirm_timeout %q: want a duration such as \"2m\"", v)
		}
		ac.ConfirmTimeout = d
	}
	switch v := config["netconf_commit_flow"]; v {
	case "", "1.0":
	case "1.1":
		ac.Persist = true
	default:
		return ac, false, fmt.Errorf("netconf: netconf_commit_flow %q: want \"1.0\" or \"1.1\"", v)
	}
	if !allow && (config["netconf_confirm_timeout"] != "" || config["netconf_commit_flow"] != "") {
		return ac, false, errors.New("netconf: netconf_confirm_timeout / netconf_commit_flow are set but netconf_apply is not \"true\" (the provider observes only)")
	}
	return ac, allow, nil
}
