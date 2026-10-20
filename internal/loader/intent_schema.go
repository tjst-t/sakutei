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

// intent_schema.go: 理想状態木（intent）の JSON Schema と round-trip emitter（S33266e-2）。
//
// スキーマは schema.go の最小検証器がサポートするキーワードだけで表現する。
// キーは snake_case を正とし、additionalProperties:false で未知キーを拒否する。
package loader

import (
	"encoding/json"
	"fmt"
	"strings"
)

// intentSchema は理想状態木のルートスキーマ。
//
//   - resources は配列（pools / scopes だけのファイルも書けるよう任意）。
//   - pools は採番プールの宣言（name / range / claim / space、§19.8）。
//   - scopes は scope ごとの mode（converge / sync）と sync.target_path（§24.5）、
//     scope 明示の remediation（auto / alert-only、§14.3）。
//   - remediation は時間帯連動の修復 mode（default / schedule[{window, mode}] / tz、§14.2）。
//   - observe_tiers は Observe 頻度の tier（name / observe_every、§16.5）。resource は observe_tier で
//     所属を書く（tier: は §8 の DAG 優先度帯 `tier: foundation` の予約名なので使わない）。
//   - 各 resource は provider / id（必須、文字列）、scope（文字列→文字列マップ）、
//     depends_on（文字列、または { id, ready_condition } の配列）、fields（文字列→スカラーのマップ）、
//     deletion_policy（enum）、update_policy（strategy / window / rollback_on_fail、§6.2）を持つ。
//   - version は「文字列必須」の代表例として scope の外側に置ける任意フィールドとして
//     許容し、型を string に固定する（Norway 問題: 1.10 を float 化させない）。
//
// 未知のトップレベルキー・未知の resource キーは additionalProperties:false で拒否する。
func intentSchema() schemaNode {
	stringMap := schemaNode{
		"type": "object",
		// scope/fields の値は文字列（provisional 形式に合わせる）。
		"additionalProperties": true,
	}
	return schemaNode{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			// version は文字列必須（Norway 問題の代表例）。任意。
			"version": schemaNode{"type": "string"},
			// scopes は scope ごとのポリシー（ScopeMode、§24.5）。
			"scopes": schemaNode{
				"type": "array",
				"items": schemaNode{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []any{"scope"},
					"properties": map[string]any{
						"scope": stringMap,
						"mode": schemaNode{
							"type": "string",
							"enum": []any{"converge", "sync"},
						},
						"remediation": schemaNode{
							"type": "string",
							"enum": []any{"auto", "alert-only"},
						},
						"sync": schemaNode{
							"type":                 "object",
							"additionalProperties": false,
							"properties": map[string]any{
								"target_path": schemaNode{"type": "string"},
							},
						},
					},
				},
			},
			// remediation は時間帯連動の修復 mode（§14.2）。窓は先に書いたものが勝ち、
			// どの窓にも入らない時刻は default（省略時 alert-only）。
			"remediation": schemaNode{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"default": schemaNode{"type": "string", "enum": []any{"auto", "alert-only"}},
					"tz":      schemaNode{"type": "string"},
					"schedule": schemaNode{
						"type": "array",
						"items": schemaNode{
							"type":                 "object",
							"additionalProperties": false,
							"required":             []any{"window", "mode"},
							"properties": map[string]any{
								"window": schemaNode{"type": "string"},
								"mode":   schemaNode{"type": "string", "enum": []any{"auto", "alert-only"}},
							},
						},
					},
				},
			},
			// observe_tiers は Observe 頻度の tier の宣言（§16.5）。observe_every は「N 周に1回」。
			"observe_tiers": schemaNode{
				"type": "array",
				"items": schemaNode{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []any{"name", "observe_every"},
					"properties": map[string]any{
						"name":          schemaNode{"type": "string"},
						"observe_every": schemaNode{"type": "integer"},
					},
				},
			},
			// pools は採番プールの宣言（§19.8）。range は "下限-上限"（整数）。
			"pools": schemaNode{
				"type": "array",
				"items": schemaNode{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []any{"name", "range"},
					"properties": map[string]any{
						"name":  schemaNode{"type": "string"},
						"range": schemaNode{"type": "string"},
						"claim": stringMap,
						"space": schemaNode{"type": "string"},
					},
				},
			},
			"resources": schemaNode{
				"type": "array",
				"items": schemaNode{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []any{"provider", "id"},
					"properties": map[string]any{
						"provider": schemaNode{"type": "string"},
						"id":       schemaNode{"type": "string"},
						"scope":    stringMap,
						// depends_on の要素は Local ID の文字列か、ready_condition 付きの
						// オブジェクト { id, ready_condition }（§8.3, §11.2）。
						"depends_on": schemaNode{"type": "array", "items": schemaNode{
							"type":                 []any{"string", "object"},
							"additionalProperties": false,
							"required":             []any{"id"},
							"properties": map[string]any{
								"id":              schemaNode{"type": "string"},
								"ready_condition": schemaNode{"type": "string"},
							},
						}},
						"fields":       stringMap,
						"observe_tier": schemaNode{"type": "string"},
						// update_policy は差分の直し方（§6.2, §6.5）。strategy: task なら
						// 直接 Apply せず Task を起動する。window はメンテ窓。
						"update_policy": schemaNode{
							"type":                 "object",
							"additionalProperties": false,
							"properties": map[string]any{
								"strategy": schemaNode{
									"type": "string",
									"enum": []any{"apply", "task"},
								},
								"window":           schemaNode{"type": "string"},
								"rollback_on_fail": schemaNode{"type": "boolean"},
							},
						},
						"deletion_policy": schemaNode{
							"type": "string",
							"enum": []any{"ExplicitAbsentOnly", "PruneWithinScope"},
						},
					},
				},
			},
		},
	}
}

// decodeIntentTree は生バイト列を（拡張子に応じて）JSON または YAML としてパースし、
// フォーマット非依存の in-memory 木（map[string]any）へ変換する。
//
// format は "json" / "yaml" のいずれか。
func decodeIntentTree(data []byte, format string) (map[string]any, error) {
	switch format {
	case "yaml":
		return parseYAML(data)
	case "json":
		var tree map[string]any
		dec := json.NewDecoder(strings.NewReader(string(data)))
		dec.UseNumber() // 数値は json.Number で受け、後段で int へ寄せる
		if err := dec.Decode(&tree); err != nil {
			return nil, fmt.Errorf("json パースに失敗: %w", err)
		}
		normalizeJSONNumbers(tree)
		return tree, nil
	default:
		return nil, fmt.Errorf("未知のフォーマット: %s", format)
	}
}

// normalizeJSONNumbers は json.Number を int（整数の場合）または string（それ以外）へ
// 寄せて、YAML パーサの型表現（整数=int、小数=string）と一致させる。これにより
// JSON と YAML が同一の in-memory 木を生む（round-trip / 等価性の前提）。
func normalizeJSONNumbers(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			t[k] = normalizeJSONNumbers(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = normalizeJSONNumbers(val)
		}
		return t
	case json.Number:
		s := t.String()
		if isIntToken(s) {
			if n, err := t.Int64(); err == nil {
				return int(n)
			}
		}
		// 小数・指数は文字列のまま（YAML パーサと同じ保守的扱い）。
		return s
	default:
		return v
	}
}

// emitCanonicalJSON は in-memory 理想状態木を決定的（キー昇順・インデント）な JSON へ
// シリアライズする。これが round-trip の canonical emit ターゲットである。
//
// round-trip 保証:
//
//	load(YAML|JSON) → 正規化木 → emitCanonicalJSON → decodeIntentTree("json")
//	が意味的に等価な木を返す。encoding/json の Marshal はマップキーを昇順に並べるので、
//	emit は決定的である。数値は int で保持され、再ロード時も int へ寄る。
func emitCanonicalJSON(tree map[string]any) ([]byte, error) {
	return json.MarshalIndent(tree, "", "  ")
}
