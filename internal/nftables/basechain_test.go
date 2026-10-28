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

package nftables_test

// basechain_test.go — 名指しした base chain と締め出しの守り（Backlog #173）を
// 偽の nft と偽の Guard で確かめる。
//
// Tags:
//   [AC-173-1-*] 宣言した base chain を読む・List に出さない・宣言を検査する
//   [AC-173-2-*] live の Apply: policy・ルール・作り直し、締め出しの守り
//   [AC-173-3-*] file の base chain

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/nftables"
	"github.com/tjst-t/sakutei/v2/internal/nftables/nftfake"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// sshBlocked は「input の policy が drop で、ssh を通すルールが無い」とき届かない。
func sshBlocked(n *nftfake.NFT) bool {
	return n.Policy("inet", "filter", "input") == "drop" && !n.HasRule("inet", "filter", "input", "tcp dport 22 accept")
}

func newGuarded(t *testing.T, fake *nftfake.NFT, guard nftables.Guard, after time.Duration) *nftables.LiveProvider {
	t.Helper()
	cfg := nftables.LiveConfig{Runner: fake, Tables: []nftables.TableRef{{Family: "inet", Name: "filter"}}, RollbackAfter: after}
	if guard != nil {
		cfg.Guard = guard
	}
	p, err := nftables.NewLive(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.ValidateProvider(p); err != nil {
		t.Fatalf("ValidateProvider: %v", err)
	}
	return p
}

func baseOp(kind core.OpKind, local string, fields map[string]string) core.Op {
	op := core.Op{Kind: kind, Target: core.NewID(nftables.ProviderName, core.NewScope(nil), local)}
	for _, k := range []string{"family", "table", "chain", "rules", "type", "hook", "priority", "policy"} {
		if v, ok := fields[k]; ok {
			op.Fields = append(op.Fields, core.FieldDiff{Field: k, To: v})
		}
	}
	return op
}

func stateOf(t *testing.T, p provider.Provider, local string) core.State {
	t.Helper()
	obs, err := p.Read(context.Background(), local)
	if err != nil {
		t.Fatalf("Read %s: %v", local, err)
	}
	if obs.Availability != core.AvailabilityOk {
		t.Fatalf("Read %s: availability %v", local, obs.Availability)
	}
	return obs.State
}

func field(st core.State, k string) string {
	v, ok := st[k]
	if !ok {
		return ""
	}
	s, _ := v.Value.(string)
	return s
}

func TestBase_ReadButNotListed(t *testing.T) {
	// [AC-173-1-1] base chain は type・hook・priority（数）・policy 付きで読める
	// （Read は宣言したものにだけ呼ばれる）。List・BulkObserve には出さない
	// （宣言していない base chain を status の宣言外に出さない）。
	fake := &nftfake.NFT{}
	fake.AddBaseChain("inet", "filter", "input", "type filter hook input priority filter; policy drop;", "tcp dport 22 accept")
	fake.AddBaseChain("inet", "filter", "output", "type filter hook output priority 0; policy accept;")
	fake.AddChain("inet", "filter", "web", "tcp dport 80 accept")
	p := newGuarded(t, fake, &nftfake.Guard{NFT: fake}, time.Second)

	st := stateOf(t, p, "inet/filter/input")
	for k, want := range map[string]string{"type": "filter", "hook": "input", "priority": "0", "policy": "drop", "rules": "tcp dport 22 accept"} {
		if got := field(st, k); got != want {
			t.Errorf("input %s = %q, want %q", k, got, want)
		}
	}
	if st := stateOf(t, p, "inet/filter/web"); field(st, "policy") != "" || field(st, "hook") != "" {
		t.Errorf("a regular chain has base fields: %v", st)
	}
	lr, err := p.List(context.Background(), core.NewScope(nil))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, id := range lr.IDs {
		ids = append(ids, id.Local)
	}
	if strings.Join(ids, ",") != "inet/filter/web" {
		t.Errorf("List = %v, want only the regular chain", ids)
	}
	bo, err := p.BulkObserve(context.Background(), core.NewScope(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(bo.Observations) != 1 {
		t.Errorf("BulkObserve has %d entries, want 1", len(bo.Observations))
	}
}

func TestBase_RegularDeclarationOfABaseChainIsRefused(t *testing.T) {
	// [AC-173-1-4] 宣言が regular chain（type・hook・priority・policy なし）で現実が
	// base chain なら、計画の時点で断る（policy を言わずにルールを変えさせない）。
	fake := &nftfake.NFT{}
	fake.AddBaseChain("inet", "filter", "input", "type filter hook input priority filter; policy accept;", "tcp dport 22 accept")
	p := newGuarded(t, fake, nil, time.Second)
	st := stateOf(t, p, "inet/filter/input")
	ops := []core.Op{chainOp(core.OpKindUpdate, "inet/filter/input", "tcp dport 80 accept")}
	_, err := p.RefinePlan("inet/filter/input", st, map[string]string{"family": "inet", "table": "filter", "chain": "input", "rules": "tcp dport 80 accept"}, ops)
	if err == nil || !strings.Contains(err.Error(), "is a base chain") {
		t.Fatalf("want the declaration refused, got %v", err)
	}
	full := map[string]string{"family": "inet", "table": "filter", "chain": "input", "rules": "tcp dport 80 accept",
		"type": "filter", "hook": "input", "priority": "filter", "policy": "accept"}
	if _, err := p.RefinePlan("inet/filter/input", st, full, ops); err != nil {
		t.Fatalf("a base chain declaration: %v", err)
	}
}

func TestBase_ValidateIntent(t *testing.T) {
	// [AC-173-1-2] base chain の宣言（4 つのどれかを書いた）は 4 つとも要り、値の
	// 形を見る。priority は名前でも数でもよい。regular chain の宣言は今までどおり。
	fake := &nftfake.NFT{}
	p := newGuarded(t, fake, nil, time.Second)
	ok := map[string]string{"family": "inet", "table": "filter", "chain": "input", "rules": "tcp dport 22 accept",
		"type": "filter", "hook": "input", "priority": "filter + 10", "policy": "drop"}
	if err := p.ValidateIntent("inet/filter/input", ok); err != nil {
		t.Fatalf("a good base chain: %v", err)
	}
	bad := []struct {
		id  string
		set map[string]string
	}{
		{"inet/filter/input", map[string]string{"policy": ""}},
		{"inet/filter/input", map[string]string{"policy": "reject"}},
		{"inet/filter/input", map[string]string{"hook": "ingress"}},
		{"inet/filter/input", map[string]string{"type": "weird"}},
		{"inet/filter/input", map[string]string{"priority": "nosuch"}},
		{"inet/filter/input", map[string]string{"priority": "filter * 2"}},
		{"inet/filter/web", map[string]string{"chain": "web", "type": "", "hook": "", "priority": ""}}, // policy only
	}
	for _, b := range bad {
		f := map[string]string{}
		for k, v := range ok {
			f[k] = v
		}
		for k, v := range b.set {
			f[k] = v
		}
		if err := p.ValidateIntent(b.id, f); err == nil {
			t.Errorf("%s %v: want an error", b.id, b.set)
		}
	}
	if err := p.ValidateIntent("inet/filter/web", map[string]string{"family": "inet", "table": "filter", "chain": "web", "rules": "tcp dport 80 accept"}); err != nil {
		t.Errorf("a regular chain: %v", err)
	}
}

func TestBase_PriorityNamesAreNotDrift(t *testing.T) {
	// [AC-173-1-3] priority "filter" と観測の "0" は同じ（PlanRefiner）。
	p := newGuarded(t, &nftfake.NFT{}, nil, time.Second)
	ops := []core.Op{{Kind: core.OpKindUpdate, Fields: []core.FieldDiff{
		{Field: "priority", From: "0", To: "filter"},
		{Field: "priority", From: "10", To: "filter + 10"},
		{Field: "priority", From: "0", To: "mangle"},
	}}}
	out, err := p.RefinePlan("inet/filter/input", nil, nil, ops)
	if err != nil {
		t.Fatal(err)
	}
	if len(out[0].Fields) != 1 || out[0].Fields[0].To != "mangle" {
		t.Errorf("refined fields = %v, want only mangle (-150) left", out[0].Fields)
	}
}

func TestBase_ApplyConfirmedPolicyDrop(t *testing.T) {
	// [AC-173-2-1] policy drop と ssh を通すルールを宣言 → 写しを取り、仕掛けを
	// 置き、流し、届くので仕掛けを取り消す。policy は drop のまま。
	fake := &nftfake.NFT{}
	fake.AddBaseChain("inet", "filter", "input", "type filter hook input priority filter; policy accept;")
	fake.AddChain("inet", "filter", "web", "tcp dport 80 accept")
	guard := &nftfake.Guard{NFT: fake, Unreachable: sshBlocked}
	p := newGuarded(t, fake, guard, 200*time.Millisecond)
	op := baseOp(core.OpKindUpdate, "inet/filter/input", map[string]string{"policy": "drop", "rules": "tcp dport 22 accept ; jump web"})
	if err := p.Apply(context.Background(), core.Plan{Ops: []core.Op{op}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	arms, disarms, fires := guard.Counts()
	if arms != 1 || disarms != 1 || fires != 0 {
		t.Errorf("arms/disarms/fires = %d/%d/%d, want 1/1/0", arms, disarms, fires)
	}
	restore := guard.Restores()[0]
	for _, want := range []string{"add table inet filter\ndelete table inet filter\n", "table inet filter {", "policy accept;", "tcp dport 80 accept"} {
		if !strings.Contains(restore, want) {
			t.Errorf("the copy does not have %q:\n%s", want, restore)
		}
	}
	if strings.Contains(restore, "# handle") {
		t.Errorf("the copy has handles:\n%s", restore)
	}
	time.Sleep(300 * time.Millisecond) // past the rollback: it was cancelled
	st := stateOf(t, p, "inet/filter/input")
	if field(st, "policy") != "drop" || field(st, "rules") != "tcp dport 22 accept ; jump web" {
		t.Errorf("after Apply: %v", st)
	}
}

func TestBase_ApplyNotConfirmedRollsBack(t *testing.T) {
	// [AC-173-2-2] ssh を塞ぐ宣言 → 新しい接続で届かない → 仕掛けを取り消さず、
	// 上限で写しに戻り、Apply は RolledBackError（一時的な失敗）。
	fake := &nftfake.NFT{}
	fake.AddBaseChain("inet", "filter", "input", "type filter hook input priority filter; policy accept;", "tcp dport 22 accept")
	before := fake.Dump()
	guard := &nftfake.Guard{NFT: fake, Unreachable: sshBlocked}
	p := newGuarded(t, fake, guard, 200*time.Millisecond)
	op := baseOp(core.OpKindUpdate, "inet/filter/input", map[string]string{"policy": "drop", "rules": "tcp dport 80 accept"})
	start := time.Now()
	err := p.Apply(context.Background(), core.Plan{Ops: []core.Op{op}})
	if !provider.IsRolledBack(err) || !isTransient(err) {
		t.Fatalf("Apply: want a transient RolledBackError, got %v", err)
	}
	if !strings.Contains(err.Error(), "could not be reached over a new connection") {
		t.Errorf("the error does not say why: %v", err)
	}
	if time.Since(start) < 200*time.Millisecond {
		t.Errorf("Apply returned before the rollback (%s)", time.Since(start))
	}
	arms, disarms, fires := guard.Counts()
	if arms != 1 || disarms != 0 || fires != 1 {
		t.Errorf("arms/disarms/fires = %d/%d/%d, want 1/0/1", arms, disarms, fires)
	}
	if got := fake.Dump(); got != before {
		t.Errorf("the ruleset is not back:\n%s\nwant:\n%s", got, before)
	}
}

func TestBase_RegularChainChangeIsNotGuarded(t *testing.T) {
	// [AC-173-2-3] regular chain だけを変える Apply は今までどおり（守りを通さない）。
	fake := &nftfake.NFT{}
	fake.AddChain("inet", "filter", "web", "tcp dport 80 accept")
	guard := &nftfake.Guard{NFT: fake}
	p := newGuarded(t, fake, guard, time.Second)
	if err := p.Apply(context.Background(), core.Plan{Ops: []core.Op{chainOp(core.OpKindUpdate, "inet/filter/web", "tcp dport 443 accept")}}); err != nil {
		t.Fatal(err)
	}
	if arms, _, _ := guard.Counts(); arms != 0 {
		t.Errorf("a regular chain change armed the guard %d time(s)", arms)
	}
}

func TestBase_NoGuardRefuses(t *testing.T) {
	// [AC-173-2-4] 守りが無い（手元で確かめの宛先を設定していない）と base chain は
	// 変えない（PermanentError、何も流さない）。
	fake := &nftfake.NFT{}
	fake.AddBaseChain("inet", "filter", "input", "type filter hook input priority filter; policy accept;")
	before := fake.Dump()
	p := newGuarded(t, fake, nil, time.Second)
	err := p.Apply(context.Background(), core.Plan{Ops: []core.Op{baseOp(core.OpKindUpdate, "inet/filter/input", map[string]string{"policy": "drop"})}})
	if !isPermanent(err) || !strings.Contains(err.Error(), "ruleset_confirm_address") {
		t.Fatalf("want a PermanentError naming ruleset_confirm_address, got %v", err)
	}
	if fake.Dump() != before {
		t.Error("the ruleset changed")
	}
}

func TestBase_CreateRecreateDelete(t *testing.T) {
	// [AC-173-2-5] 無い base chain を作る（table も）・priority を変える（作り直す）・
	// policy だけを変える（ルールはそのまま）・消す。どれも守りを通す。
	fake := &nftfake.NFT{}
	guard := &nftfake.Guard{NFT: fake}
	p := newGuarded(t, fake, guard, 200*time.Millisecond)
	ctx := context.Background()
	create := baseOp(core.OpKindCreate, "inet/filter/input", map[string]string{"family": "inet", "table": "filter", "chain": "input",
		"rules": "tcp dport 22 accept", "type": "filter", "hook": "input", "priority": "filter", "policy": "drop"})
	if err := p.Apply(ctx, core.Plan{Ops: []core.Op{create}}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if st := stateOf(t, p, "inet/filter/input"); field(st, "policy") != "drop" || field(st, "priority") != "0" || field(st, "rules") != "tcp dport 22 accept" {
		t.Fatalf("after create: %v", st)
	}
	if err := p.Apply(ctx, core.Plan{Ops: []core.Op{baseOp(core.OpKindUpdate, "inet/filter/input", map[string]string{"priority": "10"})}}); err != nil {
		t.Fatalf("priority: %v", err)
	}
	if st := stateOf(t, p, "inet/filter/input"); field(st, "priority") != "10" || field(st, "rules") != "tcp dport 22 accept" || field(st, "policy") != "drop" {
		t.Fatalf("after priority: %v", st)
	}
	if err := p.Apply(ctx, core.Plan{Ops: []core.Op{baseOp(core.OpKindUpdate, "inet/filter/input", map[string]string{"policy": "accept"})}}); err != nil {
		t.Fatalf("policy: %v", err)
	}
	if st := stateOf(t, p, "inet/filter/input"); field(st, "policy") != "accept" || field(st, "rules") != "tcp dport 22 accept" {
		t.Fatalf("after policy: %v", st)
	}
	if err := p.Apply(ctx, core.Plan{Ops: []core.Op{baseOp(core.OpKindDelete, "inet/filter/input", nil)}}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if st := stateOf(t, p, "inet/filter/input"); st != nil {
		t.Fatalf("after delete: %v", st)
	}
	if arms, disarms, fires := guard.Counts(); arms != 4 || disarms != 4 || fires != 0 {
		t.Errorf("arms/disarms/fires = %d/%d/%d, want 4/4/0", arms, disarms, fires)
	}
}

func TestBase_ArmFailureChangesNothing(t *testing.T) {
	// [AC-173-2-6] 仕掛けを置けなければ流さない（一時的な失敗）。
	fake := &nftfake.NFT{}
	fake.AddBaseChain("inet", "filter", "input", "type filter hook input priority filter; policy accept;")
	before := fake.Dump()
	guard := &nftfake.Guard{NFT: fake, ArmErr: os.ErrPermission}
	p := newGuarded(t, fake, guard, time.Second)
	err := p.Apply(context.Background(), core.Plan{Ops: []core.Op{baseOp(core.OpKindUpdate, "inet/filter/input", map[string]string{"policy": "drop"})}})
	if !isTransient(err) || provider.IsRolledBack(err) {
		t.Fatalf("want a transient error (not rolled back), got %v", err)
	}
	if fake.Dump() != before {
		t.Error("the ruleset changed")
	}
}

func TestBase_File(t *testing.T) {
	// [AC-173-3-1] file: 宣言した base chain の type 行とルールを読み書きする。
	// ほかの base chain・ほかの行は変えない。base chain は List に出さない。
	path := filepath.Join(t.TempDir(), "fw.rules")
	orig := "table inet filter {\n\tchain input {\n\t\ttype filter hook input priority filter; policy accept;\n\t\ttcp dport 22 accept\n\t}\n\tchain output {\n\t\ttype filter hook output priority 0; policy accept;\n\t}\n}\n"
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	p := nftables.New(path)
	st := stateOf(t, p, "inet/filter/input")
	if field(st, "priority") != "0" || field(st, "policy") != "accept" || field(st, "hook") != "input" || field(st, "rules") != "tcp dport 22 accept" {
		t.Fatalf("Read: %v", st)
	}
	if lr, err := p.List(context.Background(), core.NewScope(nil)); err != nil || len(lr.IDs) != 0 {
		t.Errorf("List = %v, %v; want no base chain", lr.IDs, err)
	}
	op := baseOp(core.OpKindUpdate, "inet/filter/input", map[string]string{"policy": "drop", "rules": "tcp dport 22 accept ; ct state established,related accept"})
	if err := p.Apply(context.Background(), core.Plan{Ops: []core.Op{op}}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	want := "table inet filter {\n\tchain input {\n\t\ttype filter hook input priority filter; policy drop;\n\t\ttcp dport 22 accept\n\t\tct state established,related accept\n\t}\n\tchain output {\n\t\ttype filter hook output priority 0; policy accept;\n\t}\n}\n"
	if string(got) != want {
		t.Errorf("file:\n%s\nwant:\n%s", got, want)
	}
	if err := p.ValidateIntent("inet/filter/web", map[string]string{"policy": "drop"}); err == nil {
		t.Error("a policy without type, hook and priority: want an error")
	}
	if _, err := p.RefinePlan("inet/filter/output", stateOf(t, p, "inet/filter/output"), map[string]string{"rules": ""}, nil); err == nil {
		t.Error("a regular declaration of a base chain: want an error")
	}
}
