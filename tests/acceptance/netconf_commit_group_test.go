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

// netconf_commit_group_test.go — #199: commit_group で複数の台の変更をまとめて
// 確定することを、3 台の偽の NETCONF サーバ（netconffake）とデーモンの周
// （RunCycle）で確かめる（§23.4）。
//
//	[AC-199-3-1] 全部の台で確かめが通れば全部が確定する。台ごとに
//	             CommitGroupApplied（committed）。次の周は適合
//	[AC-199-3-2] 1 台の確かめが落ちれば、どの台も確定しない（全部の台が元の
//	             まま）。次の周は落ちた台の backoff で group ごと止まる
//	[AC-199-3-3] 確定の rpc の途中で切れた台は unknown（Event に台ごとに出る）、
//	             ほかの台は committed。次の周の観測で切れた台が適合と分かる
//	[AC-199-3-4] 時間の上限（一番短い confirm-timeout の半分）に届けば、どの台も
//	             確定しない
//	[AC-199-3-5] group の中の依存の辺は読み込みで断る（周を飛ばす）
//	[AC-199-3-6] 外の realizer（network_policy の実現先）が返すリソースの Fields
//	             の commit_group でもまとめて確定する
package acceptance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/netconf"
	"github.com/tjst-t/sakutei/v2/internal/netconf/netconffake"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/pkg/netpolicy"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

var ncGroupHosts = []string{"sw1", "sw2", "sw3"}

type ncGroupRig struct {
	srvs  map[string]*netconffake.Server
	out   *syncBuffer
	cycle func() (reconcile.CycleResult, error)
}

// ncGroupBefore は 3 台の、手で変えた running。
var ncGroupBefore = strings.Replace(ncRunning, "to core", "changed by hand", 1)

// ncGroupIntent は 3 台の if-uplink を commit_group "leafs" にした intent。
func ncGroupIntent(config map[string]string) map[string]any {
	var res []map[string]any
	for _, h := range ncGroupHosts {
		c := ncDeclared
		if v, ok := config[h]; ok {
			c = v
		}
		res = append(res, map[string]any{"provider": netconf.Name, "id": h + "/if-uplink",
			"fields": map[string]string{"config": c, "commit_group": "leafs"}})
	}
	return map[string]any{"resources": res}
}

func newNCGroupRig(t *testing.T, intent map[string]any, cfg netconf.ApplyConfig, opts func(map[string]*netconffake.Server)) *ncGroupRig {
	t.Helper()
	root := t.TempDir()
	intentDir := filepath.Join(root, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIntentFile(t, intentDir, intent)
	srvs := map[string]*netconffake.Server{}
	dialers := map[string]netconf.Dialer{}
	for _, h := range ncGroupHosts {
		srv := netconffake.New(ncGroupBefore, true)
		srv.Caps = []string{netconf.CapCandidate, netconf.CapConfirmedCommit10, netconf.CapValidate11}
		srvs[h] = srv
		dialers[h] = srv.Dialer(h)
	}
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
	if cfg.ConfirmTimeout == 0 {
		cfg.ConfirmTimeout = 2 * time.Second
	}
	if err := nc.AllowApply(cfg); err != nil {
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
		Out:    out,
		ErrOut: out,
	})
	r := &ncGroupRig{srvs: srvs, out: out}
	r.cycle = func() (reconcile.CycleResult, error) {
		res, err := d.RunCycle(context.Background())
		d.Wait(30 * time.Second)
		return res, err
	}
	return r
}

func (r *ncGroupRig) mustCycle(t *testing.T) reconcile.CycleResult {
	t.Helper()
	res, err := r.cycle()
	if err != nil {
		t.Fatalf("RunCycle: %v\n%s", err, r.out.String())
	}
	return res
}

func (r *ncGroupRig) confirmed(h string) bool {
	return countOp(r.srvs[h].Ops(), "commit") > 0
}

func (r *ncGroupRig) wantEvent(t *testing.T, h, outcome string) {
	t.Helper()
	want := fmt.Sprintf(`commit_group "leafs": netconf//%s/if-uplink: %s`, h, outcome)
	if !strings.Contains(r.out.String(), want) {
		t.Fatalf("no %q in:\n%s", want, r.out.String())
	}
}

func TestNetconfCommitGroup_AllConfirmed(t *testing.T) {
	// [AC-199-3-1]
	t.Parallel()
	r := newNCGroupRig(t, ncGroupIntent(nil), netconf.ApplyConfig{}, nil)
	res := r.mustCycle(t)
	if res.Launched != 3 {
		t.Fatalf("[AC-199-3-1] launched %d\n%s", res.Launched, r.out.String())
	}
	for _, h := range ncGroupHosts {
		if !r.confirmed(h) || !strings.Contains(r.srvs[h].Running(), "to core") {
			t.Fatalf("[AC-199-3-1] %s: ops %v running %s", h, r.srvs[h].Ops(), r.srvs[h].Running())
		}
		r.wantEvent(t, h, "committed")
	}
	if res := r.mustCycle(t); res.Launched != 0 {
		t.Fatalf("[AC-199-3-1] cycle 2 launched %d\n%s", res.Launched, r.out.String())
	}
}

func TestNetconfCommitGroup_OneCheckFails_NoneConfirmed(t *testing.T) {
	// [AC-199-3-2]
	t.Parallel()
	lockout := strings.Replace(ncDeclared, "to core", "lockout", 1)
	r := newNCGroupRig(t, ncGroupIntent(map[string]string{"sw3": lockout}), netconf.ApplyConfig{}, func(s map[string]*netconffake.Server) {
		s["sw3"].RefuseDialsWhen = func(run string) bool { return strings.Contains(run, "lockout") }
	})
	r.mustCycle(t)
	for _, h := range ncGroupHosts {
		if r.confirmed(h) {
			t.Fatalf("[AC-199-3-2] %s was confirmed: %v", h, r.srvs[h].Ops())
		}
		if got := r.srvs[h].Running(); got != ncGroupBefore {
			t.Fatalf("[AC-199-3-2] %s not as before:\n%s", h, got)
		}
		r.wantEvent(t, h, "not committed")
	}
	if !strings.Contains(r.out.String(), "sw3/if-uplink was not confirmed") {
		t.Fatalf("[AC-199-3-2] the reason:\n%s", r.out.String())
	}
	// The next cycle: every member backs off (each Apply failed), so the
	// group is held whole; nothing is sent.
	ops := map[string]int{}
	for _, h := range ncGroupHosts {
		ops[h] = len(r.srvs[h].Ops())
	}
	res := r.mustCycle(t)
	if res.Launched != 0 || !strings.Contains(r.out.String(), "backoff netconf//sw3/if-uplink: suppressing Apply") {
		t.Fatalf("[AC-199-3-2] cycle 2: launched %d\n%s", res.Launched, r.out.String())
	}
	for _, h := range ncGroupHosts {
		for _, op := range r.srvs[h].Ops()[ops[h]:] {
			if op != "get-config" {
				t.Fatalf("[AC-199-3-2] cycle 2 sent %s to %s", op, h)
			}
		}
	}
}

func TestNetconfCommitGroup_DroppedWhileConfirming_Unknown(t *testing.T) {
	// [AC-199-3-3]
	t.Parallel()
	r := newNCGroupRig(t, ncGroupIntent(nil), netconf.ApplyConfig{}, func(s map[string]*netconffake.Server) {
		s["sw2"].SetDropAfter("commit", 1)
	})
	r.mustCycle(t)
	r.wantEvent(t, "sw1", "committed")
	r.wantEvent(t, "sw3", "committed")
	r.wantEvent(t, "sw2", "unknown")
	if !strings.Contains(r.out.String(), "apply netconf//sw2/if-uplink") {
		t.Fatalf("[AC-199-3-3] sw2's Apply is not reported failed:\n%s", r.out.String())
	}
	// sw2 did commit before the connection broke: the next cycle observes it
	// as declared (the unknown is left to the observation).
	for _, h := range ncGroupHosts {
		if !strings.Contains(r.srvs[h].Running(), "to core") {
			t.Fatalf("[AC-199-3-3] %s: %s", h, r.srvs[h].Running())
		}
	}
}

func TestNetconfCommitGroup_TimeLimit_NoneConfirmed(t *testing.T) {
	// [AC-199-3-4] sw1's confirm-timeout (2s) is the shortest: its half (1s)
	// is the limit, and sw3 answers its commit confirmed after 1.3s.
	t.Parallel()
	r := newNCGroupRig(t, ncGroupIntent(nil), netconf.ApplyConfig{ConfirmTimeout: 4 * time.Second,
		HostConfirmTimeouts: map[string]time.Duration{"sw1": 2 * time.Second}}, func(s map[string]*netconffake.Server) {
		s["sw3"].SetDelay("commit-confirmed", 1300*time.Millisecond)
	})
	r.mustCycle(t)
	for _, h := range ncGroupHosts {
		if r.confirmed(h) || r.srvs[h].Running() != ncGroupBefore {
			t.Fatalf("[AC-199-3-4] %s: ops %v\n%s", h, r.srvs[h].Ops(), r.srvs[h].Running())
		}
		r.wantEvent(t, h, "not committed")
	}
	if !strings.Contains(r.out.String(), "half of the shortest confirm-timeout (1s) was reached") {
		t.Fatalf("[AC-199-3-4] the reason:\n%s", r.out.String())
	}
}

func TestNetconfCommitGroup_DependencyRefused(t *testing.T) {
	// [AC-199-3-5]
	t.Parallel()
	intent := ncGroupIntent(nil)
	res := intent["resources"].([]map[string]any)
	res[1]["depends_on"] = []string{"sw1/if-uplink"}
	r := newNCGroupRig(t, intent, netconf.ApplyConfig{}, nil)
	_, err := r.cycle()
	if err == nil || !strings.Contains(err.Error(), `commit_group "leafs": netconf//sw2/if-uplink depends on netconf//sw1/if-uplink`) {
		t.Fatalf("[AC-199-3-5] RunCycle = %v", err)
	}
	for _, h := range ncGroupHosts {
		if len(r.srvs[h].Ops()) != 0 {
			t.Fatalf("[AC-199-3-5] %s was contacted: %v", h, r.srvs[h].Ops())
		}
	}
}

func TestNetconfCommitGroup_FromRealizer(t *testing.T) {
	// [AC-199-3-6] a realizer of network_policy puts each target on a
	// switch's subtree, with commit_group in the Fields.
	if err := netpolicy.Register("nc-leafs-199", netpolicy.RealizerFunc(func(t netpolicy.Target, allows []netpolicy.Passing) (netpolicy.Output, error) {
		return netpolicy.Output{Resources: []netpolicy.Resource{{Provider: netconf.Name, Scope: t.Scope, ID: t.ID,
			Fields: map[string]string{"config": ncDeclared, "commit_group": "leafs"}}}}, nil
	})); err != nil {
		t.Fatal(err)
	}
	var targets []map[string]any
	for _, h := range ncGroupHosts {
		targets = append(targets, map[string]any{"name": h, "realizer": "nc-leafs-199", "kind": "boundary",
			"networks": []string{"office", "dmz"}, "id": h + "/if-uplink"})
	}
	intent := map[string]any{"network_policy": map[string]any{
		"networks": []map[string]any{
			{"name": "office", "cidrs": []string{"10.1.0.0/24"}},
			{"name": "dmz", "cidrs": []string{"192.0.2.0/24"}},
		},
		"allows":  []map[string]any{{"from": "office", "to": "dmz", "proto": "tcp", "ports": []any{443}, "reason": "web"}},
		"targets": targets,
	}}
	r := newNCGroupRig(t, intent, netconf.ApplyConfig{}, nil)
	r.mustCycle(t)
	for _, h := range ncGroupHosts {
		r.wantEvent(t, h, "committed")
	}
	// One Apply session per device, all opened before any confirm: the
	// group went in one Apply.
	for _, h := range ncGroupHosts {
		if !r.confirmed(h) {
			t.Fatalf("[AC-199-3-6] %s not confirmed", h)
		}
	}
}
