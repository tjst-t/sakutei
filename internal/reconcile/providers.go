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
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"

	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/extowner"
	"github.com/tjst-t/sakutei/v2/internal/prune"
	"github.com/tjst-t/sakutei/v2/internal/task"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
)

// ProviderResolver returns the provider of a provider name (a resource's
// ID.Provider), e.g. a lookup in the ProviderRegistry.  An error means the
// name cannot be served: the resources of that provider are unobservable for
// the cycle (no Apply, no prune) and the resolution is retried next cycle.
type ProviderResolver func(name string) (provider.Provider, error)

// IntentProviderResolver resolves a provider name with the intent's
// providers.<name>: entry (§7.7, Backlog #181), e.g. the ProviderRegistry's
// GetWithIntent: the entry's range settings are merged into the provider
// config (a setting only in the config comes back as a warning; one in both
// must agree, or Resolve fails).
type IntentProviderResolver interface {
	// CheckIntentProviders refuses an entry naming an unknown provider or
	// one that takes no range settings from the intent.
	CheckIntentProviders(entries map[string]map[string]any) error
	// ResolveWithIntent builds the provider of name with its entry (nil:
	// none).
	ResolveWithIntent(name string, entry map[string]any) (provider.Provider, []string, error)
}

// providerEntry is one resolved provider with the per-provider engines that
// live as long as the daemon (S3): the diff engine built from its Schema and
// the prune engine whose List count baseline (§12.4b) belongs to this
// provider's List alone.
type providerEntry struct {
	name   string
	p      provider.Provider
	engine *diff.Engine
	prune  *prune.Engine // nil when p is not a Lister
	// observePathLogged is set once the daemon has logged which Observe path
	// (BulkObserve or per-resource Read, §5.7) this provider takes.  Touched
	// only by the cycle goroutine.
	observePathLogged bool
}

func newProviderEntry(name string, p provider.Provider) *providerEntry {
	e := &providerEntry{name: name, p: p, engine: diff.ForProvider(p)}
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
	// intent, when set, replaces resolve (§7.7, #181): ranges is this
	// cycle's providers: section, builtWith the entry each cached provider
	// was built with (a changed entry rebuilds it).
	intent    IntentProviderResolver
	ranges    map[string]map[string]any
	builtWith map[string]string
	errOut    io.Writer

	mu     sync.Mutex
	byName map[string]*providerEntry
	// warned are the external_owners warnings already written (§7.3, #158).
	warned map[string]bool
}

func newProviderSet(cfg Config) *providerSet {
	if cfg.Provider != nil {
		return &providerSet{single: newProviderEntry("", cfg.Provider)}
	}
	return &providerSet{resolve: cfg.Providers, intent: cfg.IntentProviders, byName: map[string]*providerEntry{}, builtWith: map[string]string{}, errOut: cfg.ErrOut}
}

// useIntent takes this cycle's providers: section (§7.7, #181): an entry
// naming an unknown provider is an error, and a cached provider whose entry
// changed is dropped so the next get builds it with the new one.
func (s *providerSet) useIntent(entries map[string]map[string]any) error {
	if s.single != nil || s.intent == nil {
		if len(entries) > 0 && s.single == nil {
			return fmt.Errorf("intent providers: this daemon resolves providers without the intent's range settings")
		}
		return nil
	}
	if err := s.intent.CheckIntentProviders(entries); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ranges = entries
	for name := range s.byName {
		if s.builtWith[name] != entryKey(entries[name]) {
			delete(s.byName, name)
		}
	}
	return nil
}

// entryKey is the canonical form of an entry (json.Marshal sorts map keys).
func entryKey(entry map[string]any) string {
	if entry == nil {
		return ""
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return fmt.Sprintf("%v", entry)
	}
	return string(b)
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
	var p provider.Provider
	var err error
	switch {
	case s.intent != nil:
		var warnings []string
		p, warnings, err = s.intent.ResolveWithIntent(name, s.ranges[name])
		if err == nil {
			s.warnLocked(warnings)
			s.builtWith[name] = entryKey(s.ranges[name])
		}
	case s.resolve != nil:
		p, err = s.resolve(name)
	default:
		return nil, fmt.Errorf("no provider configured for %q", name)
	}
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

// observeOnly reports whether the provider of name declares
// Capabilities.ObserveOnly (§23.4).  An unresolvable name is not: its
// resources are unobservable anyway.
func (s *providerSet) observeOnly(name string) bool {
	e, err := s.get(name)
	return err == nil && e.p.Capabilities().ObserveOnly
}

// providerObservesOnly reports whether the resources of providerName, applied
// through p (a provider itself, or the multi-provider router), are observe-only.
func providerObservesOnly(p provider.Provider, providerName string) bool {
	if r, ok := p.(*routedProvider); ok {
		return r.set.observeOnly(providerName)
	}
	return p.Capabilities().ObserveOnly
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
		if e.p.Capabilities().ObserveOnly {
			// §23.4: never, even if an op got this far.
			return provider.NewPermanentError(fmt.Errorf("provider %q observes only (§23.4); refusing to apply %s", name, byName[name][0].Target))
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

func (r routedTaskRunner) RollbackTask(ctx context.Context, req task.Request) (string, error) {
	tr, err := r.runner(req.Resource)
	if err != nil {
		return "", err
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

// TaskSteps implements task.PhasedRunner: the steps the resource's provider
// declares, none when that provider runs simple Tasks only.
func (r routedTaskRunner) TaskSteps(ctx context.Context, req task.Request) ([]task.StepSpec, error) {
	tr, err := r.runner(req.Resource)
	if err != nil {
		return nil, err
	}
	pr, ok := tr.(task.PhasedRunner)
	if !ok {
		return nil, nil
	}
	return pr.TaskSteps(ctx, req)
}

func (r routedTaskRunner) phased(id core.ID) (task.PhasedRunner, error) {
	tr, err := r.runner(id)
	if err != nil {
		return nil, err
	}
	pr, ok := tr.(task.PhasedRunner)
	if !ok {
		return nil, fmt.Errorf("provider %q runs no phased tasks", id.Provider)
	}
	return pr, nil
}

// CheckStep implements task.PhasedRunner.
func (r routedTaskRunner) CheckStep(ctx context.Context, req task.Request, step string) (task.StepState, error) {
	pr, err := r.phased(req.Resource)
	if err != nil {
		return task.StepUnknown, err
	}
	return pr.CheckStep(ctx, req, step)
}

// RunStep implements task.PhasedRunner.
func (r routedTaskRunner) RunStep(ctx context.Context, req task.Request, step string) error {
	pr, err := r.phased(req.Resource)
	if err != nil {
		return err
	}
	return pr.RunStep(ctx, req, step)
}

// checkExternalOwners runs the load-time external_owners check (§7.3, #158,
// extowner.Check) against the providers the desired resources route to.  A
// warning (an exclusion only in a provider config) is written once per
// daemon, not every cycle.
func checkExternalOwners(owners extowner.Set, desired []diff.DesiredResource, providers *providerSet, errOut io.Writer) error {
	byName := map[string]provider.Provider{}
	for _, r := range desired {
		if _, ok := byName[r.ID.Provider]; ok {
			continue
		}
		if p := providers.provider(r); p != nil {
			byName[r.ID.Provider] = p
		}
	}
	warnings, err := extowner.Check(owners, desired, byName)
	if err != nil {
		return err
	}
	providers.mu.Lock()
	defer providers.mu.Unlock()
	providers.warnTo(errOut, warnings)
	return nil
}

// warnLocked writes warnings once per daemon; s.mu is held.
func (s *providerSet) warnLocked(warnings []string) { s.warnTo(s.errOut, warnings) }

func (s *providerSet) warnTo(w io.Writer, warnings []string) {
	for _, msg := range warnings {
		if s.warned[msg] || w == nil {
			continue
		}
		if s.warned == nil {
			s.warned = map[string]bool{}
		}
		s.warned[msg] = true
		fmt.Fprintf(w, "sakutei: warn: %s\n", msg)
	}
}

// manualObserver observes a strategy: manual resource for its Task (§6.2
// 人の手順で合わせる Task, #150): a fresh Read through the resource's own
// provider and a Diff against the desired fields — done when nothing differs.
// A resource not seen (unknown, converging, an error) is StepUnknown, never
// pending: unknown ≠ absent.
func manualObserver(providers *providerSet) task.ManualObserver {
	return func(ctx context.Context, req task.Request) (task.StepState, error) {
		e, err := providers.get(req.Resource.Provider)
		if err != nil {
			return task.StepUnknown, err
		}
		res := diff.DesiredResource{ID: req.Resource, Fields: req.Desired, UpdatePolicy: req.Policy}
		switch out, _, _ := verifyOnce(ctx, e.p, e.engine, res); out {
		case VerifyConverged:
			return task.StepDone, nil
		case VerifyIneffective:
			return task.StepPending, nil
		}
		return task.StepUnknown, nil
	}
}

// intentRangeNames are the providers of desired that have an entry in the
// intent's providers: section, in name order.
func intentRangeNames(entries map[string]map[string]any, desired []diff.DesiredResource) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range desired {
		name := r.ID.Provider
		if _, ok := entries[name]; ok && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
