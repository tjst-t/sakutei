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

// nftables_confirm_check_test.go — #214: nftables の base chain の confirm の先の
// observe:（別のリソースの観測）と check:（名前で登録した確かめの関数）が、
// 変えたあと（巻き戻しを仕掛けて流し、到達の先に届いたあと）の確かめとして
// 働き、通らなければ仕掛けを取り消さず元の規則に戻ることを、偽の nft・偽の
// 守りとデーモンの周（RunCycle）で確かめる（§23.4。netconf の #200 と同じ口）。
//
//	[AC-214-1] observe: の先（fw2 の chain）が expect どおりなら fw1 を変えて
//	           確定する。fw2 は観測用の資格（書き込みの文脈でない）で、管理する
//	           table の list だけで読む
//	[AC-214-2] observe: の先が expect と違えば確定せず元の規則に戻る（理由に
//	           observe: の先が出る）。intent が宣言しないリソースは読み込みで断る
//	[AC-214-3] 外の realizer（network_policy の実現先）が Output.Checks で渡した
//	           check: の関数を観測の文脈で呼び、nil になるまで繰り返す。nil に
//	           ならなければ確定せず元に戻る
//	[AC-214-4] 到達の先（self・ssh://・tcp://）の無い confirm・到達の先が全部
//	           after: の confirm・登録の無い check: は読み込みで断る（after: は
//	           observe:・check: では書いても同じ）

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

	"github.com/tjst-t/sakutei/v2/internal/nftables"
	"github.com/tjst-t/sakutei/v2/internal/nftables/nftfake"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/netpolicy"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// watchRunner は nft を走らせるたびに、書き込みの文脈かどうかと引数を残す。
type watchRunner struct {
	nftables.Runner
	mu    sync.Mutex
	calls []string // "write: <args>" / "observe: <args>"
}

func (w *watchRunner) Run(ctx context.Context, args []string, stdin []byte) ([]byte, error) {
	how := "observe: "
	if provider.Writing(ctx) {
		how = "write: "
	}
	w.mu.Lock()
	w.calls = append(w.calls, how+strings.Join(args, " "))
	w.mu.Unlock()
	return w.Runner.Run(ctx, args, stdin)
}

func (w *watchRunner) Calls() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.calls...)
}

type nftConfirmRig struct {
	fw1    *nftfake.NFT
	before string
	guard  *nftfake.Guard
	fw2    *watchRunner
	probe  *probeProvider
	out    *syncBuffer
	cycle  func() (reconcile.CycleResult, error)
}

// newNFTConfirmRig は fw1（input の base chain が policy accept）と fw2（web の
// chain が宣言どおり）の live と、観測だけの probe を持つデーモン。
func newNFTConfirmRig(t *testing.T, intent map[string]any, checks map[string]provider.ConfirmCheck) *nftConfirmRig {
	t.Helper()
	root := t.TempDir()
	intentDir := filepath.Join(root, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIntentFile(t, intentDir, intent)
	fw1 := &nftfake.NFT{}
	fw1.AddBaseChain("inet", "filter", "input", "type filter hook input priority filter; policy accept;", "tcp dport 22 accept")
	fake2 := &nftfake.NFT{}
	fake2.AddChain("inet", "filter", "web", "tcp dport 80 accept")
	fw2 := &watchRunner{Runner: fake2}
	guard := &nftfake.Guard{NFT: fw1}
	p, err := nftables.NewLive(nftables.LiveConfig{
		Tables: []nftables.TableRef{{Family: "inet", Name: "filter"}},
		Hosts: []nftables.LiveHost{
			{Name: "fw1", Runner: fw1, Guard: guard},
			{Name: "fw2", Runner: fw2},
		},
		RollbackAfter: 2 * time.Second,
	}, nftables.WithScope(core.NewScope(nftLabScope)))
	if err != nil {
		t.Fatal(err)
	}
	probe := &probeProvider{}
	probe.state.Store("ready")
	out := &syncBuffer{}
	d := reconcile.NewDaemon(reconcile.Config{
		IntentDir:    intentDir,
		GeneratedDir: filepath.Join(root, "generated"),
		Providers: func(name string) (provider.Provider, error) {
			switch name {
			case nftables.ProviderName:
				return p, nil
			case "probe":
				return probe, nil
			}
			return nil, os.ErrNotExist
		},
		ConfirmChecks: checks,
		Out:           out,
		ErrOut:        out,
	})
	r := &nftConfirmRig{fw1: fw1, before: fw1.Dump(), guard: guard, fw2: fw2, probe: probe, out: out}
	r.cycle = func() (reconcile.CycleResult, error) {
		res, err := d.RunCycle(context.Background())
		d.Wait(30 * time.Second)
		return res, err
	}
	return r
}

func (r *nftConfirmRig) mustCycle(t *testing.T) {
	t.Helper()
	if _, err := r.cycle(); err != nil {
		t.Fatalf("RunCycle: %v\n%s", err, r.out.String())
	}
}

// changed は fw1 の input が policy drop になり、仕掛けが取り消されたか。
func (r *nftConfirmRig) changed() bool {
	_, disarms, fires := r.guard.Counts()
	return r.fw1.Policy("inet", "filter", "input") == "drop" && disarms == 1 && fires == 0
}

// rolledBack は fw1 が元の規則に戻ったか（仕掛けが写しに戻した）。
func (r *nftConfirmRig) rolledBack() bool {
	arms, disarms, fires := r.guard.Counts()
	return r.fw1.Dump() == r.before && arms == 1 && disarms == 0 && fires == 1
}

// inputDecl は fw1 の input を policy drop にする宣言（confirm つき）。
func inputDecl(confirm string) map[string]string {
	f := baseFields("input", "input", "drop", "tcp dport 22 accept")
	if confirm != "" {
		f[nftables.FieldConfirm] = confirm
	}
	return f
}

// nftConfirmIntent は fw1 の input と、fw2 の web（今のまま）と probe の p1 の宣言。
func nftConfirmIntent(confirm string, extra ...map[string]any) map[string]any {
	now := time.Now()
	res := []map[string]any{
		{"provider": "nftables", "id": "fw1/inet/filter/input", "scope": nftLabScope, "fields": inputDecl(confirm)},
		{"provider": "nftables", "id": "fw2/inet/filter/web", "scope": nftLabScope,
			"fields": map[string]string{"family": "inet", "table": "filter", "chain": "web", "rules": "tcp dport 80 accept"}},
		{"provider": "probe", "id": "p1", "fields": map[string]string{"state": "ready"}},
	}
	in := map[string]any{
		"resources": res,
		"planned_change": map[string]any{"scope": nftLabScope, "kind": "update", "max_count": 10,
			"window":   pcWindow(now.Add(-time.Hour), now.Add(time.Hour)),
			"approval": map[string]any{"pr": 214, "approvers": []string{"alice"}}},
	}
	for _, e := range extra {
		for k, v := range e {
			in[k] = v
		}
	}
	return in
}

const fw2Web = "observe:nftables/site=lab/fw2/inet/filter/web"

func TestNFTablesConfirmCheck_ObserveAsExpected(t *testing.T) {
	// [AC-214-1]
	t.Parallel()
	r := newNFTConfirmRig(t, nftConfirmIntent(`self, `+fw2Web+` expect:{"rules":"tcp dport 80 accept"}`), nil)
	r.mustCycle(t)
	if !r.changed() {
		t.Fatalf("[AC-214-1] fw1 not changed and confirmed:\n%s\n%s", r.fw1.Dump(), r.out.String())
	}
	// fw2 was only read: in the observe context, by listing the managed table.
	calls := r.fw2.Calls()
	if len(calls) == 0 {
		t.Fatalf("[AC-214-1] fw2 was not read")
	}
	for _, c := range calls {
		if !strings.HasPrefix(c, "observe: ") || !strings.Contains(c, "list") || !strings.Contains(c, "inet filter") {
			t.Fatalf("[AC-214-1] fw2: %q (want only lists of the managed table, in the observe context)", c)
		}
	}
	// The self target was confirmed before and after the change.
	if got := r.guard.Confirms(); len(got) != 2 || got[0] != "self" || got[1] != "self" {
		t.Fatalf("[AC-214-1] confirms %v", got)
	}
}

func TestNFTablesConfirmCheck_ObserveNotAsExpected_RolledBack(t *testing.T) {
	// [AC-214-2]
	t.Parallel()
	r := newNFTConfirmRig(t, nftConfirmIntent(`self, observe:probe//p1 expect:{"state":"routed"}`), nil)
	before := r.probe.reads.Load()
	r.mustCycle(t)
	if !r.rolledBack() {
		t.Fatalf("[AC-214-2] fw1 not rolled back:\n%s\n%s", r.fw1.Dump(), r.out.String())
	}
	for _, want := range []string{"observe:probe//p1: state not as expected", "restored tables inet/filter as they were before"} {
		if !strings.Contains(r.out.String(), want) {
			t.Fatalf("[AC-214-2] no %q in:\n%s", want, r.out.String())
		}
	}
	if r.probe.reads.Load()-before < 2 {
		t.Fatalf("[AC-214-2] the probe was not read again (reads %d)", r.probe.reads.Load()-before)
	}
	if n := r.probe.writingRead.Load(); n != 0 {
		t.Fatalf("[AC-214-2] %d read(s) were made in the write context", n)
	}
	// A resource the intent does not declare is never read: refused at load.
	r2 := newNFTConfirmRig(t, nftConfirmIntent(`self, observe:probe//p2`), nil)
	if _, err := r2.cycle(); err == nil || !strings.Contains(err.Error(), "observe:probe//p2 is not declared in the intent") {
		t.Fatalf("[AC-214-2] RunCycle = %v", err)
	}
	if arms, _, _ := r2.guard.Counts(); arms != 0 || len(r2.guard.Confirms()) != 0 {
		t.Fatalf("[AC-214-2] fw1 was touched")
	}
}

func TestNFTablesConfirmCheck_CheckFromRealizer(t *testing.T) {
	// [AC-214-3] a realizer of network_policy puts fw1's input base chain and
	// hands the check function with its Output.
	var calls, writing atomic.Int32
	var mu sync.Mutex
	var seen []core.ID
	var pass atomic.Bool
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
			return errors.New("the forwarded path does not reach the other side yet")
		}
		return nil
	}
	if err := netpolicy.Register("nft-check-214", netpolicy.RealizerFunc(func(t netpolicy.Target, allows []netpolicy.Passing) (netpolicy.Output, error) {
		return netpolicy.Output{
			Resources: []netpolicy.Resource{{Provider: nftables.ProviderName, Scope: t.Scope, ID: t.ID,
				Fields: inputDecl("self, check:forward-seen-214")}},
			Checks: map[string]provider.ConfirmCheck{"forward-seen-214": check},
		}, nil
	})); err != nil {
		t.Fatal(err)
	}
	policy := map[string]any{"network_policy": map[string]any{
		"networks": []map[string]any{
			{"name": "office", "cidrs": []string{"10.1.0.0/24"}},
			{"name": "dmz", "cidrs": []string{"192.0.2.0/24"}},
		},
		"allows": []map[string]any{{"from": "office", "to": "dmz", "proto": "tcp", "ports": []any{443}, "reason": "web"}},
		"targets": []map[string]any{{"name": "fw1", "realizer": "nft-check-214", "kind": "boundary",
			"networks": []string{"office", "dmz"}, "id": "fw1/inet/filter/input", "scope": nftLabScope}},
	}}
	intent := func() map[string]any {
		in := nftConfirmIntent("", policy)
		res := in["resources"].([]map[string]any)
		in["resources"] = res[1:] // fw1's input comes from the realizer
		return in
	}
	t.Run("passes on the third call: changed", func(t *testing.T) {
		r := newNFTConfirmRig(t, intent(), nil)
		r.mustCycle(t)
		if !r.changed() || calls.Load() != 3 {
			t.Fatalf("[AC-214-3] calls %d\n%s\n%s", calls.Load(), r.fw1.Dump(), r.out.String())
		}
		if writing.Load() != 0 {
			t.Fatalf("[AC-214-3] the check was called in the write context")
		}
		mu.Lock()
		defer mu.Unlock()
		if seen[0].Provider != nftables.ProviderName || seen[0].Local != "fw1/inet/filter/input" {
			t.Fatalf("[AC-214-3] the check was asked about %v", seen[0])
		}
	})
	t.Run("never passes: rolled back", func(t *testing.T) {
		pass.Store(false)
		r := newNFTConfirmRig(t, intent(), nil)
		r.mustCycle(t)
		if !r.rolledBack() {
			t.Fatalf("[AC-214-3] fw1 not rolled back:\n%s\n%s", r.fw1.Dump(), r.out.String())
		}
		if !strings.Contains(r.out.String(), "check:forward-seen-214: the forwarded path does not reach the other side yet") {
			t.Fatalf("[AC-214-3] the reason:\n%s", r.out.String())
		}
	})
}

func TestNFTablesConfirmCheck_LoadRefusals(t *testing.T) {
	// [AC-214-4]
	t.Parallel()
	ok := map[string]provider.ConfirmCheck{"x": func(context.Context, provider.ConfirmTarget) error { return nil }}
	for _, c := range []struct{ confirm, want string }{
		{"check:x", "no target is reached"},
		{fw2Web, "no target is reached"},
		{"after:check:x, observe:probe//p1", "no target is reached"},
		{"after:" + forwardSSH + ", check:x", "every target is marked after:"},
		{"self, check:nobody", "check:nobody: no check function is registered"},
		{`self, observe:probe//p1 expect:{"nothing":"x"}`, `field "nothing" is not a field of provider probe`},
		{"self, observe:nftables/site=lab/fw1/inet/filter/input", "is the resource itself"},
		{"self, check:x, after:check:x", "listed twice"},
	} {
		r := newNFTConfirmRig(t, nftConfirmIntent(c.confirm), ok)
		if _, err := r.cycle(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("[AC-214-4] confirm %q: RunCycle = %v", c.confirm, err)
		}
		if arms, _, _ := r.guard.Counts(); arms != 0 {
			t.Fatalf("[AC-214-4] confirm %q: fw1 was changed", c.confirm)
		}
	}
	// after: on observe: / check: is the same as without it.
	r := newNFTConfirmRig(t, nftConfirmIntent("self, after:check:x, "+fw2Web), ok)
	r.mustCycle(t)
	if !r.changed() {
		t.Fatalf("[AC-214-4] not changed:\n%s", r.out.String())
	}
}
