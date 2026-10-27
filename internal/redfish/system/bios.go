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

package system

// bios.go は Redfish の BMC provider のリソース「BIOS の設定」（provider
// redfish-bios、BiosProvider）。設計書 §29.1「Redfish の BIOS の設定」、
// Backlog #140。システム・仮想メディアと同じパッケージに別の型として置き、
// ノードごとの redfish.Client を共有する。標準階級で List と BulkObserve を
// 持たない（BIOS は BMC に前からあり、Sakutei が作ったり消したりしないので
// prune の対象にしない）。
//
// ID は "<ノード名>/bios"（例 "node1/bios"）。仮想メディアの "<ノード名>/cd" と
// 同じく、Local ID は intent 全体で一意なので、同じノードの「システム」（ID は
// ノード名）と並べて宣言できるようにする。
//
// 宣言するフィールド:
//
//   - name: ノード名（ID のノード名の部分と同じ）。
//   - attributes: 管理する BIOS の属性（JSON のオブジェクト、値は文字列・数・
//     真偽）。書いた属性だけを比べて合わせ、書かない属性は見ない・触らない。
//     "" か {} は何も管理しない。
//
// 観測（Read）: Systems/<id>/Bios の Attributes（今の値）と、その Settings
// （@Redfish.Settings の SettingsObject、次の再起動で効く保留中の変更）。
// attributes は今の値の全部（キー順の JSON）で、宣言した属性との比べ方は
// PlanRefiner（RefinePlan）が決める（宣言に無い属性の違いは差分から落とす）。
// 保留中の変更のうち今の値と違うもの（pending_attributes）か既定値への戻し
// （ResetBiosToDefaultsPending）があるあいだは Converging を返す: BMC は
// 次の再起動を待っている途中なので、同じ変更を重ねて出さない（§11）。
// 保留中の変更が宣言と違っても重ねない（ほかの道具が置いた変更と取り合わない）。
// 再起動で効けば次の周に今の値で比べ直し、違えばそこで PATCH する。
// Bios を持たない BMC は Ok で bios_present が false の観測になり、属性を宣言した
// ときだけ計画の時点のエラーになる。届かないのは Unknown（何も操作しない）。
//
// 属性の登録簿（AttributeRegistry）: Bios の AttributeRegistry の Id の登録簿を
// /redfish/v1/Registries から読み、Id ごとにノード別のインメモリ（S3）に持つ
// （登録簿は BMC の版で決まる定まった中身で、Id が変われば読み直す。観測した
// 現実の値ではない）。RefinePlan はそれで属性名・読み取り専用・型・選べる値・
// 長さ・範囲を確かめ、合わなければ計画の時点でエラーにする。登録簿を出さない
// BMC では、今の値に無い属性名と、今の値と JSON の型（文字列・数・真偽）が違う
// 値だけを断る（ほかは PATCH で BMC が断れば PermanentError）。
//
// Apply: Bios を読み直し、宣言した属性のうち「保留中があればその値、無ければ
// 今の値」と違うものだけを Bios/Settings に PATCH する。再起動はしない（反映には
// 再起動が要る。Workflow の手順の action reset で起こす）。PATCH のあと読み
// 直して、保留中にも今の値にも宣言の値が無ければ PermanentError（Settings を
// 報告しない BMC に毎周 PATCH を重ねない）。BIOS の設定の変更は破壊的に数えない。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/internal/redfish"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

const (
	// BiosName は BIOS の設定の provider 名（core.ID.Provider に入る）。
	BiosName = "redfish-bios"

	// BiosProviderVersion は diff / 正規化の版（§10.4）。
	BiosProviderVersion = BiosName + "-v1"
)

// BiosIDSuffix は BIOS の設定の ID の末尾（"<ノード名>/bios"）。
const BiosIDSuffix = "/bios"

// BiosID はノード node の BIOS の設定の ID を返す。
func BiosID(node string) string { return node + BiosIDSuffix }

// biosNode は BIOS の設定の ID からノード名を取り出す。
func biosNode(id string) (string, error) {
	node := strings.TrimSuffix(id, BiosIDSuffix)
	if node == id || node == "" {
		return "", fmt.Errorf("%s: id %q: want \"<node>%s\" (e.g. %q)", BiosName, id, BiosIDSuffix, BiosID("node1"))
	}
	return node, nil
}

// BIOS の設定のフィールド名（name は FieldName と同じ）。
const (
	FieldAttributes        = "attributes"
	FieldPendingAttributes = "pending_attributes"
	FieldAttributeRegistry = "attribute_registry"
	FieldBiosPresent       = "bios_present"
)

// registryEntry はノードの登録簿のインメモリの写し（S3）。reg が nil なら、
// その Id の登録簿を BMC が出していない。
type registryEntry struct {
	id  string
	reg map[string]redfish.RegistryAttribute
}

// BiosProvider は provider.Provider・IntentValidator・PlanRefiner を実装する。
type BiosProvider struct {
	clients map[string]redfish.Client

	mu         sync.Mutex
	registries map[string]registryEntry // ノード名 → 登録簿
}

var (
	_ provider.Provider        = (*BiosProvider)(nil)
	_ provider.IntentValidator = (*BiosProvider)(nil)
	_ provider.PlanRefiner     = (*BiosProvider)(nil)
)

// NewBios は clients（ノード名 → そのノードの BMC の Client）を使う
// BiosProvider を返す。
func NewBios(clients map[string]redfish.Client) *BiosProvider {
	cs := make(map[string]redfish.Client, len(clients))
	for k, v := range clients {
		cs[k] = v
	}
	return &BiosProvider{clients: cs, registries: map[string]registryEntry{}}
}

// Schema は §29.1「Redfish の BIOS の設定」。identity はノード名だけ。
func (p *BiosProvider) Schema() provider.Schema {
	return provider.Schema{Fields: map[string]provider.FieldSpec{
		FieldName:              {Kind: core.FieldKindString, Required: true, Identity: true},
		FieldAttributes:        {Kind: core.FieldKindString},
		FieldPendingAttributes: {Kind: core.FieldKindString, Computed: true},
		FieldAttributeRegistry: {Kind: core.FieldKindString, Computed: true},
		FieldBiosPresent:       {Kind: core.FieldKindBool, Computed: true},
	}}
}

// Capabilities は標準階級: List と BulkObserve は無く、消さない。保留中の変更が
// あるあいだは Converging を返す。
func (p *BiosProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		TrueObserve:     true,
		List:            false,
		BulkObserve:     false,
		Pruneable:       false,
		Converging:      true,
		ProviderVersion: BiosProviderVersion,
	}
}

// ---- Read ----

// Read はノード id の Bios と保留中の変更を観測する。desired を見ない（ADR-0002）。
func (p *BiosProvider) Read(ctx context.Context, id string) (core.Observation, error) {
	now := time.Now()
	unknown := core.Observation{Availability: core.AvailabilityUnknown, ObservedAt: now}
	node, err := biosNode(id)
	if err != nil {
		return unknown, provider.NewPermanentError(err)
	}
	c, ok := p.clients[node]
	if !ok {
		return unknown, provider.NewPermanentError(fmt.Errorf("%s: Read %q: no BMC is configured for this node", BiosName, id))
	}
	b, err := c.GetBios(ctx)
	if redfish.IsNoBios(err) {
		return core.Observation{Availability: core.AvailabilityOk, ObservedAt: now, State: core.State{
			FieldName:        {Kind: core.FieldKindString, Value: node},
			FieldBiosPresent: {Kind: core.FieldKindBool, Value: false},
		}}, nil
	}
	if err != nil {
		return unknown, classify(fmt.Errorf("%s: Read %q: %w", BiosName, id, err))
	}
	if err := p.loadRegistry(ctx, node, c, b.AttributeRegistry); err != nil {
		return unknown, classify(fmt.Errorf("%s: Read %q: attribute registry %q: %w", BiosName, id, b.AttributeRegistry, err))
	}
	pending := pendingChanges(b)
	str := func(v string) core.TypedValue { return core.TypedValue{Kind: core.FieldKindString, Value: v} }
	state := core.State{
		FieldName:              str(node),
		FieldAttributes:        str(canonAttrs(b.Attributes)),
		FieldPendingAttributes: str(canonAttrs(pending)),
		FieldAttributeRegistry: str(b.AttributeRegistry),
		FieldBiosPresent:       {Kind: core.FieldKindBool, Value: true},
	}
	avail := core.AvailabilityOk
	if len(pending) > 0 || b.ResetPending {
		// BMC は保留中の変更を次の再起動で効かせる途中。重ねない（§11）。
		avail = core.AvailabilityConverging
	}
	return core.Observation{Availability: avail, State: state, ObservedAt: now}, nil
}

// pendingChanges は保留中の変更のうち今の値と違うものを返す（Settings に今の
// 値と同じものを返す BMC もある）。
func pendingChanges(b redfish.Bios) map[string]any {
	out := map[string]any{}
	for k, v := range b.Pending {
		if cur, ok := b.Attributes[k]; !ok || canonValue(cur) != canonValue(v) {
			out[k] = v
		}
	}
	return out
}

// loadRegistry は node の登録簿（Id id）を、写しが無いか Id が変わったときだけ
// 読む。BMC が出していなければ（4xx）出していないと覚える。届かなければエラー。
func (p *BiosProvider) loadRegistry(ctx context.Context, node string, c redfish.Client, id string) error {
	if id == "" {
		return nil
	}
	p.mu.Lock()
	e, ok := p.registries[node]
	p.mu.Unlock()
	if ok && e.id == id {
		return nil
	}
	reg, err := c.GetAttributeRegistry(ctx, id)
	entry := registryEntry{id: id}
	switch {
	case err == nil:
		entry.reg = make(map[string]redfish.RegistryAttribute, len(reg.Attributes))
		for _, a := range reg.Attributes {
			entry.reg[a.AttributeName] = a
		}
	case redfish.IsRequest(err):
		// 登録簿を出さない BMC。
	default:
		return err
	}
	p.mu.Lock()
	p.registries[node] = entry
	p.mu.Unlock()
	return nil
}

// registry は node の登録簿（Id id）の写しを返す。無ければ nil。
func (p *BiosProvider) registry(node, id string) map[string]redfish.RegistryAttribute {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.registries[node]; ok && e.id == id {
		return e.reg
	}
	return nil
}

// ---- 値の扱い ----

// canonAttrs は属性の正規形（キー順の JSON、HTML を逃がさない）。空なら "{}"。
func canonAttrs(m map[string]any) string {
	if len(m) == 0 {
		return "{}"
	}
	return marshal(m)
}

// canonValue は 1 つの値の正規形（JSON）。
func canonValue(v any) string { return marshal(v) }

func marshal(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Sprintf("%v", v)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// ParseAttributes は attributes の宣言（JSON のオブジェクト、値は文字列・数・
// 真偽）を読む。"" は空。
func ParseAttributes(s string) (map[string]any, error) {
	if strings.TrimSpace(s) == "" {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(strings.NewReader(s))
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("attributes is not valid JSON: %v", err)
	}
	if dec.More() {
		return nil, errors.New("attributes has data after the JSON object")
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New(`attributes must be a JSON object such as {"ProcVirtualization": "Enabled"}`)
	}
	for k, v := range m {
		if strings.TrimSpace(k) == "" {
			return nil, errors.New("attributes: empty attribute name")
		}
		switch v.(type) {
		case string, float64, bool:
		default:
			return nil, fmt.Errorf("attributes: the value of %q must be a string, a number or a boolean", k)
		}
	}
	return m, nil
}

// jsonKind は値の JSON の型の名前。
func jsonKind(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	}
	return "object"
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---- 計画の時点の検査 ----

// ValidateIntent は現実を見ずに決まる誤りを、ロードの時点で弾く。
func (p *BiosProvider) ValidateIntent(localID string, fields map[string]string) error {
	node, err := biosNode(localID)
	if err != nil {
		return err
	}
	schema := p.Schema()
	names := make([]string, 0, len(fields))
	for k := range fields {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		spec, ok := schema.Fields[k]
		switch {
		case !ok:
			return fmt.Errorf("%s %q: unknown field %q", BiosName, localID, k)
		case spec.Computed:
			return fmt.Errorf("%s %q: field %q is observed only and cannot be declared", BiosName, localID, k)
		}
	}
	if v, ok := fields[FieldName]; ok && v != node {
		return fmt.Errorf("%s %q: name %q must equal the node name in the resource id (%q)", BiosName, localID, v, node)
	}
	if _, err := ParseAttributes(fields[FieldAttributes]); err != nil {
		return fmt.Errorf("%s %q: %v", BiosName, localID, err)
	}
	if _, ok := p.clients[node]; !ok {
		return fmt.Errorf("%s %q: no BMC is configured for node %q (add it to the BMC list, --redfish-config)", BiosName, localID, node)
	}
	return nil
}

// RefinePlan は attributes の差分を、宣言した属性だけの比べ方に直す（§28.4）。
//
//   - Bios を持たない BMC に属性を宣言していればエラー。
//   - 宣言した属性を登録簿（無ければ今の値）で確かめ、合わなければエラー。
//   - 宣言した属性がどれも今の値と同じなら attributes の差分を落とす（宣言に
//     無い属性の違いは見ない）。違うものがあれば、それを Diagnostic に書く。
func (p *BiosProvider) RefinePlan(localID string, observed core.State, desired map[string]string, ops []core.Op) ([]core.Op, error) {
	decl, declared := desired[FieldAttributes]
	if !declared {
		return ops, nil
	}
	want, err := ParseAttributes(decl)
	if err != nil {
		return nil, fmt.Errorf("%s %q: %v", BiosName, localID, err)
	}
	node, err := biosNode(localID)
	if err != nil {
		return nil, err
	}
	if present, ok := observed[FieldBiosPresent].Value.(bool); ok && !present {
		if len(want) == 0 {
			return dropField(ops, FieldAttributes), nil
		}
		return nil, fmt.Errorf("%s %q: the BMC of node %q has no Bios resource (the ComputerSystem has no Bios link); BIOS attributes cannot be managed on it", BiosName, localID, node)
	}
	current, err := ParseAttributes(fmt.Sprintf("%v", observed[FieldAttributes].Value))
	if err != nil {
		return nil, fmt.Errorf("%s %q: observed %v", BiosName, localID, err)
	}
	regID := fmt.Sprintf("%v", observed[FieldAttributeRegistry].Value)
	reg := p.registry(node, regID)
	var diffs []string
	for _, k := range sortedKeys(want) {
		if err := checkAttribute(k, want[k], current, reg, regID); err != nil {
			return nil, fmt.Errorf("%s %q: %v", BiosName, localID, err)
		}
		cur, ok := current[k]
		if ok && canonValue(cur) == canonValue(want[k]) {
			continue
		}
		from := "(not reported)"
		if ok {
			from = canonValue(cur)
		}
		diffs = append(diffs, fmt.Sprintf("%s: %s → %s", k, from, canonValue(want[k])))
	}
	if len(diffs) == 0 {
		return dropField(ops, FieldAttributes), nil
	}
	out := make([]core.Op, 0, len(ops))
	for _, op := range ops {
		for _, f := range op.Fields {
			if f.Field == FieldAttributes {
				op.Diagnostic = fmt.Sprintf("resource %s: BIOS attributes differ (%s); Bios/Settings will be PATCHed and apply at the next reboot",
					op.Target, strings.Join(diffs, ", "))
			}
		}
		out = append(out, op)
	}
	return out, nil
}

// dropField は ops から field の差分を落とす。
func dropField(ops []core.Op, field string) []core.Op {
	out := make([]core.Op, 0, len(ops))
	for _, op := range ops {
		var fields []core.FieldDiff
		for _, f := range op.Fields {
			if f.Field != field {
				fields = append(fields, f)
			}
		}
		op.Fields = fields
		out = append(out, op)
	}
	return out
}

// checkAttribute は属性 k の宣言の値 v を、登録簿 reg（nil なら今の値 current）で
// 確かめる。
func checkAttribute(k string, v any, current map[string]any, reg map[string]redfish.RegistryAttribute, regID string) error {
	if reg == nil {
		cur, ok := current[k]
		if !ok {
			return fmt.Errorf("BIOS attribute %q is not reported by the BMC", k)
		}
		if jsonKind(cur) != jsonKind(v) {
			return fmt.Errorf("BIOS attribute %q: the value %s is a %s, but the BMC reports a %s (%s)", k, canonValue(v), jsonKind(v), jsonKind(cur), canonValue(cur))
		}
		return nil
	}
	a, ok := reg[k]
	if !ok {
		return fmt.Errorf("BIOS attribute %q is not in the BMC's attribute registry %s", k, regID)
	}
	if a.ReadOnly || a.Immutable {
		return fmt.Errorf("BIOS attribute %q is read-only (attribute registry %s)", k, regID)
	}
	typeErr := func(want string) error {
		return fmt.Errorf("BIOS attribute %q: the value %s is a %s, the attribute registry %s says %s (want a %s)", k, canonValue(v), jsonKind(v), regID, a.Type, want)
	}
	switch a.Type {
	case "Enumeration":
		s, ok := v.(string)
		if !ok {
			return typeErr("string")
		}
		if len(a.Values) > 0 && !contains(a.Values, s) {
			return fmt.Errorf("BIOS attribute %q: %q is not one of the values the BMC offers (%s)", k, s, strings.Join(a.Values, ", "))
		}
	case "String":
		s, ok := v.(string)
		if !ok {
			return typeErr("string")
		}
		if a.MaxLength != nil && len(s) > *a.MaxLength {
			return fmt.Errorf("BIOS attribute %q: %q is longer than %d characters", k, s, *a.MaxLength)
		}
		if a.MinLength != nil && len(s) < *a.MinLength {
			return fmt.Errorf("BIOS attribute %q: %q is shorter than %d characters", k, s, *a.MinLength)
		}
	case "Integer":
		f, ok := v.(float64)
		if !ok || f != math.Trunc(f) {
			return typeErr("whole number")
		}
		if a.LowerBound != nil && f < *a.LowerBound {
			return fmt.Errorf("BIOS attribute %q: %s is below the lower bound %v", k, canonValue(v), *a.LowerBound)
		}
		if a.UpperBound != nil && f > *a.UpperBound {
			return fmt.Errorf("BIOS attribute %q: %s is above the upper bound %v", k, canonValue(v), *a.UpperBound)
		}
	case "Boolean":
		if _, ok := v.(bool); !ok {
			return typeErr("boolean")
		}
	case "Password":
		return fmt.Errorf("BIOS attribute %q is a password; it cannot be declared (intent holds no secret values, §20)", k)
	}
	return nil
}

// ---- Apply ----

// Apply は plan を適用する。Bios を読み直し、今の現実（保留中を含む）とずれて
// いる属性だけを PATCH する（同じ plan を 2 回適用しても結果は同じ）。
func (p *BiosProvider) Apply(ctx context.Context, plan core.Plan) error {
	for _, op := range plan.Ops {
		if err := p.applyOp(ctx, op); err != nil {
			return err
		}
	}
	return nil
}

func (p *BiosProvider) applyOp(ctx context.Context, op core.Op) error {
	id := op.Target.Local
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s: Apply %q: "+format, append([]any{BiosName, id}, args...)...)
	}
	node, err := biosNode(id)
	if err != nil {
		return provider.NewPermanentError(err)
	}
	if op.Kind == core.OpKindDelete {
		return provider.NewPermanentError(fail("BIOS settings are never deleted (remove the attributes from the declaration to stop managing them)"))
	}
	c, ok := p.clients[node]
	if !ok {
		return provider.NewPermanentError(fail("no BMC is configured for this node"))
	}
	decl, ok := "", false
	for _, f := range op.Fields {
		if f.Field == FieldAttributes {
			decl, ok = f.To, true
		}
	}
	if !ok {
		return nil
	}
	want, err := ParseAttributes(decl)
	if err != nil {
		return provider.NewPermanentError(fail("%v", err))
	}
	if len(want) == 0 {
		return nil
	}
	b, err := c.GetBios(ctx)
	if redfish.IsNoBios(err) {
		return provider.NewPermanentError(fail("the BMC has no Bios resource"))
	}
	if err != nil {
		return classify(fail("%w", err))
	}
	need := map[string]any{}
	for k, v := range want {
		if canonValue(effective(b, k)) != canonValue(v) {
			need[k] = v
		}
	}
	if len(need) == 0 {
		return nil
	}
	if err := c.PatchBiosSettings(ctx, need); err != nil {
		return classify(fail("PATCH %s %s: %w", b.SettingsPath, canonAttrs(need), err))
	}
	after, err := c.GetBios(ctx)
	if err != nil {
		return classify(fail("reading Bios/Settings back: %w", err))
	}
	var missing []string
	for _, k := range sortedKeys(need) {
		if canonValue(effective(after, k)) != canonValue(need[k]) {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return provider.NewPermanentError(fail("the BMC accepted the PATCH of %s but reports neither a pending nor a current value as declared for %s",
			b.SettingsPath, strings.Join(missing, ", ")))
	}
	return nil
}

// effective は属性 k の「次の再起動の後の値」: 保留中があればその値、無ければ
// 今の値（無ければ nil）。
func effective(b redfish.Bios, k string) any {
	if v, ok := b.Pending[k]; ok {
		return v
	}
	return b.Attributes[k]
}
