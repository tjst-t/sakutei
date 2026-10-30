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
// 届かない（qemu を止める）と unknown、宣言から外した VM が status の unmanaged に
// 出て（宣言が 0 件でも。触らない VM とテンプレートは出ない）消されない、消す、
// scope 付きの宣言と --claim で API で直接作った VM だけが unmanaged に出る（#169）、
// ciupgrade: false で作った VM の設定が 0 で、手で変えると戻る（#170）、
// NIC の firewall・mtu と VM の FW の options が書いたときだけ合わさり、手で変えると戻る（#196）、
// network_policy の実現先 proxmox-fw の許可が VM の FW の規則に口と向き（入りと出）で入り、comment に
// reason が出て、手の規則は消さずに status の unmanaged に出る（#197）、
// 作り直しで keep のデータのディスクを新しい VM に付け替えて中身が残り、keep でない
// ディスクは前の VM と一緒に消え、外したままの volume が unmanaged に出て、承認が
// 無ければ付け替えない（#204）。
//
// テストは build tag integration_proxmox の付いたファイルにあり、make check・
// make test（go test ./...）には入らない。回すのは make integration-proxmox
// （KVM・qemu・docker と sudo（パスワードなし）・初回は ISO を取るネットワークが
// 要る）。ISO・自動導入の ISO・導入済みのディスクは SAKUTEI_IT_CACHE（既定
// ~/.cache/sakutei-it/proxmox）に残して使い回し、テストは導入済みのディスクの
// 上の使い捨ての overlay で起動する。手順と結果は docs/providers/proxmox-vm.md の
// 「統合テスト」。
package proxmox
