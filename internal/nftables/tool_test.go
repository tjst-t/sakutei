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

// tool_test.go — 決まった道具の経路（Backlog #191）: 偽のホストの上の道具を
// 偽の nft と偽の守りで答えさせ、live の provider が nft・sh を呼ばずに道具の
// list・check・apply・arm・disarm・status だけを呼ぶこと、道具が無い・版が
// 合わない・表が合わないときに base chain を変えないことを確かめる。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/nftables"
	"github.com/tjst-t/sakutei/v2/internal/nftables/nftfake"
	"github.com/tjst-t/sakutei/v2/internal/nftguard"
	"github.com/tjst-t/sakutei/v2/internal/remote"
	"github.com/tjst-t/sakutei/v2/internal/remote/remotefake"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

const toolPath = "/usr/local/libexec/sakutei-nft-guard"

// fakeTool は道具の偽物（nft は nftfake、仕掛けは nftfake.Guard）。
type fakeTool struct {
	nft   *nftfake.NFT
	guard *nftfake.Guard

	mu       sync.Mutex
	missing  bool
	protocol int
	tables   []string
	maxSecs  int
	tokens   map[string]string
	n        int
}

func newFakeTool(nft *nftfake.NFT) *fakeTool {
	return &fakeTool{nft: nft, guard: &nftfake.Guard{NFT: nft}, protocol: nftguard.Protocol,
		tables: []string{"inet/filter"}, maxSecs: 600, tokens: map[string]string{}}
}

func (f *fakeTool) nftRun(ctx context.Context, args []string, stdin []byte) ([]byte, error) {
	out, err := f.nft.Run(ctx, args, stdin)
	var ee *nftables.ExitError
	if errors.As(err, &ee) {
		return out, &remote.ExitError{Code: nftguard.ExitNFT, Stderr: ee.Stderr}
	}
	return out, err
}

func (f *fakeTool) handle(ctx context.Context, args []string, stdin []byte) ([]byte, error) {
	f.mu.Lock()
	missing := f.missing
	f.mu.Unlock()
	if missing {
		return nil, &remote.ExitError{Code: 1, Stderr: "sudo: " + toolPath + ": command not found"}
	}
	refused := &remote.ExitError{Code: nftguard.ExitRefused, Stderr: "sakutei-nft-guard: refused"}
	switch args[0] {
	case "list":
		if args[3] == "json" {
			return f.nftRun(ctx, []string{"-j", "list", "table", args[1], args[2]}, nil)
		}
		return f.nftRun(ctx, []string{"-a", "-s", "list", "table", args[1], args[2]}, nil)
	case "check":
		if strings.Contains(string(stdin), "refuse-me") {
			return nil, refused
		}
		return f.nftRun(ctx, []string{"-c", "-f", "-"}, stdin)
	case "apply":
		return f.nftRun(ctx, []string{"-f", "-"}, stdin)
	case "arm":
		secs, _ := strconv.Atoi(args[1])
		tok, err := f.guard.Arm(ctx, nil, time.Duration(secs)*time.Second)
		if err != nil {
			return nil, err
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.n++
		hex := fmt.Sprintf("%032x", f.n)
		f.tokens[hex] = tok
		return []byte(hex + "\n"), nil
	case "disarm":
		f.mu.Lock()
		tok, ok := f.tokens[args[1]]
		f.mu.Unlock()
		if !ok {
			return nil, refused
		}
		if err := f.guard.Disarm(ctx, tok); errors.Is(err, nftables.ErrRollbackFired) {
			return []byte("fired\n"), nil
		} else if err != nil {
			return nil, err
		}
		return []byte("disarmed\n"), nil
	case "status":
		f.mu.Lock()
		defer f.mu.Unlock()
		return json.Marshal(nftguard.Status{Tool: nftguard.Name, Protocol: f.protocol, Tables: f.tables, MaxSecs: f.maxSecs, Rollbacks: []nftguard.Rollback{}})
	}
	return nil, refused
}

func newToolProvider(t *testing.T, fake *nftfake.NFT, after time.Duration) (*nftables.LiveProvider, *fakeTool, *remotefake.Host) {
	t.Helper()
	tool := newFakeTool(fake)
	host := &remotefake.Host{Name: "fw", Commands: map[string]remotefake.Handler{toolPath: tool.handle}}
	probe := func(context.Context) error {
		if sshBlocked(fake) {
			return errors.New("connection timed out")
		}
		return nil
	}
	p, err := nftables.NewLive(nftables.LiveConfig{
		Tables:        []nftables.TableRef{{Family: "inet", Name: "filter"}},
		RollbackAfter: after,
		Hosts: []nftables.LiveHost{{Name: "fw",
			Runner: nftables.RemoteRunner{Exec: host, Tool: toolPath},
			Guard:  nftables.ToolGuard{Exec: host, Tool: toolPath, Probe: probe}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.ValidateProvider(p); err != nil {
		t.Fatalf("ValidateProvider: %v", err)
	}
	return p, tool, host
}

// verbs は道具の呼ばれ方（argv[1]）の並び。道具の外のコマンドがあれば "!" を付ける。
func verbs(host *remotefake.Host) []string {
	var out []string
	for _, c := range host.Calls() {
		if c[0] != toolPath {
			out = append(out, "!"+strings.Join(c, " "))
			continue
		}
		out = append(out, c[1])
	}
	return out
}

func TestTool_ReadAndApplyThroughTheTool(t *testing.T) {
	// [AC-191-2-1] 読む・確かめる・当てるは道具の list・check・apply だけ。
	fake := &nftfake.NFT{}
	fake.AddChain("inet", "filter", "web", "tcp dport 80 accept")
	p, _, host := newToolProvider(t, fake, time.Second)
	if st := stateOf(t, p, "fw/inet/filter/web"); field(st, "rules") != "tcp dport 80 accept" {
		t.Fatalf("Read: %v", st)
	}
	// A table that is not there reads as empty (nft's error through the tool).
	if _, err := p.List(context.Background(), core.NewScope(nil)); err != nil {
		t.Fatalf("List: %v", err)
	}
	op := baseOp(core.OpKindUpdate, "fw/inet/filter/web", map[string]string{"rules": "tcp dport 443 accept"})
	if err := p.Apply(context.Background(), core.Plan{Ops: []core.Op{op}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !fake.HasRule("inet", "filter", "web", "tcp dport 443 accept") {
		t.Errorf("not applied:\n%s", fake.Dump())
	}
	for _, c := range host.Calls() {
		if c[0] != toolPath {
			t.Errorf("[AC-191-2-1] ran %q on the host, not the tool", c)
		}
	}
	for _, c := range host.Calls() {
		switch strings.Join(c[1:], " ") {
		case "list inet filter json", "list inet filter handles", "check", "apply":
		default:
			t.Errorf("[AC-191-2-1] the tool was called as %q", c[1:])
		}
	}

	// A table not there: empty, not unknown.
	fake2 := &nftfake.NFT{}
	p2, _, _ := newToolProvider(t, fake2, time.Second)
	obs, err := p2.Read(context.Background(), "fw/inet/filter/web")
	if err != nil || obs.Availability != core.AvailabilityOk {
		t.Errorf("[AC-191-2-1] Read of a chain in a table not there: %v %v", obs.Availability, err)
	}
}

func TestTool_BaseChainGuarded(t *testing.T) {
	// [AC-191-2-2] base chain の変更: status で版を見て、道具の arm（写しは道具が
	// 取る）・apply・確かめ・disarm。
	fake := &nftfake.NFT{}
	fake.AddBaseChain("inet", "filter", "input", "type filter hook input priority filter; policy accept;")
	p, tool, host := newToolProvider(t, fake, 400*time.Millisecond)
	op := baseOp(core.OpKindUpdate, "fw/inet/filter/input", map[string]string{"policy": "drop", "rules": "tcp dport 22 accept"})
	if err := p.Apply(context.Background(), core.Plan{Ops: []core.Op{op}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := strings.Join(verbs(host), " ")
	if !strings.HasPrefix(got, "list list status check arm apply ") || !strings.Contains(got, "disarm") || strings.Contains(got, "!") {
		t.Errorf("[AC-191-2-2] calls: %s", got)
	}
	for _, c := range host.Calls() {
		if c[1] == "arm" && strings.Join(c[2:], " ") != "1 inet/filter" {
			t.Errorf("[AC-191-2-2] arm %q, want 1 inet/filter", c[2:])
		}
	}
	if arms, disarms, fires := tool.guard.Counts(); arms != 1 || disarms != 1 || fires != 0 {
		t.Errorf("arms/disarms/fires = %d/%d/%d", arms, disarms, fires)
	}
	if fake.Policy("inet", "filter", "input") != "drop" {
		t.Errorf("policy: %s", fake.Dump())
	}

	// Shuts ssh out: not confirmed, the tool's rollback restores it.
	op = baseOp(core.OpKindUpdate, "fw/inet/filter/input", map[string]string{"rules": "tcp dport 80 accept"})
	err := p.Apply(context.Background(), core.Plan{Ops: []core.Op{op}})
	if !provider.IsRolledBack(err) {
		t.Fatalf("[AC-191-2-2] Apply = %v, want rolled back", err)
	}
	if !fake.HasRule("inet", "filter", "input", "tcp dport 22 accept") {
		t.Errorf("[AC-191-2-2] not rolled back:\n%s", fake.Dump())
	}
}

func TestTool_NotUsableNotChanged(t *testing.T) {
	// [AC-191-2-3] 道具が無い・版が合わない: base chain を変えない（一時的な
	// 失敗）。表・秒が道具の設定の外: 変えない（設定を直す、PermanentError）。
	for _, tc := range []struct {
		name      string
		set       func(*fakeTool)
		permanent bool
	}{
		{"missing", func(f *fakeTool) { f.missing = true }, false},
		{"another protocol", func(f *fakeTool) { f.protocol = nftguard.Protocol + 1 }, false},
		{"another table", func(f *fakeTool) { f.tables = []string{"inet/other"} }, true},
		{"a shorter max", func(f *fakeTool) { f.maxSecs = 0 }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &nftfake.NFT{}
			fake.AddBaseChain("inet", "filter", "input", "type filter hook input priority filter; policy accept;")
			before := fake.Dump()
			p, tool, host := newToolProvider(t, fake, time.Second)
			tool.mu.Lock()
			tc.set(tool)
			tool.mu.Unlock()
			op := baseOp(core.OpKindUpdate, "fw/inet/filter/input", map[string]string{"policy": "drop", "rules": "tcp dport 22 accept"})
			err := p.Apply(context.Background(), core.Plan{Ops: []core.Op{op}})
			var perm *provider.PermanentError
			var trans *provider.TransientError
			if tc.permanent && !errors.As(err, &perm) || !tc.permanent && !errors.As(err, &trans) || provider.IsRolledBack(err) {
				t.Fatalf("Apply = %T %v", err, err)
			}
			if fake.Dump() != before {
				t.Errorf("changed:\n%s", fake.Dump())
			}
			for _, v := range verbs(host) {
				if v == "arm" || v == "apply" {
					t.Errorf("%s was run: %v", v, verbs(host))
				}
			}
		})
	}
}

func TestTool_RefusedIsPermanent(t *testing.T) {
	// [AC-191-2-4] 道具が断った（引数・入力の形）検査は PermanentError、流さない。
	fake := &nftfake.NFT{}
	fake.AddChain("inet", "filter", "web", "tcp dport 80 accept")
	p, _, host := newToolProvider(t, fake, time.Second)
	op := baseOp(core.OpKindUpdate, "fw/inet/filter/web", map[string]string{"rules": "meta mark 1 comment \"refuse-me\" accept"})
	err := p.Apply(context.Background(), core.Plan{Ops: []core.Op{op}})
	var perm *provider.PermanentError
	if !errors.As(err, &perm) {
		t.Fatalf("Apply = %T %v, want PermanentError", err, err)
	}
	for _, v := range verbs(host) {
		if v == "apply" {
			t.Errorf("applied after the refusal: %v", verbs(host))
		}
	}
}
