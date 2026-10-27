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

// metrics_test.go — #54（v2：Prometheus exporter、§27.5）:
//
//	[AC-54-1] デーモンは --metrics-listen を明示したときだけ /metrics を待ち受け、
//	          Prometheus の text format（形式のテストを通る）で drifted・unknown の
//	          件数、予算の残り、halt の有無、cycle と Observe の所要時間を返す
//	[AC-54-2] 予算の gate が周を止めると sakutei_halted{gate="budget"} が 1 になる
package acceptance

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/budget"
	"github.com/tjst-t/sakutei/internal/fakeprovider"
	"github.com/tjst-t/sakutei/internal/metrics"
	"github.com/tjst-t/sakutei/internal/reconcile"
)

func fakeRes(id, size string) map[string]any {
	return map[string]any{"provider": "fake", "id": id, "fields": map[string]string{"name": id, "size": size}}
}

// scrape GETs url and parses the body strictly (the format test).
func scrape(t *testing.T, url string) (map[string]float64, error) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != metrics.ContentType {
		t.Fatalf("Content-Type %q, want %q", ct, metrics.ContentType)
	}
	ps, err := metrics.Parse(strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("[AC-54-1] /metrics is not in the text format: %v\n%s", err, body)
	}
	m := map[string]float64{}
	for _, p := range ps {
		m[p.Key()] = p.Value
	}
	return m, nil
}

// [AC-54-1]
func TestMetrics_DaemonServesConvergenceState(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	// in-sync: converged / drifted: size differs / absent: to be created /
	// unreachable: availability unknown.
	writeIntentFile(t, intentDir, map[string]any{"resources": []any{
		fakeRes("in-sync", "1"), fakeRes("drifted", "2"), fakeRes("absent", "3"), fakeRes("unreachable", "4"),
	}})
	seedState(t, statePath, map[string]map[string]any{
		"in-sync":     {"name": "in-sync", "size": "1"},
		"drifted":     {"name": "drifted", "size": "999"},
		"unreachable": {"name": "unreachable", "size": "4"},
	})
	seedStateObserveUnknown(t, statePath, "unreachable")

	// Without --metrics-listen nothing listens.
	cmd, out, _ := startDaemonWithArgs(t, intentDir, statePath, "100ms", "--mode", "alert-only")
	time.Sleep(500 * time.Millisecond)
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if strings.Contains(out.String(), "metrics listening") {
		t.Fatalf("[AC-54-1] /metrics served without --metrics-listen:\n%s", out.String())
	}

	cmd, out, errOut := startDaemonWithArgs(t, intentDir, statePath, "100ms",
		"--mode", "alert-only", "--max-destructive-per-window", "7", "--metrics-listen", "127.0.0.1:0")
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	re := regexp.MustCompile(`metrics listening on (http://\S+/metrics)`)
	var url string
	deadline := time.Now().Add(10 * time.Second)
	for url == "" && time.Now().Before(deadline) {
		if m := re.FindStringSubmatch(out.String()); m != nil {
			url = m[1]
		}
		time.Sleep(20 * time.Millisecond)
	}
	if url == "" {
		t.Fatalf("no metrics listening line:\n%s\n%s", out.String(), errOut.String())
	}
	var v map[string]float64
	for time.Now().Before(deadline) {
		var err error
		if v, err = scrape(t, url); err == nil && v["sakutei_cycles_total"] >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	want := map[string]float64{
		`sakutei_resources{state="desired"}`:          4,
		`sakutei_resources{state="drifted"}`:          2,
		`sakutei_resources{state="unknown"}`:          1,
		`sakutei_halted{gate="budget"}`:               0,
		`sakutei_halted{gate="prune"}`:                0,
		"sakutei_alert_only":                          1,
		"sakutei_budget_destructive_window_limit":     7,
		"sakutei_budget_destructive_window_remaining": 7,
		"sakutei_cycle_errors_total":                  0,
	}
	for k, w := range want {
		if got, ok := v[k]; !ok || got != w {
			t.Errorf("[AC-54-1] %s = %v (present %v), want %v", k, got, ok, w)
		}
	}
	for _, k := range []string{"sakutei_cycle_duration_seconds", "sakutei_observe_duration_seconds"} {
		got, ok := v[k]
		if !ok || got < 0 || got > 10 {
			t.Errorf("[AC-54-1] %s = %v (present %v)", k, got, ok)
		}
	}
	if v["sakutei_observe_duration_seconds"] > v["sakutei_cycle_duration_seconds"] {
		t.Errorf("[AC-54-1] Observe %v longer than the cycle %v", v["sakutei_observe_duration_seconds"], v["sakutei_cycle_duration_seconds"])
	}
}

// [AC-54-2]
func TestMetrics_BudgetHalt(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	initStateFile(t, statePath)
	writeIntentFile(t, intentDir, map[string]any{"resources": []any{fakeRes("a", "1"), fakeRes("b", "1"), fakeRes("c", "1")}})

	e := metrics.NewExporter(metrics.BudgetLimits{ChangesPerCycle: 1})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var out syncBuffer
	go func() {
		done <- reconcile.Run(ctx, reconcile.Config{
			IntentDir:    intentDir,
			GeneratedDir: filepath.Join(tmp, "generated"),
			Provider:     fakeprovider.New(statePath),
			Interval:     20 * time.Millisecond,
			BudgetConfig: budget.Config{MaxChangesPerCycleAbsolute: 1},
			Metrics:      e,
			Out:          &out,
			ErrOut:       &out,
		})
	}()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(10 * time.Second)
	var v map[string]float64
	for time.Now().Before(deadline) {
		var b strings.Builder
		if err := metrics.Write(&b, e.Families()); err != nil {
			t.Fatal(err)
		}
		ps, err := metrics.Parse(strings.NewReader(b.String()))
		if err != nil {
			t.Fatalf("[AC-54-2] not in the text format: %v\n%s", err, b.String())
		}
		v = map[string]float64{}
		for _, p := range ps {
			v[p.Key()] = p.Value
		}
		if v["sakutei_cycles_total"] >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if v[`sakutei_halted{gate="budget"}`] != 1 || v["sakutei_held_ops"] != 3 || v[`sakutei_resources{state="drifted"}`] != 3 {
		t.Errorf("[AC-54-2] first cycle: halted=%v held=%v drifted=%v\n%s", v[`sakutei_halted{gate="budget"}`],
			v["sakutei_held_ops"], v[`sakutei_resources{state="drifted"}`], out.String())
	}
	if v["sakutei_budget_changes_cycle_limit"] != 1 {
		t.Errorf("[AC-54-2] changes limit %v", v["sakutei_budget_changes_cycle_limit"])
	}
}
