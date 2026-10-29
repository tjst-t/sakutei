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

package proxmox

// fw.go — proxmox-fw provider: VM の FW の規則（/nodes/<n>/qemu/<id>/firewall/
// rules、Backlog #197）。網の間の許可の実現先 proxmox-fw（pkg/netpolicy）が
// この provider の resource を作る。手で宣言してもよい。
//
// 設計判断:
//
//   - 1 つの resource = 1 つの VM の Sakutei が置いた規則の並び。ID は
//     `<cluster>/<VM 名>/fw`（Local ID は provider をまたいで一意なので、
//     proxmox-vm の `<cluster>/<VM 名>` に `/fw` を足す）。
//   - Sakutei が置いた規則は comment の頭の印（pvefwrule.Marker）で見分け、
//     **印のある規則だけを書き換える**。印の無い規則（手で置いたもの・ほかの
//     道具のもの）は消さず、書き換えず、List が `<cluster>/<VM 名>/fw/<pos>` の
//     ID で返す（status の unmanaged に出る。prune しない型）。
//   - 規則の順は宣言どおり：Sakutei の規則は並びの先頭に宣言の順で置き、印の
//     無い規則はその後ろに元の順で残す。観測の rules は、先頭から最後の印の
//     ある規則までの並び（間に印の無い規則があれば "(unmanaged) <文>" として
//     含む）なので、順の違い・間に入った手の規則は drift になる。
//   - Apply は 1 手ずつ（読み直して digest を付けて）進める：先頭から見て
//     違う位置に、後ろにある同じ規則を動かす・要らない印の規則を書き換える・
//     作る（Proxmox は先頭に作るので、作った規則をその位置へ動かす）。最後に
//     余った印の規則を消す。規則を消して作り直さないので、変える間に許可が
//     抜ける時間を短くする。
//   - 規則を置く・変える・外す Op は破壊的（FW の変更、§13 の予算に数える）。
//   - VM が無い（まだ作っていない）ときの観測は「無い」、Apply は一時の失敗
//     （proxmox-vm が作るのを待つ）。VM の FW を有効にする（options の
//     enable・NIC の firewall=1）のは proxmox-vm の欄（#196）。

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/pvefwrule"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

const (
	// FWName は provider の名前。
	FWName = "proxmox-fw"
	// FWProviderVersion は規則の正規形の版（変えれば ghost diff を cause C に）。
	FWProviderVersion = FWName + "-v1"
	// FWSuffix は ID の末尾（`<cluster>/<VM 名>/fw`）。
	FWSuffix = "fw"
)

// proxmox-fw のフィールド。
const (
	// FieldRules は Sakutei が置く規則の並び（pvefwrule の文を " ; " でつなぐ）。
	FieldRules = "rules"
	// FieldUnmanaged は印の無い規則の数（観測だけ）。
	FieldUnmanaged = "unmanaged"
	// FieldRule・FieldPos は印の無い規則の ID の観測（その規則の文と位置）。
	FieldRule = "rule"
	FieldPos  = "pos"
)

// unmanagedPrefix は観測の rules の中の印の無い規則の頭。
const unmanagedPrefix = "(unmanaged) "

// FWProvider は provider.Provider・Lister・IntentValidator・PlanRefiner・
// OwnerFieldSelector を実装する。
type FWProvider struct {
	clusters map[string]Cluster
}

var (
	_ provider.Provider           = (*FWProvider)(nil)
	_ provider.Lister             = (*FWProvider)(nil)
	_ provider.IntentValidator    = (*FWProvider)(nil)
	_ provider.PlanRefiner        = (*FWProvider)(nil)
	_ provider.OwnerFieldSelector = (*FWProvider)(nil)
)

// NewFW は clusters（名前 → Cluster）を使う FWProvider を返す。
func NewFW(clusters map[string]Cluster) *FWProvider {
	cs := make(map[string]Cluster, len(clusters))
	for k, v := range clusters {
		cs[k] = v
	}
	return &FWProvider{clusters: cs}
}

// Schema: 書けるのは rules だけ。identity はクラスタと VM 名（ID から決まり、
// 観測だけ）。
func (p *FWProvider) Schema() provider.Schema {
	computed := func(k core.FieldKind) provider.FieldSpec { return provider.FieldSpec{Kind: k, Computed: true} }
	return provider.Schema{Fields: map[string]provider.FieldSpec{
		FieldRules:     {Kind: core.FieldKindString},
		FieldCluster:   {Kind: core.FieldKindString, Identity: true, Computed: true},
		FieldName:      {Kind: core.FieldKindString, Identity: true, Computed: true},
		FieldVMID:      computed(core.FieldKindInt),
		FieldNode:      computed(core.FieldKindString),
		FieldUnmanaged: computed(core.FieldKindInt),
		FieldRule:      computed(core.FieldKindString),
		FieldPos:       computed(core.FieldKindInt),
	}}
}

// Capabilities は標準の階級に List。規則は prune しない（印の無い規則は
// 消さない、印のある規則も宣言で消す）。
func (p *FWProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		TrueObserve:     true,
		List:            true,
		Pruneable:       false,
		ProviderVersion: FWProviderVersion,
	}
}

// FWLocalID はクラスタと VM 名から規則の並びの ID を作る。
func FWLocalID(cluster, vm string) string { return cluster + "/" + vm + "/" + FWSuffix }

// fwCoords は ID を読む。pos は印の無い規則の ID なら位置、規則の並びの ID
// なら -1。
func (p *FWProvider) fwCoords(localID string) (name string, c Cluster, vm string, pos int, err error) {
	parts := strings.Split(localID, "/")
	pos = -1
	switch {
	case len(parts) == 3 && parts[2] == FWSuffix:
	case len(parts) == 4 && parts[2] == FWSuffix:
		n, aerr := strconv.Atoi(parts[3])
		if aerr != nil || n < 0 || strconv.Itoa(n) != parts[3] {
			return "", Cluster{}, "", 0, fmt.Errorf("id %q: want <cluster>/<vm name>/fw[/<pos>]", localID)
		}
		pos = n
	default:
		return "", Cluster{}, "", 0, fmt.Errorf("id %q: want <cluster>/<vm name>/fw", localID)
	}
	name, vm = parts[0], parts[1]
	c, ok := p.clusters[name]
	if !ok {
		return "", Cluster{}, "", 0, fmt.Errorf("id %q: no cluster %q is configured (add it to the cluster list, proxmox_config)", localID, name)
	}
	if !vmName.MatchString(vm) {
		return "", Cluster{}, "", 0, fmt.Errorf("id %q: %q is not a valid VM name", localID, vm)
	}
	return name, c, vm, pos, nil
}

func (p *FWProvider) clusterNames() []string {
	names := make([]string, 0, len(p.clusters))
	for n := range p.clusters {
		names = append(names, n)
	}
	sortStrings(names)
	return names
}

// findFWVM は VM を引く。無ければ nil（エラーでない）。
func findFWVM(ctx context.Context, c Cluster, vm string) (*Resource, error) {
	if contains(c.ProtectedNames, vm) {
		return nil, fmt.Errorf("the VM is protected (protected_names): Sakutei never touches it")
	}
	res, err := c.resources(ctx)
	if err != nil {
		return nil, err
	}
	found := findVM(res, vm)
	switch {
	case len(found) == 0:
		return nil, nil
	case len(found) > 1:
		return nil, fmt.Errorf("%d VMs are named %s", len(found), vm)
	}
	r := found[0]
	if c.protected(r.VMID, r.Name) {
		return nil, fmt.Errorf("VM %d is protected: Sakutei never touches it", r.VMID)
	}
	if r.Template {
		return nil, fmt.Errorf("VM %d is a template, not a VM", r.VMID)
	}
	return &r, nil
}

// fwRule は観測した 1 つの規則。
type fwRule struct {
	pos    int
	rule   pvefwrule.Rule
	marked bool
	digest string
}

func readRules(ctx context.Context, c Cluster, r *Resource) ([]fwRule, error) {
	raw, err := c.Client.FirewallRules(ctx, r.Node, r.VMID)
	if err != nil {
		return nil, err
	}
	out := make([]fwRule, len(raw))
	for i, x := range raw {
		rule, marked := pvefwrule.FromAPI(x.Fields)
		out[i] = fwRule{pos: x.Pos, rule: rule, marked: marked, digest: x.Digest}
	}
	return out, nil
}

// observedBlock は観測の rules：先頭から最後の印のある規則まで（印の無い
// 規則は unmanagedPrefix を付けて）。
func observedBlock(rules []fwRule) string {
	last := -1
	for i, r := range rules {
		if r.marked {
			last = i
		}
	}
	parts := make([]string, 0, last+1)
	for _, r := range rules[:last+1] {
		if r.marked {
			parts = append(parts, r.rule.String())
		} else {
			parts = append(parts, unmanagedPrefix+r.rule.String())
		}
	}
	return strings.Join(parts, pvefwrule.Sep)
}

// Read は VM の規則を観測する。desired を見ない（ADR-0002）。
func (p *FWProvider) Read(ctx context.Context, localID string) (core.Observation, error) {
	name, c, vm, pos, err := p.fwCoords(localID)
	if err != nil {
		return obsOf(core.AvailabilityUnknown), provider.NewPermanentError(fmt.Errorf("%s: Read: %w", FWName, err))
	}
	r, err := findFWVM(ctx, c, vm)
	if err != nil {
		return obsOf(core.AvailabilityUnknown), classify(fmt.Errorf("%s: Read %q: %w", FWName, localID, err))
	}
	if r == nil {
		return obsOf(core.AvailabilityOk), nil
	}
	rules, err := readRules(ctx, c, r)
	if err != nil {
		return obsOf(core.AvailabilityUnknown), classify(fmt.Errorf("%s: Read %q: %w", FWName, localID, err))
	}
	now := time.Now()
	str := func(v string) core.TypedValue { return core.TypedValue{Kind: core.FieldKindString, Value: v} }
	num := func(n int) core.TypedValue { return core.TypedValue{Kind: core.FieldKindInt, Value: int64(n)} }
	st := core.State{
		FieldCluster: str(name), FieldName: str(vm), FieldVMID: num(r.VMID), FieldNode: str(r.Node),
	}
	if pos >= 0 {
		if pos >= len(rules) {
			return obsOf(core.AvailabilityOk), nil
		}
		text := rules[pos].rule
		if rules[pos].marked {
			text.Comment = pvefwrule.Mark(text.Comment)
		}
		st[FieldRule], st[FieldPos] = str(text.String()), num(pos)
		return core.Observation{Availability: core.AvailabilityOk, State: st, ObservedAt: now}, nil
	}
	unmanaged := 0
	for _, x := range rules {
		if !x.marked {
			unmanaged++
		}
	}
	st[FieldRules], st[FieldUnmanaged] = str(observedBlock(rules)), num(unmanaged)
	return core.Observation{Availability: core.AvailabilityOk, State: st, ObservedAt: now}, nil
}

// List は全クラスタの VM（テンプレートと触らない VM を除く）の、Sakutei の
// 規則の並び（印のある規則のある VM）と印の無い規則の 1 つずつを返す。
// どれかの VM の規則が読めなければ error（complete-or-error、§5.4）。
func (p *FWProvider) List(ctx context.Context, _ core.Scope) (core.ListResult, error) {
	var ids []core.ID
	for _, name := range p.clusterNames() {
		c := p.clusters[name]
		res, err := c.resources(ctx)
		if err != nil {
			return core.ListResult{}, classify(fmt.Errorf("%s: List %s: %w", FWName, name, err))
		}
		vms, err := managedVMs(c, res)
		if err != nil {
			return core.ListResult{}, provider.NewPermanentError(fmt.Errorf("%s: List %s: %w", FWName, name, err))
		}
		for i := range vms {
			rules, err := readRules(ctx, c, &vms[i])
			if err != nil {
				return core.ListResult{}, classify(fmt.Errorf("%s: List %s: VM %d: %w", FWName, name, vms[i].VMID, err))
			}
			set := FWLocalID(name, vms[i].Name)
			marked := false
			for _, r := range rules {
				if r.marked {
					marked = true
					continue
				}
				ids = append(ids, core.NewID(FWName, c.Scope, set+"/"+strconv.Itoa(r.pos)))
			}
			if marked {
				ids = append(ids, core.NewID(FWName, c.Scope, set))
			}
		}
	}
	return core.ListResult{IDs: ids, Status: core.ListComplete}, nil
}

// OwnerSelectFields は external_owners（§7.3）が観測の値で選べるフィールド。
func (p *FWProvider) OwnerSelectFields() []string { return []string{FieldVMID, FieldNode} }

// ValidateIntent は宣言を確かめる（I/O はしない）。
func (p *FWProvider) ValidateIntent(localID string, fields map[string]string) error {
	_, c, vm, pos, err := p.fwCoords(localID)
	if err != nil {
		return fmt.Errorf("%s: %w", FWName, err)
	}
	if pos >= 0 {
		return fmt.Errorf("%s %q: an id with a position names a rule Sakutei did not place (status lists it as unmanaged); declare the VM's rules as %s", FWName, localID, strings.Join(strings.Split(localID, "/")[:3], "/"))
	}
	if contains(c.ProtectedNames, vm) {
		return fmt.Errorf("%s %q: the VM is protected (protected_names): Sakutei never touches it", FWName, localID)
	}
	for k := range fields {
		if k != FieldRules {
			return fmt.Errorf("%s %q: field %q cannot be declared (only rules)", FWName, localID, k)
		}
	}
	if _, err := pvefwrule.ParseList(fields[FieldRules]); err != nil {
		return fmt.Errorf("%s %q: rules: %w", FWName, localID, err)
	}
	return nil
}

// RefinePlan は rules を正規形で比べ（書き方の違いを drift にしない）、残る
// Op を破壊的にする（FW の規則を置く・変える・外すのは FW の変更、§13）。
func (p *FWProvider) RefinePlan(localID string, observed core.State, desired map[string]string, ops []core.Op) ([]core.Op, error) {
	have := ""
	if tv, ok := observed[FieldRules]; ok {
		have = fmt.Sprintf("%v", tv.Value)
	}
	out := make([]core.Op, 0, len(ops))
	for _, op := range ops {
		var fields []core.FieldDiff
		for _, f := range op.Fields {
			if f.Field == FieldRules {
				want, err := pvefwrule.Canonical(f.To)
				if err != nil {
					return nil, fmt.Errorf("%s %q: rules: %w", FWName, localID, err)
				}
				if want == have {
					continue
				}
			}
			fields = append(fields, f)
		}
		op.Fields = fields
		op.Destructive = true
		if op.Diagnostic == "" || len(fields) > 0 {
			op.Diagnostic = fmt.Sprintf("resource %s: VM firewall rules change (counted as destructive, §13)", op.Target)
		}
		out = append(out, op)
	}
	return out, nil
}

// Apply は Op ごとに VM の規則を宣言の並びに合わせる（Delete は印のある
// 規則をすべて外す）。
func (p *FWProvider) Apply(ctx context.Context, plan core.Plan) error {
	for _, op := range plan.Ops {
		if err := p.applyOp(ctx, op); err != nil {
			return err
		}
	}
	return nil
}

func (p *FWProvider) applyOp(ctx context.Context, op core.Op) error {
	localID := op.Target.Local
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s: Apply %q: "+format, append([]any{FWName, localID}, args...)...)
	}
	_, c, vm, pos, err := p.fwCoords(localID)
	if err != nil {
		return provider.NewPermanentError(fail("%w", err))
	}
	if pos >= 0 {
		return provider.NewPermanentError(fail("a rule Sakutei did not place is never changed"))
	}
	var want []pvefwrule.Rule
	if op.Kind != core.OpKindDelete {
		text, declared := "", false
		for _, f := range op.Fields {
			if f.Field == FieldRules {
				text, declared = f.To, true
			}
		}
		if !declared {
			return nil
		}
		if want, err = pvefwrule.ParseList(text); err != nil {
			return provider.NewPermanentError(fail("rules: %w", err))
		}
	}
	r, err := findFWVM(ctx, c, vm)
	if err != nil {
		return classify(fail("%w", err))
	}
	if r == nil {
		if op.Kind == core.OpKindDelete {
			return nil
		}
		return provider.NewTransientError(fail("VM %s does not exist (yet): its firewall rules are placed once it is made", vm))
	}
	if err := converge(ctx, c, r, want); err != nil {
		return classify(fail("VM %d: %w", r.VMID, err))
	}
	return nil
}

// converge は VM r の規則を 1 手ずつ want に寄せる（毎手読み直し、digest を
// 付けて書く。間に誰かが変えれば Proxmox が断り、Apply は失敗する）。
func converge(ctx context.Context, c Cluster, r *Resource, want []pvefwrule.Rule) error {
	wantText := make([]string, len(want))
	for i, w := range want {
		wantText[i] = w.String()
	}
	limit := 0
	for step := 0; ; step++ {
		cur, err := readRules(ctx, c, r)
		if err != nil {
			return err
		}
		if step == 0 {
			limit = 4*(len(cur)+len(want)) + 8
		} else if step > limit {
			return fmt.Errorf("the rules did not settle after %d steps", limit)
		}
		digest := ""
		if len(cur) > 0 {
			digest = cur[0].digest
		}
		done, err := fwStep(ctx, c, r, cur, digest, want, wantText)
		if err != nil || done {
			return err
		}
	}
}

// fwStep は 1 手を打つ。合っていれば done。
func fwStep(ctx context.Context, c Cluster, r *Resource, cur []fwRule, digest string, want []pvefwrule.Rule, wantText []string) (bool, error) {
	text := make([]string, len(cur))
	for i, x := range cur {
		text[i] = x.rule.String()
	}
	at := func(i int, s string) bool { return i < len(cur) && cur[i].marked && text[i] == s }
	put := func(pos int, params url.Values) error {
		if digest != "" {
			params.Set("digest", digest)
		}
		return c.Client.UpdateFirewallRule(ctx, r.Node, r.VMID, pos, params)
	}
	for i := range want {
		if at(i, wantText[i]) {
			continue
		}
		// 後ろにある同じ規則を上へ動かす（moveto i で i に着く）。
		for j := i + 1; j < len(cur); j++ {
			if at(j, wantText[i]) {
				return false, put(j, url.Values{"moveto": {strconv.Itoa(i)}})
			}
		}
		// i の印の規則が後ろで要らなければ、書き換える。
		if i < len(cur) && cur[i].marked && !containsFrom(wantText[i+1:], text[i]) {
			params := want[i].Params()
			if del := want[i].Unset(cur[i].rule); len(del) > 0 {
				params.Set("delete", strings.Join(del, ","))
			}
			return false, put(i, params)
		}
		// 作る（Proxmox は先頭に入れる）。i へは moveto i+1 で着く。
		if err := c.Client.CreateFirewallRule(ctx, r.Node, r.VMID, want[i].Params()); err != nil {
			return false, err
		}
		if i == 0 {
			return false, nil
		}
		after, err := readRules(ctx, c, r)
		if err != nil {
			return false, err
		}
		if len(after) == 0 || !after[0].marked || after[0].rule.String() != wantText[i] {
			return false, nil // 間に変わった。次の手で読み直す。
		}
		digest = after[0].digest
		return false, put(0, url.Values{"moveto": {strconv.Itoa(i + 1)}})
	}
	// 余った印の規則を後ろから外す。
	for j := len(cur) - 1; j >= len(want); j-- {
		if cur[j].marked {
			return false, c.Client.DeleteFirewallRule(ctx, r.Node, r.VMID, j, digest)
		}
	}
	return true, nil
}

func containsFrom(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
