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

// clidevice_test.go — #159: CLI でしか設定できない装置を、機種の定義のファイル
// だけで観測し変える汎用の provider（cli-device）。偽の装置（clifake。プロンプト・
// ページ送り・設定の入り方を持つ架空の CLI）を相手に、デーモンの経路（RunCycle）で
// 確かめる。
//
//	[AC-159-1]  定義のファイルだけで架空の装置の項目（正規表現・キーと値・表の読み方）を
//	            観測→適合し、何周しても変えない。手で変える→戻る（cause B）。
//	[AC-159-2]  想定外の出力（読み方が当たらない）は unknown：何も Apply しない
//	            （設定のコマンドを 1 つも送らない）。届かないも同じ。戻れば収束する。
//	[AC-159-3]  エラーの印で止まり、どのコマンドまで流れたかを知らせ（Event）に出す。
//	            取り消しの無い装置は、流れたところを観測し直して次の周で直す（level）。
//	[AC-159-4]  確定・取り消しを持つ定義では、失敗したら取り消して確定しない。
//	[AC-159-5]  差し込みになる値（形に合わない・改行）は読み込みで断り、周を回さない。
//	[AC-159-6]  秘密（SecretRef）の値はログ・Event・plan に出ない。disruptive の項目は
//	            破壊的として予算に数える。
//	[AC-159-7]  ソフトの更新の手順：Workflow の action（定義の actions）で 1 台ずつ流し、
//	            承認ゲートを通ってから次の装置へ進む。
//	[AC-159-8]  registry と本物の CLI（plan・reconcile）：remote_config の ssh（偽の ssh と
//	            exec 型の偽の装置）で、定義のファイルだけで観測・変更できる。
package acceptance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/audit"
	"github.com/tjst-t/sakutei/v2/internal/budget"
	"github.com/tjst-t/sakutei/v2/internal/clidevice"
	"github.com/tjst-t/sakutei/v2/internal/clidevice/clifake"
	"github.com/tjst-t/sakutei/v2/internal/eventlog"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/internal/remote"
	"github.com/tjst-t/sakutei/v2/internal/workflow"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
	"github.com/tjst-t/sakutei/v2/pkg/registry"
)

const cdExampleDef = "../../internal/clidevice/testdata/example-device.yaml"

const cdCommunity = "S3cretCommunity42"

type cdSecrets map[string]string

func (s cdSecrets) ResolveValue(_ context.Context, _, path string, _ int64) (string, error) {
	v, ok := s[path]
	if !ok {
		return "", fmt.Errorf("no such secret %s", path)
	}
	return v, nil
}

type cdRig struct {
	t         *testing.T
	dir       string
	devs      map[string]*clifake.Device
	def       *clidevice.Definition
	p         *clidevice.Provider
	d         *reconcile.Daemon
	bc        budget.Config
	clk       *rfClock
	storePath string
	auditPath string
	out       *syncBuffer
	errOut    *syncBuffer
	rec       *recordingSink
}

// newCDRig は devices の偽の装置と、defPath の機種の定義の provider を相手にする
// デーモンを立てる。
func newCDRig(t *testing.T, defPath string, bc budget.Config, devices ...*clifake.Device) *cdRig {
	t.Helper()
	root := t.TempDir()
	r := &cdRig{t: t, dir: filepath.Join(root, "intent"), devs: map[string]*clifake.Device{}, bc: bc,
		clk:       &rfClock{t: time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)},
		storePath: filepath.Join(root, "workflows.json"), auditPath: filepath.Join(root, "audit.jsonl")}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	def, err := clidevice.LoadDefinition(defPath)
	if err != nil {
		t.Fatal(err)
	}
	r.def = def
	hosts := map[string]clidevice.Host{}
	for _, d := range devices {
		r.devs[d.Name] = d
		hosts[d.Name] = clidevice.Host{Conn: d, Def: def}
	}
	if r.p, err = clidevice.New(hosts, clidevice.Options{Secrets: cdSecrets{"snmp/community": cdCommunity}}); err != nil {
		t.Fatal(err)
	}
	r.restart()
	return r
}

func (r *cdRig) restart() {
	r.out, r.errOut, r.rec = &syncBuffer{}, &syncBuffer{}, &recordingSink{}
	r.d = reconcile.NewDaemon(reconcile.Config{
		IntentDir:    r.dir,
		GeneratedDir: filepath.Join(filepath.Dir(r.dir), "generated"),
		Providers: func(name string) (provider.Provider, error) {
			if name == clidevice.Name {
				return r.p, nil
			}
			return nil, os.ErrNotExist
		},
		Out:           r.out,
		ErrOut:        r.errOut,
		EventSinks:    []eventlog.Sink{r.rec},
		Now:           r.clk.Now,
		WorkflowStore: workflow.NewFileStore(r.storePath),
		Audit:         audit.NewLog(r.auditPath),
		BudgetConfig:  r.bc,
	})
}

// intent は resources（id → fields）と workflows（JSON）を書く。
func (r *cdRig) intent(resources map[string]map[string]string, workflows ...string) {
	r.t.Helper()
	var res []string
	for _, id := range sortedKeysOf(resources) {
		f := ""
		for _, k := range sortedKeysOf(resources[id]) {
			if f != "" {
				f += ", "
			}
			f += fmt.Sprintf("%q: %q", k, resources[id][k])
		}
		res = append(res, fmt.Sprintf(`{"provider": %q, "id": %q, "fields": {%s}}`, clidevice.Name, id, f))
	}
	body := `{"resources": [` + strings.Join(res, ",\n") + `], "workflows": [` + strings.Join(workflows, ",") + `]}`
	if err := os.WriteFile(filepath.Join(r.dir, "intent.json"), []byte(body), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func sortedKeysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func (r *cdRig) cycleErr() (reconcile.CycleResult, error) {
	res, err := r.d.RunCycle(context.Background())
	r.d.Wait(5 * time.Second)
	r.d.EndCycle(res, err)
	return res, err
}

// cycle は 1 周回し、Apply（非同期）を待つ。changes は Apply した Op の数。
func (r *cdRig) cycle() (reconcile.CycleResult, int) {
	r.t.Helper()
	res, err := r.cycleErr()
	if err != nil {
		r.t.Fatalf("RunCycle: %v\nerrOut:\n%s", err, r.errOut.String())
	}
	return res, res.Applied + res.Launched
}

func (r *cdRig) settle(n int) {
	r.t.Helper()
	for i := 0; i < n; i++ {
		if _, c := r.cycle(); c == 0 {
			return
		}
	}
	r.t.Fatalf("not converged in %d cycles\n%s", n, r.errOut.String())
}

func (r *cdRig) events() string {
	r.rec.mu.Lock()
	defer r.rec.mu.Unlock()
	return fmt.Sprintf("%+v", r.rec.evs)
}

// changes は装置が受けた設定のコマンド（show・terminal・exit を除く）を返して記録を消す。
func changes(d *clifake.Device) []string {
	var out []string
	for _, l := range d.Log() {
		switch {
		case strings.HasPrefix(l, "show "), l == "terminal length 0", l == "exit", l == "end":
		default:
			out = append(out, l)
		}
	}
	d.ResetLog()
	return out
}

var cdWant = map[string]map[string]string{
	"sw1/syslog":   {"host": "192.0.2.53", "port": "514"},
	"sw1/ntp":      {"server": "192.0.2.123"},
	"sw1/hostname": {"name": "core-sw1"},
}

func TestCLIDevice_ConvergesAndDriftComesBack(t *testing.T) {
	// [AC-159-1]
	dev := clifake.New("sw1")
	dev.PageLines = 5
	r := newCDRig(t, cdExampleDef, budget.Config{}, dev)
	r.intent(cdWant)
	r.settle(5)
	if dev.Get("logging") != "192.0.2.53 514" || dev.Get("hostname") != "core-sw1" || dev.Get("ntp") != "192.0.2.123" {
		t.Fatalf("[AC-159-1] device = logging %q hostname %q ntp %q\n%s", dev.Get("logging"), dev.Get("hostname"), dev.Get("ntp"), r.errOut.String())
	}
	changes(dev)
	for i := 0; i < 3; i++ {
		if res, c := r.cycle(); c != 0 || res.Observed != 3 {
			t.Fatalf("[AC-159-1] cycle %d: %+v\n%s", i, res, r.errOut.String())
		}
	}
	if c := changes(dev); len(c) != 0 {
		t.Fatalf("[AC-159-1] the converged device was changed: %v", c)
	}
	// 手で変える → 戻る。
	dev.Set("logging", "198.51.100.9 514")
	dev.Set("ntp", "198.51.100.1")
	r.settle(3)
	if dev.Get("logging") != "192.0.2.53 514" || dev.Get("ntp") != "192.0.2.123" {
		t.Fatalf("[AC-159-1] drift not repaired: %q %q\n%s", dev.Get("logging"), dev.Get("ntp"), r.errOut.String())
	}
	if c := changes(dev); strings.Join(c, "|") != "configure|logging host 192.0.2.53 port 514|configure|ntp server 192.0.2.123" &&
		strings.Join(c, "|") != "configure|ntp server 192.0.2.123|configure|logging host 192.0.2.53 port 514" {
		t.Fatalf("[AC-159-1] commands = %v", c)
	}
	if !strings.Contains(r.errOut.String()+r.out.String(), "[cause: B]") {
		t.Errorf("[AC-159-1] the hand edit should be drift (cause B)\n%s", r.errOut.String())
	}
}

func TestCLIDevice_UnexpectedOutputDoesNothing(t *testing.T) {
	// [AC-159-2]
	dev := clifake.New("sw1")
	r := newCDRig(t, cdExampleDef, budget.Config{}, dev)
	r.intent(cdWant)
	dev.Garble(true)
	for i := 0; i < 3; i++ {
		if _, c := r.cycle(); c != 0 {
			t.Fatalf("[AC-159-2] applied %d while the output did not match the definition\n%s", c, r.errOut.String())
		}
	}
	if c := changes(dev); len(c) != 0 {
		t.Fatalf("[AC-159-2] commands were sent while the output was unexpected: %v", c)
	}
	if !strings.Contains(r.errOut.String()+r.out.String(), "does not match the device definition") {
		t.Errorf("[AC-159-2] the cycle does not say why\n%s", r.errOut.String())
	}
	dev.Garble(false)
	dev.SetDown(remote.Unreachable)
	for i := 0; i < 2; i++ {
		if _, c := r.cycle(); c != 0 {
			t.Fatalf("[AC-159-2] applied %d to an unreachable device", c)
		}
	}
	dev.Up()
	r.settle(5)
	if dev.Get("logging") != "192.0.2.53 514" {
		t.Fatalf("[AC-159-2] did not converge after the device came back\n%s", r.errOut.String())
	}
}

// writeDef は例の定義を edit で書き換えたファイルを書く。
func writeDef(t *testing.T, edit func(string) string) string {
	t.Helper()
	b, err := os.ReadFile(cdExampleDef)
	if err != nil {
		t.Fatal(err)
	}
	s := edit(string(b))
	path := filepath.Join(t.TempDir(), "device.yaml")
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// twoStepSyslog は syslog を「消してから書く」2 つのコマンドにする。
func twoStepSyslog(s string) string {
	return strings.Replace(s, "    set:\n      - logging host {host} port {port}\n",
		"    set:\n      - no logging host\n      - logging host {host} port {port}\n", 1)
}

func TestCLIDevice_StopsAtTheErrorMarker(t *testing.T) {
	// [AC-159-3]
	dev := clifake.New("sw1")
	dev.Set("logging", "198.51.100.9 514")
	r := newCDRig(t, writeDef(t, twoStepSyslog), budget.Config{}, dev)
	r.intent(map[string]map[string]string{"sw1/syslog": {"host": "192.0.2.53", "port": "514"}})
	dev.FailOn("logging host")
	r.cycle()
	all := r.errOut.String() + r.out.String() + r.events()
	for _, want := range []string{"command 2 of 2 failed", `ran 1 before it: \"no logging host\"`, "% Error: rejected by the device"} {
		if !strings.Contains(all, want) && !strings.Contains(all, strings.ReplaceAll(want, `\"`, `"`)) {
			t.Fatalf("[AC-159-3] the report lacks %q:\n%s", want, all)
		}
	}
	if !strings.Contains(r.events(), "command 2 of 2 failed") {
		t.Fatalf("[AC-159-3] no Event says how far it ran:\n%s", r.events())
	}
	// 取り消しの無い装置：流れたところ（消した）は残る。次の周は消えた現実から直す。
	if dev.Get("logging") != "" {
		t.Fatalf("[AC-159-3] logging = %q, want the first command's effect to stay", dev.Get("logging"))
	}
	if c := changes(dev); strings.Join(c, "|") != "configure|no logging host|logging host 192.0.2.53 port 514" {
		t.Fatalf("[AC-159-3] it must stop at the failing command: %v", c)
	}
	// backoff（実時間）を待たずに、起こし直して次の周にする。
	dev.FailOn("")
	r.restart()
	r.settle(5)
	if dev.Get("logging") != "192.0.2.53 514" {
		t.Fatalf("[AC-159-3] not repaired after the failure\n%s", r.errOut.String())
	}
}

func TestCLIDevice_RollsBackOnCommitDevice(t *testing.T) {
	// [AC-159-4]
	dev := clifake.New("sw1")
	dev.CommitModel = true
	dev.Set("logging", "198.51.100.9 514")
	path := writeDef(t, func(s string) string {
		s = strings.Replace(s, "  leave:\n    - end\n", "  leave:\n    - end\n  commit:\n    - commit\n  rollback:\n    - abort\n", 1)
		return twoStepSyslog(s)
	})
	r := newCDRig(t, path, budget.Config{}, dev)
	r.intent(map[string]map[string]string{"sw1/syslog": {"host": "192.0.2.53", "port": "514"}})
	dev.FailOn("logging host")
	r.cycle()
	if dev.Get("logging") != "198.51.100.9 514" {
		t.Fatalf("[AC-159-4] logging = %q, want unchanged (rolled back)", dev.Get("logging"))
	}
	c := changes(dev)
	if strings.Join(c, "|") != "configure|no logging host|logging host 192.0.2.53 port 514|abort" {
		t.Fatalf("[AC-159-4] commands = %v, want a rollback and no commit", c)
	}
	if !strings.Contains(r.events(), "rolled back (abort), not committed") {
		t.Fatalf("[AC-159-4] the Event does not say it rolled back:\n%s", r.events())
	}
	dev.FailOn("")
	r.restart()
	r.settle(5)
	if dev.Get("logging") != "192.0.2.53 514" {
		t.Fatalf("[AC-159-4] not committed after the device accepted it\n%s", r.errOut.String())
	}
	if c := changes(dev); !containsStr(c, "commit") {
		t.Fatalf("[AC-159-4] commands = %v, want a commit", c)
	}
}

func TestCLIDevice_InjectionRefusedAtLoad(t *testing.T) {
	// [AC-159-5]
	dev := clifake.New("sw1")
	r := newCDRig(t, cdExampleDef, budget.Config{}, dev)
	for _, bad := range []map[string]string{
		{"host": "192.0.2.53 port 1\nreload", "port": "514"},
		{"host": "192.0.2.53", "port": "514 vrf mgmt"},
	} {
		r.intent(map[string]map[string]string{"sw1/syslog": bad, "sw1/hostname": {"name": "core-sw1"}})
		_, err := r.cycleErr()
		if err == nil || !strings.Contains(err.Error(), "invalid intent; cycle skipped") {
			t.Fatalf("[AC-159-5] %q: err = %v", bad, err)
		}
		if strings.Contains(err.Error(), "reload") || strings.Contains(err.Error(), "vrf mgmt") {
			t.Fatalf("[AC-159-5] the value is echoed: %v", err)
		}
		if len(dev.Log()) != 0 {
			t.Fatalf("[AC-159-5] the device was touched: %v", dev.Log())
		}
	}
}

func TestCLIDevice_SecretAndBudget(t *testing.T) {
	// [AC-159-6]
	dev := clifake.New("sw1")
	r := newCDRig(t, cdExampleDef, budget.Config{MaxDestructivePerCycle: 0, MaxDestructivePerWindow: 1, OnExceed: budget.OnExceedHaltDestructiveOnly}, dev)
	want := map[string]map[string]string{
		"sw1/community": {"community": "secretref:file/snmp/community"},
		"sw1/ntp":       {"server": "192.0.2.1"},
	}
	r.intent(want)
	r.settle(5)
	if dev.Get("community") != cdCommunity || dev.Get("ntp") != "192.0.2.1" {
		t.Fatalf("[AC-159-6] community %q ntp %q\n%s", dev.Get("community"), dev.Get("ntp"), r.errOut.String())
	}
	for i := 0; i < 2; i++ {
		if _, c := r.cycle(); c != 0 {
			t.Fatalf("[AC-159-6] the secret item does not converge: %d\n%s", c, r.errOut.String())
		}
	}
	// ntp は disruptive：窓の予算 1 を使い切ったので、次の変更は止まる。
	want["sw1/ntp"] = map[string]string{"server": "192.0.2.2"}
	r.intent(want)
	var halted bool
	for i := 0; i < 3; i++ {
		res, _ := r.cycle()
		halted = halted || res.BudgetHalted
	}
	if !halted || dev.Get("ntp") != "192.0.2.1" {
		t.Fatalf("[AC-159-6] the disruptive change was not held by the budget: halted %v ntp %q\n%s", halted, dev.Get("ntp"), r.errOut.String())
	}
}

func TestCLIDevice_SecretNeverShown(t *testing.T) {
	// [AC-159-6]
	dev := clifake.New("sw1")
	r := newCDRig(t, cdExampleDef, budget.Config{}, dev)
	r.intent(map[string]map[string]string{"sw1/community": {"community": "secretref:file/snmp/community"}})
	r.settle(5)
	// 失敗しても値は出ない。
	dev.Set("community", "SomebodyElse99")
	dev.FailOn("snmp community")
	r.cycle()
	failOut, failErr, failEvents := r.out.String(), r.errOut.String(), r.events()
	dev.FailOn("")
	for _, s := range []string{failOut, failErr, failEvents} {
		if strings.Contains(s, cdCommunity) {
			t.Fatalf("[AC-159-6] the secret appears:\n%s", s)
		}
	}
	if !strings.Contains(failEvents, "snmp community ***") {
		t.Fatalf("[AC-159-6] the failure does not show the masked command:\n%s", failEvents)
	}
	// 起こし直して（backoff を待たずに）次の周：宣言の値に戻る。
	r.restart()
	r.settle(5)
	if dev.Get("community") != cdCommunity {
		t.Fatalf("[AC-159-6] the community is not the declared one after the device accepted it\n%s", r.errOut.String())
	}
}

const cdUpgradeWorkflow = `{ "workflow": "upgrade",
  "target": { "provider": "cli-device" },
  "stages": [
    { "name": "canary", "targets": ["sw1/version"], "gate": { "manual_approval": "required", "timeout": "24h" } },
    { "name": "rest", "targets": ["sw2/version"] }
  ],
  "steps": [
    { "action": { "op": "upgrade", "args": { "image": "1.2.0.bin" } } },
    { "wait": { "fields": { "version": "1.2.0" }, "timeout": "1h" } }
  ] }`

func TestCLIDevice_UpgradeThroughWorkflowAction(t *testing.T) {
	// [AC-159-7]
	sw1, sw2 := clifake.New("sw1"), clifake.New("sw2")
	r := newCDRig(t, cdExampleDef, budget.Config{}, sw1, sw2)
	r.intent(map[string]map[string]string{
		"sw1/version": {"version": "1.2.0"},
		"sw2/version": {"version": "1.2.0"},
	}, cdUpgradeWorkflow)
	for i := 0; i < 3; i++ {
		res, err := r.cycleErr()
		if err != nil {
			t.Fatalf("RunCycle: %v\n%s", err, r.errOut.String())
		}
		if i == 2 && !hasEvent(res, workflow.EventWaiting, "awaiting approval at gate canary") {
			t.Fatalf("[AC-159-7] not waiting at the gate: %v", res.WorkflowEvents)
		}
		r.clk.Advance(time.Minute)
	}
	if sw1.Get("version") != "1.2.0" || sw2.Get("version") != "1.0.0" {
		t.Fatalf("[AC-159-7] before the approval: sw1 %s sw2 %s, want only the canary upgraded\n%s", sw1.Get("version"), sw2.Get("version"), r.errOut.String())
	}
	if c := changes(sw1); strings.Join(c, "|") != "upgrade image 1.2.0.bin" {
		t.Fatalf("[AC-159-7] sw1 commands = %v, want one upgrade", c)
	}
	if _, err := workflow.Decide(context.Background(), workflow.NewFileStore(r.storePath), audit.NewLog(r.auditPath),
		"upgrade", "canary", workflow.Approval{Approver: "alice", Signature: "sig", Decision: workflow.DecisionApprove}, r.clk.Now()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		r.cycle()
		r.clk.Advance(time.Minute)
	}
	run, _, err := workflow.NewFileStore(r.storePath).Get(context.Background(), "upgrade")
	if err != nil || run.Phase != workflow.PhaseDone {
		t.Fatalf("[AC-159-7] run = %+v, %v\n%s", run, err, r.errOut.String())
	}
	if sw2.Get("version") != "1.2.0" {
		t.Fatalf("[AC-159-7] sw2 = %s after the approval", sw2.Get("version"))
	}
	if c := changes(sw2); strings.Join(c, "|") != "upgrade image 1.2.0.bin" {
		t.Fatalf("[AC-159-7] sw2 commands = %v, want one upgrade", c)
	}
	// done の後は収束ループが観測するだけ（観測だけの項目に Apply しない）。
	for i := 0; i < 2; i++ {
		if _, c := r.cycle(); c != 0 {
			t.Fatalf("[AC-159-7] applied %d after done\n%s", c, r.errOut.String())
		}
	}
}

// cdFakeDevice は exec 型の偽の装置（sh）。$1 が CLI の 1 行。
const cdFakeDevice = `state=%s
case "$1" in
  "show syslog")
    if [ -s "$state" ]; then echo "Syslog server: $(cat "$state")"; else echo "Syslog server: none"; fi ;;
  "set syslog server "*)
    printf '%%s' "${1#set syslog server }" > "$state" ;;
  "clear syslog server")
    : > "$state" ;;
  *)
    echo "ERROR: unknown command: $1"; exit 1 ;;
esac
`

// cdFakeSSH は偽の ssh: 最後の引数（装置の CLI の 1 行）を偽の装置に渡す。
const cdFakeSSH = `#!/bin/sh
for a; do last=$a; done
exec sh %s "$last"
`

const cdExecDef = `name: tiny-device
session:
  mode: exec
errors:
  - '^ERROR:'
items:
  syslog:
    fields:
      server:
        pattern: '[0-9]{1,3}(\.[0-9]{1,3}){3}'
    observe:
      command: show syslog
      regex: '^Syslog server: (?P<server>[0-9.]+)$'
      absent: '^Syslog server: none$'
    set:
      - set syslog server {server}
    unset:
      - clear syslog server
`

func TestCLIDevice_RegistryAndCLI(t *testing.T) {
	// [AC-159-8]
	tmp := t.TempDir()
	state := filepath.Join(tmp, "device-state")
	fakeDev := filepath.Join(tmp, "device.sh")
	fakeSSH := filepath.Join(tmp, "ssh")
	for path, body := range map[string]string{
		fakeDev:                             fmt.Sprintf(cdFakeDevice, state),
		fakeSSH:                             fmt.Sprintf(cdFakeSSH, fakeDev),
		filepath.Join(tmp, "known_hosts"):   "",
		filepath.Join(tmp, "tiny.yaml"):     cdExecDef,
		filepath.Join(tmp, "intent", ".ok"): "",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeJSONFile(t, filepath.Join(tmp, "hosts.json"), map[string]any{
		"ssh": fakeSSH, "known_hosts": "known_hosts",
		"hosts": map[string]any{"dev1": map[string]any{"address": "192.0.2.21", "user": "admin"}},
	})
	cfg := map[string]string{"cli_hosts": "dev1", "remote_config": filepath.Join(tmp, "hosts.json"),
		"cli_definition": filepath.Join(tmp, "tiny.yaml")}

	reg := registry.DefaultRegistry()
	p, err := reg.Get(clidevice.Name, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.ValidateProvider(p); err != nil {
		t.Fatal(err)
	}
	if err := provider.ValidateIntent(p, "dev1/syslog", map[string]string{"server": "192.0.2.53; reboot"}); err == nil {
		t.Error("[AC-159-8] an injected value must be refused")
	}
	obs, err := p.Read(context.Background(), "dev1/syslog")
	if err != nil || obs.Availability != core.AvailabilityOk || obs.State != nil {
		t.Fatalf("[AC-159-8] Read through ssh = %+v, %v (want absent)", obs, err)
	}
	for name, bad := range map[string]map[string]string{
		"no hosts":          {"cli_definition": cfg["cli_definition"]},
		"no definition":     {"cli_hosts": "dev1", "remote_config": cfg["remote_config"]},
		"unknown device":    {"cli_hosts": "dev1", "remote_config": cfg["remote_config"], "cli_definitions": "dev9=" + cfg["cli_definition"]},
		"not in the list":   {"cli_hosts": "dev2", "remote_config": cfg["remote_config"], "cli_definition": cfg["cli_definition"]},
		"bad definitions":   {"cli_hosts": "dev1", "remote_config": cfg["remote_config"], "cli_definitions": "dev1"},
		"no remote_config ": {"cli_hosts": "dev1", "cli_definition": cfg["cli_definition"]},
	} {
		if _, err := reg.Get(clidevice.Name, bad); err == nil {
			t.Errorf("[AC-159-8] %s: want an error", name)
		}
	}

	// 本物の CLI。
	bin := buildBinary(t)
	intentDir := filepath.Join(tmp, "intent")
	writeIntentFile(t, intentDir, map[string]any{"resources": []map[string]any{
		{"provider": clidevice.Name, "id": "dev1/syslog", "fields": map[string]string{"server": "192.0.2.53"}},
	}})
	pc := filepath.Join(tmp, "providers.json")
	pcfg := map[string]any{}
	for k, v := range cfg {
		pcfg[k] = v
	}
	writeJSONFile(t, pc, map[string]any{"providers": map[string]any{clidevice.Name: pcfg}})
	common := []string{"--intent", intentDir, "--state", filepath.Join(tmp, "fake.json"), "--provider-config", pc}
	run := func(verb string) string {
		t.Helper()
		out, _ := runBin(t, bin, append([]string{verb}, common...)...)
		return out
	}
	if out := run("plan"); !strings.Contains(out, "CREATE") || !strings.Contains(out, "dev1/syslog") {
		t.Fatalf("[AC-159-8] plan:\n%s", out)
	}
	if b, _ := os.ReadFile(state); len(b) != 0 {
		t.Fatal("[AC-159-8] plan changed the device")
	}
	out := run("reconcile")
	if b, _ := os.ReadFile(state); string(b) != "192.0.2.53" {
		t.Fatalf("[AC-159-8] reconcile: device state %q\n%s", b, out)
	}
	if out := run("reconcile"); !strings.Contains(out, "Converged (no changes)") {
		t.Fatalf("[AC-159-8] 2nd reconcile:\n%s", out)
	}
	if err := os.WriteFile(state, []byte("198.51.100.7"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := run("plan"); !strings.Contains(out, "198.51.100.7") || !strings.Contains(out, "UPDATE") {
		t.Fatalf("[AC-159-8] plan of the drift:\n%s", out)
	}
	run("reconcile")
	if b, _ := os.ReadFile(state); string(b) != "192.0.2.53" {
		t.Fatalf("[AC-159-8] not repaired: %q", b)
	}
}
