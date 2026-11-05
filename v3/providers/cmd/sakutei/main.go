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

// Command sakutei は、新しいコアに次のモジュールを組み込んだ CLI（設計 §6.1 の
// cli.Main(With…)、E-O4・E-S4）。
//   - 実の provider：dnszone・hostconf・nftables・redfish・metrics-probe・netconf・
//     cli-device・guest-os・proxmox・postgresql・sqlmigrate・ldap・s3・ansible・
//     template-file
//   - Expand のモジュール：template・netpolicy（realizer は nftables・proxmox-fw）・compat
//   - secret backend：vault-kv2・command
//   - Events の sink：alertmanager・syslog・metrics・otlp
//
// どれもコアの外のモジュールで、コアは interface だけを知る。
package main

import (
	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/providers/builtin"
)

// Options は組み込むモジュールを返す（試用の手順のテストも同じものを使う）。一覧は
// builtin にある（sakutei-import と同じもの）。
func Options() []cli.Option { return builtin.Options() }

func main() {
	cli.Main(Options()...)
}
