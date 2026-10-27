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

// Package netconf は netconf provider（Backlog #141、観測だけ）の統合テスト。
// docker で公開の NETCONF サーバ（netopeer2 のコンテナ。イメージは tag と
// digest で固定）を 127.0.0.1 の空いたポートに立て、手元の sakutei の CLI
// （plan・reconcile・status）から ssh の netconf サブシステムで設定の部分木を
// 観測する: 宣言どおりなら適合、別の接続（テストが直に送る edit-config）で手で
// 変えると drift が知らされ、Apply は出ない（機器は手で変えたまま）、何周しても
// ずれが出ない、止めると unknown（観測できない）。
//
// テストは build tag integration_netconf の付いたファイルにあり、make check・
// make test（go test ./...）には入らない。回すのは make integration-netconf
// （docker・ssh・ssh-keygen と sudo（パスワードなし）が要る）。コンテナは
// 終われば（落ちても）消す。手順と結果は docs/providers/netconf.md の「統合テスト」。
package netconf
