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

// nftables_live_test.go — Backlog #121: the nftables provider's live target
// (the kernel's ruleset through nft), run against the fake nft (nftfake).  The
// real nft is exercised by tests/integration/nftables (make integration-nftables).
//
// AC tags:
//
//	AC-121-4-1  sdk.ContractTester.RunAll passes on live (incl. three-valued:
//	            nft not running is unknown, never absent)
//	AC-121-4-2  sdk.BulkEquivalenceTester passes on live
//	AC-121-4-3  DefaultRegistry builds the live provider from ruleset_target /
//	            ruleset_tables / ruleset_nft; live without tables is an error
//	AC-121-4-4  Plan on live: a rule deleted by hand and a reordering are
//	            drift, a whitespace / counter-only difference is not; Apply
//	            puts the chain back
package acceptance

import (
	"context"
	"errors"
	"testing"

	"github.com/tjst-t/sakutei/internal/cause"
	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/nftables"
	"github.com/tjst-t/sakutei/internal/nftables/nftfake"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
	"github.com/tjst-t/sakutei/pkg/registry"
	"github.com/tjst-t/sakutei/pkg/sdk"
)

var liveTables = []nftables.TableRef{{Family: "inet", Name: "filter"}, {Family: "ip", Name: "nat"}}

func newLiveNFT(t *testing.T, fake *nftfake.NFT) *nftables.LiveProvider {
	t.Helper()
	p, err := nftables.NewLive(nftables.LiveConfig{Runner: fake, Tables: liveTables})
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.ValidateProvider(p); err != nil {
		t.Fatalf("ValidateProvider: %v", err)
	}
	return p
}

func TestNFTablesLive_Contract(t *testing.T) {
	// [AC-121-4-1]
	fake := &nftfake.NFT{}
	fake.AddBaseChain("inet", "filter", "input", "type filter hook input priority filter; policy accept;", "jump web")
	fake.AddChain("inet", "filter", "web", "tcp dport 80 accept")
	p := newLiveNFT(t, fake)
	ct := sdk.ContractTester{
		Provider: p,
		Lister:   p,
		Scope:    core.NewScope(nil),
		Examples: map[string]string{
			"family": "inet",
			"table":  "filter",
			"chain":  "contract",
			"rules":  "tcp dport 22 accept" + nftables.RuleSep + "ip saddr 10.0.0.0/8 counter drop",
		},
		MakeID: func(f map[string]string) string {
			return f["family"] + "/" + f["table"] + "/" + f["chain"]
		},
		MakeUnreachable: func() (func(), error) {
			fake.Down = errors.New(`exec: "nft": executable file not found in $PATH`)
			return func() { fake.Down = nil }, nil
		},
	}
	ct.RunAll(t)
}

func TestNFTablesLive_BulkEquivalence(t *testing.T) {
	// [AC-121-4-2]
	fake := &nftfake.NFT{}
	fake.AddSet("inet", "filter", "allowed")
	fake.AddBaseChain("inet", "filter", "input", "type filter hook input priority filter; policy accept;", "jump web")
	fake.AddChain("inet", "filter", "web", "tcp dport 80 accept", "tcp dport 443 counter packets 9 bytes 99 accept")
	fake.AddChain("inet", "filter", "empty")
	fake.AddChain("ip", "nat", "pre", "tcp dport 8080 dnat to 10.0.0.2:80")
	fake.AddChain("ip", "other", "x", "accept") // not managed
	p := newLiveNFT(t, fake)
	bt := sdk.BulkEquivalenceTester{Provider: p, Lister: p, Scope: core.NewScope(nil)}
	bt.Run(t)

	b, err := p.BulkObserve(context.Background(), core.NewScope(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Observations) != 3 {
		t.Errorf("[AC-121-4-2] %d entries, want web, empty and pre", len(b.Observations))
	}
}

func TestNFTablesLive_Registry(t *testing.T) {
	// [AC-121-4-3]
	reg := registry.DefaultRegistry()
	p, err := reg.Get("nftables", map[string]string{"ruleset_target": "live", "ruleset_tables": "inet/filter", "ruleset_nft": "/nonexistent/nft"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*nftables.LiveProvider); !ok {
		t.Fatalf("[AC-121-4-3] got %T, want *nftables.LiveProvider", p)
	}
	// The nft path is used: a missing binary is unknown, not absent.
	obs, err := p.Read(context.Background(), "inet/filter/web")
	if err == nil || obs.Availability != core.AvailabilityUnknown {
		t.Errorf("[AC-121-4-3] Read with a missing nft = %+v, %v; want unknown", obs, err)
	}
	if p, err := reg.Get("nftables", map[string]string{"ruleset": "x.rules"}); err != nil {
		t.Fatal(err)
	} else if _, ok := p.(*nftables.Provider); !ok {
		t.Errorf("[AC-121-4-3] the default target must be the file, got %T", p)
	}
	for name, cfg := range map[string]map[string]string{
		"live without tables": {"ruleset_target": "live"},
		"bad tables":          {"ruleset_target": "live", "ruleset_tables": "filter"},
		"bad target":          {"ruleset_target": "kernel"},
	} {
		if _, err := reg.Get("nftables", cfg); err == nil {
			t.Errorf("[AC-121-4-3] %s must be an error", name)
		}
	}
}

func TestNFTablesLive_DriftAndRestore(t *testing.T) {
	// [AC-121-4-4]
	fake := &nftfake.NFT{}
	p := newLiveNFT(t, fake)
	ctx := context.Background()
	e := diff.ForProvider(p)
	id := core.NewID(nftables.ProviderName, core.NewScope(nil), "inet/filter/web")
	desired := func(rules ...string) diff.DesiredResource {
		return diff.DesiredResource{ID: id, Fields: map[string]string{
			"family": "inet", "table": "filter", "chain": "web", "rules": nftEncoded(rules...),
		}}
	}
	want := desired("tcp dport 22 accept", "tcp dport 80 counter accept", "drop")
	converge := func() core.Plan {
		t.Helper()
		obs, err := p.Read(ctx, id.Local)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := e.Plan(obs, want, cause.CauseContext{})
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}

	plan := converge()
	if len(plan.Ops) != 1 || plan.Ops[0].Kind != core.OpKindCreate {
		t.Fatalf("[AC-121-4-4] from empty: %+v, want one Create", plan.Ops)
	}
	if err := p.Apply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if plan := converge(); len(plan.Ops) != 0 {
			t.Fatalf("[AC-121-4-4] cycle %d after converging: %+v, want no op", i, plan.Ops)
		}
	}

	// Whitespace and counter values are not drift.
	spaced := desired("tcp dport 22  accept", "tcp dport 80 counter packets 0 bytes 0 accept", "drop")
	obs, _ := p.Read(ctx, id.Local)
	if plan, _ := e.Plan(obs, spaced, cause.CauseContext{}); len(plan.Ops) != 0 {
		t.Errorf("[AC-121-4-4] a whitespace / counter-only difference must not be drift: %+v", plan.Ops)
	}

	// A rule deleted by hand is drift, and Apply puts it back.
	fake.DeleteRule("inet", "filter", "web", 0)
	plan = converge()
	if len(plan.Ops) != 1 || plan.Ops[0].Kind != core.OpKindUpdate {
		t.Fatalf("[AC-121-4-4] after a hand delete: %+v, want one Update", plan.Ops)
	}
	if err := p.Apply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if plan := converge(); len(plan.Ops) != 0 {
		t.Fatalf("[AC-121-4-4] after restore: %+v", plan.Ops)
	}

	// A reordering is drift.
	obs, _ = p.Read(ctx, id.Local)
	if plan, _ := e.Plan(obs, desired("drop", "tcp dport 22 accept", "tcp dport 80 counter accept"), cause.CauseContext{}); len(plan.Ops) != 1 {
		t.Errorf("[AC-121-4-4] a reordering must be drift: %+v", plan.Ops)
	}
}
