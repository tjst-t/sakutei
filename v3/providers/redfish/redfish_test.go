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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/providers/redfish"
	"github.com/tjst-t/sakutei/v3/providers/redfish/redfishtest"
	"github.com/tjst-t/sakutei/v3/sdk/contract"
)

const (
	roPassword = "ro-secret-7f3a"
	rwPassword = "rw-secret-91bc"
)

type bmc struct {
	srv *redfishtest.Server
	p   *redfish.Provider
	dir string
}

// newBMC は偽の BMC と、それにつないだ provider（観測は ReadOnly の monitor、
// 書き込みは admin）を作る。
func newBMC(t *testing.T, opts redfishtest.Options) *bmc {
	t.Helper()
	opts.Password = rwPassword
	opts.ReadOnlyUsername, opts.ReadOnlyPassword = "monitor", roPassword
	srv := redfishtest.New(opts)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	ca, err := srv.WriteCAFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "ro"), []byte(roPassword+"\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "rw"), []byte(rwPassword), 0o600)
	p, err := redfish.Module(map[string]any{"nodes": map[string]any{"node1": map[string]any{
		"endpoint": srv.URL, "ca_file": ca, "timeout": "500ms",
		"system": "/redfish/v1/Systems/1", "manager": "/redfish/v1/Managers/1",
		"observe": map[string]any{"username": "monitor", "password_file": filepath.Join(dir, "ro")},
		"write":   map[string]any{"username": "admin", "password_file": filepath.Join(dir, "rw")},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return &bmc{srv: srv, p: p.(*redfish.Provider), dir: dir}
}

func id(kind, name string) core.ID { return core.ID{Type: "bmc/" + kind, Name: name} }

func (b *bmc) writes() int {
	n := 0
	for _, r := range b.srv.Requests() {
		if r.Method != http.MethodGet {
			n++
		}
	}
	return n
}

func (b *bmc) observe(t *testing.T, ids ...core.ID) map[core.ID]core.Observation {
	t.Helper()
	o, err := b.p.Observe(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func wctx() core.WriteContext { return core.NewWriteContext(nil) }

const imageURI = "http://images.example/bmc-2.0.0.bin"

func TestContract(t *testing.T) {
	contract.Run(t, func(t *testing.T) contract.Target {
		b := newBMC(t, redfishtest.Options{PowerState: "Off"})
		b.srv.AddImage(redfishtest.Image{URI: imageURI, Version: "2.0.0"})
		unreachable := func() { b.srv.SetUnreachable(true) }
		return contract.Target{
			Provider: b.p,
			Absent:   []core.ID{id("bios", "node1/NoSuchAttribute"), id("firmware", "node1/NOPE")},
			Converge: []core.Resource{
				{ID: id("system", "node1"), Fields: core.Fields{"power": "On", "boot_enabled": "Continuous", "boot_target": "Pxe"}},
				{ID: id("virtual-media", "node1/cd"), Fields: core.Fields{"image": "http://192.0.2.5/os.iso"}},
			},
			Observed: []core.ID{id("system", "node1"), id("virtual-media", "node1/cd"), id("bios", "node1/ProcVirtualization"), id("firmware", "node1/BMC")},
			Faults: map[string]func(){
				"接続を切る":  unreachable,
				"5xx":    func() { b.srv.SetFailStatus(http.StatusServiceUnavailable) },
				"タイムアウト": func() { b.srv.SetDelay(time.Second) },
				"認証の失敗":  func() { b.srv.SetFailStatus(http.StatusUnauthorized) },
				"壊れた出力":  func() { b.srv.SetBroken(true) },
			},
			Writes:               b.writes,
			Secrets:              []string{roPassword, rwPassword},
			WriteCredentialReads: func() int { c := b.srv.UserCalls("admin"); return c["GET"] + c["WRITE"] },
			Invokes: []contract.Invoke{
				{ID: id("system", "node1"), Op: redfish.OpBootOnce, Args: core.Fields{"target": "Cd"}, NotSent: unreachable},
				{ID: id("system", "node1"), Op: redfish.OpReset, NotSent: func() { b.srv.SetPowerState("PoweringOn") }},
				{ID: id("system", "node1"), Op: redfish.OpForceRestart, NotSent: unreachable},
				{ID: id("system", "node1"), Op: redfish.OpForceOff, NotSent: unreachable},
				{ID: id("firmware", "node1/BMC"), Op: redfish.OpStage, Args: core.Fields{"image_uri": imageURI},
					NotSent: unreachable, Failed: func() { b.srv.SetImageRejected(true) }},
			},
			Refuses: []contract.Refuse{{ID: id("system", "node1")}, {ID: id("virtual-media", "node1/cd")}},
		}
	})
}

// 現実を見ずに決まる誤りは読み込み時にエラーにする（E-I7・E-M3）。
func TestValidate(t *testing.T) {
	b := newBMC(t, redfishtest.Options{})
	cases := []struct {
		r    core.Resource
		want string
	}{
		{core.Resource{ID: id("system", "node9")}, "settings.nodes に無い"},
		{core.Resource{ID: id("system", "node1"), Fields: core.Fields{"power": "ForceRestart"}}, "一回きりの操作"},
		{core.Resource{ID: id("system", "node1"), Fields: core.Fields{"boot_enabled": "Once", "boot_target": "Cd"}}, "boot_once"},
		{core.Resource{ID: id("system", "node1"), Fields: core.Fields{"boot_enabled": "Continuous"}}, "boot_target を書く"},
		{core.Resource{ID: id("system", "node1"), Fields: core.Fields{"boot_target": "Pxe"}}, "Continuous と書く"},
		{core.Resource{ID: id("system", "node1"), Fields: core.Fields{"uuid": "x"}}, "観測だけ"},
		{core.Resource{ID: id("virtual-media", "node1/usb")}, "<node>/cd"},
		{core.Resource{ID: id("virtual-media", "node1/cd"), Fields: core.Fields{"image": "os.iso"}}, "URL"},
		{core.Resource{ID: id("firmware", "node1/BMC"), Fields: core.Fields{"update_state": "Completed"}}, "観測だけ"},
		{core.Resource{ID: id("bios", "node1")}, "<node>/<名前>"},
		{core.Resource{ID: id("system", "node1"), Ensure: core.EnsureAbsent}, "消せない"},
	}
	for _, c := range cases {
		d := b.p.Validate([]core.Resource{c.r}, nil)
		if len(d) == 0 || !strings.Contains(d[0].Message, c.want) {
			t.Errorf("%s %v: %v（want %q）", c.r.ID, c.r.Fields, d, c.want)
		}
	}
	if d := b.p.Validate(nil, []core.Range{{Provider: "bmc"}}); len(d) != 1 {
		t.Errorf("Range を拒否しない: %v", d)
	}
}

// Off へは GracefulShutdown だけを送り、応えない台に ForceOff を送らない。強制停止は
// 破壊的な操作 force_off（E-A1・N-A1）。
func TestPowerOffIsGraceful(t *testing.T) {
	b := newBMC(t, redfishtest.Options{GracefulTimeout: time.Hour})
	b.srv.SetIgnoreGraceful(true)
	sys := id("system", "node1")
	if err := b.p.Apply(context.Background(), wctx(), sys, core.Fields{"power": "Off"}, b.observe(t, sys)[sys]); err != nil {
		t.Fatal(err)
	}
	if o := b.observe(t, sys)[sys]; o.Availability != core.Converging {
		t.Fatalf("切り替えの途中が %+v（want converging）", o)
	}
	for _, r := range b.srv.Requests() {
		if strings.Contains(r.Body, "ForceOff") {
			t.Fatalf("ForceOff を送った: %+v", r)
		}
	}
	if spec := b.p.Ops("system")[redfish.OpForceOff]; !spec.Destructive {
		t.Fatal("force_off が破壊的な操作でない")
	}
	if err := b.p.Invoke(context.Background(), wctx(), sys, redfish.OpForceOff, nil); err != nil {
		t.Fatal(err)
	}
	if b.srv.PowerState() != "Off" {
		t.Fatalf("force_off のあと %s", b.srv.PowerState())
	}
}

// 観測は観測用の credential だけを使い、書き込み用のパスワードのファイルが無くても観測
// できる。書き込みはそのファイルを読む（E-G5・N-G3）。
func TestWriteCredentials(t *testing.T) {
	b := newBMC(t, redfishtest.Options{PowerState: "Off"})
	os.Remove(filepath.Join(b.dir, "rw"))
	sys := id("system", "node1")
	o := b.observe(t, sys, id("firmware", "node1/BMC"), id("bios", "node1/ProcVirtualization"), id("virtual-media", "node1/cd"))
	for k, v := range o {
		if v.Availability != core.Present {
			t.Fatalf("%s: %+v", k, v)
		}
	}
	if c := b.srv.UserCalls("admin"); len(c) != 0 {
		t.Fatalf("観測で書き込み用のユーザーを使った: %v", c)
	}
	err := b.p.Apply(context.Background(), wctx(), sys, core.Fields{"power": "On"}, o[sys])
	if err == nil || !strings.Contains(err.Error(), "パスワードのファイル") {
		t.Fatalf("書き込み用の credential が無いのに %v", err)
	}
	if b.srv.PowerState() != "Off" {
		t.Fatal("credential が無いのに書いた")
	}
}

// BIOS の属性は Settings に 1 度だけ PATCH し、保留中は Converging で重ねて送らない。再起動
// で効く。registry に合わない値は送らない（E-A4・N-A3）。
func TestBios(t *testing.T) {
	b := newBMC(t, redfishtest.Options{})
	ctx := context.Background()
	virt := id("bios", "node1/ProcVirtualization")
	if o := b.observe(t, virt)[virt]; o.Availability != core.Present || o.Fields["value"] != "Enabled" {
		t.Fatalf("%+v", o)
	}
	if err := b.p.Apply(ctx, wctx(), virt, core.Fields{"value": "Disabled"}, core.Observation{}); err != nil {
		t.Fatal(err)
	}
	if o := b.observe(t, virt)[virt]; o.Availability != core.Converging {
		t.Fatalf("保留中が %+v", o)
	}
	n := b.writes()
	if err := b.p.Apply(ctx, wctx(), virt, core.Fields{"value": "Disabled"}, core.Observation{}); err != nil || b.writes() != n {
		t.Fatalf("保留中に重ねて送った: %v %d", err, b.writes()-n)
	}
	b.srv.Restart()
	if o := b.observe(t, virt)[virt]; o.Availability != core.Present || o.Fields["value"] != "Disabled" {
		t.Fatalf("再起動のあと %+v", o)
	}
	retry := id("bios", "node1/BootRetryCount")
	if o := b.observe(t, retry)[retry]; o.Fields["value"] != "3" {
		t.Fatalf("数値の属性 %+v", o)
	}
	if err := b.p.Apply(ctx, wctx(), retry, core.Fields{"value": "5"}, core.Observation{}); err != nil {
		t.Fatal(err)
	}
	if got := b.srv.BiosPending()["BootRetryCount"]; got != float64(5) {
		t.Fatalf("数値で送らない: %#v", got)
	}
	b.srv.Restart()
	n = b.writes()
	for _, c := range []struct{ attr, value, want string }{
		{"BootMode", "Bios", "読み取り専用"},
		{"PowerProfile", "Turbo", "選べる値"},
		{"BootRetryCount", "99", "上限"},
		{"SriovGlobalEnable", "yes", "真偽"},
	} {
		err := b.p.Apply(ctx, wctx(), id("bios", "node1/"+c.attr), core.Fields{"value": c.value}, core.Observation{})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s=%s: %v（want %q）", c.attr, c.value, err, c.want)
		}
	}
	if b.writes() != n {
		t.Fatal("registry に合わない値を送った")
	}
	if o := b.observe(t, id("bios", "node1/X"))[id("bios", "node1/X")]; o.Availability != core.Absent {
		t.Fatalf("%+v", o)
	}
}

// FW のバージョンと最新の更新の Task を観測し、stage で更新を 1 回送る（E-A5・N-A4）。
// Payload を返さない BMC でも TargetDetermined で Task を見分ける。
func TestFirmwareStage(t *testing.T) {
	for _, noPayload := range []bool{false, true} {
		b := newBMC(t, redfishtest.Options{UpdateOptions: redfishtest.UpdateOptions{NoTaskPayload: noPayload, UpdateDuration: time.Hour}})
		b.srv.AddImage(redfishtest.Image{URI: imageURI, Version: "2.0.0"})
		fw := id("firmware", "node1/BMC")
		o := b.observe(t, fw)[fw]
		if o.Availability != core.Present || o.Fields["version"] != "1.0.0" || o.Fields["update_state"] != "" || o.Fields["updateable"] != true {
			t.Fatalf("%+v", o)
		}
		if err := b.p.Invoke(context.Background(), wctx(), fw, redfish.OpStage, core.Fields{"image_uri": imageURI}); err != nil {
			t.Fatal(err)
		}
		if b.srv.UpdateRequests() != 1 {
			t.Fatalf("更新の要求 %d 回", b.srv.UpdateRequests())
		}
		o = b.observe(t, fw)[fw]
		if o.Fields["update_state"] != "Running" || o.Fields["update_image"] != imageURI {
			t.Fatalf("payload なし=%v: %+v", noPayload, o)
		}
		if o := b.observe(t, id("firmware", "node1/BIOS"))[id("firmware", "node1/BIOS")]; o.Fields["update_state"] != "" {
			t.Fatalf("ほかの部品の Task を読んだ: %+v", o)
		}
	}
	b := newBMC(t, redfishtest.Options{})
	b.srv.AddImage(redfishtest.Image{URI: imageURI, Version: "2.0.0"})
	fw := id("firmware", "node1/BMC")
	if err := b.p.Invoke(context.Background(), wctx(), fw, redfish.OpStage, core.Fields{"image_uri": imageURI}); err != nil {
		t.Fatal(err)
	}
	if o := b.observe(t, fw)[fw]; o.Fields["version"] != "2.0.0" || o.Fields["update_state"] != "Completed" || o.Fields["update_percent"] != int64(100) {
		t.Fatalf("%+v", o)
	}
	// 更新できない部品には送らない（ErrNotSent でもない：直さないと通らない）。
	err := b.p.Invoke(context.Background(), wctx(), id("firmware", "node1/CPLD"), redfish.OpStage, core.Fields{"image_uri": imageURI})
	if err == nil || errors.Is(err, core.ErrNotSent) || b.srv.UpdateRequests() != 1 {
		t.Fatalf("%v %d", err, b.srv.UpdateRequests())
	}
}

// verify はイメージを取りに行って sha256 を確かめる。BMC には送らない。secret の
// 参照のパスワードを WriteContext から取り出す（E-A5・E-G2）。
func TestFirmwareVerify(t *testing.T) {
	body := []byte("QBMC-FW BMC 2.0.0\n")
	sum := sha256.Sum256(body)
	img := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "fw" || p != "img-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write(body)
	}))
	defer img.Close()
	b := newBMC(t, redfishtest.Options{})
	fw := id("firmware", "node1/BMC")
	w := core.NewWriteContext(core.SecretBackends{"vault": staticSecret("img-secret")})
	args := core.Fields{"image_uri": img.URL + "/bmc.bin", "sha256": hex.EncodeToString(sum[:]), "image_username": "fw", "image_password": "vault:/fw#password"}
	if err := b.p.Invoke(context.Background(), w, fw, redfish.OpVerify, args); err != nil {
		t.Fatal(err)
	}
	args["sha256"] = strings.Repeat("0", 64)
	if err := b.p.Invoke(context.Background(), w, fw, redfish.OpVerify, args); err == nil || errors.Is(err, core.ErrNotSent) {
		t.Fatalf("sha256 の不一致が %v", err)
	}
	args["sha256"] = hex.EncodeToString(sum[:])
	err := b.p.Invoke(context.Background(), core.NewWriteContext(core.SecretBackends{"vault": staticSecret("wrong")}), fw, redfish.OpVerify, args)
	if !errors.Is(err, core.ErrNotSent) || strings.Contains(err.Error(), "img-secret") {
		t.Fatalf("取りに行けないのに %v", err)
	}
	if b.writes() != 0 {
		t.Fatal("verify が BMC に書いた")
	}
	if err := b.p.Invoke(context.Background(), w, fw, redfish.OpVerify, core.Fields{"image_uri": img.URL, "sha256": args["sha256"], "image_username": "fw", "image_password": "img-secret"}); err == nil || strings.Contains(err.Error(), "img-secret") {
		t.Fatalf("平文のパスワードを拒否しない: %v", err)
	}
}

type staticSecret string

func (s staticSecret) Generation(context.Context, string) (string, error) { return "1", nil }
func (s staticSecret) Resolve(context.Context, string, string) (string, error) {
	return string(s), nil
}

// 未知の PowerState は Unknown（定義に合わない出力、E-B11）。電源の切り替えの
// 途中は Converging（E-A1）。
func TestPowerStates(t *testing.T) {
	b := newBMC(t, redfishtest.Options{})
	sys := id("system", "node1")
	for state, want := range map[string]core.Availability{"Paused": core.Unknown, "PoweringOff": core.Converging, "Off": core.Present} {
		b.srv.SetPowerState(state)
		if o := b.observe(t, sys)[sys]; o.Availability != want {
			t.Errorf("%s: %+v（want %s）", state, o, want)
		}
	}
}

// boot_once は PATCH だけで再起動せず、BMC が持たない起動先は送らない（E-A2）。
func TestBootOnce(t *testing.T) {
	b := newBMC(t, redfishtest.Options{})
	sys := id("system", "node1")
	if err := b.p.Invoke(context.Background(), wctx(), sys, redfish.OpBootOnce, core.Fields{"target": "Cd"}); err != nil {
		t.Fatal(err)
	}
	if e, tg := b.srv.Boot(); e != "Once" || tg != "Cd" || b.srv.Boots() != 0 {
		t.Fatalf("%s %s %d", e, tg, b.srv.Boots())
	}
	err := b.p.Invoke(context.Background(), wctx(), sys, redfish.OpBootOnce, core.Fields{"target": "Floppy"})
	if err == nil || errors.Is(err, core.ErrNotSent) {
		t.Fatalf("%v", err)
	}
	if err := b.p.Invoke(context.Background(), wctx(), sys, redfish.OpReset, core.Fields{"type": "x"}); err == nil {
		t.Fatal("未知の引数を拒否しない")
	}
}
