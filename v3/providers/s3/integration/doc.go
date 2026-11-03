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

// Package integration は新しいコアの s3 provider の統合テスト（v3 段 8f、設計 §6.2 の
// 統合テスト・E-P3・N-P4）。v2 と同じ S3 互換のサーバ（必須の SeaweedFS と、任意の
// MinIO・PUT の API を持たない古い SeaweedFS。イメージは tag と digest で固定し、
// 任意のものは取れなければ飛ばす）を docker で立て（コンテナは sakutei-v3s3-*。v2 の
// sakutei-s3it-* と重ならない。127.0.0.1 の空いたポートにだけ公開し、終われば
// 落ちても消す）、v3 の CLI（cli.Run の plan・reconcile・approve）を向けて回す：
// 空から作る・手の変更が戻る・何周しても宣言どおり・宣言の外は消さずに見せる・
// ensure: absent の削除と中身のあるバケットの断り・止まったサーバは Unknown・
// API を持たない endpoint・endpoint の fields・観測用と書き込み用の利用者（MinIO の
// mc admin で作る）・Object Lock（作る・観測する・承認つきで保持の設定を変える）。
//
// テストは build tag integration_v3_s3 の付いたファイルにあり、make check・make test
// には入らない。回すのは make integration-v3-s3（docker と sudo（パスワードなし）が
// 要る）。手順と結果は v3/providers/README.md。
package integration
