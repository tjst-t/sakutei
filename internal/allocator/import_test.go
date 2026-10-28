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
)

// importer is what FileAuthority and Client share for Import.
type importer interface {
	Import(ctx context.Context, pool string, lo, hi int, entries []ImportEntry) (int, error)
	Lookup(ctx context.Context, pool, owner string) (string, bool, error)
	Allocate(ctx context.Context, pool, owner string) (string, error)
	Release(ctx context.Context, pool, owner string) error
}

func checkImport(t *testing.T, a importer) {
	t.Helper()
	ctx := context.Background()
	entries := []ImportEntry{{Owner: "a", Value: 105}, {Owner: "b", Value: 100}}
	if n, err := a.Import(ctx, "p", 100, 110, entries); err != nil || n != 2 {
		t.Fatalf("import: %d %v", n, err)
	}
	if v, ok, _ := a.Lookup(ctx, "p", "a"); !ok || v != "105" {
		t.Fatalf("a: %q %v", v, ok)
	}
	// The imported values are taken: a new owner gets the lowest free one.
	if v, _ := a.Allocate(ctx, "p", "c"); v != "101" {
		t.Fatalf("new owner after import: %q, want 101", v)
	}
	if n, err := a.Import(ctx, "p", 100, 110, entries); err != nil || n != 0 {
		t.Fatalf("re-import: %d %v", n, err)
	}
	// Disagreements are refused, and nothing of the batch is written.
	for name, bad := range map[string][]ImportEntry{
		"other value":  {{Owner: "d", Value: 107}, {Owner: "a", Value: 106}},
		"value taken":  {{Owner: "d", Value: 107}, {Owner: "e", Value: 101}},
		"out of range": {{Owner: "d", Value: 107}, {Owner: "e", Value: 111}},
	} {
		if _, err := a.Import(ctx, "p", 100, 110, bad); !errors.Is(err, ErrImportConflict) {
			t.Fatalf("%s: want ErrImportConflict, got %v", name, err)
		}
		if _, ok, _ := a.Lookup(ctx, "p", "d"); ok {
			t.Fatalf("%s: a refused import must write nothing", name)
		}
	}
	if err := a.Release(ctx, "p", "c"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Import(ctx, "p", 100, 110, []ImportEntry{{Owner: "d", Value: 101}}); !errors.Is(err, ErrImportConflict) || !strings.Contains(err.Error(), "cools down") {
		t.Fatalf("a value in cooldown must be refused, got %v", err)
	}
}

func TestImport_File(t *testing.T) {
	checkImport(t, New(filepath.Join(t.TempDir(), "alloc.json")))
}

func TestImport_Service(t *testing.T) {
	srv := NewServer(New(filepath.Join(t.TempDir(), "alloc.json")))
	if _, err := srv.Promote(); err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv)
	defer hs.Close()
	checkImport(t, NewClient([]string{hs.URL}))
}
