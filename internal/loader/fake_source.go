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

	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// FakeWatchSource は早期収束 seam（DesiredSource + Watcher）の受け入れテスト用の
// in-memory 実装である。Watch は与えられた ChangeHint 群をチャネルに流して閉じる。
//
// 実バルク/実 webhook 連携は v1b の実装であり、この fake は seam が到達可能である
// こと（fake が Watch を宣言し、消費側がヒントを受け取れること）を証明するための
// 最小実装に留める。
type FakeWatchSource struct {
	// Resources は Load が返す desired resource 群。
	Resources []diff.DesiredResource
	// Revision は Load が返す revision 情報。
	Revision core.RevisionInfo
	// Hints は Watch がチャネルに順に流す ChangeHint 群。
	Hints []core.ChangeHint
}

// Load は保持している desired と revision をそのまま返す。
func (s *FakeWatchSource) Load(ctx context.Context) ([]diff.DesiredResource, core.RevisionInfo, error) {
	return s.Resources, s.Revision, nil
}

// Watch は Hints を順にチャネルへ流してから閉じる。ctx がキャンセルされたら送信を
// 中断してチャネルを閉じる。ヒントは revision+scope のみを運ぶ（truth を運ばない）。
func (s *FakeWatchSource) Watch(ctx context.Context) (<-chan core.ChangeHint, error) {
	ch := make(chan core.ChangeHint)
	go func() {
		defer close(ch)
		for _, h := range s.Hints {
			select {
			case <-ctx.Done():
				return
			case ch <- h:
			}
		}
	}()
	return ch, nil
}

// 型アサーションの意図を明示する（FakeWatchSource は DesiredSource と Watcher を満たす）。
var (
	_ DesiredSource = (*FakeWatchSource)(nil)
	_ Watcher       = (*FakeWatchSource)(nil)
)

// FakePeriodicSource は Watch を実装しない DesiredSource（= watch 非対応・periodic
// のみ）の fake である。Watcher への型アサーションが失敗することを受け入れテストで
// 確かめるために使う。
type FakePeriodicSource struct {
	Resources []diff.DesiredResource
	Revision  core.RevisionInfo
}

// Load は保持している desired と revision をそのまま返す。
func (s *FakePeriodicSource) Load(ctx context.Context) ([]diff.DesiredResource, core.RevisionInfo, error) {
	return s.Resources, s.Revision, nil
}

// 型アサーションの意図を明示する（FakePeriodicSource は DesiredSource のみを満たす）。
var _ DesiredSource = (*FakePeriodicSource)(nil)
