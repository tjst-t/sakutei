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

// Package remote は ssh 越しの実行の層（Backlog #130）の統合テスト。docker で
// sshd・nft・sudo の入った使い捨てのコンテナを 2 台立て（ベースのイメージは
// tag と digest で固定、nft のために NET_ADMIN）、手元の sakutei の CLI
// （reconcile・status）から ssh 越しに 2 台の nftables（live）と dnszone を
// 収束させる。届かない・鍵を外す・ホスト鍵を変えると観測できない（unknown）で
// 何もしないことも確かめる。
//
// テストは build tag integration_remote の付いたファイルにあり、make check・
// make test（go test ./...）には入らない。回すのは make integration-remote
// （docker・ssh・ssh-keygen と sudo（パスワードなし）が要る。初回はイメージを
// 作るので apt のミラーに届くこと）。手順と結果は docs/remote.md の「統合テスト」。
package remote
