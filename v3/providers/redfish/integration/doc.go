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

// Package integration は新しいコアの redfish provider の統合テスト（v3 Stage 8b、
// 設計 §6.2 の統合テスト・E-P3・N-P4）。v2 と同じ qemu-bmc 0.11.0 を containerlab で
// 2 台立て（lab sakutei-v3rf。v2 の sakutei-rf84 と重ならない）、v3 の sakutei の
// CLI と daemon を本物の Redfish に向けて回す。
//
// テストは build tag integration_v3_redfish の付いたファイルにあり、make check・
// make test には入らない。回すのは make integration-v3-redfish（KVM・docker・
// containerlab・sudo が要る）。実行方法と結果は v3/providers/README.md。
package integration
