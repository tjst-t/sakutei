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
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/audit"
	"github.com/tjst-t/sakutei/v2/internal/cause"
	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/redfish"
	"github.com/tjst-t/sakutei/v2/internal/redfish/redfishtest"
	"github.com/tjst-t/sakutei/v2/internal/render"
	"github.com/tjst-t/sakutei/v2/internal/task"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
)

const fwImageURI = "https://images.example/bmc-2.0.0.bin"

var fwBMCID = FirmwareID("node1", "BMC")

func fwFields(extra ...string) map[string]string {
	f := map[string]string{FieldName: "node1", FieldVersion: "2.0.0", FieldImageURI: fwImageURI}
	for i := 0; i+1 < len(extra); i += 2 {
		f[extra[i]] = extra[i+1]
	}
	return f
}

func newFirmware(t *testing.T, opts redfishtest.Options, fo FirmwareOptions) (*redfishtest.Server, *FirmwareProvider) {
	t.Helper()
	srv, c := newBMC(t, opts)
	srv.AddImage(redfishtest.Image{URI: fwImageURI, Version: "2.0.0"})
	if fo.PollInterval == 0 {
		fo.PollInterval = time.Millisecond
	}
	return srv, NewFirmware(map[string]redfish.Client{"node1": c}, fo)
}

func TestFirmware_ValidateProvider(t *testing.T) {
	if err := sdk.ValidateProvider(NewFirmware(nil, FirmwareOptions{})); err != nil {
		t.Fatalf("ValidateProvider: %v", err)
	}
	caps := NewFirmware(nil, FirmwareOptions{}).Capabilities()
	if caps.List || caps.BulkObserve || caps.Pruneable || !caps.TrueObserve {
		t.Errorf("Capabilities = %+v, want the standard tier (no List / BulkObserve, not pruneable)", caps)
	}
}

// runSteps は多相 Task の相を Manager を通さずに順に走らせる（承認は済んだもの
// とする）。契約テストの Apply の代わり。
func runSteps(ctx context.Context, p *FirmwareProvider, req task.Request) error {
	steps, err := p.TaskSteps(ctx, req)
	if err != nil {
		return err
	}
	for _, s := range steps {
		st, err := p.CheckStep(ctx, req, s.Name)
		if err != nil {
			return err
		}
		if st == task.StepDone {
			continue
		}
		if !s.Action {
			return &stepErr{s.Name}
		}
		if err := p.RunStep(ctx, req, s.Name); err != nil {
			return err
		}
		if s.Await {
			deadline := time.Now().Add(5 * time.Second)
			for st, _ := p.CheckStep(ctx, req, s.Name); st != task.StepDone; st, _ = p.CheckStep(ctx, req, s.Name) {
				if time.Now().After(deadline) {
					return &stepErr{s.Name}
				}
				time.Sleep(time.Millisecond)
			}
		}
	}
	return nil
}

type stepErr struct{ step string }

func (e *stepErr) Error() string { return "step " + e.step + " not done" }

// taskDriven は契約テスト用のアダプタ: 収束ループの Apply の代わりに多相 Task の
// 相を走らせる（how の宣言は base から）。
type fwTaskDriven struct {
	*FirmwareProvider
	base map[string]string
}

func (d fwTaskDriven) Apply(ctx context.Context, plan core.Plan) error {
	for _, op := range plan.Ops {
		desired := map[string]string{}
		for k, v := range d.base {
			desired[k] = v
		}
		for _, fd := range op.Fields {
			desired[fd.Field] = fd.To
		}
		if err := runSteps(ctx, d.FirmwareProvider, task.Request{TaskID: "contract", Resource: op.Target, Desired: desired, Plan: plan}); err != nil {
			return err
		}
	}
	return nil
}

// 契約テスト（#32）を偽 BMC の上で回す。部品は前からあるので Preexisting。
// [AC-148-1]
func TestFirmware_Contract(t *testing.T) {
	srv, p := newFirmware(t, redfishtest.Options{}, FirmwareOptions{})
	ct := sdk.ContractTester{
		Provider:      fwTaskDriven{FirmwareProvider: p, base: fwFields()},
		Scope:         core.NewScope(nil),
		SampleID:      fwBMCID,
		Preexisting:   true,
		SampleDesired: map[string]string{FieldName: "node1", FieldComponent: "BMC", FieldVersion: "2.0.0"},
		MakeUnreachable: func() (func(), error) {
			srv.SetUnreachable(true)
			return func() { srv.SetUnreachable(false) }, nil
		},
	}
	ct.RunAll(t)
	if n := srv.UpdateRequests(); n != 1 {
		t.Errorf("update requests = %d, want 1 (the second Apply sees the version and does nothing)", n)
	}
}

func fwPlan(t *testing.T, p *FirmwareProvider, id string, fields map[string]string) (core.Plan, error) {
	t.Helper()
	obs, err := p.Read(context.Background(), id)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return diff.ForProvider(p).Plan(obs, diff.DesiredResource{
		ID: core.NewID(FirmwareName, core.NewScope(nil), id), Fields: fields,
	}, cause.CauseContext{})
}

// 版が合えば何もしない（前後の空白は比べない）。違えば版だけの破壊的な Op。
// [AC-148-2]
func TestFirmware_PlanComparesVersionOnly(t *testing.T) {
	srv, p := newFirmware(t, redfishtest.Options{}, FirmwareOptions{})
	pl, err := fwPlan(t, p, fwBMCID, fwFields(FieldVersion, " 1.0.0 ", FieldSHA256, strings.Repeat("a", 64), FieldReboot, RebootGraceful))
	if err != nil || len(pl.Ops) != 0 {
		t.Fatalf("plan at the declared version = %+v, %v; want no op", pl, err)
	}
	pl, err = fwPlan(t, p, fwBMCID, fwFields(FieldApplyTime, redfish.ApplyOnReset))
	if err != nil || len(pl.Ops) != 1 {
		t.Fatalf("plan = %+v, %v; want one op", pl, err)
	}
	op := pl.Ops[0]
	if !op.Destructive || len(op.Fields) != 1 || op.Fields[0].Field != FieldVersion || op.Fields[0].From != "1.0.0" ||
		!strings.Contains(op.Diagnostic, `"1.0.0" → "2.0.0"`) {
		t.Fatalf("op = %+v, want a destructive op of the version only", op)
	}
	// 大文字と小文字は区別する。
	srv.SetFirmwareVersion("BMC", "2.0.0-RC")
	if pl, _ := fwPlan(t, p, fwBMCID, fwFields(FieldVersion, "2.0.0-rc")); len(pl.Ops) != 1 {
		t.Fatalf("2.0.0-RC vs 2.0.0-rc must differ: %+v", pl)
	}
}

// 計画の時点で断るもの。
// [AC-148-3]
func TestFirmware_PlanErrors(t *testing.T) {
	_, p := newFirmware(t, redfishtest.Options{}, FirmwareOptions{})
	if _, err := fwPlan(t, p, FirmwareID("node1", "CPLD"), fwFields()); err == nil || !strings.Contains(err.Error(), "not updateable") {
		t.Errorf("not updateable: %v", err)
	}
	if _, err := fwPlan(t, p, FirmwareID("node1", "NIC"), fwFields()); err == nil || !strings.Contains(err.Error(), "no component \"NIC\"") {
		t.Errorf("missing component: %v", err)
	}
	if _, err := fwPlan(t, p, fwBMCID, fwFields(FieldApplyTime, "AtMaintenanceWindowStart")); err == nil {
		t.Errorf("unknown apply_time must fail")
	}

	_, nopush := newFirmware(t, redfishtest.Options{UpdateOptions: redfishtest.UpdateOptions{NoMultipartPush: true}}, FirmwareOptions{})
	if _, err := fwPlan(t, nopush, fwBMCID, fwFields(FieldTransfer, TransferMultipart)); err == nil || !strings.Contains(err.Error(), "MultipartHttpPushUri") {
		t.Errorf("multipart without MultipartHttpPushUri: %v", err)
	}
	_, notask := newFirmware(t, redfishtest.Options{UpdateOptions: redfishtest.UpdateOptions{NoTaskService: true}}, FirmwareOptions{})
	if _, err := fwPlan(t, notask, fwBMCID, fwFields()); err == nil || !strings.Contains(err.Error(), "TaskService") {
		t.Errorf("no TaskService: %v", err)
	}
	_, none := newFirmware(t, redfishtest.Options{UpdateOptions: redfishtest.UpdateOptions{NoUpdateService: true}}, FirmwareOptions{})
	obs, err := none.Read(context.Background(), fwBMCID)
	if err != nil || obs.Availability != core.AvailabilityOk {
		t.Fatalf("Read without UpdateService = %+v, %v; want Ok", obs, err)
	}
	if _, err := fwPlan(t, none, fwBMCID, fwFields()); err == nil || !strings.Contains(err.Error(), "no UpdateService") {
		t.Errorf("no UpdateService: %v", err)
	}
}

func TestFirmware_ValidateIntent(t *testing.T) {
	p := NewFirmware(map[string]redfish.Client{"node1": nil}, FirmwareOptions{})
	if err := p.ValidateIntent(fwBMCID, fwFields()); err != nil {
		t.Fatalf("valid intent: %v", err)
	}
	for _, tc := range []struct {
		id     string
		fields map[string]string
		want   string
	}{
		{"node1/BMC", fwFields(), "want \"<node>/firmware/<component Id>\""},
		{FirmwareID("node2", "BMC"), map[string]string{FieldName: "node2", FieldVersion: "1", FieldImageURI: fwImageURI}, "no BMC is configured"},
		{fwBMCID, fwFields(FieldComponent, "BIOS"), "component \"BIOS\" must equal"},
		{fwBMCID, fwFields(FieldVersion, " "), "version is required"},
		{fwBMCID, fwFields(FieldImageURI, ""), "image_uri is required"},
		{fwBMCID, fwFields(FieldImageURI, "https://fw:pw@images.example/x.bin"), "must not carry credentials"},
		{fwBMCID, fwFields(FieldImageURI, "file:///srv/x.bin"), "works only with transfer: multipart"},
		{fwBMCID, fwFields(FieldImageURI, "ftp://images.example/x.bin"), "want http or https"},
		{fwBMCID, fwFields(FieldSHA256, "abc"), "64 hexadecimal"},
		{fwBMCID, fwFields(FieldTransfer, "push"), "transfer \"push\""},
		{fwBMCID, fwFields(FieldReboot, "yes"), "reboot \"yes\""},
		{fwBMCID, fwFields(FieldUpdateTimeout, "-1m"), "positive duration"},
		{fwBMCID, fwFields(FieldImageUsername, "fw", FieldImagePassword, "hunter2"), "must be a SecretRef"},
		{fwBMCID, fwFields(FieldImagePassword, "secretref:file/fw/repo"), "needs image_username"},
		{fwBMCID, fwFields(FieldPresent, "true"), "observed only"},
		{fwBMCID, fwFields("color", "red"), "unknown field"},
	} {
		err := p.ValidateIntent(tc.id, tc.fields)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ValidateIntent(%s, %v) = %v, want %q", tc.id, tc.fields, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "hunter2") {
			t.Errorf("error echoes the password: %v", err)
		}
	}
	if err := p.ValidateIntent(fwBMCID, fwFields(FieldTransfer, TransferMultipart, FieldImageURI, "file:///srv/x.bin")); err != nil {
		t.Errorf("file URI with multipart: %v", err)
	}
	if err := p.ValidateUpdatePolicy(fwBMCID, core.UpdatePolicy{}); err == nil {
		t.Errorf("strategy apply must be refused")
	}
	if err := p.ValidateUpdatePolicy(fwBMCID, core.UpdatePolicy{Strategy: core.UpdateStrategyTask, RollbackOnFail: true}); err == nil {
		t.Errorf("rollback_on_fail must be refused")
	}
	if err := p.Apply(context.Background(), core.Plan{Ops: []core.Op{{Target: core.ID{Local: fwBMCID}}}}); err == nil {
		t.Errorf("Apply must be refused")
	}
}

// fwManager は FirmwareProvider を実行体にした task.Manager。
func fwManager(t *testing.T, p *FirmwareProvider) (*task.Manager, task.Store) {
	t.Helper()
	st := task.NewFileStore(filepath.Join(t.TempDir(), "tasks.json"))
	m := task.NewManager(st, p, "me", nil)
	m.PollInterval = time.Millisecond
	return m, st
}

func fwRecord(t *testing.T, st task.Store, phase task.Phase) task.Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec, ok, err := st.Get(context.Background(), core.NewID(FirmwareName, core.NewScope(nil), fwBMCID).String())
		if err != nil {
			t.Fatal(err)
		}
		if ok && rec.Phase == phase {
			return rec
		}
		if time.Now().After(deadline) {
			t.Fatalf("record %+v (present=%v), want %s", rec, ok, phase)
		}
		time.Sleep(time.Millisecond)
	}
}

// 承認ゲートを通ってから流れ、OnReset の保留は宣言した再起動で効いて、版が
// 合って閉じる。イメージの資格は SecretRef から解決し、ログ・記録に出さない。
// [AC-148-4]
func TestFirmware_TaskApprovalStageRebootConfirm(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := filepath.Join(dir, "secrets.json")
	if err := os.WriteFile(store, []byte(`{"secrets": {"fw/repo": {"value": "s3cret", "gen": 1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, c := newBMC(t, redfishtest.Options{})
	srv.AddImage(redfishtest.Image{URI: fwImageURI, Version: "2.0.0", Username: "fw", Password: "s3cret"})
	p := NewFirmware(map[string]redfish.Client{"node1": c}, FirmwareOptions{Secrets: render.NewFileSecretBackend(store), PollInterval: time.Millisecond})
	m, st := fwManager(t, p)
	id := core.NewID(FirmwareName, core.NewScope(nil), fwBMCID)
	desired := fwFields(FieldApplyTime, redfish.ApplyOnReset, FieldReboot, RebootGraceful,
		FieldImageUsername, "fw", FieldImagePassword, "secretref:file/fw/repo")
	pol := core.UpdatePolicy{Strategy: core.UpdateStrategyTask}

	if dec, err := m.Trigger(ctx, id, desired, core.Plan{}, pol); err != nil || dec.Outcome != task.Launched {
		t.Fatalf("trigger: %+v %v", dec, err)
	}
	m.Wait(5 * time.Second)
	rec := fwRecord(t, st, task.PhaseAwaitingApproval)
	if rec.Gate != task.GateApproval || rec.Step != task.StepVerifying || srv.UpdateRequests() != 0 {
		t.Fatalf("before approval: record %+v, update requests %d", rec, srv.UpdateRequests())
	}
	log := audit.NewLog(filepath.Join(dir, "audit.jsonl"))
	if _, err := task.Decide(ctx, st, log, id.String(), task.Approval{Approver: "alice"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if dec, err := m.Trigger(ctx, id, desired, core.Plan{}, pol); err != nil || dec.Outcome != task.Resumed {
		t.Fatalf("after approval: %+v %v", dec, err)
	}
	m.Wait(5 * time.Second)
	rec = fwRecord(t, st, task.PhaseSucceeded)
	if srv.FirmwareVersion("BMC") != "2.0.0" || srv.UpdateRequests() != 1 || srv.Boots() != 1 {
		t.Fatalf("after the task: version %s, update requests %d, boots %d", srv.FirmwareVersion("BMC"), srv.UpdateRequests(), srv.Boots())
	}
	var resets, sent int
	for _, q := range srv.Requests() {
		if q.Path == redfishtest.ResetPath && strings.Contains(q.Body, redfish.ResetGracefulRestart) {
			resets++
		}
		if q.Path == redfishtest.SimpleUpdatePath && strings.Contains(q.Body, `"Password":"s3cret"`) && strings.Contains(q.Body, `"OnReset"`) {
			sent++
		}
	}
	if resets != 1 || sent != 1 {
		t.Fatalf("GracefulRestart sent %d time(s), SimpleUpdate with the resolved password %d time(s); want 1 each", resets, sent)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if strings.Contains(rec.Error, "s3cret") || strings.Contains(string(data), "s3cret") {
		t.Fatalf("the image password leaked: %+v", rec)
	}
}

// BMC の Task が失敗で終われば halt（もう一度は流さない）。資格の値はエラーに
// 出さない。
// [AC-148-5]
func TestFirmware_TaskFailsOnBMCException(t *testing.T) {
	ctx := context.Background()
	srv, p := newFirmware(t, redfishtest.Options{}, FirmwareOptions{})
	srv.SetUpdateFails(true)
	req := task.Request{TaskID: "t", Resource: core.NewID(FirmwareName, core.NewScope(nil), fwBMCID), Desired: fwFields()}
	err := runSteps(ctx, p, req)
	if err == nil || !strings.Contains(err.Error(), "Exception") || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("runSteps = %v, want the BMC task's Exception", err)
	}
	if srv.UpdateRequests() != 1 {
		t.Fatalf("update requests = %d, want 1", srv.UpdateRequests())
	}
	// 失敗で終わった Task は「もう送った」に数えない（cancel のあと起こし直せば送る）。
	if st, _ := p.CheckStep(ctx, req, task.StepStaging); st != task.StepPending {
		t.Fatalf("staging after a failed BMC task = %s, want pending", st)
	}
}

// sha256 を書けば流す前に確かめ、違えば BMC に送らない。multipart は確かめた
// 中身をそのまま送る。
// [AC-148-6]
func TestFirmware_SHA256AndMultipart(t *testing.T) {
	ctx := context.Background()
	img := []byte("firmware image 2.0.0")
	sum := sha256.Sum256(img)
	good := hex.EncodeToString(sum[:])
	images := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, pw, ok := r.BasicAuth(); !ok || u != "fw" || pw != "s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(img)
	}))
	defer images.Close()
	dir := t.TempDir()
	store := filepath.Join(dir, "secrets.json")
	if err := os.WriteFile(store, []byte(`{"secrets": {"fw/repo": {"value": "s3cret", "gen": 1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, c := newBMC(t, redfishtest.Options{})
	srv.AddImage(redfishtest.Image{Content: img, Version: "2.0.0"})
	srv.AddImage(redfishtest.Image{URI: images.URL + "/bmc.bin", Version: "2.0.0"})
	p := NewFirmware(map[string]redfish.Client{"node1": c}, FirmwareOptions{
		Secrets: render.NewFileSecretBackend(store), PollInterval: time.Millisecond, TempDir: dir,
	})
	id := core.NewID(FirmwareName, core.NewScope(nil), fwBMCID)
	creds := []string{FieldImageUsername, "fw", FieldImagePassword, "secretref:file/fw/repo"}

	bad := task.Request{TaskID: "t", Resource: id, Desired: fwFields(append(creds, FieldImageURI, images.URL+"/bmc.bin", FieldSHA256, strings.Repeat("0", 64))...)}
	err := runSteps(ctx, p, bad)
	if err == nil || !strings.Contains(err.Error(), "verifying") || !strings.Contains(err.Error(), "want "+strings.Repeat("0", 64)) || srv.UpdateRequests() != 0 {
		t.Fatalf("bad sha256: %v (update requests %d)", err, srv.UpdateRequests())
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("error echoes the password: %v", err)
	}

	push := task.Request{TaskID: "t", Resource: id, Desired: fwFields(append(creds, FieldImageURI, images.URL+"/bmc.bin", FieldSHA256, good, FieldTransfer, TransferMultipart)...)}
	if err := runSteps(ctx, p, push); err != nil {
		t.Fatalf("multipart: %v", err)
	}
	if srv.FirmwareVersion("BMC") != "2.0.0" || srv.UpdateRequests() != 1 {
		t.Fatalf("multipart: version %s, update requests %d", srv.FirmwareVersion("BMC"), srv.UpdateRequests())
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "sakutei-firmware-*")); len(left) != 0 {
		t.Fatalf("temporary image left behind: %v", left)
	}
}

// BMC に届かない間（BMC 自身の更新で再起動している）は unknown で、二重に送らない。
func TestFirmware_CheckStepUnknownWhileUnreachable(t *testing.T) {
	ctx := context.Background()
	srv, p := newFirmware(t, redfishtest.Options{}, FirmwareOptions{})
	req := task.Request{TaskID: "t", Resource: core.NewID(FirmwareName, core.NewScope(nil), fwBMCID), Desired: fwFields()}
	srv.SetUnreachable(true)
	for _, s := range []string{task.StepVerifying, task.StepStaging, task.StepApplying, task.StepRebooting, task.StepConfirming} {
		if st, _ := p.CheckStep(ctx, req, s); st != task.StepUnknown {
			t.Errorf("CheckStep(%s) while unreachable = %s, want unknown", s, st)
		}
	}
	srv.SetUnreachable(false)
	if err := p.RunStep(ctx, req, task.StepStaging); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{task.StepVerifying, task.StepStaging, task.StepApplying} {
		if st, _ := p.CheckStep(ctx, req, s); st != task.StepDone {
			t.Errorf("CheckStep(%s) after staging = %s, want done", s, st)
		}
	}
}

// 前の更新の残りかす（同じイメージで前に済んだ BMC の Task）を「もう送った」と
// 数えない。verifying は版が合うときだけ済み。StartTime は時刻として比べる。
// [AC-148-12]
func TestFirmware_IgnoresLeftoverBMCTasks(t *testing.T) {
	ctx := context.Background()
	clk := newClock()
	srv, p := newFirmware(t, redfishtest.Options{Now: clk.Now}, FirmwareOptions{})
	id := core.NewID(FirmwareName, core.NewScope(nil), fwBMCID)
	// 前の更新: 2.0.0 に上げて済んだ。そのあと人が 1.0.0 に戻した。
	if err := p.RunStep(ctx, task.Request{Resource: id, Desired: fwFields()}, task.StepStaging); err != nil {
		t.Fatal(err)
	}
	srv.SetFirmwareVersion("BMC", "1.0.0")
	clk.Advance(time.Hour)

	req := task.Request{Resource: id, Desired: fwFields(), StartedAt: clk.Now()}
	for _, s := range []string{task.StepVerifying, task.StepStaging, task.StepApplying} {
		if st, _ := p.CheckStep(ctx, req, s); st != task.StepPending {
			t.Errorf("CheckStep(%s) with only a leftover task = %s, want pending", s, st)
		}
	}
	// 起動の時刻が分からない（下限なし）なら残りかすも数えるが、verifying は
	// それでも済みにしない（承認を飛ばさない）。
	old := task.Request{Resource: id, Desired: fwFields()}
	if st, _ := p.CheckStep(ctx, old, task.StepVerifying); st != task.StepPending {
		t.Errorf("verifying with a leftover task = %s, want pending", st)
	}
	if st, _ := p.CheckStep(ctx, old, task.StepStaging); st != task.StepDone {
		t.Errorf("staging with no lower bound = %s, want done", st)
	}

	// 時計のずれの許し（既定 5 分）の内の Task は数える。
	if err := p.RunStep(ctx, req, task.StepStaging); err != nil {
		t.Fatal(err)
	}
	srv.SetFirmwareVersion("BMC", "1.0.0")
	late := task.Request{Resource: id, Desired: fwFields(), StartedAt: clk.Now().Add(4 * time.Minute)}
	if st, _ := p.CheckStep(ctx, late, task.StepStaging); st != task.StepDone {
		t.Errorf("staging with a task 4m before the launch = %s, want done", st)
	}
}

func TestFirmware_MatchTaskTimes(t *testing.T) {
	u := redfish.UpdateService{Path: redfishtest.UpdateServicePath, SimpleUpdateTarget: redfishtest.SimpleUpdatePath}
	comp := redfishtest.FirmwareInventoryPath + "/BMC"
	body := map[string]any{"ImageURI": fwImageURI, "Targets": []any{comp}}
	mk := func(id, start string) redfish.Task {
		return redfish.Task{Path: id, StartTime: start, TargetURI: redfishtest.SimpleUpdatePath, Body: body}
	}
	s := fwSpec{transfer: TransferSimpleUpdate, imageURI: fwImageURI}
	since := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	// 文字列の順と時刻の順が違う（時差の書き方）: 時刻で新しい方を選ぶ。
	tasks := []redfish.Task{
		mk("a", "2026-10-28T21:30:00+09:00"), // 12:30Z
		mk("b", "2026-10-28T12:10:00Z"),
		mk("c", "2026-10-28T11:00:00Z"), // since より前: 数えない
	}
	if got := matchTask(tasks, s, u, comp, since); got == nil || got.Path != "a" {
		t.Errorf("matchTask = %+v, want a (the latest by time)", got)
	}
	if got := matchTask(tasks[2:], s, u, comp, since); got != nil {
		t.Errorf("a task before the launch matched: %+v", got)
	}
	// 読めない StartTime は数える（二重に送る側に倒さない）。
	if got := matchTask([]redfish.Task{mk("d", "yesterday")}, s, u, comp, since); got == nil {
		t.Errorf("a task with an unreadable StartTime must count")
	}
}

// 再起動を宣言したときだけ rebooting は危険区間（Reset の最中に落ちたら人手ゲート）。
func TestFirmware_RebootingIsCriticalOnlyWithReboot(t *testing.T) {
	p := NewFirmware(nil, FirmwareOptions{})
	for reboot, want := range map[string]bool{RebootNone: false, RebootGraceful: true, RebootForce: true} {
		steps, err := p.TaskSteps(context.Background(), task.Request{Resource: core.ID{Local: fwBMCID}, Desired: fwFields(FieldReboot, reboot)})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range steps {
			if s.Name == task.StepRebooting && (s.Critical != want || !s.Await) {
				t.Errorf("reboot %s: rebooting = %+v, want Critical %v and Await", reboot, s, want)
			}
			if s.Name == task.StepApplying && !s.Critical {
				t.Errorf("applying must be critical")
			}
		}
	}
}

// Payload を返さない BMC（qemu-bmc 0.11.0）では、Update の登録簿の
// TargetDetermined で自分の送った Task を見分ける。SimpleUpdate も multipart も
// 1 回だけ送って版が合い、同じ部品の別のイメージの Task は数えない（#168）。
func TestFirmware_TaskWithoutPayload(t *testing.T) {
	ctx := context.Background()
	img := []byte("QBMC-FW BMC 3.0.0\n")
	images := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(img) }))
	defer images.Close()
	srv, p := newFirmware(t, redfishtest.Options{UpdateOptions: redfishtest.UpdateOptions{NoTaskPayload: true}}, FirmwareOptions{TempDir: t.TempDir()})
	srv.AddImage(redfishtest.Image{Content: img, Version: "3.0.0"})
	id := core.NewID(FirmwareName, core.NewScope(nil), fwBMCID)

	simple := task.Request{TaskID: "t", Resource: id, Desired: fwFields()}
	if err := runSteps(ctx, p, simple); err != nil {
		t.Fatalf("simple_update without Payload: %v", err)
	}
	if srv.FirmwareVersion("BMC") != "2.0.0" || srv.UpdateRequests() != 1 {
		t.Fatalf("simple_update: version %s, update requests %d", srv.FirmwareVersion("BMC"), srv.UpdateRequests())
	}

	push := task.Request{TaskID: "t", Resource: id, Desired: fwFields(FieldVersion, "3.0.0", FieldImageURI, images.URL+"/bmc-3.0.0.bin", FieldTransfer, TransferMultipart)}
	// 前の SimpleUpdate の Task（別のイメージ）は、この送信の Task に数えない。
	if st, _ := p.CheckStep(ctx, push, task.StepStaging); st != task.StepPending {
		t.Fatalf("staging with only another image's task = %s, want pending", st)
	}
	if err := runSteps(ctx, p, push); err != nil {
		t.Fatalf("multipart without Payload: %v", err)
	}
	if srv.FirmwareVersion("BMC") != "3.0.0" || srv.UpdateRequests() != 2 {
		t.Fatalf("multipart: version %s, update requests %d", srv.FirmwareVersion("BMC"), srv.UpdateRequests())
	}
}

// BMC がイメージを断れば（400 Update.1.0.VerificationFailed、Task は作られない。
// qemu-bmc の firmware-image-rejected）、staging は理由つきで失敗し、版は変わらず、
// 二度目は送らない。BMC の Task が ApplyFailed で終われば、その理由を出す（#168）。
func TestFirmware_ImageRejectedAndApplyFailed(t *testing.T) {
	ctx := context.Background()
	srv, p := newFirmware(t, redfishtest.Options{UpdateOptions: redfishtest.UpdateOptions{NoTaskPayload: true}}, FirmwareOptions{})
	id := core.NewID(FirmwareName, core.NewScope(nil), fwBMCID)
	req := task.Request{TaskID: "t", Resource: id, Desired: fwFields()}

	srv.SetImageRejected(true)
	err := runSteps(ctx, p, req)
	if err == nil || !strings.Contains(err.Error(), "staging") || !strings.Contains(err.Error(), "Verification of image") {
		t.Fatalf("rejected image: %v", err)
	}
	if srv.UpdateRequests() != 0 || srv.FirmwareVersion("BMC") != "1.0.0" {
		t.Fatalf("rejected image: update requests %d, version %s", srv.UpdateRequests(), srv.FirmwareVersion("BMC"))
	}
	n := 0
	for _, q := range srv.Requests() {
		if q.Path == redfishtest.SimpleUpdatePath {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("SimpleUpdate sent %d time(s), want 1", n)
	}

	srv.SetImageRejected(false)
	srv.SetUpdateFails(true)
	err = runSteps(ctx, p, req)
	if err == nil || !strings.Contains(err.Error(), "applying") || !strings.Contains(err.Error(), "(Update.1.0.ApplyFailed)") {
		t.Fatalf("apply failed: %v", err)
	}
	if srv.FirmwareVersion("BMC") != "1.0.0" {
		t.Fatalf("apply failed: version %s", srv.FirmwareVersion("BMC"))
	}
}
