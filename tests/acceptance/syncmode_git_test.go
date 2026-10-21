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

// syncmode_git_test.go — Backlog #35 (v1b): Sync Mode commits its snapshots to
// git as the system (§24.5, §26.3, §29.1).
//
// Tags:
//   [#35-1] a reality change in a sync scope makes exactly one commit, by
//           sakutei-sync <sakutei-sync@localhost>, unsigned, carrying only
//           the snapshot (not what a person has staged); no change → no commit
//   [#35-2] the committer's name and email are configurable
//   [#35-3] a field that only flaps (a managed candidate, §24.4) never makes a
//           commit, even after it settles on a new value; a real change does
//   [#35-4] a snapshot a person changed in a commit is never overwritten
//           (even with a matching body hash); deleting it in a commit lets
//           Sync write it afresh
//   [#50-1] [#35-4] holds when the intent path goes through a symbolic link
//           (git reports the work tree with links resolved; macOS's /var)

package acceptance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/fakeprovider"
	"github.com/tjst-t/sakutei/internal/loader"
	"github.com/tjst-t/sakutei/internal/syncmode"
	"github.com/tjst-t/sakutei/pkg/provider"
)

const syncSnapRel = "generated/legacy/sync-fake.yaml"

// setupSyncRepo is setupSync with the repository root under git and the
// intent committed by a person.
func setupSyncRepo(t *testing.T) (root, intent, state string) {
	t.Helper()
	requireGit(t)
	root, intent, state = setupSync(t, syncScopesYAML)
	r := newIntentRepo(t, root)
	gitCmd(t, r.dir, "add", "intent")
	gitCmd(t, r.dir, "commit", "-q", "-m", "intent: legacy is mode: sync")
	return root, intent, state
}

func commitCount(t *testing.T, root string) int {
	t.Helper()
	n, err := strconv.Atoi(gitCmd(t, root, "rev-list", "--count", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func syncCLI(t *testing.T, intent, state string, extra ...string) (string, int) {
	t.Helper()
	return runSakutei(t, append([]string{"sync", "--intent", intent, "--provider", "fake", "--state", state, "--samples", "1"}, extra...)...)
}

func TestSyncModeGit_CommitsAsSystem(t *testing.T) {
	// [#35-1]
	root, intent, state := setupSyncRepo(t)
	base := commitCount(t, root)

	// A person has something staged; Sync's commit must not take it.
	writeStateJSON(t, filepath.Join(root, "NOTES.md"), "wip\n")
	gitCmd(t, root, "add", "NOTES.md")

	if out, code := syncCLI(t, intent, state); code != 0 || !strings.Contains(out, "committed") {
		t.Fatalf("[#35-1] first sync must commit (exit %d):\n%s", code, out)
	}
	if got := commitCount(t, root); got != base+1 {
		t.Fatalf("[#35-1] want exactly one commit, got %d new", got-base)
	}
	if who := gitCmd(t, root, "log", "-1", "--format=%an <%ae>|%cn <%ce>"); who != "sakutei-sync <sakutei-sync@localhost>|sakutei-sync <sakutei-sync@localhost>" {
		t.Fatalf("[#35-1] the commit must be attributed to the system by default; got %q", who)
	}
	if files := gitCmd(t, root, "show", "--name-only", "--format=", "HEAD"); files != syncSnapRel {
		t.Fatalf("[#35-1] the commit must carry only the snapshot; got %q", files)
	}
	if msg := gitCmd(t, root, "log", "-1", "--format=%B"); !strings.Contains(msg, "Sakutei-Sync: site=legacy fake") {
		t.Fatalf("[#35-1] the commit must carry the Sync trailer:\n%s", msg)
	}
	if raw := gitCmd(t, root, "cat-file", "commit", "HEAD"); strings.Contains(raw, "gpgsig") {
		t.Fatalf("[#35-1] Sync's commits are unsigned by default")
	}
	if st := gitCmd(t, root, "status", "--porcelain", "--", "NOTES.md"); !strings.HasPrefix(st, "A ") {
		t.Fatalf("[#35-1] the person's staged file must stay staged, uncommitted; got %q", st)
	}

	// No change → no commit.
	if out, code := syncCLI(t, intent, state); code != 0 || !strings.Contains(out, "nothing to commit") {
		t.Fatalf("[#35-1] an unchanged reality must not commit (exit %d):\n%s", code, out)
	}
	if got := commitCount(t, root); got != base+1 {
		t.Fatalf("[#35-1] no change must make no commit; got %d new", got-base)
	}

	// Reality changes in the sync scope → one more commit.
	setRealityField(t, state, "old-1", "size", "9")
	if out, code := syncCLI(t, intent, state); code != 0 {
		t.Fatalf("[#35-1] sync after a change (exit %d):\n%s", code, out)
	}
	if got := commitCount(t, root); got != base+2 {
		t.Fatalf("[#35-1] a reality change must make exactly one commit; got %d new in total", got-base)
	}
	if !strings.Contains(gitCmd(t, root, "show", "HEAD:"+syncSnapRel), `size: "9"`) {
		t.Fatalf("[#35-1] the committed snapshot must record the change")
	}
	// A change outside the sync scope is not Sync's business.
	setRealityField(t, state, "web-1", "size", "3")
	syncCLI(t, intent, state)
	if got := commitCount(t, root); got != base+2 {
		t.Fatalf("[#35-1] a change outside the sync scope must not commit")
	}
}

func TestSyncModeGit_CommitterConfigurable(t *testing.T) {
	// [#35-2]
	root, intent, state := setupSyncRepo(t)
	if out, code := syncCLI(t, intent, state, "--committer-name", "legacy-bot", "--committer-email", "legacy-bot@example.com"); code != 0 {
		t.Fatalf("[#35-2] sync (exit %d):\n%s", code, out)
	}
	if who := gitCmd(t, root, "log", "-1", "--format=%an <%ae>|%cn <%ce>"); who != "legacy-bot <legacy-bot@example.com>|legacy-bot <legacy-bot@example.com>" {
		t.Fatalf("[#35-2] the configured committer must be used; got %q", who)
	}
	// Sync still knows its own commit under another name (the trailer).
	setRealityField(t, state, "old-1", "size", "4")
	if out, code := syncCLI(t, intent, state); code != 0 || !strings.Contains(out, "committed") {
		t.Fatalf("[#35-2] a renamed committer's snapshot is still Sync's (exit %d):\n%s", code, out)
	}
}

func TestSyncModeGit_FlappingFieldNeverCommits(t *testing.T) {
	// [#35-3]
	root, intent, state := setupSyncRepo(t)
	in, err := loader.LoadIntent(intent)
	if err != nil {
		t.Fatal(err)
	}
	size, name := 1, "old-1"
	write := func() {
		writeStateJSON(t, state, `{"resources": {"old-1": {"fields": {"name": "`+name+`", "size": `+strconv.Itoa(size)+`}}},
 "scopes": {"old-1": {"site": "legacy"}}, "pruneable_capability": true, "provider_version": "v-test"}`)
	}
	pass := func(flap bool) syncmode.Report {
		t.Helper()
		write()
		rep := syncmode.RunOnce(context.Background(), syncmode.Options{
			Intent: in, Root: root, Samples: 3,
			Providers: map[string]provider.Provider{"fake": fakeprovider.New(state)},
			Sleep: func(time.Duration) {
				if flap { // reality's size moves between observations
					size++
					write()
				}
			},
		})
		for _, o := range rep.Outcomes {
			if o.Err != nil {
				t.Fatalf("[#35-3] %v", o.Err)
			}
		}
		if rep.CommitErr != nil {
			t.Fatalf("[#35-3] %v", rep.CommitErr)
		}
		return rep
	}

	if rep := pass(true); rep.Commit == "" {
		t.Fatalf("[#35-3] the first pass must commit the snapshot")
	}
	base := commitCount(t, root)
	snap := readFile(t, filepath.Join(root, syncSnapRel))
	if !strings.Contains(snap, "# managed 候補: 観測のたびに値が変化（\"1\" → \"2\" → \"3\"）\n      size:") {
		t.Fatalf("[#35-3] the flapping field must be marked as a managed candidate:\n%s", snap)
	}

	size = 20
	if rep := pass(true); rep.Commit != "" {
		t.Fatalf("[#35-3] a field that only flaps must not make a commit")
	}
	size = 77 // settles on a new value: still only the candidate moved
	if rep := pass(false); rep.Commit != "" {
		t.Fatalf("[#35-3] a managed candidate's change alone must not make a commit")
	}
	if got := commitCount(t, root); got != base {
		t.Fatalf("[#35-3] no commit expected; got %d new", got-base)
	}

	name = "old-1-renamed" // a real change
	if rep := pass(false); rep.Commit == "" {
		t.Fatalf("[#35-3] a change of a stable field must commit")
	}
	snap = readFile(t, filepath.Join(root, syncSnapRel))
	if !strings.Contains(snap, `name: "old-1-renamed"`) || !strings.Contains(snap, "# managed 候補: 以前の観測で値が変化\n      size:") {
		t.Fatalf("[#35-3] the candidate must stay marked after it settles:\n%s", snap)
	}
}

// rehash recomputes a snapshot's body hash, as a person who knows the format
// might after editing it.
func rehash(t *testing.T, snap string) string {
	t.Helper()
	const marker = "# body-sha256: "
	i := strings.Index(snap, marker)
	j := strings.IndexByte(snap[i:], '\n') + i + 1
	sum := sha256.Sum256([]byte(snap[j:]))
	return snap[:i] + marker + hex.EncodeToString(sum[:]) + "\n" + snap[j:]
}

func TestSyncModeGit_RefusesHandCommit(t *testing.T) {
	// [#35-4]
	for _, tc := range []struct {
		name   string
		rehash bool
	}{{"body hash left stale", false}, {"body hash recomputed", true}} {
		t.Run(tc.name, func(t *testing.T) {
			root, intent, state := setupSyncRepo(t)
			if out, code := syncCLI(t, intent, state); code != 0 {
				t.Fatalf("sync (exit %d):\n%s", code, out)
			}
			snapPath := filepath.Join(root, syncSnapRel)
			edited := strings.Replace(readFile(t, snapPath), `size: "1"`, `size: "100"`, 1)
			if tc.rehash {
				edited = rehash(t, edited)
			}
			writeStateJSON(t, snapPath, edited)
			gitCmd(t, root, "commit", "-q", "-am", "fix legacy size by hand")
			base := commitCount(t, root)

			setRealityField(t, state, "old-1", "size", "2")
			out, code := syncCLI(t, intent, state)
			if code == 0 || !strings.Contains(out, "by hand") {
				t.Fatalf("[#35-4] a hand commit to the sync output must stop the overwrite (exit %d):\n%s", code, out)
			}
			if readFile(t, snapPath) != edited || commitCount(t, root) != base {
				t.Fatalf("[#35-4] the hand-committed snapshot must be left as it is, with no new commit")
			}

			// Deleting it in a commit hands it back to Sync.
			gitCmd(t, root, "rm", "-q", syncSnapRel)
			gitCmd(t, root, "commit", "-q", "-m", "hand the legacy snapshot back to sync")
			if out, code := syncCLI(t, intent, state); code != 0 || !strings.Contains(out, "committed") {
				t.Fatalf("[#35-4] after the deletion Sync must write and commit it afresh (exit %d):\n%s", code, out)
			}
			if !strings.Contains(gitCmd(t, root, "show", "HEAD:"+syncSnapRel), `size: "2"`) {
				t.Fatalf("[#35-4] the fresh snapshot must record reality")
			}
		})
	}
}

func TestSyncModeGit_RefusesHandCommitThroughSymlink(t *testing.T) {
	// [#50-1]
	root, _, state := setupSyncRepo(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	intent := filepath.Join(link, "intent")
	if out, code := syncCLI(t, intent, state); code != 0 || !strings.Contains(out, "committed") {
		t.Fatalf("sync through a link (exit %d):\n%s", code, out)
	}
	snapPath := filepath.Join(root, syncSnapRel)
	edited := rehash(t, strings.Replace(readFile(t, snapPath), `size: "1"`, `size: "100"`, 1))
	writeStateJSON(t, snapPath, edited)
	gitCmd(t, root, "commit", "-q", "-am", "fix legacy size by hand")
	base := commitCount(t, root)

	setRealityField(t, state, "old-1", "size", "2")
	out, code := syncCLI(t, intent, state)
	if code == 0 || !strings.Contains(out, "by hand") {
		t.Fatalf("[#50-1] a hand commit must be detected through a linked intent path (exit %d):\n%s", code, out)
	}
	if readFile(t, snapPath) != edited || commitCount(t, root) != base {
		t.Fatalf("[#50-1] the hand-committed snapshot must be left as it is, with no new commit")
	}
}
