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

// Command fakepsql は偽の psql の実行ファイル（受け入れテストが sakutei の
// バイナリから exec で呼ぶため）。状態は環境変数 SAKUTEI_FAKE_PSQL_STATE の
// JSON のファイル（pgtest.SaveState で書く）。
package main

import "github.com/tjst-t/sakutei/internal/postgresql/pgtest"

func main() { pgtest.Main() }
