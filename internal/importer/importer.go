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

// Package importer implements `sakutei import` (reverse-render, §24.4): it
// Observes existing reality and turns it into intent, so brownfield adoption
// starts from "today's reality = intent" with a zero diff.
//
// The output is a SNAPSHOT of the present, not an ideal: it carries legacy and
// mistakes, and a person must read and tidy it before it becomes intent (§24.4).
// To make that review cheaper, import observes reality several times and marks
// every field whose value changed between observations as a managed-field
// CANDIDATE — the person accepts or rejects the suggestion instead of finding
// runtime fields from scratch.
//
// Import never writes to reality (it only calls List and Read), and it holds no
// observation state: every invocation re-observes from scratch (§4.5).
package importer

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// Options controls one import run.
type Options struct {
	// Providers to import from, keyed by provider name.  Each must implement
	// provider.Lister (import enumerates reality through List, §24.4).
	Providers map[string]provider.Provider

	// Selector limits import to resources whose scope Matches it.  The zero
	// value / empty labels selects everything.
	Selector core.Scope

	// Samples is how many times reality is observed (>= 1).  A field whose
	// value differs between samples becomes a managed candidate.  With 1
	// sample no candidates can be detected.
	Samples int

	// Interval is the wait between samples.
	Interval time.Duration

	// Sleep waits between samples; nil means time.Sleep.  Tests inject a
	// function that mutates reality instead of waiting.
	Sleep func(time.Duration)
}

// IncompleteListError is returned when a provider's List is not complete:
// import refuses an incomplete view of reality (§24.1).  Callers that reuse
// the importer (Sync Mode) word it for their own verb (#111).
type IncompleteListError struct {
	Provider string
	Status   core.ListStatus
}

func (e *IncompleteListError) Error() string {
	return fmt.Sprintf("provider %q: List is not complete (status %v); refusing to import an incomplete view of reality (§24.1)", e.Provider, e.Status)
}

// Resource is one imported resource.
type Resource struct {
	// Provider is the registry name the resource was imported from; it is
	// what the intent's "provider" key must say.
	Provider string
	ID       core.ID

	// Fields holds the intent value for every exported field (last sample).
	Fields map[string]string

	// ManagedCandidates maps a field name to the distinct values it took
	// across samples (in first-seen order).  Only fields that changed appear.
	ManagedCandidates map[string][]string

	// Omitted maps a field name that was observed but not exported to the
	// reason (computed / managed / secret without SecretRef).
	Omitted map[string]string
}

// Skipped is a resource that List reported but import could not export.
type Skipped struct {
	ID     core.ID
	Reason string
}

// Result is the outcome of Run.  Resources and Skipped are sorted by
// (provider, scope, local id) for deterministic output.
type Result struct {
	Resources []Resource
	Skipped   []Skipped
}

// Run observes reality and builds the import result.
//
// Errors (nothing is exported): a provider without List; a List that returns
// an error or is not ListComplete (an incomplete enumeration would silently
// produce an incomplete intent — the very input that makes prune dangerous,
// §24.1).  A resource whose Read is not Ok (unknown ≠ absent) or that is absent
// in the final sample is reported in Skipped, never written.
func Run(ctx context.Context, opts Options) (Result, error) {
	if opts.Samples < 1 {
		opts.Samples = 1
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}

	names := make([]string, 0, len(opts.Providers))
	for name := range opts.Providers {
		names = append(names, name)
	}
	sort.Strings(names)

	listers := make(map[string]provider.Lister, len(names))
	for _, name := range names {
		l, ok := opts.Providers[name].(provider.Lister)
		if !ok || !opts.Providers[name].Capabilities().List {
			return Result{}, fmt.Errorf("provider %q cannot be imported: it does not support List (import enumerates reality through List, §24.4)", name)
		}
		listers[name] = l
	}

	type sampleObs struct {
		provider string // registry name the resource was listed by
		id       core.ID
		obs      core.Observation
	}
	// samples[i][key] — observation of resource key in sample i.
	samples := make([]map[core.IDKey]sampleObs, 0, opts.Samples)

	for i := 0; i < opts.Samples; i++ {
		if i > 0 {
			sleep(opts.Interval)
		}
		cur := make(map[core.IDKey]sampleObs)
		for _, name := range names {
			lr, err := listers[name].List(ctx, opts.Selector)
			if err != nil {
				return Result{}, fmt.Errorf("provider %q: List: %w", name, err)
			}
			if lr.Status != core.ListComplete {
				return Result{}, &IncompleteListError{Provider: name, Status: lr.Status}
			}
			for _, id := range lr.IDs {
				if !id.Scope.Matches(opts.Selector) {
					continue
				}
				obs, err := opts.Providers[name].Read(ctx, id.Local)
				if err != nil {
					// Record as unknown; reported as skipped below.
					obs = core.Observation{Availability: core.AvailabilityUnknown}
				}
				cur[id.Key()] = sampleObs{provider: name, id: id, obs: obs}
			}
		}
		samples = append(samples, cur)
	}

	last := samples[len(samples)-1]
	var res Result
	for key, so := range last {
		p := opts.Providers[so.provider]
		if so.obs.Availability != core.AvailabilityOk {
			res.Skipped = append(res.Skipped, Skipped{ID: so.id, Reason: "not observable now (unknown ≠ absent); not exported"})
			continue
		}
		if so.obs.State == nil {
			res.Skipped = append(res.Skipped, Skipped{ID: so.id, Reason: "listed but absent when read; not exported"})
			continue
		}
		schema := p.Schema()
		r := Resource{Provider: so.provider, ID: so.id, Fields: map[string]string{}, ManagedCandidates: map[string][]string{}, Omitted: map[string]string{}}
		for field, tv := range so.obs.State {
			v, include, reason := diff.ObservedIntentValue(field, tv, schema)
			if !include {
				r.Omitted[field] = reason
				continue
			}
			r.Fields[field] = v

			// Distinct values across all samples where the resource was Ok.
			var seen []string
			for _, s := range samples {
				o, ok := s[key]
				if !ok || o.obs.Availability != core.AvailabilityOk || o.obs.State == nil {
					continue
				}
				tv2, ok := o.obs.State[field]
				if !ok {
					seen = appendDistinct(seen, "")
					continue
				}
				v2, _, _ := diff.ObservedIntentValue(field, tv2, schema)
				seen = appendDistinct(seen, v2)
			}
			if len(seen) > 1 {
				r.ManagedCandidates[field] = seen
			}
		}
		res.Resources = append(res.Resources, r)
	}

	sort.Slice(res.Resources, func(i, j int) bool { return idLess(res.Resources[i].ID, res.Resources[j].ID) })
	sort.Slice(res.Skipped, func(i, j int) bool { return idLess(res.Skipped[i].ID, res.Skipped[j].ID) })
	return res, nil
}

func appendDistinct(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

func idLess(a, b core.ID) bool {
	if a.Provider != b.Provider {
		return a.Provider < b.Provider
	}
	if a.Scope.String() != b.Scope.String() {
		return a.Scope.String() < b.Scope.String()
	}
	return a.Local < b.Local
}
