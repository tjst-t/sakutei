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

// Package acceptance / change_hint_test.go covers Story Sf6a62f-1: the
// ChangeHint type and the DesiredSource.Watch capability seam.
//
// AC tags:
//
//	AC-Sf6a62f-1-1  ChangeHint carries revision+scope only, never truth/State.
//	AC-Sf6a62f-1-2  DesiredSource.Watch is an optional capability (declaration
//	                制): a fake Watcher emits a hint on a channel; a non-watch
//	                source fails the Watcher type assertion.
//	AC-Sf6a62f-1-3  CoalesceHints unions the touched scopes (load-bearing) and
//	                carries the LAST hint's intent_revision as an OPAQUE tag.
//	                intent_revision is content-addressed (SHA-256 hex), NOT
//	                monotone, so no lexical-max is assumed (FIX #1).
package acceptance

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// ---------------------------------------------------------------------------
// AC-Sf6a62f-1-1: ChangeHint carries revision + scope ONLY (no truth/State).
// ---------------------------------------------------------------------------

// TestChangeHint_CarriesRevisionAndScopeOnly verifies via reflection that
// core.ChangeHint has exactly two fields — IntentRevision (string) and
// TouchedScopes ([]core.Scope) — and no field capable of carrying truth
// (State / value / diff / observation).  This is the type-level guarantee that
// the hint cannot leak the observation-state-less invariant (§8.6, ADR-0009).
//
// [AC-Sf6a62f-1-1]
func TestChangeHint_CarriesRevisionAndScopeOnly(t *testing.T) {
	// [AC-Sf6a62f-1-1]
	typ := reflect.TypeOf(core.ChangeHint{})

	if got := typ.NumField(); got != 2 {
		t.Fatalf("[AC-Sf6a62f-1-1] ChangeHint must have exactly 2 fields (revision+scope only); got %d. "+
			"Adding any value/State/diff field breaks the observation-state-less invariant.", got)
	}

	rev, ok := typ.FieldByName("IntentRevision")
	if !ok {
		t.Fatalf("[AC-Sf6a62f-1-1] ChangeHint missing IntentRevision field")
	}
	if rev.Type.Kind() != reflect.String {
		t.Errorf("[AC-Sf6a62f-1-1] IntentRevision must be string; got %s", rev.Type)
	}

	scopes, ok := typ.FieldByName("TouchedScopes")
	if !ok {
		t.Fatalf("[AC-Sf6a62f-1-1] ChangeHint missing TouchedScopes field")
	}
	want := reflect.TypeOf([]core.Scope{})
	if scopes.Type != want {
		t.Errorf("[AC-Sf6a62f-1-1] TouchedScopes must be []core.Scope; got %s", scopes.Type)
	}

	// Guard against any future field whose name hints at carrying truth.
	forbidden := []string{"State", "Value", "Values", "Diff", "Observation",
		"Actual", "Fields", "Payload", "Data"}
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		for _, bad := range forbidden {
			if name == bad {
				t.Errorf("[AC-Sf6a62f-1-1] ChangeHint has forbidden truth-carrying field %q; "+
					"hints must carry revision+scope only", name)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// AC-Sf6a62f-1-2: DesiredSource.Watch is an optional (declaration-制) capability.
// ---------------------------------------------------------------------------

// TestDesiredSource_WatchCapabilityDeclared verifies that a fake DesiredSource
// which implements Watcher can be discovered via type assertion (same pattern
// as Lister) and emits ChangeHints on a channel; a consumer receives them.
//
// [AC-Sf6a62f-1-2]
func TestDesiredSource_WatchCapabilityDeclared(t *testing.T) {
	// [AC-Sf6a62f-1-2]
	sc := core.NewScope(map[string]string{"site": "tokyo"})
	var src loader.DesiredSource = &loader.FakeWatchSource{
		Hints: []core.ChangeHint{
			{IntentRevision: "rev-1", TouchedScopes: []core.Scope{sc}},
		},
	}

	// Declaration-制: discover the optional capability via type assertion.
	w, ok := src.(loader.Watcher)
	if !ok {
		t.Fatalf("[AC-Sf6a62f-1-2] FakeWatchSource must satisfy loader.Watcher (declared capability)")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := w.Watch(ctx)
	if err != nil {
		t.Fatalf("[AC-Sf6a62f-1-2] Watch returned error: %v", err)
	}
	if ch == nil {
		t.Fatalf("[AC-Sf6a62f-1-2] a declared Watcher must return a non-nil channel")
	}

	select {
	case got, open := <-ch:
		if !open {
			t.Fatalf("[AC-Sf6a62f-1-2] channel closed before delivering a hint")
		}
		if got.IntentRevision != "rev-1" {
			t.Errorf("[AC-Sf6a62f-1-2] expected hint IntentRevision=rev-1; got %q", got.IntentRevision)
		}
		if len(got.TouchedScopes) != 1 || !got.TouchedScopes[0].Equal(sc) {
			t.Errorf("[AC-Sf6a62f-1-2] expected hint TouchedScopes=[%s]; got %v", sc, got.TouchedScopes)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("[AC-Sf6a62f-1-2] timed out waiting for a ChangeHint on the watch channel")
	}
}

// TestDesiredSource_NonWatchIsUndeclared verifies that a DesiredSource which
// does NOT implement Watcher is treated as "watch 非対応" — the Watcher type
// assertion fails, so the loop falls back to periodic (Load only).
//
// [AC-Sf6a62f-1-2]
func TestDesiredSource_NonWatchIsUndeclared(t *testing.T) {
	// [AC-Sf6a62f-1-2]
	var src loader.DesiredSource = &loader.FakePeriodicSource{}

	if _, ok := src.(loader.Watcher); ok {
		t.Fatalf("[AC-Sf6a62f-1-2] FakePeriodicSource must NOT satisfy loader.Watcher (watch 非対応)")
	}

	// It must still be a usable DesiredSource (periodic Load works).
	if _, _, err := src.Load(context.Background()); err != nil {
		t.Fatalf("[AC-Sf6a62f-1-2] non-watch source Load must still work: %v", err)
	}
}

// TestDesiredSource_DirSourceIsNonWatch verifies the production DirSource is a
// DesiredSource but does NOT declare Watch (periodic-only in v1a).
//
// [AC-Sf6a62f-1-2]
func TestDesiredSource_DirSourceIsNonWatch(t *testing.T) {
	// [AC-Sf6a62f-1-2]
	var src loader.DesiredSource = loader.DirSource{Dir: t.TempDir()}
	if _, ok := src.(loader.Watcher); ok {
		t.Errorf("[AC-Sf6a62f-1-2] DirSource must not declare Watch in v1a (periodic only)")
	}
}

// ---------------------------------------------------------------------------
// AC-Sf6a62f-1-3: CoalesceHints — scopes unioned (load-bearing); revision is an
// opaque last-wins tag (NOT lexical-max; revisions are content-addressed hashes,
// which are non-monotone).  FIX #1.
// ---------------------------------------------------------------------------

// Real-looking, content-addressed (SHA-256 hex) revision tags.  These are
// deliberately NON-monotone under string comparison: revB < revA lexically even
// though revB is received LAST.  A lexical-max implementation would (wrongly)
// return revA; the correct last-wins implementation returns revB.  This proves
// CoalesceHints does not assume revision monotonicity.
const (
	// starts with 'c' → lexically the LARGEST of the three.
	revA = "ca97b1f0c8d5e6a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6"
	// starts with '3' → lexically the SMALLEST, but received LAST below.
	revB = "3e23a1b2c3d4e5f60718293a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b3c"
	// starts with '8' → lexically middle.
	revC = "8f1c2d3e4a5b6c7d8e9f0a1b2c3d4e5f60718293a4b5c6d7e8f9a0b1c2d3e4f5"
)

// TestCoalesceHints_LastRevisionWinsAndScopesUnioned verifies the
// level-triggered coalesce semantics: the touched scopes are unioned across ALL
// hints (load-bearing), and IntentRevision carries the LAST received hint's
// revision as an opaque tag — NOT the lexical maximum.  Dropping intermediate
// hints is safe because hints carry no truth (the next Observe sees final
// reality); the revision is only a tag for logging/expect-revision.
//
// [AC-Sf6a62f-1-3]
func TestCoalesceHints_LastRevisionWinsAndScopesUnioned(t *testing.T) {
	// [AC-Sf6a62f-1-3]
	tokyo := core.NewScope(map[string]string{"site": "tokyo"})
	osaka := core.NewScope(map[string]string{"site": "osaka"})

	// revA (lexically LARGEST) arrives first; revB (lexically SMALLEST) arrives
	// LAST.  A lexical-max implementation would return revA — the wrong answer
	// under a non-monotone (hash) revision format.  last-wins returns revB.
	hints := []core.ChangeHint{
		{IntentRevision: revA, TouchedScopes: []core.Scope{tokyo}},
		{IntentRevision: revC, TouchedScopes: []core.Scope{osaka}},
		{IntentRevision: revB, TouchedScopes: []core.Scope{tokyo, osaka}},
	}

	out := core.CoalesceHints(hints...)

	if out.IntentRevision != revB {
		t.Errorf("[AC-Sf6a62f-1-3] revision must be the LAST hint's (opaque last-wins), not lexical-max; "+
			"want %q (last), got %q (a lexical-max impl would return %q)", revB, out.IntentRevision, revA)
	}

	// Union of {tokyo}, {osaka}, {tokyo, osaka} = {tokyo, osaka} (deduped).
	// This is the load-bearing property and must hold regardless of revision.
	if len(out.TouchedScopes) != 2 {
		t.Fatalf("[AC-Sf6a62f-1-3] scopes must be unioned & deduped to 2; got %d: %v",
			len(out.TouchedScopes), out.TouchedScopes)
	}
	seen := map[string]bool{}
	for _, sc := range out.TouchedScopes {
		seen[sc.String()] = true
	}
	if !seen[tokyo.String()] || !seen[osaka.String()] {
		t.Errorf("[AC-Sf6a62f-1-3] union must contain both tokyo and osaka; got %v", out.TouchedScopes)
	}
}

// TestCoalesceHints_LastWinsUnderNonMonotoneRevisions isolates the FIX #1
// invariant: with content-addressed (non-monotone) revisions, the LAST hint's
// revision is carried — a lexically larger earlier revision must NOT clobber it.
//
// [AC-Sf6a62f-1-3]
func TestCoalesceHints_LastWinsUnderNonMonotoneRevisions(t *testing.T) {
	// [AC-Sf6a62f-1-3]
	// revA is lexically LARGER than revB (revA starts 'c', revB starts '3'),
	// yet revB is received LAST, so revB must win.  If this returns revA, the
	// implementation has (wrongly) reintroduced a lexical-max/monotonicity
	// assumption over a hash-shaped, non-monotone revision format.
	out := core.CoalesceHints(
		core.ChangeHint{IntentRevision: revA},
		core.ChangeHint{IntentRevision: revB},
	)
	if out.IntentRevision != revB {
		t.Errorf("[AC-Sf6a62f-1-3] last-wins violated: an earlier lexically-larger revision clobbered "+
			"the last one; want %q (last), got %q", revB, out.IntentRevision)
	}
}

// TestCoalesceHints_Empty verifies the empty input degenerate case.
//
// [AC-Sf6a62f-1-3]
func TestCoalesceHints_Empty(t *testing.T) {
	// [AC-Sf6a62f-1-3]
	out := core.CoalesceHints()
	if out.IntentRevision != "" {
		t.Errorf("[AC-Sf6a62f-1-3] empty coalesce must yield empty revision; got %q", out.IntentRevision)
	}
	if len(out.TouchedScopes) != 0 {
		t.Errorf("[AC-Sf6a62f-1-3] empty coalesce must yield no scopes; got %v", out.TouchedScopes)
	}
}
