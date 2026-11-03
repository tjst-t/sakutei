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

// Package integration は新しいコアの proxmox provider の統合テスト（v3 段 8d、
// 設計 §6.2 の統合テスト・E-P3・N-P4）。v2 と同じ入れ子の Proxmox VE 9.2（ISO は版と
// sha256 で固定、自動導入の ISO を docker で作り qemu の入れ子の KVM で入れる。
// ISO・自動導入の ISO・導入済みのディスクは v2 と同じキャッシュ SAKUTEI_IT_CACHE、
// 既定 ~/.cache/sakutei-it/proxmox を使い回す）に向けて、v3 の sakutei の CLI を
// 回す：テンプレートから作る・何周しても適合・手の変更が戻る・pending を重ねず
// apply_pending で再起動・触らない VM（external_owners）・届かないと Unknown・
// 宣言の外は消さずに見せる・ciupgrade と NIC の firewall/mtu と VM の FW の options・
// 観測用（PVEAuditor）と書き込み用のトークン・network_policy の実現先 proxmox-fw・
// keep のディスクの付け替え（承認つき）・承認つきの削除・pool。
//
// テストは build tag integration_v3_proxmox の付いたファイルにあり、make check・
// make test には入らない。回すのは make integration-v3-proxmox（KVM・qemu・docker・
// curl・ssh と sudo（パスワードなし）、初回は ISO を取るネットワークが要る）。
// 手順と結果は v3/providers/README.md。
package integration
