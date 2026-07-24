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

// schema.go: stdlib-only な最小 JSON Schema 検証器（S33266e-2）。
//
// # なぜ自前実装か
//
// オフライン + stdlib-only の制約下では santhosh-tekuri/jsonschema 等の外部検証器を
// import できない。理想状態木（intent）の検証に必要十分なキーワードだけをサポートする
// 最小の検証器を実装する。
//
// # サポートするキーワード（明示的境界）
//
//   - type: "object" / "array" / "string" / "integer" / "boolean" / "null"
//   - required: 必須プロパティ名の配列
//   - properties: プロパティ名 → サブスキーマ
//   - additionalProperties: false のとき未知キーを拒否（未定義または true なら許可）
//   - items: 配列要素のサブスキーマ
//   - enum: 許容値の列挙
//   - pattern: 文字列に対する正規表現（regexp/stdlib）
//   - minItems: 配列の最小要素数
//
// 上記以外のキーワードは無視する（intent 検証には不要）。$ref / oneOf / allOf 等の
// 参照・合成は未サポート（必要になったら拡張）。
//
// # 検証器の役割（Norway 問題の最終防衛線）
//
// YAML パーサは曖昧なスカラーを保守的に文字列へ倒す。検証器は「version は文字列必須」
// のような型制約を強制し、intent が意図した型と一致することを保証する。違反は
// どのフィールドがどの制約に反したかを含む明確なエラーで拒否する。
package loader

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// schemaNode は JSON Schema のノード（stdlib で map[string]any として保持）。
type schemaNode = map[string]any

// validateAgainstSchema は value を schema で検証し、違反があれば全て集約した
// エラーを返す。path は診断メッセージ用のルートからの JSON パス。
func validateAgainstSchema(value any, schema schemaNode) error {
	v := &schemaValidator{}
	v.validate("$", value, schema)
	if len(v.errs) == 0 {
		return nil
	}
	sort.Strings(v.errs)
	return fmt.Errorf("intent スキーマ検証エラー:\n  - %s", strings.Join(v.errs, "\n  - "))
}

type schemaValidator struct {
	errs []string
}

func (v *schemaValidator) fail(path, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	v.errs = append(v.errs, fmt.Sprintf("%s: %s", path, msg))
}

func (v *schemaValidator) validate(path string, value any, schema schemaNode) {
	// type
	if t, ok := schema["type"].(string); ok {
		if !typeMatches(t, value) {
			v.fail(path, "型が %q でなければなりません（実際: %s）", t, typeName(value))
			return // 型が違えば以降の制約検査は無意味
		}
	}

	// enum
	if enum, ok := schema["enum"].([]any); ok {
		if !enumContains(enum, value) {
			v.fail(path, "許容されない値です: %v（enum: %v）", value, enum)
		}
	}

	// pattern（文字列のみ）
	if pat, ok := schema["pattern"].(string); ok {
		if s, isStr := value.(string); isStr {
			re, err := regexp.Compile(pat)
			if err != nil {
				v.fail(path, "スキーマの pattern が不正です: %v", err)
			} else if !re.MatchString(s) {
				v.fail(path, "値 %q が pattern %q に一致しません", s, pat)
			}
		}
	}

	switch typed := value.(type) {
	case map[string]any:
		v.validateObject(path, typed, schema)
	case []any:
		v.validateArray(path, typed, schema)
	}
}

func (v *schemaValidator) validateObject(path string, obj map[string]any, schema schemaNode) {
	props, _ := schema["properties"].(map[string]any)

	// required
	if req, ok := schema["required"].([]any); ok {
		for _, r := range req {
			name, _ := r.(string)
			if _, present := obj[name]; !present {
				v.fail(path, "必須プロパティ %q がありません", name)
			}
		}
	}

	// additionalProperties: false → 未知キー拒否
	addlFalse := false
	if ap, ok := schema["additionalProperties"]; ok {
		if b, isBool := ap.(bool); isBool && !b {
			addlFalse = true
		}
	}

	// 各プロパティの検証
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		child := childPath(path, k)
		sub, defined := props[k]
		if !defined {
			if addlFalse {
				v.fail(child, "未知のプロパティ %q は許可されていません", k)
			}
			continue
		}
		subSchema, ok := sub.(map[string]any)
		if !ok {
			continue // サブスキーマが map でなければスキップ
		}
		v.validate(child, obj[k], subSchema)
	}
}

func (v *schemaValidator) validateArray(path string, arr []any, schema schemaNode) {
	if mi, ok := toInt(schema["minItems"]); ok {
		if len(arr) < mi {
			v.fail(path, "要素数が最小 %d 未満です（実際: %d）", mi, len(arr))
		}
	}
	if items, ok := schema["items"].(map[string]any); ok {
		for i, el := range arr {
			v.validate(fmt.Sprintf("%s[%d]", path, i), el, items)
		}
	}
}

func childPath(path, key string) string {
	return path + "." + key
}

// typeMatches は JSON Schema の type と Go 値の対応を判定する。
// パーサは整数を int、真偽を bool、null を nil で表す。
func typeMatches(t string, value any) bool {
	switch t {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "integer":
		return isIntegerValue(value)
	case "number":
		return isIntegerValue(value) || isFloatValue(value)
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	default:
		return true // 未知の type 指定は制約なし扱い
	}
}

func isIntegerValue(value any) bool {
	switch value.(type) {
	case int, int64:
		return true
	default:
		return false
	}
}

func isFloatValue(value any) bool {
	switch value.(type) {
	case float32, float64:
		return true
	default:
		return false
	}
}

func typeName(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case int, int64:
		return "integer"
	case float32, float64:
		return "number"
	case string:
		return "string"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	default:
		return fmt.Sprintf("%T", value)
	}
}

func enumContains(enum []any, value any) bool {
	for _, e := range enum {
		if scalarEqual(e, value) {
			return true
		}
	}
	return false
}

// scalarEqual は enum 比較用の緩い等価判定（int/int64 と数値表現の差を吸収）。
func scalarEqual(a, b any) bool {
	if ai, aok := toInt(a); aok {
		if bi, bok := toInt(b); bok {
			return ai == bi
		}
		return false
	}
	return a == b
}

// toInt は JSON/YAML/スキーマ由来の数値表現を int へ寄せる。
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		// JSON パーサ由来の整数値（例: minItems）。
		if n == float64(int(n)) {
			return int(n), true
		}
	}
	return 0, false
}
