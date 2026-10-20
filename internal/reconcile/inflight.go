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

// Package reconcile — inflight.go implements the in-flight set and per-resource
// backoff table for Story S94f1f9-3.
//
// In-flight set (§8.4):
//   - Apply is launched as a goroutine; the resource Local ID is inserted into
//     the in-flight set for the duration.
//   - On the next Cycle, if a resource is in-flight, it is treated as
//     "converging" — no re-plan, no re-Apply, and its downstream is skipped
//     (same semantics as an unobservable upstream).
//   - When Apply completes (success or failure) the resource is removed from the
//     in-flight set.  On failure, the resource moves to the backoff table.
//
// Per-resource backoff (§8.5):
//   - (resource Local ID, error kind) → next-retry time.
//   - Transient errors: exponential backoff, initial 1 s, doubling each time,
//     capped at 15 minutes, plus ±10% jitter.
//   - Permanent errors: resource is "stuck"; backoff is set to a very large
//     value (24 h). The operator must change desired state to clear it.
//   - Both tables live in memory only (S3 state class per ADR-0015).
//     A restart resets them; the next Cycle will re-observe fresh (ADR-0002).
//   - DD-1: a resource in backoff is an unconverged upstream (its last Apply
//     failed and we are waiting to retry).  While it is in backoff its
//     transitive downstream is skipped — the same halt-default invariant used
//     for Apply failures and unobservable upstreams (D-1).  This applies even to
//     a 24 h Permanent-error backoff: downstream is not created on top of a
//     broken upstream.

package reconcile

import (
	"errors"
	"math/rand"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/pkg/provider"
)

// errKind classifies an Apply error for the backoff table key.
type errKind int

const (
	errKindTransient errKind = iota
	errKindPermanent
)

// errKindOf returns the errKind for an Apply error.
// Anything that is not a *provider.PermanentError is treated as transient.
func errKindOf(err error) errKind {
	var pe *provider.PermanentError
	if errors.As(err, &pe) {
		return errKindPermanent
	}
	return errKindTransient
}

// backoffKey identifies a (resource, error-kind) backoff slot.
type backoffKey struct {
	local   string
	errKind errKind
}

// backoffEntry holds the retry-after time and the current backoff duration
// for a resource that failed Apply.
type backoffEntry struct {
	// retryAfter is the earliest time at which Apply may be retried.
	retryAfter time.Time
	// current is the current backoff duration (before jitter).
	current time.Duration
}

const (
	backoffInitial  = 1 * time.Second
	backoffMax      = 15 * time.Minute
	backoffStuck    = 24 * time.Hour // permanent error: do not retry for a very long time
	backoffJitterPc = 10             // ±10% jitter as a percentage
)

// InFlightSet tracks which resource Local IDs have an active async Apply
// running, and the per-resource backoff table for failed Applies.
//
// All public methods are safe for concurrent use.
type InFlightSet struct {
	mu sync.Mutex

	// inflight maps resource Local ID → true for resources with an active Apply.
	inflight map[string]bool

	// backoff maps (local, errKind) → backoffEntry for resources in cooldown.
	backoff map[backoffKey]backoffEntry

	// lane marks in-flight resources whose Apply was dispatched in a
	// planned_change lane (§13.3); they are not counted in the normal budget.
	lane map[string]bool
}

// NewInFlightSet creates an empty InFlightSet.
func NewInFlightSet() *InFlightSet {
	return &InFlightSet{
		inflight: make(map[string]bool),
		backoff:  make(map[backoffKey]backoffEntry),
		lane:     make(map[string]bool),
	}
}

// IsInFlight reports whether the resource identified by local has an active
// Apply goroutine running.
func (s *InFlightSet) IsInFlight(local string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inflight[local]
}

// Add inserts local into the in-flight set.
// Callers must call Remove when the Apply goroutine completes.
func (s *InFlightSet) Add(local string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inflight[local] = true
}

// Remove removes local from the in-flight set.  It is idempotent.
func (s *InFlightSet) Remove(local string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, local)
	delete(s.lane, local)
}

// MarkLane records that the in-flight Apply of local was dispatched in a
// planned_change lane (§13.3).  Cleared by Remove.
func (s *InFlightSet) MarkLane(local string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight[local] {
		s.lane[local] = true
	}
}

// IsLane reports whether local is in flight in a planned_change lane.
func (s *InFlightSet) IsLane(local string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lane[local]
}

// InFlightLocals returns a snapshot of all Local IDs currently in-flight.
// The returned slice is a copy; modifications do not affect the set.
func (s *InFlightSet) InFlightLocals() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.inflight))
	for local := range s.inflight {
		out = append(out, local)
	}
	return out
}

// InBackoff reports whether the resource is currently in backoff and must not
// be retried yet.
func (s *InFlightSet) InBackoff(local string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Check both error kinds.
	for _, k := range []errKind{errKindTransient, errKindPermanent} {
		key := backoffKey{local: local, errKind: k}
		if entry, ok := s.backoff[key]; ok {
			if time.Now().Before(entry.retryAfter) {
				return true
			}
		}
	}
	return false
}

// RecordFailure records an Apply failure for local, advancing or initialising
// the backoff entry for the (local, error-kind) pair.
func (s *InFlightSet) RecordFailure(local string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	kind := errKindOf(err)
	key := backoffKey{local: local, errKind: kind}

	entry, exists := s.backoff[key]
	if !exists {
		entry = backoffEntry{current: backoffInitial}
	}

	switch kind {
	case errKindPermanent:
		// Stuck: do not retry for a very long time.
		entry.retryAfter = time.Now().Add(backoffStuck)
		entry.current = backoffStuck
	default:
		// Transient: exponential backoff with jitter, capped at backoffMax.
		if entry.current <= 0 {
			entry.current = backoffInitial
		}
		jittered := applyJitter(entry.current)
		entry.retryAfter = time.Now().Add(jittered)
		// Double for next failure, cap at max.
		next := entry.current * 2
		if next > backoffMax {
			next = backoffMax
		}
		entry.current = next
	}

	s.backoff[key] = entry
}

// ClearBackoff removes all backoff entries for local (called on Apply success
// or when desired state changes).
func (s *InFlightSet) ClearBackoff(local string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range []errKind{errKindTransient, errKindPermanent} {
		delete(s.backoff, backoffKey{local: local, errKind: k})
	}
}

// Wait blocks until all currently in-flight Apply goroutines have finished.
// It returns once the in-flight set is empty.  If the in-flight set is already
// empty, it returns immediately.
//
// This is called during graceful shutdown to ensure in-flight Applies have a
// chance to complete before the process exits.
func (s *InFlightSet) Wait(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for {
		s.mu.Lock()
		n := len(s.inflight)
		s.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// applyJitter returns d with ±jitterPc% randomness applied.
func applyJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	pct := int64(d) * int64(backoffJitterPc) / 100
	if pct <= 0 {
		pct = 1
	}
	delta := rand.Int63n(2*pct) - pct // random value in [-pct, +pct)
	result := time.Duration(int64(d) + delta)
	if result <= 0 {
		return d
	}
	return result
}
