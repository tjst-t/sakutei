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

// listen_auth_test.go — #161（/metrics と読み取りの口の TLS と認可）:
//
//	[AC-161-1] TLS の口に TLS なし（平文）や古い TLS（1.1）で繋ぐと断り、断った数が
//	           /metrics の sakutei_listen_rejected_total に出る
//	[AC-161-2] 読むだけのトークンが無い・違うと 401 で断り、正しいトークンで読める
//	[AC-161-3] クライアント証明書が無いと 401、CA の違う証明書と許していない subject は
//	           403 で断り、正しい証明書で読める
//	[AC-161-4] loopback 以外を TLS なし・認可なしで待ち受けるのは、明示の旗
//	           （--metrics-insecure-listen・--read-insecure-listen）のときだけ。無ければ
//	           起動で断る
//	[AC-161-5] 書き込みの口は読み取りの資格で使えない：読み取りの口は正しい資格でも
//	           書き込みを 405 で断り、承認の口を読み取りの口と同じアドレスに置くと起動で断る
package acceptance

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// testCA is a throwaway CA for the tests.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  string
}

var serialN int64

func newTestCA(t *testing.T, cn string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serialN++
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serialN), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return &testCA{cert: c, key: key, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

// issue returns a leaf (PEM cert, PEM key) for cn: a server certificate for
// 127.0.0.1 when server, else a client certificate.
func (ca *testCA) issue(t *testing.T, cn string, server bool) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serialN++
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serialN), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature,
	}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	kd, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}))
}

func (ca *testCA) clientCert(t *testing.T, cn string) tls.Certificate {
	t.Helper()
	c, k := ca.issue(t, cn, false)
	pair, err := tls.X509KeyPair([]byte(c), []byte(k))
	if err != nil {
		t.Fatal(err)
	}
	return pair
}

// writeSecretStore writes a file SecretBackend store holding values.
func writeSecretStore(t *testing.T, path string, values map[string]string) {
	t.Helper()
	secrets := map[string]any{}
	for p, v := range values {
		secrets[p] = map[string]any{"gen": 1, "value": v}
	}
	data, _ := json.Marshal(map[string]any{"secrets": secrets})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

type listenAuthEnv struct {
	intentDir, statePath, configPath string
	serverCA, clientCA, otherCA      *testCA
	token                            string
}

// newListenAuthEnv writes an intent, a secret store and a --listen-auth-config:
// metrics takes TLS and a bearer token, read takes TLS and a client
// certificate from clientCA with the subject "reader".
func newListenAuthEnv(t *testing.T) *listenAuthEnv {
	t.Helper()
	tmp := t.TempDir()
	e := &listenAuthEnv{
		intentDir: filepath.Join(tmp, "intent"), statePath: filepath.Join(tmp, "state.json"),
		configPath: filepath.Join(tmp, "listen-auth.yaml"),
		serverCA:   newTestCA(t, "server-ca"), clientCA: newTestCA(t, "client-ca"), otherCA: newTestCA(t, "other-ca"),
		token: "read-only-token-161",
	}
	if err := os.MkdirAll(e.intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIntentFile(t, e.intentDir, map[string]any{"resources": []any{fakeRes("web", "1")}})
	seedState(t, e.statePath, map[string]map[string]any{"web": {"name": "web", "size": "1"}})
	cert, key := e.serverCA.issue(t, "sakutei", true)
	writeSecretStore(t, filepath.Join(tmp, "secrets.json"), map[string]string{
		"sakutei/listen_cert": cert, "sakutei/listen_key": key, "sakutei/metrics_token": e.token,
	})
	if err := os.WriteFile(filepath.Join(tmp, "clients-ca.pem"), []byte(e.clientCA.pem), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := `secret_store: secrets.json
metrics:
  tls:
    cert: secretref:file/sakutei/listen_cert
    key: secretref:file/sakutei/listen_key
  bearer_token: secretref:file/sakutei/metrics_token
read:
  tls:
    cert: secretref:file/sakutei/listen_cert
    key: secretref:file/sakutei/listen_key
    min_version: "1.2"
  client_ca_file: clients-ca.pem
  allowed_subjects:
    - reader
`
	if err := os.WriteFile(e.configPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *listenAuthEnv) client(certs ...tls.Certificate) *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(e.serverCA.cert)
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, Certificates: certs},
	}}
}

func doReq(t *testing.T, c *http.Client, method, url, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

var (
	metricsTLSRe = regexp.MustCompile(`metrics listening on (https://\S+/metrics)`)
	readTLSRe    = regexp.MustCompile(`read-only API listening on (https://\S+) `)
)

func TestListenAuth_TLSAndAuthorization(t *testing.T) {
	e := newListenAuthEnv(t)
	cmd, out, errOut := startDaemonWithArgs(t, e.intentDir, e.statePath, "100ms", "--mode", "alert-only",
		"--listen-auth-config", e.configPath, "--metrics-listen", "127.0.0.1:0", "--read-listen", "127.0.0.1:0")
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	var metricsURL, readURL string
	if !waitForCondition(10*time.Second, func() bool {
		if m := metricsTLSRe.FindStringSubmatch(out.String()); m != nil {
			metricsURL = m[1]
		}
		if m := readTLSRe.FindStringSubmatch(out.String()); m != nil {
			readURL = m[1]
		}
		return metricsURL != "" && readURL != ""
	}) {
		t.Fatalf("no TLS listening lines:\n%s\n%s", out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "TLS 1.2+; authorization: bearer token") ||
		!strings.Contains(out.String(), "TLS 1.2+; authorization: client certificate") {
		t.Errorf("listening lines do not tell the protection:\n%s", out.String())
	}

	// [AC-161-1] plaintext and TLS 1.1 are refused.
	plain := &http.Client{Timeout: 5 * time.Second}
	if code, body := doReq(t, plain, "GET", strings.Replace(metricsURL, "https://", "http://", 1), e.token); code == 200 {
		t.Errorf("[AC-161-1] plaintext to the TLS metrics listener was served: %d %s", code, body)
	}
	if code, body := doReq(t, plain, "GET", strings.Replace(readURL, "https://", "http://", 1)+"/status", ""); code == 200 {
		t.Errorf("[AC-161-1] plaintext to the TLS read listener was served: %d %s", code, body)
	}
	old := e.client()
	old.Transport.(*http.Transport).TLSClientConfig.MaxVersion = tls.VersionTLS11
	if code, _ := doReq(t, old, "GET", metricsURL, e.token); code != 0 {
		t.Errorf("[AC-161-1] TLS 1.1 was accepted (status %d)", code)
	}

	// [AC-161-2] bearer token on /metrics.
	c := e.client()
	if code, body := doReq(t, c, "GET", metricsURL, ""); code != 401 || strings.Contains(body, "token") {
		t.Errorf("[AC-161-2] no token: %d %q, want 401 without the reason", code, body)
	}
	if code, _ := doReq(t, c, "GET", metricsURL, "wrong-token"); code != 401 {
		t.Errorf("[AC-161-2] wrong token: %d, want 401", code)
	}
	if code, body := doReq(t, c, "GET", metricsURL, e.token); code != 200 || !strings.Contains(body, "sakutei_cycles_total") {
		t.Fatalf("[AC-161-2] right token: %d\n%s", code, body)
	}

	// [AC-161-3] client certificate on the read API.
	if code, _ := doReq(t, e.client(), "GET", readURL+"/status", ""); code != 401 {
		t.Errorf("[AC-161-3] no client certificate: %d, want 401", code)
	}
	if code, body := doReq(t, e.client(e.otherCA.clientCert(t, "reader")), "GET", readURL+"/status", ""); code != 403 || strings.Contains(body, "certificate") {
		t.Errorf("[AC-161-3] client certificate of another CA: %d %q, want 403 without the reason", code, body)
	}
	if code, _ := doReq(t, e.client(e.clientCA.clientCert(t, "intruder")), "GET", readURL+"/status", ""); code != 403 {
		t.Errorf("[AC-161-3] subject not allowed: %d, want 403", code)
	}
	reader := e.client(e.clientCA.clientCert(t, "reader"))
	var code int
	var body string
	waitForCondition(10*time.Second, func() bool {
		code, body = doReq(t, reader, "GET", readURL+"/status", "")
		return code == 200
	})
	if code != 200 || !strings.Contains(body, `"cycle"`) {
		t.Fatalf("[AC-161-3] right client certificate: %d\n%s", code, body)
	}

	// [AC-161-5] the read credentials take no writes.
	if code, _ := doReq(t, reader, "POST", readURL+"/workflows/wf/gates/g/approve", ""); code != 404 && code != 405 {
		t.Errorf("[AC-161-5] POST to the read listener with its credentials: %d, want 404/405", code)
	}
	if code, _ := doReq(t, reader, "POST", readURL+"/status", ""); code != 405 {
		t.Errorf("[AC-161-5] POST /status with the read credentials: %d, want 405", code)
	}
	if code, _ := doReq(t, c, "POST", metricsURL, e.token); code != 405 {
		t.Errorf("[AC-161-5] POST /metrics with the token: %d, want 405", code)
	}

	// [AC-161-1] the refusals are counted on /metrics.
	_, body = doReq(t, c, "GET", metricsURL, e.token)
	for _, want := range []string{
		`sakutei_listen_rejected_total{listener="metrics",reason="plaintext"} 1`,
		`sakutei_listen_rejected_total{listener="read",reason="plaintext"} 1`,
		`sakutei_listen_rejected_total{listener="metrics",reason="tls_handshake"} 1`,
		`sakutei_listen_rejected_total{listener="metrics",reason="no_token"} 1`,
		`sakutei_listen_rejected_total{listener="metrics",reason="bad_token"} 1`,
		`sakutei_listen_rejected_total{listener="read",reason="no_client_certificate"} 1`,
		`sakutei_listen_rejected_total{listener="read",reason="bad_client_certificate"} 1`,
		`sakutei_listen_rejected_total{listener="read",reason="client_not_allowed"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("[AC-161-1] /metrics lacks %s\n%s\n%s", want, body, errOut.String())
		}
	}
	// The reasons are in the log.
	if !strings.Contains(errOut.String(), "metrics: refused") || !strings.Contains(errOut.String(), "bad_token") {
		t.Errorf("refusals are not logged:\n%s", errOut.String())
	}
}

// [AC-161-4] / [AC-161-5] start-up refusals.
func TestListenAuth_StartupRefusals(t *testing.T) {
	e := newListenAuthEnv(t)
	base := []string{"--intent", e.intentDir, "--state", e.statePath, "--interval", "100ms", "--mode", "alert-only"}
	tokenOnly := filepath.Join(filepath.Dir(e.configPath), "token-only.yaml")
	if err := os.WriteFile(tokenOnly, []byte("secret_store: secrets.json\nread:\n  bearer_token: secretref:file/sakutei/metrics_token\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"metrics plaintext on all interfaces", []string{"--metrics-listen", "0.0.0.0:0"}, "--metrics-insecure-listen"},
		{"metrics plaintext on an empty host", []string{"--metrics-listen", ":0"}, "--metrics-insecure-listen"},
		{"read plaintext on all interfaces", []string{"--read-listen", "0.0.0.0:0"}, "--read-insecure-listen"},
		{"read token without TLS", []string{"--read-listen", "0.0.0.0:0", "--listen-auth-config", tokenOnly}, "no TLS"},
		{"approval on the read address", []string{"--read-listen", "127.0.0.1:9471", "--approval-listen", "127.0.0.1:9471",
			"--workflow-store", filepath.Join(t.TempDir(), "wf.json"), "--audit-log", filepath.Join(t.TempDir(), "audit.log")}, "shares neither"},
	} {
		out, code := runSakutei(t, append(append([]string{}, base...), tc.args...)...)
		if code == 0 || !strings.Contains(out, tc.want) {
			t.Errorf("[AC-161-4] %s: exit %d, want a refusal naming %q:\n%s", tc.name, code, tc.want, out)
		}
	}

	// With the explicit flags the plaintext listeners start.
	cmd, out, errOut := startDaemonWithArgs(t, e.intentDir, e.statePath, "100ms", "--mode", "alert-only",
		"--metrics-listen", "0.0.0.0:0", "--metrics-insecure-listen", "--read-listen", "0.0.0.0:0", "--read-insecure-listen")
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if !waitForCondition(10*time.Second, func() bool {
		return strings.Contains(out.String(), "metrics listening on http://") &&
			strings.Contains(out.String(), "read-only API listening on http://")
	}) {
		t.Fatalf("[AC-161-4] the insecure flags did not open the plaintext listeners:\n%s\n%s", out.String(), errOut.String())
	}
}
