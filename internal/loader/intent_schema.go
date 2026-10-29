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

	"github.com/tjst-t/sakutei/v2/internal/budget"
	"github.com/tjst-t/sakutei/v2/internal/runtimeargs"
)

// intentSchema は理想状態木のルートスキーマ。
//
//   - resources は配列（pools / scopes だけのファイルも書けるよう任意）。
//   - external_owners は触らない宣言（owner / provider / scope / id / id_prefix / fields、§7.3・#158）。
//   - providers は provider のインスタンスごとの範囲（scope・管理する表など、§7.7・#181）。
//   - compatibility は部品の版の互換の表（name / components / allow、§29.1・#151）。
//   - pools は採番プールの宣言（name / range / claim / space、§19.8）。
//   - scopes は scope ごとの mode（converge / sync）と sync.target_path（§24.5）、
//     scope 明示の remediation（auto / alert-only、§14.3）。
//   - remediation は時間帯連動の修復 mode（default / schedule[{window, mode}] / tz、§14.2）。
//   - observe_tiers は Observe 頻度の tier（name / observe_every、§16.5）。resource は observe_tier で
//     所属を書く（tier: は §8 の DAG 優先度帯 `tier: foundation` の予約名なので使わない）。
//   - planned_change は大量正当作業の宣言（scope / kind / max_count / window / applicant / approval /
//     requirements / cooling_off、§13.3 / §14.4 / §14.5）。1 件ならオブジェクト、複数なら配列で書く。
//   - apply_check は provider の型ごとの Apply の効きの確かめ（provider / settle / max_ineffective_applies /
//     max_ineffective_duration、§11.1、Backlog #166）。resource も apply_check（provider を除く同じ鍵）を書ける。
//   - profile は運用プロファイル（solo / team / enterprise、§14.5）、solo はその設定（cooling_off）。
//   - workflows は最小 Workflow の宣言（workflow / applicant / trigger / target / invariant / strategy / stages /
//     steps / on_failure、§6.3 / §6.4）。書式の詳細と v1.5 で拒否するものは workflow.go。
//   - 各 resource は provider / id（必須、文字列）、scope（文字列→文字列マップ）、
//     depends_on（文字列、または { id, ready_condition } の配列）、fields（文字列→スカラーのマップ）、
//     deletion_policy（enum）、update_policy（strategy / window / rollback_on_fail、manual の procedure / procedure_url / confirm_timeout、§6.2）を持つ。
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
			// apply_check は型（provider）ごとの Apply の効きの確かめ（§11.1 Tier2・Tier3、
			// Backlog #166）: settle と、効かない Apply を止める上限（回数・時間）。
			"apply_check": schemaNode{
				"type":  "array",
				"items": applyCheckSchema(true),
			},
			// planned_change は専用予算レーンの宣言（§13.3）。1 件はオブジェクト、複数は配列。
			"planned_change": plannedChangeSchema(),
			// profile は運用プロファイル（§14.5）。solo は solo の設定（cooling_off）。
			"profile": schemaNode{"type": "string", "enum": []any{"solo", "team", "enterprise"}},
			"solo": schemaNode{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"cooling_off": schemaNode{"type": []any{"string", "integer"}},
				},
			},
			// workflows は最小 Workflow（§6.3）。直列 + invariant + 手動承認ゲート。
			"workflows": workflowsSchema(),
			// external_owners は触らない宣言（§7.3、#158）。owner と、選び方
			// （provider / scope / id か id_prefix / fields）。
			"external_owners": schemaNode{
				"type": "array",
				"items": schemaNode{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []any{"owner"},
					"properties": map[string]any{
						"owner":     schemaNode{"type": "string"},
						"provider":  schemaNode{"type": "string"},
						"scope":     stringMap,
						"id":        schemaNode{"type": "string"},
						"id_prefix": schemaNode{"type": "string"},
						"fields":    stringMap,
					},
				},
			},
			// providers は provider のインスタンスごとの範囲（§7.7、#181）。
			// provider の名前 → その範囲の設定。中身の意味は provider の
			// IntentRanges が確かめる（registry、provider を組み立てる入口）。
			"providers": schemaNode{
				"type":                 "object",
				"additionalProperties": true,
			},
			// network_policy は網の間の許可（§7.6、#174）。網（名前と CIDR）・許可
			// （from・to・proto・ports・reason・tags）・実現先。中身の意味は
			// netpolicy.Parse と netpolicy.Check が確かめる。
			"network_policy": networkPolicySchema(),
			// runtime は長く効く起動の引数（§26.7、#183）。値の形と範囲は
			// runtimeargs.Parse が確かめる。
			"runtime": runtimeSchema(),
			// compatibility は部品の版の互換の表（§29.1、#151）。部品（id と版の
			// field）と、許す版の組み合わせの行。行の値が文字列かは compat.Parse。
			"compatibility": schemaNode{
				"type": "array",
				"items": schemaNode{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []any{"name", "components", "allow"},
					"properties": map[string]any{
						"name": schemaNode{"type": "string"},
						// 部品の名前 → { id, field }。中身は compat.Parse が確かめる。
						"components": schemaNode{"type": "object", "additionalProperties": true},
						"allow": schemaNode{
							"type":  "array",
							"items": schemaNode{"type": "object", "additionalProperties": true},
						},
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
						// write_back は払い出した結果を宣言の側のファイルへ書き戻し、
						// それを正として読むか（§19.7、#157）。
						"write_back": schemaNode{"type": "boolean"},
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
						// apply_check はこのリソースの settle と上限（型の宣言より勝つ、#166）。
						"apply_check": applyCheckSchema(false),
						// update_policy は差分の直し方（§6.2, §6.5）。strategy: task なら
						// 直接 Apply せず Task を起動する。window はメンテ窓。strategy: manual は
						// 人の手順で合わせる Task（#150）で、procedure / procedure_url /
						// confirm_timeout を持つ。
						"update_policy": schemaNode{
							"type":                 "object",
							"additionalProperties": false,
							"properties": map[string]any{
								"strategy": schemaNode{
									"type": "string",
									"enum": []any{"apply", "task", "manual"},
								},
								"window":           schemaNode{"type": "string"},
								"rollback_on_fail": schemaNode{"type": "boolean"},
								"procedure":        schemaNode{"type": "string"},
								"procedure_url":    schemaNode{"type": "string"},
								"confirm_timeout":  schemaNode{"type": "string"},
							},
						},
						"deletion_policy": schemaNode{
							"type": "string",
							"enum": []any{"ExplicitAbsentOnly", "PruneWithinScope"},
						},
						// ensure: absent は宣言で消す（明示の absent、§12.2・Backlog #163）。
						// 既定は present。
						"ensure": schemaNode{
							"type": "string",
							"enum": []any{"present", "absent"},
						},
					},
				},
			},
		},
	}
}

// networkPolicySchema は network_policy:（§7.6、#174）の形。
func networkPolicySchema() schemaNode {
	strList := schemaNode{"type": "array", "items": schemaNode{"type": "string"}}
	return schemaNode{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"networks": schemaNode{
				"type": "array",
				"items": schemaNode{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []any{"name", "cidrs"},
					"properties": map[string]any{
						"name": schemaNode{"type": "string"},
						// domain はアドレスの空間（#195）。書かなければ既定の domain。
						"domain": schemaNode{"type": "string"},
						"cidrs":  schemaNode{"type": "array", "minItems": 1, "items": schemaNode{"type": "string"}},
					},
				},
			},
			"allows": schemaNode{
				"type": "array",
				"items": schemaNode{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []any{"from", "to"},
					"properties": map[string]any{
						"from":   schemaNode{"type": "string"},
						"to":     schemaNode{"type": "string"},
						"proto":  schemaNode{"type": "string", "enum": []any{"any", "tcp", "udp", "icmp", "icmpv6"}},
						"ports":  schemaNode{"type": "array", "items": schemaNode{"type": []any{"string", "integer"}}},
						"reason": schemaNode{"type": "string"},
						"tags":   strList,
					},
				},
			},
			"targets": schemaNode{
				"type": "array",
				"items": schemaNode{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []any{"name", "kind", "networks", "id"},
					"properties": map[string]any{
						"name":     schemaNode{"type": "string"},
						"realizer": schemaNode{"type": "string"},
						"kind":     schemaNode{"type": "string", "enum": []any{"boundary", "host"}},
						"networks": strList,
						// 口ごとにどの domain の網を見るか（#195）。
						"interfaces": schemaNode{
							"type": "array",
							"items": schemaNode{
								"type":                 "object",
								"additionalProperties": false,
								"required":             []any{"name", "domain"},
								"properties": map[string]any{
									"name":     schemaNode{"type": "string"},
									"domain":   schemaNode{"type": "string"},
									"networks": strList,
								},
							},
						},
						"id":     schemaNode{"type": "string"},
						"scope":  schemaNode{"type": "object", "additionalProperties": true},
						"before": strList,
						"after":  strList,
						// realizer に固有の値（nftables は hook・priority・policy・confirm）。
						"settings": schemaNode{"type": "object", "additionalProperties": true},
					},
				},
			},
		},
	}
}

// workflowsSchema は workflows（§6.3 / §6.4）のスキーマ。step の中身・v1.5 で
// 未対応のキーの拒否は parseWorkflow が行う。
func workflowsSchema() schemaNode {
	stringMap := schemaNode{"type": "object", "additionalProperties": true}
	return schemaNode{
		"type": "array",
		"items": schemaNode{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []any{"workflow", "target", "stages", "steps"},
			"properties": map[string]any{
				"workflow":  schemaNode{"type": "string"},
				"applicant": schemaNode{"type": "string"},
				// trigger: reimage は planned_change kind: reimage で起こす（#88）。
				"trigger": schemaNode{"type": "string"},
				"target": schemaNode{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []any{"provider"},
					"properties": map[string]any{
						"provider": schemaNode{"type": "string"},
						"scope":    stringMap,
					},
				},
				"invariant": schemaNode{"type": "object", "additionalProperties": true},
				"strategy":  schemaNode{"type": "object", "additionalProperties": true},
				"stages": schemaNode{
					"type": "array",
					"items": schemaNode{
						"type":                 "object",
						"additionalProperties": false,
						"required":             []any{"name", "targets"},
						"properties": map[string]any{
							"name":    schemaNode{"type": "string"},
							"targets": schemaNode{"type": "array", "items": schemaNode{"type": "string"}},
							"gate": schemaNode{
								"type":                 "object",
								"additionalProperties": false,
								"properties": map[string]any{
									"manual_approval": schemaNode{"type": []any{"string", "object"}},
									"health_check":    schemaNode{"type": []any{"string", "object"}},
									"timeout":         schemaNode{"type": "string"},
								},
							},
						},
					},
				},
				"steps": schemaNode{"type": "array", "items": schemaNode{"type": "object"}},
				// disruption は全体を止める型の更新（#152）。中身は parseDisruption が検証する。
				"disruption": schemaNode{"type": "object", "additionalProperties": true},
				"on_failure": schemaNode{
					"type":                 "object",
					"additionalProperties": false,
					"properties": map[string]any{
						"policy":       schemaNode{"type": "string"},
						"keep_drained": schemaNode{"type": "boolean"},
					},
				},
			},
		},
	}
}

// plannedChangeSchema は planned_change（§13.3）のスキーマ。オブジェクト 1 件か、その配列。
func plannedChangeSchema() schemaNode {
	kinds := make([]any, 0, len(budget.PlannedKinds))
	for _, k := range budget.PlannedKinds {
		kinds = append(kinds, string(k))
	}
	decl := schemaNode{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"scope", "kind", "max_count", "window"},
		"properties": map[string]any{
			"scope":     schemaNode{"type": "object", "additionalProperties": true},
			"kind":      schemaNode{"type": "string", "enum": kinds},
			"max_count": schemaNode{"type": "integer"},
			"window":    schemaNode{"type": "string"},
			"applicant": schemaNode{"type": "string"},
			"approval": schemaNode{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"pr": schemaNode{"type": "integer"},
					// 承認者は ID か { id, roles }（§14.4）。
					"approvers": schemaNode{"type": "array", "items": schemaNode{"type": []any{"string", "object"}}},
				},
			},
			// requirements は §14.4 の承認者要件（プロファイルの既定より優先）。
			"requirements": schemaNode{"type": "object", "additionalProperties": true},
			// cooling_off は宣言からレーンが開くまでの遅延（プロファイルの既定より優先、§14.5）。
			"cooling_off": schemaNode{"type": []any{"string", "integer"}},
		},
	}
	out := schemaNode{"type": []any{"object", "array"}, "items": decl}
	for k, v := range decl {
		if k != "type" {
			out[k] = v
		}
	}
	return out
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

// applyCheckSchema は apply_check の宣言 1 つの形（§11.1、Backlog #166）。
// top-level の要素は provider を必須にする。
func applyCheckSchema(top bool) schemaNode {
	props := map[string]any{
		"settle":                   schemaNode{"type": "string"},
		"max_ineffective_applies":  schemaNode{"type": "integer"},
		"max_ineffective_duration": schemaNode{"type": "string"},
	}
	n := schemaNode{"type": "object", "additionalProperties": false, "properties": props}
	if top {
		props["provider"] = schemaNode{"type": "string"}
		n["required"] = []any{"provider"}
	}
	return n
}

// runtimeSchema は runtime:（§26.7、#183）の形。キーは runtimeargs.Args。
func runtimeSchema() schemaNode {
	props := map[string]any{}
	for _, a := range runtimeargs.Args {
		props[a.Key] = schemaNode{"type": []any{"string", "integer"}}
	}
	// daemon: / oneshot:（#193）は同じ欄。種類に意味の無い欄は
	// runtimeargs.Parse が断る。
	kindProps := map[string]any{}
	for k, v := range props {
		kindProps[k] = v
	}
	for _, k := range runtimeargs.Kinds {
		props[string(k)] = schemaNode{"type": "object", "additionalProperties": false, "properties": kindProps}
	}
	return schemaNode{"type": "object", "additionalProperties": false, "properties": props}
}
