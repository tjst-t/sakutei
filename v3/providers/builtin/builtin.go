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

// Package builtin は sakutei と sakutei-import に組み込むモジュールの一覧
// （provider・Expand・secret backend・Events の sink）。どちらのツールも同じ
// 接続設定（config ファイル）を読むので、一覧を 1 か所に置く（E-O4・E-S4）。
package builtin

import (
	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/expand"
	"github.com/tjst-t/sakutei/v3/load"
	"github.com/tjst-t/sakutei/v3/providers/ansible"
	"github.com/tjst-t/sakutei/v3/providers/clidevice"
	"github.com/tjst-t/sakutei/v3/providers/dnszone"
	"github.com/tjst-t/sakutei/v3/providers/expanders/compat"
	"github.com/tjst-t/sakutei/v3/providers/expanders/netpolicy"
	"github.com/tjst-t/sakutei/v3/providers/expanders/template"
	"github.com/tjst-t/sakutei/v3/providers/guestos"
	"github.com/tjst-t/sakutei/v3/providers/hostconf"
	"github.com/tjst-t/sakutei/v3/providers/ldap"
	"github.com/tjst-t/sakutei/v3/providers/metricsprobe"
	"github.com/tjst-t/sakutei/v3/providers/netconf"
	"github.com/tjst-t/sakutei/v3/providers/nftables"
	"github.com/tjst-t/sakutei/v3/providers/postgresql"
	"github.com/tjst-t/sakutei/v3/providers/proxmox"
	"github.com/tjst-t/sakutei/v3/providers/redfish"
	"github.com/tjst-t/sakutei/v3/providers/s3"
	"github.com/tjst-t/sakutei/v3/providers/secretbackend"
	"github.com/tjst-t/sakutei/v3/providers/sinks/alertmanager"
	"github.com/tjst-t/sakutei/v3/providers/sinks/metrics"
	"github.com/tjst-t/sakutei/v3/providers/sinks/otlp"
	"github.com/tjst-t/sakutei/v3/providers/sinks/syslog"
	"github.com/tjst-t/sakutei/v3/providers/sqlmigrate"
	"github.com/tjst-t/sakutei/v3/providers/templatefile"
	"github.com/tjst-t/sakutei/v3/sdk/fake"
)

// Modules は provider のモジュール（接続設定の providers.<名前>.module）。
func Modules() map[string]load.Module {
	return map[string]load.Module{
		"dnszone":       dnszone.Module,
		"hostconf":      hostconf.Module,
		"nftables":      nftables.Module,
		"redfish":       redfish.Module,
		"proxmox":       proxmox.Module,
		"metrics-probe": metricsprobe.Module,
		"netconf":       netconf.Module,
		"cli-device":    clidevice.Module,
		"guest-os":      guestos.Module,
		"postgresql":    postgresql.Module,
		"sqlmigrate":    sqlmigrate.Module,
		"ldap":          ldap.Module,
		"template-file": templatefile.Module,
		"ansible":       ansible.Module,
		"s3":            s3.Module,
		"fake":          fake.Module,
	}
}

// Expanders は Go で組み込んだ Expand（intent の expand: の go:<名前>）。
func Expanders() map[string]expand.Expander {
	return map[string]expand.Expander{
		"template":  template.Expander{},
		"netpolicy": netpolicy.New(netpolicy.WithRealizer(proxmox.RealizerName, proxmox.Realizer{})),
		"compat":    compat.Expander{},
	}
}

// SecretBackends は secret backend のモジュール（file はコアが初めから持つ）。
func SecretBackends() map[string]load.SecretModule {
	return map[string]load.SecretModule{
		secretbackend.VaultKV2:    secretbackend.VaultModule,
		secretbackend.CommandName: secretbackend.CommandModule,
	}
}

// Sinks は Events の sink のモジュール。
func Sinks() map[string]load.SinkModule {
	return map[string]load.SinkModule{
		"alertmanager": alertmanager.Module,
		"syslog":       syslog.Module,
		"metrics":      metrics.Module,
		"otlp":         otlp.Module,
	}
}

// Options は上の全部を cli.Main に渡す形にする。
func Options() []cli.Option {
	var opts []cli.Option
	for n, m := range Modules() {
		opts = append(opts, cli.WithModule(n, m))
	}
	for n, e := range Expanders() {
		opts = append(opts, cli.WithExpander(n, e))
	}
	for n, m := range SecretBackends() {
		opts = append(opts, cli.WithSecretBackend(n, m))
	}
	for n, m := range Sinks() {
		opts = append(opts, cli.WithSink(n, m))
	}
	return opts
}
