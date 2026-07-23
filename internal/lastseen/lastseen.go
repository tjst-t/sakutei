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

// Package lastseen implements the last-seen ring (§10.5, ADR-0008, Scb0ecd-3).
//
// # Purpose
//
// The engine is observation-state-less (ADR-0002): it never caches "current
// actual state" across cycles.  This is the source of its correctness
// (no drift-detection blind spots), but it also means the engine cannot
// natively answer "what was the previous value?" for a drifted field.
//
// The last-seen ring is a deliberate, bounded exception.  It records a fixed
// number of recent actual-state snapshots (per resource, per cycle) in memory
// only (S3 classification, ADR-0015), so that when cause B (drift) is
// detected the operator can see "size was 10 last cycle, now it is 20".
//
// # Safety properties
//
//   - S3 (memory-only, non-persistent): the ring is reset on daemon restart.
//     After restart, the first cycle has no prior snapshot; the second cycle
//     onwards the ring starts filling.  This matches the scenario-Scb0ecd-3.json
//     step 3 verification: "non-persistent — first cycle has no snapshot".
//
//   - Observation-state-less is preserved: the ring records what was observed
//     in a previous cycle as a hint for operator diagnostics, not as a truth
//     value used by the diff engine.  The diff engine always re-observes fresh;
//     it never reads from the ring.  Losing the ring (restart) only makes B
//     diagnostics "no previous snapshot" for one cycle — it never affects
//     convergence correctness.
//
//   - The ring is NOT shared with the one-shot "sakutei reconcile" path.  It
//     only makes sense in the long-lived daemon (each cycle populates the ring;
//     the next cycle reads from it).  The one-shot reconcile has no "previous
//     cycle" by definition, so it always produces "no previous snapshot".
//
// # Ring design
//
// Ring is a per-resource ring buffer of fixed capacity N (default 5 cycles).
// The default is intentionally small: the value of the last-seen ring is in
// the most-recent cycle diff, not in deep history.
//
// Thread-safety: Ring is NOT concurrency-safe on its own.  The caller (daemon
// loop) must call Record and Diff from the same goroutine, or protect with its
// own lock.  In the current daemon design, the Observe/Diff phase is single-
// threaded per cycle; only Apply is async.  Record is called after Observe,
// in the single-threaded phase, so no lock is needed.
package lastseen

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// DefaultRingSize is the number of per-resource cycle snapshots the ring
// retains.  Five cycles is enough to show "the last N times we looked, the
// value was X" without growing unboundedly.
const DefaultRingSize = 5

// Snapshot captures the actual state of a single resource as of one Observe
// call.  It is intentionally a simple string map so the ring does not import
// core types and avoids circular imports.
type Snapshot struct {
	// LocalID is the provider-local resource identifier.
	LocalID string
	// ObservedAt is when this snapshot was taken.
	ObservedAt time.Time
	// Fields is a copy of the observed field values (as strings, matching
	// the string representation used by the diff engine).
	Fields map[string]string
}

// FieldDiff is a single field-level difference between two snapshots.
type FieldDiff struct {
	Field string
	Prev  string
	Cur   string
}

// Ring is a per-resource ring buffer of Snapshots.  The zero value is
// ready-to-use with DefaultRingSize capacity.
type Ring struct {
	size    int
	entries []Snapshot // ring buffer, up to size elements
	// head is the index where the next write goes (oldest entry when full).
	head int
	// count is the number of valid entries stored so far (≤ size).
	count int
}

// New creates a Ring with the given capacity.  Capacity must be > 0; if ≤ 0
// the ring falls back to DefaultRingSize.
func New(size int) *Ring {
	if size <= 0 {
		size = DefaultRingSize
	}
	return &Ring{
		size:    size,
		entries: make([]Snapshot, size),
	}
}

// Record adds a snapshot to the ring, overwriting the oldest entry when the
// ring is full.
func (r *Ring) Record(s Snapshot) {
	if r.size == 0 {
		r.size = DefaultRingSize
		r.entries = make([]Snapshot, r.size)
	}
	r.entries[r.head] = s
	r.head = (r.head + 1) % r.size
	if r.count < r.size {
		r.count++
	}
}

// Latest returns the most recently recorded Snapshot for the resource, and
// whether one exists.  Returns false if the ring is empty (e.g. first cycle
// after daemon start or after restart).
func (r *Ring) Latest() (Snapshot, bool) {
	if r == nil || r.count == 0 {
		return Snapshot{}, false
	}
	// The most recent write was at head-1 (mod size).
	idx := (r.head - 1 + r.size) % r.size
	return r.entries[idx], true
}

// DiffFromLatest compares curFields against the most recent snapshot and
// returns a list of FieldDiff entries where values differ, plus a human-
// readable summary string suitable for logging.
//
// If the ring is empty (no prior snapshot), it returns nil diffs and the
// summary "no previous snapshot (first cycle or post-restart)".
func (r *Ring) DiffFromLatest(curFields map[string]string) ([]FieldDiff, string) {
	prev, ok := r.Latest()
	if !ok {
		return nil, "no previous snapshot (first cycle or post-restart)"
	}

	// Collect all field names from both prev and cur for deterministic output.
	nameSet := make(map[string]struct{})
	for k := range prev.Fields {
		nameSet[k] = struct{}{}
	}
	for k := range curFields {
		nameSet[k] = struct{}{}
	}
	names := make([]string, 0, len(nameSet))
	for k := range nameSet {
		names = append(names, k)
	}
	sort.Strings(names)

	var diffs []FieldDiff
	for _, name := range names {
		prevVal := prev.Fields[name]
		curVal := curFields[name]
		if prevVal != curVal {
			diffs = append(diffs, FieldDiff{Field: name, Prev: prevVal, Cur: curVal})
		}
	}

	if len(diffs) == 0 {
		return nil, fmt.Sprintf("prev snapshot at %s matches current (no field change in ring)", prev.ObservedAt.Format(time.RFC3339))
	}

	parts := make([]string, 0, len(diffs))
	for _, d := range diffs {
		parts = append(parts, fmt.Sprintf("%s was %q prev cycle now %q", d.Field, d.Prev, d.Cur))
	}
	summary := fmt.Sprintf("drift B: %s (prev snapshot at %s)", strings.Join(parts, "; "), prev.ObservedAt.Format(time.RFC3339))
	return diffs, summary
}

// ResourceRing is a per-resource ring map: LocalID → *Ring.
// It is the single last-seen state store for a daemon run.
//
// Thread-safety: not concurrency-safe; the caller must hold its own lock if
// multiple goroutines share a ResourceRing (the daemon loop accesses it only
// from the single-threaded Observe/Diff phase).
type ResourceRing struct {
	rings    map[string]*Ring
	ringSize int
}

// NewResourceRing creates a ResourceRing where each per-resource ring has the
// given capacity.
func NewResourceRing(ringSize int) *ResourceRing {
	if ringSize <= 0 {
		ringSize = DefaultRingSize
	}
	return &ResourceRing{
		rings:    make(map[string]*Ring),
		ringSize: ringSize,
	}
}

// Record stores a snapshot for the given localID.
func (rr *ResourceRing) Record(localID string, s Snapshot) {
	if _, ok := rr.rings[localID]; !ok {
		rr.rings[localID] = New(rr.ringSize)
	}
	rr.rings[localID].Record(s)
}

// DriftSummary compares curFields against the most recent snapshot for
// localID.  Returns a human-readable summary string for B-cause drift Events.
// If no prior snapshot exists (first cycle), returns a "no previous snapshot"
// message.
func (rr *ResourceRing) DriftSummary(localID string, curFields map[string]string) string {
	ring, ok := rr.rings[localID]
	if !ok {
		return "no previous snapshot (first cycle or post-restart)"
	}
	_, summary := ring.DiffFromLatest(curFields)
	return summary
}
