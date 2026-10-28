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

// version_compat_test.go — Backlog #151: 部品の版の互換の表（intent の
// compatibility:、§29.1）。fake provider の size を部品の版とみなし、装置
// node1 の bmc と bios（node1/bmc・node1/bios）の組を表で縛る:
//
//	bmc=1, bios=1 / bmc=2, bios=1 / bmc=2, bios=2   （bios を先に 2 にすると合わない）
//
//	[AC-151-1]  合う組み合わせ（depends_on で bmc を先に）は plan も reconcile も
//	            通り、表に無い部品（app）は対象外。
//	[AC-151-2]  望む版の組み合わせが表に無い → plan は部品と版を出して止まり、
//	            reconcile も何もしない。
//	[AC-151-3]  途中の組み合わせが合わない順序（順序の無い更新・bios を先にする
//	            depends_on）→ plan は止まり depends_on の案内を出す。reconcile は
//	            何もしない。
//	[AC-151-4]  表を直せば（bmc=1, bios=2 を許す）同じ intent が通る。
//	[AC-151-5]  常駐ループ（strategy: task）: 合わない順序では Task を起こさず
//	            VersionIncompatible で知らせ、depends_on を書けば bmc → bios の順に
//	            Task が走って収束し、知らせは閉じる。
//	[AC-151-6]  表の書き誤り（行に部品が欠ける・版が真偽）は読み込みで断る。
package acceptance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/fakeprovider"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/internal/task"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

var compatRows = []map[string]any{
	{"bmc": "1", "bios": "1"},
	{"bmc": "2", "bios": "1"},
	{"bmc": "2", "bios": "2"},
}

func compatTable(rows []map[string]any) []map[string]any {
	return []map[string]any{{
		"name": "bmc-bios",
		"components": map[string]any{
			"bmc":  map[string]any{"id": "{node}/bmc", "field": "size"},
			"bios": map[string]any{"id": "{node}/bios", "field": "size"},
		},
		"allow": rows,
	}}
}

// compatIntent は node1 の bmc・bios と、表に無い app の intent。biosDeps は
// bios の depends_on、bmcDeps は bmc の depends_on。policy が空でなければ
// bmc・bios の update_policy。
func compatIntent(bmc, bios string, rows []map[string]any, bmcDeps, biosDeps []string, policy map[string]any) map[string]any {
	res := func(id, size string, deps []string) map[string]any {
		m := map[string]any{"provider": "fake", "id": id, "fields": map[string]string{"name": id, "size": size}}
		if len(deps) > 0 {
			m["depends_on"] = deps
		}
		if policy != nil {
			m["update_policy"] = policy
		}
		return m
	}
	return map[string]any{
		"resources": []map[string]any{
			res("node1/bmc", bmc, bmcDeps),
			res("node1/bios", bios, biosDeps),
			{"provider": "fake", "id": "app", "fields": map[string]string{"name": "app", "size": "7"}},
		},
		"compatibility": compatTable(rows),
	}
}

type compatRig struct {
	t         *testing.T
	root      string
	intentDir string
	statePath string
	fp        *fakeprovider.Provider
}

func newCompatRig(t *testing.T) *compatRig {
	t.Helper()
	root := t.TempDir()
	r := &compatRig{t: t, root: root, intentDir: filepath.Join(root, "intent"), statePath: filepath.Join(root, "state.json")}
	if err := os.MkdirAll(r.intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seedState(t, r.statePath, map[string]map[string]any{
		"node1/bmc":  {"name": "node1/bmc", "size": "1"},
		"node1/bios": {"name": "node1/bios", "size": "1"},
		"app":        {"name": "app", "size": "1"},
	})
	r.fp = fakeprovider.New(r.statePath)
	return r
}

func (r *compatRig) write(intent map[string]any) {
	r.t.Helper()
	writeIntentFile(r.t, r.intentDir, intent)
}

func (r *compatRig) sizes() (bmc, bios, app int) {
	r.t.Helper()
	return mustSize(r.t, r.fp, "node1/bmc"), mustSize(r.t, r.fp, "node1/bios"), mustSize(r.t, r.fp, "app")
}

func (r *compatRig) untouched(tag, out string) {
	r.t.Helper()
	if bmc, bios, app := r.sizes(); bmc != 1 || bios != 1 || app != 1 {
		r.t.Fatalf("%s: reality changed (bmc=%d bios=%d app=%d), want nothing done:\n%s", tag, bmc, bios, app, out)
	}
}

func TestVersionCompat_Allowed(t *testing.T) {
	// [AC-151-1]
	r := newCompatRig(t)
	r.write(compatIntent("2", "2", compatRows, nil, []string{"node1/bmc"}, nil))
	out, stderr, code := runPlan(t, r.intentDir, r.statePath)
	if code != 0 || !strings.Contains(out, "node1/bmc") || !strings.Contains(out, "node1/bios") || !strings.Contains(out, "app") {
		t.Fatalf("[AC-151-1] plan: exit %d, want the three updates planned:\n%s\n%s", code, out, stderr)
	}
	out, _, code = runReconcileCombined(t, r.intentDir, r.statePath)
	if bmc, bios, app := r.sizes(); code != 0 || bmc != 2 || bios != 2 || app != 7 {
		t.Fatalf("[AC-151-1] reconcile: exit %d, bmc=%d bios=%d app=%d, want 2/2/7:\n%s", code, bmc, bios, app, out)
	}
}

func TestVersionCompat_DesiredRefused(t *testing.T) {
	// [AC-151-2]
	r := newCompatRig(t)
	r.write(compatIntent("1", "2", compatRows, nil, nil, nil))
	out, stderr, code := runPlan(t, r.intentDir, r.statePath)
	if code == 0 || !strings.Contains(stderr, `compatibility "bmc-bios" (node=node1)`) ||
		!strings.Contains(stderr, "bios=2 (node1/bios)") || !strings.Contains(stderr, "bmc=1 (node1/bmc)") {
		t.Fatalf("[AC-151-2] plan: exit %d, want refused naming the components and versions:\n%s\n%s", code, out, stderr)
	}
	out, _, code = runReconcileCombined(t, r.intentDir, r.statePath)
	if code == 0 || !strings.Contains(out, "not an allowed combination") {
		t.Fatalf("[AC-151-2] reconcile: exit %d, want refused:\n%s", code, out)
	}
	r.untouched("[AC-151-2] reconcile", out)
}

func TestVersionCompat_OrderRefused(t *testing.T) {
	// [AC-151-3]
	r := newCompatRig(t)
	// 順序が無い: bios が先になりうる（bmc=1, bios=2 は表に無い）。
	r.write(compatIntent("2", "2", compatRows, nil, nil, nil))
	out, stderr, code := runPlan(t, r.intentDir, r.statePath)
	if code == 0 || !strings.Contains(stderr, "updating node1/bios first gives") ||
		!strings.Contains(stderr, "declare node1/bios depends_on node1/bmc") {
		t.Fatalf("[AC-151-3] plan without an order: exit %d, want refused with the depends_on hint:\n%s\n%s", code, out, stderr)
	}
	out, _, code = runReconcileCombined(t, r.intentDir, r.statePath)
	if code == 0 || !strings.Contains(out, "VersionIncompatible") || !strings.Contains(out, "nothing was done") {
		t.Fatalf("[AC-151-3] reconcile without an order: exit %d, want refused:\n%s", code, out)
	}
	r.untouched("[AC-151-3] reconcile without an order", out)

	// bios を先にする depends_on: 合わない順序。
	r.write(compatIntent("2", "2", compatRows, []string{"node1/bios"}, nil, nil))
	out, stderr, code = runPlan(t, r.intentDir, r.statePath)
	if code == 0 || !strings.Contains(stderr, "drop the depends_on") {
		t.Fatalf("[AC-151-3] plan with bios first: exit %d, want refused:\n%s\n%s", code, out, stderr)
	}
	out, _, _ = runReconcileCombined(t, r.intentDir, r.statePath)
	r.untouched("[AC-151-3] reconcile with bios first", out)
}

func TestVersionCompat_FixTable(t *testing.T) {
	// [AC-151-4]
	r := newCompatRig(t)
	rows := append([]map[string]any{{"bmc": "1", "bios": "2"}}, compatRows...)
	r.write(compatIntent("2", "2", rows, nil, nil, nil))
	out, stderr, code := runPlan(t, r.intentDir, r.statePath)
	if code != 0 {
		t.Fatalf("[AC-151-4] plan with the fixed table: exit %d:\n%s\n%s", code, out, stderr)
	}
	out, _, code = runReconcileCombined(t, r.intentDir, r.statePath)
	if bmc, bios, _ := r.sizes(); code != 0 || bmc != 2 || bios != 2 {
		t.Fatalf("[AC-151-4] reconcile with the fixed table: exit %d, bmc=%d bios=%d:\n%s", code, bmc, bios, out)
	}
}

func TestVersionCompat_Daemon(t *testing.T) {
	// [AC-151-5]
	r := newCompatRig(t)
	ev := &evBuf{}
	clock := &taskClock{now: time.Date(2026, 10, 24, 3, 0, 0, 0, time.UTC)}
	newDaemon := func() *reconcile.Daemon {
		return reconcile.NewDaemon(reconcile.Config{
			IntentDir: r.intentDir, GeneratedDir: filepath.Join(r.root, "generated"),
			Provider: r.fp, Out: ev, ErrOut: ev, Now: clock.Now,
			TaskStore: task.NewFileStore(filepath.Join(r.root, "tasks.json")), TaskPollInterval: 2 * time.Millisecond,
		})
	}
	cycle := func(d *reconcile.Daemon) reconcile.CycleResult {
		t.Helper()
		res, err := d.RunCycle(context.Background())
		if err != nil {
			t.Fatalf("RunCycle: %v", err)
		}
		d.EndCycle(res, err)
		d.Wait(5 * time.Second)
		return res
	}
	incompat := func(state core.EventState) int {
		ev.mu.Lock()
		defer ev.mu.Unlock()
		n := 0
		for _, e := range ev.evs {
			if e.Kind == core.EventVersionIncompatible && e.State == state {
				n++
			}
		}
		return n
	}
	taskPol := map[string]any{"strategy": "task"}

	r.write(compatIntent("2", "2", compatRows, nil, nil, taskPol))
	d := newDaemon()
	for i := 0; i < 3; i++ {
		res := cycle(d)
		if len(res.Incompatible) != 2 {
			t.Fatalf("[AC-151-5] cycle %d: Incompatible %v, want both held:\n%s", i, res.Incompatible, ev.String())
		}
	}
	if bmc, bios, app := r.sizes(); bmc != 1 || bios != 1 || app != 7 {
		t.Fatalf("[AC-151-5] held: bmc=%d bios=%d app=%d, want 1/1 (no Task) and app applied", bmc, bios, app)
	}
	if strings.Contains(ev.String(), "task ") && strings.Contains(ev.String(), "launched") {
		t.Fatalf("[AC-151-5] a Task was launched while held:\n%s", ev.String())
	}
	if incompat(core.EventFiring) != 2 {
		t.Fatalf("[AC-151-5] want VersionIncompatible firing once per held resource:\n%s", ev.String())
	}

	// depends_on で bmc を先に: bmc の Task → bios の Task → 収束。
	r.write(compatIntent("2", "2", compatRows, nil, []string{"node1/bmc"}, taskPol))
	sawBMCFirst := false
	for i := 0; i < 10; i++ {
		res := cycle(d)
		if len(res.Incompatible) != 0 {
			t.Fatalf("[AC-151-5] ordered: still held %v:\n%s", res.Incompatible, ev.String())
		}
		if bmc, bios, _ := r.sizes(); bmc == 2 && bios == 1 {
			sawBMCFirst = true
		} else if bmc == 1 && bios == 2 {
			t.Fatalf("[AC-151-5] bios was updated before bmc:\n%s", ev.String())
		}
		if bmc, bios, _ := r.sizes(); bmc == 2 && bios == 2 {
			break
		}
	}
	if bmc, bios, _ := r.sizes(); bmc != 2 || bios != 2 || !sawBMCFirst {
		t.Fatalf("[AC-151-5] ordered: bmc=%d bios=%d (bmc first seen: %v), want 2/2 via bmc first:\n%s", bmc, bios, sawBMCFirst, ev.String())
	}
	cycle(d)
	if incompat(core.EventResolved) != 2 {
		t.Fatalf("[AC-151-5] want VersionIncompatible resolved:\n%s", ev.String())
	}
}

func TestVersionCompat_LoadRefusals(t *testing.T) {
	// [AC-151-6]
	for name, rows := range map[string][]map[string]any{
		"row without bios": {{"bmc": "1"}},
		"boolean version":  {{"bmc": true, "bios": "1"}},
	} {
		r := newCompatRig(t)
		r.write(compatIntent("1", "1", rows, nil, nil, nil))
		out, stderr, code := runPlan(t, r.intentDir, r.statePath)
		if code == 0 || !strings.Contains(stderr, "compatibility[0]") {
			t.Fatalf("[AC-151-6] %s: exit %d, want refused at load:\n%s\n%s", name, code, out, stderr)
		}
	}
}
