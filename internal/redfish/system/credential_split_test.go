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

package system_test

// credential_split_test.go — 観測用と書き込み用の BMC の資格（§20.7、#186）。
// 観測用は Redfish の ReadOnly の役割の利用者、書き込み用は管理者。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/cause"
	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/redfish/redfishtest"
	"github.com/tjst-t/sakutei/v2/internal/redfish/system"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
	"github.com/tjst-t/sakutei/v2/pkg/registry"
)

const (
	roUser     = "sakutei-ro"
	roPassword = "ro-secret"
)

// splitRig は ReadOnly の利用者と管理者を受ける偽 BMC と、その 2 本を observe:・
// write: に書いた BMC の一覧（secret store は store）。
func splitRig(t *testing.T, store string) (*redfishtest.Server, string) {
	t.Helper()
	srv := redfishtest.New(redfishtest.Options{PowerState: "Off", ReadOnlyUsername: roUser, ReadOnlyPassword: roPassword})
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	ca, err := srv.WriteCAFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secrets.json"), []byte(store), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := "secret_store: secrets.json\nbmcs:\n  node1:\n    address: " + srv.URL + "\n    ca_file: " + ca + "\n" +
		"    observe:\n      username: " + roUser + "\n      password: secretref:file/bmc/ro\n" +
		"    write:\n      username: admin\n      password: secretref:file/bmc/admin\n"
	path := filepath.Join(dir, "bmcs.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return srv, path
}

const bothSecrets = `{"secrets": {"bmc/ro": {"value": "` + roPassword + `"}, "bmc/admin": {"value": "password"}}}`

func splitWant() map[string]string {
	return map[string]string{system.FieldName: "node1", system.FieldPower: "On", system.FieldBootEnabled: "Continuous", system.FieldBootTarget: "Pxe"}
}

// splitPlan は観測（書き込みでない ctx）で読み、diff を通す。
func splitPlan(t *testing.T, p provider.Provider) core.Plan {
	t.Helper()
	obs, err := p.Read(context.Background(), "node1")
	if err != nil || obs.Availability != core.AvailabilityOk {
		t.Fatalf("Read with the observe credential: %v %+v", err, obs)
	}
	pl, err := diff.ForProvider(p).Plan(obs, diff.DesiredResource{ID: core.NewID(system.Name, core.NewScope(nil), "node1"), Fields: splitWant()},
		cause.CauseContext{})
	if err != nil {
		t.Fatal(err)
	}
	return pl
}

func TestCredentialSplit_ObserveReadOnlyWriteAdmin(t *testing.T) {
	// [AC-186-3-1] 観測は ReadOnly の利用者の GET だけ、Apply と Action は
	// 管理者が書いて収束する。
	srv, path := splitRig(t, bothSecrets)
	p, err := registry.DefaultRegistry().Get(system.Name, map[string]string{"redfish_config": path})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		pl := splitPlan(t, p)
		if len(pl.Ops) == 0 {
			break
		}
		if err := p.Apply(provider.WithWrite(context.Background()), pl); err != nil {
			t.Fatalf("Apply with the write credential: %v", err)
		}
	}
	if pl := splitPlan(t, p); len(pl.Ops) != 0 {
		t.Fatalf("not converged: %+v", pl.Ops)
	}
	if srv.PowerState() != "On" {
		t.Fatalf("power = %s, want On", srv.PowerState())
	}
	ro, admin := srv.UserCalls(roUser), srv.UserCalls("admin")
	if ro["GET"] == 0 || ro["WRITE"] != 0 {
		t.Errorf("the ReadOnly user: %v; want reads only", ro)
	}
	if admin["WRITE"] == 0 {
		t.Errorf("the admin: %v; want the writes", admin)
	}

	// 一度きりの操作（Action）も書き込み用の資格で。
	srv.ClearRequests()
	ap, ok := p.(provider.Actioner)
	if !ok {
		t.Fatal("redfish-system is not an Actioner")
	}
	ctx := provider.WithWrite(context.Background())
	if err := ap.Action(ctx, "node1", system.ActionBootOnce, map[string]string{system.ArgTarget: "Cd"}); err != nil {
		t.Fatalf("boot_once: %v", err)
	}
	if err := ap.Action(ctx, "node1", system.ActionReset, nil); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if ro := srv.UserCalls(roUser); len(ro) != 0 {
		t.Errorf("the ReadOnly user was used by an Action: %v", ro)
	}
	if admin := srv.UserCalls("admin"); admin["WRITE"] != 2 {
		t.Errorf("the admin: %v; want 2 writes (boot_once, reset)", admin)
	}
	for _, r := range srv.Requests() {
		if r.Method != "GET" && r.User != "admin" {
			t.Errorf("%s %s by %q", r.Method, r.Path, r.User)
		}
	}
}

func TestCredentialSplit_ObserveOnlyRunNeedsNoWritePassword(t *testing.T) {
	// [AC-186-3-2] 書かない回は書き込み用の password が store に無くても
	// 起動して観測し、書き込みは送らずに ErrNoWriteAccess で断る。書く回は
	// 起動を断る。
	srv, path := splitRig(t, `{"secrets": {"bmc/ro": {"value": "`+roPassword+`"}}}`)
	reg := registry.DefaultRegistry()
	for _, name := range []string{system.Name, system.FirmwareName} {
		if _, err := reg.Get(name, map[string]string{"redfish_config": path}); err == nil ||
			!strings.Contains(err.Error(), "bmcs.node1.write.password") {
			t.Fatalf("%s: a writing run without the write password: %v", name, err)
		}
		if _, err := reg.Get(name, map[string]string{"redfish_config": path, provider.ConfigAccess: provider.AccessObserve}); err != nil {
			t.Fatalf("%s: an observe-only run: %v", name, err)
		}
	}
	p, err := reg.Get(system.Name, map[string]string{"redfish_config": path, provider.ConfigAccess: provider.AccessObserve})
	if err != nil {
		t.Fatal(err)
	}
	pl := splitPlan(t, p)
	if len(pl.Ops) == 0 {
		t.Fatal("want a plan to power on")
	}
	if err := p.Apply(provider.WithWrite(context.Background()), pl); !errors.Is(err, provider.ErrNoWriteAccess) {
		t.Fatalf("Apply in an observe-only run: %v", err)
	}
	ap := p.(provider.Actioner)
	if err := ap.Action(provider.WithWrite(context.Background()), "node1", system.ActionReset, nil); !errors.Is(err, provider.ErrNoWriteAccess) {
		t.Fatalf("Action in an observe-only run: %v", err)
	}
	if a := srv.UserCalls("admin"); len(a) != 0 {
		t.Errorf("the write user was used: %v", a)
	}
	if ro := srv.UserCalls(roUser); ro["WRITE"] != 0 {
		t.Errorf("the ReadOnly user tried to write: %v", ro)
	}
	if srv.PowerState() != "Off" {
		t.Errorf("power = %s; the observe-only run changed the BMC", srv.PowerState())
	}
	if _, err := reg.Get(system.Name, map[string]string{"redfish_config": path, provider.ConfigAccess: "read"}); err == nil {
		t.Error("an unknown access must be refused")
	}
}

func TestCredentialSplit_ReadOnlyWriteIsRefusedByBMC(t *testing.T) {
	// [AC-186-3-3] 書き込み用に ReadOnly の利用者を書き間違えれば、BMC が 403 で
	// 断り（認証の失敗、設定を直すまで治らない）、何も変わらない。
	srv, path := splitRig(t, bothSecrets)
	cfg, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wrong := strings.Replace(string(cfg), "    write:\n      username: admin\n      password: secretref:file/bmc/admin\n",
		"    write:\n      username: "+roUser+"\n      password: secretref:file/bmc/ro\n", 1)
	if err := os.WriteFile(path, []byte(wrong), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := registry.DefaultRegistry().Get(system.Name, map[string]string{"redfish_config": path})
	if err != nil {
		t.Fatal(err)
	}
	pl := splitPlan(t, p)
	err = p.Apply(provider.WithWrite(context.Background()), pl)
	var pe *provider.PermanentError
	if err == nil || !errors.As(err, &pe) || !strings.Contains(err.Error(), "403") {
		t.Fatalf("Apply with the ReadOnly user: %v; want a permanent 403", err)
	}
	if ro := srv.UserCalls(roUser); ro["WRITE"] == 0 {
		t.Errorf("the ReadOnly user: %v; want the refused write", ro)
	}
	if srv.PowerState() != "Off" {
		t.Errorf("power = %s; a refused write changed the BMC", srv.PowerState())
	}
}
