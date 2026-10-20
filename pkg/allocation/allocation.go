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
)
