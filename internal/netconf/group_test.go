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

package netconf_test

// group_test.go — commit_group で複数の台の変更をまとめて確定する（Backlog
// #199）を、3 台の偽の NETCONF サーバで確かめる。
//
// Tags:
//   [AC-199-1-1] 全部の台で確かめが通れば、全部の台に確定の commit を送る
//                （commit confirmed を全部に送ってから確かめ、確かめてから確定）
//   [AC-199-1-2] 1 台の確かめが落ちれば、どの台も確定しない。1.1 の台には
//                cancel-commit を送り、ほかは confirm-timeout で戻る
//   [AC-199-1-3] 確定の rpc の途中で切れた台は unknown、ほかは committed
//   [AC-199-1-4] 時間の上限（一番短い confirm-timeout の半分）に届けば、どの台も
//                確定しない
//   [AC-199-1-5] commit_group の宣言・1 台に 2 つの member は断る。commit_group
//                は差分にならない

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/netconf"
	"github.com/tjst-t/sakutei/v2/internal/netconf/netconffake"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

var groupHosts = []string{"sw1", "sw2", "sw3"}

// groupLab は 3 台の偽の機器と、それを Apply する provider。
func groupLab(t *testing.T, cfg netconf.ApplyConfig, caps map[string][]string) (*netconf.Provider, map[string]*netconffake.Server) {
	t.Helper()
	srvs := map[string]*netconffake.Server{}
	dialers := map[string]netconf.Dialer{}
	for _, h := range groupHosts {
		c := caps10
		if v, ok := caps[h]; ok {
			c = v
		}
		srvs[h] = newServer(strings.Replace(running, "to core", "changed by hand", 1), c...)
		dialers[h] = srvs[h].Dialer(h)
	}
	p, err := netconf.New(dialers, subtrees(t))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConfirmTimeout == 0 {
		cfg.ConfirmTimeout = 2 * time.Second
	}
	if err := p.AllowApply(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, srvs
}

// groupPlan は 3 台の if-uplink を group "g" にまとめた plan（コアがする
// こと: op の CommitGroup）。
func groupPlan(t *testing.T, p *netconf.Provider, config map[string]string) core.Plan {
	t.Helper()
	var out core.Plan
	for _, h := range groupHosts {
		id := h + "/if-uplink"
		fields := map[string]string{"config": config[h], "commit_group": "g"}
		if got := p.CommitGroup(id, fields); got != "g" {
			t.Fatalf("CommitGroup(%s) = %q", id, got)
		}
		pl := plan(t, p, id, fields, read(t, p, id))
		if len(pl.Ops) != 1 {
			t.Fatalf("plan of %s: %+v", id, pl.Ops)
		}
		for _, f := range pl.Ops[0].Fields {
			if f.Field == "commit_group" {
				t.Fatalf("[AC-199-1-5] commit_group must not be a diff: %+v", pl.Ops[0].Fields)
			}
		}
		op := pl.Ops[0]
		op.CommitGroup = "g"
		out.Ops = append(out.Ops, op)
	}
	return out
}

func sameConfig(c string) map[string]string {
	return map[string]string{"sw1": c, "sw2": c, "sw3": c}
}

func groupError(t *testing.T, err error) *provider.CommitGroupError {
	t.Helper()
	var ge *provider.CommitGroupError
	if !errors.As(err, &ge) {
		t.Fatalf("Apply = %v, want a *provider.CommitGroupError", err)
	}
	return ge
}

// confirmedOn は機器が確定の commit を受けたか。
func confirmedOn(srv *netconffake.Server) bool {
	for _, op := range srv.Ops() {
		if op == "commit" {
			return true
		}
	}
	return false
}

func TestCommitGroup_AllConfirmed(t *testing.T) {
	// [AC-199-1-1]
	t.Parallel()
	p, srvs := groupLab(t, netconf.ApplyConfig{}, nil)
	if err := p.Apply(context.Background(), groupPlan(t, p, sameConfig(declared))); err != nil {
		t.Fatalf("[AC-199-1-1] Apply: %v", err)
	}
	for _, h := range groupHosts {
		srv := srvs[h]
		if !converged(t, p, h+"/if-uplink", map[string]string{"config": declared}) {
			t.Fatalf("[AC-199-1-1] %s not as declared:\n%s", h, srv.Running())
		}
		as := sessionOf(srv.Calls(), "commit-confirmed")
		want := "lock get-config edit-config validate commit-confirmed commit unlock close-session"
		if got := strings.Join(opsOf(srv.Calls(), as), " "); got != want {
			t.Fatalf("[AC-199-1-1] %s: the Apply session's ops = %s", h, got)
		}
		if srv.Pending() || len(srv.Rollbacks()) != 0 {
			t.Fatalf("[AC-199-1-1] %s: pending %v, rollbacks %v", h, srv.Pending(), srv.Rollbacks())
		}
	}
}

// TestCommitGroup_OrderAcrossDevices: no device is confirmed before every
// device committed confirmed and was checked.
func TestCommitGroup_OrderAcrossDevices(t *testing.T) {
	// [AC-199-1-1]
	t.Parallel()
	p, srvs := groupLab(t, netconf.ApplyConfig{}, nil)
	// sw3 is slow to answer its commit confirmed, and its check (a new
	// connection after it) must see no other device confirmed yet. (The hook
	// runs under sw3's lock: it reads only the other servers.)
	srvs["sw3"].SetDelay("commit-confirmed", 300*time.Millisecond)
	confirmedEarly := make(chan string, 8)
	srvs["sw3"].RefuseDialsWhen = func(string) bool {
		for _, h := range []string{"sw1", "sw2"} {
			if confirmedOn(srvs[h]) {
				confirmedEarly <- h
			}
		}
		return false
	}
	if err := p.Apply(context.Background(), groupPlan(t, p, sameConfig(declared))); err != nil {
		t.Fatalf("[AC-199-1-1] Apply: %v", err)
	}
	for _, h := range []string{"sw1", "sw2"} {
		ops := strings.Join(srvs[h].Ops(), " ")
		if !strings.Contains(ops, "commit-confirmed") || !confirmedOn(srvs[h]) {
			t.Fatalf("[AC-199-1-1] %s ops: %s", h, ops)
		}
	}
	select {
	case h := <-confirmedEarly:
		t.Fatalf("[AC-199-1-1] %s was confirmed before sw3 committed confirmed", h)
	default:
	}
}

func TestCommitGroup_OneCheckFails_NoneConfirmed(t *testing.T) {
	// [AC-199-1-2]
	t.Parallel()
	caps11 := []string{netconf.CapCandidate, netconf.CapConfirmedCommit10, netconf.CapConfirmedCommit11, netconf.CapValidate11}
	p, srvs := groupLab(t, netconf.ApplyConfig{}, map[string][]string{"sw2": caps11})
	before := map[string]string{}
	for h, srv := range srvs {
		before[h] = srv.Running()
	}
	// sw3's declaration cuts off new connections (a lockout): its check fails.
	srvs["sw3"].RefuseDialsWhen = func(r string) bool { return strings.Contains(r, "lockout") }
	cfg := sameConfig(declared)
	cfg["sw3"] = strings.Replace(declared, "to core", "lockout", 1)
	start := time.Now()
	err := p.Apply(context.Background(), groupPlan(t, p, cfg))
	took := time.Since(start)
	ge := groupError(t, err)
	for _, h := range groupHosts {
		m := ge.Members[h+"/if-uplink"]
		if m.Outcome != provider.CommitNotCommitted || m.Err == nil {
			t.Fatalf("[AC-199-1-2] %s: %+v", h, m)
		}
		if confirmedOn(srvs[h]) {
			t.Fatalf("[AC-199-1-2] %s received a confirming commit: %v", h, srvs[h].Ops())
		}
		if got := srvs[h].Running(); got != before[h] {
			t.Fatalf("[AC-199-1-2] %s not rolled back:\n%s", h, got)
		}
	}
	if m := ge.Members["sw3/if-uplink"]; !isPermanent(m.Err) || !strings.Contains(m.Err.Error(), "not confirmed") {
		t.Fatalf("[AC-199-1-2] sw3: %v", m.Err)
	}
	if m := ge.Members["sw1/if-uplink"]; !isTransient(m.Err) || !strings.Contains(m.Err.Error(), "sw3/if-uplink was not confirmed") {
		t.Fatalf("[AC-199-1-2] sw1: %v", m.Err)
	}
	// sw2 (:confirmed-commit:1.1) was cancelled at once; the others rolled
	// back at their timeout.
	if got := srvs["sw2"].Rollbacks(); len(got) != 1 || got[0] != "cancel" {
		t.Fatalf("[AC-199-1-2] sw2 rollbacks = %v (want cancel)", got)
	}
	for _, h := range []string{"sw1", "sw3"} {
		if got := srvs[h].Rollbacks(); len(got) != 1 || got[0] != "timeout" {
			t.Fatalf("[AC-199-1-2] %s rollbacks = %v (want timeout)", h, got)
		}
	}
	if !strings.Contains(strings.Join(srvs["sw2"].Ops(), " "), "cancel-commit") {
		t.Fatalf("[AC-199-1-2] sw2 got no cancel-commit: %v", srvs["sw2"].Ops())
	}
	if took < 2*time.Second {
		t.Fatalf("[AC-199-1-2] returned after %s, before the confirm-timeout", took)
	}
	// The core's per-member view.
	if provider.MemberError(err, core.ID{Provider: netconf.Name, Local: "sw1/if-uplink"}) == nil {
		t.Fatalf("[AC-199-1-2] MemberError of sw1 is nil")
	}
}

func TestCommitGroup_DroppedWhileConfirming_Unknown(t *testing.T) {
	// [AC-199-1-3]
	t.Parallel()
	p, srvs := groupLab(t, netconf.ApplyConfig{}, nil)
	// sw2 commits and the connection breaks before the reply.
	srvs["sw2"].SetDropAfter("commit", 1)
	err := p.Apply(context.Background(), groupPlan(t, p, sameConfig(declared)))
	ge := groupError(t, err)
	for h, want := range map[string]provider.CommitOutcome{"sw1": provider.CommitCommitted, "sw2": provider.CommitUnknown, "sw3": provider.CommitCommitted} {
		m := ge.Members[h+"/if-uplink"]
		if m.Outcome != want {
			t.Fatalf("[AC-199-1-3] %s: outcome %s, want %s (%v)", h, m.Outcome, want, m.Err)
		}
	}
	m := ge.Members["sw2/if-uplink"]
	if !isTransient(m.Err) || !strings.Contains(m.Err.Error(), "the outcome is unknown") {
		t.Fatalf("[AC-199-1-3] sw2: %v", m.Err)
	}
	if provider.MemberError(err, core.ID{Provider: netconf.Name, Local: "sw1/if-uplink"}) != nil {
		t.Fatalf("[AC-199-1-3] sw1 must count as applied")
	}
	if !strings.Contains(err.Error(), "unknown sw2/if-uplink") {
		t.Fatalf("[AC-199-1-3] error: %v", err)
	}
}

func TestCommitGroup_TimeLimit_NoneConfirmed(t *testing.T) {
	// [AC-199-1-4] the shortest confirm-timeout (sw1: 2s, half 1s) is the
	// limit: sw3 answers its commit confirmed after 1.3s (within half of its
	// own 4s), and nothing is confirmed.
	t.Parallel()
	p, srvs := groupLab(t, netconf.ApplyConfig{ConfirmTimeout: 4 * time.Second,
		HostConfirmTimeouts: map[string]time.Duration{"sw1": 2 * time.Second}}, nil)
	srvs["sw3"].SetDelay("commit-confirmed", 1300*time.Millisecond)
	before := srvs["sw1"].Running()
	err := p.Apply(context.Background(), groupPlan(t, p, sameConfig(declared)))
	ge := groupError(t, err)
	for _, h := range groupHosts {
		m := ge.Members[h+"/if-uplink"]
		if m.Outcome != provider.CommitNotCommitted || !strings.Contains(m.Err.Error(), "half of the shortest confirm-timeout (1s) was reached") {
			t.Fatalf("[AC-199-1-4] %s: %+v", h, m)
		}
		if confirmedOn(srvs[h]) {
			t.Fatalf("[AC-199-1-4] %s was confirmed", h)
		}
	}
	if srvs["sw1"].Running() != before {
		t.Fatalf("[AC-199-1-4] sw1 not rolled back")
	}
}

func TestCommitGroup_StageFails_NothingSent(t *testing.T) {
	// [AC-199-1-2] a member that cannot be sent: no commit confirmed anywhere.
	t.Parallel()
	p, srvs := groupLab(t, netconf.ApplyConfig{}, nil)
	srvs["sw2"].SetOpError("edit-config", "invalid-value")
	ge := groupError(t, p.Apply(context.Background(), groupPlan(t, p, sameConfig(declared))))
	for _, h := range groupHosts {
		if strings.Contains(strings.Join(srvs[h].Ops(), " "), "commit") {
			t.Fatalf("[AC-199-1-2] %s: %v", h, srvs[h].Ops())
		}
		if ge.Members[h+"/if-uplink"].Outcome != provider.CommitNotCommitted {
			t.Fatalf("[AC-199-1-2] %s: %+v", h, ge.Members[h+"/if-uplink"])
		}
	}
	if m := ge.Members["sw2/if-uplink"]; !isPermanent(m.Err) || !strings.Contains(m.Err.Error(), "edit-config") {
		t.Fatalf("[AC-199-1-2] sw2: %v", m.Err)
	}
}

func TestCommitGroup_Validate(t *testing.T) {
	// [AC-199-1-5]
	p, _ := groupLab(t, netconf.ApplyConfig{}, nil)
	if err := p.ValidateIntent("sw1/if-uplink", map[string]string{"config": declared, "commit_group": "Bad Name"}); err == nil {
		t.Fatalf("[AC-199-1-5] a bad commit_group name must be refused")
	}
	if err := p.ValidateCommitGroup("g", []string{"sw1/if-absent", "sw1/if-uplink"}); err == nil || !strings.Contains(err.Error(), "same host sw1") {
		t.Fatalf("[AC-199-1-5] two members on one host: %v", err)
	}
	if err := p.ValidateCommitGroup("g", []string{"sw1/if-uplink", "sw2/if-uplink"}); err != nil {
		t.Fatalf("[AC-199-1-5] %v", err)
	}
	if err := p.AllowApply(netconf.ApplyConfig{HostConfirmTimeouts: map[string]time.Duration{"nope": 3 * time.Second}}); err == nil {
		t.Fatalf("[AC-199-1-5] a confirm timeout of an unknown host must be refused")
	}
}
