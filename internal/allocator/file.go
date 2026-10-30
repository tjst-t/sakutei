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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v2/pkg/allocation"
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
	// Block is the block the value was taken from (#206); "" for a pool
	// without blocks.
	Block string `json:"block,omitempty"`
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
	// Blocks are the pool's blocks (#206), name → inclusive range.
	Blocks map[string][2]int `json:"blocks,omitempty"`
	// NoReuse: values go upwards only (`reuse: false`, #206).  Last is the
	// last value handed out per block ("" for the whole pool) — the high
	// water mark the next value is taken above — and Retired the values
	// released since, which never come back (released at).
	NoReuse bool                 `json:"no_reuse,omitempty"`
	Last    map[string]int       `json:"last,omitempty"`
	Retired map[string]time.Time `json:"retired,omitempty"`
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

// EnsurePool declares pool name with the inclusive range [lo, hi] and no
// blocks (DeclarePool).
func (a *FileAuthority) EnsurePool(name string, lo, hi int) error {
	return a.DeclarePool(name, allocation.PoolSpec{Lo: lo, Hi: hi})
}

// DeclarePool declares pool name as spec says, creating it or changing it.
// A change that would leave an existing allocation outside the range, or
// outside the block it was taken from, is refused (the value is in use).
// Turning reuse off starts the last value of each block at the highest
// value it holds or cools down: nothing below that is handed out again.
func (a *FileAuthority) DeclarePool(name string, spec allocation.PoolSpec) error {
	return a.update(func(s *store) error {
		p, ok := s.Pools[name]
		if !ok {
			p = newPool()
		}
		if err := p.declare(name, spec); err != nil {
			return err
		}
		s.Pools[name] = p
		return nil
	})
}

func newPool() *pool {
	return &pool{Allocations: map[string]entry{}, Cooldown: map[string]time.Time{}}
}

// declare sets the pool's spec, refusing one that would exclude a value in
// use.
func (p *pool) declare(name string, spec allocation.PoolSpec) error {
	blocks := map[string][2]int{}
	for _, b := range spec.Blocks {
		blocks[b.Name] = [2]int{b.Lo, b.Hi}
	}
	for owner, e := range p.Allocations {
		if e.Value < spec.Lo || e.Value > spec.Hi {
			return fmt.Errorf("allocator: pool %q: new range %d-%d would exclude %d, allocated to %q", name, spec.Lo, spec.Hi, e.Value, owner)
		}
		if e.Block == "" {
			continue
		}
		r, ok := blocks[e.Block]
		if !ok {
			return fmt.Errorf("allocator: pool %q: block %q would go, but %d of it is allocated to %q", name, e.Block, e.Value, owner)
		}
		if e.Value < r[0] || e.Value > r[1] {
			return fmt.Errorf("allocator: pool %q: block %q's new range %d-%d would exclude %d, allocated to %q", name, e.Block, r[0], r[1], e.Value, owner)
		}
	}
	p.Lo, p.Hi = spec.Lo, spec.Hi
	p.Blocks = nil
	if len(blocks) > 0 {
		p.Blocks = blocks
	}
	if spec.NoReuse && !p.NoReuse {
		// from now on upwards only: start each block above what it holds or
		// cools down (the cooldown ends, the value still stays behind).
		if p.Last == nil {
			p.Last = map[string]int{}
		}
		held := make([]int, 0, len(p.Allocations)+len(p.Cooldown))
		for _, e := range p.Allocations {
			held = append(held, e.Value)
		}
		for v := range p.Cooldown {
			if n, err := strconv.Atoi(v); err == nil {
				held = append(held, n)
			}
		}
		for _, v := range held {
			b := p.blockOf(v)
			if last, ok := p.Last[b]; !ok || v > last {
				p.Last[b] = v
			}
		}
	}
	p.NoReuse = spec.NoReuse
	return nil
}

// blockOf returns the block holding v ("" when the pool has no blocks or
// none holds v).
func (p *pool) blockOf(v int) string {
	for name, r := range p.Blocks {
		if v >= r[0] && v <= r[1] {
			return name
		}
	}
	return ""
}

// rangeOf returns the range a new value of block is taken from: the block's,
// or the pool's when block is "".  A pool with blocks needs one named.
func (p *pool) rangeOf(poolName, block string) (int, int, error) {
	if block == "" {
		if len(p.Blocks) > 0 {
			return 0, 0, fmt.Errorf("%w: pool %q is split into blocks; a request names one (#206)", allocation.ErrUnknownPool, poolName)
		}
		return p.Lo, p.Hi, nil
	}
	r, ok := p.Blocks[block]
	if !ok {
		return 0, 0, fmt.Errorf("%w: pool %q has no block %q", allocation.ErrUnknownPool, poolName, block)
	}
	return r[0], r[1], nil
}

// Allocate implements allocation.AllocationAuthority.
//
// poolName may name a block of the pool ("pool/block",
// allocation.BlockRef, #206): a new value is then taken from the block alone.
// An owner already holding a value keeps it (idempotent), whichever block it
// names now.
func (a *FileAuthority) Allocate(_ context.Context, poolName, owner string) (string, error) {
	var value int
	err := a.update(func(s *store) error {
		p, block, err := s.blockPool(poolName)
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
		v, err := p.take(poolName, block, now)
		if err != nil {
			return err
		}
		p.Allocations[owner] = entry{Value: v, State: stateAllocated, Block: block}
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
		p, block, err := s.blockPool(poolName)
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
		v, err := p.take(poolName, block, now)
		if err != nil {
			return err
		}
		exp := now.Add(ttl)
		p.Allocations[owner] = entry{Value: v, State: stateReserved, ExpiresAt: &exp, Block: block}
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
// cooldown and is not handed out again before it ends — or, in a pool with
// reuse: false, is retired and never handed out again (#206).  Releasing an
// owner that holds nothing is a no-op.
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
		if p.NoReuse {
			if p.Retired == nil {
				p.Retired = map[string]time.Time{}
			}
			p.Retired[value] = a.now()
			return nil
		}
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
// out: neither allocated, reserved nor cooling down (nor, with reuse: false,
// at or below the last value handed out).  The range is the one the intent
// declares (EnsurePool sets it before the next Allocate), so a pool not
// created yet has the whole range free.  poolName may name a block
// ("pool/block", #206), whose range lo-hi then is.  It never writes (plan /
// status, #111).
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
	base, block := allocation.SplitBlockRef(poolName)
	p, ok := s.Pools[base]
	if !ok {
		return hi - lo + 1, nil
	}
	now := a.now()
	p.expire(now)
	used := p.used(now)
	n := 0
	for v := p.start(block, lo); v <= hi; v++ {
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

// pool returns the pool name refers to.  name may name a block
// ("pool/block", #206): owners are unique across the pool's blocks, so
// Lookup, Confirm and Release need no more than the pool.
func (s *store) pool(name string) (*pool, error) {
	p, _, err := s.blockPool(name)
	return p, err
}

// blockPool returns the pool and the block name refers to.  A block the pool
// does not declare is an unknown pool.
func (s *store) blockPool(name string) (*pool, string, error) {
	base, block := allocation.SplitBlockRef(name)
	p, ok := s.Pools[base]
	if !ok {
		return nil, "", fmt.Errorf("%w: %q", allocation.ErrUnknownPool, base)
	}
	if block != "" {
		if _, ok := p.Blocks[block]; !ok {
			return nil, "", fmt.Errorf("%w: pool %q has no block %q", allocation.ErrUnknownPool, base, block)
		}
	}
	return p, block, nil
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

// take hands out a new value of block (the whole pool when ""), recording it
// as the block's last value in a pool with reuse: false.
func (p *pool) take(poolName, block string, now time.Time) (int, error) {
	v, err := p.free(poolName, block, now)
	if err != nil {
		return 0, err
	}
	if p.NoReuse {
		if p.Last == nil {
			p.Last = map[string]int{}
		}
		p.Last[block] = v
	}
	return v, nil
}

// free returns the lowest value of block's range (the pool's when "") that
// is neither allocated, reserved nor cooling down — and, with reuse: false,
// above the block's last value.  Lowest-first keeps allocation
// deterministic.  An exhausted pool or block says why.
func (p *pool) free(poolName, block string, now time.Time) (int, error) {
	lo, hi, err := p.rangeOf(poolName, block)
	if err != nil {
		return 0, err
	}
	start := p.start(block, lo)
	used := p.used(now)
	for v := start; v <= hi; v++ {
		if !used[v] {
			return v, nil
		}
	}
	where := fmt.Sprintf("pool %q", poolName)
	if block != "" {
		base, _ := allocation.SplitBlockRef(poolName)
		where = fmt.Sprintf("block %q of pool %q", block, base)
	}
	if p.NoReuse {
		last := "none yet"
		if l, ok := p.Last[block]; ok {
			last = strconv.Itoa(l)
		}
		return 0, fmt.Errorf("%w: %s (range %d-%d): the last value handed out is %s, and with reuse: false no value at or below it is handed out again; widen the range or declare a new block",
			allocation.ErrPoolExhausted, where, lo, hi, last)
	}
	return 0, fmt.Errorf("%w: %s (range %d-%d): every value is allocated, reserved or cooling down", allocation.ErrPoolExhausted, where, lo, hi)
}

// start is the lowest value block may still hand out: lo, or above the
// block's last value with reuse: false.
func (p *pool) start(block string, lo int) int {
	if !p.NoReuse {
		return lo
	}
	if last, ok := p.Last[block]; ok && last+1 > lo {
		return last + 1
	}
	return lo
}

// used is the set of values allocated, reserved, cooling down or retired at
// now.
func (p *pool) used(now time.Time) map[int]bool {
	used := make(map[int]bool, len(p.Allocations)+len(p.Cooldown)+len(p.Retired))
	for _, e := range p.Allocations {
		used[e.Value] = true
	}
	for v := range p.Retired {
		if n, err := strconv.Atoi(v); err == nil {
			used[n] = true
		}
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

// ImportEntry is one allocation handed to Import: owner holds value.
type ImportEntry struct {
	Owner string `json:"owner"`
	Value int    `json:"value"`
}

// ErrImportConflict is returned (wrapped) by Import when an entry disagrees
// with a record the store already has.
var ErrImportConflict = errors.New("allocator: import conflicts with the store")

// Import rebuilds pool's allocations from entries — the pool's write-back
// file, the source of truth (`sakutei allocator import`, #157, §19.7).  The
// pool is declared with the range lo-hi first (created if missing).  An entry
// the store already holds as it is is left alone; an entry that disagrees
// with the store — the owner holds another value, the value is held by
// another owner or cools down (it was released), or it is outside the range —
// is a conflict.  On any conflict nothing is written and every conflict is
// reported: the store is never made to agree by guessing which side is right.
// It returns how many entries were added.
func (a *FileAuthority) Import(ctx context.Context, poolName string, lo, hi int, entries []ImportEntry) (int, error) {
	return a.ImportPool(ctx, poolName, allocation.PoolSpec{Lo: lo, Hi: hi}, entries, nil)
}

// ImportPool is Import of a pool declared as spec (blocks, reuse: false,
// #206).  An entry's block is the one its value lies in; a value in no block
// of a pool with blocks is a conflict, as is a value the store has retired
// (released with reuse: false).  last is the file's last value per block
// ("" for the whole pool): the store's last value becomes the highest of its
// own, the file's and the values imported, so a store rebuilt from the file
// never hands out a released value again.  A last value of a block the pool
// does not declare is a conflict.
func (a *FileAuthority) ImportPool(_ context.Context, poolName string, spec allocation.PoolSpec, entries []ImportEntry, last map[string]int) (int, error) {
	lo, hi := spec.Lo, spec.Hi
	added := 0
	err := a.update(func(s *store) error {
		p, ok := s.Pools[poolName]
		if !ok {
			p = newPool()
		}
		now := a.now()
		p.expire(now)
		holder := make(map[int]string, len(p.Allocations))
		for owner, e := range p.Allocations {
			holder[e.Value] = owner
		}
		var conflicts []string
		var add []ImportEntry
		for _, in := range entries {
			if in.Value < lo || in.Value > hi {
				conflicts = append(conflicts, fmt.Sprintf("%s: value %d is outside the range %d-%d", in.Owner, in.Value, lo, hi))
				continue
			}
			if _, ok := spec.BlockOf(in.Value); !ok {
				conflicts = append(conflicts, fmt.Sprintf("%s: value %d lies in no block of the pool", in.Owner, in.Value))
				continue
			}
			if e, ok := p.Allocations[in.Owner]; ok {
				if e.Value != in.Value {
					conflicts = append(conflicts, fmt.Sprintf("%s: the file says %d, the store says %d", in.Owner, in.Value, e.Value))
				}
				continue
			}
			if other, ok := holder[in.Value]; ok {
				conflicts = append(conflicts, fmt.Sprintf("%s: value %d is held by %s in the store", in.Owner, in.Value, other))
				continue
			}
			if until, ok := p.Cooldown[strconv.Itoa(in.Value)]; ok {
				conflicts = append(conflicts, fmt.Sprintf("%s: value %d was released and cools down until %s in the store", in.Owner, in.Value, until.UTC().Format(time.RFC3339)))
				continue
			}
			if at, ok := p.Retired[strconv.Itoa(in.Value)]; ok {
				conflicts = append(conflicts, fmt.Sprintf("%s: value %d was released at %s and is retired in the store (reuse: false)", in.Owner, in.Value, at.UTC().Format(time.RFC3339)))
				continue
			}
			add = append(add, in)
		}
		blocks := map[string]bool{}
		for _, b := range spec.Blocks {
			blocks[b.Name] = true
		}
		for b := range last {
			if (b == "" && len(blocks) > 0) || (b != "" && !blocks[b]) {
				conflicts = append(conflicts, fmt.Sprintf("last value %d of block %q: the pool declares no such block", last[b], b))
			}
		}
		if len(conflicts) > 0 {
			sort.Strings(conflicts)
			return fmt.Errorf("%w: pool %q: %s", ErrImportConflict, poolName, strings.Join(conflicts, "; "))
		}
		if err := p.declare(poolName, spec); err != nil {
			return err
		}
		for _, in := range add {
			b, _ := spec.BlockOf(in.Value)
			p.Allocations[in.Owner] = entry{Value: in.Value, State: stateAllocated, Block: b}
		}
		if p.NoReuse {
			if p.Last == nil {
				p.Last = map[string]int{}
			}
			raise := func(b string, v int) {
				if cur, ok := p.Last[b]; !ok || v > cur {
					p.Last[b] = v
				}
			}
			for b, v := range last {
				raise(b, v)
			}
			for _, e := range p.Allocations {
				raise(e.Block, e.Value)
			}
		}
		s.Pools[poolName] = p
		added = len(add)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return added, nil
}

// The Record states besides allocated and reserved.  None has an owner.
const (
	// StateCooldown: a released value cooling down (§19.5 (b)).
	StateCooldown = "cooldown"
	// StateRetired: a value released in a pool with reuse: false, never
	// handed out again (#206).  Until is when it was released.
	StateRetired = "retired"
	// StateLast: the last value a block of a pool with reuse: false handed
	// out (#206) — the next one is taken above it.
	StateLast = "last"
)

// Record is one row of List: a value in use (allocated or reserved, with its
// owner), cooling down or retired, or the last value of a block.  Until is
// when a reservation expires or a cooldown ends (when a retired value was
// released).  Block is the block the value lies in (#206).
type Record struct {
	Pool  string     `json:"pool"`
	Block string     `json:"block,omitempty"`
	Owner string     `json:"owner,omitempty"`
	Value int        `json:"value"`
	State string     `json:"state"`
	Until *time.Time `json:"until,omitempty"`
}

// List returns the records of poolName (every pool when empty; a block ref
// gives its pool's), sorted by pool and value.  Expired reservations and
// ended cooldowns are left out.  It never writes (`sakutei allocator list`,
// #116).
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
		base, _ := allocation.SplitBlockRef(poolName)
		names = append(names, base)
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
			rows = append(rows, Record{Pool: n, Block: e.Block, Owner: owner, Value: e.Value, State: e.State, Until: e.ExpiresAt})
		}
		for v, at := range p.Retired {
			n2, err := strconv.Atoi(v)
			if err != nil {
				continue
			}
			u := at
			rows = append(rows, Record{Pool: n, Block: p.blockOf(n2), Value: n2, State: StateRetired, Until: &u})
		}
		if p.NoReuse {
			for b, v := range p.Last {
				rows = append(rows, Record{Pool: n, Block: b, Value: v, State: StateLast})
			}
		}
		for v, until := range p.Cooldown {
			n2, err := strconv.Atoi(v)
			if err != nil || !now.Before(until) {
				continue
			}
			u := until
			rows = append(rows, Record{Pool: n, Block: p.blockOf(n2), Value: n2, State: StateCooldown, Until: &u})
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Value != rows[j].Value {
				return rows[i].Value < rows[j].Value
			}
			if rows[i].State != rows[j].State {
				return rows[i].State < rows[j].State
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
