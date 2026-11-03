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

// Command sakutei は新しいコアに実の provider（dnszone・hostconf・nftables・
// redfish・metrics-probe・netconf・cli-device・guest-os・proxmox・postgresql・
// sqlmigrate・ldap）と
// Expand のモジュール（template・netpolicy（実現先 nftables・proxmox-fw）・compat）と秘密の保管先（vault-kv2・
// command）と Events の送り先（alertmanager・syslog・metrics・otlp）を組み込んだ CLI（設計 §6.1 の cli.Main(With…)、E-O4・E-S4）。どれも
// コアの外のモジュールで、コアはインターフェースだけを知る。
package main

import (
	"github.com/tjst-t/sakutei/v3/cli"
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
	"github.com/tjst-t/sakutei/v3/providers/secretbackend"
	"github.com/tjst-t/sakutei/v3/providers/sinks/alertmanager"
	"github.com/tjst-t/sakutei/v3/providers/sinks/metrics"
	"github.com/tjst-t/sakutei/v3/providers/sinks/otlp"
	"github.com/tjst-t/sakutei/v3/providers/sinks/syslog"
	"github.com/tjst-t/sakutei/v3/providers/sqlmigrate"
	"github.com/tjst-t/sakutei/v3/sdk/fake"
)

// Options は組み込むモジュール（試用の手順のテストも同じものを使う）。
func Options() []cli.Option {
	return []cli.Option{
		cli.WithModule("dnszone", dnszone.Module),
		cli.WithModule("hostconf", hostconf.Module),
		cli.WithModule("nftables", nftables.Module),
		cli.WithModule("redfish", redfish.Module),
		cli.WithModule("proxmox", proxmox.Module),
		cli.WithModule("metrics-probe", metricsprobe.Module),
		cli.WithModule("netconf", netconf.Module),
		cli.WithModule("cli-device", clidevice.Module),
		cli.WithModule("guest-os", guestos.Module),
		cli.WithModule("postgresql", postgresql.Module),
		cli.WithModule("sqlmigrate", sqlmigrate.Module),
		cli.WithModule("ldap", ldap.Module),
		cli.WithModule("fake", fake.Module),
		cli.WithExpander("template", template.Expander{}),
		cli.WithExpander("netpolicy", netpolicy.New(netpolicy.WithRealizer(proxmox.RealizerName, proxmox.Realizer{}))),
		cli.WithExpander("compat", compat.Expander{}),
		cli.WithSecretBackend(secretbackend.VaultKV2, secretbackend.VaultModule),
		cli.WithSecretBackend(secretbackend.CommandName, secretbackend.CommandModule),
		cli.WithSink("alertmanager", alertmanager.Module),
		cli.WithSink("syslog", syslog.Module),
		cli.WithSink("metrics", metrics.Module),
		cli.WithSink("otlp", otlp.Module),
	}
}

func main() {
	cli.Main(Options()...)
}
