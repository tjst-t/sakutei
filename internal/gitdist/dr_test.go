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
	"crypto/ed25519"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/audit"
	"github.com/tjst-t/sakutei/pkg/core"
)

func drPlanYAML(name, from, to string, enabled bool) string {
	e := "false"
	if enabled {
		e = "true"
	}
	return "dr_plan:\n  name: " + name + "\n  primary:\n    site: tokyo\n  standby:\n    site: osaka\n" +
		"  on_activation:\n    claim_change:\n      from:\n        shard: " + from + "\n      to:\n        shard: " + to + "\n    enabled: " + e + "\n"
}

func writeShards(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestResolveClaim_DRPlan(t *testing.T) {
	tokyo := core.NewScope(map[string]string{"site": "tokyo"})
	base := map[string]string{
		"shards/tokyo.yaml":    "claim:\n  site: tokyo\n",
		"shards/osaka.yaml":    "claim:\n  site: osaka\n",
		"shards/osaka-dr.yaml": "claim:\n",
	}
	with := func(extra map[string]string) string {
		files := map[string]string{}
		for k, v := range base {
			files[k] = v
		}
		for k, v := range extra {
			files[k] = v
		}
		return writeShards(t, files)
	}

	// 平時（enabled: false）は何も動かない。
	dir := with(map[string]string{"shards/dr/tokyo-to-osaka.yaml": drPlanYAML("tokyo-to-osaka", "tokyo", "osaka-dr", false)})
	for name, want := range map[string]bool{"tokyo": false, "osaka-dr": true} {
		sc, plan, err := ResolveClaim(dir, name)
		if err != nil || sc.Idle != want || plan != "" {
			t.Errorf("peacetime %s = %+v plan=%q err=%v; want idle=%v", name, sc, plan, err, want)
		}
	}

	// 発動（enabled: true）: tokyo は idle、osaka-dr が tokyo を claim。osaka は変わらない。
	dir = with(map[string]string{"shards/dr/tokyo-to-osaka.yaml": drPlanYAML("tokyo-to-osaka", "tokyo", "osaka-dr", true)})
	if sc, plan, err := ResolveClaim(dir, "tokyo"); err != nil || !sc.Idle || plan != "tokyo-to-osaka" {
		t.Errorf("active tokyo = %+v plan=%q err=%v; want idle", sc, plan, err)
	}
	if sc, plan, err := ResolveClaim(dir, "osaka-dr"); err != nil || sc.Idle || !sc.Claim.Equal(tokyo) || plan != "tokyo-to-osaka" {
		t.Errorf("active osaka-dr = %+v plan=%q err=%v; want claim site=tokyo", sc, plan, err)
	}
	if sc, plan, err := ResolveClaim(dir, "osaka"); err != nil || plan != "" || sc.Claim.Labels["site"] != "osaka" {
		t.Errorf("active osaka = %+v plan=%q err=%v; want untouched", sc, plan, err)
	}

	// 矛盾する宣言は収束させない（エラー）。
	for name, tc := range map[string]struct {
		files map[string]string
		shard string
	}{
		"standby not idle":   {map[string]string{"shards/dr/x.yaml": drPlanYAML("x", "tokyo", "osaka", true)}, "osaka"},
		"from not primary":   {map[string]string{"shards/dr/x.yaml": drPlanYAML("x", "osaka", "osaka-dr", true)}, "osaka"},
		"two active plans":   {map[string]string{"shards/dr/x.yaml": drPlanYAML("x", "tokyo", "osaka-dr", true), "shards/dr/y.yaml": drPlanYAML("y", "tokyo", "osaka-dr", true)}, "osaka-dr"},
		"name != file":       {map[string]string{"shards/dr/x.yaml": drPlanYAML("other", "tokyo", "osaka-dr", false)}, "tokyo"},
		"enabled not given":  {map[string]string{"shards/dr/x.yaml": strings.Replace(drPlanYAML("x", "tokyo", "osaka-dr", false), "    enabled: false\n", "", 1)}, "tokyo"},
		"unknown key":        {map[string]string{"shards/dr/x.yaml": drPlanYAML("x", "tokyo", "osaka-dr", false) + "extra: 1\n"}, "tokyo"},
		"same shard on both": {map[string]string{"shards/dr/x.yaml": drPlanYAML("x", "tokyo", "tokyo", false)}, "tokyo"},
	} {
		if _, _, err := ResolveClaim(with(tc.files), tc.shard); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func requireGitBin(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := runGit(context.Background(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestBreakGlass_SignAndVerify: 署名は tree・parent・メッセージを覆い、改竄・別の鍵・
// 到達できる中央を拒む。
func TestBreakGlass_SignAndVerify(t *testing.T) {
	requireGitBin(t)
	ctx := context.Background()
	root := t.TempDir()
	central := filepath.Join(root, "central")
	if err := os.MkdirAll(central, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, central, "init", "-q", "-b", "main")
	git(t, central, "config", "user.name", "t")
	git(t, central, "config", "user.email", "t@example.com")
	if err := os.WriteFile(filepath.Join(central, "a.txt"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, central, "add", "-A")
	git(t, central, "commit", "-q", "-m", "init")
	mirror := filepath.Join(root, "mirror.git")
	if err := (&Mirror{Upstream: central, Dir: mirror}).Sync(ctx); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(root, "wt")
	git(t, root, "clone", "-q", "file://"+mirror, wt)
	if err := os.WriteFile(filepath.Join(wt, "a.txt"), []byte("2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(root, "bg.key")
	fp, err := GenerateBreakGlassKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	key, err := LoadBreakGlassKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := LoadBreakGlassPublicKey(keyPath + ".pub")
	if err != nil || KeyFingerprint(pub) != fp {
		t.Fatalf("public key %v fingerprint mismatch: %v", err, fp)
	}
	logPath := filepath.Join(root, "audit.jsonl")
	w := &Write{Repo: wt, Key: key, Actor: "alice", Reason: "emergency", AuditLog: logPath,
		Probe: Probe{Attempts: 3, Interval: time.Millisecond, Timeout: 5 * time.Second}}

	// 中央に届くうちは書けない。
	if _, err := w.Commit(ctx); err == nil || !strings.Contains(err.Error(), "reachable") {
		t.Fatalf("commit while the central is reachable: %v", err)
	}
	if recs, _ := audit.ReadAll(logPath); len(recs) != 0 {
		t.Fatalf("a refused write left S4 records: %+v", recs)
	}
	// 判定の回数を減らしては書けない。
	w2 := *w
	w2.Probe.Attempts = 1
	if _, err := w2.Commit(ctx); err == nil {
		t.Fatal("a single probe was accepted as unreachable")
	}

	if err := os.Rename(central, central+".down"); err != nil {
		t.Fatal(err)
	}
	res, err := w.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := git(t, mirror, "rev-parse", "refs/heads/main"); got != res.Commit {
		t.Errorf("mirror main = %s, want the break-glass commit %s", got, res.Commit)
	}
	s, err := VerifyBreakGlassCommit(ctx, mirror, res.Commit, []ed25519.PublicKey{pub})
	if err != nil || s.Actor != "alice" || s.Key != fp || s.Session != res.Session {
		t.Fatalf("verify = %+v, %v", s, err)
	}
	other, _, _ := ed25519.GenerateKey(nil)
	if _, err := VerifyBreakGlassCommit(ctx, mirror, res.Commit, []ed25519.PublicKey{other}); err == nil {
		t.Error("verified against an untrusted key")
	}
	// 改竄: 同じメッセージで tree を変えたコミットは検証に通らない。
	raw, _ := runGitRaw(ctx, mirror, "cat-file", "commit", res.Commit)
	_, msg, _ := strings.Cut(raw, "\n\n")
	if err := os.WriteFile(filepath.Join(wt, "a.txt"), []byte("3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, wt, "add", "-A")
	tree := git(t, wt, "write-tree")
	forged, err := commitTree(ctx, wt, tree, git(t, mirror, "rev-parse", "refs/heads/main~1"), msg, "mallory", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBreakGlassCommit(ctx, wt, forged, []ed25519.PublicKey{pub}); err == nil {
		t.Error("a forged commit (other tree, copied signature) verified")
	}
	recs, _ := audit.ReadAll(logPath)
	if len(recs) != 1 || recs[0].Kind != audit.KindBreakGlassWrite || recs[0].Commit != res.Commit || recs[0].Key != fp || recs[0].Signature == "" {
		t.Errorf("S4 = %+v", recs)
	}
	if got := BreakGlassCommits(ctx, mirror, "refs/heads/main~1", "refs/heads/main"); len(got) != 1 || got[0] != res.Commit {
		t.Errorf("BreakGlassCommits = %v", got)
	}
}
