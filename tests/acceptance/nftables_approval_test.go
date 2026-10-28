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

package acceptance

// nftables_approval_test.go — #179: base chain の変更の確かめる先を宣言に書き、
// 変更の前にも確かめる。base chain を作る・type/hook/priority/policy を変える
// 変更は壊す変更として予算に数え、承認ゲート（planned_change のレーン）を通す。
// 偽の nft（nftfake.NFT）と偽の守り（nftfake.Guard）で。
//
//	[AC-179-1]  plan: base chain の作成・policy の変更・hook の変更（作り直し）は
//	            destructive で承認の要る op（理由つき、plan の JSON に
//	            approval_required）。ルールだけの変更・regular chain の変更は
//	            今までどおり。confirm だけの差分は収束。
//	[AC-179-2]  常駐: レーンが無い・承認の無いレーンでは流さない
//	            （ChangeAwaitingApproval、守りは仕掛けない）。承認したレーンで
//	            流し、宣言した確かめる先のすべてを前と後に確かめる。
//	[AC-179-3]  常駐: 承認したレーンがあっても、壊す変更の予算が尽きていれば
//	            base chain は流さない。regular chain の変更は流れる。
//	[AC-179-4]  前の確かめ: 宣言した先の 1 つに前で届かなければ変えない（一時的
//	            な失敗、仕掛けを置かない、ruleset はそのまま、届かない先を名指し）。
//	[AC-179-5]  後の確かめ: 宣言した複数の先の 1 つが変えた後に届かなければ戻す
//	            （RolledBackError、ほかの先は届いていても）。
//	[AC-179-6]  読み込み: confirm は base chain の宣言だけ・形が読めること・手元の
//	            live に ssh:// は書けない。

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/budget"
	"github.com/tjst-t/sakutei/v2/internal/cause"
	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/eventlog"
	"github.com/tjst-t/sakutei/v2/internal/nftables"
	"github.com/tjst-t/sakutei/v2/internal/nftables/nftfake"
	"github.com/tjst-t/sakutei/v2/internal/planreport"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// baseFields は base chain の宣言のフィールド。
func baseFields(chain, hook, policy, rules string) map[string]string {
	return map[string]string{
		"family": "inet", "table": "filter", "chain": chain, "rules": rules,
		"type": "filter", "hook": hook, "priority": "filter", "policy": policy,
	}
}

// labLive は離れたホスト fw1（ID は fw1/<family>/<table>/<chain>）の live。
func labLive(t *testing.T, fake *nftfake.NFT, guard nftables.Guard) *nftables.LiveProvider {
	t.Helper()
	p, err := nftables.NewLive(nftables.LiveConfig{
		Tables:        []nftables.TableRef{{Family: "inet", Name: "filter"}},
		Hosts:         []nftables.LiveHost{{Name: "fw1", Runner: fake, Guard: guard}},
		RollbackAfter: 200 * time.Millisecond,
	}, nftables.WithScope(core.NewScope(nftLabScope)))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNFTablesApproval_Plan(t *testing.T) {
	// [AC-179-1]
	fake := &nftfake.NFT{}
	fake.AddBaseChain("inet", "filter", "input", "type filter hook input priority filter; policy accept;", "tcp dport 22 accept")
	fake.AddChain("inet", "filter", "web", "tcp dport 80 accept")
	p := labLive(t, fake, &nftfake.Guard{NFT: fake})
	eng := diff.ForProvider(p)
	plan := func(id string, fields map[string]string) []core.Op {
		t.Helper()
		obs, err := p.Read(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		pl, err := eng.Plan(obs, diff.DesiredResource{ID: core.NewID("nftables", core.NewScope(nftLabScope), id), Fields: fields}, cause.CauseContext{})
		if err != nil {
			t.Fatal(err)
		}
		return pl.Ops
	}
	gated := func(what string, ops []core.Op, kind core.OpKind, reason string) {
		t.Helper()
		if len(ops) != 1 || ops[0].Kind != kind || !ops[0].Destructive || !ops[0].RequiresApproval || !strings.Contains(ops[0].ApprovalReason, reason) {
			t.Fatalf("[AC-179-1] %s: ops = %+v, want one destructive %v requiring approval (%q)", what, ops, kind, reason)
		}
		o := planreport.NewOperation(ops[0], false)
		if !o.ApprovalRequired || o.Absent || !o.Destructive || o.ApprovalReason == "" {
			t.Fatalf("[AC-179-1] %s: plan JSON = %+v, want approval_required (not absent), destructive", what, o)
		}
	}
	free := func(what string, ops []core.Op) {
		t.Helper()
		if len(ops) != 1 || ops[0].Destructive || ops[0].RequiresApproval {
			t.Fatalf("[AC-179-1] %s: ops = %+v, want one plain op", what, ops)
		}
	}

	gated("create", plan("fw1/inet/filter/output", baseFields("output", "output", "drop", "ct state established accept")), core.OpKindCreate, "creates base chain fw1/inet/filter/output")
	gated("policy", plan("fw1/inet/filter/input", baseFields("input", "input", "drop", "tcp dport 22 accept")), core.OpKindUpdate, "policy accept → drop")
	gated("hook", plan("fw1/inet/filter/input", baseFields("input", "forward", "accept", "tcp dport 22 accept")), core.OpKindUpdate, "deleted and recreated")
	gated("regular to base", plan("fw1/inet/filter/web", baseFields("web", "output", "accept", "tcp dport 80 accept")), core.OpKindUpdate, "turns chain fw1/inet/filter/web into a base chain")
	free("rules of a base chain", plan("fw1/inet/filter/input", baseFields("input", "input", "accept", "tcp dport 22 accept ; tcp dport 80 accept")))
	free("regular chain", plan("fw1/inet/filter/web", map[string]string{"family": "inet", "table": "filter", "chain": "web", "rules": "tcp dport 81 accept"}))

	// confirm alone is never observed: converged.
	same := baseFields("input", "input", "accept", "tcp dport 22 accept")
	same[nftables.FieldConfirm] = "self, tcp://192.0.2.10:443"
	if ops := plan("fw1/inet/filter/input", same); len(ops) != 0 {
		t.Fatalf("[AC-179-1] a confirm-only difference must be converged: %+v", ops)
	}
	// With another change, confirm rides along for the Apply.
	same["policy"] = "drop"
	ops := plan("fw1/inet/filter/input", same)
	if len(ops) != 1 || !hasField(ops[0], nftables.FieldConfirm) {
		t.Fatalf("[AC-179-1] confirm must ride along with a change: %+v", ops)
	}
}

func hasField(op core.Op, name string) bool {
	for _, f := range op.Fields {
		if f.Field == name {
			return true
		}
	}
	return false
}

// approvalDaemon は偽の nft・偽の守りの上で常駐を回す。
type approvalDaemon struct {
	t         *testing.T
	intentDir string
	rec       *recordingSink
	out, err  lockedBuf
	stop      func()
}

func startApprovalDaemon(t *testing.T, p *nftables.LiveProvider, intent map[string]any, bc budget.Config) *approvalDaemon {
	t.Helper()
	dir := t.TempDir()
	d := &approvalDaemon{t: t, intentDir: dir + "/intent", rec: &recordingSink{}}
	if err := os.MkdirAll(d.intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIntentFile(t, d.intentDir, intent)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- reconcile.Run(ctx, reconcile.Config{
			IntentDir: d.intentDir, GeneratedDir: dir + "/generated", Provider: p,
			Interval: 20 * time.Millisecond, Out: &d.out, ErrOut: &d.err,
			EventSinks: []eventlog.Sink{d.rec}, StuckAfter: 1000, BudgetConfig: bc,
		})
	}()
	d.stop = func() { cancel(); <-done }
	t.Cleanup(d.stop)
	return d
}

func (d *approvalDaemon) waitFor(what string, cond func() bool) {
	d.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			d.t.Fatalf("timed out waiting for %s; err:\n%s", what, d.err.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// lane は scope nftLabScope の planned_change（approved なら承認つき）。
func lane(kind string, approved bool) map[string]any {
	now := time.Now()
	l := map[string]any{"scope": nftLabScope, "kind": kind, "max_count": 10,
		"window": pcWindow(now.Add(-time.Hour), now.Add(time.Hour))}
	if approved {
		l["approval"] = map[string]any{"pr": 179, "approvers": []string{"alice"}}
	}
	return l
}

func nftIntent(lanes []map[string]any, res ...map[string]any) map[string]any {
	in := map[string]any{"resources": res}
	if len(lanes) > 0 {
		in["planned_change"] = lanes
	}
	return in
}

func nftRes(id string, fields map[string]string) map[string]any {
	return map[string]any{"provider": "nftables", "id": id, "scope": nftLabScope, "fields": fields}
}

func TestNFTablesApproval_DaemonNeedsApproval(t *testing.T) {
	// [AC-179-2]
	fake := &nftfake.NFT{}
	fake.AddBaseChain("inet", "filter", "input", "type filter hook input priority filter; policy accept;", "tcp dport 22 accept")
	guard := &nftfake.Guard{NFT: fake}
	p := labLive(t, fake, guard)
	before := fake.Dump()
	input := baseFields("input", "input", "drop", "tcp dport 22 accept")
	input[nftables.FieldConfirm] = "self, ssh://admin@192.0.2.10:2222, tcp://192.0.2.10:443"
	output := baseFields("output", "output", "drop", "ct state established accept")

	// No lane, then an unapproved one: held, the guard never armed.
	d := startApprovalDaemon(t, p, nftIntent(nil, nftRes("fw1/inet/filter/input", input), nftRes("fw1/inet/filter/output", output)), budget.Config{})
	waitAwaiting := func(n int) {
		d.waitFor("ChangeAwaitingApproval", func() bool { return len(d.rec.firingOf(core.EventChangeAwaitingApproval)) >= n })
	}
	waitAwaiting(2)
	writeIntentFile(t, d.intentDir, nftIntent([]map[string]any{lane("update", false), lane("create", false)},
		nftRes("fw1/inet/filter/input", input), nftRes("fw1/inet/filter/output", output)))
	d.waitFor("the unapproved lanes", func() bool { return strings.Contains(d.out.String()+d.err.String(), "PlannedChangeUnapproved") })
	time.Sleep(200 * time.Millisecond)
	if arms, _, _ := guard.Counts(); arms != 0 || fake.Dump() != before {
		t.Fatalf("[AC-179-2] changed without approval: arms %d, ruleset:\n%s", arms, fake.Dump())
	}
	evs := d.rec.firingOf(core.EventChangeAwaitingApproval)
	if !strings.Contains(evs[0].Message, "planned_change lane of kind") {
		t.Errorf("[AC-179-2] the Event must say which lane opens it: %q", evs[0].Message)
	}

	// Approved lanes: applied, every declared target confirmed before and after.
	writeIntentFile(t, d.intentDir, nftIntent([]map[string]any{lane("update", true), lane("create", true)},
		nftRes("fw1/inet/filter/input", input), nftRes("fw1/inet/filter/output", output)))
	d.waitFor("policy drop on both", func() bool {
		return fake.Policy("inet", "filter", "input") == "drop" && fake.Policy("inet", "filter", "output") == "drop"
	})
	d.waitFor("both guards disarmed", func() bool { _, disarms, _ := guard.Counts(); return disarms == 2 })
	count := map[string]int{}
	for _, c := range guard.Confirms() {
		count[c]++
	}
	// input: its three targets, before and after; output: self, before and after.
	if count["self"] != 4 || count["ssh://admin@192.0.2.10:2222"] != 2 || count["tcp://192.0.2.10:443"] != 2 {
		t.Fatalf("[AC-179-2] confirms = %v, want every declared target before and after", guard.Confirms())
	}
}

func TestNFTablesApproval_DaemonBudget(t *testing.T) {
	// [AC-179-3]
	fake := &nftfake.NFT{}
	fake.AddBaseChain("inet", "filter", "input", "type filter hook input priority filter; policy accept;", "tcp dport 22 accept")
	fake.AddBaseChain("inet", "filter", "output", "type filter hook output priority filter; policy accept;")
	fake.AddChain("inet", "filter", "web", "tcp dport 80 accept")
	guard := &nftfake.Guard{NFT: fake}
	p := labLive(t, fake, guard)
	intent := nftIntent([]map[string]any{lane("update", true)},
		nftRes("fw1/inet/filter/input", baseFields("input", "input", "drop", "tcp dport 22 accept")),
		nftRes("fw1/inet/filter/output", baseFields("output", "output", "drop", "")),
		nftRes("fw1/inet/filter/web", map[string]string{"family": "inet", "table": "filter", "chain": "web", "rules": "tcp dport 81 accept"}))
	d := startApprovalDaemon(t, p, intent, budget.Config{MaxDestructivePerCycle: 1, OnExceed: budget.OnExceedHaltDestructiveOnly})
	d.waitFor("the regular chain", func() bool { return fake.HasRule("inet", "filter", "web", "tcp dport 81 accept") })
	time.Sleep(300 * time.Millisecond)
	if arms, _, _ := guard.Counts(); arms != 0 || fake.Policy("inet", "filter", "input") != "accept" || fake.Policy("inet", "filter", "output") != "accept" {
		t.Fatalf("[AC-179-3] the base chains were changed over the destructive budget: arms %d, input %s, output %s",
			arms, fake.Policy("inet", "filter", "input"), fake.Policy("inet", "filter", "output"))
	}
	if !strings.Contains(d.err.String(), "max_destructive_per_cycle=1") {
		t.Errorf("[AC-179-3] want the budget halt said:\n%s", d.err.String())
	}
}

func TestNFTablesApproval_ConfirmBefore(t *testing.T) {
	// [AC-179-4]
	fake := &nftfake.NFT{}
	fake.AddBaseChain("inet", "filter", "input", "type filter hook input priority filter; policy accept;", "tcp dport 22 accept")
	before := fake.Dump()
	guard := &nftfake.Guard{NFT: fake, UnreachableTo: func(_ *nftfake.NFT, to nftables.ConfirmTarget) bool {
		return to.Kind == nftables.ConfirmSSH // the other path is down already
	}}
	p := labLive(t, fake, guard)
	fields := baseFields("input", "input", "drop", "tcp dport 22 accept")
	fields[nftables.FieldConfirm] = "self, ssh://192.0.2.10:2222"
	err := p.Apply(context.Background(), core.Plan{Ops: []core.Op{createOp("fw1/inet/filter/input", fields)}})
	var te *provider.TransientError
	if !errors.As(err, &te) || !strings.Contains(err.Error(), "before the change") || !strings.Contains(err.Error(), "ssh://192.0.2.10:2222") {
		t.Fatalf("[AC-179-4] Apply = %v, want a transient error naming the target not reached before the change", err)
	}
	if arms, _, _ := guard.Counts(); arms != 0 || fake.Dump() != before {
		t.Fatalf("[AC-179-4] changed although a target was not reached before: arms %d", arms)
	}
}

func TestNFTablesApproval_ConfirmEveryTarget(t *testing.T) {
	// [AC-179-5]
	fake := &nftfake.NFT{}
	fake.AddBaseChain("inet", "filter", "input", "type filter hook input priority filter; policy accept;", "tcp dport 22 accept")
	before := fake.Dump()
	// The declaration lets the management ssh in but shuts out port 443.
	guard := &nftfake.Guard{NFT: fake, UnreachableTo: func(n *nftfake.NFT, to nftables.ConfirmTarget) bool {
		return to.Kind == nftables.ConfirmTCP && n.Policy("inet", "filter", "input") == "drop" && !n.HasRule("inet", "filter", "input", "tcp dport 443 accept")
	}}
	p := labLive(t, fake, guard)
	fields := baseFields("input", "input", "drop", "tcp dport 22 accept")
	fields[nftables.FieldConfirm] = "self, tcp://192.0.2.10:443"
	err := p.Apply(context.Background(), core.Plan{Ops: []core.Op{createOp("fw1/inet/filter/input", fields)}})
	var rb *provider.RolledBackError
	if !errors.As(err, &rb) || !strings.Contains(err.Error(), "tcp://192.0.2.10:443") {
		t.Fatalf("[AC-179-5] Apply = %v, want rolled back naming the target not reached", err)
	}
	if fake.Dump() != before {
		t.Fatalf("[AC-179-5] the ruleset is not back:\n%s", fake.Dump())
	}
	if arms, disarms, fires := guard.Counts(); arms != 1 || disarms != 0 || fires != 1 {
		t.Fatalf("[AC-179-5] arms/disarms/fires = %d/%d/%d, want 1/0/1", arms, disarms, fires)
	}
	// With the port open, every target is reached and the change stays.
	fields["rules"] = "tcp dport 22 accept ; tcp dport 443 accept"
	if err := p.Apply(context.Background(), core.Plan{Ops: []core.Op{createOp("fw1/inet/filter/input", fields)}}); err != nil {
		t.Fatalf("[AC-179-5] Apply: %v", err)
	}
	if fake.Policy("inet", "filter", "input") != "drop" {
		t.Fatal("[AC-179-5] want policy drop")
	}
}

// createOp は fields のすべてを持つ op（Update として、現実の chain を宣言どおりにする）。
func createOp(id string, fields map[string]string) core.Op {
	op := core.Op{Kind: core.OpKindUpdate, Target: core.NewID("nftables", core.NewScope(nftLabScope), id)}
	for k, v := range fields {
		op.Fields = append(op.Fields, core.FieldDiff{Field: k, To: v})
	}
	return op
}

func TestNFTablesApproval_Validate(t *testing.T) {
	// [AC-179-6]
	fake := &nftfake.NFT{}
	remote := labLive(t, fake, nil)
	local, err := nftables.NewLive(nftables.LiveConfig{Runner: fake, Tables: []nftables.TableRef{{Family: "inet", Name: "filter"}}})
	if err != nil {
		t.Fatal(err)
	}
	file := nftables.New(t.TempDir() + "/fw.rules")
	with := func(confirm string) map[string]string {
		f := baseFields("input", "input", "drop", "tcp dport 22 accept")
		f[nftables.FieldConfirm] = confirm
		return f
	}
	regular := map[string]string{"family": "inet", "table": "filter", "chain": "web", "rules": "accept", nftables.FieldConfirm: "self"}
	cases := []struct {
		p       provider.IntentValidator
		id      string
		fields  map[string]string
		wantErr string
	}{
		{local, "inet/filter/input", with("self, tcp://192.0.2.10:443"), ""},
		{local, "inet/filter/input", with("ssh://192.0.2.10"), "local nft has no ssh"},
		{remote, "fw1/inet/filter/input", with("self, ssh://admin@192.0.2.10:2222, tcp://[2001:db8::1]:22"), ""},
		{file, "inet/filter/input", with("ssh://192.0.2.10"), ""},
		{local, "inet/filter/web", regular, "is for a base chain declaration"},
		{local, "inet/filter/input", with("tcp://192.0.2.10"), "needs a port"},
		{local, "inet/filter/input", with("http://192.0.2.10:80"), "want self"},
		{local, "inet/filter/input", with("self, self"), "listed twice"},
		{local, "inet/filter/input", with("self,,tcp://a:1"), "empty entry"},
		{remote, "fw1/inet/filter/input", with("ssh://u:pw@192.0.2.10"), "never a password"},
	}
	for _, c := range cases {
		err := c.p.ValidateIntent(c.id, c.fields)
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("[AC-179-6] %s %q: %v", c.id, c.fields[nftables.FieldConfirm], err)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("[AC-179-6] %s %q: %v, want %q", c.id, c.fields[nftables.FieldConfirm], err, c.wantErr)
		}
	}
}
