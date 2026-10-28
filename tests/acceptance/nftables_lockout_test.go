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

// nftables_lockout_test.go — #173: base chain の policy を宣言で管理し、
// 締め出しを時間切れの巻き戻しで防ぐ。収束のループの側（偽の nft・偽の Guard）。
//
//	[AC-173-5-1]  常駐: ssh を塞ぐ宣言の Apply は巻き戻され（RolledBackError）、
//	              ループは同じ差分の Apply をそこで止める（oscillating、reason
//	              rolled_back、stuck の ApplyIneffective）。何周回しても仕掛けは
//	              1 回だけで、ruleset は元のまま。
//	[AC-173-5-2]  宣言を直す（ssh を通すルールを足す）と止めが解け、Apply は
//	              確かめられて policy drop に収束する。
//	[AC-173-5-3]  DefaultRegistry: ruleset_rollback_timeout・ruleset_confirm_address
//	              を読み、悪い値は断る。

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/eventlog"
	"github.com/tjst-t/sakutei/v2/internal/nftables"
	"github.com/tjst-t/sakutei/v2/internal/nftables/nftfake"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/registry"
)

// lockedBuf is a bytes.Buffer safe for the daemon's writes and the test's reads.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func inputIntent(rules string) map[string]any {
	return map[string]any{"resources": []map[string]any{{
		"provider": "nftables", "id": "inet/filter/input",
		"fields": map[string]string{
			"family": "inet", "table": "filter", "chain": "input", "rules": rules,
			"type": "filter", "hook": "input", "priority": "filter", "policy": "drop",
		},
	}}}
}

func TestNFTablesLockout_DaemonStopsARolledBackApply(t *testing.T) {
	// [AC-173-5-1] [AC-173-5-2]
	fake := &nftfake.NFT{}
	fake.AddBaseChain("inet", "filter", "input", "type filter hook input priority filter; policy accept;", "tcp dport 22 accept")
	before := fake.Dump()
	guard := &nftfake.Guard{NFT: fake, Unreachable: func(n *nftfake.NFT) bool {
		return n.Policy("inet", "filter", "input") == "drop" && !n.HasRule("inet", "filter", "input", "tcp dport 22 accept")
	}}
	p, err := nftables.NewLive(nftables.LiveConfig{
		Runner: fake, Tables: []nftables.TableRef{{Family: "inet", Name: "filter"}},
		Guard: guard, RollbackAfter: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	intentDir := dir + "/intent"
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The declaration shuts ssh out: policy drop, only port 80.
	writeIntentFile(t, intentDir, inputIntent("tcp dport 80 accept"))
	rec := &recordingSink{}
	var out, errOut lockedBuf
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- reconcile.Run(ctx, reconcile.Config{
			IntentDir: intentDir, GeneratedDir: dir + "/generated", Provider: p,
			Interval: 20 * time.Millisecond, Out: &out, ErrOut: &errOut,
			EventSinks: []eventlog.Sink{rec}, StuckAfter: 100,
		})
	}()
	stop := func() { cancel(); <-done }
	defer stop()
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s; out:\n%s\nerr:\n%s", what, out.String(), errOut.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	stopped := func() []core.Event {
		var evs []core.Event
		for _, ev := range rec.firingOf(core.EventApplyIneffective) {
			if ev.Attrs["stopped"] == "true" {
				evs = append(evs, ev)
			}
		}
		return evs
	}

	waitFor("the stop of the rolled back Apply", func() bool { return len(stopped()) > 0 })
	ev := stopped()[0]
	if ev.Resource.Local != "inet/filter/input" || ev.Attrs["reason"] != reconcile.StopRolledBack || ev.Class != core.EventStuck {
		t.Fatalf("[AC-173-5-1] stopped = %+v, want inet/filter/input, rolled_back, stuck", ev)
	}
	if !strings.Contains(errOut.String(), "rolled back") {
		t.Errorf("[AC-173-5-1] the Apply failure must say rolled back:\n%s", errOut.String())
	}
	// Many cycles later: still one Apply, and the ruleset as it was.
	time.Sleep(400 * time.Millisecond)
	if arms, disarms, fires := guard.Counts(); arms != 1 || disarms != 0 || fires != 1 {
		t.Fatalf("[AC-173-5-1] arms/disarms/fires = %d/%d/%d, want 1/0/1 (the same change is not applied again)", arms, disarms, fires)
	}
	if got := fake.Dump(); got != before {
		t.Fatalf("[AC-173-5-1] the ruleset is not back:\n%s", got)
	}

	// The declaration keeps ssh open: the stop is lifted and it converges.
	writeIntentFile(t, intentDir, inputIntent("tcp dport 22 accept ; tcp dport 80 accept"))
	waitFor("policy drop", func() bool {
		return fake.Policy("inet", "filter", "input") == "drop" && fake.HasRule("inet", "filter", "input", "tcp dport 80 accept")
	})
	waitFor("the guard disarmed", func() bool { _, disarms, _ := guard.Counts(); return disarms == 1 })
	time.Sleep(300 * time.Millisecond)
	if fake.Policy("inet", "filter", "input") != "drop" {
		t.Fatalf("[AC-173-5-2] policy went back to %q", fake.Policy("inet", "filter", "input"))
	}
	if arms, _, fires := guard.Counts(); arms != 2 || fires != 1 {
		t.Errorf("[AC-173-5-2] arms/fires = %d/%d, want 2/1 (converged, no more Applies)", arms, fires)
	}
}

func TestNFTablesLockout_RegistryConfig(t *testing.T) {
	// [AC-173-5-3]
	reg := registry.DefaultRegistry()
	base := map[string]string{"ruleset_target": "live", "ruleset_tables": "inet/filter", "ruleset_nft": "/nonexistent/nft"}
	with := func(kv ...string) map[string]string {
		c := map[string]string{}
		for k, v := range base {
			c[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			c[kv[i]] = kv[i+1]
		}
		return c
	}
	good := with("ruleset_rollback_timeout", "20s", "ruleset_confirm_address", "127.0.0.1:22")
	if _, err := reg.Get("nftables", good); err != nil {
		t.Fatalf("good config: %v", err)
	}
	for _, bad := range []map[string]string{
		with("ruleset_rollback_timeout", "500ms"),
		with("ruleset_rollback_timeout", "soon"),
		with("ruleset_confirm_address", "127.0.0.1"),
	} {
		if _, err := reg.Get("nftables", bad); err == nil {
			t.Errorf("config %v: want an error", bad)
		}
	}
	// A remote host is confirmed over ssh, not the local address.
	if _, err := reg.Get("nftables", with("ruleset_confirm_address", "127.0.0.1:22", "ruleset_hosts", "fw1", "remote_config", "/nonexistent")); err == nil {
		t.Error("ruleset_confirm_address with ruleset_hosts: want an error")
	}
}
