// Copyright 2026 tjst-t
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// runtime_kinds_test.go — #193: runtime: の値を実行の種類（常駐・一回性）ごとに
// 宣言する（runtime.daemon: / runtime.oneshot:、§26.7）。種類の欄にあればそれ、
// 無ければ共通の runtime: の値が効き、旗はその種類に効く値と突き合わせる。
//
//	[AC-193-1]  常駐と一回性で、それぞれ自分の種類の値が効く（常駐は
//	            runtime.daemon.mode: alert-only で何も Apply せず、一回性の
//	            reconcile は runtime.oneshot.mode: auto で Apply する）。
//	            種類の値と同じ旗は締めではない（RuntimeTightened が出ない）。
//	[AC-193-2]  種類の欄が無いキーは共通の runtime: の値に戻る。
//	[AC-193-3]  締める旗は種類に効く値に対して効き、RuntimeTightened に
//	            その値と欄（runtime.oneshot.mode）が出る。緩める旗は種類に効く
//	            値に対して断り、直し方はその欄を指す（常駐・reconcile・plan）。
//	[AC-193-4]  oneshot: の interval・stuck_after は読み込みで断る。
package acceptance

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/statusreport"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// startRuntimeDaemon runs the daemon on r until it has run cycles cycles and
// returns its output and status; the daemon is killed before returning.
func startRuntimeDaemon(t *testing.T, tag string, r *runtimeRig, cycles int, args ...string) (string, statusreport.DaemonHealth) {
	t.Helper()
	cmd := exec.Command(buildBinary(t), append([]string{"--intent", r.intent, "--state", r.state,
		"--daemon-status", r.health, "--event-log", r.events}, args...)...)
	var buf syncBuffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if !waitForCondition(10*time.Second, func() bool {
		h, err := statusreport.ReadDaemonHealth(r.health)
		return err == nil && h.Cycle >= cycles
	}) {
		t.Fatalf("%s daemon did not run %d cycles within 10s:\n%s", tag, cycles, buf.String())
	}
	h, _ := statusreport.ReadDaemonHealth(r.health)
	return buf.String(), h
}

const runtimeKindsYAML = "runtime:\n  mode: auto\n  max_changes_per_cycle: 20\n" +
	"  daemon:\n    mode: alert-only\n    interval: 150ms\n" +
	"  oneshot:\n    mode: auto\n    max_changes_per_cycle: 5\n"

func TestRuntimeKinds_EachKindTakesItsOwnValues(t *testing.T) {
	// 常駐: runtime.daemon.mode alert-only と同じ旗（systemd のユニットに残った
	// --mode alert-only）は締めではない。何も Apply しない。
	d := newRuntimeRig(t, runtimeKindsYAML)
	out, h := startRuntimeDaemon(t, "[AC-193-1]", d, 2, "--mode", "alert-only")
	if h.Interval != "150ms" || len(h.RuntimeTightened) != 0 || h.RuntimeChanged != "" {
		t.Errorf("[AC-193-1] daemon health = %+v, want runtime.daemon.interval 150ms and no tightening", h)
	}
	if data := readStateFile(t, d.state); resourceInState(data, "web-1") {
		t.Errorf("[AC-193-1] runtime.daemon.mode alert-only must not Apply; state:\n%s", data)
	}
	if n := len(eventsOf(readEvents(t, d.events), core.EventRuntimeTightened, core.EventFiring)); n != 0 {
		t.Errorf("[AC-193-1] the daemon's own value as a flag must not raise RuntimeTightened (%d):\n%s", n, out)
	}

	// 一回性: runtime.oneshot.mode auto で Apply する。
	o := newRuntimeRig(t, runtimeKindsYAML)
	out, code := o.run(t, "reconcile", "--intent", o.intent, "--state", o.state, "--event-log", o.events)
	if code != 0 {
		t.Fatalf("[AC-193-1] reconcile exit=%d:\n%s", code, out)
	}
	if data := readStateFile(t, o.state); !resourceInState(data, "web-1") || !resourceInState(data, "web-2") {
		t.Errorf("[AC-193-1] runtime.oneshot.mode auto must Apply; state:\n%s", data)
	}
	if len(eventsOf(readEvents(t, o.events), core.EventRuntimeTightened, "")) != 0 {
		t.Errorf("[AC-193-1] no flag given: no RuntimeTightened")
	}
}

func TestRuntimeKinds_FallBackToCommon(t *testing.T) {
	// 種類の欄に mode が無い: 共通の alert-only が両方に効く。
	yaml := "runtime:\n  mode: alert-only\n  daemon:\n    interval: 150ms\n  oneshot:\n    settle: 1s\n"
	o := newRuntimeRig(t, yaml)
	if out, code := o.run(t, "reconcile", "--intent", o.intent, "--state", o.state); code != 0 {
		t.Fatalf("[AC-193-2] reconcile exit=%d:\n%s", code, out)
	}
	if data := readStateFile(t, o.state); resourceInState(data, "web-1") {
		t.Errorf("[AC-193-2] reconcile must fall back to runtime.mode alert-only; state:\n%s", data)
	}
	out, code := o.run(t, "reconcile", "--intent", o.intent, "--state", o.state, "--mode", "auto")
	assertRefusalLine(t, "[AC-193-2] reconcile --mode auto", out, code, "mode", "mode", "auto", "alert-only")

	d := newRuntimeRig(t, yaml)
	out, h := startRuntimeDaemon(t, "[AC-193-2]", d, 2)
	if h.Interval != "150ms" || !strings.Contains(out, "alert-only") {
		t.Errorf("[AC-193-2] daemon must take runtime.daemon.interval and runtime.mode: health %+v\n%s", h, out)
	}
	if data := readStateFile(t, d.state); resourceInState(data, "web-1") {
		t.Errorf("[AC-193-2] daemon must fall back to runtime.mode alert-only; state:\n%s", data)
	}
}

func TestRuntimeKinds_FlagsAgainstTheKindsValues(t *testing.T) {
	r := newRuntimeRig(t, runtimeKindsYAML)

	// 締める: 一回性の --mode alert-only は runtime.oneshot.mode auto を締める。
	out, code := r.run(t, "reconcile", "--intent", r.intent, "--state", r.state,
		"--mode", "alert-only", "--max-changes-per-cycle", "2", "--event-log", r.events)
	if code != 0 {
		t.Fatalf("[AC-193-3] reconcile exit=%d:\n%s", code, out)
	}
	if data := readStateFile(t, r.state); resourceInState(data, "web-1") {
		t.Errorf("[AC-193-3] --mode alert-only must win; state:\n%s", data)
	}
	evs := eventsOf(readEvents(t, r.events), core.EventRuntimeTightened, "")
	if len(evs) != 1 {
		t.Fatalf("[AC-193-3] RuntimeTightened = %d, want 1:\n%s", len(evs), out)
	}
	a := evs[0].Attrs
	if a["mode.flag"] != "alert-only" || a["mode.intent"] != "auto" || a["max_changes_per_cycle.flag"] != "2" || a["max_changes_per_cycle.intent"] != "5" {
		t.Errorf("[AC-193-3] attrs = %+v, want the one-shot values (auto, 5)", a)
	}
	if !strings.Contains(evs[0].Message, "runtime.oneshot.mode auto") || !strings.Contains(evs[0].Message, "runtime.oneshot.max_changes_per_cycle 5") {
		t.Errorf("[AC-193-3] message does not name the runtime.oneshot: values: %s", evs[0].Message)
	}

	// 緩める: 一回性の 10 は runtime.oneshot の 5 を緩める（共通の 20 より小さくても）。
	out, code = r.run(t, "reconcile", "--intent", r.intent, "--state", r.state, "--max-changes-per-cycle", "10")
	assertRefusalLine(t, "[AC-193-3] reconcile", out, code, "max-changes-per-cycle", "oneshot.max_changes_per_cycle", "10", "5")
	out, code = r.run(t, "plan", "--intent", r.intent, "--state", r.state, "--max-changes-per-cycle", "10")
	assertRefusalLine(t, "[AC-193-3] plan", out, code, "max-changes-per-cycle", "oneshot.max_changes_per_cycle", "10", "5")
	// 常駐の --mode auto は runtime.daemon.mode alert-only を緩める（共通は auto でも）。
	out, code = r.run(t, "--intent", r.intent, "--state", r.state, "--mode", "auto")
	assertRefusalLine(t, "[AC-193-3] daemon", out, code, "mode", "daemon.mode", "auto", "alert-only")
	// 向きの無い引数: 常駐の interval は runtime.daemon.interval と比べる。
	out, code = r.run(t, "--intent", r.intent, "--state", r.state, "--interval", "5s")
	assertRefusalLine(t, "[AC-193-3] daemon interval", out, code, "interval", "daemon.interval", "5s", "150ms")
	// 常駐の予算の旗は共通の 20 と比べる（daemon: に無い）: 25 は緩める。
	out, code = r.run(t, "--intent", r.intent, "--state", r.state, "--max-changes-per-cycle", "25")
	assertRefusalLine(t, "[AC-193-3] daemon budget", out, code, "max-changes-per-cycle", "max_changes_per_cycle", "25", "20")
	if data := readStateFile(t, r.state); resourceInState(data, "web-1") {
		t.Errorf("[AC-193-3] a refused start must not Apply; state:\n%s", data)
	}

	// 常駐の締め: status に欄の名前が出る。
	d := newRuntimeRig(t, runtimeKindsYAML)
	_, h := startRuntimeDaemon(t, "[AC-193-3]", d, 2, "--max-changes-per-cycle", "1")
	if len(h.RuntimeTightened) != 1 || h.RuntimeTightened[0].IntentValue != "20" || h.RuntimeTightened[0].IntentPath() != "runtime.max_changes_per_cycle" {
		t.Errorf("[AC-193-3] daemon runtime_tightened = %+v", h.RuntimeTightened)
	}
}

func TestRuntimeKinds_LoadRefusals(t *testing.T) {
	r := newRuntimeRig(t, "runtime:\n  interval: 30s\n")
	for _, c := range []struct{ yaml, want string }{
		{"runtime:\n  oneshot:\n    interval: 30s\n", "runtime.oneshot: interval"},
		{"runtime:\n  oneshot:\n    stuck_after: 3\n", "runtime.oneshot: stuck_after"},
		{"runtime:\n  daemon:\n    intervl: 30s\n", "intervl"},
		{"runtime:\n  daemon:\n    mode: sometimes\n", "mode"},
	} {
		r.writeRuntime(t, c.yaml)
		for _, verb := range [][]string{{"plan"}, {"reconcile"}} {
			out, code := r.run(t, append(verb, "--intent", r.intent, "--state", r.state)...)
			if code != 1 || !strings.Contains(out, c.want) {
				t.Errorf("[AC-193-4] %v %q must be refused naming %q: exit %d:\n%s", verb, c.yaml, c.want, code, out)
			}
		}
	}
	if data := readStateFile(t, r.state); resourceInState(data, "web-1") {
		t.Errorf("[AC-193-4] a refused load must not Apply; state:\n%s", data)
	}
}
