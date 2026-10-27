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

// Package guestos はゲスト OS の観測（guest-os provider）で OS 導入の完了を
// 確かめる統合テスト（Backlog #86・#139、§29.1）。qemu-bmc 0.10.0 を containerlab で
// 3 台立て、Alpine の自動インストールの ISO（docs/trial/redfish-os-install/make-iso.sh）
// で本当に OS を入れ、常駐の sakutei の Workflow が「ゲスト OS が目印つきで起動
// した」で先へ進む・インストーラが失敗したときは進まないことを示す。#139 で、
// 起動先を 1 回だけ CD にする手順（action boot_once・reset）と、宣言から書き出す
// 応答ファイル（template-file）で 1 つの ISO を 3 台に使う形にし、起動先の上書きが
// 残らないことも確かめる。
//
// テストは build tag integration_guestos の付いたファイルにあり、make check・
// make test（go test ./...）には入らない。回すのは make integration-guestos
// （KVM・docker・containerlab・sudo と、初回は ISO とパッケージを取るネットワーク
// が要る）。手順と結果は docs/providers/guest-os.md の「統合テスト」。
package guestos
