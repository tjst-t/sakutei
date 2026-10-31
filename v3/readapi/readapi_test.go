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

package readapi_test

import (
	"bufio"
	"context"
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
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/config"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/daemon"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/readapi"
	"github.com/tjst-t/sakutei/v3/record"
)

func newServer(t *testing.T) (*readapi.Server, *record.Record) {
	t.Helper()
	rec, err := record.Open(filepath.Join(t.TempDir(), "record"))
	if err != nil {
		t.Fatal(err)
	}
	s := &readapi.Server{
		RecordDir: rec.Dir(),
		Plan: func(context.Context) (plan.Result, error) {
			return plan.Result{Summary: map[plan.Op]int{plan.Update: 1}, Changes: []plan.Change{{Resource: "p/rec/a", Op: plan.Update}}}, nil
		},
		Graph: func(context.Context) (readapi.Graph, error) {
			return readapi.GraphOf([]core.Resource{
				{ID: core.ID{Type: "p/rec", Name: "a"}},
				{ID: core.ID{Type: "p/rec", Name: "b"}, DependsOn: []core.Dependency{{On: core.ID{Type: "p/rec", Name: "a"}}}},
			}), nil
		},
	}
	return s, rec
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// E-H5・E-L4：/status は最後の周の結果（reconcile --json と同じ構造）、/plan は
// plan --json と同じ構造、/graph は依存、/health は続いた誤りで 503。書き込みは断る。
func TestEndpoints(t *testing.T) {
	s, rec := newServer(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	if code, _ := get(t, ts.URL+"/status"); code != http.StatusServiceUnavailable {
		t.Fatalf("周の前の /status: %d", code)
	}
	r := plan.Result{Summary: map[plan.Op]int{plan.None: 1}, Changes: []plan.Change{{Resource: "p/rec/a", Op: plan.None}}}
	s.Publish(daemon.Cycle{N: 1, Result: &r})
	s.Publish(daemon.Cycle{N: 2, Err: io.EOF}) // 結果の無い周は前の結果を残す
	var want strings.Builder
	r.WriteJSON(&want)
	if code, body := get(t, ts.URL+"/status"); code != 200 || body != want.String() {
		t.Fatalf("/status %d:\n%s", code, body)
	}
	if code, body := get(t, ts.URL+"/plan"); code != 200 || !strings.Contains(body, `"op": "update"`) {
		t.Fatalf("/plan %d:\n%s", code, body)
	}
	if code, body := get(t, ts.URL+"/graph"); code != 200 || !strings.Contains(body, `"from": "p/rec/b"`) {
		t.Fatalf("/graph %d:\n%s", code, body)
	}

	if code, _ := get(t, ts.URL+"/health"); code != http.StatusServiceUnavailable {
		t.Fatalf("周の前の /health: %d", code)
	}
	now := time.Date(2026, 11, 5, 22, 0, 0, 0, time.UTC)
	rec.RecordCycle(now, "Record に書けない")
	if code, body := get(t, ts.URL+"/health"); code != http.StatusServiceUnavailable || !strings.Contains(body, `"consecutive_errors": 1`) {
		t.Fatalf("/health %d %s", code, body)
	}
	rec.RecordCycle(now.Add(time.Minute), "")
	if code, _ := get(t, ts.URL+"/health"); code != 200 {
		t.Fatalf("/health %d", code)
	}

	resp, err := http.Post(ts.URL+"/status", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", resp.StatusCode)
	}
}

// /events は送り出しの列を SSE で流す。?since の続きから、後から積んだものも流す。
func TestEventsSSE(t *testing.T) {
	defer func(d time.Duration) { readapi.PollInterval = d }(readapi.PollInterval)
	readapi.PollInterval = 10 * time.Millisecond
	s, rec := newServer(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	at := time.Date(2026, 11, 5, 22, 0, 0, 0, time.UTC)
	rec.AppendOutbox([]core.Event{{Seq: 1, Kind: "Drift", At: at}, {Seq: 2, Kind: "CycleDone", At: at}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events?since=1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	var ids []string
	readID := func() {
		t.Helper()
		for sc.Scan() {
			if id, ok := strings.CutPrefix(sc.Text(), "id: "); ok {
				ids = append(ids, id)
				return
			}
		}
		t.Fatalf("流れが切れた: %v", sc.Err())
	}
	readID()
	rec.AppendOutbox([]core.Event{{Seq: 3, Kind: "Applied", At: at}})
	readID()
	if strings.Join(ids, ",") != "2,3" {
		t.Fatalf("ids %v", ids)
	}
	if code, _ := get(t, ts.URL+"/events?since=x"); code != http.StatusBadRequest {
		t.Fatalf("since=x: %d", code)
	}
}

// 遅い読み手で周を止めない：/plan を計算している間の 2 つ目の /plan は待たずに 503。
// /status は /plan の計算を待たない。
func TestSlowPlanDoesNotBlock(t *testing.T) {
	s, _ := newServer(t)
	started, release := make(chan struct{}), make(chan struct{})
	s.Plan = func(context.Context) (plan.Result, error) {
		close(started)
		<-release
		return plan.Result{}, nil
	}
	r := plan.Result{}
	s.Publish(daemon.Cycle{Result: &r})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	done := make(chan struct{})
	go func() {
		get(t, ts.URL+"/plan")
		close(done)
	}()
	<-started
	if code, _ := get(t, ts.URL+"/plan"); code != http.StatusServiceUnavailable {
		t.Fatalf("2 つ目の /plan: %d", code)
	}
	if code, _ := get(t, ts.URL+"/status"); code != 200 {
		t.Fatalf("/status: %d", code)
	}
	close(release)
	<-done
}

// N-L4・E-L3：読むだけのトークン（Bearer）。値は誤りの文に出さない。
func TestTokenAuth(t *testing.T) {
	s, _ := newServer(t)
	dir := t.TempDir()
	tokens := filepath.Join(dir, "tokens")
	os.WriteFile(tokens, []byte("# 監査者\nsecret-token-1\n"), 0o600)
	srv, ln, err := readapi.Listen(&config.ReadListen{Address: "127.0.0.1:0", TokensFile: tokens}, s.Handler())
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Close()
	url := "http://" + ln.Addr().String() + "/graph"
	for _, auth := range []string{"", "Bearer wrong", "secret-token-1"} {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized || strings.Contains(string(body), "secret-token-1") {
			t.Fatalf("%q: %d %s", auth, resp.StatusCode, body)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer secret-token-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("正しいトークン: %d", resp.StatusCode)
	}
}

// N-L4：TLS とクライアント証明書。証明書の無いクライアントは断る。
func TestClientCertificate(t *testing.T) {
	dir := t.TempDir()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	ca, _ := x509.ParseCertificate(caDER)
	issue := func(name string, usage x509.ExtKeyUsage) (string, string, tls.Certificate) {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{usage},
			IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		kder, _ := x509.MarshalECPrivateKey(key)
		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
		cf, kf := filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
		os.WriteFile(cf, certPEM, 0o600)
		os.WriteFile(kf, keyPEM, 0o600)
		pair, _ := tls.X509KeyPair(certPEM, keyPEM)
		return cf, kf, pair
	}
	caFile := filepath.Join(dir, "ca.crt")
	os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600)
	scert, skey, _ := issue("server", x509.ExtKeyUsageServerAuth)
	_, _, client := issue("client", x509.ExtKeyUsageClientAuth)

	s, _ := newServer(t)
	srv, ln, err := readapi.Listen(&config.ReadListen{Address: "127.0.0.1:0", TLSCert: scert, TLSKey: skey, ClientCA: caFile}, s.Handler())
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	url := "https://" + ln.Addr().String() + "/graph"
	without := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	if resp, err := without.Get(url); err == nil {
		resp.Body.Close()
		t.Fatalf("証明書の無いクライアントを通した: %d", resp.StatusCode)
	}
	with := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{client}}}}
	resp, err := with.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	var g readapi.Graph
	json.NewDecoder(resp.Body).Decode(&g)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(g.Edges) != 1 {
		t.Fatalf("%d %+v", resp.StatusCode, g)
	}
}
