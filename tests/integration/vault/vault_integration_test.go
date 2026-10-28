//go:build integration_vault

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

package vault

// サーバのイメージは SAKUTEI_IT_OPENBAO_IMAGE で差し替えられる（既定は tag と
// digest で固定）。
//
//	IT-188-1  token のファイル：reconcile が KV の版を世代として ref に入れ、その版の
//	          値で Apply する（値のダイジェストだけがファイルに入る）
//	IT-188-2  版が上がると plan に差分（cause E）が出て、reconcile が新しい版で Apply する
//	IT-188-3  AppRole：role_id・secret_id のファイルでログインして同じことをする
//	IT-188-4  常駐：版が上がると次の周で Apply する
//	IT-188-5  届かない（閉じたポート）・sealed：そのリソースは unknown（plan は計画しない、
//	          reconcile は触らず終了コード 1、status は観測できないもの）。ほかは収束する
//	IT-188-6  値はどの出力にも記録のファイルにも出ない

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 既定のイメージ。digest は docker pull で確かめた値（2026-10-28）。
const openbaoImage = "openbao/openbao:2.4.1@sha256:597f62847dd382382056a1d6704d50465908c2040038c4611832a23269a67112"

const (
	rootToken = "sakutei-it-root"
	container = "sakutei-vaultit-openbao"
	secretPfx = "VAULT-IT-SECRET-"
)

var lab struct {
	dir     string
	bin     string
	addr    string
	token   string // file
	roleID  string // file
	secretI string // file
}

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	if err := setup(); err != nil {
		fmt.Fprintf(os.Stderr, "integration-vault: %v\n", err)
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

func setup() error {
	if out, err := docker("version", "--format", "{{.Server.Version}}"); err != nil {
		return fmt.Errorf("docker (through sudo -n) is required: %v: %s", err, out)
	}
	dir, err := os.MkdirTemp("", "sakutei-vault-it-")
	if err != nil {
		return err
	}
	lab.dir = dir

	// The sakutei binary with the example kvfile provider (pkg/sdk resolver).
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		return err
	}
	lab.bin = filepath.Join(dir, "sakutei-ext")
	build := exec.Command("go", "build", "-o", lab.bin, ".")
	build.Dir = filepath.Join(root, "examples", "external-provider")
	build.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "GOPROXY=off")
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("go build the example: %v\n%s", err, out)
	}

	img := openbaoImage
	if v, ok := os.LookupEnv("SAKUTEI_IT_OPENBAO_IMAGE"); ok && v != "" {
		img = v
	}
	if out, err := docker("pull", img); err != nil {
		return fmt.Errorf("docker pull %s: %v: %s", img, err, out)
	}
	port, err := freePort()
	if err != nil {
		return err
	}
	_, _ = docker("rm", "-f", "-v", container)
	if out, err := docker("run", "-d", "--name", container, "-p", "127.0.0.1:"+port+":8200",
		"-e", "BAO_DEV_ROOT_TOKEN_ID="+rootToken, "-e", "BAO_DEV_LISTEN_ADDRESS=0.0.0.0:8200",
		img, "server", "-dev"); err != nil {
		return fmt.Errorf("docker run: %v: %s", err, out)
	}
	lab.addr = "http://127.0.0.1:" + port
	end := time.Now().Add(time.Minute)
	for {
		resp, err := http.Get(lab.addr + "/v1/sys/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(end) {
			logs, _ := docker("logs", container)
			return fmt.Errorf("OpenBao did not become healthy: %v\n%s", err, logs)
		}
		time.Sleep(300 * time.Millisecond)
	}

	// A read-only policy on secret/app/*, a token with it, and an AppRole
	// with it.
	policy := `path "secret/data/app/*" { capabilities = ["read"] }
path "secret/metadata/app/*" { capabilities = ["read"] }`
	steps := []struct {
		method, path string
		body         any
	}{
		{"PUT", "/v1/sys/policies/acl/sakutei-read", map[string]any{"policy": policy}},
		{"POST", "/v1/sys/auth/approle", map[string]any{"type": "approle"}},
		{"POST", "/v1/auth/approle/role/sakutei", map[string]any{"token_policies": []string{"sakutei-read"}, "token_ttl": "1h"}},
	}
	for _, s := range steps {
		if _, err := bao(s.method, s.path, s.body); err != nil {
			return err
		}
	}
	var tok struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if err := baoJSON("POST", "/v1/auth/token/create", map[string]any{"policies": []string{"sakutei-read"}, "ttl": "1h"}, &tok); err != nil {
		return err
	}
	var rid struct {
		Data struct {
			RoleID string `json:"role_id"`
		} `json:"data"`
	}
	if err := baoJSON("GET", "/v1/auth/approle/role/sakutei/role-id", nil, &rid); err != nil {
		return err
	}
	var sid struct {
		Data struct {
			SecretID string `json:"secret_id"`
		} `json:"data"`
	}
	if err := baoJSON("POST", "/v1/auth/approle/role/sakutei/secret-id", map[string]any{}, &sid); err != nil {
		return err
	}
	lab.token = filepath.Join(dir, "token")
	lab.roleID = filepath.Join(dir, "role-id")
	lab.secretI = filepath.Join(dir, "secret-id")
	for p, v := range map[string]string{lab.token: tok.Auth.ClientToken, lab.roleID: rid.Data.RoleID, lab.secretI: sid.Data.SecretID} {
		if v == "" {
			return fmt.Errorf("OpenBao returned no credential for %s", p)
		}
		if err := os.WriteFile(p, []byte(v+"\n"), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func teardown() {
	_, _ = docker("rm", "-f", "-v", container)
	if lab.dir != "" {
		_ = os.RemoveAll(lab.dir)
	}
}

// bao sends a root request to OpenBao.
func bao(method, path string, body any) ([]byte, error) {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, lab.addr+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Vault-Token", rootToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, out)
	}
	return out, nil
}

func baoJSON(method, path string, body, out any) error {
	b, err := bao(method, path, body)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// putSecret writes a new KV version of secret/app/<name> and returns it.
func putSecret(t *testing.T, name, value string) int {
	t.Helper()
	var out struct {
		Data struct {
			Version int `json:"version"`
		} `json:"data"`
	}
	if err := baoJSON("POST", "/v1/secret/data/app/"+name, map[string]any{"data": map[string]any{"password": value}}, &out); err != nil {
		t.Fatal(err)
	}
	return out.Data.Version
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// env is one test's intent, records and provider config.
type env struct {
	t       *testing.T
	dir     string
	intent  string
	records string
	pc      string
	outputs []string
}

func newEnv(t *testing.T, secret string, backend map[string]any) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{t: t, dir: dir, intent: filepath.Join(dir, "intent"), records: filepath.Join(dir, "records.json"),
		pc: filepath.Join(dir, "provider-config.json")}
	if err := os.MkdirAll(e.intent, 0o755); err != nil {
		t.Fatal(err)
	}
	e.writeIntent(secret, "one")
	e.writeConfig(backend)
	t.Cleanup(e.checkNoLeak)
	return e
}

func (e *env) writeIntent(secret, plain string) {
	e.t.Helper()
	writeJSON(e.t, filepath.Join(e.intent, "intent.json"), map[string]any{"resources": []map[string]any{
		{"provider": "kvfile", "id": "app", "fields": map[string]string{
			"name": "app", "value": "v", "token": "secretref:vault/app/" + secret + "#password"}},
		{"provider": "kvfile", "id": "plain", "fields": map[string]string{"name": "plain", "value": plain}},
	}})
}

func (e *env) writeConfig(backend map[string]any) {
	e.t.Helper()
	writeJSON(e.t, e.pc, map[string]any{
		"providers":       map[string]any{"kvfile": map[string]any{"path": e.records}},
		"secret_backends": map[string]any{"vault": backend},
	})
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *env) args(verb string, extra ...string) []string {
	return append([]string{verb, "--intent", e.intent, "--state", filepath.Join(e.dir, "state.json"),
		"--provider-config", e.pc, "--cause-state", filepath.Join(e.dir, "cause-state.json")}, extra...)
}

func (e *env) run(verb string, extra ...string) (string, int) {
	e.t.Helper()
	out, err := exec.Command(lab.bin, e.args(verb, extra...)...).CombinedOutput()
	e.outputs = append(e.outputs, string(out))
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		e.t.Fatalf("run %s: %v", verb, err)
	}
	return string(out), 0
}

func (e *env) record(name string) map[string]string {
	e.t.Helper()
	b, err := os.ReadFile(e.records)
	if err != nil {
		e.t.Fatal(err)
	}
	var recs map[string]map[string]string
	if err := json.Unmarshal(b, &recs); err != nil {
		e.t.Fatal(err)
	}
	return recs[name]
}

// checkNoLeak is IT-188-6.
func (e *env) checkNoLeak() {
	for _, out := range e.outputs {
		if strings.Contains(out, secretPfx) {
			e.t.Errorf("[IT-188-6] a secret value reached the output:\n%s", out)
		}
	}
	if b, err := os.ReadFile(e.records); err == nil && bytes.Contains(b, []byte(secretPfx)) {
		e.t.Errorf("[IT-188-6] a secret value reached the records file")
	}
}

// converges checks that the app record holds the ref of version ver and the
// digest of value.
func (e *env) converges(tag string, secret string, ver int, value string) {
	e.t.Helper()
	out, code := e.run("reconcile")
	if code != 0 {
		e.t.Fatalf("[%s] reconcile exit %d:\n%s", tag, code, out)
	}
	rec := e.record("app")
	wantRef := fmt.Sprintf("secretref:vault/app/%s#password#gen%d", secret, ver)
	if rec["token"] != wantRef || rec["token_sha256"] != digest(value) {
		e.t.Fatalf("[%s] record = %v, want %s and the digest of its value", tag, rec, wantRef)
	}
	out, code = e.run("plan")
	if code != 0 || !strings.Contains(out, "Converged (no changes)") {
		e.t.Errorf("[%s] plan after reconcile exit %d, want converged:\n%s", tag, code, out)
	}
}

func tokenBackend() map[string]any {
	return map[string]any{"type": "vault-kv2", "address": lab.addr, "token_file": lab.token, "timeout": "3s"}
}

func TestVault_TokenFile(t *testing.T) {
	v1 := putSecret(t, "tokenfile", secretPfx+"t1")
	e := newEnv(t, "tokenfile", tokenBackend())
	e.converges("IT-188-1", "tokenfile", v1, secretPfx+"t1")

	v2 := putSecret(t, "tokenfile", secretPfx+"t2")
	out, code := e.run("plan")
	if code != 0 || !strings.Contains(out, "UPDATE kvfile//app") || !strings.Contains(out, "cause:  E") {
		t.Fatalf("[IT-188-2] plan exit %d, want an UPDATE of kvfile//app with cause E:\n%s", code, out)
	}
	e.converges("IT-188-2", "tokenfile", v2, secretPfx+"t2")
}

func TestVault_AppRole(t *testing.T) {
	v1 := putSecret(t, "approle", secretPfx+"a1")
	e := newEnv(t, "approle", map[string]any{"type": "vault-kv2", "address": lab.addr,
		"approle_role_id_file": lab.roleID, "approle_secret_id_file": lab.secretI})
	e.converges("IT-188-3", "approle", v1, secretPfx+"a1")
	v2 := putSecret(t, "approle", secretPfx+"a2")
	e.converges("IT-188-3", "approle", v2, secretPfx+"a2")
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestVault_Daemon(t *testing.T) {
	v1 := putSecret(t, "daemon", secretPfx+"d1")
	e := newEnv(t, "daemon", tokenBackend())
	log := &lockedBuffer{}
	// The daemon keeps its cause state in memory (no --cause-state, #165).
	cmd := exec.Command(lab.bin, "--intent", e.intent, "--state", filepath.Join(e.dir, "state.json"),
		"--provider-config", e.pc, "--interval", "300ms")
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		e.outputs = append(e.outputs, log.String())
	}()
	waitRef := func(ver int, value string) {
		t.Helper()
		want := fmt.Sprintf("secretref:vault/app/daemon#password#gen%d", ver)
		end := time.Now().Add(time.Minute)
		for time.Now().Before(end) {
			if b, err := os.ReadFile(e.records); err == nil {
				var recs map[string]map[string]string
				if json.Unmarshal(b, &recs) == nil && recs["app"]["token"] == want && recs["app"]["token_sha256"] == digest(value) {
					return
				}
			}
			time.Sleep(300 * time.Millisecond)
		}
		t.Fatalf("[IT-188-4] the daemon did not Apply %s\nlog:\n%s", want, log.String())
	}
	waitRef(v1, secretPfx+"d1")
	v2 := putSecret(t, "daemon", secretPfx+"d2")
	waitRef(v2, secretPfx+"d2")
}

// unknown checks IT-188-5 against the backend as configured now.
func (e *env) unknown(why, reason string) {
	e.t.Helper()
	before := e.record("app")
	e.writeIntent("unreachable", "changed-"+why)
	out, code := e.run("plan")
	if code != 0 || !strings.Contains(out, "kvfile//app: secret generation of secretref:vault/app/") ||
		!strings.Contains(out, reason) || strings.Contains(out, "UPDATE kvfile//app") {
		e.t.Errorf("[IT-188-5] %s: plan exit %d, want kvfile//app unknown and not planned:\n%s", why, code, out)
	}
	out, code = e.run("reconcile")
	if code != 1 || !strings.Contains(out, "unknown") {
		e.t.Errorf("[IT-188-5] %s: reconcile exit %d, want 1 (unknown):\n%s", why, code, out)
	}
	after := e.record("app")
	if after["token"] != before["token"] || after["token_sha256"] != before["token_sha256"] {
		e.t.Errorf("[IT-188-5] %s: the unknown resource was touched: %v → %v", why, before, after)
	}
	if got := e.record("plain")["value"]; got != "changed-"+why {
		e.t.Errorf("[IT-188-5] %s: the other resource did not converge: value %q", why, got)
	}
	out, _ = e.run("status", "--json")
	if !strings.Contains(out, "kvfile//app") || !strings.Contains(out, "secret generation") {
		e.t.Errorf("[IT-188-5] %s: status does not report kvfile//app as unobservable:\n%s", why, out)
	}
}

func TestVault_Unreachable(t *testing.T) {
	putSecret(t, "unreachable", secretPfx+"u1")
	e := newEnv(t, "unreachable", tokenBackend())
	if out, code := e.run("reconcile"); code != 0 {
		t.Fatalf("reconcile exit %d:\n%s", code, out)
	}

	// A closed port.
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	closed := tokenBackend()
	closed["address"] = "http://127.0.0.1:" + port
	e.writeConfig(closed)
	e.unknown("closed-port", "unreachable")

	// Sealed (last: the dev server is not unsealed again).
	e.writeConfig(tokenBackend())
	if _, err := bao("PUT", "/v1/sys/seal", nil); err != nil {
		t.Fatal(err)
	}
	e.unknown("sealed", "HTTP 503 (sealed or in standby)")
}
