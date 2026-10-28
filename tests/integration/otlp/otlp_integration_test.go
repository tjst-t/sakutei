//go:build integration_otlp

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

package otlp

// Collector のイメージは SAKUTEI_IT_OTELCOL_IMAGE で差し替えられる（既定は tag と
// digest で固定）。
//
//	IT-189-1  常駐：周ごとのトレース（sakutei.cycle）が Collector に届き、その下に
//	          観測の段・Read・plan の段・Apply・Workflow と手順がある。resource は
//	          service.name=sakutei。欄の値は載らない
//	IT-189-2  plan・reconcile：1 回が 1 つのトレース（根は sakutei.plan・sakutei.reconcile）
//	IT-189-3  TLS（Collector の CA で確かめる）・クライアント証明書・bearer トークンの
//	          受け口に届く
//	IT-189-4  トークンが違う・クライアント証明書が無いと Collector が断り、何も
//	          届かない。sakutei はそう 1 度知らせ、収束は止めない（終了コード 0）
//	IT-189-5  --otlp-endpoint が無ければ（OTEL_* の環境変数があっても）何も届かない

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// 既定のイメージ。digest は docker pull で確かめた値（2026-10-28）。
const otelcolImage = "otel/opentelemetry-collector-contrib:0.163.2@sha256:7f22d2a6748110ffb6a59bc0df448bccbf501cdb667d610ede4c61d3863d5849"

const (
	container = "sakutei-otlpit-collector"
	token     = "sakutei-it-otlp-token"
)

var lab struct {
	dir, bin, out       string
	plainURL, secureURL string
}

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	if err := setup(); err != nil {
		fmt.Fprintf(os.Stderr, "integration-otlp: %v\n", err)
		teardown()
		return 1
	}
	defer teardown()
	return m.Run()
}

func docker(args ...string) (string, error) {
	out, err := exec.Command("sudo", append([]string{"-n", "docker"}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func freePort() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port, nil
}

// collectorConfig: otlp/plain (0.0.0.0:4318, no TLS) and otlp/secure
// (0.0.0.0:4319, TLS with a client certificate from client-ca.pem and a bearer
// token), each written to its own file.
const collectorConfig = `extensions:
  bearertokenauth:
    token: "` + token + `"
receivers:
  otlp/plain:
    protocols:
      http:
        endpoint: 0.0.0.0:4318
  otlp/secure:
    protocols:
      http:
        endpoint: 0.0.0.0:4319
        tls:
          cert_file: /etc/otelcol-it/server.pem
          key_file: /etc/otelcol-it/server-key.pem
          client_ca_file: /etc/otelcol-it/client-ca.pem
          min_version: "1.2"
        auth:
          authenticator: bearertokenauth
exporters:
  file/plain:
    path: /out/plain.jsonl
    flush_interval: 200ms
  file/secure:
    path: /out/secure.jsonl
    flush_interval: 200ms
service:
  extensions: [bearertokenauth]
  pipelines:
    traces/plain:
      receivers: [otlp/plain]
      exporters: [file/plain]
    traces/secure:
      receivers: [otlp/secure]
      exporters: [file/secure]
`

func setup() error {
	if out, err := docker("version", "--format", "{{.Server.Version}}"); err != nil {
		return fmt.Errorf("docker (through sudo -n) is required: %v: %s", err, out)
	}
	dir, err := os.MkdirTemp("", "sakutei-otlp-it-")
	if err != nil {
		return err
	}
	lab.dir = dir
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		return err
	}
	lab.bin = filepath.Join(dir, "sakutei")
	build := exec.Command("go", "build", "-o", lab.bin, "./cmd/sakutei")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("go build: %v\n%s", err, out)
	}

	// Certificates: the Collector's server certificate for 127.0.0.1 and a
	// client certificate, from two throwaway CAs.
	etc := filepath.Join(dir, "etc")
	lab.out = filepath.Join(dir, "out")
	for _, d := range []string{etc, lab.out} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	serverCA, err := newCA("otlp-it-server-ca")
	if err != nil {
		return err
	}
	clientCA, err := newCA("otlp-it-client-ca")
	if err != nil {
		return err
	}
	sc, sk, err := serverCA.issue("collector", true)
	if err != nil {
		return err
	}
	cc, ck, err := clientCA.issue("sakutei", false)
	if err != nil {
		return err
	}
	files := map[string]string{
		filepath.Join(etc, "config.yaml"): collectorConfig, filepath.Join(etc, "server.pem"): sc,
		filepath.Join(etc, "server-key.pem"): sk, filepath.Join(etc, "client-ca.pem"): clientCA.pem,
		filepath.Join(dir, "server-ca.pem"): serverCA.pem,
	}
	for p, v := range files {
		if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
			return err
		}
	}
	// sakutei's side: the file SecretBackend with the client certificate and
	// the tokens (the right one and a wrong one).
	store := map[string]any{"secrets": map[string]any{
		"otlp/client_cert": map[string]any{"gen": 1, "value": cc},
		"otlp/client_key":  map[string]any{"gen": 1, "value": ck},
		"otlp/token":       map[string]any{"gen": 1, "value": token},
		"otlp/wrong_token": map[string]any{"gen": 1, "value": "not-the-token"},
	}}
	data, _ := json.Marshal(store)
	if err := os.WriteFile(filepath.Join(dir, "secrets.json"), data, 0o600); err != nil {
		return err
	}

	img := otelcolImage
	if v, ok := os.LookupEnv("SAKUTEI_IT_OTELCOL_IMAGE"); ok && v != "" {
		img = v
	}
	if out, err := docker("pull", img); err != nil {
		return fmt.Errorf("docker pull %s: %v: %s", img, err, out)
	}
	plain, err := freePort()
	if err != nil {
		return err
	}
	secure, err := freePort()
	if err != nil {
		return err
	}
	_, _ = docker("rm", "-f", "-v", container)
	// The Collector runs as this user, so it reads the files written here
	// (whatever the umask) and its output files are ours to remove.
	if out, err := docker("run", "-d", "--name", container, "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-p", "127.0.0.1:"+plain+":4318", "-p", "127.0.0.1:"+secure+":4319",
		"-v", etc+":/etc/otelcol-it:ro", "-v", lab.out+":/out",
		img, "--config=/etc/otelcol-it/config.yaml"); err != nil {
		return fmt.Errorf("docker run: %v: %s", err, out)
	}
	lab.plainURL = "http://127.0.0.1:" + plain
	lab.secureURL = "https://127.0.0.1:" + secure
	end := time.Now().Add(time.Minute)
	for {
		logs, _ := docker("logs", container)
		if strings.Contains(logs, "Everything is ready") {
			break
		}
		if st, _ := docker("inspect", "-f", "{{.State.Running}}", container); st == "false" {
			return fmt.Errorf("the Collector exited:\n%s", logs)
		}
		if time.Now().After(end) {
			return fmt.Errorf("the Collector did not start:\n%s", logs)
		}
		time.Sleep(300 * time.Millisecond)
	}
	return nil
}

func teardown() {
	if out, err := docker("rm", "-f", "-v", container); err != nil && !strings.Contains(out, "No such container") {
		fmt.Fprintf(os.Stderr, "integration-otlp: docker rm: %v: %s\n", err, out)
	}
	if lab.dir != "" {
		_ = os.RemoveAll(lab.dir)
	}
}

// ── certificates ──

type ca struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  string
}

var serial int64

func newCA(cn string) (*ca, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial++
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	c, _ := x509.ParseCertificate(der)
	return &ca{cert: c, key: key, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}, nil
}

func (c *ca) issue(cn string, server bool) (string, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	serial++
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return "", "", err
	}
	kd, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd})), nil
}

// ── what the Collector wrote ──

type span struct {
	TraceID      string `json:"traceId"`
	SpanID       string `json:"spanId"`
	ParentSpanID string `json:"parentSpanId"`
	Name         string `json:"name"`
	Attributes   []struct {
		Key   string         `json:"key"`
		Value map[string]any `json:"value"`
	} `json:"attributes"`
}

func (s span) attr(key string) string {
	for _, a := range s.Attributes {
		if a.Key == key {
			for _, v := range a.Value {
				return fmt.Sprint(v)
			}
		}
	}
	return ""
}

type received struct {
	spans    []span
	services map[string]bool
	raw      string
}

// read parses the file exporter's output: one OTLP JSON request per line.
func read(t *testing.T, name string) received {
	t.Helper()
	r := received{services: map[string]bool{}}
	data, err := os.ReadFile(filepath.Join(lab.out, name))
	if err != nil {
		if os.IsNotExist(err) {
			return r
		}
		t.Fatal(err)
	}
	// Only whole lines: the Collector may be writing the last one.
	lines := strings.Split(string(data), "\n")
	lines = lines[:len(lines)-1]
	var raw strings.Builder
	for _, line := range lines {
		raw.WriteString(line)
		var req struct {
			ResourceSpans []struct {
				Resource struct {
					Attributes []struct {
						Key   string         `json:"key"`
						Value map[string]any `json:"value"`
					} `json:"attributes"`
				} `json:"resource"`
				ScopeSpans []struct {
					Spans []span `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, rs := range req.ResourceSpans {
			for _, a := range rs.Resource.Attributes {
				if a.Key == "service.name" {
					r.services[fmt.Sprint(a.Value["stringValue"])] = true
				}
			}
			for _, ss := range rs.ScopeSpans {
				r.spans = append(r.spans, ss.Spans...)
			}
		}
	}
	r.raw = raw.String()
	return r
}

func names(spans []span) map[string]int {
	m := map[string]int{}
	for _, s := range spans {
		m[s.Name]++
	}
	return m
}

// ── the intent ──

const intent = `{
  "resources": [
    { "provider": "fake", "id": "web", "fields": { "name": "web", "size": "it189-desired" } },
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

// env writes the intent and a fake reality in which web drifts and n1 is the
// Workflow's target; it returns the intent dir and the state file.
func env(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	intentDir := filepath.Join(dir, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(intentDir, "intent.json"), []byte(intent), 0o644); err != nil {
		t.Fatal(err)
	}
	state := map[string]any{"provider_version": "fake/1.0.0", "resources": map[string]any{
		"web": map[string]any{"fields": map[string]any{"name": "web", "size": "it189-observed"}},
		"n1":  map[string]any{"fields": map[string]any{"name": "n1", "size": "1"}},
	}}
	data, _ := json.Marshal(state)
	statePath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return intentDir, statePath
}

func run(t *testing.T, environ []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(lab.bin, args...)
	cmd.Env = append(os.Environ(), environ...)
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), 0
}

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitFor polls cond for up to d.
func waitFor(d time.Duration, cond func() bool) bool {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return cond()
}

// ── tests (in order: the files only grow) ──

func TestIT189_5_NothingByDefault(t *testing.T) {
	intentDir, statePath := env(t)
	environ := []string{"OTEL_EXPORTER_OTLP_ENDPOINT=" + lab.plainURL, "OTEL_TRACES_EXPORTER=otlp"}
	for _, verb := range []string{"plan", "reconcile"} {
		if out, code := run(t, environ, verb, "--intent", intentDir, "--state", statePath); code != 0 {
			t.Fatalf("%s exit %d:\n%s", verb, code, out)
		}
	}
	dir := t.TempDir()
	cmd := exec.Command(lab.bin, "--intent", intentDir, "--state", statePath, "--interval", "100ms",
		"--workflow-store", filepath.Join(dir, "wf.json"))
	cmd.Env = append(os.Environ(), environ...)
	var out syncBuf
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
	time.Sleep(time.Second) // the file exporter's flush_interval
	for _, f := range []string{"plain.jsonl", "secure.jsonl"} {
		if got := read(t, f); len(got.spans) != 0 {
			t.Errorf("[IT-189-5] %d span(s) in %s without --otlp-endpoint", len(got.spans), f)
		}
	}
}

func TestIT189_1_DaemonCycles(t *testing.T) {
	intentDir, statePath := env(t)
	dir := t.TempDir()
	cmd := exec.Command(lab.bin, "--intent", intentDir, "--state", statePath, "--interval", "200ms",
		"--workflow-store", filepath.Join(dir, "wf.json"), "--otlp-endpoint", lab.plainURL)
	var out syncBuf
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ok := waitFor(30*time.Second, func() bool {
		n := names(read(t, "plain.jsonl").spans)
		return n["sakutei.cycle"] >= 3 && n["sakutei.workflow.step"] >= 1
	})
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
	got := read(t, "plain.jsonl")
	if !ok {
		t.Fatalf("[IT-189-1] spans %v\n%s", names(got.spans), out.String())
	}
	if !got.services["sakutei"] {
		t.Errorf("[IT-189-1] service.name: %v", got.services)
	}
	byID := map[string]span{}
	for _, s := range got.spans {
		byID[s.SpanID] = s
	}
	parent := func(s span) string { return byID[s.ParentSpanID].Name }
	n := names(got.spans)
	for _, want := range []string{"sakutei.cycle", "sakutei.observe", "sakutei.read", "sakutei.plan", "sakutei.apply", "sakutei.workflow", "sakutei.workflow.step"} {
		if n[want] == 0 {
			t.Errorf("[IT-189-1] no %s: %v", want, n)
		}
	}
	for _, s := range got.spans {
		if s.Name == "sakutei.cycle" {
			continue
		}
		p, ok := byID[s.ParentSpanID]
		if !ok || p.TraceID != s.TraceID {
			t.Errorf("[IT-189-1] %s has no parent in its trace", s.Name)
		}
		switch s.Name {
		case "sakutei.read":
			if pn := parent(s); pn != "sakutei.observe" && pn != "sakutei.workflow" && pn != "sakutei.workflow.step" {
				t.Errorf("[IT-189-1] read under %s", pn)
			}
		case "sakutei.workflow.step":
			if parent(s) != "sakutei.workflow" || s.attr("sakutei.workflow") != "roll" {
				t.Errorf("[IT-189-1] step %+v", s)
			}
		}
	}
	var webApply, stepApply bool
	for _, s := range got.spans {
		if s.Name == "sakutei.apply" {
			webApply = webApply || (s.attr("sakutei.resource") == "fake//web" && parent(s) == "sakutei.cycle")
			stepApply = stepApply || (s.attr("sakutei.resource") == "fake//n1" && parent(s) == "sakutei.workflow.step")
		}
	}
	if !webApply || !stepApply {
		t.Errorf("[IT-189-1] Apply under the cycle %v, under the Workflow step %v", webApply, stepApply)
	}
	if strings.Contains(got.raw, "it189-") {
		t.Errorf("[IT-189-1] a field value reached the Collector")
	}
}

func TestIT189_2_OneShot(t *testing.T) {
	for _, tc := range []struct{ verb, root, child string }{
		{"plan", "sakutei.plan", "sakutei.read"},
		{"reconcile", "sakutei.reconcile", "sakutei.apply"},
	} {
		intentDir, statePath := env(t)
		before := len(read(t, "plain.jsonl").spans)
		if out, code := run(t, nil, tc.verb, "--intent", intentDir, "--state", statePath, "--otlp-endpoint", lab.plainURL); code != 0 {
			t.Fatalf("%s exit %d:\n%s", tc.verb, code, out)
		}
		// The root is the span of that name without a parent (a daemon's
		// cycle has a sakutei.plan phase under it).
		var spans []span
		var root span
		if !waitFor(10*time.Second, func() bool {
			spans = read(t, "plain.jsonl").spans[before:]
			for _, s := range spans {
				if s.Name == tc.root && s.ParentSpanID == "" {
					root = s
					return true
				}
			}
			return false
		}) {
			t.Fatalf("[IT-189-2] %s: %v", tc.verb, names(spans))
		}
		found := false
		for _, s := range spans {
			found = found || (s.Name == tc.child && s.TraceID == root.TraceID && s.ParentSpanID == root.SpanID)
		}
		if !found || root.attr("sakutei.exit_code") != "0" {
			t.Errorf("[IT-189-2] %s: root %+v, %s under it %v", tc.verb, root, tc.child, found)
		}
	}
}

func otlpConfig(t *testing.T, tokenRef string, clientCert bool) string {
	t.Helper()
	cfg := fmt.Sprintf("secret_store: %s\ntls:\n  ca_file: %s\n  min_version: \"1.3\"\n",
		filepath.Join(lab.dir, "secrets.json"), filepath.Join(lab.dir, "server-ca.pem"))
	if clientCert {
		cfg += "  client_cert: secretref:file/otlp/client_cert\n  client_key: secretref:file/otlp/client_key\n"
	}
	if tokenRef != "" {
		cfg += "bearer_token: secretref:file/" + tokenRef + "\n"
	}
	p := filepath.Join(t.TempDir(), "otlp.yaml")
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestIT189_3_TLSAndAuthorization(t *testing.T) {
	intentDir, statePath := env(t)
	out, code := run(t, nil, "reconcile", "--intent", intentDir, "--state", statePath,
		"--otlp-endpoint", lab.secureURL, "--otlp-config", otlpConfig(t, "otlp/token", true))
	if code != 0 || !strings.Contains(out, "(TLS 1.3+; authorization: bearer token + client certificate)") {
		t.Fatalf("[IT-189-3] exit %d:\n%s", code, out)
	}
	if strings.Contains(out, "cannot send") {
		t.Errorf("[IT-189-3] not sent:\n%s", out)
	}
	if !waitFor(10*time.Second, func() bool { return names(read(t, "secure.jsonl").spans)["sakutei.reconcile"] == 1 }) {
		t.Fatalf("[IT-189-3] nothing on the secure receiver: %v", names(read(t, "secure.jsonl").spans))
	}
	if strings.Contains(read(t, "secure.jsonl").raw, token) {
		t.Errorf("[IT-189-3] the token is in the spans")
	}
}

func TestIT189_4_RefusedByTheCollector(t *testing.T) {
	before := len(read(t, "secure.jsonl").spans)
	for _, tc := range []struct {
		name, token string
		cert        bool
	}{
		{"wrong token", "otlp/wrong_token", true},
		{"no client certificate", "otlp/token", false},
	} {
		intentDir, statePath := env(t)
		out, code := run(t, nil, "reconcile", "--intent", intentDir, "--state", statePath,
			"--otlp-endpoint", lab.secureURL, "--otlp-config", otlpConfig(t, tc.token, tc.cert))
		if code != 0 {
			t.Errorf("[IT-189-4] %s: the reconcile failed for the traces (exit %d):\n%s", tc.name, code, out)
		}
		if strings.Count(out, "otlp: cannot send") != 1 || strings.Contains(out, "not-the-token") {
			t.Errorf("[IT-189-4] %s: want one notice without the token:\n%s", tc.name, out)
		}
		data, _ := os.ReadFile(statePath)
		if !strings.Contains(string(data), "it189-desired") {
			t.Errorf("[IT-189-4] %s: not converged", tc.name)
		}
	}
	time.Sleep(time.Second)
	if after := len(read(t, "secure.jsonl").spans); after != before {
		t.Errorf("[IT-189-4] %d span(s) were accepted with a bad credential", after-before)
	}
}
