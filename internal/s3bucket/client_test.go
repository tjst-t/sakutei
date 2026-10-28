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

package s3bucket

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// recorder は来た要求の Host とパスを覚える。
type recorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *recorder) handler(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.seen = append(r.seen, req.Method+" "+req.Host+" "+req.URL.RequestURI())
	r.mu.Unlock()
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`))
}

func newRecordingClient(t *testing.T, virtual bool, basePath string) (*recorder, *Session) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	c, err := NewClient(ClientConfig{
		Endpoint: "http://s3.test:" + port, VirtualHost: virtual, APIBasePath: basePath,
		Credentials: func(context.Context) (Credentials, error) { return Credentials{AccessKey: "a", SecretKey: "b"}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	// s3.test とその下の名前を偽サーバへ向ける。
	tr := c.hc.Transport.(*http.Transport)
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(srv.URL, "http://"))
	}
	s, err := c.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rec, s
}

func TestAddressing(t *testing.T) {
	for _, tc := range []struct {
		virtual  bool
		basePath string
		want     string
	}{
		{false, "", "GET s3.test:PORT /my-bucket?versioning"},
		{true, "", "GET my-bucket.s3.test:PORT /?versioning"},
		{false, "/", "GET s3.test:PORT /my-bucket?versioning"},
		// API のパスの前置き（#160）。
		{false, "/gw/s3/", "GET s3.test:PORT /gw/s3/my-bucket?versioning"},
		{true, "/gw/s3", "GET my-bucket.s3.test:PORT /gw/s3/?versioning"},
	} {
		rec, s := newRecordingClient(t, tc.virtual, tc.basePath)
		if v, err := s.GetVersioning(context.Background(), "my-bucket"); err != nil || v != "Enabled" {
			t.Fatalf("GetVersioning = %q, %v", v, err)
		}
		got := rec.seen[0]
		host := strings.Fields(got)[1]
		_, port, _ := net.SplitHostPort(host)
		if want := strings.Replace(tc.want, "PORT", port, 1); got != want {
			t.Errorf("virtual=%v base=%q: request = %q, want %q", tc.virtual, tc.basePath, got, want)
		}
	}
}

func TestNewClient_RejectsBadEndpoints(t *testing.T) {
	creds := func(context.Context) (Credentials, error) { return Credentials{}, nil }
	for _, ep := range []string{"", "s3.example.net", "ftp://x", "http://", "http://h/path", "http://h?q=1", "http://u:p@h"} {
		if _, err := NewClient(ClientConfig{Endpoint: ep, Credentials: creds}); err == nil {
			t.Errorf("NewClient(%q) succeeded", ep)
		}
	}
}

func TestNewClient_RejectsBadAPIBasePath(t *testing.T) {
	creds := func(context.Context) (Credentials, error) { return Credentials{}, nil }
	for _, bp := range []string{"s3", "/a//b", "/a/../b", "/a?x", "/a b"} {
		if _, err := NewClient(ClientConfig{Endpoint: "http://h:9000", APIBasePath: bp, Credentials: creds}); err == nil {
			t.Errorf("NewClient(api_base_path %q) succeeded", bp)
		}
	}
}

func TestErrorKinds(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		kind   ErrorKind
	}{
		{501, "", KindNotImplemented},
		{400, "<Error><Code>NotImplemented</Code></Error>", KindNotImplemented},
		{403, "<Error><Code>AccessDenied</Code></Error>", KindAuth},
		{401, "", KindAuth},
		{500, "", KindUnreachable},
		{503, "<Error><Code>SlowDown</Code></Error>", KindUnreachable},
		{301, "<Error><Code>PermanentRedirect</Code></Error>", KindRequest},
		{409, "<Error><Code>BucketNotEmpty</Code><Message>not empty</Message></Error>", KindRequest},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		c, _ := NewClient(ClientConfig{Endpoint: srv.URL, Credentials: func(context.Context) (Credentials, error) {
			return Credentials{AccessKey: "a", SecretKey: "top-secret"}, nil
		}})
		s, _ := c.Open(context.Background())
		_, err := s.GetVersioning(context.Background(), "bucket")
		if KindOf(err) != tc.kind {
			t.Errorf("HTTP %d %s: kind = %v (%v), want %v", tc.status, tc.body, KindOf(err), err, tc.kind)
		}
		if err != nil && strings.Contains(err.Error(), "top-secret") {
			t.Errorf("the error shows the secret key: %v", err)
		}
		srv.Close()
	}
}
