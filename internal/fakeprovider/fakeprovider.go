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
type stateFile struct {
	Resources      map[string]storedResource `json:"resources"`
	FailInject     map[string]string         `json:"fail_inject,omitempty"`
	ObserveUnknown map[string]string         `json:"observe_unknown,omitempty"`
	ApplyDelay     map[string]string         `json:"apply_delay,omitempty"`
	ApplyCount     map[string]int            `json:"apply_count,omitempty"`
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

// Capabilities declares the fake as a fully-capable provider for testing.
func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		TrueObserve: true,
		List:        true,
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

	return core.Observation{
		Availability: core.AvailabilityOk,
		State:        st,
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

// List returns all resource local IDs in the given scope.
func (p *Provider) List(_ context.Context, _ core.Scope) ([]core.ID, error) {
	// Hold mu for the file read (see Read): an in-flight Apply from a prior cycle
	// may be writing the state file concurrently.
	p.mu.Lock()
	sf, err := p.load()
	p.mu.Unlock()
	if err != nil {
		return nil, provider.NewTransientError(fmt.Errorf("fakeprovider: load state: %w", err))
	}

	scope := core.NewScope(nil) // fake uses empty scope for simplicity in tests
	ids := make([]core.ID, 0, len(sf.Resources))
	for local := range sf.Resources {
		ids = append(ids, core.NewID(ProviderName, scope, local))
	}
	return ids, nil
}

// StatePath returns the path of the underlying state file.
func (p *Provider) StatePath() string {
	return p.statePath
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

// save serialises the stateFile and writes it atomically-ish (write + rename
// would be ideal but for test simplicity we write directly).
//
// NOTE: a production Provider MUST persist state via an atomic write to a temp
// file followed by os.Rename onto the target path, so a crash mid-write can
// never leave a truncated/corrupt state file (S2 progress; loss = incident).
func (p *Provider) save(sf *stateFile) error {
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return fmt.Errorf("fakeprovider: marshal state: %w", err)
	}
	return os.WriteFile(p.statePath, data, 0600)
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
