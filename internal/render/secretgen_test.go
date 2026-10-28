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

package render

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

type fakeGen struct {
	gens  map[string]int64
	calls map[string]int
}

func (f *fakeGen) Generation(_ context.Context, path string) (int64, error) {
	f.calls[path]++
	g, ok := f.gens[path]
	if !ok {
		return 0, errors.New("sealed")
	}
	return g, nil
}

func (f *fakeGen) Resolve(context.Context, string, int64) (string, error) { return "v", nil }

func TestResolveSecretGenerations(t *testing.T) {
	be := &fakeGen{gens: map[string]int64{"app/db#password": 4}, calls: map[string]int{}}
	id := func(l string) core.ID { return core.NewID("kvfile", core.NewScope(nil), l) }
	in := []diff.DesiredResource{
		{ID: id("a"), Fields: map[string]string{"token": "secretref:vault/app/db#password", "x": "plain"}},
		{ID: id("b"), Fields: map[string]string{"token": "secretref:vault/app/db#password"}},
		{ID: id("pinned"), Fields: map[string]string{"token": "secretref:vault/app/db#password#gen2"}},
		{ID: id("file"), Fields: map[string]string{"token": "secretref:file/app/db"}},
		{ID: id("down"), Fields: map[string]string{"token": "secretref:vault/other"}},
	}
	res := ResolveSecretGenerations(context.Background(), map[string]GenerationalSecretBackend{"vault": be}, in)
	want := map[string]string{
		"a": "secretref:vault/app/db#password#gen4", "b": "secretref:vault/app/db#password#gen4",
		"pinned": "secretref:vault/app/db#password#gen2", "file": "secretref:file/app/db", "down": "secretref:vault/other",
	}
	for _, r := range res.Resources {
		if r.Fields["token"] != want[r.ID.Local] {
			t.Errorf("%s: token = %q, want %q", r.ID.Local, r.Fields["token"], want[r.ID.Local])
		}
	}
	if in[0].Fields["token"] != "secretref:vault/app/db#password" {
		t.Errorf("the input was modified")
	}
	if be.calls["app/db#password"] != 1 {
		t.Errorf("generation read %d times, want once per call", be.calls["app/db#password"])
	}
	if !reflect.DeepEqual(res.Inputs, []string{"secret:vault/app/db#password=4"}) {
		t.Errorf("Inputs = %v", res.Inputs)
	}
	if len(res.Unresolved) != 1 || res.Unresolved[0].ID.Local != "down" || res.Unresolved[0].Field != "token" {
		t.Fatalf("Unresolved = %+v", res.Unresolved)
	}
	unk := res.UnknownLocals(map[string][]string{"down": {"down-child"}})
	if !unk["down"] || !unk["down-child"] || len(unk) != 2 {
		t.Errorf("UnknownLocals = %v", unk)
	}

	// No backend configured: the resources pass through as written.
	if got := ResolveSecretGenerations(context.Background(), nil, in); !reflect.DeepEqual(got.Resources, in) || len(got.Unresolved) != 0 {
		t.Errorf("no backends: %+v", got)
	}
}

type stubBackend struct{ fakeGen }

func (s *stubBackend) Resolve(_ context.Context, path string, gen int64) (string, error) {
	return path + "@" + string(rune('0'+gen)), nil
}

func TestFileSecretBackend_DelegatesConfigured(t *testing.T) {
	UseSecretBackends(map[string]GenerationalSecretBackend{"vault": &stubBackend{}})
	defer UseSecretBackends(nil)
	b := NewFileSecretBackend("")
	got, err := b.ResolveValue(context.Background(), "vault", "app/db#pw", 3)
	if err != nil || got != "app/db#pw@3" {
		t.Errorf("ResolveValue(vault) = %q, %v", got, err)
	}
	if _, err := b.ResolveValue(context.Background(), "file", "app/db", 1); err == nil {
		t.Errorf("a file ref without a store must fail")
	}
}
