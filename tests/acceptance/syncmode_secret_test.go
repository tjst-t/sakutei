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

// syncmode_secret_test.go — Backlog #97 (trial of Sync Mode and import,
// docs/trial/sync-import).  No built-in provider has a secret field, so the
// trial could not check by hand that Sync keeps secrets out of git; this does.
//
// Tags:
//   [#97-1] Sync Mode never writes a plaintext secret into its snapshot nor
//           into any commit, and a change of the secret alone makes no commit
//           (it is not recorded); a SecretRef is recorded in reference form
//   [#97-2] an incomplete List is named in the error ("partial"), not shown
//           as a number

package acceptance

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/importer"
	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/syncmode"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// scopedSecretProvider is secretProvider with its resource in site=legacy, so
// a sync scope can take it in.
type scopedSecretProvider struct{ secretProvider }

func (s *scopedSecretProvider) List(_ context.Context, _ core.Scope) (core.ListResult, error) {
	return core.ListResult{IDs: []core.ID{core.NewID("sec", core.NewScope(map[string]string{"site": "legacy"}), "db-1")}, Status: core.ListComplete}, nil
}

func TestSyncMode_SecretNeverRecorded(t *testing.T) {
	// [#97-1]
	requireGit(t)
	root, intent, _ := setupSync(t, syncScopesYAML)
	r := newIntentRepo(t, root)
	gitCmd(t, r.dir, "add", "intent")
	gitCmd(t, r.dir, "commit", "-q", "-m", "intent: legacy is mode: sync")
	in, err := loader.LoadIntent(intent)
	if err != nil {
		t.Fatal(err)
	}
	p := &scopedSecretProvider{secretProvider{value: "hunter2-plaintext"}}
	pass := func() syncmode.Report {
		t.Helper()
		rep := syncmode.RunOnce(context.Background(), syncmode.Options{
			Intent: in, Root: root, Samples: 1,
			Providers: map[string]provider.Provider{"sec": p},
			Sleep:     func(time.Duration) {},
		})
		for _, o := range rep.Outcomes {
			if o.Err != nil {
				t.Fatalf("[#97-1] %v", o.Err)
			}
		}
		if rep.CommitErr != nil {
			t.Fatalf("[#97-1] %v", rep.CommitErr)
		}
		return rep
	}
	snapPath := filepath.Join(root, "generated", "legacy", "sync-sec.yaml")

	if rep := pass(); rep.Commit == "" {
		t.Fatalf("[#97-1] the first pass must commit the snapshot")
	}
	if snap := readFile(t, snapPath); strings.Contains(snap, "hunter2") || !strings.Contains(snap, `name: "db-1"`) {
		t.Fatalf("[#97-1] the snapshot must hold the resource without its plaintext secret:\n%s", snap)
	}

	p.value = "rotated-plaintext" // the secret alone changes
	if rep := pass(); rep.Commit != "" {
		t.Fatalf("[#97-1] a change of a plaintext secret alone must not make a commit")
	}

	p.value = "secretref:file/db/root#gen3" // the reference form is recorded
	if rep := pass(); rep.Commit == "" {
		t.Fatalf("[#97-1] a SecretRef appearing must be recorded")
	}
	if snap := readFile(t, snapPath); !strings.Contains(snap, `password: "secretref:file/db/root#gen3"`) {
		t.Fatalf("[#97-1] a SecretRef must be recorded in its reference form:\n%s", snap)
	}

	if hist := gitCmd(t, root, "log", "-p", "--all"); strings.Contains(hist, "hunter2") || strings.Contains(hist, "rotated-plaintext") {
		t.Fatalf("[#97-1] a plaintext secret leaked into git history:\n%s", hist)
	}
}

// partialLister lists with ListPartial.
type partialLister struct{ secretProvider }

func (s *partialLister) List(_ context.Context, _ core.Scope) (core.ListResult, error) {
	return core.ListResult{Status: core.ListPartial}, nil
}

func TestSyncMode_IncompleteListNamedInError(t *testing.T) {
	// [#97-2]
	_, err := importer.Run(context.Background(), importer.Options{
		Providers: map[string]provider.Provider{"sec": &partialLister{}},
		Samples:   1,
	})
	if err == nil || !strings.Contains(err.Error(), "(status partial)") {
		t.Fatalf("[#97-2] want the List status by name, got %v", err)
	}
	for s, want := range map[core.ListStatus]string{core.ListComplete: "complete", core.ListPartial: "partial", core.ListUnknown: "unknown"} {
		if got := s.String(); got != want {
			t.Errorf("[#97-2] ListStatus(%d).String() = %q, want %q", int(s), got, want)
		}
	}
}
