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

package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/events"
	"github.com/tjst-t/sakutei/v3/providers/sinks/metrics"
)

// syslogReceiver は偽の syslog の受け手（octet-counting の TCP）。
type syslogReceiver struct {
	ln   net.Listener
	mu   sync.Mutex
	msgs []string
}

func newSyslogReceiver(t *testing.T) *syslogReceiver {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &syslogReceiver{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					n, err := br.ReadString(' ')
					if err != nil {
						return
					}
					size, _ := strconv.Atoi(strings.TrimSpace(n))
					buf := make([]byte, size)
					if _, err := io.ReadFull(br, buf); err != nil {
						return
					}
					r.mu.Lock()
					r.msgs = append(r.msgs, string(buf))
					r.mu.Unlock()
				}
			}()
		}
	}()
	return r
}

func (r *syslogReceiver) all() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.msgs, "\n")
}

// 完了条件（#223）：sink が落ちていても・戻らなくても 1 cycle の処理を止めない
// （不変条件 28）。届く sink には送る。落ちた sink は位置を保ち、戻ったら続きから
// 送る。syslog は streams: [events, audit] で Events と audit のハッシュチェーンの
// 両方を送る（E-H2・E-H3・E-L1）。
func TestSinksDoNotStopCycle(t *testing.T) {
	defer func(d time.Duration) { events.DeliverTimeout = d }(events.DeliverTimeout)
	events.DeliverTimeout = 300 * time.Millisecond

	w := t.TempDir()
	logs := newSyslogReceiver(t)
	// 戻らない OTLP の受け手。
	hang := make(chan struct{})
	otel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-hang }))
	defer otel.Close()
	defer close(hang)
	// 落ちている Alertmanager（あとで同じ宛先で起こす）。
	amLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	amAddr := amLn.Addr().String()
	amLn.Close()
	metricsAddr := freeAddr(t)

	state := filepath.Join(w, "state.json")
	os.WriteFile(state, []byte(`{"dns/record/stray": {"availability": "present", "fields": {"value": "192.0.2.50"}}}`), 0o644)
	cfg := `providers:
  dns:
    module: fake
    settings:
      state: ` + state + `
      kinds:
        record:
          fields:
            value: string
sinks:
  am:
    module: alertmanager
    settings: {urls: ["http://` + amAddr + `"]}
  log:
    module: syslog
    settings: {address: "tcp://` + logs.ln.Addr().String() + `"}
    streams: [events, audit]
  otel:
    module: otlp
    settings: {endpoint: "` + otel.URL + `"}
  prom:
    module: metrics
    settings: {listen: "` + metricsAddr + `"}
`
	os.WriteFile(filepath.Join(w, "config.yaml"), []byte(cfg), 0o644)
	os.MkdirAll(filepath.Join(w, "intent"), 0o755)
	os.WriteFile(filepath.Join(w, "intent", "dns.yaml"), []byte(`ranges:
  - provider: dns
resources:
  - type: dns/record
    id: www
    fields: {value: 192.0.2.10}
`), 0o644)
	run := func() (int, string) {
		var out, errOut bytes.Buffer
		code := cli.Run(context.Background(), []string{"reconcile", "--intent", filepath.Join(w, "intent"), "--config", filepath.Join(w, "config.yaml"),
			"--record", filepath.Join(w, "record")}, &out, &errOut, Options()...)
		return code, out.String() + errOut.String()
	}
	start := time.Now()
	code, out := run()
	if code != cli.ExitOK || time.Since(start) > 10*time.Second {
		t.Fatalf("1 cycle 目: %d（%s）\n%s", code, time.Since(start), out)
	}
	for _, want := range []string{"sink am: 送れない", "sink otel: 送れない"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q が無い:\n%s", want, out)
		}
	}
	if strings.Contains(out, "sink log") || strings.Contains(out, "sink prom") {
		t.Errorf("届く sink のエラー:\n%s", out)
	}
	got := logs.all()
	for _, want := range []string{"resource.apply [meta", `"kind":"Applied"`, `"kind":"CycleDone"`} {
		if !strings.Contains(got, want) {
			t.Errorf("syslog に %q が無い:\n%s", want, got)
		}
	}
	resp, err := http.Get("http://" + metricsAddr + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "sakutei_cycles_total 1") || !strings.Contains(string(body), `sakutei_firing{kind="Unmanaged"} 1`) {
		t.Errorf("/metrics:\n%s", body)
	}

	// Alertmanager が戻れば、1 cycle 目の Event から送る。
	var mu sync.Mutex
	var alerts []string
	amLn, err = net.Listen("tcp", amAddr)
	if err != nil {
		t.Skipf("同じアドレスで listen できない: %v", err)
	}
	am := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		alerts = append(alerts, string(b))
		mu.Unlock()
	})}
	go am.Serve(amLn)
	defer am.Close()
	if code, out := run(); code != cli.ExitOK || strings.Contains(out, "sink am") {
		t.Fatalf("2 cycle 目: %d\n%s", code, out)
	}
	mu.Lock()
	all := strings.Join(alerts, "\n")
	mu.Unlock()
	if !strings.Contains(all, `"alertname":"Unmanaged"`) || !strings.Contains(all, `"resource":"dns/record/stray"`) {
		t.Errorf("Alertmanager に送っていない:\n%s", all)
	}
	if s, err := metrics.Module(map[string]any{"listen": metricsAddr}); err == nil {
		s.(*metrics.Sink).Close()
	}
}

func freeAddr(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}
