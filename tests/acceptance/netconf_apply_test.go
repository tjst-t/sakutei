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

// netconf_apply_test.go — #175: NETCONF の commit confirmed の 2 段の Apply を、
// 偽の NETCONF サーバ（netconffake、:candidate・:confirmed-commit:1.0）で
// デーモンの周（RunCycle）と registry で確かめる（§23.4、§29.1）。
//
//	[AC-175-3-1] 設定で許すと、ずれた部分木を周の Apply が 2 段で直す（同じセッションで
//	             commit confirmed と確定の commit、その間に新しい接続で確かめる）。
//	             次の周は適合で、Apply は出ない
//	[AC-175-3-2] 確かめに失敗すると確定せず、機器が confirm-timeout で戻す。Apply は
//	             PermanentError で、同じ宣言を次の周に当て直さない
//	[AC-175-3-3] 許しても、remediation が alert-only の周は Apply に渡らない（今の
//	             Apply と同じ門を通る）
//	[AC-175-3-4] registry: netconf_apply が "true" のときだけ CommitConfirm を宣言し
//	             ObserveOnly を外す。誤った値・Apply を許さずに Apply の設定だけ、はエラー
package acceptance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/netconf"
	"github.com/tjst-t/sakutei/v2/internal/netconf/netconffake"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/internal/remediation"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
	"github.com/tjst-t/sakutei/v2/pkg/registry"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
)

type ncApplyRig struct {
	srv   *netconffake.Server
	d     *reconcile.Daemon
	out   *syncBuffer
	cycle func() reconcile.CycleResult
}

func newNCApplyRig(t *testing.T, running, declaredConfig string, rem remediation.Config, opts ...func(*netconffake.Server)) *ncApplyRig {
	t.Helper()
	root := t.TempDir()
	intentDir := filepath.Join(root, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIntentFile(t, intentDir, map[string]any{"resources": []map[string]any{
		{"provider": netconf.Name, "id": "sw1/if-uplink", "fields": map[string]string{"config": declaredConfig}}}})
	srv := netconffake.New(running, true)
	srv.Caps = []string{netconf.CapCandidate, netconf.CapConfirmedCommit10, netconf.CapValidate11}
	for _, o := range opts {
		o(srv)
	}
	st, err := netconf.NewSubtree("if-uplink", ncFilter)
	if err != nil {
		t.Fatal(err)
	}
	nc, err := netconf.New(map[string]netconf.Dialer{"sw1": srv.Dialer("sw1")}, map[string]netconf.Subtree{"if-uplink": st})
	if err != nil {
		t.Fatal(err)
	}
	if err := nc.AllowApply(netconf.ApplyConfig{ConfirmTimeout: 2 * time.Second}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nc.Close() })
	out := &syncBuffer{}
	d := reconcile.NewDaemon(reconcile.Config{
		IntentDir:    intentDir,
		GeneratedDir: filepath.Join(root, "generated"),
		Providers: func(name string) (provider.Provider, error) {
			if name == netconf.Name {
				return nc, nil
			}
			return nil, os.ErrNotExist
		},
		Remediation: rem,
		Out:         out,
		ErrOut:      out,
	})
	r := &ncApplyRig{srv: srv, d: d, out: out}
	r.cycle = func() reconcile.CycleResult {
		t.Helper()
		res, err := d.RunCycle(context.Background())
		if err != nil {
			t.Fatalf("RunCycle: %v\n%s", err, out.String())
		}
		d.Wait(30 * time.Second)
		return res
	}
	return r
}

func countOp(ops []string, op string) int {
	n := 0
	for _, o := range ops {
		if o == op {
			n++
		}
	}
	return n
}

func TestNetconfApply_DaemonAppliesInTwoSteps(t *testing.T) {
	// [AC-175-3-1]
	r := newNCApplyRig(t, strings.Replace(ncRunning, "to core", "changed by hand", 1), ncDeclared, remediation.Config{})
	res := r.cycle()
	if res.ObserveOnlyCount != 0 || res.Launched != 1 {
		t.Fatalf("[AC-175-3-1] cycle 1: observe-only %d, launched %d\n%s", res.ObserveOnlyCount, res.Launched, r.out.String())
	}
	if !strings.Contains(r.srv.Running(), "to core") || !strings.Contains(r.srv.Running(), "<name>mgmt</name>") {
		t.Fatalf("[AC-175-3-1] running after the cycle:\n%s\n%s", r.srv.Running(), r.out.String())
	}
	calls := r.srv.Calls()
	as := 0
	for _, c := range calls {
		if c.Op == "commit-confirmed" {
			as = c.Session
		}
	}
	var seq []string
	checked := false
	for _, c := range calls {
		if c.Session == as {
			seq = append(seq, c.Op)
		} else if len(seq) > 0 && seq[len(seq)-1] == "commit-confirmed" && c.Op == "get-config" {
			checked = true
		}
	}
	if strings.Join(seq, " ") != "lock get-config edit-config validate commit-confirmed commit unlock close-session" || !checked {
		t.Fatalf("[AC-175-3-1] the Apply session: %v, checked over a new connection: %v", seq, checked)
	}
	for i := 0; i < 2; i++ {
		if res = r.cycle(); res.Launched != 0 || res.ObserveOnlyCount != 0 {
			t.Fatalf("[AC-175-3-1] cycle %d after: %+v", i+2, res)
		}
	}
	if n := countOp(r.srv.Ops(), "commit-confirmed"); n != 1 {
		t.Fatalf("[AC-175-3-1] %d commit confirmed, want 1", n)
	}
}

func TestNetconfApply_DaemonVerifyFailsRollsBack(t *testing.T) {
	// [AC-175-3-2]
	before := strings.Replace(ncRunning, "to core", "changed by hand", 1)
	r := newNCApplyRig(t, before, strings.Replace(ncDeclared, "to core", "lockout", 1), remediation.Config{})
	r.srv.RefuseDialsWhen = func(running string) bool { return strings.Contains(running, "lockout") }
	if res := r.cycle(); res.Launched != 1 {
		t.Fatalf("[AC-175-3-2] cycle 1: %+v\n%s", res, r.out.String())
	}
	if got := r.srv.Rollbacks(); len(got) != 1 || got[0] != "timeout" || r.srv.Running() != before {
		t.Fatalf("[AC-175-3-2] rollbacks %v, running %s", got, r.srv.Running())
	}
	if !strings.Contains(r.out.String(), "not confirmed") || !strings.Contains(r.out.String(), "rolled back at the timeout") {
		t.Fatalf("[AC-175-3-2] the failure is not reported:\n%s", r.out.String())
	}
	if n := countOp(r.srv.Ops(), "commit"); n != 0 {
		t.Fatalf("[AC-175-3-2] %d confirming commit(s) sent", n)
	}
	// Permanent: the same declaration is not applied again.
	for i := 0; i < 2; i++ {
		r.cycle()
	}
	if n := countOp(r.srv.Ops(), "commit-confirmed"); n != 1 {
		t.Fatalf("[AC-175-3-2] %d commit confirmed, want 1 (not retried)\n%s", n, r.out.String())
	}
}

func TestNetconfApply_AlertOnlyHolds(t *testing.T) {
	// [AC-175-3-3]
	r := newNCApplyRig(t, strings.Replace(ncRunning, "to core", "changed by hand", 1), ncDeclared,
		remediation.Config{Mode: remediation.ModeAlertOnly})
	for i := 0; i < 2; i++ {
		res := r.cycle()
		if !res.AlertOnly || res.DriftCount != 1 || res.Launched != 0 {
			t.Fatalf("[AC-175-3-3] cycle %d: alert-only %v, drift %d, launched %d", i, res.AlertOnly, res.DriftCount, res.Launched)
		}
	}
	for _, op := range r.srv.Ops() {
		if op != "get-config" {
			t.Fatalf("[AC-175-3-3] the device received %q", op)
		}
	}
}

func TestNetconfApply_Registry(t *testing.T) {
	// [AC-175-3-4]
	dir := t.TempDir()
	hosts := filepath.Join(dir, "hosts.yaml")
	if err := os.WriteFile(hosts, []byte("known_hosts: known_hosts\nhosts:\n  sw1:\n    address: 192.0.2.1\n    port: 830\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	subtrees := filepath.Join(dir, "subtrees.yaml")
	if err := os.WriteFile(subtrees, []byte("subtrees:\n  if-uplink:\n    filter: '"+ncFilter+"'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := func(extra map[string]string) map[string]string {
		c := map[string]string{"remote_config": hosts, "netconf_hosts": "sw1", "netconf_subtrees": subtrees}
		for k, v := range extra {
			c[k] = v
		}
		return c
	}
	reg := registry.DefaultRegistry()
	for _, extra := range []map[string]string{
		{"netconf_apply": "true"},
		{"netconf_apply": "true", "netconf_confirm_timeout": "30s", "netconf_commit_flow": "1.1"},
	} {
		p, err := reg.Get(netconf.Name, base(extra))
		if err != nil {
			t.Fatalf("[AC-175-3-4] %v: %v", extra, err)
		}
		if caps := p.Capabilities(); caps.ObserveOnly || !caps.CommitConfirm {
			t.Fatalf("[AC-175-3-4] %v: capabilities %+v", extra, caps)
		}
		if err := sdk.ValidateProvider(p); err != nil {
			t.Fatalf("[AC-175-3-4] %v", err)
		}
	}
	for _, extra := range []map[string]string{nil, {"netconf_apply": "false"}} {
		p, err := reg.Get(netconf.Name, base(extra))
		if err != nil || !p.Capabilities().ObserveOnly || p.Capabilities().CommitConfirm {
			t.Fatalf("[AC-175-3-4] %v: must stay observe-only (%v)", extra, err)
		}
	}
	for name, extra := range map[string]map[string]string{
		"bad apply":            {"netconf_apply": "yes"},
		"bad timeout":          {"netconf_apply": "true", "netconf_confirm_timeout": "soon"},
		"short timeout":        {"netconf_apply": "true", "netconf_confirm_timeout": "1s"},
		"bad flow":             {"netconf_apply": "true", "netconf_commit_flow": "2.0"},
		"timeout, no apply":    {"netconf_confirm_timeout": "30s"},
		"flow, apply is false": {"netconf_apply": "false", "netconf_commit_flow": "1.1"},
	} {
		if _, err := reg.Get(netconf.Name, base(extra)); err == nil {
			t.Errorf("[AC-175-3-4] %s must be an error", name)
		}
	}
}
