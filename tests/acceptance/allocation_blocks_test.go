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

// allocation_blocks_test.go — Backlog #206: a pool split into blocks, and a
// pool that hands out values upwards only and never reuses one (§19.8).
//
// Tags:
//   [AC-206-1] a request naming a block gets a value of that block alone
//   [AC-206-2] a block that runs out stops its requests with the reason
//              (waiting-allocation: the block, its range); the other
//              resources keep converging
//   [AC-206-3] the intent is refused at load: a request without a block from
//              a pool with blocks, an undeclared block, blocks that overlap
//              or lie outside the pool's range, a pool name holding "/"
//   [AC-206-4] reuse: false: a released value is retired, never handed out
//              again even when the cooldown would have ended
//   [AC-206-5] reuse: false with write_back: the last value is written back
//              (last:); the store is lost, rebuilt by `sakutei allocator
//              import`, and the released value is still not handed out again

package acceptance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/allocator"
	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/pkg/allocation"
)

const blocksIntent = `pools:
  - name: user-ids
    range: "1000-1999"
    blocks:
      - name: staff
        range: "1000-1001"
      - name: guests
        range: "1100-1199"
resources:
  - provider: fake
    id: staff-a
    fields:
      name: staff-a
      uid:
        from_pool: user-ids
        block: staff
  - provider: fake
    id: guest-a
    fields:
      name: guest-a
      uid:
        from_pool: user-ids
        block: guests
  - provider: fake
    id: staff-b
    fields:
      name: staff-b
      uid:
        from_pool: user-ids
        block: staff
`

const blocksExtra = `  - provider: fake
    id: staff-c
    fields:
      name: staff-c
      uid:
        from_pool: user-ids
        block: staff
`

func TestAllocationBlocks_AllocateWithinBlockAndExhaust(t *testing.T) {
	dir := t.TempDir()
	intent := filepath.Join(dir, "intent")
	if err := os.MkdirAll(intent, 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state.json")
	store := filepath.Join(dir, "alloc.json")
	writeStateJSON(t, filepath.Join(intent, "users.yaml"), blocksIntent)
	writeStateJSON(t, state, `{"resources": {}, "provider_version": "v-test"}`)
	run := func(verb string) (string, int) {
		return runSakutei(t, verb, "--intent", intent, "--state", state, "--alloc-store", store)
	}

	// [AC-206-1]
	if out, code := run("reconcile"); code != 0 {
		t.Fatalf("[AC-206-1] reconcile: exit %d\n%s", code, out)
	}
	st := readFile(t, state)
	for _, want := range []string{`"uid": "1000"`, `"uid": "1001"`, `"uid": "1100"`} {
		if !strings.Contains(st, want) {
			t.Fatalf("[AC-206-1] staff takes 1000 / 1001 and guests 1100 (want %s):\n%s", want, st)
		}
	}

	// [AC-206-2] staff (1000-1001) is full: staff-c waits with the reason.
	writeStateJSON(t, filepath.Join(intent, "users.yaml"), blocksIntent+blocksExtra)
	out, _ := run("reconcile")
	for _, want := range []string{"waiting-allocation", "pool exhausted", `block "staff" of pool "user-ids"`, "1000-1001"} {
		if !strings.Contains(out, want) {
			t.Fatalf("[AC-206-2] the exhausted block must stop staff-c with the reason (want %q):\n%s", want, out)
		}
	}
	if strings.Contains(readFile(t, state), `"name": "staff-c"`) {
		t.Fatalf("[AC-206-2] staff-c must not be applied:\n%s", readFile(t, state))
	}
	if strings.Contains(readFile(t, store), `"1101"`) || strings.Contains(readFile(t, store), `"value": 1101`) {
		t.Fatalf("[AC-206-2] staff-c must not spill into another block:\n%s", readFile(t, store))
	}
	out, _ = run("plan")
	if !strings.Contains(out, "user-ids/staff") {
		t.Fatalf("[AC-206-2] plan must name the block of the pending request:\n%s", out)
	}
}

func TestAllocationBlocks_RefusedAtLoad(t *testing.T) {
	// [AC-206-3]
	cases := []struct{ name, intent, want string }{
		{"no block", strings.Replace(blocksIntent, "        block: guests\n", "", 1), "split into blocks: name one"},
		{"unknown block", strings.Replace(blocksIntent, "block: guests", "block: admins", 1), `block "admins", which pool "user-ids" does not declare`},
		{"overlap", strings.Replace(blocksIntent, `range: "1100-1199"`, `range: "1001-1199"`, 1), "overlap"},
		{"outside", strings.Replace(blocksIntent, `range: "1100-1199"`, `range: "1100-2100"`, 1), "outside the pool's range"},
		{"slash", strings.ReplaceAll(blocksIntent, "user-ids", "user/ids"), `holds no "/"`},
	}
	for _, c := range cases {
		dir := t.TempDir()
		writeStateJSON(t, filepath.Join(dir, "users.yaml"), c.intent)
		state := filepath.Join(t.TempDir(), "state.json")
		writeStateJSON(t, state, `{"resources": {}, "provider_version": "v-test"}`)
		out, code := runSakutei(t, "plan", "--intent", dir, "--state", state, "--alloc-store", filepath.Join(t.TempDir(), "a.json"))
		if code == 0 || !strings.Contains(out, c.want) {
			t.Fatalf("[AC-206-3] %s: the intent must be refused with %q: exit %d\n%s", c.name, c.want, code, out)
		}
	}
}

func TestAllocationNoReuse_ReleasedValueIsRetired(t *testing.T) {
	// [AC-206-4] the cooldown is 1ns and the clock moves on: a reusing pool
	// would hand 100 out again, a reuse: false pool never does.
	ctx := context.Background()
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	for _, noReuse := range []bool{false, true} {
		a := allocator.New(filepath.Join(t.TempDir(), "alloc.json"))
		a.Cooldown = time.Nanosecond
		a.Now = func() time.Time { return now }
		if err := a.DeclarePool("p", allocation.PoolSpec{Lo: 100, Hi: 102, NoReuse: noReuse}); err != nil {
			t.Fatal(err)
		}
		if v, err := a.Allocate(ctx, "p", "a"); err != nil || v != "100" {
			t.Fatalf("[AC-206-4] first value: %q %v", v, err)
		}
		if err := a.Release(ctx, "p", "a"); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Hour)
		v, err := a.Allocate(ctx, "p", "b")
		if err != nil {
			t.Fatal(err)
		}
		if !noReuse {
			if v != "100" {
				t.Fatalf("[AC-206-4] a reusing pool hands 100 out again after the cooldown, got %s", v)
			}
			continue
		}
		if v != "101" {
			t.Fatalf("[AC-206-4] reuse: false must not hand the released 100 out again, got %s", v)
		}
		recs, _ := a.List(ctx, "p")
		var states []string
		for _, r := range recs {
			states = append(states, r.State)
		}
		if got := strings.Join(states, ","); got != "retired,allocated,last" {
			t.Fatalf("[AC-206-4] the released value is retired (no cooldown) and the last value recorded, got %s", got)
		}
		// 102 is the last value; once 101 is released too nothing is left.
		if v, err := a.Allocate(ctx, "p", "c"); err != nil || v != "102" {
			t.Fatalf("[AC-206-4] %q %v", v, err)
		}
		_ = a.Release(ctx, "p", "b")
		if _, err := a.Allocate(ctx, "p", "d"); err == nil || !strings.Contains(err.Error(), "reuse: false") {
			t.Fatalf("[AC-206-4] the pool is exhausted with the reason (released values are not reused): %v", err)
		}
	}
}

const noReuseIntent = `pools:
  - name: vlans
    range: "100-199"
    write_back: true
    reuse: false
resources:
  - provider: fake
    id: net-a
    fields:
      name: net-a
      vlan:
        from_pool: vlans
  - provider: fake
    id: net-b
    fields:
      name: net-b
      vlan:
        from_pool: vlans
`

const noReuseNetC = `  - provider: fake
    id: net-c
    fields:
      name: net-c
      vlan:
        from_pool: vlans
`

func TestAllocationNoReuse_WriteBackSurvivesStoreLoss(t *testing.T) {
	// [AC-206-5]
	r := newWBRig(t, true)
	netYAML := filepath.Join(r.intent, "net.yaml")
	writeStateJSON(t, netYAML, noReuseIntent)
	r.git(t, "commit", "-q", "-am", "reuse: false")
	if out, code := r.run(t, "reconcile"); code != 0 {
		t.Fatalf("[AC-206-5] reconcile: exit %d\n%s", code, out)
	}
	readLast := func() map[string]int {
		t.Helper()
		f, _, err := loader.ReadAllocationFile(r.file, "vlans")
		if err != nil {
			t.Fatal(err)
		}
		return f.Last
	}
	if got := r.readAlloc(t); got["fake/net-a#vlan"] != 100 || got["fake/net-b#vlan"] != 101 {
		t.Fatalf("[AC-206-5] %v", got)
	}
	if l := readLast(); l[""] != 101 {
		t.Fatalf("[AC-206-5] the file must hold the last value 101: %v\n%s", l, readFile(t, r.file))
	}

	// net-b goes; its value 101 is released (the highest one handed out).
	writeStateJSON(t, netYAML, strings.SplitN(noReuseIntent, "  - provider: fake\n    id: net-b", 2)[0])
	r.git(t, "commit", "-q", "-am", "drop net-b")
	out, code := runSakutei(t, "allocator", "release", "--pool", "vlans", "--owner", "fake/net-b#vlan", "--store", r.store, "--intent", r.intent)
	if code != 0 || !strings.Contains(out, "released value 101") || !strings.Contains(out, "retired") {
		t.Fatalf("[AC-206-5] release: exit %d\n%s", code, out)
	}
	if got := r.readAlloc(t); len(got) != 1 {
		t.Fatalf("[AC-206-5] net-b leaves the file: %v", got)
	}
	if l := readLast(); l[""] != 101 {
		t.Fatalf("[AC-206-5] the release must leave the last value 101 in the file: %v", l)
	}
	if strings.Contains(readFile(t, r.store), `"cooldown": {
        "101"`) {
		t.Fatalf("[AC-206-5] reuse: false keeps no cooldown:\n%s", readFile(t, r.store))
	}

	// Lose the store; reconcile holds and points at import.
	if err := os.Remove(r.store); err != nil {
		t.Fatal(err)
	}
	writeStateJSON(t, netYAML, strings.SplitN(noReuseIntent, "  - provider: fake\n    id: net-b", 2)[0]+noReuseNetC)
	r.git(t, "commit", "-q", "-am", "add net-c")
	out, _ = r.run(t, "reconcile")
	if !strings.Contains(out, "sakutei allocator import") || strings.Contains(readFile(t, r.state), `"name": "net-c"`) {
		t.Fatalf("[AC-206-5] with the store lost no new value is handed out until import:\n%s", out)
	}
	out, code = runSakutei(t, "allocator", "import", "--intent", r.intent, "--store", r.store)
	if code != 0 || !strings.Contains(out, "pool vlans: imported 1 allocation(s)") {
		t.Fatalf("[AC-206-5] import: exit %d\n%s", code, out)
	}
	if out, code := r.run(t, "reconcile"); code != 0 {
		t.Fatalf("[AC-206-5] reconcile after import: exit %d\n%s", code, out)
	}
	if got := r.readAlloc(t); got["fake/net-c#vlan"] != 102 || got["fake/net-a#vlan"] != 100 {
		t.Fatalf("[AC-206-5] the rebuilt store must hand out 102 — never the released 101: %v", got)
	}
	if l := readLast(); l[""] != 102 {
		t.Fatalf("[AC-206-5] the last value moves up to 102: %v", l)
	}
	if st := readFile(t, r.state); !strings.Contains(st, `"name": "net-c",
        "vlan": "102"`) {
		t.Fatalf("[AC-206-5] net-c is applied with 102:\n%s", st)
	}

	// A store that lost only its last value (the file holds every owner) is
	// held too: it would hand out a released value again.
	writeStateJSON(t, r.store, `{"version": 1, "pools": {"vlans": {"lo": 100, "hi": 199, "no_reuse": true,
  "allocations": {"fake/net-a#vlan": {"value": 100, "state": "allocated"}, "fake/net-c#vlan": {"value": 102, "state": "allocated"}},
  "cooldown": {}, "last": {"": 100}}}}`)
	writeStateJSON(t, netYAML, strings.SplitN(noReuseIntent, "  - provider: fake\n    id: net-b", 2)[0]+noReuseNetC+strings.ReplaceAll(noReuseNetC, "net-c", "net-d"))
	out, _ = r.run(t, "reconcile")
	if !strings.Contains(out, "last value is below") || strings.Contains(readFile(t, r.state), `"name": "net-d"`) {
		t.Fatalf("[AC-206-5] a store whose last value is behind the file's must hold new values:\n%s", out)
	}
	if out, code := runSakutei(t, "allocator", "import", "--intent", r.intent, "--store", r.store); code != 0 {
		t.Fatalf("[AC-206-5] import: exit %d\n%s", code, out)
	}
	if out, code := r.run(t, "reconcile"); code != 0 {
		t.Fatalf("[AC-206-5] reconcile: exit %d\n%s", code, out)
	}
	if got := r.readAlloc(t); got["fake/net-d#vlan"] != 103 {
		t.Fatalf("[AC-206-5] net-d gets 103 after import raised the last value: %v", got)
	}
}
