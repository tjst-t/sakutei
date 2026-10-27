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

// netconf_test.go — #141: NETCONF provider（観測だけ）。設定の部分木を観測して
// ずれを知らせ、Apply を出さないことを、偽の NETCONF サーバ（netconffake）で
// デーモンの周（RunCycle）と CLI（plan・reconcile・status、偽の ssh の先で
// このテストのバイナリが偽のサーバになる）で確かめる（§23.4、§29.1）。
//
//	[AC-141-4-1] 宣言どおりなら適合。手で変えるとずれの知らせ（DriftWouldApply）が
//	             出て、何周しても Apply は出ない（機器が受ける rpc は get-config と
//	             close-session だけ）。ほかの provider の資源は同じ周でふつうに収束する
//	[AC-141-4-2] 観測だけ（Capabilities.ObserveOnly）は型で止める: Apply に届いた
//	             操作も executor と router が断る。ObserveOnly で List / Pruneable は
//	             起動時にエラー
//	[AC-141-4-3] CLI: plan は blocked_by observe_only、reconcile は「never applied」で
//	             何も当てない、status は drifted。止めると unknown（unobservable）
//	[AC-141-4-4] registry: netconf_hosts・netconf_subtrees・remote_config で作れる。
//	             足りない・誤った設定はエラー
package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/executor"
	"github.com/tjst-t/sakutei/internal/fakeprovider"
	"github.com/tjst-t/sakutei/internal/netconf"
	"github.com/tjst-t/sakutei/internal/netconf/netconffake"
	"github.com/tjst-t/sakutei/internal/reconcile"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
	"github.com/tjst-t/sakutei/pkg/registry"
	"github.com/tjst-t/sakutei/pkg/sdk"
)

const ncIfNS = "urn:ietf:params:xml:ns:yang:ietf-interfaces"

const ncRunning = `<interfaces xmlns="` + ncIfNS + `" xmlns:ianaift="urn:ietf:params:xml:ns:yang:iana-if-type">` +
	`<interface><name>uplink</name><description>to core</description><type>ianaift:ethernetCsmacd</type></interface>` +
	`<interface><name>mgmt</name><description>oob</description></interface></interfaces>`

const ncDeclared = `<interfaces xmlns="` + ncIfNS + `"><interface><type xmlns:t="urn:ietf:params:xml:ns:yang:iana-if-type">t:ethernetCsmacd</type>` +
	`<name>uplink</name><description>to core</description></interface></interfaces>`

const ncFilter = `<interfaces xmlns="` + ncIfNS + `"><interface><name>uplink</name></interface></interfaces>`

func ncIntent() map[string]any {
	return map[string]any{"resources": []map[string]any{
		{"provider": netconf.Name, "id": "sw1/if-uplink", "fields": map[string]string{"config": ncDeclared}},
		{"provider": "fake", "id": "a", "fields": map[string]string{"name": "a", "size": "1"}},
	}}
}

func TestNetconf_DaemonReportsDriftNeverApplies(t *testing.T) {
	// [AC-141-4-1]
	root := t.TempDir()
	intentDir := filepath.Join(root, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIntentFile(t, intentDir, ncIntent())
	statePath := filepath.Join(root, "state.json")
	initStateFile(t, statePath)
	fake := fakeprovider.New(statePath)

	srv := netconffake.New(ncRunning, true)
	st, err := netconf.NewSubtree("if-uplink", ncFilter)
	if err != nil {
		t.Fatal(err)
	}
	nc, err := netconf.New(map[string]netconf.Dialer{"sw1": srv.Dialer("sw1")}, map[string]netconf.Subtree{"if-uplink": st})
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	var out syncBuffer
	d := reconcile.NewDaemon(reconcile.Config{
		IntentDir:    intentDir,
		GeneratedDir: filepath.Join(root, "generated"),
		Providers: func(name string) (provider.Provider, error) {
			switch name {
			case netconf.Name:
				return nc, nil
			case "fake":
				return fake, nil
			}
			return nil, os.ErrNotExist
		},
		Out:    &out,
		ErrOut: &out,
	})
	cycle := func() reconcile.CycleResult {
		t.Helper()
		res, err := d.RunCycle(context.Background())
		if err != nil {
			t.Fatalf("RunCycle: %v\n%s", err, out.String())
		}
		d.Wait(5 * time.Second)
		return res
	}

	// Converged: the declaration matches the device; the fake resource is created.
	res := cycle()
	if res.ObserveOnlyCount != 0 || res.Launched != 1 {
		t.Fatalf("[AC-141-4-1] cycle 1: observe-only drift %d, launched %d; want 0 and 1 (only the fake)\n%s", res.ObserveOnlyCount, res.Launched, out.String())
	}

	// Someone changes the device by hand: drift is reported every cycle, never applied.
	srv.SetRunning(strings.Replace(ncRunning, "to core", "changed by hand", 1))
	for i := 0; i < 3; i++ {
		res = cycle()
		if res.ObserveOnlyCount != 1 || len(res.ObserveOnlyEvents) != 1 || res.Launched != 0 ||
			len(res.Skipped) != 1 || res.Skipped[0] != "sw1/if-uplink" {
			t.Fatalf("[AC-141-4-1] drift cycle %d: %+v", i, res)
		}
		ev := res.ObserveOnlyEvents[0]
		if ev.Kind != core.EventDriftWouldApply || !strings.Contains(ev.Message, "would be UPDATE netconf//sw1/if-uplink") {
			t.Fatalf("[AC-141-4-1] event = %+v", ev)
		}
	}
	for _, op := range srv.Ops() {
		if op != "get-config" {
			t.Fatalf("[AC-141-4-1] the device received %q (ops %v)", op, srv.Ops())
		}
	}

	// Back as declared: converged again.
	srv.SetRunning(ncRunning)
	if res = cycle(); res.ObserveOnlyCount != 0 {
		t.Fatalf("[AC-141-4-1] restored: %+v", res)
	}
	// Stopped: unknown, not absent, nothing reported as drift.
	srv.SetDown(true)
	if res = cycle(); res.ObserveOnlyCount != 0 || res.Observe.Unknown != 1 {
		t.Fatalf("[AC-141-4-1] down: observe-only drift %d, unknown %d; want 0 and 1", res.ObserveOnlyCount, res.Observe.Unknown)
	}
}

// viewRecorder keeps the CycleViews of the daemon (its read-only GET /plan).
type viewRecorder struct{ ch chan reconcile.CycleView }

func (v viewRecorder) RecordCycle(cv reconcile.CycleView) {
	select {
	case v.ch <- cv:
	default:
	}
}

func TestNetconf_DaemonPlanView(t *testing.T) {
	// [AC-141-4-1] the daemon's GET /plan shows the drift as blocked by observe_only.
	root := t.TempDir()
	intentDir := filepath.Join(root, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIntentFile(t, intentDir, map[string]any{"resources": []map[string]any{
		{"provider": netconf.Name, "id": "sw1/if-uplink", "fields": map[string]string{"config": ncDeclared}}}})
	srv := netconffake.New(strings.Replace(ncRunning, "to core", "changed by hand", 1), false)
	st, err := netconf.NewSubtree("if-uplink", ncFilter)
	if err != nil {
		t.Fatal(err)
	}
	nc, err := netconf.New(map[string]netconf.Dialer{"sw1": srv.Dialer("sw1")}, map[string]netconf.Subtree{"if-uplink": st})
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	rec := viewRecorder{ch: make(chan reconcile.CycleView, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var out syncBuffer
	go func() {
		done <- reconcile.Run(ctx, reconcile.Config{
			IntentDir: intentDir, GeneratedDir: filepath.Join(root, "generated"),
			Provider: nc, Interval: 10 * time.Millisecond, Out: &out, ErrOut: &out, View: rec,
		})
	}()
	defer func() { cancel(); <-done }()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case cv := <-rec.ch:
			if cv.Plan == nil {
				continue
			}
			ops := cv.Plan.Operations
			if len(ops) != 1 || !ops[0].Blocked || ops[0].BlockedBy != "observe_only" {
				t.Fatalf("[AC-141-4-1] /plan operations = %+v", ops)
			}
			if !strings.Contains(out.String(), "observe-only provider(s): 1 drifted resource(s) reported — never applied") {
				continue // the log line follows the view
			}
			for _, op := range srv.Ops() {
				if op != "get-config" {
					t.Fatalf("[AC-141-4-1] the device received %q", op)
				}
			}
			return
		case <-deadline:
			t.Fatalf("[AC-141-4-1] no view with the plan\n%s", out.String())
		}
	}
}

// applyCounter counts Apply calls; it claims ObserveOnly when observeOnly.
type applyCounter struct {
	applied     int
	observeOnly bool
	list        bool
}

func (a *applyCounter) Schema() provider.Schema {
	return provider.Schema{Fields: map[string]provider.FieldSpec{"name": {Kind: core.FieldKindString, Identity: true}}}
}
func (a *applyCounter) Capabilities() provider.Capabilities {
	return provider.Capabilities{TrueObserve: true, ObserveOnly: a.observeOnly, List: a.list, ProviderVersion: "c1"}
}
func (a *applyCounter) Read(context.Context, string) (core.Observation, error) {
	return core.Observation{Availability: core.AvailabilityOk}, nil
}
func (a *applyCounter) Apply(context.Context, core.Plan) error { a.applied++; return nil }
func (a *applyCounter) List(context.Context, core.Scope) (core.ListResult, error) {
	return core.ListResult{}, nil
}

func TestNetconf_ObserveOnlyByType(t *testing.T) {
	// [AC-141-4-2]
	p := &applyCounter{observeOnly: true}
	plan := core.Plan{Ops: []core.Op{{Kind: core.OpKindUpdate, Target: core.ID{Provider: "x", Local: "y"}}}}
	err := executor.New().Execute(context.Background(), p, plan)
	var pe *provider.PermanentError
	if !errors.As(err, &pe) || p.applied != 0 {
		t.Fatalf("[AC-141-4-2] executor: err %v, applied %d", err, p.applied)
	}
	if err := sdk.ValidateProvider(p); err != nil {
		t.Fatalf("[AC-141-4-2] %v", err)
	}
	if err := sdk.ValidateProvider(&applyCounter{observeOnly: true, list: true}); err == nil {
		t.Fatalf("[AC-141-4-2] ObserveOnly with List must be refused at startup")
	}
	// Not observe-only: applied as usual.
	q := &applyCounter{}
	if err := executor.New().Execute(context.Background(), q, plan); err != nil || q.applied != 1 {
		t.Fatalf("[AC-141-4-2] %v %d", err, q.applied)
	}
}

// ---- CLI（偽の ssh の先の偽の NETCONF サーバ）----

// 偽のサーバのプロセスへの指示（環境変数）。
const (
	ncHelperEnv  = "SAKUTEI_IT_NETCONF_FAKE"
	ncRunningEnv = "SAKUTEI_IT_NETCONF_RUNNING"
	ncOpsEnv     = "SAKUTEI_IT_NETCONF_OPS"
	ncBinEnv     = "SAKUTEI_IT_NETCONF_BIN"
	ncDownEnv    = "SAKUTEI_IT_NETCONF_DOWN"
)

// serveNetconfHelperIfAsked は、偽の ssh から起こされたときだけ、標準入出力で
// 偽の NETCONF サーバ（base:1.1）になって終わる（TestMain の頭で呼ぶ）。
func serveNetconfHelperIfAsked() {
	if os.Getenv(ncHelperEnv) == "" {
		return
	}
	srv := netconffake.New("", true)
	srv.RunningFile = os.Getenv(ncRunningEnv)
	srv.OpsFile = os.Getenv(ncOpsEnv)
	srv.ServeStream(stdio{})
	os.Exit(0)
}

type stdio struct{}

func (stdio) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdio) Close() error                { return os.Stdout.Close() }

// ncFakeSSH は ssh のふり: -s ... netconf でこのテストのバイナリを偽のサーバとして
// 起こす。止めている（$SAKUTEI_IT_NETCONF_DOWN のファイルがある）なら ssh と同じく
// 255 で終わる。
const ncFakeSSH = `#!/bin/sh
sub=
for a; do last=$a; [ "$a" = "-s" ] && sub=1; done
[ -n "$sub" ] && [ "$last" = netconf ] || { echo "fake ssh: want -s ... netconf" >&2; exit 2; }
if [ -e "$` + ncDownEnv + `" ]; then
  echo "ssh: connect to host 192.0.2.1 port 830: Connection refused" >&2
  exit 255
fi
` + ncHelperEnv + `=1 exec "$` + ncBinEnv + `"
`

func TestNetconf_CLI(t *testing.T) {
	// [AC-141-4-3]
	bin := buildBinary(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIntentFile(t, intentDir, ncIntent())
	statePath := filepath.Join(tmp, "state.json")
	initStateFile(t, statePath)
	runningPath := filepath.Join(tmp, "running.xml")
	opsPath := filepath.Join(tmp, "ops.log")
	downPath := filepath.Join(tmp, "down")
	writeFile := func(path, body string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(runningPath, ncRunning, 0o600)
	writeFile(filepath.Join(tmp, "ssh"), ncFakeSSH, 0o755)
	writeFile(filepath.Join(tmp, "known_hosts"), "", 0o600)
	ctrl, err := os.MkdirTemp("/tmp", "snc-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(ctrl)
	writeFile(filepath.Join(tmp, "hosts.yaml"), "ssh: ./ssh\nknown_hosts: known_hosts\ncontrol_dir: "+ctrl+"\nhosts:\n  sw1:\n    address: 192.0.2.1\n    port: 830\n    user: netconf\n    connect_timeout: 1s\n", 0o600)
	subtreesBody, _ := json.Marshal(map[string]any{"subtrees": map[string]any{"if-uplink": map[string]string{"filter": ncFilter}}})
	writeFile(filepath.Join(tmp, "subtrees.json"), string(subtreesBody), 0o600)
	writeJSONFile(t, filepath.Join(tmp, "providers.json"), map[string]any{"providers": map[string]any{
		netconf.Name: map[string]string{
			"remote_config":    filepath.Join(tmp, "hosts.yaml"),
			"netconf_hosts":    "sw1",
			"netconf_subtrees": filepath.Join(tmp, "subtrees.json"),
		},
	}})
	common := []string{"--intent", intentDir, "--state", statePath, "--provider-config", filepath.Join(tmp, "providers.json")}
	run := func(args ...string) (string, string, int) {
		t.Helper()
		cmd := exec.Command(bin, append(args, common...)...)
		cmd.Dir = tmp
		cmd.Env = append(os.Environ(), ncBinEnv+"="+self, ncRunningEnv+"="+runningPath, ncOpsEnv+"="+opsPath, ncDownEnv+"="+downPath)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return stdout.String(), stderr.String(), code
	}

	// The fake resource is created; the netconf one is converged.
	if out, errOut, code := run("reconcile"); code != 0 || strings.Contains(out+errOut, "Observe-only") {
		t.Fatalf("[AC-141-4-3] first reconcile: exit %d\n%s%s", code, out, errOut)
	}
	if out, errOut, code := run("plan"); code != 0 || !strings.Contains(out, "Converged") {
		t.Fatalf("[AC-141-4-3] plan: exit %d\n%s%s", code, out, errOut)
	}

	// Changed by hand.
	writeFile(runningPath, strings.Replace(ncRunning, "to core", "changed by hand", 1), 0o600)

	out, errOut, code := run("plan", "--json")
	if code != 0 {
		t.Fatalf("[AC-141-4-3] plan --json: exit %d\n%s%s", code, out, errOut)
	}
	var rep struct {
		Operations []struct {
			ID         string `json:"id"`
			BlockedBy  string `json:"blocked_by"`
			Diagnostic string `json:"diagnostic"`
			Blocked    bool   `json:"blocked"`
		} `json:"operations"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("plan --json: %v\n%s", err, out)
	}
	if len(rep.Operations) != 1 || !rep.Operations[0].Blocked || rep.Operations[0].BlockedBy != "observe_only" ||
		!strings.Contains(rep.Operations[0].Diagnostic, "changed by hand") {
		t.Fatalf("[AC-141-4-3] plan operations = %+v", rep.Operations)
	}
	if out, _, _ := run("plan"); !strings.Contains(out, "blocked: observe_only") {
		t.Errorf("[AC-141-4-3] plan text lacks the block:\n%s", out)
	}

	for i := 0; i < 2; i++ {
		out, errOut, code = run("reconcile")
		if code != 0 || !strings.Contains(out, "Observe-only provider(s): 1 drifted resource(s) reported — never applied") ||
			!strings.Contains(out, "would be UPDATE netconf//sw1/if-uplink") {
			t.Fatalf("[AC-141-4-3] reconcile %d: exit %d\n%s%s", i, code, out, errOut)
		}
	}
	if b, _ := os.ReadFile(runningPath); !strings.Contains(string(b), "changed by hand") {
		t.Fatalf("[AC-141-4-3] the device was changed")
	}

	out, errOut, _ = run("status")
	if !strings.Contains(out+errOut, "netconf//sw1/if-uplink") || !strings.Contains(out+errOut, "drifted") {
		t.Fatalf("[AC-141-4-3] status:\n%s%s", out, errOut)
	}

	ops, err := os.ReadFile(opsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range strings.Fields(string(ops)) {
		if op != "get-config" && op != "close-session" {
			t.Fatalf("[AC-141-4-3] the device received %q:\n%s", op, ops)
		}
	}

	// Stopped: unobservable (unknown — not absent, not a Create).
	writeFile(downPath, "", 0o600)
	out, errOut, _ = run("status")
	if !strings.Contains(out, "UNOBSERVABLE netconf//sw1/if-uplink") || !strings.Contains(out, "unreachable") || strings.Contains(out, "CREATE") {
		t.Fatalf("[AC-141-4-3] status while down:\n%s%s", out, errOut)
	}
}

func TestNetconf_Registry(t *testing.T) {
	// [AC-141-4-4]
	dir := t.TempDir()
	hosts := filepath.Join(dir, "hosts.yaml")
	if err := os.WriteFile(hosts, []byte("known_hosts: known_hosts\nhosts:\n  sw1:\n    address: 192.0.2.1\n    port: 830\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	subtrees := filepath.Join(dir, "subtrees.yaml")
	if err := os.WriteFile(subtrees, []byte("subtrees:\n  if-uplink:\n    filter: '"+ncFilter+"'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := registry.DefaultRegistry()
	p, err := reg.Get(netconf.Name, map[string]string{"remote_config": hosts, "netconf_hosts": "sw1", "netconf_subtrees": subtrees})
	if err != nil {
		t.Fatal(err)
	}
	if err := sdk.ValidateProvider(p); err != nil {
		t.Fatalf("[AC-141-4-4] %v", err)
	}
	if !p.Capabilities().ObserveOnly {
		t.Fatalf("[AC-141-4-4] netconf must be observe-only")
	}
	for name, c := range map[string]map[string]string{
		"no hosts":     {"remote_config": hosts, "netconf_subtrees": subtrees},
		"no subtrees":  {"remote_config": hosts, "netconf_hosts": "sw1"},
		"no config":    {"netconf_hosts": "sw1", "netconf_subtrees": subtrees},
		"unknown host": {"remote_config": hosts, "netconf_hosts": "sw9", "netconf_subtrees": subtrees},
		"bad subtrees": {"remote_config": hosts, "netconf_hosts": "sw1", "netconf_subtrees": hosts},
	} {
		if _, err := reg.Get(netconf.Name, c); err == nil {
			t.Errorf("[AC-141-4-4] %s must be an error", name)
		}
	}
}
