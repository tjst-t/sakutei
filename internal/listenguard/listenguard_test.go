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

package listenguard

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// selfSigned returns a PEM certificate and key for 127.0.0.1 with cn.
func selfSigned(t *testing.T, cn string) (string, string, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalECPrivateKey(key)
	c, _ := x509.ParseCertificate(der)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd})), c
}

func writeStore(t *testing.T, path string, values map[string]string) {
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

func TestIsLoopbackAndCheckAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:9470": true, "[::1]:9470": true, "localhost:9470": true, "127.0.0.2:1": true,
		"0.0.0.0:9470": false, ":9470": false, "192.0.2.1:9470": false, "metrics.example:9470": false, "bad": false,
	} {
		if got := IsLoopback(addr); got != want {
			t.Errorf("IsLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
	ref := core.NewSecretRef("file", "t", 0)
	tok := Listener{Token: &ref}
	full := Listener{TLS: &TLS{}, Token: tok.Token}
	if err := CheckAddr("metrics-listen", "metrics-insecure-listen", "0.0.0.0:1", Listener{}, false); err == nil ||
		!strings.Contains(err.Error(), "--metrics-insecure-listen") {
		t.Errorf("plaintext non-loopback: %v", err)
	}
	if err := CheckAddr("metrics-listen", "metrics-insecure-listen", "0.0.0.0:1", tok, false); err == nil ||
		!strings.Contains(err.Error(), "no TLS") {
		t.Errorf("token without TLS: %v", err)
	}
	if err := CheckAddr("metrics-listen", "metrics-insecure-listen", "0.0.0.0:1", Listener{TLS: &TLS{}}, false); err == nil {
		t.Error("TLS without authorization on a non-loopback address was accepted")
	}
	for _, ok := range []error{
		CheckAddr("m", "i", "0.0.0.0:1", Listener{}, true),
		CheckAddr("m", "i", "127.0.0.1:1", Listener{}, false),
		CheckAddr("m", "i", "0.0.0.0:1", full, false),
	} {
		if ok != nil {
			t.Errorf("refused: %v", ok)
		}
	}
}

func TestParseConfigRefusals(t *testing.T) {
	dir := t.TempDir()
	cert, key, _ := selfSigned(t, "s")
	writeStore(t, filepath.Join(dir, "secrets.json"), map[string]string{"c": cert, "k": key, "t": "s3cr3t-v4lue", "empty": ""})
	for name, tc := range map[string]struct {
		tree map[string]any
		want string
	}{
		"unknown key":     {map[string]any{"metrics": map[string]any{"token": "x"}}, `unknown key "metrics.token"`},
		"raw token":       {map[string]any{"secret_store": "secrets.json", "metrics": map[string]any{"bearer_token": "s3cr3t-v4lue"}}, "must be a SecretRef"},
		"no store":        {map[string]any{"metrics": map[string]any{"bearer_token": "secretref:file/t"}}, `"secret_store" is required`},
		"missing secret":  {map[string]any{"secret_store": "secrets.json", "metrics": map[string]any{"bearer_token": "secretref:file/none"}}, "bearer_token"},
		"empty token":     {map[string]any{"secret_store": "secrets.json", "metrics": map[string]any{"bearer_token": "secretref:file/empty"}}, "is empty"},
		"old TLS":         {map[string]any{"secret_store": "secrets.json", "read": map[string]any{"tls": map[string]any{"cert": "secretref:file/c", "key": "secretref:file/k", "min_version": "1.1"}}}, "min_version"},
		"no key":          {map[string]any{"secret_store": "secrets.json", "read": map[string]any{"tls": map[string]any{"cert": "secretref:file/c"}}}, "tls.key: required"},
		"bad pair":        {map[string]any{"secret_store": "secrets.json", "read": map[string]any{"tls": map[string]any{"cert": "secretref:file/c", "key": "secretref:file/t"}}}, "not a valid PEM key pair"},
		"CA without TLS":  {map[string]any{"read": map[string]any{"client_ca_file": "ca.pem"}}, "needs tls"},
		"subjects w/o CA": {map[string]any{"read": map[string]any{"allowed_subjects": []any{"x"}}}, "need client_ca_file"},
	} {
		_, err := parseConfig(tc.tree, dir)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("%s: the error quotes a secret value: %v", name, err)
		}
	}
	cfg, err := parseConfig(map[string]any{"secret_store": "secrets.json",
		"metrics": map[string]any{"tls": map[string]any{"cert": "secretref:file/c", "key": "secretref:file/k", "min_version": "1.3"},
			"bearer_token": "secretref:file/t"}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Metrics.TLS.MinVersion != tls.VersionTLS13 || !cfg.Metrics.Authorizes() || cfg.Read.Configured() {
		t.Errorf("config = %+v", cfg)
	}
}

// The certificate is read again from the store without a restart.
func TestServe_ReloadsCertificate(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "secrets.json")
	cert1, key1, c1 := selfSigned(t, "first")
	writeStore(t, store, map[string]string{"c": cert1, "k": key1, "t": "tok-1"})
	cfg, err := parseConfig(map[string]any{"secret_store": "secrets.json",
		"metrics": map[string]any{"tls": map[string]any{"cert": "secretref:file/c", "key": "secretref:file/k"},
			"bearer_token": "secretref:file/t"}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	rej := NewRejects()
	s, err := Serve(Options{Name: "metrics", Addr: "127.0.0.1:0", Listener: cfg.Metrics, SecretStore: cfg.SecretStore,
		Rejects: rej, ReloadInterval: 50 * time.Millisecond,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	get := func(root *x509.Certificate, token string) (int, error) {
		pool := x509.NewCertPool()
		pool.AddCert(root)
		c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
		req, _ := http.NewRequest("GET", s.URL+"/", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := c.Do(req)
		if err != nil {
			return 0, err
		}
		resp.Body.Close()
		return resp.StatusCode, nil
	}
	if code, err := get(c1, "tok-1"); err != nil || code != 200 {
		t.Fatalf("first certificate: %d %v", code, err)
	}
	cert2, key2, c2 := selfSigned(t, "second")
	writeStore(t, store, map[string]string{"c": cert2, "k": key2, "t": "tok-2"})
	time.Sleep(100 * time.Millisecond)
	var code int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if code, err = get(c2, "tok-2"); err == nil && code == 200 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if code != 200 {
		t.Fatalf("after the reload: %d %v", code, err)
	}
	if code, _ := get(c2, "tok-1"); code != 401 {
		t.Errorf("the old token still works after the reload: %d", code)
	}
	// A broken store keeps the previous credentials.
	if err := os.WriteFile(store, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if code, err := get(c2, "tok-2"); err != nil || code != 200 {
		t.Errorf("a broken store dropped the credentials: %d %v", code, err)
	}
	if rej.Count("metrics", ReasonBadToken) != 1 {
		t.Errorf("bad_token count = %d", rej.Count("metrics", ReasonBadToken))
	}
}
