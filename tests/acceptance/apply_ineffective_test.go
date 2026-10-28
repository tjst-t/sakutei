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

// apply_ineffective_test.go — #166: Apply が成功を返しても現実が変わらないとき
// （BMC が電源 On に 204 を返して電源を変えない）、同じ Apply を繰り返して成功と
// して終わっていた。Apply のあとに読み直して確かめ（§11.1 Tier2）、同じ Diff で
// 効かないまま上限に達したらそのリソースへの Apply を止める（§11.1 Tier3・§15.3、
// oscillating）。
//
//	[AC-166-1]  一回性の reconcile: Apply は成功を返すが現実が変わらない → 終了
//	            コード 4。要約は「not converged」と言い、applied と言い切らない。
//	            ApplyIneffective の Event が出る。
//	[AC-166-2]  一回性でも --apply-check-state で回数を持ち越し、上限で Apply を
//	            止める（Apply せずに 4）。apply-check list が STOPPED と言い、
//	            apply-check release（S4 監査に記録）で解くと次の回は Apply する。
//	[AC-166-3]  ふつうに効く Apply は今どおり 0、観測できない・skip は今どおり 1
//	            （効かない Apply と同時なら 1 が勝つ）。
//	[AC-166-4]  常駐: 上限に達したら Apply が止まり、status（GET /status の
//	            oscillating・--daemon-status の apply_stopped）・/metrics
//	            （sakutei_resources{state="oscillating"}・
//	            sakutei_apply_ineffective_total）に出て、ApplyIneffective が
//	            firing（stuck・critical）。現実が直ると resolved で、次のずれは
//	            また Apply する。
//	[AC-166-5]  常駐: desired を変えると解け、Apply が戻る。
//	[AC-166-6]  常駐: 人が apply-check release で解くと次の周に Apply が戻る。
//	[AC-166-7]  偽の Redfish（電源の Reset に 204 を返すが電源を変えない）で、常駐
//	            が上限で Reset を止め、電源が直ると resolved。
//	[AC-166-8]  偽の Redfish で一回性の reconcile が終了コード 4。intent の
//	            apply_check: の settle が使われる（旗とは厳しい方、§14.3）。
package acceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/fakeprovider"
	"github.com/tjst-t/sakutei/v2/internal/metrics"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/internal/redfish/redfishtest"
	"github.com/tjst-t/sakutei/v2/internal/redfish/system"
	"github.com/tjst-t/sakutei/v2/internal/statusreport"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// seedFakeIgnore writes the fake provider's reality atomically: resources,
// provider_version v1.0.0, and the resources whose Apply reports success
// without changing anything (#166).
func seedFakeIgnore(t *testing.T, statePath string, resources map[string]map[string]any, ignore map[string]bool) {
	t.Helper()
	seedFake(t, statePath, resources, ignore, nil)
}

// seedFake is seedFakeIgnore plus the resources observed unknown.
func seedFake(t *testing.T, statePath string, resources map[string]map[string]any, ignore map[string]bool, unknown map[string]string) {
	t.Helper()
	m := make(map[string]any, len(resources))
	for id, fields := range resources {
		m[id] = map[string]any{"fields": fields}
	}
	data, err := json.MarshalIndent(map[string]any{
		"resources": m, "provider_version": "v1.0.0", "apply_ignore": ignore, "observe_unknown": unknown,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(statePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func applyCount(t *testing.T, statePath, local string) int {
	t.Helper()
	counts, err := fakeprovider.New(statePath).GetApplyCount()
	if err != nil {
		t.Fatal(err)
	}
	return counts[local]
}

// oneShotRig is an intent with db-vm (size 10) against the fake provider.
type oneShotRig struct {
	intent, state, applyState, audit string
}

func newOneShotRig(t *testing.T, extra ...map[string]any) *oneShotRig {
	t.Helper()
	tmp := t.TempDir()
	r := &oneShotRig{
		intent: filepath.Join(tmp, "intent"), state: filepath.Join(tmp, "state.json"),
		applyState: filepath.Join(tmp, "apply-check.json"), audit: filepath.Join(tmp, "audit.jsonl"),
	}
	if err := os.MkdirAll(r.intent, 0o755); err != nil {
		t.Fatal(err)
	}
	res := []map[string]any{{"provider": "fake", "id": "db-vm", "fields": map[string]string{"name": "db-vm", "size": "10"}}}
	writeIntentFile(t, r.intent, map[string]any{"resources": append(res, extra...)})
	return r
}

func (r *oneShotRig) reconcile(t *testing.T, args ...string) (string, int) {
	t.Helper()
	return runSakutei(t, append([]string{"reconcile", "--intent", r.intent, "--state", r.state}, args...)...)
}

func TestApplyIneffective_OneShotExit4(t *testing.T) {
	// [AC-166-1]
	r := newOneShotRig(t)
	seedFakeIgnore(t, r.state, map[string]map[string]any{"db-vm": {"name": "db-vm", "size": "30"}}, map[string]bool{"db-vm": true})
	out, code := r.reconcile(t)
	if code != 4 {
		t.Fatalf("[AC-166-1] exit code = %d, want 4:\n%s", code, out)
	}
	if !strings.Contains(out, "Reconcile: not converged: 1 change(s) reported applied, but 1 resource(s) still differ from desired after the settle") {
		t.Errorf("[AC-166-1] the summary must say it did not converge:\n%s", out)
	}
	if strings.Contains(out, "Reconcile: 1 change(s) applied") || strings.Contains(out, "Converged") {
		t.Errorf("[AC-166-1] the summary must not say applied / converged:\n%s", out)
	}
	if !strings.Contains(out, "ApplyIneffective: fake//db-vm: Apply reported success but reality did not change") ||
		!strings.Contains(out, `size: "30" → "10"`) {
		t.Errorf("[AC-166-1] no ApplyIneffective notice with the diff:\n%s", out)
	}
	// JSON: the Event is ApplyIneffective with the resource.
	out, code = r.reconcile(t, "--log-format", "json")
	if code != 4 || !strings.Contains(out, `"kind":"ApplyIneffective"`) || !strings.Contains(out, `"resource":"fake//db-vm"`) {
		t.Errorf("[AC-166-1] --log-format json: exit %d, want 4 and an ApplyIneffective Event:\n%s", code, out)
	}
}

func TestApplyIneffective_OneShotStopsAcrossRuns(t *testing.T) {
	// [AC-166-2]
	r := newOneShotRig(t)
	seedFakeIgnore(t, r.state, map[string]map[string]any{"db-vm": {"name": "db-vm", "size": "30"}}, map[string]bool{"db-vm": true})
	args := []string{"--apply-check-state", r.applyState, "--max-ineffective-applies", "2"}
	for i := 1; i <= 2; i++ {
		out, code := r.reconcile(t, args...)
		if code != 4 {
			t.Fatalf("[AC-166-2] run %d: exit %d, want 4:\n%s", i, code, out)
		}
		if i == 2 && !strings.Contains(out, "ApplyIneffective: fake//db-vm: STOPPED") {
			t.Fatalf("[AC-166-2] run 2 reaches the limit; no STOPPED notice:\n%s", out)
		}
	}
	if n := applyCount(t, r.state, "db-vm"); n != 2 {
		t.Fatalf("[AC-166-2] apply_count after 2 runs = %d, want 2", n)
	}
	out, code := r.reconcile(t, args...)
	if code != 4 || !strings.Contains(out, "1 resource(s) not applied: Apply stopped (oscillating") {
		t.Fatalf("[AC-166-2] run 3: exit %d, want 4 and the stopped summary:\n%s", code, out)
	}
	if n := applyCount(t, r.state, "db-vm"); n != 2 {
		t.Fatalf("[AC-166-2] the stopped resource was applied again: apply_count = %d", n)
	}
	out, code = runSakutei(t, "apply-check", "list", "--state", r.applyState)
	if code != 0 || !strings.Contains(out, "fake//db-vm: STOPPED") || !strings.Contains(out, `size: "30" → "10"`) {
		t.Fatalf("[AC-166-2] apply-check list: exit %d:\n%s", code, out)
	}
	out, code = runSakutei(t, "apply-check", "list", "--state", r.applyState, "--json")
	var st reconcile.ApplyCheckState
	if code != 0 || json.Unmarshal([]byte(out), &st) != nil || len(st.Resources) != 1 || !st.Resources[0].Stopped ||
		st.Resources[0].Reason != reconcile.StopMaxApplies || st.Resources[0].IneffectiveApplies != 2 {
		t.Fatalf("[AC-166-2] apply-check list --json: exit %d:\n%s", code, out)
	}
	// Release needs the audit log and the approver, like task approve.
	if _, code := runSakutei(t, "apply-check", "release", "--state", r.applyState, "db-vm"); code != 1 {
		t.Fatalf("[AC-166-2] release without --audit-log / --approver: exit %d, want 1", code)
	}
	out, code = runSakutei(t, "apply-check", "release", "--state", r.applyState, "--audit-log", r.audit, "--approver", "alice", "db-vm")
	if code != 0 {
		t.Fatalf("[AC-166-2] release: exit %d:\n%s", code, out)
	}
	if a := readFile(t, r.audit); !strings.Contains(a, `"kind":"apply_check.released"`) || !strings.Contains(a, `"actor":"alice"`) {
		t.Fatalf("[AC-166-2] the release is not in the S4 audit log:\n%s", a)
	}
	out, code = r.reconcile(t, args...)
	if code != 4 || applyCount(t, r.state, "db-vm") != 3 {
		t.Fatalf("[AC-166-2] after release: exit %d, apply_count %d; want 4 and the Apply sent again:\n%s",
			code, applyCount(t, r.state, "db-vm"), out)
	}
}

func TestApplyIneffective_OneShotExitCodesUnchanged(t *testing.T) {
	// [AC-166-3]
	r := newOneShotRig(t)
	seedFakeIgnore(t, r.state, map[string]map[string]any{"db-vm": {"name": "db-vm", "size": "30"}}, nil)
	out, code := r.reconcile(t)
	if code != 0 || !strings.Contains(out, "Reconcile: 1 change(s) applied") {
		t.Fatalf("[AC-166-3] an Apply that takes: exit %d, want 0 and applied:\n%s", code, out)
	}
	out, code = r.reconcile(t)
	if code != 0 || !strings.Contains(out, "Converged (no changes)") {
		t.Fatalf("[AC-166-3] converged: exit %d:\n%s", code, out)
	}

	// An unobservable resource together with an Apply that does not take: 1 wins.
	r2 := newOneShotRig(t, map[string]any{"provider": "fake", "id": "web-vm", "fields": map[string]string{"name": "web-vm", "size": "1"}})
	seedFake(t, r2.state, map[string]map[string]any{"db-vm": {"name": "db-vm", "size": "30"}}, map[string]bool{"db-vm": true},
		map[string]string{"web-vm": "error"})
	out, code = r2.reconcile(t)
	if code != 1 {
		t.Fatalf("[AC-166-3] unobservable + ineffective: exit %d, want 1:\n%s", code, out)
	}
	if !strings.Contains(out, "ApplyIneffective") {
		t.Errorf("[AC-166-3] the ineffective Apply must still be reported:\n%s", out)
	}
}

// aiDaemon is a daemon (reconcile.Run) against the fake provider whose db-vm
// Apply does not take, with the status, metrics and Events wired.
type aiDaemon struct {
	*sinkRig
	rec    *recordingSink
	views  viewRecorder
	e      *metrics.Exporter
	health string
	state  string
}

func startAIDaemon(t *testing.T, cfg reconcile.Config) *aiDaemon {
	t.Helper()
	r := newSinkRig(t)
	r.seedDrift() // db-vm desired 10, reality 30
	seedFakeIgnore(t, r.statePath, map[string]map[string]any{"db-vm": {"name": "db-vm", "size": "30"}}, map[string]bool{"db-vm": true})
	d := &aiDaemon{sinkRig: r, rec: &recordingSink{}, views: viewRecorder{ch: make(chan reconcile.CycleView, 1)},
		e: metrics.NewExporter(metrics.BudgetLimits{}), health: filepath.Join(t.TempDir(), "daemon-status.json")}
	max := 2
	cfg.ApplyCheck.MaxApplies = &max
	cfg.StuckAfter = 100
	cfg.EventSinks = append(cfg.EventSinks, d.rec)
	cfg.View = d.views
	cfg.Metrics = d.e
	cfg.DaemonStatusPath = d.health
	r.start(cfg)
	return d
}

// lastView is a CycleView that observed, recorded after the call (the
// recorder keeps one and drops the rest, so a stale one is drained first).
func (d *aiDaemon) lastView() reconcile.CycleView {
	d.t.Helper()
	select {
	case <-d.views.ch:
	default:
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case v := <-d.views.ch:
			if v.Observed {
				return v
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
	d.t.Fatalf("no CycleView")
	return reconcile.CycleView{}
}

func (d *aiDaemon) stoppedFiring() []core.Event {
	var out []core.Event
	for _, ev := range d.rec.firingOf(core.EventApplyIneffective) {
		if ev.Attrs["stopped"] == "true" {
			out = append(out, ev)
		}
	}
	return out
}

func (d *aiDaemon) metric(key string) float64 {
	d.t.Helper()
	var b strings.Builder
	if err := metrics.Write(&b, d.e.Families()); err != nil {
		d.t.Fatal(err)
	}
	ps, err := metrics.Parse(strings.NewReader(b.String()))
	if err != nil {
		d.t.Fatal(err)
	}
	for _, p := range ps {
		if p.Key() == key {
			return p.Value
		}
	}
	return -1
}

func TestApplyIneffective_DaemonStopsReportsAndResolves(t *testing.T) {
	// [AC-166-4]
	d := startAIDaemon(t, reconcile.Config{})
	d.waitFor("the stop of db-vm", func() bool { return len(d.stoppedFiring()) > 0 })
	ev := d.stoppedFiring()[0]
	if ev.Resource.Local != "db-vm" || ev.Class != core.EventStuck || ev.Severity != core.SeverityCritical ||
		ev.Attrs["reason"] != reconcile.StopMaxApplies || ev.Attrs["ineffective_applies"] != "2" {
		t.Fatalf("[AC-166-4] stopped firing = %+v, want db-vm, stuck, critical, max_ineffective_applies after 2", ev)
	}
	// No more Applies while stopped.
	d.waitCycle(d.lastCycle() + 3)
	if n := applyCount(t, d.statePath, "db-vm"); n != 2 {
		t.Fatalf("[AC-166-4] apply_count = %d after the stop, want 2 (no Apply while stopped)", n)
	}
	if !strings.Contains(d.errOut.String(), "Apply stopped (oscillating") {
		t.Errorf("[AC-166-4] the cycle summary must say the Apply is stopped:\n%s", d.errOut.String())
	}

	// GET /status: db-vm is oscillating.
	v := d.lastView()
	if v.Status == nil || len(v.Status.Oscillating) != 1 || v.Status.Oscillating[0].ID.Local != "db-vm" {
		t.Fatalf("[AC-166-4] status Oscillating = %+v", v.Status)
	}
	js := v.Status.ToJSON(time.Now())
	found := false
	for _, res := range js.Resources {
		if res.LocalID == "db-vm" && res.State == statusreport.StateOscillating && res.ApplyStopped != nil &&
			res.ApplyStopped.IneffectiveApplies == 2 {
			found = true
		}
	}
	if !found || js.Summary.Oscillating != 1 {
		t.Fatalf("[AC-166-4] status JSON has no oscillating db-vm: %+v", js)
	}
	if !strings.Contains(v.Status.String(), "OSCILLATING fake//db-vm") {
		t.Errorf("[AC-166-4] status text:\n%s", v.Status.String())
	}
	if len(v.Health.ApplyStopped) != 1 {
		t.Errorf("[AC-166-4] /status health apply_stopped = %+v", v.Health.ApplyStopped)
	}
	h, err := statusreport.ReadDaemonHealth(d.health)
	if err != nil || len(h.ApplyStopped) != 1 || h.ApplyStopped[0].Resource != "fake//db-vm" {
		t.Fatalf("[AC-166-4] --daemon-status apply_stopped = %+v (%v)", h.ApplyStopped, err)
	}
	if v.Plan == nil || len(v.Plan.Operations) != 1 || v.Plan.Operations[0].BlockedBy != "apply_stopped" {
		t.Errorf("[AC-166-4] GET /plan: the op must be blocked_by apply_stopped: %+v", v.Plan)
	}
	// /metrics.
	if got := d.metric(`sakutei_resources{state="oscillating"}`); got != 1 {
		t.Errorf("[AC-166-4] sakutei_resources{state=oscillating} = %v, want 1", got)
	}
	if got := d.metric("sakutei_apply_ineffective_total"); got < 2 {
		t.Errorf("[AC-166-4] sakutei_apply_ineffective_total = %v, want >= 2", got)
	}

	// Reality is fixed by hand: resolved, no longer oscillating.
	seedFakeIgnore(t, d.statePath, map[string]map[string]any{"db-vm": {"name": "db-vm", "size": "10"}}, nil)
	d.waitFor("resolved ApplyIneffective", func() bool { return len(d.rec.resolvedOf(core.EventApplyIneffective)) > 0 })
	d.waitCycle(d.lastCycle() + 2)
	if got := d.metric(`sakutei_resources{state="oscillating"}`); got != 0 {
		t.Errorf("[AC-166-4] after the fix sakutei_resources{state=oscillating} = %v, want 0", got)
	}
	// The next drift is applied again (and takes).
	seedFakeIgnore(t, d.statePath, map[string]map[string]any{"db-vm": {"name": "db-vm", "size": "30"}}, nil)
	d.waitFor("db-vm applied again", func() bool {
		b, _ := os.ReadFile(d.statePath)
		return applyCount(t, d.statePath, "db-vm") >= 1 && strings.Contains(string(b), `"size": "10"`)
	})
	d.stop()
	if n := len(d.stoppedFiring()); n != 1 {
		t.Errorf("[AC-166-4] the stop fired %d time(s), want 1 (level)", n)
	}
}

func TestApplyIneffective_DaemonDesiredChangeReleases(t *testing.T) {
	// [AC-166-5]
	d := startAIDaemon(t, reconcile.Config{})
	d.waitFor("the stop of db-vm", func() bool { return len(d.stoppedFiring()) > 0 })
	writeIntentFile(t, d.intentDir, map[string]any{"resources": []map[string]any{
		{"provider": "fake", "id": "db-vm", "fields": map[string]string{"name": "db-vm", "size": "20"}},
	}})
	d.waitFor("the Apply resumes", func() bool {
		return strings.Contains(d.out.String(), "sakutei: apply-check: fake//db-vm: the diff changed") &&
			applyCount(t, d.statePath, "db-vm") > 2
	})
}

func TestApplyIneffective_DaemonHumanRelease(t *testing.T) {
	// [AC-166-6]
	statePath := filepath.Join(t.TempDir(), "apply-check.json")
	d := startAIDaemon(t, reconcile.Config{ApplyCheckState: statePath})
	d.waitFor("the stop recorded in the state file", func() bool {
		st, err := reconcile.ReadApplyCheckState(statePath)
		return err == nil && len(st.Resources) == 1 && st.Resources[0].Stopped
	})
	audit := filepath.Join(t.TempDir(), "audit.jsonl")
	if out, code := runSakutei(t, "apply-check", "release", "--state", statePath, "--audit-log", audit, "--approver", "bob", "fake//db-vm"); code != 0 {
		t.Fatalf("[AC-166-6] release: exit %d:\n%s", code, out)
	}
	d.waitFor("the Apply resumes after the release", func() bool {
		return strings.Contains(d.out.String(), "sakutei: apply-check: fake//db-vm released by a person") &&
			applyCount(t, d.statePath, "db-vm") > 2
	})
}

func TestApplyIneffective_RedfishDaemon(t *testing.T) {
	// [AC-166-7]
	settle := 20 * time.Millisecond
	max := 2
	r := newRFRig(t, []string{"node1"}, redfishtest.Options{PowerState: "Off"},
		rfResource("node1", `"power": "On"`), reconcile.Config{ApplyCheck: reconcile.ApplyCheckFlags{Settle: &settle, MaxApplies: &max}})
	r.bmcs["node1"].SetIgnorePowerActions(true) // 204, but the power stays Off
	for i := 1; i <= 2; i++ {
		r.mustCycle()
		if w := r.writes("node1"); len(w) != 1 || !strings.Contains(w[0], `"ResetType":"On"`) {
			t.Fatalf("[AC-166-7] cycle %d writes = %v, want Reset On", i, w)
		}
	}
	for i := 0; i < 3; i++ {
		res := r.mustCycle()
		if w := r.writes("node1"); len(w) != 0 {
			t.Fatalf("[AC-166-7] stopped, but the BMC got %v", w)
		}
		if len(res.Oscillating) != 1 || res.Oscillating[0] != "node1" {
			t.Fatalf("[AC-166-7] Oscillating = %v, want [node1]", res.Oscillating)
		}
	}
	var stopped []core.Event
	for _, ev := range r.rec.firingOf(core.EventApplyIneffective) {
		if ev.Attrs["stopped"] == "true" {
			stopped = append(stopped, ev)
		}
	}
	if len(stopped) != 1 || stopped[0].Resource.Local != "node1" || !strings.Contains(stopped[0].Message, `power: "Off" → "On"`) {
		t.Fatalf("[AC-166-7] stopped firing = %+v", stopped)
	}
	// The power is turned on by hand: resolved.
	r.bmcs["node1"].SetIgnorePowerActions(false)
	r.bmcs["node1"].SetPowerState("On")
	if res := r.mustCycle(); len(res.Oscillating) != 0 {
		t.Fatalf("[AC-166-7] still oscillating after the power is on: %v", res.Oscillating)
	}
	if len(r.rec.resolvedOf(core.EventApplyIneffective)) != 1 {
		t.Fatalf("[AC-166-7] no resolved ApplyIneffective: %+v", r.rec.ofKind(core.EventApplyIneffective))
	}
}

func TestApplyIneffective_RedfishOneShot(t *testing.T) {
	// [AC-166-8]
	r := newRFCLIRig(t)
	r.bmc.SetPowerState("Off")
	r.bmc.SetIgnorePowerActions(true)
	writeIntentFile(t, r.intent, map[string]any{
		"apply_check": []map[string]any{{"provider": system.Name, "settle": "300ms"}},
		"resources": []map[string]any{
			{"provider": system.Name, "id": "node1", "fields": map[string]string{"power": "On"}},
		},
	})
	start := time.Now()
	// The flag's 100ms is shorter than the intent's 300ms: the longer wins.
	out, code := runSakutei(t, "reconcile", "--intent", r.intent, "--state", r.state, "--redfish-config", r.config, "--settle", "100ms")
	if code != 4 {
		t.Fatalf("[AC-166-8] exit %d, want 4:\n%s", code, out)
	}
	if !strings.Contains(out, "settle (300ms)") || time.Since(start) < 300*time.Millisecond {
		t.Errorf("[AC-166-8] the intent's settle (300ms) must be used:\n%s", out)
	}
	if !strings.Contains(out, `power: "Off" → "On"`) || !strings.Contains(out, "Reconcile: not converged") {
		t.Errorf("[AC-166-8] summary / notice:\n%s", out)
	}
	if r.bmc.PowerState() != "Off" {
		t.Fatalf("[AC-166-8] power %s", r.bmc.PowerState())
	}
	// The BMC works again: the same reconcile converges with 0.
	r.bmc.SetIgnorePowerActions(false)
	if out, code := runSakutei(t, "reconcile", "--intent", r.intent, "--state", r.state, "--redfish-config", r.config); code != 0 {
		t.Fatalf("[AC-166-8] working BMC: exit %d:\n%s", code, out)
	}
}
