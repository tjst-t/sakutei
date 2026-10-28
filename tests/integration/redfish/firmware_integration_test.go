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

//go:build integration_redfish

// firmware_integration_test.go — #168: redfish-firmware（FW の版の観測と多相
// Task での更新、#148）を qemu-bmc 0.11.0 の UpdateService の模擬に向ける
// （10〜13 段目。[AC-168-10]〜[AC-168-13] は redfish_integration_test.go の
// 冒頭）。イメージ（1 行目が "QBMC-FW <BMC|BIOS> <version>"）はテストが立てる
// HTTP で配り、BMC は管理ネットのゲートウェイのアドレスで取りに来る。Task が
// 止まったことは、テストが立てる webhook（--event-webhook）で受ける。
package redfish

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/task"
)

// ---- イメージを配る HTTP と、通知を受ける webhook ----

// fwImages は FW のイメージを配る HTTP（BMC が管理ネットから取りに来る）。
type fwImages struct {
	base string // BMC から見た URL（http://<管理ネットのゲートウェイ>:<port>）
	srv  *http.Server
	mu   sync.Mutex
	gets map[string]int
}

// newImages は name → 1 行目の版（"BMC 2.0.0" など）のイメージを配る。
func newImages(t *testing.T, images map[string]string) *fwImages {
	t.Helper()
	out, err := sudoCmd("docker", "network", "inspect", labName, "-f", "{{range .IPAM.Config}}{{.Gateway}}{{end}}").Output()
	gw := strings.TrimSpace(string(out))
	if err != nil || net.ParseIP(gw) == nil {
		gw = "172.31.84.1"
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(gw, "0"))
	if err != nil {
		t.Fatalf("listen for the firmware images on %s: %v", gw, err)
	}
	im := &fwImages{base: "http://" + ln.Addr().String(), gets: map[string]int{}}
	im.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		head, ok := images[name]
		im.mu.Lock()
		im.gets[name]++
		im.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, "QBMC-FW %s\nsakutei integration test image %s\n", head, name)
	})}
	go func() { _ = im.srv.Serve(ln) }()
	t.Cleanup(func() { _ = im.srv.Close() })
	return im
}

func (im *fwImages) url(name string) string { return im.base + "/" + name }

// count は name を取りに来た回数。
func (im *fwImages) count(name string) int {
	im.mu.Lock()
	defer im.mu.Unlock()
	return im.gets[name]
}

// webhook は --event-webhook の受け口。
type webhook struct {
	srv *httptest.Server
	mu  sync.Mutex
	evs []event
}

func newWebhook(t *testing.T) *webhook {
	t.Helper()
	h := &webhook{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var ev event
		if json.Unmarshal(b, &ev) == nil {
			h.mu.Lock()
			h.evs = append(h.evs, ev)
			h.mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *webhook) find(match func(event) bool) (event, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ev := range h.evs {
		if match(ev) {
			return ev, true
		}
	}
	return event{}, false
}

// haltNotice は Task が止まったことの知らせか: RequiresTask の firing で、
// warning（続いた周数が --stuck-after に達していれば critical）。
func haltNotice(ev event) bool {
	return ev.State == "firing" && (ev.Severity == "warning" || ev.Severity == "critical")
}

func (h *webhook) list() []event {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]event(nil), h.evs...)
}

// ---- Redfish の FW の口を手で読む ----

type inventoryDoc struct {
	Version string
	Status  struct{ State string }
}

func getInventory(t *testing.T, n node, comp string) inventoryDoc {
	t.Helper()
	var d inventoryDoc
	if err := getJSON(n, "/redfish/v1/UpdateService/FirmwareInventory/"+comp, &d); err != nil {
		t.Fatalf("%s: %v", n.name, err)
	}
	return d
}

type bmcTaskDoc struct {
	ID        string `json:"Id"`
	TaskState string
	Messages  []struct {
		MessageID   string `json:"MessageId"`
		Message     string
		MessageArgs []string
	}
}

// hasMessage は MessageId が id のメッセージを持つか（args を書けば、その
// MessageArgs も先頭から同じ）。
func (d bmcTaskDoc) hasMessage(id string, args ...string) bool {
	for _, m := range d.Messages {
		if m.MessageID != id || len(m.MessageArgs) < len(args) {
			continue
		}
		ok := true
		for i, a := range args {
			ok = ok && m.MessageArgs[i] == a
		}
		if ok {
			return true
		}
	}
	return false
}

func bmcTasks(t *testing.T, n node) []bmcTaskDoc {
	t.Helper()
	var coll struct {
		Members []struct {
			ID string `json:"@odata.id"`
		}
	}
	if err := getJSON(n, "/redfish/v1/TaskService/Tasks", &coll); err != nil {
		t.Fatalf("%s: %v", n.name, err)
	}
	out := make([]bmcTaskDoc, 0, len(coll.Members))
	for _, m := range coll.Members {
		var d bmcTaskDoc
		if err := getJSON(n, m.ID, &d); err != nil {
			t.Fatalf("%s: %v", n.name, err)
		}
		out = append(out, d)
	}
	return out
}

// taskWatch は BMC の Task の TaskState の移り変わりを 200ms ごとに記録する。
type taskWatch struct {
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	seen   map[string][]string // Task の Id → 重複を除いた TaskState
}

func watchTasks(t *testing.T, n node) *taskWatch {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := &taskWatch{cancel: cancel, done: make(chan struct{}), seen: map[string][]string{}}
	go func() {
		defer close(w.done)
		for ctx.Err() == nil {
			var coll struct {
				Members []struct {
					ID string `json:"@odata.id"`
				}
			}
			if getJSON(n, "/redfish/v1/TaskService/Tasks", &coll) == nil {
				for _, m := range coll.Members {
					var d bmcTaskDoc
					if getJSON(n, m.ID, &d) != nil {
						continue
					}
					w.mu.Lock()
					if s := w.seen[d.ID]; len(s) == 0 || s[len(s)-1] != d.TaskState {
						w.seen[d.ID] = append(s, d.TaskState)
					}
					w.mu.Unlock()
				}
			}
			select {
			case <-ctx.Done():
			case <-time.After(200 * time.Millisecond):
			}
		}
	}()
	t.Cleanup(w.stop)
	return w
}

func (w *taskWatch) states(id string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.seen[id]...)
}

func (w *taskWatch) stop() { w.cancel(); <-w.done }

// ---- sakutei の側 ----

func fwRes(n node, comp, version string, fields map[string]string) resource {
	f := map[string]string{"name": n.name, "version": version}
	for k, v := range fields {
		f[k] = v
	}
	return resource{Provider: "redfish-firmware", ID: n.name + "/firmware/" + comp,
		UpdatePolicy: map[string]string{"strategy": "task"}, Fields: f}
}

func fwID(n node, comp string) string { return "redfish-firmware//" + n.name + "/firmware/" + comp }

// fwTaskStore は 11〜13 段目の常駐の sakutei の --task-store。
func fwTaskStore() string { return filepath.Join(lab.dir, "fw-tasks.json") }

func startFWDaemon(t *testing.T, name string, hook *webhook) *daemon {
	t.Helper()
	return startDaemon(t, name, "--task-store", fwTaskStore(), "--event-webhook", hook.srv.URL)
}

func fwRecord(t *testing.T, key string) (task.Record, bool) {
	t.Helper()
	rec, ok, err := task.NewFileStore(fwTaskStore()).Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return rec, ok
}

func waitFWRecord(t *testing.T, timeout time.Duration, key, what string, cond func(task.Record, bool) bool) task.Record {
	t.Helper()
	var rec task.Record
	waitFor(t, timeout, key+" "+what, func() (bool, string) {
		var ok bool
		rec, ok = fwRecord(t, key)
		return cond(rec, ok), fmt.Sprintf("present=%v %+v", ok, rec)
	})
	return rec
}

func awaitingApproval(rec task.Record, ok bool) bool {
	return ok && rec.Phase == task.PhaseAwaitingApproval && rec.Step == task.StepVerifying
}

// closed は Task が成功で閉じた（記録が消えたか、succeeded）。
func closed(rec task.Record, ok bool) bool { return !ok || rec.Phase == task.PhaseSucceeded }

func failed(rec task.Record, ok bool) bool { return ok && rec.Phase == task.PhaseFailed }

func sakuteiTask(t *testing.T, verb string, args ...string) string {
	t.Helper()
	all := []string{"task", verb, "--store", fwTaskStore()}
	if verb == "approve" || verb == "reject" {
		all = append(all, "--audit-log", filepath.Join(lab.dir, "audit-168.jsonl"), "--approver", "integration")
	}
	out, err := exec.Command(lab.bin, append(all, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("sakutei task %s %v: %v\n%s", verb, args, err, out)
	}
	return string(out)
}

// cycles は常駐の sakutei が今から k 周回るのを待つ。
func cycles(t *testing.T, d *daemon, k int) {
	t.Helper()
	n0 := cycleCount(d)
	waitFor(t, time.Duration(k)*30*time.Second, fmt.Sprintf("%d more cycles", k), func() (bool, string) {
		n := cycleCount(d)
		return n >= n0+k, fmt.Sprint(n - n0)
	})
}

// cycleCount は常駐の sakutei の出力に出た最後の周の番号。
func cycleCount(d *daemon) int {
	out := d.out.String()
	last := 0
	for {
		i := strings.LastIndex(out, "sakutei: cycle ")
		if i < 0 {
			return last
		}
		var n int
		if _, err := fmt.Sscanf(out[i+len("sakutei: cycle "):], "%d:", &n); err == nil {
			return n
		}
		out = out[:i]
	}
}

// ---- 段 ----

// [AC-168-10]
func stepFirmwareObserve(t *testing.T) {
	im := newImages(t, nil)
	tasks1, tasks2 := len(bmcTasks(t, node1)), len(bmcTasks(t, node2))
	for comp, want := range map[string]string{"BMC": node1BMCVer, "BIOS": node1BIOSVer} {
		if got := getInventory(t, node1, comp); got.Version != want || got.Status.State != "Enabled" {
			t.Fatalf("node1 %s firmware = %+v, want %s (the topology's %s_FIRMWARE_VERSION)", comp, got, want, comp)
		}
	}
	img := map[string]string{"image_uri": im.url("bmc.bin")}
	declared := []resource{
		systemRes(node1, map[string]string{"power": "On"}),
		fwRes(node1, "BMC", node1BMCVer, img),
		fwRes(node1, "BIOS", node1BIOSVer, img),
		fwRes(node2, "BMC", qemuBMCVer, img),
		fwRes(node2, "BIOS", node2BIOSVer, img),
	}
	writeIntent(t, declared...)
	if out, code := sakutei(t, "plan"); code != 0 || !strings.Contains(out, "Converged (no changes)") {
		t.Fatalf("[AC-168-10] plan with the observed versions (exit %d) is not converged:\n%s", code, out)
	}

	declared[1] = fwRes(node1, "BMC", "2.0.0", img)
	declared[4] = fwRes(node2, "BIOS", "QBMC-BIOS-2.0.0", img)
	writeIntent(t, declared...)
	out, code := sakutei(t, "plan")
	t.Logf("plan:\n%s", out)
	for _, want := range []string{fwID(node1, "BMC"), `"1.0.0" → "2.0.0"`, fwID(node2, "BIOS"), `"QBMC-BIOS-1.0.0" → "QBMC-BIOS-2.0.0"`} {
		if !strings.Contains(out, want) {
			t.Errorf("[AC-168-10] plan does not mention %s:\n%s", want, out)
		}
	}
	if code != 0 || strings.Contains(out, "Converged (no changes)") || strings.Contains(out, fwID(node1, "BIOS")) {
		t.Fatalf("[AC-168-10] plan (exit %d) must show only the two version diffs:\n%s", code, out)
	}
	if v := getInventory(t, node1, "BMC").Version; v != node1BMCVer || len(bmcTasks(t, node1)) != tasks1 || len(bmcTasks(t, node2)) != tasks2 || im.count("bmc.bin") != 0 {
		t.Fatalf("[AC-168-10] plan sent an update: node1 BMC %s, BMC tasks %d/%d → %d/%d, image fetched %d time(s)",
			v, tasks1, tasks2, len(bmcTasks(t, node1)), len(bmcTasks(t, node2)), im.count("bmc.bin"))
	}
}

// [AC-168-11]
func stepFirmwareBMCUpdate(t *testing.T) {
	im := newImages(t, map[string]string{"bmc-2.0.0.bin": "BMC 2.0.0", "bmc-2.1.0.bin": "BMC 2.1.0"})
	hook := newWebhook(t)
	_ = os.Remove(fwTaskStore())
	key := fwID(node1, "BMC")
	before := map[string]bool{}
	for _, d := range bmcTasks(t, node1) {
		before[d.ID] = true
	}
	newTasks := func() []bmcTaskDoc {
		var out []bmcTaskDoc
		for _, d := range bmcTasks(t, node1) {
			if !before[d.ID] {
				out = append(out, d)
			}
		}
		return out
	}
	tw := watchTasks(t, node1)

	// SimpleUpdate（BMC が image_uri を取りに来る）。
	writeIntent(t, systemRes(node1, map[string]string{"power": "On"}),
		fwRes(node1, "BMC", "2.0.0", map[string]string{"image_uri": im.url("bmc-2.0.0.bin")}))
	d := startFWDaemon(t, "fw-bmc", hook)
	waitFWRecord(t, time.Minute, key, "awaiting approval", awaitingApproval)
	cycles(t, d, 3)
	if n, got := len(newTasks()), im.count("bmc-2.0.0.bin"); n != 0 || got != 0 {
		t.Fatalf("[AC-168-11] before the approval: %d BMC task(s), the image fetched %d time(s); want nothing sent", n, got)
	}
	if v := getInventory(t, node1, "BMC").Version; v != node1BMCVer {
		t.Fatalf("[AC-168-11] node1 BMC is %s before the approval", v)
	}
	if _, ok := hook.find(func(ev event) bool {
		return ev.Kind == "RequiresTask" && ev.Resource == key && strings.Contains(ev.Message, "awaiting approval")
	}); !ok {
		t.Errorf("[AC-168-11] the approval gate was not notified to the webhook: %+v", hook.list())
	}
	if out := sakuteiTask(t, "list"); !strings.Contains(out, "awaiting_approval:verifying") {
		t.Errorf("[AC-168-11] task list does not show the gate:\n%s", out)
	}

	sakuteiTask(t, "approve", key)
	waitFor(t, 2*time.Minute, "node1 BMC 2.0.0", func() (bool, string) {
		v := getInventory(t, node1, "BMC").Version
		return v == "2.0.0", v
	})
	waitFWRecord(t, time.Minute, key, "closed", closed)
	nt := newTasks()
	if len(nt) != 1 {
		t.Fatalf("[AC-168-11] %d new BMC task(s), want 1: %+v", len(nt), nt)
	}
	bt := nt[0]
	states := tw.states(bt.ID)
	t.Logf("BMC task %s states: %v", bt.ID, states)
	if bt.TaskState != "Completed" || !contains(states, "Running") || states[len(states)-1] != "Completed" {
		t.Fatalf("[AC-168-11] BMC task %s = %s (states %v), want Running → Completed", bt.ID, bt.TaskState, states)
	}
	comp := "/redfish/v1/UpdateService/FirmwareInventory/BMC"
	if !bt.hasMessage("Update.1.0.TargetDetermined", comp, im.url("bmc-2.0.0.bin")) || !bt.hasMessage("Update.1.0.UpdateSuccessful") {
		t.Fatalf("[AC-168-11] BMC task messages %+v", bt.Messages)
	}
	if got := im.count("bmc-2.0.0.bin"); got != 1 {
		t.Fatalf("[AC-168-11] the BMC fetched the image %d time(s), want 1", got)
	}
	var mgr struct{ FirmwareVersion string }
	if err := getJSON(node1, "/redfish/v1/Managers/1", &mgr); err != nil || mgr.FirmwareVersion != "2.0.0" {
		t.Fatalf("[AC-168-11] Managers/1 FirmwareVersion %q %v, want 2.0.0", mgr.FirmwareVersion, err)
	}

	// multipart（Sakutei が取りに行って UpdateService/upload に送る）。
	before[bt.ID] = true
	writeIntent(t, systemRes(node1, map[string]string{"power": "On"}),
		fwRes(node1, "BMC", "2.1.0", map[string]string{"image_uri": im.url("bmc-2.1.0.bin"), "transfer": "multipart"}))
	waitFWRecord(t, time.Minute, key, "awaiting approval (multipart)", awaitingApproval)
	cycles(t, d, 2)
	if n, got := len(newTasks()), im.count("bmc-2.1.0.bin"); n != 0 || got != 0 {
		t.Fatalf("[AC-168-11] multipart before the approval: %d BMC task(s), the image fetched %d time(s)", n, got)
	}
	sakuteiTask(t, "approve", key)
	waitFor(t, 2*time.Minute, "node1 BMC 2.1.0", func() (bool, string) {
		v := getInventory(t, node1, "BMC").Version
		return v == "2.1.0", v
	})
	waitFWRecord(t, time.Minute, key, "closed (multipart)", closed)
	nt = newTasks()
	// UpdateService/upload の Task は、イメージとして送ったファイルの名前を名指す
	// （SimpleUpdate なら URI）。
	if len(nt) != 1 || nt[0].TaskState != "Completed" || !nt[0].hasMessage("Update.1.0.TargetDetermined", comp, "bmc-2.1.0.bin") {
		t.Fatalf("[AC-168-11] multipart: new BMC tasks %+v, want 1 Completed naming bmc-2.1.0.bin", nt)
	}
	if got := im.count("bmc-2.1.0.bin"); got != 1 {
		t.Fatalf("[AC-168-11] multipart: the image was fetched %d time(s), want 1 (by Sakutei)", got)
	}
	d.stop()
	if out, code := sakutei(t, "plan"); code != 0 || !strings.Contains(out, "Converged (no changes)") {
		t.Fatalf("[AC-168-11] plan after the updates (exit %d) is not converged:\n%s", code, out)
	}
}

// [AC-168-12]
func stepFirmwareBIOSPending(t *testing.T) {
	im := newImages(t, map[string]string{"bios-2.0.0.bin": "BIOS BIOS-2.0.0"})
	hook := newWebhook(t)
	_ = os.Remove(fwTaskStore())
	key := fwID(node1, "BIOS")
	if s := mustSystem(t, node1); s.PowerState != "On" {
		t.Fatalf("node1 is %s at the start, want On", s.PowerState)
	}
	ntasks := len(bmcTasks(t, node1))
	bios := map[string]string{"image_uri": im.url("bios-2.0.0.bin"), "reboot_timeout": "20s"}
	writeIntent(t, systemRes(node1, map[string]string{"power": "On"}), fwRes(node1, "BIOS", "BIOS-2.0.0", bios))

	// reboot を宣言しない: 再起動を送らず、保留のまま reboot_timeout で止まる。
	w := watchPower(t, node1)
	d := startFWDaemon(t, "fw-bios", hook)
	waitFWRecord(t, time.Minute, key, "awaiting approval", awaitingApproval)
	sakuteiTask(t, "approve", key)
	waitFor(t, 2*time.Minute, "the BIOS update pending", func() (bool, string) {
		inv := getInventory(t, node1, "BIOS")
		return inv.Status.State == "StandbyOffline", fmt.Sprintf("%+v", inv)
	})
	ts := bmcTasks(t, node1)
	if len(ts) != ntasks+1 {
		t.Fatalf("[AC-168-12] %d new BMC task(s), want 1", len(ts)-ntasks)
	}
	bt := ts[len(ts)-1]
	waitFor(t, time.Minute, "the BIOS BMC task Completed", func() (bool, string) {
		for _, d := range bmcTasks(t, node1) {
			if d.ID == bt.ID {
				bt = d
			}
		}
		return bt.TaskState == "Completed", bt.TaskState
	})
	if !bt.hasMessage("Update.1.0.AwaitToActivate") || bt.Messages[len(bt.Messages)-1].MessageID != "Update.1.0.AwaitToActivate" {
		t.Fatalf("[AC-168-12] the BIOS BMC task does not end with AwaitToActivate: %+v", bt.Messages)
	}
	if inv := getInventory(t, node1, "BIOS"); inv.Version != node1BIOSVer {
		t.Fatalf("[AC-168-12] BIOS %s before a boot, want %s (pending)", inv.Version, node1BIOSVer)
	}
	rec := waitFWRecord(t, 2*time.Minute, key, "halted (no reboot declared)", failed)
	t.Logf("halted: step %s: %s", rec.Step, rec.Error)
	if rec.Step != task.StepRebooting || rec.Error == "" {
		t.Fatalf("[AC-168-12] the task halted at %s (%q), want rebooting with a reason", rec.Step, rec.Error)
	}
	waitFor(t, 30*time.Second, "the halt notified to the webhook", func() (bool, string) {
		_, ok := hook.find(func(ev event) bool {
			return ev.Kind == "RequiresTask" && ev.Resource == key && haltNotice(ev) && strings.Contains(ev.Message, "rebooting")
		})
		return ok, fmt.Sprintf("%+v", hook.list())
	})
	cycles(t, d, 2)
	d.stop()
	w.stop()
	if seen := w.states(); len(seen) != 1 || seen[0] != "On" {
		t.Fatalf("[AC-168-12] node1 power moved without a declared reboot: %v", seen)
	}
	if inv := getInventory(t, node1, "BIOS"); inv.Version != node1BIOSVer || inv.Status.State != "StandbyOffline" {
		t.Fatalf("[AC-168-12] BIOS after the halt = %+v, want %s still pending", inv, node1BIOSVer)
	}
	if n := len(bmcTasks(t, node1)); n != ntasks+1 || im.count("bios-2.0.0.bin") != 1 {
		t.Fatalf("[AC-168-12] after the halt: %d new BMC task(s), image fetched %d time(s); want 1 and 1", n-ntasks, im.count("bios-2.0.0.bin"))
	}

	// 人が確かめて cancel し、reboot: force を宣言する。BMC の Task は済んでいるので
	// 送り直さず、ForceRestart だけを送って、起動したあとに版が変わる。
	sakuteiTask(t, "cancel", key)
	bios["reboot"] = "force"
	bios["reboot_timeout"] = "2m"
	writeIntent(t, systemRes(node1, map[string]string{"power": "On"}), fwRes(node1, "BIOS", "BIOS-2.0.0", bios))
	w = watchPower(t, node1)
	d = startFWDaemon(t, "fw-bios-reboot", hook)
	waitFWRecord(t, time.Minute, key, "awaiting approval (reboot: force)", awaitingApproval)
	if inv := getInventory(t, node1, "BIOS"); inv.Version != node1BIOSVer {
		t.Fatalf("[AC-168-12] BIOS changed before the approval: %+v", inv)
	}
	sakuteiTask(t, "approve", key)
	waitFor(t, 2*time.Minute, "BIOS-2.0.0 after the reboot", func() (bool, string) {
		inv := getInventory(t, node1, "BIOS")
		return inv.Version == "BIOS-2.0.0" && inv.Status.State == "Enabled", fmt.Sprintf("%+v", inv)
	})
	waitFWRecord(t, time.Minute, key, "closed", closed)
	d.stop()
	w.stop()
	t.Logf("node1 power states: %v", w.states())
	if n := len(bmcTasks(t, node1)); n != ntasks+1 || im.count("bios-2.0.0.bin") != 1 {
		t.Fatalf("[AC-168-12] the update was sent again: %d new BMC task(s), image fetched %d time(s)", n-ntasks, im.count("bios-2.0.0.bin"))
	}
	var sys struct{ BiosVersion, PowerState string }
	if err := getJSON(node1, "/redfish/v1/Systems/1", &sys); err != nil || sys.BiosVersion != "BIOS-2.0.0" || sys.PowerState != "On" {
		t.Fatalf("[AC-168-12] Systems/1 = %+v %v, want BiosVersion BIOS-2.0.0 and On", sys, err)
	}
	if out, code := sakutei(t, "plan"); code != 0 || !strings.Contains(out, "Converged (no changes)") {
		t.Fatalf("[AC-168-12] plan after the reboot (exit %d) is not converged:\n%s", code, out)
	}
}

// [AC-168-13]
func stepFirmwareFaults(t *testing.T) {
	im := newImages(t, map[string]string{"n2-bios-9.0.0.bin": "BIOS QBMC-BIOS-9.0.0", "n2-bmc-9.0.0.bin": "BMC v9.0.0"})
	hook := newWebhook(t)
	_ = os.Remove(fwTaskStore())
	faultDo(t, node2, "POST", map[string]any{"kind": "firmware-image-rejected", "target": "BIOS"})
	faultDo(t, node2, "POST", map[string]any{"kind": "firmware-update-fail", "target": "BMC", "atPercent": 30})
	defer faultDo(t, node2, "DELETE", nil)
	ntasks := len(bmcTasks(t, node2))
	biosKey, bmcKey := fwID(node2, "BIOS"), fwID(node2, "BMC")
	writeIntent(t, systemRes(node2, map[string]string{"power": "On"}),
		fwRes(node2, "BIOS", "QBMC-BIOS-9.0.0", map[string]string{"image_uri": im.url("n2-bios-9.0.0.bin")}),
		fwRes(node2, "BMC", "v9.0.0", map[string]string{"image_uri": im.url("n2-bmc-9.0.0.bin")}))

	d := startFWDaemon(t, "fw-faults", hook)
	for _, key := range []string{biosKey, bmcKey} {
		waitFWRecord(t, time.Minute, key, "awaiting approval", awaitingApproval)
		sakuteiTask(t, "approve", key)
	}
	bios := waitFWRecord(t, 2*time.Minute, biosKey, "halted (image rejected)", failed)
	bmc := waitFWRecord(t, 2*time.Minute, bmcKey, "halted (update failed)", failed)
	t.Logf("BIOS halted at %s: %s", bios.Step, bios.Error)
	t.Logf("BMC halted at %s: %s", bmc.Step, bmc.Error)
	if bios.Step != task.StepStaging || !strings.Contains(bios.Error, "Verification of image") || !strings.Contains(bios.Error, "400") {
		t.Errorf("[AC-168-13] image rejected: halted at %s: %q, want staging with the BMC's VerificationFailed", bios.Step, bios.Error)
	}
	if bmc.Step != task.StepApplying || !strings.Contains(bmc.Error, "Exception") || !strings.Contains(bmc.Error, "Update.1.0.ApplyFailed") {
		t.Errorf("[AC-168-13] update failed: halted at %s: %q, want applying with Exception and Update.1.0.ApplyFailed", bmc.Step, bmc.Error)
	}
	for key, want := range map[string]string{biosKey: "Verification of image", bmcKey: "Update.1.0.ApplyFailed"} {
		waitFor(t, 30*time.Second, key+" notified to the webhook", func() (bool, string) {
			_, ok := hook.find(func(ev event) bool {
				return ev.Kind == "RequiresTask" && ev.Resource == key && haltNotice(ev) && strings.Contains(ev.Message, want)
			})
			return ok, fmt.Sprintf("%+v", hook.list())
		})
	}
	// 二度目を勝手に送らない。
	cycles(t, d, 3)
	d.stop()
	ts := bmcTasks(t, node2)[ntasks:]
	if len(ts) != 1 || ts[0].TaskState != "Exception" || !ts[0].hasMessage("Update.1.0.ApplyFailed") ||
		!ts[0].hasMessage("Update.1.0.TargetDetermined", "/redfish/v1/UpdateService/FirmwareInventory/BMC") {
		t.Fatalf("[AC-168-13] new BMC tasks %+v, want only the BMC update ending in Exception (no task for the rejected image)", ts)
	}
	if b, m := im.count("n2-bios-9.0.0.bin"), im.count("n2-bmc-9.0.0.bin"); b > 1 || m != 1 {
		t.Fatalf("[AC-168-13] the images were fetched %d (BIOS) and %d (BMC) time(s); an update was sent again", b, m)
	}
	for comp, want := range map[string]string{"BIOS": node2BIOSVer, "BMC": qemuBMCVer} {
		if inv := getInventory(t, node2, comp); inv.Version != want || inv.Status.State != "Enabled" {
			t.Fatalf("[AC-168-13] node2 %s = %+v, want %s unchanged", comp, inv, want)
		}
	}
	for _, key := range []string{biosKey, bmcKey} {
		if rec, ok := fwRecord(t, key); !failed(rec, ok) {
			t.Fatalf("[AC-168-13] %s = %+v, want still halted (a person checks the BMC and cancels)", key, rec)
		}
	}
}
