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

package load

import (
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/v3/condition"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/graph"
)

// targetPlaceholder は読み込みのときに {target} の代わりに置く名前（式の形と
// 参照の種類・項目を確かめる）。
const targetPlaceholder = "_target_"

// checkProcedures は手順を断る（不変条件 15・25、E-M3）：同じ名前・知らない種類・
// 項目・操作・式の誤り、同じ項目を 2 つの手順が受け持つ、申請者が分からない
// separation の gate。
func checkProcedures(procs []core.Procedure, res []core.Resource, providers map[string]core.Provider, checks map[string]core.CheckFunc) []core.Diagnostic {
	var diags []core.Diagnostic
	seen := map[string]core.Origin{}
	g := graph.New(res)
	owner := map[string]string{} // 対象の ID と項目 → 受け持つ手順
	lookup := func(typ string) (core.KindSchema, string) { return kindOf(core.ID{Type: typ}, providers) }
	hasCheck := func(name string) bool { _, ok := checks[name]; return ok }
	for _, p := range procs {
		target := "procedure " + p.Name
		rej := func(format string, a ...any) { diags = append(diags, reject(p.Origin, target, format, a...)) }
		if first, dup := seen[p.Name]; dup {
			rej("同じ名前の手順が 2 つある（先は %s）", first)
		}
		seen[p.Name] = p.Origin
		ks, why := kindOf(core.ID{Type: p.Target.Type}, providers)
		if why != "" {
			rej("target の type %s: %s", p.Target.Type, why)
			continue
		}
		// 手順が受け持つ項目（E-F2・E-A5）。同じ対象の同じ項目を 2 つの手順が受け
		// 持てば、どちらが当てるかが決まらない。
		for _, f := range p.Trigger.Fields {
			if _, ok := ks.Fields[f]; !ok {
				rej("drift_fields の %q は種類 %q の項目に無い（ある項目: %s）", f, p.Target.Type, strings.Join(sortedKeys(ks.Fields), ", "))
			}
		}
		var targets []core.ID
		if p.Target.ID != "" {
			targets = []core.ID{{Type: p.Target.Type, Name: p.Target.ID}}
		} else {
			targets = g.Select(core.Selection{Type: p.Target.Type, Labels: p.Target.Labels})
		}
		if p.Trigger.Kind == core.TriggerOnDrift {
			for _, id := range targets {
				for _, f := range p.Trigger.Fields {
					k := id.String() + "." + f
					if other, ok := owner[k]; ok && other != p.Name {
						rej("%s の項目 %q を手順 %s も受け持つ", id, f, other)
					}
					owner[k] = p.Name
				}
			}
		}
		// separation の gate は申請者（sakutei start --as）が要る（E-L2・N-L2）。
		for _, s := range p.Steps {
			if s.Kind == core.StepGate && s.Gate != nil && s.Gate.Separation && p.Trigger.Kind != core.TriggerManual {
				rej("段 %s: separation: true の gate は trigger: manual の手順にだけ書ける（申請者が分からない）", s.Name)
			}
		}
		lint := func(what, src string) {
			if src == "" {
				return
			}
			c, err := condition.Parse(core.Substitute(src, targetPlaceholder, nil))
			if err != nil {
				rej("%s %q: %v", what, src, err)
				return
			}
			for _, msg := range condition.Lint(c, lookup, &ks, hasCheck) {
				rej("%s %q: %s", what, src, msg)
			}
		}
		lint("precondition", p.Precondition)
		lint("done_when", p.DoneWhen)
		lint("invariant", p.Invariant)
		for _, s := range p.Steps {
			lint("段 "+s.Name+" の skip_if", s.SkipIf)
			lint("段 "+s.Name+" の wait の when", s.When)
			switch s.Kind {
			case core.StepInvoke:
				// 一回きりの操作は Invoker を持つ provider の、種類の操作だけ（不変条件 15）。
				tid := core.ID{Type: p.Target.Type}
				inv, ok := providers[tid.Provider()].(core.Invoker)
				if !ok {
					rej("段 %s: provider %q は一回きりの操作（invoke）を持たない", s.Name, tid.Provider())
					continue
				}
				ops := inv.Ops(tid.Kind())
				if _, ok := ops[s.Op]; !ok {
					names := make([]string, 0, len(ops))
					for n := range ops {
						names = append(names, n)
					}
					sort.Strings(names)
					rej("段 %s: 種類 %q に操作 %q が無い（ある操作: %s）", s.Name, p.Target.Type, s.Op, strings.Join(names, ", "))
				}
			case core.StepApply:
				r := *s.Apply
				r.ID.Name = core.Substitute(r.ID.Name, targetPlaceholder, nil)
				f := core.Fields{}
				for k, v := range r.Fields {
					if str, ok := v.(string); ok && strings.Contains(str, "{") {
						continue // 対象ごとに置き換える値は当てるときに確かめる
					}
					f[k] = v
				}
				r.Fields = f
				_, ds := checkResource(r, providers, checks)
				for _, d := range ds {
					rej("段 %s の apply: %s", s.Name, d.Message)
				}
			case core.StepPolicy:
				for _, t := range s.Policy.Select.Types {
					if _, why := kindOf(core.ID{Type: t}, providers); why != "" {
						rej("段 %s の policy の type %s: %s", s.Name, t, why)
					}
				}
			}
		}
	}
	return diags
}
