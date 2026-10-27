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
// Package allocator is the v1b minimal allocation authority (§19.9): a JSON
// file guarded by a lock, behind the allocation.AllocationAuthority seam.
//
// The design names SQLite + BEGIN IMMEDIATE; this implementation keeps the
// repository stdlib-only (decided 2026-10) and provides the same guarantees
// for a single host: every operation takes an exclusive lock (in-process
// mutex + flock on "<store>.lock"), reads the store, mutates it, and writes it
// back atomically (temp file + fsync + rename), so concurrent Allocate calls —
// even from different processes — cannot hand out the same value twice.
// A SQLite authority can replace it later behind the same interface (#36).
package allocator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/pkg/allocation"
)

// DefaultCooldown is how long a released value stays out of circulation
// before it can be handed out again (§19.5 (b), like a DHCP lease grace).
const DefaultCooldown = 24 * time.Hour

const (
	stateAllocated = "allocated"
	stateReserved  = "reserved"
)

type entry struct {
	Value     int        `json:"value"`
	State     string     `json:"state"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"` // reservations only
}

// expired reports whether e is a reservation whose TTL has passed.
func (e entry) expired(now time.Time) bool {
	return e.State == stateReserved && e.ExpiresAt != nil && !now.Before(*e.ExpiresAt)
}

type pool struct {
	Lo          int                  `json:"lo"`
	Hi          int                  `json:"hi"`
	Allocations map[string]entry     `json:"allocations"`
	Cooldown    map[string]time.Time `json:"cooldown"` // value → usable again at
}

type store struct {
	Version int `json:"version"`
	// Generation is the fencing token of the active authority (§19.8).  It
	// is 0 until an HA instance promotes itself; each promotion increments
	// it, so every older instance is fenced from then on.
	Generation uint64           `json:"generation,omitempty"`
	Pools      map[string]*pool `json:"pools"`
}

// FileAuthority implements allocation.AllocationAuthority on a JSON file.
type FileAuthority struct {
	path string
	mu   sync.Mutex
	// gen is the fencing generation this instance holds (0 = never
	// promoted: the single-instance mode of `--alloc-store`).  A write is
	// refused unless it equals the store's generation (§19.8).
	gen uint64

	// Cooldown after Release; DefaultCooldown when zero.
	Cooldown time.Duration
	// Now is the clock; time.Now when nil (tests inject one).
	Now func() time.Time
}

var _ allocation.AllocationAuthority = (*FileAuthority)(nil)

// New returns an authority backed by the store file at path.  The file is
// created on the first write.
func New(path string) *FileAuthority {
	return &FileAuthority{path: path}
}

func (a *FileAuthority) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *FileAuthority) cooldown() time.Duration {
	if a.Cooldown > 0 {
		return a.Cooldown
	}
	return DefaultCooldown
}

// Promote makes this instance the active authority (§19.8 active/standby):
// it increments the store's fencing generation and takes it, so the previous
// active — should it still be alive — is fenced from its next write on.
// Called by a standby when it takes over.
func (a *FileAuthority) Promote() (uint64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	unlock, err := lockFile(a.path + ".lock")
	if err != nil {
		return 0, err
	}
	defer unlock()
	s, err := a.load()
	if err != nil {
		return 0, err
	}
	s.Generation++
	if err := a.save(s); err != nil {
		return 0, err
	}
	a.gen = s.Generation
	return a.gen, nil
}

// Generation returns the fencing generation this instance holds.
func (a *FileAuthority) Generation() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.gen
}

// CheckActive returns allocation.ErrFenced when another instance has been
// promoted since this one (the store's generation moved on).
func (a *FileAuthority) CheckActive() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	unlock, err := lockFile(a.path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	s, err := a.load()
	if err != nil {
		return err
	}
	return a.fence(s)
}

func (a *FileAuthority) fence(s *store) error {
	if s.Generation != a.gen {
		return fmt.Errorf("%w: holds generation %d, store is at %d", allocation.ErrFenced, a.gen, s.Generation)
	}
	return nil
}

// EnsurePool declares pool name with the inclusive range [lo, hi], creating
// it or changing its range.  A range change that would leave an existing
// allocation outside the range is refused (the value is in use).
func (a *FileAuthority) EnsurePool(name string, lo, hi int) error {
	return a.update(func(s *store) error {
		p, ok := s.Pools[name]
		if !ok {
			s.Pools[name] = &pool{Lo: lo, Hi: hi, Allocations: map[string]entry{}, Cooldown: map[string]time.Time{}}
			return nil
		}
		for owner, e := range p.Allocations {
			if e.Value < lo || e.Value > hi {
				return fmt.Errorf("allocator: pool %q: new range %d-%d would exclude %d, allocated to %q", name, lo, hi, e.Value, owner)
			}
		}
		p.Lo, p.Hi = lo, hi
		return nil
	})
}

// Allocate implements allocation.AllocationAuthority.
func (a *FileAuthority) Allocate(_ context.Context, poolName, owner string) (string, error) {
	var value int
	err := a.update(func(s *store) error {
		p, err := s.pool(poolName)
		if err != nil {
			return err
		}
		now := a.now()
		p.expire(now)
		if e, ok := p.Allocations[owner]; ok {
			e.State, e.ExpiresAt = stateAllocated, nil
			p.Allocations[owner] = e
			value = e.Value
			return nil
		}
		v, err := p.free(now)
		if err != nil {
			return fmt.Errorf("%w: %q", err, poolName)
		}
		p.Allocations[owner] = entry{Value: v, State: stateAllocated}
		value = v
		return nil
	})
	if err != nil {
		return "", err
	}
	return strconv.Itoa(value), nil
}

// Reserve implements allocation.AllocationAuthority.
func (a *FileAuthority) Reserve(_ context.Context, poolName, owner string, ttl time.Duration) (string, error) {
	var value int
	err := a.update(func(s *store) error {
		p, err := s.pool(poolName)
		if err != nil {
			return err
		}
		now := a.now()
		p.expire(now)
		if e, ok := p.Allocations[owner]; ok {
			if e.State == stateReserved {
				exp := now.Add(ttl)
				e.ExpiresAt = &exp
				p.Allocations[owner] = e
			}
			value = e.Value
			return nil
		}
		v, err := p.free(now)
		if err != nil {
			return fmt.Errorf("%w: %q", err, poolName)
		}
		exp := now.Add(ttl)
		p.Allocations[owner] = entry{Value: v, State: stateReserved, ExpiresAt: &exp}
		value = v
		return nil
	})
	if err != nil {
		return "", err
	}
	return strconv.Itoa(value), nil
}

// Confirm implements allocation.AllocationAuthority.
func (a *FileAuthority) Confirm(_ context.Context, poolName, owner string) error {
	return a.update(func(s *store) error {
		p, err := s.pool(poolName)
		if err != nil {
			return err
		}
		p.expire(a.now())
		e, ok := p.Allocations[owner]
		if !ok {
			return fmt.Errorf("%w: pool %q owner %q", allocation.ErrNoReservation, poolName, owner)
		}
		e.State, e.ExpiresAt = stateAllocated, nil
		p.Allocations[owner] = e
		return nil
	})
}

// Release implements allocation.AllocationAuthority.  The value enters the
// cooldown and is not handed out again before it ends.  Releasing an owner
// that holds nothing is a no-op.
func (a *FileAuthority) Release(ctx context.Context, poolName, owner string) error {
	_, _, err := a.ReleaseOwner(ctx, poolName, owner)
	return err
}

// ReleaseOwner is Release that also reports what it released: the value and
// whether owner held one at all (`sakutei allocator release`, #116, tells a
// human when there was nothing to release).
func (a *FileAuthority) ReleaseOwner(_ context.Context, poolName, owner string) (string, bool, error) {
	var value string
	var released bool
	err := a.update(func(s *store) error {
		p, err := s.pool(poolName)
		if err != nil {
			return err
		}
		e, ok := p.Allocations[owner]
		if !ok {
			return nil
		}
		delete(p.Allocations, owner)
		value, released = strconv.Itoa(e.Value), true
		p.Cooldown[value] = a.now().Add(a.cooldown())
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return value, released, nil
}

// Lookup implements allocation.AllocationAuthority.  It never writes.
func (a *FileAuthority) Lookup(_ context.Context, poolName, owner string) (string, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	unlock, err := lockFile(a.path + ".lock")
	if err != nil {
		return "", false, err
	}
	defer unlock()
	s, err := a.load()
	if err != nil {
		return "", false, err
	}
	p, err := s.pool(poolName)
	if err != nil {
		return "", false, err
	}
	e, ok := p.Allocations[owner]
	if !ok || e.expired(a.now()) {
		return "", false, nil
	}
	return strconv.Itoa(e.Value), true, nil
}

// FreeCount reports how many values of the range lo-hi pool could still hand
// out: neither allocated, reserved nor cooling down.  The range is the one the
// intent declares (EnsurePool sets it before the next Allocate), so a pool not
// created yet has the whole range free.  It never writes (plan / status, #111).
func (a *FileAuthority) FreeCount(_ context.Context, poolName string, lo, hi int) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	unlock, err := lockFile(a.path + ".lock")
	if err != nil {
		return 0, err
	}
	defer unlock()
	s, err := a.load()
	if err != nil {
		return 0, err
	}
	p, ok := s.Pools[poolName]
	if !ok {
		return hi - lo + 1, nil
	}
	now := a.now()
	p.expire(now)
	used := p.used(now)
	n := 0
	for v := lo; v <= hi; v++ {
		if !used[v] {
			n++
		}
	}
	return n, nil
}

// update runs fn on the store under the exclusive lock and writes the result.
// A fenced instance (stale generation) writes nothing.
func (a *FileAuthority) update(fn func(*store) error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	unlock, err := lockFile(a.path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	s, err := a.load()
	if err != nil {
		return err
	}
	if err := a.fence(s); err != nil {
		return err
	}
	if err := fn(s); err != nil {
		return err
	}
	return a.save(s)
}

func (a *FileAuthority) load() (*store, error) {
	data, err := os.ReadFile(a.path)
	if os.IsNotExist(err) {
		return &store{Version: 1, Pools: map[string]*pool{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("allocator: read store: %w", err)
	}
	var s store
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("allocator: parse store %s: %w", a.path, err)
	}
	if s.Pools == nil {
		s.Pools = map[string]*pool{}
	}
	for _, p := range s.Pools {
		if p.Allocations == nil {
			p.Allocations = map[string]entry{}
		}
		if p.Cooldown == nil {
			p.Cooldown = map[string]time.Time{}
		}
	}
	return &s, nil
}

// save writes the store atomically and durably: losing an allocation record
// is an incident (§19.7), so the temp file is fsynced before the rename.
func (a *FileAuthority) save(s *store) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(a.path), "."+filepath.Base(a.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("allocator: write store: %w", err)
	}
	name := tmp.Name()
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(name, a.path)
	}
	if err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("allocator: write store: %w", err)
	}
	return nil
}

func (s *store) pool(name string) (*pool, error) {
	p, ok := s.Pools[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", allocation.ErrUnknownPool, name)
	}
	return p, nil
}

// expire drops reservations whose TTL has passed and cooldowns that ended.
// An expired reservation simply frees its value (it was never really used —
// that is the point of Reserve, §19.4).
func (p *pool) expire(now time.Time) {
	for owner, e := range p.Allocations {
		if e.expired(now) {
			delete(p.Allocations, owner)
		}
	}
	for v, until := range p.Cooldown {
		if !now.Before(until) {
			delete(p.Cooldown, v)
		}
	}
}

// free returns the lowest value in range that is neither allocated, reserved
// nor cooling down.  Lowest-first keeps allocation deterministic.
func (p *pool) free(now time.Time) (int, error) {
	used := p.used(now)
	for v := p.Lo; v <= p.Hi; v++ {
		if !used[v] {
			return v, nil
		}
	}
	return 0, allocation.ErrPoolExhausted
}

// used is the set of values allocated, reserved or cooling down at now.
func (p *pool) used(now time.Time) map[int]bool {
	used := make(map[int]bool, len(p.Allocations)+len(p.Cooldown))
	for _, e := range p.Allocations {
		used[e.Value] = true
	}
	for v, until := range p.Cooldown {
		if now.Before(until) {
			if n, err := strconv.Atoi(v); err == nil {
				used[n] = true
			}
		}
	}
	return used
}

// Record states besides stateAllocated and stateReserved: a released value
// cooling down (§19.5 (b)).  It has no owner any more.
const stateCooldown = "cooldown"

// Record is one row of List: a value in use (allocated or reserved, with its
// owner) or cooling down.  Until is when a reservation expires or a cooldown
// ends.
type Record struct {
	Pool  string     `json:"pool"`
	Owner string     `json:"owner,omitempty"`
	Value int        `json:"value"`
	State string     `json:"state"`
	Until *time.Time `json:"until,omitempty"`
}

// List returns the records of poolName (every pool when empty), sorted by
// pool and value.  Expired reservations and ended cooldowns are left out.  It
// never writes (`sakutei allocator list`, #116).
func (a *FileAuthority) List(_ context.Context, poolName string) ([]Record, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	unlock, err := lockFile(a.path + ".lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	s, err := a.load()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(s.Pools))
	if poolName != "" {
		if _, err := s.pool(poolName); err != nil {
			return nil, err
		}
		names = append(names, poolName)
	} else {
		for n := range s.Pools {
			names = append(names, n)
		}
		sort.Strings(names)
	}
	now := a.now()
	out := []Record{}
	for _, n := range names {
		p := s.Pools[n]
		var rows []Record
		for owner, e := range p.Allocations {
			if e.expired(now) {
				continue
			}
			rows = append(rows, Record{Pool: n, Owner: owner, Value: e.Value, State: e.State, Until: e.ExpiresAt})
		}
		for v, until := range p.Cooldown {
			n2, err := strconv.Atoi(v)
			if err != nil || !now.Before(until) {
				continue
			}
			u := until
			rows = append(rows, Record{Pool: n, Value: n2, State: stateCooldown, Until: &u})
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Value != rows[j].Value {
				return rows[i].Value < rows[j].Value
			}
			return rows[i].Owner < rows[j].Owner
		})
		out = append(out, rows...)
	}
	return out, nil
}

// Owners returns pool's owners sorted, for diagnostics and tests.
func (a *FileAuthority) Owners(poolName string) ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	unlock, err := lockFile(a.path + ".lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	s, err := a.load()
	if err != nil {
		return nil, err
	}
	p, err := s.pool(poolName)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(p.Allocations))
	for o := range p.Allocations {
		out = append(out, o)
	}
	sort.Strings(out)
	return out, nil
}
