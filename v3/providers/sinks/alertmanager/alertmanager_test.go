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

package alertmanager_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/providers/sinks/alertmanager"
)

// receiver は偽の Alertmanager（POST /api/v2/alerts を覚える）。
type receiver struct {
	mu     sync.Mutex
	alerts []alertmanager.Alert
	auth   []string
	srv    *httptest.Server
}

func newReceiver(t *testing.T) *receiver {
	r := &receiver{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.Path != "/api/v2/alerts" {
			http.Error(w, "no", http.StatusNotFound)
			return
		}
		var got []alertmanager.Alert
		if err := json.NewDecoder(req.Body).Decode(&got); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		r.alerts = append(r.alerts, got...)
		r.auth = append(r.auth, req.Header.Get("Authorization"))
		r.mu.Unlock()
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) take() []alertmanager.Alert {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.alerts
	r.alerts = nil
	return out
}

func open(t *testing.T, settings map[string]any) *alertmanager.Sink {
	t.Helper()
	s, err := alertmanager.Module(settings)
	if err != nil {
		t.Fatal(err)
	}
	return s.(*alertmanager.Sink)
}

// E-H2：複数の宛先に送る。firing は今の一覧を endsAt = 今 + ttl で送り直し、
// resolved は 1 回送る。stuck に上がれば stuck の label の無い前の alert を閉じる。
// 1 台が落ちていても送れたとし、全部が落ちればエラー（コアが次の cycle に送り直す）。
func TestAlertmanager(t *testing.T) {
	am1, am2 := newReceiver(t), newReceiver(t)
	tok := filepath.Join(t.TempDir(), "am.token")
	os.WriteFile(tok, []byte("s3cret\n"), 0o600)
	s := open(t, map[string]any{"urls": []any{am1.srv.URL, am2.srv.URL + "/"}, "token_file": tok, "ttl": "5m",
		"labels": map[string]any{"instance": "tokyo"}, "incidents": []any{"Applied"}})
	now := time.Date(2026, 11, 5, 22, 41, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	since := now.Add(-time.Hour)
	drift := core.Event{Seq: 7, Kind: "Drift", Type: core.StateEvent, State: core.Firing, Resource: "dns/record/www",
		Labels: map[string]string{"site": "a", "owner.team": "net"}, Since: &since, Count: 3, At: since, Message: "value: …"}
	ctx := context.Background()

	if err := s.DeliverState(ctx, []core.Event{drift}); err != nil {
		t.Fatal(err)
	}
	for _, am := range []*receiver{am1, am2} {
		got := am.take()
		if len(got) != 1 {
			t.Fatalf("firing: %+v", got)
		}
		a := got[0]
		l := a.Labels
		if l["alertname"] != "Drift" || l["resource"] != "dns/record/www" || l["site"] != "a" || l["owner_team"] != "net" || l["instance"] != "tokyo" || l["stuck"] != "" {
			t.Errorf("labels: %v", l)
		}
		if !a.StartsAt.Equal(since) || !a.EndsAt.Equal(now.Add(5*time.Minute)) || a.Annotations["count"] != "3" {
			t.Errorf("times: %+v", a)
		}
		if am.auth[0] != "Bearer s3cret" {
			t.Errorf("Authorization: %q", am.auth[0])
		}
	}

	stuck := drift
	stuck.Stuck = true
	resolved := drift
	resolved.Seq, resolved.State, resolved.At = 9, core.Resolved, now
	applied := core.Event{Seq: 10, Kind: "Applied", Type: core.IncidentEvent, Resource: "dns/record/api", At: now}
	cycle := core.Event{Seq: 11, Kind: "CycleDone", Type: core.IncidentEvent, At: now}
	if err := s.Deliver(ctx, []core.Event{stuck, resolved, applied, cycle}); err != nil {
		t.Fatal(err)
	}
	got := am1.take()
	am2.take()
	if len(got) != 3 {
		t.Fatalf("stuck・resolved・Applied の 3 つ（CycleDone は送らない）: %+v", got)
	}
	if got[0].Labels["stuck"] != "" || !got[0].EndsAt.Equal(since) {
		t.Errorf("stuck に上がる前の alert を閉じる: %+v", got[0])
	}
	if !got[1].EndsAt.Equal(now) || got[1].Labels["alertname"] != "Drift" {
		t.Errorf("resolved: %+v", got[1])
	}
	if got[2].Labels["alertname"] != "Applied" || !got[2].EndsAt.Equal(now.Add(alertmanager.DefaultIncidentTTL)) {
		t.Errorf("edge Event: %+v", got[2])
	}

	// 1 台が落ちても送れた。全部落ちればエラー。エラーメッセージにトークンを出さない。
	am2.srv.Close()
	if err := s.DeliverState(ctx, []core.Event{stuck}); err != nil {
		t.Fatalf("1 台が落ちた: %v", err)
	}
	if got := am1.take(); len(got) != 1 || got[0].Labels["stuck"] != "true" {
		t.Fatalf("stuck の alert: %+v", got)
	}
	am1.srv.Close()
	err := s.DeliverState(ctx, []core.Event{stuck})
	if err == nil || !strings.Contains(err.Error(), "どの Alertmanager にも届かない") || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("全部落ちた: %v", err)
	}
}

func TestAlertmanagerRejectsSettings(t *testing.T) {
	for _, c := range []struct {
		name     string
		settings map[string]any
		want     string
	}{
		{"宛先なし", map[string]any{}, "urls に"},
		{"外向きの平文", map[string]any{"urls": []any{"http://am.example.com:9093"}}, "plaintext: true"},
		{"URL の credential", map[string]any{"urls": []any{"https://u:p@am.example.com"}}, "token_file に書く"},
		{"不明な field", map[string]any{"urls": []any{"https://am.example.com"}, "url": "x"}, "不明な field url"},
		{"予約の label", map[string]any{"urls": []any{"https://am.example.com"}, "labels": map[string]any{"alertname": "x"}}, "labels.alertname"},
		{"duration", map[string]any{"urls": []any{"https://am.example.com"}, "ttl": "soon"}, "ttl"},
	} {
		_, err := alertmanager.Module(c.settings)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v（%q を含むはず）", c.name, err, c.want)
		}
	}
	if _, err := alertmanager.Module(map[string]any{"urls": []any{"http://am.example.com:9093"}, "plaintext": true}); err != nil {
		t.Errorf("明示した平文: %v", err)
	}
}
