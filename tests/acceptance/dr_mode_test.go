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

// dr_mode_test.go: DR モード（#45、§25.1・§25.4・§29.1）。
//
// claim_git_test.go と同じく実バイナリで、中央 git → `sakutei mirror` → 拠点の常駐ループの
// 順に流す。中央の被災は中央リポジトリのディレクトリを rename して表す（ミラーは拠点側に
// あって生きている）。
//
//	AC-v1.5-dr-1 — プリステージした DR intent（shards/dr/<名前>.yaml）の enabled: false → true の
//	               1 行だけの変更で、大阪の DR シャードが東京の scope を claim して収束し（自己修復・
//	               以後の intent 変更も）、fencing の holder が移り、戻ってきた東京は idle に留まる
//	AC-v1.5-dr-2 — 書込 break-glass: 中央に届くうち・S4 に記録できないとき・必須の指定が無いときは
//	               書けず、届かないときだけ署名付きのコミットがミラーに入り、S4 と
//	               BreakGlassWriteUsed Event に残り（拠点の常駐ループでも Event）、拠点が収束する
//	AC-v1.5-dr-3 — 事後合流: 中央が戻ると合流用ブランチ breakglass/<session> が中央に push され、
//	               PR の下書き（コミット・署名鍵・両側で変わったパス）が作られる。取り込まれるまで
//	               ミラーは緊急書込を配り続け、merge commit で取り込まれると中央の追従に戻って
//	               鍵を失効させる（同じ鍵では二度と書けない）
package acceptance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/audit"
)

func drPlan(enabled bool) string {
	e := "false"
	if enabled {
		e = "true"
	}
	return `# 平時に PR 承認済みで保持する DR intent（§25.1 (a)）。発動は enabled: true への切替だけ。
dr_plan:
  name: tokyo-to-osaka
  primary:
    site: tokyo
  standby:
    site: osaka
  on_activation:
    claim_change:
      from:
        shard: tokyo
      to:
        shard: osaka-dr
    enabled: ` + e + "\n"
}

// ── AC-v1.5-dr-1 ─────────────────────────────────────────────────────────────

// TestDR_PrestagedPlanActivatedByEnabledOnly: enabled: true にするだけで DR 側が収束する。
//
// [AC-v1.5-dr-1]
func TestDR_PrestagedPlanActivatedByEnabledOnly(t *testing.T) {
	f := newGitFixture(t, map[string]string{
		"intent/site=tokyo/vms.yaml":    siteIntent("tokyo", map[string]string{"tk-vm": "1"}),
		"intent/site=osaka/vms.yaml":    siteIntent("osaka", map[string]string{"os-vm": "1"}),
		"shards/tokyo.yaml":             shardYAML(map[string]string{"site": "tokyo"}),
		"shards/osaka.yaml":             shardYAML(map[string]string{"site": "osaka"}),
		"shards/osaka-dr.yaml":          shardYAML(nil), // 大阪で平時 idle で待つ DR シャード
		"shards/dr/tokyo-to-osaka.yaml": drPlan(false),
	})
	fence := filepath.Join(f.root, "fencing.json")
	common := []string{"--intent-repo", f.remote(), "--git-poll", "100ms", "--interval", "100ms",
		"--state", f.state, "--fencing-store", fence}
	shard := func(name string) (*exec.Cmd, *syncBuffer, *syncBuffer) {
		return startEdge(t, append([]string{"--workdir", filepath.Join(f.root, name+"-host"),
			"--shard", name, "--shard-id", name}, common...)...)
	}
	tokyo, tOut, tErr := shard("tokyo")
	_, oOut, oErr := shard("osaka")
	dr, dOut, dErr := shard("osaka-dr")
	logs := []*syncBuffer{tOut, tErr, oOut, oErr, dOut, dErr}

	waitField(t, f.state, "tk-vm", "size", "1", logs...)
	waitField(t, f.state, "os-vm", "size", "1", logs...)
	if !waitForCondition(10*time.Second, func() bool { return strings.Contains(dOut.String(), "idle") }) {
		t.Fatalf("[AC-v1.5-dr-1] the DR shard is not idle in peacetime:\n%s", dOut.String())
	}
	if h := fenceHolder(t, fence, "site=tokyo"); h != "tokyo" {
		t.Fatalf("[AC-v1.5-dr-1] holder of site=tokyo in peacetime = %q, want tokyo", h)
	}

	// 被災: 東京のデーモンが死ぬ。人が DR を発動する（§25.4 人間トリガ）——
	// プリステージ済みの plan の enabled を true にするだけ。
	_ = tokyo.Process.Kill()
	_, _ = tokyo.Process.Wait()
	f.merge(t, "DR: activate tokyo-to-osaka", map[string]string{"shards/dr/tokyo-to-osaka.yaml": drPlan(true)})
	if stat := gitCmd(t, f.repo.dir, "diff", "--numstat", "HEAD~1", "HEAD"); stat != "1\t1\tshards/dr/tokyo-to-osaka.yaml" {
		t.Fatalf("[AC-v1.5-dr-1] the activation is not a one-line change: %q", stat)
	}

	if !waitForCondition(20*time.Second, func() bool { return fenceHolder(t, fence, "site=tokyo") == "osaka-dr" }) {
		t.Fatalf("[AC-v1.5-dr-1] holder did not move to osaka-dr; logs:\n%s%s", dOut, dErr)
	}
	if !strings.Contains(dOut.String(), "DRPlanActive: DR plan tokyo-to-osaka") {
		t.Errorf("[AC-v1.5-dr-1] no DRPlanActive Event on the DR shard:\n%s", dOut.String())
	}
	// DR 側が東京 scope を収束する: drift の自己修復と、以後の intent 変更。
	setRealityField(t, f.state, "tk-vm", "size", "9")
	waitField(t, f.state, "tk-vm", "size", "1", logs...)
	f.merge(t, "resize tk-vm during DR", map[string]string{
		"intent/site=tokyo/vms.yaml": siteIntent("tokyo", map[string]string{"tk-vm": "2"}),
	})
	waitField(t, f.state, "tk-vm", "size", "2", logs...)
	if v, _ := stateResourceField(f.state, "os-vm", "size"); v != "1" {
		t.Errorf("[AC-v1.5-dr-1] the osaka scope was disturbed: os-vm size=%q", v)
	}

	// 東京が戻ってきても、DR plan が有効な間は claim を持たない（split-brain にならない）。
	tokyo2, t2Out, t2Err := shard("tokyo")
	if !waitForCondition(10*time.Second, func() bool { return strings.Contains(t2Out.String(), "shard claims nothing") }) {
		t.Fatalf("[AC-v1.5-dr-1] the recovered tokyo shard is not idle while DR is active:\n%s%s", t2Out, t2Err)
	}
	if !strings.Contains(t2Out.String(), "DRPlanActive") {
		t.Errorf("[AC-v1.5-dr-1] the recovered tokyo shard did not report the DR plan:\n%s", t2Out)
	}
	if h := fenceHolder(t, fence, "site=tokyo"); h != "osaka-dr" {
		t.Errorf("[AC-v1.5-dr-1] the recovered tokyo took site=tokyo back: holder=%q", h)
	}
	stopEdge(t, tokyo2)
	stopEdge(t, dr)
	for _, out := range []string{dOut.String(), t2Out.String()} {
		if strings.Contains(out, "pruned=") {
			t.Errorf("[AC-v1.5-dr-1] the DR handoff pruned something:\n%s", out)
		}
	}
}

// ── AC-v1.5-dr-2 / AC-v1.5-dr-3 ──────────────────────────────────────────────

// breakGlass は `sakutei breakglass commit` を走らせる。
func breakGlass(t *testing.T, args ...string) (string, error) {
	t.Helper()
	all := append([]string{"breakglass", "commit", "--probe-interval", "20ms", "--probe-timeout", "5s"}, args...)
	out, err := exec.Command(buildBinary(t), all...).CombinedOutput()
	return string(out), err
}

// TestDR_BreakGlassWriteAndMergeBack: 届かないときだけの署名付きローカル書込と、事後合流。
//
// [AC-v1.5-dr-2] [AC-v1.5-dr-3]
func TestDR_BreakGlassWriteAndMergeBack(t *testing.T) {
	f := newGitFixture(t, map[string]string{
		"intent/site=a/vms.yaml": siteIntent("a", map[string]string{"a-vm": "1"}),
		"shards/store-a.yaml":    shardYAML(map[string]string{"site": "a"}),
	})
	edge, eOut, eErr := startEdge(t,
		"--intent-repo", f.remote(), "--workdir", filepath.Join(f.root, "host-a"), "--shard", "store-a",
		"--git-poll", "100ms", "--interval", "100ms", "--state", f.state)
	waitField(t, f.state, "a-vm", "size", "1", eOut, eErr)

	bin := buildBinary(t)
	key := filepath.Join(f.root, "offline", "bg.key")
	if out, err := exec.Command(bin, "breakglass", "keygen", "--out", key).CombinedOutput(); err != nil {
		t.Fatalf("keygen: %v\n%s", err, out)
	}
	s4 := filepath.Join(f.root, "s4", "audit.jsonl")
	if err := os.MkdirAll(filepath.Dir(s4), 0o755); err != nil {
		t.Fatal(err)
	}
	// 運用者の作業場所: ローカルミラーの clone に緊急の変更を書く。
	wt := filepath.Join(f.root, "operator")
	gitCmd(t, f.root, "clone", "-q", f.remote(), wt)
	edit := func(sizes map[string]string) {
		if err := os.WriteFile(filepath.Join(wt, "intent", "site=a", "vms.yaml"), []byte(siteIntent("a", sizes)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	edit(map[string]string{"a-vm": "5"})
	mirrorMain := func() string { return gitCmd(t, f.mirror, "rev-parse", "refs/heads/main") }
	before := mirrorMain()
	ok := []string{"--repo", wt, "--key", key, "--actor", "alice", "--reason", "DR: grow a-vm while the central git is down", "--audit-log", s4}

	// 中央に届くうちは break-glass で書けない。
	if out, err := breakGlass(t, ok...); err == nil || !strings.Contains(out, "reachable") {
		t.Fatalf("[AC-v1.5-dr-2] wrote while the central git is reachable: err=%v\n%s", err, out)
	}

	// 中央が被災する（ミラーは拠点側で生きている）。
	if err := os.Rename(f.repo.dir, f.repo.dir+".down"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no audit log", []string{"--repo", wt, "--key", key, "--actor", "alice", "--reason", "x"}},
		{"no actor", []string{"--repo", wt, "--key", key, "--reason", "x", "--audit-log", s4}},
		{"no key", []string{"--repo", wt, "--actor", "alice", "--reason", "x", "--audit-log", s4}},
		{"S4 unwritable", []string{"--repo", wt, "--key", key, "--actor", "alice", "--reason", "x",
			"--audit-log", filepath.Join(f.root, "no-such-dir", "audit.jsonl")}},
		{"too few probes", append([]string{"--probe-attempts", "1"}, ok...)},
	} {
		if out, err := breakGlass(t, tc.args...); err == nil {
			t.Errorf("[AC-v1.5-dr-2] %s: break-glass write accepted:\n%s", tc.name, out)
		}
	}
	if got := mirrorMain(); got != before {
		t.Fatalf("[AC-v1.5-dr-2] a refused break-glass write reached the mirror (%s -> %s)", before, got)
	}
	if recs, _ := audit.ReadAll(s4); len(recs) != 0 {
		t.Fatalf("[AC-v1.5-dr-2] refused writes left S4 records: %+v", recs)
	}

	out, err := breakGlass(t, ok...)
	if err != nil {
		t.Fatalf("[AC-v1.5-dr-2] break-glass write while the central is unreachable: %v\n%s", err, out)
	}
	if !strings.Contains(out, "EVENT BreakGlassWriteUsed severity=critical") {
		t.Errorf("[AC-v1.5-dr-2] no BreakGlassWriteUsed Event:\n%s", out)
	}
	bg := mirrorMain()
	recs, _ := audit.ReadAll(s4)
	if len(recs) != 1 || recs[0].Kind != audit.KindBreakGlassWrite || recs[0].Commit != bg ||
		recs[0].Actor != "alice" || recs[0].Signature == "" || recs[0].Key == "" || recs[0].Session == "" {
		t.Fatalf("[AC-v1.5-dr-2] S4 after the write = %+v (mirror main %s)", recs, bg)
	}
	session := recs[0].Session
	vout, err := exec.Command(bin, "breakglass", "verify", "--repo", f.mirror, "--pubkey", key+".pub", bg).CombinedOutput()
	if err != nil || !strings.Contains(string(vout), "ok   "+bg) {
		t.Errorf("[AC-v1.5-dr-2] signature does not verify: %v\n%s", err, vout)
	}
	// 拠点は緊急書込を収束し、届いた break-glass コミットを Event にする。
	waitField(t, f.state, "a-vm", "size", "5", eOut, eErr)
	if !waitForCondition(10*time.Second, func() bool {
		return strings.Contains(eErr.String(), "EVENT BreakGlassWriteUsed") && strings.Contains(eErr.String(), bg[:12])
	}) {
		t.Errorf("[AC-v1.5-dr-2] the edge did not raise BreakGlassWriteUsed:\n%s", eErr.String())
	}

	// 中央が戻るまでミラーは中央を追わない（同期は失敗し、緊急書込は残る）。
	if out, err := exec.Command(bin, "mirror", "--upstream", f.repo.dir, "--dir", f.mirror, "--once").CombinedOutput(); err == nil {
		t.Errorf("[AC-v1.5-dr-3] mirror sync succeeded with the central down:\n%s", out)
	}
	if got := mirrorMain(); got != bg {
		t.Fatalf("[AC-v1.5-dr-3] the mirror dropped the break-glass head: %s", got)
	}

	// 中央が戻る。戻った直後に中央でも同じファイルが変わった（競合）。
	if err := os.Rename(f.repo.dir+".down", f.repo.dir); err != nil {
		t.Fatal(err)
	}
	f.repo.commit("add a-db, a-vm to 3 (merged on the central right after recovery)", map[string]string{
		"intent/site=a/vms.yaml": siteIntent("a", map[string]string{"a-vm": "3", "a-db": "2"}),
	})
	syncMirror(t, f.repo.dir, f.mirror)
	branch := "breakglass/" + session
	if got := gitCmd(t, f.repo.dir, "rev-parse", "refs/heads/"+branch); got != bg {
		t.Fatalf("[AC-v1.5-dr-3] merge-back branch %s = %s, want %s", branch, got, bg)
	}
	draft := filepath.Join(f.mirror, "sakutei-breakglass-"+session+"-pr.md")
	body, err := os.ReadFile(draft)
	if err != nil {
		t.Fatalf("[AC-v1.5-dr-3] no PR draft: %v", err)
	}
	for _, want := range []string{branch, bg[:12], "alice", recs[0].Key, "`intent/site=a/vms.yaml`", "merge commit"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("[AC-v1.5-dr-3] PR draft lacks %q:\n%s", want, body)
		}
	}
	// 取り込まれるまでは緊急書込を配り続ける（中央の a-vm=3 で巻き戻さない）。
	if got := mirrorMain(); got != bg {
		t.Errorf("[AC-v1.5-dr-3] the mirror followed the central before the merge-back: %s", got)
	}
	time.Sleep(500 * time.Millisecond)
	if v, _ := stateResourceField(f.state, "a-vm", "size"); v != "5" {
		t.Errorf("[AC-v1.5-dr-3] the emergency write was rolled back before the merge: a-vm size=%q", v)
	}

	// 事後レビュー: 人が競合を解いて merge commit で取り込む。
	if out, err := exec.Command("git", "-C", f.repo.dir, "merge", "--no-ff", "-q", branch).CombinedOutput(); err == nil {
		t.Fatalf("[AC-v1.5-dr-3] expected a conflict between the central and the break-glass write:\n%s", out)
	}
	f.repo.commit("merge "+branch+" (post-hoc review)", map[string]string{
		"intent/site=a/vms.yaml": siteIntent("a", map[string]string{"a-vm": "5", "a-db": "2"}),
	})
	if parents := strings.Fields(gitCmd(t, f.repo.dir, "log", "-1", "--format=%P")); len(parents) != 2 {
		t.Fatalf("[AC-v1.5-dr-3] the post-hoc merge is not a merge commit: parents=%v", parents)
	}
	syncMirror(t, f.repo.dir, f.mirror)
	if got, want := mirrorMain(), gitCmd(t, f.repo.dir, "rev-parse", "HEAD"); got != want {
		t.Errorf("[AC-v1.5-dr-3] the mirror did not return to following the central: %s, want %s", got, want)
	}
	if exists(filepath.Join(f.mirror, "sakutei-breakglass.json")) {
		t.Errorf("[AC-v1.5-dr-3] the break-glass session is still open after the merge")
	}
	waitField(t, f.state, "a-db", "size", "2", eOut, eErr)
	if v, _ := stateResourceField(f.state, "a-vm", "size"); v != "5" {
		t.Errorf("[AC-v1.5-dr-3] a-vm size=%q after the merge, want 5", v)
	}
	stopEdge(t, edge)

	kinds := map[audit.Kind]int{}
	recs, _ = audit.ReadAll(s4)
	for _, r := range recs {
		kinds[r.Kind]++
	}
	for _, k := range []audit.Kind{audit.KindBreakGlassWrite, audit.KindBreakGlassMergeBack, audit.KindBreakGlassMerged, audit.KindBreakGlassKeyRetired} {
		if kinds[k] != 1 {
			t.Errorf("[AC-v1.5-dr-3] S4 has %d %s record(s), want 1; all: %+v", kinds[k], k, recs)
		}
	}
	// 使った鍵は失効した: 同じ鍵では（中央が落ちても）二度と書けない。
	gitCmd(t, wt, "pull", "-q", "--ff-only")
	edit(map[string]string{"a-vm": "6", "a-db": "2"})
	if out, err := breakGlass(t, ok...); err == nil || !strings.Contains(out, "retired") {
		t.Errorf("[AC-v1.5-dr-3] a retired key wrote again: err=%v\n%s", err, out)
	}
}
