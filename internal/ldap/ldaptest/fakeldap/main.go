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

// Command fakeldap は偽の ldapsearch・ldapmodify の実行ファイル（受け入れテストが
// sakutei のバイナリから exec で呼ぶため）。ファイルの名前に "modify" を含めば
// ldapmodify として動く。状態は環境変数 SAKUTEI_FAKE_LDAP_STATE の JSON の
// ファイル（ldaptest.SaveState で書く）。
package main

import "github.com/tjst-t/sakutei/v2/internal/ldap/ldaptest"

func main() { ldaptest.Main() }
