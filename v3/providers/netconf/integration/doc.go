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

// Package integration は新しいコアの netconf provider の統合テスト（v3 段 8c、
// 設計 §6.2 の統合テスト・E-P3・N-P4・不変条件 17）。v2 と同じ netopeer2 のコンテナ
// （イメージは tag と digest で固定）を docker で 4 台（sakutei-v3nc-sw1・g1〜g3。
// v2 の sakutei-ncit-* と重ならない）127.0.0.1 の空いたポートに立て、v3 の CLI
// （cli.Run の plan・reconcile）を本物の NETCONF over SSH に向けて回す。確かめるのは、
// 観測だけ・commit confirmed の two-phase Apply（1.0 と 1.1 の persist）・NACM で
// 部分木だけに絞ったユーザー・check が通らなければ確定せず戻る・Prepare のあと確定
// しなければ機器の confirm-timeout で戻る・commit_group と confirm の check(…)・
// present(…)・機器を停止すると Unknown。
//
// テストは build tag integration_v3_netconf の付いたファイルにあり、make check・
// make test には入らない。回すのは make integration-v3-netconf（docker・ssh・
// ssh-keygen と sudo（パスワードなし）が要る）。コンテナは終われば（落ちても）消す。
// 手順と結果は v3/providers/README.md。
package integration
