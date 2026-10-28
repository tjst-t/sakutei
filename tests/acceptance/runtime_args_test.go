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

// runtime_args_test.go — #183: 長く効く起動の引数を intent の runtime: に書く
// （§26.7）。旗と宣言が食い違ったとき、締める旗は効いて status と level の
// Event に両方の値が出る。緩める旗と向きの無い食い違いは起動を断り、その 1 行に
// 引数の名前・両方の値・直し方が出る。
//
//	[AC-183-1]  runtime: が読める: 宣言だけの値が使われる（一回性の reconcile の
//	            mode: alert-only は何も Apply しない。常駐の interval は宣言の値）。
//	[AC-183-2]  締める旗（一回性の reconcile）: 旗が効き、RuntimeTightened の
//	            Event に旗の値と宣言の値が両方出る。
//	[AC-183-3]  締める旗（常駐）: --daemon-status の runtime_tightened・sakutei
//	            status の表示・level の Event（firing）に旗の値と宣言の値が出る。
//	[AC-183-4]  緩める旗は起動を断る（常駐・reconcile・plan）: 1 行に引数の名前・
//	            両方の値・直し方（旗を外すか宣言を変えるか）。上限の 0（上限なし）も
//	            緩める側。
//	[AC-183-5]  向きの無い引数（interval・settle）は食い違えば断る（同じ値なら通る）。
//	[AC-183-6]  runtime: を 2 つのファイルに書く・知らないキー・範囲の外は読み込みで断る。
//	[AC-183-7]  常駐が起動したあとに runtime: が変わると RuntimeDeclarationChanged
//	            （起動したときの値で回る）が出て、status にも出る。戻すと resolved。
package acceptance

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/statusreport"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

type runtimeRig struct {
	dir, intent, state, events, health string
}

func newRuntimeRig(t *testing.T, runtimeYAML string) *runtimeRig {
	t.Helper()
	tmp := t.TempDir()
	r := &runtimeRig{dir: tmp, intent: filepath.Join(tmp, "intent"), state: filepath.Join(tmp, "state.json"),
		events: filepath.Join(tmp, "events.jsonl"), health: filepath.Join(tmp, "daemon-status.json")}
	if err := os.MkdirAll(r.intent, 0o755); err != nil {
		t.Fatal(err)
	}
	initStateFile(t, r.state)
	writeIntentFile(t, r.intent, map[string]any{"resources": []map[string]any{
		{"provider": "fake", "id": "web-1", "fields": map[string]string{"name": "web-1"}},
		{"provider": "fake", "id": "web-2", "fields": map[string]string{"name": "web-2"}},
	}})
	r.writeRuntime(t, runtimeYAML)
	return r
}

func (r *runtimeRig) writeRuntime(t *testing.T, runtimeYAML string) {
	t.Helper()
	if err := writeFileAtomic(filepath.Join(r.intent, "00-runtime.yaml"), []byte(runtimeYAML), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (r *runtimeRig) run(t *testing.T, args ...string) (string, int) {
	t.Helper()
	return runSakutei(t, args...)
}

// readEvents reads the --event-log file.
func readEvents(t *testing.T, path string) []core.Event {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []core.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var ev core.Event
		if json.Unmarshal(sc.Bytes(), &ev) == nil {
			out = append(out, ev)
		}
	}
	return out
}

func eventsOf(evs []core.Event, kind core.EventKind, state core.EventState) []core.Event {
	var out []core.Event
	for _, ev := range evs {
		if ev.Kind == kind && ev.State == state {
			out = append(out, ev)
		}
	}
	return out
}

// assertRefusalLine checks that out is exit 1 with one line naming the
// argument, both values and how to fix it.
func assertRefusalLine(t *testing.T, tag, out string, code int, flagName, key, flagVal, intentVal string) {
	t.Helper()
	if code != 1 {
		t.Fatalf("%s exit = %d, want 1 (refused):\n%s", tag, code, out)
	}
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "--"+flagName+" ") && strings.Contains(l, "runtime."+key) {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("%s no refusal line naming --%s and runtime.%s:\n%s", tag, flagName, key, out)
	}
	for _, want := range []string{"--" + flagName + " " + flagVal, "runtime." + key + " " + intentVal,
		"remove --" + flagName, "change runtime." + key + " in 00-runtime.yaml"} {
		if !strings.Contains(line, want) {
			t.Errorf("%s refusal line lacks %q: %s", tag, want, line)
		}
	}
}

func TestRuntime_IntentOnlyValuesAreUsed(t *testing.T) {
	r := newRuntimeRig(t, "runtime:\n  mode: alert-only\n  interval: 150ms\n")
	out, code := r.run(t, "reconcile", "--intent", r.intent, "--state", r.state)
	if code != 0 {
		t.Fatalf("[AC-183-1] reconcile exit=%d:\n%s", code, out)
	}
	if data := readStateFile(t, r.state); resourceInState(data, "web-1") {
		t.Errorf("[AC-183-1] runtime.mode alert-only must not Apply; state:\n%s", data)
	}

	// 常駐: 宣言の interval で回る（旗の既定の 10s ではない）。mode も宣言どおり。
	cmd := exec.Command(buildBinary(t), "--intent", r.intent, "--state", r.state, "--daemon-status", r.health)
	var buf syncBuffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if !waitForCondition(10*time.Second, func() bool {
		h, err := statusreport.ReadDaemonHealth(r.health)
		return err == nil && h.Cycle >= 2
	}) {
		t.Fatalf("[AC-183-1] daemon did not run 2 cycles within 10s (interval from runtime: not used?):\n%s", buf.String())
	}
	h, _ := statusreport.ReadDaemonHealth(r.health)
	if h.Interval != "150ms" || len(h.RuntimeTightened) != 0 || h.RuntimeChanged != "" {
		t.Errorf("[AC-183-1] daemon health = %+v, want interval 150ms and no tightening", h)
	}
	if !strings.Contains(buf.String(), "interval=150ms") || !strings.Contains(buf.String(), "alert-only") {
		t.Errorf("[AC-183-1] start line does not show the declared interval / mode:\n%s", buf.String())
	}
}

func TestRuntime_TighteningFlagOneShot(t *testing.T) {
	r := newRuntimeRig(t, "runtime:\n  mode: auto\n  max_changes_per_cycle: 20\n")
	out, code := r.run(t, "reconcile", "--intent", r.intent, "--state", r.state,
		"--mode", "alert-only", "--max-changes-per-cycle", "5", "--event-log", r.events)
	if code != 0 {
		t.Fatalf("[AC-183-2] reconcile exit=%d:\n%s", code, out)
	}
	if data := readStateFile(t, r.state); resourceInState(data, "web-1") {
		t.Errorf("[AC-183-2] --mode alert-only tightens auto and must win (nothing applied); state:\n%s", data)
	}
	var ev *core.Event
	for _, e := range readEvents(t, r.events) {
		if e.Kind == core.EventRuntimeTightened {
			e := e
			ev = &e
		}
	}
	if ev == nil {
		t.Fatalf("[AC-183-2] no RuntimeTightened Event:\n%s", out)
	}
	for k, want := range map[string]string{"mode.flag": "alert-only", "mode.intent": "auto",
		"max_changes_per_cycle.flag": "5", "max_changes_per_cycle.intent": "20"} {
		if ev.Attrs[k] != want {
			t.Errorf("[AC-183-2] attrs[%s] = %q, want %q (%+v)", k, ev.Attrs[k], want, ev.Attrs)
		}
	}
	if !strings.Contains(ev.Message, "--max-changes-per-cycle 5") || !strings.Contains(ev.Message, "runtime.max_changes_per_cycle 20") {
		t.Errorf("[AC-183-2] message lacks both values: %s", ev.Message)
	}
}

func TestRuntime_TighteningFlagDaemon(t *testing.T) {
	r := newRuntimeRig(t, "runtime:\n  interval: 100ms\n  max_changes_per_cycle: 20\n  max_destructive_per_cycle: 3\n")
	cmd := exec.Command(buildBinary(t), "--intent", r.intent, "--state", r.state,
		"--max-changes-per-cycle", "1", "--daemon-status", r.health, "--event-log", r.events)
	var buf syncBuffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if !waitForCondition(10*time.Second, func() bool {
		h, err := statusreport.ReadDaemonHealth(r.health)
		return err == nil && h.Cycle >= 3
	}) {
		t.Fatalf("[AC-183-3] daemon did not run:\n%s", buf.String())
	}
	h, _ := statusreport.ReadDaemonHealth(r.health)
	if len(h.RuntimeTightened) != 1 || h.RuntimeTightened[0].Key != "max_changes_per_cycle" ||
		h.RuntimeTightened[0].FlagValue != "1" || h.RuntimeTightened[0].IntentValue != "20" {
		t.Fatalf("[AC-183-3] daemon status runtime_tightened = %+v", h.RuntimeTightened)
	}
	// 締めは効く: 1 周に 1 つ（BudgetHalted が出る）。
	if !strings.Contains(buf.String(), "BUDGET HALTED") {
		t.Errorf("[AC-183-3] --max-changes-per-cycle 1 must hold the second create:\n%s", buf.String())
	}

	firing := eventsOf(readEvents(t, r.events), core.EventRuntimeTightened, core.EventFiring)
	if len(firing) != 1 {
		t.Fatalf("[AC-183-3] RuntimeTightened firing = %d, want 1 (level: once)", len(firing))
	}
	if a := firing[0].Attrs; a["max_changes_per_cycle.flag"] != "1" || a["max_changes_per_cycle.intent"] != "20" {
		t.Errorf("[AC-183-3] Event attrs = %+v, want both values", a)
	}

	stdout, stderr, code := runStatusCmd(t, r.intent, r.state, "--daemon-status", r.health)
	if code == 1 {
		t.Fatalf("[AC-183-3] status exit=%d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "--max-changes-per-cycle=1") || !strings.Contains(stdout, "runtime.max_changes_per_cycle=20") {
		t.Errorf("[AC-183-3] status does not show both values:\n%s", stdout)
	}
	jout, _, _ := runStatusCmd(t, r.intent, r.state, "--daemon-status", r.health, "--json")
	if !strings.Contains(jout, `"runtime_tightened"`) || !strings.Contains(jout, `"flag_value": "1"`) || !strings.Contains(jout, `"intent_value": "20"`) {
		t.Errorf("[AC-183-3] status --json lacks runtime_tightened with both values:\n%s", jout)
	}
}

func TestRuntime_LooseningFlagRefused(t *testing.T) {
	r := newRuntimeRig(t, "runtime:\n  mode: alert-only\n  max_changes_per_cycle: 20\n  max_ineffective_applies: 3\n")
	out, code := r.run(t, "--intent", r.intent, "--state", r.state, "--max-changes-per-cycle", "50")
	assertRefusalLine(t, "[AC-183-4] daemon", out, code, "max-changes-per-cycle", "max_changes_per_cycle", "50", "20")
	if !strings.Contains(out, "loosens") {
		t.Errorf("[AC-183-4] refusal does not say the flag loosens: %s", out)
	}
	out, code = r.run(t, "reconcile", "--intent", r.intent, "--state", r.state, "--max-changes-per-cycle", "0")
	assertRefusalLine(t, "[AC-183-4] reconcile 0", out, code, "max-changes-per-cycle", "max_changes_per_cycle", "0 (unlimited)", "20")
	out, code = r.run(t, "reconcile", "--intent", r.intent, "--state", r.state, "--mode", "auto")
	assertRefusalLine(t, "[AC-183-4] reconcile mode", out, code, "mode", "mode", "auto", "alert-only")
	out, code = r.run(t, "reconcile", "--intent", r.intent, "--state", r.state, "--max-ineffective-applies", "5")
	assertRefusalLine(t, "[AC-183-4] reconcile max-ineffective-applies", out, code, "max-ineffective-applies", "max_ineffective_applies", "5", "3")
	out, code = r.run(t, "plan", "--intent", r.intent, "--state", r.state, "--max-changes-per-cycle", "21")
	assertRefusalLine(t, "[AC-183-4] plan", out, code, "max-changes-per-cycle", "max_changes_per_cycle", "21", "20")
	if data := readStateFile(t, r.state); resourceInState(data, "web-1") {
		t.Errorf("[AC-183-4] a refused start must not Apply; state:\n%s", data)
	}
}

func TestRuntime_NoDirectionMismatchRefused(t *testing.T) {
	r := newRuntimeRig(t, "runtime:\n  interval: 30s\n  settle: 2s\n  mode: alert-only\n")
	out, code := r.run(t, "--intent", r.intent, "--state", r.state, "--interval", "5s")
	assertRefusalLine(t, "[AC-183-5] daemon interval", out, code, "interval", "interval", "5s", "30s")
	if !strings.Contains(out, "differs from") {
		t.Errorf("[AC-183-5] refusal does not say the values differ: %s", out)
	}
	out, code = r.run(t, "reconcile", "--intent", r.intent, "--state", r.state, "--settle", "1s")
	assertRefusalLine(t, "[AC-183-5] reconcile settle (shorter)", out, code, "settle", "settle", "1s", "2s")
	out, code = r.run(t, "reconcile", "--intent", r.intent, "--state", r.state, "--settle", "1m")
	assertRefusalLine(t, "[AC-183-5] reconcile settle (longer)", out, code, "settle", "settle", "1m0s", "2s")
	// 同じ値（書き方が違っても）は通る。
	if out, code := r.run(t, "reconcile", "--intent", r.intent, "--state", r.state, "--settle", "2000ms"); code != 0 {
		t.Errorf("[AC-183-5] the same settle must be accepted (exit %d):\n%s", code, out)
	}
}

func TestRuntime_LoadRefusals(t *testing.T) {
	r := newRuntimeRig(t, "runtime:\n  interval: 30s\n")
	if err := os.WriteFile(filepath.Join(r.intent, "01-more.yaml"), []byte("runtime:\n  mode: auto\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, code := r.run(t, "plan", "--intent", r.intent, "--state", r.state); code != 1 || !strings.Contains(out, "runtime: declared in both 00-runtime.yaml and 01-more.yaml") {
		t.Errorf("[AC-183-6] runtime: in two files: exit %d:\n%s", code, out)
	}
	_ = os.Remove(filepath.Join(r.intent, "01-more.yaml"))
	for _, c := range []struct{ yaml, want string }{
		{"runtime:\n  intervl: 30s\n", "intervl"},
		{"runtime:\n  max_changes_per_cycle: -1\n", "max_changes_per_cycle"},
		{"runtime:\n  mode: sometimes\n", "mode"},
		{"runtime:\n  interval: 0s\n", "interval"},
	} {
		r.writeRuntime(t, c.yaml)
		if out, code := r.run(t, "plan", "--intent", r.intent, "--state", r.state); code != 1 || !strings.Contains(out, c.want) {
			t.Errorf("[AC-183-6] %q must be refused naming %s: exit %d:\n%s", c.yaml, c.want, code, out)
		}
	}
}

func TestRuntime_DeclarationChangedWhileRunning(t *testing.T) {
	r := newRuntimeRig(t, "runtime:\n  interval: 100ms\n  max_changes_per_cycle: 20\n")
	cmd := exec.Command(buildBinary(t), "--intent", r.intent, "--state", r.state,
		"--daemon-status", r.health, "--event-log", r.events)
	var buf syncBuffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	health := func() statusreport.DaemonHealth {
		h, _ := statusreport.ReadDaemonHealth(r.health)
		return h
	}
	if !waitForCondition(10*time.Second, func() bool { return health().Cycle >= 2 }) {
		t.Fatalf("[AC-183-7] daemon did not run:\n%s", buf.String())
	}
	r.writeRuntime(t, "runtime:\n  interval: 100ms\n  max_changes_per_cycle: 5\n")
	if !waitForCondition(10*time.Second, func() bool { return health().RuntimeChanged != "" }) {
		t.Fatalf("[AC-183-7] runtime_changed not reported:\n%s", buf.String())
	}
	if h := health(); !strings.Contains(h.RuntimeChanged, "max_changes_per_cycle: started with 20, now 5") {
		t.Errorf("[AC-183-7] runtime_changed = %q", h.RuntimeChanged)
	}
	firing := eventsOf(readEvents(t, r.events), core.EventRuntimeDeclarationChanged, core.EventFiring)
	if len(firing) == 0 || firing[0].Attrs["max_changes_per_cycle.started"] != "20" || firing[0].Attrs["max_changes_per_cycle.intent"] != "5" {
		t.Fatalf("[AC-183-7] RuntimeDeclarationChanged firing = %+v", firing)
	}
	r.writeRuntime(t, "runtime:\n  interval: 100ms\n  max_changes_per_cycle: 20\n")
	if !waitForCondition(10*time.Second, func() bool {
		return len(eventsOf(readEvents(t, r.events), core.EventRuntimeDeclarationChanged, core.EventResolved)) > 0
	}) {
		t.Fatalf("[AC-183-7] RuntimeDeclarationChanged not resolved after the declaration came back:\n%s", buf.String())
	}
	if !waitForCondition(5*time.Second, func() bool { return health().RuntimeChanged == "" }) {
		t.Errorf("[AC-183-7] runtime_changed not cleared: %q", health().RuntimeChanged)
	}
}
