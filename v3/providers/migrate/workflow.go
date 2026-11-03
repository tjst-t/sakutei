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

package migrate

// workflow.go — v2 の Workflow を新しいコアの Procedure にする（§7.2：単純 Task・
// 多相 Task・Workflow は Procedure）。
//
//   - stage 1 つが手順 1 つ（stage ごとに gate と対象が違うため）。対象が 2 つ以上の
//     stage は、対象の宣言にラベル workflow・workflow-stage を付けて選ぶ。
//   - v2 は宣言してある間は済むまで走らせた。新しいコアは drift_fields（手順が
//     対象に当てる項目）がずれたら起こす（trigger: on_drift）。収束ループは
//     その項目を当てず手順に渡す（v2 が対象を Workflow に任せたのと同じ）。
//   - 前の stage の対象が宣言どおりになるまで次の stage を起こさない
//     （precondition。v2 の stage の順）。
//   - invariant.min_active は count(…) の invariant に、verify: invariant は
//     落とす（新しいコアは段を進める前に毎回 invariant を確かめる）。
//   - gate の承認者は名前の並び。v2 の roles は --role で名前に、ロールの無い gate
//     は --approver の名前にする。applicant_ne_approver は申請者を並びから外す。

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/internal/yaml"
)

type stage struct {
	name    string
	targets []string
	gate    *yaml.Node
}

func (c *conv) workflow(file string, i int, n *yaml.Node) []*obj {
	name, _ := str(n.Map["workflow"])
	at := fmt.Sprintf("%s: workflows[%d] %s", file, i, name)
	if name == "" {
		c.refuse("%s: workflow（名前）を書く", at)
		return nil
	}
	var provider string
	var scope map[string]string
	if t := n.Map["target"]; t != nil && t.Kind == yaml.Map {
		provider, _ = str(t.Map["provider"])
		scope, _ = strMap(t.Map["scope"])
	}
	typ, ok := typeOf(provider)
	if !ok {
		c.refuse("%s: target.provider %q は書き直せない", at, provider)
		return nil
	}
	applicant, _ := str(n.Map["applicant"])
	var stages []stage
	var steps []*yaml.Node
	var invariant string
	var doneWhen string
	for _, k := range n.Keys {
		v := n.Map[k]
		switch k {
		case "workflow", "target", "applicant":
		case "strategy":
			if o, _ := str(v.Map["order"]); o != "" && o != "one-at-a-time" {
				c.refuse("%s: strategy.order %q は書き直せない", at, o)
			}
		case "on_failure":
			if p, _ := str(v.Map["policy"]); p != "" && p != "halt" {
				c.refuse("%s: on_failure.policy %q は書き直せない（halt だけ）", at, p)
			}
		case "stages":
			for j, s := range v.Seq {
				st := stage{gate: s.Map["gate"]}
				st.name, _ = str(s.Map["name"])
				if st.name == "" {
					st.name = fmt.Sprint(j + 1)
				}
				if ts := s.Map["targets"]; ts != nil {
					for _, t := range ts.Seq {
						id, _ := str(t)
						st.targets = append(st.targets, id)
					}
				}
				stages = append(stages, st)
			}
		case "steps":
			steps = v.Seq
		case "invariant":
			invariant = c.invariant(at, typ, name, v)
		case "done_when":
			doneWhen = c.fieldsExpr(at, typ, v)
		case "trigger":
			c.refuse("%s: trigger（reimage）は新しいコアの Policy の allow に当たるが、まだ受け皿が無い", at)
		case "disruption":
			c.refuse("%s: disruption は新しいコアの手順の disruption に当たるが、まだ受け皿が無い", at)
		default:
			c.refuse("%s: %s は書き直せない", at, k)
		}
	}
	if len(stages) == 0 {
		c.refuse("%s: stages が無い", at)
		return nil
	}
	// 対象の宣言にラベルを付け、手順が当てる項目（drift_fields）を決める。
	targetRes := map[string]*v2res{}
	for _, st := range stages {
		for _, t := range st.targets {
			r := c.find(provider, scope, t)
			if r == nil || r.out == nil {
				c.refuse("%s: stage %s の対象 %s が宣言に無い", at, st.name, v2ID(provider, scope, t))
				continue
			}
			targetRes[t] = r
		}
	}
	c.dropStepResources(at, stages, steps, scope)
	applied := map[string]bool{}
	var stepsOut []any
	for j, s := range steps {
		if st := c.step(at, typ, j, s, applied); st != nil {
			stepsOut = append(stepsOut, st)
		}
	}
	var procs []*obj
	for si, st := range stages {
		p := newObj()
		pname := name
		if len(stages) > 1 {
			pname = name + "-" + st.name
		}
		p.Set("name", pname)
		target := newObj().Set("type", typ)
		if len(st.targets) == 1 {
			target.Set("id", st.targets[0])
		} else {
			target.Set("select", newObj().Set("labels", newObj().Set("workflow", name).Set("workflow-stage", st.name)))
		}
		p.Set("target", target)
		p.Set("trigger", "on_drift")
		var drift []any
		for _, t := range st.targets {
			r := targetRes[t]
			if r == nil {
				continue
			}
			if r.labels == nil {
				r.labels = map[string]string{}
			}
			r.labels["workflow"] = name
			if len(st.targets) > 1 {
				r.labels["workflow-stage"] = st.name
			}
			r.out.Set("labels", strObj(r.labels))
			if drift == nil {
				drift = driftFields(r, applied)
			}
		}
		if len(drift) == 0 {
			c.refuse("%s: stage %s の対象の宣言に、手順が当てる項目が無い（drift_fields を決められない）", at, st.name)
		}
		p.Set("drift_fields", drift)
		if si > 0 {
			if pre := c.stageDone(stages[si-1], targetRes, typ); pre != "" {
				p.Set("precondition", pre)
			}
		}
		if invariant != "" {
			p.Set("invariant", invariant)
		}
		var ss []any
		if st.gate != nil {
			if g := c.gate(at+" stage "+st.name, applicant, st.gate); g != nil {
				ss = append(ss, newObj().Set("name", "approve").Set("gate", g))
			}
		}
		ss = append(ss, stepsOut...)
		p.Set("steps", ss)
		if doneWhen != "" {
			p.Set("done_when", doneWhen)
		}
		p.Set("on_failure", "halt")
		procs = append(procs, p)
	}
	return procs
}

// dropStepResources は手順の段が当てる別の宣言（対象でないもの）を宣言から外す。
// v2 は Workflow が済むまでその宣言を収束から外したが、新しいコアは手順の対象
// だけを任せるので、残すと収束が手順の当てた値を宣言どおりに戻して取り合う
// （例：導入中の CD を抜く）。終わりの値は手順の段が当てる。
func (c *conv) dropStepResources(at string, stages []stage, steps []*yaml.Node, scope map[string]string) {
	for _, s := range steps {
		if s.Kind != yaml.Map || len(s.Keys) != 1 || s.Keys[0] != "apply" {
			continue
		}
		res, _ := str(s.Map["apply"].Map["resource"])
		provider, local, ok := strings.Cut(res, "/")
		if !ok {
			continue
		}
		for _, st := range stages {
			for _, t := range st.targets {
				r := c.find(provider, scope, strings.ReplaceAll(local, "{target}", t))
				if r == nil || r.out == nil {
					continue
				}
				c.note("%s: %s は手順の段が当てるので宣言から外した（新しいコアは手順の対象でない宣言を収束で戻すため。終わりの値は手順の最後の段が当てる）", at, r.v2id)
				r.out = nil
				delete(c.ids, r.v2id)
			}
		}
	}
}

func (c *conv) find(provider string, scope map[string]string, local string) *v2res {
	id := v2ID(provider, scope, local)
	for _, r := range c.resources {
		if r.v2id == id {
			return r
		}
	}
	return nil
}

// driftFields は手順が対象に当てる項目のうち、対象の宣言にあるもの。当てる項目
// が無ければ宣言の項目全部。
func driftFields(r *v2res, applied map[string]bool) []any {
	f, _ := r.out.Get("fields").(*obj)
	if f == nil {
		return nil
	}
	var out []any
	for _, k := range f.Keys {
		if applied[k] {
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		for _, k := range f.Keys {
			out = append(out, k)
		}
	}
	return out
}

// stageDone は stage の対象が全部宣言どおり（drift_fields が desired）かの式。
func (c *conv) stageDone(st stage, targets map[string]*v2res, typ string) string {
	var parts []string
	for _, t := range st.targets {
		r := targets[t]
		if r == nil {
			continue
		}
		f, _ := r.out.Get("fields").(*obj)
		if f == nil {
			continue
		}
		for _, k := range f.Keys {
			if s, ok := f.Vals[k].(string); ok {
				parts = append(parts, fmt.Sprintf("observe(%s/%s).%s == %s", typ, t, k, lit(s)))
			}
		}
	}
	return strings.Join(parts, " and ")
}

func lit(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

// invariant は v2 の invariant.min_active を count(…) の式にする。
func (c *conv) invariant(at, typ, wf string, n *yaml.Node) string {
	min := 0
	active := map[string]string{"active": "true"}
	for _, k := range n.Keys {
		switch k {
		case "min_active":
			min, _ = n.Map[k].Scalar.(int)
		case "active_when":
			m, ok := strMap(n.Map[k])
			if !ok || len(m) == 0 {
				c.refuse("%s: invariant.active_when は文字列のマップ", at)
				return ""
			}
			active = m
		default:
			c.refuse("%s: invariant.%s は書き直せない", at, k)
		}
	}
	if min <= 0 {
		return ""
	}
	var keys []string
	for k := range active {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var conds []string
	for _, k := range keys {
		conds = append(conds, fmt.Sprintf("it.%s == %s", k, lit(active[k])))
	}
	return fmt.Sprintf("count(%s[workflow=%s], %s) >= %d", typ, wf, strings.Join(conds, " and "), min)
}

// ref は v2 の参照（<provider>/<local>、{target} を含む）を新しい参照にする。
func (c *conv) ref(at, typ, s string) (string, bool) {
	if s == "" {
		return typ + "/{target}", true
	}
	p, local, ok := strings.Cut(s, "/")
	t, known := typeOf(p)
	if !ok || !known {
		c.refuse("%s: 参照 %q は書き直せない（<provider>/<id>、provider は %s）", at, s, strings.Join(known2(), ", "))
		return "", false
	}
	return t + "/" + local, true
}

func known2() []string {
	var s []string
	for _, k := range kinds {
		s = append(s, k.v2)
	}
	return s
}

// fieldsExpr は v2 の {resource, fields} を「全部の項目が等しい」式にする。
func (c *conv) fieldsExpr(at, typ string, n *yaml.Node) string {
	res, _ := str(n.Map["resource"])
	ref, ok := c.ref(at, typ, res)
	if !ok {
		return ""
	}
	f := n.Map["fields"]
	if f == nil || f.Kind != yaml.Map || len(f.Keys) == 0 {
		c.refuse("%s: fields を書く", at)
		return ""
	}
	if strings.HasPrefix(ref, "guest/os/") {
		c.instance(kindOf("guest-os"))
	}
	var parts []string
	for _, k := range f.Keys {
		s, _ := str(f.Map[k])
		parts = append(parts, fmt.Sprintf("observe(%s).%s == %s", ref, k, lit(s)))
	}
	return strings.Join(parts, " and ")
}

// step は v2 の段 1 つ（apply・wait・require・action・verify）を書き直す。
// applied には対象に当てる項目を足す。
func (c *conv) step(at, typ string, j int, n *yaml.Node, applied map[string]bool) *obj {
	at = fmt.Sprintf("%s steps[%d]", at, j)
	if n.Kind != yaml.Map || len(n.Keys) != 1 {
		c.refuse("%s: 段は apply・wait・require・action・verify のどれか 1 つ", at)
		return nil
	}
	k := n.Keys[0]
	v := n.Map[k]
	switch k {
	case "apply":
		ref := typ + "/{target}"
		fn := v
		if v.Map["fields"] != nil && v.Map["fields"].Kind == yaml.Map {
			res, _ := str(v.Map["resource"])
			var ok bool
			if ref, ok = c.ref(at, typ, res); !ok {
				return nil
			}
			fn = v.Map["fields"]
		}
		t, id := splitRef(ref)
		fs := newObj()
		for _, fk := range fn.Keys {
			s, _ := str(fn.Map[fk])
			fs.Set(fk, s)
			if ref == typ+"/{target}" {
				applied[fk] = true
			}
		}
		return newObj().Set("apply", newObj().Set("type", t).Set("id", id).Set("fields", fs))
	case "wait", "require":
		w := newObj().Set("when", c.fieldsExpr(at, typ, v))
		if to, _ := str(v.Map["timeout"]); to != "" {
			if _, err := time.ParseDuration(to); err != nil {
				c.refuse("%s: timeout %q", at, to)
			}
			w.Set("timeout", to)
		}
		return newObj().Set("wait", w)
	case "action":
		if v.Map["resource"] != nil {
			c.refuse("%s: action.resource（別の宣言への一回きりの操作）は書き直せない", at)
			return nil
		}
		op, _ := str(v.Map["op"])
		inv := newObj().Set("op", op)
		if a := v.Map["args"]; a != nil {
			m, ok := strMap(a)
			if !ok {
				c.refuse("%s: action.args は文字列のマップ", at)
				return nil
			}
			inv.Set("args", strObj(m))
		}
		return newObj().Set("invoke", inv)
	case "verify":
		c.note("%s: verify: invariant は落とした（新しいコアは段を進める前に毎回 invariant を確かめる）", at)
		return nil
	}
	c.refuse("%s: 段 %s は書き直せない", at, k)
	return nil
}

func splitRef(ref string) (typ, id string) {
	parts := strings.SplitN(ref, "/", 3)
	return parts[0] + "/" + parts[1], parts[2]
}

// gate は v2 の gate（manual_approval・timeout）を承認の段にする。
func (c *conv) gate(at, applicant string, n *yaml.Node) *obj {
	ma := n.Map["manual_approval"]
	if ma == nil {
		c.refuse("%s: gate に manual_approval が無い", at)
		return nil
	}
	need := 1
	var roles []string
	separation := false
	if s, ok := str(ma); ok {
		if s != "required" {
			c.refuse("%s: manual_approval %q は書き直せない", at, s)
			return nil
		}
		// requirements の無い gate はプロファイルの既定（enterprise は 2 人と
		// 申請者≠承認者）。
		if c.profileName() == "enterprise" {
			need, separation = 2, true
		}
	} else if req := ma.Map["requirements"]; req != nil {
		for _, k := range req.Keys {
			v := req.Map[k]
			switch k {
			case "quorum":
				need, _ = v.Map["n"].Scalar.(int)
			case "roles":
				for _, r := range v.Seq {
					s, _ := str(r)
					roles = append(roles, s)
				}
			case "separation":
				s, _ := str(v)
				if s != "applicant_ne_approver" {
					c.refuse("%s: separation %q は書き直せない", at, s)
				}
				separation = true
			default:
				c.refuse("%s: requirements.%s は書き直せない", at, k)
			}
		}
	}
	var names []string
	if len(roles) == 0 {
		names = append(names, c.in.Approvers...)
		if len(names) == 0 {
			c.refuse("%s: 承認者が分からない（新しいコアは承認者を名前で並べる。--approver で渡す）", at)
			return nil
		}
	}
	seen := map[string]bool{}
	for _, r := range roles {
		ms, ok := c.in.Roles[r]
		if !ok {
			c.refuse("%s: ロール %s の承認者が分からない（--role %s=<名前>,… で渡す）", at, r, r)
			return nil
		}
		for _, m := range ms {
			if !seen[m] {
				seen[m] = true
				names = append(names, m)
			}
		}
	}
	if separation && applicant != "" {
		var kept []string
		for _, m := range names {
			if m != applicant {
				kept = append(kept, m)
			}
		}
		names = kept
		c.note("%s: applicant_ne_approver：申請者 %s を承認者の並びから外した", at, applicant)
	}
	if need < 1 || need > len(names) {
		c.refuse("%s: %d 人の承認が要るが、承認者は %d 人（%s）", at, need, len(names), strings.Join(names, ", "))
		return nil
	}
	of := make([]any, len(names))
	for i, m := range names {
		of[i] = m
	}
	g := newObj().Set("approval", newObj().Set("quorum", newObj().Set("n", need).Set("of", of)))
	if to, _ := str(n.Map["timeout"]); to != "" {
		g.Set("timeout", to)
	}
	for _, k := range n.Keys {
		if k != "manual_approval" && k != "timeout" {
			c.refuse("%s: gate.%s は書き直せない", at, k)
		}
	}
	return g
}
