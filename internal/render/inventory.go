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

// inventory.go — render phase 1 inventory composition (§9.1) and the
// incomplete≠empty guard (§9.2).
//
// An inventory (CMDB) composes desired resources that are not written in
// intent.  "Unreachable → keep the previous value" (ResolverCache) does not
// cover a 200 OK whose content shrank (replica lag, partial outage): rendering
// a half inventory turns the missing half into "absent from desired" and
// closed-world prune deletes it — and since desired is not empty, the
// empty-desired protection does not catch it either.
//
// InventoryGuard therefore compares each fetch with the last accepted one and
// treats a sudden drop (to DefaultInventoryMinRatio or below) as incomplete,
// not empty: the caller halts the cycle and emits an InventoryIncomplete Event
// (原則3: 確信が持てないとき減算しない).
package render

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/loader"
)

// InventorySource is the render-phase-1 seam for inventory composition: it
// returns the desired resources an external inventory composes.
//
// Contract: Fetch returns a non-nil error when the inventory cannot be read
// (unreachable, unparsable); it must never report an unreadable inventory as
// an empty one.
type InventorySource interface {
	Fetch(ctx context.Context) ([]diff.DesiredResource, error)
}

// FileInventorySource reads the inventory from one intent-format file — the
// stand-in for a CMDB export.  A missing or unparsable file (e.g. caught
// mid-write) is reported as unreachable.
type FileInventorySource struct {
	Path string
}

// Fetch implements InventorySource.
func (s *FileInventorySource) Fetch(_ context.Context) ([]diff.DesiredResource, error) {
	resources, err := loader.LoadResourcesFile(s.Path)
	if err != nil {
		return nil, &ExternalInputUnreachableError{Source: "inventory " + s.Path, Cause: err}
	}
	return resources, nil
}

// DefaultInventoryMinRatio is the default drop threshold (§9.2 既定 50%): a
// fetch with at most this fraction of the last accepted item count is
// incomplete.  The boundary is inclusive — exactly half is incomplete — the
// same "50%+ drop halts" rule as the prune List count-drop gate (§12.4b).
const DefaultInventoryMinRatio = 0.50

// InventoryGuard composes the inventory each cycle and applies the
// incomplete≠empty guard.  It keeps the last accepted inventory in memory
// (S3, ADR-0015): a restart resets the baseline and the first fetch after it
// is accepted as is, the same rule as the prune count-drop baseline.
//
// InventoryGuard is safe for concurrent use.
type InventoryGuard struct {
	source      InventorySource
	minRatio    float64
	expectedMin int

	mu       sync.Mutex
	last     []diff.DesiredResource
	hasValue bool
}

// InventoryOption configures an InventoryGuard.
type InventoryOption func(*InventoryGuard)

// WithInventoryMinRatio sets the drop threshold; it must be in (0, 1),
// otherwise DefaultInventoryMinRatio is kept.
func WithInventoryMinRatio(r float64) InventoryOption {
	return func(g *InventoryGuard) {
		if r > 0 && r < 1 {
			g.minRatio = r
		}
	}
}

// WithInventoryExpectedMin sets the expected cardinality (§9.2 期待基数): a
// fetch with fewer items is incomplete even without a previous baseline (the
// first cycle after a restart).  0 disables the check.
func WithInventoryExpectedMin(n int) InventoryOption {
	return func(g *InventoryGuard) {
		if n > 0 {
			g.expectedMin = n
		}
	}
}

// NewInventoryGuard creates a guard over src.
func NewInventoryGuard(src InventorySource, opts ...InventoryOption) *InventoryGuard {
	g := &InventoryGuard{source: src, minRatio: DefaultInventoryMinRatio}
	for _, o := range opts {
		o(g)
	}
	return g
}

// InventoryResult is the outcome of InventoryGuard.Compose.
type InventoryResult struct {
	// Resources are the inventory-composed desired resources.  Nil when Halt.
	Resources []diff.DesiredResource
	// Input is the "inventory=<sha256>" external input for
	// revision.ComputeRenderedRevision (an inventory change is cause E-inventory).
	// Empty when Halt.
	Input string
	// UnreachableFallback is true when the source failed and the last accepted
	// inventory was returned (§9.2 前回保持).
	UnreachableFallback bool
	// Halt is true when the caller must not converge this cycle: the inventory
	// is incomplete, or it is unreachable with nothing accepted yet.
	Halt bool
	// Incomplete is true when Halt is due to the incomplete≠empty guard.
	Incomplete bool
	// Reason explains Halt or UnreachableFallback.
	Reason string
}

// Compose fetches the inventory and applies the guard:
//   - unreachable with a previous inventory → that inventory (UnreachableFallback);
//   - unreachable with none → Halt;
//   - fewer items than the expected cardinality, or at most minRatio of the
//     last accepted count → Halt + Incomplete.  The baseline is NOT moved, so
//     the guard keeps halting until the inventory recovers;
//   - otherwise the fetch is accepted and becomes the new baseline.
func (g *InventoryGuard) Compose(ctx context.Context) InventoryResult {
	g.mu.Lock()
	defer g.mu.Unlock()

	fetched, err := g.source.Fetch(ctx)
	if err != nil {
		if g.hasValue {
			return InventoryResult{
				Resources:           cloneResources(g.last),
				Input:               inventoryInput(g.last),
				UnreachableFallback: true,
				Reason:              err.Error(),
			}
		}
		return InventoryResult{Halt: true, Reason: fmt.Sprintf("%v; no previous inventory to fall back to", err)}
	}

	n := len(fetched)
	if g.expectedMin > 0 && n < g.expectedMin {
		return InventoryResult{Halt: true, Incomplete: true,
			Reason: fmt.Sprintf("inventory has %d item(s), fewer than the expected %d", n, g.expectedMin)}
	}
	if g.hasValue {
		prev := len(g.last)
		if prev > 0 && float64(n) <= float64(prev)*g.minRatio {
			return InventoryResult{Halt: true, Incomplete: true,
				Reason: fmt.Sprintf("inventory dropped from %d to %d item(s) (at or below %.0f%% of the previous)",
					prev, n, g.minRatio*100)}
		}
	}

	g.last = cloneResources(fetched)
	g.hasValue = true
	return InventoryResult{Resources: fetched, Input: inventoryInput(fetched)}
}

// inventoryInput hashes the inventory independently of item order, so a CMDB
// that reorders its export does not look like an inventory change (§9.3).
func inventoryInput(rs []diff.DesiredResource) string {
	enc := make([]string, 0, len(rs))
	for _, r := range rs {
		b, _ := json.Marshal(r) // map keys are sorted by encoding/json
		enc = append(enc, string(b))
	}
	sort.Strings(enc)
	h := sha256.New()
	for _, e := range enc {
		h.Write([]byte(e))
		h.Write([]byte{0})
	}
	return "inventory=" + hex.EncodeToString(h.Sum(nil))
}

func cloneResources(rs []diff.DesiredResource) []diff.DesiredResource {
	if rs == nil {
		return nil
	}
	out := make([]diff.DesiredResource, len(rs))
	copy(out, rs)
	return out
}
