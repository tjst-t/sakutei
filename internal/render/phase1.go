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

// Package render — render phase 1 (§9.1 フェーズ1, §9.2, §9.3).
//
// This file implements:
//
//   - ExternalInputResolver seam — the interface through which render phase 1
//     fetches external inputs (inventory, etc.) from external authorities.
//
//   - FakeInventoryResolver — the v1a fake implementation used by acceptance
//     tests.  Returns a single configurable deterministic hash string.
//
//   - AllocationAuthority / SecretBackend — seam-only interface stubs.  The
//     types are declared so callers can refer to them in signatures; no
//     implementation exists in v1a (implementations land in v1b / S8).
//
//   - ResolverCache — TTL-backed cache that wraps any ExternalInputResolver.
//     On a cache hit it returns the cached value without calling the resolver
//     (§9.3 平常時の頻度規律).  On a cache miss or error it calls the resolver;
//     if the call fails it returns the last known good value and records an
//     ExternalInputUnreachable event (§9.2 不達時前回保持 — the most important
//     safety property of this story: we MUST NOT produce empty desired).
//
// # IMPORTANT — rendered_revision 算出定義は §26 カテゴリ A
//
// The canonical definition of what is included in rendered_revision is a
// "must decide before implementation" item (§26 category A).  The v1a
// decision (external inputs = inventory hash only, passed as externalInputs
// to revision.ComputeRenderedRevision) is explicitly provisional and must be
// revisited before expanding to Vault secret generations and allocator epochs.
//
// # out-of-scope
//
// Allocation (AllocationAuthority.Resolve) and secret backend
// (SecretBackend.GenerationOf) implementations are not provided here (v1b /
// S8, per decisions.json).  The interface types are declared so call-sites
// can name them in type assertions and future wiring without import cycles.
package render

import (
	"context"
	"sort"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// ExternalInputResolver seam (§9.1 フェーズ1)
// ─────────────────────────────────────────────────────────────────────────────

// ExternalInputResolver is the render-phase-1 seam for external input
// resolution.  Implementations are responsible for fetching deterministic
// hashes from external authorities (CMDB inventory, etc.) and returning them
// as a map of named keys to hex-string hashes.
//
// Contract:
//   - Resolve MUST return a non-nil map on success.
//   - The values MUST be deterministic: the same external state always returns
//     the same map (same keys, same values).
//   - If the external authority is unreachable, Resolve MUST return a non-nil
//     error.  The ResolverCache will then apply the last-value-on-unreachable
//     rule (§9.2) and return the previous cached value.
//   - Resolve MUST NOT mutate the caller's state; it is a pure read.
type ExternalInputResolver interface {
	// Resolve fetches the current external input hashes.
	// Returns (nil, err) when the external authority is unreachable.
	Resolve(ctx context.Context) (map[string]string, error)
}

// ─────────────────────────────────────────────────────────────────────────────
// AllocationAuthority seam (v1b / S8 placeholder — no implementation)
// ─────────────────────────────────────────────────────────────────────────────

// AllocationAuthority is the seam through which render phase 1 requests
// deterministic address/VLAN/ASN allocations (§9.1 フェーズ1 allocation).
//
// The configured backends of the run (Vault KV v2, an outside command) use
// GenerationalSecretBackend and ResolveSecretGenerations (secretgen.go, #188).
//
// IMPORTANT — §26 カテゴリ A: allocation inclusion in rendered_revision
// must be settled before the AllocationAuthority implementation is built.
type AllocationAuthority interface {
	// Resolve returns the allocation assigned to the named claim.
	// Returns ("", err) when the authority is unreachable.
	Resolve(ctx context.Context, claimKey string) (string, error)
}

// ─────────────────────────────────────────────────────────────────────────────
// SecretBackend seam (the file store and devenv's fake)
// ─────────────────────────────────────────────────────────────────────────────

// SecretBackend is the seam through which render phase 1 resolves secret
// generations (Vault metadata, §9.1 フェーズ1 シークレット世代解決).
//
// The configured backends of the run (Vault KV v2, an outside command) use
// GenerationalSecretBackend and ResolveSecretGenerations (secretgen.go, #188).
//
// NOTE: SecretBackend resolves ONLY metadata (generation numbers / version
// IDs) — never secret values.  Secret values are resolved JIT at Apply time
// via the credential-broker (§20 SecretRef).
type SecretBackend interface {
	// GenerationOf returns the current generation identifier for the secret
	// at the given path.
	// Returns ("", err) when Vault is unreachable (e.g. sealed restart).
	GenerationOf(ctx context.Context, secretPath string) (string, error)
}

// ─────────────────────────────────────────────────────────────────────────────
// FakeInventoryResolver
// ─────────────────────────────────────────────────────────────────────────────

// FakeInventoryResolver is the v1a fake ExternalInputResolver used by
// acceptance tests.  It returns a single deterministic hash string under the
// key "inventory_hash".
//
// Behaviour:
//   - When Fail is false (default), Resolve returns {"inventory_hash": Hash}.
//   - When Fail is true, Resolve returns nil + error to simulate an
//     unreachable external authority (tests the §9.2 last-value-on-unreachable
//     safety path).
//
// FakeInventoryResolver is safe for concurrent use (fields are read-only after
// construction; callers must not mutate them).
type FakeInventoryResolver struct {
	// Hash is the deterministic inventory hash to return.
	// An empty string is valid (represents "no inventory hash available").
	Hash string

	// Fail controls whether Resolve returns an error (unreachable simulation).
	Fail bool
}

// inventoryHashKey is the map key under which FakeInventoryResolver publishes
// its hash.  Using a named constant prevents typos across test / production wiring.
const inventoryHashKey = "inventory_hash"

// Resolve implements ExternalInputResolver.
//
// Returns {"inventory_hash": r.Hash} on success, or (nil, error) when Fail is true.
func (r *FakeInventoryResolver) Resolve(_ context.Context) (map[string]string, error) {
	if r.Fail {
		return nil, &ExternalInputUnreachableError{Source: "FakeInventoryResolver"}
	}
	return map[string]string{inventoryHashKey: r.Hash}, nil
}

// ExternalInputUnreachableError is the canonical error type returned when an
// external authority cannot be reached.  ResolverCache recognises this type
// (as well as any non-nil error) to trigger the last-value-on-unreachable path.
type ExternalInputUnreachableError struct {
	// Source names the resolver that was unreachable (for logging).
	Source string
	// Cause is the underlying error, if any.
	Cause error
}

// Error implements the error interface.
func (e *ExternalInputUnreachableError) Error() string {
	if e.Cause != nil {
		return "external input unreachable [" + e.Source + "]: " + e.Cause.Error()
	}
	return "external input unreachable [" + e.Source + "]"
}

// Unwrap returns the underlying cause for errors.As/Is chaining.
func (e *ExternalInputUnreachableError) Unwrap() error { return e.Cause }

// ─────────────────────────────────────────────────────────────────────────────
// ResolverCache — TTL cache + last-value-on-unreachable (§9.2, §9.3)
// ─────────────────────────────────────────────────────────────────────────────

// DefaultTTL is the default TTL for cached external input resolutions (§9.3).
// Within this window, Resolve returns the cached value without calling the
// underlying resolver.  Set short for tests; increase in production.
const DefaultTTL = 30 * time.Second

// ResolverCache wraps an ExternalInputResolver with a TTL cache and
// implements the §9.2 last-value-on-unreachable safety rule.
//
// Safety invariant (§9.2): if the underlying resolver returns an error,
// the cache returns the last successfully resolved value (if any) rather than
// an error.  This prevents an unreachable external authority from causing an
// empty desired state (→ mass deletion).
//
// If no prior value exists (first call fails), Resolve returns
// (nil, ErrFirstCallUnreachable) so the caller can halt safely (convergence
// is not advanced; halt-default).
//
// ResolverCache is safe for concurrent use.
type ResolverCache struct {
	resolver ExternalInputResolver
	ttl      time.Duration

	mu          sync.Mutex
	lastValue   map[string]string // nil = no value cached yet
	lastFetched time.Time
	hasValue    bool // true once a successful resolution has been cached
}

// ErrFirstCallUnreachable is returned by ResolverCache.Resolve when the
// first-ever resolution attempt fails.  There is no prior value to fall back
// to; the caller must treat this as a halt (convergence not advanced).
type ErrFirstCallUnreachable struct {
	Cause error
}

func (e *ErrFirstCallUnreachable) Error() string {
	return "external input unreachable on first call (no prior value to fall back to): " + e.Cause.Error()
}
func (e *ErrFirstCallUnreachable) Unwrap() error { return e.Cause }

// NewResolverCache creates a new ResolverCache wrapping r with the given TTL.
// Pass DefaultTTL for production; use a short TTL (e.g. 0) in tests where
// you want to control every resolution call explicitly.
func NewResolverCache(r ExternalInputResolver, ttl time.Duration) *ResolverCache {
	return &ResolverCache{
		resolver: r,
		ttl:      ttl,
	}
}

// Resolve returns the resolved external input map, using the cache when the
// TTL has not expired.
//
// §9.3 TTL semantics:
//   - If the cache was populated within the TTL window, returns the cached
//     value immediately (no resolver call).
//
// §9.2 last-value-on-unreachable semantics:
//   - If the underlying resolver returns an error AND a prior value is cached,
//     logs an ExternalInputUnreachable event (via the returned bool) and returns
//     the prior cached value with no error.
//   - If the underlying resolver returns an error AND no prior value is cached
//     (first call), returns (nil, *ErrFirstCallUnreachable).
//
// The returned bool `unreachableFallback` is true when the last-value fallback
// was applied.  Callers should log an ExternalInputUnreachable event in this
// case (§9.2 ログに ExternalInputUnreachable イベントが出る).
func (c *ResolverCache) Resolve(ctx context.Context) (result map[string]string, unreachableFallback bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// TTL cache hit: return cached value without calling the resolver.
	if c.hasValue && time.Since(c.lastFetched) < c.ttl {
		return copyMap(c.lastValue), false, nil
	}

	// TTL miss (or first call): call the underlying resolver.
	resolved, resolveErr := c.resolver.Resolve(ctx)
	if resolveErr != nil {
		// §9.2: resolver is unreachable.
		if c.hasValue {
			// Last-value-on-unreachable: return the prior cached value.
			// The caller will log the ExternalInputUnreachable event.
			return copyMap(c.lastValue), true, nil
		}
		// First call failed: no prior value to fall back to.  Halt-safe.
		return nil, false, &ErrFirstCallUnreachable{Cause: resolveErr}
	}

	// Successful resolution: update the cache.
	c.lastValue = copyMap(resolved)
	c.lastFetched = time.Now()
	c.hasValue = true
	return copyMap(resolved), false, nil
}

// copyMap returns a shallow copy of m, or nil if m is nil.
func copyMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// ResolveExternalInputs — phase 1 integration helper
// ─────────────────────────────────────────────────────────────────────────────

// ResolveResult is the result of a render phase 1 external input resolution.
type ResolveResult struct {
	// Inputs is the resolved external input map.  May be nil when
	// FirstCallFailed is true (no value available).
	Inputs map[string]string

	// ExternalInputHashes is the sorted slice of "key=value" strings suitable
	// for passing to revision.ComputeRenderedRevision as externalInputs.
	// Empty when FirstCallFailed is true.
	ExternalInputHashes []string

	// UnreachableFallback is true when the last-value-on-unreachable path was
	// applied.  The caller should log an ExternalInputUnreachable event.
	UnreachableFallback bool

	// FirstCallFailed is true when no prior cached value exists and the
	// resolver returned an error.  Convergence must not be advanced.
	FirstCallFailed bool

	// Err holds the error when FirstCallFailed is true.
	Err error
}

// ResolveExternalInputs runs render phase 1: fetches external inputs via the
// cache and converts them to a slice of "key=value" strings for
// revision.ComputeRenderedRevision.
//
// If cache is nil, the function returns an empty ResolveResult (no external
// inputs — equivalent to the v1a seam with no resolver configured).
func ResolveExternalInputs(ctx context.Context, cache *ResolverCache) ResolveResult {
	if cache == nil {
		return ResolveResult{} // no resolver configured: v1a / no external inputs
	}

	resolved, fallback, err := cache.Resolve(ctx)
	if err != nil {
		// First call failed: halt-safe.
		return ResolveResult{FirstCallFailed: true, Err: err}
	}

	// Convert map to sorted "key=value" slice for deterministic hashing (§9.3:
	// TTL hit/miss must not change the hash; sorted order prevents key-order churn).
	hashes := mapToSortedKV(resolved)
	return ResolveResult{
		Inputs:              resolved,
		ExternalInputHashes: hashes,
		UnreachableFallback: fallback,
	}
}

// mapToSortedKV converts a map to a sorted slice of "key=value" strings.
// The sort is lexicographic by key.  This ensures the slice is deterministic
// regardless of map iteration order (Go maps iterate in random order).
func mapToSortedKV(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	// Collect keys, sort, then build the slice.
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(m))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}
