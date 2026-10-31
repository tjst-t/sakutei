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

// Command sakutei は新しいコアの CLI。実の provider は段 6 から組み込む。今は
// 偽の provider（モジュール fake、E-P1）だけを持つ。
package main

import (
	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/sdk/fake"
)

func main() {
	cli.Main(cli.WithModule("fake", fake.Module))
}
