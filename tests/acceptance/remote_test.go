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

// remote_test.go — Backlog #130: one daemon handles several remote hosts
// through the execution layer (internal/remote).  The hosts here are fake
// (remotefake: the zone file scripts really run with sh / flock under a
// per-host directory, nft is the fake nft); real sshd hosts are exercised by
// tests/integration/remote (make integration-remote).
//
// AC tags:
//
//	AC-130-2-1  nftables live on 2 hosts: ContractTester (incl. three-valued:
//	            an unreachable host is unknown) and BulkEquivalenceTester pass
//	AC-130-2-2  the same table/chain on 2 hosts does not collide: ids carry the
//	            host, Apply goes to that host only
//	AC-130-2-3  one host unreachable: BulkObserve still returns the other host,
//	            Read of the down host is unknown, List is not complete; a host
//	            key mismatch is a PermanentError
//	AC-130-3-1  dnszone on 2 hosts: ContractTester and BulkEquivalenceTester
//	AC-130-3-2  the zone files are separate; the reload command runs on the
//	            host after a change only; its failure fails the Apply and it is
//	            run again by the next Apply
//	AC-130-3-3  dnszone: a down host is unknown (never absent), a zone whose
//	            directory cannot be searched is unknown, a host key mismatch
//	            is permanent; the remote write keeps the file's mode
//	AC-130-4-1  the registry builds both providers from remote_config +
//	            ruleset_hosts / zone_hosts; without them nothing changes
package acceptance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/dnszone"
	"github.com/tjst-t/sakutei/v2/internal/nftables"
	"github.com/tjst-t/sakutei/v2/internal/nftables/nftfake"
	"github.com/tjst-t/sakutei/v2/internal/remote"
	"github.com/tjst-t/sakutei/v2/internal/remote/remotefake"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
	"github.com/tjst-t/sakutei/v2/pkg/registry"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
)

// nftHost is a fake remote host whose nft is a fake nft.
func nftHost(name string, nft *nftfake.NFT) *remotefake.Host {
	return &remotefake.Host{Name: name, Commands: map[string]remotefake.Handler{
		"nft": func(ctx context.Context, args []string, stdin []byte) ([]byte, error) {
			out, err := nft.Run(ctx, args, stdin)
			var ee *nftables.ExitError
			if errors.As(err, &ee) {
				return out, &remote.ExitError{Code: ee.Code, Stderr: ee.Stderr}
			}
			return out, err
		},
	}}
}

func newRemoteNFT(t *testing.T, hosts ...*remotefake.Host) *nftables.LiveProvider {
	t.Helper()
	cfg := nftables.LiveConfig{Tables: []nftables.TableRef{{Family: "inet", Name: "filter"}}}
	for _, h := range hosts {
		cfg.Hosts = append(cfg.Hosts, nftables.LiveHost{Name: h.Name, Runner: nftables.RemoteRunner{Exec: h}})
	}
	p, err := nftables.NewLive(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.ValidateProvider(p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRemoteNFT_ContractAndBulk(t *testing.T) {
	// [AC-130-2-1]
	n1, n2 := &nftfake.NFT{}, &nftfake.NFT{}
	n1.AddChain("inet", "filter", "web", "tcp dport 80 accept")
	n2.AddChain("inet", "filter", "web", "tcp dport 443 accept")
	n2.AddBaseChain("inet", "filter", "input", "type filter hook input priority filter; policy accept;", "jump web")
	h1, h2 := nftHost("fw1", n1), nftHost("fw2", n2)
	p := newRemoteNFT(t, h1, h2)
	ct := sdk.ContractTester{
		Provider: p,
		Lister:   p,
		Scope:    core.NewScope(nil),
		Examples: map[string]string{
			"family": "inet", "table": "filter", "chain": "contract",
			"rules": "tcp dport 22 accept" + nftables.RuleSep + "ip saddr 10.0.0.0/8 counter drop",
		},
		MakeID: func(f map[string]string) string {
			return "fw2/" + f["family"] + "/" + f["table"] + "/" + f["chain"]
		},
		MakeUnreachable: func() (func(), error) {
			h2.SetDown(remote.Unreachable)
			return h2.Up, nil
		},
	}
	ct.RunAll(t)
	bt := sdk.BulkEquivalenceTester{Provider: p, Lister: p, Scope: core.NewScope(nil)}
	bt.Run(t)
}

func TestRemoteNFT_SameChainTwoHosts(t *testing.T) {
	// [AC-130-2-2]
	n1, n2 := &nftfake.NFT{}, &nftfake.NFT{}
	n1.AddChain("inet", "filter", "web", "tcp dport 80 accept")
	n2.AddChain("inet", "filter", "web", "tcp dport 80 accept")
	p := newRemoteNFT(t, nftHost("fw1", n1), nftHost("fw2", n2))
	ctx := context.Background()
	lr, err := p.List(ctx, core.NewScope(nil))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, id := range lr.IDs {
		ids = append(ids, id.Local)
	}
	if strings.Join(ids, ",") != "fw1/inet/filter/web,fw2/inet/filter/web" {
		t.Fatalf("[AC-130-2-2] List = %v", ids)
	}
	op := chainOpOn("fw2/inet/filter/web", core.OpKindUpdate, "tcp dport 8080 accept")
	if err := p.Apply(ctx, core.Plan{Ops: []core.Op{op}}); err != nil {
		t.Fatal(err)
	}
	for local, want := range map[string]string{"fw1/inet/filter/web": "tcp dport 80 accept", "fw2/inet/filter/web": "tcp dport 8080 accept"} {
		obs, err := p.Read(ctx, local)
		if err != nil || obs.State["rules"].Value != want {
			t.Errorf("[AC-130-2-2] %s = %v, %v; want %q", local, obs.State, err, want)
		}
	}
	// An id without the host (the local form) is refused.
	if err := p.ValidateIntent("inet/filter/web", nil); err == nil {
		t.Error("[AC-130-2-2] an id without the host must be refused")
	}
	if err := p.ValidateIntent("fw3/inet/filter/web", nil); err == nil {
		t.Error("[AC-130-2-2] an unknown host must be refused")
	}
}

func chainOpOn(local string, kind core.OpKind, rules ...string) core.Op {
	parts := strings.Split(local, "/")
	return core.Op{
		Kind:   kind,
		Target: core.NewID(nftables.ProviderName, core.NewScope(nil), local),
		Fields: []core.FieldDiff{{Field: "family", To: parts[1]}, {Field: "table", To: parts[2]}, {Field: "chain", To: parts[3]}, {Field: "rules", To: nftEncoded(rules...)}},
	}
}

func TestRemoteNFT_OneHostDown(t *testing.T) {
	// [AC-130-2-3]
	n1, n2 := &nftfake.NFT{}, &nftfake.NFT{}
	n1.AddChain("inet", "filter", "web", "accept")
	n2.AddChain("inet", "filter", "web", "accept")
	h1, h2 := nftHost("fw1", n1), nftHost("fw2", n2)
	p := newRemoteNFT(t, h1, h2)
	ctx := context.Background()
	h2.SetDown(remote.Unreachable)

	b, err := p.BulkObserve(ctx, core.NewScope(nil))
	if err != nil {
		t.Fatalf("[AC-130-2-3] BulkObserve with one host down: %v", err)
	}
	if len(b.Observations) != 1 {
		t.Errorf("[AC-130-2-3] %d entries, want only fw1's", len(b.Observations))
	}
	obs, err := p.Read(ctx, "fw2/inet/filter/web")
	if err == nil || obs.Availability != core.AvailabilityUnknown {
		t.Errorf("[AC-130-2-3] Read of the down host = %+v, %v; want unknown", obs, err)
	}
	if _, err := p.List(ctx, core.NewScope(nil)); err == nil {
		t.Error("[AC-130-2-3] List must not be complete with a host down")
	}
	h1.SetDown(remote.Unreachable)
	if _, err := p.BulkObserve(ctx, core.NewScope(nil)); err == nil {
		t.Error("[AC-130-2-3] BulkObserve with every host down must be an error")
	}
	h1.Up()
	h2.SetDown(remote.HostKeyMismatch)
	_, err = p.Read(ctx, "fw2/inet/filter/web")
	var pe *provider.PermanentError
	if !errors.As(err, &pe) {
		t.Errorf("[AC-130-2-3] a host key mismatch must be permanent: %T %v", err, err)
	}
	// Apply to the down host changes nothing anywhere.
	before := len(h1.Calls())
	if err := p.Apply(ctx, core.Plan{Ops: []core.Op{chainOpOn("fw2/inet/filter/web", core.OpKindUpdate, "drop")}}); err == nil {
		t.Error("[AC-130-2-3] Apply to a down host must fail")
	}
	if len(h1.Calls()) != before {
		t.Error("[AC-130-2-3] Apply to fw2 touched fw1")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// dnszone
// ─────────────────────────────────────────────────────────────────────────────

const remoteZonePath = "/zones/example.com.zone"

type zoneHost struct {
	*remotefake.Host
	reloads  *int
	failNext *bool
}

func newZoneHost(t *testing.T, name, content string) zoneHost {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "zones"), 0o755); err != nil {
		t.Fatal(err)
	}
	if content != "" {
		if err := os.WriteFile(filepath.Join(root, remoteZonePath), []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(root, remoteZonePath), 0o640); err != nil { // whatever the umask
			t.Fatal(err)
		}
	}
	n, fail := 0, false
	h := &remotefake.Host{Name: name, Root: root, Commands: map[string]remotefake.Handler{
		"knotc": func(_ context.Context, args []string, _ []byte) ([]byte, error) {
			if strings.Join(args, " ") != "zone-reload example.com." {
				return nil, &remote.ExitError{Code: 2, Stderr: "bad args"}
			}
			if fail {
				fail = false
				return nil, &remote.ExitError{Code: 1, Stderr: "error: (zone not found)"}
			}
			n++
			return nil, nil
		},
	}}
	return zoneHost{Host: h, reloads: &n, failNext: &fail}
}

func (z zoneHost) file(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(z.Root, remoteZonePath))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newRemoteZone(t *testing.T, hosts ...zoneHost) *dnszone.Provider {
	t.Helper()
	execs := map[string]remote.Executor{}
	var order []string
	for _, h := range hosts {
		execs[h.Name] = h.Host
		order = append(order, h.Name)
	}
	p := dnszone.New(remoteZonePath, dnszone.WithHosts(order, execs),
		dnszone.WithReload([]string{"knotc", "zone-reload", "example.com."}))
	if err := provider.ValidateProvider(p); err != nil {
		t.Fatal(err)
	}
	return p
}

const remoteZoneSeed = `$ORIGIN example.com.
@ 300 IN SOA ns1 admin 2026102700 3600 900 604800 300
@ 300 IN NS ns1.example.com.
www 300 IN A 192.0.2.10
`

func TestRemoteZone_ContractAndBulk(t *testing.T) {
	// [AC-130-3-1]
	h1, h2 := newZoneHost(t, "ns1", remoteZoneSeed), newZoneHost(t, "ns2", "")
	p := newRemoteZone(t, h1, h2)
	ct := sdk.ContractTester{
		Provider: p,
		Lister:   p,
		Scope:    core.NewScope(nil),
		SampleID: "ns2/contract.example.com./A",
		SampleDesired: map[string]string{
			"name": "contract.example.com.", "type": "A", "value": "192.0.2.1", "ttl": "300",
		},
		MakeUnreachable: func() (func(), error) {
			h2.SetDown(remote.Unreachable)
			return h2.Up, nil
		},
	}
	ct.RunAll(t)
	bt := sdk.BulkEquivalenceTester{Provider: p, Lister: p, Scope: core.NewScope(nil)}
	bt.Run(t)
}

func zoneOp(local, value string) core.Op {
	return core.Op{
		Kind:   core.OpKindCreate,
		Target: core.NewID(dnszone.ProviderName, core.NewScope(nil), local),
		Fields: []core.FieldDiff{{Field: "value", To: value}, {Field: "ttl", To: "300"}},
	}
}

func TestRemoteZone_TwoHostsAndReload(t *testing.T) {
	// [AC-130-3-2]
	h1, h2 := newZoneHost(t, "ns1", remoteZoneSeed), newZoneHost(t, "ns2", remoteZoneSeed)
	p := newRemoteZone(t, h1, h2)
	ctx := context.Background()

	if err := p.Apply(ctx, core.Plan{Ops: []core.Op{zoneOp("ns1/api.example.com./A", "192.0.2.20")}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h1.file(t), "api.example.com. 300 IN A 192.0.2.20") || strings.Contains(h2.file(t), "api.") {
		t.Fatalf("[AC-130-3-2] the change went to the wrong zone:\nns1:\n%s\nns2:\n%s", h1.file(t), h2.file(t))
	}
	if !strings.Contains(h1.file(t), "2026102701") && !strings.Contains(h1.file(t), "IN SOA ns1 admin 20") {
		t.Errorf("[AC-130-3-2] SOA serial:\n%s", h1.file(t))
	}
	if *h1.reloads != 1 || *h2.reloads != 0 {
		t.Errorf("[AC-130-3-2] reloads ns1=%d ns2=%d; want 1 and 0", *h1.reloads, *h2.reloads)
	}
	// An Apply that changes nothing does not reload.
	if err := p.Apply(ctx, core.Plan{Ops: []core.Op{zoneOp("ns1/api.example.com./A", "192.0.2.20")}}); err != nil {
		t.Fatal(err)
	}
	if *h1.reloads != 1 {
		t.Errorf("[AC-130-3-2] an Apply without a change reloaded (%d)", *h1.reloads)
	}
	// Same record on both hosts: two ids.
	if err := p.Apply(ctx, core.Plan{Ops: []core.Op{zoneOp("ns2/api.example.com./A", "192.0.2.21")}}); err != nil {
		t.Fatal(err)
	}
	for local, want := range map[string]string{"ns1/api.example.com./A": "192.0.2.20", "ns2/api.example.com./A": "192.0.2.21"} {
		obs, err := p.Read(ctx, local)
		if err != nil || obs.State["value"].Value != want {
			t.Errorf("[AC-130-3-2] %s = %v, %v", local, obs.State, err)
		}
	}
	// A failed reload fails the Apply and is run again by the next Apply.
	*h1.failNext = true
	err := p.Apply(ctx, core.Plan{Ops: []core.Op{zoneOp("ns1/www.example.com./A", "192.0.2.11")}})
	if err == nil || !strings.Contains(err.Error(), "reload") {
		t.Fatalf("[AC-130-3-2] a failed reload must fail the Apply: %v", err)
	}
	if !strings.Contains(h1.file(t), "192.0.2.11") {
		t.Error("[AC-130-3-2] the zone was not written")
	}
	if err := p.Apply(ctx, core.Plan{Ops: []core.Op{zoneOp("ns1/www.example.com./A", "192.0.2.11")}}); err != nil {
		t.Fatal(err)
	}
	if *h1.reloads != 2 {
		t.Errorf("[AC-130-3-2] the pending reload was not run again (%d)", *h1.reloads)
	}
	// The remote write keeps the file's mode (0640 here).
	fi, err := os.Stat(filepath.Join(h1.Root, remoteZonePath))
	if err != nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("[AC-130-3-3] mode after the write: %v %v", fi.Mode(), err)
	}
	left, _ := filepath.Glob(filepath.Join(h1.Root, "zones", ".sakutei-dnszone-*"))
	if len(left) != 0 {
		t.Errorf("[AC-130-3-3] temp files left: %v", left)
	}
	// The local id form is refused.
	if err := p.ValidateIntent("www.example.com./A", nil); err == nil {
		t.Error("[AC-130-3-2] an id without the host must be refused")
	}
	if err := p.ValidateIntent("ns1/WWW.example.com./A", nil); err == nil || !strings.Contains(err.Error(), "ns1/www.example.com./A") {
		t.Errorf("[AC-130-3-2] the canonical id must carry the host: %v", err)
	}
}

func TestRemoteZone_Unobservable(t *testing.T) {
	// [AC-130-3-3]
	h1, h2 := newZoneHost(t, "ns1", remoteZoneSeed), newZoneHost(t, "ns2", remoteZoneSeed)
	p := newRemoteZone(t, h1, h2)
	ctx := context.Background()
	h2.SetDown(remote.AuthFailed)
	obs, err := p.Read(ctx, "ns2/www.example.com./A")
	if err == nil || obs.Availability != core.AvailabilityUnknown {
		t.Errorf("[AC-130-3-3] down host: %+v %v", obs, err)
	}
	b, err := p.BulkObserve(ctx, core.NewScope(nil))
	if err != nil || len(b.Observations) != 1 {
		t.Errorf("[AC-130-3-3] BulkObserve with ns2 down = %d entries, %v; want ns1's", len(b.Observations), err)
	}
	if _, err := p.List(ctx, core.NewScope(nil)); err == nil {
		t.Error("[AC-130-3-3] List must not be complete with a host down")
	}
	if err := p.Apply(ctx, core.Plan{Ops: []core.Op{zoneOp("ns2/x.example.com./A", "192.0.2.9")}}); err == nil {
		t.Error("[AC-130-3-3] Apply to a down host must fail")
	}
	h2.SetDown(remote.HostKeyMismatch)
	_, err = p.Read(ctx, "ns2/www.example.com./A")
	var pe *provider.PermanentError
	if !errors.As(err, &pe) {
		t.Errorf("[AC-130-3-3] host key mismatch: %T %v", err, err)
	}
	h2.Up()

	// A zone directory that cannot be searched: unknown, not absent.
	if os.Getuid() != 0 {
		dir := filepath.Join(h1.Root, "zones")
		if err := os.Chmod(dir, 0o600); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(dir, 0o755)
		obs, err := p.Read(ctx, "ns1/www.example.com./A")
		if err == nil || obs.Availability != core.AvailabilityUnknown {
			t.Errorf("[AC-130-3-3] unsearchable directory: %+v %v; want unknown", obs, err)
		}
	}
	// A missing zone file in a searchable directory is empty.
	h3 := newZoneHost(t, "ns3", "")
	p3 := newRemoteZone(t, h3)
	obs, err = p3.Read(ctx, "ns3/www.example.com./A")
	if err != nil || obs.Availability != core.AvailabilityOk || obs.State != nil {
		t.Errorf("[AC-130-3-3] missing zone: %+v %v; want absent", obs, err)
	}
}

func TestRemote_Registry(t *testing.T) {
	// [AC-130-4-1]
	dir := t.TempDir()
	cfg := filepath.Join(dir, "hosts.yaml")
	if err := os.WriteFile(cfg, []byte("known_hosts: known_hosts\nhosts:\n  fw1:\n    address: 192.0.2.1\n  fw2:\n    address: 192.0.2.2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := registry.DefaultRegistry()
	p, err := reg.Get("nftables", map[string]string{"ruleset_target": "live", "ruleset_tables": "inet/filter", "remote_config": cfg, "ruleset_hosts": "fw1,fw2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.(provider.IntentValidator).ValidateIntent("fw2/inet/filter/web", nil); err != nil {
		t.Errorf("[AC-130-4-1] nftables: %v", err)
	}
	z, err := reg.Get("dnszone", map[string]string{"zone": "/var/lib/knot/example.com.zone", "remote_config": cfg, "zone_hosts": "fw1", "zone_reload": "knotc zone-reload example.com."})
	if err != nil {
		t.Fatal(err)
	}
	if err := z.(provider.IntentValidator).ValidateIntent("fw1/www.example.com./A", nil); err != nil {
		t.Errorf("[AC-130-4-1] dnszone: %v", err)
	}
	// Without hosts: the ids are as before.
	z0, err := reg.Get("dnszone", map[string]string{"zone": filepath.Join(dir, "z.db")})
	if err != nil {
		t.Fatal(err)
	}
	if err := z0.(provider.IntentValidator).ValidateIntent("www.example.com./A", nil); err != nil {
		t.Errorf("[AC-130-4-1] local dnszone: %v", err)
	}
	for name, c := range map[string]map[string]string{
		"hosts without remote_config": {"ruleset_target": "live", "ruleset_tables": "inet/filter", "ruleset_hosts": "fw1"},
		"unknown host":                {"ruleset_target": "live", "ruleset_tables": "inet/filter", "ruleset_hosts": "fw9", "remote_config": cfg},
		"hosts on the file target":    {"ruleset_hosts": "fw1", "remote_config": cfg},
	} {
		if _, err := reg.Get("nftables", c); err == nil {
			t.Errorf("[AC-130-4-1] nftables %s must be an error", name)
		}
	}
	for name, c := range map[string]map[string]string{
		"relative zone": {"zone": "zone.db", "remote_config": cfg, "zone_hosts": "fw1"},
		"no zone":       {"state": "/x", "remote_config": cfg, "zone_hosts": "fw1"},
		"no config":     {"zone": "/z", "zone_hosts": "fw1"},
	} {
		if _, err := reg.Get("dnszone", c); err == nil {
			t.Errorf("[AC-130-4-1] dnszone %s must be an error", name)
		}
	}
}
