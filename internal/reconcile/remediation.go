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
	"fmt"

	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// Held is the outcome of HoldAlertOnly: what was held back as alert-only
// drift, and the prune Deletes left to run.
type Held struct {
	// DriftCount is the number of held drifted resources (and held orphans).
	DriftCount int
	// Events are the DriftWouldApply Events of the held ops; Message is the
	// drift event string ("would be UPDATE fake/x", ...).
	Events []core.Event
	// PruneOps are the prune Deletes outside the held scopes.
	PruneOps []core.Op
	// Locals are the Local IDs of the held resources (scope hold only; they
	// were marked in unobservable to hold their downstream).
	Locals map[string]bool
}

// HoldAll reports every drifted resource and orphan of a cycle whose run-wide
// decision is alert-only (§14.1).  Nothing is removed: the caller returns
// before Apply and prune.
func HoldAll(desired []diff.DesiredResource, plans map[string]core.Plan, unobservable map[string]bool,
	pruneOps []core.Op) Held {
	return hold(desired, plans, unobservable, pruneOps, func(core.ID) bool { return true }, false)
}

// HoldAlertOnly takes the resources and orphans of scopes declared
// `remediation: alert-only` (§14.3) out of this cycle's remediation; an empty
// scopes holds nothing.
//
// A held resource's plan is removed from plans and the resource is marked in
// unobservable so its downstream waits, as for any unconverged upstream.  It
// is still in desired, so it can never become a prune candidate.  Drift is
// reported, never carried: the next cycle re-observes it and decides afresh.
func HoldAlertOnly(desired []diff.DesiredResource, plans map[string]core.Plan, unobservable map[string]bool,
	pruneOps []core.Op, scopes []core.Scope) Held {
	return hold(desired, plans, unobservable, pruneOps, func(id core.ID) bool {
		for _, sc := range scopes {
			if id.Scope.Matches(sc) {
				return true
			}
		}
		return false
	}, true)
}

// HoldObserveOnly takes the resources (and any orphans) of the providers that
// declare Capabilities.ObserveOnly (§23.4) out of Apply and prune, by type:
// observeOnly names them.  As for a scope hold, a held resource's plan is
// removed, it is marked in unobservable (its downstream waits) and its drift
// is reported as DriftWouldApply Events — never applied, in any mode.
func HoldObserveOnly(desired []diff.DesiredResource, plans map[string]core.Plan, unobservable map[string]bool,
	pruneOps []core.Op, observeOnly func(providerName string) bool) Held {
	return hold(desired, plans, unobservable, pruneOps, func(id core.ID) bool {
		return observeOnly(id.Provider)
	}, true)
}

func hold(desired []diff.DesiredResource, plans map[string]core.Plan, unobservable map[string]bool,
	pruneOps []core.Op, held func(core.ID) bool, remove bool) Held {
	var h Held
	for _, res := range desired {
		local := res.ID.Local
		if unobservable[local] || !held(res.ID) {
			continue
		}
		plan, ok := plans[local]
		if !ok || len(plan.Ops) == 0 {
			continue
		}
		h.DriftCount++
		for _, op := range plan.Ops {
			h.Events = append(h.Events, wouldApply(op, fmt.Sprintf("would be %s %s", opKindLabel(op.Kind), op.Target)))
		}
		if remove {
			delete(plans, local)
			unobservable[local] = true
			if h.Locals == nil {
				h.Locals = map[string]bool{}
			}
			h.Locals[local] = true
		}
	}
	for _, op := range pruneOps {
		if held(op.Target) {
			h.DriftCount++
			h.Events = append(h.Events, wouldApply(op, fmt.Sprintf("DELETE %s (orphan; would be pruned)", op.Target)))
			continue
		}
		h.PruneOps = append(h.PruneOps, op)
	}
	return h
}

// wouldApply is the DriftWouldApply Event of a held op (§14.1, §14.3).
func wouldApply(op core.Op, msg string) core.Event {
	return core.Event{
		Kind:     core.EventDriftWouldApply,
		Severity: core.SeverityWarning,
		Resource: op.Target,
		Cause:    op.Cause,
		Message:  msg,
		Attrs:    map[string]string{"op": opKindLabel(op.Kind)},
		Text:     "DRIFT EVENT: " + msg,
	}
}

func opKindLabel(k core.OpKind) string {
	switch k {
	case core.OpKindCreate:
		return "CREATE"
	case core.OpKindUpdate:
		return "UPDATE"
	case core.OpKindDelete:
		return "DELETE"
	default:
		return "OP"
	}
}
