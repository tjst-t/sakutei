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

package otlpexport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/tracing"
)

type receiver struct {
	mu   sync.Mutex
	reqs []*http.Request
	body []exportRequest
	code int
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	data, _ := io.ReadAll(req.Body)
	var er exportRequest
	_ = json.Unmarshal(data, &er)
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.body = append(r.body, er)
	code := r.code
	r.mu.Unlock()
	if code == 0 {
		code = 200
	}
	w.WriteHeader(code)
}

func (r *receiver) spans() []span {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []span
	for _, b := range r.body {
		for _, rs := range b.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				out = append(out, ss.Spans...)
			}
		}
	}
	return out
}

func attr(s span, key string) (anyValue, bool) {
	for _, kv := range s.Attributes {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return anyValue{}, false
}

func TestExporter_SendsParentAndChildOverJSON(t *testing.T) {
	rcv := &receiver{}
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	cfg, err := Load(srv.URL+"/otel/", "", false)
	if err != nil {
		t.Fatal(err)
	}
	exp, err := New(cfg, Resource{ServiceVersion: "test"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx := tracing.WithTracer(context.Background(), tracing.NewTracer(exp))
	ctx, root := tracing.Start(ctx, tracing.SpanCycle, tracing.Int(tracing.AttrCycle, 7))
	_, child := tracing.Start(ctx, tracing.SpanApply, tracing.Strings(tracing.AttrResources, []string{"fake:/web"}),
		tracing.Bool(tracing.AttrPrune, false))
	child.Fail(errors.New("boom"))
	child.End()
	root.End()
	root.End() // twice: sent once
	exp.Shutdown(5 * time.Second)

	got := rcv.spans()
	if len(got) != 2 {
		t.Fatalf("spans = %d, want 2", len(got))
	}
	if rcv.reqs[0].URL.Path != "/otel/v1/traces" || rcv.reqs[0].Header.Get("Content-Type") != "application/json" {
		t.Errorf("request %s %q", rcv.reqs[0].URL.Path, rcv.reqs[0].Header.Get("Content-Type"))
	}
	c, r := got[0], got[1]
	if c.Name != tracing.SpanApply || r.Name != tracing.SpanCycle {
		t.Fatalf("names %q %q", c.Name, r.Name)
	}
	if c.TraceID != r.TraceID || c.ParentSpanID != r.SpanID || r.ParentSpanID != "" || len(r.TraceID) != 32 || len(r.SpanID) != 16 {
		t.Errorf("ids: child %+v root %+v", c, r)
	}
	if c.Status.Code != statusError || c.Status.Message != "boom" || r.Status.Code != statusOK {
		t.Errorf("status: child %+v root %+v", c.Status, r.Status)
	}
	if v, ok := attr(r, tracing.AttrCycle); !ok || v.IntValue == nil || *v.IntValue != "7" {
		t.Errorf("cycle attr %+v", v)
	}
	if v, ok := attr(c, tracing.AttrResources); !ok || v.ArrayValue == nil || *v.ArrayValue.Values[0].StringValue != "fake:/web" {
		t.Errorf("resources attr %+v", v)
	}
	res := rcv.body[0].ResourceSpans[0].Resource.Attributes
	if len(res) == 0 || res[0].Key != "service.name" || *res[0].Value.StringValue != "sakutei" {
		t.Errorf("resource attrs %+v", res)
	}
}

func TestExporter_UnreachableNeverBlocksAndToldOnce(t *testing.T) {
	cfg, err := Load("http://127.0.0.1:1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Timeout = 200 * time.Millisecond
	var log bytes.Buffer
	var mu sync.Mutex
	exp, err := New(cfg, Resource{}, writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return log.Write(p)
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctx := tracing.WithTracer(context.Background(), tracing.NewTracer(exp))
	start := time.Now()
	for i := 0; i < QueueSize+100; i++ {
		_, s := tracing.Start(ctx, tracing.SpanRead)
		s.End()
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("ending spans took %s while the receiver is down", d)
	}
	exp.Shutdown(5 * time.Second)
	if exp.Dropped() == 0 || exp.Sent() != 0 {
		t.Errorf("sent %d dropped %d", exp.Sent(), exp.Dropped())
	}
	mu.Lock()
	defer mu.Unlock()
	if n := strings.Count(log.String(), "cannot send"); n != 1 {
		t.Errorf("told %d times:\n%s", n, log.String())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestNoTracer_NoSpans(t *testing.T) {
	ctx, s := tracing.Start(context.Background(), tracing.SpanCycle)
	if s != nil || tracing.Enabled(ctx) {
		t.Fatal("a span without a tracer")
	}
	s.SetAttributes(tracing.String("k", "v"))
	s.Fail(errors.New("x"))
	s.End()
}

func TestLoad_Refusals(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	store := write("secrets.json", `{"secrets":{"sakutei/otlp_token":{"gen":1,"value":"tok-189"}}}`)
	_ = store
	tokenCfg := write("otlp.yaml", "secret_store: secrets.json\nbearer_token: secretref:file/sakutei/otlp_token\n")
	for _, tc := range []struct {
		name, endpoint, config string
		insecure               bool
		want                   string // "" = accepted
	}{
		{"off", "", "", false, ""},
		{"config without endpoint", "", tokenCfg, false, "go with --otlp-endpoint"},
		{"loopback plaintext", "http://127.0.0.1:4318", "", false, ""},
		{"localhost plaintext", "http://localhost:4318", "", false, ""},
		{"remote plaintext", "http://collector.example:4318", "", false, "takes --otlp-insecure"},
		{"remote plaintext insecure", "http://collector.example:4318", "", true, ""},
		{"remote https without authorization", "https://collector.example:4318", "", false, "authorization"},
		{"remote https with token", "https://collector.example:4318", tokenCfg, false, ""},
		{"remote http with token", "http://collector.example:4318", tokenCfg, false, "TLS (https)"},
		{"bad scheme", "grpc://127.0.0.1:4317", "", false, "want http://"},
		{"credentials in url", "https://u:p@collector.example", "", false, "never written in the URL"},
		{"inline token", "https://collector.example", write("bad.yaml", "bearer_token: s3cr3t\n"), false, "must be a SecretRef"},
		{"tls on http", "http://127.0.0.1:4318", write("tls.yaml", "tls:\n  min_version: \"1.3\"\n"), false, "not https"},
		{"old tls", "https://127.0.0.1:4318", write("old.yaml", "tls:\n  min_version: \"1.1\"\n"), false, "1.2"},
		{"unknown key", "https://127.0.0.1:4318", write("unk.yaml", "headers: x\n"), false, "unknown key"},
		{"token without store", "https://127.0.0.1:4318", write("nostore.yaml", "bearer_token: secretref:file/sakutei/otlp_token\n"), false, "secret_store"},
		{"missing secret", "https://127.0.0.1:4318", write("miss.yaml", "secret_store: secrets.json\nbearer_token: secretref:file/sakutei/none\n"), false, "bearer_token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(tc.endpoint, tc.config, tc.insecure)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("err = %v, want %q", err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), "s3cr3t") {
				t.Errorf("the value is in the error: %v", err)
			}
		})
	}
}

func TestExporter_BearerToken(t *testing.T) {
	rcv := &receiver{}
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "secrets.json"), []byte(`{"secrets":{"t":{"gen":1,"value":"tok-189"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "otlp.yaml")
	if err := os.WriteFile(cfgPath, []byte("secret_store: secrets.json\nbearer_token: secretref:file/t\nservice_name: sakutei-tokyo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(srv.URL, cfgPath, false)
	if err != nil {
		t.Fatal(err)
	}
	exp, err := New(cfg, Resource{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx := tracing.WithTracer(context.Background(), tracing.NewTracer(exp))
	_, s := tracing.Start(ctx, tracing.SpanPlan)
	s.End()
	exp.Shutdown(5 * time.Second)
	if len(rcv.reqs) != 1 || rcv.reqs[0].Header.Get("Authorization") != "Bearer tok-189" {
		t.Fatalf("requests %d, auth %q", len(rcv.reqs), rcv.reqs[0].Header.Get("Authorization"))
	}
	if v := rcv.body[0].ResourceSpans[0].Resource.Attributes[0]; *v.Value.StringValue != "sakutei-tokyo" {
		t.Errorf("service.name %+v", v)
	}
}
