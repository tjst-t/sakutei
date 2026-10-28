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

// Package redfish は Redfish の BMC provider の統合テスト（#84、設計 §26.1 の
// 3 段目・§29.1 の（7））。qemu-bmc 0.11.0 を containerlab で 2 台立て、sakutei の
// CLI と常駐ループを本物の Redfish に向けて回す。10〜13 段目（#168）は FW の更新
// （redfish-firmware）を qemu-bmc の UpdateService の模擬に向ける。
//
// テストは build tag integration_redfish の付いたファイルにあり、make check・
// make test（go test ./...）には入らない。回すのは make integration-redfish
// （KVM・docker・containerlab・sudo が要る）。手順と結果は
// docs/providers/redfish.md の「統合テスト」。
package redfish
