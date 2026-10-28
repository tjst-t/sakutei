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

// Package otlp はトレースの出口（Backlog #189、§26.8）の統合テスト。docker で
// OpenTelemetry Collector（contrib の配布。tag と digest で固定）を立て、OTLP/HTTP
// の受け口を 2 つ（loopback の平文と、TLS・クライアント証明書・bearer トークンを
// 求めるもの）開き、受けた span を file exporter でファイルに書かせる。sakutei の
// 常駐・plan・reconcile の span が届くこと、TLS と認可で送れること、資格が違えば
// Collector が断り sakutei は収束を止めないこと、--otlp-endpoint が無ければ何も
// 届かないことを確かめる。コンテナは 127.0.0.1 の空いたポートにだけ公開し、
// 終われば（落ちても）消す。
//
// テストは build tag integration_otlp の付いたファイルにあり、make check・
// make test（go test ./...）には入らない。回すのは make integration-otlp
// （docker と sudo（パスワードなし）が要る）。手順と結果は docs/tracing.md の
// 「統合テスト」。
package otlp
