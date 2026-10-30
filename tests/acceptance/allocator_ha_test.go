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

// allocator_ha_test.go — Backlog #25 (v1b): the allocator's HA minimal form
// (§19.8), the shard-local Lookup cache (§19.6), waiting-allocation (§19.9)
// and pools split by claim (§19.8).  Principle 8: a stopped satellite stops
// change, not maintenance.
//
// Tags:
//   [#25-1] Lookup cache: the steady state makes zero allocator calls; a
//           stopped allocator still answers cached facts; only the own
//           Release invalidates
//   [#25-2] fencing: promoting a standby fences the former active (and any
//           unpromoted writer) on the shared store
//   [#25-3] HTTP active/standby: the client fails over to the promoted
//           standby, keeps the same value per owner, skips a fenced old active
//   [#25-4] daemon (in-process): allocator down → drift on existing resources
//           is still repaired, a new request is waiting-allocation (it and its
//           downstream skipped, nothing pruned), allocated once the allocator
//           is back
//   [#25-5] chaos (processes): kill the allocator under a running daemon;
//           maintenance continues, only the new request waits; the standby
//           taking over clears it
//   [#25-6] pools split by claim: overlapping claimed pools and requests from
//           outside the claim are rejected; a shard declares and allocates
//           only its own pools
//   [#25-7] one-shot reconcile with the allocator down: the rest converges,
//           the new request waits, exit 0
//   [#105]  Backlog #105: a stale (past-TTL) cache hit is revalidated with
//           one Lookup; if the authority no longer has the owner (released
//           through the v1 manual /v1/release escape hatch, outside this
//           shard's own Release), the cache self-heals via a fresh Allocate
//           instead of going on handing out a value another owner now holds

package acceptance

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/allocator"
	"github.com/tjst-t/sakutei/v2/internal/fakeprovider"
	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/internal/render"
	"github.com/tjst-t/sakutei/v2/pkg/allocation"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// switchableAuthority stands in for an allocator that can be stopped: while
// down every call fails with allocation.ErrUnavailable.  calls counts the
// calls that reached it (down or not).
type switchableAuthority struct {
	inner allocation.AllocationAuthority
	down  atomic.Bool
	calls atomic.Int64
}

func (s *switchableAuthority) gate() error {
	s.calls.Add(1)
	if s.down.Load() {
		return fmt.Errorf("%w: allocator stopped (test)", allocation.ErrUnavailable)
	}
	return nil
}

func (s *switchableAuthority) Allocate(ctx context.Context, pool, owner string) (string, error) {
	if err := s.gate(); err != nil {
		return "", err
	}
	return s.inner.Allocate(ctx, pool, owner)
}

func (s *switchableAuthority) Reserve(ctx context.Context, pool, owner string, ttl time.Duration) (string, error) {
	if err := s.gate(); err != nil {
		return "", err
	}
	return s.inner.Reserve(ctx, pool, owner, ttl)
}

func (s *switchableAuthority) Confirm(ctx context.Context, pool, owner string) error {
	if err := s.gate(); err != nil {
		return err
	}
	return s.inner.Confirm(ctx, pool, owner)
}

func (s *switchableAuthority) Release(ctx context.Context, pool, owner string) error {
	if err := s.gate(); err != nil {
		return err
	}
	return s.inner.Release(ctx, pool, owner)
}

func (s *switchableAuthority) Lookup(ctx context.Context, pool, owner string) (string, bool, error) {
	if err := s.gate(); err != nil {
		return "", false, err
	}
	return s.inner.Lookup(ctx, pool, owner)
}

func (s *switchableAuthority) DeclarePool(name string, spec allocation.PoolSpec) error {
	if err := s.gate(); err != nil {
		return err
	}
	return s.inner.(render.PoolEnsurer).DeclarePool(name, spec)
}

func TestAllocatorHA_LookupCache(t *testing.T) {
	// [#25-1]
	ctx := context.Background()
	fa, _ := newAuthority(t, 100, 199)
	sw := &switchableAuthority{inner: fa}
	c := allocator.NewCache(sw)

	v, err := c.Allocate(ctx, "p", "net-1")
	if err != nil || v != "100" {
		t.Fatalf("[#25-1] first Allocate: %q %v", v, err)
	}
	if err := c.EnsurePool("p", 100, 199); err != nil {
		t.Fatalf("EnsurePool: %v", err)
	}
	before := sw.calls.Load()
	for i := 0; i < 100; i++ {
		if got, _ := c.Allocate(ctx, "p", "net-1"); got != v {
			t.Fatalf("[#25-1] cached Allocate changed value: %q", got)
		}
		if got, ok, _ := c.Lookup(ctx, "p", "net-1"); !ok || got != v {
			t.Fatalf("[#25-1] cached Lookup: %q %v", got, ok)
		}
		_ = c.EnsurePool("p", 100, 199)
	}
	if n := sw.calls.Load() - before; n != 0 {
		t.Fatalf("[#25-1] the steady state must make zero allocator calls; made %d", n)
	}

	sw.down.Store(true)
	if got, err := c.Allocate(ctx, "p", "net-1"); err != nil || got != v {
		t.Fatalf("[#25-1] a cached fact must be served while the allocator is down: %q %v", got, err)
	}
	if _, err := c.Allocate(ctx, "p", "net-2"); !errors.Is(err, allocation.ErrUnavailable) {
		t.Fatalf("[#25-1] a new request must fail with ErrUnavailable while down; got %v", err)
	}
	if err := c.Release(ctx, "p", "net-1"); err == nil {
		t.Fatalf("[#25-1] Release must not succeed while the allocator is down")
	}
	if got, _ := c.Allocate(ctx, "p", "net-1"); got != v {
		t.Fatalf("[#25-1] a deferred Release must keep the value in use; got %q", got)
	}

	sw.down.Store(false)
	if err := c.Release(ctx, "p", "net-1"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, ok, _ := c.Lookup(ctx, "p", "net-1"); ok {
		t.Fatalf("[#25-1] the own Release must invalidate the cached fact")
	}
}

func TestAllocatorHA_LookupCacheTTLRevalidation(t *testing.T) {
	// [#105] The v1 manual escape hatch (`POST /v1/release`, the only way to
	// release in v1 — docs/trial/allocation/run.sh step 8f) changes the
	// allocation fact from outside the shard's own Release.  Backlog #105:
	// an unbounded cache would keep answering the stale value forever; the
	// TTL revalidation must notice within Cache.TTL and self-heal via a
	// fresh Allocate, never going on claiming a value the authority has
	// since given to someone else.
	ctx := context.Background()
	fa, _ := newAuthority(t, 100, 101) // a 2-value pool
	now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	fa.Now = func() time.Time { return now }
	fa.Cooldown = time.Minute

	c := allocator.NewCache(fa)
	c.Now = func() time.Time { return now }
	c.TTL = time.Minute

	v, err := c.Allocate(ctx, "p", "A")
	if err != nil || v != "100" {
		t.Fatalf("[#105] first Allocate for A: %q %v", v, err)
	}

	// Manual release bypasses the cache entirely.
	if err := fa.Release(ctx, "p", "A"); err != nil {
		t.Fatalf("[#105] manual Release: %v", err)
	}

	// Still within TTL: this is the bounded exposure window the TTL
	// accepts (it must be short next to the 24h cooldown so a revalidation
	// always lands before a collision could happen).
	if got, _ := c.Allocate(ctx, "p", "A"); got != v {
		t.Fatalf("[#105] within TTL the cache must still answer the unrevalidated value; got %q", got)
	}

	// Past the cooldown, "100" is free again and another owner B gets it
	// straight from the authority (as if another shard, unaware of A's
	// stale cache, allocated it).
	now = now.Add(2 * time.Minute)
	vb, err := fa.Allocate(ctx, "p", "B")
	if err != nil || vb != v {
		t.Fatalf("[#105] B must receive the released value %q once cooldown clears; got %q %v", v, vb, err)
	}

	// A's cache entry is now stale too (TTL elapsed). The next Allocate for
	// A must revalidate, discover the authority no longer has A, and
	// self-heal with a fresh Allocate — not go on believing it still owns
	// "100", which would collide with B.
	got, err := c.Allocate(ctx, "p", "A")
	if err != nil {
		t.Fatalf("[#105] self-heal Allocate for A: %v", err)
	}
	if got == vb {
		t.Fatalf("[#105] A must not collide with B's value %q after revalidation; got %q too", vb, got)
	}
	if got != "101" {
		t.Fatalf("[#105] A must receive the only remaining free value 101; got %q", got)
	}

	// From here it is a normal fresh fact again: stable within TTL.
	again, _ := c.Allocate(ctx, "p", "A")
	if again != got {
		t.Fatalf("[#105] the healed value must now be stable: %q then %q", got, again)
	}

	if _, err := c.Allocate(ctx, "p", "C"); err == nil {
		t.Fatalf("[#105] setup: pool must be exhausted after A and B hold both values")
	}

	// Lookup must self-heal the same way as Allocate. Cache B too (an
	// idempotent Allocate on an owner that already has a value just adopts
	// it into the cache), release it manually, and let TTL elapse: a stale
	// Lookup for B must now report found=false (a clean miss) instead of
	// going on with the stale value.
	if cb, err := c.Allocate(ctx, "p", "B"); err != nil || cb != vb {
		t.Fatalf("[#105] caching B: %q %v (want %q)", cb, err, vb)
	}
	if err := fa.Release(ctx, "p", "B"); err != nil {
		t.Fatalf("[#105] manual Release of B: %v", err)
	}
	now = now.Add(2 * time.Minute) // past TTL again, cooldown too
	if _, ok, err := c.Lookup(ctx, "p", "B"); ok || err != nil {
		t.Fatalf("[#105] a stale Lookup must self-heal to found=false once the authority no longer has the owner; got ok=%v err=%v", ok, err)
	}
}

func TestAllocatorHA_Daemon_LookupCacheTTLRevalidation(t *testing.T) {
	// [#105] End-to-end through the real daemon process: --alloc-lookup-ttl
	// bounds how long the daemon's in-process Lookup cache trusts an
	// allocation before revalidating it, so a value released from outside
	// the daemon (the v1 manual escape hatch, e.g. a direct edit of the
	// store or an admin API call against a separate allocator process)
	// cannot be used forever once a collision risk exists.
	root := t.TempDir()
	intentDir := filepath.Join(root, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStateJSON(t, filepath.Join(intentDir, "net.yaml"), `pools:
  - name: p
    range: "100-101"
resources:
  - provider: fake
    id: net-1
    fields:
      name: net-1
      vlan:
        from_pool: p
`)
	state := filepath.Join(root, "state.json")
	writeStateJSON(t, state, `{"resources": {}, "provider_version": "v-test"}`)
	store := filepath.Join(root, "alloc.json")

	cmd, stdout, stderr := startDaemonWithArgs(t, intentDir, state, "50ms", "--alloc-store", store, "--alloc-lookup-ttl", "100ms")
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	fp := fakeprovider.New(state)
	field := func() string {
		obs, err := fp.Read(context.Background(), "net-1")
		if err != nil || obs.State == nil {
			return ""
		}
		return fmt.Sprint(obs.State["vlan"].Value)
	}
	dump := func() string {
		return fmt.Sprintf("stdout:\n%s\nstderr:\n%s\nstore:\n%s", stdout.String(), stderr.String(), readFile(t, store))
	}
	if !waitForCondition(10*time.Second, func() bool { return field() == "100" }) {
		t.Fatalf("[#105] initial allocation failed\n%s", dump())
	}

	// Manual release, outside the daemon — a direct edit of the store, the
	// same effect an admin's `/v1/release` call against a separate
	// allocator process has (docs/trial/allocation/run.sh step 8f).  "100"
	// then sits in its (real, 24h) cooldown, so the only way the daemon can
	// recover net-1 is a genuine self-heal to the pool's other free value
	// (101) — never by going on reporting "100", which a since-arrived
	// owner elsewhere could by then hold.
	if err := allocator.New(store).Release(context.Background(), "p", render.AllocationOwner(core.NewID("fake", core.NewScope(nil), "net-1"), "vlan")); err != nil {
		t.Fatalf("[#105] manual Release: %v", err)
	}

	if !waitForCondition(10*time.Second, func() bool { return field() == "101" }) {
		t.Fatalf("[#105] the daemon must notice the external release within --alloc-lookup-ttl and self-heal to a fresh value (101, since 100 is in cooldown)\n%s", dump())
	}
}

func TestAllocatorHA_Fencing(t *testing.T) {
	// [#25-2]
	ctx := context.Background()
	_, path := newAuthority(t, 1, 100)
	legacy := allocator.New(path) // single-instance writer, never promoted
	active := allocator.New(path)
	standby := allocator.New(path)

	g1, err := active.Promote()
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if _, err := legacy.Allocate(ctx, "p", "x"); !errors.Is(err, allocation.ErrFenced) {
		t.Fatalf("[#25-2] an unpromoted writer must be fenced once an HA generation exists; got %v", err)
	}
	v, err := active.Allocate(ctx, "p", "net-1")
	if err != nil {
		t.Fatalf("active Allocate: %v", err)
	}

	g2, err := standby.Promote() // failover: the standby takes over
	if err != nil || g2 <= g1 {
		t.Fatalf("[#25-2] promotion must raise the generation: %d → %d (%v)", g1, g2, err)
	}
	for name, op := range map[string]func() error{
		"Allocate": func() error { _, err := active.Allocate(ctx, "p", "net-2"); return err },
		"Release":  func() error { return active.Release(ctx, "p", "net-1") },
		"Confirm":  func() error { return active.Confirm(ctx, "p", "net-1") },
	} {
		if err := op(); !errors.Is(err, allocation.ErrFenced) {
			t.Fatalf("[#25-2] the former active's %s must be fenced; got %v", name, err)
		}
	}
	if err := active.CheckActive(); !errors.Is(err, allocation.ErrFenced) {
		t.Fatalf("[#25-2] CheckActive must report the fence; got %v", err)
	}
	if got, err := standby.Allocate(ctx, "p", "net-1"); err != nil || got != v {
		t.Fatalf("[#25-2] the new active must keep the allocation fact: %q %v (want %q)", got, err, v)
	}
}

func TestAllocatorHA_HTTPFailover(t *testing.T) {
	// [#25-3]
	ctx := context.Background()
	_, path := newAuthority(t, 100, 199)
	srvA := allocator.NewServer(allocator.New(path))
	if _, err := srvA.Promote(); err != nil {
		t.Fatal(err)
	}
	srvB := allocator.NewServer(allocator.New(path)) // standby
	a := httptest.NewServer(srvA)
	b := httptest.NewServer(srvB)
	defer b.Close()

	cl := allocator.NewClient([]string{a.URL, b.URL})
	v, err := cl.Allocate(ctx, "p", "net-1")
	if err != nil || v != "100" {
		t.Fatalf("[#25-3] Allocate via the active: %q %v", v, err)
	}
	if _, err := cl.Allocate(ctx, "nope", "x"); !errors.Is(err, allocation.ErrUnknownPool) {
		t.Fatalf("[#25-3] domain errors must cross the wire typed; got %v", err)
	}

	a.Close() // the active's node dies
	if _, err := cl.Allocate(ctx, "p", "net-2"); !errors.Is(err, allocation.ErrUnavailable) {
		t.Fatalf("[#25-3] with the active down and B still standby the client must report ErrUnavailable; got %v", err)
	}
	if _, err := srvB.Promote(); err != nil { // VM-HA promotes the standby
		t.Fatal(err)
	}
	if got, err := cl.Allocate(ctx, "p", "net-1"); err != nil || got != v {
		t.Fatalf("[#25-3] after failover the same owner must keep its value: %q %v (want %q)", got, err, v)
	}

	// The former active comes back with its old generation: it is fenced,
	// steps down, and the client keeps using B.
	a2 := httptest.NewServer(srvA)
	defer a2.Close()
	cl2 := allocator.NewClient([]string{a2.URL, b.URL})
	got, err := cl2.Allocate(ctx, "p", "net-3")
	if err != nil || got != "101" {
		t.Fatalf("[#25-3] the client must skip the fenced old active: %q %v", got, err)
	}
	if srvA.Active() {
		t.Fatalf("[#25-3] a fenced server must step down")
	}
}

// haIntent: db-net (plain) and app-net (composite → two children) allocated
// from tenant-vlans; extra resources are appended by the tests.
const haIntentBase = `pools:
  - name: tenant-vlans
    range: "100-199"
resources:
  - provider: fake
    id: app-net
    fields:
      kind: composite
      vlan:
        from_pool: tenant-vlans
  - provider: fake
    id: db-net
    fields:
      name: db-net
      vlan:
        from_pool: tenant-vlans
  - provider: fake
    id: static-a
    fields:
      name: static-a
      size: "1"
`

const haIntentNew = `  - provider: fake
    id: web-net
    fields:
      name: web-net
      vlan:
        from_pool: tenant-vlans
  - provider: fake
    id: web-app
    depends_on:
      - web-net
    fields:
      name: web-app
      size: "1"
`

func TestAllocatorHA_DaemonWaitingAllocation(t *testing.T) {
	// [#25-4]
	root := t.TempDir()
	intentDir := filepath.Join(root, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStateJSON(t, filepath.Join(intentDir, "net.yaml"), haIntentBase)
	fp := fakeprovider.New(filepath.Join(root, "state.json"))
	sw := &switchableAuthority{inner: allocator.New(filepath.Join(root, "alloc.json"))}
	cache := allocator.NewCache(sw)
	var errOut syncBuffer
	d := reconcile.NewDaemon(reconcile.Config{
		IntentDir:    intentDir,
		GeneratedDir: filepath.Join(root, "generated"),
		Provider:     fp,
		Allocation:   cache,
		Out:          io.Discard,
		ErrOut:       &errOut,
	})
	cycle := func() reconcile.CycleResult {
		t.Helper()
		res, err := d.RunCycle(context.Background())
		if err != nil {
			t.Fatalf("RunCycle: %v\n%s", err, errOut.String())
		}
		d.Wait(5 * time.Second)
		return res
	}
	field := func(local, name string) string {
		obs, err := fp.Read(context.Background(), local)
		if err != nil || obs.State == nil {
			return ""
		}
		return fmt.Sprint(obs.State[name].Value)
	}

	cycle()
	if field("db-net", "vlan") != "101" || field("app-net-child-a", "vlan") != "100" {
		t.Fatalf("[#25-4] initial convergence: db-net vlan=%q app-net-child-a vlan=%q", field("db-net", "vlan"), field("app-net-child-a", "vlan"))
	}
	cycle()
	steady := sw.calls.Load()
	cycle()
	if n := sw.calls.Load() - steady; n != 0 {
		t.Fatalf("[#25-4] a steady-state cycle must make zero allocator calls; made %d", n)
	}

	// Stop the allocator, drift an existing resource, request a new one.
	sw.down.Store(true)
	if err := fp.SeedResource("db-net", map[string]any{"name": "db-net", "vlan": "999"}); err != nil {
		t.Fatal(err)
	}
	writeStateJSON(t, filepath.Join(intentDir, "net.yaml"), haIntentBase+haIntentNew)
	res := cycle()

	if got := field("db-net", "vlan"); got != "101" {
		t.Fatalf("[#25-4] drift on an existing resource must be repaired with the allocator down; vlan=%q\n%s", got, errOut.String())
	}
	if strings.Join(res.Waiting, ",") != "web-net" {
		t.Fatalf("[#25-4] only web-net must be waiting-allocation; got %v", res.Waiting)
	}
	if field("web-net", "name") != "" || field("web-app", "name") != "" {
		t.Fatalf("[#25-4] a waiting resource and its downstream must not be created")
	}
	if res.Pruned != 0 || field("static-a", "name") != "static-a" || field("app-net-child-b", "vlan") != "100" {
		t.Fatalf("[#25-4] nothing may be pruned or disturbed while waiting (pruned=%d)", res.Pruned)
	}
	if !strings.Contains(errOut.String(), "waiting-allocation: fake//web-net") {
		t.Fatalf("[#25-4] waiting-allocation must be reported:\n%s", errOut.String())
	}

	// The allocator comes back: the next cycle allocates, then the
	// downstream follows.
	sw.down.Store(false)
	res = cycle()
	if len(res.Waiting) != 0 || field("web-net", "vlan") != "102" {
		t.Fatalf("[#25-4] once the allocator is back web-net must get vlan 102; waiting=%v vlan=%q", res.Waiting, field("web-net", "vlan"))
	}
	cycle()
	if field("web-app", "name") != "web-app" {
		t.Fatalf("[#25-4] the downstream of the formerly waiting resource must converge")
	}
}

// allocProc is a `sakutei allocator` process.
type allocProc struct {
	cmd  *exec.Cmd
	url  string
	done chan struct{}
}

func startAllocator(t *testing.T, store, listen string, extra ...string) *allocProc {
	t.Helper()
	args := append([]string{"allocator", "--store", store, "--listen", listen}, extra...)
	cmd := exec.Command(buildBinary(t), args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start allocator: %v", err)
	}
	p := &allocProc{cmd: cmd, done: make(chan struct{})}
	urlCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if _, u, ok := strings.Cut(sc.Text(), "listening on "); ok {
				urlCh <- u
			}
		}
	}()
	go func() { _ = cmd.Wait(); close(p.done) }()
	t.Cleanup(p.kill)
	select {
	case p.url = <-urlCh:
	case <-time.After(10 * time.Second):
		t.Fatalf("allocator did not start")
	}
	return p
}

func (p *allocProc) kill() {
	_ = p.cmd.Process.Kill()
	<-p.done
}

// freeAddr returns a loopback address with a currently free port.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func TestAllocatorHA_Chaos_KillAllocator(t *testing.T) {
	// [#25-5] The completion condition of #25: stop the allocator and the
	// steady-state convergence of existing resources goes on; only the new
	// allocation becomes waiting-allocation.
	root := t.TempDir()
	intentDir := filepath.Join(root, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStateJSON(t, filepath.Join(intentDir, "net.yaml"), haIntentBase)
	state := filepath.Join(root, "state.json")
	writeStateJSON(t, state, `{"resources": {}, "provider_version": "v-test"}`)
	store := filepath.Join(root, "alloc.json")

	active := startAllocator(t, store, "127.0.0.1:0")
	standbyAddr := freeAddr(t)
	urls := active.url + ",http://" + standbyAddr

	cmd, stdout, stderr := startDaemonWithArgs(t, intentDir, state, "100ms", "--alloc-url", urls)
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	fp := fakeprovider.New(state)
	field := func(local, name string) string {
		obs, err := fp.Read(context.Background(), local)
		if err != nil || obs.State == nil {
			return ""
		}
		return fmt.Sprint(obs.State[name].Value)
	}
	dump := func() string {
		return fmt.Sprintf("stdout:\n%s\nstderr:\n%s\nstate:\n%s", stdout.String(), stderr.String(), readFile(t, state))
	}
	if !waitForCondition(15*time.Second, func() bool {
		return field("db-net", "vlan") == "101" && field("app-net-child-b", "vlan") == "100" && field("static-a", "name") == "static-a"
	}) {
		t.Fatalf("[#25-5] initial convergence through the allocator service failed\n%s", dump())
	}

	// Chaos: SIGKILL the allocator.
	active.kill()

	// Existing resources: drift is still repaired, cycle after cycle.
	for i, bad := range []string{"901", "902"} {
		if err := fp.SeedResource("db-net", map[string]any{"name": "db-net", "vlan": bad}); err != nil {
			t.Fatal(err)
		}
		if err := fp.SeedResource("static-a", map[string]any{"name": "static-a", "size": "9"}); err != nil {
			t.Fatal(err)
		}
		if !waitForCondition(15*time.Second, func() bool {
			return field("db-net", "vlan") == "101" && field("static-a", "size") == "1"
		}) {
			t.Fatalf("[#25-5] drift #%d must be repaired with the allocator down\n%s", i+1, dump())
		}
	}

	// A new request waits; nothing else is held back or pruned.
	writeStateJSON(t, filepath.Join(intentDir, "net.yaml"), haIntentBase+haIntentNew)
	if !waitForCondition(15*time.Second, func() bool {
		return strings.Contains(stdout.String(), "waiting-allocation (retry next cycle): [web-net]")
	}) {
		t.Fatalf("[#25-5] web-net must be reported waiting-allocation\n%s", dump())
	}
	if err := fp.SeedResource("db-net", map[string]any{"name": "db-net", "vlan": "903"}); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(15*time.Second, func() bool { return field("db-net", "vlan") == "101" }) {
		t.Fatalf("[#25-5] maintenance must continue while web-net waits\n%s", dump())
	}
	if field("web-net", "name") != "" || field("web-app", "name") != "" {
		t.Fatalf("[#25-5] web-net and its downstream must not be created without an allocation\n%s", dump())
	}
	if field("app-net-child-a", "vlan") != "100" || field("static-a", "name") != "static-a" {
		t.Fatalf("[#25-5] existing resources must not be pruned or disturbed\n%s", dump())
	}

	// The standby takes over (VM-HA): waiting-allocation clears by itself.
	startAllocator(t, store, standbyAddr)
	if !waitForCondition(15*time.Second, func() bool {
		return field("web-net", "vlan") == "102" && field("web-app", "name") == "web-app"
	}) {
		t.Fatalf("[#25-5] after failover web-net must be allocated (102) and web-app converge\n%s", dump())
	}
}

const claimPoolsIntent = `pools:
  - name: site-a-vlans
    range: "100-199"
    claim:
      site: site-a
  - name: site-b-vlans
    range: "200-299"
    claim:
      site: site-b
  - name: shared-asns
    range: "100-150"
    space: asn
resources:
  - provider: fake
    id: a-net
    scope:
      site: site-a
    fields:
      name: a-net
      vlan:
        from_pool: site-a-vlans
      asn:
        from_pool: shared-asns
  - provider: fake
    id: b-net
    scope:
      site: site-b
    fields:
      name: b-net
      vlan:
        from_pool: site-b-vlans
`

func TestAllocatorHA_PoolSplitByClaim(t *testing.T) {
	// [#25-6]
	dir := t.TempDir()
	write := func(body string) {
		t.Helper()
		writeStateJSON(t, filepath.Join(dir, "net.yaml"), body)
	}

	write(strings.Replace(claimPoolsIntent, `range: "200-299"`, `range: "199-299"`, 1))
	if _, err := loader.LoadIntent(dir); err == nil || !strings.Contains(err.Error(), "disjoint") {
		t.Fatalf("[#25-6] overlapping pools of different claims must be rejected; got %v", err)
	}
	write(strings.Replace(claimPoolsIntent, "resources:",
		"  - name: site-b-asns\n    range: \"100-120\"\n    claim:\n      site: site-b\n    space: asn2\nresources:", 1))
	if _, err := loader.LoadIntent(dir); err != nil {
		t.Fatalf("[#25-6] pools in different spaces may overlap: %v", err)
	}
	write(strings.Replace(claimPoolsIntent, "from_pool: site-b-vlans", "from_pool: site-a-vlans", 1))
	if _, err := loader.LoadIntent(dir); err == nil || !strings.Contains(err.Error(), "outside that claim") {
		t.Fatalf("[#25-6] a request for another claim's pool must be rejected; got %v", err)
	}

	write(claimPoolsIntent)
	in, err := loader.LoadIntent(dir)
	if err != nil {
		t.Fatalf("LoadIntent: %v", err)
	}
	fa := allocator.New(filepath.Join(t.TempDir(), "alloc.json"))
	claimA := core.NewScope(map[string]string{"site": "site-a"})
	res, err := render.RenderAllocations(context.Background(), fa, render.AllocAllocate, in, claimA)
	if err != nil {
		t.Fatalf("RenderAllocations: %v", err)
	}
	if len(res.Waiting) != 0 {
		t.Fatalf("[#25-6] shard site-a must allocate its own requests: waiting=%v", res.Waiting)
	}
	if _, err := fa.Owners("site-b-vlans"); !errors.Is(err, allocation.ErrUnknownPool) {
		t.Fatalf("[#25-6] shard site-a must not declare site-b's pool; got %v", err)
	}
	if owners, _ := fa.Owners("site-a-vlans"); len(owners) != 1 {
		t.Fatalf("[#25-6] shard site-a must allocate a-net from site-a-vlans; owners=%v", owners)
	}
	if owners, _ := fa.Owners("shared-asns"); len(owners) != 1 {
		t.Fatalf("[#25-6] a shared pool (no claim) belongs to every shard; owners=%v", owners)
	}
	for _, r := range res.Resources {
		if r.ID.Local == "b-net" && r.Fields["vlan"] != "" {
			t.Fatalf("[#25-6] shard site-a must leave site-b's request alone; got vlan=%q", r.Fields["vlan"])
		}
	}

	// The CLI: reconcile --claim site=site-a converges only a-net.
	intent, state := filepath.Join(t.TempDir(), "intent"), filepath.Join(t.TempDir(), "state.json")
	if err := os.MkdirAll(intent, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStateJSON(t, filepath.Join(intent, "net.yaml"), claimPoolsIntent)
	writeStateJSON(t, state, `{"resources": {}, "provider_version": "v-test"}`)
	store := filepath.Join(t.TempDir(), "alloc.json")
	out := mustRun(t, "reconcile", "--intent", intent, "--state", state, "--alloc-store", store, "--claim", "site=site-a")
	st := readFile(t, state)
	if !strings.Contains(st, `"a-net"`) || strings.Contains(st, `"b-net"`) || !strings.Contains(st, `"vlan": "100"`) {
		t.Fatalf("[#25-6] reconcile --claim site=site-a must converge a-net only:\n%s\n%s", out, st)
	}
	if strings.Contains(readFile(t, store), "site-b-vlans") {
		t.Fatalf("[#25-6] shard site-a must not declare site-b-vlans in its store")
	}
}

func TestAllocatorHA_OneShotReconcileWaiting(t *testing.T) {
	// [#25-7]
	intent, state, _ := setupAllocCLI(t)
	writeStateJSON(t, filepath.Join(intent, "net.yaml"), allocIntent+`  - provider: fake
    id: plain
    fields:
      name: plain
`)
	dead := "http://" + freeAddr(t)
	out, code := runSakutei(t, "reconcile", "--intent", intent, "--state", state, "--alloc-url", dead)
	if code != 0 || !strings.Contains(out, "waiting-allocation") {
		t.Fatalf("[#25-7] reconcile with the allocator down must exit 0 and report waiting-allocation (exit %d):\n%s", code, out)
	}
	st := readFile(t, state)
	if !strings.Contains(st, `"plain"`) || strings.Contains(st, `"db-net"`) || strings.Contains(st, "waiting-allocation") {
		t.Fatalf("[#25-7] the plain resource must converge and the waiting ones must not be applied:\n%s", st)
	}
	if out, code := runSakutei(t, "plan", "--intent", intent, "--state", state, "--alloc-url", dead); code != 0 || !strings.Contains(out, "allocator unavailable") {
		t.Fatalf("[#25-7] plan must degrade to pending with the reason (exit %d):\n%s", code, out)
	}
	if _, code := runSakutei(t, "reconcile", "--intent", intent, "--state", state, "--alloc-url", dead, "--alloc-store", "x"); code == 0 {
		t.Fatalf("[#25-7] --alloc-store and --alloc-url together must be rejected")
	}
}
