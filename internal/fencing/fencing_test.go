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

package fencing

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// 同時の Acquire は世代を飛ばさず重複させない（ロックで直列化される）。
func TestFileStore_ConcurrentAcquireIsMonotonic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fencing.json")
	const n = 20
	gens := make(chan int64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// プロセスを跨ぐ場合と同じく、別々の FileStore 値で同じファイルを使う。
			tok, err := NewFileStore(path).Acquire(context.Background(), "site=tokyo", fmt.Sprintf("s%d", i))
			if err != nil {
				t.Error(err)
				return
			}
			gens <- tok.Generation
		}(i)
	}
	wg.Wait()
	close(gens)
	seen := map[int64]bool{}
	for g := range gens {
		if seen[g] {
			t.Fatalf("generation %d handed out twice", g)
		}
		seen[g] = true
	}
	for g := int64(1); g <= n; g++ {
		if !seen[g] {
			t.Fatalf("generation %d missing; got %v", g, seen)
		}
	}
}

// desired に無い scope（prune だけが触る scope）は、他シャードの記録を奪わない。
func TestGuard_CheckDoesNotTakeOverForeignScope(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(filepath.Join(t.TempDir(), "fencing.json"))
	scope := core.NewScope(map[string]string{"site": "osaka"})
	if _, err := store.Acquire(ctx, "site=osaka", "shard-osaka"); err != nil {
		t.Fatal(err)
	}
	g := NewGuard(store, "shard-tokyo")
	if err := g.Check(ctx, scope); !IsFenced(err) {
		t.Fatalf("Check on a foreign scope = %v; want FencedError", err)
	}
	tok, _, _ := store.Current(ctx, "site=osaka")
	if tok.Holder != "shard-osaka" || tok.Generation != 1 {
		t.Fatalf("foreign scope token changed: %+v", tok)
	}
	// 記録の無い scope は取得してよい。
	if err := g.Check(ctx, core.NewScope(nil)); err != nil {
		t.Fatalf("Check on an unclaimed scope: %v", err)
	}
	if tok, ok, _ := store.Current(ctx, RootKey); !ok || tok.Holder != "shard-tokyo" {
		t.Fatalf("root scope token=%+v ok=%v; want shard-tokyo", tok, ok)
	}
}
