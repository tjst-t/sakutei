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

package core

// ChangeHint は早期収束パス（early convergence path）の edge ヒントである（§8.6 /
// ADR-0009）。「いつ収束を早起きさせるか」だけを運び、「何が真実か」（変更後の値・
// 観測 state・diff・truth）は一切運ばない。
//
// この不変条件は設計の根幹（level-triggered + 観測 state レス・§3 原則1）を守るため
// に型で強制される。ヒントは収束ループを起こすためのノック（wake）に過ぎず、真実は
// 起床後の新鮮な Observe が現実から再導出する。もしヒントが変更後の値を運べば、
//   - キャッシュした「現在状態」を持つことになり観測 state レスが崩れ、
//   - 途中ヒントの取りこぼし（後述 CoalesceHints）で真実を失う可能性が生まれ、
//   - three-way-diff / state ドリフト問題が構造的に再流入する。
//
// したがって ChangeHint に State/値/diff/観測結果を運ぶフィールドを足すことは禁止で
// ある（この禁を破る変更はレビューで拒否される）。フィールドは IntentRevision と
// TouchedScopes の 2 つのみを維持すること。
type ChangeHint struct {
	// IntentRevision は、このヒントの起点となった intent revision（どの意図世代の
	// 変更が起きたか）を表す不透明タグ（opaque content-address）。
	//
	// 重要: この値は content-addressed な SHA-256 hex（internal/revision）であり、
	// 単調（monotone）ではない。string の大小比較で「新しさ」を判定してはならない。
	// IntentRevision はログや --expect-revision の表示に使うタグに過ぎず、真実
	// （変更後の値・観測 state・diff）は一切運ばない。真実は起床後の新鮮な Observe が
	// 現実から再導出する（§3 原則1: level-triggered + 観測 state レス）。
	IntentRevision string

	// TouchedScopes は、この変更が影響しうる scope 群。影響サブ DAG を絞り込む
	// ためのルーティングキーであり、変更内容（=truth）は運ばない（§8.6）。
	TouchedScopes []Scope
}

// CoalesceHints は複数の ChangeHint を level-triggered 意味論で単一のヒントに畳み込む。
//
// 畳み込み規則:
//   - TouchedScopes は全ヒントの和集合（union）を取る。重複 scope は 1 つに畳む。
//     これが唯一の正しさを担う（load-bearing）部分である — 影響サブ DAG を漏れなく
//     絞り込むため、どのヒントの scope も落とさない。
//   - IntentRevision は「最後に受け取ったヒント」の revision を採用する（last-wins）。
//     最大値（辞書比較）ではない。
//
// なぜ last-wins が正しく十分か:
//
//	IntentRevision は content-addressed な SHA-256 hex（internal/revision）であり、
//	単調ではない — string の大小比較で「新しさ」を測ることはできない（辞書順で大きい
//	revision が新しいとは限らない）。しかしヒントは truth を一切運ばない。畳み込み後に
//	起床した収束ループは、TouchedScopes に対して新鮮な Observe→Diff→Apply を行い、真実は
//	その現実観測から再導出される（§3 原則1）。したがって revision はログや
//	--expect-revision 表示のためのタグに過ぎず、どのヒントの revision タグを残すかは
//	正しさに影響しない。最後に drain したヒントの revision を残すのが最も自然で、
//	単調性の誤仮定を持ち込まない。
//
// この合流が安全なのは、ヒントが truth を一切運ばないからである。途中のヒントを 1 件
// 落としても失うものは何もない — 起床後の Observe は必ず「最終的な現実」を見る（§8.6 の
// 「実行時に最新 desired を読む一点に集約する」level 意味論）。
//
// 入力が空なら空の ChangeHint（IntentRevision=="" / TouchedScopes==nil）を返す。
func CoalesceHints(hints ...ChangeHint) ChangeHint {
	var out ChangeHint

	// scope の和集合を Path() キーで重複排除しつつ、出現順を保った決定的な結果にする。
	seen := make(map[string]struct{})
	for _, h := range hints {
		// IntentRevision は last-wins（最後に受け取ったヒントの revision タグを採用）。
		// revision は content-address で単調ではないため、辞書比較の max は使わない。
		out.IntentRevision = h.IntentRevision
		for _, sc := range h.TouchedScopes {
			key := sc.Path()
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out.TouchedScopes = append(out.TouchedScopes, sc)
		}
	}
	return out
}
