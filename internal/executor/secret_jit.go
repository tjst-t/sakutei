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

// This file implements the JIT (just-in-time) secret resolution hook at the
// Apply boundary (§20.2: 実値化は Apply 直前のみ / 使用後 Zero()).
//
// Design constraint: the core.Plan is value-free by construction (§20 — only the
// SecretRef ref+generation enters diff/convergence, never the value).  So the
// resolved plaintext must NOT be threaded through the Plan.  Instead the
// SecretResolvingExecutor resolves each SecretRef JUST before Apply, hands the
// values to the provider out-of-band via a context-scoped bag, and Zeroes them
// immediately after Apply returns.  The value is never cached, never logged,
// never diffed, and never stored back on any SecretRef (§20.2).

package executor

import (
	"context"

	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// secretBagKey is the unexported context key under which resolved secret values
// are carried to the provider for a single Apply.  Unexported so only this
// package can put values into the context, and providers read them via the
// exported SecretFromContext accessor.
type secretBagKey struct{}

// secretBag holds resolved secret values for one Apply, keyed by SecretRef path.
// It is placed in the Apply context and its values are Zeroed after Apply.
type secretBag struct {
	// values maps secret path → resolved SecretValue.  Pointers so Zero() on the
	// stored value is visible to the accessor and to the deferred cleanup.
	values map[string]*core.SecretValue
}

// SecretFromContext returns the resolved plaintext for the given secret path if
// the current Apply resolved it JIT.  Providers call this at the moment they
// hand the value to the underlying system.  Returns ("", false) when no value
// was resolved for that path.
//
// The returned string is live ONLY for the duration of the Apply call; after
// Apply returns, the SecretResolvingExecutor Zeroes the backing storage.
func SecretFromContext(ctx context.Context, path string) (string, bool) {
	bag, ok := ctx.Value(secretBagKey{}).(*secretBag)
	if !ok || bag == nil {
		return "", false
	}
	v, ok := bag.values[path]
	if !ok {
		return "", false
	}
	return v.Expose(), true
}

// SecretResolvingExecutor wraps an Executor and performs JIT secret resolution
// at the Apply boundary (§20.2).  It resolves every SecretRef referenced by the
// plan just before delegating to the inner Executor, and Zeroes the resolved
// values immediately after Apply returns (success or failure).
type SecretResolvingExecutor struct {
	inner    Executor
	resolver core.SecretResolver
	// refs maps a resource target's Local ID + field name to the SecretRef to
	// resolve for that op.  In v1a the reconcile driver populates this from the
	// desired intent (the only place SecretRefs are known); the Plan itself
	// stays value-free.  A nil/empty map means "no secrets to resolve" and the
	// wrapper is a transparent pass-through.
	refs []core.SecretRef
}

var _ Executor = (*SecretResolvingExecutor)(nil)

// NewSecretResolving wraps inner with JIT secret resolution using resolver.
// refs is the set of SecretRefs whose values must be resolved for the plans
// this executor will run this cycle (populated by the driver from desired
// intent; the Plan stays value-free).
func NewSecretResolving(inner Executor, resolver core.SecretResolver, refs []core.SecretRef) *SecretResolvingExecutor {
	return &SecretResolvingExecutor{inner: inner, resolver: resolver, refs: refs}
}

// Execute resolves all SecretRefs JIT, injects them into the Apply context,
// delegates to the inner executor, then Zeroes every resolved value (§20.2).
//
// Invariants enforced here:
//   - Resolution happens ONLY here, immediately before Apply (not during Diff).
//   - Values are Zeroed in a deferred call so they are wiped even if Apply
//     panics or errors.
//   - No value is ever written back onto a SecretRef (the refs slice is read
//     only; SecretRef has no value setter).
func (e *SecretResolvingExecutor) Execute(ctx context.Context, p provider.Provider, plan core.Plan) error {
	if len(e.refs) == 0 || e.resolver == nil {
		// Nothing to resolve: transparent pass-through.
		return e.inner.Execute(ctx, p, plan)
	}

	bag := &secretBag{values: make(map[string]*core.SecretValue, len(e.refs))}
	// Deferred Zero: guarantees the plaintext does not linger past this Apply
	// even on panic/error (§20.2 使用後 Zero()).
	defer func() {
		for _, v := range bag.values {
			v.Zero()
		}
	}()

	for _, ref := range e.refs {
		val, err := ref.Resolve(ctx, e.resolver)
		if err != nil {
			return err
		}
		v := val // copy addressable
		bag.values[ref.Path()] = &v
	}

	ctxWithSecrets := context.WithValue(ctx, secretBagKey{}, bag)
	return e.inner.Execute(ctxWithSecrets, p, plan)
}
