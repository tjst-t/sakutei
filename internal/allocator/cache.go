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
// authority, §19.7), so a value once allocated is cached with no expiry and
// invalidated only by this shard's own Release.  The steady-state cycle then
// resolves every request from the cache: zero calls to the allocator, and the
// allocator being down stops only new allocations (§19.9, §22.4).
//
// The cache is S3 (memory only): losing it on restart costs one Allocate per
// request, never an incident.  It holds facts this shard established, not
// observations of reality, so observation state-lessness (§3) is untouched.
//
// Reservations (Reserve) are never cached: they expire unless confirmed.
type Cache struct {
	inner allocation.AllocationAuthority

	mu     sync.Mutex
	values map[cacheKey]string
	pools  map[string][2]int // pools declared through EnsurePool → range

	calls atomic.Int64
}

type cacheKey struct{ pool, owner string }

var _ allocation.AllocationAuthority = (*Cache)(nil)

// NewCache wraps inner with the Lookup cache.
func NewCache(inner allocation.AllocationAuthority) *Cache {
	return &Cache{inner: inner, values: map[cacheKey]string{}, pools: map[string][2]int{}}
}

// InnerCalls returns how many calls reached the wrapped authority — the
// allocator QPS this shard generates (§19.6: proportional to churn, not to
// the fleet size).
func (c *Cache) InnerCalls() int64 { return c.calls.Load() }

func (c *Cache) get(pool, owner string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.values[cacheKey{pool, owner}]
	return v, ok
}

func (c *Cache) put(pool, owner, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values[cacheKey{pool, owner}] = value
}

// Allocate implements allocation.AllocationAuthority: a cache hit never
// reaches the allocator.
func (c *Cache) Allocate(ctx context.Context, pool, owner string) (string, error) {
	if v, ok := c.get(pool, owner); ok {
		return v, nil
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
// when possible.  A miss is passed through and not cached (the value may be a
// reservation that can still expire).
func (c *Cache) Lookup(ctx context.Context, pool, owner string) (string, bool, error) {
	if v, ok := c.get(pool, owner); ok {
		return v, true, nil
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
