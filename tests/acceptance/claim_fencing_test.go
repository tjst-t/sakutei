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

// claim_fencing_test.go: 静的シャーディングの実動作（#28、§17.1・§15.2・§17.6）。
//
// 2つの常駐ループ（reconcile.Daemon）を同じ現実（1つの fake provider）と同じ
// fencing ストアに向けて1周ずつ回し、fake provider の Read 回数の周ごとの増分で
// 「どのデーモンがどの scope を Observe したか」を見る。
//
//	AC-v1b-claim-fencing-1 — claim が分かれていれば、互いの scope を Observe しない（両方とも収束する）
//	AC-v1b-claim-fencing-2 — 同じ scope を2つのデーモンが claim すると、古い世代の方が fencing で止まる
//	AC-v1b-claim-fencing-3 — 古い世代の writer の Apply は Provider に届く前に拒否される（token 検証）
//	AC-v1b-claim-fencing-4 — 再シャード: 新シャードの世代更新で旧シャードは当該 scope だけ止まり、claim 変更後は衝突しない
//	AC-v1b-claim-fencing-5 — fencing ストアに到達できない周は Observe も Apply もしない
//	AC-v1b-claim-fencing-6 — 実バイナリ2つが同じ --claim で起動すると、先に起動した方が FENCED を出して止まる
package acceptance

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/executor"
	"github.com/tjst-t/sakutei/v2/internal/fakeprovider"
	"github.com/tjst-t/sakutei/v2/internal/fencing"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

const twoSiteFencingIntent = `{
  "resources": [
    { "provider": "fake", "id": "tokyo-vm", "scope": { "site": "tokyo" }, "fields": { "name": "tokyo-vm", "size": "1" } },
    { "provider": "fake", "id": "osaka-vm", "scope": { "site": "osaka" }, "fields": { "name": "osaka-vm", "size": "2" } }
  ]
}`

// shardRig は1つの現実（fake provider）と1つの fencing ストアを共有するシャード群。
type shardRig struct {
	t         *testing.T
	root      string
	intentDir string
	fp        *fakeprovider.Provider
	store     *fencing.FileStore
}

func newShardRig(t *testing.T) *shardRig {
	t.Helper()
	root := t.TempDir()
	intentDir := filepath.Join(root, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(intentDir, "intent.json"), []byte(twoSiteFencingIntent), 0o644); err != nil {
		t.Fatal(err)
	}
	return &shardRig{
		t:         t,
		root:      root,
		intentDir: intentDir,
		fp:        fakeprovider.New(filepath.Join(root, "state.json")),
		store:     fencing.NewFileStore(filepath.Join(root, "fencing.json")),
	}
}

type shard struct {
	rig   *shardRig
	name  string
	d     *reconcile.Daemon
	guard *fencing.Guard
}

// shard は claim（"" は全世界）で holder=name のデーモンを作る。
func (r *shardRig) shard(name, claim string) *shard {
	r.t.Helper()
	var sel core.Scope
	if claim != "" {
		k, v, _ := strings.Cut(claim, "=")
		sel = core.NewScope(map[string]string{k: v})
	}
	g := fencing.NewGuard(r.store, name)
	d := reconcile.NewDaemon(reconcile.Config{
		IntentDir:    r.intentDir,
		GeneratedDir: filepath.Join(r.root, "generated-"+name),
		Provider:     r.fp,
		Claim:        sel,
		Fencing:      g,
		Out:          io.Discard,
		ErrOut:       io.Discard,
	})
	return &shard{rig: r, name: name, d: d, guard: g}
}

// cycle は1周回し、その周に Observe された（Read が増えた）リソースと結果を返す。
func (s *shard) cycle() (map[string]bool, reconcile.CycleResult) {
	t := s.rig.t
	t.Helper()
	before, _ := s.rig.fp.GetReadCount()
	res, err := s.d.RunCycle(context.Background())
	if err != nil {
		t.Fatalf("%s: RunCycle: %v", s.name, err)
	}
	s.d.Wait(5 * time.Second)
	after, _ := s.rig.fp.GetReadCount()
	observed := map[string]bool{}
	for id, n := range after {
		if n > before[id] {
			observed[id] = true
		}
	}
	return observed, res
}

func (r *shardRig) token(scope string) fencing.Token {
	r.t.Helper()
	tok, ok, err := r.store.Current(context.Background(), scope)
	if err != nil || !ok {
		r.t.Fatalf("fencing store has no token for %s (ok=%v err=%v)", scope, ok, err)
	}
	return tok
}

// [AC-v1b-claim-fencing-1]
func TestClaimFencing_DisjointClaimsDoNotObserveEachOther(t *testing.T) {
	r := newShardRig(t)
	tokyo := r.shard("shard-tokyo", "site=tokyo")
	osaka := r.shard("shard-osaka", "site=osaka")

	for c := 1; c <= 3; c++ {
		obs, res := tokyo.cycle()
		if !reflect.DeepEqual(obs, map[string]bool{"tokyo-vm": true}) {
			t.Fatalf("[AC-v1b-claim-fencing-1] cycle %d: shard-tokyo Observed %v; want only tokyo-vm", c, obs)
		}
		if len(res.Fenced) != 0 || res.ExcludedByClaim != 1 {
			t.Fatalf("[AC-v1b-claim-fencing-1] cycle %d: shard-tokyo Fenced=%v ExcludedByClaim=%d; want none / 1", c, res.Fenced, res.ExcludedByClaim)
		}
		obs, res = osaka.cycle()
		if !reflect.DeepEqual(obs, map[string]bool{"osaka-vm": true}) {
			t.Fatalf("[AC-v1b-claim-fencing-1] cycle %d: shard-osaka Observed %v; want only osaka-vm", c, obs)
		}
		if len(res.Fenced) != 0 {
			t.Fatalf("[AC-v1b-claim-fencing-1] cycle %d: shard-osaka Fenced=%v; want none", c, res.Fenced)
		}
	}
	// 両方とも収束した（それぞれ自分の scope だけを書いた）。
	if got := mustSize(t, r.fp, "tokyo-vm"); got != 1 {
		t.Errorf("[AC-v1b-claim-fencing-1] tokyo-vm size=%d; want 1", got)
	}
	if got := mustSize(t, r.fp, "osaka-vm"); got != 2 {
		t.Errorf("[AC-v1b-claim-fencing-1] osaka-vm size=%d; want 2", got)
	}
	// 各 scope は1回ずつ取得され、世代は 1 のまま（互いに fence していない）。
	if tok := r.token("site=tokyo"); tok.Holder != "shard-tokyo" || tok.Generation != 1 {
		t.Errorf("[AC-v1b-claim-fencing-1] site=tokyo token=%+v; want shard-tokyo gen 1", tok)
	}
	if tok := r.token("site=osaka"); tok.Holder != "shard-osaka" || tok.Generation != 1 {
		t.Errorf("[AC-v1b-claim-fencing-1] site=osaka token=%+v; want shard-osaka gen 1", tok)
	}
}

// [AC-v1b-claim-fencing-2]
func TestClaimFencing_DoubleClaimFencesOlderShard(t *testing.T) {
	r := newShardRig(t)
	a := r.shard("shard-a", "site=tokyo")
	if obs, _ := a.cycle(); !obs["tokyo-vm"] {
		t.Fatalf("[AC-v1b-claim-fencing-2] shard-a did not Observe tokyo-vm on its first cycle: %v", obs)
	}

	// 設定ミスで2つ目のデーモンが同じ scope を claim する。
	b := r.shard("shard-b", "site=tokyo")
	if obs, res := b.cycle(); !obs["tokyo-vm"] || len(res.Fenced) != 0 {
		t.Fatalf("[AC-v1b-claim-fencing-2] shard-b (newer generation) must converge the scope: observed=%v fenced=%v", obs, res.Fenced)
	}
	if tok := r.token("site=tokyo"); tok.Holder != "shard-b" || tok.Generation != 2 {
		t.Fatalf("[AC-v1b-claim-fencing-2] token=%+v; want shard-b gen 2", tok)
	}

	// 現実をドリフトさせる。止まった shard-a は観測も修復もしない。
	mustSeed(t, r.fp, "tokyo-vm", 9)
	for c := 1; c <= 3; c++ {
		obs, res := a.cycle()
		if !reflect.DeepEqual(res.Fenced, []string{"site=tokyo"}) {
			t.Fatalf("[AC-v1b-claim-fencing-2] cycle %d: shard-a Fenced=%v; want [site=tokyo]", c, res.Fenced)
		}
		if len(obs) != 0 || res.Applied+res.Launched != 0 {
			t.Fatalf("[AC-v1b-claim-fencing-2] cycle %d: fenced shard-a Observed %v / applied %d launched %d; want nothing",
				c, obs, res.Applied, res.Launched)
		}
	}
	if got := mustSize(t, r.fp, "tokyo-vm"); got != 9 {
		t.Fatalf("[AC-v1b-claim-fencing-2] fenced shard-a wrote tokyo-vm (size=%d)", got)
	}
	// fence された側は取り返さない（発振しない）。
	if tok := r.token("site=tokyo"); tok.Holder != "shard-b" || tok.Generation != 2 {
		t.Fatalf("[AC-v1b-claim-fencing-2] fenced shard re-acquired the scope: token=%+v", tok)
	}

	// 新しい世代の shard-b だけが修復する。
	if _, res := b.cycle(); len(res.Fenced) != 0 {
		t.Fatalf("[AC-v1b-claim-fencing-2] shard-b fenced: %v", res.Fenced)
	}
	if got := mustSize(t, r.fp, "tokyo-vm"); got != 1 {
		t.Fatalf("[AC-v1b-claim-fencing-2] shard-b did not repair tokyo-vm: size=%d", got)
	}
}

// [AC-v1b-claim-fencing-3]
func TestClaimFencing_StaleWriterApplyRejected(t *testing.T) {
	r := newShardRig(t)
	ctx := context.Background()
	scope := core.NewScope(map[string]string{"site": "tokyo"})
	ga := fencing.NewGuard(r.store, "shard-a")
	if _, err := ga.Sync(ctx, []core.Scope{scope}); err != nil {
		t.Fatal(err)
	}
	ex := &fencing.Executor{Inner: executor.New(), Guard: ga}
	plan := func(size int) core.Plan {
		return core.Plan{Ops: []core.Op{{
			Kind:   core.OpKindCreate,
			Target: core.NewID("fake", scope, "tokyo-vm"),
			Fields: []core.FieldDiff{{Field: "name", To: "tokyo-vm"}, {Field: "size", To: strconv.Itoa(size)}},
		}}}
	}
	if err := ex.Execute(ctx, r.fp, plan(1)); err != nil {
		t.Fatalf("[AC-v1b-claim-fencing-3] current-generation Apply failed: %v", err)
	}
	before, _ := r.fp.GetApplyCount()

	// Sync の後・Apply の前に別シャードが世代を上げた（GC pause / 分断の典型）。
	if _, err := r.store.Acquire(ctx, "site=tokyo", "shard-b"); err != nil {
		t.Fatal(err)
	}
	err := ex.Execute(ctx, r.fp, plan(7))
	if !fencing.IsFenced(err) {
		t.Fatalf("[AC-v1b-claim-fencing-3] stale-generation Apply returned %v; want a FencedError", err)
	}
	after, _ := r.fp.GetApplyCount()
	if after["tokyo-vm"] != before["tokyo-vm"] {
		t.Fatalf("[AC-v1b-claim-fencing-3] stale-generation Apply reached the provider (apply count %d -> %d)",
			before["tokyo-vm"], after["tokyo-vm"])
	}
	if got := ga.Fenced(); !reflect.DeepEqual(got, []string{"site=tokyo"}) {
		t.Fatalf("[AC-v1b-claim-fencing-3] guard Fenced()=%v; want [site=tokyo]", got)
	}
}

// [AC-v1b-claim-fencing-4]
func TestClaimFencing_Reshard(t *testing.T) {
	r := newShardRig(t)
	// 旧構成: 1つのシャードが全世界を claim している。
	old := r.shard("shard-central", "")
	if obs, _ := old.cycle(); len(obs) != 2 {
		t.Fatalf("[AC-v1b-claim-fencing-4] central shard Observed %v; want both", obs)
	}

	// 再シャード手順（§17.6、v1b は手動）:
	//   1. drain（旧シャードの osaka への新規 Apply を止める）
	//   2. claim 変更 PR をマージ（osaka を新シャードの claim に足す）
	//   3. 新シャードが起動して osaka の世代を更新 → 旧シャードは osaka だけ fence される
	newShard := r.shard("shard-osaka", "site=osaka")
	if obs, res := newShard.cycle(); !reflect.DeepEqual(obs, map[string]bool{"osaka-vm": true}) || len(res.Fenced) != 0 {
		t.Fatalf("[AC-v1b-claim-fencing-4] new shard observed=%v fenced=%v", obs, res.Fenced)
	}
	mustSeed(t, r.fp, "tokyo-vm", 5)
	obs, res := old.cycle()
	if !reflect.DeepEqual(res.Fenced, []string{"site=osaka"}) {
		t.Fatalf("[AC-v1b-claim-fencing-4] central shard Fenced=%v; want [site=osaka]", res.Fenced)
	}
	if !reflect.DeepEqual(obs, map[string]bool{"tokyo-vm": true}) {
		t.Fatalf("[AC-v1b-claim-fencing-4] central shard Observed %v; want only tokyo-vm (osaka moved away)", obs)
	}
	if got := mustSize(t, r.fp, "tokyo-vm"); got != 1 {
		t.Fatalf("[AC-v1b-claim-fencing-4] central shard stopped converging its remaining scope: tokyo-vm size=%d", got)
	}

	// 4. 旧シャードを新しい claim（tokyo のみ）で再起動する。衝突は残らない。
	restarted := r.shard("shard-central", "site=tokyo")
	for c := 1; c <= 2; c++ {
		if obs, res := restarted.cycle(); !reflect.DeepEqual(obs, map[string]bool{"tokyo-vm": true}) || len(res.Fenced) != 0 {
			t.Fatalf("[AC-v1b-claim-fencing-4] restarted central cycle %d: observed=%v fenced=%v", c, obs, res.Fenced)
		}
		if obs, res := newShard.cycle(); !reflect.DeepEqual(obs, map[string]bool{"osaka-vm": true}) || len(res.Fenced) != 0 {
			t.Fatalf("[AC-v1b-claim-fencing-4] osaka shard cycle %d: observed=%v fenced=%v", c, obs, res.Fenced)
		}
	}
	if tok := r.token("site=osaka"); tok.Holder != "shard-osaka" || tok.Generation != 2 {
		t.Errorf("[AC-v1b-claim-fencing-4] site=osaka token=%+v; want shard-osaka gen 2", tok)
	}
}

// [AC-v1b-claim-fencing-5]
func TestClaimFencing_StoreUnreachableSkipsCycle(t *testing.T) {
	r := newShardRig(t)
	g := fencing.NewGuard(fencing.NewFileStore(filepath.Join(r.root, "missing-dir", "fencing.json")), "shard-a")
	d := reconcile.NewDaemon(reconcile.Config{
		IntentDir:    r.intentDir,
		GeneratedDir: filepath.Join(r.root, "generated"),
		Provider:     r.fp,
		Fencing:      g,
		Out:          io.Discard,
		ErrOut:       io.Discard,
	})
	_, err := d.RunCycle(context.Background())
	d.Wait(5 * time.Second)
	if err == nil || !strings.Contains(err.Error(), "fencing store unreachable") {
		t.Fatalf("[AC-v1b-claim-fencing-5] RunCycle err=%v; want a fencing-store error", err)
	}
	if reads, _ := r.fp.GetReadCount(); len(reads) != 0 {
		t.Fatalf("[AC-v1b-claim-fencing-5] resources were Observed with the store unreachable: %v", reads)
	}
	if applies, _ := r.fp.GetApplyCount(); len(applies) != 0 {
		t.Fatalf("[AC-v1b-claim-fencing-5] Apply ran with the store unreachable: %v", applies)
	}
}

// [AC-v1b-claim-fencing-6]
func TestClaimFencing_TwoDaemonProcesses(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTwoSiteIntent(t, intentDir)
	store := filepath.Join(tmp, "fencing.json")

	start := func(name string) (*exec.Cmd, *syncBuffer, *syncBuffer) {
		statePath := filepath.Join(tmp, name+"-state.json")
		initStateFile(t, statePath)
		cmd, out, errb := startDaemonWithArgs(t, intentDir, statePath, "100ms",
			"--generated", filepath.Join(tmp, "generated-"+name),
			"--claim", "site=tokyo", "--fencing-store", store, "--shard-id", name)
		t.Cleanup(func() {
			_ = cmd.Process.Signal(os.Interrupt)
			_ = cmd.Wait()
		})
		return cmd, out, errb
	}
	waitFor := func(buf *syncBuffer, substr string) bool {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(buf.String(), substr) {
				return true
			}
			time.Sleep(50 * time.Millisecond)
		}
		return false
	}

	_, aOut, aErr := start("shard-a")
	if !waitFor(aOut, `scope site=tokyo acquired by "shard-a" at generation 1`) {
		t.Fatalf("[AC-v1b-claim-fencing-6] shard-a never acquired site=tokyo\nstdout:\n%s\nstderr:\n%s", aOut.String(), aErr.String())
	}
	_, bOut, bErr := start("shard-b")
	if !waitFor(bErr, `scope site=tokyo taken over from "shard-a" (generation 1 -> 2)`) {
		t.Fatalf("[AC-v1b-claim-fencing-6] shard-b did not take over\nstdout:\n%s\nstderr:\n%s", bOut.String(), bErr.String())
	}
	if !waitFor(aErr, "FENCED scope(s) [site=tokyo]") {
		t.Fatalf("[AC-v1b-claim-fencing-6] shard-a was not fenced\nstdout:\n%s\nstderr:\n%s", aOut.String(), aErr.String())
	}
	if strings.Contains(bErr.String(), "FENCED") {
		t.Fatalf("[AC-v1b-claim-fencing-6] the newer shard-b was fenced:\n%s", bErr.String())
	}

	// sakutei fence は現在の holder と世代を表示する。
	out, err := exec.Command(buildBinary(t), "fence", "--store", store).CombinedOutput()
	if err != nil {
		t.Fatalf("[AC-v1b-claim-fencing-6] sakutei fence: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "site=tokyo") || !strings.Contains(string(out), "shard-b") {
		t.Fatalf("[AC-v1b-claim-fencing-6] sakutei fence output lacks site=tokyo/shard-b:\n%s", out)
	}
}
