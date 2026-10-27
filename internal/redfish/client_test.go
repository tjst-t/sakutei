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

package redfish_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/redfish"
	"github.com/tjst-t/sakutei/v2/internal/redfish/redfishtest"
)

// clock は偽 BMC に渡す手で進める時計。
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

// newPair は偽 BMC と、その証明書を CA ファイルで信頼するクライアントを作る。
func newPair(t *testing.T, opts redfishtest.Options) (*redfishtest.Server, *redfish.HTTPClient) {
	t.Helper()
	srv := redfishtest.New(opts)
	t.Cleanup(srv.Close)
	ca, err := srv.WriteCAFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := redfish.New(redfish.Config{
		Endpoint: srv.URL, Username: "admin", Password: "password", CAFile: ca, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, c
}

func TestGetSystem(t *testing.T) {
	_, c := newPair(t, redfishtest.Options{
		UUID: "11111111-2222-3333-4444-555555555555", MACs: []string{"52:54:00:00:00:01", "52:54:00:00:00:02"},
		MemoryGiB: 4, CPUs: 3,
	})
	s, err := c.GetSystem(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := redfish.System{
		Path:           redfishtest.SystemPath,
		PowerState:     redfish.PowerOn,
		UUID:           "11111111-2222-3333-4444-555555555555",
		TotalMemoryGiB: 4,
		ProcessorCount: 3,
		Boot: redfish.Boot{
			Enabled: redfish.BootDisabled, Target: "None", Mode: "UEFI",
			AllowableTargets: []string{"None", "Pxe", "Hdd", "Cd", "BiosSetup"},
		},
		ResetTypes:   []string{"On", "ForceOff", "GracefulShutdown", "ForceRestart", "GracefulRestart"},
		ResetTarget:  redfishtest.ResetPath,
		MACAddresses: []string{"52:54:00:00:00:01", "52:54:00:00:00:02"},
	}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("GetSystem:\n got %+v\nwant %+v", s, want)
	}
}

func TestExplicitPaths(t *testing.T) {
	srv, _ := newPair(t, redfishtest.Options{})
	c, err := redfish.New(redfish.Config{
		Endpoint: strings.TrimPrefix(srv.URL, "https://"), Username: "admin", Password: "password",
		InsecureSkipVerify: true, SystemPath: redfishtest.SystemPath, ManagerPath: redfishtest.ManagerPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.ClearRequests()
	if _, err := c.GetSystem(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetVirtualMedia(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, r := range srv.Requests() {
		if r.Path == "/redfish/v1/Systems" || r.Path == "/redfish/v1/Managers" {
			t.Errorf("collection %s was read although the path was configured", r.Path)
		}
	}
	// 無いパスを指定すれば BMC の 404 を内容つきで返す。
	c2, _ := redfish.New(redfish.Config{
		Endpoint: srv.URL, Username: "admin", Password: "password", InsecureSkipVerify: true,
		SystemPath: "/redfish/v1/Systems/9",
	})
	_, err = c2.GetSystem(context.Background())
	if !redfish.IsRequest(err) || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "Systems/9") {
		t.Fatalf("missing system: got %v", err)
	}
}

func TestResetGracefulShutdown(t *testing.T) {
	clk := newClock()
	srv, c := newPair(t, redfishtest.Options{Now: clk.Now, GracefulDelay: 10 * time.Second})
	ctx := context.Background()
	srv.ClearRequests()
	if err := c.Reset(ctx, redfish.ResetGracefulShutdown); err != nil {
		t.Fatal(err)
	}
	reqs := srv.Requests()
	last := reqs[len(reqs)-1]
	if last.Method != http.MethodPost || last.Path != redfishtest.ResetPath || last.Body != `{"ResetType":"GracefulShutdown"}` {
		t.Fatalf("reset request: %+v", last)
	}
	if s, _ := c.GetSystem(ctx); s.PowerState != redfish.PowerPoweringOff {
		t.Fatalf("after GracefulShutdown: %s, want PoweringOff", s.PowerState)
	}
	clk.Advance(10 * time.Second)
	if s, _ := c.GetSystem(ctx); s.PowerState != redfish.PowerOff {
		t.Fatalf("after graceful delay: %s, want Off", s.PowerState)
	}
	if err := c.Reset(ctx, redfish.ResetOn); err != nil {
		t.Fatal(err)
	}
	if s, _ := c.GetSystem(ctx); s.PowerState != redfish.PowerOn {
		t.Fatalf("after On: %s, want On", s.PowerState)
	}
}

func TestResetGracefulIgnored(t *testing.T) {
	clk := newClock()
	srv, c := newPair(t, redfishtest.Options{Now: clk.Now, GracefulTimeout: 120 * time.Second})
	srv.SetIgnoreGraceful(true)
	ctx := context.Background()
	if err := c.Reset(ctx, redfish.ResetGracefulShutdown); err != nil {
		t.Fatal(err)
	}
	clk.Advance(119 * time.Second)
	if s, _ := c.GetSystem(ctx); s.PowerState != redfish.PowerPoweringOff {
		t.Fatalf("before GRACEFUL_TIMEOUT: %s, want PoweringOff", s.PowerState)
	}
	clk.Advance(time.Second)
	if s, _ := c.GetSystem(ctx); s.PowerState != redfish.PowerOn {
		t.Fatalf("after GRACEFUL_TIMEOUT: %s, want On", s.PowerState)
	}

	// 途中で ForceOff が来たら打ち切ってそれに従う。
	if err := c.Reset(ctx, redfish.ResetGracefulShutdown); err != nil {
		t.Fatal(err)
	}
	if err := c.Reset(ctx, redfish.ResetForceOff); err != nil {
		t.Fatal(err)
	}
	clk.Advance(200 * time.Second)
	if s, _ := c.GetSystem(ctx); s.PowerState != redfish.PowerOff {
		t.Fatalf("ForceOff during PoweringOff: %s, want Off", s.PowerState)
	}
}

func TestResetNotAllowed(t *testing.T) {
	_, c := newPair(t, redfishtest.Options{ResetTypes: []string{"On", "ForceOff"}})
	err := c.Reset(context.Background(), redfish.ResetGracefulRestart)
	var re *redfish.Error
	if !errors.As(err, &re) || re.Kind != redfish.KindRequest || re.StatusCode != http.StatusBadRequest ||
		!strings.Contains(re.Message, "GracefulRestart") {
		t.Fatalf("got %v, want 400 request error naming the reset type", err)
	}
}

func TestPatchBoot(t *testing.T) {
	srv, c := newPair(t, redfishtest.Options{})
	ctx := context.Background()

	// Continuous は On・ForceRestart のあとも残る。
	if err := c.PatchBoot(ctx, redfish.BootContinuous, "Pxe"); err != nil {
		t.Fatal(err)
	}
	if err := c.Reset(ctx, redfish.ResetForceRestart); err != nil {
		t.Fatal(err)
	}
	s, _ := c.GetSystem(ctx)
	if s.Boot.Enabled != redfish.BootContinuous || s.Boot.Target != "Pxe" {
		t.Fatalf("Continuous after restart: %+v", s.Boot)
	}

	// Disabled でも Target は設定した値のまま返る。
	if err := c.PatchBoot(ctx, redfish.BootDisabled, ""); err != nil {
		t.Fatal(err)
	}
	s, _ = c.GetSystem(ctx)
	if s.Boot.Enabled != redfish.BootDisabled || s.Boot.Target != "Pxe" {
		t.Fatalf("Disabled keeps Target: %+v", s.Boot)
	}

	// Once は 1 回の起動で Disabled・None に戻る。
	if err := c.PatchBoot(ctx, redfish.BootOnce, "Cd"); err != nil {
		t.Fatal(err)
	}
	if err := c.Reset(ctx, redfish.ResetForceOff); err != nil {
		t.Fatal(err)
	}
	if s, _ = c.GetSystem(ctx); s.Boot.Enabled != redfish.BootOnce {
		t.Fatalf("Once survives power off: %+v", s.Boot)
	}
	boots := srv.Boots()
	if err := c.Reset(ctx, redfish.ResetOn); err != nil {
		t.Fatal(err)
	}
	s, _ = c.GetSystem(ctx)
	if s.Boot.Enabled != redfish.BootDisabled || s.Boot.Target != "None" || srv.Boots() != boots+1 {
		t.Fatalf("Once after boot: %+v (boots %d→%d)", s.Boot, boots, srv.Boots())
	}

	// AllowableValues にない Target は 400 を内容つきで返す。
	err := c.PatchBoot(ctx, redfish.BootContinuous, "Floppy")
	if !redfish.IsRequest(err) || !strings.Contains(err.Error(), "Floppy") {
		t.Fatalf("bad target: got %v", err)
	}
}

func TestVirtualMedia(t *testing.T) {
	srv, c := newPair(t, redfishtest.Options{})
	ctx := context.Background()
	const img = "http://192.0.2.10/images/install.iso"

	vm, err := c.GetVirtualMedia(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if vm.Path != redfishtest.VirtualMediaPath || vm.Inserted || vm.Image != "" ||
		vm.InsertTarget != redfishtest.InsertMediaPath || vm.EjectTarget != redfishtest.EjectMediaPath {
		t.Fatalf("initial media: %+v", vm)
	}
	if err := c.InsertMedia(ctx, img); err != nil {
		t.Fatal(err)
	}
	// 電源オフ中も残る。
	if err := c.Reset(ctx, redfish.ResetForceOff); err != nil {
		t.Fatal(err)
	}
	if vm, _ = c.GetVirtualMedia(ctx); !vm.Inserted || vm.Image != img {
		t.Fatalf("after insert: %+v", vm)
	}
	// 入っているところへの Insert は BMC が拒む（Eject してから入れる）。
	if err := c.InsertMedia(ctx, "/images/other.iso"); !redfish.IsRequest(err) {
		t.Fatalf("insert over inserted: got %v", err)
	}
	if err := c.EjectMedia(ctx); err != nil {
		t.Fatal(err)
	}
	if vm, _ = c.GetVirtualMedia(ctx); vm.Inserted || vm.Image != "" {
		t.Fatalf("after eject: %+v", vm)
	}

	// 取得に失敗すると Inserted は false に戻る。
	srv.SetMediaFetchFails(true)
	if err := c.InsertMedia(ctx, img); err != nil {
		t.Fatal(err)
	}
	if vm, _ = c.GetVirtualMedia(ctx); vm.Inserted {
		t.Fatalf("after failed fetch: %+v, want Inserted false", vm)
	}
}

func TestSelfSignedRejectedUnlessExplicit(t *testing.T) {
	srv := redfishtest.New(redfishtest.Options{})
	defer srv.Close()
	ctx := context.Background()

	strict, err := redfish.New(redfish.Config{Endpoint: srv.URL, Username: "admin", Password: "password"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = strict.GetSystem(ctx)
	if !redfish.IsCertificate(err) || !redfish.IsPermanent(err) || redfish.IsUnreachable(err) {
		t.Fatalf("self-signed without opt-in: got %v, want certificate error", err)
	}
	if n := len(srv.Requests()); n != 0 {
		t.Fatalf("BMC received %d requests over an unverified connection", n)
	}

	insecure, err := redfish.New(redfish.Config{Endpoint: srv.URL, Username: "admin", Password: "password", InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := insecure.GetSystem(ctx); err != nil {
		t.Fatalf("insecure_skip_verify: %v", err)
	}

	// 別の CA ではやはり拒む。
	ca := writeOtherCA(t)
	wrongCA, err := redfish.New(redfish.Config{Endpoint: srv.URL, Username: "admin", Password: "password", CAFile: ca})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrongCA.GetSystem(ctx); !redfish.IsCertificate(err) {
		t.Fatalf("wrong CA: got %v, want certificate error", err)
	}
}

func TestAuthFailure(t *testing.T) {
	srv := redfishtest.New(redfishtest.Options{})
	defer srv.Close()
	c, err := redfish.New(redfish.Config{Endpoint: srv.URL, Username: "admin", Password: "wrong", InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.GetSystem(context.Background())
	var re *redfish.Error
	if !errors.As(err, &re) || re.Kind != redfish.KindAuth || re.StatusCode != http.StatusUnauthorized ||
		!redfish.IsPermanent(err) || redfish.IsUnreachable(err) {
		t.Fatalf("got %v, want 401 auth error", err)
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Fatalf("error leaks the password: %v", err)
	}
	if err := c.Reset(context.Background(), redfish.ResetOn); !redfish.IsAuth(err) {
		t.Fatalf("reset: got %v, want auth error", err)
	}
}

func TestUnreachable(t *testing.T) {
	ctx := context.Background()

	t.Run("5xx", func(t *testing.T) {
		srv, c := newPair(t, redfishtest.Options{})
		srv.SetFailStatus(http.StatusServiceUnavailable)
		_, err := c.GetSystem(ctx)
		var re *redfish.Error
		if !errors.As(err, &re) || re.Kind != redfish.KindUnreachable || re.StatusCode != 503 || redfish.IsPermanent(err) {
			t.Fatalf("got %v, want unreachable 503", err)
		}
		srv.SetFailStatus(0)
		if _, err := c.GetSystem(ctx); err != nil {
			t.Fatalf("after recovery: %v", err)
		}
	})

	t.Run("connection closed", func(t *testing.T) {
		srv, c := newPair(t, redfishtest.Options{})
		srv.SetUnreachable(true)
		if _, err := c.GetSystem(ctx); !redfish.IsUnreachable(err) {
			t.Fatalf("got %v, want unreachable", err)
		}
		if err := c.Reset(ctx, redfish.ResetForceOff); !redfish.IsUnreachable(err) {
			t.Fatalf("reset: got %v, want unreachable", err)
		}
		srv.SetUnreachable(false)
		if s, err := c.GetSystem(ctx); err != nil || s.PowerState != redfish.PowerOn {
			t.Fatalf("after recovery: %+v %v (reset must not have reached the BMC)", s, err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		srv := redfishtest.New(redfishtest.Options{})
		defer srv.Close()
		srv.SetDelay(2 * time.Second)
		c, err := redfish.New(redfish.Config{Endpoint: srv.URL, Username: "admin", Password: "password",
			InsecureSkipVerify: true, Timeout: 100 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		_, err = c.GetSystem(ctx)
		if !redfish.IsUnreachable(err) {
			t.Fatalf("got %v, want unreachable", err)
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("timeout took %v", d)
		}
	})

	t.Run("server stopped", func(t *testing.T) {
		srv, c := newPair(t, redfishtest.Options{})
		srv.Close()
		if _, err := c.GetSystem(ctx); !redfish.IsUnreachable(err) {
			t.Fatalf("got %v, want unreachable", err)
		}
	})
}

func TestNewRejectsHTTP(t *testing.T) {
	for _, ep := range []string{"", "http://192.0.2.1", "https://"} {
		if _, err := redfish.New(redfish.Config{Endpoint: ep}); err == nil {
			t.Errorf("New(%q) accepted", ep)
		}
	}
	if _, err := redfish.New(redfish.Config{Endpoint: "192.0.2.1", CAFile: "/nonexistent/ca.pem"}); err == nil {
		t.Error("missing CA file accepted")
	}
}

// writeOtherCA は偽 BMC とは別の自己署名 CA を PEM で書き、そのパスを返す。
func writeOtherCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "other CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "other-ca.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPoweringOnAndInterruptByOn(t *testing.T) {
	clk := newClock()
	srv, c := newPair(t, redfishtest.Options{Now: clk.Now, PowerState: "Off", PowerOnDelay: 5 * time.Second,
		GracefulDelay: 30 * time.Second})
	ctx := context.Background()
	if err := c.Reset(ctx, redfish.ResetOn); err != nil {
		t.Fatal(err)
	}
	if s, _ := c.GetSystem(ctx); s.PowerState != redfish.PowerPoweringOn {
		t.Fatalf("after On: %s, want PoweringOn", s.PowerState)
	}
	clk.Advance(5 * time.Second)
	if s, _ := c.GetSystem(ctx); s.PowerState != redfish.PowerOn {
		t.Fatalf("after power-on delay: %s, want On", s.PowerState)
	}
	// PoweringOff の途中の On は正常停止を打ち切り、On のまま（起動し直さない）。
	boots := srv.Boots()
	if err := c.Reset(ctx, redfish.ResetGracefulShutdown); err != nil {
		t.Fatal(err)
	}
	if err := c.Reset(ctx, redfish.ResetOn); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Minute)
	if s, _ := c.GetSystem(ctx); s.PowerState != redfish.PowerOn || srv.Boots() != boots {
		t.Fatalf("On during PoweringOff: %s (boots %d→%d)", s.PowerState, boots, srv.Boots())
	}
}

func TestTaskResponseIsNotSuccess(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"Members":[{"@odata.id":"/redfish/v1/Systems/1"}]}`))
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ts.Close()
	c, err := redfish.New(redfish.Config{Endpoint: ts.URL, InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	err = c.PatchBoot(context.Background(), redfish.BootContinuous, "Pxe")
	if !redfish.IsUnreachable(err) || !strings.Contains(err.Error(), "202") {
		t.Fatalf("got %v, want unreachable for 202", err)
	}
}

func TestBios(t *testing.T) {
	srv, c := newPair(t, redfishtest.Options{})
	ctx := context.Background()
	b, err := c.GetBios(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if b.Path != redfishtest.BiosPath || b.SettingsPath != redfishtest.BiosSettingsPath || b.AttributeRegistry != redfishtest.BiosRegistryID ||
		b.Attributes["ProcVirtualization"] != "Enabled" || b.Attributes["BootRetryCount"] != float64(3) || b.Attributes["SriovGlobalEnable"] != false ||
		len(b.Pending) != 0 || b.ResetPending {
		t.Fatalf("GetBios = %+v", b)
	}
	if err := c.PatchBiosSettings(ctx, map[string]any{"ProcVirtualization": "Disabled"}); err != nil {
		t.Fatal(err)
	}
	if b, err = c.GetBios(ctx); err != nil || b.Pending["ProcVirtualization"] != "Disabled" || b.Attributes["ProcVirtualization"] != "Enabled" {
		t.Fatalf("GetBios after PATCH = %+v, %v", b, err)
	}
	err = c.PatchBiosSettings(ctx, map[string]any{"BootMode": "Bios"})
	if !redfish.IsRequest(err) || !strings.Contains(err.Error(), "read only") {
		t.Fatalf("PATCH of a read-only attribute = %v, want a request error with the BMC's message", err)
	}
	srv.Restart()
	if b, err = c.GetBios(ctx); err != nil || len(b.Pending) != 0 || b.Attributes["ProcVirtualization"] != "Disabled" {
		t.Fatalf("GetBios after the reboot = %+v, %v", b, err)
	}

	reg, err := c.GetAttributeRegistry(ctx, redfishtest.BiosRegistryID)
	if err != nil || reg.ID != redfishtest.BiosRegistryID || len(reg.Attributes) != len(redfishtest.DefaultBiosAttributes) {
		t.Fatalf("GetAttributeRegistry = %+v, %v", reg, err)
	}
	for _, a := range reg.Attributes {
		if a.AttributeName == "PowerProfile" && !reflect.DeepEqual(a.Values, []string{"MaxPerformance", "Balanced", "PowerSaving"}) {
			t.Errorf("PowerProfile values = %v", a.Values)
		}
		if a.AttributeName == "BootRetryCount" && (a.UpperBound == nil || *a.UpperBound != 10 || a.Type != "Integer") {
			t.Errorf("BootRetryCount = %+v", a)
		}
	}
	if _, err := c.GetAttributeRegistry(ctx, "OtherRegistry.1.0.0"); !redfish.IsRequest(err) {
		t.Fatalf("GetAttributeRegistry of an unknown id = %v, want a request error", err)
	}
}

func TestBiosMissing(t *testing.T) {
	_, c := newPair(t, redfishtest.Options{NoBios: true})
	_, err := c.GetBios(context.Background())
	if !redfish.IsNoBios(err) || !redfish.IsRequest(err) {
		t.Fatalf("GetBios without Bios = %v, want ErrNoBios", err)
	}
	if !errors.Is(err, redfish.ErrNoBios) {
		t.Fatalf("errors.Is(%v, ErrNoBios) = false", err)
	}
}

// Settings を持たない BMC（@Redfish.Settings が無く /Settings は 404）では
// 保留中は空。登録簿は Id で引ける。Bios が 404 なら ErrNoBios。
func TestBiosVendorLayout(t *testing.T) {
	var biosGone atomic.Bool
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redfish/v1/Systems/S":
			w.Write([]byte(`{"Bios":{"@odata.id":"/redfish/v1/Systems/S/Bios"}}`))
		case "/redfish/v1/Systems/S/Bios":
			if biosGone.Load() {
				http.NotFound(w, r)
				return
			}
			w.Write([]byte(`{"AttributeRegistry":"BiosAttributeRegistry.v1_0_3","Attributes":{"LogicalProc":"Enabled"}}`))
		case "/redfish/v1/Registries":
			w.Write([]byte(`{"Members":[{"@odata.id":"/redfish/v1/Registries/Base"},{"@odata.id":"/redfish/v1/Registries/BiosAttributeRegistry.v1_0_3"}]}`))
		case "/redfish/v1/Registries/Base":
			w.Write([]byte(`{"Id":"Base.1.8","Registry":"Base.1.8","Location":[{"Language":"en","Uri":"/registries/base.json"}]}`))
		case "/redfish/v1/Registries/BiosAttributeRegistry.v1_0_3":
			w.Write([]byte(`{"Id":"BiosAttributeRegistry.v1_0_3","Location":[{"Language":"ja","Uri":"/registries/ja.json"},{"Language":"en","Uri":"/registries/bios.json"}]}`))
		case "/registries/bios.json":
			w.Write([]byte(`{"Id":"BiosAttributeRegistry.v1_0_3","RegistryEntries":{"Attributes":[{"AttributeName":"LogicalProc","Type":"Enumeration","Value":[{"ValueName":"Enabled"},{"ValueName":"Disabled"}]}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	c, err := redfish.New(redfish.Config{Endpoint: ts.URL, InsecureSkipVerify: true, SystemPath: "/redfish/v1/Systems/S"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	b, err := c.GetBios(ctx)
	if err != nil || b.SettingsPath != "/redfish/v1/Systems/S/Bios/Settings" || len(b.Pending) != 0 || b.Attributes["LogicalProc"] != "Enabled" {
		t.Fatalf("GetBios = %+v, %v", b, err)
	}
	reg, err := c.GetAttributeRegistry(ctx, b.AttributeRegistry)
	if err != nil || len(reg.Attributes) != 1 || !reflect.DeepEqual(reg.Attributes[0].Values, []string{"Enabled", "Disabled"}) {
		t.Fatalf("GetAttributeRegistry = %+v, %v", reg, err)
	}
	biosGone.Store(true)
	if _, err := c.GetBios(ctx); !redfish.IsNoBios(err) {
		t.Fatalf("GetBios with Bios 404 = %v, want ErrNoBios", err)
	}
}
