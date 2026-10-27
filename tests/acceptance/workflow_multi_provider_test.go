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

// workflow_multi_provider_test.go — #87: Workflow の手順ごとに対象の provider・ID
// を書く（resource）・手順の前提条件（require）を、偽 BMC を相手にデーモンの経路
// （RunCycle）で確かめる（§6.3・§29.1）。
//
// OS の導入: 対象は redfish-system のノード、手順は
// 「CD（redfish-virtual-media/{target}/cd）を差す → 差さったことを require で確かめる
// → CD 起動で電源オン → 電源オフを待つ → ディスクから起動」。resources は終わった
// あとの姿（CD は抜く・電源オン・起動先の上書きなし）。
//
//	[AC-87-1]  CD が差さったと観測できるまで電源は入らない。BMC が受けた要求は CD の
//	           Insert → 起動先の PATCH → 電源オンの順。
//	[AC-87-2]  手順が触る CD は、Workflow が done になるまで収束ループが触らない
//	           （resources の「抜く」を当てない）。WorkflowHeld に出る。done で収束に戻り抜かれる。
//	[AC-87-3]  承認ゲート・1 台ずつ・S2 の進行位置・起こし直しからの再開が別の provider の
//	           手順でも同じ規則に乗る（node1 のゲートを承認するまで node2 の CD も電源も触らない）。
//	[AC-87-4]  require は timeout で halt し、その先の手順（電源オン）は施さない。
//	[AC-87-6]  plan・status（#89）は手順が触るリソースも「Workflow に任せる」と示し、
//	           普段の差分（UPDATE・DRIFTED）として出さない。
//	[AC-87-5]  誤った書き方（task に resource・timeout の無い require・"<provider>/<id>"
//	           でない resource・2 つの Workflow が同じ resource を触る）はロード時に拒否される。
package acceptance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/audit"
	"github.com/tjst-t/sakutei/internal/eventlog"
	"github.com/tjst-t/sakutei/internal/fakeprovider"
	"github.com/tjst-t/sakutei/internal/loader"
	"github.com/tjst-t/sakutei/internal/reconcile"
	"github.com/tjst-t/sakutei/internal/redfish/redfishtest"
	"github.com/tjst-t/sakutei/internal/redfish/system"
	"github.com/tjst-t/sakutei/internal/workflow"
	"github.com/tjst-t/sakutei/pkg/provider"
)

const osInstallISO = "/isos/install.iso"

// osInstallResources は終わったあとの姿（nodes ごとに電源オン・上書きなし・CD は空）。
func osInstallResources(nodes ...string) string {
	var parts []string
	for _, n := range nodes {
		parts = append(parts,
			rfResource(n, `"power": "On", "boot_enabled": "Disabled"`),
			rfMedia(n, ""))
	}
	return strings.Join(parts, ",\n    ")
}

// osInstallWorkflow は stages（JSON の配列の中身）を回す OS 導入の Workflow。
func osInstallWorkflow(stages string) string {
	return `{ "workflow": "os-install",
      "target": { "provider": "` + system.Name + `" },
      "stages": [` + stages + `],
      "steps": [
        { "apply": { "resource": "` + system.MediaName + `/{target}/cd", "fields": { "image": "` + osInstallISO + `" } } },
        { "require": { "resource": "` + system.MediaName + `/{target}/cd", "fields": { "image": "` + osInstallISO + `" }, "timeout": "10m" } },
        { "apply": { "boot_enabled": "Continuous", "boot_target": "Cd", "power": "On" } },
        { "wait": { "fields": { "power": "Off" }, "timeout": "1h" } },
        { "apply": { "boot_enabled": "Disabled", "power": "On" } }
      ] }`
}

// wfRF は偽 BMC の rfRig に Workflow の S2・S4 を足したもの。
type wfRF struct {
	*rfRig
	storePath string
	auditPath string
}

func newWFRF(t *testing.T, nodes []string, resources, workflows string) *wfRF {
	t.Helper()
	r := &wfRF{rfRig: newRFRig(t, nodes, redfishtest.Options{PowerState: "Off"}, "", reconcile.Config{})}
	root := filepath.Dir(r.dir)
	r.storePath = filepath.Join(root, "workflows.json")
	r.auditPath = filepath.Join(root, "audit.jsonl")
	intent := `{"resources": [` + resources + `], "workflows": [` + workflows + `]}`
	if err := os.WriteFile(filepath.Join(r.dir, "intent.json"), []byte(intent), 0o644); err != nil {
		t.Fatal(err)
	}
	r.restart()
	for _, n := range nodes {
		r.writes(n)
	}
	return r
}

// restart は同じ intent・現実・S2・S4 で新しいデーモンを作る（起こし直し）。
func (r *wfRF) restart() {
	r.d = reconcile.NewDaemon(reconcile.Config{
		IntentDir:    r.dir,
		GeneratedDir: filepath.Join(filepath.Dir(r.dir), "generated"),
		Providers: func(name string) (provider.Provider, error) {
			switch name {
			case system.Name:
				return r.p, nil
			case system.MediaName:
				return r.m, nil
			}
			return nil, os.ErrNotExist
		},
		Out:           r.out,
		ErrOut:        r.errOut,
		EventSinks:    []eventlog.Sink{r.rec},
		Now:           r.clk.Now,
		WorkflowStore: workflow.NewFileStore(r.storePath),
		Audit:         audit.NewLog(r.auditPath),
	})
}

func (r *wfRF) run() workflow.Run {
	r.t.Helper()
	run, _, err := workflow.NewFileStore(r.storePath).Get(context.Background(), "os-install")
	if err != nil {
		r.t.Fatal(err)
	}
	return run
}

// systemWrites は writes のうち redfish-system への要求（起動先の PATCH・Reset）。
func systemWrites(w []string) []string {
	var out []string
	for _, x := range w {
		if strings.HasPrefix(x, "PATCH ") || strings.Contains(x, "ResetType") {
			out = append(out, x)
		}
	}
	return out
}

func TestWorkflowMultiProvider_CDBeforePower(t *testing.T) {
	r := newWFRF(t, []string{"node1"}, osInstallResources("node1"),
		osInstallWorkflow(`{ "name": "install", "targets": ["node1"] }`))
	bmc := r.bmcs["node1"]

	// [AC-87-1] BMC が ISO を取りに行けない間（Insert しても Inserted が false に戻る）、
	// 手順 1 は確かめられず、電源も起動先も触らない。
	bmc.SetMediaFetchFails(true)
	for i := 0; i < 3; i++ {
		res := r.mustCycle()
		w := r.writes("node1")
		if countContaining(w, osInstallISO) == 0 {
			t.Fatalf("[AC-87-1] cycle %d: no InsertMedia of the ISO: %v", i+1, w)
		}
		if s := systemWrites(w); len(s) != 0 {
			t.Fatalf("[AC-87-1] cycle %d: the system was touched before the CD was inserted: %v", i+1, s)
		}
		if bmc.PowerState() != "Off" {
			t.Fatalf("[AC-87-1] power = %s before the CD was inserted", bmc.PowerState())
		}
		// [AC-87-2] CD は Workflow の手順が触るので、収束ループは対象として持つ。
		if !containsStr(res.WorkflowHeld, "node1/cd") || !containsStr(res.WorkflowHeld, "node1") {
			t.Fatalf("[AC-87-2] WorkflowHeld = %v, want node1 and node1/cd", res.WorkflowHeld)
		}
		r.clk.Advance(time.Minute)
	}
	if run := r.run(); run.Phase != workflow.PhaseRunning || run.Step != 0 {
		t.Fatalf("[AC-87-1] run = %+v, want running at step 1", run)
	}

	// 取得元が直ると、同じ周に CD → require → 電源オンの順に進む。
	bmc.SetMediaFetchFails(false)
	r.mustCycle()
	w := r.writes("node1")
	insert, patch, on := -1, -1, -1
	for i, x := range w {
		switch {
		case strings.Contains(x, osInstallISO) && insert < 0:
			insert = i
		case strings.HasPrefix(x, "PATCH ") && patch < 0:
			patch = i
		case strings.Contains(x, `"ResetType":"On"`) && on < 0:
			on = i
		}
	}
	if insert < 0 || patch < 0 || on < 0 || !(insert < patch && patch < on) {
		t.Fatalf("[AC-87-1] writes = %v, want InsertMedia → PATCH boot → Reset On", w)
	}
	if in, img := bmc.Media(); !in || img != osInstallISO || bmc.PowerState() != "On" {
		t.Fatalf("[AC-87-1] media = %v %q, power %s", in, img, bmc.PowerState())
	}
	if run := r.run(); run.Step != 3 {
		t.Fatalf("[AC-87-1] run = %+v, want waiting at step 4 (power Off)", run)
	}

	// [AC-87-2] インストーラが走っている間、resources の「CD は空」を当てない。
	r.clk.Advance(time.Minute)
	r.mustCycle()
	if w := r.writes("node1"); len(w) != 0 {
		t.Fatalf("[AC-87-2] writes while the installer runs = %v, want none", w)
	}

	// インストーラが終わって電源を切った → ディスクから起動して done。
	bmc.SetPowerState("Off")
	r.mustCycle()
	if run := r.run(); run.Phase != workflow.PhaseDone {
		t.Fatalf("run = %+v, want done\n%s", run, r.out.String())
	}
	if en, _ := bmc.Boot(); en != "Disabled" || bmc.PowerState() != "On" {
		t.Fatalf("boot %s power %s after the workflow", en, bmc.PowerState())
	}
	// [AC-87-2] done になると CD は収束に戻り、resources どおり抜かれる。
	r.writes("node1")
	r.clk.Advance(time.Minute)
	res := r.mustCycle()
	if len(res.WorkflowHeld) != 0 {
		t.Fatalf("[AC-87-2] WorkflowHeld after done = %v", res.WorkflowHeld)
	}
	if in, _ := bmc.Media(); in {
		t.Fatalf("[AC-87-2] the CD was not ejected once the workflow was done; writes %v", r.writes("node1"))
	}
}

func TestWorkflowMultiProvider_GateOneAtATimeRestart(t *testing.T) {
	// [AC-87-3]
	r := newWFRF(t, []string{"node1", "node2"}, osInstallResources("node1", "node2"),
		osInstallWorkflow(`{ "name": "first", "targets": ["node1"], "gate": { "manual_approval": "required", "timeout": "1h" } },
        { "name": "rest", "targets": ["node2"] }`))
	r.mustCycle()
	if run := r.run(); run.Step != 3 || run.Target != 0 || run.Stage != 0 {
		t.Fatalf("run = %+v, want node1 at step 4", run)
	}
	r.bmcs["node1"].SetPowerState("Off")
	r.mustCycle()
	if run := r.run(); run.Phase != workflow.PhaseAwaitingApproval || run.Gate != "first" {
		t.Fatalf("[AC-87-3] run = %+v, want awaiting approval at gate first", run)
	}
	if w := r.writes("node2"); len(w) != 0 {
		t.Fatalf("[AC-87-3] node2 was touched before the gate: %v", w)
	}

	// 起こし直しても待ちに留まり、node2 の CD も電源も触らない。
	r.restart()
	for i := 0; i < 2; i++ {
		r.clk.Advance(time.Minute)
		r.mustCycle()
	}
	if run := r.run(); run.Phase != workflow.PhaseAwaitingApproval {
		t.Fatalf("[AC-87-3] run after restart = %+v", run)
	}
	if in, _ := r.bmcs["node2"].Media(); in || r.bmcs["node2"].PowerState() != "Off" || len(r.writes("node2")) != 0 {
		t.Fatalf("[AC-87-3] node2 was touched while awaiting approval")
	}

	if _, err := workflow.Decide(context.Background(), workflow.NewFileStore(r.storePath), audit.NewLog(r.auditPath),
		"os-install", "first", workflow.Approval{Approver: "bob", Signature: "sig", Decision: workflow.DecisionApprove}, r.clk.Now()); err != nil {
		t.Fatal(err)
	}
	r.restart()
	r.mustCycle()
	if in, img := r.bmcs["node2"].Media(); !in || img != osInstallISO || r.bmcs["node2"].PowerState() != "On" {
		t.Fatalf("[AC-87-3] node2 after approval: media %v %q, power %s", in, img, r.bmcs["node2"].PowerState())
	}
	if run := r.run(); run.Stage != 1 || run.Step != 3 {
		t.Fatalf("[AC-87-3] run = %+v, want node2 at step 4", run)
	}
}

func TestWorkflowMultiProvider_RequireTimesOut(t *testing.T) {
	// [AC-87-4] 誰かが差すはずの CD を前提にする（Workflow 自身は差さない）。
	wf := `{ "workflow": "os-install",
      "target": { "provider": "` + system.Name + `" },
      "stages": [{ "name": "install", "targets": ["node1"] }],
      "steps": [
        { "require": { "resource": "` + system.MediaName + `/{target}/cd", "fields": { "image": "` + osInstallISO + `" }, "timeout": "5m" } },
        { "apply": { "boot_enabled": "Continuous", "boot_target": "Cd", "power": "On" } }
      ] }`
	r := newWFRF(t, []string{"node1"}, rfResource("node1", `"power": "On", "boot_enabled": "Disabled"`), wf)
	res := r.mustCycle()
	if !hasEvent(res, workflow.EventWaiting, "waiting for map[image:"+osInstallISO+"]") {
		t.Fatalf("[AC-87-4] no waiting event: %+v", res.WorkflowEvents)
	}
	r.clk.Advance(6 * time.Minute)
	res = r.mustCycle()
	if run := r.run(); run.Phase != workflow.PhaseHalted || !strings.Contains(run.Reason, "require") || !strings.Contains(run.Reason, "timed out") {
		t.Fatalf("[AC-87-4] run = %+v, want halted by the require timeout", run)
	}
	if !hasEvent(res, workflow.EventHalted, "timed out") {
		t.Fatalf("[AC-87-4] no halt event: %+v", res.WorkflowEvents)
	}
	if s := systemWrites(r.writes("node1")); len(s) != 0 || r.bmcs["node1"].PowerState() != "Off" {
		t.Fatalf("[AC-87-4] the system was touched: %v (power %s)", s, r.bmcs["node1"].PowerState())
	}
}

func TestWorkflowMultiProvider_LoadRejects(t *testing.T) {
	// [AC-87-5]
	base := func(steps string) string {
		return `{"workflows": [{ "workflow": "w", "target": { "provider": "fake" },
          "stages": [{ "name": "s", "targets": ["n1"] }], "steps": [` + steps + `] }]}`
	}
	cases := map[string]struct{ intent, want string }{
		"task with resource":      {base(`{ "task": { "resource": "fake/x", "fields": { "a": "1" } } }`), "task takes the target's fields"},
		"require without timeout": {base(`{ "require": { "resource": "fake/x", "fields": { "a": "1" } } }`), "timeout is required"},
		"resource without id":     {base(`{ "apply": { "resource": "fake", "fields": { "a": "1" } } }`), "<provider>/<id>"},
		"unknown placeholder":     {base(`{ "wait": { "resource": "fake/{node}", "fields": { "a": "1" } } }`), "only {target}"},
		"unknown apply key":       {base(`{ "apply": { "resource": "fake/x", "fields": { "a": "1" }, "timeout": "1m" } }`), "apply.timeout"},
		"shared step resource": {`{"workflows": [
          { "workflow": "a", "target": { "provider": "fake" }, "stages": [{ "name": "s", "targets": ["n1"] }],
            "steps": [{ "apply": { "resource": "media/shared", "fields": { "a": "1" } } }] },
          { "workflow": "b", "target": { "provider": "fake" }, "stages": [{ "name": "s", "targets": ["n2"] }],
            "steps": [{ "require": { "resource": "media/shared", "fields": { "a": "1" }, "timeout": "1m" } }] }]}`, "shared is in both workflow a and b"},
	}
	for name, c := range cases {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "intent.json"), []byte(c.intent), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := loader.LoadIntent(dir)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("[AC-87-5] %s: err = %v, want containing %q", name, err, c.want)
		}
	}

	// 今の書き方（target の fields をそのまま書く apply）と新しい書き方が並べて書ける。
	dir := t.TempDir()
	ok := base(`{ "apply": { "active": "false" } }, { "apply": { "resource": "media/{target}-cd", "fields": { "image": "x" } } }, { "wait": { "resource": "media/{target}-cd", "fields": { "image": "x" } } }`)
	if err := os.WriteFile(filepath.Join(dir, "intent.json"), []byte(ok), 0o644); err != nil {
		t.Fatal(err)
	}
	in, err := loader.LoadIntent(dir)
	if err != nil {
		t.Fatalf("[AC-87-5] %v", err)
	}
	steps := in.Workflows[0].Steps
	if steps[0].Resource != "" || steps[0].Fields["active"] != "false" || steps[1].Resource != "media/{target}-cd" || steps[1].Fields["image"] != "x" {
		t.Fatalf("[AC-87-5] steps = %+v", steps)
	}
	target := in.Workflows[0].Targets()[0]
	if id := steps[1].ResourceFor(target); id.Provider != "media" || id.Local != "n1-cd" {
		t.Fatalf("[AC-87-5] ResourceFor = %+v", id)
	}
	res := in.Workflows[0].Resources()
	if len(res) != 2 || res[1].Provider != "media" || res[1].Local != "n1-cd" {
		t.Fatalf("[AC-87-5] Resources = %+v", res)
	}
}

func TestWorkflowMultiProvider_PlanStatusShowStepResources(t *testing.T) {
	// [AC-87-6] 対象 n1 の手順が別のリソース n1-cd を触る。どちらも desired と現実が違う。
	root := t.TempDir()
	dir := filepath.Join(root, "intent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	intent := `{
  "resources": [
    { "provider": "fake", "id": "n1", "fields": { "name": "n1", "size": "2" } },
    { "provider": "fake", "id": "n1-cd", "fields": { "name": "n1-cd", "size": "0" } },
    { "provider": "fake", "id": "app", "fields": { "name": "app", "size": "1" } }
  ],
  "workflows": [
    { "workflow": "install", "target": { "provider": "fake" },
      "stages": [{ "name": "s", "targets": ["n1"] }],
      "steps": [
        { "apply": { "resource": "fake/{target}-cd", "fields": { "size": "9" } } },
        { "require": { "resource": "fake/{target}-cd", "fields": { "size": "9" }, "timeout": "5m" } },
        { "apply": { "size": "2" } }
      ] }
  ]
}`
	if err := os.WriteFile(filepath.Join(dir, "intent.json"), []byte(intent), 0o644); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "state.json")
	initStateFile(t, state)
	fp := fakeprovider.New(state)
	for _, n := range []string{"n1", "n1-cd", "app"} {
		if err := fp.SeedResource(n, map[string]any{"name": n, "size": 5}); err != nil {
			t.Fatal(err)
		}
	}

	out, stderr, code := runPlan(t, dir, state)
	if code != 0 {
		t.Fatalf("plan exit %d:\n%s", code, stderr)
	}
	if !strings.Contains(out, "Workflow に任せる（収束ループは触らない）: 2 target(s)") || !strings.Contains(out, "~ UPDATE fake//app") {
		t.Fatalf("[AC-87-6] plan output:\n%s", out)
	}
	for _, n := range []string{"n1", "n1-cd"} {
		if !strings.Contains(out, "= WORKFLOW fake//"+n) {
			t.Fatalf("[AC-87-6] plan must show %s as left to its Workflow:\n%s", n, out)
		}
	}
	out, stderr, code = runStatusCmd(t, dir, state)
	if code != 0 {
		t.Fatalf("status exit %d:\n%s", code, stderr)
	}
	if !strings.Contains(out, "Workflow に任せる（収束ループは触らない）: 2 リソース") || !strings.Contains(out, "DRIFTED fake//app") {
		t.Fatalf("[AC-87-6] status output:\n%s", out)
	}
	for _, n := range []string{"n1", "n1-cd"} {
		if !strings.Contains(out, "WORKFLOW fake//"+n) {
			t.Fatalf("[AC-87-6] status must show %s as left to its Workflow:\n%s", n, out)
		}
	}
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
