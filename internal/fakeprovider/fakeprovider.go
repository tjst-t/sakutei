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

// Package fakeprovider provides an in-memory, file-backed fake implementation
// of provider.Provider and provider.Lister for use in CLI acceptance tests.
//
// The fake persists "reality" (the current actual state of resources) as a
// JSON file so that tests launched as subprocesses can seed and inspect state
// without sharing process memory.
//
// Design contract:
//   - Read (Observe) reads from the state file; it never touches desired state.
//   - Apply writes to the state file (used only by reconcile—never by plan).
//   - List returns all local IDs stored in the state file for the given scope.
//   - An empty or absent state file means reality is empty (no resources exist).
package fakeprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// ProviderName is the canonical name used in intent files and IDs.
const ProviderName = "fake"

// storedResource holds the raw JSON field values for a resource in the state
// file.  Fields are stored as map[string]any to keep the file format simple
// and human-readable for tests.
type storedResource struct {
	Fields map[string]any `json:"fields"`
}

// stateFile is the on-disk representation of the fake provider's reality.
// The map key is the resource Local identifier (provider-local, Scope-unqualified);
// Sprint 1 resources share an empty Scope. When multi-Scope support lands, extend
// the key to "<scope>/<local>" together with the code that reads/writes it.
//
// FailInject is a set of Local IDs for which Apply should return an error.
// This is a test-only mechanism; production providers do not use this field.
// Tests write this field before launching the binary to simulate Apply failures.
//
// ObserveUnknown is a set of Local IDs for which Read (Observe) should behave as
// if the target is unreachable.  The value selects the injected behaviour:
//   - "error"   → Read returns a transient error (Observe failed).
//   - anything else (e.g. "unknown", "") → Read returns AvailabilityUnknown with
//     no error (target unreachable but the call itself succeeded).
//
// Both cases are "unobservable upstream" for D-1: the reconciler must treat the
// resource as failed/unknown (NOT converged) and skip its transitive downstream
// this cycle (unknown ≠ absent; do not proceed downstream on uncertainty).
// This is a test-only mechanism; production providers do not use this field.
//
// ApplyDelay is a map from Local ID to a duration string (e.g. "500ms").
// When set, Apply sleeps for the specified duration before writing the resource.
// This is a test-only mechanism for S94f1f9-3 to simulate long-running Applies.
//
// ApplyCount is a map from Local ID to the number of times Apply has been called
// for that resource.  Incremented atomically on each Apply call.  Used by tests
// to verify that a resource is not double-applied while in-flight.
// This is a test-only mechanism; production providers do not use this field.
//
// ListPartial is a boolean that, when true, causes List to return ListPartial
// status instead of ListComplete.  Used by Scf7762-1 tests to simulate an
// incomplete List result triggering the prune-halt safety guard.
// This is a test-only mechanism; production providers do not use this field.
//
// PruneableCapability is a boolean that controls what Capabilities().Pruneable
// reports for the fake provider TYPE.  Default false (matching ADR-0005's
// "destructive types default to pruneable:false").  Tests set it to true (via
// SetPruneable) to opt the whole fake type in to automatic deletion so the
// normal prune path can be exercised.  This is a type-level property, so it is
// a single boolean, not a per-resource map.
// This is a test-only mechanism; production providers do not use this field.
//
// ProviderVersion is a string that represents the fake provider's
// diff/normalisation source hash (see §10.4, ADR-0008).  In production, the
// SDK computes this automatically from the provider source.  For the fake, tests
// write this field to simulate a provider upgrade (cause-tag C).  Default ""
// (empty) means no version is embedded; the Scb0ecd-3 fail-closed guard is not
// yet enforced, so empty is accepted here as "unknown/stable".
// This is a test-only mechanism; production providers do not use this field.
// Scopes is a test-only map from Local ID to the scope label set that List
// should attach to that resource's ID.  The fake keys everything by Local for
// simplicity, so by default List returns empty-scope IDs (all resources share
// the root scope).  When a test needs List to report reality WITH scopes — e.g.
// to exercise the --claim prune-boundary filter (FIX #6) — it seeds this map so
// List can hand back properly-scoped IDs and the claim filter has something to
// match against.  Absent (nil) → every listed ID carries the empty root scope,
// preserving the historical behaviour of every existing test.
// This is a test-only mechanism; production providers do not use this field.
//
// Ready is a test-only map from Local ID to its named ready conditions
// (§8.3, §11.2).  Read returns it as Observation.Ready for a resource that
// exists, so tests can flip a condition (e.g. cloud_init_done) between cycles
// to simulate a long-running bring-up that finishes several cycles later.
// This is a test-only mechanism; production providers do not use this field.
type stateFile struct {
	Resources           map[string]storedResource    `json:"resources"`
	FailInject          map[string]string            `json:"fail_inject,omitempty"`
	ObserveUnknown      map[string]string            `json:"observe_unknown,omitempty"`
	ApplyDelay          map[string]string            `json:"apply_delay,omitempty"`
	ApplyCount          map[string]int               `json:"apply_count,omitempty"`
	Scopes              map[string]map[string]string `json:"scopes,omitempty"`
	ListPartial         bool                         `json:"list_partial,omitempty"`
	PruneableCapability bool                         `json:"pruneable_capability,omitempty"`
	ProviderVersion     string                       `json:"provider_version,omitempty"`
	Ready               map[string]map[string]bool   `json:"ready,omitempty"`
	// Task runner fields (test-only, #41, §6.2): see task.go.
	TaskRunCount      map[string]int    `json:"task_run_count,omitempty"`
	TaskRollbackCount map[string]int    `json:"task_rollback_count,omitempty"`
	TaskFailInject    map[string]string `json:"task_fail_inject,omitempty"`
	TaskSkipMark      map[string]bool   `json:"task_skip_mark,omitempty"`
}

// Provider is the fake implementation.  It satisfies both provider.Provider
// and provider.Lister.
//
// Thread-safety: The Provider is safe for concurrent use.  Apply, Read, and
// List all serialise their state-file access via mu, so no two goroutines touch
// the file concurrently.  This matters for the daemon (S94f1f9-3): an async
// Apply goroutine launched by a prior cycle may still be in flight when the next
// cycle calls Read/List, so Read and Apply can run concurrently in-process.
// Holding mu around every file access prevents that file-level race.
//
// The Apply delay (ApplyDelay sleep) is taken OUTSIDE mu so a slow Apply for one
// resource does not block Read/List or Applies for other resources.
type Provider struct {
	// statePath is the path to the JSON state file.
	statePath string
	// mu serialises all state-file access (Apply read-modify-write, Read, List)
	// so concurrent in-process goroutines never race on the file.
	mu sync.Mutex

	// readMu guards readCount, the in-memory per-resource Observe counter
	// (test-only, Sf6a62f-2).  Kept separate from the state file so Read never
	// mutates observable reality (see Read).  Not persisted; visible only to the
	// in-process test that holds this Provider value.
	readMu    sync.Mutex
	readCount map[string]int

	// holdMu guards taskHolds, the in-memory gates that keep RunTask blocked
	// until a test releases them (test-only, #41; see HoldTask).
	holdMu    sync.Mutex
	taskHolds map[string]chan struct{}
}

// New creates a new fake Provider backed by the JSON file at statePath.
// If the file does not exist, it is treated as an empty state (no resources).
func New(statePath string) *Provider {
	return &Provider{statePath: statePath}
}

// Schema returns a simple two-field schema suitable for tests.
// Fields: "name" (String, identity) and "size" (Int).
func (p *Provider) Schema() provider.Schema {
	return provider.Schema{
		Fields: map[string]provider.FieldSpec{
			"name": {
				Kind:     core.FieldKindString,
				Required: true,
				Identity: true,
			},
			"size": {
				Kind:     core.FieldKindInt,
				Required: false,
			},
		},
	}
}

// Capabilities declares the fake's capabilities for testing.
//
// TrueObserve and List are always true (the fake fully observes and enumerates
// its in-memory reality).  Pruneable is TYPE-level and defaults to false
// (ADR-0005): the fake reports its type as non-pruneable unless a test opts in
// via SetPruneable, which flips the pruneable_capability flag in the state
// file.  Reading the flag from the state file (rather than a struct field) lets
// a subprocess-launched daemon observe the setting written by the parent test.
//
// If the state file cannot be read, Capabilities falls back to the safe default
// (Pruneable:false): an unreadable state is never a licence to delete.
func (p *Provider) Capabilities() provider.Capabilities {
	p.mu.Lock()
	sf, err := p.load()
	p.mu.Unlock()
	pruneable := false
	providerVersion := ""
	if err == nil {
		pruneable = sf.PruneableCapability
		providerVersion = sf.ProviderVersion
	}
	return provider.Capabilities{
		TrueObserve:     true,
		List:            true,
		Pruneable:       pruneable,
		ProviderVersion: providerVersion,
	}
}

// Read observes the current state of the resource identified by id from the
// state file.  If the resource is not present, it returns AvailabilityOk
// with an empty State (representing the absent case for the fake).
//
// For the fake, "absent" is represented as AvailabilityOk with nil State so
// that the diff engine can distinguish it from AvailabilityUnknown (target
// unreachable).  The diff engine checks len(obs.State) == 0 to detect absent.
func (p *Provider) Read(_ context.Context, id string) (core.Observation, error) {
	// Hold mu for the file read so an in-flight Apply goroutine (from a prior
	// cycle) cannot race us on the state file.  The load result is a private
	// copy, so we can unlock immediately and compute the Observation lock-free.
	// Record the Observe (test-only, Sf6a62f-2): increment the per-resource read
	// count so tests can assert which resources were freshly Observed.  The count
	// is kept in an IN-MEMORY field, NOT the state file, so Read remains faithful
	// to its design contract ("Read never touches state"): a read must not mutate
	// the observable reality, or freeze/alert-only invariants that assert the
	// state file is byte-identical before/after would break.  In-process tests
	// (the scoped reconcile path) share this counter directly.
	p.readMu.Lock()
	if p.readCount == nil {
		p.readCount = make(map[string]int)
	}
	p.readCount[id]++
	p.readMu.Unlock()

	p.mu.Lock()
	sf, err := p.load()
	p.mu.Unlock()
	if err != nil {
		return core.Observation{
			Availability: core.AvailabilityUnknown,
			ObservedAt:   time.Now(),
		}, provider.NewTransientError(fmt.Errorf("fakeprovider: load state: %w", err))
	}

	key := id

	// Observe-failure injection (test-only, D-1): if this resource is listed in
	// ObserveUnknown, simulate an unreachable target.  Value "error" injects a
	// Read error; any other value injects AvailabilityUnknown without an error.
	// Reason strings are sanitised to a single line to keep stderr logs intact.
	if sf.ObserveUnknown != nil {
		if mode, ok := sf.ObserveUnknown[key]; ok {
			reason := strings.ReplaceAll(mode, "\n", "\\n")
			if reason == "error" {
				return core.Observation{
						Availability: core.AvailabilityUnknown,
						ObservedAt:   time.Now(),
					}, provider.NewTransientError(
						fmt.Errorf("fakeprovider: injected observe error for %s", key))
			}
			return core.Observation{
				Availability: core.AvailabilityUnknown,
				ObservedAt:   time.Now(),
			}, nil
		}
	}

	res, ok := sf.Resources[key]
	if !ok {
		// Resource does not exist in reality.  Return Ok with nil State so
		// the diff engine detects absent (not Unknown/unreachable).
		return core.Observation{
			Availability: core.AvailabilityOk,
			State:        nil,
			ObservedAt:   time.Now(),
		}, nil
	}

	st := make(core.State, len(res.Fields))
	for field, val := range res.Fields {
		st[field] = anyToTypedValue(val)
	}

	var ready map[string]bool
	if conds, ok := sf.Ready[key]; ok {
		ready = make(map[string]bool, len(conds))
		for name, v := range conds {
			ready[name] = v
		}
	}

	return core.Observation{
		Availability: core.AvailabilityOk,
		State:        st,
		Ready:        ready,
		ObservedAt:   time.Now(),
	}, nil
}

// Apply writes the desired state from plan into the state file.
// It handles Create, Update, and Delete Ops (Delete removes the resource).
// Note: the diff engine does not yet emit Delete Ops (prune lands in a later
// Sprint), so in Sprint 1 Apply only ever sees Create/Update in practice.
//
// IMPORTANT: plan (sakutei plan) MUST NEVER call Apply.  Apply is the
// reconcile path only (ADR-0002); AC-Sead944-2-2 verifies this at the CLI
// level (state file unchanged after plan), not via Apply's return value.
//
// S94f1f9-2: Apply checks the FailInject map in the state file.  If the
// target Local ID appears in FailInject, Apply returns a synthetic error
// instead of writing the resource.  This is a test-only mechanism to simulate
// Apply failures for acceptance tests ([AC-S94f1f9-2-2]).
//
// S94f1f9-3: Apply checks the ApplyDelay map and sleeps if a delay is configured
// (test-only, for [AC-S94f1f9-3-1]).  Apply also increments ApplyCount for each
// target resource (test-only, for [AC-S94f1f9-3-2]).
// Apply is concurrency-safe: it holds p.mu for the read-modify-write on the
// state file.  The sleep (ApplyDelay) happens outside the lock so concurrent
// reads/applies for other resources are not blocked.
func (p *Provider) Apply(_ context.Context, plan core.Plan) error {
	// Phase 0: resolve per-resource delay, then sleep OUTSIDE the lock so a slow
	// Apply for one resource does not block Read/List or Applies for others.
	// The delay-lookup file read is itself serialised under mu (a concurrent
	// Apply may be writing the file); we release mu before sleeping.
	var sleepDur time.Duration
	if len(plan.Ops) > 0 {
		p.mu.Lock()
		sf0, err := p.load()
		p.mu.Unlock()
		if err == nil && sf0.ApplyDelay != nil {
			key := plan.Ops[0].Target.Local
			if ds, ok := sf0.ApplyDelay[key]; ok {
				if d, parseErr := time.ParseDuration(ds); parseErr == nil {
					sleepDur = d
				}
			}
		}
	}
	if sleepDur > 0 {
		time.Sleep(sleepDur)
	}

	// Phase 1 (inside lock): read-modify-write the state file.
	p.mu.Lock()
	defer p.mu.Unlock()

	sf, err := p.load()
	if err != nil {
		return provider.NewTransientError(fmt.Errorf("fakeprovider: load state: %w", err))
	}

	// S94f1f9-3: increment Apply call count for all ops upfront (even if
	// FailInject subsequently returns an error).  This ensures that tests can
	// observe that Apply was attempted for a resource regardless of the result.
	if sf.ApplyCount == nil {
		sf.ApplyCount = make(map[string]int)
	}
	for _, op := range plan.Ops {
		sf.ApplyCount[op.Target.Local]++
	}

	// Check FailInject before doing any resource modifications.
	// We do this after incrementing ApplyCount so the count is accurate.
	var failKey, failReason string
	for _, op := range plan.Ops {
		key := op.Target.Local
		if sf.FailInject != nil {
			if reason, ok := sf.FailInject[key]; ok {
				failKey = key
				failReason = strings.ReplaceAll(reason, "\n", "\\n")
				break
			}
		}
	}
	if failKey != "" {
		// Save the incremented apply_count before returning the error so the
		// test can observe the attempt count.
		_ = p.save(sf) // best-effort; ignore error (we're already returning one)
		return provider.NewTransientError(fmt.Errorf("fakeprovider: injected failure for %s: %s", failKey, failReason))
	}

	for _, op := range plan.Ops {
		key := op.Target.Local

		switch op.Kind {
		case core.OpKindCreate:
			// Create: resource did not exist; write all desired fields.
			// From is empty for every FieldDiff; To holds the desired value.
			fields := make(map[string]any, len(op.Fields))
			for _, fd := range op.Fields {
				fields[fd.Field] = fd.To
			}
			sf.Resources[key] = storedResource{Fields: fields}
		case core.OpKindUpdate:
			// Update: resource already exists; merge only the changed fields.
			//
			// Bug fix (S94f1f9-1 fold-in): the previous implementation
			// reconstructed the resource from op.Fields alone, which only
			// contains the *changed* fields.  Unchanged fields (e.g. "name"
			// when only "size" drifted) were silently dropped.
			//
			// Correct behaviour: start from the existing stored resource and
			// overlay only the fields that appear in the FieldDiff list.
			existing := sf.Resources[key]
			merged := make(map[string]any, len(existing.Fields)+len(op.Fields))
			// Copy all currently-stored fields first.
			for k, v := range existing.Fields {
				merged[k] = v
			}
			// Overlay only the changed fields with their new (To) values.
			for _, fd := range op.Fields {
				merged[fd.Field] = fd.To
			}
			sf.Resources[key] = storedResource{Fields: merged}
		case core.OpKindDelete:
			delete(sf.Resources, key)
		}
	}

	return p.save(sf)
}

// List returns all resource local IDs in the given scope together with a
// completeness status (core.ListResult).
//
// In normal operation, the fake returns ListComplete (all resources in the
// state file are returned).  When ListPartial is set in the state file (via
// SetListPartial), the fake returns ListPartial to simulate an incomplete List
// result — triggering the prune-halt safety guard in the prune engine (§12.4c,
// Scf7762-1 [AC-Scf7762-1-2]).
func (p *Provider) List(_ context.Context, _ core.Scope) (core.ListResult, error) {
	// Hold mu for the file read (see Read): an in-flight Apply from a prior cycle
	// may be writing the state file concurrently.
	p.mu.Lock()
	sf, err := p.load()
	p.mu.Unlock()
	if err != nil {
		return core.ListResult{}, provider.NewTransientError(fmt.Errorf("fakeprovider: load state: %w", err))
	}

	// By default the fake uses the empty root scope for every resource (Sprint 1
	// simplification, most tests).  When the state file seeds a per-resource scope
	// (sf.Scopes[local]), List attaches THAT scope so callers can exercise the
	// --claim prune-boundary filter (FIX #6): reality is reported with real scopes,
	// and out-of-claim IDs can be recognised and excluded before set-difference.
	ids := make([]core.ID, 0, len(sf.Resources))
	for local := range sf.Resources {
		scope := core.NewScope(nil)
		if labels, ok := sf.Scopes[local]; ok {
			scope = core.NewScope(labels)
		}
		ids = append(ids, core.NewID(ProviderName, scope, local))
	}

	status := core.ListComplete
	if sf.ListPartial {
		status = core.ListPartial
	}

	return core.ListResult{IDs: ids, Status: status}, nil
}

// SetListPartial writes the list_partial flag into the state file.
// When true, subsequent List calls will return ListPartial status to simulate
// an incomplete List (triggering prune-halt safety guard, §12.4c).
// This is a test-only helper for Scf7762-1 ([AC-Scf7762-1-2]).
func (p *Provider) SetListPartial(partial bool) error {
	sf, err := p.load()
	if err != nil {
		return fmt.Errorf("fakeprovider: load state for list partial: %w", err)
	}
	sf.ListPartial = partial
	return p.save(sf)
}

// SetPruneable writes the pruneable_capability flag into the state file.
// When true, subsequent Capabilities() calls report Pruneable:true, opting the
// fake provider TYPE in to automatic deletion by the prune engine (ADR-0005).
// Default (unset) is false — destructive types are not pruneable unless the
// author opts in.  This is a test-only helper for Scf7762-1.
func (p *Provider) SetPruneable(pruneable bool) error {
	sf, err := p.load()
	if err != nil {
		return fmt.Errorf("fakeprovider: load state for pruneable capability: %w", err)
	}
	sf.PruneableCapability = pruneable
	return p.save(sf)
}

// StatePath returns the path of the underlying state file.
func (p *Provider) StatePath() string {
	return p.statePath
}

// ProviderVersion returns the provider_version stored in the state file.
// An empty string means no version is embedded (Scb0ecd-3 will enforce
// fail-closed for empty versions; here we return "" gracefully).
// This is a test-only accessor for Scb0ecd-2 cause-tag C verification.
func (p *Provider) ProviderVersion() string {
	p.mu.Lock()
	sf, err := p.load()
	p.mu.Unlock()
	if err != nil {
		return ""
	}
	return sf.ProviderVersion
}

// SetProviderVersion writes the provider_version into the state file.
// Tests call this to simulate a provider upgrade that triggers cause-tag C
// (ghost diff: the provider's normalisation logic changed, §10.4, ADR-0008).
// This is a test-only helper for Scb0ecd-2 ([AC-Scb0ecd-2-3]).
func (p *Provider) SetProviderVersion(version string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	sf, err := p.load()
	if err != nil {
		return fmt.Errorf("fakeprovider: load state for provider version: %w", err)
	}
	sf.ProviderVersion = version
	return p.save(sf)
}

// SetFailInject writes a set of Local IDs that should fail on Apply into the
// state file.  Pass nil or an empty map to clear all injected failures.
// This is a test-only helper; production code must not call it.
func (p *Provider) SetFailInject(failMap map[string]string) error {
	sf, err := p.load()
	if err != nil {
		return fmt.Errorf("fakeprovider: load state for fail inject: %w", err)
	}
	sf.FailInject = failMap
	return p.save(sf)
}

// SetApplyDelay writes a per-resource apply delay into the state file.
// The delay is a map from Local ID to a duration string (e.g. "500ms").
// Pass nil or an empty map to clear all delays.
// This is a test-only helper for S94f1f9-3 ([AC-S94f1f9-3-1]).
func (p *Provider) SetApplyDelay(delayMap map[string]string) error {
	sf, err := p.load()
	if err != nil {
		return fmt.Errorf("fakeprovider: load state for apply delay: %w", err)
	}
	sf.ApplyDelay = delayMap
	return p.save(sf)
}

// GetApplyCount reads and returns the apply_count map from the state file.
// This is a test-only helper for S94f1f9-3 ([AC-S94f1f9-3-2]).
func (p *Provider) GetApplyCount() (map[string]int, error) {
	sf, err := p.load()
	if err != nil {
		return nil, fmt.Errorf("fakeprovider: load state for apply count: %w", err)
	}
	if sf.ApplyCount == nil {
		return map[string]int{}, nil
	}
	// Return a copy so callers cannot mutate the internal map.
	out := make(map[string]int, len(sf.ApplyCount))
	for k, v := range sf.ApplyCount {
		out[k] = v
	}
	return out, nil
}

// GetReadCount returns a copy of the in-memory per-resource Observe counter.
// This is a test-only helper for Sf6a62f-2 (scoped reconcile Observe scoping):
// tests assert that resources outside the impacted sub-DAG have a read count of
// zero after a scoped run.  The counter is in-memory (not the state file), so it
// is only meaningful within the process that owns this Provider value — which is
// exactly the in-process scoped-path tests.  It returns nil error to keep a
// uniform signature with the other Get* accessors.
func (p *Provider) GetReadCount() (map[string]int, error) {
	p.readMu.Lock()
	defer p.readMu.Unlock()
	out := make(map[string]int, len(p.readCount))
	for k, v := range p.readCount {
		out[k] = v
	}
	return out, nil
}

// SeedResource writes a resource directly into the state file.  This is a
// test-helper that simulates "reality already has this resource" without going
// through Apply, so that acceptance tests can seed initial state.
func (p *Provider) SeedResource(local string, fields map[string]any) error {
	sf, err := p.load()
	if err != nil {
		return fmt.Errorf("fakeprovider: load state for seed: %w", err)
	}
	sf.Resources[local] = storedResource{Fields: fields}
	return p.save(sf)
}

// load reads and parses the state file.  If the file does not exist, it
// returns an empty stateFile (reality is empty).
func (p *Provider) load() (*stateFile, error) {
	data, err := os.ReadFile(p.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return &stateFile{Resources: map[string]storedResource{}}, nil
	}
	if err != nil {
		return nil, err
	}
	// Empty file also means empty state.
	if len(data) == 0 {
		return &stateFile{Resources: map[string]storedResource{}}, nil
	}
	var sf stateFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, fmt.Errorf("parse state file %s: %w", p.statePath, err)
	}
	if sf.Resources == nil {
		sf.Resources = map[string]storedResource{}
	}
	return &sf, nil
}

// save serialises the stateFile and writes it atomically: the JSON is written
// to a temp file in the same directory and then os.Rename'd onto statePath.
//
// Readers (the acceptance tests poll the state file while the daemon runs)
// therefore always see either the previous or the new complete file, never a
// truncated one. A direct os.WriteFile truncates first and made concurrent
// readers intermittently fail with "unexpected end of JSON input".
func (p *Provider) save(sf *stateFile) error {
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return fmt.Errorf("fakeprovider: marshal state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(p.statePath), "."+filepath.Base(p.statePath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("fakeprovider: create temp state: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("fakeprovider: write temp state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("fakeprovider: close temp state: %w", err)
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("fakeprovider: chmod temp state: %w", err)
	}
	if err := os.Rename(tmpName, p.statePath); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("fakeprovider: rename state: %w", err)
	}
	return nil
}

// anyToTypedValue converts a JSON-decoded any value to a core.TypedValue.
// JSON numbers decode as float64; strings as string; booleans as bool.
func anyToTypedValue(v any) core.TypedValue {
	switch val := v.(type) {
	case string:
		return core.TypedValue{Kind: core.FieldKindString, Value: val}
	case float64:
		return core.TypedValue{Kind: core.FieldKindInt, Value: int64(val)}
	case bool:
		return core.TypedValue{Kind: core.FieldKindBool, Value: val}
	default:
		// Fall back to string representation for unknown types.
		return core.TypedValue{Kind: core.FieldKindString, Value: fmt.Sprintf("%v", v)}
	}
}
