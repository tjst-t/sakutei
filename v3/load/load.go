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

	"github.com/tjst-t/sakutei/v3/alloc"
	"github.com/tjst-t/sakutei/v3/condition"
	"github.com/tjst-t/sakutei/v3/config"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/expand"
	"github.com/tjst-t/sakutei/v3/graph"
	"github.com/tjst-t/sakutei/v3/intent"
	"github.com/tjst-t/sakutei/v3/record"
	"github.com/tjst-t/sakutei/v3/secret"
)

// Module は provider のモジュール。つなぎ方の設定の settings から provider を
// 作る（E-O4・E-S1・N-S2）。範囲は settings でなく Range から受け取る。
type Module func(settings map[string]any) (core.Provider, error)

// SinkModule は Event の送り先のモジュール。つなぎ方の設定の sinks.<名前>.settings
// から送り先を作る（E-H2・E-H3）。
type SinkModule func(settings map[string]any) (core.Sink, error)

// SecretModule は秘密の保管先のモジュール。つなぎ方の設定の
// secret_backends.<名前>.settings から保管先を作る（E-G3・E-O2・N-O2）。
type SecretModule func(settings map[string]any) (core.SecretBackend, error)

// Options は読み込みの入力。
type Options struct {
	IntentDir  string
	ConfigFile string
	Modules    map[string]Module
	Sinks      map[string]SinkModule
	// Expanders は Go で組み込んだ Expand（intent の expand: の go:<名前>、E-A11）。
	Expanders map[string]expand.Expander
	// Secrets は秘密の保管先のモジュール（file は初めから、E-G3）。
	Secrets map[string]SecretModule
	// Allocator は採番の空きの選び方。nil なら空きの先頭（E-T4・N-T5）。
	Allocator core.Allocator
	// RecordDir は前の周の目印（外部入力の件数）と採番の写しを読む Record（読む
	// だけ。空なら読まない）。
	RecordDir string
	// Checks は Condition の check(名前) で使う確かめ（E-B9）。登録していない名前は
	// 読み込みで断る。
	Checks map[string]core.CheckFunc
}

// Loaded は読み込みと検証を通った結果。Resources の Fields は Schema の正規形。
type Loaded struct {
	Config    *config.Config
	Providers map[string]core.Provider
	Resources []core.Resource
	Ranges    []core.Range
	Policies  []core.Policy
	// Procedures は一回きりの手順（§3.7）。
	Procedures []core.Procedure
	Cycle      core.CycleSettings   // 書かなければ既定の間隔
	Sinks      map[string]core.Sink // つなぎ方の設定の sinks の名前 → 送り先
	Checks     map[string]core.CheckFunc
	Warnings   []core.Diagnostic
	// Unknown は Expand・秘密の世代・採番で「分からない」にした宣言 → 理由
	// （観測も Apply もしない、下流は待つ、E-G4・E-K3）。
	Unknown map[core.ID]string
	// Incomplete は宣言が欠けているかもしれない provider → 理由（外部入力が
	// 届かない。その provider の範囲は消さない、E-K3・不変条件 26）。
	Incomplete map[string]string
	// Secrets は秘密の保管先（書き込みの文脈で値を取り出す、E-G2）。
	Secrets core.SecretBackends
	// IntentDir は intent のディレクトリ（採番のファイルを書き足す）。
	IntentDir string
	// Allocs は周がロックを持って書く採番（新しい値・写しの書き直し、N-B13）。
	Allocs map[string]alloc.Write
	// InputCounts は届いた外部入力の件数（Record の目印に書く、E-K3）。
	InputCounts map[string]int
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
	backends, bdiags := buildSecrets(cfg, opt.Secrets)
	diags = append(diags, bdiags...)
	if len(diags) > 0 {
		return nil, &core.RejectError{Diagnostics: diags}
	}
	docs, err := intent.LoadDir(opt.IntentDir)
	if err != nil {
		return nil, err
	}

	// 段 2：外部入力を読み、Expand を順に呼ぶ（§2.1）。
	var marks record.Marks
	if opt.RecordDir != "" {
		marks, _ = record.ReadMarks(opt.RecordDir) // 読めなければ前の件数を見ない（急減は判定しない）
	}
	inputs, counts, idiags := readInputs(cfg, docs, marks)
	if len(idiags) > 0 {
		return nil, &core.RejectError{Diagnostics: idiags}
	}
	execs := make(map[string]string, len(cfg.Expanders))
	for n, e := range cfg.Expanders {
		execs[n] = e.Exec
	}
	out, err := expand.Run(ctx, docs, inputs, opt.Expanders, execs)
	if err != nil {
		return nil, fmt.Errorf("expand: %w", err)
	}

	// 採番：intent のファイルの値を差し込み、無ければ空きを選ぶ（書くのは周）。
	reqs, adiags := alloc.Requests(out.Resources)
	out.Diagnostics = append(out.Diagnostics, adiags...)
	copies, err := alloc.ReadCopies(opt.RecordDir, out.Pools)
	if err != nil {
		return nil, fmt.Errorf("Record の採番の写し: %w", err)
	}
	aplan, adiags := alloc.Resolve(alloc.State{Pools: out.Pools, Files: out.Allocations, Copies: copies}, reqs, opt.Allocator)
	out.Diagnostics = append(out.Diagnostics, adiags...)
	out.Resources, adiags = alloc.Substitute(out.Resources, aplan)
	out.Diagnostics = append(out.Diagnostics, adiags...)

	// 秘密の世代（値は読まない、E-G2・E-G4）。
	kind := func(id core.ID) (core.KindSchema, bool) { ks, why := kindOf(id, providers); return ks, why == "" }
	out.Resources, adiags = secret.Stamp(ctx, out.Resources, kind, backends)
	out.Diagnostics = append(out.Diagnostics, adiags...)

	res, diags := Validate(out, providers, opt.Checks)
	var rejects, warns []core.Diagnostic
	unknown, incomplete := map[core.ID]string{}, map[string]string{}
	declared := map[string]bool{}
	for _, r := range res {
		declared[r.ID.String()] = true
	}
	for _, d := range diags {
		switch d.Severity {
		case core.Reject:
			rejects = append(rejects, d)
		case core.Uncertain:
			warns = append(warns, d)
			uncertain(d, declared, providers, unknown, incomplete)
		default:
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
		Procedures: out.Procedures, Cycle: cyc, Sinks: sinks, Checks: opt.Checks, Warnings: warns,
		Unknown: unknown, Incomplete: incomplete, Secrets: backends, IntentDir: opt.IntentDir, Allocs: aplan.Writes,
		InputCounts: counts}, nil
}

// uncertain は Uncertain の診断の Target を、宣言（Unknown にする）か provider・
// 種類（その provider の範囲を消さない）に振り分ける（E-G4・E-K3）。どれにも
// 当たらなければ、全部の provider の範囲を消さない（安全側）。
func uncertain(d core.Diagnostic, declared map[string]bool, providers map[string]core.Provider, unknown map[core.ID]string, incomplete map[string]string) {
	add := func(p string) {
		if incomplete[p] == "" {
			incomplete[p] = d.Message
		}
	}
	switch {
	case declared[d.Target]:
		id, _ := core.ParseRef(d.Target)
		if unknown[id] == "" {
			unknown[id] = d.Message
		}
	case providers[d.Target] != nil:
		add(d.Target)
	case core.ParseType(d.Target) == nil && providers[core.ID{Type: d.Target}.Provider()] != nil:
		add(core.ID{Type: d.Target}.Provider())
	default:
		for n := range providers {
			add(n)
		}
	}
}

func buildSecrets(cfg *config.Config, modules map[string]SecretModule) (core.SecretBackends, []core.Diagnostic) {
	out := core.SecretBackends{}
	var diags []core.Diagnostic
	for _, n := range sortedKeys(cfg.SecretBackends) {
		sc := cfg.SecretBackends[n]
		m, ok := modules[sc.Module]
		if !ok {
			diags = append(diags, reject(sc.Origin, "secret_backends."+n, "知らない保管先のモジュール %q（組み込まれているもの: %s）", sc.Module, strings.Join(sortedKeys(modules), ", ")))
			continue
		}
		b, err := m(sc.Settings)
		if err != nil {
			diags = append(diags, reject(sc.Origin, "secret_backends."+n, "%v", err))
			continue
		}
		out[n] = b
	}
	return out, diags
}

// readInputs は intent の inputs: に書いた外部入力を、つなぎ方の設定の在りかから
// 読む（E-K3・E-P4）。入力は最上位がマップの YAML か JSON。件数は items の
// リストの長さ（書かなければ最上位の項目の数）。読めない・形の誤りは届かない。expected_min より少ない・
// 前の周の件数から max_drop を超えて減ったものは「届いたが欠けている」で、どちらも
// Reachable を false にする（Expand はそれに依る宣言を作らない）。counts は届いた
// ものの件数（Record の目印に書く）。
func readInputs(cfg *config.Config, docs []intent.Document, marks record.Marks) (map[string]expand.ExternalInput, map[string]int, []core.Diagnostic) {
	inputs := map[string]expand.ExternalInput{}
	counts := map[string]int{}
	var diags []core.Diagnostic
	for _, doc := range docs {
		d, _ := intent.Decode(doc, topKeysOf(doc)...)
		for _, spec := range d.Inputs {
			if _, dup := inputs[spec.Name]; dup {
				diags = append(diags, reject(spec.Origin, "inputs", "外部入力 %q が 2 回書かれている", spec.Name))
				continue
			}
			ic, ok := cfg.Inputs[spec.Name]
			if !ok {
				diags = append(diags, reject(spec.Origin, "inputs", "外部入力 %q の在りかがつなぎ方の設定の inputs に無い", spec.Name))
				continue
			}
			in := expand.ExternalInput{}
			n, err := intent.ReadFile(ic.File)
			if err != nil {
				in.Note = "届かない: " + err.Error()
				inputs[spec.Name] = in
				continue
			}
			in.Data = n.Value()
			count := len(n.Keys)
			list, isList := in.Data.(map[string]any)[spec.Items].([]any)
			if spec.Items != "" {
				count = len(list)
			}
			prev, had := marks.InputCounts[spec.Name]
			switch {
			case spec.Items != "" && !isList:
				in.Note = fmt.Sprintf("届いたが形が違う（%s がリストでない）", spec.Items)
			case spec.ExpectedMin > 0 && count < spec.ExpectedMin:
				in.Note = fmt.Sprintf("届いたが欠けている（%d 件。expected_min %d）", count, spec.ExpectedMin)
			case spec.MaxDrop >= 0 && had && prev > 0 && (prev-count)*100 > prev*spec.MaxDrop:
				in.Note = fmt.Sprintf("届いたが欠けている（前の周の %d 件から %d 件に減った。max_drop %d%%）", prev, count, spec.MaxDrop)
			default:
				in.Reachable = true
				counts[spec.Name] = count
			}
			if !in.Reachable {
				in.Data = nil // 欠けた入力から宣言を作らせない
			}
			inputs[spec.Name] = in
		}
	}
	return inputs, counts, diags
}

// topKeysOf は文書の最上位の項目を全部（inputs: だけを読むので、ほかの項目を
// ここでは断らない）。
func topKeysOf(doc intent.Document) []string {
	if doc.Root == nil {
		return nil
	}
	return doc.Root.Keys
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
// （不変条件 25）。Expand の診断もそのまま含める。checks は Condition の check(名前)
// で使える確かめ。
func Validate(out expand.Output, providers map[string]core.Provider, checks map[string]core.CheckFunc) ([]core.Resource, []core.Diagnostic) {
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
		r, ds := checkResource(r, providers, checks)
		diags = append(diags, ds...)
		res = append(res, r)
	}
	diags = append(diags, checkCycles(res)...)
	diags = append(diags, checkGroups(res)...)
	diags = append(diags, checkRanges(res, out.Ranges, providers)...)
	diags = append(diags, checkPolicies(out.Policies, providers)...)
	diags = append(diags, checkProcedures(out.Procedures, res, providers, checks)...)
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

func checkResource(r core.Resource, providers map[string]core.Provider, checks map[string]core.CheckFunc) (core.Resource, []core.Diagnostic) {
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
	lookup := func(typ string) (core.KindSchema, string) { return kindOf(core.ID{Type: typ}, providers) }
	hasCheck := func(name string) bool { _, ok := checks[name]; return ok }
	for _, d := range r.DependsOn {
		if d.HasOn() {
			if _, why := kindOf(d.On, providers); why != "" {
				diags = append(diags, reject(r.Origin, target, "depends_on %s: %s", d.On, why))
			}
		}
		// when の参照を Schema と突き合わせる（E-M3：知らない種類・項目・確かめは断る）。
		// desired.項目 はこの宣言の項目。
		if d.When != nil {
			for _, msg := range condition.Lint(d.When, lookup, &ks, hasCheck) {
				diags = append(diags, reject(r.Origin, target, "depends_on の when %q: %s", d.When.String(), msg))
			}
		}
	}
	// 2 段の Apply は Transactor を持つ provider だけ（E-B3・E-B8）。確かめの式も
	// when と同じに突き合わせる（E-B9・N-B9）。
	if t := r.Txn(); t != nil {
		if _, ok := providers[r.ID.Provider()].(core.Transactor); !ok {
			diags = append(diags, reject(r.Origin, target, "provider %q は 2 段の Apply（apply.transaction）を持たない", r.ID.Provider()))
		}
		if ks.ObserveOnly {
			diags = append(diags, reject(r.Origin, target, "観測だけの種類に apply.transaction は書けない"))
		}
		for _, c := range t.Confirm {
			for _, msg := range condition.Lint(c.When, lookup, &ks, hasCheck) {
				diags = append(diags, reject(r.Origin, target, "confirm（%s）の when %q: %s", c.Phase, c.When.String(), msg))
			}
		}
	}
	return r, diags
}

// checkGroups は 2 段の Apply の group を確かめる（E-B9・N-B3）。同じ group は
// 同じ波で 1 回に当てるので、group の中の依存は断る（片方が済むまで待てば
// group ごと進めない）。
func checkGroups(res []core.Resource) []core.Diagnostic {
	g := graph.New(res)
	var diags []core.Diagnostic
	for _, r := range res {
		t := r.Txn()
		if t == nil || t.Group == "" {
			continue
		}
		for _, up := range g.Upstream(r.ID) {
			u := g.Resource(up)
			if u != nil && u.Txn() != nil && u.Txn().Group == t.Group {
				diags = append(diags, reject(r.Origin, r.ID.String(), "同じ transaction.group %q の %s に依っている（group の中の依存は書けない）", t.Group, up))
			}
		}
	}
	return diags
}

// checkCycles は宣言どうしの依存（depends_on の id と when が名指しする対象）の
// 循環を断る（E-J7・E-M3）。宣言に無い依存先（外の持ち主のもの）は辺にしない。
func checkCycles(res []core.Resource) []core.Diagnostic {
	g := graph.New(res)
	var diags []core.Diagnostic
	for _, path := range g.Cycles() {
		r := g.Resource(path[len(path)-1])
		diags = append(diags, reject(r.Origin, r.ID.String(), "depends_on が循環している: %s", graph.PathString(path)))
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
