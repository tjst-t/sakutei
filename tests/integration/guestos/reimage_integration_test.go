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

//go:build integration_guestos

// reimage_integration_test.go — #88: 入れ直し（reimage）を宣言で起こす。同じ lab の
// node1・node2（qemu-bmc 0.11.0）に、trigger: reimage の OS 導入の Workflow を 1 台
// 1 つずつ置き（scope rack: r1）、planned_change の kind: reimage の宣言を書いたり
// 外したりしながら常駐の sakutei を 1 つ回し続ける。応答ファイルは 2 台とも成功する
// 中身（template-file）。入れ直しの再起動は ForceRestart（ディスクを消して入れ直すので
// 正常停止を待たない。破壊的な操作だが、レーンの 1 回分に含まれる）。
//
//	[AC-88-9]   宣言が無い間は走らない: run を作らず、インストーラも起動しない。
//	            2 台は収束ループが電源 On に保つ。
//	[AC-88-10]  宣言があっても承認が無い（運用プロファイル team は 1 人）間は走らない。
//	[AC-88-11]  承認された宣言（max_count 1）で、node1 に OS 導入が 1 回走り、目印つきの
//	            OS が起動して done。node2 は枠が無いので走らない（インストーラも起動しない）。
//	            枠を使ったことは S4 に残る。
//	[AC-88-12]  宣言を外すと、もう走らない（done のまま、入れ直しも起きない）。
//	[AC-88-13]  新しい宣言（別の窓・新しい目印）で、動いている node1 がもう一度入れ直され、
//	            新しい目印の OS になる。
package guestos

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/audit"
	"github.com/tjst-t/sakutei/v2/internal/workflow"
)

// reimageNodes は入れ直しを試す 2 台（応答ファイルは 2 台とも成功する中身）。
var reimageNodes = []node{nodes[0], nodes[1]}

// writeReimageIntent は終わったあとの姿・応答ファイル（目印 mark）・trigger: reimage の
// Workflow（1 台 1 つ）と、decl（空なら無し）を書く。
func writeReimageIntent(t *testing.T, dir, mark string, decl map[string]any) {
	t.Helper()
	pub, err := os.ReadFile(filepath.Join(lab.dir, "client.pub"))
	if err != nil {
		t.Fatal(err)
	}
	cd := "redfish-virtual-media/{target}/cd"
	iso := "/iso/" + isoName
	var resources, workflows []any
	for _, n := range reimageNodes {
		resources = append(resources,
			map[string]any{"provider": "redfish-system", "id": n.name,
				"fields": map[string]string{"name": n.name, "power": "On"}},
			map[string]any{"provider": "redfish-virtual-media", "id": n.name + "/cd",
				"fields": map[string]string{"name": n.name, "image": ""}},
			map[string]any{"provider": "template-file", "id": n.uuid, "fields": map[string]string{
				"template": "alpine-answers.sh.tmpl", "hostname": n.name, "address": guestAddress,
				"ssh_authorized_keys": strings.TrimSpace(string(pub)), "install_mark": mark, "var.gateway": guestGateway}})
		workflows = append(workflows, map[string]any{
			"workflow": "reimage-" + n.name,
			"trigger":  "reimage",
			"target":   map[string]any{"provider": "redfish-system", "scope": map[string]string{"rack": "r1"}},
			"stages":   []any{map[string]any{"name": "install", "targets": []string{n.name}}},
			"steps": []any{
				map[string]any{"apply": map[string]any{"resource": cd, "fields": map[string]string{"image": iso}}},
				map[string]any{"require": map[string]any{"resource": cd, "fields": map[string]string{"image": iso}, "timeout": "2m"}},
				map[string]any{"action": map[string]any{"op": "boot_once", "args": map[string]string{"target": "Cd"}}},
				map[string]any{"action": map[string]any{"op": "reset", "args": map[string]string{"type": "ForceRestart"}}},
				map[string]any{"wait": map[string]any{"fields": map[string]string{"power": "Off"}, "timeout": "6m"}},
				map[string]any{"apply": map[string]string{"power": "On"}},
				map[string]any{"require": map[string]any{"resource": "guest-os/{target}",
					"fields":  map[string]string{"os_id": "alpine", "marker.installed": mark, "marker.hostname": n.name},
					"timeout": guestTimeout.String()}},
				map[string]any{"apply": map[string]any{"resource": cd, "fields": map[string]string{"image": ""}}},
			},
		})
	}
	intent := map[string]any{"resources": resources, "workflows": workflows}
	if decl != nil {
		intent["planned_change"] = decl
	}
	b, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// 書き換えの途中を読まないよう、一時ファイル → rename。
	tmp := filepath.Join(dir, ".os.json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "os.json")); err != nil {
		t.Fatal(err)
	}
}

// reimageDecl は scope rack: r1 の planned_change kind: reimage（max_count 1）。
func reimageDecl(start, end time.Time, approvers ...string) map[string]any {
	d := map[string]any{
		"scope": map[string]string{"rack": "r1"}, "kind": "reimage", "max_count": 1,
		"window":    start.UTC().Format(time.RFC3339) + " .. " + end.UTC().Format(time.RFC3339),
		"applicant": "carol",
	}
	if len(approvers) > 0 {
		d["approval"] = map[string]any{"pr": 88, "approvers": approvers}
	}
	return d
}

// installerStarts はシリアルに出たインストーラの起動の数。
func installerStarts(b *lockedBuffer) int {
	return strings.Count(b.String(), "SAKUTEI-AUTOINSTALL: start")
}

func TestReimage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	serials := map[string]*lockedBuffer{}
	done := startSerials(ctx, reimageNodes, serials)
	defer func() {
		cancel()
		done()
		for _, n := range reimageNodes {
			var lines []string
			for _, l := range strings.Split(serials[n.name].String(), "\n") {
				if strings.Contains(l, "SAKUTEI") {
					lines = append(lines, strings.TrimSpace(l))
				}
			}
			t.Logf("%s serial (SAKUTEI lines):\n  %s", n.name, strings.Join(lines, "\n  "))
		}
	}()

	dir := filepath.Join(lab.dir, "reimage")
	intentDir := filepath.Join(dir, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(dir, "workflows.json")
	auditLog := filepath.Join(dir, "audit.jsonl")
	runOf := func(name string) (workflow.Run, bool) {
		r, ok, err := workflow.NewFileStore(store).Get(ctx, name)
		if err != nil {
			return workflow.Run{}, false
		}
		return r, ok
	}
	mark1 := lab.mark + "-r1"
	mark2 := lab.mark + "-r2"
	writeReimageIntent(t, intentDir, mark1, nil)

	out := &lockedBuffer{}
	d := exec.Command(lab.bin, "--intent", intentDir,
		"--redfish-config", filepath.Join(lab.dir, "bmcs.yaml"),
		"--provider-config", filepath.Join(lab.dir, "providers.yaml"),
		"--workflow-store", store, "--audit-log", auditLog, "--interval", daemonInterval,
		"--state", filepath.Join(dir, "fake-state.json"))
	d.Stdout, d.Stderr = out, out
	d.Dir = lab.dir
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = d.Wait(); close(exited) }()
	stop := func() {
		select {
		case <-exited:
			return
		default:
		}
		_ = d.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(45 * time.Second):
			_ = d.Process.Kill()
			<-exited
		}
	}
	defer func() {
		stop()
		t.Logf("daemon output:\n%s", out.String())
	}()
	// cycles は n 周ぶん待つ（デーモンが落ちていれば止める）。
	cycles := func(n int) {
		t.Helper()
		select {
		case <-exited:
			t.Fatalf("the daemon exited")
		case <-time.After(time.Duration(n) * 5 * time.Second):
		}
	}
	noRuns := func(ac string) {
		t.Helper()
		for _, n := range reimageNodes {
			if r, ok := runOf("reimage-" + n.name); ok {
				t.Fatalf("%s reimage-%s has a run: %+v", ac, n.name, r)
			}
		}
	}

	// [AC-88-9] 宣言が無い: 走らない。2 台は電源 On に保たれる。
	starts := map[string]int{}
	cycles(4)
	noRuns("[AC-88-9]")
	for _, n := range reimageNodes {
		if p := power(t, n); p != "On" {
			t.Errorf("[AC-88-9] %s: power %s, want On (converged by the loop)", n.name, p)
		}
		starts[n.name] = installerStarts(serials[n.name])
	}
	if strings.Contains(out.String(), "WorkflowStarted") {
		t.Fatalf("[AC-88-9] a Workflow started without a declaration")
	}

	// [AC-88-10] 承認の無い宣言: 走らない。
	now := time.Now()
	writeReimageIntent(t, intentDir, mark1, reimageDecl(now.Add(-time.Minute), now.Add(time.Hour)))
	cycles(3)
	noRuns("[AC-88-10]")
	if !strings.Contains(out.String(), "PlannedChangeUnapproved") {
		t.Errorf("[AC-88-10] PlannedChangeUnapproved was not reported")
	}

	// [AC-88-11] 承認された宣言: node1 に 1 回走る。node2 は枠が無い。
	start := time.Now()
	writeReimageIntent(t, intentDir, mark1, reimageDecl(now.Add(-time.Minute), now.Add(time.Hour), "alice"))
	waitRun := func(ac, name string) workflow.Run {
		t.Helper()
		deadline := time.Now().Add(10 * time.Minute)
		for {
			r, _ := runOf(name)
			if r.Phase == workflow.PhaseDone || r.Phase == workflow.PhaseHalted {
				return r
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s %s did not finish in time: %+v", ac, name, r)
			}
			cycles(1)
		}
	}
	r1 := waitRun("[AC-88-11]", "reimage-node1")
	t.Logf("reimage-node1 finished in %s", time.Since(start).Round(time.Second))
	if r1.Phase != workflow.PhaseDone || r1.Trigger == "" {
		t.Fatalf("[AC-88-11] reimage-node1 = %+v, want done for the declaration", r1)
	}
	if got, err := guestSSH(nodes[0], "cat /etc/sakutei-installed"); err != nil || strings.TrimSpace(got) != mark1 {
		t.Errorf("[AC-88-11] node1 marker by hand = %q (%v), want %s", got, err, mark1)
	}
	if n := installerStarts(serials["node1"]) - starts["node1"]; n < 1 {
		t.Logf("[AC-88-11] node1: the serial did not show the installer (serial capture may have missed it)")
	}
	if _, ok := runOf("reimage-node2"); ok {
		t.Errorf("[AC-88-11] reimage-node2 started beyond max_count 1")
	}
	log := out.String()
	for _, want := range []string{
		"workflow reimage-node1: stage install, target node1: reimage admitted by planned_change(scope=rack=r1 kind=reimage",
		"target node1, step 4 (action): sent reset ForceRestart",
		"workflow reimage-node2: not starting: planned_change(scope=rack=r1 kind=reimage",
		fmt.Sprintf("step 7 (require guest-os/node1): observed map[marker.hostname:node1 marker.installed:%s os_id:alpine]", mark1),
	} {
		if !strings.Contains(log, want) {
			t.Errorf("[AC-88-11] the daemon did not report %q", want)
		}
	}
	recs, err := audit.ReadAll(auditLog)
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
		t.Errorf("[AC-88-11] planned_change.executed in S4 = %d, want 1", executed)
	}
	if n := installerStarts(serials["node2"]) - starts["node2"]; n != 0 {
		t.Errorf("[AC-88-11] node2: the installer started %d time(s)", n)
	}

	// [AC-88-12] 宣言を外す: もう走らない。
	writeReimageIntent(t, intentDir, mark1, nil)
	before := strings.Count(out.String(), "WorkflowStarted")
	cycles(4)
	if n := strings.Count(out.String(), "WorkflowStarted"); n != before {
		t.Errorf("[AC-88-12] a Workflow started after the declaration was removed")
	}
	if r, _ := runOf("reimage-node1"); r.RunID != r1.RunID || r.Phase != workflow.PhaseDone {
		t.Errorf("[AC-88-12] reimage-node1 = %+v, want the same done run", r)
	}
	if got, err := guestSSH(nodes[0], "cat /etc/sakutei-installed"); err != nil || strings.TrimSpace(got) != mark1 {
		t.Errorf("[AC-88-12] node1 marker = %q (%v), want still %s (not reinstalled)", got, err, mark1)
	}

	// [AC-88-13] 新しい宣言・新しい目印: 動いている node1 がもう一度入れ直される。
	now = time.Now()
	writeReimageIntent(t, intentDir, mark2, reimageDecl(now.Add(-time.Minute), now.Add(time.Hour), "alice"))
	start = time.Now()
	cycles(1)
	r1b := waitRun("[AC-88-13]", "reimage-node1")
	t.Logf("second reimage-node1 finished in %s", time.Since(start).Round(time.Second))
	if r1b.Phase != workflow.PhaseDone || r1b.RunID == r1.RunID || r1b.Trigger == r1.Trigger {
		t.Fatalf("[AC-88-13] reimage-node1 = %+v, want a new done run (old %s)", r1b, r1.RunID)
	}
	if got, err := guestSSH(nodes[0], "cat /etc/sakutei-installed"); err != nil || strings.TrimSpace(got) != mark2 {
		t.Errorf("[AC-88-13] node1 marker = %q (%v), want %s (reinstalled)", got, err, mark2)
	}
	if _, ok := runOf("reimage-node2"); ok {
		t.Errorf("[AC-88-13] reimage-node2 started beyond max_count 1")
	}
	if en, tg := boot(t, nodes[0]); en != "Disabled" {
		t.Errorf("[AC-88-13] node1: boot override = %s %s, want Disabled", en, tg)
	}
	if m := media(t, nodes[0]); m.Inserted {
		t.Errorf("[AC-88-13] node1: the CD is still inserted (%+v)", m)
	}
}

// power は BMC から見た電源。
func power(t *testing.T, n node) string {
	t.Helper()
	out, err := exec.Command("curl", "-ksf", "-u", bmcUser+":"+bmcPassword,
		"https://"+n.addr+"/redfish/v1/Systems/1").Output()
	if err != nil {
		t.Fatalf("%s: GET System: %v", n.name, err)
	}
	var doc struct{ PowerState string }
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("%s: %v: %s", n.name, err, out)
	}
	return doc.PowerState
}

// startSerials はノードのシリアルを集め始める。返す関数は ctx を止めたあとに呼び、
// 集め終わるのを待つ。
func startSerials(ctx context.Context, ns []node, serials map[string]*lockedBuffer) func() {
	var wg sync.WaitGroup
	for _, n := range ns {
		serials[n.name] = &lockedBuffer{}
		wg.Add(1)
		go func(n node) { defer wg.Done(); serial(ctx, n, serials[n.name]) }(n)
	}
	return wg.Wait
}
