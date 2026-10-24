// Copyright 2026 tjst-t
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package allocator

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjst-t/sakutei/pkg/allocation"
)

// Cache is the shard-local Lookup cache of §19.6.  An allocation fact does
// not change until its Release, and only its owner releases it (1 pool, 1
// authority, §19.7), so a value once allocated is cached and invalidated
// only by this shard's own Release — in theory. The steady-state cycle then
// resolves most requests from the cache: calls to the allocator are
// proportional to churn + revalidations, not to the fleet size (§19.9, §22.4).
//
// **v3.19 の決まり（Backlog #105、2026-10-24 人が決定）**：v1 は "自分の
// Release のときだけ" を破る経路を持つ——`POST /v1/release` による手の
// 解放（v1 で解放する唯一の手段）は、どのシャードの自分の Release でもなく
// 外から割当事実を変える。無期限キャッシュのままだと、この経路で解放された
// 値をこのシャードが知らずに使い続け、cooldown 明けに別 owner へ再払い出し
// されて衝突しうる（§19.5 が最悪級と呼ぶ事故）。secret の
// last-resolved + TTL キャッシュ（§19.6 の同じ段落、§20.3/§20.5）と同型に、
// **TTL（既定 DefaultLookupTTL）を超えたエントリは使う前に Lookup で
// 1 回だけ確かめる**：authority にまだ同じ owner の記録があれば使い続け
// （値が動いていれば更新）、無くなっていれば（他所で解放された）キャッシュを
// 捨てて新たに Allocate する。authority に届かないときは確定した無効化の
// 証拠が無いので、度に古い値を使い続ける（§19.9 の waiting-allocation は
// 新規の採番だけに掛かる）。TTL は cooldown（既定 24h）より十分短くし、
// 衝突が起きる前に必ず一度は確かめが入るようにする。これにより定常状態の
// コール数はなお「周回数」ではなく「churn + 1/TTL」に比例する。
//
// The cache is S3 (memory only): losing it on restart costs one Allocate per
// request, never an incident.  It holds facts this shard established (plus a
// bounded-staleness revalidation of them), not observations of reality, so
// observation state-lessness (§3) is untouched.
//
// Reservations (Reserve) are never cached: they expire unless confirmed.
type Cache struct {
	inner allocation.AllocationAuthority

	mu     sync.Mutex
	values map[cacheKey]cacheEntry
	pools  map[string][2]int // pools declared through EnsurePool → range

	calls atomic.Int64

	// TTL bounds how long a cached value is trusted before being
	// revalidated against the authority (§19.6, Backlog #105).
	// DefaultLookupTTL when zero.
	TTL time.Duration
	// Now is the clock; time.Now when nil (tests inject one).
	Now func() time.Time
}

type cacheKey struct{ pool, owner string }

type cacheEntry struct {
	value     string
	checkedAt time.Time
}

// DefaultLookupTTL is the revalidation interval applied when Cache.TTL is
// zero: well under allocator.DefaultCooldown (24h) so a revalidation always
// lands before a manually-released value could be reissued elsewhere.
const DefaultLookupTTL = 15 * time.Minute

var _ allocation.AllocationAuthority = (*Cache)(nil)

// NewCache wraps inner with the Lookup cache (DefaultLookupTTL; set c.TTL
// after construction to change it).
func NewCache(inner allocation.AllocationAuthority) *Cache {
	return &Cache{inner: inner, values: map[cacheKey]cacheEntry{}, pools: map[string][2]int{}}
}

// InnerCalls returns how many calls reached the wrapped authority — the
// allocator QPS this shard generates (§19.6: proportional to churn plus
// bounded revalidations, not to the fleet size).
func (c *Cache) InnerCalls() int64 { return c.calls.Load() }

func (c *Cache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Cache) ttl() time.Duration {
	if c.TTL > 0 {
		return c.TTL
	}
	return DefaultLookupTTL
}

// get returns the cached value for pool/owner (ok=false if absent) and
// whether it is due for revalidation (stale).
func (c *Cache) get(pool, owner string) (value string, ok, stale bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.values[cacheKey{pool, owner}]
	if !ok {
		return "", false, false
	}
	return e.value, true, c.now().Sub(e.checkedAt) >= c.ttl()
}

func (c *Cache) put(pool, owner, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values[cacheKey{pool, owner}] = cacheEntry{value: value, checkedAt: c.now()}
}

func (c *Cache) drop(pool, owner string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.values, cacheKey{pool, owner})
}

// revalidate confirms a stale cached value against the authority (one
// Lookup, never a write).  found=false means the authority no longer has an
// allocation for owner (released elsewhere, Backlog #105): the caller must
// treat this as a cache miss.  An authority error leaves the stale value in
// place (ok=true) — unreachability is not evidence of invalidation (§19.9
// degrades only new allocations).
func (c *Cache) revalidate(ctx context.Context, pool, owner, stale string) (value string, found bool) {
	c.calls.Add(1)
	v, ok, err := c.inner.Lookup(ctx, pool, owner)
	if err != nil {
		return stale, true
	}
	if !ok {
		c.drop(pool, owner)
		return "", false
	}
	c.put(pool, owner, v)
	return v, true
}

// Allocate implements allocation.AllocationAuthority: a fresh cache hit
// never reaches the allocator.  A hit past TTL is revalidated with one
// Lookup (Backlog #105) before being trusted again; if the authority no
// longer has the owner (released through another path, e.g. the v1 manual
// `/v1/release` escape hatch), the stale entry is dropped and a real
// Allocate self-heals from the authority's live state.
func (c *Cache) Allocate(ctx context.Context, pool, owner string) (string, error) {
	if v, ok, stale := c.get(pool, owner); ok {
		if !stale {
			return v, nil
		}
		if rv, found := c.revalidate(ctx, pool, owner, v); found {
			return rv, nil
		}
		// fall through: cache entry dropped, allocate fresh below.
	}
	c.calls.Add(1)
	v, err := c.inner.Allocate(ctx, pool, owner)
	if err != nil {
		return "", err
	}
	c.put(pool, owner, v)
	return v, nil
}

// Reserve implements allocation.AllocationAuthority (passed through, not cached).
func (c *Cache) Reserve(ctx context.Context, pool, owner string, ttl time.Duration) (string, error) {
	c.calls.Add(1)
	return c.inner.Reserve(ctx, pool, owner, ttl)
}

// Confirm implements allocation.AllocationAuthority (passed through).
func (c *Cache) Confirm(ctx context.Context, pool, owner string) error {
	c.calls.Add(1)
	return c.inner.Confirm(ctx, pool, owner)
}

// Release implements allocation.AllocationAuthority.  The cached value is
// invalidated only once the allocator has accepted the Release: while the
// allocator is down the Release is simply deferred (§19.9) and the value
// stays in use.
func (c *Cache) Release(ctx context.Context, pool, owner string) error {
	c.calls.Add(1)
	if err := c.inner.Release(ctx, pool, owner); err != nil {
		return err
	}
	c.mu.Lock()
	delete(c.values, cacheKey{pool, owner})
	c.mu.Unlock()
	return nil
}

// Lookup implements allocation.AllocationAuthority: answered from the cache
// when possible.  A fresh hit is returned without a call; a stale hit is
// revalidated the same way Allocate does (Backlog #105), including the
// self-heal that reports found=false if the authority has released it
// elsewhere.  A cache miss is passed through and not cached (the value may
// be a reservation that can still expire).
func (c *Cache) Lookup(ctx context.Context, pool, owner string) (string, bool, error) {
	if v, ok, stale := c.get(pool, owner); ok {
		if !stale {
			return v, true, nil
		}
		rv, found := c.revalidate(ctx, pool, owner, v)
		return rv, found, nil
	}
	c.calls.Add(1)
	return c.inner.Lookup(ctx, pool, owner)
}

// EnsurePool declares the pool to the wrapped authority when it accepts pool
// declarations, once per range: an unchanged declaration is not repeated
// every cycle.
func (c *Cache) EnsurePool(name string, lo, hi int) error {
	pe, ok := c.inner.(interface {
		EnsurePool(name string, lo, hi int) error
	})
	if !ok {
		return nil
	}
	c.mu.Lock()
	r, done := c.pools[name]
	c.mu.Unlock()
	if done && r == [2]int{lo, hi} {
		return nil
	}
	c.calls.Add(1)
	if err := pe.EnsurePool(name, lo, hi); err != nil {
		return err
	}
	c.mu.Lock()
	c.pools[name] = [2]int{lo, hi}
	c.mu.Unlock()
	return nil
}
