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

package loader

import (
	"context"

	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/pkg/core"
)

// DesiredSource は理想状態（desired）の供給源を表す seam である。
//
// Load は現時点の desired resource 群と revision 情報を返す。level-triggered な
// 定期周期は Load だけで回る（毎周期 desired を読み直す）。Load のシグネチャは既存の
// LoadDir と等価（[]diff.DesiredResource / core.RevisionInfo / error）で、DirSource が
// LoadDir へ委譲する。
type DesiredSource interface {
	// Load は現在の desired resource 群と revision 情報を返す。
	Load(ctx context.Context) ([]diff.DesiredResource, core.RevisionInfo, error)
}

// Watcher は DesiredSource の任意 capability（宣言制）であり、早期収束パス（§8.6 /
// ADR-0009）の edge ヒントを供給する。
//
// Lister と同じ宣言制（別 interface への型アサーション）で切られている: ある
// DesiredSource が Watch に対応するかは、その値が Watcher を実装しているかを
//
//	w, ok := src.(loader.Watcher)
//
// で判定する。Watch を実装しない DesiredSource は seam 上「未宣言」＝ watch 非対応と
// して扱われ、periodic（Load のみ）で回る。
//
// Watch は revision+scope のみを運ぶ core.ChangeHint をチャネルに流す。ヒントは truth を
// 運ばない（core.ChangeHint のドキュメント参照）。実バルク/実 webhook 連携（webhook・
// watch デーモン）は v1b の実装であり、この seam では型だけを切る。
type Watcher interface {
	// Watch は ChangeHint を流すチャネルを返す。
	//
	// 契約:
	//   - watch 非対応の実装は (nil, nil) を返す（= 未宣言・periodic のみ）。ただし
	//     宣言制の主経路は Watcher への型アサーションであり、この (nil, nil) 返却は
	//     「Watcher を実装しつつ実行時に watch できない」場合の退避経路である。
	//   - ヒントは revision+scope のみ（truth を運ばない）。
	//   - ctx のキャンセルでチャネルを閉じてよい。消費側はチャネル close を購読終了と
	//     みなす。
	Watch(ctx context.Context) (<-chan core.ChangeHint, error)
}

// DirSource は既存の LoadDir を DesiredSource として包む実装である。dir 上の JSON
// intent を読む。Watch は実装しない（= watch 非対応・periodic のみ）。
type DirSource struct {
	// Dir は intent JSON を含むディレクトリ。
	Dir string
}

// Load は LoadDir に委譲する。
func (s DirSource) Load(ctx context.Context) ([]diff.DesiredResource, core.RevisionInfo, error) {
	return LoadDir(s.Dir)
}

// 型アサーションの意図を明示する（DirSource は DesiredSource を満たす）。
var _ DesiredSource = DirSource{}
