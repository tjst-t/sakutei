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

// Package registry provides a provider name → factory registry.
//
// The registry decouples cmd/ from concrete provider packages: cmd/ registers
// factories at startup (one per provider name) and resolves them at runtime
// using the provider name declared in each intent resource (ID.Provider).
//
// Design notes (ADR-0003, ADR-0022):
//
//   - ProviderFactory accepts a config map so each provider can read its own
//     settings (e.g. fake reads config["state"] for the state-file path;
//     dnszone reads config["zone"] for the zone-file path).
//   - DefaultRegistry() is a convenience function that returns a new registry
//     with the built-in providers pre-registered.  cmd/ calls it once at
//     startup and passes the returned registry to every subcommand.
//   - Register validates the provider via sdk.ValidateProvider; registering an
//     invalid provider is a programmer error and panics (fail-closed).
//   - Lookup returns (factory, true) or ("", false); the caller decides how to
//     handle an unknown provider (typically: error out with "unknown provider").
//   - The registry is NOT concurrent-safe for writes after construction.
//     Register is intended to be called at program startup (init or main)
//     before any goroutine calls Lookup.
package registry

import (
	"fmt"
	"sort"

	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// ProviderFactory creates a provider instance from a string→string config map.
//
// config is provider-specific.  Well-known keys:
//   - "state": path to the fake provider state file ("fake" provider).
//   - "zone":  path to the DNS zone file ("dnszone" provider, S62dbba-3).
//
// Implementations MUST return a fully-initialised provider whose
// sdk.ValidateProvider check passes (ProviderVersion non-empty, Schema lint
// clean).  The registry validates on Register; the factory itself need not
// re-validate.
type ProviderFactory func(config map[string]string) (provider.Provider, error)

// ProviderRegistry maps provider names to ProviderFactory functions.
//
// One registry instance is created per program run.  Factories are registered
// at startup via Register; providers are resolved at runtime via Get.
type ProviderRegistry struct {
	factories map[string]ProviderFactory
}

// New returns an empty ProviderRegistry.
// Use DefaultRegistry() to get a registry pre-loaded with built-in providers.
func New() *ProviderRegistry {
	return &ProviderRegistry{factories: make(map[string]ProviderFactory)}
}

// Register adds a named factory to the registry.
//
// Calling Register with the same name twice replaces the earlier entry.
// name must be non-empty; factory must be non-nil.
//
// Unlike DefaultRegistry, Register does NOT invoke the factory to validate the
// provider at registration time (the factory requires a config that may not be
// known yet).  Validation happens in Get when the factory is first called.
func (r *ProviderRegistry) Register(name string, factory ProviderFactory) {
	if name == "" {
		panic("registry.Register: name must not be empty")
	}
	if factory == nil {
		panic("registry.Register: factory must not be nil")
	}
	r.factories[name] = factory
}

// Lookup returns the factory registered under name, or (nil, false) when no
// factory is registered for that name.
func (r *ProviderRegistry) Lookup(name string) (ProviderFactory, bool) {
	f, ok := r.factories[name]
	return f, ok
}

// Names returns the registered provider names, sorted.
func (r *ProviderRegistry) Names() []string {
	names := make([]string, 0, len(r.factories))
	for name := range r.factories {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Get resolves the named provider by calling its factory with the given config.
//
// Returns an error if:
//   - name is not registered ("unknown provider: <name>").
//   - the factory itself returns an error.
//
// The returned provider is ready to use; the caller does not need to re-validate.
func (r *ProviderRegistry) Get(name string, config map[string]string) (provider.Provider, error) {
	factory, ok := r.factories[name]
	if !ok {
		return nil, fmt.Errorf("unknown provider: %s", name)
	}
	p, err := factory(config)
	if err != nil {
		return nil, fmt.Errorf("provider %q: factory: %w", name, err)
	}
	return p, nil
}
