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

// allocator_release_cli_test.go — Backlog #116 (#108 の 2): the thin admin
// CLI of the allocator, `sakutei allocator release / list` (§19.3, §26.6).
//
// Tags:
//   [AC-114-2-1] file store: allocate → release → the value cools down and is
//                not reused → after the cooldown it is reused; --audit-log
//                records the release in S4
//   [AC-114-2-2] service (--url): the same through POST /v1/release
//   [AC-114-2-3] releasing an owner with no record exits 1 with a clear message
//   [AC-114-2-4] list shows pool, owner, value and state (allocated, reserved,
//                cooldown with the time left)

package acceptance

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/allocator"
	"github.com/tjst-t/sakutei/v2/pkg/allocation"
)

// endCooldowns moves every cooldown of the store file into the past: the
// stand-in for waiting out the 24h cooldown.
func endCooldowns(t *testing.T, store string) {
	t.Helper()
	data, err := os.ReadFile(store)
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	for _, p := range s["pools"].(map[string]any) {
		cd, _ := p.(map[string]any)["cooldown"].(map[string]any)
		for v := range cd {
			cd[v] = past
		}
	}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// releaseCycle runs the allocate → release → cooldown → reuse scenario on
// auth, releasing through the CLI with target (--store <f> or --url <u>).
func releaseCycle(t *testing.T, tag string, auth allocation.AllocationAuthority, ensure func(string, int, int) error, store string, target ...string) {
	t.Helper()
	ctx := context.Background()
	if err := ensure("vlan", 100, 101); err != nil {
		t.Fatal(err)
	}
	if v, err := auth.Allocate(ctx, "vlan", "vm-a"); err != nil || v != "100" {
		t.Fatalf("[%s] allocate vm-a: %q %v", tag, v, err)
	}
	if v, err := auth.Allocate(ctx, "vlan", "vm-b"); err != nil || v != "101" {
		t.Fatalf("[%s] allocate vm-b: %q %v", tag, v, err)
	}

	audit := filepath.Join(t.TempDir(), "audit.jsonl")
	args := append([]string{"allocator", "release", "--pool", "vlan", "--owner", "vm-a", "--audit-log", audit}, target...)
	out, code := runSakutei(t, args...)
	if code != 0 || !strings.Contains(out, "released value 100 of pool vlan from owner vm-a") || !strings.Contains(out, "cooling down until") {
		t.Fatalf("[%s] release: exit %d, output:\n%s", tag, code, out)
	}
	if _, ok, err := auth.Lookup(ctx, "vlan", "vm-a"); err != nil || ok {
		t.Fatalf("[%s] vm-a must hold nothing after release: ok=%v err=%v", tag, ok, err)
	}
	rec := readFile(t, audit)
	if !strings.Contains(rec, `"kind":"allocation.released"`) || !strings.Contains(rec, "owner vm-a value 100") {
		t.Fatalf("[%s] the release must be recorded in the audit log:\n%s", tag, rec)
	}

	// Cooling down: 100 is not handed out again; with 101 in use the pool is
	// exhausted.
	if _, err := auth.Allocate(ctx, "vlan", "vm-c"); err == nil || !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("[%s] the released value must not be reused during the cooldown, got err=%v", tag, err)
	}

	endCooldowns(t, store)
	if v, err := auth.Allocate(ctx, "vlan", "vm-c"); err != nil || v != "100" {
		t.Fatalf("[%s] after the cooldown the value must be reused: %q %v", tag, v, err)
	}
}

func TestAllocatorReleaseCLI_FileStore(t *testing.T) {
	// [AC-114-2-1]
	store := filepath.Join(t.TempDir(), "alloc.json")
	auth := allocator.New(store)
	releaseCycle(t, "AC-114-2-1", auth, auth.EnsurePool, store, "--store", store)
}

func TestAllocatorReleaseCLI_Service(t *testing.T) {
	// [AC-114-2-2]
	store := filepath.Join(t.TempDir(), "alloc.json")
	p := startAllocator(t, store, "127.0.0.1:0")
	c := allocator.NewClient([]string{p.url})
	releaseCycle(t, "AC-114-2-2", c, c.EnsurePool, store, "--url", p.url)

	// The served store is fenced against a direct write: --store says so.
	out, code := runSakutei(t, "allocator", "release", "--pool", "vlan", "--owner", "vm-b", "--store", store)
	if code != 1 || !strings.Contains(out, "--url") {
		t.Fatalf("[AC-114-2-2] a direct release on a served store must be refused and point at --url: exit %d\n%s", code, out)
	}
}

func TestAllocatorReleaseCLI_NoRecordExits1(t *testing.T) {
	// [AC-114-2-3]
	store := filepath.Join(t.TempDir(), "alloc.json")
	auth := allocator.New(store)
	if err := auth.EnsurePool("vlan", 100, 199); err != nil {
		t.Fatal(err)
	}
	audit := filepath.Join(t.TempDir(), "audit.jsonl")
	out, code := runSakutei(t, "allocator", "release", "--pool", "vlan", "--owner", "ghost", "--store", store, "--audit-log", audit)
	if code != 1 || !strings.Contains(out, `pool "vlan" has no allocation for owner "ghost"`) {
		t.Fatalf("[AC-114-2-3] release of an unknown owner: exit %d, output:\n%s", code, out)
	}
	if _, err := os.Stat(audit); !os.IsNotExist(err) {
		t.Fatalf("[AC-114-2-3] nothing released, nothing audited (err=%v)", err)
	}
	out, code = runSakutei(t, "allocator", "release", "--pool", "nope", "--owner", "ghost", "--store", store)
	if code != 1 || !strings.Contains(out, "unknown pool") {
		t.Fatalf("[AC-114-2-3] release in an unknown pool: exit %d, output:\n%s", code, out)
	}

	p := startAllocator(t, filepath.Join(t.TempDir(), "svc.json"), "127.0.0.1:0")
	if err := allocator.NewClient([]string{p.url}).EnsurePool("vlan", 100, 199); err != nil {
		t.Fatal(err)
	}
	out, code = runSakutei(t, "allocator", "release", "--pool", "vlan", "--owner", "ghost", "--url", p.url)
	if code != 1 || !strings.Contains(out, `has no allocation for owner "ghost"`) {
		t.Fatalf("[AC-114-2-3] service release of an unknown owner: exit %d, output:\n%s", code, out)
	}

	// Neither or both of --store / --url is a usage error.
	if _, code := runSakutei(t, "allocator", "release", "--pool", "vlan", "--owner", "x"); code != 2 {
		t.Fatalf("[AC-114-2-3] release without --store/--url must exit 2, got %d", code)
	}
}

func TestAllocatorListCLI(t *testing.T) {
	// [AC-114-2-4]
	ctx := context.Background()
	store := filepath.Join(t.TempDir(), "alloc.json")
	auth := allocator.New(store)
	for _, pl := range []struct {
		name   string
		lo, hi int
	}{{"vlan", 100, 199}, {"vni", 5000, 5999}} {
		if err := auth.EnsurePool(pl.name, pl.lo, pl.hi); err != nil {
			t.Fatal(err)
		}
	}
	mustAlloc := func(pool, owner string) {
		if _, err := auth.Allocate(ctx, pool, owner); err != nil {
			t.Fatal(err)
		}
	}
	mustAlloc("vlan", "vm-a")                                               // 100 → released below
	mustAlloc("vlan", "vm-b")                                               // 101
	mustAlloc("vni", "net-x")                                               // 5000
	if _, err := auth.Reserve(ctx, "vlan", "vm-r", time.Hour); err != nil { // 102
		t.Fatal(err)
	}
	if err := auth.Release(ctx, "vlan", "vm-a"); err != nil {
		t.Fatal(err)
	}

	row := func(out, pool, owner, value, state, left string) bool {
		re := regexp.MustCompile(`(?m)^` + pool + `\s+` + regexp.QuoteMeta(owner) + `\s+` + value + `\s+` + state + `\s+` + left + `$`)
		return re.MatchString(out)
	}
	check := func(tag string, target ...string) {
		out, code := runSakutei(t, append([]string{"allocator", "list"}, target...)...)
		if code != 0 || !strings.Contains(out, "POOL") || !strings.Contains(out, "LEFT") {
			t.Fatalf("[%s] list: exit %d\n%s", tag, code, out)
		}
		for _, want := range [][5]string{
			{"vlan", "-", "100", "cooldown", `2[34]h\d+m\d+s`},
			{"vlan", "vm-b", "101", "allocated", "-"},
			{"vlan", "vm-r", "102", "reserved", `(59m\d+s|1h0m0s)`},
			{"vni", "net-x", "5000", "allocated", "-"},
		} {
			if !row(out, want[0], want[1], want[2], want[3], want[4]) {
				t.Fatalf("[%s] list must show %v:\n%s", tag, want, out)
			}
		}
		out, code = runSakutei(t, append([]string{"allocator", "list", "--pool", "vni"}, target...)...)
		if code != 0 || strings.Contains(out, "vlan") || !row(out, "vni", "net-x", "5000", "allocated", "-") {
			t.Fatalf("[%s] list --pool vni: exit %d\n%s", tag, code, out)
		}
		out, code = runSakutei(t, append([]string{"allocator", "list", "--pool", "nope"}, target...)...)
		if code != 1 || !strings.Contains(out, "unknown pool") {
			t.Fatalf("[%s] list of an unknown pool: exit %d\n%s", tag, code, out)
		}
	}
	check("AC-114-2-4", "--store", store)

	// The service answers the same list (read only).
	p := startAllocator(t, store, "127.0.0.1:0")
	check("AC-114-2-4", "--url", p.url)

	empty := filepath.Join(t.TempDir(), "empty.json")
	if out, code := runSakutei(t, "allocator", "list", "--store", empty); code != 0 || !strings.Contains(out, "no allocation") {
		t.Fatalf("[AC-114-2-4] list of an empty store: exit %d\n%s", code, out)
	}
}
