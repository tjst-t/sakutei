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

// Package integration は新しいコアの ldap provider の統合テスト（v3 Stage 8e、設計
// §6.2 の統合テスト・E-G1・N-G1・E-G5・E-K1・E-P3）。v2 と同じ 389 Directory Server の
// 公式のイメージ（tag と digest で固定）を docker で立て（sakutei-v3ldap-server。v2 の
// sakutei-ldapit-* と重ならない）、v3 の CLI（cli.Run の plan・reconcile）をそこへ
// 向けて回す：空から作る・手で変えたものを戻す・userPassword は書き込みだけ・何 cycle
// 回しても書かない・StartTLS と LDAPS・bind の失敗は Unknown・子のある entry は prune
// しない・観測と書き込みの credential を分ける・サーバを止めると Unknown で何も消さない。
//
// ホストに OpenLDAP のクライアントが無くてもよいように、ldapsearch・ldapmodify は
// 同じイメージのクライアントのコンテナ（sakutei-v3ldap-client）の中で動かす
// （ラッパーのスクリプト。-y のパスワードのファイルと CA は docker cp で渡し、引数に
// secret を出さない）。
//
// テストは build tag integration_v3_ldap の付いたファイルにあり、make check・
// make test には入らない。回すのは make integration-v3-ldap（docker と sudo
// （パスワードなし）が要る）。コンテナとネットワークは終われば（落ちても）消す。
// 手順と結果は v3/providers/README.md。
package integration
