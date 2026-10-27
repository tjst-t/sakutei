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

// Package ansible は ansible provider（Backlog #127）の統合テスト。本物の
// ansible-core（版を固定した venv。make integration-ansible が作る）を、
// local 接続で一時ディレクトリを相手に sakutei の CLI（plan・reconcile・status）
// から回す。届かないホストは 127.0.0.1 の閉じたポートへの ssh。
//
// テストは build tag integration_ansible の付いたファイルにあり、make check・
// make test（go test ./...）には入らない。回すのは make integration-ansible
// （python3 と、初回は PyPI に届くこと）。手順と結果は docs/providers/ansible.md
// の「統合テスト」。
package ansible
