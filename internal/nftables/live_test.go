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

// live_test.go — nftables の live 版（Backlog #121）を偽の nft で確かめる。
//
// Tags:
//   [AC-121-1-*] 観測: 空・unknown・管理境界・読む間の変化
//   [AC-121-3-*] Apply: 1 トランザクション・-c で検査・境界・読み戻し
//   [AC-121-2-*] 正規形（PlanRefiner）・ValidateIntent

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/nftables"
	"github.com/tjst-t/sakutei/v2/internal/nftables/nftfake"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

const baseType = "type filter hook input priority filter; policy accept;"

func newLive(t *testing.T, fake *nftfake.NFT, tables ...nftables.TableRef) *nftables.LiveProvider {
	t.Helper()
	if len(tables) == 0 {
		tables = []nftables.TableRef{{Family: "inet", Name: "filter"}}
	}
	p, err := nftables.NewLive(nftables.LiveConfig{Runner: fake, Tables: tables})
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.ValidateProvider(p); err != nil {
		t.Fatalf("ValidateProvider: %v", err)
	}
	return p
}

func chainOp(kind core.OpKind, local, rules string) core.Op {
	op := core.Op{Kind: kind, Target: core.NewID(nftables.ProviderName, core.NewScope(nil), local)}
	if kind != core.OpKindDelete {
		parts := strings.Split(local, "/")
		op.Fields = []core.FieldDiff{{Field: "family", To: parts[0]}, {Field: "table", To: parts[1]}, {Field: "chain", To: parts[2]}, {Field: "rules", To: rules}}
	}
	return op
}

func rulesOf(t *testing.T, p *nftables.LiveProvider, local string) (string, bool) {
	t.Helper()
	obs, err := p.Read(context.Background(), local)
	if err != nil {
		t.Fatalf("Read %s: %v", local, err)
	}
	if obs.Availability != core.AvailabilityOk {
		t.Fatalf("Read %s: availability %v", local, obs.Availability)
	}
	if obs.State == nil {
		return "", false
	}
	return obs.State["rules"].Value.(string), true
}

func TestLive_NewNeedsTables(t *testing.T) {
	if _, err := nftables.NewLive(nftables.LiveConfig{}); err == nil {
		t.Fatal("[AC-121-1-1] NewLive without tables must fail")
	}
	if _, err := nftables.ParseTables("inet/filter, ip/nat"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "filter", "inet/", "foo/filter", "inet/a b", "inet/x,inet/x"} {
		if _, err := nftables.ParseTables(bad); err == nil {
			t.Errorf("[AC-121-1-1] ParseTables(%q) must fail", bad)
		}
	}
}

// [AC-121-1-1] table が無いのは「空」: Read は absent、List は complete で空。
func TestLive_MissingTableIsEmpty(t *testing.T) {
	p := newLive(t, &nftfake.NFT{})
	if _, ok := rulesOf(t, p, "inet/filter/web"); ok {
		t.Fatal("[AC-121-1-1] a chain of a missing table must be absent")
	}
	lr, err := p.List(context.Background(), core.NewScope(nil))
	if err != nil || lr.Status != core.ListComplete || len(lr.IDs) != 0 {
		t.Fatalf("[AC-121-1-1] List = %+v, %v; want complete and empty", lr, err)
	}
	b, err := p.BulkObserve(context.Background(), core.NewScope(nil))
	if err != nil || len(b.Observations) != 0 {
		t.Fatalf("[AC-121-1-1] BulkObserve = %+v, %v", b, err)
	}
}

// [AC-121-1-2] nft が走らない・権限が無い・読む間に変わったのは「観測できない」。
// absent にせず、List は complete を返さない。
func TestLive_UnobservableIsUnknown(t *testing.T) {
	cases := map[string]func(n *nftfake.NFT){
		"nft missing": func(n *nftfake.NFT) { n.Down = errors.New(`exec: "nft": executable file not found in $PATH`) },
		"not root":    func(n *nftfake.NFT) { n.Denied = true },
		"changed while reading": func(n *nftfake.NFT) {
			n.BetweenLists = func(n *nftfake.NFT) { n.AddChain("inet", "filter", "web", "drop") }
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			fake := &nftfake.NFT{}
			fake.AddChain("inet", "filter", "web", "tcp dport 80 accept")
			p := newLive(t, fake)
			breakIt(fake)
			obs, err := p.Read(context.Background(), "inet/filter/web")
			if err == nil || obs.Availability != core.AvailabilityUnknown {
				t.Fatalf("[AC-121-1-2] Read = %+v, %v; want unknown with an error", obs, err)
			}
			if !isTransient(err) {
				t.Errorf("[AC-121-1-2] want a transient error, got %v", err)
			}
			if _, err := p.List(context.Background(), core.NewScope(nil)); err == nil {
				t.Errorf("[AC-121-1-2] List must fail, not return a complete list")
			}
			if _, err := p.BulkObserve(context.Background(), core.NewScope(nil)); err == nil {
				t.Errorf("[AC-121-1-2] BulkObserve must fail")
			}
		})
	}
}

// [AC-121-1-3] 管理境界: base chain・管理しない table は List に出ず、Read・Apply は
// PermanentError。管理しない table は読みもしない。
func TestLive_ManagementBoundary(t *testing.T) {
	fake := &nftfake.NFT{}
	fake.AddSet("inet", "filter", "allowed")
	fake.AddBaseChain("inet", "filter", "input", baseType, "jump web")
	fake.AddChain("inet", "filter", "web", "tcp dport 80 accept")
	fake.AddChain("ip", "other", "x", "accept")
	p := newLive(t, fake)

	lr, err := p.List(context.Background(), core.NewScope(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(lr.IDs) != 1 || lr.IDs[0].Local != "inet/filter/web" {
		t.Fatalf("[AC-121-1-3] List = %v, want only inet/filter/web", lr.IDs)
	}
	for _, c := range fake.Calls() {
		if strings.Contains(strings.Join(c, " "), "other") {
			t.Errorf("[AC-121-1-3] an unmanaged table was read: %v", c)
		}
	}
	// #173: a base chain is read only because it is declared (List does not
	// list it); a regular declaration of it is refused by the PlanRefiner and
	// Apply never changes it without the lockout guard.
	if obs, err := p.Read(context.Background(), "inet/filter/input"); err != nil || obs.State["hook"].Value != "input" {
		t.Errorf("[AC-173-1-1] Read of the base chain = %v, %v", obs.State, err)
	}
	if _, err := p.Read(context.Background(), "ip/other/x"); !isPermanent(err) {
		t.Errorf("[AC-121-1-3] Read ip/other/x: want PermanentError, got %v", err)
	}
	for _, id := range []string{"inet/filter/input", "ip/other/x"} {
		before := fake.Dump()
		err := p.Apply(context.Background(), core.Plan{Ops: []core.Op{chainOp(core.OpKindUpdate, id, "drop")}})
		if !isPermanent(err) {
			t.Errorf("[AC-121-1-3] Apply %s: want PermanentError, got %v", id, err)
		}
		if fake.Dump() != before {
			t.Errorf("[AC-121-1-3] Apply %s changed the ruleset", id)
		}
	}
}

// [AC-121-3-1] 空から Create → 適合。Update はその chain の中身だけを入れ替え、
// base chain・set・ほかの table はそのまま。-c で検査してから 1 回の -f で流す。
func TestLive_CreateUpdateDelete(t *testing.T) {
	fake := &nftfake.NFT{}
	fake.AddSet("inet", "filter", "allowed")
	fake.AddBaseChain("inet", "filter", "input", baseType, "jump web")
	fake.AddChain("ip", "other", "x", "accept")
	p := newLive(t, fake)
	ctx := context.Background()

	if err := p.Apply(ctx, core.Plan{Ops: []core.Op{chainOp(core.OpKindCreate, "inet/filter/web", "tcp dport 80 accept"+nftables.RuleSep+"tcp dport 443 accept")}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := rulesOf(t, p, "inet/filter/web"); got != "tcp dport 80 accept"+nftables.RuleSep+"tcp dport 443 accept" {
		t.Fatalf("[AC-121-3-1] rules after Create = %q", got)
	}
	var checks, applies int
	for _, c := range fake.Calls() {
		switch strings.Join(c, " ") {
		case "-c -f -":
			checks++
		case "-f -":
			if checks == 0 {
				t.Errorf("[AC-121-3-1] -f ran before -c")
			}
			applies++
		}
	}
	if checks != 1 || applies != 1 {
		t.Errorf("[AC-121-3-1] %d checks, %d applies; want 1 each", checks, applies)
	}

	other := fake.Rules("ip", "other", "x")
	input := fake.Rules("inet", "filter", "input")
	if err := p.Apply(ctx, core.Plan{Ops: []core.Op{chainOp(core.OpKindUpdate, "inet/filter/web", "tcp dport 443 accept"+nftables.RuleSep+"tcp dport 80 accept")}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := rulesOf(t, p, "inet/filter/web"); got != "tcp dport 443 accept"+nftables.RuleSep+"tcp dport 80 accept" {
		t.Fatalf("[AC-121-3-1] rules after reorder = %q", got)
	}
	if strings.Join(fake.Rules("ip", "other", "x"), ";") != strings.Join(other, ";") ||
		strings.Join(fake.Rules("inet", "filter", "input"), ";") != strings.Join(input, ";") {
		t.Errorf("[AC-121-3-1] the unmanaged table or the base chain changed")
	}
	if !strings.Contains(fake.Dump(), "set allowed") {
		t.Errorf("[AC-121-3-1] the set is gone")
	}

	// Delete of a chain a base chain jumps to: nft -c refuses (busy) → nothing changes.
	before := fake.Dump()
	err := p.Apply(ctx, core.Plan{Ops: []core.Op{chainOp(core.OpKindDelete, "inet/filter/web", "")}})
	if err == nil || fake.Dump() != before {
		t.Fatalf("[AC-121-3-1] delete of a referenced chain: err=%v, changed=%v", err, fake.Dump() != before)
	}

	// A free chain can be deleted, and deleting it again is a no-op.
	fake.AddChain("inet", "filter", "tmp", "drop")
	for i := 0; i < 2; i++ {
		if err := p.Apply(ctx, core.Plan{Ops: []core.Op{chainOp(core.OpKindDelete, "inet/filter/tmp", "")}}); err != nil {
			t.Fatalf("[AC-121-3-1] delete #%d: %v", i, err)
		}
	}
	if _, ok := rulesOf(t, p, "inet/filter/tmp"); ok {
		t.Fatal("[AC-121-3-1] tmp is still there")
	}
}

// [AC-121-3-2] nft -c が落ちるルールは PermanentError で、何も変わらない（-f は呼ばない）。
func TestLive_CheckFailureChangesNothing(t *testing.T) {
	fake := &nftfake.NFT{Reject: func(r string) bool { return strings.Contains(r, "bogus") }}
	fake.AddChain("inet", "filter", "web", "tcp dport 80 accept")
	p := newLive(t, fake)
	before := fake.Dump()
	err := p.Apply(context.Background(), core.Plan{Ops: []core.Op{
		chainOp(core.OpKindUpdate, "inet/filter/web", "tcp dport 22 accept"+nftables.RuleSep+"bogus"),
		chainOp(core.OpKindCreate, "inet/filter/new", "drop"),
	}})
	if !isPermanent(err) {
		t.Fatalf("[AC-121-3-2] want PermanentError, got %v", err)
	}
	if fake.Dump() != before {
		t.Errorf("[AC-121-3-2] the ruleset changed")
	}
	for _, c := range fake.Calls() {
		if strings.Join(c, " ") == "-f -" {
			t.Errorf("[AC-121-3-2] -f ran after a failed check")
		}
	}
}

// [AC-121-2-3] 宣言の空白・カウンタの値だけの違いは drift にならない（RefinePlan が
// 落とす）。nft が宣言を書き換える形の宣言は、Apply の読み戻しで PermanentError。
func TestLive_CanonicalForm(t *testing.T) {
	fake := &nftfake.NFT{Rewrite: func(r string) string {
		return strings.ReplaceAll(r, "ct state new,established", "ct state established,new")
	}}
	p := newLive(t, fake)
	ctx := context.Background()
	if err := p.Apply(ctx, core.Plan{Ops: []core.Op{chainOp(core.OpKindCreate, "inet/filter/web", "tcp dport 80  counter packets 5 bytes 7 accept")}}); err != nil {
		t.Fatal(err)
	}
	observed, _ := rulesOf(t, p, "inet/filter/web")
	if observed != "tcp dport 80 counter accept" {
		t.Fatalf("observed = %q", observed)
	}
	op := chainOp(core.OpKindUpdate, "inet/filter/web", "tcp  dport 80 counter accept")
	op.Fields = []core.FieldDiff{{Field: "rules", From: observed, To: "tcp  dport 80 counter accept"}}
	refined, err := p.RefinePlan("inet/filter/web", nil, nil, []core.Op{op})
	if err != nil || len(refined) != 1 || len(refined[0].Fields) != 0 {
		t.Fatalf("[AC-121-2-3] a whitespace-only difference must be dropped: %+v, %v", refined, err)
	}
	op.Fields = []core.FieldDiff{{Field: "rules", From: observed, To: "tcp dport 81 counter accept"}}
	if refined, _ := p.RefinePlan("inet/filter/web", nil, nil, []core.Op{op}); len(refined[0].Fields) != 1 {
		t.Fatalf("[AC-121-2-3] a real difference must stay")
	}

	err = p.Apply(ctx, core.Plan{Ops: []core.Op{chainOp(core.OpKindUpdate, "inet/filter/web", "ct state new,established accept")}})
	if !isPermanent(err) || !strings.Contains(err.Error(), "established,new") {
		t.Fatalf("[AC-121-2-3] a rule nft rewrites must be reported with nft's form: %v", err)
	}
}

// [AC-178-1-4] nft が集合の隣り合う範囲をまとめて書き戻しても、Apply の読み戻しは
// 宣言と同じとみなし、次の周の RefinePlan も差分を落とす（適合のまま）。要素が
// 本当に違う宣言は差分のまま。
func TestLive_MergedSets(t *testing.T) {
	fake := &nftfake.NFT{Rewrite: func(r string) string {
		r = strings.ReplaceAll(r, "{ 10.0.0.0/24, 10.0.1.0/24 }", "{ 10.0.0.0/23 }")
		return strings.ReplaceAll(r, "{ 23, 22 }", "{ 22-23 }")
	}}
	p := newLive(t, fake)
	ctx := context.Background()
	declared := "ip saddr { 10.0.0.0/24, 10.0.1.0/24 } accept" + nftables.RuleSep + "tcp dport { 23, 22 } accept"
	if err := p.Apply(ctx, core.Plan{Ops: []core.Op{chainOp(core.OpKindCreate, "inet/filter/web", declared)}}); err != nil {
		t.Fatalf("[AC-178-1-4] Apply must accept nft's merged form: %v", err)
	}
	observed, _ := rulesOf(t, p, "inet/filter/web")
	if observed != "ip saddr { 10.0.0.0/23 } accept"+nftables.RuleSep+"tcp dport { 22-23 } accept" {
		t.Fatalf("observed = %q", observed)
	}
	op := chainOp(core.OpKindUpdate, "inet/filter/web", declared)
	op.Fields = []core.FieldDiff{{Field: "rules", From: observed, To: declared}}
	if refined, err := p.RefinePlan("inet/filter/web", nil, nil, []core.Op{op}); err != nil || len(refined[0].Fields) != 0 {
		t.Fatalf("[AC-178-1-4] the merged form must not be a diff: %+v, %v", refined, err)
	}
	op.Fields = []core.FieldDiff{{Field: "rules", From: observed, To: "ip saddr { 10.0.0.0/24, 10.0.2.0/24 } accept" + nftables.RuleSep + "tcp dport { 23, 22 } accept"}}
	if refined, _ := p.RefinePlan("inet/filter/web", nil, nil, []core.Op{op}); len(refined[0].Fields) != 1 {
		t.Fatalf("[AC-178-1-4] a real difference in the set must stay")
	}
}

// [AC-121-2-2] ValidateIntent: 管理しない table・id とずれた identity・スクリプトに
// 埋め込めないルール文を読み込みの時点で断る。
func TestLive_ValidateIntent(t *testing.T) {
	p := newLive(t, &nftfake.NFT{})
	ok := map[string]string{"family": "inet", "table": "filter", "chain": "web", "rules": "tcp dport 22 accept"}
	if err := p.ValidateIntent("inet/filter/web", ok); err != nil {
		t.Fatal(err)
	}
	bad := map[string]map[string]string{
		"ip/nat/web":      {"rules": "accept"},
		"inet/filter/w-1": {"rules": "accept"},
		"inet/filter/web": {"chain": "other", "rules": "accept"},
	}
	for id, f := range bad {
		if err := p.ValidateIntent(id, f); err == nil {
			t.Errorf("[AC-121-2-2] %s %v must be refused", id, f)
		}
	}
	if err := p.ValidateIntent("inet/filter/web", map[string]string{"rules": "accept; flush ruleset"}); err == nil {
		t.Errorf("[AC-121-2-2] a ';' outside quotes must be refused")
	}
	// Apply refuses it too (defence in depth), without running nft -f.
	fake := &nftfake.NFT{}
	p = newLive(t, fake)
	err := p.Apply(context.Background(), core.Plan{Ops: []core.Op{chainOp(core.OpKindCreate, "inet/filter/web", "accept; flush ruleset")}})
	if !isPermanent(err) {
		t.Fatalf("[AC-121-2-2] Apply: want PermanentError, got %v", err)
	}
	if len(fake.Calls()) > 2 {
		t.Errorf("[AC-121-2-2] nft was asked to apply: %v", fake.Calls())
	}
}

// [AC-121-3-3] 同じホストの Apply は直列化される（-c から読み戻しまでが重ならない）。
func TestLive_ApplySerialized(t *testing.T) {
	fake := &nftfake.NFT{}
	p := newLive(t, fake)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- p.Apply(context.Background(), core.Plan{Ops: []core.Op{chainOp(core.OpKindCreate, "inet/filter/c"+string(rune('a'+i)), "accept")}})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("[AC-121-3-3] %v", err)
		}
	}
	lr, err := p.List(context.Background(), core.NewScope(nil))
	if err != nil || len(lr.IDs) != 8 {
		t.Fatalf("[AC-121-3-3] List = %v, %v; want 8 chains", lr.IDs, err)
	}
	var last string
	for _, c := range fake.Calls() {
		a := strings.Join(c, " ")
		if a == "-f -" && last != "-c -f -" {
			t.Fatalf("[AC-121-3-3] an apply was interleaved: %q after %q", a, last)
		}
		last = a
	}
}

// 記録した出力を返すだけの Runner。
type recorded struct{ json, text []byte }

func (r recorded) Run(_ context.Context, args []string, _ []byte) ([]byte, error) {
	if args[0] == "-j" {
		return r.json, nil
	}
	return r.text, nil
}

// [AC-121-1-1] 本物の nft の出力（testdata）を Read・BulkObserve で読む。
func TestLive_RecordedOutput(t *testing.T) {
	js, _ := os.ReadFile("testdata/inet-filter.json")
	txt, _ := os.ReadFile("testdata/inet-filter.txt")
	p, err := nftables.NewLive(nftables.LiveConfig{Runner: recorded{js, txt}, Tables: []nftables.TableRef{{Family: "inet", Name: "filter"}}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.BulkObserve(context.Background(), core.NewScope(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Observations) != 2 {
		t.Fatalf("[AC-121-1-1] %d entries, want web and empty", len(b.Observations))
	}
	got, _ := rulesOf(t, p, "inet/filter/web")
	if !strings.HasPrefix(got, "tcp dport { 80, 443 } ct state established,new counter accept") || strings.Count(got, nftables.RuleSep) != 2 {
		t.Errorf("[AC-121-1-1] web = %q", got)
	}
	if got, ok := rulesOf(t, p, "inet/filter/empty"); !ok || got != "" {
		t.Errorf("[AC-121-1-1] empty = %q, %v", got, ok)
	}
	// #173: the base chain is not listed, and read with its hook and policy.
	if obs, err := p.Read(context.Background(), "inet/filter/input"); err != nil || obs.State["hook"].Value != "input" || obs.State["policy"].Value == "" {
		t.Errorf("[AC-173-1-1] the base chain = %v, %v", obs.State, err)
	}
}

func isPermanent(err error) bool {
	var pe *provider.PermanentError
	return errors.As(err, &pe)
}

func isTransient(err error) bool {
	var te *provider.TransientError
	return errors.As(err, &te)
}
