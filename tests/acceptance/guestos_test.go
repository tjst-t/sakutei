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

// guestos_test.go — #86: OS の導入の完了を、BMC から見た電源の状態ではなく
// ゲスト OS の観測（guest-os provider、ssh 越し）で判断する（§29.1）。偽 BMC と
// 偽のホスト（remotefake）を相手に、デーモンの経路（RunCycle）で確かめる。
//
// Workflow は #87 の OS 導入の手順のあとに「ゲスト OS が目印つきで起動したのを
// 待つ（wait resource: guest-os/{target}）→ CD を抜く」を足したもの。CD を抜く
// 手順まで進んだかどうかで「先へ進んだか」を見る。
//
//	[AC-86-1]  インストーラが成功し、ゲスト OS が目印つきで起動すると先へ進む
//	           （CD を抜いて done）。起動するまで（届かない間）は待つ。
//	[AC-86-2]  インストーラが失敗しても電源を切る（BMC から見て Off）と、手順は
//	           ディスクからの起動までは進むが、ゲスト OS に届かない（unknown）まま
//	           待ち、timeout で halt する。CD を抜く手順は施さない。
//	[AC-86-3]  ゲスト OS に届いても目印が無い・違う（前の導入の目印）なら待ち、
//	           timeout で halt する。
//	[AC-86-4]  guest-os は観測だけ: intent の resources に書くと周を飛ばす
//	           （何も Observe・Apply しない）。
//	[AC-86-8]  registry: guest_hosts と remote_config（#130 の対象の一覧）と
//	           guest_markers で作れる。足りない・誤った設定はエラー。
package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/audit"
	"github.com/tjst-t/sakutei/internal/eventlog"
	"github.com/tjst-t/sakutei/internal/guestos"
	"github.com/tjst-t/sakutei/internal/reconcile"
	"github.com/tjst-t/sakutei/internal/redfish/system"
	"github.com/tjst-t/sakutei/internal/remote"
	"github.com/tjst-t/sakutei/internal/remote/remotefake"
	"github.com/tjst-t/sakutei/internal/workflow"
	"github.com/tjst-t/sakutei/pkg/provider"
	"github.com/tjst-t/sakutei/pkg/registry"
	"github.com/tjst-t/sakutei/pkg/sdk"
)

const guestMark = "sakutei-run-2"

// guestInstallWorkflow は OS の導入（#87 の手順）+ ゲスト OS の目印を待つ + CD を抜く。
func guestInstallWorkflow() string {
	return `{ "workflow": "os-install",
      "target": { "provider": "` + system.Name + `" },
      "stages": [{ "name": "install", "targets": ["node1"] }],
      "steps": [
        { "apply": { "resource": "` + system.MediaName + `/{target}/cd", "fields": { "image": "` + osInstallISO + `" } } },
        { "require": { "resource": "` + system.MediaName + `/{target}/cd", "fields": { "image": "` + osInstallISO + `" }, "timeout": "10m" } },
        { "apply": { "boot_enabled": "Continuous", "boot_target": "Cd", "power": "On" } },
        { "wait": { "fields": { "power": "Off" }, "timeout": "1h" } },
        { "apply": { "boot_enabled": "Disabled", "power": "On" } },
        { "wait": { "resource": "` + guestos.Name + `/{target}", "fields": { "os_id": "alpine", "marker.installed": "` + guestMark + `" }, "timeout": "10m" } },
        { "apply": { "resource": "` + system.MediaName + `/{target}/cd", "fields": { "image": "" } } }
      ] }`
}

// guestRig は wfRF に偽のホスト（ゲスト OS）と guest-os provider を足したもの。
type guestRig struct {
	*wfRF
	host *remotefake.Host
	g    *guestos.Provider
}

func newGuestRig(t *testing.T, resources string) *guestRig {
	t.Helper()
	r := &guestRig{wfRF: newWFRF(t, []string{"node1"}, resources, guestInstallWorkflow())}
	r.host = &remotefake.Host{Name: "node1", Root: t.TempDir()}
	// 電源が入るまで、ゲスト OS には届かない。
	r.host.SetDown(remote.Unreachable)
	markers, err := guestos.ParseMarkers("installed=/etc/sakutei-installed")
	if err != nil {
		t.Fatal(err)
	}
	if r.g, err = guestos.New(map[string]remote.Executor{"node1": r.host}, markers); err != nil {
		t.Fatal(err)
	}
	r.restart()
	return r
}

func (r *guestRig) restart() {
	r.d = reconcile.NewDaemon(reconcile.Config{
		IntentDir:    r.dir,
		GeneratedDir: filepath.Join(filepath.Dir(r.dir), "generated"),
		Providers: func(name string) (provider.Provider, error) {
			switch name {
			case system.Name:
				return r.p, nil
			case system.MediaName:
				return r.m, nil
			case guestos.Name:
				return r.g, nil
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

// boot はゲスト OS が起動した姿にする（os-release と、mark があれば目印）。
func (r *guestRig) boot(mark string) {
	r.t.Helper()
	files := map[string]string{
		"etc/os-release":           "ID=alpine\nVERSION_ID=3.24.3\nPRETTY_NAME=\"Alpine Linux v3.24\"\n",
		"proc/sys/kernel/hostname": "sakutei-node\n",
	}
	if mark != "" {
		files["etc/sakutei-installed"] = mark + "\n"
	}
	for p, body := range files {
		full := filepath.Join(r.host.Root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
	r.host.Up()
}

// toGuestWait は手順 1〜5 を進め、インストーラが電源を切ってディスクから起動した
// ところ（手順 6 の待ち）まで来たことを確かめる。
func (r *guestRig) toGuestWait() {
	r.t.Helper()
	r.mustCycle()
	if run := r.run(); run.Step != 3 {
		r.t.Fatalf("run = %+v, want waiting for power Off\n%s", run, r.out.String())
	}
	// インストーラは（成功しても失敗しても）電源を切る。BMC から見えるのはこれだけ。
	r.bmcs["node1"].SetPowerState("Off")
	r.mustCycle()
	if run := r.run(); run.Phase != workflow.PhaseRunning || run.Step != 5 {
		r.t.Fatalf("run = %+v, want waiting for the guest OS (step 6)\n%s", run, r.out.String())
	}
	if en, _ := r.bmcs["node1"].Boot(); en != "Disabled" || r.bmcs["node1"].PowerState() != "On" {
		r.t.Fatalf("boot %s power %s: want booted from disk", en, r.bmcs["node1"].PowerState())
	}
}

func (r *guestRig) cdInserted() bool {
	in, img := r.bmcs["node1"].Media()
	return in && img == osInstallISO
}

func TestGuestOS_InstallSucceeded(t *testing.T) {
	// [AC-86-1]
	r := newGuestRig(t, osInstallResources("node1"))
	r.toGuestWait()

	// 起動するまでは届かない: 待つ（unknown ≠ absent）。先へ進まない。
	for i := 0; i < 3; i++ {
		r.clk.Advance(time.Minute)
		res := r.mustCycle()
		if !hasEvent(res, workflow.EventWaiting, "unobservable") {
			t.Fatalf("[AC-86-1] no waiting (unobservable) event: %+v", res.WorkflowEvents)
		}
		if !containsStr(res.WorkflowHeld, "node1") {
			t.Fatalf("[AC-86-1] WorkflowHeld = %v", res.WorkflowHeld)
		}
		if !r.cdInserted() {
			t.Fatalf("[AC-86-1] the CD was ejected before the guest OS was observed")
		}
	}
	if run := r.run(); run.Step != 5 {
		t.Fatalf("[AC-86-1] run = %+v", run)
	}

	// 目印つきで起動した → 先へ進み（CD を抜く）、done。
	r.boot(guestMark)
	r.clk.Advance(time.Minute)
	res := r.mustCycle()
	if !hasEvent(res, workflow.EventStepDone, "map[marker.installed:"+guestMark+" os_id:alpine]") {
		t.Fatalf("[AC-86-1] no step-done event for the guest OS: %+v", res.WorkflowEvents)
	}
	if run := r.run(); run.Phase != workflow.PhaseDone {
		t.Fatalf("[AC-86-1] run = %+v, want done\n%s", run, r.out.String())
	}
	if in, _ := r.bmcs["node1"].Media(); in {
		t.Fatalf("[AC-86-1] the CD is still inserted after the workflow")
	}
}

func TestGuestOS_InstallerFailedButPoweredOff(t *testing.T) {
	// [AC-86-2] ディスクに何も入っていない: 電源を入れてもゲスト OS は上がらない。
	r := newGuestRig(t, osInstallResources("node1"))
	r.toGuestWait()
	for i := 0; i < 9; i++ {
		r.clk.Advance(time.Minute)
		res := r.mustCycle()
		// 待ちの理由に、なぜ観測できないか（届かない）を添える。
		if !hasEvent(res, workflow.EventWaiting, "(unobservable: ") || !hasEvent(res, workflow.EventWaiting, "remote node1: unreachable") {
			t.Fatalf("[AC-86-2] minute %d: no waiting event with the reason: %+v", i+1, res.WorkflowEvents)
		}
	}
	r.clk.Advance(2 * time.Minute)
	res := r.mustCycle()
	run := r.run()
	if run.Phase != workflow.PhaseHalted || run.Step != 5 || !strings.Contains(run.Reason, "guest-os/node1") || !strings.Contains(run.Reason, "timed out") {
		t.Fatalf("[AC-86-2] run = %+v, want halted at the guest OS wait by its timeout", run)
	}
	if !hasEvent(res, workflow.EventHalted, "timed out") {
		t.Fatalf("[AC-86-2] no halt event: %+v", res.WorkflowEvents)
	}
	// 先へ進んでいない: CD を抜く手順は施されず、halt の間は収束ループも触らない。
	for i := 0; i < 2; i++ {
		r.clk.Advance(time.Minute)
		res := r.mustCycle()
		if !r.cdInserted() {
			t.Fatalf("[AC-86-2] the CD was ejected after the halt")
		}
		if !containsStr(res.WorkflowHeld, "node1/cd") {
			t.Fatalf("[AC-86-2] WorkflowHeld = %v", res.WorkflowHeld)
		}
	}
	// halt のあとにゲスト OS が上がっても、勝手に進み直さない。
	r.boot(guestMark)
	r.clk.Advance(time.Minute)
	r.mustCycle()
	if run := r.run(); run.Phase != workflow.PhaseHalted || !r.cdInserted() {
		t.Fatalf("[AC-86-2] run = %+v after the guest came up later", run)
	}
}

func TestGuestOS_MarkerMissingOrStale(t *testing.T) {
	// [AC-86-3]
	for name, mark := range map[string]string{"missing": "", "stale": "sakutei-run-1"} {
		t.Run(name, func(t *testing.T) {
			r := newGuestRig(t, osInstallResources("node1"))
			r.toGuestWait()
			r.boot(mark)
			r.clk.Advance(time.Minute)
			res := r.mustCycle()
			if !hasEvent(res, workflow.EventWaiting, "waiting for map[marker.installed:"+guestMark) || hasEvent(res, workflow.EventWaiting, "unobservable") {
				t.Fatalf("[AC-86-3] events = %+v, want waiting for the marker (observable)", res.WorkflowEvents)
			}
			r.clk.Advance(10 * time.Minute)
			r.mustCycle()
			if run := r.run(); run.Phase != workflow.PhaseHalted || !strings.Contains(run.Reason, "timed out") {
				t.Fatalf("[AC-86-3] run = %+v, want halted", run)
			}
			if !r.cdInserted() {
				t.Fatalf("[AC-86-3] the CD was ejected")
			}
		})
	}
}

func TestGuestOS_NotAResource(t *testing.T) {
	// [AC-86-4]
	r := newGuestRig(t, osInstallResources("node1")+`,
    {"provider": "`+guestos.Name+`", "id": "node2", "fields": {"os_id": "alpine"}}`)
	before := len(r.host.Calls())
	_, err := r.cycle()
	if err == nil || !strings.Contains(err.Error(), "observes only") {
		t.Fatalf("[AC-86-4] RunCycle err = %v, want the guest-os resource rejected", err)
	}
	if w := r.writes("node1"); len(w) != 0 {
		t.Fatalf("[AC-86-4] writes = %v, want none (the cycle is skipped)", w)
	}
	if n := len(r.host.Calls()); n != before {
		t.Fatalf("[AC-86-4] the guest was observed")
	}
}

func TestGuestOS_Registry(t *testing.T) {
	// [AC-86-8]
	dir := t.TempDir()
	cfg := filepath.Join(dir, "hosts.yaml")
	if err := os.WriteFile(cfg, []byte("known_hosts: known_hosts\nhosts:\n  node1:\n    address: 192.0.2.1\n  node2:\n    address: 192.0.2.2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := registry.DefaultRegistry()
	p, err := reg.Get(guestos.Name, map[string]string{"remote_config": cfg, "guest_hosts": "node1,node2", "guest_markers": "installed=/etc/sakutei-installed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := sdk.ValidateProvider(p); err != nil {
		t.Fatalf("[AC-86-8] %v", err)
	}
	if _, ok := p.Schema().Fields["marker.installed"]; !ok {
		t.Errorf("[AC-86-8] schema = %v, want marker.installed", p.Schema().Fields)
	}
	for name, c := range map[string]map[string]string{
		"no hosts":       {"remote_config": cfg},
		"no config":      {"guest_hosts": "node1"},
		"unknown host":   {"remote_config": cfg, "guest_hosts": "node9"},
		"bad marker":     {"remote_config": cfg, "guest_hosts": "node1", "guest_markers": "installed=relative"},
		"duplicate host": {"remote_config": cfg, "guest_hosts": "node1,node1"},
	} {
		if _, err := reg.Get(guestos.Name, c); err == nil {
			t.Errorf("[AC-86-8] %s must be an error", name)
		}
	}
}
