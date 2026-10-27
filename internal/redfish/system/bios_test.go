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
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/cause"
	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/redfish"
	"github.com/tjst-t/sakutei/v2/internal/redfish/redfishtest"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
)

func TestBios_ValidateProvider(t *testing.T) {
	if err := sdk.ValidateProvider(NewBios(nil)); err != nil {
		t.Fatalf("ValidateProvider: %v", err)
	}
	caps := NewBios(nil).Capabilities()
	if caps.List || caps.BulkObserve || caps.Pruneable || !caps.TrueObserve || !caps.Converging {
		t.Errorf("Capabilities = %+v, want the standard tier that reports Converging (no List / BulkObserve, not pruneable)", caps)
	}
}

// rebootAfterApply は Apply のあと偽 BMC を再起動する（BIOS の変更は次の
// 起動で効く。Workflow の手順の reset の代役）。契約テストは Apply の直後の
// Read が宣言どおりであることを求めるので、Apply と再起動を組にして回す。
type rebootAfterApply struct {
	*BiosProvider
	c redfish.Client
}

func (r rebootAfterApply) Apply(ctx context.Context, plan core.Plan) error {
	if err := r.BiosProvider.Apply(ctx, plan); err != nil {
		return err
	}
	return r.c.Reset(ctx, redfish.ResetForceRestart)
}

// 契約テスト（#32）を偽 BMC の上で回す。BIOS は前からあるので Preexisting。
// 宣言は今の値の全部（契約テストは観測の attributes を宣言の文字列と比べる）
// で、そのうち 4 つを変える。
// [AC-140-1]
func TestBios_Contract(t *testing.T) {
	srv, c := newBMC(t, redfishtest.Options{})
	p := NewBios(map[string]redfish.Client{"node1": c})
	ct := sdk.ContractTester{
		Provider:    rebootAfterApply{BiosProvider: p, c: c},
		Scope:       core.NewScope(nil),
		SampleID:    BiosID("node1"),
		Preexisting: true,
		SampleDesired: map[string]string{
			FieldName: "node1",
			FieldAttributes: `{"AssetTag":"rack-1 <a&b>","BootMode":"Uefi","BootRetryCount":5,"PowerProfile":"Balanced",` +
				`"ProcHyperThreading":"Enabled","ProcVirtualization":"Disabled","SriovGlobalEnable":true}`,
		},
		MakeUnreachable: func() (func(), error) {
			srv.SetUnreachable(true)
			return func() { srv.SetUnreachable(false) }, nil
		},
	}
	ct.RunAll(t)
}

// biosPlan は偽 BMC を Read して diff.Engine.Plan を通す。
func biosPlan(t *testing.T, p *BiosProvider, node string, fields map[string]string) (core.Observation, core.Plan, error) {
	t.Helper()
	obs, err := p.Read(context.Background(), BiosID(node))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	pl, err := diff.ForProvider(p).Plan(obs, diff.DesiredResource{
		ID: core.NewID(BiosName, core.NewScope(nil), BiosID(node)), Fields: fields,
	}, cause.CauseContext{})
	return obs, pl, err
}

func biosFields(attrs string) map[string]string {
	return map[string]string{FieldName: "n", FieldAttributes: attrs}
}

// 宣言した属性だけを比べ、書かない属性の違いは差分にしない。PATCH は宣言した
// 属性のうち違うものだけ。
// [AC-140-2]
func TestBios_OnlyDeclaredAttributes(t *testing.T) {
	srv, c := newBMC(t, redfishtest.Options{})
	p := NewBios(map[string]redfish.Client{"n": c})
	ctx := context.Background()

	// 宣言に無い属性を人が変えても差分にしない。
	srv.SetBiosAttribute("PowerProfile", "PowerSaving")
	_, pl, err := biosPlan(t, p, "n", biosFields(`{"ProcVirtualization":"Enabled","BootRetryCount":3}`))
	if err != nil || len(pl.Ops) != 0 {
		t.Fatalf("plan with the declared attributes as they are = %+v, %v; want no op", pl, err)
	}

	// 宣言した属性が違えば差分。Diagnostic に違う属性だけを書く。
	_, pl, err = biosPlan(t, p, "n", biosFields(`{"ProcVirtualization":"Disabled","BootRetryCount":3}`))
	if err != nil || len(pl.Ops) != 1 {
		t.Fatalf("plan = %+v, %v; want one op", pl, err)
	}
	d := pl.Ops[0].Diagnostic
	if !strings.Contains(d, `ProcVirtualization: "Enabled" → "Disabled"`) || strings.Contains(d, "BootRetryCount") || strings.Contains(d, "PowerProfile") {
		t.Errorf("Diagnostic = %q, want only ProcVirtualization", d)
	}
	if pl.Ops[0].Destructive {
		t.Errorf("op is destructive; a BIOS setting is not")
	}

	srv.ClearRequests()
	if err := p.Apply(ctx, pl); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var patches []redfishtest.Request
	for _, r := range srv.Requests() {
		if r.Method != http.MethodGet {
			patches = append(patches, r)
		}
	}
	if len(patches) != 1 || patches[0].Method != http.MethodPatch || patches[0].Path != redfishtest.BiosSettingsPath ||
		patches[0].Body != `{"Attributes":{"ProcVirtualization":"Disabled"}}` {
		t.Fatalf("writes = %+v, want one PATCH of Bios/Settings with ProcVirtualization only", patches)
	}
	// 再起動はしない。今の値は変わらず、保留中に入る。
	if got := srv.BiosAttributes()["ProcVirtualization"]; got != "Enabled" {
		t.Errorf("current ProcVirtualization = %v right after Apply, want Enabled (applies at the next reboot)", got)
	}
	if got := srv.BiosPending(); len(got) != 1 || got["ProcVirtualization"] != "Disabled" {
		t.Errorf("pending = %v, want ProcVirtualization Disabled", got)
	}
	if srv.Boots() != 0 {
		t.Errorf("the BMC booted %d times; Apply must not reboot", srv.Boots())
	}
	if got := srv.BiosAttributes()["PowerProfile"]; got != "PowerSaving" {
		t.Errorf("PowerProfile = %v, want PowerSaving (not declared, not touched)", got)
	}
}

// 保留中の変更があるあいだは Converging で、同じ変更を重ねない。再起動で
// 効けば Ok に戻り、差分は無い。
// [AC-140-3]
func TestBios_PendingIsConverging(t *testing.T) {
	srv, c := newBMC(t, redfishtest.Options{})
	p := NewBios(map[string]redfish.Client{"n": c})
	ctx := context.Background()
	fields := biosFields(`{"ProcVirtualization":"Disabled","SriovGlobalEnable":true}`)

	_, pl, err := biosPlan(t, p, "n", fields)
	if err != nil || len(pl.Ops) != 1 {
		t.Fatalf("plan = %+v, %v", pl, err)
	}
	if err := p.Apply(ctx, pl); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	obs, err := p.Read(ctx, BiosID("n"))
	if err != nil || obs.Availability != core.AvailabilityConverging {
		t.Fatalf("Read after Apply = %+v, %v; want Converging (the change is pending)", obs, err)
	}
	if got := sdkString(obs.State[FieldPendingAttributes]); got != `{"ProcVirtualization":"Disabled","SriovGlobalEnable":true}` {
		t.Errorf("pending_attributes = %s", got)
	}
	// Converging の観測からは計画しない（core は op を出さない）。
	pl2, err := diff.ForProvider(p).Plan(obs, diff.DesiredResource{
		ID: core.NewID(BiosName, core.NewScope(nil), BiosID("n")), Fields: fields,
	}, cause.CauseContext{})
	if err != nil || len(pl2.Ops) != 0 {
		t.Fatalf("plan on a Converging observation = %+v, %v; want no op", pl2, err)
	}
	// 同じ plan をもう一度 Apply しても PATCH を重ねない（保留中が宣言どおり）。
	srv.ClearRequests()
	if err := p.Apply(ctx, pl); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	for _, r := range srv.Requests() {
		if r.Method != http.MethodGet {
			t.Fatalf("second Apply sent %s %s; the pending change is already as declared", r.Method, r.Path)
		}
	}

	// 再起動で効く。
	if err := c.Reset(ctx, redfish.ResetForceRestart); err != nil {
		t.Fatal(err)
	}
	_, pl, err = biosPlan(t, p, "n", fields)
	if err != nil || len(pl.Ops) != 0 {
		t.Fatalf("plan after the reboot = %+v, %v; want converged", pl, err)
	}
	if got := sdkString(func() core.State { o, _ := p.Read(ctx, BiosID("n")); return o.State }()[FieldPendingAttributes]); got != "{}" {
		t.Errorf("pending_attributes after the reboot = %s, want {}", got)
	}
}

// 保留中の変更が今の値と同じなら保留中に数えない。既定値への戻しが保留中なら
// Converging。保留中の変更が宣言と違えば Apply は上書きする。
func TestBios_PendingEdgeCases(t *testing.T) {
	srv, c := newBMC(t, redfishtest.Options{})
	p := NewBios(map[string]redfish.Client{"n": c})
	ctx := context.Background()

	srv.SetBiosPending(map[string]any{"ProcVirtualization": "Enabled"}) // 今の値と同じ
	if obs, err := p.Read(ctx, BiosID("n")); err != nil || obs.Availability != core.AvailabilityOk {
		t.Fatalf("Read with a pending value equal to the current one = %+v, %v; want Ok", obs, err)
	}

	srv.SetBiosResetPending(true)
	if obs, err := p.Read(ctx, BiosID("n")); err != nil || obs.Availability != core.AvailabilityConverging {
		t.Fatalf("Read with ResetBiosToDefaultsPending = %+v, %v; want Converging", obs, err)
	}
	srv.SetBiosResetPending(false)

	// ほかの道具が違う値を保留中に置いた: Read は Converging（取り合わない）。
	srv.SetBiosPending(map[string]any{"PowerProfile": "PowerSaving"})
	if obs, err := p.Read(ctx, BiosID("n")); err != nil || obs.Availability != core.AvailabilityConverging {
		t.Fatalf("Read = %+v, %v; want Converging", obs, err)
	}
	// Apply は保留中の値と比べる（今の値と同じでも保留中が違えば PATCH する）。
	plan := core.Plan{Ops: []core.Op{{Kind: core.OpKindUpdate, Target: core.NewID(BiosName, core.NewScope(nil), BiosID("n")),
		Fields: []core.FieldDiff{{Field: FieldAttributes, To: `{"PowerProfile":"MaxPerformance"}`}}}}}
	if err := p.Apply(ctx, plan); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := srv.BiosPending()["PowerProfile"]; got != "MaxPerformance" {
		t.Fatalf("pending PowerProfile = %v, want MaxPerformance", got)
	}
	if obs, err := p.Read(ctx, BiosID("n")); err != nil || obs.Availability != core.AvailabilityOk {
		t.Fatalf("Read after overwriting the pending value with the current one = %+v, %v; want Ok", obs, err)
	}
}

// 登録簿で属性名・読み取り専用・型・選べる値・長さ・範囲を計画の時点で断る。
// [AC-140-4]
func TestBios_RegistryChecks(t *testing.T) {
	srv, c := newBMC(t, redfishtest.Options{})
	p := NewBios(map[string]redfish.Client{"n": c})
	for attrs, want := range map[string]string{
		`{"NoSuchThing":"x"}`:                            `"NoSuchThing" is not in the BMC's attribute registry BiosAttributeRegistry.1.0.0`,
		`{"BootMode":"Bios"}`:                            `"BootMode" is read-only`,
		`{"ProcVirtualization":"Off"}`:                   `"Off" is not one of the values the BMC offers (Enabled, Disabled)`,
		`{"ProcVirtualization":true}`:                    `the value true is a boolean`,
		`{"BootRetryCount":"5"}`:                         `want a whole number`,
		`{"BootRetryCount":2.5}`:                         `want a whole number`,
		`{"BootRetryCount":11}`:                          `above the upper bound 10`,
		`{"BootRetryCount":-1}`:                          `below the lower bound 0`,
		`{"SriovGlobalEnable":"Enabled"}`:                `want a boolean`,
		`{"AssetTag":"` + strings.Repeat("x", 65) + `"}`: `longer than 64 characters`,
	} {
		_, pl, err := biosPlan(t, p, "n", biosFields(attrs))
		if err == nil || !strings.Contains(err.Error(), want) || len(pl.Ops) != 0 {
			t.Errorf("plan %s = %+v, %v; want an error containing %q", attrs, pl, err, want)
		}
	}
	// 登録簿は 1 度だけ読む（Id が同じあいだ）。
	srv.ClearRequests()
	for i := 0; i < 3; i++ {
		if _, err := p.Read(context.Background(), BiosID("n")); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range srv.Requests() {
		if strings.HasPrefix(r.Path, redfishtest.RegistriesPath) {
			t.Fatalf("the registry was read again: %s %s", r.Method, r.Path)
		}
	}
}

// 登録簿の無い BMC では、今の値に無い属性名と、JSON の型の違いだけを断る。
func TestBios_NoRegistry(t *testing.T) {
	srv, c := newBMC(t, redfishtest.Options{NoBiosRegistry: true})
	p := NewBios(map[string]redfish.Client{"n": c})
	for attrs, want := range map[string]string{
		`{"NoSuchThing":"x"}`:         `"NoSuchThing" is not reported by the BMC`,
		`{"BootRetryCount":"5"}`:      `the value "5" is a string, but the BMC reports a number (3)`,
		`{"SriovGlobalEnable":"yes"}`: `the BMC reports a boolean`,
	} {
		_, _, err := biosPlan(t, p, "n", biosFields(attrs))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("plan %s = %v; want an error containing %q", attrs, err, want)
		}
	}
	// 選べない値は計画を通り、PATCH で BMC が断る（設定を直すまで治らない）。
	_, pl, err := biosPlan(t, p, "n", biosFields(`{"ProcVirtualization":"Off"}`))
	if err != nil || len(pl.Ops) != 1 {
		t.Fatalf("plan = %+v, %v", pl, err)
	}
	err = p.Apply(context.Background(), pl)
	if !sdk.IsPermanent(err) || !strings.Contains(err.Error(), "not in the list of acceptable values") {
		t.Fatalf("Apply = %v; want a permanent error with the BMC's message", err)
	}
	if len(srv.BiosPending()) != 0 {
		t.Fatalf("pending = %v after a rejected PATCH", srv.BiosPending())
	}
}

// Bios を持たない BMC は、属性を宣言したときだけ計画の時点のエラー。
// [AC-140-5]
func TestBios_NoBios(t *testing.T) {
	_, c := newBMC(t, redfishtest.Options{NoBios: true})
	p := NewBios(map[string]redfish.Client{"n": c})
	obs, err := p.Read(context.Background(), BiosID("n"))
	if err != nil || obs.Availability != core.AvailabilityOk || sdkString(obs.State[FieldBiosPresent]) != "false" {
		t.Fatalf("Read = %+v, %v; want Ok with bios_present false", obs, err)
	}
	_, pl, err := biosPlan(t, p, "n", biosFields(`{"ProcVirtualization":"Disabled"}`))
	if err == nil || !strings.Contains(err.Error(), "has no Bios resource") || len(pl.Ops) != 0 {
		t.Fatalf("plan = %+v, %v; want a no-Bios error", pl, err)
	}
	for _, f := range []map[string]string{{FieldName: "n"}, biosFields("{}")} {
		if _, pl, err := biosPlan(t, p, "n", f); err != nil || len(pl.Ops) != 0 {
			t.Fatalf("plan %v = %+v, %v; want nothing to do", f, pl, err)
		}
	}
	// システムの provider は Bios が無くても困らない。
	if obs, err := New(map[string]redfish.Client{"n": c}, Options{}).Read(context.Background(), "n"); err != nil || obs.Availability != core.AvailabilityOk {
		t.Fatalf("system Read = %+v, %v", obs, err)
	}
}

// 届かないのは Unknown（何も操作しない）。
func TestBios_ReadUnreachableIsUnknown(t *testing.T) {
	cases := map[string]func(*redfishtest.Server){
		"connection closed": func(s *redfishtest.Server) { s.SetUnreachable(true) },
		"5xx":               func(s *redfishtest.Server) { s.SetFailStatus(http.StatusServiceUnavailable) },
		"timeout":           func(s *redfishtest.Server) { s.SetDelay(2 * time.Second) },
	}
	for name, break_ := range cases {
		t.Run(name, func(t *testing.T) {
			srv, c := newBMC(t, redfishtest.Options{})
			break_(srv)
			obs, err := NewBios(map[string]redfish.Client{"n": c}).Read(context.Background(), BiosID("n"))
			if obs.Availability != core.AvailabilityUnknown || obs.State != nil || !sdk.IsTransient(err) {
				t.Fatalf("Read = %+v, %v; want Unknown with a transient error", obs, err)
			}
		})
	}
	for _, id := range []string{"nobody/bios", "n", "/bios"} {
		obs, err := NewBios(nil).Read(context.Background(), id)
		if obs.Availability != core.AvailabilityUnknown || obs.State != nil || err == nil {
			t.Fatalf("Read(%q) = %+v, %v; want Unknown and an error", id, obs, err)
		}
	}
	// 届かないあいだの Apply は一時的なエラーで、何も送っていない。
	srv, c := newBMC(t, redfishtest.Options{})
	srv.SetUnreachable(true)
	plan := core.Plan{Ops: []core.Op{{Kind: core.OpKindUpdate, Target: core.NewID(BiosName, core.NewScope(nil), BiosID("n")),
		Fields: []core.FieldDiff{{Field: FieldAttributes, To: `{"ProcVirtualization":"Disabled"}`}}}}}
	if err := NewBios(map[string]redfish.Client{"n": c}).Apply(context.Background(), plan); !sdk.IsTransient(err) {
		t.Fatalf("Apply = %v; want a transient error", err)
	}
	srv.SetUnreachable(false)
	if len(srv.BiosPending()) != 0 {
		t.Fatalf("pending = %v", srv.BiosPending())
	}
}

func TestBios_ValidateIntent(t *testing.T) {
	p := NewBios(map[string]redfish.Client{"n": nil})
	for _, f := range []map[string]string{
		{"name": "n", "attributes": `{"ProcVirtualization": "Disabled", "BootRetryCount": 5, "SriovGlobalEnable": true}`},
		{"attributes": "{}"},
		{"attributes": ""},
		{},
	} {
		if err := p.ValidateIntent("n/bios", f); err != nil {
			t.Errorf("ValidateIntent(%v) = %v, want nil", f, err)
		}
	}
	bad := []struct {
		id     string
		fields map[string]string
		want   string
	}{
		{"n/bios", map[string]string{"attributes": `["a"]`}, "must be a JSON object"},
		{"n/bios", map[string]string{"attributes": `{"a": 1`}, "not valid JSON"},
		{"n/bios", map[string]string{"attributes": `{"a": 1} {}`}, "data after"},
		{"n/bios", map[string]string{"attributes": `{"a": null}`}, `the value of "a" must be`},
		{"n/bios", map[string]string{"attributes": `{"a": {"b": 1}}`}, `the value of "a" must be`},
		{"n/bios", map[string]string{"attributes": `{"": 1}`}, "empty attribute name"},
		{"n/bios", map[string]string{"pending_attributes": "{}"}, "observed only"},
		{"n/bios", map[string]string{"bios_present": "true"}, "observed only"},
		{"n/bios", map[string]string{"power": "On"}, "unknown field"},
		{"n/bios", map[string]string{"name": "m"}, "must equal the node name"},
		{"n", map[string]string{}, `want "<node>/bios"`},
		{"m/bios", map[string]string{}, "no BMC is configured"},
	}
	for _, b := range bad {
		if err := p.ValidateIntent(b.id, b.fields); err == nil || !strings.Contains(err.Error(), b.want) {
			t.Errorf("ValidateIntent(%q, %v) = %v, want an error containing %q", b.id, b.fields, err, b.want)
		}
	}
}

func TestBios_ApplyDeleteIsPermanent(t *testing.T) {
	_, c := newBMC(t, redfishtest.Options{})
	plan := core.Plan{Ops: []core.Op{{Kind: core.OpKindDelete, Target: core.NewID(BiosName, core.NewScope(nil), BiosID("n"))}}}
	err := NewBios(map[string]redfish.Client{"n": c}).Apply(context.Background(), plan)
	if !sdk.IsPermanent(err) || !strings.Contains(err.Error(), "never deleted") {
		t.Fatalf("Apply delete = %v; want permanent", err)
	}
}
