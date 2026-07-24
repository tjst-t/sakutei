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

// roundtrip.go: フォーマット非依存な木の load/emit/validate の公開 API（S33266e-2）。
//
// 内部の decodeIntentTree / validateAgainstSchema / emitCanonicalJSON を薄く公開し、
// round-trip 安定性（load→emit→load が等価な木を返す）と JSON Schema 検証を
// パッケージ外（受入テスト・将来のツール）から呼べるようにする。
package loader

// DecodeIntentTree は生バイト列を（format="json"/"yaml"）フォーマット非依存の
// in-memory 木へパースする。YAML はサブセットのみ受理し、サブセット外は明確な
// エラーで拒否する（yaml.go 参照）。
func DecodeIntentTree(data []byte, format string) (map[string]any, error) {
	return decodeIntentTree(data, format)
}

// ValidateIntentTree はロード済みの木を intent JSON Schema で検証する。
// 違反（未知キー・型不一致・enum 逸脱・必須欠落）は明確なエラーで返す。
func ValidateIntentTree(tree map[string]any) error {
	return validateAgainstSchema(tree, intentSchema())
}

// EmitCanonicalJSON は木を決定的（キー昇順・インデント）な JSON へシリアライズする。
// これが round-trip の canonical emit ターゲットである。
func EmitCanonicalJSON(tree map[string]any) ([]byte, error) {
	return emitCanonicalJSON(tree)
}
