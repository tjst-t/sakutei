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

// metrics_probe_test.go — #149: /metrics の値を観測する provider（metrics-probe）
// を Workflow の require（#87）と ready_condition（§8.3）から使う（§29.1）。
// httptest の /metrics（キューの長さ）を相手に、デーモンの経路（RunCycle）で。
//
// Workflow は「上流のキューが 1000 未満であることを確かめる（require）→ 対象を
// 止める（apply active=false）」。止める手順まで進んだかで「先へ進んだか」を見る。
//
//	[AC-149-2-1] 値が閾値を越えている間は require が満たされず、手順は進まない
//	             （対象を止めない）。下がれば進み、done になる。
//	[AC-149-2-2] /metrics に届かない・メトリクスが無い間は unknown で進まない
//	             （unknown ≠ absent）。待ちの理由に観測できない訳を添え、timeout で
//	             halt する。
//	[AC-149-2-3] ready_condition: 下流は上流の probe の比べ方が真の周にだけ Apply
//	             される。越えている・届かない周は待つ。probe は観測だけで Apply
//	             されない。
//	[AC-149-2-4] registry: metrics_probe_config で作れる。無ければエラー。
package acceptance

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/audit"
	"github.com/tjst-t/sakutei/v2/internal/fakeprovider"
	"github.com/tjst-t/sakutei/v2/internal/metricsprobe"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/internal/workflow"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
	"github.com/tjst-t/sakutei/v2/pkg/registry"
)

// queueMetrics は差し替えられる /metrics（RabbitMQ の exporter の形）。
type queueMetrics struct {
	mu     sync.Mutex
	orders string // orders キューの値（"" なら系列を出さない）
	down   bool   // 503 を返す
}

func (q *queueMetrics) set(orders string, down bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.orders, q.down = orders, down
}

func (q *queueMetrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.down {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	body := "# HELP rabbitmq_queue_messages Messages ready and unacknowledged.\n# TYPE rabbitmq_queue_messages gauge\n" +
		"rabbitmq_queue_messages{vhost=\"/\",queue=\"audit\"} 7\n"
	if q.orders != "" {
		body += "rabbitmq_queue_messages{vhost=\"/\",queue=\"orders\"} " + q.orders + "\n"
	}
	_, _ = w.Write([]byte(body))
}

type probeRig struct {
	t         *testing.T
	root      string
	intentDir string
	storePath string
	fp        *fakeprovider.Provider
	mp        *metricsprobe.Provider
	q         *queueMetrics
	srv       *httptest.Server
	clock     *taskClock
	d         *reconcile.Daemon
	out       *syncBuf
}

const probeWorkflow = `{ "workflow": "stop-for-update",
      "target": { "provider": "fake" },
      "stages": [{ "name": "s1", "targets": ["n1"] }],
      "steps": [
        { "require": { "resource": "metrics-probe/orders", "fields": { "ready.below_limit": "true" }, "timeout": "30m" } },
        { "apply": { "active": "false" } }
      ] }`

func newProbeRig(t *testing.T, intent string) *probeRig {
	t.Helper()
	root := t.TempDir()
	r := &probeRig{
		t:         t,
		root:      root,
		intentDir: filepath.Join(root, "intent"),
		storePath: filepath.Join(root, "workflows.json"),
		fp:        fakeprovider.New(filepath.Join(root, "state.json")),
		q:         &queueMetrics{},
		clock:     &taskClock{now: time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)},
	}
	r.srv = httptest.NewServer(r.q)
	t.Cleanup(r.srv.Close)
	cfgPath := filepath.Join(root, "probes.yaml")
	cfg := `endpoints:
  mq:
    url: ` + r.srv.URL + `/metrics
    timeout: 2s
probes:
  orders:
    endpoint: mq
    metric: rabbitmq_queue_messages
    labels:
      vhost: /
      queue: orders
    ready:
      below_limit: "< 1000"
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	// registry の経路で作る（--provider-config の metrics-probe の値）。
	p, err := registry.DefaultRegistry().Get(metricsprobe.Name, map[string]string{"metrics_probe_config": cfgPath})
	if err != nil {
		t.Fatalf("[AC-149-2-4] open metrics-probe: %v", err)
	}
	r.mp = p.(*metricsprobe.Provider)

	if err := os.MkdirAll(r.intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.intentDir, "intent.json"), []byte(intent), 0o644); err != nil {
		t.Fatal(err)
	}
	initStateFile(t, filepath.Join(root, "state.json"))
	if err := r.fp.SeedResource("n1", map[string]any{"name": "n1", "active": "true"}); err != nil {
		t.Fatal(err)
	}
	if err := r.fp.SeedResource("app", map[string]any{"name": "app", "size": 0}); err != nil {
		t.Fatal(err)
	}
	r.out = &syncBuf{}
	r.d = reconcile.NewDaemon(reconcile.Config{
		IntentDir:    r.intentDir,
		GeneratedDir: filepath.Join(root, "generated"),
		Providers: func(name string) (provider.Provider, error) {
			switch name {
			case "fake":
				return r.fp, nil
			case metricsprobe.Name:
				return r.mp, nil
			}
			return nil, os.ErrNotExist
		},
		Out:           r.out,
		ErrOut:        r.out,
		Now:           r.clock.Now,
		WorkflowStore: workflow.NewFileStore(r.storePath),
		Audit:         audit.NewLog(filepath.Join(root, "audit.jsonl")),
	})
	return r
}

func (r *probeRig) cycle() reconcile.CycleResult {
	r.t.Helper()
	res, err := r.d.RunCycle(context.Background())
	if err != nil {
		r.t.Fatalf("RunCycle: %v\n%s", err, r.out.String())
	}
	r.d.Wait(5 * time.Second)
	return res
}

func (r *probeRig) advance(d time.Duration) { r.clock.Set(r.clock.Now().Add(d)) }

func (r *probeRig) run() workflow.Run {
	r.t.Helper()
	run, _, err := workflow.NewFileStore(r.storePath).Get(context.Background(), "stop-for-update")
	if err != nil {
		r.t.Fatal(err)
	}
	return run
}

func (r *probeRig) field(local, name string) string {
	r.t.Helper()
	obs, err := r.fp.Read(context.Background(), local)
	if err != nil || obs.State == nil {
		r.t.Fatalf("Read %s: %v", local, err)
	}
	tv, ok := obs.State[name]
	if !ok {
		return ""
	}
	return fmt.Sprintf("%v", tv.Value)
}

func workflowIntent() string {
	return `{"resources": [{ "provider": "fake", "id": "n1", "fields": { "name": "n1", "active": "false" } }],
  "workflows": [` + probeWorkflow + `]}`
}

func TestMetricsProbe_RequireHoldsUntilBelowLimit(t *testing.T) {
	// [AC-149-2-1]
	r := newProbeRig(t, workflowIntent())
	r.q.set("1500", false)
	for i := 0; i < 3; i++ {
		res := r.cycle()
		if run := r.run(); run.Phase != workflow.PhaseRunning || run.Step != 0 {
			t.Fatalf("[AC-149-2-1] run = %+v, want waiting at the require\n%s", run, r.out.String())
		}
		if !hasEvent(res, workflow.EventWaiting, "ready.below_limit") {
			t.Fatalf("[AC-149-2-1] no waiting event: %+v", res.WorkflowEvents)
		}
		if got := r.field("n1", "active"); got != "true" {
			t.Fatalf("[AC-149-2-1] n1 was stopped while the queue is 1500 (active=%s)", got)
		}
		r.advance(time.Minute)
	}
	// ちょうど閾値（1000）は「1000 未満」ではない。
	r.q.set("1000", false)
	r.cycle()
	if run := r.run(); run.Step != 0 || r.field("n1", "active") != "true" {
		t.Fatalf("[AC-149-2-1] at 1000: run = %+v active=%s", run, r.field("n1", "active"))
	}
	// 下がれば進む。
	r.q.set("999", false)
	r.advance(time.Minute)
	res := r.cycle()
	if !hasEvent(res, workflow.EventStepDone, "ready.below_limit") {
		t.Fatalf("[AC-149-2-1] no step-done event for the require: %+v", res.WorkflowEvents)
	}
	if run := r.run(); run.Phase != workflow.PhaseDone {
		r.advance(time.Minute)
		r.cycle()
		if run = r.run(); run.Phase != workflow.PhaseDone {
			t.Fatalf("[AC-149-2-1] run = %+v, want done\n%s", run, r.out.String())
		}
	}
	if got := r.field("n1", "active"); got != "false" {
		t.Fatalf("[AC-149-2-1] n1 active = %s after the workflow", got)
	}
}

func TestMetricsProbe_UnobservableDoesNotProceed(t *testing.T) {
	// [AC-149-2-2]
	r := newProbeRig(t, workflowIntent())
	for i, c := range []struct {
		orders string
		down   bool
		reason string
	}{
		{"", true, "503"},
		{"", false, "no series of rabbitmq_queue_messages"},
		{"12", true, "503"},
	} {
		r.q.set(c.orders, c.down)
		res := r.cycle()
		if !hasEvent(res, workflow.EventWaiting, "unobservable") || !hasEvent(res, workflow.EventWaiting, c.reason) {
			t.Fatalf("[AC-149-2-2] %d: no waiting (unobservable: %s) event: %+v", i, c.reason, res.WorkflowEvents)
		}
		if run := r.run(); run.Step != 0 || r.field("n1", "active") != "true" {
			t.Fatalf("[AC-149-2-2] %d: run = %+v active=%s", i, run, r.field("n1", "active"))
		}
		r.advance(time.Minute)
	}
	// 届かない（サーバが居ない）まま timeout を過ぎると halt。対象は止めない。
	r.srv.Close()
	r.cycle()
	r.advance(30 * time.Minute)
	res := r.cycle()
	run := r.run()
	if run.Phase != workflow.PhaseHalted || run.Step != 0 || !strings.Contains(run.Reason, "timed out") {
		t.Fatalf("[AC-149-2-2] run = %+v, want halted at the require by its timeout\n%s", run, r.out.String())
	}
	if !hasEvent(res, workflow.EventHalted, "timed out") {
		t.Fatalf("[AC-149-2-2] no halt event: %+v", res.WorkflowEvents)
	}
	if got := r.field("n1", "active"); got != "true" {
		t.Fatalf("[AC-149-2-2] n1 was stopped (active=%s)", got)
	}
}

func TestMetricsProbe_ReadyCondition(t *testing.T) {
	// [AC-149-2-3]
	intent := `{"resources": [
    { "provider": "metrics-probe", "id": "orders" },
    { "provider": "fake", "id": "app", "fields": { "name": "app", "size": "1" },
      "depends_on": [{ "id": "orders", "ready_condition": "below_limit" }] }
  ]}`
	r := newProbeRig(t, intent)
	for i, c := range []struct {
		orders string
		down   bool
	}{{"4000", false}, {"", true}, {"", false}} {
		r.q.set(c.orders, c.down)
		r.cycle()
		if got := r.field("app", "size"); got != "0" {
			t.Fatalf("[AC-149-2-3] %d: app was applied (size=%s) while the upstream is not ready\n%s", i, got, r.out.String())
		}
		r.advance(time.Minute)
	}
	r.q.set("10", false)
	r.cycle()
	if got := r.field("app", "size"); got != "1" {
		t.Fatalf("[AC-149-2-3] app size = %s after the queue drained\n%s", got, r.out.String())
	}
}

func TestMetricsProbe_Registry(t *testing.T) {
	// [AC-149-2-4]
	if _, err := registry.DefaultRegistry().Get(metricsprobe.Name, map[string]string{}); err == nil ||
		!strings.Contains(err.Error(), "metrics_probe_config") {
		t.Fatalf("[AC-149-2-4] open without metrics_probe_config: %v", err)
	}
	if _, err := registry.DefaultRegistry().Get(metricsprobe.Name, map[string]string{"metrics_probe_config": filepath.Join(t.TempDir(), "none.yaml")}); err == nil {
		t.Fatalf("[AC-149-2-4] open with a missing file succeeded")
	}
}
