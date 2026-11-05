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

// Package integration は新しいコアの ansible provider の統合テスト（v3 Stage 8f、
// 設計 §6.2 の統合テスト・E-E3・E-K1・E-P3）。v2 の統合テストと同じバージョンに固定した
// venv の ansible-core を、local 接続で一時ディレクトリを相手に v3 の CLI
// （cli.Run の plan・reconcile、ansible.Module）から動かす。到達できないホストは
// 127.0.0.1 の閉じたポートへの ssh。
//
// テストは build tag integration_v3_ansible の付いたファイルにあり、make check・
// make test には入らない。動かすのは make integration-v3-ansible（python3 と、初回は
// PyPI に到達できることが要る）。ansible の実行ファイルは環境変数 SAKUTEI_IT_ANSIBLE で渡す。
// 手順と結果は v3/providers/README.md。
package integration
