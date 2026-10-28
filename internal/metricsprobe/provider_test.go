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

// provider_test.go — metrics-probe の契約（#149）。httptest の /metrics を相手に。
//
//	[AC-149-1-1] 宣言したメトリクス（名前とラベルの一致）の値を観測し、比べ方の
//	             結果を ready.<名前> と Observation.Ready に出す。sum・max で集計する。
//	[AC-149-1-2] 届かない・2xx 以外・形が壊れている・メトリクスが無い・集計なしで
//	             2 本以上が一致・NaN は unknown（エラー）。absent（Ok で State が
//	             nil）にしない。
//	[AC-149-1-3] 観測だけ: ObserveOnly・List なし・Apply は PermanentError。intent に
//	             書けるのは設定にある probe の ready.<名前> だけ。
//	[AC-149-1-4] TLS は ca_file で確かめ、insecure は明示したときだけ。資格は
//	             SecretRef で、取得の直前に解決する（bearer・basic）。
//	[AC-149-1-5] 設定の誤り（未知のキー・URL に資格・比べ方の書き間違い・生の資格）
//	             は読み込みで断る。
package metricsprobe

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
)

// page は差し替えられる /metrics。
type page struct {
	mu     sync.Mutex
	body   string
	status int
	auth   string // 要る Authorization（空なら問わない）
}

func (p *page) set(body string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.body, p.status = body, http.StatusOK
}

func (p *page) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.auth != "" && r.Header.Get("Authorization") != p.auth {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if p.status != http.StatusOK {
		w.WriteHeader(p.status)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(p.body))
}

const queuePage = `# HELP rabbitmq_queue_messages Messages in the queue.
# TYPE rabbitmq_queue_messages gauge
rabbitmq_queue_messages{vhost="/",queue="orders"} 1200
rabbitmq_queue_messages{vhost="/",queue="audit"} 30
rabbitmq_queue_messages{vhost="b",queue="orders"} 5
# a histogram without TYPE order, as other exporters write it
http_request_duration_seconds_bucket{le="0.1"} 3
http_request_duration_seconds_bucket{le="+Inf"} 4
up 1
`

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func newProvider(t *testing.T, cfgText string) *Provider {
	t.Helper()
	dir := t.TempDir()
	p, err := NewFromConfig(writeFile(t, dir, "probes.yaml", cfgText))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func probesConfig(url string) string {
	return `endpoints:
  mq:
    url: ` + url + `
probes:
  orders:
    endpoint: mq
    metric: rabbitmq_queue_messages
    labels:
      vhost: /
      queue: orders
    ready:
      below_limit: "< 1000"
      not_empty: "> 0"
  orders-all:
    endpoint: mq
    metric: rabbitmq_queue_messages
    labels:
      queue: orders
    aggregate: sum
    ready:
      below_limit: "<= 1205"
  biggest:
    endpoint: mq
    metric: rabbitmq_queue_messages
    aggregate: max
  ambiguous:
    endpoint: mq
    metric: rabbitmq_queue_messages
    labels:
      queue: orders
  up:
    endpoint: mq
    metric: up
    labels:
      job: ""
    ready:
      up: "= 1"
  missing:
    endpoint: mq
    metric: no_such_metric
`
}

func field(obs core.Observation, name string) string {
	tv, ok := obs.State[name]
	if !ok {
		return "<none>"
	}
	s, _ := tv.Value.(string)
	return s
}

func TestRead_Values(t *testing.T) {
	// [AC-149-1-1]
	pg := &page{}
	pg.set(queuePage)
	srv := httptest.NewServer(pg)
	defer srv.Close()
	p := newProvider(t, probesConfig(srv.URL+"/metrics"))
	ctx := context.Background()

	obs, err := p.Read(ctx, "orders")
	if err != nil || obs.Availability != core.AvailabilityOk {
		t.Fatalf("Read orders: %v %+v", err, obs)
	}
	if field(obs, FieldValue) != "1200" || field(obs, FieldSeries) != "1" || field(obs, FieldProbe) != "orders" ||
		field(obs, FieldMetric) != "rabbitmq_queue_messages" {
		t.Fatalf("[AC-149-1-1] state = %v", obs.State)
	}
	if field(obs, "ready.below_limit") != "false" || field(obs, "ready.not_empty") != "true" ||
		obs.Ready["below_limit"] || !obs.Ready["not_empty"] {
		t.Fatalf("[AC-149-1-1] ready = %v state %v", obs.Ready, obs.State)
	}

	// 下がれば満たす（観測を持たない: 毎回取り直す）。
	pg.set(strings.Replace(queuePage, "1200", "999.5", 1))
	obs, err = p.Read(ctx, "orders")
	if err != nil || field(obs, FieldValue) != "999.5" || !obs.Ready["below_limit"] || field(obs, "ready.below_limit") != "true" {
		t.Fatalf("[AC-149-1-1] after the drop: %v %v %v", err, obs.State, obs.Ready)
	}

	for id, want := range map[string]string{"orders-all": "1004.5", "biggest": "999.5", "up": "1"} {
		obs, err := p.Read(ctx, id)
		if err != nil || field(obs, FieldValue) != want {
			t.Fatalf("[AC-149-1-1] %s: %v value %s, want %s", id, err, field(obs, FieldValue), want)
		}
	}
	if obs, _ := p.Read(ctx, "orders-all"); field(obs, FieldSeries) != "2" || !obs.Ready["below_limit"] {
		t.Fatalf("[AC-149-1-1] orders-all: %v %v", obs.State, obs.Ready)
	}
}

func isTransient(err error) bool {
	var te *provider.TransientError
	return errors.As(err, &te)
}

func assertUnknown(t *testing.T, p *Provider, id, wantErr string) {
	t.Helper()
	obs, err := p.Read(context.Background(), id)
	if err == nil || obs.Availability == core.AvailabilityOk || obs.State != nil {
		t.Fatalf("[AC-149-1-2] %s: err %v obs %+v, want unknown", id, err, obs)
	}
	if !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("[AC-149-1-2] %s: err %v, want %q", id, err, wantErr)
	}
	if !isTransient(err) {
		t.Fatalf("[AC-149-1-2] %s: err %v is not transient", id, err)
	}
}

func TestRead_Unknown(t *testing.T) {
	// [AC-149-1-2]
	pg := &page{}
	pg.set(queuePage)
	srv := httptest.NewServer(pg)
	p := newProvider(t, probesConfig(srv.URL+"/metrics"))

	assertUnknown(t, p, "missing", "no series of no_such_metric")
	assertUnknown(t, p, "ambiguous", "2 series match")

	pg.set("rabbitmq_queue_messages{queue=\"orders\",vhost=\"/\"} NaN\n")
	assertUnknown(t, p, "orders", "NaN")
	pg.set("rabbitmq_queue_messages{queue=\"orders\",vhost=\"/\" 12\n")
	assertUnknown(t, p, "orders", "not the text exposition format")
	pg.set("rabbitmq_queue_messages{queue=\"orders\",vhost=\"/\"} twelve\n")
	assertUnknown(t, p, "orders", "not the text exposition format")
	pg.set("<html>Service Unavailable</html>\n")
	assertUnknown(t, p, "orders", "not the text exposition format")

	pg.mu.Lock()
	pg.status = http.StatusServiceUnavailable
	pg.mu.Unlock()
	assertUnknown(t, p, "orders", "503")

	srv.Close()
	assertUnknown(t, p, "orders", "GET")
}

func TestRead_RedirectNotFollowed(t *testing.T) {
	// 資格を別のホストへ運ばない: 3xx は観測できない。
	srv := httptest.NewServer(http.RedirectHandler("http://192.0.2.1/metrics", http.StatusFound))
	defer srv.Close()
	p := newProvider(t, probesConfig(srv.URL+"/metrics"))
	assertUnknown(t, p, "orders", "302")
}

func TestObserveOnly(t *testing.T) {
	// [AC-149-1-3]
	p := newProvider(t, probesConfig("http://127.0.0.1:1/metrics"))
	caps := p.Capabilities()
	if !caps.ObserveOnly || caps.List || caps.Pruneable || caps.BulkObserve {
		t.Fatalf("[AC-149-1-3] caps = %+v", caps)
	}
	if err := sdk.ValidateProvider(p); err != nil {
		t.Fatalf("[AC-149-1-3] ValidateProvider: %v", err)
	}
	if _, ok := any(p).(provider.Lister); ok {
		t.Fatalf("[AC-149-1-3] metrics-probe must not list")
	}
	err := p.Apply(context.Background(), core.Plan{Ops: []core.Op{{Kind: core.OpKindUpdate, Target: core.ID{Provider: Name, Local: "orders"}}}})
	if err == nil || isTransient(err) {
		t.Fatalf("[AC-149-1-3] Apply = %v, want a permanent error", err)
	}
	if err := p.Apply(context.Background(), core.Plan{}); err != nil {
		t.Fatalf("[AC-149-1-3] empty Apply = %v", err)
	}
	if _, err := p.Read(context.Background(), "nope"); err == nil || isTransient(err) {
		t.Fatalf("[AC-149-1-3] Read of an unknown probe = %v", err)
	}

	for _, ok := range []map[string]string{nil, {"ready.below_limit": "true"}, {"ready.not_empty": "false"}} {
		if err := p.ValidateIntent("orders", ok); err != nil {
			t.Fatalf("[AC-149-1-3] ValidateIntent(%v) = %v", ok, err)
		}
	}
	for id, bad := range map[string]map[string]string{
		"nope":   nil,
		"orders": {"value": "5"},
	} {
		if err := p.ValidateIntent(id, bad); err == nil {
			t.Fatalf("[AC-149-1-3] ValidateIntent(%s, %v) accepted", id, bad)
		}
	}
	if err := p.ValidateIntent("orders", map[string]string{"ready.up": "true"}); err == nil {
		t.Fatalf("[AC-149-1-3] another probe's condition was accepted")
	}
	if err := p.ValidateIntent("orders", map[string]string{"ready.below_limit": "yes"}); err == nil {
		t.Fatalf("[AC-149-1-3] a non-boolean was accepted")
	}
	if _, ok := p.Schema().Fields["ready.up"]; !ok {
		t.Fatalf("[AC-149-1-3] schema = %v", p.Schema().Fields)
	}
}

func TestTLSAndCredentials(t *testing.T) {
	// [AC-149-1-4]
	pg := &page{auth: "Bearer s3cr3t-token"}
	pg.set(queuePage)
	srv := httptest.NewTLSServer(pg)
	defer srv.Close()
	dir := t.TempDir()
	writeFile(t, dir, "ca.pem", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})))
	writeFile(t, dir, "secrets.json", `{"secrets": {"mq/token": {"gen": 1, "value": "s3cr3t-token"}, "mq/pw": {"gen": 1, "value": "pa55"}}}`)
	load := func(ep string) (*Provider, error) {
		return NewFromConfig(writeFile(t, dir, "probes.yaml", `secret_store: secrets.json
endpoints:
  mq:
    url: `+srv.URL+`/metrics
`+ep+`
probes:
  orders:
    endpoint: mq
    metric: rabbitmq_queue_messages
    labels:
      vhost: /
      queue: orders
`))
	}
	read := func(p *Provider) (core.Observation, error) { return p.Read(context.Background(), "orders") }

	// ca_file と bearer: 読める。
	p, err := load("    ca_file: ca.pem\n    bearer_token: secretref:file/mq/token")
	if err != nil {
		t.Fatal(err)
	}
	if obs, err := read(p); err != nil || field(obs, FieldValue) != "1200" {
		t.Fatalf("[AC-149-1-4] ca_file + bearer: %v %v", err, obs.State)
	}
	// ca_file が無ければ確かめられない: unknown。
	p, err = load("    bearer_token: secretref:file/mq/token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := read(p); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("[AC-149-1-4] without ca_file: %v", err)
	}
	// insecure は明示したときだけ。
	p, err = load("    insecure_skip_verify: true\n    bearer_token: secretref:file/mq/token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := read(p); err != nil {
		t.Fatalf("[AC-149-1-4] insecure_skip_verify: %v", err)
	}
	// 資格が違えば 401: unknown。資格の値はエラーに出さない。
	p, err = load("    ca_file: ca.pem\n    username: sakutei\n    password: secretref:file/mq/pw")
	if err != nil {
		t.Fatal(err)
	}
	_, err = read(p)
	if err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "pa55") {
		t.Fatalf("[AC-149-1-4] basic against bearer: %v", err)
	}
	pg.mu.Lock()
	pg.auth = "Basic c2FrdXRlaTpwYTU1" // sakutei:pa55
	pg.mu.Unlock()
	if obs, err := read(p); err != nil || field(obs, FieldValue) != "1200" {
		t.Fatalf("[AC-149-1-4] basic: %v %v", err, obs.State)
	}
	// 取得の直前に解決する: store から消えれば unknown。
	writeFile(t, dir, "secrets.json", `{"secrets": {"mq/token": {"gen": 1, "value": "x"}}}`)
	if _, err := read(p); err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("[AC-149-1-4] unresolvable password: %v", err)
	}
}

func TestConfigErrors(t *testing.T) {
	// [AC-149-1-5]
	dir := t.TempDir()
	writeFile(t, dir, "secrets.json", `{"secrets": {"t": {"gen": 1, "value": "v"}}}`)
	base := func(ep, probe string) string {
		return "secret_store: secrets.json\nendpoints:\n  mq:\n    url: https://mq.example.net/metrics\n" + ep +
			"probes:\n  p:\n    endpoint: mq\n    metric: m\n" + probe
	}
	for name, text := range map[string]string{
		"unknown key":       base("    nope: 1\n", ""),
		"url credentials":   strings.Replace(base("", ""), "https://", "https://u:p@", 1),
		"not http":          strings.Replace(base("", ""), "https://", "ftp://", 1),
		"raw token":         base("    bearer_token: s3cr3t\n", ""),
		"token not stored":  base("    bearer_token: secretref:file/missing\n", ""),
		"user without pw":   base("    username: u\n", ""),
		"ca and insecure":   base("    ca_file: ca.pem\n    insecure_skip_verify: true\n", ""),
		"insecure not bool": base("    insecure_skip_verify: \"yes\"\n", ""),
		"bad timeout":       base("    timeout: soon\n", ""),
		"bad aggregate":     base("", "    aggregate: avg\n"),
		"bad comparison":    base("", "    ready:\n      ok: \"under 1000\"\n"),
		"NaN comparison":    base("", "    ready:\n      ok: \"< NaN\"\n"),
		"bad label":         base("", "    labels:\n      __name__: x\n"),
		"bad metric":        strings.Replace(base("", ""), "metric: m", "metric: 1m", 1),
		"unknown endpoint":  strings.Replace(base("", ""), "endpoint: mq", "endpoint: other", 1),
		"probe unknown key": base("", "    labls:\n      a: b\n"),
		// [AC-186-8-1] observe-only: no write credential to split (#186).
		"write credential": base("    write:\n      bearer_token: secretref:file/t\n", ""),
		"observe section":  base("    observe:\n      bearer_token: secretref:file/t\n", ""),
	} {
		if _, err := LoadConfig(writeFile(t, dir, "c.yaml", text)); err == nil {
			t.Errorf("[AC-149-1-5] %s: accepted\n%s", name, text)
		} else if strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("[AC-149-1-5] %s: the error shows the raw credential: %v", name, err)
		}
	}
	if _, err := LoadConfig(writeFile(t, dir, "c.yaml", base("    bearer_token: secretref:file/t\n    timeout: 3s\n", "    aggregate: sum\n    ready:\n      ok: \"≤ 10\"\n"))); err != nil {
		t.Fatalf("[AC-149-1-5] a valid config: %v", err)
	}
}

func TestParseCond(t *testing.T) {
	for _, c := range []struct {
		in   string
		want string
		v    float64
		hold bool
	}{
		{"< 1000", "< 1000", 999, true},
		{"<1000", "< 1000", 1000, false},
		{"<= 1000", "<= 1000", 1000, true},
		{"≤ 1000", "<= 1000", 1000.5, false},
		{"> 0", "> 0", 0, false},
		{">= 5", ">= 5", 5, true},
		{"≥ 5", ">= 5", 4, false},
		{"= 0", "= 0", 0, true},
		{"== 0", "= 0", 1, false},
		{"0", "= 0", 0, true},
		{"< 1e3", "< 1000", 2, true},
		{" < -0.5 ", "< -0.5", -1, true},
	} {
		got, err := ParseCond(c.in)
		if err != nil {
			t.Fatalf("ParseCond(%q): %v", c.in, err)
		}
		if got.String() != c.want || got.Holds(c.v) != c.hold {
			t.Fatalf("ParseCond(%q) = %s, Holds(%v) = %v", c.in, got, c.v, got.Holds(c.v))
		}
	}
	for _, bad := range []string{"", "<", "< x", "< Inf", "< NaN", "!= 3", "< 0x10", "< 1_000", "< 1000 messages"} {
		if _, err := ParseCond(bad); err == nil {
			t.Fatalf("ParseCond(%q) accepted", bad)
		}
	}
}
