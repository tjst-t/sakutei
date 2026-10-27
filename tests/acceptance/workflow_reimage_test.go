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

// workflow_reimage_test.go — #88: 入れ直し（reimage）を宣言で起こす。planned_change の
// kind: reimage（§13.3）と、trigger: reimage の OS 導入の Workflow をつなぐ。偽 BMC を
// 相手にデーモンの経路（RunCycle）で確かめる。qemu-bmc で本当に入れ直すのは
// make integration-guestos の TestReimage。
//
//	[AC-88-1]  宣言が無い間、trigger: reimage の Workflow は走らない（run を作らず、
//	           起動先も電源も触らない）。対象は収束ループが普段どおり収束させる。
//	[AC-88-2]  宣言があっても承認（運用プロファイル team の 1 人）が無ければレーンは
//	           開かず、走らない。
//	[AC-88-3]  承認された宣言で、動いている台に 1 回走る（boot_once → GracefulRestart）。
//	           対象ごとに max_count を 1 つ使い（S4 に executed として残る）、通常予算の
//	           破壊的変更には数えない。max_count を使い切った宣言では、同じ scope の
//	           別の Workflow は起動しない（対象も持たない）。
//	[AC-88-4]  done のあと、同じ宣言ではもう走らない。宣言を外しても走らない。
//	           workflow store を失って起こし直しても、S4 の使った数で max_count を守る。
//	[AC-88-5]  新しい宣言（別の窓）を足すと、もう一度 1 回走る。
//	[AC-88-6]  複数の対象を持つ Workflow は対象ごとに枠を使い、使い切った対象の手前で
//	           halt する（その対象には触らない）。
//	[AC-88-8]  入れ直しの中の破壊的な一度きりの操作（ForceRestart）は、レーンで使った
//	           1 回分に含まれ、通常予算（累積の上限）に止められない。
//	[AC-88-7]  誤った書き方（知らない trigger・target.scope の無い trigger: reimage）は
//	           ロード時に拒否される。
package acceptance

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/audit"
	"github.com/tjst-t/sakutei/v2/internal/budget"
	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/internal/redfish/redfishtest"
	"github.com/tjst-t/sakutei/v2/internal/redfish/system"
	"github.com/tjst-t/sakutei/v2/internal/workflow"
)

// reimageSteps は 1 回だけ CD から起動して入れ直す手順（#139 の形）。
func reimageSteps() string {
	cd := system.MediaName + "/{target}/cd"
	return `[
        { "apply": { "resource": "` + cd + `", "fields": { "image": "` + osInstallISO + `" } } },
        { "require": { "resource": "` + cd + `", "fields": { "image": "` + osInstallISO + `" }, "timeout": "10m" } },
        { "action": { "op": "boot_once", "args": { "target": "Cd" } } },
        { "action": { "op": "reset" } },
        { "wait": { "fields": { "power": "Off" }, "timeout": "1h" } },
        { "apply": { "power": "On" } },
        { "apply": { "resource": "` + cd + `", "fields": { "image": "" } } }
      ]`
}

// reimageWorkflow は trigger: reimage の Workflow（scope rack: r1）。stages は JSON の配列の中身。
func reimageWorkflow(name, stages string) string {
	return `{ "workflow": "` + name + `", "trigger": "reimage",
      "target": { "provider": "` + system.Name + `", "scope": { "rack": "r1" } },
      "stages": [` + stages + `],
      "steps": ` + reimageSteps() + ` }`
}

func reimageStage(node string) string {
	return `{ "name": "` + node + `", "targets": ["` + node + `"] }`
}

// reimageDecl は scope rack: r1 の planned_change kind: reimage。approvers が空なら承認なし。
func reimageDecl(window string, maxCount int, approvers ...string) string {
	approval := ""
	if len(approvers) > 0 {
		approval = `, "approval": { "pr": 88, "approvers": ["` + strings.Join(approvers, `", "`) + `"] }`
	}
	return `{ "scope": { "rack": "r1" }, "kind": "reimage", "max_count": ` + strconv.Itoa(maxCount) +
		`, "window": "` + window + `", "applicant": "carol"` + approval + ` }`
}

// reimageWindows は 2 つの別の宣言の窓（どちらも今を含む。予算の窓は実時刻で見る）。
func reimageWindows() (string, string) {
	now := time.Now()
	return pcWindow(now.Add(-time.Hour), now.Add(6*time.Hour)), pcWindow(now.Add(-2*time.Hour), now.Add(6*time.Hour))
}

// setReimageIntent は終わったあとの姿・Workflow・planned_change（空なら無し）を書く。
func (r *wfRF) setReimageIntent(nodes []string, workflows []string, decls ...string) {
	r.t.Helper()
	intent := `{"resources": [` + osInstallResources(nodes...) + `], "workflows": [` + strings.Join(workflows, ",") + `]`
	if len(decls) > 0 {
		intent += `, "planned_change": [` + strings.Join(decls, ",") + `]`
	}
	intent += `}`
	if err := os.WriteFile(filepath.Join(r.dir, "intent.json"), []byte(intent), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *wfRF) runOf(name string) (workflow.Run, bool) {
	r.t.Helper()
	run, ok, err := workflow.NewFileStore(r.storePath).Get(context.Background(), name)
	if err != nil {
		r.t.Fatal(err)
	}
	return run, ok
}

// cycleText は周の Workflow と planned_change の出来事を行に並べる。
func cycleText(res reconcile.CycleResult) string {
	var b strings.Builder
	for _, ev := range res.WorkflowEvents {
		b.WriteString(ev.String() + "\n")
	}
	for _, ev := range res.PlannedChangeEvents {
		b.WriteString(ev + "\n")
	}
	return b.String()
}

func TestWorkflowReimage_DeclarationRunsOnce(t *testing.T) {
	reimageWindow1, reimageWindow2 := reimageWindows()
	nodes := []string{"node1", "node2"}
	wfs := []string{reimageWorkflow("reimage-node1", reimageStage("node1")), reimageWorkflow("reimage-node2", reimageStage("node2"))}
	r := newWFRFWith(t, nodes, redfishtest.Options{PowerState: "Off"}, budget.Config{MaxDestructivePerCycle: 1},
		osInstallResources(nodes...), strings.Join(wfs, ","))
	r.setReimageIntent(nodes, wfs)

	// [AC-88-1] 宣言が無い: 走らない。対象（電源オン）は普段どおり収束する。
	for i := 0; i < 3; i++ {
		res := r.mustCycle()
		if len(res.WorkflowHeld) != 0 {
			t.Fatalf("[AC-88-1] WorkflowHeld = %v without a declaration", res.WorkflowHeld)
		}
		r.clk.Advance(time.Minute)
	}
	for _, n := range nodes {
		if _, ok := r.runOf("reimage-" + n); ok {
			t.Fatalf("[AC-88-1] reimage-%s has a run without a declaration", n)
		}
		if w := r.writes(n); countContaining(w, "Once") != 0 || countContaining(w, "Restart") != 0 || countContaining(w, osInstallISO) != 0 {
			t.Fatalf("[AC-88-1] %s: writes = %v, want no install", n, w)
		}
		if r.bmcs[n].PowerState() != "On" {
			t.Fatalf("[AC-88-1] %s: power %s, want On (converged by the loop)", n, r.bmcs[n].PowerState())
		}
	}

	// [AC-88-2] 承認の無い宣言: レーンは開かない。
	r.setReimageIntent(nodes, wfs, reimageDecl(reimageWindow1, 1))
	var text strings.Builder
	for i := 0; i < 2; i++ {
		res := r.mustCycle()
		text.WriteString(cycleText(res))
		if len(res.WorkflowHeld) != 0 {
			t.Fatalf("[AC-88-2] WorkflowHeld = %v without approval", res.WorkflowHeld)
		}
		r.clk.Advance(time.Minute)
	}
	if !strings.Contains(text.String(), "PlannedChangeUnapproved") {
		t.Fatalf("[AC-88-2] no PlannedChangeUnapproved:\n%s", text.String())
	}
	for _, n := range nodes {
		if _, ok := r.runOf("reimage-" + n); ok {
			t.Fatalf("[AC-88-2] reimage-%s started without approval", n)
		}
		if w := r.writes(n); len(systemWrites(w)) != 0 {
			t.Fatalf("[AC-88-2] %s: writes = %v", n, w)
		}
	}

	// [AC-88-3] 承認された宣言（max_count 1）: node1 だけが入れ直される。動いているので
	// GracefulRestart。node2 は枠が無いので起動しない。
	r.setReimageIntent(nodes, wfs, reimageDecl(reimageWindow1, 1, "alice"))
	res := r.mustCycle()
	text.Reset()
	text.WriteString(cycleText(res))
	run1, ok := r.runOf("reimage-node1")
	if !ok || run1.Trigger == "" || run1.Phase != workflow.PhaseRunning || run1.Step != 4 {
		t.Fatalf("[AC-88-3] reimage-node1 run = %+v (%v), want running at the power-off wait\n%s", run1, ok, text.String())
	}
	if w := r.writes("node1"); countContaining(w, `"Once"`) != 1 || countContaining(w, `"ResetType":"GracefulRestart"`) != 1 {
		t.Fatalf("[AC-88-3] node1 writes = %v, want one boot_once and one GracefulRestart", w)
	}
	if _, ok := r.runOf("reimage-node2"); ok {
		t.Fatalf("[AC-88-3] reimage-node2 started beyond max_count")
	}
	if w := r.writes("node2"); len(systemWrites(w)) != 0 {
		t.Fatalf("[AC-88-3] node2 writes = %v", w)
	}
	for _, want := range []string{"reimage of redfish-system/", "admitted to the lane (used 1 of max_count 1)",
		"workflow reimage-node2: not starting: planned_change(scope=rack=r1 kind=reimage"} {
		if !strings.Contains(text.String(), want) {
			t.Fatalf("[AC-88-3] events lack %q:\n%s", want, text.String())
		}
	}
	if !containsStr(res.WorkflowHeld, "node1") || containsStr(res.WorkflowHeld, "node2") {
		t.Fatalf("[AC-88-3] WorkflowHeld = %v, want node1 only", res.WorkflowHeld)
	}
	recs, err := audit.ReadAll(r.auditPath)
	if err != nil {
		t.Fatal(err)
	}
	executed := 0
	for _, rec := range recs {
		if rec.Kind == audit.KindPlannedChangeExecuted {
			executed += rec.Count
		}
	}
	if executed != 1 {
		t.Fatalf("[AC-88-3] planned_change.executed in S4 = %d, want 1", executed)
	}
	// インストーラが電源を切る → 電源 On → CD を抜いて done。
	r.clk.Advance(time.Minute)
	r.mustCycle()
	r.bmcs["node1"].SetPowerState("Off")
	r.mustCycle()
	if run, _ := r.runOf("reimage-node1"); run.Phase != workflow.PhaseDone {
		t.Fatalf("[AC-88-3] run = %+v, want done\n%s", run, r.out.String())
	}
	for _, n := range nodes {
		r.writes(n)
	}

	// [AC-88-4] 同じ宣言ではもう走らない。
	for i := 0; i < 2; i++ {
		r.clk.Advance(time.Minute)
		if res := r.mustCycle(); len(res.WorkflowHeld) != 0 {
			t.Fatalf("[AC-88-4] WorkflowHeld = %v after done", res.WorkflowHeld)
		}
	}
	for _, n := range nodes {
		if w := r.writes(n); len(systemWrites(w)) != 0 {
			t.Fatalf("[AC-88-4] %s: writes after done = %v", n, w)
		}
	}
	// workflow store を失って起こし直しても、S4 の使った数で二度目は起こさない。
	if err := os.Remove(r.storePath); err != nil {
		t.Fatal(err)
	}
	r.restart()
	for i := 0; i < 2; i++ {
		r.clk.Advance(time.Minute)
		r.mustCycle()
	}
	for _, n := range nodes {
		if _, ok := r.runOf("reimage-" + n); ok {
			t.Fatalf("[AC-88-4] reimage-%s ran again after the store was lost (max_count is in S4)", n)
		}
		if w := r.writes(n); len(systemWrites(w)) != 0 {
			t.Fatalf("[AC-88-4] %s: writes after the store was lost = %v", n, w)
		}
	}
	// 宣言を外す: 走らない。
	r.setReimageIntent(nodes, wfs)
	for i := 0; i < 2; i++ {
		if res := r.mustCycle(); len(res.WorkflowHeld) != 0 {
			t.Fatalf("[AC-88-4] WorkflowHeld = %v without a declaration", res.WorkflowHeld)
		}
		r.clk.Advance(time.Minute)
	}
	for _, n := range nodes {
		if w := r.writes(n); len(systemWrites(w)) != 0 {
			t.Fatalf("[AC-88-4] %s: writes without a declaration = %v", n, w)
		}
	}

	// [AC-88-5] 新しい宣言（別の窓）: もう一度 1 回走る。
	r.setReimageIntent(nodes, wfs, reimageDecl(reimageWindow2, 1, "alice"))
	r.mustCycle()
	run1b, _ := r.runOf("reimage-node1")
	if run1b.RunID == run1.RunID || run1b.Trigger == run1.Trigger || run1b.Phase != workflow.PhaseRunning {
		t.Fatalf("[AC-88-5] run = %+v, want a new run for the new declaration (old %s)", run1b, run1.RunID)
	}
	if w := r.writes("node1"); countContaining(w, `"ResetType":"GracefulRestart"`) != 1 {
		t.Fatalf("[AC-88-5] node1 writes = %v, want one GracefulRestart", w)
	}
}

func TestWorkflowReimage_MaxCountPerTarget(t *testing.T) {
	// [AC-88-6] 2 台を 1 つの Workflow で。max_count 1 なので node1 だけを入れ直し、
	// node2 の手前で halt する（node2 には触らない）。
	reimageWindow1, _ := reimageWindows()
	nodes := []string{"node1", "node2"}
	wfs := []string{reimageWorkflow("reimage", reimageStage("node1")+","+reimageStage("node2"))}
	r := newWFRFWith(t, nodes, redfishtest.Options{PowerState: "On"}, budget.Config{},
		osInstallResources(nodes...), strings.Join(wfs, ","))
	r.setReimageIntent(nodes, wfs, reimageDecl(reimageWindow1, 1, "alice"))
	r.mustCycle()
	r.bmcs["node1"].SetPowerState("Off")
	r.clk.Advance(time.Minute)
	res := r.mustCycle()
	run, _ := r.runOf("reimage")
	if run.Phase != workflow.PhaseHalted || run.Stage != 1 || !strings.Contains(run.Reason, "not reimaged") ||
		!strings.Contains(run.Reason, "max_count 1") {
		t.Fatalf("[AC-88-6] run = %+v, want halted before node2 (max_count used up)\n%s", run, cycleText(res))
	}
	if w := r.writes("node2"); len(systemWrites(w)) != 0 || countContaining(w, osInstallISO) != 0 {
		t.Fatalf("[AC-88-6] node2 writes = %v, want none", w)
	}
	if in, _ := r.bmcs["node1"].Media(); in {
		t.Fatalf("[AC-88-6] node1's CD is still inserted")
	}
}

func TestWorkflowReimage_ForceRestartInTheLane(t *testing.T) {
	// [AC-88-8] 累積の上限 1 でも、入れ直しの run の ForceRestart 2 回は通る（#139 の
	// [AC-139-5] では 2 回目が止まる）。
	reimageWindow1, _ := reimageWindows()
	wf := `{ "workflow": "reimage", "trigger": "reimage",
      "target": { "provider": "` + system.Name + `", "scope": { "rack": "r1" } },
      "stages": [{ "name": "install", "targets": ["node1"] }],
      "steps": [
        { "action": { "op": "reset", "args": { "type": "ForceRestart" } } },
        { "action": { "op": "reset", "args": { "type": "ForceRestart" } } }
      ] }`
	r := newWFRFWith(t, []string{"node1"}, redfishtest.Options{PowerState: "On"},
		budget.Config{MaxDestructivePerWindow: 1, WindowDuration: time.Hour}, osInstallResources("node1"), wf)
	r.setReimageIntent([]string{"node1"}, []string{wf}, reimageDecl(reimageWindow1, 1, "alice"))
	res := r.mustCycle()
	if n := countContaining(r.writes("node1"), "ForceRestart"); n != 2 {
		t.Fatalf("[AC-88-8] ForceRestart sent %d times, want 2\n%s", n, cycleText(res))
	}
	if run, _ := r.runOf("reimage"); run.Phase != workflow.PhaseDone {
		t.Fatalf("[AC-88-8] run = %+v, want done", run)
	}
	if !strings.Contains(cycleText(res), "sent reset ForceRestart") || !strings.Contains(cycleText(res), "(in the planned_change lane)") {
		t.Fatalf("[AC-88-8] events:\n%s", cycleText(res))
	}
}

func TestWorkflowReimage_LoadErrors(t *testing.T) {
	// [AC-88-7]
	for name, wf := range map[string]string{
		"unknown trigger": `{ "workflow": "w", "trigger": "nightly", "target": { "provider": "fake", "scope": { "rack": "r1" } },
          "stages": [{ "name": "s", "targets": ["n1"] }], "steps": [{ "apply": { "power": "On" } }] }`,
		"no scope": `{ "workflow": "w", "trigger": "reimage", "target": { "provider": "fake" },
          "stages": [{ "name": "s", "targets": ["n1"] }], "steps": [{ "apply": { "power": "On" } }] }`,
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "intent.json"), []byte(`{"workflows": [`+wf+`]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := loader.LoadIntent(dir); err == nil || !strings.Contains(err.Error(), "trigger") {
			t.Errorf("[AC-88-7] %s: err = %v, want a trigger error", name, err)
		}
	}
}
