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

// Package acceptance / inflight_budget_test.go covers Backlog #13: the
// InFlightSet records whether each in-flight Apply is destructive, and the
// cycle rate limit (§13.1) counts only the destructive ones toward
// max_destructive_per_cycle.
//
// In-process like scoped_reconcile_test.go: the in-flight set is pre-populated
// to stand in for async Applies launched by earlier cycles, and the scoped
// reconcile path evaluates the shared budget against it.
//
//	AC-13-1  many in-flight Creates do not trip the destructive rate limit.
//	AC-13-2  an in-flight Delete is still counted as before.
package acceptance

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/budget"
	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/executor"
	"github.com/tjst-t/sakutei/v2/internal/fakeprovider"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// runScopedTombstoneWithInFlight runs a scoped reconcile whose only own op is
// the tombstone delete of "a" (one destructive op), while the resources in
// inFlight are already in flight with the given destructive flag.  The budget
// allows one destructive op per cycle.
func runScopedTombstoneWithInFlight(t *testing.T, fp *fakeprovider.Provider, inFlight map[string]bool) reconcile.ScopedReconcileResult {
	t.Helper()
	mustSeed(t, fp, "a", 1)

	oldDesired := []diff.DesiredResource{dres(scopeX(), "a", "1")}
	newDesired := []diff.DesiredResource{absentRes(scopeX(), "a")} // a → ensure: absent (#163)
	ifl := reconcile.NewInFlightSet()
	for local, destructive := range inFlight {
		newDesired = append(newDesired, dres(scopeX(), local, "1"))
		ifl.Add(local, destructive)
	}
	hint := core.ChangeHint{IntentRevision: "rev-new", TouchedScopes: []core.Scope{scopeX()}}
	spr := reconcile.ScopedPlan(hint, oldDesired, newDesired)

	be := budget.New(budget.Config{MaxDestructivePerCycle: 1})
	be.SetPlannedChanges([]budget.PlannedChange{deleteLane(scopeX())})
	var errBuf bytes.Buffer
	res := reconcile.ScopedReconcile(context.Background(), spr, fp, executor.New(),
		diff.New(fp.Schema()), be, ifl, causeA(), &errBuf)
	t.Logf("scoped log:\n%s", errBuf.String())
	return res
}

// [AC-13-1]
func TestInFlightBudget_CreatesDoNotHalt(t *testing.T) {
	fp := newFake(t)
	inFlight := map[string]bool{}
	for i := 0; i < 20; i++ {
		inFlight[fmt.Sprintf("c%d", i)] = false // in-flight Create
	}

	res := runScopedTombstoneWithInFlight(t, fp, inFlight)

	if res.BudgetHalted {
		t.Fatalf("[AC-13-1] 20 in-flight Creates must not count toward max_destructive_per_cycle=1; halted: %s", res.BudgetReason)
	}
	if mustExists(t, fp, "a") {
		t.Errorf("[AC-13-1] the tombstone delete fits the budget and must run")
	}
}

// [AC-13-2]
func TestInFlightBudget_DeleteStillCounted(t *testing.T) {
	fp := newFake(t)

	res := runScopedTombstoneWithInFlight(t, fp, map[string]bool{"d": true}) // in-flight Delete

	if !res.BudgetHalted {
		t.Fatalf("[AC-13-2] 1 in-flight Delete + 1 planned delete must exceed max_destructive_per_cycle=1")
	}
	if !mustExists(t, fp, "a") {
		t.Errorf("[AC-13-2] the tombstone delete must be held when the budget halts")
	}
}
