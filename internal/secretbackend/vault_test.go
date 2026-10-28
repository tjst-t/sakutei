// Copyright 2026 tjst-t
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package secretbackend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const value = "TOPSECRET-v"

// fakeKV is a KV v2 mount "secret" with one secret "app/db" and AppRole.
type fakeKV struct {
	mu       sync.Mutex
	versions []map[string]any // index = version-1
	logins   int
	revoked  bool
	sealed   bool
	tokens   map[string]bool
}

func (f *fakeKV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sealed {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"errors":["Vault is sealed"]}`))
		return
	}
	if r.URL.Path == "/v1/auth/approle/login" {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["role_id"] != "rid" || body["secret_id"] != "sid" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"errors":["invalid role or secret ID"]}`))
			return
		}
		f.logins++
		tok := "approle-token-" + string(rune('0'+f.logins))
		f.tokens[tok] = true
		f.revoked = false
		json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"client_token": tok, "lease_duration": 3600}})
		return
	}
	tok := r.Header.Get("X-Vault-Token")
	if !f.tokens[tok] || (f.revoked && strings.HasPrefix(tok, "approle-")) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"errors":["permission denied"]}`))
		return
	}
	switch r.URL.Path {
	case "/v1/secret/metadata/app/db":
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"current_version": len(f.versions),
			"versions": map[string]any{"1": map[string]any{"deletion_time": "", "destroyed": false}}}})
	case "/v1/secret/data/app/db":
		v := len(f.versions)
		if q := r.URL.Query().Get("version"); q != "" {
			v = int(q[0] - '0')
		}
		if v < 1 || v > len(f.versions) {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"errors":[]}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": f.versions[v-1], "metadata": map[string]any{"version": v}}})
	default:
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"errors":[]}`))
	}
}

func newFakeKV(t *testing.T) (*fakeKV, *httptest.Server) {
	f := &fakeKV{tokens: map[string]bool{"file-token": true},
		versions: []map[string]any{{"password": value + "1", "n": 3.0}}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func writeFile(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestVault_TokenFile(t *testing.T) {
	f, srv := newFakeKV(t)
	v, err := NewVault(map[string]string{"address": srv.URL, "token_file": writeFile(t, "tok", "file-token\n")})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	gen, err := v.Generation(ctx, "app/db#password")
	if err != nil || gen != 1 {
		t.Fatalf("Generation = %d, %v; want 1", gen, err)
	}
	got, err := v.Resolve(ctx, "app/db#password", 1)
	if err != nil || got != value+"1" {
		t.Fatalf("Resolve = %q, %v", got, err)
	}
	f.mu.Lock()
	f.versions = append(f.versions, map[string]any{"password": value + "2"})
	f.mu.Unlock()
	if gen, _ := v.Generation(ctx, "app/db"); gen != 2 {
		t.Errorf("Generation after a new version = %d, want 2", gen)
	}
	// A pinned generation reads that version.
	if got, _ := v.Resolve(ctx, "app/db#password", 1); got != value+"1" {
		t.Errorf("Resolve(gen 1) = %q", got)
	}
	if got, _ := v.Resolve(ctx, "app/db#password", 2); got != value+"2" {
		t.Errorf("Resolve(gen 2) = %q", got)
	}
	// No such key; a non-string key; no "#key" reads "value".
	for _, p := range []string{"app/db#nope", "app/db#n", "app/db"} {
		if _, err := v.Resolve(ctx, p, 1); err == nil || strings.Contains(err.Error(), value) {
			t.Errorf("Resolve(%s) = %v, want an error without the value", p, err)
		}
	}
	// Sealed: an error naming the status, never a value.
	f.mu.Lock()
	f.sealed = true
	f.mu.Unlock()
	if _, err := v.Generation(ctx, "app/db"); err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("sealed: %v, want HTTP 503", err)
	}
}

func TestVault_AppRole(t *testing.T) {
	f, srv := newFakeKV(t)
	v, err := NewVault(map[string]string{"address": srv.URL,
		"approle_role_id_file": writeFile(t, "rid", "rid"), "approle_secret_id_file": writeFile(t, "sid", "sid\n")})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := v.Generation(ctx, "app/db"); err != nil {
			t.Fatal(err)
		}
	}
	if f.logins != 1 {
		t.Errorf("logins = %d, want 1 (the token is kept)", f.logins)
	}
	// A revoked token: one 403, one new login, and the request succeeds.
	f.mu.Lock()
	f.revoked = true
	f.mu.Unlock()
	if _, err := v.Generation(ctx, "app/db"); err != nil {
		t.Fatalf("after revoke: %v", err)
	}
	if f.logins != 2 {
		t.Errorf("logins = %d, want 2", f.logins)
	}
	bad, _ := NewVault(map[string]string{"address": srv.URL,
		"approle_role_id_file": writeFile(t, "rid", "rid"), "approle_secret_id_file": writeFile(t, "sid", "wrong")})
	if _, err := bad.Generation(ctx, "app/db"); err == nil || !strings.Contains(err.Error(), "AppRole login") {
		t.Errorf("bad secret_id: %v", err)
	}
}

func TestVault_Unreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.URL
	srv.Close()
	v, err := NewVault(map[string]string{"address": addr, "token_file": writeFile(t, "tok", "x")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Generation(context.Background(), "app/db"); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("Generation = %v, want unreachable", err)
	}
}

func TestVault_Settings(t *testing.T) {
	tok := writeFile(t, "tok", "x")
	for name, s := range map[string]map[string]string{
		"no address":        {"token_file": tok},
		"no auth":           {"address": "http://127.0.0.1:8200"},
		"both auths":        {"address": "http://127.0.0.1:8200", "token_file": tok, "approle_role_id_file": tok, "approle_secret_id_file": tok},
		"half approle":      {"address": "http://127.0.0.1:8200", "approle_role_id_file": tok},
		"plain http remote": {"address": "http://vault.example:8200", "token_file": tok},
		"unknown key":       {"address": "https://vault.example:8200", "token_file": tok, "tokn": "x"},
		"bad timeout":       {"address": "https://vault.example:8200", "token_file": tok, "timeout": "soon"},
	} {
		if _, err := NewVault(s); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := NewVault(map[string]string{"address": "http://vault.example:8200", "token_file": tok, "allow_plain_http": "true"}); err != nil {
		t.Errorf("allow_plain_http: %v", err)
	}
}

func TestSplitKey(t *testing.T) {
	for in, want := range map[string][2]string{
		"app/db#password": {"app/db", "password"},
		"app/db":          {"app/db", "value"},
		"/app/db/#k":      {"app/db", "k"},
	} {
		s, k, err := SplitKey(in)
		if err != nil || s != want[0] || k != want[1] {
			t.Errorf("SplitKey(%q) = %q %q %v", in, s, k, err)
		}
	}
	for _, in := range []string{"#k", "app/db#", "app/../x", "a//b", "app/db#generic"} {
		if _, _, err := SplitKey(in); err == nil {
			t.Errorf("SplitKey(%q): want an error", in)
		}
	}
}
