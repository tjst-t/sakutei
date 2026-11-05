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

// Package secretbackend は secret backend のモジュール（設計 §3.8・§5.6・§7.1、
// E-G3・E-G4・E-O2・N-O2）。Vault KV v2（module: vault-kv2。OpenBao も同じ API）と
// 外部コマンド（module: command）を持つ。v2 の internal/secretbackend を移した。
//
// コアは cycle の初めに Generation を呼ぶ（世代だけを読み、値は読まない）。provider は
// Apply の直前に WriteContext.Secret 経由で Resolve を呼ぶ。どちらもエラーメッセージに
// 値を入れない（不変条件 27）。backend に接続できないときはエラーを返す。コアはその
// secret に依る宣言をその cycle の間 Unknown にする（E-G4）。
package secretbackend

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// errorDetailMax はエラーメッセージに引用する外部の文字列の長さの上限。
const errorDetailMax = 200

// checkKeys は未知の設定をエラーにする。
func checkKeys(kind string, settings map[string]any, known ...string) error {
	ok := map[string]bool{}
	for _, k := range known {
		ok[k] = true
	}
	var bad []string
	for k := range settings {
		if !ok[k] {
			bad = append(bad, k)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	sort.Strings(known)
	return fmt.Errorf("%s: 未知の設定 %s（書ける設定: %s）", kind, strings.Join(bad, ", "), strings.Join(known, ", "))
}

// str は文字列の設定（無ければ ""）。
func str(kind string, settings map[string]any, key string) (string, error) {
	v, ok := settings[key]
	if !ok {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s: %s に文字列を書く", kind, key)
	}
	return s, nil
}

// timeoutSetting は timeout を読む（Go の duration 形式。既定は def）。
func timeoutSetting(kind string, settings map[string]any, def time.Duration) (time.Duration, error) {
	s, err := str(kind, settings, "timeout")
	if err != nil || s == "" {
		return def, err
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s: timeout %q: 10s のような正の時間を書く", kind, s)
	}
	return d, nil
}

// clip はエラーメッセージに引用する外部の文字列を 1 行にし、長さの上限で切る。
func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > errorDetailMax {
		s = s[:errorDetailMax] + "…"
	}
	return s
}
