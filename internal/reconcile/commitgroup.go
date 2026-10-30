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

// commitgroup.go — commit_group（Backlog #199、§23.4）: provider が名指した
// group のリソース（例: 複数の台の同じ変更）を 1 つの Apply で当て、provider が
// 全部か何もしないかで確定する。コアの受け持ち:
//
//   - 読み込みの時点: group を割り当て（AssignCommitGroups）、group の中の依存
//     （直接でも間接でも）・scope をまたぐ group・strategy: task の member を
//     断り、provider にも確かめさせる（ValidateCommitGroup）。
//   - 波: member は同じ波に並ぶ（buildDAG が各 member に全 member の依存先を
//     持たせる）。
//   - 1 つでも行けない member（観測できない・止めている・上流が落ちた・
//     backoff・in-flight・予算・承認待ち）があれば、どの member も行かない
//     （一部だけの承認はさせない）。
//   - 行くときは、差分のある member の op をまとめて 1 回の Execute（op の
//     CommitGroup に group の名前）。結果は member ごと（provider.MemberError）
//     で、台ごとに CommitGroupApplied の Event を出す。

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/eventlog"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// commitGroupKey は group を provider ごとに分けたキー（"" は group に無い）。
func commitGroupKey(r diff.DesiredResource) string {
	if r.CommitGroup == "" {
		return ""
	}
	return r.ID.Provider + "/" + r.CommitGroup
}

// AssignCommitGroups は desired の各リソースに、その provider が名指す
// commit_group を書き込み（desired を書き換える）、group を確かめる。
// providerOf が nil を返すリソース（provider を作れない）は group に入れない。
func AssignCommitGroups(desired []diff.DesiredResource, providerOf func(diff.DesiredResource) provider.Provider) error {
	groups := map[string][]int{}
	var keys []string
	for i := range desired {
		r := &desired[i]
		r.CommitGroup = ""
		if r.Absent {
			continue
		}
		p := providerOf(*r)
		if p == nil {
			continue
		}
		r.CommitGroup = provider.CommitGroupOf(p, r.ID.Local, r.Fields)
		if k := commitGroupKey(*r); k != "" {
			if _, ok := groups[k]; !ok {
				keys = append(keys, k)
			}
			groups[k] = append(groups[k], i)
		}
	}
	if len(groups) == 0 {
		return nil
	}
	sort.Strings(keys)
	idx := make(map[string]int, len(desired))
	for i, r := range desired {
		idx[r.ID.Local] = i
	}
	for _, k := range keys {
		members := groups[k]
		first := desired[members[0]]
		in := map[int]bool{}
		var ids []string
		for _, m := range members {
			r := desired[m]
			in[m] = true
			ids = append(ids, r.ID.Local)
			if !r.ID.Scope.Equal(first.ID.Scope) {
				return fmt.Errorf("commit_group %q: %s and %s are in different scopes (%s, %s); a commit group lies in one scope", r.CommitGroup, first.ID, r.ID, first.ID.Scope, r.ID.Scope)
			}
			if r.UpdatePolicy.IsTask() {
				return fmt.Errorf("commit_group %q: %s has update_policy.strategy: %s; a member of a commit group is applied, not handed to a Task", r.CommitGroup, r.ID, r.UpdatePolicy.Strategy)
			}
		}
		for _, m := range members {
			if dep := dependsOnMember(desired, idx, m, in); dep >= 0 {
				return fmt.Errorf("commit_group %q: %s depends on %s (directly or not); the members of a commit group are committed together, so none may depend on another",
					first.CommitGroup, desired[m].ID, desired[dep].ID)
			}
		}
		sort.Strings(ids)
		if g, ok := providerOf(first).(provider.CommitGrouper); ok {
			if err := g.ValidateCommitGroup(first.CommitGroup, ids); err != nil {
				return fmt.Errorf("commit_group %q: %w", first.CommitGroup, err)
			}
		}
	}
	return nil
}

// dependsOnMember は desired[from] が（間接にでも）依存する in の member の
// 添字（無ければ -1）。
func dependsOnMember(desired []diff.DesiredResource, idx map[string]int, from int, in map[int]bool) int {
	seen := map[int]bool{from: true}
	stack := []int{from}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, dep := range desired[n].DependsOn {
			j, ok := idx[dep]
			if !ok || seen[j] {
				continue
			}
			if in[j] {
				return j
			}
			seen[j] = true
			stack = append(stack, j)
		}
	}
	return -1
}

// commitGroupMembers は desired の group ごとの member の添字（desired の順）。
func commitGroupMembers(desired []diff.DesiredResource) map[string][]int {
	out := map[string][]int{}
	for i, r := range desired {
		if k := commitGroupKey(r); k != "" {
			out[k] = append(out[k], i)
		}
	}
	return out
}

// HoldPartialCommitGroups は予算・承認で一部の member の op が止まった group の
// member を filtered から全部落とす（一部だけ当てない・一部だけの承認はさせない、
// #199）。planned は止める前の plans。落とした op の数を返し、group ごとに 1 行
// 理由を w に書く。
func HoldPartialCommitGroups(desired []diff.DesiredResource, planned, filtered map[string]core.Plan, unobservable map[string]bool, w io.Writer, prefix string) int {
	held := 0
	groups := commitGroupMembers(desired)
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		why := ""
		for _, m := range groups[k] {
			local := desired[m].ID.Local
			if unobservable[local] {
				continue // held at the wave with its own reason
			}
			if len(filtered[local].Ops) < len(planned[local].Ops) {
				why = local
				break
			}
		}
		if why == "" {
			continue
		}
		var names []string
		for _, m := range groups[k] {
			local := desired[m].ID.Local
			if n := len(filtered[local].Ops); n > 0 {
				held += n
				names = append(names, local)
				delete(filtered, local)
			}
		}
		if len(names) > 0 && w != nil {
			fmt.Fprintf(w, "%scommit_group %q: %s held too (%s's change is held by the budget or awaits approval; a commit group goes all or none)\n",
				prefix, desired[groups[k][0]].CommitGroup, strings.Join(names, ", "), why)
		}
	}
	return held
}

// groupWave は 1 つの波の group の扱い。
type groupWave struct {
	// hold は group の別の member のせいで行かない member の理由。
	hold map[int]string
	// launch は行く group ごとの、差分のある member の添字（desired の順）。
	launch map[string][]int
}

// planWaveGroups は波の group を決める: 行けない member（blocked が理由を
// 返す）が 1 つでもあり、ほかに差分のある member があれば、その member たちは
// 止める。そうでなければ差分のある member をまとめて行かせる。
func planWaveGroups(desired []diff.DesiredResource, wave []int, plans map[string]core.Plan, blocked func(int) string) groupWave {
	gw := groupWave{hold: map[int]string{}, launch: map[string][]int{}}
	byGroup := map[string][]int{}
	var keys []string
	for _, idx := range wave {
		if k := commitGroupKey(desired[idx]); k != "" {
			if _, ok := byGroup[k]; !ok {
				keys = append(keys, k)
			}
			byGroup[k] = append(byGroup[k], idx)
		}
	}
	for _, k := range keys {
		var withPlan []int
		block, blockedBy := "", ""
		for _, idx := range byGroup[k] {
			if why := blocked(idx); why != "" {
				if block == "" {
					block, blockedBy = why, desired[idx].ID.Local
				}
				continue
			}
			if len(plans[desired[idx].ID.Local].Ops) > 0 {
				withPlan = append(withPlan, idx)
			}
		}
		if len(withPlan) == 0 {
			continue
		}
		if block != "" {
			for _, idx := range withPlan {
				gw.hold[idx] = fmt.Sprintf("commit_group %q: held with %s (%s); a commit group goes all or none", desired[idx].CommitGroup, blockedBy, block)
			}
			continue
		}
		gw.launch[k] = withPlan
	}
	return gw
}

// groupPlan はまとめた plan（op の CommitGroup に group の名前）。
func groupPlan(desired []diff.DesiredResource, members []int, plans map[string]core.Plan) core.Plan {
	var out core.Plan
	for _, idx := range members {
		for _, op := range plans[desired[idx].ID.Local].Ops {
			op.CommitGroup = desired[idx].CommitGroup
			out.Ops = append(out.Ops, op)
		}
	}
	return out
}

// EmitCommitGroupApplied は group の Apply の台ごとの結果を Event で出す
// （#199）。err は Execute の返り。
func EmitCommitGroupApplied(w io.Writer, prefix string, desired []diff.DesiredResource, members []int, err error) {
	for _, idx := range members {
		r := desired[idx]
		outcome, sev, text := provider.CommitCommitted, core.SeverityInfo, "committed"
		merr := provider.MemberError(err, r.ID)
		if merr != nil {
			outcome, sev, text = provider.CommitNotCommitted, core.SeverityWarning, "not committed: "+merr.Error()
			if m, ok := commitMember(err, r.ID.Local); ok && m.Outcome == provider.CommitUnknown {
				outcome, text = provider.CommitUnknown, "unknown (whether it was committed is not known; the next cycle observes): "+merr.Error()
			}
		}
		eventlog.Emit(w, core.Event{
			Kind: core.EventCommitGroupApplied, Severity: sev, Resource: r.ID,
			Message: text,
			Attrs:   map[string]string{"group": r.CommitGroup, "outcome": string(outcome)},
			Text:    fmt.Sprintf("%scommit_group %q: %s: %s", prefix, r.CommitGroup, r.ID, text),
		})
	}
}

func commitMember(err error, local string) (provider.CommitMember, bool) {
	ge, ok := asCommitGroupError(err)
	if !ok {
		return provider.CommitMember{}, false
	}
	m, ok := ge.Members[local]
	return m, ok
}

func asCommitGroupError(err error) (*provider.CommitGroupError, bool) {
	var ge *provider.CommitGroupError
	if errors.As(err, &ge) {
		return ge, true
	}
	return nil, false
}
