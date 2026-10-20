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

// claim_git_test.go: claim の git 配布・S2-free シャード・エッジの資格局所性（#29、
// §17.4・§17.5・§17.7・§20.5）。
//
// 実バイナリを使う。中央 git（作業ツリー付きのリポジトリ）→ `sakutei mirror` の
// 読み取り専用ミラー → 拠点の常駐ループ（--intent-repo file://<ミラー>）の順に流す。
// 拠点が持つのは --workdir（手元写しと render 出力）と拠点ローカルの資格ファイルだけ。
// 現実は fake provider の state ファイル（拠点ホストの外にある機器）で表す。
//
//	AC-v1b-claim-git-1 — 拠点は自 claim の intent/<scope>/ と platform・shards だけを sparse checkout
//	                     （他拠点の blob は partial clone で手元に来ない）で取得し、自 claim だけを収束する
//	AC-v1b-claim-git-2 — S2-free シャードはホスト全損（workdir と資格ファイルの消失）から
//	                     再イメージ → 資格の再配布 → pull → 収束で復旧する（S2 ストアなし）
//	AC-v1b-claim-git-3 — ミラー断（WAN 断）中も前回 pull した intent と局所資格で自己修復を続け、
//	                     一度も pull できていない拠点は空の desired で収束しない（周を飛ばす）
//	AC-v1b-claim-git-4 — 開店/閉店 = claim 変更 PR のハンドオフ: shards/<名前>.yaml の claim を
//	                     書き換えるだけで scope の担当が中央 ⇄ 店舗で移り、fencing の holder も移る
//	AC-v1b-claim-git-5 — 資格局所性: `sakutei secrets extract` は当該拠点の資格だけを書き出し、
//	                     他拠点の資格を持つ store では拠点デーモンは収束しない
//	AC-v1b-claim-git-6 — --s2-free / --intent-repo のフラグ検証（S2 を要する設定を拒否する）
package acceptance

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// intentRepo は中央 git（作業ツリー付き）。commit が「PR のマージ」にあたる。
type intentRepo struct {
	t   *testing.T
	dir string
}

func newIntentRepo(t *testing.T, dir string) *intentRepo {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "init", "-q", "-b", "main")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@example.com")
	return &intentRepo{t: t, dir: dir}
}

// commit は files（リポジトリ内パス → 内容。内容が "" なら削除）を書いてコミットする。
func (r *intentRepo) commit(msg string, files map[string]string) {
	r.t.Helper()
	for rel, content := range files {
		p := filepath.Join(r.dir, filepath.FromSlash(rel))
		if content == "" {
			if err := os.Remove(p); err != nil {
				r.t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
	gitCmd(r.t, r.dir, "add", "-A")
	gitCmd(r.t, r.dir, "commit", "-q", "-m", msg)
}

// blob は HEAD の rel の blob ID を返す。
func (r *intentRepo) blob(rel string) string {
	r.t.Helper()
	return gitCmd(r.t, r.dir, "rev-parse", "HEAD:"+rel)
}

// syncMirror は `sakutei mirror --once` でミラーを中央に追従させる。
func syncMirror(t *testing.T, upstream, mirror string) {
	t.Helper()
	out, err := exec.Command(buildBinary(t), "mirror", "--upstream", upstream, "--dir", mirror, "--once").CombinedOutput()
	if err != nil {
		t.Fatalf("sakutei mirror: %v\n%s", err, out)
	}
}

// siteIntent は scope {site: <site>} の fake リソースを並べた intent YAML。
func siteIntent(site string, sizes map[string]string) string {
	var b strings.Builder
	b.WriteString("resources:\n")
	for _, id := range sortedKeys(sizes) {
		fmt.Fprintf(&b, "  - provider: fake\n    id: %s\n    scope:\n      site: %s\n    fields:\n      name: %s\n      size: \"%s\"\n",
			id, site, id, sizes[id])
	}
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

const platformIntent = `resources:
  - provider: fake
    id: plat-lb
    scope:
      role: platform
    fields:
      name: plat-lb
      size: "1"
`

func shardYAML(labels map[string]string) string {
	if len(labels) == 0 {
		return "claim:\n"
	}
	var b strings.Builder
	b.WriteString("claim:\n")
	for _, k := range sortedKeys(labels) {
		fmt.Fprintf(&b, "  %s: %s\n", k, labels[k])
	}
	return b.String()
}

// startEdge は拠点の常駐ループを起動する。
func startEdge(t *testing.T, args ...string) (*exec.Cmd, *syncBuffer, *syncBuffer) {
	t.Helper()
	var stdout, stderr syncBuffer
	cmd := exec.Command(buildBinary(t), args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd, &stdout, &stderr
}

// stopEdge は SIGTERM で常駐ループを止め、終了を待つ。
func stopEdge(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	_ = cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("daemon did not stop after SIGTERM")
	}
}

// setRealityField は現実（fake の state）の1フィールドを書き換える（drift の注入）。
func setRealityField(t *testing.T, statePath, local, field, value string) {
	t.Helper()
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var sf map[string]any
	if err := json.Unmarshal(data, &sf); err != nil {
		t.Fatal(err)
	}
	res := sf["resources"].(map[string]any)[local].(map[string]any)
	res["fields"].(map[string]any)[field] = value
	out, _ := json.MarshalIndent(sf, "", "  ")
	if err := writeFileAtomic(statePath, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitField(t *testing.T, statePath, local, field, want string, logs ...*syncBuffer) {
	t.Helper()
	if !waitForCondition(20*time.Second, func() bool {
		v, ok := stateResourceField(statePath, local, field)
		return ok && v == want
	}) {
		v, _ := stateResourceField(statePath, local, field)
		var b strings.Builder
		for _, l := range logs {
			b.WriteString(l.String())
		}
		t.Fatalf("%s.%s = %q, want %q; logs:\n%s", local, field, v, want, b.String())
	}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// gitFixture は中央 git・ミラー・現実を用意する。
type gitFixture struct {
	root, mirror, state string
	repo                *intentRepo
}

func (f *gitFixture) remote() string { return "file://" + f.mirror }

func newGitFixture(t *testing.T, files map[string]string) *gitFixture {
	t.Helper()
	requireGit(t)
	root := t.TempDir()
	f := &gitFixture{
		root:   root,
		mirror: filepath.Join(root, "mirror.git"),
		state:  filepath.Join(root, "reality.json"),
		repo:   newIntentRepo(t, filepath.Join(root, "central")),
	}
	f.repo.commit("initial intent", files)
	syncMirror(t, f.repo.dir, f.mirror)
	initStateFile(t, f.state)
	return f
}

func (f *gitFixture) merge(t *testing.T, msg string, files map[string]string) {
	t.Helper()
	f.repo.commit(msg, files)
	syncMirror(t, f.repo.dir, f.mirror)
}

// ── AC-v1b-claim-git-1 ───────────────────────────────────────────────────────

// TestClaimGit_SparseCheckoutOwnClaimOnly: 拠点は自 claim 分だけを取得して収束する。
//
// [AC-v1b-claim-git-1]
func TestClaimGit_SparseCheckoutOwnClaimOnly(t *testing.T) {
	f := newGitFixture(t, map[string]string{
		"intent/site=a/vms.yaml":      siteIntent("a", map[string]string{"a-vm": "1"}),
		"intent/site=b/vms.yaml":      siteIntent("b", map[string]string{"b-vm": "1"}),
		"intent/platform/shared.yaml": platformIntent,
		"shards/store-a.yaml":         shardYAML(map[string]string{"site": "a"}),
		"shards/store-b.yaml":         shardYAML(map[string]string{"site": "b"}),
	})
	work := filepath.Join(f.root, "host-a", "work")
	cmd, stdout, stderr := startEdge(t,
		"--intent-repo", f.remote(), "--workdir", work, "--shard", "store-a", "--s2-free",
		"--git-poll", "100ms", "--interval", "100ms", "--state", f.state)

	waitField(t, f.state, "a-vm", "size", "1", stdout, stderr)
	// 収束が何周か回ってから、他 scope に触れていないことを見る。
	if !waitForCondition(10*time.Second, func() bool { return strings.Contains(stdout.String(), "converged (no changes)") }) {
		t.Fatalf("[AC-v1b-claim-git-1] never converged; stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	stopEdge(t, cmd)

	ids := stateResourceIDs(t, f.state)
	if ids["b-vm"] || ids["plat-lb"] {
		t.Errorf("[AC-v1b-claim-git-1] converged outside the claim: reality=%v", ids)
	}

	repo := filepath.Join(work, "repo")
	for rel, want := range map[string]bool{
		"intent/site=a/vms.yaml":      true,
		"intent/platform/shared.yaml": true,
		"shards/store-a.yaml":         true,
		"intent/site=b":               false,
	} {
		if got := exists(filepath.Join(repo, filepath.FromSlash(rel))); got != want {
			t.Errorf("[AC-v1b-claim-git-1] %s present=%v in the sparse checkout, want %v", rel, got, want)
		}
	}
	// partial clone: 他拠点の intent の blob は手元のオブジェクトにも無い。
	missing := gitCmd(t, repo, "rev-list", "--objects", "--missing=print", "HEAD")
	if b := f.repo.blob("intent/site=b/vms.yaml"); !strings.Contains(missing, "?"+b) {
		t.Errorf("[AC-v1b-claim-git-1] the blob of intent/site=b (%s) was fetched; rev-list:\n%s", b, missing)
	}
	if b := f.repo.blob("intent/site=a/vms.yaml"); strings.Contains(missing, "?"+b) {
		t.Errorf("[AC-v1b-claim-git-1] the blob of the own claim (%s) is missing", b)
	}
}

// ── AC-v1b-claim-git-2 ───────────────────────────────────────────────────────

// TestClaimGit_S2FreeTotalLossRecovery: ホスト全損 → 再イメージ → pull → 収束。
//
// [AC-v1b-claim-git-2]
func TestClaimGit_S2FreeTotalLossRecovery(t *testing.T) {
	f := newGitFixture(t, map[string]string{
		"intent/site=a/vms.yaml":      siteIntent("a", map[string]string{"a-vm": "1", "a-db": "2"}),
		"intent/site=b/vms.yaml":      siteIntent("b", map[string]string{"b-vm": "1"}),
		"intent/platform/shared.yaml": platformIntent,
		"shards/store-a.yaml":         shardYAML(map[string]string{"site": "a"}),
	})
	// 現実には別シャードの所有物（b-vm）もある。拠点 a はこれに触れてはならない。
	seedStateAtomic(t, f.state, map[string]map[string]any{
		"b-vm": {"name": "b-vm", "size": "7"},
	}, testProviderVersion)

	central := filepath.Join(f.root, "central-secrets.json")
	writeJSONFile(t, central, map[string]any{"secrets": map[string]any{
		"site=a/router/admin": map[string]any{"gen": 1, "value": "pw-a"},
		"site=b/router/admin": map[string]any{"gen": 1, "value": "pw-b"},
	}})

	host := filepath.Join(f.root, "host-a")
	secrets := filepath.Join(host, "secrets.json")
	reimage := func() {
		// 再イメージ: 空のホストに、中央から当該拠点の資格だけを配る（§20.5）。
		if err := os.MkdirAll(host, 0o755); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(buildBinary(t), "secrets", "extract",
			"--from", central, "--claim", "site=a", "--out", secrets).CombinedOutput()
		if err != nil {
			t.Fatalf("secrets extract: %v\n%s", err, out)
		}
	}
	args := []string{
		"--intent-repo", f.remote(), "--workdir", filepath.Join(host, "work"),
		"--shard", "store-a", "--s2-free", "--secret-store", secrets,
		"--git-poll", "100ms", "--interval", "100ms", "--state", f.state,
	}

	reimage()
	cmd, stdout, stderr := startEdge(t, args...)
	waitField(t, f.state, "a-vm", "size", "1", stdout, stderr)
	waitField(t, f.state, "a-db", "size", "2", stdout, stderr)
	stopEdge(t, cmd)

	// 拠点が止まっている間に: 現実が drift し、中央では intent が進む。
	setRealityField(t, f.state, "a-vm", "size", "9")
	f.merge(t, "add a-cache to site a", map[string]string{
		"intent/site=a/vms.yaml": siteIntent("a", map[string]string{"a-vm": "1", "a-db": "2", "a-cache": "3"}),
	})

	// 全損: 拠点ホストのディスクは何も残らない（手元写し・render 出力・資格）。
	if err := os.RemoveAll(host); err != nil {
		t.Fatal(err)
	}

	reimage()
	cmd2, stdout2, stderr2 := startEdge(t, args...)
	waitField(t, f.state, "a-vm", "size", "1", stdout2, stderr2)    // drift を直した
	waitField(t, f.state, "a-cache", "size", "3", stdout2, stderr2) // 最新の intent を pull した
	if !waitForCondition(10*time.Second, func() bool { return strings.Contains(stdout2.String(), "converged (no changes)") }) {
		t.Fatalf("[AC-v1b-claim-git-2] never converged after recovery; stdout:\n%s\nstderr:\n%s", stdout2.String(), stderr2.String())
	}
	stopEdge(t, cmd2)

	if v, _ := stateResourceField(f.state, "a-db", "size"); v != "2" {
		t.Errorf("[AC-v1b-claim-git-2] a-db size=%q after recovery, want 2 (untouched)", v)
	}
	if v, _ := stateResourceField(f.state, "b-vm", "size"); v != "7" {
		t.Errorf("[AC-v1b-claim-git-2] another shard's b-vm was touched: size=%q", v)
	}
	if ids := stateResourceIDs(t, f.state); ids["plat-lb"] {
		t.Errorf("[AC-v1b-claim-git-2] platform resource converged by the edge: %v", ids)
	}
	for _, out := range []string{stdout.String(), stdout2.String()} {
		if strings.Contains(out, "pruned=") {
			t.Errorf("[AC-v1b-claim-git-2] the edge pruned something:\n%s", out)
		}
	}
	// 復旧後のホストにあるのは、pull し直した手元写し・render 出力・配り直した資格だけ。
	entries, _ := os.ReadDir(host)
	for _, e := range entries {
		if e.Name() != "work" && e.Name() != "secrets.json" {
			t.Errorf("[AC-v1b-claim-git-2] unexpected local state on the S2-free host: %s", e.Name())
		}
	}
	work, _ := os.ReadDir(filepath.Join(host, "work"))
	for _, e := range work {
		if e.Name() != "repo" && e.Name() != "generated" {
			t.Errorf("[AC-v1b-claim-git-2] unexpected local state under --workdir: %s", e.Name())
		}
	}
}

// ── AC-v1b-claim-git-3 ───────────────────────────────────────────────────────

// TestClaimGit_MirrorDownKeepsSelfHealing: ミラー断中も前回の intent で自己修復を続ける。
//
// [AC-v1b-claim-git-3]
func TestClaimGit_MirrorDownKeepsSelfHealing(t *testing.T) {
	f := newGitFixture(t, map[string]string{
		"intent/site=a/vms.yaml": siteIntent("a", map[string]string{"a-vm": "1"}),
		"shards/store-a.yaml":    shardYAML(map[string]string{"site": "a"}),
	})
	cmd, stdout, stderr := startEdge(t,
		"--intent-repo", f.remote(), "--workdir", filepath.Join(f.root, "host-a"), "--shard", "store-a", "--s2-free",
		"--git-poll", "100ms", "--interval", "100ms", "--state", f.state)
	waitField(t, f.state, "a-vm", "size", "1", stdout, stderr)

	// WAN 断: ミラーに届かない。
	if err := os.RemoveAll(f.mirror); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(10*time.Second, func() bool {
		return strings.Contains(stderr.String(), "converging on the last pulled revision")
	}) {
		t.Fatalf("[AC-v1b-claim-git-3] no stale-source report; stderr:\n%s", stderr.String())
	}
	setRealityField(t, f.state, "a-vm", "size", "9")
	waitField(t, f.state, "a-vm", "size", "1", stdout, stderr)
	stopEdge(t, cmd)

	// 一度も pull できていない拠点は、空の desired で収束しない（現実の a-vm を消さない）。
	cmd2, stdout2, stderr2 := startEdge(t,
		"--intent-repo", f.remote(), "--workdir", filepath.Join(f.root, "host-a2"), "--shard", "store-a", "--s2-free",
		"--git-poll", "100ms", "--interval", "100ms", "--state", f.state)
	if !waitForCondition(10*time.Second, func() bool {
		return strings.Count(stderr2.String(), "intent source unavailable") >= 3
	}) {
		t.Fatalf("[AC-v1b-claim-git-3] cold start without the mirror did not skip cycles; stderr:\n%s", stderr2.String())
	}
	stopEdge(t, cmd2)
	if strings.Contains(stdout2.String(), "cycle 1:") {
		t.Errorf("[AC-v1b-claim-git-3] a cycle ran without any pulled intent:\n%s", stdout2.String())
	}
	if !stateResourceIDs(t, f.state)["a-vm"] {
		t.Errorf("[AC-v1b-claim-git-3] a-vm disappeared from reality")
	}
}

// ── AC-v1b-claim-git-4 ───────────────────────────────────────────────────────

// fenceHolder は `sakutei fence` が示す scope の holder を返す。
func fenceHolder(t *testing.T, store, scope string) string {
	t.Helper()
	out, err := exec.Command(buildBinary(t), "fence", "--store", store).CombinedOutput()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == scope {
			return fields[2]
		}
	}
	return ""
}

// TestClaimGit_OpenCloseHandoffByClaimPR: 開店/閉店 = claim 変更 PR のハンドオフ。
//
// [AC-v1b-claim-git-4]
func TestClaimGit_OpenCloseHandoffByClaimPR(t *testing.T) {
	f := newGitFixture(t, map[string]string{
		"intent/site=c/vms.yaml": siteIntent("c", map[string]string{"c-vm": "1"}),
		// 開店前: 中央のプロビジョニング claim が site=c を持ち、店舗デーモンは idle。
		"shards/central.yaml": shardYAML(map[string]string{"site": "c"}),
		"shards/store-c.yaml": shardYAML(nil),
	})
	fence := filepath.Join(f.root, "fencing.json")
	common := []string{"--intent-repo", f.remote(), "--git-poll", "100ms", "--interval", "100ms",
		"--state", f.state, "--fencing-store", fence}
	central, cOut, cErr := startEdge(t, append([]string{"--workdir", filepath.Join(f.root, "central-host"),
		"--shard", "central", "--shard-id", "central"}, common...)...)
	store, sOut, sErr := startEdge(t, append([]string{"--workdir", filepath.Join(f.root, "store-host"),
		"--shard", "store-c", "--shard-id", "store-c", "--s2-free"}, common...)...)
	logs := []*syncBuffer{cOut, cErr, sOut, sErr}

	waitField(t, f.state, "c-vm", "size", "1", logs...)
	if !waitForCondition(10*time.Second, func() bool { return strings.Contains(sOut.String(), "idle") }) {
		t.Fatalf("[AC-v1b-claim-git-4] the store daemon is not idle before opening:\n%s", sOut.String())
	}
	if h := fenceHolder(t, fence, "site=c"); h != "central" {
		t.Fatalf("[AC-v1b-claim-git-4] holder of site=c before opening = %q, want central", h)
	}
	if exists(filepath.Join(f.root, "store-host", "repo", "intent", "site=c")) {
		t.Errorf("[AC-v1b-claim-git-4] an idle store checked out intent/site=c")
	}

	// 開店: claim 変更 PR（中央から外し、店舗に足す）。
	f.merge(t, "open store c: hand site=c over to store-c", map[string]string{
		"shards/central.yaml": shardYAML(nil),
		"shards/store-c.yaml": shardYAML(map[string]string{"site": "c"}),
	})
	if !waitForCondition(20*time.Second, func() bool { return fenceHolder(t, fence, "site=c") == "store-c" }) {
		t.Fatalf("[AC-v1b-claim-git-4] holder did not move to store-c; logs:\n%s%s%s%s", cOut, cErr, sOut, sErr)
	}
	if !waitForCondition(10*time.Second, func() bool { return strings.Contains(cOut.String(), "claim released") }) {
		t.Fatalf("[AC-v1b-claim-git-4] central did not release the claim:\n%s", cOut.String())
	}
	// 以後の intent 変更は店舗が収束する。
	f.merge(t, "resize c-vm", map[string]string{
		"intent/site=c/vms.yaml": siteIntent("c", map[string]string{"c-vm": "2"}),
	})
	waitField(t, f.state, "c-vm", "size", "2", logs...)
	if !exists(filepath.Join(f.root, "store-host", "repo", "intent", "site=c", "vms.yaml")) {
		t.Errorf("[AC-v1b-claim-git-4] the store did not check out its new claim")
	}

	// 閉店: 逆順（店舗から外し、中央へ戻す）。
	f.merge(t, "close store c: hand site=c back to central", map[string]string{
		"shards/central.yaml": shardYAML(map[string]string{"site": "c"}),
		"shards/store-c.yaml": shardYAML(nil),
	})
	if !waitForCondition(20*time.Second, func() bool { return fenceHolder(t, fence, "site=c") == "central" }) {
		t.Fatalf("[AC-v1b-claim-git-4] holder did not move back to central; logs:\n%s%s%s%s", cOut, cErr, sOut, sErr)
	}
	if !waitForCondition(10*time.Second, func() bool { return strings.Contains(sOut.String(), "claim released") }) {
		t.Fatalf("[AC-v1b-claim-git-4] the store did not release the claim:\n%s", sOut.String())
	}
	f.merge(t, "resize c-vm again", map[string]string{
		"intent/site=c/vms.yaml": siteIntent("c", map[string]string{"c-vm": "3"}),
	})
	waitField(t, f.state, "c-vm", "size", "3", logs...)
	if !waitForCondition(10*time.Second, func() bool {
		return !exists(filepath.Join(f.root, "store-host", "repo", "intent", "site=c"))
	}) {
		t.Errorf("[AC-v1b-claim-git-4] the closed store still holds intent/site=c")
	}
	stopEdge(t, central)
	stopEdge(t, store)

	for _, out := range []string{cOut.String(), sOut.String()} {
		if strings.Contains(out, "pruned=") {
			t.Errorf("[AC-v1b-claim-git-4] a handoff pruned something:\n%s", out)
		}
	}
}

// ── AC-v1b-claim-git-5 ───────────────────────────────────────────────────────

// TestClaimGit_SecretLocality: 拠点には当該拠点の資格だけを置く。
//
// [AC-v1b-claim-git-5]
func TestClaimGit_SecretLocality(t *testing.T) {
	f := newGitFixture(t, map[string]string{
		"intent/site=a/vms.yaml": siteIntent("a", map[string]string{"a-vm": "1"}),
		"shards/store-a.yaml":    shardYAML(map[string]string{"site": "a"}),
	})
	central := filepath.Join(f.root, "central-secrets.json")
	writeJSONFile(t, central, map[string]any{"secrets": map[string]any{
		"site=a/router/admin": map[string]any{"gen": 3, "value": "pw-a"},
		"site=a/switch/admin": map[string]any{"gen": 1, "value": "pw-a2"},
		"site=b/router/admin": map[string]any{"gen": 1, "value": "pw-b"},
		"site=ab/router":      map[string]any{"gen": 1, "value": "pw-ab"},
	}})

	site := filepath.Join(f.root, "site-a-secrets.json")
	out, err := exec.Command(buildBinary(t), "secrets", "extract",
		"--from", central, "--claim", "site=a", "--out", site).CombinedOutput()
	if err != nil {
		t.Fatalf("secrets extract: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "pw-") {
		t.Errorf("[AC-v1b-claim-git-5] secrets extract printed a value:\n%s", out)
	}
	var got struct {
		Secrets map[string]struct {
			Gen   int    `json:"gen"`
			Value string `json:"value"`
		} `json:"secrets"`
	}
	data, _ := os.ReadFile(site)
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Secrets) != 2 || got.Secrets["site=a/router/admin"].Value != "pw-a" ||
		got.Secrets["site=a/router/admin"].Gen != 3 || got.Secrets["site=a/switch/admin"].Value != "pw-a2" {
		t.Errorf("[AC-v1b-claim-git-5] site store = %+v, want only the two site=a credentials", got.Secrets)
	}
	if fi, err := os.Stat(site); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("[AC-v1b-claim-git-5] site store mode = %v, want 0600", fi.Mode().Perm())
	}

	// 中央の全拠点資格を持たせた拠点デーモンは収束しない。
	cmd, stdout, stderr := startEdge(t,
		"--intent-repo", f.remote(), "--workdir", filepath.Join(f.root, "host-a"), "--shard", "store-a", "--s2-free",
		"--secret-store", central, "--git-poll", "100ms", "--interval", "100ms", "--state", f.state)
	if !waitForCondition(10*time.Second, func() bool {
		return strings.Count(stderr.String(), "outside this site's prefix") >= 3
	}) {
		t.Fatalf("[AC-v1b-claim-git-5] no locality refusal; stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	stopEdge(t, cmd)
	if strings.Contains(stderr.String()+stdout.String(), "pw-") {
		t.Errorf("[AC-v1b-claim-git-5] a secret value leaked into the log")
	}
	if stateResourceIDs(t, f.state)["a-vm"] {
		t.Errorf("[AC-v1b-claim-git-5] converged with a non-local secret store")
	}

	// --intent 直読みの常駐ループでも同じ検査をする（起動時に拒否）。
	intentDir := filepath.Join(f.root, "flat")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range [][]string{
		{"--claim", "site=a", "--secret-store", central},
		{"--secret-store", site}, // claim の無い store は局所性を定義できない
	} {
		args := append([]string{"--intent", intentDir, "--state", f.state, "--interval", "100ms"}, tc...)
		c := exec.Command(buildBinary(t), args...)
		done := make(chan error, 1)
		go func() { done <- c.Run() }()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("[AC-v1b-claim-git-5] %v: daemon started; want a refusal", tc)
			}
		case <-time.After(10 * time.Second):
			_ = c.Process.Kill()
			t.Errorf("[AC-v1b-claim-git-5] %v: daemon kept running; want a refusal", tc)
		}
	}

	// 局所の store なら収束する。
	cmd2, stdout2, stderr2 := startEdge(t,
		"--intent-repo", f.remote(), "--workdir", filepath.Join(f.root, "host-a"), "--shard", "store-a", "--s2-free",
		"--secret-store", site, "--git-poll", "100ms", "--interval", "100ms", "--state", f.state)
	waitField(t, f.state, "a-vm", "size", "1", stdout2, stderr2)
	stopEdge(t, cmd2)
}

// ── AC-v1b-claim-git-6 ───────────────────────────────────────────────────────

// TestClaimGit_FlagValidation: S2 を要する設定や矛盾するフラグを起動時に拒否する。
//
// [AC-v1b-claim-git-6]
func TestClaimGit_FlagValidation(t *testing.T) {
	tmp := t.TempDir()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--intent", tmp, "--s2-free"}, "--s2-free requires --intent-repo"},
		{[]string{"--intent-repo", "file:///nonexistent", "--workdir", tmp, "--shard", "s", "--s2-free",
			"--alloc-store", filepath.Join(tmp, "alloc.json")}, "--s2-free refuses --alloc-store"},
		{[]string{"--intent-repo", "file:///nonexistent", "--workdir", tmp}, "exactly one of --shard"},
		{[]string{"--intent-repo", "file:///nonexistent", "--workdir", tmp, "--shard", "s", "--claim", "site=a"}, "exactly one of --shard"},
		{[]string{"--intent-repo", "file:///nonexistent", "--shard", "s"}, "requires --workdir"},
		{[]string{"--intent", tmp, "--intent-repo", "file:///nonexistent", "--workdir", tmp, "--shard", "s"}, "exclusive"},
	} {
		out, err := exec.Command(buildBinary(t), tc.args...).CombinedOutput()
		if err == nil || !strings.Contains(string(out), tc.want) {
			t.Errorf("[AC-v1b-claim-git-6] %v: err=%v output=%q, want a refusal containing %q", tc.args, err, out, tc.want)
		}
	}
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
