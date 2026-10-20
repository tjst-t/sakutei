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

package gitdist

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/tjst-t/sakutei/internal/reconcile"
	"github.com/tjst-t/sakutei/pkg/core"
)

// DefaultPoll は拠点がミラーを見に行く既定の間隔（§17.4 の算術: 300 拠点 × 60s）。
const DefaultPoll = 60 * time.Second

// Source は git 配布の intent を常駐ループに渡す reconcile.Source。
//
// 毎周、Poll（+ jitter）が過ぎていれば Remote から取得して手元写しを揃え、
// claim を決め、sparse checkout の cone を claim のパスに合わせる。
//
//   - Shard を指定すると claim は git 上の shards/<Shard>.yaml から読む（§17.1）。
//     claim 変更 PR がマージされると、次の取得で claim が変わる（§17.7 ハンドオフ）。
//     shards/dr/ の DR plan が enabled: true になると、claim はその plan に従って
//     移る（§25.1 (a) DR intent のプリステージ。ResolveClaim）。
//   - Shard が空なら Claim（--claim）を固定で使う。
//
// 取得に失敗しても、手元写しがあればそれで収束を続ける（Stale）。手元写しが一度も
// できていない、claim の intent/<scope>/ がコミットに無い、拠点の資格が局所でない、
// のいずれかなら error を返し、常駐ループはその周を飛ばす（空の desired で収束しない）。
type Source struct {
	Checkout *Checkout
	// Shard はシャード名（shards/<Shard>.yaml）。空なら Claim を使う。
	Shard string
	// Claim は Shard が空のときの固定の claim。
	Claim core.Scope
	// SecretStore は拠点ローカルの file SecretBackend（§20.5）。空なら確かめない。
	SecretStore string
	// Poll は取得の間隔。0 なら DefaultPoll。実際の間隔は Poll〜1.5×Poll の jitter 付き。
	Poll time.Duration

	nextFetch time.Time
	ready     bool
	state     reconcile.SourceState
}

var _ reconcile.Source = (*Source)(nil)

// IntentDir は常駐ループが読む intent の木（手元写しの intent/）を返す。
func (s *Source) IntentDir() string {
	return filepath.Join(s.Checkout.Dir, IntentRoot)
}

// Refresh implements reconcile.Source.
func (s *Source) Refresh(ctx context.Context) (reconcile.SourceState, error) {
	now := time.Now()
	if s.ready && now.Before(s.nextFetch) {
		st := s.state
		st.Stale, st.BreakGlass = nil, nil
		return st, nil
	}
	s.nextFetch = now.Add(s.jitteredPoll())

	sparse, err := s.wantSparse(ctx)
	if err != nil {
		return s.fail(err)
	}
	var stale error
	if err := s.Checkout.Sync(ctx, sparse); err != nil {
		if !s.Checkout.Cloned() || s.Checkout.Head(ctx) == "" {
			return s.fail(fmt.Errorf("first pull from %s failed: %w", s.Checkout.Remote, err))
		}
		stale = err
	}

	st, err := s.resolve(ctx)
	if err != nil {
		return s.fail(err)
	}
	// 前回の取得から新しく来た break-glass コミットを拾う（§25.1 (b) 必ず検出される）。
	if s.state.Revision != "" && st.Revision != s.state.Revision {
		st.BreakGlass = BreakGlassCommits(ctx, s.Checkout.Dir, s.state.Revision, st.Revision)
	}
	s.state, s.ready = st, true
	st.Stale = stale
	return st, nil
}

// fail はこの周を飛ばさせる。次の周にもう一度取りに行く。
func (s *Source) fail(err error) (reconcile.SourceState, error) {
	s.ready = false
	return reconcile.SourceState{}, err
}

// wantSparse は取得前の cone を返す。claim を git から読む場合、claim が分かるまでは
// shards/ だけ（と現在の cone）を取る。
func (s *Source) wantSparse(ctx context.Context) ([]string, error) {
	if s.Shard == "" {
		return SparsePaths(s.Claim)
	}
	if s.Checkout.Cloned() {
		if cur, err := s.Checkout.SparsePaths(ctx); err == nil && slices.Contains(cur, ShardsDir) {
			return cur, nil
		}
	}
	return []string{ShardsDir}, nil
}

// resolve は手元写しから claim を決め、cone を claim のパスに合わせ、claim の
// intent と拠点の資格を確かめる。
func (s *Source) resolve(ctx context.Context) (reconcile.SourceState, error) {
	st := reconcile.SourceState{Claim: s.Claim, Revision: s.Checkout.Head(ctx)}
	if s.Shard != "" {
		sc, plan, err := ResolveClaim(s.Checkout.Dir, s.Shard)
		if err != nil {
			return st, err
		}
		st.DRPlan = plan
		if sc.Idle {
			st.Claim, st.Idle = core.Scope{}, true
			// claim を持たないシャードは shards/ 以外を手元に置かない。
			if err := s.Checkout.SetSparse(ctx, []string{ShardsDir}); err != nil {
				return st, err
			}
			return st, nil
		}
		st.Claim = sc.Claim
	}
	want, err := SparsePaths(st.Claim)
	if err != nil {
		return st, err
	}
	if cur, err := s.Checkout.SparsePaths(ctx); err != nil || !slices.Equal(cur, sortedCopy(want)) {
		if err := s.Checkout.SetSparse(ctx, want); err != nil {
			return st, err
		}
	}
	dir, _ := ScopeDir(st.Claim)
	if fi, err := os.Stat(filepath.Join(s.Checkout.Dir, filepath.FromSlash(dir))); err != nil || !fi.IsDir() {
		return st, fmt.Errorf("claim %s has no %s/ at revision %s; refusing to converge an empty desired", st.Claim, dir, st.Revision)
	}
	if s.SecretStore != "" {
		if err := CheckSecretLocality(s.SecretStore, st.Claim); err != nil {
			return st, err
		}
	}
	return st, nil
}

func (s *Source) jitteredPoll() time.Duration {
	p := s.Poll
	if p <= 0 {
		p = DefaultPoll
	}
	n := int64(p) / 2
	if n <= 0 {
		return p
	}
	return p + time.Duration(rand.Int63n(n))
}

func sortedCopy(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}
