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

// otlp_trace_test.go — #189（OTLP のトレースの出口、§26.8）:
//
//	[AC-189-1] 既定は出さない：--otlp-endpoint が無ければ、OTEL_* の環境変数が
//	           あっても常駐・plan・reconcile は 1 つも送らない
//	[AC-189-2] 常駐は周ごとに 1 つのトレース（sakutei.cycle）を送り、その下に観測の段・
//	           Read・plan の段・Apply・Workflow と手順の span がある。欄の値は載らない
//	[AC-189-3] plan・reconcile は 1 回が 1 つのトレース（根は sakutei.plan・
//	           sakutei.reconcile、その下に Read・Apply）。終わる前に送り終える
//	[AC-189-4] 宛先の TLS と認可は読み取りの口（#161）と同じ考え：loopback 以外へ
//	           TLS と認可を欠いて送るのは --otlp-insecure のときだけで、無ければ起動で
//	           断る。トークンは SecretRef でしか書けず、https とトークンで送る
package acceptance

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// otlpSpan is the part of an OTLP/HTTP JSON span the tests look at.
type otlpSpan struct {
	TraceID      string `json:"traceId"`
	SpanID       string `json:"spanId"`
	ParentSpanID string `json:"parentSpanId"`
	Name         string `json:"name"`
	Attributes   []struct {
		Key   string         `json:"key"`
		Value map[string]any `json:"value"`
	} `json:"attributes"`
}

func (s otlpSpan) attr(key string) string {
	for _, a := range s.Attributes {
		if a.Key == key {
			for _, v := range a.Value {
				if str, ok := v.(string); ok {
					return str
				}
				b, _ := json.Marshal(v)
				return string(b)
			}
		}
	}
	return ""
}

// otlpReceiver is an OTLP/HTTP trace receiver for the tests.
type otlpReceiver struct {
	mu     sync.Mutex
	n      int
	bodies []string
	auth   []string
	spans  []otlpSpan
}

func (r *otlpReceiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	data, _ := io.ReadAll(req.Body)
	var er struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []otlpSpan `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	_ = json.Unmarshal(data, &er)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
	r.bodies = append(r.bodies, string(data))
	r.auth = append(r.auth, req.Header.Get("Authorization"))
	if req.URL.Path != "/v1/traces" || req.Header.Get("Content-Type") != "application/json" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	for _, rs := range er.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			r.spans = append(r.spans, ss.Spans...)
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (r *otlpReceiver) snapshot() (int, []otlpSpan, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n, append([]otlpSpan(nil), r.spans...), strings.Join(r.bodies, "\n")
}

func spanNames(spans []otlpSpan) map[string]int {
	m := map[string]int{}
	for _, s := range spans {
		m[s.Name]++
	}
	return m
}

// otlpIntent: web drifts (size), n1 is the target of a one-step Workflow.
const otlpIntent = `{
  "resources": [
    { "provider": "fake", "id": "web", "fields": { "name": "web", "size": "zz189-desired" } },
    { "provider": "fake", "id": "n1", "fields": { "name": "n1", "size": "2" } }
  ],
  "workflows": [
    { "workflow": "roll",
      "target": { "provider": "fake" },
      "stages": [ { "name": "s1", "targets": ["n1"] } ],
      "steps": [ { "apply": { "size": "2" } } ],
      "on_failure": { "policy": "halt" } }
  ]
}`

func newOTLPEnv(t *testing.T) (intentDir, statePath, root string) {
	t.Helper()
	root = t.TempDir()
	intentDir = filepath.Join(root, "intent")
	statePath = filepath.Join(root, "state.json")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(intentDir, "intent.json"), []byte(otlpIntent), 0o644); err != nil {
		t.Fatal(err)
	}
	seedState(t, statePath, map[string]map[string]any{
		"web": {"name": "web", "size": "zz189-observed"},
		"n1":  {"name": "n1", "size": "1"},
	})
	return
}

func runSakuteiEnv(t *testing.T, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(buildBinary(t), args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		t.Fatalf("run sakutei %v: %v", args, err)
	}
	return string(out), 0
}

func TestOTLP_OffByDefault(t *testing.T) {
	rcv := &otlpReceiver{}
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	// What an OpenTelemetry SDK would pick up: Sakutei must not.
	env := []string{"OTEL_EXPORTER_OTLP_ENDPOINT=" + srv.URL, "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=" + srv.URL + "/v1/traces",
		"OTEL_TRACES_EXPORTER=otlp", "OTEL_SDK_DISABLED=false"}

	intentDir, statePath, root := newOTLPEnv(t)
	if out, code := runSakuteiEnv(t, env, "plan", "--intent", intentDir, "--state", statePath); code != 0 || strings.Contains(out, "traces") {
		t.Fatalf("[AC-189-1] plan exit %d:\n%s", code, out)
	}
	if out, code := runSakuteiEnv(t, env, "reconcile", "--intent", intentDir, "--state", statePath); code != 0 || strings.Contains(out, "traces") {
		t.Fatalf("[AC-189-1] reconcile exit %d:\n%s", code, out)
	}
	cmd := exec.Command(buildBinary(t), "--intent", intentDir, "--state", statePath, "--interval", "100ms",
		"--workflow-store", filepath.Join(root, "wf.json"))
	cmd.Env = append(os.Environ(), env...)
	var out syncBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(10*time.Second, func() bool { return strings.Count(out.String(), "cycle ") >= 6 }) {
		_ = cmd.Process.Kill()
		t.Fatalf("daemon did not run cycles:\n%s", out.String())
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
	if n, _, _ := rcv.snapshot(); n != 0 {
		t.Errorf("[AC-189-1] %d request(s) reached the receiver without --otlp-endpoint", n)
	}
	if strings.Contains(out.String(), "traces") {
		t.Errorf("[AC-189-1] the daemon talks about traces without --otlp-endpoint:\n%s", out.String())
	}
}

func TestOTLP_DaemonCycleTrace(t *testing.T) {
	rcv := &otlpReceiver{}
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	intentDir, statePath, root := newOTLPEnv(t)
	cmd, out, errOut := startDaemonWithArgs(t, intentDir, statePath, "200ms",
		"--workflow-store", filepath.Join(root, "wf.json"), "--otlp-endpoint", srv.URL)
	if !waitForCondition(15*time.Second, func() bool {
		_, spans, _ := rcv.snapshot()
		names := spanNames(spans)
		return names["sakutei.cycle"] >= 2 && names["sakutei.workflow.step"] >= 1 && names["sakutei.apply"] >= 2
	}) {
		_ = cmd.Process.Kill()
		_, spans, _ := rcv.snapshot()
		t.Fatalf("[AC-189-2] spans %v\n%s\n%s", spanNames(spans), out.String(), errOut.String())
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
	if !strings.Contains(errOut.String(), "sending traces to "+srv.URL+"/v1/traces (plaintext; authorization: none) over OTLP/HTTP") {
		t.Errorf("[AC-189-2] no start line:\n%s", errOut.String())
	}
	_, spans, bodies := rcv.snapshot()
	byID := map[string]otlpSpan{}
	for _, s := range spans {
		byID[s.SpanID] = s
	}
	// rootOf follows the parents up to the root.
	rootOf := func(s otlpSpan) otlpSpan {
		for s.ParentSpanID != "" {
			p, ok := byID[s.ParentSpanID]
			if !ok {
				t.Fatalf("[AC-189-2] %s: parent %s not sent", s.Name, s.ParentSpanID)
			}
			if p.TraceID != s.TraceID {
				t.Fatalf("[AC-189-2] %s is in another trace than its parent", s.Name)
			}
			s = p
		}
		return s
	}
	parentName := func(s otlpSpan) string { return byID[s.ParentSpanID].Name }
	cycles := map[string]bool{}
	for _, s := range spans {
		if s.Name == "sakutei.cycle" {
			if s.ParentSpanID != "" || s.attr("sakutei.cycle.number") == "" {
				t.Errorf("[AC-189-2] cycle span %+v", s)
			}
			if cycles[s.TraceID] {
				t.Errorf("[AC-189-2] two cycles in one trace")
			}
			cycles[s.TraceID] = true
			continue
		}
		if r := rootOf(s); r.Name != "sakutei.cycle" {
			t.Errorf("[AC-189-2] %s is under %s, not a cycle", s.Name, r.Name)
		}
		switch s.Name {
		case "sakutei.observe", "sakutei.plan", "sakutei.workflow":
			if parentName(s) != "sakutei.cycle" {
				t.Errorf("[AC-189-2] %s under %s", s.Name, parentName(s))
			}
		case "sakutei.workflow.step":
			if parentName(s) != "sakutei.workflow" || s.attr("sakutei.workflow") != "roll" || s.attr("sakutei.workflow.step_kind") != "apply" {
				t.Errorf("[AC-189-2] step %+v under %s", s, parentName(s))
			}
		}
	}
	names := spanNames(spans)
	for _, n := range []string{"sakutei.observe", "sakutei.read", "sakutei.plan", "sakutei.apply", "sakutei.workflow", "sakutei.workflow.step"} {
		if names[n] == 0 {
			t.Errorf("[AC-189-2] no %s span: %v", n, names)
		}
	}
	var webApply, stepApply bool
	for _, s := range spans {
		if s.Name != "sakutei.apply" {
			continue
		}
		switch {
		case s.attr("sakutei.resource") == "fake//web" && parentName(s) == "sakutei.cycle":
			webApply = s.attr("sakutei.ops") == "1" && s.attr("sakutei.provider") == "fake"
		case s.attr("sakutei.resource") == "fake//n1" && parentName(s) == "sakutei.workflow.step":
			stepApply = true
		}
	}
	if !webApply || !stepApply {
		for _, s := range spans {
			if s.Name == "sakutei.apply" {
				t.Logf("apply %+v under %s", s, parentName(s))
			}
		}
		t.Errorf("[AC-189-2] Apply spans: web under the cycle %v, n1 under the Workflow step %v", webApply, stepApply)
	}
	if strings.Contains(bodies, "zz189") {
		t.Errorf("[AC-189-2] a field value is in the spans")
	}
}

func TestOTLP_OneShotTraces(t *testing.T) {
	rcv := &otlpReceiver{}
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	intentDir, statePath, _ := newOTLPEnv(t)

	check := func(verb, root string, children ...string) {
		t.Helper()
		before, _, _ := rcv.snapshot()
		out, code := runSakutei(t, verb, "--intent", intentDir, "--state", statePath, "--otlp-endpoint", srv.URL)
		if code != 0 {
			t.Fatalf("[AC-189-3] %s exit %d:\n%s", verb, code, out)
		}
		// Sent before the process exited: nothing to wait for.
		n, spans, _ := rcv.snapshot()
		if n == before {
			t.Fatalf("[AC-189-3] %s sent nothing:\n%s", verb, out)
		}
		var rootSpan *otlpSpan
		for i := range spans {
			if spans[i].Name == root {
				rootSpan = &spans[i]
			}
		}
		if rootSpan == nil || rootSpan.ParentSpanID != "" || rootSpan.attr("sakutei.exit_code") != "0" {
			t.Fatalf("[AC-189-3] %s: root %+v", verb, rootSpan)
		}
		got := map[string]int{}
		for _, s := range spans {
			if s.TraceID == rootSpan.TraceID && s.SpanID != rootSpan.SpanID {
				got[s.Name]++
			}
		}
		for _, c := range children {
			if got[c] == 0 {
				t.Errorf("[AC-189-3] %s: no %s in the trace: %v", verb, c, got)
			}
		}
	}
	check("plan", "sakutei.plan", "sakutei.read")
	check("reconcile", "sakutei.reconcile", "sakutei.read", "sakutei.apply")
}

func TestOTLP_TLSAndAuthorization(t *testing.T) {
	intentDir, statePath, root := newOTLPEnv(t)

	// [AC-189-4] a non-loopback endpoint without TLS and authorization is refused.
	for _, args := range [][]string{
		{"--otlp-endpoint", "http://192.0.2.1:4318"},
		{"--otlp-endpoint", "https://192.0.2.1:4318"},
	} {
		out, code := runSakutei(t, append([]string{"plan", "--intent", intentDir, "--state", statePath}, args...)...)
		if code != 1 || !strings.Contains(out, "--otlp-insecure") {
			t.Errorf("[AC-189-4] %v: exit %d:\n%s", args, code, out)
		}
	}
	// A token written in the config is refused (SecretRef only), without its value.
	bad := filepath.Join(root, "bad.yaml")
	if err := os.WriteFile(bad, []byte("bearer_token: plain-zz189\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := runSakutei(t, "plan", "--intent", intentDir, "--state", statePath,
		"--otlp-endpoint", "https://127.0.0.1:4318", "--otlp-config", bad); code != 1 || !strings.Contains(out, "SecretRef") || strings.Contains(out, "plain-zz189") {
		t.Errorf("[AC-189-4] inline token: exit %d:\n%s", code, out)
	}

	// https with a bearer token from the file SecretBackend.
	ca := newTestCA(t, "collector-ca")
	rcv := &otlpReceiver{}
	srv := newTLSReceiver(t, ca, rcv)
	defer srv.Close()
	caPath := filepath.Join(root, "collector-ca.pem")
	if err := os.WriteFile(caPath, []byte(ca.pem), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSecretStore(t, filepath.Join(root, "secrets.json"), map[string]string{"sakutei/otlp_token": "tok-zz189"})
	cfg := filepath.Join(root, "otlp.yaml")
	if err := os.WriteFile(cfg, []byte(`secret_store: secrets.json
tls:
  ca_file: collector-ca.pem
  min_version: "1.2"
bearer_token: secretref:file/sakutei/otlp_token
`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code := runSakutei(t, "reconcile", "--intent", intentDir, "--state", statePath, "--otlp-endpoint", srv.URL, "--otlp-config", cfg)
	if code != 0 || !strings.Contains(out, "(TLS 1.2+; authorization: bearer token)") || strings.Contains(out, "tok-zz189") {
		t.Fatalf("[AC-189-4] reconcile exit %d:\n%s", code, out)
	}
	n, spans, _ := rcv.snapshot()
	if n == 0 || spanNames(spans)["sakutei.reconcile"] != 1 {
		t.Fatalf("[AC-189-4] nothing over TLS: %d %v", n, spanNames(spans))
	}
	rcv.mu.Lock()
	for _, a := range rcv.auth {
		if a != "Bearer tok-zz189" {
			t.Errorf("[AC-189-4] Authorization %q", a)
		}
	}
	rcv.mu.Unlock()

	// A receiver whose certificate the CA does not sign gets nothing (and the
	// run does not fail for it: traces never hold back convergence).
	other := newTLSReceiver(t, newTestCA(t, "other-ca"), &otlpReceiver{})
	defer other.Close()
	out, code = runSakutei(t, "plan", "--intent", intentDir, "--state", statePath, "--otlp-endpoint", other.URL, "--otlp-config", cfg)
	if code != 0 || !strings.Contains(out, "otlp: cannot send") {
		t.Errorf("[AC-189-4] unverified receiver: exit %d:\n%s", code, out)
	}
}

// newTLSReceiver serves h over TLS with a 127.0.0.1 certificate from ca.
func newTLSReceiver(t *testing.T, ca *testCA, h http.Handler) *httptest.Server {
	t.Helper()
	c, k := ca.issue(t, "collector", true)
	pair, err := tls.X509KeyPair([]byte(c), []byte(k))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the refused handshakes are expected
	srv.StartTLS()
	return srv
}
