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

// event_notify_test.go — #131: 通知の口（§22.3: Event を webhook と
// Alertmanager に送る sink）。送り先は httptest。
//
//	[AC-131-1]  デーモンのずれ（Drift）の level の Event が、webhook には JSON
//	            （state: firing・resolved）で、Alertmanager には alert（labels
//	            kind・resource・severity・class、startsAt = since、resolved は
//	            endsAt = 消えた時刻）で届く。webhook の認証のヘッダは SecretRef
//	            から解決し、秘密の値は Event にもログにも出ない。
//	[AC-131-2]  halt（BudgetHalted）も firing と resolved で両方に届く。
//	            Alertmanager には firing の間、送り直しが続く。
//	[AC-131-3]  送り先が落ちていても（503）デーモンは収束を続け（ずれを直し、次の周は
//	            converged）、送れなかったことをログに出す。送り先が戻ったら、
//	            待っていた firing・resolved が届く。
//	[AC-131-4]  一回性の `sakutei reconcile --event-webhook --event-alertmanager`
//	            は、終わる前に送り待ちを送り切る。
package acceptance

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/budget"
	"github.com/tjst-t/sakutei/v2/internal/eventlog"
	"github.com/tjst-t/sakutei/v2/internal/eventnotify"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

const notifySecret = "Bearer n0tify-s3cret"

// notifyReceiver is an httptest receiver for both the webhook (one Event per
// body) and Alertmanager (an array of alerts per body).  down answers 503.
type notifyReceiver struct {
	mu     sync.Mutex
	bodies [][]byte
	auth   []string
	down   atomic.Bool
}

func (r *notifyReceiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	if r.down.Load() {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	r.mu.Lock()
	r.bodies = append(r.bodies, b)
	r.auth = append(r.auth, req.Header.Get("Authorization"))
	r.mu.Unlock()
}

func (r *notifyReceiver) start(t *testing.T) string {
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv.URL
}

type webhookEvent struct {
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Resource string `json:"resource"`
	State    string `json:"state"`
	Class    string `json:"class"`
	Since    string `json:"since"`
	Message  string `json:"message"`
}

func (r *notifyReceiver) events(t *testing.T) []webhookEvent {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []webhookEvent
	for _, b := range r.bodies {
		var ev webhookEvent
		if err := json.Unmarshal(b, &ev); err != nil {
			t.Fatalf("webhook body %q: %v", b, err)
		}
		out = append(out, ev)
	}
	return out
}

func (r *notifyReceiver) hasEvent(t *testing.T, kind, state string) bool {
	for _, ev := range r.events(t) {
		if ev.Kind == kind && ev.State == state {
			return true
		}
	}
	return false
}

func (r *notifyReceiver) alerts(t *testing.T) []eventnotify.Alert {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []eventnotify.Alert
	for _, b := range r.bodies {
		var as []eventnotify.Alert
		if err := json.Unmarshal(b, &as); err != nil {
			t.Fatalf("alertmanager body %q: %v", b, err)
		}
		out = append(out, as...)
	}
	return out
}

// alertsOf is the alerts of kind k; firing are those whose endsAt is still
// ahead of the moment they were received (well past now), resolved those
// ending no later than now.
func (r *notifyReceiver) alertsOf(t *testing.T, k string) (firing, resolved []eventnotify.Alert) {
	now := time.Now()
	for _, a := range r.alerts(t) {
		if a.Labels["kind"] != k {
			continue
		}
		if a.EndsAt.After(now) {
			firing = append(firing, a)
		} else {
			resolved = append(resolved, a)
		}
	}
	return firing, resolved
}

func (r *notifyReceiver) authHeaders() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.auth...)
}

// notifySinks starts a webhook and an Alertmanager sink at the receivers; the
// webhook carries an Authorization header from a SecretRef.
func notifySinks(t *testing.T, wh, am *notifyReceiver, errOut io.Writer) []eventlog.Sink {
	t.Helper()
	store := filepath.Join(t.TempDir(), "secrets.json")
	if err := os.WriteFile(store, []byte(`{"secrets":{"notify/webhook":{"gen":1,"value":"`+notifySecret+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := eventnotify.Options{ErrOut: errOut, MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond}
	w, err := eventnotify.NewWebhook(eventnotify.Target{URL: wh.start(t),
		Headers: map[string]core.SecretRef{"Authorization": core.NewSecretRef("file", "notify/webhook", 0)}}, store, opts)
	if err != nil {
		t.Fatal(err)
	}
	a, err := eventnotify.NewAlertmanager(eventnotify.Target{URL: am.start(t), ResendInterval: 100 * time.Millisecond}, "", opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close(context.Background()); _ = a.Close(context.Background()) })
	return []eventlog.Sink{w, a}
}

func TestEventNotify_DriftReachesWebhookAndAlertmanager(t *testing.T) {
	// [AC-131-1]
	wh, am := &notifyReceiver{}, &notifyReceiver{}
	r := newSinkRig(t)
	r.seedDrift()
	r.start(reconcile.Config{EventSinks: notifySinks(t, wh, am, &r.errOut)})
	r.waitFor("Drift firing and resolved at the webhook", func() bool {
		return wh.hasEvent(t, "Drift", "firing") && wh.hasEvent(t, "Drift", "resolved")
	})
	r.waitFor("Drift resolved at Alertmanager", func() bool {
		_, res := am.alertsOf(t, "Drift")
		return len(res) > 0
	})
	r.stop()

	for _, ev := range wh.events(t) {
		if ev.Kind == "Drift" && (ev.Resource != "fake//db-vm" || ev.Since == "") {
			t.Errorf("[AC-131-1] webhook Drift Event = %+v, want fake//db-vm with since", ev)
		}
	}
	for _, a := range wh.authHeaders() {
		if a != notifySecret {
			t.Errorf("[AC-131-1] webhook Authorization = %q, want the value resolved from the SecretRef", a)
		}
	}
	firing, resolved := am.alertsOf(t, "Drift")
	if len(firing) == 0 {
		t.Fatalf("[AC-131-1] no firing Drift alert at Alertmanager: %+v", am.alerts(t))
	}
	f := firing[0]
	if f.Labels["alertname"] != "Drift" || f.Labels["resource"] != "fake//db-vm" || f.Labels["severity"] == "" ||
		f.Labels["class"] != "transient" || f.Annotations["message"] == "" || f.Annotations["cause"] != "B" || f.StartsAt.IsZero() {
		t.Errorf("[AC-131-1] firing alert = %+v", f)
	}
	res := resolved[len(resolved)-1]
	if res.Labels["resource"] != "fake//db-vm" || res.Labels["severity"] != f.Labels["severity"] ||
		!res.StartsAt.Equal(f.StartsAt) || !res.EndsAt.After(res.StartsAt) {
		t.Errorf("[AC-131-1] resolved alert = %+v, want the firing labels and an endsAt after startsAt", res)
	}
	for _, s := range []string{r.errOut.String(), r.out.String(), string(mustJSON(t, wh.events(t)))} {
		if strings.Contains(s, "n0tify-s3cret") {
			t.Errorf("[AC-131-1] the secret value leaked: %s", s)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEventNotify_HaltFiresResendsAndResolves(t *testing.T) {
	// [AC-131-2]
	wh, am := &notifyReceiver{}, &notifyReceiver{}
	r := newSinkRig(t)
	r.seedOrphans() // 2 orphans, 1 destructive op per cycle: the budget halts
	r.start(reconcile.Config{
		BudgetConfig: budget.Config{MaxDestructivePerCycle: 1},
		StuckAfter:   1000,
		EventSinks:   notifySinks(t, wh, am, &r.errOut),
	})
	r.waitFor("BudgetHalted firing at the webhook", func() bool { return wh.hasEvent(t, "BudgetHalted", "firing") })
	// The level Event is emitted once; Alertmanager hears it again and again.
	r.waitFor("BudgetHalted resent to Alertmanager", func() bool {
		firing, _ := am.alertsOf(t, "BudgetHalted")
		return len(firing) >= 3
	})
	writeDesiredKeepResources(t, r.intentDir, "vm-01", "vm-02", "vm-03")
	r.waitFor("BudgetHalted resolved at both", func() bool {
		_, res := am.alertsOf(t, "BudgetHalted")
		return wh.hasEvent(t, "BudgetHalted", "resolved") && len(res) > 0
	})
	r.stop()
	n := 0
	for _, ev := range wh.events(t) {
		if ev.Kind == "BudgetHalted" && ev.State == "firing" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("[AC-131-2] webhook BudgetHalted firing = %d, want 1 (level: not every cycle)", n)
	}
	firing, _ := am.alertsOf(t, "BudgetHalted")
	for _, a := range firing[1:] {
		if !a.StartsAt.Equal(firing[0].StartsAt) {
			t.Errorf("[AC-131-2] a resent alert moved startsAt: %v → %v", firing[0].StartsAt, a.StartsAt)
		}
	}
}

func TestEventNotify_DownReceiversDoNotStopConvergence(t *testing.T) {
	// [AC-131-3]
	wh, am := &notifyReceiver{}, &notifyReceiver{}
	wh.down.Store(true)
	am.down.Store(true)
	r := newSinkRig(t)
	r.seedDrift()
	r.start(reconcile.Config{EventSinks: notifySinks(t, wh, am, &r.errOut)})
	r.waitFor("a converged cycle", func() bool {
		return strings.Contains(r.out.String(), "observed=1 converged (no changes)")
	})
	if got := readFileString(r.statePath); !strings.Contains(got, `"size": "10"`) && !strings.Contains(got, `"size":"10"`) {
		t.Errorf("[AC-131-3] reality not converged to size 10; state:\n%s", got)
	}
	r.waitFor("delivery failures logged", func() bool {
		log := r.errOut.String()
		return strings.Contains(log, "sakutei: warn: event sink: webhook ") &&
			strings.Contains(log, "sakutei: warn: event sink: alertmanager ") && strings.Contains(log, "HTTP 503")
	})
	// Back up: what waited is delivered.
	wh.down.Store(false)
	am.down.Store(false)
	r.waitFor("the queued Drift firing and resolved at both", func() bool {
		_, res := am.alertsOf(t, "Drift")
		return wh.hasEvent(t, "Drift", "firing") && wh.hasEvent(t, "Drift", "resolved") && len(res) > 0
	})
	r.stop()
}

func TestEventNotify_OneShotReconcileFlushes(t *testing.T) {
	// [AC-131-4]
	wh, am := &notifyReceiver{}, &notifyReceiver{}
	whURL, amURL := wh.start(t), am.start(t)
	r := newSinkRig(t)
	r.seedDrift()
	out, code := runSakutei(t, "reconcile", "--intent", r.intentDir, "--state", r.statePath,
		"--mode", "alert-only", "--event-webhook", whURL, "--event-alertmanager", amURL)
	if code != 0 {
		t.Fatalf("[AC-131-4] reconcile exit %d:\n%s", code, out)
	}
	evs := wh.events(t)
	var kinds []string
	for _, ev := range evs {
		kinds = append(kinds, ev.Kind)
	}
	if !strings.Contains(strings.Join(kinds, ","), "Drift") {
		t.Fatalf("[AC-131-4] webhook got %v, want the Drift Events of the run; output:\n%s", kinds, out)
	}
	if len(am.alerts(t)) == 0 {
		t.Errorf("[AC-131-4] nothing reached Alertmanager before exit; output:\n%s", out)
	}
	if strings.Contains(out, "not delivered") {
		t.Errorf("[AC-131-4] a delivery failed:\n%s", out)
	}
}
