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

// fakeKV は KV v2 の mount "secret" に secret "app/db" を 1 つ持ち、AppRole を受け付ける。
type fakeKV struct {
	mu       sync.Mutex
	versions []map[string]any // 添字 = バージョン-1
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

func TestVaultTokenFile(t *testing.T) {
	f, srv := newFakeKV(t)
	v, err := VaultModule(map[string]any{"address": srv.URL, "token_file": writeFile(t, "tok", "file-token\n")})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	gen, err := v.Generation(ctx, "app/db#password")
	if err != nil || gen != "1" {
		t.Fatalf("Generation = %q, %v; want 1", gen, err)
	}
	got, err := v.Resolve(ctx, "app/db#password", "1")
	if err != nil || got != value+"1" {
		t.Fatalf("Resolve = %q, %v", got, err)
	}
	f.mu.Lock()
	f.versions = append(f.versions, map[string]any{"password": value + "2"})
	f.mu.Unlock()
	if gen, _ := v.Generation(ctx, "app/db"); gen != "2" {
		t.Errorf("新しいバージョンのあとの Generation = %q, want 2", gen)
	}
	// 世代を指定すればそのバージョンを読む。
	if got, _ := v.Resolve(ctx, "app/db#password", "1"); got != value+"1" {
		t.Errorf("Resolve(1) = %q", got)
	}
	if got, _ := v.Resolve(ctx, "app/db#password", "2"); got != value+"2" {
		t.Errorf("Resolve(2) = %q", got)
	}
	for _, p := range []string{"app/db#nope", "app/db#n", "app/db"} {
		if _, err := v.Resolve(ctx, p, "1"); err == nil || strings.Contains(err.Error(), value) {
			t.Errorf("Resolve(%s) = %v, want 値を含まないエラー", p, err)
		}
	}
	f.mu.Lock()
	f.sealed = true
	f.mu.Unlock()
	if _, err := v.Generation(ctx, "app/db"); err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("sealed: %v, want HTTP 503", err)
	}
}

func TestVaultAppRole(t *testing.T) {
	f, srv := newFakeKV(t)
	approle := map[string]any{"role_id_file": writeFile(t, "rid", "rid"), "secret_id_file": writeFile(t, "sid", "sid\n")}
	v, err := VaultModule(map[string]any{"address": srv.URL, "approle": approle})
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
		t.Errorf("ログイン %d 回, want 1（トークンを持ち続ける）", f.logins)
	}
	f.mu.Lock()
	f.revoked = true
	f.mu.Unlock()
	if _, err := v.Generation(ctx, "app/db"); err != nil {
		t.Fatalf("失効のあと: %v", err)
	}
	if f.logins != 2 {
		t.Errorf("ログイン %d 回, want 2", f.logins)
	}
	bad, _ := VaultModule(map[string]any{"address": srv.URL,
		"approle": map[string]any{"role_id_file": writeFile(t, "rid", "rid"), "secret_id_file": writeFile(t, "sid", "wrong")}})
	if _, err := bad.Generation(ctx, "app/db"); err == nil || !strings.Contains(err.Error(), "AppRole のログイン") {
		t.Errorf("誤った secret_id: %v", err)
	}
}

// TestVaultUnreachable：接続できなければエラー（コアはその secret に依る宣言を Unknown にする、E-G4）。
func TestVaultUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.URL
	srv.Close()
	v, err := VaultModule(map[string]any{"address": addr, "token_file": writeFile(t, "tok", "x")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Generation(context.Background(), "app/db"); err == nil || !strings.Contains(err.Error(), "接続できない") {
		t.Errorf("Generation = %v, want 接続できない", err)
	}
}

func TestVaultSettings(t *testing.T) {
	tok := writeFile(t, "tok", "x")
	for name, s := range map[string]map[string]any{
		"address が無い": {"token_file": tok},
		"認証が無い":       {"address": "http://127.0.0.1:8200"},
		"認証が 2 つ":     {"address": "http://127.0.0.1:8200", "token_file": tok, "approle": map[string]any{"role_id_file": tok, "secret_id_file": tok}},
		"approle が半分": {"address": "http://127.0.0.1:8200", "approle": map[string]any{"role_id_file": tok}},
		"外への平文":       {"address": "http://vault.example:8200", "token_file": tok},
		"未知の設定":       {"address": "https://vault.example:8200", "token_file": tok, "tokn": "x"},
		"timeout が不正": {"address": "https://vault.example:8200", "token_file": tok, "timeout": "soon"},
		"plain の型が違う": {"address": "http://vault.example:8200", "token_file": tok, "allow_plain_http": "true"},
	} {
		if _, err := VaultModule(s); err == nil {
			t.Errorf("%s: エラーを返さない", name)
		}
	}
	if _, err := VaultModule(map[string]any{"address": "http://vault.example:8200", "token_file": tok, "allow_plain_http": true}); err != nil {
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
	for _, in := range []string{"#k", "app/db#", "app/../x", "a//b"} {
		if _, _, err := SplitKey(in); err == nil {
			t.Errorf("SplitKey(%q): エラーを返さない", in)
		}
	}
}

func script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "get-secret")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCommand(t *testing.T) {
	c, err := CommandModule(map[string]any{"command": script(t, `
case "$1" in
generation) [ "$2" = app/token ] && echo 7 && exit 0; echo "no such secret $2" >&2; exit 3 ;;
value) [ "$3" = 7 ] && printf '%s\n' "`+value+`"; exit 0 ;;
esac
exit 2
`)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if g, err := c.Generation(ctx, "app/token"); err != nil || g != "7" {
		t.Fatalf("Generation = %q, %v", g, err)
	}
	if v, err := c.Resolve(ctx, "app/token", "7"); err != nil || v != value {
		t.Fatalf("Resolve = %q, %v", v, err)
	}
	_, err = c.Generation(ctx, "app/other")
	if err == nil || !strings.Contains(err.Error(), "終了コード 3") || !strings.Contains(err.Error(), "no such secret app/other") {
		t.Errorf("Generation(無い) = %v", err)
	}
}

func TestCommandNeverQuotesValue(t *testing.T) {
	c, err := CommandModule(map[string]any{"command": script(t, `
case "$1" in
generation) echo "`+value+`" ;;
value) echo "`+value+`"; echo "`+value+`" >&2; exit 1 ;;
esac
`)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.Generation(ctx, "x"); err == nil || strings.Contains(err.Error(), value) {
		t.Errorf("数でない世代: %v", err)
	}
	if _, err := c.Resolve(ctx, "x", "1"); err == nil || strings.Contains(err.Error(), value) {
		t.Errorf("Resolve の失敗: %v", err)
	}
}

func TestCommandTimeout(t *testing.T) {
	c, err := CommandModule(map[string]any{"command": script(t, "exec sleep 5\n"), "timeout": "200ms"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Generation(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "タイムアウト") {
		t.Errorf("Generation = %v, want タイムアウト", err)
	}
}

func TestCommandSettings(t *testing.T) {
	for name, s := range map[string]map[string]any{
		"command が無い": {},
		"見つからない":      {"command": "/nonexistent/get-secret"},
		"未知の設定":       {"command": "/bin/sh", "args": "x"},
	} {
		if _, err := CommandModule(s); err == nil {
			t.Errorf("%s: エラーを返さない", name)
		}
	}
}
