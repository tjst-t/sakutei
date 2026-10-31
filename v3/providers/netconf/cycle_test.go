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

// cycle_test.go — コアの周（cli.Run の reconcile）を偽の機器に当てる：commit_group
// （transaction.group）の台は全部の確かめが通ったときだけ確定し、check(…)・
// observe(…) の確かめが通らなければどの台も確定しない（E-B9・不変条件 17）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/providers/netconf"
	"github.com/tjst-t/sakutei/v3/providers/netconf/netconffake"
)

type cycleLab struct {
	t      *testing.T
	dir    string
	srvs   map[string]*netconffake.Server
	pass   atomic.Bool
	checks atomic.Int64
}

func newCycleLab(t *testing.T, hosts ...string) *cycleLab {
	l := &cycleLab{t: t, dir: t.TempDir(), srvs: map[string]*netconffake.Server{}}
	for _, h := range hosts {
		l.srvs[h] = newServer(running, caps11...)
	}
	l.pass.Store(true)
	os.WriteFile(filepath.Join(l.dir, "config.yaml"), []byte("providers:\n  nc:\n    module: netconf\n"), 0o644)
	os.MkdirAll(filepath.Join(l.dir, "intent"), 0o755)
	return l
}

func (l *cycleLab) intent(body string) {
	var b strings.Builder
	b.WriteString("ranges:\n")
	for _, h := range sortedHosts(l.srvs) {
		fmt.Fprintf(&b, "  - provider: nc\n    args: {host: %s, filter.if-uplink: '%s'}\n", h, uplinkFilter)
	}
	b.WriteString(body)
	os.WriteFile(filepath.Join(l.dir, "intent", "nc.yaml"), []byte(b.String()), 0o644)
}

func sortedHosts(m map[string]*netconffake.Server) []string {
	var out []string
	for h := range m {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

func (l *cycleLab) reconcile() (plan.Result, int, string) {
	l.t.Helper()
	module := func(map[string]any) (core.Provider, error) {
		hosts := map[string]netconf.Host{}
		for h, s := range l.srvs {
			hosts[h] = netconf.Host{Observe: s.Dialer(h)}
		}
		return netconf.New(hosts, netconf.Options{Apply: true})
	}
	check := func(context.Context, core.ObservationView) (core.Tri, string) {
		l.checks.Add(1)
		if l.pass.Load() {
			return core.TriTrue, ""
		}
		return core.TriFalse, "管理の経路に届かない"
	}
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), []string{"reconcile", "--intent", filepath.Join(l.dir, "intent"),
		"--config", filepath.Join(l.dir, "config.yaml"), "--record", filepath.Join(l.dir, "record"), "--json"},
		&stdout, &stderr, cli.WithModule("netconf", module), cli.WithCheck("mgmt", check))
	var r plan.Result
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		l.t.Fatalf("出力を読めない: %v\n%s\n%s", err, stdout.String(), stderr.String())
	}
	return r, code, stderr.String()
}

func groupIntent(desc, confirm string) string {
	cfg := strings.ReplaceAll(strings.Replace(declared, "to core", desc, 1), "\n", " ")
	var b strings.Builder
	b.WriteString("resources:\n")
	for _, h := range []string{"g1", "g2", "g3"} {
		fmt.Fprintf(&b, "  - type: nc/subtree\n    id: %s/if-uplink\n    fields: {config: '%s'}\n", h, cfg)
		fmt.Fprintf(&b, "    apply:\n      transaction:\n        group: core\n        timeout: 4s\n")
		if confirm != "" {
			fmt.Fprintf(&b, "        confirm:\n          - {phase: after, when: '%s'}\n", confirm)
		}
	}
	return b.String()
}

// commit_group：全部の台で check(mgmt) が通れば 3 台とも確定する。通らなければ
// どの台も確定せず（すぐ戻す）、同じ宣言のまま当て直さない。
func TestCycleCommitGroupCheck(t *testing.T) {
	l := newCycleLab(t, "g1", "g2", "g3")
	l.intent(groupIntent("to spine", "check(mgmt)"))
	r, code, stderr := l.reconcile()
	if code != cli.ExitOK {
		t.Fatalf("exit=%d %+v\n%s", code, r.Changes, stderr)
	}
	for h, s := range l.srvs {
		if !strings.Contains(s.Running(), "to spine") || s.Pending() {
			t.Fatalf("%s が確定していない:\n%s", h, s.Running())
		}
	}
	for _, c := range r.Changes {
		if c.Txn != "group core" || c.Outcome != plan.OutcomeOK {
			t.Fatalf("%+v", c)
		}
	}

	l.pass.Store(false)
	l.intent(groupIntent("to leaf", "check(mgmt)"))
	start := time.Now()
	r, code, stderr = l.reconcile()
	if code != cli.ExitHalted {
		t.Fatalf("確かめが通らない周: exit=%d %+v\n%s", code, r.Changes, stderr)
	}
	if d := time.Since(start); d < 1500*time.Millisecond {
		t.Fatalf("確かめを %s で諦めた（timeout の半分まで待つ）", d)
	}
	for _, c := range r.Changes {
		if !c.RolledBack || c.Outcome != plan.OutcomeError {
			t.Fatalf("確定しないはず: %+v", c)
		}
	}
	for h, s := range l.srvs {
		if !strings.Contains(s.Running(), "to spine") || s.Pending() {
			t.Fatalf("%s が戻っていない:\n%s", h, s.Running())
		}
	}
	// 同じ宣言のまま当て直さない（不変条件 13・14）。
	r, code, _ = l.reconcile()
	if code != cli.ExitHalted {
		t.Fatalf("次の周: exit=%d", code)
	}
	for h, s := range l.srvs {
		if strings.Contains(s.Running(), "to leaf") {
			t.Fatalf("%s に当て直した", h)
		}
	}
}

// confirm の observe(…)：別のリソースの観測が期待と違えば確定しない（E-B9）。
func TestCycleConfirmObserve(t *testing.T) {
	l := newCycleLab(t, "g1", "g2")
	cfg := strings.ReplaceAll(strings.Replace(declared, "to core", "to spine", 1), "\n", " ")
	l.intent(fmt.Sprintf(`resources:
  - type: nc/subtree
    id: g2/if-uplink
    fields: {config: '%s'}
  - type: nc/subtree
    id: g1/if-uplink
    fields: {config: '%s'}
    apply:
      transaction:
        timeout: 4s
        confirm:
          - {phase: after, when: 'present(nc/subtree/g2/if-uplink)'}
`, strings.ReplaceAll(declared, "\n", " "), cfg))
	r, code, stderr := l.reconcile()
	if code != cli.ExitOK || !strings.Contains(l.srvs["g1"].Running(), "to spine") {
		t.Fatalf("exit=%d %+v\n%s", code, r.Changes, stderr)
	}

	// g2 の部分木が無い（観測が期待と違う）：g1 を確定しない。
	l.srvs["g2"].SetRunning(`<system xmlns="urn:example:system"><hostname>g2</hostname></system>`)
	l.intent(fmt.Sprintf(`resources:
  - type: nc/subtree
    id: g1/if-uplink
    fields: {config: '%s'}
    apply:
      transaction:
        timeout: 4s
        confirm:
          - {phase: after, when: 'present(nc/subtree/g2/if-uplink)'}
`, strings.Replace(cfg, "to spine", "to leaf", 1)))
	r, code, stderr = l.reconcile()
	if code == cli.ExitOK || strings.Contains(l.srvs["g1"].Running(), "to leaf") {
		t.Fatalf("確定した: exit=%d %+v\n%s\n%s", code, r.Changes, stderr, l.srvs["g1"].Running())
	}
}
