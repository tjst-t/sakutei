// Copyright 2026 tjst-t
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package vault は Vault 形（KV v2）の秘密の取り出し先（Backlog #188）の統合テスト。
// docker で OpenBao（tag と digest で固定）を dev モードで立て、KV v2 に秘密を書き、
// 読むだけの policy の token のファイルと AppRole の両方で、sakutei の CLI
// （plan・reconcile・status）と常駐を examples/external-provider の kvfile に向ける。
// 取り出し・版が上がったときの Apply・届かない（ポートが閉じている・sealed）ときの
// unknown を確かめる。コンテナは 127.0.0.1 の空いたポートにだけ公開し、終われば
// （落ちても）消す。
//
// テストは build tag integration_vault の付いたファイルにあり、make check・
// make test（go test ./...）には入らない。回すのは make integration-vault
// （docker と sudo（パスワードなし）が要る）。手順と結果は
// docs/providers/secret-backends.md の「統合テスト」。
package vault
