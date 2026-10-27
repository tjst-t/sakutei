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

// Package ldap は ldap provider（Backlog #144）の統合テスト。docker の 389
// Directory Server の公式のイメージ（tag と digest で固定）でサーバを立て、
// sakutei の CLI（reconcile・status）を --provider-config でそこへ向ける。ホストに
// OpenLDAP のクライアントが無くてもよいように、ldapsearch・ldapmodify は同じ
// イメージのクライアントのコンテナの中で動かす（ラッパーのスクリプト。-y の
// パスワードのファイルと CA は docker cp で渡し、引数に秘密を出さない）。接続は
// StartTLS（-ZZ、サーバの自己署名の CA で確かめる）と LDAPS。コンテナと
// ネットワークは終われば（落ちても）消す。
//
// テストは build tag integration_ldap の付いたファイルにあり、make check・
// make test（go test ./...）には入らない。回すのは make integration-ldap
// （docker と sudo（パスワードなし）が要る）。手順と結果は
// docs/providers/ldap.md の「統合テスト」。
package ldap
