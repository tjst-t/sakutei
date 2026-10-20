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

package reconcile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/internal/diff"
)

// ObserveScheduler is the Observe scheduler seam of §16.5: it decides, per
// resource and per daemon cycle, whether the resource is Observed this cycle.
// v1b ships the tier thinning below; full thinning optimisation and the watch
// hybrid (§16.6) are v2 and plug in here without touching the loop.
//
// A resource that is not due is neither Observed nor Diffed this cycle.  It is
// NOT treated as unobservable either: its downstream proceeds, and prune never
// sees it as an orphan because it is still in desired.
type ObserveScheduler interface {
	// BeginCycle starts daemon cycle `cycle` with this cycle's desired set and
	// tier intervals (tier name → observe_every).  Entries for resources that
	// left desired are forgotten.
	BeginCycle(cycle int, desired []diff.DesiredResource, tiers map[string]int)
	// Due reports whether res must be Observed this cycle.
	Due(res diff.DesiredResource) bool
	// Observed records an Observe of res this cycle.  converged is true when
	// the Observe succeeded and the Diff produced no op to apply.
	Observed(res diff.DesiredResource, converged bool)
}

// TierScheduler thins Observes by tier (§16.5).  The default stays "every
// resource, every cycle" (§8); a resource in a tier with observe_every N > 1
// is Observed once every N cycles — but only while it is stable.  It is due
// in any cycle where:
//   - it has never been Observed by this daemon;
//   - its last Observe was not converged (error, not Ok, or a diff): it stays
//     on every-cycle until it converges, so a repair is never thinned;
//   - its desired changed since the last Observe (intent edit, external input,
//     allocation): cause A/E is acted on at once, not N cycles later;
//   - N cycles have passed since its last Observe.
//
// Only drift (cause B) on a stable resource is detected up to N-1 cycles
// late, which §16.5 accepts.
//
// The table is S3 (memory only, reset on restart): it records when this
// daemon last looked and a hash of the desired it looked with — never what
// reality looked like — so the loop stays observation-state-less (ADR-0002).
// A restart makes every resource due on the first cycle.
type TierScheduler struct {
	cycle int
	tiers map[string]int
	last  map[string]observeRecord
}

type observeRecord struct {
	cycle       int
	desiredHash string
	converged   bool
}

// NewTierScheduler returns an empty TierScheduler.
func NewTierScheduler() *TierScheduler {
	return &TierScheduler{last: make(map[string]observeRecord)}
}

// BeginCycle implements ObserveScheduler.
func (s *TierScheduler) BeginCycle(cycle int, desired []diff.DesiredResource, tiers map[string]int) {
	s.cycle = cycle
	s.tiers = tiers
	keep := make(map[string]bool, len(desired))
	for _, r := range desired {
		keep[r.ID.Local] = true
	}
	for local := range s.last {
		if !keep[local] {
			delete(s.last, local)
		}
	}
}

// Due implements ObserveScheduler.
func (s *TierScheduler) Due(res diff.DesiredResource) bool {
	every := s.tiers[res.Tier]
	if every <= 1 {
		return true
	}
	rec, ok := s.last[res.ID.Local]
	if !ok || !rec.converged || rec.desiredHash != desiredHash(res) {
		return true
	}
	return s.cycle-rec.cycle >= every
}

// Observed implements ObserveScheduler.
func (s *TierScheduler) Observed(res diff.DesiredResource, converged bool) {
	s.last[res.ID.Local] = observeRecord{cycle: s.cycle, desiredHash: desiredHash(res), converged: converged}
}

// desiredHash fingerprints everything in res that the Diff or the DAG reads.
func desiredHash(res diff.DesiredResource) string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			h.Write([]byte(p))
			h.Write([]byte{0})
		}
	}
	write(res.ID.String(), res.Tier, fmt.Sprint(res.DeletionPolicy), strings.Join(res.DependsOn, ","))
	keys := make([]string, 0, len(res.Fields))
	for k := range res.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		write(k, res.Fields[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}
