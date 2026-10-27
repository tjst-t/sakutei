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

// Package postgresql は postgresql provider（Backlog #125）の統合テスト。docker の
// 公式の postgres イメージ（tag と digest で固定）でサーバを立て、sakutei の CLI
// （reconcile・status）をそこへ向ける。ホストに psql が無くてもよいように、psql は
// 同じイメージのクライアントのコンテナの中で動かす（ラッパーのスクリプト。
// passfile は docker cp で渡し、引数に秘密を出さない）。コンテナとネットワークは
// 終われば（落ちても）消す。
//
// テストは build tag integration_postgresql の付いたファイルにあり、make check・
// make test（go test ./...）には入らない。回すのは make integration-postgresql
// （docker と sudo（パスワードなし）が要る）。手順と結果は
// docs/providers/postgresql.md の「統合テスト」。
package postgresql
