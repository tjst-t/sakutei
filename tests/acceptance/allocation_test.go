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

// allocation_test.go — Backlog #24 (v1b): AllocationAuthority (§19.3), the
// file authority (§19.9, stdlib-only), and allocation in render phase 1
// (§9.1) across plan / reconcile / status / the daemon.
//
// Tags:
//   [#24-1] Allocate is idempotent per owner; lowest free first; exhausted
//           and unknown pools are errors
//   [#24-2] Reserve → Confirm; an expired reservation frees its value
//   [#24-3] Release puts the value in cooldown; reused only after it ends
//   [#24-4] concurrent Allocate from separate authorities on one store never
//           hands out a value twice (file lock)
//   [#24-5] EnsurePool refuses a range that would exclude an allocation
//   [#24-6] CLI: plan only looks up (pending, store untouched); reconcile
//           allocates and the value reaches composite children; re-running is
//           a no-op; rendered_revision carries the allocations
//   [#24-7] the owner excludes the scope: a scope change keeps the value
//   [#24-8] errors: from_pool without --alloc-store; undeclared pool
//   [#24-9] the daemon allocates too

package acceptance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/allocator"
	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/render"
	"github.com/tjst-t/sakutei/v2/pkg/allocation"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

func newAuthority(t *testing.T, lo, hi int) (*allocator.FileAuthority, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "alloc.json")
	a := allocator.New(path)
	if err := a.EnsurePool("p", lo, hi); err != nil {
		t.Fatalf("EnsurePool: %v", err)
	}
	return a, path
}

func TestAllocator_AllocateIdempotentLowestFirst(t *testing.T) {
	// [#24-1]
	ctx := context.Background()
	a, _ := newAuthority(t, 10, 11)
	v1, err := a.Allocate(ctx, "p", "owner-1")
	if err != nil || v1 != "10" {
		t.Fatalf("[#24-1] first allocation: %q %v, want 10", v1, err)
	}
	again, _ := a.Allocate(ctx, "p", "owner-1")
	if again != v1 {
		t.Fatalf("[#24-1] Allocate must be idempotent per owner: %q then %q", v1, again)
	}
	v2, _ := a.Allocate(ctx, "p", "owner-2")
	if v2 != "11" {
		t.Fatalf("[#24-1] second owner must get the next free value 11; got %q", v2)
	}
	if _, err := a.Allocate(ctx, "p", "owner-3"); !errors.Is(err, allocation.ErrPoolExhausted) {
		t.Fatalf("[#24-1] a full pool must return ErrPoolExhausted; got %v", err)
	}
	if _, err := a.Allocate(ctx, "nope", "owner-1"); !errors.Is(err, allocation.ErrUnknownPool) {
		t.Fatalf("[#24-1] an unknown pool must return ErrUnknownPool; got %v", err)
	}
	if v, ok, _ := a.Lookup(ctx, "p", "owner-2"); !ok || v != "11" {
		t.Fatalf("[#24-1] Lookup must return the allocation; got %q %v", v, ok)
	}
}

func TestAllocator_ReserveConfirmExpire(t *testing.T) {
	// [#24-2]
	ctx := context.Background()
	a, _ := newAuthority(t, 1, 5)
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	a.Now = func() time.Time { return now }

	r, err := a.Reserve(ctx, "p", "pr-1", time.Hour)
	if err != nil || r != "1" {
		t.Fatalf("[#24-2] Reserve: %q %v", r, err)
	}
	if err := a.Confirm(ctx, "p", "pr-1"); err != nil {
		t.Fatalf("[#24-2] Confirm: %v", err)
	}
	now = now.Add(2 * time.Hour) // past the TTL, but confirmed: stays
	if v, ok, _ := a.Lookup(ctx, "p", "pr-1"); !ok || v != "1" {
		t.Fatalf("[#24-2] a confirmed reservation must not expire; got %q %v", v, ok)
	}

	r2, _ := a.Reserve(ctx, "p", "pr-2", time.Hour) // gets 2
	now = now.Add(2 * time.Hour)                    // pr-2 expires
	if _, ok, _ := a.Lookup(ctx, "p", "pr-2"); ok {
		t.Fatalf("[#24-2] an expired reservation must not be found")
	}
	if err := a.Confirm(ctx, "p", "pr-2"); !errors.Is(err, allocation.ErrNoReservation) {
		t.Fatalf("[#24-2] confirming an expired reservation must fail; got %v", err)
	}
	if v, _ := a.Allocate(ctx, "p", "other"); v != r2 {
		t.Fatalf("[#24-2] an expired reservation must free its value %q for reuse; got %q", r2, v)
	}
}

func TestAllocator_ReleaseCooldown(t *testing.T) {
	// [#24-3]
	ctx := context.Background()
	a, _ := newAuthority(t, 1, 2)
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	a.Now = func() time.Time { return now }
	a.Cooldown = time.Hour

	v, _ := a.Allocate(ctx, "p", "old") // 1
	if err := a.Release(ctx, "p", "old"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got, _ := a.Allocate(ctx, "p", "new-1"); got == v {
		t.Fatalf("[#24-3] a released value must not be reused during the cooldown; got %q", got)
	}
	now = now.Add(2 * time.Hour)
	if got, _ := a.Allocate(ctx, "p", "new-2"); got != v {
		t.Fatalf("[#24-3] after the cooldown the value %q must be reusable; got %q", v, got)
	}
}

func TestAllocator_ConcurrentAllocateNoDuplicates(t *testing.T) {
	// [#24-4] separate authorities (separate in-process mutexes) on one
	// store: only the file lock keeps them from handing out a value twice.
	ctx := context.Background()
	_, path := newAuthority(t, 1, 1000)
	const n = 40
	values := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			values[i], errs[i] = allocator.New(path).Allocate(ctx, "p", "owner-"+strconv.Itoa(i))
		}(i)
	}
	wg.Wait()
	seen := map[string]int{}
	for i, v := range values {
		if errs[i] != nil {
			t.Fatalf("[#24-4] Allocate: %v", errs[i])
		}
		if j, dup := seen[v]; dup {
			t.Fatalf("[#24-4] value %s handed out twice (owners %d and %d)", v, j, i)
		}
		seen[v] = i
	}
}

func TestAllocator_EnsurePoolRefusesShrinkOverAllocation(t *testing.T) {
	// [#24-5]
	ctx := context.Background()
	a, _ := newAuthority(t, 1, 10)
	_, _ = a.Allocate(ctx, "p", "a") // 1
	_, _ = a.Allocate(ctx, "p", "b") // 2
	if err := a.EnsurePool("p", 2, 10); err == nil {
		t.Fatalf("[#24-5] shrinking the range past an allocated value must be refused")
	}
	if err := a.EnsurePool("p", 1, 5); err != nil {
		t.Fatalf("[#24-5] shrinking while keeping every allocation must be allowed: %v", err)
	}
}

const allocIntent = `pools:
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
`

func setupAllocCLI(t *testing.T) (intent, state, store string) {
	t.Helper()
	tmp := t.TempDir()
	intent = filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intent, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStateJSON(t, filepath.Join(intent, "net.yaml"), allocIntent)
	state = filepath.Join(tmp, "state.json")
	writeStateJSON(t, state, `{"resources": {}, "provider_version": "v-test"}`)
	return intent, state, filepath.Join(tmp, "alloc.json")
}

func TestAllocation_CLI_PlanReconcile(t *testing.T) {
	// [#24-6]
	intent, state, store := setupAllocCLI(t)

	out, code := runSakutei(t, "plan", "--intent", intent, "--state", state, "--alloc-store", store)
	if code != 0 || !strings.Contains(out, "pending allocation") {
		t.Fatalf("[#24-6] plan must show pending allocations (exit %d):\n%s", code, out)
	}
	if _, err := os.Stat(store); err == nil {
		t.Fatalf("[#24-6] plan is read-only and must not create the allocation store")
	}

	out, code = runSakutei(t, "reconcile", "--intent", intent, "--state", state, "--alloc-store", store)
	if code != 0 {
		t.Fatalf("[#24-6] reconcile failed (exit %d):\n%s", code, out)
	}
	st := readFile(t, state)
	for _, want := range []string{`"app-net-child-a"`, `"app-net-child-b"`, `"db-net"`} {
		if !strings.Contains(st, want) {
			t.Fatalf("[#24-6] %s must exist after reconcile:\n%s", want, st)
		}
	}
	if strings.Count(st, `"vlan": "100"`) != 2 || strings.Count(st, `"vlan": "101"`) != 1 {
		t.Fatalf("[#24-6] app-net's children must carry vlan 100 and db-net vlan 101:\n%s", st)
	}

	out2, code := runSakutei(t, "reconcile", "--intent", intent, "--state", state, "--alloc-store", store)
	if code != 0 || !strings.Contains(out2, "Converged (no changes)") {
		t.Fatalf("[#24-6] re-running reconcile must be a no-op (same allocations):\n%s", out2)
	}
	intentRev, renderedRev := extractRevisions(out2)
	if intentRev == "" || intentRev == renderedRev {
		t.Fatalf("[#24-6] rendered_revision must include the allocations (differ from intent_revision): %q %q", intentRev, renderedRev)
	}
	_, renderedRev3 := extractRevisions(mustRun(t, "plan", "--intent", intent, "--state", state, "--alloc-store", store))
	if renderedRev3 != renderedRev {
		t.Fatalf("[#24-6] plan and reconcile must compute the same rendered_revision: %q vs %q", renderedRev3, renderedRev)
	}
	if out, code := runSakutei(t, "status", "--intent", intent, "--state", state, "--alloc-store", store); code != 0 || strings.Contains(out, "pending allocation") {
		t.Fatalf("[#24-6] status must see the allocations (exit %d):\n%s", code, out)
	}
}

func mustRun(t *testing.T, args ...string) string {
	t.Helper()
	out, code := runSakutei(t, args...)
	if code != 0 {
		t.Fatalf("sakutei %v failed (exit %d):\n%s", args, code, out)
	}
	return out
}

func TestAllocation_OwnerSurvivesScopeChange(t *testing.T) {
	// [#24-7]
	ctx := context.Background()
	a, _ := newAuthority(t, 1, 100)
	req := func(scope map[string]string) string {
		r := diff.DesiredResource{
			ID:          core.NewID("fake", core.NewScope(scope), "vm-1"),
			Fields:      map[string]string{"name": "vm-1"},
			Allocations: map[string]string{"vlan": "p"},
		}
		res, err := render.ResolveAllocations(ctx, a, render.AllocAllocate, []diff.DesiredResource{r})
		if err != nil {
			t.Fatalf("ResolveAllocations: %v", err)
		}
		return res.Resources[0].Fields["vlan"]
	}
	before := req(map[string]string{"site": "tokyo"})
	after := req(map[string]string{"site": "osaka"})
	if before == "" || before != after {
		t.Fatalf("[#24-7] moving the resource to another scope must keep its allocation: %q → %q", before, after)
	}
}

func TestAllocation_Errors(t *testing.T) {
	// [#24-8]
	intent, state, _ := setupAllocCLI(t)
	if out, code := runSakutei(t, "reconcile", "--intent", intent, "--state", state); code == 0 || !strings.Contains(out, "--alloc-store") {
		t.Fatalf("[#24-8] from_pool without --alloc-store must fail clearly (exit %d):\n%s", code, out)
	}
	if strings.Contains(readFile(t, state), "db-net") {
		t.Fatalf("[#24-8] nothing may be applied when allocation fails")
	}

	bad := strings.Replace(allocIntent, "name: db-net\n      vlan:\n        from_pool: tenant-vlans", "name: db-net\n      vlan:\n        from_pool: no-such-pool", 1)
	writeStateJSON(t, filepath.Join(intent, "net.yaml"), bad)
	if out, code := runSakutei(t, "plan", "--intent", intent, "--state", state, "--alloc-store", filepath.Join(t.TempDir(), "a.json")); code == 0 || !strings.Contains(out, "no-such-pool") {
		t.Fatalf("[#24-8] a request for an undeclared pool must be rejected at load (exit %d):\n%s", code, out)
	}
}

func TestAllocation_Daemon(t *testing.T) {
	// [#24-9]
	intent, state, store := setupAllocCLI(t)
	cmd, stdout, stderr := startDaemonWithArgs(t, intent, state, "100ms", "--alloc-store", store)
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if !waitForCondition(10*time.Second, func() bool {
		b, err := os.ReadFile(state)
		return err == nil && strings.Count(string(b), `"vlan": "100"`) == 2 && strings.Contains(string(b), `"vlan": "101"`)
	}) {
		t.Fatalf("[#24-9] the daemon must allocate and converge:\nstdout:\n%s\nstderr:\n%s\nstate:\n%s", stdout.String(), stderr.String(), readFile(t, state))
	}
}
