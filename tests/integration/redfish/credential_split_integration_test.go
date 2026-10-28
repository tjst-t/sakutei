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

//go:build integration_redfish

// credential_split_integration_test.go — #186: 観測用と書き込み用の BMC の資格
// （§20.7）を qemu-bmc 0.11.0 に向ける（14 段目。[AC-186-14] は
// redfish_integration_test.go の冒頭）。
//
// qemu-bmc 0.11.0 は AccountService を持たず（/redfish/v1/AccountService は
// 404）、Redfish の役割（ReadOnly など）も持たない。2 人目の利用者は IPMI の
// Set User Name・Set User Password（ipmitool user set）で作れて、Redfish の
// Basic 認証はそれを受け付けるが、IPMI の権限（USER）は Redfish では効かず、
// その利用者も PATCH・POST を通す。そこで、ReadOnly の役割の BMC の振る舞い
// （GET は通し、ほかは 403 InsufficientPrivilege）はテストが BMC の前に立てる
// 中継（HTTPS）で真似、利用者ごとの GET と書き込みの回数も中継で数える。BMC に
// 届く要求は本物の qemu-bmc が受ける。
package redfish

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 2 人目の利用者（IPMI の user 3 に作る。観測用）。
const (
	roUser     = "sakutei-ro"
	roPassword = "ro-secret-186"
	roUserID   = "3"
)

// roleProxy は BMC の前に立つ中継: roUser の GET 以外を 403 で断り（ReadOnly の
// 役割の真似）、利用者ごとに GET と書き込みを数える。
type roleProxy struct {
	*httptest.Server
	mu      sync.Mutex
	calls   map[string]map[string]int // 利用者 → "GET"・"WRITE"・"REFUSED" → 回数
	writers []string                  // 書き込みを通した要求（"<利用者> <method> <path>"）
}

func newRoleProxy(t *testing.T, n node) *roleProxy {
	t.Helper()
	target, err := url.Parse("https://" + n.addr)
	if err != nil {
		t.Fatal(err)
	}
	p := &roleProxy{calls: map[string]map[string]int{}}
	rp := &httputil.ReverseProxy{
		Rewrite:   func(r *httputil.ProxyRequest) { r.SetURL(target) },
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // qemu-bmc の自己署名
	}
	p.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _, _ := r.BasicAuth()
		kind := "WRITE"
		if r.Method == http.MethodGet {
			kind = "GET"
		}
		p.mu.Lock()
		if p.calls[user] == nil {
			p.calls[user] = map[string]int{}
		}
		p.calls[user][kind]++
		refuse := user == roUser && kind == "WRITE"
		if refuse {
			p.calls[user]["REFUSED"]++
		} else if kind == "WRITE" {
			p.writers = append(p.writers, user+" "+r.Method+" "+r.URL.Path)
		}
		p.mu.Unlock()
		if refuse {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"code":    "Base.1.0.InsufficientPrivilege",
				"message": "There are insufficient privileges for the account or credentials associated with the current session to perform the requested operation.",
			}})
			return
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *roleProxy) userCalls(user string) map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]int{}
	for k, v := range p.calls[user] {
		out[k] = v
	}
	return out
}

func (p *roleProxy) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls, p.writers = map[string]map[string]int{}, nil
}

// splitConfig は node1 だけの BMC の一覧（address は中継）を書く。observe・write
// はそれぞれ「利用者 secret の path」。
func splitConfig(t *testing.T, name, store string, proxy *roleProxy, observe, write [2]string) string {
	t.Helper()
	cfg := "# BMC の一覧（#186 の統合テスト、観測用と書き込み用の資格）\nsecret_store: " + store + "\nbmcs:\n  node1:\n" +
		"    address: " + proxy.URL + "\n    system: /redfish/v1/Systems/1\n    manager: /redfish/v1/Managers/1\n" +
		"    insecure_skip_verify: true\n    timeout: 3s\n" +
		"    observe:\n      username: " + observe[0] + "\n      password: secretref:file/" + observe[1] + "\n" +
		"    write:\n      username: " + write[0] + "\n      password: secretref:file/" + write[1] + "\n"
	p := filepath.Join(lab.dir, name)
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// sakuteiWith は sakutei を BMC の一覧 cfg で動かす。
func sakuteiWith(t *testing.T, cfg, verb string, extra ...string) (string, int) {
	t.Helper()
	args := []string{verb, "--intent", filepath.Join(lab.dir, "intent"), "--state", filepath.Join(lab.dir, "fake-state.json"),
		"--redfish-config", cfg}
	return runSakutei(t, append(args, extra...))
}

// ipmiUser は node の BMC に IPMI の利用者を作る（コンテナの中の ipmitool で、
// 自分自身の IPMI over LAN に）。
func ipmiUser(t *testing.T, n node, args ...string) {
	t.Helper()
	full := append([]string{"exec", containerName(n), "ipmitool", "-I", "lanplus", "-H", "127.0.0.1",
		"-U", bmcUser, "-P", bmcPassword}, args...)
	if out, err := sudoCmd("docker", full...).CombinedOutput(); err != nil {
		t.Fatalf("ipmitool %v: %v\n%s", args, err, out)
	}
}

// [AC-186-14]
func stepCredentialSplit(t *testing.T) {
	// (a) 2 人目の利用者を作る。qemu-bmc の Redfish はそれを Basic 認証で受け付ける。
	ipmiUser(t, node1, "user", "set", "name", roUserID, roUser)
	ipmiUser(t, node1, "user", "set", "password", roUserID, roPassword)
	ipmiUser(t, node1, "channel", "setaccess", "1", roUserID, "privilege=2") // USER
	ipmiUser(t, node1, "user", "enable", roUserID)
	asRO := func(method, path string, body any) int {
		t.Helper()
		var rd *strings.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = strings.NewReader(string(b))
		} else {
			rd = strings.NewReader("")
		}
		req, err := http.NewRequest(method, "https://"+node1.addr+path, rd)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.SetBasicAuth(roUser, roPassword)
		resp, err := httpc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := asRO("GET", "/redfish/v1/Systems/1", nil); code != http.StatusOK {
		t.Fatalf("[AC-186-14] GET Systems/1 as %s: %d, want 200 (the IPMI user is a Redfish user)", roUser, code)
	}
	// qemu-bmc の限界を記録する（役割を持たない）。
	s := mustSystem(t, node1)
	t.Logf("qemu-bmc %s: AccountService %d; PATCH of the same Boot by the USER-privilege account: %d (no Redfish roles: a ReadOnly account cannot be made on the BMC itself, the proxy plays it)",
		qemuBMCVer, asRO("GET", "/redfish/v1/AccountService", nil),
		asRO("PATCH", "/redfish/v1/Systems/1", map[string]any{"Boot": map[string]string{
			"BootSourceOverrideEnabled": s.Boot.BootSourceOverrideEnabled, "BootSourceOverrideTarget": s.Boot.BootSourceOverrideTarget}}))

	// (b) 中継と BMC の一覧・secret store。
	proxy := newRoleProxy(t, node1)
	both := `{"secrets": {"bmc/password": {"gen": 1, "value": "` + bmcPassword + `"}, "bmc/ro": {"gen": 1, "value": "` + roPassword + `"}}}`
	roOnly := `{"secrets": {"bmc/ro": {"gen": 1, "value": "` + roPassword + `"}}}`
	for name, body := range map[string]string{"secrets-split.json": both, "secrets-ro.json": roOnly} {
		if err := os.WriteFile(filepath.Join(lab.dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ro, admin := [2]string{roUser, "bmc/ro"}, [2]string{bmcUser, "bmc/password"}
	split := splitConfig(t, "bmcs-split.yaml", "secrets-split.json", proxy, ro, admin)
	noWriteSecret := splitConfig(t, "bmcs-split-ro.yaml", "secrets-ro.json", proxy, ro, admin)
	wrongWrite := splitConfig(t, "bmcs-split-wrong.yaml", "secrets-split.json", proxy, ro, ro)

	// 宣言: 電源は今のまま、起動先を Continuous の Hdd に。手で Disabled にしておく。
	waitFor(t, time.Minute, "node1 On or Off", func() (bool, string) {
		s, err := getSystem(node1)
		if err != nil {
			return false, err.Error()
		}
		return s.PowerState == "On" || s.PowerState == "Off", s.PowerState
	})
	power := mustSystem(t, node1).PowerState
	handWrite(t, node1, "PATCH", "/redfish/v1/Systems/1", map[string]any{"Boot": map[string]string{"BootSourceOverrideEnabled": "Disabled"}})
	writeIntent(t, systemRes(node1, map[string]string{"power": power, "boot_enabled": "Continuous", "boot_target": "Hdd"}))
	bootIs := func(enabled, target string) bool {
		b := mustSystem(t, node1).Boot
		return b.BootSourceOverrideEnabled == enabled && (target == "" || b.BootSourceOverrideTarget == target)
	}

	// (c) 書かない回（plan、reconcile --mode alert-only）は、書き込み用の password が
	// store に無くても起動して、観測用の利用者の GET だけで差分を出す。
	proxy.reset()
	out, code := sakuteiWith(t, noWriteSecret, "plan")
	if code != 0 || !strings.Contains(out, `"Hdd"`) {
		t.Fatalf("[AC-186-14] plan without the write password (exit %d) does not show the boot drift:\n%s", code, out)
	}
	out, code = sakuteiWith(t, noWriteSecret, "reconcile", "--mode", "alert-only")
	if code != 0 {
		t.Fatalf("[AC-186-14] reconcile --mode alert-only without the write password: exit %d:\n%s", code, out)
	}
	if c := proxy.userCalls(roUser); c["GET"] == 0 || c["WRITE"] != 0 {
		t.Errorf("[AC-186-14] the observe-only runs as %s: %v, want reads only", roUser, c)
	}
	if c := proxy.userCalls(bmcUser); len(c) != 0 {
		t.Errorf("[AC-186-14] the observe-only runs used the write user: %v", c)
	}
	if !bootIs("Disabled", "") {
		t.Fatalf("[AC-186-14] an observe-only run changed the boot override: %+v", mustSystem(t, node1).Boot)
	}

	// (d) 書く回は、書き込み用の password が無ければ起動しない。
	out, code = sakuteiWith(t, noWriteSecret, "reconcile")
	if code == 0 || !strings.Contains(out, "bmcs.node1.write.password") {
		t.Fatalf("[AC-186-14] reconcile without the write password: exit %d, want a load error naming bmcs.node1.write.password:\n%s", code, out)
	}

	// (e) 両方あれば、観測は観測用、書き込みは書き込み用で収束する。
	proxy.reset()
	out, code = sakuteiWith(t, split, "reconcile")
	if code != 0 {
		t.Fatalf("[AC-186-14] reconcile with the split credentials: exit %d:\n%s", code, out)
	}
	if !bootIs("Continuous", "Hdd") {
		t.Fatalf("[AC-186-14] node1 Boot = %+v, want Continuous/Hdd", mustSystem(t, node1).Boot)
	}
	roCalls, adminCalls := proxy.userCalls(roUser), proxy.userCalls(bmcUser)
	if roCalls["GET"] == 0 || roCalls["WRITE"] != 0 {
		t.Errorf("[AC-186-14] %s during reconcile: %v, want reads only", roUser, roCalls)
	}
	if adminCalls["WRITE"] == 0 {
		t.Errorf("[AC-186-14] %s during reconcile: %v, want the writes", bmcUser, adminCalls)
	}
	t.Logf("reconcile: %s %v, %s %v, writes %v", roUser, roCalls, bmcUser, adminCalls, proxy.writers)
	if out, code := sakuteiWith(t, split, "plan"); code != 0 || !strings.Contains(out, "Converged (no changes)") {
		t.Fatalf("[AC-186-14] plan after reconcile (exit %d) is not converged:\n%s", code, out)
	}

	// (f) 書き込み用に ReadOnly の利用者を書き間違えれば、403 で断られ（設定を
	// 直すまで治らない permanent）、BMC は変わらない。reconcile の終了コードは
	// Apply の失敗では変わらない（「not converged」と出す）。
	handWrite(t, node1, "PATCH", "/redfish/v1/Systems/1", map[string]any{"Boot": map[string]string{"BootSourceOverrideEnabled": "Disabled"}})
	proxy.reset()
	out, _ = sakuteiWith(t, wrongWrite, "reconcile")
	if !strings.Contains(out, "permanent") || !strings.Contains(out, "HTTP 403") || !strings.Contains(out, "not converged") {
		t.Fatalf("[AC-186-14] reconcile with the ReadOnly user as the write user does not report the 403:\n%s", out)
	}
	if c := proxy.userCalls(roUser); c["REFUSED"] == 0 {
		t.Errorf("[AC-186-14] the proxy refused nothing: %v", c)
	}
	if !bootIs("Disabled", "") {
		t.Fatalf("[AC-186-14] a refused write changed the boot override: %+v", mustSystem(t, node1).Boot)
	}
	// 片づけ: 正しい資格で戻す。
	if out, code := sakuteiWith(t, split, "reconcile"); code != 0 || !bootIs("Continuous", "Hdd") {
		t.Fatalf("[AC-186-14] reconcile after the refusal: exit %d:\n%s", code, out)
	}
}
