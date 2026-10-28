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

// Package hostconf は host-file と systemd-unit（Backlog #155）の統合テスト。
// docker で systemd を PID 1 で動かす使い捨てのコンテナ（ベースのイメージは tag と
// digest で固定。sshd・sudo・podman）を立て、手元の sakutei の CLI（reconcile・
// status）から ssh 越しに、ファイルと通常のユニット、podman の quadlet で動く
// コンテナを収束させる。依って立つファイルを書き換えるとユニットが 1 回だけ
// 入れ替わること、届かない・sudo が通らないと何もしないことも確かめる。
//
// テストは build tag integration_hostconf の付いたファイルにあり、make check・
// make test（go test ./...）には入らない。回すのは make integration-hostconf
// （docker（--privileged）・ssh・ssh-keygen と sudo（パスワードなし）が要る。初回は
// イメージを作るので apt のミラーに届くこと）。手順と結果は
// docs/providers/systemd-unit.md の「統合テスト」。
package hostconf
