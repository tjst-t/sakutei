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

// Package allocwriteback writes what the allocator hands out back to the
// intent side and reads it there as the source of truth (#157, §19.7).
//
// For a pool declared `write_back: true` the allocation file
// (<path>/<pool>.yaml, loader.AllocationFile) is the truth; the allocation
// store becomes its copy plus the working records (cooldowns, reservations).
// Authority wraps the store's AllocationAuthority:
//
//   - file and store agree → the value is used as it is;
//   - they disagree, or the store lacks what the file holds → the owner's
//     resource is held (allocation.ErrWriteBackHeld: waiting-allocation, its
//     Apply stopped and reported) — never resolved by preferring a side.
//     While the store lacks or contradicts any entry of the file, the pool
//     hands out no new value either (the store would think the file's
//     value free): `sakutei allocator import` rebuilds the store;
//   - the store hands out a value the file does not hold yet → commit / file
//     write it to the file before it is used; review commits it to a review
//     branch and holds the resource until people take it into the intent and
//     the value appears in the file.  The value stays allocated in the store
//     meanwhile, so it is not handed out to anyone else;
//   - a Release removes the owner from the file the same way.  Until review
//     takes the removal in, the file still holds an owner the store has
//     released (its value cools down): the pool is held as 解放の取り込み待ち
//     (ErrReleaseAwaitingReview), lifted by taking the removal in — not as a
//     lost store, which `sakutei allocator import` would rebuild (the import
//     would refuse the cooling value anyway, #167).
//   - a pool with reuse: false (#206) also writes back the last value it
//     handed out per block (the file's last:), which a release leaves as it
//     is.  While the store's last value is below the file's — the store was
//     lost or rebuilt from an older copy — the pool hands out nothing new:
//     it would hand out a released value again; `sakutei allocator import`
//     raises the store's last value to the file's.
//
// The decisions are taken from the file and the store every time — nothing
// of them is kept here — so one-shot runs and the daemon behave the same.
package allocwriteback

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/allocator"
	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/syncmode"
	"github.com/tjst-t/sakutei/v2/pkg/allocation"
)

// The reasons a request is held.  Each is wrapped together with
// allocation.ErrWriteBackHeld.
var (
	// ErrConflict: the file and the store give the owner different values.
	ErrConflict = errors.New("the write-back file and the store disagree")
	// ErrNotInStore: the file holds an allocation the store does not have.
	ErrNotInStore = errors.New("the store lacks an allocation the write-back file holds")
	// ErrAwaitingReview: the value is allocated and committed to the review
	// branch, and is not in the intent's file yet (書き戻しの取り込み待ち).
	ErrAwaitingReview = errors.New("書き戻しの取り込み待ち (the write-back awaits review)")
	// ErrReleaseAwaitingReview: the store released the owner (its value cools
	// down) and the file still holds it, because the removal is not taken in
	// yet (解放の取り込み待ち, #167).  Taking it in lifts the hold; import
	// does not (it refuses a value cooling down).
	ErrReleaseAwaitingReview = errors.New("解放の取り込み待ち (the release awaits review of the write-back)")
	// ErrUnreadable: the file cannot be read for sure.
	ErrUnreadable = errors.New("the write-back file cannot be read")
)

func held(kind error, format string, args ...any) error {
	return fmt.Errorf("%w: %w: "+format, append([]any{allocation.ErrWriteBackHeld, kind}, args...)...)
}

// Authority is the AllocationAuthority of the write-back: inner (the store,
// possibly behind the Lookup cache) for pools without write_back, and the
// file-first rules above for those with it.
type Authority struct {
	inner allocation.AllocationAuthority
	cfg   Config
	root  string

	// Now is the clock of allocated_at; time.Now when nil.
	Now func() time.Time

	mu    sync.Mutex
	pools map[string]loader.Pool
	// written are the files written by commit mode and not committed yet.
	written map[string]string // path → pool
	// review are the values to commit to the review branch at Flush.
	review map[string]map[string]int // pool → owner → value
	// verified are the pools whose file the store fully agrees with, for
	// this pass (SetIntentPools starts a pass).
	verified map[string]error
}

var _ allocation.AllocationAuthority = (*Authority)(nil)

// New wraps inner.  intentDir is the intent directory; a relative cfg.Path
// is taken from its parent (the repository root, as Sync Mode does).
func New(inner allocation.AllocationAuthority, cfg Config, intentDir string) *Authority {
	return &Authority{
		inner: inner, cfg: cfg.withDefaults(), root: filepath.Dir(filepath.Clean(intentDir)),
		pools: map[string]loader.Pool{}, written: map[string]string{},
		review: map[string]map[string]int{}, verified: map[string]error{},
	}
}

// Config returns the configuration in effect (defaults filled in).
func (a *Authority) Config() Config { return a.cfg }

func (a *Authority) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// SetIntentPools tells the pools of the intent loaded for this pass; those
// with WriteBack are handled here.  render.RenderAllocations calls it before
// resolving the requests.
func (a *Authority) SetIntentPools(pools []loader.Pool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pools = map[string]loader.Pool{}
	for _, p := range pools {
		if p.WriteBack {
			a.pools[p.Name] = p
		}
	}
	a.verified = map[string]error{}
}

// WriteBackPools returns the write-back pools of the intent, sorted by name.
func (a *Authority) WriteBackPools() []loader.Pool {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]loader.Pool, 0, len(a.pools))
	for _, p := range a.pools {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// IsWriteBack reports whether pool (or a block ref of it) is written back.
func (a *Authority) IsWriteBack(pool string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	base, _ := allocation.SplitBlockRef(pool)
	_, ok := a.pools[base]
	return ok
}

// ref is the pool argument the store is asked with for value v of pool: its
// block's ref (#206), so the Lookup cache in front of the store answers the
// same key reconcile allocated with.
func (a *Authority) ref(pool string, v int) string {
	b, _ := a.pools[pool].Spec().BlockOf(v)
	return allocation.BlockRef(pool, b)
}

// raiseLast records in f the last values of a reuse: false pool: at least
// every value f holds and the extra ones (#206).  A pool that reuses keeps
// no last value.
func (a *Authority) raiseLast(pool string, f *loader.AllocationFile, extra ...int) {
	p := a.pools[pool]
	if !p.NoReuse {
		return
	}
	spec := p.Spec()
	values := extra
	for _, e := range f.Allocations {
		values = append(values, e.Value)
	}
	for _, v := range values {
		b, ok := spec.BlockOf(v)
		if !ok {
			continue
		}
		if f.Last == nil {
			f.Last = map[string]int{}
		}
		if cur, ok := f.Last[b]; !ok || v > cur {
			f.Last[b] = v
		}
	}
}

// FilePath is the allocation file of pool.
func (a *Authority) FilePath(pool string) string {
	dir := a.cfg.Path
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(a.root, filepath.FromSlash(dir))
	}
	return filepath.Join(dir, pool+".yaml")
}

// ReadFile reads pool's allocation file (exists false when there is none).
func (a *Authority) ReadFile(pool string) (loader.AllocationFile, bool, error) {
	return loader.ReadAllocationFile(a.FilePath(pool), pool)
}

func (a *Authority) readFile(pool string) (loader.AllocationFile, error) {
	f, _, err := a.ReadFile(pool)
	if err != nil {
		return f, held(ErrUnreadable, "pool %q: %v (fix the file; nothing from this pool is used meanwhile)", pool, err)
	}
	return f, nil
}

// verify checks once per pass that the store holds every entry of pool's
// file with the same value.  Until it does, the pool hands out nothing new:
// the store would treat a value of the file as free.
func (a *Authority) verify(ctx context.Context, pool string, f loader.AllocationFile) error {
	if err, done := a.verified[pool]; done {
		return err
	}
	var missing, released, differ []string
	var cooling map[int]bool
	for _, e := range f.Allocations {
		v, ok, err := a.inner.Lookup(ctx, a.ref(pool, e.Value), e.Owner)
		if err != nil && !errors.Is(err, allocation.ErrUnknownPool) {
			return err // unavailable: not evidence either way; not remembered
		}
		switch {
		case !ok:
			if cooling == nil {
				cooling = a.cooling(ctx, pool)
			}
			if cooling[e.Value] {
				released = append(released, fmt.Sprintf("%s=%d", e.Owner, e.Value))
			} else {
				missing = append(missing, fmt.Sprintf("%s=%d", e.Owner, e.Value))
			}
		case v != strconv.Itoa(e.Value):
			differ = append(differ, fmt.Sprintf("%s=%d (store: %s)", e.Owner, e.Value, v))
		}
	}
	var behind []string
	if len(f.Last) > 0 && a.pools[pool].NoReuse {
		if storeLast, listed := a.storeLast(ctx, pool); listed {
			for _, b := range sortedBlocks(f.Last) {
				if sl, ok := storeLast[b]; !ok || sl < f.Last[b] {
					got := "none"
					if ok {
						got = strconv.Itoa(sl)
					}
					behind = append(behind, fmt.Sprintf("%s=%d (store: %s)", blockLabel(b), f.Last[b], got))
				}
			}
		}
	}
	var err error
	switch {
	case len(missing) > 0 || len(behind) > 0:
		if len(missing) == 0 {
			err = held(ErrNotInStore, "pool %q (reuse: false): the store's last value is below %s's on %s; no value is handed out from this pool until `sakutei allocator import` raises it from the file — it would hand out a released value again (§19.7, #206)",
				pool, a.FilePath(pool), strings.Join(behind, ", "))
			break
		}
		also := ""
		if len(released) > 0 {
			also = fmt.Sprintf("; %s were released in the store and still wait for the removal to be %s — do that first, the import refuses a value cooling down",
				strings.Join(released, ", "), a.releaseRemedy())
		}
		if len(behind) > 0 {
			also += fmt.Sprintf("; its last value is behind the file's on %s (reuse: false, #206)", strings.Join(behind, ", "))
		}
		err = held(ErrNotInStore, "pool %q: the store lacks %d allocation(s) of %s (%s); no value is handed out from this pool until `sakutei allocator import` rebuilds the store from the file (§19.7)%s",
			pool, len(missing), a.FilePath(pool), strings.Join(missing, ", "), also)
	case len(released) > 0:
		err = held(ErrReleaseAwaitingReview, "pool %q: %s still holds %s, released in the store (cooling down); no value is handed out from this pool until the removal is %s, which lifts the hold (the store needs no rebuilding, §19.7)",
			pool, a.FilePath(pool), strings.Join(released, ", "), a.releaseRemedy())
	case len(differ) > 0:
		err = held(ErrConflict, "pool %q: the store disagrees with %s on %s; no value is handed out from this pool until they agree (neither side is preferred; fix the file or the store by hand)",
			pool, a.FilePath(pool), strings.Join(differ, ", "))
	}
	a.verified[pool] = err
	return err
}

// check is the file-first rule for an owner the file holds.
func (a *Authority) check(ctx context.Context, pool, owner string, e loader.AllocationEntry) (string, error) {
	v, ok, err := a.inner.Lookup(ctx, a.ref(pool, e.Value), owner)
	if errors.Is(err, allocation.ErrUnknownPool) {
		ok, err = false, nil
	}
	if err != nil {
		return "", err
	}
	if !ok && a.cooling(ctx, pool)[e.Value] {
		return "", held(ErrReleaseAwaitingReview, "%s still holds %d for %s, which the store has released (cooling down): Apply of this resource is stopped until the removal is %s (the store needs no rebuilding, §19.7)",
			a.FilePath(pool), e.Value, owner, a.releaseRemedy())
	}
	if !ok {
		return "", held(ErrNotInStore, "%s holds %d for %s but the store has no record of it: Apply of this resource is stopped; run `sakutei allocator import` to rebuild the store from the file (§19.7)",
			a.FilePath(pool), e.Value, owner)
	}
	if v != strconv.Itoa(e.Value) {
		return "", conflict(a.FilePath(pool), owner, e.Value, v)
	}
	return v, nil
}

// cooling returns the values of pool cooling down in the store, i.e. released
// (§19.5 (b)) — or retired, released with reuse: false (#206).  A store that
// cannot list them, or fails to, gives none: the entry is then reported as
// lost, as before #167.
func (a *Authority) cooling(ctx context.Context, pool string) map[int]bool {
	recs, _ := a.records(ctx, pool)
	out := map[int]bool{}
	for _, r := range recs {
		if r.State == allocator.StateCooldown || r.State == allocator.StateRetired {
			out[r.Value] = true
		}
	}
	return out
}

// storeLast returns the store's last value per block of pool (#206); listed
// is false when the store cannot list its records.
func (a *Authority) storeLast(ctx context.Context, pool string) (map[string]int, bool) {
	recs, listed := a.records(ctx, pool)
	out := map[string]int{}
	for _, r := range recs {
		if r.State == allocator.StateLast {
			out[r.Block] = r.Value
		}
	}
	return out, listed
}

// records lists pool's records in the store (listed false when it cannot).
func (a *Authority) records(ctx context.Context, pool string) ([]allocator.Record, bool) {
	l, ok := a.inner.(interface {
		List(ctx context.Context, pool string) ([]allocator.Record, error)
	})
	if !ok {
		return nil, false
	}
	recs, err := l.List(ctx, pool)
	if err != nil {
		return nil, false
	}
	return recs, true
}

func sortedBlocks(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func blockLabel(b string) string {
	if b == "" {
		return "the pool"
	}
	return "block " + strconv.Quote(b)
}

// releaseRemedy says what lifts a hold on a released owner the file still
// holds: review waits for people to take the removal in; commit and file
// remove it at release, so the entry is left over (a failed write, or put
// back by hand) and is removed by hand.
func (a *Authority) releaseRemedy() string {
	if a.cfg.Mode == ModeReview {
		return fmt.Sprintf("taken into the intent from review branch %s (解放の書き戻しの取り込み待ち)", a.cfg.Branch)
	}
	return "removed from the file (by hand: the store released it)"
}

func conflict(path, owner string, file int, store string) error {
	return held(ErrConflict, "%s says %d for %s but the store says %s: Apply of this resource is stopped; make them agree by hand (neither side is preferred)",
		path, file, owner, store)
}

// Allocate implements allocation.AllocationAuthority.  ref is the pool, or a
// block of it ("pool/block", #206).
func (a *Authority) Allocate(ctx context.Context, ref, owner string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	pool, block := allocation.SplitBlockRef(ref)
	if _, ok := a.pools[pool]; !ok {
		return a.inner.Allocate(ctx, ref, owner)
	}
	f, err := a.readFile(pool)
	if err != nil {
		return "", err
	}
	if e, ok := f.ByOwner()[owner]; ok {
		v, err := a.check(ctx, pool, owner, e)
		if err != nil {
			return "", err
		}
		// Allocate the same owner again (idempotent: the same value) so a
		// Lookup cache in front of the store keeps the fact (§19.6).
		v2, err := a.inner.Allocate(ctx, a.ref(pool, e.Value), owner)
		if err != nil {
			return "", err
		}
		if v2 != v {
			return "", conflict(a.FilePath(pool), owner, e.Value, v2)
		}
		return v, nil
	}
	if err := a.verify(ctx, pool, f); err != nil {
		return "", err
	}
	v, err := a.inner.Allocate(ctx, ref, owner)
	if err != nil {
		return "", err
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return "", fmt.Errorf("allocwriteback: pool %q: value %q is not an integer", pool, v)
	}
	if last, ok := f.Last[block]; ok && n <= last && a.pools[pool].NoReuse {
		// The store is behind the file's last value and could not tell
		// (verify needs its records).  Retire the value at once — it was
		// handed out to nobody — and hold until the store is rebuilt.
		_ = a.inner.Release(ctx, ref, owner)
		return "", held(ErrNotInStore, "the store handed %d to %s, at or below the last value %d %s records for %s (reuse: false): Apply of this resource is stopped; run `sakutei allocator import` to raise the store's last value from the file (§19.7, #206)",
			n, owner, last, a.FilePath(pool), blockLabel(block))
	}
	for _, e := range f.Allocations {
		if e.Value == n {
			return "", held(ErrConflict, "the store handed %d to %s but %s holds it for %s: Apply of this resource is stopped; make them agree by hand",
				n, owner, a.FilePath(pool), e.Owner)
		}
	}
	if a.cfg.Mode == ModeReview {
		if a.review[pool] == nil {
			a.review[pool] = map[string]int{}
		}
		a.review[pool][owner] = n
		return "", held(ErrAwaitingReview, "value %d of pool %q is allocated in the store and committed for review to branch %s; Apply of this resource waits until the value appears in %s",
			n, pool, a.cfg.Branch, a.FilePath(pool))
	}
	// commit / file: written back before it is used.
	f.Allocations = append(f.Allocations, loader.AllocationEntry{Owner: owner, Value: n, AllocatedAt: a.now().UTC().Truncate(time.Second)})
	a.raiseLast(pool, &f)
	if err := a.writeFile(pool, f); err != nil {
		return "", fmt.Errorf("%w: pool %q: write back %d for %s: %v", allocation.ErrWriteBackHeld, pool, n, owner, err)
	}
	return v, nil
}

// Lookup implements allocation.AllocationAuthority: the read-only verbs see
// the same holds as reconcile.  It never writes.
func (a *Authority) Lookup(ctx context.Context, ref, owner string) (string, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	pool, _ := allocation.SplitBlockRef(ref)
	if _, ok := a.pools[pool]; !ok {
		return a.inner.Lookup(ctx, ref, owner)
	}
	f, err := a.readFile(pool)
	if err != nil {
		return "", false, err
	}
	if e, ok := f.ByOwner()[owner]; ok {
		v, err := a.check(ctx, pool, owner, e)
		return v, err == nil, err
	}
	v, ok, err := a.inner.Lookup(ctx, ref, owner)
	if errors.Is(err, allocation.ErrUnknownPool) {
		ok, err = false, nil
	}
	if err != nil {
		return "", false, err
	}
	if verr := a.verify(ctx, pool, f); verr != nil {
		return "", false, verr
	}
	if ok && a.cfg.Mode == ModeReview {
		return "", false, held(ErrAwaitingReview, "value %s of pool %q is allocated in the store and not yet in %s (review branch %s); Apply of this resource waits until it is taken in",
			v, pool, a.FilePath(pool), a.cfg.Branch)
	}
	// commit / file: reconcile writes it back before using it.
	return v, ok, nil
}

// Reserve implements allocation.AllocationAuthority (passed through: a
// reservation is not an allocation until Confirm, §19.4).
func (a *Authority) Reserve(ctx context.Context, pool, owner string, ttl time.Duration) (string, error) {
	return a.inner.Reserve(ctx, pool, owner, ttl)
}

// Confirm implements allocation.AllocationAuthority (passed through; the
// next Allocate of the owner writes it back).
func (a *Authority) Confirm(ctx context.Context, pool, owner string) error {
	return a.inner.Confirm(ctx, pool, owner)
}

// Release implements allocation.AllocationAuthority: the store releases the
// value (and keeps its cooldown, a working record), then the owner leaves the
// file the same way an allocation enters it.
func (a *Authority) Release(ctx context.Context, ref, owner string) error {
	if err := a.inner.Release(ctx, ref, owner); err != nil {
		return err
	}
	pool, _ := allocation.SplitBlockRef(ref)
	return a.Forget(ctx, pool, owner)
}

// Forget removes owner from pool's file after the store released it (the
// human `sakutei allocator release` calls it after the store's release).
// commit writes and commits at once, review commits the removal to the review
// branch, file writes.  A pool without write_back, or an owner the file does
// not hold, is a no-op.  A pool with reuse: false keeps the owner's value
// in its last values (#206): the release does not lower them.
func (a *Authority) Forget(ctx context.Context, pool, owner string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	pool, _ = allocation.SplitBlockRef(pool)
	if _, ok := a.pools[pool]; !ok {
		return nil
	}
	delete(a.review[pool], owner)
	if a.cfg.Mode == ModeReview {
		return a.updateReviewBranch(ctx, pool, nil, owner)
	}
	f, _, err := a.ReadFile(pool)
	if err != nil {
		return err
	}
	a.raiseLast(pool, &f) // before the owner leaves: its value stays behind
	kept := f.Allocations[:0]
	found := false
	for _, e := range f.Allocations {
		if e.Owner == owner {
			found = true
			continue
		}
		kept = append(kept, e)
	}
	if !found {
		return nil
	}
	f.Allocations = kept
	if err := a.writeFile(pool, f); err != nil {
		return err
	}
	return a.commitWritten(ctx)
}

// EnsurePool is DeclarePool of a pool without blocks.
func (a *Authority) EnsurePool(name string, lo, hi int) error {
	return a.DeclarePool(name, allocation.PoolSpec{Lo: lo, Hi: hi})
}

// DeclarePool passes the pool declaration through to the store.
func (a *Authority) DeclarePool(name string, spec allocation.PoolSpec) error {
	if pe, ok := a.inner.(interface {
		DeclarePool(string, allocation.PoolSpec) error
	}); ok {
		return pe.DeclarePool(name, spec)
	}
	return nil
}

// FreeCount passes through to the store when it can count (#111).
func (a *Authority) FreeCount(ctx context.Context, pool string, lo, hi int) (int, error) {
	if fc, ok := a.inner.(interface {
		FreeCount(context.Context, string, int, int) (int, error)
	}); ok {
		return fc.FreeCount(ctx, pool, lo, hi)
	}
	return 0, errors.New("allocwriteback: the store cannot count free values")
}

// Flush finishes a pass: commit commits the files written in it, review
// commits the values handed out to the review branch.  A failure is returned
// as a warning, never as an error: commit's files are written (the next pass
// commits them), review's values stay held.
func (a *Authority) Flush(ctx context.Context) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var warnings []string
	if err := a.commitWritten(ctx); err != nil {
		warnings = append(warnings, fmt.Sprintf("write-back: commit: %v (the files are written; the next pass commits them)", err))
	}
	pools := make([]string, 0, len(a.review))
	for p := range a.review {
		pools = append(pools, p)
	}
	sort.Strings(pools)
	for _, p := range pools {
		if len(a.review[p]) == 0 {
			continue
		}
		if err := a.updateReviewBranch(ctx, p, a.review[p], ""); err != nil {
			warnings = append(warnings, fmt.Sprintf("write-back: pool %q: review branch %s: %v (the values stay held)", p, a.cfg.Branch, err))
			continue
		}
		delete(a.review, p)
	}
	return warnings
}

func (a *Authority) writeFile(pool string, f loader.AllocationFile) error {
	f.SchemaVersion, f.Pool = loader.AllocationFileSchemaVersion, pool
	if !a.pools[pool].NoReuse {
		f.Last = nil // last values belong to reuse: false pools only (#206)
	}
	path := a.FilePath(pool)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := writeAtomic(path, loader.EmitAllocationFile(f)); err != nil {
		return err
	}
	if a.cfg.Mode == ModeCommit {
		a.written[path] = pool
	}
	return nil
}

// repo is the git work tree to commit in ("" when there is none).
func (a *Authority) repo(ctx context.Context) string {
	if a.cfg.Repo != "" {
		return syncmode.FindRepo(ctx, a.cfg.Repo)
	}
	dir := filepath.Dir(a.FilePath("x"))
	for {
		if _, err := os.Stat(dir); err == nil {
			return syncmode.FindRepo(ctx, dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// commitWritten commits the files commit mode wrote (one commit, as the
// system) and pushes when a remote is configured.  Outside a git work tree
// the files stay written only.
func (a *Authority) commitWritten(ctx context.Context) error {
	if a.cfg.Mode != ModeCommit || len(a.written) == 0 {
		return nil
	}
	repo := a.repo(ctx)
	if repo == "" {
		a.written = map[string]string{}
		return nil
	}
	paths := make([]string, 0, len(a.written))
	for p := range a.written {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	trailers := make([]string, len(paths))
	for i, p := range paths {
		trailers[i] = Trailer + ": " + a.written[p]
	}
	commit, err := syncmode.CommitPaths(ctx, repo, paths, trailers, a.cfg.Committer,
		func(n int) string { return fmt.Sprintf("sakutei allocation: write back (%d pool(s))", n) },
		"採番の結果の書き戻し（§19.7）を、システムとして自動でコミットしたものです。この pool の割当の正はこのファイルです。")
	if err != nil {
		return err
	}
	a.written = map[string]string{}
	if commit != "" && a.cfg.Remote != "" {
		if _, err := syncmode.Git(ctx, repo, a.pushEnv(), "push", "-q", a.cfg.Remote, "HEAD"); err != nil {
			return fmt.Errorf("committed %s, push to %s failed: %w", commit, a.cfg.Remote, err)
		}
	}
	return nil
}

func (a *Authority) pushEnv() []string {
	if a.cfg.SSHKey == "" {
		return nil
	}
	return []string{"GIT_SSH_COMMAND=ssh -i " + a.cfg.SSHKey + " -o IdentitiesOnly=yes -o BatchMode=yes"}
}

// updateReviewBranch commits pool's file with add added and remove removed to
// the review branch, without touching the work tree: on top of the branch
// while people have not taken it in yet, or afresh on top of HEAD once they
// have (the branch tip is in HEAD's history) or when there is no branch.
// Nothing is committed when the file would not change.
func (a *Authority) updateReviewBranch(ctx context.Context, pool string, add map[string]int, remove string) error {
	repo := a.repo(ctx)
	if repo == "" {
		return fmt.Errorf("mode review needs a git work tree (none holds %s)", a.FilePath(pool))
	}
	rel, err := syncmode.RelPath(repo, a.FilePath(pool))
	if err != nil {
		return err
	}
	rel = filepath.ToSlash(rel)
	head, err := syncmode.Git(ctx, repo, nil, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	ref := "refs/heads/" + a.cfg.Branch
	tip, _ := syncmode.Git(ctx, repo, nil, "rev-parse", "-q", "--verify", ref)
	parent := head
	if tip != "" {
		if _, err := syncmode.Git(ctx, repo, nil, "merge-base", "--is-ancestor", tip, head); err != nil {
			parent = tip // not taken in yet: stack on it
		}
	}
	f := loader.AllocationFile{SchemaVersion: loader.AllocationFileSchemaVersion, Pool: pool}
	if data, err := syncmode.Git(ctx, repo, nil, "show", parent+":"+rel); err == nil {
		if f, err = loader.ParseAllocationFile([]byte(data+"\n"), rel, pool); err != nil {
			return err
		}
	}
	before := string(loader.EmitAllocationFile(f))
	byOwner, byValue := f.ByOwner(), map[int]string{}
	for _, e := range f.Allocations {
		byValue[e.Value] = e.Owner
	}
	owners := make([]string, 0, len(add))
	for o := range add {
		owners = append(owners, o)
	}
	sort.Strings(owners)
	for _, o := range owners {
		if _, ok := byOwner[o]; ok {
			continue
		}
		if other, ok := byValue[add[o]]; ok {
			return fmt.Errorf("value %d for %s is held by %s on the branch", add[o], o, other)
		}
		f.Allocations = append(f.Allocations, loader.AllocationEntry{Owner: o, Value: add[o], AllocatedAt: a.now().UTC().Truncate(time.Second)})
		byValue[add[o]] = o
	}
	a.raiseLast(pool, &f) // the removed owner's value too: it stays behind
	if !a.pools[pool].NoReuse {
		f.Last = nil
	}
	if remove != "" {
		kept := f.Allocations[:0]
		for _, e := range f.Allocations {
			if e.Owner != remove {
				kept = append(kept, e)
			}
		}
		f.Allocations = kept
	}
	after := loader.EmitAllocationFile(f)
	if string(after) == before {
		return nil
	}

	tmp, err := os.MkdirTemp("", "sakutei-writeback-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	blobFile := filepath.Join(tmp, "blob")
	if err := os.WriteFile(blobFile, after, 0o644); err != nil {
		return err
	}
	blob, err := syncmode.Git(ctx, repo, nil, "hash-object", "-w", "--", blobFile)
	if err != nil {
		return err
	}
	idx := []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}
	if _, err := syncmode.Git(ctx, repo, idx, "read-tree", parent); err != nil {
		return err
	}
	if _, err := syncmode.Git(ctx, repo, idx, "update-index", "--add", "--cacheinfo", "100644,"+blob+","+rel); err != nil {
		return err
	}
	tree, err := syncmode.Git(ctx, repo, idx, "write-tree")
	if err != nil {
		return err
	}
	commit, err := syncmode.Git(ctx, repo, syncmode.CommitterEnv(a.cfg.Committer), "commit-tree", tree, "-p", parent,
		"-m", fmt.Sprintf("sakutei allocation: write back pool %s (for review)", pool),
		"-m", "採番の結果の書き戻し（§19.7）です。レビューして intent のブランチへ取り込むと、その値を使う Apply が進みます（取り込むまでは止まります）。",
		"-m", Trailer+": "+pool)
	if err != nil {
		return err
	}
	if _, err := syncmode.Git(ctx, repo, nil, "update-ref", ref, commit, tip); err != nil {
		return err
	}
	if a.cfg.Remote != "" {
		if _, err := syncmode.Git(ctx, repo, a.pushEnv(), "push", "-q", a.cfg.Remote, ref+":"+ref); err != nil {
			return fmt.Errorf("committed %s to %s, push to %s failed: %w", commit, a.cfg.Branch, a.cfg.Remote, err)
		}
	}
	return nil
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
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
		err = os.Chmod(name, 0o644)
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		_ = os.Remove(name)
	}
	return err
}
