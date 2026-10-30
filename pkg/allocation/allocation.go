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

// Package allocation defines AllocationAuthority, the seam to the allocation
// authority (component: allocator, §19).  Pool allocations (VLAN IDs, IPs,
// ASNs) are side-effecting writes that are not idempotent if done naively, so
// they are split into an idempotent REQUEST (intent: "one VLAN from pool X")
// and an allocation FACT persisted by the authority ("app-net → VLAN 573").
// The convergence engine keeps no allocation state of its own (§3 principle 4).
//
// Implementations are interchangeable behind this interface: the v1b file
// authority (internal/allocator), and later SQLite, NetBox, Infoblox,
// phpIPAM (§19.9).
package allocation

import (
	"context"
	"errors"
	"strings"
	"time"
)

// AllocationAuthority is the five-operation seam of §19.3.
//
// owner identifies the requester stably — Sakutei uses
// "<provider>/<local id>#<field>", deliberately without the scope, because the
// scope moves on re-sharding while the local id does not (§4.2).
type AllocationAuthority interface {
	// Allocate returns the value allocated to owner in pool, allocating one
	// if owner has none.  Idempotent: the same owner always gets the same
	// value until Release.  A pending Reserve for owner is promoted.
	Allocate(ctx context.Context, pool, owner string) (value string, err error)

	// Reserve tentatively allocates a value to owner for ttl (PR flow,
	// §19.4).  It expires unless Confirm is called.  Idempotent while live.
	Reserve(ctx context.Context, pool, owner string, ttl time.Duration) (value string, err error)

	// Confirm promotes owner's live reservation to a real allocation.
	Confirm(ctx context.Context, pool, owner string) error

	// Release returns owner's value to the pool after a cooldown (§19.5).
	// Callers MUST call it only after confirming, via List, that the
	// downstream resources are gone from reality — never merely because the
	// owner vanished from desired.
	Release(ctx context.Context, pool, owner string) error

	// Lookup returns owner's current value (allocated, or live reservation)
	// without allocating.  ok is false when owner has none.
	Lookup(ctx context.Context, pool, owner string) (value string, ok bool, err error)
}

var (
	// ErrUnknownPool: the pool is not known to the authority.
	ErrUnknownPool = errors.New("allocation: unknown pool")
	// ErrPoolExhausted: no free value is left in the pool.
	ErrPoolExhausted = errors.New("allocation: pool exhausted")
	// ErrNoReservation: Confirm found no live reservation for the owner.
	ErrNoReservation = errors.New("allocation: no live reservation")
	// ErrUnavailable: the authority cannot be reached (stopped, failing over,
	// network).  Render degrades the requesting resource to
	// waiting-allocation instead of failing the cycle (§19.9).
	ErrUnavailable = errors.New("allocation: authority unavailable")
	// ErrFenced: this authority instance has been superseded by a promoted
	// standby (its fencing generation is stale, §19.8); it must not write.
	ErrFenced = errors.New("allocation: authority fenced (a newer generation is active)")
	// ErrWriteBackHeld: the request's value is held back by the pool's
	// write-back file (`write_back: true`, §19.7): the file and the store
	// disagree, the store lacks an allocation the file holds, or the value
	// awaits review before it appears in the file.  Like ErrUnavailable it
	// makes only the requesting resource waiting-allocation — its Apply is
	// stopped and reported, never resolved by preferring either side.
	ErrWriteBackHeld = errors.New("allocation: held by the write-back file")
)

// PoolSpec is a pool as the intent declares it (`pools:`, §19.8): the
// inclusive range Lo-Hi, the blocks it is split into (#206) and whether a
// value handed out is never handed out again (`reuse: false`, #206).
type PoolSpec struct {
	Lo     int     `json:"lo"`
	Hi     int     `json:"hi"`
	Blocks []Block `json:"blocks,omitempty"`
	// NoReuse makes the pool hand out values upwards only: the authority
	// records the last value handed out (per block) and never goes back to
	// it or below, so a released value is never handed out again.  The
	// cooldown of a release (§19.5 (b)) has no meaning then.
	NoReuse bool `json:"no_reuse,omitempty"`
}

// Block is a named sub-range of a pool (`blocks: [{ name, range }]`, #206),
// e.g. the user IDs of one use.  A request naming the block gets a value
// from it alone; blocks of a pool are disjoint and lie inside its range.
type Block struct {
	Name string `json:"name"`
	Lo   int    `json:"lo"`
	Hi   int    `json:"hi"`
}

// BlockOf returns the block of s holding v ("" and true when s has no
// blocks; false when it has some and none holds v).
func (s PoolSpec) BlockOf(v int) (string, bool) {
	if len(s.Blocks) == 0 {
		return "", v >= s.Lo && v <= s.Hi
	}
	for _, b := range s.Blocks {
		if v >= b.Lo && v <= b.Hi {
			return b.Name, true
		}
	}
	return "", false
}

// BlockSeparator joins a pool and a block into the pool argument of the
// AllocationAuthority methods: a request from block "staff" of pool
// "user-ids" asks for "user-ids/staff" (#206).  Pool and block names never
// hold it.
const BlockSeparator = "/"

// BlockRef is the pool argument for block of pool (pool itself when block
// is empty).
func BlockRef(pool, block string) string {
	if block == "" {
		return pool
	}
	return pool + BlockSeparator + block
}

// SplitBlockRef is BlockRef reversed.
func SplitBlockRef(ref string) (pool, block string) {
	pool, block, _ = strings.Cut(ref, BlockSeparator)
	return pool, block
}
