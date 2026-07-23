// Copyright 2026 Takumi Tsujishita
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
// The map key is "<scope>/<local>" encoded as a composite string.
type stateFile struct {
	Resources map[string]storedResource `json:"resources"`
}

// Provider is the fake implementation.  It satisfies both provider.Provider
// and provider.Lister.
type Provider struct {
	// statePath is the path to the JSON state file.
	statePath string
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
	sf, err := p.load()
	if err != nil {
		return core.Observation{
			Availability: core.AvailabilityUnknown,
			ObservedAt:   time.Now(),
		}, provider.NewTransientError(fmt.Errorf("fakeprovider: load state: %w", err))
	}

	key := id
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
//
// This implements the Create and Update operations.  Delete is not implemented
// in this Story (no prune scope yet); a Delete Op produces an error so tests
// can verify it is not called by plan.
//
// IMPORTANT: plan (sakutei plan) MUST NEVER call Apply.  Apply is the
// reconcile path only (ADR-0002).
func (p *Provider) Apply(_ context.Context, plan core.Plan) error {
	sf, err := p.load()
	if err != nil {
		return provider.NewTransientError(fmt.Errorf("fakeprovider: load state: %w", err))
	}

	for _, op := range plan.Ops {
		key := op.Target.Local
		switch op.Kind {
		case core.OpKindCreate, core.OpKindUpdate:
			// Reconstruct fields from the Op's FieldDiff list.
			// For Create, From is empty; To holds the desired value string.
			fields := make(map[string]any, len(op.Fields))
			for _, fd := range op.Fields {
				fields[fd.Field] = fd.To
			}
			sf.Resources[key] = storedResource{Fields: fields}
		case core.OpKindDelete:
			delete(sf.Resources, key)
		}
	}

	return p.save(sf)
}

// List returns all resource local IDs in the given scope.
func (p *Provider) List(_ context.Context, _ core.Scope) ([]core.ID, error) {
	sf, err := p.load()
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
