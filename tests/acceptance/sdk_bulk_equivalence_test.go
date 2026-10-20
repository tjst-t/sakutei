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

// sdk_bulk_equivalence_test.go — Backlog #21 (v1b): the SDK BulkObserve
// equivalence harness, BulkObserve(scope) ≡ {Read(id) | id ∈ scope} (§5.7,
// §28.3, ADR-0011).
//
// Tags:
//   [#21-1] the fake bulk provider passes the harness (ids from List, and ids
//           given explicitly; unknown entries compare equal too)
//   [#21-2] each kind of disagreement is reported: missing entry, State
//           mismatch, Availability mismatch, zero ObservedAt, invented id
//   [#21-3] declaration/implementation mismatch is reported in both directions
//   [#21-4] the fake bulk call is complete-or-error (injected observe error
//           fails the whole call instead of dropping the resource)

package acceptance

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/fakeprovider"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
	"github.com/tjst-t/sakutei/pkg/sdk"
)

// recordTB collects what a harness reports instead of failing the real test,
// so a test can assert that a broken provider IS reported.
type recordTB struct {
	errors []string
	fatals []string
	logs   []string
}

func (r *recordTB) Helper() {}
func (r *recordTB) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}
func (r *recordTB) Fatalf(format string, args ...any) {
	r.fatals = append(r.fatals, fmt.Sprintf(format, args...))
}
func (r *recordTB) Logf(format string, args ...any) {
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}

func (r *recordTB) all() string {
	return strings.Join(append(append([]string{}, r.fatals...), r.errors...), "\n")
}

// newSeededBulkFake returns a fake bulk provider with vm-1..vm-3 in reality.
func newSeededBulkFake(t *testing.T) *fakeprovider.BulkProvider {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "state.json")
	writeStateJSON(t, statePath, `{
  "resources": {
    "vm-1": {"fields": {"name": "vm-1", "size": 1}},
    "vm-2": {"fields": {"name": "vm-2", "size": 2}},
    "vm-3": {"fields": {"name": "vm-3", "size": 3}}
  },
  "provider_version": "v-test"
}`)
	return fakeprovider.NewBulk(statePath)
}

func writeStateJSON(t *testing.T, path, content string) {
	t.Helper()
	if err := writeFileAtomic(path, []byte(content), 0600); err != nil {
		t.Fatalf("write state: %v", err)
	}
}

func rootID(local string) core.ID {
	return core.NewID(fakeprovider.ProviderName, core.NewScope(nil), local)
}

// mutatedBulk wraps the fake bulk provider and lets a test corrupt the bulk
// result — the bug classes the harness must catch.
type mutatedBulk struct {
	*fakeprovider.BulkProvider
	mutate func(map[core.IDKey]core.Observation)
}

func (m *mutatedBulk) BulkObserve(ctx context.Context, scope core.Scope) (provider.BulkObservation, error) {
	b, err := m.BulkProvider.BulkObserve(ctx, scope)
	if err == nil {
		m.mutate(b.Observations)
	}
	return b, err
}

func TestBulkEquivalence_FakeBulkProviderPasses(t *testing.T) {
	// [#21-1]
	p := newSeededBulkFake(t)
	if err := provider.ValidateProvider(p); err != nil {
		t.Fatalf("[#21-1] fake bulk provider must validate: %v", err)
	}

	// Targets from List.
	bt := sdk.BulkEquivalenceTester{Provider: p, Lister: p, Scope: core.NewScope(nil)}
	bt.Run(t)

	// Targets given explicitly (a subset of the scope).  Extra resources in the
	// bulk result are allowed here: only List-derived targets define the whole
	// scope.
	bt2 := sdk.BulkEquivalenceTester{Provider: p, Scope: core.NewScope(nil), IDs: []core.ID{rootID("vm-1"), rootID("vm-3")}}
	bt2.Run(t)
}

func TestBulkEquivalence_UnknownEntriesCompareEqual(t *testing.T) {
	// [#21-1] an unreachable resource is Unknown on both paths.
	statePath := filepath.Join(t.TempDir(), "state.json")
	writeStateJSON(t, statePath, `{
  "resources": {
    "vm-1": {"fields": {"name": "vm-1", "size": 1}},
    "vm-2": {"fields": {"name": "vm-2", "size": 2}}
  },
  "observe_unknown": {"vm-2": "unreachable"},
  "provider_version": "v-test"
}`)
	p := fakeprovider.NewBulk(statePath)
	bt := sdk.BulkEquivalenceTester{Provider: p, Lister: p, Scope: core.NewScope(nil)}
	bt.Run(t)
}

func TestBulkEquivalence_ReportsDisagreements(t *testing.T) {
	// [#21-2]
	cases := []struct {
		name   string
		mutate func(map[core.IDKey]core.Observation)
		want   string
	}{
		{
			name:   "missing entry",
			mutate: func(o map[core.IDKey]core.Observation) { delete(o, rootID("vm-2").Key()) },
			want:   "missing from BulkObserve result",
		},
		{
			name: "state mismatch (stale bulk view)",
			mutate: func(o map[core.IDKey]core.Observation) {
				k := rootID("vm-2").Key()
				ob := o[k]
				ob.State = core.State{"name": {Kind: core.FieldKindString, Value: "vm-2"}, "size": {Kind: core.FieldKindInt, Value: 99}}
				o[k] = ob
			},
			want: "State differs",
		},
		{
			name: "availability mismatch",
			mutate: func(o map[core.IDKey]core.Observation) {
				k := rootID("vm-3").Key()
				ob := o[k]
				ob.Availability = core.AvailabilityUnknown
				o[k] = ob
			},
			want: "Availability bulk=",
		},
		{
			name: "zero ObservedAt",
			mutate: func(o map[core.IDKey]core.Observation) {
				k := rootID("vm-1").Key()
				ob := o[k]
				ob.ObservedAt = time.Time{}
				o[k] = ob
			},
			want: "zero ObservedAt",
		},
		{
			name: "invented id",
			mutate: func(o map[core.IDKey]core.Observation) {
				o[rootID("ghost").Key()] = core.Observation{Availability: core.AvailabilityOk, ObservedAt: time.Now()}
			},
			want: "which List",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := newSeededBulkFake(t)
			p := &mutatedBulk{BulkProvider: base, mutate: tc.mutate}
			rec := &recordTB{}
			bt := sdk.BulkEquivalenceTester{Provider: p, Lister: p, Scope: core.NewScope(nil)}
			bt.Run(rec)
			if !strings.Contains(rec.all(), tc.want) {
				t.Fatalf("[#21-2] %s: harness must report %q; got:\n%s", tc.name, tc.want, rec.all())
			}
		})
	}
}

// declaresBulkOnly claims BulkObserve but does not implement BulkObserver.
type declaresBulkOnly struct{ *fakeprovider.Provider }

func (d *declaresBulkOnly) Capabilities() provider.Capabilities {
	c := d.Provider.Capabilities()
	c.BulkObserve = true
	return c
}

// implementsBulkOnly implements BulkObserver but does not declare it.
type implementsBulkOnly struct{ *fakeprovider.BulkProvider }

func (i *implementsBulkOnly) Capabilities() provider.Capabilities {
	c := i.BulkProvider.Capabilities()
	c.BulkObserve = false
	return c
}

func TestBulkEquivalence_ReportsDeclarationMismatch(t *testing.T) {
	// [#21-3]
	base := newSeededBulkFake(t)

	rec := &recordTB{}
	bt := sdk.BulkEquivalenceTester{Provider: &declaresBulkOnly{Provider: base.Provider}, Lister: base, Scope: core.NewScope(nil)}
	bt.Run(rec)
	if !strings.Contains(rec.all(), "does not implement provider.BulkObserver") {
		t.Errorf("[#21-3] declared-but-not-implemented must be reported; got:\n%s", rec.all())
	}

	rec2 := &recordTB{}
	bt2 := sdk.BulkEquivalenceTester{Provider: &implementsBulkOnly{BulkProvider: base}, Lister: base, Scope: core.NewScope(nil)}
	bt2.Run(rec2)
	if !strings.Contains(rec2.all(), "does not declare Capabilities.BulkObserve") {
		t.Errorf("[#21-3] implemented-but-not-declared must be reported; got:\n%s", rec2.all())
	}

	// A plain (non-bulk) provider is not a violation: nothing to compare.
	rec3 := &recordTB{}
	bt3 := sdk.BulkEquivalenceTester{Provider: base.Provider, Lister: base, Scope: core.NewScope(nil)}
	bt3.Run(rec3)
	if rec3.all() != "" {
		t.Errorf("[#21-3] non-bulk provider must not be reported as a violation; got:\n%s", rec3.all())
	}
}

func TestFakeBulkObserve_CompleteOrError(t *testing.T) {
	// [#21-4]
	statePath := filepath.Join(t.TempDir(), "state.json")
	writeStateJSON(t, statePath, `{
  "resources": {
    "vm-1": {"fields": {"name": "vm-1", "size": 1}},
    "vm-2": {"fields": {"name": "vm-2", "size": 2}}
  },
  "observe_unknown": {"vm-2": "error"},
  "provider_version": "v-test"
}`)
	p := fakeprovider.NewBulk(statePath)
	b, err := p.BulkObserve(context.Background(), core.NewScope(nil))
	if err == nil {
		t.Fatalf("[#21-4] an observe error on one resource must fail the whole bulk call; got %d observations", len(b.Observations))
	}
	if len(b.Observations) != 0 {
		t.Errorf("[#21-4] a failed bulk call must not return a partial result; got %d observations", len(b.Observations))
	}
}
