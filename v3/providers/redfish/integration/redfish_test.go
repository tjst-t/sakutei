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

//go:build integration_v3_redfish

package integration

// redfish_test.go — v2 の統合テスト（tests/integration/redfish）の段を、新しいコアの
// redfish provider と sakutei（v3）で回す。前の段が作った現実を次の段が使うので、
// 1 つ落ちたら残りは回さない。
//
//	 1 plan は書かず、reconcile で電源・起動先・仮想メディアに収束し、次の plan は変えない（E-A1・E-A3）
//	 2 手で変えた電源・起動先・仮想メディアを次の reconcile で戻す
//	 3 既定の停止（GracefulShutdown）に応えない台に ForceOff を送らず、効かない Apply で
//	   止まる（終了コード 4、E-J6）。強制停止は手順の壊す操作 force_off で（E-A1・N-A1）
//	 4 BMC を止める（docker pause）とそのノードは Unknown で何もしない。もう 1 台は収束する（E-K1）
//	 5 boot_enabled: Once と power: ForceRestart は読み込みで断る（E-A2）
//	 6 手順の boot_once と force_restart：CD から 1 回だけ起動し、BMC が上書きを Disabled に戻す（E-A2・N-A2）
//	 7 BIOS の属性：Settings に 1 度だけ PATCH し、保留中は Converging で重ねない。読み取り専用は
//	   送らない。手順の force_restart で効く（E-A4・N-A3）。
//	   ディスクの無い VM は ACPI に応えず GracefulRestart では再起動しないので、6・7 は force_restart
//	 8 FW の更新（BMC）：常駐が承認の前は何も送らず（イメージも取りに来ない）、承認の後に
//	   verify・stage（critical）を 1 回ずつ送り、BMC の Task の完了を待って版で閉じる（E-A5・N-A4）
//	 9 FW の更新（BIOS）：BMC の Task が終わっても版は再起動まで変わらず、手順の force_restart で効く
//	10 FW の故障：イメージを断られる（400）と手順は止まって送り直さない。BMC の Task が Exception で
//	   終わると版は変わらず、待ちの時間切れで止まる（不変条件 16）
//	11 書き込み用の資格：書き込み用のパスワードのファイルが無くても plan は観測できて差分を出し、
//	   reconcile は書かずに失敗する（E-G5・N-G3）

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRedfish(t *testing.T) {
	steps := []struct {
		name string
		f    func(*testing.T)
	}{
		{"00_lab", stepLab},
		{"01_plan_reconcile", stepConverge},
		{"02_drift", stepDrift},
		{"03_graceful_ineffective_force_off", stepGraceful},
		{"04_unreachable", stepUnreachable},
		{"05_reject", stepReject},
		{"06_boot_once", stepBootOnce},
		{"07_bios", stepBios},
		{"08_firmware_bmc", stepFirmwareBMC},
		{"09_firmware_bios", stepFirmwareBIOS},
		{"10_firmware_faults", stepFirmwareFaults},
		{"11_write_credentials", stepWriteCredentials},
	}
	for _, s := range steps {
		if !t.Run(s.name, s.f) {
			t.Fatalf("段 %s が落ちたので残りは回さない", s.name)
		}
	}
}

// 立てた lab が想定どおりか（版・固定した値）。
func stepLab(t *testing.T) {
	var root struct{ RedfishVersion string }
	if err := getJSON(node1, "/redfish/v1", &root); err != nil {
		t.Fatal(err)
	}
	for comp, want := range map[string]string{"BMC": node1BMCVer, "BIOS": node1BIOSVer} {
		if v := getInventory(t, node1, comp).Version; v != want {
			t.Fatalf("node1 の %s が %s（want %s）", comp, v, want)
		}
	}
	if v := getInventory(t, node2, "BMC").Version; v != qemuBMCVer {
		t.Fatalf("node2 の BMC が %s（qemu-bmc %s を期待）", v, qemuBMCVer)
	}
}

const converged = `resources:
  - {type: bmc/system, id: node1, fields: {power: "On", boot_enabled: Continuous, boot_target: Pxe}}
  - {type: bmc/virtual-media, id: node1/cd, fields: {image: ` + isoA + `}}
  - {type: bmc/system, id: node2, fields: {power: "On", boot_enabled: Disabled}}
`

func stepConverge(t *testing.T) {
	writeIntent(t, converged)
	before := mustSystem(t, node1)
	out, code := sakutei(t, "plan")
	if code != 0 || len(changes(out)) == 0 {
		t.Fatalf("plan exit=%d\n%s", code, out)
	}
	if after := mustSystem(t, node1); after != before {
		t.Fatalf("plan が BMC に書いた: %+v → %+v", before, after)
	}
	if out, code = sakutei(t, "reconcile"); code != 0 {
		t.Fatalf("reconcile exit=%d\n%s", code, out)
	}
	s1, s2, m := mustSystem(t, node1), mustSystem(t, node2), mustMedia(t, node1)
	if s1.PowerState != "On" || s1.Boot.BootSourceOverrideEnabled != "Continuous" || s1.Boot.BootSourceOverrideTarget != "Pxe" ||
		s2.PowerState != "On" || !m.Inserted || m.Image != isoA {
		t.Fatalf("収束していない: %+v %+v %+v\n%s", s1, s2, m, out)
	}
	if out, code = sakutei(t, "plan"); code != 0 || len(changes(out)) != 0 {
		t.Fatalf("収束したあとの plan exit=%d\n%s", code, out)
	}
}

func stepDrift(t *testing.T) {
	writeIntent(t, converged)
	handReset(t, node1, "ForceOff")
	handWrite(t, node1, "PATCH", "/redfish/v1/Systems/1", map[string]any{"Boot": map[string]string{"BootSourceOverrideEnabled": "Disabled"}})
	handWrite(t, node1, "POST", "/redfish/v1/Managers/1/VirtualMedia/CD1/Actions/VirtualMedia.EjectMedia", map[string]any{})
	waitFor(t, 30*time.Second, "node1 Off", powerIs(node1, "Off"))
	out, code := sakutei(t, "plan")
	if code != 0 || len(changes(out)) != 2 {
		t.Fatalf("手のずれが plan に出ない: exit=%d\n%s", code, out)
	}
	if out, code = sakutei(t, "reconcile"); code != 0 {
		t.Fatalf("reconcile exit=%d\n%s", code, out)
	}
	s, m := mustSystem(t, node1), mustMedia(t, node1)
	if s.PowerState != "On" || s.Boot.BootSourceOverrideEnabled != "Continuous" || m.Image != isoA || !m.Inserted {
		t.Fatalf("戻っていない: %+v %+v\n%s", s, m, out)
	}
}

func stepGraceful(t *testing.T) {
	writeIntent(t, `resources:
  - {type: bmc/system, id: node1, fields: {power: "On"}}
  - {type: bmc/system, id: node2, fields: {power: "Off"}}
procedures:
  - name: force-off
    target: {type: bmc/system, id: node2}
    trigger: manual
    steps:
      - invoke: {op: force_off}
    done_when: 'observe(bmc/system/{target}).power == "Off"'
`)
	w := watchPower(t, node2)
	var out string
	code := 0
	for i := 0; i < 5 && code != 4; i++ {
		out, code = sakutei(t, "reconcile")
	}
	if code != 4 || !strings.Contains(out, "効かない Apply") {
		t.Fatalf("応えない台の停止が効かない Apply で止まらない: exit=%d\n%s", code, out)
	}
	for _, s := range w.states() {
		if s == "Off" {
			t.Fatalf("既定の停止で Off になった（ForceOff を送った）: %v", w.states())
		}
	}
	if !strings.Contains(strings.Join(w.states(), ","), "PoweringOff") {
		t.Fatalf("GracefulShutdown を送っていない: %v", w.states())
	}
	if out, code := sakutei(t, "start", "force-off", "--as", "alice"); code != 0 {
		t.Fatalf("start exit=%d\n%s", code, out)
	}
	out, code = sakutei(t, "reconcile")
	waitFor(t, 30*time.Second, "node2 Off", powerIs(node2, "Off"))
	if out, code = sakutei(t, "reconcile"); code != 0 {
		t.Fatalf("force_off のあと exit=%d\n%s", code, out)
	}
}

func stepUnreachable(t *testing.T) {
	writeIntent(t, `resources:
  - {type: bmc/virtual-media, id: node1/cd, fields: {image: `+isoB+`}}
  - {type: bmc/system, id: node2, fields: {power: "On"}}
`)
	if out, err := sudoCmd("docker", "pause", containerName(node2)).CombinedOutput(); err != nil {
		t.Fatalf("docker pause: %v\n%s", err, out)
	}
	paused := true
	defer func() {
		if paused {
			_ = sudoCmd("docker", "unpause", containerName(node2)).Run()
		}
	}()
	out, code := sakutei(t, "reconcile")
	if code != 3 || !strings.Contains(out, "bmc/system/node2") {
		t.Fatalf("届かない台で exit=%d（want 3）\n%s", code, out)
	}
	if m := mustMedia(t, node1); m.Image != isoB {
		t.Fatalf("届く台が収束しない: %+v\n%s", m, out)
	}
	if out, err := sudoCmd("docker", "unpause", containerName(node2)).CombinedOutput(); err != nil {
		t.Fatalf("docker unpause: %v\n%s", err, out)
	}
	paused = false
	waitFor(t, 30*time.Second, "node2 の Redfish", func() (bool, string) {
		_, err := getSystem(node2)
		return err == nil, fmt.Sprint(err)
	})
	if s := mustSystem(t, node2); s.PowerState != "Off" {
		t.Fatalf("届かないのに node2 を変えた: %+v", s)
	}
}

func stepReject(t *testing.T) {
	for _, c := range []struct{ fields, want string }{
		{`{boot_enabled: Once, boot_target: Cd}`, "boot_once"},
		{`{power: ForceRestart}`, "一回きりの操作"},
	} {
		writeIntent(t, "resources:\n  - {type: bmc/system, id: node1, fields: "+c.fields+"}\n")
		before := mustSystem(t, node1)
		if out, code := sakutei(t, "plan"); code != 2 || !strings.Contains(out, c.want) {
			t.Fatalf("%s: exit=%d\n%s", c.fields, code, out)
		}
		if out, code := sakutei(t, "reconcile"); code != 2 {
			t.Fatalf("%s: reconcile exit=%d\n%s", c.fields, code, out)
		}
		if after := mustSystem(t, node1); after != before {
			t.Fatalf("断った宣言で BMC が変わった")
		}
	}
}

func stepBootOnce(t *testing.T) {
	writeIntent(t, `resources:
  - {type: bmc/system, id: node1, fields: {power: "On"}}
  - {type: bmc/virtual-media, id: node1/cd, fields: {image: `+isoA+`}}
procedures:
  - name: cd-once
    target: {type: bmc/system, id: node1}
    trigger: manual
    steps:
      - name: boot-once
        invoke: {op: boot_once, args: {target: Cd}}
      - name: reboot
        invoke: {op: force_restart}
`)
	if out, code := sakutei(t, "reconcile"); code != 0 {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	if out, code := sakutei(t, "start", "cd-once", "--as", "alice"); code != 0 {
		t.Fatalf("start exit=%d\n%s", code, out)
	}
	var out string
	for i := 0; i < 5 && !strings.Contains(out, "済んだ"); i++ {
		out, _ = sakutei(t, "reconcile")
	}
	if !strings.Contains(out, "手順 cd-once bmc/system/node1: 済んだ") {
		t.Fatalf("手順が済まない\n%s", out)
	}
	// BMC は 1 回の起動で上書きを Disabled に戻す（Once が使われた）。
	waitFor(t, time.Minute, "上書きが使われて Disabled", func() (bool, string) {
		s, err := getSystem(node1)
		if err != nil {
			return false, err.Error()
		}
		return s.PowerState == "On" && s.Boot.BootSourceOverrideEnabled == "Disabled", fmt.Sprintf("%+v", s)
	})
}

type biosDoc struct {
	Attributes map[string]any
}

func getBios(t *testing.T, n node) (current, pending biosDoc) {
	t.Helper()
	if err := getJSON(n, "/redfish/v1/Systems/1/Bios", &current); err != nil {
		t.Fatalf("%s: %v", n.name, err)
	}
	if err := getJSON(n, "/redfish/v1/Systems/1/Bios/Settings", &pending); err != nil {
		t.Fatalf("%s: %v", n.name, err)
	}
	return current, pending
}

// biosPatches は qemu-bmc が Settings の PATCH を受けた回数（ログの数）。
func biosPatches(n node) int {
	return strings.Count(containerLogs(n), "BIOS: pending until the next boot")
}

func stepBios(t *testing.T) {
	before, pending := getBios(t, node1)
	if len(pending.Attributes) != 0 || before.Attributes["ProcVirtualization"] != "Enabled" || before.Attributes["PowerProfile"] != "MaxPerformance" {
		t.Fatalf("node1 の BIOS が既定でない: %v %v", before.Attributes, pending.Attributes)
	}
	intent := `resources:
  - {type: bmc/system, id: node1, fields: {power: "On"}}
  - {type: bmc/bios, id: node1/ProcVirtualization, fields: {value: Disabled}}
  - {type: bmc/bios, id: node1/PowerProfile, fields: {value: Balanced}}
procedures:
  - name: bios-reboot
    target: {type: bmc/system, id: node1}
    trigger: manual
    steps:
      - invoke: {op: force_restart}
`
	writeIntent(t, intent)
	patches := biosPatches(node1)
	if out, code := sakutei(t, "plan"); code != 0 || len(changes(out)) != 2 {
		t.Fatalf("plan exit=%d\n%s", code, out)
	}
	if _, p := getBios(t, node1); len(p.Attributes) != 0 {
		t.Fatalf("plan が BMC に書いた: %v", p.Attributes)
	}
	out, code := sakutei(t, "reconcile")
	if code != 3 {
		t.Fatalf("保留中が残るのに exit=%d\n%s", code, out)
	}
	cur, p := getBios(t, node1)
	if p.Attributes["ProcVirtualization"] != "Disabled" || p.Attributes["PowerProfile"] != "Balanced" || cur.Attributes["ProcVirtualization"] != "Enabled" {
		t.Fatalf("保留中 %v・今 %v", p.Attributes, cur.Attributes)
	}
	if s := mustSystem(t, node1); s.PowerState != "On" {
		t.Fatalf("BIOS の Apply で再起動した: %+v", s)
	}
	if out, code = sakutei(t, "reconcile"); code != 3 || !strings.Contains(out, "次の再起動を待っている") {
		t.Fatalf("保留中の reconcile exit=%d\n%s", code, out)
	}
	if n := biosPatches(node1) - patches; n != 2 {
		t.Fatalf("Settings の PATCH が %d 回（want 2、保留中に重ねない）", n)
	}

	// 登録簿で読み取り専用の属性は送らない。
	other := "Bios"
	if cur, _ := getBios(t, node2); cur.Attributes["BootMode"] == "Bios" {
		other = "Uefi"
	}
	writeIntent(t, "resources:\n  - {type: bmc/bios, id: node2/BootMode, fields: {value: "+other+"}}\n")
	if out, code := sakutei(t, "reconcile"); code != 1 || !strings.Contains(out, "読み取り専用") {
		t.Fatalf("読み取り専用の属性で exit=%d\n%s", code, out)
	}
	if _, p := getBios(t, node2); len(p.Attributes) != 0 {
		t.Fatalf("node2 に保留中が付いた: %v", p.Attributes)
	}

	// 手順の force_restart で効く。
	writeIntent(t, intent)
	if out, code := sakutei(t, "start", "bios-reboot", "--as", "alice"); code != 0 {
		t.Fatalf("start exit=%d\n%s", code, out)
	}
	sakutei(t, "reconcile")
	waitFor(t, 2*time.Minute, "BIOS の保留が効く", func() (bool, string) {
		cur, p := getBios(t, node1)
		return len(p.Attributes) == 0 && cur.Attributes["ProcVirtualization"] == "Disabled", fmt.Sprint(p.Attributes)
	})
	waitFor(t, time.Minute, "node1 On", powerIs(node1, "On"))
	if out, code := sakutei(t, "plan"); code != 0 || len(changes(out)) != 0 {
		t.Fatalf("再起動のあとの plan exit=%d\n%s", code, out)
	}
	after, _ := getBios(t, node1)
	for k, v := range before.Attributes {
		if k == "ProcVirtualization" || k == "PowerProfile" {
			continue
		}
		if after.Attributes[k] != v {
			t.Fatalf("宣言していない属性 %s が %v → %v", k, v, after.Attributes[k])
		}
	}
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func stepFirmwareBMC(t *testing.T) {
	im := newImages(t, map[string]string{"bmc-2.0.0.bin": "BMC 2.0.0"})
	tasks0 := len(bmcTasks(t, node1))
	writeIntent(t, fmt.Sprintf(`cycle: {interval: 2s}
resources:
  - {type: bmc/system, id: node1, fields: {power: "On"}}
  - {type: bmc/firmware, id: node1/BMC, fields: {version: "2.0.0"}}
procedures:
  - name: bmc-fw
    target: {type: bmc/firmware, id: node1/BMC}
    trigger: on_drift
    drift_fields: [version]
    steps:
      - name: approve
        gate: {approval: {quorum: {n: 1, of: [bob]}}}
      - name: verify
        invoke: {op: verify, args: {image_uri: "%[1]s", sha256: "%[2]s"}}
      - name: stage
        critical: true
        skip_if: 'observe(bmc/firmware/{target}).version == desired.version'
        invoke: {op: stage, args: {image_uri: "%[1]s"}}
      - name: applied
        wait: {when: 'observe(bmc/firmware/{target}).update_state == "Completed"', timeout: 10m}
    done_when: 'observe(bmc/firmware/{target}).version == desired.version'
`, im.url("bmc-2.0.0.bin"), sum(im.body["bmc-2.0.0.bin"])))
	stop := startDaemon(t)
	var reqs []string
	waitFor(t, time.Minute, "承認の依頼", func() (bool, string) {
		reqs = pendingApprovals(t)
		return len(reqs) == 1, fmt.Sprint(reqs)
	})
	time.Sleep(5 * time.Second) // 承認の前に何も送らないことを数周見る
	if n, got := len(bmcTasks(t, node1))-tasks0, im.count("bmc-2.0.0.bin"); n != 0 || got != 0 {
		t.Fatalf("承認の前に BMC の Task %d・イメージの取得 %d", n, got)
	}
	if out, code := sakutei(t, "approve", reqs[0], "--as", "bob"); code != 0 {
		t.Fatalf("approve exit=%d\n%s", code, out)
	}
	waitFor(t, 3*time.Minute, "BMC 2.0.0", func() (bool, string) {
		v := getInventory(t, node1, "BMC").Version
		return v == "2.0.0", v
	})
	waitFor(t, time.Minute, "手順が閉じる", func() (bool, string) {
		out, _ := sakutei(t, "plan")
		return len(changes(out)) == 0 && !strings.Contains(out, "手順 bmc-fw"), out
	})
	stop()
	all := bmcTasks(t, node1)
	nt := all[tasks0:]
	if len(nt) != 1 || nt[0].TaskState != "Completed" || !nt[0].names(im.url("bmc-2.0.0.bin")) {
		t.Fatalf("新しい BMC の Task %+v（want 1 つ Completed でイメージを名指す）", nt)
	}
	if got := im.count("bmc-2.0.0.bin"); got != 2 {
		t.Fatalf("イメージの取得 %d 回（want 2：verify と BMC）", got)
	}
}

func stepFirmwareBIOS(t *testing.T) {
	im := newImages(t, map[string]string{"bios-2.0.0.bin": "BIOS BIOS-2.0.0"})
	tasks0 := len(bmcTasks(t, node1))
	os.RemoveAll(recordDir())
	writeIntent(t, fmt.Sprintf(`cycle: {interval: 2s}
resources:
  - {type: bmc/system, id: node1, fields: {power: "On"}}
  - {type: bmc/firmware, id: node1/BIOS, fields: {version: BIOS-2.0.0}}
procedures:
  - name: bios-fw
    target: {type: bmc/firmware, id: node1/BIOS}
    trigger: on_drift
    drift_fields: [version]
    steps:
      - name: stage
        critical: true
        skip_if: 'observe(bmc/firmware/{target}).update_image == "%[1]s"'
        invoke: {op: stage, args: {image_uri: "%[1]s"}}
      - name: staged
        wait: {when: 'observe(bmc/firmware/{target}).update_state == "Completed"', timeout: 5m}
      - name: reboot
        critical: true
        invoke: {op: force_restart}
      - name: active
        wait: {when: 'observe(bmc/firmware/{target}).version == desired.version', timeout: 5m}
    done_when: 'observe(bmc/firmware/{target}).version == desired.version'
`, im.url("bios-2.0.0.bin")))
	stop := startDaemon(t)
	waitFor(t, 2*time.Minute, "BIOS の BMC の Task が Completed", func() (bool, string) {
		ts := bmcTasks(t, node1)
		return len(ts) > tasks0 && ts[len(ts)-1].TaskState == "Completed", fmt.Sprint(len(ts))
	})
	waitFor(t, 3*time.Minute, "再起動のあと BIOS-2.0.0", func() (bool, string) {
		inv := getInventory(t, node1, "BIOS")
		return inv.Version == "BIOS-2.0.0" && inv.Status.State == "Enabled", fmt.Sprintf("%+v", inv)
	})
	waitFor(t, time.Minute, "node1 On", powerIs(node1, "On"))
	waitFor(t, time.Minute, "手順が閉じる", func() (bool, string) {
		out, _ := sakutei(t, "plan")
		return len(changes(out)) == 0 && !strings.Contains(out, "手順 bios-fw"), out
	})
	stop()
	if n := len(bmcTasks(t, node1)) - tasks0; n != 1 {
		t.Fatalf("BIOS の更新の Task %d（want 1）", n)
	}
}

func stepFirmwareFaults(t *testing.T) {
	im := newImages(t, map[string]string{"n2-bios-9.0.0.bin": "BIOS QBMC-BIOS-9.0.0", "n2-bmc-9.0.0.bin": "BMC v9.0.0"})
	os.RemoveAll(recordDir())
	faultDo(t, "POST", map[string]any{"kind": "firmware-image-rejected", "target": "BIOS"})
	faultDo(t, "POST", map[string]any{"kind": "firmware-update-fail", "target": "BMC", "atPercent": 30})
	defer faultDo(t, "DELETE", nil)
	tasks0 := len(bmcTasks(t, node2))
	proc := func(name, comp, image string) string {
		return fmt.Sprintf(`  - name: %[1]s
    target: {type: bmc/firmware, id: node2/%[2]s}
    trigger: on_drift
    drift_fields: [version]
    steps:
      - name: stage
        invoke: {op: stage, args: {image_uri: "%[3]s"}}
      - name: applied
        wait: {when: 'observe(bmc/firmware/{target}).update_state == "Completed"', timeout: 20s}
    done_when: 'observe(bmc/firmware/{target}).version == desired.version'
`, name, comp, im.url(image))
	}
	writeIntent(t, `resources:
  - {type: bmc/firmware, id: node2/BIOS, fields: {version: QBMC-BIOS-9.0.0}}
  - {type: bmc/firmware, id: node2/BMC, fields: {version: v9.0.0}}
procedures:
`+proc("bios-fw", "BIOS", "n2-bios-9.0.0.bin")+proc("bmc-fw", "BMC", "n2-bmc-9.0.0.bin"))
	var out string
	deadline := time.Now().Add(90 * time.Second)
	for !(strings.Contains(out, "手順 bios-fw bmc/firmware/node2/BIOS: 止めた") && strings.Contains(out, "手順 bmc-fw bmc/firmware/node2/BMC: 止めた")) {
		if time.Now().After(deadline) {
			t.Fatalf("故障で手順が止まらない\n%s", out)
		}
		out, _ = sakutei(t, "reconcile")
		time.Sleep(3 * time.Second)
	}
	// 止まったあとは送り直さない（不変条件 16）。
	for i := 0; i < 2; i++ {
		out, _ = sakutei(t, "reconcile")
	}
	nt := bmcTasks(t, node2)[tasks0:]
	if len(nt) != 1 || nt[0].TaskState != "Exception" || !nt[0].names(im.url("n2-bmc-9.0.0.bin")) {
		t.Fatalf("新しい BMC の Task %+v（want BMC の 1 つが Exception）\n%s", nt, out)
	}
	if b, m := im.count("n2-bios-9.0.0.bin"), im.count("n2-bmc-9.0.0.bin"); b > 1 || m != 1 {
		t.Fatalf("イメージの取得 BIOS %d・BMC %d 回（送り直した）", b, m)
	}
	if v := getInventory(t, node2, "BIOS").Version; v != node2BIOSVer {
		t.Fatalf("BIOS が %s", v)
	}
	if v := getInventory(t, node2, "BMC").Version; v != qemuBMCVer {
		t.Fatalf("BMC が %s", v)
	}
	if !strings.Contains(out, "止めた") {
		t.Fatalf("止めた理由が出ない\n%s", out)
	}
}

func stepWriteCredentials(t *testing.T) {
	cfg := filepath.Join(lab.dir, "config-nowrite.yaml")
	if err := os.WriteFile(cfg, []byte(config(filepath.Join(lab.dir, "no-such-password"))), 0o600); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(recordDir()) // 10 段目で止めた手順を持ち込まない
	saved := lab.config
	lab.config = cfg
	defer func() { lab.config = saved }()
	want := isoA
	if mustMedia(t, node1).Image == isoA {
		want = isoB
	}
	writeIntent(t, "resources:\n  - {type: bmc/virtual-media, id: node1/cd, fields: {image: "+want+"}}\n")
	if out, code := sakutei(t, "plan"); code != 0 || len(changes(out)) != 1 {
		t.Fatalf("書き込み用の資格が無くても plan は観測できる: exit=%d\n%s", code, out)
	}
	out, code := sakutei(t, "reconcile")
	if code != 1 || !strings.Contains(out, "パスワードのファイル") {
		t.Fatalf("書き込み用の資格が無いのに exit=%d\n%s", code, out)
	}
	if m := mustMedia(t, node1); m.Image == want {
		t.Fatalf("書き込み用の資格が無いのに書いた")
	}
}
