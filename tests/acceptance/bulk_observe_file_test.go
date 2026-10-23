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

// bulk_observe_file_test.go — Backlog #27 (v1b): BulkObserve on dnszone and
// nftables, and the consistency contract enforced by the SDK's CycleObserver on
// the convergence path (§5.7, ADR-0011).
//
// Tags:
//   [#27-1] dnszone and nftables declare BulkObserve, validate, and pass the
//           SDK BulkEquivalenceTester (including the first-match cases of Read);
//           the contract tests for both are TestContractGenerated_DNSZone /
//           TestContractGenerated_NFTables (sdk_contract_generated_test.go)
//   [#27-2] complete-or-error: a file that cannot be read or parsed makes the
//           bulk call fail as a whole, and every resource of the cycle is
//           unobservable (nothing Applied), as when Read returns Unknown
//   [#27-3] the daemon cycle reads the file once on the bulk path, against once
//           per resource on the per-resource Read path; a resource the bulk
//           call did not return is Read (unknown ≠ absent) and gets created
//   [#27-4] the SDK CycleObserver enforces the contract: zero ObservedAt
//           rejects the whole bulk result, a bulk error is never answered by
//           Read, an undeclared bulk implementation is not used

package acceptance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/dnszone"
	"github.com/tjst-t/sakutei/internal/nftables"
	"github.com/tjst-t/sakutei/internal/reconcile"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
	"github.com/tjst-t/sakutei/pkg/sdk"
)

const bulkZone = `$ORIGIN example.com.
@ 3600 IN SOA ns1.example.com. admin.example.com. 1 7200 3600 1209600 300
@ 3600 IN NS ns1.example.com.
; comment
www.example.com. 300 IN A 192.0.2.1
www.example.com. 300 IN AAAA 2001:db8::1
WWW.example.com. 600 IN A 192.0.2.99
api.example.com. IN CNAME www.example.com.
mail.example.com. 300 IN MX 10 mx.example.com.
`

const bulkRuleset = `table ip filter {
	chain input {
		type filter hook input priority 0; policy drop;
		tcp dport 22 accept
	}
	chain web {
		tcp dport 80 accept
		tcp dport 443 accept
	}
	chain web {
		tcp dport 8080 accept
	}
	chain empty {
	}
}
table inet nat {
	chain pre {
		ip saddr 10.0.0.0/8 accept
	}
}
`

// [#27-1]
func TestBulkObserve_DNSZoneEquivalentToRead(t *testing.T) {
	zone := filepath.Join(t.TempDir(), "example.com.zone")
	writeZoneFile(t, zone, bulkZone)
	p := dnszone.New(zone)
	if !p.Capabilities().BulkObserve {
		t.Fatalf("[#27-1] dnszone must declare Capabilities.BulkObserve")
	}
	if err := provider.ValidateProvider(p); err != nil {
		t.Fatalf("[#27-1] ValidateProvider: %v", err)
	}
	bt := sdk.BulkEquivalenceTester{Provider: p, Lister: p, Scope: core.NewScope(nil)}
	bt.Run(t)

	// The two A lines that differ only in the case of the name have one
	// (lower-cased, #71) id: Read gives it the first line's state, and so must
	// the bulk call.
	b, err := p.BulkObserve(context.Background(), core.NewScope(nil))
	if err != nil {
		t.Fatal(err)
	}
	got := b.Observations[core.NewID(dnszone.ProviderName, core.NewScope(nil), "www.example.com./A").Key()]
	if v := got.State["value"].Value; v != "192.0.2.1" {
		t.Errorf("[#27-1] www.example.com./A value = %v, want the first line's 192.0.2.1", v)
	}
	if _, ok := b.Observations[core.NewID(dnszone.ProviderName, core.NewScope(nil), "WWW.example.com./A").Key()]; ok {
		t.Errorf("[#27-1] BulkObserve must not report the upper-case id WWW.example.com./A")
	}
}

// [#27-1]
func TestBulkObserve_NFTablesEquivalentToRead(t *testing.T) {
	rules := filepath.Join(t.TempDir(), "fw.rules")
	writeZoneFile(t, rules, bulkRuleset)
	p := nftables.New(rules)
	if !p.Capabilities().BulkObserve {
		t.Fatalf("[#27-1] nftables must declare Capabilities.BulkObserve")
	}
	if err := provider.ValidateProvider(p); err != nil {
		t.Fatalf("[#27-1] ValidateProvider: %v", err)
	}
	bt := sdk.BulkEquivalenceTester{Provider: p, Lister: p, Scope: core.NewScope(nil)}
	bt.Run(t)

	b, err := p.BulkObserve(context.Background(), core.NewScope(nil))
	if err != nil {
		t.Fatal(err)
	}
	root := core.NewScope(nil)
	if _, ok := b.Observations[core.NewID(nftables.ProviderName, root, "ip/filter/input").Key()]; ok {
		t.Errorf("[#27-1] the base chain must not be in the bulk result (Read refuses it)")
	}
	web := b.Observations[core.NewID(nftables.ProviderName, root, "ip/filter/web").Key()]
	if v := web.State["rules"].Value; v != "tcp dport 80 accept"+nftables.RuleSep+"tcp dport 443 accept" {
		t.Errorf("[#27-1] ip/filter/web rules = %q, want the first block's (as Read)", v)
	}
	// #104: List is a set — the twice-written chain web is listed once, so List
	// and BulkObserve name the same resources.
	lr, err := p.List(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, id := range lr.IDs {
		if seen[id.Local] {
			t.Errorf("[#104] List returned %s twice", id.Local)
		}
		seen[id.Local] = true
	}
	if len(lr.IDs) != len(b.Observations) {
		t.Errorf("[#104] List has %d ids, BulkObserve %d entries; want the same resources", len(lr.IDs), len(b.Observations))
	}
}

// bulkFileCase is one file-backed bulk provider and the ways to break its file.
type bulkFileCase struct {
	name     string
	file     string
	content  string
	newP     func(path string) provider.Provider
	breakers map[string]func(t *testing.T, path string)
}

func bulkFileCases() []bulkFileCase {
	tooLong := func(t *testing.T, path string) {
		writeZoneFile(t, path, strings.Repeat("x", 128*1024)+"\n")
	}
	directory := func(t *testing.T, path string) {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return []bulkFileCase{
		{
			name: "dnszone", file: "example.com.zone", content: bulkZone,
			newP: func(path string) provider.Provider { return dnszone.New(path) },
			breakers: map[string]func(*testing.T, string){
				"unscannable line": tooLong,
				"not a file":       directory,
			},
		},
		{
			name: "nftables", file: "fw.rules", content: bulkRuleset,
			newP: func(path string) provider.Provider { return nftables.New(path) },
			breakers: map[string]func(*testing.T, string){
				"chain outside table": func(t *testing.T, path string) {
					writeZoneFile(t, path, bulkRuleset+"chain orphan {\n\tip saddr 10.9.9.9 accept\n}\n")
				},
				"unscannable line": tooLong,
				"not a file":       directory,
			},
		},
	}
}

// [#27-2]
func TestBulkObserve_BrokenFileIsAnError(t *testing.T) {
	for _, c := range bulkFileCases() {
		for bname, brk := range c.breakers {
			t.Run(c.name+"/"+bname, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), c.file)
				writeZoneFile(t, path, c.content)
				p := c.newP(path)
				lr, err := p.(provider.Lister).List(context.Background(), core.NewScope(nil))
				if err != nil || len(lr.IDs) == 0 {
					t.Fatalf("List before breaking: %v (%d ids)", err, len(lr.IDs))
				}
				brk(t, path)

				b, err := p.(provider.BulkObserver).BulkObserve(context.Background(), core.NewScope(nil))
				if err == nil {
					t.Fatalf("[#27-2] BulkObserve on a broken file must fail; got %d entries", len(b.Observations))
				}
				if len(b.Observations) != 0 {
					t.Errorf("[#27-2] a failed BulkObserve must not return a partial result (%d entries)", len(b.Observations))
				}
				if !sdk.IsTransient(err) {
					t.Errorf("[#27-2] want a TransientError (as Read), got %v", err)
				}

				// On the convergence path: every resource is Unknown with an
				// error, as Read gives when it cannot read the file.
				o := sdk.NewCycleObserver(p, core.NewScope(nil))
				for _, id := range lr.IDs {
					obs, err := o.Observe(context.Background(), id)
					if err == nil || obs.Availability != core.AvailabilityUnknown {
						t.Errorf("[#27-2] Observe(%s) = %v, %v; want Unknown and an error", id, obs.Availability, err)
					}
					ref, rerr := p.Read(context.Background(), id.Local)
					if rerr == nil || ref.Availability != core.AvailabilityUnknown {
						t.Errorf("[#27-2] Read(%s) = %v, %v; the per-resource path must agree", id, ref.Availability, rerr)
					}
				}
			})
		}
	}
}

// countingProvider counts the calls that read the file: each Read and each
// BulkObserve of a file-backed provider opens the file exactly once.
type countingProvider struct {
	inner provider.Provider
	bulk  bool // expose BulkObserve (and declare it)

	mu    sync.Mutex
	reads int
	bulks int
}

func (c *countingProvider) Schema() provider.Schema { return c.inner.Schema() }
func (c *countingProvider) Capabilities() provider.Capabilities {
	caps := c.inner.Capabilities()
	caps.BulkObserve = c.bulk
	return caps
}
func (c *countingProvider) Read(ctx context.Context, id string) (core.Observation, error) {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	return c.inner.Read(ctx, id)
}
func (c *countingProvider) Apply(ctx context.Context, plan core.Plan) error {
	return c.inner.Apply(ctx, plan)
}
func (c *countingProvider) List(ctx context.Context, scope core.Scope) (core.ListResult, error) {
	return c.inner.(provider.Lister).List(ctx, scope)
}
func (c *countingProvider) fileReads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads + c.bulks
}
func (c *countingProvider) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads, c.bulks = 0, 0
}

// countingBulkProvider is countingProvider plus BulkObserver.
type countingBulkProvider struct{ *countingProvider }

func (c *countingBulkProvider) BulkObserve(ctx context.Context, scope core.Scope) (provider.BulkObservation, error) {
	c.mu.Lock()
	c.bulks++
	c.mu.Unlock()
	return c.inner.(provider.BulkObserver).BulkObserve(ctx, scope)
}

type bulkRig struct {
	t         *testing.T
	intentDir string
	cp        *countingProvider
	d         *reconcile.Daemon
}

func newBulkRig(t *testing.T, inner provider.Provider, bulk bool, resources []map[string]any) *bulkRig {
	t.Helper()
	root := t.TempDir()
	intentDir := filepath.Join(root, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIntentFile(t, intentDir, map[string]any{"resources": resources})
	cp := &countingProvider{inner: inner, bulk: bulk}
	var p provider.Provider = cp
	if bulk {
		p = &countingBulkProvider{cp}
	}
	return &bulkRig{t: t, intentDir: intentDir, cp: cp, d: reconcile.NewDaemon(reconcile.Config{
		IntentDir:    intentDir,
		GeneratedDir: filepath.Join(root, "generated"),
		Provider:     p,
		Out:          io.Discard,
		ErrOut:       io.Discard,
	})}
}

// cycle runs one cycle and returns its result and the file reads it made while
// observing (it waits for the async Apply after the count).
func (r *bulkRig) cycle() (reconcile.CycleResult, int) {
	r.t.Helper()
	r.cp.reset()
	res, err := r.d.RunCycle(context.Background())
	if err != nil {
		r.t.Fatalf("RunCycle: %v", err)
	}
	n := r.cp.fileReads()
	r.d.Wait(5 * time.Second)
	return res, n
}

func dnsBulkResources(n int) []map[string]any {
	var out []map[string]any
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("h%d.example.com.", i)
		out = append(out, dnszoneResource(name+"/A", name, "A", fmt.Sprintf("192.0.2.%d", i), "300", ""))
	}
	return out
}

func dnsBulkZone(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "h%d.example.com. 300 IN A 192.0.2.%d\n", i, i)
	}
	return b.String()
}

func nftBulkResources(n int) []map[string]any {
	var out []map[string]any
	for i := 1; i <= n; i++ {
		chain := fmt.Sprintf("c%d", i)
		out = append(out, map[string]any{
			"provider": "nftables",
			"id":       "ip/filter/" + chain,
			"fields": map[string]string{
				"family": "ip", "table": "filter", "chain": chain,
				"rules": fmt.Sprintf("tcp dport %d accept", 1000+i),
			},
		})
	}
	return out
}

func nftBulkRuleset(n int) string {
	var b strings.Builder
	b.WriteString("table ip filter {\n")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "\tchain c%d {\n\t\ttcp dport %d accept\n\t}\n", i, 1000+i)
	}
	b.WriteString("}\n")
	return b.String()
}

// [#27-3]
func TestBulkObserve_DaemonCycleReadsFileOnce(t *testing.T) {
	const n = 5
	cases := []struct {
		name      string
		file      string
		content   string
		newP      func(string) provider.Provider
		resources []map[string]any
	}{
		{"dnszone", "zone.db", dnsBulkZone(n), func(p string) provider.Provider { return dnszone.New(p) }, dnsBulkResources(n)},
		{"nftables", "fw.rules", nftBulkRuleset(n), func(p string) provider.Provider { return nftables.New(p) }, nftBulkResources(n)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			readsPerCycle := map[bool]int{}
			for _, bulk := range []bool{false, true} {
				path := filepath.Join(t.TempDir(), c.file)
				writeZoneFile(t, path, c.content)
				r := newBulkRig(t, c.newP(path), bulk, c.resources)
				res, reads := r.cycle()
				if res.Observed != n || res.Launched != 0 {
					t.Fatalf("bulk=%v: converged cycle Observed=%d Launched=%d, want %d and 0", bulk, res.Observed, res.Launched, n)
				}
				readsPerCycle[bulk] = reads
			}
			if readsPerCycle[false] != n {
				t.Errorf("[#27-3] per-resource path: %d file reads per cycle, want %d", readsPerCycle[false], n)
			}
			if readsPerCycle[true] != 1 {
				t.Errorf("[#27-3] bulk path: %d file reads per cycle, want 1", readsPerCycle[true])
			}
			if readsPerCycle[true] >= readsPerCycle[false] {
				t.Errorf("[#27-3] the bulk path must read the file fewer times than per-resource Read (%d vs %d)",
					readsPerCycle[true], readsPerCycle[false])
			}
		})
	}
}

// [#27-3] a resource missing from the file is not in the bulk result; it is
// Read (which alone says absent) and then created.
func TestBulkObserve_DaemonCreatesWhatBulkDidNotReturn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zone.db")
	writeZoneFile(t, path, dnsBulkZone(2))
	r := newBulkRig(t, dnszone.New(path), true, dnsBulkResources(3))

	res, reads := r.cycle()
	if res.Observed != 3 || res.Launched != 1 {
		t.Fatalf("[#27-3] Observed=%d Launched=%d, want 3 and 1", res.Observed, res.Launched)
	}
	if reads != 2 || r.cp.reads != 1 {
		t.Errorf("[#27-3] want one bulk call and one Read for the missing record; got %d file reads (%d Read)", reads, r.cp.reads)
	}
	if !strings.Contains(readZoneFile(t, path), "h3.example.com. 300 IN A 192.0.2.3") {
		t.Errorf("[#27-3] the missing record was not created:\n%s", readZoneFile(t, path))
	}
	res, reads = r.cycle()
	if res.Launched != 0 || reads != 1 {
		t.Errorf("[#27-3] next cycle: Launched=%d reads=%d, want 0 and 1", res.Launched, reads)
	}
}

// [#27-2] on the daemon path a broken file leaves every resource unobservable
// and nothing is written.
func TestBulkObserve_DaemonBrokenFileAppliesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fw.rules")
	writeZoneFile(t, path, nftBulkRuleset(3))
	resources := nftBulkResources(3)
	resources[0]["fields"].(map[string]string)["rules"] = "tcp dport 9 drop" // a diff to apply
	r := newBulkRig(t, nftables.New(path), true, resources)

	broken := nftBulkRuleset(3) + "chain orphan {\n}\n"
	writeZoneFile(t, path, broken)
	res, reads := r.cycle()
	if res.Observed != 0 || res.Launched != 0 {
		t.Errorf("[#27-2] Observed=%d Launched=%d, want 0 and 0", res.Observed, res.Launched)
	}
	if reads != 1 || r.cp.reads != 0 {
		t.Errorf("[#27-2] want the single bulk call and no Read fallback; got %d file reads (%d Read)", reads, r.cp.reads)
	}
	if got := readZoneFile(t, path); got != broken {
		t.Errorf("[#27-2] the broken file was rewritten:\n%s", got)
	}
}

// stubBulk is a scriptable bulk provider for the SDK contract checks.
type stubBulk struct {
	declare bool
	bulk    func() (provider.BulkObservation, error)
	reads   int
	bulks   int
}

func (s *stubBulk) Schema() provider.Schema { return provider.Schema{} }
func (s *stubBulk) Capabilities() provider.Capabilities {
	return provider.Capabilities{ProviderVersion: "v-test", BulkObserve: s.declare}
}
func (s *stubBulk) Read(_ context.Context, _ string) (core.Observation, error) {
	s.reads++
	return core.Observation{Availability: core.AvailabilityOk, ObservedAt: time.Now()}, nil
}
func (s *stubBulk) Apply(_ context.Context, _ core.Plan) error { return nil }
func (s *stubBulk) BulkObserve(_ context.Context, _ core.Scope) (provider.BulkObservation, error) {
	s.bulks++
	return s.bulk()
}

// [#27-4]
func TestCycleObserver_EnforcesContract(t *testing.T) {
	ctx := context.Background()
	scope := core.NewScope(map[string]string{"site": "tokyo"})
	a := core.NewID("stub", scope, "a")
	b := core.NewID("stub", scope, "b")
	ok := func(at time.Time) func() (provider.BulkObservation, error) {
		return func() (provider.BulkObservation, error) {
			return provider.BulkObservation{Observations: map[core.IDKey]core.Observation{
				a.Key(): {Availability: core.AvailabilityOk, State: core.State{"x": {Value: "1"}}, ObservedAt: at},
			}}, nil
		}
	}

	t.Run("one bulk call per cycle; missing entry is Read", func(t *testing.T) {
		s := &stubBulk{declare: true, bulk: ok(time.Now())}
		o := sdk.NewCycleObserver(s, scope)
		for i := 0; i < 3; i++ {
			if _, err := o.Observe(ctx, a); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := o.Observe(ctx, b); err != nil {
			t.Fatal(err)
		}
		if s.bulks != 1 || s.reads != 1 {
			t.Errorf("[#27-4] bulks=%d reads=%d, want 1 and 1", s.bulks, s.reads)
		}
	})

	t.Run("zero ObservedAt rejects the whole result", func(t *testing.T) {
		s := &stubBulk{declare: true, bulk: ok(time.Time{})}
		o := sdk.NewCycleObserver(s, scope)
		for _, id := range []core.ID{a, b} {
			obs, err := o.Observe(ctx, id)
			if err == nil || !sdk.IsPermanent(err) || obs.Availability != core.AvailabilityUnknown {
				t.Errorf("[#27-4] Observe(%s) = %v, %v; want Unknown and a PermanentError", id.Local, obs.Availability, err)
			}
		}
		if s.reads != 0 {
			t.Errorf("[#27-4] a rejected bulk result must not be answered by Read (reads=%d)", s.reads)
		}
	})

	t.Run("bulk error: every resource Unknown, no Read", func(t *testing.T) {
		boom := errors.New("boom")
		s := &stubBulk{declare: true, bulk: func() (provider.BulkObservation, error) {
			return provider.BulkObservation{}, provider.NewTransientError(boom)
		}}
		o := sdk.NewCycleObserver(s, scope)
		for _, id := range []core.ID{a, b} {
			obs, err := o.Observe(ctx, id)
			if !errors.Is(err, boom) || obs.Availability != core.AvailabilityUnknown || obs.ObservedAt.IsZero() {
				t.Errorf("[#27-4] Observe(%s) = %+v, %v; want stamped Unknown and the bulk error", id.Local, obs, err)
			}
		}
		if s.bulks != 1 || s.reads != 0 {
			t.Errorf("[#27-4] bulks=%d reads=%d, want 1 and 0", s.bulks, s.reads)
		}
	})

	t.Run("undeclared bulk implementation is not used", func(t *testing.T) {
		s := &stubBulk{declare: false, bulk: ok(time.Now())}
		o := sdk.NewCycleObserver(s, scope)
		if o.Bulk() {
			t.Errorf("[#27-4] an undeclared BulkObserver must get the per-resource path")
		}
		_, _ = o.Observe(ctx, a)
		if s.bulks != 0 || s.reads != 1 {
			t.Errorf("[#27-4] bulks=%d reads=%d, want 0 and 1", s.bulks, s.reads)
		}
	})
}
