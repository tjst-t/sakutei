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

// Package s3 は s3-bucket provider（Backlog #122）の統合テスト。docker で
// S3 互換のサーバを立て（MinIO と SeaweedFS、加えて PUT の API を持たない古い
// SeaweedFS）、sakutei の CLI（plan・reconcile・status）をそこへ向ける。
// コンテナは 127.0.0.1 の空いたポートにだけ公開し、終われば（落ちても）消す。
//
// テストは build tag integration_s3 の付いたファイルにあり、make check・
// make test（go test ./...）には入らない。回すのは make integration-s3
// （docker と sudo（パスワードなし）が要る）。手順と結果は
// docs/providers/s3-bucket.md の「統合テスト」。
package s3
