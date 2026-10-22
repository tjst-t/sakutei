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

package system

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/cause"
	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/redfish"
	"github.com/tjst-t/sakutei/internal/redfish/redfishtest"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
	"github.com/tjst-t/sakutei/pkg/sdk"
)

// clock は偽 BMC と provider で共有する時刻。
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// newBMC は偽 BMC と、それに向けた Client を返す。
func newBMC(t *testing.T, opts redfishtest.Options) (*redfishtest.Server, redfish.Client) {
	t.Helper()
	srv := redfishtest.New(opts)
	t.Cleanup(srv.Close)
	ca, err := srv.WriteCAFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := redfish.New(redfish.Config{
		Endpoint: srv.URL, Username: "admin", Password: "password", CAFile: ca, Timeout: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, c
}

func TestValidateProvider(t *testing.T) {
	if err := sdk.ValidateProvider(New(nil, Options{})); err != nil {
		t.Fatalf("ValidateProvider: %v", err)
	}
	caps := New(nil, Options{}).Capabilities()
	if caps.List || caps.BulkObserve || caps.Pruneable || !caps.TrueObserve {
		t.Errorf("Capabilities = %+v, want the standard tier (no List / BulkObserve, not pruneable)", caps)
	}
}

// 契約テスト（#32）を偽 BMC の上で回す。サーバは前からあるので Preexisting。
func TestContract(t *testing.T) {
	srv, c := newBMC(t, redfishtest.Options{PowerState: "Off"})
	p := New(map[string]redfish.Client{"node1": c}, Options{})
	ct := sdk.ContractTester{
		Provider:    p,
		Scope:       core.NewScope(nil),
		SampleID:    "node1",
		Preexisting: true,
		SampleDesired: map[string]string{
			FieldName: "node1", FieldPower: "On", FieldBootEnabled: "Continuous", FieldBootTarget: "Pxe",
		},
		MakeUnreachable: func() (func(), error) {
			srv.SetUnreachable(true)
			return func() { srv.SetUnreachable(false) }, nil
		},
	}
	ct.RunAll(t)
}

func TestRead_ObservedOnlyValues(t *testing.T) {
	_, c := newBMC(t, redfishtest.Options{
		UUID: "11111111-2222-3333-4444-555555555555", MACs: []string{"52:54:00:00:00:01", "52:54:00:00:00:02"},
		MemoryGiB: 8, CPUs: 4, BootMode: "Legacy",
	})
	obs, err := New(map[string]redfish.Client{"n": c}, Options{}).Read(context.Background(), "n")
	if err != nil || obs.Availability != core.AvailabilityOk {
		t.Fatalf("Read = %+v, %v", obs, err)
	}
	want := map[string]string{
		FieldName: "n", FieldPower: "On", FieldPowerState: "On", FieldUUID: "11111111-2222-3333-4444-555555555555",
		FieldMACs: "52:54:00:00:00:01,52:54:00:00:00:02", FieldCPUCount: "4", FieldMemoryGiB: "8", FieldBootMode: "Legacy",
		FieldBootEnabled: "Disabled", FieldBootTarget: "None",
	}
	for k, v := range want {
		if got := sdkString(obs.State[k]); got != v {
			t.Errorf("State[%s] = %q, want %q", k, got, v)
		}
	}
	for _, k := range []string{FieldPowerOff, FieldForceAfter} {
		if _, ok := obs.State[k]; ok {
			t.Errorf("State has %s; it is desired-only", k)
		}
	}
	schema := New(nil, Options{}).Schema()
	for _, k := range []string{FieldUUID, FieldMACs, FieldCPUCount, FieldMemoryGiB, FieldBootMode, FieldPowerState} {
		if s := schema.Fields[k]; !s.Computed || s.Identity {
			t.Errorf("Schema %s = %+v, want Computed and not Identity", k, s)
		}
	}
}

func sdkString(tv core.TypedValue) string { return fmt.Sprintf("%v", tv.Value) }

func TestRead_PoweringIsConverging(t *testing.T) {
	clk := newClock()
	srv, c := newBMC(t, redfishtest.Options{GracefulDelay: time.Minute, Now: clk.Now})
	p := New(map[string]redfish.Client{"n": c}, Options{})
	if err := c.Reset(context.Background(), redfish.ResetGracefulShutdown); err != nil {
		t.Fatal(err)
	}
	obs, err := p.Read(context.Background(), "n")
	if err != nil || obs.Availability != core.AvailabilityConverging || obs.State != nil {
		t.Fatalf("Read while PoweringOff = %+v, %v; want Converging without State", obs, err)
	}
	clk.Advance(time.Minute)
	if srv.PowerState() != "Off" {
		t.Fatalf("power = %s, want Off", srv.PowerState())
	}
}

func TestRead_UnreachableIsUnknown(t *testing.T) {
	cases := map[string]func(*redfishtest.Server){
		"connection closed": func(s *redfishtest.Server) { s.SetUnreachable(true) },
		"5xx":               func(s *redfishtest.Server) { s.SetFailStatus(http.StatusServiceUnavailable) },
		"timeout":           func(s *redfishtest.Server) { s.SetDelay(2 * time.Second) },
	}
	for name, break_ := range cases {
		t.Run(name, func(t *testing.T) {
			srv, c := newBMC(t, redfishtest.Options{})
			p := New(map[string]redfish.Client{"n": c}, Options{})
			break_(srv)
			obs, err := p.Read(context.Background(), "n")
			if obs.Availability != core.AvailabilityUnknown || obs.State != nil || !sdk.IsTransient(err) {
				t.Fatalf("Read = %+v, %v; want Unknown with a transient error", obs, err)
			}
		})
	}
	// BMC の設定が無いノードも absent ではない。
	obs, err := New(nil, Options{}).Read(context.Background(), "nobody")
	if obs.Availability != core.AvailabilityUnknown || err == nil {
		t.Fatalf("Read of an unconfigured node = %+v, %v; want Unknown and an error", obs, err)
	}
}

func TestValidateIntent(t *testing.T) {
	p := New(map[string]redfish.Client{"n": nil}, Options{})
	ok := []map[string]string{
		{"name": "n", "power": "On"},
		{"power": "Off", "power_off": "graceful"},
		{"power": "Off", "power_off": "graceful_then_force", "force_after": "5m"},
		{"boot_enabled": "Continuous", "boot_target": "Pxe"},
		{"boot_enabled": "Disabled"},
		{"boot_enabled": "Disabled", "boot_target": "Pxe"},
	}
	for _, f := range ok {
		if err := p.ValidateIntent("n", f); err != nil {
			t.Errorf("ValidateIntent(%v) = %v, want nil", f, err)
		}
	}
	bad := []struct {
		fields map[string]string
		want   string
	}{
		{map[string]string{"boot_enabled": "Once", "boot_target": "Pxe"}, "Task"},
		{map[string]string{"power": "ForceRestart"}, "one-time operation"},
		{map[string]string{"power": "GracefulRestart"}, "one-time operation"},
		{map[string]string{"power": "Paused"}, "want On or Off"},
		{map[string]string{"power": "Off", "power_off": "graceful_then_force"}, "needs force_after"},
		{map[string]string{"power": "Off", "force_after": "5m"}, "only for power_off graceful_then_force"},
		{map[string]string{"power": "Off", "power_off": "graceful", "force_after": "5m"}, "only for power_off"},
		{map[string]string{"power_off": "graceful_then_force", "force_after": "soon"}, "positive duration"},
		{map[string]string{"power_off": "force"}, "power_off \"force\""},
		{map[string]string{"boot_enabled": "Continuous"}, "needs boot_target"},
		{map[string]string{"boot_target": "Pxe"}, "needs boot_enabled"},
		{map[string]string{"uuid": "x"}, "observed only"},
		{map[string]string{"colour": "x"}, "unknown field"},
		{map[string]string{"name": "other"}, "must equal"},
	}
	for _, b := range bad {
		err := p.ValidateIntent("n", b.fields)
		if err == nil || !strings.Contains(err.Error(), b.want) {
			t.Errorf("ValidateIntent(%v) = %v, want an error containing %q", b.fields, err, b.want)
		}
	}
	if err := p.ValidateIntent("m", map[string]string{"power": "On"}); err == nil || !strings.Contains(err.Error(), "no BMC is configured") {
		t.Errorf("a node without a BMC: %v, want an error", err)
	}
	if err := p.ValidateIntent("n", map[string]string{"boot_enabled": "Once"}); !strings.Contains(err.Error(), "never converges") {
		t.Errorf("the Once error does not say why: %v", err)
	}
}

// plan は偽 BMC を Read して diff.Engine.Plan を通す（RefinePlan まで）。
func plan(t *testing.T, p *Provider, node string, fields map[string]string) (core.Plan, error) {
	t.Helper()
	obs, err := p.Read(context.Background(), node)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return diff.ForProvider(p).Plan(obs, diff.DesiredResource{ID: core.NewID(Name, core.NewScope(nil), node), Fields: fields}, cause.CauseContext{})
}

func TestRefinePlan(t *testing.T) {
	_, c := newBMC(t, redfishtest.Options{BootTargets: []string{"None", "Pxe", "Hdd"}})
	p := New(map[string]redfish.Client{"n": c}, Options{})

	// Disabled のときは boot_target を比べず、power_off は差分にならない。
	pl, err := plan(t, p, "n", map[string]string{"name": "n", "power": "On", "power_off": "graceful", "boot_enabled": "Disabled", "boot_target": "Pxe"})
	if err != nil || len(pl.Ops) != 0 {
		t.Fatalf("Plan = %+v, %v; want converged", pl, err)
	}
	// AllowableValues に無い起動先は計画の時点でエラー。
	if _, err := plan(t, p, "n", map[string]string{"boot_enabled": "Continuous", "boot_target": "Cd"}); err == nil ||
		!strings.Contains(err.Error(), `boot_target "Cd" is not offered`) {
		t.Fatalf("Plan with an unoffered boot_target = %v, want an error", err)
	}
	// 既定の停止は破壊的に数えない。
	pl, err = plan(t, p, "n", map[string]string{"power": "Off"})
	if err != nil || len(pl.Ops) != 1 || pl.Ops[0].Destructive || len(pl.Ops[0].Fields) != 1 {
		t.Fatalf("Plan power Off = %+v, %v; want one non-destructive op with the power diff only", pl, err)
	}
}

func TestRefinePlan_ResetTypeNotOffered(t *testing.T) {
	_, c := newBMC(t, redfishtest.Options{ResetTypes: []string{"On", "ForceOff"}})
	p := New(map[string]redfish.Client{"n": c}, Options{})
	if _, err := plan(t, p, "n", map[string]string{"power": "Off"}); err == nil || !strings.Contains(err.Error(), "GracefulShutdown") {
		t.Fatalf("Plan = %v, want an error naming GracefulShutdown", err)
	}
}

func TestApply_BootBeforePowerAndNoReboot(t *testing.T) {
	srv, c := newBMC(t, redfishtest.Options{PowerState: "Off"})
	p := New(map[string]redfish.Client{"n": c}, Options{})
	pl, err := plan(t, p, "n", map[string]string{"power": "On", "boot_enabled": "Continuous", "boot_target": "Hdd"})
	if err != nil {
		t.Fatal(err)
	}
	srv.ClearRequests()
	if err := p.Apply(context.Background(), pl); err != nil {
		t.Fatal(err)
	}
	var writes []string
	for _, r := range srv.Requests() {
		if r.Method != http.MethodGet {
			writes = append(writes, r.Method+" "+r.Body)
		}
	}
	if len(writes) != 2 || !strings.HasPrefix(writes[0], "PATCH") || !strings.Contains(writes[1], `"On"`) {
		t.Fatalf("writes = %v, want PATCH Boot then Reset On", writes)
	}
	// 起動先だけを変えても再起動しない。
	pl, _ = plan(t, p, "n", map[string]string{"power": "On", "boot_enabled": "Continuous", "boot_target": "Pxe"})
	boots := srv.Boots()
	if err := p.Apply(context.Background(), pl); err != nil {
		t.Fatal(err)
	}
	if srv.Boots() != boots || srv.PowerState() != "On" {
		t.Fatalf("changing the boot target rebooted (boots %d -> %d)", boots, srv.Boots())
	}
	if e, tg := srv.Boot(); e != "Continuous" || tg != "Pxe" {
		t.Fatalf("Boot = %s/%s, want Continuous/Pxe", e, tg)
	}
}

// 時刻を失ったら（provider を作り直したら）force_after は数え直す。
func TestGracefulThenForce_LostClockRecounts(t *testing.T) {
	clk := newClock()
	srv, c := newBMC(t, redfishtest.Options{GracefulTimeout: time.Second, Now: clk.Now})
	srv.SetIgnoreGraceful(true)
	desired := map[string]string{"power": "Off", "power_off": "graceful_then_force", "force_after": "1m"}
	p := New(map[string]redfish.Client{"n": c}, Options{Now: clk.Now})
	pl, _ := plan(t, p, "n", desired)
	if err := p.Apply(context.Background(), pl); err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Minute)

	p = New(map[string]redfish.Client{"n": c}, Options{Now: clk.Now}) // restart: S3 lost
	pl, err := plan(t, p, "n", desired)
	if err != nil || len(pl.Ops) != 1 || pl.Ops[0].Destructive {
		t.Fatalf("Plan after losing the clock = %+v, %v; want a non-destructive GracefulShutdown", pl, err)
	}
	if err := p.Apply(context.Background(), pl); err != nil {
		t.Fatal(err)
	}
	if at, ok := p.GracefulSentAt("n"); !ok || !at.Equal(clk.Now()) {
		t.Fatalf("GracefulSentAt = %v, %v; want recounted from now", at, ok)
	}
}

var _ provider.PlanRefiner = (*Provider)(nil)
