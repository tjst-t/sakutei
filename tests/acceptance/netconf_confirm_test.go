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

// netconf_confirm_test.go — #200: confirm の先の observe:（別のリソースの観測）
// と check:（名前で登録した確かめの関数）が、変えたあと（commit confirmed の
// あと）の確かめとして働き、通らなければ確定しないことを、偽の NETCONF
// サーバ（netconffake）とデーモンの周（RunCycle）で確かめる（§23.4）。
//
//	[AC-200-1] observe: の先（sw2 の部分木）が expect どおりなら sw1 を確定する
//	[AC-200-2] observe: の先が expect と違えば sw1 を確定しない（元に戻る。
//	           理由に observe: の先が出る）
//	[AC-200-3] observe: は観測用の資格（書き込みの文脈でない）と、ふだんの範囲
//	           （部分木の filter の get-config だけ。機器の NACM の外を読まない）
//	           で読む。intent が宣言しないリソースは読み込みで断る
//	[AC-200-4] 外の realizer（network_policy の実現先）が Output.Checks で渡した
//	           check: の関数を呼び、nil になるまで繰り返す。nil にならなければ
//	           確定しない
//	[AC-200-5] commit_group では、全部の台が commit confirmed を終えてから
//	           check: を待つ
//	[AC-200-6] 到達の先（self）の無い confirm・登録の無い check: は読み込みで
//	           断る（after: は observe:・check: では書いても同じ）
package acceptance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/netconf"
	"github.com/tjst-t/sakutei/v2/internal/netconf/netconffake"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/netpolicy"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// ncConfirmBefore は sw1 の、手で変えた running。
var ncConfirmBefore = strings.Replace(ncRunning, "to core", "changed by hand", 1)

// probeProvider は観測だけの「別のリソース」: Read の文脈が書き込みかを残す。
type probeProvider struct {
	state       atomic.Value // string
	reads       atomic.Int32
	writingRead atomic.Int32
}

func (p *probeProvider) Schema() provider.Schema {
	return provider.Schema{Fields: map[string]provider.FieldSpec{
		"name":  {Kind: core.FieldKindString, Identity: true},
		"state": {Kind: core.FieldKindString},
	}}
}

func (p *probeProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{TrueObserve: true, ObserveOnly: true, ProviderVersion: "probe-v1"}
}

func (p *probeProvider) Read(ctx context.Context, id string) (core.Observation, error) {
	p.reads.Add(1)
	if provider.Writing(ctx) {
		p.writingRead.Add(1)
	}
	s, _ := p.state.Load().(string)
	return core.Observation{Availability: core.AvailabilityOk, ObservedAt: time.Now(),
		State: core.State{"name": {Kind: core.FieldKindString, Value: id}, "state": {Kind: core.FieldKindString, Value: s}}}, nil
}

func (p *probeProvider) Apply(ctx context.Context, plan core.Plan) error {
	return provider.NewPermanentError(errors.New("probe observes only"))
}

type ncConfirmRig struct {
	srvs  map[string]*netconffake.Server
	probe *probeProvider
	out   *syncBuffer
	cycle func() (reconcile.CycleResult, error)
}

func newNCConfirmRig(t *testing.T, intent map[string]any, checks map[string]provider.ConfirmCheck, opts func(map[string]*netconffake.Server)) *ncConfirmRig {
	t.Helper()
	root := t.TempDir()
	intentDir := filepath.Join(root, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIntentFile(t, intentDir, intent)
	srvs := map[string]*netconffake.Server{}
	dialers := map[string]netconf.Dialer{}
	for _, h := range []string{"sw1", "sw2"} {
		srv := netconffake.New(ncConfirmBefore, true)
		srv.Caps = []string{netconf.CapCandidate, netconf.CapConfirmedCommit10, netconf.CapValidate11}
		srvs[h] = srv
		dialers[h] = srv.Dialer(h)
	}
	srvs["sw2"].SetRunning(ncRunning) // the peer is as declared
	if opts != nil {
		opts(srvs)
	}
	st, err := netconf.NewSubtree("if-uplink", ncFilter)
	if err != nil {
		t.Fatal(err)
	}
	nc, err := netconf.New(dialers, map[string]netconf.Subtree{"if-uplink": st})
	if err != nil {
		t.Fatal(err)
	}
	if err := nc.AllowApply(netconf.ApplyConfig{ConfirmTimeout: 2 * time.Second}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nc.Close() })
	probe := &probeProvider{}
	probe.state.Store("ready")
	out := &syncBuffer{}
	d := reconcile.NewDaemon(reconcile.Config{
		IntentDir:    intentDir,
		GeneratedDir: filepath.Join(root, "generated"),
		Providers: func(name string) (provider.Provider, error) {
			switch name {
			case netconf.Name:
				return nc, nil
			case "probe":
				return probe, nil
			}
			return nil, os.ErrNotExist
		},
		ConfirmChecks: checks,
		Out:           out,
		ErrOut:        out,
	})
	r := &ncConfirmRig{srvs: srvs, probe: probe, out: out}
	r.cycle = func() (reconcile.CycleResult, error) {
		res, err := d.RunCycle(context.Background())
		d.Wait(30 * time.Second)
		return res, err
	}
	return r
}

func (r *ncConfirmRig) mustCycle(t *testing.T) reconcile.CycleResult {
	t.Helper()
	res, err := r.cycle()
	if err != nil {
		t.Fatalf("RunCycle: %v\n%s", err, r.out.String())
	}
	return res
}

// ncConfirmIntent は sw1 の if-uplink を confirm つきで、sw2 の if-uplink を
// 今の running どおりに宣言した intent。
func ncConfirmIntent(confirm string, more ...map[string]any) map[string]any {
	res := []map[string]any{
		{"provider": netconf.Name, "id": "sw1/if-uplink", "fields": map[string]string{"config": ncDeclared, "confirm": confirm}},
		{"provider": netconf.Name, "id": "sw2/if-uplink", "fields": map[string]string{"config": ncDeclared}},
	}
	return map[string]any{"resources": append(res, more...)}
}

// expectSW2 は sw2 の部分木の config に description を期待する observe: の先。
func expectSW2(desc string) string {
	return `observe:netconf//sw2/if-uplink expect:{"config":"` +
		strings.ReplaceAll(strings.Replace(ncDeclared, "to core", desc, 1), `"`, `\"`) + `"}`
}

func TestNetconfConfirm_ObserveAsExpected_Committed(t *testing.T) {
	// [AC-200-1]
	t.Parallel()
	r := newNCConfirmRig(t, ncConfirmIntent("self, "+expectSW2("to core")), nil, nil)
	if res := r.mustCycle(t); res.Launched != 1 {
		t.Fatalf("[AC-200-1] launched %d\n%s", res.Launched, r.out.String())
	}
	if !strings.Contains(r.srvs["sw1"].Running(), "to core") || countOp(r.srvs["sw1"].Ops(), "commit") == 0 {
		t.Fatalf("[AC-200-1] sw1 not confirmed: %v\n%s", r.srvs["sw1"].Ops(), r.out.String())
	}
	if res := r.mustCycle(t); res.Launched != 0 {
		t.Fatalf("[AC-200-1] cycle 2 launched %d\n%s", res.Launched, r.out.String())
	}
}

func TestNetconfConfirm_ObserveNotAsExpected_NotCommitted(t *testing.T) {
	// [AC-200-2]
	t.Parallel()
	r := newNCConfirmRig(t, ncConfirmIntent("self, after:"+expectSW2("route seen")), nil, nil)
	r.mustCycle(t)
	if countOp(r.srvs["sw1"].Ops(), "commit") != 0 {
		t.Fatalf("[AC-200-2] sw1 was confirmed: %v", r.srvs["sw1"].Ops())
	}
	if got := r.srvs["sw1"].Running(); got != ncConfirmBefore {
		t.Fatalf("[AC-200-2] sw1 not rolled back:\n%s", got)
	}
	for _, want := range []string{"not confirmed: observe:netconf//sw2/if-uplink: config not as expected", "the confirming commit was not sent"} {
		if !strings.Contains(r.out.String(), want) {
			t.Fatalf("[AC-200-2] no %q in:\n%s", want, r.out.String())
		}
	}
	// The peer was only read.
	for _, op := range r.srvs["sw2"].Ops() {
		if op != "get-config" {
			t.Fatalf("[AC-200-2] sent %s to sw2", op)
		}
	}
}

func TestNetconfConfirm_ObserveReadsWithObserveCredentialWithinRange(t *testing.T) {
	// [AC-200-3]
	t.Parallel()
	probe := map[string]any{"provider": "probe", "id": "p1", "fields": map[string]string{"state": "ready"}}
	r := newNCConfirmRig(t, ncConfirmIntent(`self, observe:probe//p1 expect:{"state":"ready"}, observe:netconf//sw2/if-uplink`, probe), nil,
		func(s map[string]*netconffake.Server) {
			// sw2's user may read the subtree only, and send get-config only.
			s["sw2"].Scope = ncFilter
			s["sw2"].ScopeOps = []string{"get-config"}
		})
	before := r.probe.reads.Load()
	r.mustCycle(t)
	if countOp(r.srvs["sw1"].Ops(), "commit") == 0 {
		t.Fatalf("[AC-200-3] sw1 not confirmed:\n%s", r.out.String())
	}
	if r.probe.reads.Load()-before < 2 {
		t.Fatalf("[AC-200-3] the observe: did not read the probe (reads %d)", r.probe.reads.Load()-before)
	}
	if n := r.probe.writingRead.Load(); n != 0 {
		t.Fatalf("[AC-200-3] %d read(s) were made in the write context (with the write credential)", n)
	}
	if d := r.srvs["sw2"].Denials(); len(d) != 0 {
		t.Fatalf("[AC-200-3] sw2 was read or written outside its range: %v", d)
	}
	// A resource the intent does not declare is never read: refused at load.
	r2 := newNCConfirmRig(t, ncConfirmIntent(`self, observe:probe//p2`, probe), nil, nil)
	if _, err := r2.cycle(); err == nil || !strings.Contains(err.Error(), "observe:probe//p2 is not declared in the intent") {
		t.Fatalf("[AC-200-3] RunCycle = %v", err)
	}
	if len(r2.srvs["sw1"].Ops()) != 0 {
		t.Fatalf("[AC-200-3] sw1 was contacted: %v", r2.srvs["sw1"].Ops())
	}
}

func TestNetconfConfirm_CheckFromRealizer(t *testing.T) {
	// [AC-200-4] a realizer of network_policy puts the target on sw1's subtree
	// and hands the check function with its Output.
	var calls atomic.Int32
	var writing atomic.Int32
	var mu sync.Mutex
	var seen []core.ID
	pass := atomic.Bool{}
	pass.Store(true)
	check := func(ctx context.Context, tg provider.ConfirmTarget) error {
		n := calls.Add(1)
		if provider.Writing(ctx) {
			writing.Add(1)
		}
		mu.Lock()
		seen = append(seen, tg.ID)
		mu.Unlock()
		if !pass.Load() || n < 3 {
			return errors.New("the route is not seen from the peer yet")
		}
		return nil
	}
	if err := netpolicy.Register("nc-check-200", netpolicy.RealizerFunc(func(t netpolicy.Target, allows []netpolicy.Passing) (netpolicy.Output, error) {
		return netpolicy.Output{
			Resources: []netpolicy.Resource{{Provider: netconf.Name, Scope: t.Scope, ID: t.ID,
				Fields: map[string]string{"config": ncDeclared, "confirm": "self, check:route-seen-200"}}},
			Checks: map[string]provider.ConfirmCheck{"route-seen-200": check},
		}, nil
	})); err != nil {
		t.Fatal(err)
	}
	intent := map[string]any{"network_policy": map[string]any{
		"networks": []map[string]any{
			{"name": "office", "cidrs": []string{"10.1.0.0/24"}},
			{"name": "dmz", "cidrs": []string{"192.0.2.0/24"}},
		},
		"allows": []map[string]any{{"from": "office", "to": "dmz", "proto": "tcp", "ports": []any{443}, "reason": "web"}},
		"targets": []map[string]any{{"name": "sw1", "realizer": "nc-check-200", "kind": "boundary",
			"networks": []string{"office", "dmz"}, "id": "sw1/if-uplink"}},
	}}
	t.Run("passes on the third call: committed", func(t *testing.T) {
		r := newNCConfirmRig(t, intent, nil, nil)
		r.mustCycle(t)
		if countOp(r.srvs["sw1"].Ops(), "commit") == 0 || calls.Load() != 3 {
			t.Fatalf("[AC-200-4] calls %d, ops %v\n%s", calls.Load(), r.srvs["sw1"].Ops(), r.out.String())
		}
		if writing.Load() != 0 {
			t.Fatalf("[AC-200-4] the check was called in the write context")
		}
		mu.Lock()
		defer mu.Unlock()
		if seen[0].Provider != netconf.Name || seen[0].Local != "sw1/if-uplink" {
			t.Fatalf("[AC-200-4] the check was asked about %v", seen[0])
		}
	})
	t.Run("never passes: not committed", func(t *testing.T) {
		pass.Store(false)
		r := newNCConfirmRig(t, intent, nil, nil)
		r.mustCycle(t)
		if countOp(r.srvs["sw1"].Ops(), "commit") != 0 || r.srvs["sw1"].Running() != ncConfirmBefore {
			t.Fatalf("[AC-200-4] sw1: ops %v\n%s", r.srvs["sw1"].Ops(), r.srvs["sw1"].Running())
		}
		if !strings.Contains(r.out.String(), "not confirmed: check:route-seen-200: the route is not seen from the peer yet") {
			t.Fatalf("[AC-200-4] the reason:\n%s", r.out.String())
		}
	})
}

func TestNetconfConfirm_CommitGroup_ChecksAfterEveryCommitConfirmed(t *testing.T) {
	// [AC-200-5] sw1 and sw2 in a group; sw2 answers its commit confirmed
	// late. sw1's check runs only once both have committed confirmed.
	t.Parallel()
	var srvs map[string]*netconffake.Server
	var pendingBoth, calls atomic.Int32
	check := func(ctx context.Context, tg provider.ConfirmTarget) error {
		calls.Add(1)
		if srvs["sw1"].Pending() && srvs["sw2"].Pending() {
			pendingBoth.Add(1)
		}
		return nil
	}
	intent := map[string]any{"resources": []map[string]any{
		{"provider": netconf.Name, "id": "sw1/if-uplink", "fields": map[string]string{"config": ncDeclared, "commit_group": "pair", "confirm": "self, check:both"}},
		{"provider": netconf.Name, "id": "sw2/if-uplink", "fields": map[string]string{"config": ncDeclared, "commit_group": "pair"}},
	}}
	r := newNCConfirmRig(t, intent, map[string]provider.ConfirmCheck{"both": check}, func(s map[string]*netconffake.Server) {
		srvs = s
		s["sw2"].SetRunning(ncConfirmBefore)
		s["sw2"].SetDelay("commit-confirmed", 300*time.Millisecond)
	})
	r.mustCycle(t)
	if calls.Load() == 0 || pendingBoth.Load() != calls.Load() {
		t.Fatalf("[AC-200-5] check calls %d, with both committed confirmed %d\n%s", calls.Load(), pendingBoth.Load(), r.out.String())
	}
	for _, h := range []string{"sw1", "sw2"} {
		if countOp(r.srvs[h].Ops(), "commit") == 0 {
			t.Fatalf("[AC-200-5] %s not confirmed\n%s", h, r.out.String())
		}
	}
}

func TestNetconfConfirm_LoadRefusals(t *testing.T) {
	// [AC-200-6]
	t.Parallel()
	for _, c := range []struct{ confirm, want string }{
		{expectSW2("to core"), "no target is reached"},
		{"after:check:x", "no target is reached"},
		{"after:self", "after:self"},
		{"self, check:nobody", "check:nobody: no check function is registered"},
		{`self, observe:netconf//sw2/if-uplink expect:{"capabilities":"x"}`, `field "capabilities" is not compared`},
		{`self, observe:netconf//sw1/if-uplink`, "is the resource itself"},
	} {
		r := newNCConfirmRig(t, ncConfirmIntent(c.confirm), map[string]provider.ConfirmCheck{"x": func(context.Context, provider.ConfirmTarget) error { return nil }}, nil)
		if _, err := r.cycle(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("[AC-200-6] confirm %q: RunCycle = %v", c.confirm, err)
		}
	}
	// after: on observe: / check: is the same as without it.
	r := newNCConfirmRig(t, ncConfirmIntent("self, after:check:x"), map[string]provider.ConfirmCheck{"x": func(context.Context, provider.ConfirmTarget) error { return nil }}, nil)
	r.mustCycle(t)
	if countOp(r.srvs["sw1"].Ops(), "commit") == 0 {
		t.Fatalf("[AC-200-6] not confirmed:\n%s", r.out.String())
	}
}
