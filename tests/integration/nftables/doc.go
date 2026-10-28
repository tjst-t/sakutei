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

// Package nftables は nftables provider の live 版（Backlog #121）の統合テスト。
// 本物の nft を、`sudo -n unshare -n` で作る使い捨ての network namespace の
// 中で回し、sakutei の CLI（plan・reconcile・status）をそこへ向ける。ホストの
// FW には触れない。
//
// base chain の policy drop と締め出しの守り（Backlog #173）は、sshd・nft・sudo の
// 入った使い捨ての docker のコンテナ（自分の network namespace を持つ）に手元から
// ssh で入り、policy drop にして到達を確かめる・ssh を塞ぐ宣言が時間切れで元の
// ruleset に戻る・sakutei が落ちてもホストの側で戻る、を確かめる
// （lockout_integration_test.go）。宣言に書いた確かめる先（sshd を 2 つのポートで
// 待たせた別の経路の ssh）での確かめと、変更の前の確かめ（Backlog #179）も足した。
// 手元の live（確かめは TCP の宛先。設定の宛先と宣言の tcp://）も namespace で
// 確かめる。
//
// 網の間の許可（network_policy:、Backlog #174）から展開した base chain を
// namespace の nft に流し、reason の comment が読み戻しで drift にならないことを
// 確かめる（netpolicy_integration_test.go）。
//
// テストは build tag integration_nftables の付いたファイルにあり、make check・
// make test（go test ./...）には入らない。回すのは make integration-nftables
// （nft・unshare・nsenter・docker・ssh・ssh-keygen と sudo（パスワードなし）が要る）。手順と結果は
// docs/providers/nftables.md の「統合テスト」。
package nftables
