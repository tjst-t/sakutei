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

package reconcile

// providers.go — the daemon's per-resource provider routing (#67, §5).
//
// A single intent may declare resources of several providers (e.g. dnszone
// and nftables).  Each resource is Read, Diffed (with its own provider's
// Schema), Applied, Listed for prune and gated against its own provider's
// provider_version.  The concrete providers still come from the caller
// through the provider.Provider seam (F-1): Config.Provider pins every
// resource to one provider (the original single-provider daemon), and
// Config.Providers resolves each resource's provider name.

import (
	"context"
	"fmt"
	"io"
	"sort"
	"sync"

	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/prune"
	"github.com/tjst-t/sakutei/internal/task"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
	"github.com/tjst-t/sakutei/pkg/sdk"
)

// ProviderResolver returns the provider of a provider name (a resource's
// ID.Provider), e.g. a lookup in the ProviderRegistry.  An error means the
// name cannot be served: the resources of that provider are unobservable for
// the cycle (no Apply, no prune) and the resolution is retried next cycle.
type ProviderResolver func(name string) (provider.Provider, error)

// providerEntry is one resolved provider with the per-provider engines that
// live as long as the daemon (S3): the diff engine built from its Schema and
// the prune engine whose List count baseline (§12.4b) belongs to this
// provider's List alone.
type providerEntry struct {
	name   string
	p      provider.Provider
	engine *diff.Engine
	prune  *prune.Engine // nil when p is not a Lister
}

func newProviderEntry(name string, p provider.Provider) *providerEntry {
	e := &providerEntry{name: name, p: p, engine: diff.New(p.Schema())}
	if _, ok := p.(provider.Lister); ok {
		e.prune = prune.New()
	}
	return e
}

// providerSet routes each resource to its provider.  It is shared by the
// cycle and the async Apply goroutines, so lookups are locked.
type providerSet struct {
	single  *providerEntry // Config.Provider: every resource routes here
	resolve ProviderResolver

	mu     sync.Mutex
	byName map[string]*providerEntry
}

func newProviderSet(cfg Config) *providerSet {
	if cfg.Provider != nil {
		return &providerSet{single: newProviderEntry("", cfg.Provider)}
	}
	return &providerSet{resolve: cfg.Providers, byName: map[string]*providerEntry{}}
}

// get returns the provider of name, resolving it on first use.  A resolved
// provider must pass sdk.ValidateProvider (fail-closed, §10.4): an
// unversioned provider would let ghost diffs (cause C) through as drift.
func (s *providerSet) get(name string) (*providerEntry, error) {
	if s.single != nil {
		return s.single, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.byName[name]; ok {
		return e, nil
	}
	if s.resolve == nil {
		return nil, fmt.Errorf("no provider configured for %q", name)
	}
	p, err := s.resolve(name)
	if err != nil {
		return nil, fmt.Errorf("provider %q: %w", name, err)
	}
	if err := sdk.ValidateProvider(p); err != nil {
		return nil, fmt.Errorf("provider %q validation failed: %w", name, err)
	}
	e := newProviderEntry(name, p)
	s.byName[name] = e
	return e, nil
}

// entries returns the resolved providers in name order (deterministic prune
// and log order).
func (s *providerSet) entries() []*providerEntry {
	if s.single != nil {
		return []*providerEntry{s.single}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*providerEntry, 0, len(s.byName))
	for _, e := range s.byName {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// key is the per-provider key of the cross-cycle baselines (provider_version).
func (s *providerSet) key(name string) string {
	if s.single != nil {
		return ""
	}
	return name
}

// provider returns the provider of r, or nil when it cannot be resolved.
func (s *providerSet) provider(r diff.DesiredResource) provider.Provider {
	e, err := s.get(r.ID.Provider)
	if err != nil {
		return nil
	}
	return e.p
}

// router returns the provider.Provider the executor applies plans through.
// With a single provider it is that provider itself.
func (s *providerSet) router() provider.Provider {
	if s.single != nil {
		return s.single.p
	}
	return &routedProvider{set: s}
}

// taskRunner returns the runner of strategy: task resources: the provider's
// own task.Runner, routed by the resource's provider.
func (s *providerSet) taskRunner() task.Runner {
	if s.single != nil {
		r, _ := s.single.p.(task.Runner)
		return r
	}
	return routedTaskRunner{set: s}
}

// hasTaskRunner reports whether the provider of id can run Tasks.
func (s *providerSet) hasTaskRunner(id core.ID) bool {
	e, err := s.get(id.Provider)
	if err != nil {
		return false
	}
	_, ok := e.p.(task.Runner)
	return ok
}

// routedProvider applies each op through the provider of its target.  It is
// the provider the executor (and the fencing executor around it) sees when a
// daemon converges several providers.
type routedProvider struct {
	set *providerSet
}

var _ provider.Provider = (*routedProvider)(nil)

// Schema is empty: diffs are computed by the per-provider engines, never
// through the router.
func (r *routedProvider) Schema() provider.Schema { return provider.Schema{} }

// Capabilities declares nothing: capabilities are per provider.
func (r *routedProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }

// Read cannot be routed: a bare Local ID does not name its provider.  Every
// Observe goes through the resource's own provider.
func (r *routedProvider) Read(ctx context.Context, id string) (core.Observation, error) {
	return core.Observation{Availability: core.AvailabilityUnknown},
		provider.NewPermanentError(fmt.Errorf("read %q: a multi-provider router cannot route a bare local id", id))
}

// Apply groups the ops by their target's provider and applies each group
// through that provider.  An op whose provider cannot be resolved is never
// sent to another provider (the wrong provider would change the wrong thing).
func (r *routedProvider) Apply(ctx context.Context, plan core.Plan) error {
	byName := map[string][]core.Op{}
	var order []string
	for _, op := range plan.Ops {
		name := op.Target.Provider
		if _, seen := byName[name]; !seen {
			order = append(order, name)
		}
		byName[name] = append(byName[name], op)
	}
	for _, name := range order {
		e, err := r.set.get(name)
		if err != nil {
			return provider.NewPermanentError(err)
		}
		if err := e.p.Apply(ctx, core.Plan{Ops: byName[name]}); err != nil {
			return err
		}
	}
	return nil
}

// routedTaskRunner runs a Task through the task.Runner of its resource's
// provider.
type routedTaskRunner struct {
	set *providerSet
}

func (r routedTaskRunner) runner(id core.ID) (task.Runner, error) {
	e, err := r.set.get(id.Provider)
	if err != nil {
		return nil, err
	}
	tr, ok := e.p.(task.Runner)
	if !ok {
		return nil, fmt.Errorf("provider %q has no task runner", id.Provider)
	}
	return tr, nil
}

func (r routedTaskRunner) RunTask(ctx context.Context, req task.Request) error {
	tr, err := r.runner(req.Resource)
	if err != nil {
		return err
	}
	return tr.RunTask(ctx, req)
}

func (r routedTaskRunner) RollbackTask(ctx context.Context, req task.Request) error {
	tr, err := r.runner(req.Resource)
	if err != nil {
		return err
	}
	return tr.RollbackTask(ctx, req)
}

// resolveCycleProviders resolves the providers of this cycle's desired, in
// name order.  The names it cannot resolve are returned in unresolved (and
// logged): their resources are unobservable this cycle.  With a single
// Config.Provider it is always that provider, even with no desired (its List
// keeps the prune count baseline fresh).
func resolveCycleProviders(providers *providerSet, desired []diff.DesiredResource, errOut io.Writer) ([]*providerEntry, map[string]bool) {
	unresolved := map[string]bool{}
	if providers.single != nil {
		return []*providerEntry{providers.single}, unresolved
	}
	seen := map[string]bool{}
	var names []string
	for _, r := range desired {
		if !seen[r.ID.Provider] {
			seen[r.ID.Provider] = true
			names = append(names, r.ID.Provider)
		}
	}
	sort.Strings(names)
	var out []*providerEntry
	for _, name := range names {
		e, err := providers.get(name)
		if err != nil {
			unresolved[name] = true
			fmt.Fprintf(errOut, "sakutei: %v; its resources are unobservable this cycle (no Apply, no prune)\n", err)
			continue
		}
		out = append(out, e)
	}
	return out, unresolved
}
