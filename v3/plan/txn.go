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

package plan

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/graph"
)

// ConfirmRefs は r の phase の check が観測する対象（E-B3・N-B9）。
func ConfirmRefs(r core.Resource, phase core.ConfirmPhase, g *graph.Graph) []core.ID {
	t := r.Txn()
	if t == nil {
		return nil
	}
	var out []core.ID
	for _, c := range t.Confirm {
		if c.Phase == phase {
			out = append(out, c.When.Refs(g.Select)...)
		}
	}
	return out
}

// confirmChecks は r の phase の confirm が使う check(名前)。
func confirmChecks(r core.Resource, phase core.ConfirmPhase) []string {
	t := r.Txn()
	if t == nil {
		return nil
	}
	var out []string
	for _, c := range t.Confirm {
		if c.Phase == phase {
			out = append(out, c.When.Checks()...)
		}
	}
	return out
}

// Confirmed は r の phase の confirm を今の観測で評価する（E-B3・E-B9・N-B9）。
// 全部が真のときだけ真。不明は通らない（確かでなければ確定しない、不変条件 17）。
// 通らなければ、通らない式と check の理由を返す。checks は登録した check の関数で、
// この呼び出しで 1 回ずつ呼ぶ。
func Confirmed(ctx context.Context, r core.Resource, phase core.ConfirmPhase, g *graph.Graph,
	obs map[core.ID]core.Observation, checks map[string]core.CheckFunc) (bool, string) {
	env := &condEnv{obs: obs, g: g, desired: r.Fields, checks: callChecks(ctx, confirmChecks(r, phase), checks, obs)}
	return confirmedIn(r, phase, env)
}

func confirmedIn(r core.Resource, phase core.ConfirmPhase, env *condEnv) (bool, string) {
	t := r.Txn()
	if t == nil {
		return true, ""
	}
	env.desired = r.Fields
	var failed []string
	for _, c := range t.Confirm {
		if c.Phase != phase {
			continue
		}
		switch c.When.Eval(env) {
		case core.TriTrue:
		case core.TriFalse:
			failed = append(failed, fmt.Sprintf("%q が偽%s", c.When.String(), env.checkNotes(c.When)))
		default:
			failed = append(failed, fmt.Sprintf("%q が分からない%s", c.When.String(), env.checkNotes(c.When)))
		}
	}
	return len(failed) == 0, strings.Join(failed, "・")
}

// callChecks は names の check を 1 回ずつ呼ぶ。登録の無い名前は不明。
func callChecks(ctx context.Context, names []string, funcs map[string]core.CheckFunc, obs map[core.ID]core.Observation) map[string]checkResult {
	out := map[string]checkResult{}
	view := &condEnv{obs: obs}
	for _, name := range names {
		if _, done := out[name]; done {
			continue
		}
		f, ok := funcs[name]
		if !ok {
			out[name] = checkResult{core.TriUnknown, "登録した check に無い"}
			continue
		}
		t, why := f(ctx, view)
		out[name] = checkResult{t, why}
	}
	return out
}

// holdGroups は two-phase Apply の group のうち、1 つでも Apply できない（待つ・
// 承認待ち・report・halt した）変更があれば、group の残りも Apply しない（E-B9・
// N-B3：全部で check が通ったときだけ確定する group を、欠けたまま確定しない）。
func holdGroups(out *Result, byID map[core.ID]core.Resource) {
	blocked := map[string]string{}
	for _, c := range out.Changes {
		r, ok := byID[c.ID]
		if !ok || r.Txn() == nil || r.Txn().Group == "" || !c.Op.Writes() || c.Action == ActApply {
			continue
		}
		g := r.Txn().Group
		if _, seen := blocked[g]; !seen {
			blocked[g] = c.Resource
		}
	}
	if len(blocked) == 0 {
		return
	}
	for i := range out.Changes {
		c := &out.Changes[i]
		r, ok := byID[c.ID]
		if !ok || c.Action != ActApply || r.Txn() == nil {
			continue
		}
		if who, hit := blocked[r.Txn().Group]; hit {
			c.Action, c.Blocked = ActWait, fmt.Sprintf("transaction.group %s の %s が Apply できないので group ごと待つ", r.Txn().Group, who)
		}
	}
}

// TxnUnits は Apply する変更（idx は Changes の番号）を two-phase Apply の単位に
// まとめる。two-phase Apply しないものは plain に返す。単位は名前順。
func TxnUnits(r Result, idx []int, byID map[core.ID]*core.Resource) (plain []int, units [][]int) {
	byKey := map[string][]int{}
	for _, i := range idx {
		res := byID[r.Changes[i].ID]
		if res == nil || res.Txn() == nil {
			plain = append(plain, i)
			continue
		}
		k := res.TxnKey()
		byKey[k] = append(byKey[k], i)
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		units = append(units, byKey[k])
	}
	return plain, units
}
