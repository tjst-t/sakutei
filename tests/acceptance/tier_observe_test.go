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

// tier_observe_test.go: tier 別の Observe 頻度（間引き、#30、§16.5）。
//
// 常駐ループを reconcile.Daemon で1周ずつ回し、fake provider の Read 回数の
// 周ごとの増分で「どの周に Observe されたか」を見る。
//
//	AC-v1b-tier-observe-1 — 低頻度の tier は宣言どおりの周期でだけ Observe される（tier なしは毎周）
//	AC-v1b-tier-observe-2 — 間引かれた周の drift は次に予定された周で検知・修復される
//	AC-v1b-tier-observe-3 — 未収束のリソースは収束するまで毎周 Observe される
//	AC-v1b-tier-observe-4 — intent の変更は間引かれず、次の周で Observe・Apply される
//	AC-v1b-tier-observe-5 — tiers: の宣言の誤りはロード時に拒否される
package acceptance

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/fakeprovider"
	"github.com/tjst-t/sakutei/internal/loader"
	"github.com/tjst-t/sakutei/internal/reconcile"
)

// tierIntent は fast（tier なし）と slow（observe_every: every の tier）の2リソースの intent。
func tierIntent(every, slowSize string) string {
	return `{
  "tiers": [ { "name": "slow", "observe_every": ` + every + ` } ],
  "resources": [
    { "provider": "fake", "id": "fast", "fields": { "name": "fast", "size": "1" } },
    { "provider": "fake", "id": "slow", "tier": "slow", "fields": { "name": "slow", "size": "` + slowSize + `" } }
  ]
}`
}

type tierRig struct {
	t         *testing.T
	intentDir string
	fp        *fakeprovider.Provider
	d         *reconcile.Daemon
}

func newTierRig(t *testing.T, intent string) *tierRig {
	t.Helper()
	root := t.TempDir()
	intentDir := filepath.Join(root, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	r := &tierRig{t: t, intentDir: intentDir, fp: fakeprovider.New(filepath.Join(root, "state.json"))}
	r.writeIntent(intent)
	r.d = reconcile.NewDaemon(reconcile.Config{
		IntentDir:    intentDir,
		GeneratedDir: filepath.Join(root, "generated"),
		Provider:     r.fp,
		Out:          io.Discard,
		ErrOut:       io.Discard,
	})
	return r
}

func (r *tierRig) writeIntent(intent string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.intentDir, "intent.json"), []byte(intent), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// cycle は1周回し、この周に Observe された（Read が増えた）リソースを返す。
// Apply は待ってから返る。
func (r *tierRig) cycle() map[string]bool {
	r.t.Helper()
	before, _ := r.fp.GetReadCount()
	res, err := r.d.RunCycle(context.Background())
	if err != nil {
		r.t.Fatalf("RunCycle: %v", err)
	}
	after, _ := r.fp.GetReadCount()
	r.d.Wait(5 * time.Second)
	observed := map[string]bool{}
	for id, n := range after {
		if n > before[id] {
			observed[id] = true
		}
	}
	if len(observed) != res.Observed {
		r.t.Fatalf("CycleResult.Observed=%d but %d resource(s) were Read", res.Observed, len(observed))
	}
	return observed
}

// observedCycles は n 周回し、local が Observe された周の番号（1 始まり）を返す。
func (r *tierRig) observedCycles(n int, local string, hook func(cycle int)) []int {
	r.t.Helper()
	var got []int
	for c := 1; c <= n; c++ {
		if hook != nil {
			hook(c)
		}
		if r.cycle()[local] {
			got = append(got, c)
		}
	}
	return got
}

func seedConverged(t *testing.T, fp *fakeprovider.Provider) {
	t.Helper()
	mustSeed(t, fp, "fast", 1)
	mustSeed(t, fp, "slow", 1)
}

// [AC-v1b-tier-observe-1]
func TestTierObserve_LowFrequencyTierObservedOnlyOnSchedule(t *testing.T) {
	r := newTierRig(t, tierIntent("3", "1"))
	seedConverged(t, r.fp)

	var fast, slow []int
	for c := 1; c <= 10; c++ {
		obs := r.cycle()
		if obs["fast"] {
			fast = append(fast, c)
		}
		if obs["slow"] {
			slow = append(slow, c)
		}
	}
	if want := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}; !reflect.DeepEqual(fast, want) {
		t.Errorf("[AC-v1b-tier-observe-1] tier なしは毎周 Observe されるべき: got %v want %v", fast, want)
	}
	if want := []int{1, 4, 7, 10}; !reflect.DeepEqual(slow, want) {
		t.Errorf("[AC-v1b-tier-observe-1] observe_every: 3 は 3 周に1回だけ Observe されるべき: got %v want %v", slow, want)
	}

	// observe_every: 1 は既定（毎周）と同じ。
	r1 := newTierRig(t, tierIntent("1", "1"))
	seedConverged(t, r1.fp)
	if got := r1.observedCycles(4, "slow", nil); !reflect.DeepEqual(got, []int{1, 2, 3, 4}) {
		t.Errorf("[AC-v1b-tier-observe-1] observe_every: 1 は毎周: got %v", got)
	}
}

// [AC-v1b-tier-observe-2]
func TestTierObserve_DriftOnThinnedResourceRepairedAtNextScheduledCycle(t *testing.T) {
	r := newTierRig(t, tierIntent("3", "1"))
	seedConverged(t, r.fp)

	r.cycle() // 周1: 両方 Observe、収束済み
	mustSeed(t, r.fp, "slow", 9)
	for c := 2; c <= 3; c++ {
		if r.cycle()["slow"] {
			t.Fatalf("[AC-v1b-tier-observe-2] 周%d: slow は間引かれるべき", c)
		}
		if got := mustSize(t, r.fp, "slow"); got != 9 {
			t.Fatalf("[AC-v1b-tier-observe-2] 周%d: Observe していない周に修復された（size=%d）", c, got)
		}
	}
	if !r.cycle()["slow"] { // 周4
		t.Fatalf("[AC-v1b-tier-observe-2] 周4: slow は予定どおり Observe されるべき")
	}
	if got := mustSize(t, r.fp, "slow"); got != 1 {
		t.Errorf("[AC-v1b-tier-observe-2] 周4 で drift が修復されるべき: size=%d", got)
	}
}

// [AC-v1b-tier-observe-3]
func TestTierObserve_UnconvergedResourceObservedEveryCycle(t *testing.T) {
	r := newTierRig(t, tierIntent("3", "1"))
	mustSeed(t, r.fp, "fast", 1)
	// slow は現実に無い → 周1 で Create。周2 は前回が未収束なので Observe、
	// 収束を確かめてから間引きに入る（次は周5）。
	got := r.observedCycles(8, "slow", nil)
	if want := []int{1, 2, 5, 8}; !reflect.DeepEqual(got, want) {
		t.Errorf("[AC-v1b-tier-observe-3] got %v want %v", got, want)
	}
	if !mustExists(t, r.fp, "slow") {
		t.Errorf("[AC-v1b-tier-observe-3] slow が作られていない")
	}
}

// [AC-v1b-tier-observe-4]
func TestTierObserve_IntentChangeIsNotThinned(t *testing.T) {
	r := newTierRig(t, tierIntent("5", "1"))
	seedConverged(t, r.fp)

	got := r.observedCycles(4, "slow", func(c int) {
		if c == 3 {
			r.writeIntent(tierIntent("5", "2"))
		}
	})
	// 周1 Observe、周2 間引き、周3 は desired が変わったので Observe・Apply、
	// 周4 は Apply 後の確認で Observe。
	if want := []int{1, 3, 4}; !reflect.DeepEqual(got, want) {
		t.Errorf("[AC-v1b-tier-observe-4] got %v want %v", got, want)
	}
	if size := mustSize(t, r.fp, "slow"); size != 2 {
		t.Errorf("[AC-v1b-tier-observe-4] intent の変更が反映されていない: size=%d", size)
	}
}

// [AC-v1b-tier-observe-5]
func TestTierObserve_InvalidTierDeclarationsRejected(t *testing.T) {
	cases := map[string]struct{ intent, want string }{
		"undeclared tier": {`{"resources":[{"provider":"fake","id":"a","tier":"nope","fields":{"name":"a"}}]}`, `tier "nope" is not declared`},
		"observe_every 0": {`{"tiers":[{"name":"s","observe_every":0}]}`, "observe_every must be >= 1"},
		"duplicate":       {`{"tiers":[{"name":"s","observe_every":2},{"name":"s","observe_every":3}]}`, "declared twice"},
		"empty name":      {`{"tiers":[{"name":"","observe_every":2}]}`, "empty name"},
		"not integer":     {`{"tiers":[{"name":"s","observe_every":"2"}]}`, "observe_every"},
		"unknown key":     {`{"tiers":[{"name":"s","observe_every":2,"every":2}]}`, "every"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "i.json"), []byte(tc.intent), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := loader.LoadIntent(dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("[AC-v1b-tier-observe-5] want error containing %q, got %v", tc.want, err)
			}
		})
	}

	// YAML でも同じく宣言でき、リソースの tier が読める。
	dir := t.TempDir()
	yaml := "tiers:\n  - name: ansible\n    observe_every: 5\nresources:\n  - provider: fake\n    id: a\n    tier: ansible\n    fields:\n      name: a\n"
	if err := os.WriteFile(filepath.Join(dir, "i.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	in, err := loader.LoadIntent(dir)
	if err != nil {
		t.Fatalf("[AC-v1b-tier-observe-5] YAML: %v", err)
	}
	if in.TierIntervals()["ansible"] != 5 || in.Resources[0].Tier != "ansible" {
		t.Errorf("[AC-v1b-tier-observe-5] YAML: tiers=%v resource tier=%q", in.Tiers, in.Resources[0].Tier)
	}
}
