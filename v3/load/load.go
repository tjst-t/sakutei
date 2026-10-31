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

// Package load は 1 周の段 1・2（設計 §2.1）を行う：つなぎ方の設定と intent を
// 読み、provider を組み、Expand を呼んで、その出力を検証する。断るときは
// *core.RejectError（終了コード 2）。
package load

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/v3/config"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/expand"
	"github.com/tjst-t/sakutei/v3/intent"
)

// Module は provider のモジュール。つなぎ方の設定の settings から provider を
// 作る（E-O4・E-S1・N-S2）。範囲は settings でなく Range から受け取る。
type Module func(settings map[string]any) (core.Provider, error)

// SinkModule は Event の送り先のモジュール。つなぎ方の設定の sinks.<名前>.settings
// から送り先を作る（E-H2・E-H3）。
type SinkModule func(settings map[string]any) (core.Sink, error)

// Options は読み込みの入力。
type Options struct {
	IntentDir  string
	ConfigFile string
	Modules    map[string]Module
	Sinks      map[string]SinkModule
	Expander   expand.Expander // nil なら expand.Passthrough
}

// Loaded は読み込みと検証を通った結果。Resources の Fields は Schema の正規形。
type Loaded struct {
	Config    *config.Config
	Providers map[string]core.Provider
	Resources []core.Resource
	Ranges    []core.Range
	Policies  []core.Policy
	Cycle     core.CycleSettings   // 書かなければ既定の間隔
	Sinks     map[string]core.Sink // つなぎ方の設定の sinks の名前 → 送り先
	Warnings  []core.Diagnostic
}

// Load は設定と intent を読み、検証する。
func Load(ctx context.Context, opt Options) (*Loaded, error) {
	cfg, err := config.Load(opt.ConfigFile)
	if err != nil {
		return nil, err
	}
	providers, diags := build(cfg, opt.Modules)
	sinks, sdiags := buildSinks(cfg, opt.Sinks)
	diags = append(diags, sdiags...)
	if len(diags) > 0 {
		return nil, &core.RejectError{Diagnostics: diags}
	}
	docs, err := intent.LoadDir(opt.IntentDir)
	if err != nil {
		return nil, err
	}
	ex := opt.Expander
	if ex == nil {
		ex = expand.Passthrough{}
	}
	out, err := ex.Expand(ctx, expand.Input{Intent: docs})
	if err != nil {
		return nil, fmt.Errorf("expand: %w", err)
	}
	res, diags := Validate(out, providers)
	var rejects, warns []core.Diagnostic
	for _, d := range diags {
		if d.Severity == core.Reject {
			rejects = append(rejects, d)
		} else {
			warns = append(warns, d)
		}
	}
	if len(rejects) > 0 {
		return nil, &core.RejectError{Diagnostics: rejects}
	}
	cyc := core.CycleSettings{Interval: core.DefaultInterval}
	if len(out.Cycles) > 0 {
		cyc = out.Cycles[0]
	}
	return &Loaded{Config: cfg, Providers: providers, Resources: res, Ranges: out.Ranges, Policies: out.Policies,
		Cycle: cyc, Sinks: sinks, Warnings: warns}, nil
}

func buildSinks(cfg *config.Config, modules map[string]SinkModule) (map[string]core.Sink, []core.Diagnostic) {
	names := make([]string, 0, len(cfg.Sinks))
	for n := range cfg.Sinks {
		names = append(names, n)
	}
	sort.Strings(names)
	sinks := make(map[string]core.Sink, len(names))
	var diags []core.Diagnostic
	for _, n := range names {
		sc := cfg.Sinks[n]
		m, ok := modules[sc.Module]
		if !ok {
			known := make([]string, 0, len(modules))
			for k := range modules {
				known = append(known, k)
			}
			sort.Strings(known)
			diags = append(diags, reject(sc.Origin, "sinks."+n, "知らない送り先のモジュール %q（組み込まれているもの: %s）", sc.Module, strings.Join(known, ", ")))
			continue
		}
		s, err := m(sc.Settings)
		if err != nil {
			diags = append(diags, reject(sc.Origin, "sinks."+n, "%v", err))
			continue
		}
		sinks[n] = s
	}
	return sinks, diags
}

func build(cfg *config.Config, modules map[string]Module) (map[string]core.Provider, []core.Diagnostic) {
	names := make([]string, 0, len(cfg.Providers))
	for n := range cfg.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	providers := make(map[string]core.Provider, len(names))
	var diags []core.Diagnostic
	for _, n := range names {
		pc := cfg.Providers[n]
		m, ok := modules[pc.Module]
		if !ok {
			diags = append(diags, reject(pc.Origin, "providers."+n, "知らないモジュール %q（組み込まれているもの: %s）", pc.Module, moduleNames(modules)))
			continue
		}
		p, err := m(pc.Settings)
		if err != nil {
			diags = append(diags, reject(pc.Origin, "providers."+n, "%v", err))
			continue
		}
		providers[n] = p
	}
	return providers, diags
}

func moduleNames(m map[string]Module) string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func reject(o core.Origin, target, format string, a ...any) core.Diagnostic {
	return core.Diagnostic{Severity: core.Reject, Origin: o, Target: target, Message: fmt.Sprintf(format, a...)}
}

// Validate は Expand の出力を検証し、Fields を正規形にした Resource を返す
// （不変条件 25）。Expand の診断もそのまま含める。
func Validate(out expand.Output, providers map[string]core.Provider) ([]core.Resource, []core.Diagnostic) {
	diags := append([]core.Diagnostic(nil), out.Diagnostics...)
	res := make([]core.Resource, 0, len(out.Resources))
	seen := map[core.ID]core.Origin{}
	for _, r := range out.Resources {
		target := r.ID.String()
		// 同じ ID は断る（E-M3・N-M3）。
		if first, dup := seen[r.ID]; dup {
			diags = append(diags, reject(r.Origin, target, "同じ ID が 2 回宣言されている（先は %s）", first))
			continue
		}
		seen[r.ID] = r.Origin
		r, ds := checkResource(r, providers)
		diags = append(diags, ds...)
		res = append(res, r)
	}
	diags = append(diags, checkCycles(res)...)
	diags = append(diags, checkRanges(res, out.Ranges, providers)...)
	diags = append(diags, checkPolicies(out.Policies, providers)...)
	// cycle: は intent に 1 か所だけ（E-M3：どちらが効くかを迷わせない）。
	for _, c := range out.Cycles[min(len(out.Cycles), 1):] {
		diags = append(diags, reject(c.Origin, "cycle", "cycle: が 2 か所に書かれている（先は %s）", out.Cycles[0].Origin))
	}
	return res, diags
}

// checkPolicies は同じ名前の Policy と、知らない種類に当てる select を断る
// （E-M3：当たらない Policy は書き誤り）。
func checkPolicies(ps []core.Policy, providers map[string]core.Provider) []core.Diagnostic {
	var diags []core.Diagnostic
	seen := map[string]core.Origin{}
	for _, p := range ps {
		target := "policy " + p.Name
		if first, dup := seen[p.Name]; dup {
			diags = append(diags, reject(p.Origin, target, "同じ名前の Policy が 2 つある（先は %s）", first))
		}
		seen[p.Name] = p.Origin
		for _, t := range p.Select.Types {
			id := core.ID{Type: t}
			if _, why := kindOf(id, providers); why != "" {
				diags = append(diags, reject(p.Origin, target, "select の type %s: %s", t, why))
			}
		}
	}
	return diags
}

// kindOf は ID の provider と種類を引く。無ければ断る理由を返す
// （E-M3：知らない provider・種類は未知の項目）。
func kindOf(id core.ID, providers map[string]core.Provider) (core.KindSchema, string) {
	p, ok := providers[id.Provider()]
	if !ok {
		return core.KindSchema{}, fmt.Sprintf("provider %q がつなぎ方の設定に無い", id.Provider())
	}
	k, ok := p.Schema().Kinds[id.Kind()]
	if !ok {
		return core.KindSchema{}, fmt.Sprintf("provider %q に種類 %q が無い", id.Provider(), id.Kind())
	}
	return k, ""
}

func checkResource(r core.Resource, providers map[string]core.Provider) (core.Resource, []core.Diagnostic) {
	var diags []core.Diagnostic
	target := r.ID.String()
	ks, why := kindOf(r.ID, providers)
	if why != "" {
		return r, []core.Diagnostic{reject(r.Origin, target, "%s", why)}
	}
	if r.Ensure == core.EnsureAbsent {
		// 観測だけの種類は消せない（E-B7）。消す宣言に比べる項目は無い（E-D3）。
		if ks.ObserveOnly {
			diags = append(diags, reject(r.Origin, target, "観測だけの種類に ensure: absent は書けない"))
		}
		if len(r.Fields) > 0 {
			diags = append(diags, reject(r.Origin, target, "ensure: absent に fields は書けない"))
		}
	}
	// Schema に無い項目・型の合わない値は断る（E-M3・E-B12）。
	norm := make(core.Fields, len(r.Fields))
	for _, k := range sortedKeys(r.Fields) {
		spec, ok := ks.Fields[k]
		if !ok {
			diags = append(diags, reject(r.Origin, target, "種類 %q に項目 %q が無い（ある項目: %s）", r.ID.Type, k, strings.Join(sortedKeys(ks.Fields), ", ")))
			continue
		}
		v, err := core.Normalize(spec.Type, r.Fields[k])
		if err != nil {
			diags = append(diags, reject(r.Origin, target, "項目 %q: %v", k, err))
			continue
		}
		norm[k] = v
	}
	if r.Fields != nil {
		r.Fields = norm
	}
	for _, d := range r.DependsOn {
		if _, why := kindOf(d.On, providers); why != "" {
			diags = append(diags, reject(r.Origin, target, "depends_on %s: %s", d.On, why))
		}
	}
	return r, diags
}

// checkCycles は宣言どうしの depends_on の循環を断る（E-J7・E-M3）。宣言に無い
// 依存先（外の持ち主のもの）は辺にしない。
func checkCycles(res []core.Resource) []core.Diagnostic {
	byID := make(map[core.ID]*core.Resource, len(res))
	for i := range res {
		byID[res[i].ID] = &res[i]
	}
	const (
		white = iota
		grey
		black
	)
	color := map[core.ID]int{}
	var diags []core.Diagnostic
	var stack []core.ID
	var visit func(id core.ID)
	visit = func(id core.ID) {
		color[id] = grey
		stack = append(stack, id)
		for _, d := range byID[id].DependsOn {
			if _, declared := byID[d.On]; !declared {
				continue
			}
			switch color[d.On] {
			case white:
				visit(d.On)
			case grey:
				var path []string
				for i := len(stack) - 1; i >= 0; i-- {
					path = append([]string{stack[i].String()}, path...)
					if stack[i] == d.On {
						break
					}
				}
				path = append(path, d.On.String())
				r := byID[d.On]
				diags = append(diags, reject(r.Origin, r.ID.String(), "depends_on が循環している: %s", strings.Join(path, " → ")))
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
	}
	for i := range res {
		if color[res[i].ID] == white {
			visit(res[i].ID)
		}
	}
	return diags
}

func checkRanges(res []core.Resource, ranges []core.Range, providers map[string]core.Provider) []core.Diagnostic {
	var diags []core.Diagnostic
	byProvider := map[string][]core.Range{}
	for _, rg := range ranges {
		if _, ok := providers[rg.Provider]; !ok {
			diags = append(diags, reject(rg.Origin, "ranges", "provider %q がつなぎ方の設定に無い", rg.Provider))
			continue
		}
		// 同じ引数の Range は重なる（E-K8・N-K7）。意味の上の重なりは provider の
		// Validator が断る。
		for _, other := range byProvider[rg.Provider] {
			if sameArgs(rg.Args, other.Args) {
				diags = append(diags, reject(rg.Origin, "ranges", "provider %q の範囲 %s が %s の範囲と重なる", rg.Provider, argsString(rg.Args), other.Origin))
			}
		}
		byProvider[rg.Provider] = append(byProvider[rg.Provider], rg)
		// 外の持ち主のものを宣言していれば断る（E-D2：触らないものを書き換え・
		// absent にしない）。
		for _, r := range res {
			if r.ID.Provider() != rg.Provider {
				continue
			}
			for _, e := range rg.ExternalOwners {
				if e.Matches(r.ID) {
					diags = append(diags, reject(r.Origin, r.ID.String(), "外の持ち主 %q のもの（%s の external_owners）を宣言している", e.Owner, rg.Origin))
				}
			}
		}
	}
	// provider 固有の検証（範囲の外の宣言・意味の上の重なり、E-I6・E-M3・E-K8）。
	names := make([]string, 0, len(providers))
	for n := range providers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		v, ok := providers[n].(core.Validator)
		if !ok {
			continue
		}
		var mine []core.Resource
		for _, r := range res {
			if r.ID.Provider() == n {
				mine = append(mine, r)
			}
		}
		diags = append(diags, v.Validate(mine, byProvider[n])...)
	}
	return diags
}

func sameArgs(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func argsString(a map[string]string) string {
	keys := sortedKeys(a)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + a[k]
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// IsReject は err が読み込みで断ったもの（終了コード 2）か。
func IsReject(err error) bool {
	var rej *core.RejectError
	return errors.As(err, &rej)
}
