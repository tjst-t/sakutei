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

package metrics

import (
	"bytes"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func values(t *testing.T, text string) map[string]float64 {
	t.Helper()
	ps, err := Parse(strings.NewReader(text))
	if err != nil {
		t.Fatalf("Parse: %v\n%s", err, text)
	}
	m := map[string]float64{}
	for _, p := range ps {
		m[p.Key()] = p.Value
	}
	return m
}

func TestWrite_Format(t *testing.T) {
	var b bytes.Buffer
	err := Write(&b, []Family{
		{Name: "a_total", Type: Counter, Help: "line\nwith \\ back", Samples: []Sample{{Value: 3}}},
		{Name: "empty", Type: Gauge, Help: "no samples: not written"},
		{Name: "g", Type: Gauge, Help: "g", Samples: []Sample{
			{Labels: []Label{{"k", "q\"\\\n"}}, Value: 0.5},
			{Labels: []Label{{"k", "inf"}}, Value: math.Inf(1)},
			{Labels: []Label{{"k", "-inf"}}, Value: math.Inf(-1)},
			{Labels: []Label{{"k", "nan"}}, Value: math.NaN()},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `# HELP a_total line\nwith \\ back
# TYPE a_total counter
a_total 3
# HELP g g
# TYPE g gauge
g{k="q\"\\\n"} 0.5
g{k="inf"} +Inf
g{k="-inf"} -Inf
g{k="nan"} NaN
`
	if b.String() != want {
		t.Fatalf("got\n%s\nwant\n%s", b.String(), want)
	}
	ps, err := Parse(strings.NewReader(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if ps[1].Labels["k"] != "q\"\\\n" {
		t.Errorf("label value read back as %q", ps[1].Labels["k"])
	}
}

func TestParse_RejectsBadFormat(t *testing.T) {
	for name, text := range map[string]string{
		"no TYPE":        "x 1\n",
		"not contiguous": "# TYPE x gauge\n# TYPE y gauge\nx 1\ny 1\nx{a=\"b\"} 1\n",
		"two TYPE":       "# TYPE x gauge\n# TYPE x gauge\nx 1\n",
		"bad name":       "# TYPE 1x gauge\n1x 1\n",
		"bad label":      "# TYPE x gauge\nx{1a=\"b\"} 1\n",
		"bad escape":     "# TYPE x gauge\nx{a=\"\\t\"} 1\n",
		"bad value":      "# TYPE x gauge\nx one\n",
		"twice":          "# TYPE x gauge\nx 1\nx 2\n",
		"counter name":   "# TYPE x counter\nx 1\n",
		"unterminated":   "# TYPE x gauge\nx{a=\"b} 1\n",
	} {
		if _, err := Parse(strings.NewReader(text)); err == nil {
			t.Errorf("%s: Parse accepted\n%s", name, text)
		}
	}
}

func TestExporter_BeforeFirstCycle(t *testing.T) {
	e := NewExporter(BudgetLimits{DestructivePerWindow: 5, Window: time.Hour})
	var b bytes.Buffer
	if err := Write(&b, e.Families()); err != nil {
		t.Fatal(err)
	}
	v := values(t, b.String())
	if v["sakutei_cycles_total"] != 0 || v["sakutei_budget_destructive_window_limit"] != 5 || v["sakutei_budget_window_seconds"] != 3600 {
		t.Errorf("got %v", v)
	}
	// No cycle yet: the cycle's gauges have no value (not zero).
	for _, k := range []string{`sakutei_resources{state="drifted"}`, "sakutei_cycle_duration_seconds", "sakutei_budget_destructive_window_remaining"} {
		if _, ok := v[k]; ok {
			t.Errorf("%s exported before the first cycle", k)
		}
	}
}

func TestExporter_Cycle(t *testing.T) {
	e := NewExporter(BudgetLimits{DestructivePerWindow: 3, DestructivePerCycle: 2, Window: 24 * time.Hour})
	e.Record(Cycle{Number: 1, Err: true, End: time.Unix(100, 0)})
	e.Record(Cycle{
		Number: 2, End: time.Unix(200, 500_000_000), Duration: 1500 * time.Millisecond,
		ObserveRan: true, ObserveDuration: 250 * time.Millisecond,
		Desired: 10, Observed: 8, Drifted: 3, Unknown: 2, Converging: 1, Held: 4,
		BudgetHalted: true, BudgetWindowUsed: 5, ConsecutiveUnreachableSkips: 0,
	})
	rec := httptest.NewRecorder()
	e.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != ContentType {
		t.Fatalf("code=%d content-type=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
	v := values(t, rec.Body.String())
	want := map[string]float64{
		"sakutei_cycles_total":                        2,
		"sakutei_cycle_errors_total":                  1,
		"sakutei_last_cycle":                          2,
		"sakutei_last_cycle_end_timestamp_seconds":    200.5,
		"sakutei_cycle_duration_seconds":              1.5,
		"sakutei_observe_duration_seconds":            0.25,
		`sakutei_resources{state="desired"}`:          10,
		`sakutei_resources{state="observed"}`:         8,
		`sakutei_resources{state="drifted"}`:          3,
		`sakutei_resources{state="unknown"}`:          2,
		`sakutei_resources{state="converging"}`:       1,
		`sakutei_halted{gate="budget"}`:               1,
		`sakutei_halted{gate="capacity"}`:             0,
		`sakutei_halted{gate="prune"}`:                0,
		`sakutei_halted{gate="inventory"}`:            0,
		`sakutei_halted{gate="external_input"}`:       0,
		"sakutei_held_ops":                            4,
		"sakutei_last_cycle_error":                    0,
		"sakutei_budget_destructive_window_used":      5,
		"sakutei_budget_destructive_window_limit":     3,
		"sakutei_budget_destructive_window_remaining": 0, // used over the limit: never negative
		"sakutei_budget_destructive_cycle_limit":      2,
	}
	for k, w := range want {
		if got, ok := v[k]; !ok || got != w {
			t.Errorf("%s = %v (present %v), want %v", k, got, ok, w)
		}
	}
	if _, ok := v["sakutei_budget_changes_cycle_limit"]; ok {
		t.Error("an unlimited budget exports a limit")
	}
}

// A cycle that ended before Observe does not export its zero counts: the last
// observing cycle's values stay, with its number (unknown ≠ absent).
func TestExporter_CycleWithoutObserveKeepsLastObservation(t *testing.T) {
	e := NewExporter(BudgetLimits{})
	render := func() map[string]float64 {
		var b bytes.Buffer
		if err := Write(&b, e.Families()); err != nil {
			t.Fatal(err)
		}
		return values(t, b.String())
	}
	e.Record(Cycle{Number: 1, Err: true})
	v := render()
	for _, k := range []string{`sakutei_resources{state="drifted"}`, "sakutei_observe_duration_seconds", "sakutei_last_observe_cycle"} {
		if _, ok := v[k]; ok {
			t.Errorf("%s exported though no cycle observed", k)
		}
	}
	e.Record(Cycle{Number: 2, ObserveRan: true, ObserveDuration: time.Second, Desired: 4, Drifted: 2, Unknown: 1})
	e.Record(Cycle{Number: 3, ExternalInputSkipped: true})
	v = render()
	for k, w := range map[string]float64{
		`sakutei_resources{state="desired"}`:    4,
		`sakutei_resources{state="drifted"}`:    2,
		`sakutei_resources{state="unknown"}`:    1,
		"sakutei_observe_duration_seconds":      1,
		"sakutei_last_observe_cycle":            2,
		"sakutei_last_cycle":                    3,
		`sakutei_halted{gate="external_input"}`: 1,
	} {
		if got, ok := v[k]; !ok || got != w {
			t.Errorf("%s = %v (present %v), want %v", k, got, ok, w)
		}
	}
}

func TestExporter_HTTP(t *testing.T) {
	h := NewExporter(BudgetLimits{}).Handler()
	for _, c := range []struct {
		method, path string
		code         int
	}{
		{http.MethodGet, "/metrics", 200},
		{http.MethodHead, "/metrics", 200},
		{http.MethodPost, "/metrics", 405},
		{http.MethodGet, "/", 404},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != c.code {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, rec.Code, c.code)
		}
	}
}

// Record and the HTTP side run on different goroutines (make test-race).
func TestExporter_Concurrent(t *testing.T) {
	e := NewExporter(BudgetLimits{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			e.Record(Cycle{Number: i})
		}
	}()
	for i := 0; i < 200; i++ {
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	}
	<-done
}

func TestExporter_NotifyPerDestination(t *testing.T) {
	e := NewExporter(BudgetLimits{})
	last := time.Unix(1700000000, 0)
	e.SetNotify(func() []NotifyTarget {
		return []NotifyTarget{
			{Sink: "alertmanager", URL: "http://am-1:9093", Delivered: 7, Active: 2, LastSuccess: last},
			{Sink: "alertmanager", URL: "http://am-2:9093", Failed: 3, Pending: 1, ConsecutiveFailures: 3},
			{Sink: "webhook", URL: "https://hook", Delivered: 1, Rejected: 1, Dropped: 4},
		}
	})
	var b bytes.Buffer
	if err := Write(&b, e.Families()); err != nil {
		t.Fatal(err)
	}
	v := values(t, b.String())
	for k, want := range map[string]float64{
		`sakutei_notify_requests_total{result="delivered",sink="alertmanager",url="http://am-1:9093"}`: 7,
		`sakutei_notify_requests_total{result="failed",sink="alertmanager",url="http://am-2:9093"}`:    3,
		`sakutei_notify_requests_total{result="rejected",sink="webhook",url="https://hook"}`:           1,
		`sakutei_notify_dropped_total{sink="webhook",url="https://hook"}`:                              4,
		`sakutei_notify_pending{sink="alertmanager",url="http://am-2:9093"}`:                           1,
		`sakutei_notify_up{sink="alertmanager",url="http://am-1:9093"}`:                                1,
		`sakutei_notify_up{sink="alertmanager",url="http://am-2:9093"}`:                                0,
		`sakutei_notify_consecutive_failures{sink="alertmanager",url="http://am-2:9093"}`:              3,
		`sakutei_notify_active_alerts{sink="alertmanager",url="http://am-1:9093"}`:                     2,
		`sakutei_notify_last_success_timestamp_seconds{sink="alertmanager",url="http://am-1:9093"}`:    1700000000,
	} {
		if got, ok := v[k]; !ok || got != want {
			t.Errorf("%s = %v (exported %v), want %v", k, got, ok, want)
		}
	}
	if _, ok := v[`sakutei_notify_last_success_timestamp_seconds{sink="alertmanager",url="http://am-2:9093"}`]; ok {
		t.Error("last success exported for a destination never reached")
	}
	if _, ok := v[`sakutei_notify_active_alerts{sink="webhook",url="https://hook"}`]; ok {
		t.Error("active alerts exported for a webhook")
	}
}

// TestParseScrape は他の exporter の書くページを読む（#149 の metrics-probe）。
func TestParseScrape(t *testing.T) {
	text := "# HELP q Messages.\n" +
		"# TYPE q gauge\n" +
		"q{ queue = \"orders\" , vhost=\"/\" } 1200\n" +
		"h_bucket{le=\"+Inf\"} 4 1700000000000\n" +
		"untyped\t7\n" +
		"q{queue=\"a\\\"b\\\\c\\nd\"} 1.5e3 1.7e9\n" +
		"  # an indented comment\n" +
		"x -Inf\n" +
		"# EOF\n"
	ps, err := ParseScrape(strings.NewReader(text))
	if err != nil {
		t.Fatalf("ParseScrape: %v", err)
	}
	got := map[string]float64{}
	for _, p := range ps {
		got[p.Key()] = p.Value
	}
	for k, want := range map[string]float64{
		`q{queue="orders",vhost="/"}`: 1200,
		`h_bucket{le="+Inf"}`:         4,
		`untyped`:                     7,
		`q{queue="a\"b\\c\nd"}`:       1500,
	} {
		if v, ok := got[k]; !ok || v != want {
			t.Fatalf("%s = %v (%v), want %v; all %v", k, v, ok, want, got)
		}
	}
	if !math.IsInf(got["x"], -1) {
		t.Fatalf("x = %v", got["x"])
	}
	for name, bad := range map[string]string{
		"bad value":    "x one\n",
		"no value":     "x\n",
		"bad name":     "1x 1\n",
		"bad label":    "x{1a=\"b\"} 1\n",
		"unterminated": "x{a=\"b} 1\n",
		"twice":        "x 1\nx 2\n",
		"bad ts":       "x 1 soon\n",
		"extra":        "x 1 2 3\n",
		"html":         "<html>\n",
		"no comma":     "x{a=\"b\" c=\"d\"} 1\n",
		"unquoted":     "x{a=b} 1\n",
	} {
		if _, err := ParseScrape(strings.NewReader(bad)); err == nil {
			t.Errorf("%s: ParseScrape accepted %q", name, bad)
		}
	}
}
