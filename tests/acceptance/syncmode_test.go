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

// syncmode_test.go — Backlog #23 (v1b): ScopeMode (converge / sync) and Sync
// Mode's write-out to generated/ (§24.5).  Committing to git is #35.
//
// Tags:
//   [#23-1] scopes: loading and validation (mode default, target_path under
//           generated/, one mode per scope, no empty scope, no intent
//           resources inside a sync scope)
//   [#23-2] sync records only the sync scope, with the generated header;
//           unchanged reality → file untouched; changed reality → updated
//   [#23-3] a hand-edited snapshot is never overwritten
//   [#23-4] converge scopes still converge and prune, but reality in a sync
//           scope is never a prune candidate (plan, reconcile, daemon)
//   [#23-5] sync never writes to reality
//   [#23-6] an incomplete List skips the scope and keeps the old snapshot
//   [#23-7] YAML subset: "- key:" with a nested block parses (was a panic)

package acceptance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/loader"
)

const syncScopesYAML = `scopes:
  - scope:
      site: legacy
    mode: sync
    sync:
      target_path: generated/legacy/
resources:
  - provider: fake
    id: web-1
    scope:
      site: tokyo
    deletion_policy: PruneWithinScope
    fields:
      name: web-1
      size: "2"
`

const syncState = `{"resources": {"web-1": {"fields": {"name": "web-1", "size": 5}}, "old-1": {"fields": {"name": "old-1", "size": 1}}, "orphan-t": {"fields": {"name": "orphan-t", "size": 1}}},
 "scopes": {"web-1": {"site": "tokyo"}, "old-1": {"site": "legacy"}, "orphan-t": {"site": "tokyo"}},
 "pruneable_capability": true, "provider_version": "v-test"}`

func setupSync(t *testing.T, intentYAML string) (root, intent, state string) {
	t.Helper()
	root = t.TempDir()
	intent = filepath.Join(root, "intent")
	if err := os.MkdirAll(intent, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStateJSON(t, filepath.Join(intent, "scopes.yaml"), intentYAML)
	state = filepath.Join(root, "state.json")
	writeStateJSON(t, state, syncState)
	return root, intent, state
}

func loadErr(t *testing.T, yaml string) error {
	t.Helper()
	dir := t.TempDir()
	writeStateJSON(t, filepath.Join(dir, "i.yaml"), yaml)
	_, err := loader.LoadIntent(dir)
	return err
}

func TestSyncMode_LoadAndValidate(t *testing.T) {
	// [#23-1] [#23-7]
	dir := t.TempDir()
	writeStateJSON(t, filepath.Join(dir, "i.yaml"), syncScopesYAML+`  - provider: fake
    id: other
    fields:
      name: other
`)
	in, err := loader.LoadIntent(dir)
	if err != nil {
		t.Fatalf("[#23-1][#23-7] valid scopes must load: %v", err)
	}
	ss := in.SyncScopes()
	if len(ss) != 1 || ss[0].Scope.Labels["site"] != "legacy" || ss[0].TargetPath != "generated/legacy/" {
		t.Fatalf("[#23-1] sync scope not parsed: %+v", in.Scopes)
	}

	if err := loadErr(t, "scopes:\n  - scope:\n      site: a\n"); err != nil {
		t.Errorf("[#23-1] mode defaults to converge: %v", err)
	}
	jdir := t.TempDir()
	writeStateJSON(t, filepath.Join(jdir, "i.json"), `{"scopes": [{"scope": {}, "mode": "sync", "sync": {"target_path": "generated/x/"}}]}`)
	if _, err := loader.LoadIntent(jdir); err == nil || !strings.Contains(err.Error(), "empty scope") {
		t.Errorf("[#23-1] an empty scope must be rejected (it would put the whole world in one mode); got %v", err)
	}
	for name, tc := range map[string]struct{ yaml, want string }{
		"sync without target":  {"scopes:\n  - scope:\n      site: a\n    mode: sync\n", "target_path"},
		"target outside gen":   {"scopes:\n  - scope:\n      site: a\n    mode: sync\n    sync:\n      target_path: intent/a/\n", "generated/"},
		"target escapes":       {"scopes:\n  - scope:\n      site: a\n    mode: sync\n    sync:\n      target_path: generated/../intent/\n", "generated/"},
		"scope declared twice": {"scopes:\n  - scope:\n      site: a\n    mode: sync\n    sync:\n      target_path: generated/a/\n  - scope:\n      site: a\n", "declared twice"},
		"null scope (YAML)":    {"scopes:\n  - scope:\n    mode: converge\n", "scopes[0].scope"},
		"resource in sync":     {"scopes:\n  - scope:\n      site: a\n    mode: sync\n    sync:\n      target_path: generated/a/\nresources:\n  - provider: fake\n    id: r\n    scope:\n      site: a\n", "mode: sync"},
		"unknown mode":         {"scopes:\n  - scope:\n      site: a\n    mode: both\n", "mode"},
	} {
		err := loadErr(t, tc.yaml)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("[#23-1] %s: want an error containing %q, got %v", name, tc.want, err)
		}
	}
}

func TestSyncMode_WritesSnapshot(t *testing.T) {
	// [#23-2] [#23-5]
	root, intent, state := setupSync(t, syncScopesYAML)
	before := readFile(t, state)

	out := mustRun(t, "sync", "--intent", intent, "--provider", "fake", "--state", state)
	snap := filepath.Join(root, "generated", "legacy", "sync-fake.yaml")
	body := readFile(t, snap)
	if !strings.HasPrefix(body, "# sakutei:generated sync\n") || !strings.Contains(body, "# body-sha256: ") {
		t.Fatalf("[#23-2] the snapshot must carry the generated header:\n%s", body)
	}
	if !strings.Contains(body, `id: "old-1"`) || strings.Contains(body, "web-1") || strings.Contains(body, "orphan-t") {
		t.Fatalf("[#23-2] only the sync scope (site=legacy) may be recorded:\n%s", body)
	}
	if !strings.Contains(out, "updated") {
		t.Errorf("[#23-2] first pass must report an update:\n%s", out)
	}
	if readFile(t, state) != before {
		t.Fatalf("[#23-5] sync must never write to reality")
	}

	info1, _ := os.Stat(snap)
	time.Sleep(20 * time.Millisecond)
	out = mustRun(t, "sync", "--intent", intent, "--provider", "fake", "--state", state)
	info2, _ := os.Stat(snap)
	if !strings.Contains(out, "unchanged") || !info1.ModTime().Equal(info2.ModTime()) {
		t.Fatalf("[#23-2] unchanged reality must leave the snapshot untouched:\n%s", out)
	}

	// Reality changes in the sync scope → recorded.
	writeStateJSON(t, state, strings.Replace(syncState, `"old-1": {"fields": {"name": "old-1", "size": 1}}`, `"old-1": {"fields": {"name": "old-1", "size": 9}}`, 1))
	mustRun(t, "sync", "--intent", intent, "--provider", "fake", "--state", state)
	if !strings.Contains(readFile(t, snap), `size: "9"`) {
		t.Fatalf("[#23-2] a reality change in the sync scope must be recorded:\n%s", readFile(t, snap))
	}
}

func TestSyncMode_RefusesHandEdit(t *testing.T) {
	// [#23-3]
	root, intent, state := setupSync(t, syncScopesYAML)
	mustRun(t, "sync", "--intent", intent, "--provider", "fake", "--state", state)
	snap := filepath.Join(root, "generated", "legacy", "sync-fake.yaml")
	edited := strings.Replace(readFile(t, snap), `size: "1"`, `size: "100"`, 1)
	writeStateJSON(t, snap, edited)

	writeStateJSON(t, state, strings.Replace(syncState, `"size": 1}}, "orphan-t"`, `"size": 2}}, "orphan-t"`, 1))
	out, code := runSakutei(t, "sync", "--intent", intent, "--provider", "fake", "--state", state)
	if code == 0 || !strings.Contains(out, "edited by hand") {
		t.Fatalf("[#23-3] a hand-edited snapshot must stop the overwrite (exit %d):\n%s", code, out)
	}
	if readFile(t, snap) != edited {
		t.Fatalf("[#23-3] the hand-edited snapshot must be left untouched")
	}
}

func TestSyncMode_ConvergeScopeStillConverges_SyncScopeNeverPruned(t *testing.T) {
	// [#23-4]
	_, intent, state := setupSync(t, syncScopesYAML)

	out := mustRun(t, "plan", "--intent", intent, "--state", state)
	if strings.Contains(out, "old-1") {
		t.Fatalf("[#23-4] plan must not touch the sync scope:\n%s", out)
	}
	if !strings.Contains(out, "DELETE fake/site=tokyo/orphan-t") {
		t.Fatalf("[#23-4] the converge scope must still prune its orphan:\n%s", out)
	}

	mustRun(t, "reconcile", "--intent", intent, "--state", state)
	res := fakeResources(t, state)
	if _, ok := res["old-1"]; !ok {
		t.Fatalf("[#23-4] reconcile must never delete reality in a sync scope: %v", res)
	}
	if _, ok := res["orphan-t"]; ok || res["web-1"]["size"] != "2" {
		t.Fatalf("[#23-4] reconcile must still converge and prune the converge scope: %v", res)
	}
}

func TestSyncMode_DaemonNeverPrunesSyncScope(t *testing.T) {
	// [#23-4]
	_, intent, state := setupSync(t, syncScopesYAML)
	cmd, stdout, stderr := startDaemonWithArgs(t, intent, state, "100ms")
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if !waitForCondition(10*time.Second, func() bool {
		res, ok := tryFakeResources(state)
		_, orphan := res["orphan-t"]
		return ok && !orphan && res["web-1"]["size"] == "2"
	}) {
		t.Fatalf("[#23-4] the daemon must converge the converge scope:\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	time.Sleep(500 * time.Millisecond) // a few more cycles
	if _, ok := fakeResources(t, state)["old-1"]; !ok {
		t.Fatalf("[#23-4] the daemon must never delete reality in a sync scope")
	}
}

func TestSyncMode_IncompleteListKeepsSnapshot(t *testing.T) {
	// [#23-6]
	root, intent, state := setupSync(t, syncScopesYAML)
	mustRun(t, "sync", "--intent", intent, "--provider", "fake", "--state", state)
	snap := filepath.Join(root, "generated", "legacy", "sync-fake.yaml")
	good := readFile(t, snap)

	writeStateJSON(t, state, strings.Replace(syncState, `"pruneable_capability": true`, `"pruneable_capability": true, "list_partial": true`, 1))
	out, code := runSakutei(t, "sync", "--intent", intent, "--provider", "fake", "--state", state)
	if code == 0 || !strings.Contains(out, "previous snapshot kept") {
		t.Fatalf("[#23-6] an incomplete List must skip the scope (exit %d):\n%s", code, out)
	}
	if readFile(t, snap) != good {
		t.Fatalf("[#23-6] the previous snapshot must be kept")
	}
}

// fakeResources returns the fake state file's resources as field maps
// (values stringified).
func fakeResources(t *testing.T, path string) map[string]map[string]string {
	t.Helper()
	res, ok := tryFakeResources(path)
	if !ok {
		t.Fatalf("cannot read fake state %s", path)
	}
	return res
}

func tryFakeResources(path string) (map[string]map[string]string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var sf struct {
		Resources map[string]struct {
			Fields map[string]any `json:"fields"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(b, &sf); err != nil {
		return nil, false
	}
	out := map[string]map[string]string{}
	for k, r := range sf.Resources {
		m := map[string]string{}
		for f, v := range r.Fields {
			m[f] = fmt.Sprint(v)
		}
		out[k] = m
	}
	return out, true
}
