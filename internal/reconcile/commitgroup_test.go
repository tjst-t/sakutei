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

// commitgroup_test.go — commit_group のコアの受け持ち（Backlog #199）。
//
// Tags:
//   [AC-199-2-1] group の中の依存（直接・間接）・scope をまたぐ group は断る。
//                provider の ValidateCommitGroup も呼ぶ
//   [AC-199-2-2] member は同じ波に並び、差分のある member は 1 回の Apply（op の
//                CommitGroup）。結果は member ごと
//   [AC-199-2-3] 1 つでも行けない member があれば、どの member も行かない
//   [AC-199-2-4] 予算・承認で一部が止まった group は全部止める

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// groupingProvider は fields の "g" を commit group とする provider。
type groupingProvider struct {
	refuse string
	asked  []string
}

func (p *groupingProvider) Schema() provider.Schema             { return provider.Schema{} }
func (p *groupingProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }
func (p *groupingProvider) Read(context.Context, string) (core.Observation, error) {
	return core.Observation{}, nil
}
func (p *groupingProvider) Apply(context.Context, core.Plan) error { return nil }
func (p *groupingProvider) CommitGroup(_ string, fields map[string]string) string {
	return fields["g"]
}
func (p *groupingProvider) ValidateCommitGroup(group string, ids []string) error {
	p.asked = append(p.asked, group+":"+strings.Join(ids, ","))
	if group == p.refuse {
		return errors.New("refused by the provider")
	}
	return nil
}

func gres(local, group string, deps ...string) diff.DesiredResource {
	r := dres(local, deps...)
	r.Fields = map[string]string{}
	if group != "" {
		r.Fields["g"] = group
	}
	return r
}

func assign(t *testing.T, p *groupingProvider, desired []diff.DesiredResource) error {
	t.Helper()
	return AssignCommitGroups(desired, func(diff.DesiredResource) provider.Provider { return p })
}

func TestAssignCommitGroups(t *testing.T) {
	// [AC-199-2-1]
	p := &groupingProvider{}
	ok := []diff.DesiredResource{gres("base", ""), gres("a", "g", "base"), gres("b", "g"), gres("c", "")}
	if err := assign(t, p, ok); err != nil {
		t.Fatalf("[AC-199-2-1] %v", err)
	}
	if ok[1].CommitGroup != "g" || ok[2].CommitGroup != "g" || ok[0].CommitGroup != "" {
		t.Fatalf("[AC-199-2-1] groups: %+v", ok)
	}
	if len(p.asked) != 1 || p.asked[0] != "g:a,b" {
		t.Fatalf("[AC-199-2-1] ValidateCommitGroup calls: %v", p.asked)
	}
	for name, desired := range map[string][]diff.DesiredResource{
		"direct":   {gres("a", "g"), gres("b", "g", "a")},
		"indirect": {gres("a", "g"), gres("x", "", "a"), gres("b", "g", "x")},
	} {
		err := assign(t, &groupingProvider{}, desired)
		if err == nil || !strings.Contains(err.Error(), "depends on") {
			t.Fatalf("[AC-199-2-1] %s: %v", name, err)
		}
	}
	other := gres("b", "g")
	other.ID.Scope = core.NewScope(map[string]string{"site": "x"})
	if err := assign(t, &groupingProvider{}, []diff.DesiredResource{gres("a", "g"), other}); err == nil || !strings.Contains(err.Error(), "different scopes") {
		t.Fatalf("[AC-199-2-1] scopes: %v", err)
	}
	if err := assign(t, &groupingProvider{refuse: "g"}, []diff.DesiredResource{gres("a", "g"), gres("b", "g")}); err == nil || !strings.Contains(err.Error(), "refused by the provider") {
		t.Fatalf("[AC-199-2-1] provider: %v", err)
	}
}

// recordingExec は Execute の plan を残し、errFor の error を返す。
type recordingExec struct {
	mu     sync.Mutex
	plans  []core.Plan
	errFor func(core.Plan) error
}

func (e *recordingExec) Execute(_ context.Context, _ provider.Provider, plan core.Plan) error {
	e.mu.Lock()
	e.plans = append(e.plans, plan)
	e.mu.Unlock()
	if e.errFor != nil {
		return e.errFor(plan)
	}
	return nil
}

func (e *recordingExec) targets() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, p := range e.plans {
		var ids []string
		for _, op := range p.Ops {
			ids = append(ids, op.Target.Local+"@"+op.CommitGroup)
		}
		out = append(out, strings.Join(ids, "+"))
	}
	return out
}

func updatePlan(locals ...string) map[string]core.Plan {
	out := map[string]core.Plan{}
	for _, l := range locals {
		out[l] = core.Plan{Ops: []core.Op{{Kind: core.OpKindUpdate, Target: core.NewID("fake", core.NewScope(nil), l)}}}
	}
	return out
}

func TestApplyWaves_CommitGroup(t *testing.T) {
	// [AC-199-2-2] base ← a; b has no dependency: with the group, b waits
	// for base too and goes with a in one Apply; c is not in the group.
	desired := []diff.DesiredResource{gres("base", ""), gres("a", "g", "base"), gres("b", "g"), gres("c", "")}
	if err := assign(t, &groupingProvider{}, desired); err != nil {
		t.Fatal(err)
	}
	exec := &recordingExec{}
	var out bytes.Buffer
	_, applied, skipped, err := applyWaves(context.Background(), nil, exec, desired, updatePlan("base", "a", "b", "c"), nil, nil, nil, &out)
	if err != nil || len(skipped) != 0 {
		t.Fatalf("[AC-199-2-2] %v %v", err, skipped)
	}
	got := strings.Join(exec.targets(), " | ")
	if got != "base@ | c@ | a@g+b@g" {
		t.Fatalf("[AC-199-2-2] Applies = %s", got)
	}
	if !applied["a"] || !applied["b"] {
		t.Fatalf("[AC-199-2-2] applied = %v", applied)
	}
	if n := strings.Count(out.String(), `commit_group "g"`); n != 2 || !strings.Contains(out.String(), "committed") {
		t.Fatalf("[AC-199-2-2] events:\n%s", out.String())
	}

	// Per member: a committed, b unknown.
	exec = &recordingExec{errFor: func(p core.Plan) error {
		if len(p.Ops) < 2 {
			return nil
		}
		return &provider.CommitGroupError{Group: "g", Members: map[string]provider.CommitMember{
			"a": {Outcome: provider.CommitCommitted},
			"b": {Outcome: provider.CommitUnknown, Err: provider.NewTransientError(errors.New("the outcome is unknown"))},
		}}
	}}
	out.Reset()
	_, applied, _, _ = applyWaves(context.Background(), nil, exec, desired, updatePlan("a", "b"), nil, nil, nil, &out)
	if !applied["a"] || applied["b"] {
		t.Fatalf("[AC-199-2-2] applied = %v", applied)
	}
	for _, want := range []string{`commit_group "g": fake//a: committed`, `commit_group "g": fake//b: unknown`, "apply fake//b: transient: the outcome is unknown"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("[AC-199-2-2] want %q in:\n%s", want, out.String())
		}
	}
}

func TestApplyWaves_CommitGroupHeldWhole(t *testing.T) {
	// [AC-199-2-3] b is unobservable: a (with a diff) does not go either.
	desired := []diff.DesiredResource{gres("a", "g"), gres("b", "g"), gres("c", "")}
	if err := assign(t, &groupingProvider{}, desired); err != nil {
		t.Fatal(err)
	}
	exec := &recordingExec{}
	var out bytes.Buffer
	_, _, skipped, _ := applyWaves(context.Background(), nil, exec, desired, updatePlan("a", "c"), map[string]bool{"b": true}, nil, nil, &out)
	if got := strings.Join(exec.targets(), " | "); got != "c@" {
		t.Fatalf("[AC-199-2-3] Applies = %s", got)
	}
	if fmt.Sprint(skipped) != "[a b]" || !strings.Contains(out.String(), `skip fake//a: commit_group "g": held with b (unobservable this cycle)`) {
		t.Fatalf("[AC-199-2-3] skipped %v\n%s", skipped, out.String())
	}

	// The daemon: b backing off holds a; nothing is in flight.
	ifl := NewInFlightSet()
	ifl.RecordFailure("b", provider.NewTransientError(errors.New("x")))
	exec = &recordingExec{}
	out.Reset()
	_, launched, _, skipped, _ := applyWavesAsync(context.Background(), nil, exec, ifl, desired, updatePlan("a", "b"), map[string]bool{}, nil, nil, nil, &out, nil, nil, nil, nil, nil)
	if launched != 0 || len(exec.targets()) != 0 || !strings.Contains(out.String(), "held with b") {
		t.Fatalf("[AC-199-2-3] async: launched %d %v skipped %v\n%s", launched, exec.targets(), skipped, out.String())
	}
	// Once b is out of backoff, both go in one goroutine.
	ifl.ClearBackoff("b")
	exec = &recordingExec{}
	var committed []string
	var mu sync.Mutex
	_, launched, _, _, _ = applyWavesAsync(context.Background(), nil, exec, ifl, desired, updatePlan("a", "b"), map[string]bool{}, nil, nil, nil, &out,
		nil, func(ops []core.Op) {
			mu.Lock()
			committed = append(committed, ops[0].Target.Local)
			mu.Unlock()
		}, nil, nil, nil)
	ifl.Wait(5 * time.Second)
	if launched != 2 || fmt.Sprint(exec.targets()) != "[a@g+b@g]" {
		t.Fatalf("[AC-199-2-2] async: launched %d %v", launched, exec.targets())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(committed) != 2 {
		t.Fatalf("[AC-199-2-2] async: committed %v", committed)
	}
}

func TestHoldPartialCommitGroups(t *testing.T) {
	// [AC-199-2-4]
	desired := []diff.DesiredResource{gres("a", "g"), gres("b", "g"), gres("c", "")}
	if err := assign(t, &groupingProvider{}, desired); err != nil {
		t.Fatal(err)
	}
	planned := updatePlan("a", "b", "c")
	filtered := updatePlan("a", "c") // b's op held by the budget
	var out bytes.Buffer
	if n := HoldPartialCommitGroups(desired, planned, filtered, nil, &out, "sakutei: "); n != 1 {
		t.Fatalf("[AC-199-2-4] held %d", n)
	}
	if _, ok := filtered["a"]; ok || len(filtered["c"].Ops) != 1 {
		t.Fatalf("[AC-199-2-4] filtered = %v", filtered)
	}
	if !strings.Contains(out.String(), `commit_group "g": a held too (b's change is held by the budget or awaits approval`) {
		t.Fatalf("[AC-199-2-4] %s", out.String())
	}
	// Nothing held: untouched.
	filtered = updatePlan("a", "b")
	if n := HoldPartialCommitGroups(desired, updatePlan("a", "b"), filtered, nil, nil, ""); n != 0 || len(filtered) != 2 {
		t.Fatalf("[AC-199-2-4] %d %v", n, filtered)
	}
}

func TestApplyWavesAsync_ConvergedMemberNotInFlight(t *testing.T) {
	// [AC-199-2-2] only a has a diff: b (converged) is not sent and its
	// dependent d is not held behind it.
	desired := []diff.DesiredResource{gres("a", "g"), gres("b", "g"), gres("d", "", "b")}
	if err := assign(t, &groupingProvider{}, desired); err != nil {
		t.Fatal(err)
	}
	ifl := NewInFlightSet()
	exec := &recordingExec{}
	var out bytes.Buffer
	_, launched, _, skipped, _ := applyWavesAsync(context.Background(), nil, exec, ifl, desired, updatePlan("a", "d"), map[string]bool{}, nil, nil, nil, &out, nil, nil, nil, nil, nil)
	ifl.Wait(5 * time.Second)
	if launched != 2 || len(skipped) != 0 {
		t.Fatalf("[AC-199-2-2] launched %d skipped %v\n%s", launched, skipped, out.String())
	}
	if got := fmt.Sprint(exec.targets()); got != "[a@g d@]" && got != "[d@ a@g]" {
		t.Fatalf("[AC-199-2-2] Applies %s", got)
	}
}
