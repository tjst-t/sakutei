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

package metrics_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/providers/sinks/metrics"
)

func freeAddr(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func scrape(t *testing.T, addr, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/metrics", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// E-H3：CycleDone から周の件数・時間・予算の残りを、今の一覧から firing の数を
// 出す。数えていない値は出さない（不変条件 29）。読むだけのトークンを求める。
// 設定を読み直しても同じ送り先（値と待ち受け）を返す。
func TestMetrics(t *testing.T) {
	addr := freeAddr(t)
	tok := filepath.Join(t.TempDir(), "tokens")
	os.WriteFile(tok, []byte("reader\n"), 0o600)
	settings := map[string]any{"listen": addr, "auth": map[string]any{"tokens_file": tok}}
	s1, err := metrics.Module(settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s1.(*metrics.Sink).Close() })
	if s1.(*metrics.Sink).Addr() != "" {
		t.Fatal("送る前に待ち受けた（plan・status で待ち受けない）")
	}
	ctx := context.Background()
	at := time.Unix(1_800_000_000, 0)
	if err := s1.Deliver(ctx, []core.Event{{Seq: 1, Kind: "Drift", Type: core.StateEvent, State: core.Firing}}); err != nil {
		t.Fatal(err)
	}
	code, body := scrape(t, addr, "reader")
	if code != 200 || strings.Contains(body, "sakutei_cycles_total") || strings.Contains(body, "sakutei_firing") {
		t.Fatalf("周の無いうちに周の値を出した: %d\n%s", code, body)
	}
	if code, _ := scrape(t, addr, ""); code != http.StatusUnauthorized {
		t.Fatalf("トークンなし: %d", code)
	}

	s2, err := metrics.Module(settings)
	if err != nil || s2 != s1 {
		t.Fatalf("読み直しで別の送り先: %v", err)
	}
	cycle := core.Event{Seq: 2, Kind: "CycleDone", Type: core.IncidentEvent, At: at,
		Values: map[string]int{"duration_ms": 1500, "drift": 2, "resources": 5, "halts": 0, "budget_remaining.platform": 3}}
	if err := s2.Deliver(ctx, []core.Event{cycle}); err != nil {
		t.Fatal(err)
	}
	if err := s2.(core.StateSink).DeliverState(ctx, []core.Event{{Kind: "Drift", Stuck: true}, {Kind: "Drift"}, {Kind: "Unknown"}}); err != nil {
		t.Fatal(err)
	}
	_, body = scrape(t, addr, "reader")
	for _, want := range []string{
		"sakutei_cycles_total 1\n",
		"sakutei_last_cycle_timestamp_seconds 1800000000\n",
		"sakutei_cycle_duration_seconds 1.5\n",
		`sakutei_resources{state="drift"} 2`,
		`sakutei_resources{state="unknown"} 0`,
		"sakutei_declared_resources 5\n",
		`sakutei_budget_remaining{policy="platform"} 3`,
		`sakutei_firing{kind="Drift"} 2`,
		`sakutei_firing{kind="Unknown"} 1`,
		`sakutei_firing_stuck{kind="Drift"} 1`,
		`sakutei_events_total{kind="CycleDone"} 1`,
		"# TYPE sakutei_cycles_total counter",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("%q が無い:\n%s", want, body)
		}
	}
	if strings.Contains(body, "budget_window_remaining") {
		t.Errorf("載っていない予算の窓を出した:\n%s", body)
	}

	// 設定が変われば前の待ち受けを止めて作り直す。
	s3, err := metrics.Module(map[string]any{"listen": addr})
	if err != nil || s3 == s1 {
		t.Fatalf("設定が変わっても同じ送り先: %v", err)
	}
	t.Cleanup(func() { s3.(*metrics.Sink).Close() })
	if err := s3.Deliver(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if code, _ := scrape(t, addr, ""); code != 200 {
		t.Fatalf("作り直した待ち受け: %d", code)
	}
}

func TestMetricsRejectsSettings(t *testing.T) {
	for _, c := range []struct {
		settings map[string]any
		want     string
	}{
		{map[string]any{}, "listen に"},
		{map[string]any{"listen": "0.0.0.0:9464"}, "plaintext: true"},
		{map[string]any{"listen": "127.0.0.1:9464", "tls": map[string]any{"cert": "a"}}, "cert と key を両方"},
		{map[string]any{"listen": "127.0.0.1:9464", "tls": map[string]any{"client_ca": "a"}}, "client_ca は cert と key"},
		{map[string]any{"listen": "127.0.0.1:9464", "path": "/m"}, "知らない項目 path"},
	} {
		if _, err := metrics.Module(c.settings); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v（%q を含むはず）", c.settings, err, c.want)
		}
	}
}

// 待ち受けられない（宛先が使われている）ときは誤りを返し、周を止めない。
func TestMetricsListenFails(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	s, err := metrics.Module(map[string]any{"listen": ln.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.(*metrics.Sink).Close() })
	if err := s.Deliver(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "metrics の待ち受け") {
		t.Fatalf("%v", err)
	}
}
