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

package otlp_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/providers/sinks/otlp"
)

// E-H3・N-H5：CycleDone を周の span に、その前の出来事型を子の span にして
// OTLP/HTTP の JSON で送る。ID は Event の番号から決まり、送り直しても同じ。欄の
// 値（Drift の message）は載せない。受け手が落ちれば誤り。
func TestOTLP(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "no", http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
	}))
	defer srv.Close()
	s, err := otlp.Module(map[string]any{"endpoint": srv.URL, "service_name": "sakutei-tokyo"})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1_800_000_000, 0)
	batch := []core.Event{
		{Seq: 1, Kind: "Drift", Type: core.StateEvent, State: core.Firing, Resource: "dns/record/www", Message: "value: 192.0.2.10 → 192.0.2.11", At: at},
		{Seq: 2, Kind: "Applied", Type: core.IncidentEvent, Resource: "dns/record/www", Labels: map[string]string{"site": "a"}, At: at.Add(-time.Second)},
		{Seq: 3, Kind: "CycleDone", Type: core.IncidentEvent, At: at, Values: map[string]int{"duration_ms": 2000, "drift": 1, "failed": 1}},
		{Seq: 4, Kind: "Deleted", Type: core.IncidentEvent, Resource: "dns/record/old", At: at},
	}
	ctx := context.Background()
	if err := s.Deliver(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if err := s.Deliver(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || bodies[0] != bodies[1] {
		t.Fatalf("送り直して違う ID:\n%v", bodies)
	}
	if strings.Contains(bodies[0], "192.0.2") {
		t.Fatalf("欄の値を載せた: %s", bodies[0])
	}
	var req struct {
		ResourceSpans []struct {
			Resource struct {
				Attributes []struct {
					Key   string `json:"key"`
					Value struct {
						String string `json:"stringValue"`
					} `json:"value"`
				} `json:"attributes"`
			} `json:"resource"`
			ScopeSpans []struct {
				Spans []otlp.Span `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal([]byte(bodies[0]), &req); err != nil {
		t.Fatal(err)
	}
	if a := req.ResourceSpans[0].Resource.Attributes[0]; a.Key != "service.name" || a.Value.String != "sakutei-tokyo" {
		t.Errorf("resource: %+v", a)
	}
	spans := req.ResourceSpans[0].ScopeSpans[0].Spans
	if len(spans) != 3 {
		t.Fatalf("周・Applied・残った Deleted の 3 つ: %+v", spans)
	}
	cyc, apply, del := spans[0], spans[1], spans[2]
	if cyc.Name != "sakutei.cycle" || cyc.Start != "1799999998000000000" || cyc.End != "1800000000000000000" || cyc.Status.Code != 2 || len(cyc.TraceID) != 32 || len(cyc.SpanID) != 16 {
		t.Errorf("周の span: %+v", cyc)
	}
	if apply.Name != "sakutei.apply" || apply.TraceID != cyc.TraceID || apply.ParentSpanID != cyc.SpanID || apply.Start != apply.End {
		t.Errorf("子の span: %+v", apply)
	}
	if del.Name != "sakutei.delete" || del.ParentSpanID != "" || del.TraceID == cyc.TraceID {
		t.Errorf("周の無い出来事は根の span: %+v", del)
	}

	srv.Close()
	if err := s.Deliver(ctx, batch); err == nil {
		t.Fatal("受け手が落ちても送れたとした")
	}
	// span の無い batch は送らない。
	if err := s.Deliver(ctx, batch[:1]); err != nil {
		t.Fatalf("状態型だけ: %v", err)
	}
}

func TestOTLPRejectsSettings(t *testing.T) {
	for _, c := range []struct {
		settings map[string]any
		want     string
	}{
		{map[string]any{}, "endpoint に"},
		{map[string]any{"endpoint": "http://otel.example.com:4318"}, "plaintext: true"},
		{map[string]any{"endpoint": "grpc://otel:4317"}, "http(s)://host:port"},
		{map[string]any{"endpoint": "https://otel:4318", "headers": map[string]any{}}, "知らない項目 headers"},
	} {
		if _, err := otlp.Module(c.settings); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v（%q を含むはず）", c.settings, err, c.want)
		}
	}
	s, err := otlp.Module(map[string]any{"endpoint": "http://otel.example.com:4318/custom", "plaintext": true})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.(*otlp.Sink).Spans([]core.Event{{Seq: 9, Kind: "CycleDone", Type: core.IncidentEvent}}); len(got) != 1 {
		t.Fatalf("%+v", got)
	}
}
