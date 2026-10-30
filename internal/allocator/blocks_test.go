// Copyright 2026 tjst-t
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package allocator

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/pkg/allocation"
)

// blockAuthority is what FileAuthority and Client share for #206.
type blockAuthority interface {
	DeclarePool(name string, spec allocation.PoolSpec) error
	ImportPool(ctx context.Context, pool string, spec allocation.PoolSpec, entries []ImportEntry, last map[string]int) (int, error)
	Allocate(ctx context.Context, pool, owner string) (string, error)
	Release(ctx context.Context, pool, owner string) error
	Lookup(ctx context.Context, pool, owner string) (string, bool, error)
}

var blockSpec = allocation.PoolSpec{Lo: 1, Hi: 100, NoReuse: true, Blocks: []allocation.Block{
	{Name: "a", Lo: 1, Hi: 10}, {Name: "b", Lo: 50, Hi: 59},
}}

func checkBlocks(t *testing.T, x blockAuthority) {
	t.Helper()
	ctx := context.Background()
	if err := x.DeclarePool("p", blockSpec); err != nil {
		t.Fatal(err)
	}
	if v, err := x.Allocate(ctx, "p/b", "o1"); err != nil || v != "50" {
		t.Fatalf("block b: %q %v", v, err)
	}
	if _, err := x.Allocate(ctx, "p", "o2"); !errors.Is(err, allocation.ErrUnknownPool) {
		t.Fatalf("a pool with blocks needs one named: %v", err)
	}
	if _, err := x.Allocate(ctx, "p/c", "o2"); !errors.Is(err, allocation.ErrUnknownPool) {
		t.Fatalf("an undeclared block: %v", err)
	}
	// owners are unique across the pool: the base name finds them.
	if v, ok, err := x.Lookup(ctx, "p", "o1"); err != nil || !ok || v != "50" {
		t.Fatalf("lookup: %q %v %v", v, ok, err)
	}
	if err := x.Release(ctx, "p", "o1"); err != nil {
		t.Fatal(err)
	}
	// the released 50 is retired: the import may not bring it back, and a
	// last value of a block the pool does not declare is refused.
	if _, err := x.ImportPool(ctx, "p", blockSpec, []ImportEntry{{Owner: "o1", Value: 50}}, nil); !errors.Is(err, ErrImportConflict) || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("a retired value must be refused: %v", err)
	}
	if _, err := x.ImportPool(ctx, "p", blockSpec, nil, map[string]int{"z": 3}); !errors.Is(err, ErrImportConflict) {
		t.Fatalf("a last value of an undeclared block must be refused: %v", err)
	}
	if _, err := x.ImportPool(ctx, "p", blockSpec, []ImportEntry{{Owner: "o9", Value: 30}}, nil); !errors.Is(err, ErrImportConflict) || !strings.Contains(err.Error(), "no block") {
		t.Fatalf("a value in no block must be refused: %v", err)
	}
	// the file's last value of block a raises the store's.
	if n, err := x.ImportPool(ctx, "p", blockSpec, []ImportEntry{{Owner: "o3", Value: 2}}, map[string]int{"a": 7}); err != nil || n != 1 {
		t.Fatalf("import: %d %v", n, err)
	}
	if v, err := x.Allocate(ctx, "p/a", "o4"); err != nil || v != "8" {
		t.Fatalf("block a hands out above the imported last value 7: %q %v", v, err)
	}
	if v, err := x.Allocate(ctx, "p/b", "o5"); err != nil || v != "51" {
		t.Fatalf("block b goes on above the retired 50: %q %v", v, err)
	}
	// a block's range may not drop a value taken from it.
	shrunk := blockSpec
	shrunk.Blocks = []allocation.Block{{Name: "a", Lo: 1, Hi: 5}, {Name: "b", Lo: 50, Hi: 59}}
	if err := x.DeclarePool("p", shrunk); err == nil {
		t.Fatal("shrinking block a over 8 must be refused")
	}
}

func TestBlocks_File(t *testing.T) {
	checkBlocks(t, New(filepath.Join(t.TempDir(), "alloc.json")))
}

func TestBlocks_HTTP(t *testing.T) {
	srv := NewServer(New(filepath.Join(t.TempDir(), "alloc.json")))
	if _, err := srv.Promote(); err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv)
	defer hs.Close()
	checkBlocks(t, NewClient([]string{hs.URL}))
}

// TestDeclarePool_TurningReuseOff starts the last value above what the pool
// holds or cools down.
func TestDeclarePool_TurningReuseOff(t *testing.T) {
	ctx := context.Background()
	a := New(filepath.Join(t.TempDir(), "alloc.json"))
	a.Cooldown = time.Nanosecond // 1 and 3 would be free again at once
	if err := a.EnsurePool("p", 1, 10); err != nil {
		t.Fatal(err)
	}
	for _, o := range []string{"x", "y", "z"} {
		if _, err := a.Allocate(ctx, "p", o); err != nil {
			t.Fatal(err)
		}
	}
	_ = a.Release(ctx, "p", "x")
	_ = a.Release(ctx, "p", "z")
	if err := a.DeclarePool("p", allocation.PoolSpec{Lo: 1, Hi: 10, NoReuse: true}); err != nil {
		t.Fatal(err)
	}
	if v, err := a.Allocate(ctx, "p", "w"); err != nil || v != "4" {
		t.Fatalf("after turning reuse off the next value is above the released 3, not 1: %q %v", v, err)
	}
}
