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

package eventnotify

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

const secretValue = "Bearer s3cr3t-token"

// syncBuffer is a goroutine-safe buffer for ErrOut.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// recv is an httptest receiver whose answer can be switched (down = 503).
type recv struct {
	mu     sync.Mutex
	bodies [][]byte
	auth   []string
	down   atomic.Bool
	status atomic.Int32 // non-zero: answer this
}

func (r *recv) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	if r.down.Load() {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	if s := r.status.Load(); s != 0 {
		http.Error(w, "bad", int(s))
		return
	}
	r.mu.Lock()
	r.bodies = append(r.bodies, b)
	r.auth = append(r.auth, req.Header.Get("Authorization"))
	r.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (r *recv) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

func (r *recv) events(t *testing.T) []map[string]any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []map[string]any
	for _, b := range r.bodies {
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("webhook body %q: %v", b, err)
		}
		out = append(out, m)
	}
	return out
}

func (r *recv) alertBatches(t *testing.T) [][]Alert {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out [][]Alert
	for _, b := range r.bodies {
		var as []Alert
		if err := json.Unmarshal(b, &as); err != nil {
			t.Fatalf("alertmanager body %q: %v", b, err)
		}
		out = append(out, as)
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func writeStore(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "secrets.json")
	data := `{"secrets": {"notify/auth": {"gen": 1, "value": "` + secretValue + `"}}}`
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

var fastOpts = Options{MinBackoff: 5 * time.Millisecond, MaxBackoff: 20 * time.Millisecond}

func driftEvent(state core.EventState) core.Event {
	since := time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC)
	return core.Event{
		Kind: core.EventDrift, Severity: core.SeverityWarning, Time: since.Add(time.Minute),
		Resource: core.ID{Provider: "fake", Local: "db-vm"}, Cause: core.CauseTagB,
		Message: "size: 30 → 10", State: state, Since: since, Count: 1, Class: core.EventTransient,
	}
}

func TestWebhook_PostsEventJSONWithSecretHeader(t *testing.T) {
	rv := &recv{}
	srv := httptest.NewServer(rv)
	defer srv.Close()
	store := writeStore(t, t.TempDir())
	var errOut syncBuffer
	o := fastOpts
	o.ErrOut = &errOut
	w, err := NewWebhook(Target{URL: srv.URL, Headers: map[string]core.SecretRef{
		"Authorization": core.NewSecretRef("file", "notify/auth", 0)}}, store, o)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Emit(context.Background(), driftEvent(core.EventFiring)); err != nil {
		t.Fatal(err)
	}
	if err := w.Emit(context.Background(), driftEvent(core.EventResolved)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(ctxTimeout(t)); err != nil {
		t.Fatal(err)
	}
	evs := rv.events(t)
	if len(evs) != 2 || evs[0]["state"] != "firing" || evs[1]["state"] != "resolved" ||
		evs[0]["kind"] != "Drift" || evs[0]["resource"] != "fake//db-vm" || evs[0]["cause"] != "B" {
		t.Fatalf("webhook got %v", evs)
	}
	for _, a := range rv.auth {
		if a != secretValue {
			t.Errorf("Authorization = %q, want the resolved secret", a)
		}
	}
	if strings.Contains(errOut.String(), secretValue) {
		t.Errorf("the secret leaked to the log: %s", errOut.String())
	}
}

func ctxTimeout(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestWebhook_RetriesWhileDownThenDelivers(t *testing.T) {
	rv := &recv{}
	rv.down.Store(true)
	srv := httptest.NewServer(rv)
	defer srv.Close()
	var errOut syncBuffer
	o := fastOpts
	o.ErrOut = &errOut
	w, err := NewWebhook(Target{URL: srv.URL}, "", o)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())
	start := time.Now()
	if err := w.Emit(context.Background(), driftEvent(core.EventFiring)); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("Emit blocked on a down receiver")
	}
	waitFor(t, "3 failures logged", func() bool { return strings.Count(errOut.String(), "not delivered") >= 3 })
	if !strings.Contains(errOut.String(), "HTTP 503") || !strings.Contains(errOut.String(), "convergence continues") {
		t.Errorf("log = %s", errOut.String())
	}
	rv.down.Store(false)
	waitFor(t, "delivery after recovery", func() bool { return rv.count() == 1 })
	// The receiver counts the delivery before the sender has read the
	// response and taken the event off its queue, so wait for that too.
	waitFor(t, "queue empty after delivery", func() bool { return w.Pending() == 0 })
	if rv.count() != 1 {
		t.Errorf("delivered %d times, want 1", rv.count())
	}
}

func TestWebhook_RejectedIsDroppedAndQueueIsBounded(t *testing.T) {
	rv := &recv{}
	rv.status.Store(http.StatusBadRequest)
	srv := httptest.NewServer(rv)
	defer srv.Close()
	var errOut syncBuffer
	o := fastOpts
	o.ErrOut = &errOut
	w, _ := NewWebhook(Target{URL: srv.URL}, "", o)
	_ = w.Emit(context.Background(), driftEvent(core.EventFiring))
	waitFor(t, "rejected log", func() bool { return strings.Contains(errOut.String(), "rejected, dropped") })
	waitFor(t, "queue empty", func() bool { return w.Pending() == 0 })
	_ = w.Close(ctxTimeout(t))

	// Bounded: a down receiver and a queue of 2.
	rv2 := &recv{}
	rv2.down.Store(true)
	srv2 := httptest.NewServer(rv2)
	defer srv2.Close()
	o.MaxQueue = 2
	w2, _ := NewWebhook(Target{URL: srv2.URL}, "", o)
	var dropErr error
	for i := 0; i < 4; i++ {
		if err := w2.Emit(context.Background(), driftEvent(core.EventFiring)); err != nil {
			dropErr = err
		}
	}
	if dropErr == nil || !strings.Contains(dropErr.Error(), "dropped the oldest") || w2.Pending() > 2 {
		t.Errorf("overflow err=%v pending=%d", dropErr, w2.Pending())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := w2.Close(ctx); err == nil || !strings.Contains(err.Error(), "not delivered before shutdown") {
		t.Errorf("Close on a down receiver = %v", err)
	}
}

func TestWebhook_TLS(t *testing.T) {
	rv := &recv{}
	srv := httptest.NewUnstartedServer(rv)
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the "no trust" handshakes fail
	srv.StartTLS()
	defer srv.Close()
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(ca, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		t    Target
		ok   bool
	}{
		{"no trust", Target{URL: srv.URL}, false},
		{"ca_file", Target{URL: srv.URL, CAFile: ca}, true},
		{"insecure_skip_verify", Target{URL: srv.URL, InsecureSkipVerify: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var errOut syncBuffer
			o := fastOpts
			o.ErrOut = &errOut
			before := rv.count()
			w, err := NewWebhook(tc.t, "", o)
			if err != nil {
				t.Fatal(err)
			}
			_ = w.Emit(context.Background(), driftEvent(core.EventFiring))
			if tc.ok {
				waitFor(t, "delivery", func() bool { return rv.count() == before+1 })
			} else {
				waitFor(t, "a certificate failure", func() bool { return strings.Contains(errOut.String(), "certificate") })
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_ = w.Close(ctx)
		})
	}
}

func TestAlertmanager_FiringResendResolved(t *testing.T) {
	rv := &recv{}
	srv := httptest.NewServer(rv)
	defer srv.Close()
	var errOut syncBuffer
	o := fastOpts
	o.ErrOut = &errOut
	resend := 30 * time.Millisecond
	a, err := NewAlertmanager(Target{URL: srv.URL + "/", ResendInterval: resend,
		Labels: map[string]string{"instance": "tokyo"}}, "", o)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	fire := driftEvent(core.EventFiring)
	_ = a.Emit(context.Background(), fire)
	// The level Event is emitted once; the sink resends it.
	waitFor(t, "3 sends of the firing alert", func() bool { return rv.count() >= 3 })
	for i, batch := range rv.alertBatches(t) {
		if len(batch) != 1 {
			t.Fatalf("batch %d = %+v", i, batch)
		}
		al := batch[0]
		want := map[string]string{"alertname": "Drift", "kind": "Drift", "severity": "warning",
			"resource": "fake//db-vm", "class": "transient", "instance": "tokyo"}
		if !sameLabels(al.Labels, want) {
			t.Errorf("labels = %v, want %v", al.Labels, want)
		}
		if al.Annotations["message"] != fire.Message || al.Annotations["cause"] != "B" || al.Annotations["count"] != "1" {
			t.Errorf("annotations = %v", al.Annotations)
		}
		if !al.StartsAt.Equal(fire.Since) || !al.EndsAt.After(time.Now()) {
			t.Errorf("startsAt=%v endsAt=%v, want since and a future endsAt", al.StartsAt, al.EndsAt)
		}
	}

	res := driftEvent(core.EventResolved)
	res.Severity, res.Time = core.SeverityInfo, time.Now()
	_ = a.Emit(context.Background(), res)
	waitFor(t, "a resolved alert", func() bool {
		bs := rv.alertBatches(t)
		last := bs[len(bs)-1]
		return len(last) == 1 && last[0].EndsAt.Equal(res.Time)
	})
	bs := rv.alertBatches(t)
	if got := bs[len(bs)-1][0].Labels["severity"]; got != "warning" {
		t.Errorf("resolved alert severity label = %q, want the firing labels", got)
	}
	n := rv.count()
	time.Sleep(4 * resend)
	if rv.count() != n || a.Active() != 0 {
		t.Errorf("sends after resolved: %d → %d (active %d)", n, rv.count(), a.Active())
	}
}

func TestAlertmanager_StuckRelabelsAndEdges(t *testing.T) {
	rv := &recv{}
	srv := httptest.NewServer(rv)
	defer srv.Close()
	a, _ := NewAlertmanager(Target{URL: srv.URL, ResendInterval: time.Hour, EdgeTTL: 10 * time.Minute}, "", fastOpts)
	defer a.Close(context.Background())
	_ = a.Emit(context.Background(), driftEvent(core.EventFiring))
	waitFor(t, "first send", func() bool { return rv.count() >= 1 })

	// stuck: severity and class change → the old labels resolve.
	stuck := driftEvent(core.EventFiring)
	stuck.Severity, stuck.Class, stuck.Count, stuck.Time = core.SeverityCritical, core.EventStuck, 5, time.Now()
	_ = a.Emit(context.Background(), stuck)
	waitFor(t, "stuck firing and the transient resolved in one batch", func() bool {
		return anyBatch(rv.alertBatches(t), func(b []Alert) bool {
			var sawStuck, sawOldResolved bool
			for _, al := range b {
				switch {
				case al.Labels["class"] == "stuck" && al.Labels["severity"] == "critical" && al.EndsAt.After(time.Now()):
					sawStuck = true
				case al.Labels["class"] == "transient" && al.EndsAt.Equal(stuck.Time):
					sawOldResolved = true
				}
			}
			return len(b) == 2 && sawStuck && sawOldResolved
		})
	})

	// Edge: warning is sent once with edge_ttl; info is not sent.
	edge := core.Event{Kind: core.EventWorkflowHalted, Severity: core.SeverityWarning, Time: time.Now(), Message: "invariant"}
	_ = a.Emit(context.Background(), core.Event{Kind: core.EventPruned, Severity: core.SeverityInfo, Time: time.Now()})
	_ = a.Emit(context.Background(), edge)
	waitFor(t, "edge send", func() bool { return a.Pending() == 0 && rv.count() >= 3 })
	var edges []Alert
	for _, b := range rv.alertBatches(t) {
		for _, al := range b {
			if al.Labels["kind"] != "Drift" {
				edges = append(edges, al)
			}
		}
	}
	if len(edges) != 1 || edges[0].Labels["kind"] != "WorkflowHalted" || !edges[0].EndsAt.Equal(edge.Time.Add(10*time.Minute)) {
		t.Errorf("edge alerts = %+v, want one WorkflowHalted ending after edge_ttl", edges)
	}
}

func anyBatch(bs [][]Alert, ok func([]Alert) bool) bool {
	for _, b := range bs {
		if ok(b) {
			return true
		}
	}
	return false
}

func TestAlertmanager_DownThenUp(t *testing.T) {
	rv := &recv{}
	rv.down.Store(true)
	srv := httptest.NewServer(rv)
	defer srv.Close()
	var errOut syncBuffer
	o := fastOpts
	o.ErrOut = &errOut
	a, _ := NewAlertmanager(Target{URL: srv.URL, ResendInterval: time.Hour}, "", o)
	defer a.Close(context.Background())
	_ = a.Emit(context.Background(), driftEvent(core.EventFiring))
	res := driftEvent(core.EventResolved)
	res.Time = time.Now()
	_ = a.Emit(context.Background(), res)
	waitFor(t, "failures logged", func() bool { return strings.Count(errOut.String(), "not delivered") >= 2 })
	rv.down.Store(false)
	// 受け手に届いたあと、送り手が待ち行列から外すまでの間がある（CI で揺れた）ので、
	// 両方がそろうまで待つ。
	waitFor(t, "delivery", func() bool { return rv.count() >= 1 && a.Pending() == 0 })
	b := rv.alertBatches(t)[0]
	if len(b) != 1 || !b[0].EndsAt.Equal(res.Time) {
		t.Errorf("after recovery = %+v, want the resolved alert", b)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	writeStore(t, dir)
	write := func(s string) string {
		p := filepath.Join(dir, "notify.yaml")
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg, err := LoadConfig(write(`secret_store: secrets.json
webhook:
  headers:
    Authorization: secretref:file/notify/auth
  insecure_skip_verify: true
alertmanager:
  ca_file: ca.pem
  timeout: 5s
  resend_interval: 2m
  edge_ttl: 30m
  labels:
    instance: tokyo
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SecretStore != filepath.Join(dir, "secrets.json") || cfg.Webhook.Headers["Authorization"].Path() != "notify/auth" ||
		!cfg.Webhook.InsecureSkipVerify || cfg.Alertmanager.CAFile != filepath.Join(dir, "ca.pem") ||
		cfg.Alertmanager.ResendInterval != 2*time.Minute || cfg.Alertmanager.EdgeTTL != 30*time.Minute ||
		cfg.Alertmanager.Timeout != 5*time.Second || cfg.Alertmanager.Labels["instance"] != "tokyo" {
		t.Errorf("cfg = %+v", cfg)
	}
	for _, tc := range []struct{ in, want string }{
		{"webhook:\n  headers:\n    Authorization: " + secretValue + "\nsecret_store: secrets.json\n", "must be a SecretRef"},
		{"webhook:\n  headers:\n    Authorization: secretref:file/notify/auth\n", `"secret_store" is required`},
		{"secret_store: secrets.json\nwebhook:\n  headers:\n    X-Token: secretref:file/nope\n", "is not in the secret store"},
		{"webhook:\n  ca_file: a.pem\n  insecure_skip_verify: true\n", "exclusive"},
		{"webhook:\n  resend_interval: 1m\n", `unknown key "webhook.resend_interval"`},
		{"alertmanager:\n  labels:\n    severity: x\n", "set from the Event"},
		{"alertmanager:\n  timeout: soon\n", "positive duration"},
	} {
		_, err := LoadConfig(write(tc.in))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("LoadConfig(%q) = %v, want %q", tc.in, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("the raw value leaked into the error: %v", err)
		}
	}
	for _, u := range []string{"ftp://x", "localhost:9093", "http://user:pw@h/"} {
		if CheckURL(u) == nil {
			t.Errorf("CheckURL(%q) = nil", u)
		}
	}
}
