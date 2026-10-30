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

// confirm.go — 変えたあとの確かめ（Backlog #200、§23.4）のコアの受け持ち:
// provider.Confirmer の実装。
//
//   - observe: は、intent が宣言するリソースだけを読む（宣言に無い ID は
//     読み込みで断る）。読みはそのリソースの provider の Read を、観測用の
//     資格（provider.WithObserve、§20.7）で 1 度呼ぶ。範囲（netconf の部分木と
//     機器の NACM など）はその provider がふだん観測するときのまま。
//   - expect: は、そのリソースの宣言に expect を重ねた desired を Plan（diff と
//     provider の PlanRefiner）に通し、expect の欄の差分だけを見る（比べ方は
//     ふだんの収束と同じ: netconf の config なら正規形と ordered）。
//   - check: は名前で登録された関数（pkg/cli.WithConfirmCheck と、
//     network_policy の realizer の Output.Checks）を呼ぶ。同じ名前を 2 度
//     登録すれば断る。

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/v2/internal/cause"
	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/netpolicy"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// ConfirmChecks は static（pkg/cli.WithConfirmCheck）に、network_policy の
// realizer が Output.Checks で渡した関数を足したもの。同じ名前は断る。
func ConfirmChecks(static map[string]provider.ConfirmCheck, np *netpolicy.Result) (map[string]provider.ConfirmCheck, error) {
	out := make(map[string]provider.ConfirmCheck, len(static))
	for n, f := range static {
		out[n] = f
	}
	if np == nil {
		return out, nil
	}
	for _, z := range np.Realizations {
		names := make([]string, 0, len(z.Output.Checks))
		for n := range z.Output.Checks {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			f := z.Output.Checks[n]
			if err := provider.ValidConfirmCheckName(n); err != nil || f == nil {
				return nil, fmt.Errorf("network_policy: target %s (%s): check %q: want a name of letters, digits, '_', '.', '-' and a non-nil function", z.Target.Name, z.Target.Realizer, n)
			}
			if _, dup := out[n]; dup {
				return nil, fmt.Errorf("network_policy: target %s (%s): check %q is registered twice (a check name must be unique among cli.WithConfirmCheck and the realizers' checks)", z.Target.Name, z.Target.Realizer, n)
			}
			out[n] = f
		}
	}
	return out, nil
}

// InstallConfirmer は desired の provider のうち provider.ConfirmUser に、
// この intent の Confirmer を渡す。ValidateIntent の前に呼ぶ（provider は
// 読み込みの時点で observe: と check: の名前を確かめる）。providerOf が nil を
// 返す名前（作れない provider）は飛ばす。
func InstallConfirmer(desired []diff.DesiredResource, providerOf func(name string) provider.Provider, checks map[string]provider.ConfirmCheck) {
	c := &confirmer{desired: map[core.IDKey]diff.DesiredResource{}, providerOf: providerOf, checks: checks}
	seen := map[string]bool{}
	for _, r := range desired {
		c.desired[r.ID.Key()] = r
		seen[r.ID.Provider] = true
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if u, ok := providerOf(n).(provider.ConfirmUser); ok {
			u.UseConfirmer(c)
		}
	}
}

// confirmer は 1 つの読み込みの intent の provider.Confirmer。
type confirmer struct {
	desired    map[core.IDKey]diff.DesiredResource
	providerOf func(name string) provider.Provider
	checks     map[string]provider.ConfirmCheck
}

var _ provider.Confirmer = (*confirmer)(nil)

func (c *confirmer) target(id core.ID, expect map[string]string) (diff.DesiredResource, provider.Provider, error) {
	r, ok := c.desired[id.Key()]
	if !ok {
		return r, nil, fmt.Errorf("observe:%s is not declared in the intent (observe: reads only a resource the intent declares, as its provider observes it, within its range)", id)
	}
	if r.Absent {
		return r, nil, fmt.Errorf("observe:%s is declared ensure: absent (there is nothing to expect of it)", id)
	}
	p := c.providerOf(id.Provider)
	if p == nil {
		return r, nil, fmt.Errorf("observe:%s: its provider %q cannot be built", id, id.Provider)
	}
	if len(expect) == 0 {
		return r, p, nil
	}
	schema := p.Schema().Fields
	names := make([]string, 0, len(expect))
	for f := range expect {
		names = append(names, f)
	}
	sort.Strings(names)
	for _, f := range names {
		spec, ok := schema[f]
		switch {
		case !ok:
			return r, nil, fmt.Errorf("observe:%s: expect: field %q is not a field of provider %s", id, f, id.Provider)
		case spec.Computed || spec.Managed:
			return r, nil, fmt.Errorf("observe:%s: expect: field %q is not compared by the core (computed or managed)", id, f)
		case spec.Secret:
			return r, nil, fmt.Errorf("observe:%s: expect: field %q is secret (its value is never compared)", id, f)
		}
	}
	return r, p, nil
}

// CheckObserve は読み込みの時点の observe: の確かめ（I/O をしない）。
func (c *confirmer) CheckObserve(id core.ID, expect map[string]string) error {
	_, _, err := c.target(id, expect)
	return err
}

// HasCheck は name の確かめの関数があるか。
func (c *confirmer) HasCheck(name string) bool {
	_, ok := c.checks[name]
	return ok
}

// Observe は id を観測用の資格で 1 度読み、expect どおりなら nil。
func (c *confirmer) Observe(ctx context.Context, id core.ID, expect map[string]string) error {
	r, p, err := c.target(id, expect)
	if err != nil {
		return err
	}
	obs, err := p.Read(provider.WithObserve(ctx), id.Local)
	if err != nil {
		return fmt.Errorf("could not be observed: %v", err)
	}
	switch {
	case obs.Availability != core.AvailabilityOk:
		return fmt.Errorf("is %s", obs.Availability)
	case obs.State == nil && obs.Check == nil:
		return fmt.Errorf("is absent")
	case len(expect) == 0:
		return nil
	}
	want := diff.DesiredResource{ID: id, Fields: map[string]string{}}
	for k, v := range r.Fields {
		want.Fields[k] = v
	}
	for k, v := range expect {
		want.Fields[k] = v
	}
	plan, err := diff.ForProvider(p).Plan(obs, want, cause.CauseContext{})
	if err != nil {
		return fmt.Errorf("cannot be compared: %v", err)
	}
	var fields []string
	diag := ""
	for _, op := range plan.Ops {
		for _, f := range op.Fields {
			if _, ok := expect[f.Field]; ok {
				fields = append(fields, f.Field)
				if diag == "" {
					diag = op.Diagnostic
				}
			}
		}
	}
	if len(fields) == 0 {
		return nil
	}
	sort.Strings(fields)
	why := fmt.Sprintf("%s not as expected", strings.Join(fields, ", "))
	if diag != "" {
		why += " (" + diag + ")"
	}
	return fmt.Errorf("%s", why)
}

// Check は name の確かめの関数を 1 度呼ぶ（観測の文脈で）。
func (c *confirmer) Check(ctx context.Context, name string, t provider.ConfirmTarget) error {
	f, ok := c.checks[name]
	if !ok {
		return fmt.Errorf("no check function is registered as %q", name)
	}
	return f(provider.WithObserve(ctx), t)
}
