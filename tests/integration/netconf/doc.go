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

// Package netconf は netconf provider（Backlog #141 の観測、#175 の 2 段の
// Apply、#176 の部分木だけに絞ったユーザー）の統合テスト。
// docker で公開の NETCONF サーバ（netopeer2 のコンテナ。イメージは tag と
// digest で固定）を 127.0.0.1 の空いたポートに立て、手元の sakutei の CLI
// （plan・reconcile・status）から ssh の netconf サブシステムで設定の部分木を
// 観測する: 宣言どおりなら適合、別の接続（テストが直に送る edit-config）で手で
// 変えると drift が知らされ、Apply は出ない（機器は手で変えたまま）、何周しても
// ずれが出ない、止めると unknown（観測できない）。Apply を許した設定
// （netconf_apply）では、手で変えた部分木を commit confirmed の 2 段で宣言どおりに
// 当て（1.0 の流れと 1.1 の persist の流れ）、Sakutei 自身が NACM を読めなく
// なる宣言は確かめに失敗して確定されず、confirm-timeout で機器が戻す。
// NACM で 2 つの部分木と要る rpc だけを許した利用者（#176）でも観測と 2 段の
// Apply が通り（その間に機器が断った rpc が無い）、部分木の外を変える宣言は
// access-denied で断られて機器は変わらない。
//
// テストは build tag integration_netconf の付いたファイルにあり、make check・
// make test（go test ./...）には入らない。回すのは make integration-netconf
// （docker・ssh・ssh-keygen と sudo（パスワードなし）が要る）。コンテナは
// 終われば（落ちても）消す。手順と結果は docs/providers/netconf.md の「統合テスト」。
package netconf
