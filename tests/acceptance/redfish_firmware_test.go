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

// redfish_firmware_test.go — #148: redfish-firmware（FW の版の観測と、多相 Task
// での更新）を常駐ループ（RunCycle）で偽 BMC に向ける（§29.1「Redfish の FW の
// 更新」、§6.2 多相型）。
//
//	[AC-148-7]   版が違えば多相 Task が起き、承認ゲートで止まる。承認すると
//	             SimpleUpdate を 1 回送り、BMC の Task の完了を待ち、宣言した
//	             再起動で版が宣言どおりになったことを観測して閉じる。収束ループは
//	             Apply しない。閉じたあとは何もしない。
//	[AC-148-8]   承認が無ければ流さない（何周回しても BMC に送らない）。却下は halt。
//	[AC-148-9]   版が合っていれば Task を起こさず、BMC に何も送らない。
//	[AC-148-10]  途中で落ちても二重に流さない: staging の後で落ちた Task は、入口の
//	             観測で送ったと分かって送り直さない。applying（BMC が書いている）の
//	             途中で落ちた Task は人手ゲートで止まり、BMC の Task が終わって版が
//	             合ったと観測できれば承認なしで閉じる。
//	[AC-148-11]  更新は破壊的として予算（max-destructive-per-cycle）に数え、超えれば
//	             Task を起こさない。
//	[AC-148-14]  rebooting の途中で落ちても二度目の再起動を送らない: Reset を送った
//	             後の待ちの間に落ちた Task は、引き継いだデーモンが待つだけ（S2 の
//	             Awaiting の印）。Reset を送る最中に落ちた Task は人手ゲートで止まり、
//	             再起動で版が合ったと観測できれば承認なしで閉じる。
//	[AC-148-13]  前の更新の残りかす（同じイメージで前に済んだ BMC の Task）が
//	             TaskService に残ったまま版が戻っていても、承認なしでは何も送らず、
//	             再起動もしない。承認すると送り直して版を合わせる。
package acceptance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/audit"
	"github.com/tjst-t/sakutei/v2/internal/budget"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/internal/redfish"
	"github.com/tjst-t/sakutei/v2/internal/redfish/redfishtest"
	"github.com/tjst-t/sakutei/v2/internal/redfish/system"
	"github.com/tjst-t/sakutei/v2/internal/task"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

const fwAccImage = "https://images.example/bmc-2.0.0.bin"

type fwRig struct {
	t      *testing.T
	root   string
	bmcs   map[string]*redfishtest.Server
	p      *system.FirmwareProvider
	store  string
	cfg    reconcile.Config
	d      *reconcile.Daemon
	errOut *syncBuffer
}

func newFWRig(t *testing.T, nodes []string, opts redfishtest.Options, fields string, cfg reconcile.Config) *fwRig {
	t.Helper()
	root := t.TempDir()
	r := &fwRig{t: t, root: root, bmcs: map[string]*redfishtest.Server{}, store: filepath.Join(root, "tasks.json")}
	clients := map[string]redfish.Client{}
	var resources []string
	for _, n := range nodes {
		srv := redfishtest.New(opts)
		t.Cleanup(srv.Close)
		srv.AddImage(redfishtest.Image{URI: fwAccImage, Version: "2.0.0"})
		c, err := redfish.New(redfish.Config{Endpoint: srv.URL, Username: "admin", Password: "password",
			InsecureSkipVerify: true, Timeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		r.bmcs[n], clients[n] = srv, c
		resources = append(resources, fmt.Sprintf(`{"provider": %q, "id": %q, "update_policy": {"strategy": "task"},
			"fields": {"name": %q, "version": "2.0.0", "image_uri": %q %s}}`,
			system.FirmwareName, system.FirmwareID(n, "BMC"), n, fwAccImage, fields))
	}
	r.p = system.NewFirmware(clients, system.FirmwareOptions{PollInterval: time.Millisecond})
	intentDir := filepath.Join(root, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(intentDir, "intent.json"), []byte(`{"resources": [`+strings.Join(resources, ",")+`]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.IntentDir = intentDir
	cfg.GeneratedDir = filepath.Join(root, "generated")
	cfg.Providers = func(name string) (provider.Provider, error) {
		if name == system.FirmwareName {
			return r.p, nil
		}
		return nil, fmt.Errorf("unknown provider %q", name)
	}
	cfg.TaskPollInterval = time.Millisecond
	r.cfg = cfg
	r.d = r.newDaemon()
	return r
}

// newDaemon は同じ intent・BMC・task store で新しいデーモンを作る（起こし直し）。
func (r *fwRig) newDaemon() *reconcile.Daemon {
	cfg := r.cfg
	r.errOut = &syncBuffer{}
	cfg.Out, cfg.ErrOut = r.errOut, r.errOut
	cfg.TaskStore = task.NewFileStore(r.store)
	return reconcile.NewDaemon(cfg)
}

func (r *fwRig) cycle() reconcile.CycleResult {
	r.t.Helper()
	res, err := r.d.RunCycle(context.Background())
	if err != nil {
		r.t.Fatalf("RunCycle: %v\n%s", err, r.errOut)
	}
	r.d.Wait(5 * time.Second)
	return res
}

func fwKey(node string) string {
	return core.NewID(system.FirmwareName, core.NewScope(nil), system.FirmwareID(node, "BMC")).String()
}

func (r *fwRig) record(node string) (task.Record, bool) {
	r.t.Helper()
	rec, ok, err := task.NewFileStore(r.store).Get(context.Background(), fwKey(node))
	if err != nil {
		r.t.Fatal(err)
	}
	return rec, ok
}

func (r *fwRig) waitRecord(node, what string, cond func(task.Record, bool) bool) task.Record {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec, ok := r.record(node)
		if cond(rec, ok) {
			return rec
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("task record of %s = %+v (present=%v), want %s\n%s", node, rec, ok, what, r.errOut)
		}
		time.Sleep(time.Millisecond)
	}
}

func (r *fwRig) decide(node string, reject bool) {
	r.t.Helper()
	log := audit.NewLog(filepath.Join(r.root, "audit.log"))
	if _, err := task.Decide(context.Background(), task.NewFileStore(r.store), log, fwKey(node),
		task.Approval{Approver: "alice", Reject: reject}, time.Now()); err != nil {
		r.t.Fatal(err)
	}
}

// [AC-148-7]
func TestRedfishFirmware_UpdateFlowsAsPhasedTask(t *testing.T) {
	r := newFWRig(t, []string{"node1"}, redfishtest.Options{}, `, "apply_time": "OnReset", "reboot": "graceful"`, reconcile.Config{})
	bmc := r.bmcs["node1"]

	res := r.cycle()
	if !contains(res.TasksLaunched, "node1/firmware/BMC") {
		t.Fatalf("[AC-148-7] cycle 1: no phased Task launched: %+v\n%s", res, r.errOut)
	}
	rec := r.waitRecord("node1", "the approval gate", func(rec task.Record, ok bool) bool {
		return ok && rec.Phase == task.PhaseAwaitingApproval
	})
	if rec.Gate != task.GateApproval || rec.Step != task.StepVerifying || bmc.UpdateRequests() != 0 {
		t.Fatalf("[AC-148-7] before approval: %+v, update requests %d", rec, bmc.UpdateRequests())
	}
	res = r.cycle()
	if !contains(res.TasksAwaitingApproval, "node1/firmware/BMC") || !strings.Contains(r.errOut.String(), "needs approval before step verifying") {
		t.Fatalf("[AC-148-7] cycle 2 must report the approval gate: %+v\n%s", res, r.errOut)
	}

	r.decide("node1", false)
	if res := r.cycle(); !contains(res.TasksResumed, "node1/firmware/BMC") {
		t.Fatalf("[AC-148-7] after approval: not resumed: %+v\n%s", res, r.errOut)
	}
	r.waitRecord("node1", "succeeded", func(rec task.Record, ok bool) bool { return ok && rec.Phase == task.PhaseSucceeded })
	if bmc.FirmwareVersion("BMC") != "2.0.0" || bmc.UpdateRequests() != 1 || bmc.Boots() != 1 {
		t.Fatalf("[AC-148-7] version %s, update requests %d, boots %d; want 2.0.0, 1, 1",
			bmc.FirmwareVersion("BMC"), bmc.UpdateRequests(), bmc.Boots())
	}
	if res := r.cycle(); !contains(res.TasksCompleted, "node1/firmware/BMC") {
		t.Fatalf("[AC-148-7] the version is observed but the Task is not settled: %+v\n%s", res, r.errOut)
	}
	if _, ok := r.record("node1"); ok {
		t.Fatalf("[AC-148-7] S2 record must be gone once Done")
	}
	for c := 0; c < 3; c++ {
		if res := r.cycle(); len(res.TasksLaunched) != 0 || len(res.TasksHeld) != 0 {
			t.Fatalf("[AC-148-7] converged firmware got a Task decision: %+v", res)
		}
	}
	if bmc.UpdateRequests() != 1 || bmc.Boots() != 1 {
		t.Fatalf("[AC-148-7] after convergence: update requests %d, boots %d", bmc.UpdateRequests(), bmc.Boots())
	}
}

// [AC-148-8]
func TestRedfishFirmware_NoApprovalNoUpdate(t *testing.T) {
	r := newFWRig(t, []string{"node1"}, redfishtest.Options{}, "", reconcile.Config{})
	bmc := r.bmcs["node1"]
	for c := 0; c < 5; c++ {
		r.cycle()
	}
	if rec, ok := r.record("node1"); !ok || rec.Phase != task.PhaseAwaitingApproval || bmc.UpdateRequests() != 0 {
		t.Fatalf("[AC-148-8] without approval: record %+v, update requests %d", rec, bmc.UpdateRequests())
	}
	r.decide("node1", true)
	for c := 0; c < 3; c++ {
		r.cycle()
	}
	rec, _ := r.record("node1")
	if rec.Phase != task.PhaseFailed || bmc.UpdateRequests() != 0 || bmc.FirmwareVersion("BMC") != "1.0.0" {
		t.Fatalf("[AC-148-8] rejected: record %+v, update requests %d", rec, bmc.UpdateRequests())
	}
}

// [AC-148-9]
func TestRedfishFirmware_VersionMatchesDoesNothing(t *testing.T) {
	r := newFWRig(t, []string{"node1"}, redfishtest.Options{}, "", reconcile.Config{})
	bmc := r.bmcs["node1"]
	bmc.SetFirmwareVersion("BMC", "2.0.0")
	bmc.ClearRequests()
	for c := 0; c < 3; c++ {
		if res := r.cycle(); len(res.TasksLaunched) != 0 || len(res.TasksHeld) != 0 {
			t.Fatalf("[AC-148-9] matching version got a Task decision: %+v\n%s", res, r.errOut)
		}
	}
	if _, ok := r.record("node1"); ok || bmc.UpdateRequests() != 0 {
		t.Fatalf("[AC-148-9] a Task record or an update request exists")
	}
	for _, q := range bmc.Requests() {
		if q.Method != "GET" {
			t.Fatalf("[AC-148-9] the BMC was written: %+v", q)
		}
	}
}

// [AC-148-10]
func TestRedfishFirmware_CrashDoesNotUpdateTwice(t *testing.T) {
	t.Run("after staging", func(t *testing.T) {
		clk := &rfClock{t: time.Now()}
		r := newFWRig(t, []string{"node1"}, redfishtest.Options{Now: clk.Now, UpdateOptions: redfishtest.UpdateOptions{UpdateDuration: time.Hour}},
			"", reconcile.Config{})
		bmc := r.bmcs["node1"]
		// 前のデーモンが SimpleUpdate を送ってから、次の相（applying）を書く前に落ちた。
		r.stageDirectly(t, "node1")
		seedFW(t, r, "node1", task.StepStaging, false)
		res, err := r.d.RunCycle(context.Background())
		if err != nil || !contains(res.TasksResumed, "node1/firmware/BMC") {
			t.Fatalf("[AC-148-10] not resumed: %+v %v\n%s", res, err, r.errOut)
		}
		r.waitRecord("node1", "applying", func(rec task.Record, ok bool) bool { return ok && rec.Step == task.StepApplying })
		if bmc.UpdateRequests() != 1 {
			t.Fatalf("[AC-148-10] update sent %d times, want 1 (the staging entry observes the BMC task)", bmc.UpdateRequests())
		}
		clk.Advance(time.Hour)
		r.d.Wait(5 * time.Second)
		r.waitRecord("node1", "succeeded", func(rec task.Record, ok bool) bool { return ok && rec.Phase == task.PhaseSucceeded })
		if bmc.UpdateRequests() != 1 || bmc.FirmwareVersion("BMC") != "2.0.0" {
			t.Fatalf("[AC-148-10] update requests %d, version %s", bmc.UpdateRequests(), bmc.FirmwareVersion("BMC"))
		}
	})

	t.Run("inside applying", func(t *testing.T) {
		clk := &rfClock{t: time.Now()}
		r := newFWRig(t, []string{"node1"}, redfishtest.Options{Now: clk.Now, UpdateOptions: redfishtest.UpdateOptions{UpdateDuration: time.Hour}},
			"", reconcile.Config{})
		bmc := r.bmcs["node1"]
		r.stageDirectly(t, "node1")
		// BMC が書いている途中で、前のデーモンが落ちた（危険区間の印が立ったまま）。
		seedFW(t, r, "node1", task.StepApplying, true)
		for c := 0; c < 3; c++ {
			res := r.cycle()
			if !contains(res.TasksAwaitingApproval, "node1/firmware/BMC") || len(res.TasksResumed) != 0 {
				t.Fatalf("[AC-148-10] crashed inside applying: want the gate: %+v\n%s", res, r.errOut)
			}
		}
		// BMC の Task が終わって版が合った: 承認なしで閉じる（完了印＝版が見えたので
		// Settle で Done）。
		clk.Advance(time.Hour)
		if res := r.cycle(); !contains(res.TasksCompleted, "node1/firmware/BMC") {
			t.Fatalf("[AC-148-10] not settled: %+v\n%s", res, r.errOut)
		}
		if _, ok := r.record("node1"); ok {
			t.Fatalf("[AC-148-10] S2 record must be gone once Done")
		}
		if bmc.UpdateRequests() != 1 || bmc.FirmwareVersion("BMC") != "2.0.0" {
			t.Fatalf("[AC-148-10] update requests %d, version %s", bmc.UpdateRequests(), bmc.FirmwareVersion("BMC"))
		}
	})
}

// stageDirectly は前のデーモンの staging の相の効果（SimpleUpdate を送った）を起こす。
func (r *fwRig) stageDirectly(t *testing.T, node string) {
	t.Helper()
	if err := r.p.RunStep(context.Background(), task.Request{
		Resource: core.NewID(system.FirmwareName, core.NewScope(nil), system.FirmwareID(node, "BMC")),
		Desired:  map[string]string{"name": node, "version": "2.0.0", "image_uri": fwAccImage},
	}, task.StepStaging); err != nil {
		t.Fatal(err)
	}
}

// seedFW は前のデーモン（old-host）が step で落ちた記録を置く（lease は切れている）。
func seedFW(t *testing.T, r *fwRig, node, step string, critical bool) {
	t.Helper()
	desired := map[string]string{"name": node, "version": "2.0.0", "image_uri": fwAccImage}
	now := time.Now()
	if _, started, err := task.NewFileStore(r.store).Begin(context.Background(), task.Record{
		Resource: fwKey(node), TaskID: "old-1", Phase: task.PhaseRunning, Holder: "old-host",
		DesiredHash: task.DesiredHash(desired), StartedAt: now.Add(-20 * time.Minute),
		Step: step, StepStartedAt: now.Add(-time.Minute), HeartbeatAt: now.Add(-10 * time.Minute),
		Critical: critical, ApprovedBy: "alice",
	}); err != nil || !started {
		t.Fatalf("seed: started=%v err=%v", started, err)
	}
}

// [AC-148-11]
func TestRedfishFirmware_CountsAsDestructive(t *testing.T) {
	r := newFWRig(t, []string{"node1", "node2"}, redfishtest.Options{}, "",
		reconcile.Config{BudgetConfig: budget.Config{MaxDestructivePerCycle: 1}})
	res := r.cycle()
	if !res.BudgetHalted || !strings.Contains(res.BudgetReason, "max_destructive_per_cycle=1") || len(res.TasksLaunched) != 0 {
		t.Fatalf("[AC-148-11] two firmware updates with max-destructive-per-cycle 1: halted=%v reason=%q launched=%v",
			res.BudgetHalted, res.BudgetReason, res.TasksLaunched)
	}
	for n, bmc := range r.bmcs {
		if _, ok := r.record(n); ok || bmc.UpdateRequests() != 0 {
			t.Fatalf("[AC-148-11] %s: a Task was launched over the budget", n)
		}
	}
}

// [AC-148-13]
func TestRedfishFirmware_LeftoverBMCTaskDoesNotSkipApproval(t *testing.T) {
	clk := &rfClock{t: time.Now().Add(-time.Hour)}
	r := newFWRig(t, []string{"node1"}, redfishtest.Options{Now: clk.Now}, `, "reboot": "graceful"`, reconcile.Config{})
	bmc := r.bmcs["node1"]
	// 1 時間前に 2.0.0 へ上げて済んだ（Completed の Task が残る）。そのあと人が 1.0.0 に戻した。
	r.stageDirectly(t, "node1")
	if bmc.FirmwareVersion("BMC") != "2.0.0" {
		t.Fatalf("[AC-148-13] the earlier update did not complete")
	}
	bmc.SetFirmwareVersion("BMC", "1.0.0")
	clk.Advance(time.Hour)
	boots := bmc.Boots()

	for c := 0; c < 5; c++ {
		r.cycle()
	}
	rec, ok := r.record("node1")
	if !ok || rec.Phase != task.PhaseAwaitingApproval || rec.Step != task.StepVerifying {
		t.Fatalf("[AC-148-13] want the approval gate at verifying: %+v (present=%v)\n%s", rec, ok, r.errOut)
	}
	if bmc.UpdateRequests() != 1 || bmc.Boots() != boots || bmc.FirmwareVersion("BMC") != "1.0.0" {
		t.Fatalf("[AC-148-13] without approval: update requests %d (want 1, the earlier one), boots %d (want %d), version %s",
			bmc.UpdateRequests(), bmc.Boots(), boots, bmc.FirmwareVersion("BMC"))
	}

	r.decide("node1", false)
	r.cycle()
	r.waitRecord("node1", "succeeded", func(rec task.Record, ok bool) bool { return ok && rec.Phase == task.PhaseSucceeded })
	if bmc.UpdateRequests() != 2 || bmc.FirmwareVersion("BMC") != "2.0.0" {
		t.Fatalf("[AC-148-13] after approval: update requests %d (want 2), version %s", bmc.UpdateRequests(), bmc.FirmwareVersion("BMC"))
	}
	if res := r.cycle(); !contains(res.TasksCompleted, "node1/firmware/BMC") {
		t.Fatalf("[AC-148-13] not settled: %+v\n%s", res, r.errOut)
	}
}

// [AC-148-14]
func TestRedfishFirmware_CrashInRebootingDoesNotResetTwice(t *testing.T) {
	const fields = `, "apply_time": "OnReset", "reboot": "force"`
	desired := map[string]string{"name": "node1", "version": "2.0.0", "image_uri": fwAccImage, "apply_time": "OnReset", "reboot": "force"}
	resets := func(bmc *redfishtest.Server) int {
		n := 0
		for _, q := range bmc.Requests() {
			if q.Method == "POST" && q.Path == redfishtest.ResetPath {
				n++
			}
		}
		return n
	}
	// setup は、前のデーモンが OnReset の更新を BMC に渡し（保留中）、rebooting の相に
	// 入ったところで落ちた記録を置く。
	setup := func(t *testing.T, mark func(*task.Record)) (*fwRig, *redfishtest.Server) {
		r := newFWRig(t, []string{"node1"}, redfishtest.Options{}, fields, reconcile.Config{})
		bmc := r.bmcs["node1"]
		id := core.NewID(system.FirmwareName, core.NewScope(nil), system.FirmwareID("node1", "BMC"))
		if err := r.p.RunStep(context.Background(), task.Request{Resource: id, Desired: desired}, task.StepStaging); err != nil {
			t.Fatal(err)
		}
		if bmc.FirmwarePending("BMC") != "2.0.0" || bmc.FirmwareVersion("BMC") != "1.0.0" {
			t.Fatalf("[AC-148-14] setup: pending %q, version %q", bmc.FirmwarePending("BMC"), bmc.FirmwareVersion("BMC"))
		}
		now := time.Now()
		rec := task.Record{
			Resource: fwKey("node1"), TaskID: "old-1", Phase: task.PhaseRunning, Holder: "old-host",
			DesiredHash: task.DesiredHash(desired), StartedAt: now.Add(-20 * time.Minute),
			Step: task.StepRebooting, StepStartedAt: now.Add(-time.Minute), HeartbeatAt: now.Add(-10 * time.Minute),
			ApprovedBy: "alice",
		}
		mark(&rec)
		if _, started, err := task.NewFileStore(r.store).Begin(context.Background(), rec); err != nil || !started {
			t.Fatalf("seed: started=%v err=%v", started, err)
		}
		bmc.ClearRequests()
		return r, bmc
	}

	t.Run("while waiting after the reset", func(t *testing.T) {
		r, bmc := setup(t, func(rec *task.Record) { rec.Awaiting = true })
		res, err := r.d.RunCycle(context.Background())
		if err != nil || !contains(res.TasksResumed, "node1/firmware/BMC") {
			t.Fatalf("[AC-148-14] not resumed: %+v %v\n%s", res, err, r.errOut)
		}
		time.Sleep(50 * time.Millisecond) // 引き継いだデーモンが待ちに入る
		if n := resets(bmc); n != 0 || bmc.Boots() != 0 {
			t.Fatalf("[AC-148-14] the resumed Task sent Reset %d time(s) (boots %d); the reset was already sent", n, bmc.Boots())
		}
		if rec, _ := r.record("node1"); rec.Step != task.StepRebooting || !rec.Awaiting || rec.Critical {
			t.Fatalf("[AC-148-14] record while waiting = %+v", rec)
		}
		bmc.Restart() // 前のデーモンが送った再起動が効いた
		r.d.Wait(5 * time.Second)
		r.waitRecord("node1", "succeeded", func(rec task.Record, ok bool) bool { return ok && rec.Phase == task.PhaseSucceeded })
		if res := r.cycle(); !contains(res.TasksCompleted, "node1/firmware/BMC") {
			t.Fatalf("[AC-148-14] not settled: %+v\n%s", res, r.errOut)
		}
		if n := resets(bmc); n != 0 || bmc.Boots() != 1 || bmc.FirmwareVersion("BMC") != "2.0.0" {
			t.Fatalf("[AC-148-14] resets %d, boots %d, version %s; want 0, 1, 2.0.0", n, bmc.Boots(), bmc.FirmwareVersion("BMC"))
		}
	})

	t.Run("inside the reset", func(t *testing.T) {
		r, bmc := setup(t, func(rec *task.Record) { rec.Critical = true })
		for c := 0; c < 3; c++ {
			res := r.cycle()
			if !contains(res.TasksAwaitingApproval, "node1/firmware/BMC") || len(res.TasksResumed) != 0 {
				t.Fatalf("[AC-148-14] crashed inside the reset: want the gate: %+v\n%s", res, r.errOut)
			}
		}
		if n := resets(bmc); n != 0 || bmc.Boots() != 0 {
			t.Fatalf("[AC-148-14] the gated Task sent Reset %d time(s)", n)
		}
		bmc.Restart() // 落ちる前に送った再起動が効いた
		if res := r.cycle(); !contains(res.TasksCompleted, "node1/firmware/BMC") {
			t.Fatalf("[AC-148-14] version observed but not settled: %+v\n%s", res, r.errOut)
		}
		if n := resets(bmc); n != 0 || bmc.Boots() != 1 {
			t.Fatalf("[AC-148-14] resets %d, boots %d; want 0, 1", n, bmc.Boots())
		}
	})
}
