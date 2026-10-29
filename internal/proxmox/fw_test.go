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

package proxmox_test

// fw_test.go — proxmox-fw provider（VM の FW の規則、Backlog #197）。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/cause"
	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/proxmox"
	"github.com/tjst-t/sakutei/v2/internal/proxmox/pvetest"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

const (
	fwID   = "lab/gw1/fw"
	ruleA  = "in ACCEPT iface=net1 source=172.16.1.0/24 dest=172.16.0.11 proto=tcp dport=443 comment=サービス側は 443 だけ"
	ruleB  = "out ACCEPT iface=net0 source=10.0.0.1 dest=10.0.0.10,10.0.0.11 proto=udp dport=53,123 comment=DNS と NTP"
	ruleC  = "out ACCEPT iface=net0 source=10.0.0.1 dest=10.0.0.10 proto=tcp dport=88 comment=KDC"
	handIn = "in DROP proto=tcp dport=22 comment=hand"
)

type fwEnv struct {
	srv *pvetest.Server
	p   *proxmox.FWProvider
}

func newFWEnv(t *testing.T) *fwEnv {
	t.Helper()
	srv := pvetest.New(pvetest.Options{})
	t.Cleanup(srv.Close)
	srv.AddVM(pvetest.VM{VMID: 100, Node: "pve1", Name: "sakutei-ctl", Status: "running"})
	srv.AddVM(pvetest.VM{VMID: 9000, Node: "pve1", Name: "debian-tmpl", Template: true})
	srv.AddVM(pvetest.VM{VMID: 201, Node: "pve1", Name: "gw1", Status: "running"})
	c, err := proxmox.NewClient(proxmox.ClientConfig{
		Endpoint: srv.URL, TokenID: srv.TokenID(), Timeout: 2 * time.Second,
		Secret: func(context.Context) (string, error) { return srv.Secret(), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	p := proxmox.NewFW(map[string]proxmox.Cluster{"lab": {Client: c, Node: "pve1", ProtectedVMIDs: []int{100}, ProtectedNames: []string{"sakutei-ctl"}}})
	return &fwEnv{srv: srv, p: p}
}

// hand は手で置いた規則（印の無い comment）。
func hand(typ, action, comment string, kv ...string) map[string]string {
	m := map[string]string{"type": typ, "action": action, "enable": "1", "comment": comment}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

// plan は観測と rules の宣言から計画を作る（core の Plan と同じ道）。
func (e *fwEnv) plan(t *testing.T, rules string) (core.Plan, core.Observation) {
	t.Helper()
	obs, err := e.p.Read(context.Background(), fwID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	eng := diff.ForProvider(e.p)
	pl, err := eng.Plan(obs, diff.DesiredResource{ID: core.NewID(proxmox.FWName, core.NewScope(nil), fwID), Fields: map[string]string{proxmox.FieldRules: rules}}, cause.CauseContext{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return pl, obs
}

func (e *fwEnv) converge(t *testing.T, rules string) {
	t.Helper()
	pl, _ := e.plan(t, rules)
	if err := e.p.Apply(provider.WithWrite(context.Background()), pl); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if pl2, obs := e.plan(t, rules); len(pl2.Ops) != 0 {
		t.Fatalf("not converged after one Apply: rules %v; ops %+v", obs.State[proxmox.FieldRules].Value, pl2.Ops)
	}
}

// texts は偽サーバの規則の並び（comment は Proxmox の値のまま）。
func (e *fwEnv) texts(t *testing.T) []string {
	t.Helper()
	vm, _ := e.srv.VM(201)
	var out []string
	for _, r := range vm.Rules {
		s := r["type"] + " " + r["action"]
		for _, k := range []string{"iface", "dport", "enable"} {
			if r[k] != "" {
				s += " " + k + "=" + r[k]
			}
		}
		out = append(out, s+" #"+r["comment"])
	}
	return out
}

func TestFW_ConvergeKeepsHandRulesAndOrder(t *testing.T) {
	e := newFWEnv(t)
	e.srv.SetRules(201, []map[string]string{hand("in", "DROP", "hand", "proto", "tcp", "dport", "22")})

	// 空から 3 つ：先頭に宣言の順、手の規則は後ろに残る。作る規則は enable=1。
	pl, _ := e.plan(t, ruleA+" ; "+ruleB+" ; "+ruleC)
	if len(pl.Ops) != 1 || !pl.Ops[0].Destructive {
		t.Fatalf("plan from empty = %+v; want one destructive op (a firewall change, §13)", pl.Ops)
	}
	e.converge(t, ruleA+" ; "+ruleB+" ; "+ruleC)
	got := strings.Join(e.texts(t), "\n")
	want := strings.Join([]string{
		"in ACCEPT iface=net1 dport=443 enable=1 #sakutei: サービス側は 443 だけ",
		"out ACCEPT iface=net0 dport=53,123 enable=1 #sakutei: DNS と NTP",
		"out ACCEPT iface=net0 dport=88 enable=1 #sakutei: KDC",
		"in DROP dport=22 enable=1 #hand",
	}, "\n")
	if got != want {
		t.Fatalf("rules:\n%s\nwant:\n%s", got, want)
	}

	// 並べ替え・1 つ外す・1 つ足す（C, A, 新しい D）。手の規則は残る。
	ruleD := "in ACCEPT iface=net1 proto=icmp comment=ping"
	e.converge(t, ruleC+" ; "+ruleA+" ; "+ruleD)
	got = strings.Join(e.texts(t), "\n")
	want = strings.Join([]string{
		"out ACCEPT iface=net0 dport=88 enable=1 #sakutei: KDC",
		"in ACCEPT iface=net1 dport=443 enable=1 #sakutei: サービス側は 443 だけ",
		"in ACCEPT iface=net1 enable=1 #sakutei: ping",
		"in DROP dport=22 enable=1 #hand",
	}, "\n")
	if got != want {
		t.Fatalf("rules after reorder:\n%s\nwant:\n%s", got, want)
	}

	// 手の規則が先頭に入る・印の規則を手で変える → drift（順も）で、戻すと手の規則は後ろへ。
	vm, _ := e.srv.VM(201)
	rules := append([]map[string]string{hand("in", "ACCEPT", "hand top")}, vm.Rules...)
	rules[2]["dport"] = "444"
	e.srv.SetRules(201, rules)
	pl, obs := e.plan(t, ruleC+" ; "+ruleA+" ; "+ruleD)
	if len(pl.Ops) != 1 || !strings.Contains(obs.State[proxmox.FieldRules].Value.(string), "(unmanaged) in ACCEPT comment=hand top") {
		t.Fatalf("after the hand changes: ops %+v, observed %v", pl.Ops, obs.State[proxmox.FieldRules].Value)
	}
	if n := obs.State[proxmox.FieldUnmanaged].Value; n != int64(2) {
		t.Errorf("unmanaged = %v, want 2", n)
	}
	e.converge(t, ruleC+" ; "+ruleA+" ; "+ruleD)
	got = strings.Join(e.texts(t), "\n")
	if !strings.HasSuffix(got, "in ACCEPT enable=1 #hand top\nin DROP dport=22 enable=1 #hand") || strings.Contains(got, "444") {
		t.Fatalf("rules after putting back:\n%s", got)
	}

	// rules を空に：印の規則だけを外す。
	e.converge(t, "")
	if got := e.texts(t); len(got) != 2 {
		t.Fatalf("rules after emptying: %v; want the 2 hand rules", got)
	}
}

func TestFW_ListShowsHandRulesAsUnmanaged(t *testing.T) {
	e := newFWEnv(t)
	e.srv.AddVM(pvetest.VM{VMID: 202, Node: "pve1", Name: "other"})
	e.srv.SetRules(202, []map[string]string{hand("in", "ACCEPT", "x"), hand("out", "DROP", "y")})
	e.srv.SetRules(100, []map[string]string{hand("in", "ACCEPT", "protected")})
	e.converge(t, ruleA)
	e.srv.SetRules(201, append(func() []map[string]string { vm, _ := e.srv.VM(201); return vm.Rules }(), hand("in", "DROP", "z")))
	res, err := e.p.List(context.Background(), core.NewScope(nil))
	if err != nil || res.Status != core.ListComplete {
		t.Fatalf("List: %v %v", res, err)
	}
	var ids []string
	for _, id := range res.IDs {
		ids = append(ids, id.Local)
	}
	if got := strings.Join(ids, " "); got != "lab/gw1/fw/1 lab/gw1/fw lab/other/fw/0 lab/other/fw/1" {
		t.Errorf("List = %s", got)
	}
	obs, err := e.p.Read(context.Background(), "lab/other/fw/1")
	if err != nil || obs.State[proxmox.FieldRule].Value != "out DROP comment=y" || obs.State[proxmox.FieldPos].Value != int64(1) {
		t.Errorf("Read of an unmanaged rule = %+v, %v", obs.State, err)
	}
	if err := e.p.ValidateIntent("lab/other/fw/1", map[string]string{proxmox.FieldRules: ""}); err == nil || !strings.Contains(err.Error(), "did not place") {
		t.Errorf("declaring an unmanaged rule's id: %v", err)
	}
	if obs, err := e.p.Read(context.Background(), "lab/other/fw/9"); err != nil || obs.State != nil {
		t.Errorf("Read past the end = %+v, %v; want absent", obs, err)
	}
}

func TestFW_ValidateAndRefuse(t *testing.T) {
	e := newFWEnv(t)
	for _, c := range []struct {
		id     string
		fields map[string]string
		want   string
	}{
		{"lab/gw1", map[string]string{proxmox.FieldRules: ruleA}, "want <cluster>/<vm name>/fw"},
		{"nope/gw1/fw", map[string]string{proxmox.FieldRules: ruleA}, "no cluster"},
		{"lab/sakutei-ctl/fw", map[string]string{proxmox.FieldRules: ruleA}, "protected"},
		{fwID, map[string]string{proxmox.FieldRules: "in ALLOW"}, "action"},
		{fwID, map[string]string{proxmox.FieldRules: ruleA, "unmanaged": "0"}, "only rules"},
	} {
		if err := e.p.ValidateIntent(c.id, c.fields); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("ValidateIntent(%s, %v) = %v, want %q", c.id, c.fields, err, c.want)
		}
	}
	if err := e.p.ValidateIntent(fwID, map[string]string{proxmox.FieldRules: "IN accept proto=tcp dport=1"}); err != nil {
		t.Errorf("ValidateIntent(lower case) = %v", err)
	}
	// 書き方の違いは drift にしない。
	e.converge(t, ruleA)
	if pl, _ := e.plan(t, strings.Replace(ruleA, "in ACCEPT", "IN accept", 1)); len(pl.Ops) != 0 {
		t.Errorf("a differently written same rule is a change: %+v", pl.Ops)
	}

	// VM がまだ無い：観測は「無い」、Apply は一時の失敗。
	obs, err := e.p.Read(context.Background(), "lab/later/fw")
	if err != nil || obs.Availability != core.AvailabilityOk || obs.State != nil {
		t.Errorf("Read of a missing VM = %+v, %v; want absent", obs, err)
	}
	op := core.Op{Kind: core.OpKindCreate, Target: core.NewID(proxmox.FWName, core.NewScope(nil), "lab/later/fw"),
		Fields: []core.FieldDiff{{Field: proxmox.FieldRules, To: ruleA}}}
	err = e.p.Apply(provider.WithWrite(context.Background()), core.Plan{Ops: []core.Op{op}})
	var te *provider.TransientError
	if !errors.As(err, &te) || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("Apply for a missing VM = %v; want a transient error", err)
	}

	// 届かないときは unknown（無いとは言わない）。
	e.srv.SetUnreachable(true)
	if obs, err := e.p.Read(context.Background(), fwID); err == nil && obs.Availability == core.AvailabilityOk {
		t.Errorf("Read while unreachable = %+v", obs)
	}
	e.srv.SetUnreachable(false)
}

func TestFW_DeleteRemovesOnlyMarkedRules(t *testing.T) {
	e := newFWEnv(t)
	e.converge(t, ruleA+" ; "+ruleB)
	vm, _ := e.srv.VM(201)
	e.srv.SetRules(201, append(vm.Rules, hand("in", "ACCEPT", "kept")))
	op := core.Op{Kind: core.OpKindDelete, Target: core.NewID(proxmox.FWName, core.NewScope(nil), fwID), Destructive: true}
	if err := e.p.Apply(provider.WithWrite(context.Background()), core.Plan{Ops: []core.Op{op}}); err != nil {
		t.Fatal(err)
	}
	if got := e.texts(t); len(got) != 1 || got[0] != "in ACCEPT enable=1 #kept" {
		t.Errorf("after delete: %v", got)
	}
}

// 手で宣言した規則の comment にタブ・続く空白・長すぎる文があっても、宣言どおり
// に置けば読み戻しと同じになり、次の周は drift にならない（毎周の PUT・予算の
// 消費・oscillating を起こさない）。
func TestFW_DeclaredCommentIsNotPerpetualDrift(t *testing.T) {
	e := newFWEnv(t)
	rules := "in ACCEPT proto=tcp dport=22 comment=管理\tの  口 ; " +
		"out ACCEPT proto=udp dport=53 comment=  前後の   空白  ; " +
		"in ACCEPT proto=tcp dport=443 comment=" + strings.Repeat("とても長い理由", 20)
	e.converge(t, rules)
	before := e.srv.Calls("fwrules")
	for i := 0; i < 3; i++ {
		if pl, obs := e.plan(t, rules); len(pl.Ops) != 0 {
			t.Fatalf("cycle %d: drift after converging: observed %v; ops %+v", i, obs.State[proxmox.FieldRules].Value, pl.Ops)
		}
	}
	if got := e.srv.Calls("fwrules") - before; got != 3 {
		t.Errorf("rule calls in 3 converged cycles = %d, want 3 reads and no writes", got)
	}
	if got := e.texts(t); len(got) != 3 || !strings.HasSuffix(got[0], "#sakutei: 管理 の 口") || !strings.HasSuffix(got[2], "…") {
		t.Errorf("rules = %v", got)
	}
}
