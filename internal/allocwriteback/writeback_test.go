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

package allocwriteback

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/allocator"
	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/pkg/allocation"
)

func newRig(t *testing.T) (*Authority, *allocator.Cache, string) {
	t.Helper()
	root := t.TempDir()
	store := allocator.New(filepath.Join(t.TempDir(), "alloc.json"))
	cache := allocator.NewCache(store)
	a := New(cache, Config{Mode: ModeFile}, filepath.Join(root, "intent"))
	if err := a.EnsurePool("p", 1, 5); err != nil {
		t.Fatal(err)
	}
	a.SetIntentPools([]loader.Pool{{Name: "p", Lo: 1, Hi: 5, WriteBack: true}, {Name: "q", Lo: 1, Hi: 5}})
	return a, cache, root
}

// The steady state asks the store nothing: the write-back sits in front of
// the Lookup cache and the file is local (§19.6).
func TestSteadyStateNeedsNoStoreCall(t *testing.T) {
	ctx := context.Background()
	a, cache, _ := newRig(t)
	if v, err := a.Allocate(ctx, "p", "o1"); err != nil || v != "1" {
		t.Fatalf("allocate: %q %v", v, err)
	}
	a.SetIntentPools([]loader.Pool{{Name: "p", Lo: 1, Hi: 5, WriteBack: true}})
	if v, err := a.Allocate(ctx, "p", "o1"); err != nil || v != "1" { // fills the cache
		t.Fatalf("again: %q %v", v, err)
	}
	before := cache.InnerCalls()
	for i := 0; i < 3; i++ {
		a.SetIntentPools([]loader.Pool{{Name: "p", Lo: 1, Hi: 5, WriteBack: true}})
		if v, err := a.Allocate(ctx, "p", "o1"); err != nil || v != "1" {
			t.Fatalf("cycle %d: %q %v", i, v, err)
		}
	}
	if n := cache.InnerCalls() - before; n != 0 {
		t.Fatalf("the steady state must not reach the store, got %d call(s)", n)
	}
}

// A file entry the store lacks blocks new values from the pool: the store
// would hand that value out again.  Pools without write_back pass through.
func TestStoreMissingEntryBlocksPool(t *testing.T) {
	ctx := context.Background()
	a, _, _ := newRig(t)
	f := loader.AllocationFile{Pool: "p", Allocations: []loader.AllocationEntry{{Owner: "lost", Value: 1}}}
	if err := os.MkdirAll(filepath.Dir(a.FilePath("p")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.FilePath("p"), loader.EmitAllocationFile(f), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Allocate(ctx, "p", "new"); !errors.Is(err, allocation.ErrWriteBackHeld) || !errors.Is(err, ErrNotInStore) {
		t.Fatalf("a new owner must be held while the store lacks a file entry, got %v", err)
	}
	if _, err := a.Allocate(ctx, "p", "lost"); !errors.Is(err, ErrNotInStore) {
		t.Fatalf("the lost owner must be held, got %v", err)
	}
	if _, ok, err := a.Lookup(ctx, "p", "new"); ok || !errors.Is(err, ErrNotInStore) {
		t.Fatalf("Lookup must show the same hold, got ok=%v err=%v", ok, err)
	}
	// q is not declared to the store: the store's own error passes through.
	if _, err := a.Allocate(ctx, "q", "x"); !errors.Is(err, allocation.ErrUnknownPool) {
		t.Fatalf("pool q must pass through to the store, got %v", err)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wb.yaml")
	if err := os.WriteFile(path, []byte("mode: review\nrepo: repo\nssh_key: key\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Mode != ModeReview || c.Path != DefaultPath || c.Branch != DefaultBranch || c.Repo != filepath.Join(dir, "repo") || c.SSHKey != filepath.Join(dir, "key") {
		t.Fatalf("config: %+v", c)
	}
	for _, bad := range []string{"mode: push\n", "unknown: x\n", "committer_name: x\n"} {
		_ = os.WriteFile(path, []byte(bad), 0o644)
		if _, err := LoadConfig(path); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if c, err := LoadConfig(""); err != nil || c.Mode != ModeCommit {
		t.Fatalf("defaults: %+v %v", c, err)
	}
}
