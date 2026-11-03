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

// Command sakutei-migrate は v2 の intent と provider の設定を、新しいコアの intent と
// つなぎ方の設定に書き直す道具（設計 §7.2・§8 の段 9）。使い方は migrate.Run。
package main

import (
	"os"

	"github.com/tjst-t/sakutei/v3/providers/migrate"
)

func main() {
	os.Exit(migrate.Run(os.Args[1:], os.Stdout, os.Stderr))
}
