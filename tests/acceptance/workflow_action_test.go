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

// workflow_action_test.go — #139: Workflow の手順の一度きりの操作（action）を、
// 偽 BMC を相手にデーモンの経路（RunCycle）で確かめる（§6.3・§29.1）。
//
// OS の導入: CD を差す → 差さったことを require → 次の 1 回だけ CD から起動
// （action boot_once）→ 再起動（action reset。電源が切れていれば On）→
// インストーラが電源を切るのを待つ → 電源 On（上書きは使い切ったのでディスク
// から）→ CD を抜く。resources（終わったあとの姿）は電源オンと CD は空だけで、
// 起動先の上書きは宣言しない（Workflow が上書きを残さないことを見るため）。
//
//	[AC-139-1]  BMC が受けた要求は CD の Insert → 起動先の PATCH（Once・Cd）→
//	            Reset On の順。Continuous の PATCH は一度も無い。起動したあと上書きは
//	            Disabled に戻っていて、done のあとも残らない。CD は手順で抜かれる。
//	[AC-139-2]  インストーラが電源を切らない（失敗する導入）と wait の timeout で halt し、
//	            その先（電源 On・CD を抜く）は施さない。
//	[AC-139-3]  一度きりの操作は二度送らない: 送りかけの印（S2）が残ったまま起こし
//	            直すと、送らずに halt する。
//	[AC-139-4]  BMC に届かない間は送らずに待ち（送りかけの印も残さない）、届けば送る。
//	[AC-139-5]  ForceRestart は破壊的として予算（§13）に数え、累積の上限を超える 2 回目は
//	            送らずに待つ。GracefulRestart（既定）は数えない。
//	[AC-139-6]  誤った書き方（op の無い action・action に fields・"<provider>/<id>" でない
//	            resource）はロード時に、
//	            provider の知らない操作は実行時に halt で拒否される。
package acceptance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/budget"
	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/redfish/redfishtest"
	"github.com/tjst-t/sakutei/v2/internal/redfish/system"
	"github.com/tjst-t/sakutei/v2/internal/workflow"
)

// osInstallOnceWorkflow は action で 1 回だけ CD から起動する OS 導入の Workflow。
func osInstallOnceWorkflow() string {
	cd := system.MediaName + "/{target}/cd"
	return `{ "workflow": "os-install",
      "target": { "provider": "` + system.Name + `" },
      "stages": [{ "name": "install", "targets": ["node1"] }],
      "steps": [
        { "apply": { "resource": "` + cd + `", "fields": { "image": "` + osInstallISO + `" } } },
        { "require": { "resource": "` + cd + `", "fields": { "image": "` + osInstallISO + `" }, "timeout": "10m" } },
        { "action": { "op": "boot_once", "args": { "target": "Cd" } } },
        { "action": { "op": "reset" } },
        { "wait": { "fields": { "power": "Off" }, "timeout": "1h" } },
        { "apply": { "power": "On" } },
        { "apply": { "resource": "` + cd + `", "fields": { "image": "" } } }
      ] }`
}

func osInstallOnceRig(t *testing.T, opts redfishtest.Options, bc budget.Config, wf string) *wfRF {
	t.Helper()
	return newWFRFWith(t, []string{"node1"}, opts, bc,
		rfResource("node1", `"power": "On"`)+",\n    "+rfMedia("node1", ""), wf)
}

func TestWorkflowAction_BootOnceLeavesNoOverride(t *testing.T) {
	r := osInstallOnceRig(t, redfishtest.Options{PowerState: "Off"}, budget.Config{}, osInstallOnceWorkflow())
	bmc := r.bmcs["node1"]

	// [AC-139-1] 1 周で CD → require → boot_once → reset（Off なので On）まで進み、
	// インストーラの電源オフを待つ。
	r.mustCycle()
	w := r.writes("node1")
	insert, once, on := -1, -1, -1
	for i, x := range w {
		switch {
		case strings.Contains(x, osInstallISO) && insert < 0:
			insert = i
		case strings.HasPrefix(x, "PATCH ") && strings.Contains(x, `"Once"`) && once < 0:
			once = i
		case strings.Contains(x, `"ResetType":"On"`) && on < 0:
			on = i
		}
		if strings.Contains(x, "Continuous") {
			t.Fatalf("[AC-139-1] a Continuous override was written: %v", w)
		}
	}
	if insert < 0 || once < 0 || on < 0 || !(insert < once && once < on) {
		t.Fatalf("[AC-139-1] writes = %v, want InsertMedia → PATCH Once Cd → Reset On", w)
	}
	if bmc.Boots() != 1 || bmc.PowerState() != "On" {
		t.Fatalf("[AC-139-1] boots %d power %s, want one boot", bmc.Boots(), bmc.PowerState())
	}
	if en, _ := bmc.Boot(); en != "Disabled" {
		t.Fatalf("[AC-139-1] boot override after the boot = %s, want Disabled (Once used up)", en)
	}
	if run := r.run(); run.Phase != workflow.PhaseRunning || run.Step != 4 || run.ActionSending {
		t.Fatalf("[AC-139-1] run = %+v, want waiting at step 5 (power Off) with no action in flight", run)
	}

	// インストーラが走っている間に周が回っても、一度きりの操作は繰り返さない。
	for i := 0; i < 2; i++ {
		r.clk.Advance(time.Minute)
		r.mustCycle()
		if w := r.writes("node1"); len(w) != 0 {
			t.Fatalf("[AC-139-1] writes while the installer runs = %v, want none", w)
		}
	}

	// インストーラが電源を切った → 電源 On（ディスクから）→ CD を抜いて done。
	bmc.SetPowerState("Off")
	r.mustCycle()
	if run := r.run(); run.Phase != workflow.PhaseDone {
		t.Fatalf("run = %+v, want done\n%s", run, r.out.String())
	}
	w = r.writes("node1")
	if countContaining(w, "Once") != 0 || countContaining(w, `"ResetType":"On"`) != 1 {
		t.Fatalf("[AC-139-1] writes after the installer = %v, want one Reset On and no override", w)
	}
	if in, _ := bmc.Media(); in {
		t.Fatalf("[AC-139-1] the CD is still inserted after done")
	}
	if en, _ := bmc.Boot(); en != "Disabled" || bmc.PowerState() != "On" || bmc.Boots() != 2 {
		t.Fatalf("[AC-139-1] after done: override %s power %s boots %d", en, bmc.PowerState(), bmc.Boots())
	}
	r.clk.Advance(time.Minute)
	if res := r.mustCycle(); len(res.WorkflowHeld) != 0 {
		t.Fatalf("WorkflowHeld after done = %v", res.WorkflowHeld)
	}
	if w := r.writes("node1"); len(w) != 0 {
		t.Fatalf("[AC-139-1] the loop changed something after done: %v", w)
	}
}

func TestWorkflowAction_FailedInstallDoesNotProceed(t *testing.T) {
	// [AC-139-2]
	r := osInstallOnceRig(t, redfishtest.Options{PowerState: "Off"}, budget.Config{}, osInstallOnceWorkflow())
	bmc := r.bmcs["node1"]
	r.mustCycle()
	r.writes("node1")
	r.clk.Advance(2 * time.Hour)
	res := r.mustCycle()
	run := r.run()
	if run.Phase != workflow.PhaseHalted || run.Step != 4 || !strings.Contains(run.Reason, "timed out") {
		t.Fatalf("[AC-139-2] run = %+v, want halted at the power Off wait", run)
	}
	if !hasEvent(res, workflow.EventHalted, "timed out") {
		t.Fatalf("[AC-139-2] no halt event: %+v", res.WorkflowEvents)
	}
	if in, img := bmc.Media(); !in || img != osInstallISO {
		t.Fatalf("[AC-139-2] CD = %v %q, want still inserted", in, img)
	}
	r.clk.Advance(time.Minute)
	r.mustCycle()
	if w := r.writes("node1"); len(w) != 0 || bmc.Boots() != 1 {
		t.Fatalf("[AC-139-2] the halted run went on: writes %v, boots %d", w, bmc.Boots())
	}
}

func TestWorkflowAction_NeverSentTwice(t *testing.T) {
	// [AC-139-3] CD の取得が失敗する間は手順 2 で待つ。そこで「reset を送りかけて
	// 止まった」記録を S2 に作り、起こし直す。
	r := osInstallOnceRig(t, redfishtest.Options{PowerState: "Off"}, budget.Config{}, osInstallOnceWorkflow())
	bmc := r.bmcs["node1"]
	bmc.SetMediaFetchFails(true)
	r.mustCycle()
	r.writes("node1")
	bmc.SetMediaFetchFails(false)

	store := workflow.NewFileStore(r.storePath)
	run := r.run()
	run.Step, run.ActionSending = 3, true
	if _, err := store.Save(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	r.restart()
	res := r.mustCycle()
	run = r.run()
	if run.Phase != workflow.PhaseHalted || !strings.Contains(run.Reason, "not sending it twice") {
		t.Fatalf("[AC-139-3] run = %+v, want halted without sending", run)
	}
	if !hasEvent(res, workflow.EventHalted, "not sending it twice") {
		t.Fatalf("[AC-139-3] no halt event: %+v", res.WorkflowEvents)
	}
	if s := systemWrites(r.writes("node1")); len(s) != 0 || bmc.Boots() != 0 {
		t.Fatalf("[AC-139-3] the system was touched: %v (boots %d)", s, bmc.Boots())
	}
}

func TestWorkflowAction_UnreachableWaitsThenSends(t *testing.T) {
	// [AC-139-4] boot_once だけの Workflow（BMC が CD を取りに行く手順は要らない）。
	wf := `{ "workflow": "os-install",
      "target": { "provider": "` + system.Name + `" },
      "stages": [{ "name": "install", "targets": ["node1"] }],
      "steps": [
        { "action": { "op": "boot_once", "args": { "target": "Pxe" } } },
        { "action": { "op": "reset" } }
      ] }`
	r := osInstallOnceRig(t, redfishtest.Options{PowerState: "Off"}, budget.Config{}, wf)
	bmc := r.bmcs["node1"]
	bmc.SetUnreachable(true)
	for i := 0; i < 2; i++ {
		res := r.mustCycle()
		if !hasEvent(res, workflow.EventWaiting, "not sent") {
			t.Fatalf("[AC-139-4] cycle %d: no waiting event: %+v", i+1, res.WorkflowEvents)
		}
		if run := r.run(); run.Phase != workflow.PhaseRunning || run.Step != 0 || run.ActionSending {
			t.Fatalf("[AC-139-4] run = %+v, want waiting at step 1 without an action in flight", run)
		}
		r.clk.Advance(time.Minute)
	}
	bmc.SetUnreachable(false)
	r.writes("node1")
	r.mustCycle()
	if run := r.run(); run.Phase != workflow.PhaseDone {
		t.Fatalf("[AC-139-4] run = %+v, want done", run)
	}
	if bmc.Boots() != 1 {
		t.Fatalf("[AC-139-4] boots = %d, want 1", bmc.Boots())
	}
}

func TestWorkflowAction_ForceRestartCountsAgainstBudget(t *testing.T) {
	// [AC-139-5] 累積の上限 1 で ForceRestart を 2 回。GracefulRestart は数えない。
	wf := `{ "workflow": "os-install",
      "target": { "provider": "` + system.Name + `" },
      "stages": [{ "name": "install", "targets": ["node1"] }],
      "steps": [
        { "action": { "op": "reset", "args": { "type": "ForceRestart" } } },
        { "action": { "op": "reset", "args": { "type": "ForceRestart" } } }
      ] }`
	r := osInstallOnceRig(t, redfishtest.Options{PowerState: "On"},
		budget.Config{MaxDestructivePerWindow: 1, WindowDuration: time.Hour}, wf)
	res := r.mustCycle()
	if n := countContaining(r.writes("node1"), "ForceRestart"); n != 1 {
		t.Fatalf("[AC-139-5] ForceRestart sent %d times, want 1 (the second is over the budget)", n)
	}
	if !hasEvent(res, workflow.EventWaiting, "held by the change budget") {
		t.Fatalf("[AC-139-5] no budget wait: %+v", res.WorkflowEvents)
	}
	if run := r.run(); run.Phase != workflow.PhaseRunning || run.Step != 1 || run.ActionSending {
		t.Fatalf("[AC-139-5] run = %+v, want waiting at step 2", run)
	}

	wf = strings.ReplaceAll(wf, `"args": { "type": "ForceRestart" }`, `"args": { "type": "GracefulRestart" }`)
	g := osInstallOnceRig(t, redfishtest.Options{PowerState: "On"},
		budget.Config{MaxDestructivePerWindow: 1, WindowDuration: time.Hour}, wf)
	res = g.mustCycle()
	if hasEvent(res, workflow.EventWaiting, "change budget") {
		t.Fatalf("[AC-139-5] GracefulRestart was held by the budget: %+v", res.WorkflowEvents)
	}
	if run := g.run(); run.Phase != workflow.PhaseDone || countContaining(g.writes("node1"), "GracefulRestart") != 2 {
		t.Fatalf("[AC-139-5] GracefulRestart run = %+v, want done with two restarts", run)
	}
}

func TestWorkflowAction_Rejects(t *testing.T) {
	// [AC-139-6] ロード時。
	base := `{"resources": [], "workflows": [{ "workflow": "w", "target": { "provider": "` + system.Name + `" },
      "stages": [{ "name": "s", "targets": ["node1"] }], "steps": [STEP] }]}`
	for _, c := range []struct{ step, want string }{
		{`{ "action": { "args": { "target": "Cd" } } }`, "action.op must be a string"},
		{`{ "action": { "op": "reset", "fields": { "power": "On" } } }`, "action.fields is not supported"},
		{`{ "action": "reset" }`, "action must be"},
		{`{ "action": { "op": "reset", "args": "ForceRestart" } }`, "action.args must be an object"},
		{`{ "action": { "op": "reset", "resource": "nope" } }`, "<provider>/<id>"},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "intent.json"), []byte(strings.Replace(base, "STEP", c.step, 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := loader.LoadIntent(dir)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("[AC-139-6] %s: err = %v, want %q", c.step, err, c.want)
		}
	}
	// 実行時: provider の知らない操作は halt（送らない）。
	wf := `{ "workflow": "os-install",
      "target": { "provider": "` + system.Name + `" },
      "stages": [{ "name": "install", "targets": ["node1"] }],
      "steps": [ { "action": { "op": "power_cycle" } } ] }`
	r := osInstallOnceRig(t, redfishtest.Options{PowerState: "Off"}, budget.Config{}, wf)
	r.mustCycle()
	if run := r.run(); run.Phase != workflow.PhaseHalted || !strings.Contains(run.Reason, `unknown action "power_cycle"`) {
		t.Fatalf("[AC-139-6] run = %+v, want halted on the unknown action", run)
	}
	if s := systemWrites(r.writes("node1")); len(s) != 0 {
		t.Fatalf("[AC-139-6] the system was touched: %v", s)
	}
}
