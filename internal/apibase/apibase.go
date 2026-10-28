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

// Package apibase は HTTP の管理 API を使う provider の、API のパスの前置き
// （proxmox-vm の /api2/json など）を設定で変える欄 api_base_path の検査
// （Backlog #160、§28.9）。実機と試験用の模擬で前置きが違っても、provider の
// 設定（つなぎ方、A3）だけを変えて同じ intent を両方に当てられる。
package apibase

import (
	"fmt"
	"strings"
)

// Clean は api_base_path の値を検査し、末尾の "/" を落とした形を返す。"/" は
// 前置き無し（""）。"/" で始まり、クエリ・フラグメント・%・空白・空の段・
// "." と ".." の段を持たないこと。
func Clean(p string) (string, error) {
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("api_base_path %q: want a path that starts with \"/\" (such as \"/api2/json\"; \"/\" for none)", p)
	}
	if strings.ContainsAny(p, "?#% \t\r\n\\") {
		return "", fmt.Errorf("api_base_path %q: only a plain path is allowed (no query, fragment, escape or space)", p)
	}
	out := strings.TrimRight(p, "/")
	if out == "" {
		return "", nil
	}
	for _, seg := range strings.Split(out[1:], "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("api_base_path %q: empty, \".\" and \"..\" segments are not allowed", p)
		}
	}
	return out, nil
}
