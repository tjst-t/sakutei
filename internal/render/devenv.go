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

// devenv.go — devenv（坪庭）最小形: render 外部入力の差し替え（§26.2 / §23.6 /
// ADR-0016）。
//
// # 坪庭 = 本番の内側に隔離された、同じ材料の小さな庭
//
// devenv は本番と同じ intent を、同じ reconciler で流す。ただし render フェーズ1
// の「外部入力の解決点」——inventory / allocation / secret——だけを差し替える。
// これにより本番プール（inventory/allocation）を消費せず、本番 SecretBackend を
// 叩かずに、収束経路の全体をドッグフーディングできる（§26.2 devenv dogfooding）。
//
// # reconciler 本体は分岐しない（§26.2）
//
// devenv は render 第一級化した「入力の差し替え点」として表現される。reconciler や
// render の本体コードに devenv 分岐は入らない。DevenvProfile は既存 seam
// （ExternalInputResolver / AllocationAuthority / SecretBackend）と ResolverCache
// の上に、フェイク/専用実装を束ねて配線するだけである。呼び出し側は
// DevenvProfile.ExternalInputCache() を既存の ResolveExternalInputs にそのまま渡す。
//
// # 隔離保証（本番非接触 = テスト失敗）
//
// DevenvProfile が組み立てる resolver / backend は、いずれも呼び出しを記録する
// フェイクである（RecordingResolver / RecordingAllocationAuthority /
// FakeSecretBackend）。本番実装を devenv に配線したくない、という不変条件を
// テスト可能にするため、ProductionGuard* 型を用意する。これらは「本番資源を叩く
// はずの実装」を表し、一度でも呼ばれたら panic する（＝テスト失敗として検出できる）。
// devenv 経路が ProductionGuard* を一度も呼ばないことを assert すれば、「本番非接触」
// を機械的に検証できる。
package render

import (
	"context"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// RecordingResolver — 呼び出しを記録するフェイク ExternalInputResolver
// ─────────────────────────────────────────────────────────────────────────────

// RecordingResolver wraps an ExternalInputResolver (a fake, in devenv) and
// records the number of times Resolve was called.  It is the substituted
// inventory input for devenv: it never contacts a production CMDB — it delegates
// to the wrapped fake — but it makes the fact that ONLY the fake was used
// observable (§26.2 本番非接触の検証).
//
// RecordingResolver is safe for concurrent use.
type RecordingResolver struct {
	inner ExternalInputResolver

	mu    sync.Mutex
	calls int
}

// NewRecordingResolver wraps inner with call recording.
func NewRecordingResolver(inner ExternalInputResolver) *RecordingResolver {
	return &RecordingResolver{inner: inner}
}

// Resolve implements ExternalInputResolver, recording the call before
// delegating to the wrapped resolver.
func (r *RecordingResolver) Resolve(ctx context.Context) (map[string]string, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return r.inner.Resolve(ctx)
}

// Calls returns how many times Resolve was invoked.
func (r *RecordingResolver) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// ─────────────────────────────────────────────────────────────────────────────
// FakeAllocationAuthority — 差し替え版 AllocationAuthority（本番プール非消費）
// ─────────────────────────────────────────────────────────────────────────────

// FakeAllocationAuthority is the substituted AllocationAuthority for devenv.  It
// returns a deterministic pseudo-allocation for any claim key WITHOUT consuming
// a production allocation pool (§26.2 本番プールを消費しない).  It records every
// claim key it was asked to resolve so a test can assert that allocation ran
// entirely against the fake.
//
// FakeAllocationAuthority is safe for concurrent use.
type FakeAllocationAuthority struct {
	// Prefix is prepended to the returned pseudo-allocation string.  Empty is
	// valid; it defaults to "devenv-alloc" at Resolve time.
	Prefix string

	mu       sync.Mutex
	resolved []string
}

// Resolve implements AllocationAuthority.  It returns a deterministic
// "<prefix>/<claimKey>" allocation and records the claim key.  It never touches
// a production pool.
func (a *FakeAllocationAuthority) Resolve(_ context.Context, claimKey string) (string, error) {
	a.mu.Lock()
	a.resolved = append(a.resolved, claimKey)
	a.mu.Unlock()
	prefix := a.Prefix
	if prefix == "" {
		prefix = "devenv-alloc"
	}
	return prefix + "/" + claimKey, nil
}

// ResolvedClaims returns a copy of the claim keys resolved so far, in call order.
func (a *FakeAllocationAuthority) ResolvedClaims() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.resolved))
	copy(out, a.resolved)
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// FakeSecretBackend — 差し替え版 SecretBackend（本番 secret 非展開）
// ─────────────────────────────────────────────────────────────────────────────

// FakeSecretBackend is the substituted SecretBackend for devenv.  It satisfies
// the render SecretBackend seam (metadata only: GenerationOf) but reads NOTHING
// from a real secret store — it returns a fixed generation string and records
// each path it was asked about.  This is deliberately NOT the real
// FileSecretBackend: devenv must not point at a production (or even site-local)
// secret store (§26.2 本番シークレットを展開しない).
//
// FakeSecretBackend is safe for concurrent use.
type FakeSecretBackend struct {
	// Generation is the fixed generation string returned for every path.
	// Empty defaults to "devenv-gen-1" at GenerationOf time.
	Generation string

	mu    sync.Mutex
	paths []string
}

// GenerationOf implements the render SecretBackend seam.  It returns the fixed
// devenv generation and records the queried path.  It contacts no real store.
func (b *FakeSecretBackend) GenerationOf(_ context.Context, secretPath string) (string, error) {
	b.mu.Lock()
	b.paths = append(b.paths, secretPath)
	b.mu.Unlock()
	gen := b.Generation
	if gen == "" {
		gen = "devenv-gen-1"
	}
	return gen, nil
}

// QueriedPaths returns a copy of the secret paths queried so far, in call order.
func (b *FakeSecretBackend) QueriedPaths() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.paths))
	copy(out, b.paths)
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// ProductionGuard* — 「本番を叩いたら panic」= 本番非接触をテスト可能にする番人
// ─────────────────────────────────────────────────────────────────────────────

// errProductionContactInDevenv is the panic value raised by any ProductionGuard*
// seam when it is invoked inside devenv.  A devenv run that reaches a production
// resolver/authority/backend is a hard bug: the panic converts "touched
// production" into a detectable test failure (§26.2).
const errProductionContactInDevenv = "sakutei devenv: production external input was contacted in devenv mode (§26.2 本番非接触 violated)"

// ProductionGuardResolver stands in for a production ExternalInputResolver that
// MUST NOT be reached in devenv.  If Resolve is ever called it panics, so any
// devenv path that accidentally wires the production inventory resolver fails
// loudly (and a test can recover the panic to assert non-contact).
type ProductionGuardResolver struct{}

// Resolve panics: reaching a production inventory resolver in devenv is a bug.
func (ProductionGuardResolver) Resolve(context.Context) (map[string]string, error) {
	panic(errProductionContactInDevenv)
}

// ProductionGuardAllocationAuthority stands in for a production
// AllocationAuthority that MUST NOT be reached in devenv.
type ProductionGuardAllocationAuthority struct{}

// Resolve panics: reaching a production allocation pool in devenv is a bug.
func (ProductionGuardAllocationAuthority) Resolve(context.Context, string) (string, error) {
	panic(errProductionContactInDevenv)
}

// ProductionGuardSecretBackend stands in for a production SecretBackend that
// MUST NOT be reached in devenv.
type ProductionGuardSecretBackend struct{}

// GenerationOf panics: reaching a production secret store in devenv is a bug.
func (ProductionGuardSecretBackend) GenerationOf(context.Context, string) (string, error) {
	panic(errProductionContactInDevenv)
}

// Compile-time assertions: the substituted and guard types satisfy exactly the
// same seams as their production counterparts, so devenv is a drop-in input swap
// (§26.2) — the fakes and guards are interchangeable with real implementations.
var (
	_ ExternalInputResolver = (*RecordingResolver)(nil)
	_ ExternalInputResolver = ProductionGuardResolver{}
	_ AllocationAuthority   = (*FakeAllocationAuthority)(nil)
	_ AllocationAuthority   = ProductionGuardAllocationAuthority{}
	_ SecretBackend         = (*FakeSecretBackend)(nil)
	_ SecretBackend         = ProductionGuardSecretBackend{}
)

// ─────────────────────────────────────────────────────────────────────────────
// DevenvProfile — 外部入力を全て差し替えた render 実行プロファイル（§26.2）
// ─────────────────────────────────────────────────────────────────────────────

// DevenvProfile bundles the substituted render phase-1 external inputs for a
// devenv (坪庭) run.  Every external input is a fake/recording implementation;
// none contacts a production system.  The profile is a pure wiring value — it
// adds no branch to render or the reconciler (§26.2): callers feed
// ExternalInputCache() into the existing ResolveExternalInputs, and pass
// Allocation()/Secret() wherever those seams are consumed.
//
// The load-bearing property is isolation: a render/reconcile driven by a
// DevenvProfile produces desired state from substituted inputs and CANNOT reach
// a production pool/allocator/secret store, because the profile holds only the
// fakes and the ProductionGuard* number of Contacts stays zero.
type DevenvProfile struct {
	// Inventory is the recording wrapper around the fake inventory resolver.
	// Inspect Inventory.Calls() to prove the fake (and only the fake) was used.
	Inventory *RecordingResolver

	// inventoryCache is the ResolverCache built on top of Inventory.  devenv is
	// built ON the existing FakeInventoryResolver + ResolverCache path (§26.2 /
	// AC-S33266e-3-3): no bespoke resolution path is introduced.
	inventoryCache *ResolverCache

	// Allocation is the substituted allocation authority (no production pool).
	Allocation *FakeAllocationAuthority

	// Secret is the substituted secret backend (no production secret store).
	Secret *FakeSecretBackend
}

// DevenvOption configures a DevenvProfile at construction time.
type DevenvOption func(*devenvConfig)

// devenvConfig holds the resolved options for NewDevenvProfile.
type devenvConfig struct {
	inventoryHash string
	ttl           time.Duration
	allocPrefix   string
	secretGen     string
}

// WithDevenvInventoryHash sets the deterministic inventory hash the fake
// inventory resolver returns.  Default: "devenv-inventory".
func WithDevenvInventoryHash(hash string) DevenvOption {
	return func(c *devenvConfig) { c.inventoryHash = hash }
}

// WithDevenvAllocationPrefix sets the prefix of the fake allocation strings.
func WithDevenvAllocationPrefix(prefix string) DevenvOption {
	return func(c *devenvConfig) { c.allocPrefix = prefix }
}

// WithDevenvSecretGeneration sets the fixed generation the fake secret backend
// returns.
func WithDevenvSecretGeneration(gen string) DevenvOption {
	return func(c *devenvConfig) { c.secretGen = gen }
}

// NewDevenvProfile constructs a DevenvProfile whose phase-1 external inputs are
// ALL substituted with fakes (§26.2):
//
//   - inventory  → FakeInventoryResolver wrapped in a RecordingResolver, behind
//     the existing ResolverCache (built on the existing §9.2/§9.3 path);
//   - allocation → FakeAllocationAuthority (no production pool consumed);
//   - secret     → FakeSecretBackend (no production secret store contacted).
//
// No production external system is referenced anywhere in the returned profile.
func NewDevenvProfile(opts ...DevenvOption) *DevenvProfile {
	cfg := devenvConfig{
		inventoryHash: "devenv-inventory",
		ttl:           0, // resolve fresh every call (deterministic acceptance runs)
		allocPrefix:   "devenv-alloc",
		secretGen:     "devenv-gen-1",
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	// Build the inventory input on the EXISTING FakeInventoryResolver +
	// ResolverCache path (§26.2 / AC-S33266e-3-3): devenv is an input swap, not
	// a new resolution mechanism.
	fake := &FakeInventoryResolver{Hash: cfg.inventoryHash, Fail: false}
	recording := NewRecordingResolver(fake)
	cache := NewResolverCache(recording, cfg.ttl)

	return &DevenvProfile{
		Inventory:      recording,
		inventoryCache: cache,
		Allocation:     &FakeAllocationAuthority{Prefix: cfg.allocPrefix},
		Secret:         &FakeSecretBackend{Generation: cfg.secretGen},
	}
}

// ExternalInputCache returns the *ResolverCache to feed into the existing
// ResolveExternalInputs (render phase 1).  Because it wraps the recording fake,
// running ResolveExternalInputs against it drives ONLY the substituted inventory
// input (§26.2 reconciler 本体を分岐させない — the same function the production
// path calls, just wrapping a fake).
func (p *DevenvProfile) ExternalInputCache() *ResolverCache { return p.inventoryCache }

// ResolveExternalInputs runs render phase 1 for this devenv profile against the
// substituted inventory input.  It is a thin convenience over the package-level
// ResolveExternalInputs; it exists so tests and a devenv reconcile share one
// reachable entry point.
func (p *DevenvProfile) ResolveExternalInputs(ctx context.Context) ResolveResult {
	return ResolveExternalInputs(ctx, p.inventoryCache)
}

// AllocationAuthority returns the substituted allocation seam, typed as the
// AllocationAuthority interface, so callers wire the fake exactly where a
// production authority would go (proving the swap point is the only seam).
func (p *DevenvProfile) AllocationAuthority() AllocationAuthority { return p.Allocation }

// SecretBackend returns the substituted secret seam, typed as the SecretBackend
// interface, for the same reason as AllocationAuthority.
func (p *DevenvProfile) SecretBackend() SecretBackend { return p.Secret }
