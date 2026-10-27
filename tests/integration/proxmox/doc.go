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

// Package proxmox は proxmox-vm provider（Backlog #143）の統合テスト。本物の
// Proxmox VE（版を固定した ISO を、Proxmox の proxmox-auto-install-assistant で
// 自動導入の ISO にしたもの）を入れ子の KVM（qemu）で動かし、手元の sakutei の
// CLI（plan・reconcile・status）から REST API で VM を収束させる: テンプレート
// （Debian 13 のクラウドイメージ、版と sha512 で固定）から作る → 適合し、
// cloud-init の鍵とアドレスで ssh で入れる、手でメモリを変える → 戻る（走って
// いる VM なので pending を経る）、再起動が要る変更は pending で止まり重ねて出ない（apply_pending: reboot で効く）、
// 触らない VM に触れない（宣言すれば計画のエラー）、何周しても drift が出ない、
// 届かない（qemu を止める）と unknown、消す。
//
// テストは build tag integration_proxmox の付いたファイルにあり、make check・
// make test（go test ./...）には入らない。回すのは make integration-proxmox
// （KVM・qemu・docker と sudo（パスワードなし）・初回は ISO を取るネットワークが
// 要る）。ISO・自動導入の ISO・導入済みのディスクは SAKUTEI_IT_CACHE（既定
// ~/.cache/sakutei-it/proxmox）に残して使い回し、テストは導入済みのディスクの
// 上の使い捨ての overlay で起動する。手順と結果は docs/providers/proxmox-vm.md の
// 「統合テスト」。
package proxmox
