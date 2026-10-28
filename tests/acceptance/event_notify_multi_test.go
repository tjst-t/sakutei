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

// event_notify_multi_test.go — #154: 通知の口の宛先を複数にする（Alertmanager を
// 複数台で動かす構成。送る側がすべての Alertmanager に同じ alert を送る）。
//
//	[AC-154-1]  2 台の Alertmanager の両方に、同じ firing の alert（同じ labels・
//	            startsAt）が届き、送り直しも両方に続く。
//	[AC-154-2]  1 台を止めても（503）もう 1 台には届き続け（resolved も届く）、
//	            止めた方の失敗は宛先ごとの状態（status の notify・/metrics）に出る。
//	            戻れば、止めた方にも待っていた resolved と firing の送り直しが届く。
//	[AC-154-3]  CLI の `--event-alertmanager a,b`（"," でつなぐ）とフラグの繰り返し
//	            のどちらでも、一回性の reconcile が両方に送り切る。
package acceptance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/budget"
	"github.com/tjst-t/sakutei/v2/internal/eventnotify"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/internal/statusreport"
)

// twoAlertmanagers opens the notification sinks for two Alertmanagers the way
// the CLI does (eventnotify.Open with the --event-alertmanager values).
func twoAlertmanagers(t *testing.T, am1, am2 *notifyReceiver, r *sinkRig) *eventnotify.Set {
	t.Helper()
	opts := eventnotify.Options{ErrOut: &r.errOut, MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond}
	cfg := eventnotify.Config{Alertmanager: eventnotify.Target{ResendInterval: 100 * time.Millisecond}}
	set, err := eventnotify.Open(cfg, nil, eventnotify.SplitURLs([]string{am1.start(t) + "," + am2.start(t)}), opts)
	if err != nil {
		t.Fatal(err)
	}
	if set.Len() != 2 {
		t.Fatalf("Open: %d sinks, want 2", set.Len())
	}
	t.Cleanup(func() { _ = set.Close(context.Background()) })
	return set
}

func TestEventNotifyMulti_BothAlertmanagersGetTheSameAlerts(t *testing.T) {
	// [AC-154-1]
	am1, am2 := &notifyReceiver{}, &notifyReceiver{}
	r := newSinkRig(t)
	r.seedOrphans() // the budget halts and stays halted: BudgetHalted keeps firing
	set := twoAlertmanagers(t, am1, am2, r)
	r.start(reconcile.Config{BudgetConfig: budget.Config{MaxDestructivePerCycle: 1}, StuckAfter: 1000, EventSinks: set.Sinks()})
	r.waitFor("BudgetHalted resent to both Alertmanagers", func() bool {
		f1, _ := am1.alertsOf(t, "BudgetHalted")
		f2, _ := am2.alertsOf(t, "BudgetHalted")
		return len(f1) >= 3 && len(f2) >= 3
	})
	r.stop()
	f1, _ := am1.alertsOf(t, "BudgetHalted")
	f2, _ := am2.alertsOf(t, "BudgetHalted")
	if !sameAlertIdentity(f1[0], f2[0]) {
		t.Errorf("[AC-154-1] the Alertmanagers got different alerts:\n%+v\n%+v", f1[0], f2[0])
	}
}

func sameAlertIdentity(a, b eventnotify.Alert) bool {
	if len(a.Labels) != len(b.Labels) || !a.StartsAt.Equal(b.StartsAt) {
		return false
	}
	for k, v := range a.Labels {
		if b.Labels[k] != v {
			return false
		}
	}
	return true
}

func TestEventNotifyMulti_OneDownTheOtherStillGetsThem(t *testing.T) {
	// [AC-154-2]
	am1, am2 := &notifyReceiver{}, &notifyReceiver{}
	am2.down.Store(true)
	r := newSinkRig(t)
	r.seedDrift()
	set := twoAlertmanagers(t, am1, am2, r)
	statusPath := t.TempDir() + "/daemon-status.json"
	r.start(reconcile.Config{EventSinks: set.Sinks(), NotifyStatus: set.Status, DaemonStatusPath: statusPath})
	r.waitFor("Drift firing and resolved at the Alertmanager that is up", func() bool {
		f, res := am1.alertsOf(t, "Drift")
		return len(f) > 0 && len(res) > 0
	})
	r.waitFor("the down Alertmanager's failures in the daemon status", func() bool {
		h, err := statusreport.ReadDaemonHealth(statusPath)
		return err == nil && len(h.Notify) == 2 && h.Notify[1].Down() && h.Notify[0].Delivered > 0
	})
	if got := am2.alerts(t); len(got) != 0 {
		t.Fatalf("[AC-154-2] the down Alertmanager stored alerts: %+v", got)
	}
	h, _ := statusreport.ReadDaemonHealth(statusPath)
	if h.Notify[0].Sink != "alertmanager" || h.Notify[0].Down() || h.Notify[1].Delivered != 0 || !strings.Contains(h.Notify[1].LastError, "503") {
		t.Errorf("[AC-154-2] notify status = %+v", h.Notify)
	}
	text := statusreport.Report{Daemon: &h}.String()
	if !strings.Contains(text, "notify: alertmanager "+h.Notify[1].URL) || !strings.Contains(text, "に届いていない") {
		t.Errorf("[AC-154-2] status text does not report the down destination:\n%s", text)
	}

	// Back up: the stopped one gets the resolved it missed.
	am2.down.Store(false)
	r.waitFor("Drift resolved at the recovered Alertmanager", func() bool {
		_, res := am2.alertsOf(t, "Drift")
		return len(res) > 0
	})
	r.waitFor("both up in the daemon status", func() bool {
		h, err := statusreport.ReadDaemonHealth(statusPath)
		return err == nil && len(h.Notify) == 2 && !h.Notify[0].Down() && !h.Notify[1].Down() && h.Notify[1].Delivered > 0
	})
	r.stop()
}

func TestEventNotifyMulti_CLIFlagForms(t *testing.T) {
	// [AC-154-3]
	for _, form := range []string{"comma", "repeat"} {
		t.Run(form, func(t *testing.T) {
			am1, am2 := &notifyReceiver{}, &notifyReceiver{}
			u1, u2 := am1.start(t), am2.start(t)
			r := newSinkRig(t)
			r.seedDrift()
			args := []string{"reconcile", "--intent", r.intentDir, "--state", r.statePath, "--mode", "alert-only"}
			if form == "comma" {
				args = append(args, "--event-alertmanager", u1+","+u2)
			} else {
				args = append(args, "--event-alertmanager", u1, "--event-alertmanager", u2)
			}
			out, code := runSakutei(t, args...)
			if code != 0 {
				t.Fatalf("[AC-154-3] reconcile exit %d:\n%s", code, out)
			}
			if len(am1.alerts(t)) == 0 || len(am2.alerts(t)) == 0 {
				t.Errorf("[AC-154-3] alerts: %d and %d, want both; output:\n%s", len(am1.alerts(t)), len(am2.alerts(t)), out)
			}
		})
	}
}
