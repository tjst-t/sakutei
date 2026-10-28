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

// Package system は Redfish の BMC provider のリソース「システム」（電源・
// 起動先の上書き）と「仮想メディア」（CD、media.go の MediaProvider）。設計書 v3.15 §29.1「Redfish の BMC provider」の（2）〜（5）、
// Backlog #48。templates/provider/full から作った標準階級の provider で、
// List と BulkObserve を持たない（サーバは Sakutei が作ったり消したりしない
// ので prune の対象にしない）。
//
// ID はノード名（物理トポロジの座標、§28.4）。BMC のアドレスと資格は provider
// に渡す Client（ノードごと）の側の設定で、intent には書かない。
//
// 宣言するフィールド:
//
//   - power: On / Off。Off の観測で On なら Reset On、On の観測で Off なら
//     GracefulShutdown。PoweringOn / PoweringOff のあいだは Converging（操作を
//     重ねない）。BMC に届かなければ Unknown（何も操作しない）。
//   - boot_enabled: Continuous / Disabled。Once は計画の時点でエラー（1 回起動
//     すると BMC が Disabled に戻すので収束の対象にならない。Workflow の手順の
//     action boot_once で施す、下の「一度きりの操作」）。
//   - boot_target: boot_enabled が Continuous のときだけ比べる。BMC の
//     @Redfish.AllowableValues に無ければ計画の時点でエラー。起動先は PATCH
//     だけで、再起動はしない。
//   - power_off: graceful（既定）/ graceful_then_force。graceful は ForceOff に
//     進まず、止まらなければ GracefulShutdown を送り直す（ずれの level が続いて
//     stuck に上がる、§22.3）。graceful_then_force は最初の GracefulShutdown から
//     force_after を過ぎても止まらなければ ForceOff を送る。ForceOff の Op は
//     破壊的（Destructive）で、予算（§13）に数える。
//   - force_after: 時間（例 "5m"）。graceful_then_force のときだけ書け、そのとき
//     は必須。
//
// power_off と force_after は「どう止めるか」の宣言で、BMC には無い。Read は
// 返さず、PlanRefiner（RefinePlan）が差分から落とす。
//
// 観測だけの値（Computed、Diff しない・識別子にしない）: uuid・macs・
// cpu_count・memory_gib・boot_mode・power_state（生の PowerState）と、計画の
// 時点の検査に使う boot_target_allowable・reset_type_allowable。
//
// 再起動（ForceRestart・GracefulRestart）は「あるべき状態」ではなく一度きりの
// 操作なので宣言できない。
//
// 一度きりの操作（provider.Actioner、Workflow の action 手順、#139）:
//
//   - boot_once（args: target。必須）: 起動先の上書きを Once・target にする
//     （PATCH だけで再起動しない）。target は BMC の AllowableValues にあるもの。
//     BMC は次の 1 回の起動で上書きを Disabled に戻すので、上書きは残らない。
//     PATCH のあと読み直して Once・target になっていなければエラー。
//   - reset（args: type。既定 GracefulRestart、ほかに ForceRestart）: 電源が
//     On なら type で再起動し、Off なら On を送る（切れた台の再起動は電源
//     オン）。ForceRestart は破壊的（ActionPlan.Destructive、予算 §13 に数える）。
//     電源の切り替えの途中なら送らない（ErrActionNotSent、次の周に送り直す）。
//
// BMC に届かない・読めないときも送っていない（ErrActionNotSent）。
//
// GracefulShutdown を送った時刻はノードごとにインメモリ（S3）に持つ。Off を
// 観測したら消し、失えば（再起動）数え直す（強制停止が遅れる側＝安全側）。
package system

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/redfish"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

const (
	// Name は provider 名（core.ID.Provider に入る）。
	Name = "redfish-system"

	// ProviderVersion は diff / 正規化の版（§10.4）。toState や RefinePlan の
	// 比べ方を変えたら上げる。
	ProviderVersion = Name + "-v1"
)

// フィールド名。
const (
	FieldName                = "name"
	FieldPower               = "power"
	FieldBootEnabled         = "boot_enabled"
	FieldBootTarget          = "boot_target"
	FieldPowerOff            = "power_off"
	FieldForceAfter          = "force_after"
	FieldPowerState          = "power_state"
	FieldUUID                = "uuid"
	FieldMACs                = "macs"
	FieldCPUCount            = "cpu_count"
	FieldMemoryGiB           = "memory_gib"
	FieldBootMode            = "boot_mode"
	FieldBootTargetAllowable = "boot_target_allowable"
	FieldResetTypeAllowable  = "reset_type_allowable"
)

// power_off の値。
const (
	PowerOffGraceful          = "graceful"
	PowerOffGracefulThenForce = "graceful_then_force"
)

// ListSep は macs・*_allowable（StringList）の区切り。
const ListSep = ","

// Options は Provider の設定。
type Options struct {
	// Now は時刻（force_after の計測）。nil なら time.Now。
	Now func() time.Time
}

// Provider は provider.Provider・IntentValidator・PlanRefiner を実装する。
type Provider struct {
	clients map[string]redfish.Client
	now     func() time.Time

	mu sync.Mutex
	// gracefulSent はノードごとに最初の GracefulShutdown を送った時刻（S3）。
	gracefulSent map[string]time.Time
}

var (
	_ provider.Provider        = (*Provider)(nil)
	_ provider.IntentValidator = (*Provider)(nil)
	_ provider.PlanRefiner     = (*Provider)(nil)
	_ provider.Actioner        = (*Provider)(nil)
)

// 一度きりの操作（provider.Actioner）の名前と引数。
const (
	ActionBootOnce = "boot_once"
	ActionReset    = "reset"
	ArgTarget      = "target"
	ArgType        = "type"
)

// New は clients（ノード名 → そのノードの BMC の Client）を使う Provider を返す。
func New(clients map[string]redfish.Client, opts Options) *Provider {
	cs := make(map[string]redfish.Client, len(clients))
	for k, v := range clients {
		cs[k] = v
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Provider{clients: cs, now: now, gracefulSent: map[string]time.Time{}}
}

// Schema は §29.1 の（2）〜（5）。identity はノード名だけで、観測だけの値は
// Computed（識別子にしない）。
func (p *Provider) Schema() provider.Schema {
	computed := func(k core.FieldKind) provider.FieldSpec { return provider.FieldSpec{Kind: k, Computed: true} }
	list := provider.FieldSpec{Kind: core.FieldKindStringList, Computed: true, Ordered: true, ListSep: ListSep}
	return provider.Schema{Fields: map[string]provider.FieldSpec{
		FieldName:        {Kind: core.FieldKindString, Required: true, Identity: true},
		FieldPower:       {Kind: core.FieldKindString},
		FieldBootEnabled: {Kind: core.FieldKindString},
		FieldBootTarget:  {Kind: core.FieldKindString},
		FieldPowerOff:    {Kind: core.FieldKindString},
		FieldForceAfter:  {Kind: core.FieldKindString},

		FieldPowerState:          computed(core.FieldKindString),
		FieldUUID:                computed(core.FieldKindString),
		FieldMACs:                list,
		FieldCPUCount:            computed(core.FieldKindInt),
		FieldMemoryGiB:           computed(core.FieldKindFloat),
		FieldBootMode:            computed(core.FieldKindString),
		FieldBootTargetAllowable: list,
		FieldResetTypeAllowable:  list,
	}}
}

// Capabilities は標準階級: Read は値を返すが List と BulkObserve は無く、
// サーバは消さない（Pruneable:false）。Settle（#166、§11.1 Tier2）: BMC は
// Reset に 204 を返してから PowerState を変えるので、Apply のあと
// DefaultSettle まで読み直して確かめる（PoweringOn / PoweringOff の間は
// Converging なので数えない）。
func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		TrueObserve:     true,
		List:            false,
		BulkObserve:     false,
		Pruneable:       false,
		ProviderVersion: ProviderVersion,
		Settle:          DefaultSettle,
	}
}

// DefaultSettle は電源・起動先の Apply の settle の既定（#166）。intent の
// apply_check: と --settle で変えられる。
const DefaultSettle = 15 * time.Second

// ---- Read ----

// Read はノード id の ComputerSystem を観測する。desired を見ない（ADR-0002）。
func (p *Provider) Read(ctx context.Context, id string) (core.Observation, error) {
	now := time.Now()
	c, ok := p.clients[id]
	if !ok {
		// BMC の設定が無いノードは観測できない。absent ではない（unknown ≠ absent）。
		return core.Observation{Availability: core.AvailabilityUnknown, ObservedAt: now},
			provider.NewPermanentError(fmt.Errorf("%s: Read %q: no BMC is configured for this node", Name, id))
	}
	sys, err := c.GetSystem(ctx)
	if err != nil {
		return core.Observation{Availability: core.AvailabilityUnknown, ObservedAt: now},
			classify(fmt.Errorf("%s: Read %q: %w", Name, id, err))
	}
	switch sys.PowerState {
	case redfish.PowerPoweringOn, redfish.PowerPoweringOff:
		// BMC 自身が電源を切り替えている途中。操作を重ねない（§11）。
		return core.Observation{Availability: core.AvailabilityConverging, ObservedAt: now}, nil
	case redfish.PowerOn, redfish.PowerOff:
	default:
		return core.Observation{Availability: core.AvailabilityUnknown, ObservedAt: now},
			provider.NewTransientError(fmt.Errorf("%s: Read %q: unrecognised PowerState %q", Name, id, sys.PowerState))
	}
	if sys.PowerState == redfish.PowerOff {
		// 止まった。GracefulShutdown の時刻はもう要らない。
		p.forget(id)
	}
	return core.Observation{Availability: core.AvailabilityOk, State: toState(id, sys), ObservedAt: now}, nil
}

// toState は観測を Schema どおりの State にする。
func toState(id string, s redfish.System) core.State {
	str := func(v string) core.TypedValue { return core.TypedValue{Kind: core.FieldKindString, Value: v} }
	list := func(v []string) core.TypedValue {
		return core.TypedValue{Kind: core.FieldKindStringList, Value: strings.Join(v, ListSep)}
	}
	return core.State{
		FieldName:                str(id),
		FieldPower:               str(s.PowerState),
		FieldBootEnabled:         str(s.Boot.Enabled),
		FieldBootTarget:          str(s.Boot.Target),
		FieldPowerState:          str(s.PowerState),
		FieldUUID:                str(s.UUID),
		FieldMACs:                list(s.MACAddresses),
		FieldCPUCount:            {Kind: core.FieldKindInt, Value: int64(s.ProcessorCount)},
		FieldMemoryGiB:           {Kind: core.FieldKindFloat, Value: s.TotalMemoryGiB},
		FieldBootMode:            str(s.Boot.Mode),
		FieldBootTargetAllowable: list(s.Boot.AllowableTargets),
		FieldResetTypeAllowable:  list(s.ResetTypes),
	}
}

// ---- 計画の時点の検査 ----

// ValidateIntent は現実を見ずに決まる誤りを、ロードの時点（計画の前）で弾く。
func (p *Provider) ValidateIntent(localID string, fields map[string]string) error {
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
			return fmt.Errorf("%s %q: unknown field %q", Name, localID, k)
		case spec.Computed:
			return fmt.Errorf("%s %q: field %q is observed only and cannot be declared", Name, localID, k)
		}
	}
	if v, ok := fields[FieldName]; ok && v != localID {
		return fmt.Errorf("%s %q: name %q must equal the resource id (the node name)", Name, localID, v)
	}

	if v, ok := fields[FieldPower]; ok {
		switch v {
		case redfish.PowerOn, redfish.PowerOff:
		case redfish.ResetForceRestart, redfish.ResetGracefulRestart, "Restart", "Reboot":
			return fmt.Errorf("%s %q: power %q cannot be declared: a restart is a one-time operation, not a state to converge to (restart with a Workflow step, action: { op: %s }); declare On or Off", Name, localID, v, ActionReset)
		default:
			return fmt.Errorf("%s %q: power %q: want On or Off", Name, localID, v)
		}
	}

	enabled, hasEnabled := fields[FieldBootEnabled]
	if hasEnabled {
		switch enabled {
		case redfish.BootContinuous, redfish.BootDisabled:
		case redfish.BootOnce:
			return fmt.Errorf("%s %q: boot_enabled \"Once\" cannot be declared: the BMC resets a one-time boot override to Disabled after one boot, so it never converges (§29.1); boot once with a Workflow step (action: { op: %s, args: { target: Cd } }). Declare Continuous or Disabled", Name, localID, ActionBootOnce)
		default:
			return fmt.Errorf("%s %q: boot_enabled %q: want Continuous or Disabled", Name, localID, enabled)
		}
	}
	if _, ok := fields[FieldBootTarget]; ok && !hasEnabled {
		return fmt.Errorf("%s %q: boot_target needs boot_enabled (Continuous)", Name, localID)
	}
	if enabled == redfish.BootContinuous && fields[FieldBootTarget] == "" {
		return fmt.Errorf("%s %q: boot_enabled Continuous needs boot_target", Name, localID)
	}

	mode, hasMode := fields[FieldPowerOff]
	if hasMode && mode != PowerOffGraceful && mode != PowerOffGracefulThenForce {
		return fmt.Errorf("%s %q: power_off %q: want %s or %s", Name, localID, mode, PowerOffGraceful, PowerOffGracefulThenForce)
	}
	fa, hasFA := fields[FieldForceAfter]
	switch {
	case mode == PowerOffGracefulThenForce && !hasFA:
		return fmt.Errorf("%s %q: power_off %s needs force_after (a duration such as \"5m\")", Name, localID, PowerOffGracefulThenForce)
	case mode != PowerOffGracefulThenForce && hasFA:
		return fmt.Errorf("%s %q: force_after is only for power_off %s (the default %s never forces the power off)", Name, localID, PowerOffGracefulThenForce, PowerOffGraceful)
	case hasFA:
		if d, err := time.ParseDuration(fa); err != nil || d <= 0 {
			return fmt.Errorf("%s %q: force_after %q: want a positive duration such as \"5m\"", Name, localID, fa)
		}
	}
	if _, ok := p.clients[localID]; !ok {
		return fmt.Errorf("%s %q: no BMC is configured for node %q (add it to the BMC list, --redfish-config)", Name, localID, localID)
	}
	return nil
}

// RefinePlan は core の差分を、Schema では書けない決まりで直す（§28.4）。
//
//   - power_off・force_after（BMC に無い「どう止めるか」）の差分を落とす。
//   - boot_enabled が Continuous でなければ boot_target を比べない。
//   - boot_target と、要る Reset の種類が BMC の AllowableValues に無ければ
//     エラー（計画の時点で止める）。
//   - graceful_then_force で最初の GracefulShutdown から force_after を過ぎた
//     停止は ForceOff になるので、その Op を破壊的にする（§13）。
//   - power を Off と宣言していなければ、GracefulShutdown の時刻を忘れる。
func (p *Provider) RefinePlan(localID string, observed core.State, desired map[string]string, ops []core.Op) ([]core.Op, error) {
	if desired[FieldPower] != redfish.PowerOff {
		p.forget(localID)
	}
	continuous := desired[FieldBootEnabled] == redfish.BootContinuous
	if continuous {
		if allowed := observedList(observed, FieldBootTargetAllowable); len(allowed) > 0 && !contains(allowed, desired[FieldBootTarget]) {
			return nil, fmt.Errorf("%s %q: boot_target %q is not offered by the BMC (allowable: %s)",
				Name, localID, desired[FieldBootTarget], strings.Join(allowed, ", "))
		}
	}
	resets := observedList(observed, FieldResetTypeAllowable)
	needReset := func(t string) error {
		if len(resets) > 0 && !contains(resets, t) {
			return fmt.Errorf("%s %q: the BMC does not offer ResetType %s (allowable: %s)", Name, localID, t, strings.Join(resets, ", "))
		}
		return nil
	}

	out := make([]core.Op, 0, len(ops))
	for _, op := range ops {
		var fields []core.FieldDiff
		for _, f := range op.Fields {
			switch {
			case f.Field == FieldPowerOff || f.Field == FieldForceAfter:
				continue
			case f.Field == FieldBootTarget && !continuous:
				continue
			}
			fields = append(fields, f)
		}
		op.Fields = fields
		for _, f := range fields {
			if f.Field != FieldPower {
				continue
			}
			switch f.To {
			case redfish.PowerOn:
				if err := needReset(redfish.ResetOn); err != nil {
					return nil, err
				}
			case redfish.PowerOff:
				if err := needReset(redfish.ResetGracefulShutdown); err != nil {
					return nil, err
				}
				if force, why := p.forceDue(localID, desired); force {
					if err := needReset(redfish.ResetForceOff); err != nil {
						return nil, err
					}
					op.Destructive = true
					op.Diagnostic = fmt.Sprintf("resource %s did not power off after GracefulShutdown; %s: will ForceOff (destructive)", op.Target, why)
				}
			}
		}
		out = append(out, op)
	}
	return out, nil
}

// forceDue は、localID の停止が ForceOff に進む時か（graceful_then_force で、
// 最初の GracefulShutdown から force_after を過ぎた）を返す。
func (p *Provider) forceDue(localID string, desired map[string]string) (bool, string) {
	if desired[FieldPowerOff] != PowerOffGracefulThenForce {
		return false, ""
	}
	after, err := time.ParseDuration(desired[FieldForceAfter])
	if err != nil || after <= 0 {
		return false, "" // ValidateIntent が弾いている。
	}
	p.mu.Lock()
	sent, ok := p.gracefulSent[localID]
	p.mu.Unlock()
	if !ok {
		return false, ""
	}
	if el := p.now().Sub(sent); el >= after {
		return true, fmt.Sprintf("force_after %s elapsed since the first GracefulShutdown (%s ago)", after, el.Round(time.Second))
	}
	return false, ""
}

// ---- Apply ----

// Apply は plan を適用する。BMC を読み直し、今の現実とずれているものだけを
// 操作する（同じ plan を 2 回適用しても結果は同じ）。順序は起動先が先、電源が
// 後。ForceOff は Op が破壊的（RefinePlan が force_after を過ぎたと決め、予算を
// 通った）ときだけ送る。
func (p *Provider) Apply(ctx context.Context, plan core.Plan) error {
	for _, op := range plan.Ops {
		if err := p.applyOp(ctx, op); err != nil {
			return err
		}
	}
	return nil
}

func (p *Provider) applyOp(ctx context.Context, op core.Op) error {
	node := op.Target.Local
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s: Apply %q: "+format, append([]any{Name, node}, args...)...)
	}
	if op.Kind == core.OpKindDelete {
		return provider.NewPermanentError(fail("a system is never deleted"))
	}
	c, ok := p.clients[node]
	if !ok {
		return provider.NewPermanentError(fail("no BMC is configured for this node"))
	}
	want := map[string]string{}
	for _, f := range op.Fields {
		want[f.Field] = f.To
	}
	sys, err := c.GetSystem(ctx)
	if err != nil {
		return classify(fail("%w", err))
	}

	// 起動先（PATCH だけで、再起動しない）。
	enabled, hasEnabled := want[FieldBootEnabled]
	target, hasTarget := want[FieldBootTarget]
	if hasEnabled || hasTarget {
		if !hasEnabled {
			enabled = sys.Boot.Enabled
		}
		if !hasTarget {
			target = sys.Boot.Target
		}
		switch enabled {
		case redfish.BootContinuous:
			if len(sys.Boot.AllowableTargets) > 0 && !contains(sys.Boot.AllowableTargets, target) {
				return provider.NewPermanentError(fail("boot_target %q is not offered by the BMC", target))
			}
			if sys.Boot.Enabled != enabled || sys.Boot.Target != target {
				if err := c.PatchBoot(ctx, enabled, target); err != nil {
					return classify(fail("PATCH Boot: %w", err))
				}
			}
		case redfish.BootDisabled:
			if sys.Boot.Enabled != enabled {
				if err := c.PatchBoot(ctx, enabled, ""); err != nil {
					return classify(fail("PATCH Boot: %w", err))
				}
			}
		default:
			return provider.NewPermanentError(fail("boot_enabled %q cannot be applied (want Continuous or Disabled)", enabled))
		}
	}

	// 電源。
	power, ok := want[FieldPower]
	if !ok {
		return nil
	}
	switch power {
	case redfish.PowerOn:
		if sys.PowerState != redfish.PowerOff {
			return nil // On か切り替えの途中: 重ねない。
		}
		p.forget(node)
		if err := c.Reset(ctx, redfish.ResetOn); err != nil {
			return classify(fail("Reset On: %w", err))
		}
	case redfish.PowerOff:
		switch sys.PowerState {
		case redfish.PowerOff:
			p.forget(node)
			return nil
		case redfish.PowerOn:
		default:
			return nil // 切り替えの途中: 重ねない。
		}
		if op.Destructive {
			if err := c.Reset(ctx, redfish.ResetForceOff); err != nil {
				return classify(fail("Reset ForceOff: %w", err))
			}
			return nil
		}
		p.mu.Lock()
		if _, sent := p.gracefulSent[node]; !sent {
			p.gracefulSent[node] = p.now()
		}
		p.mu.Unlock()
		if err := c.Reset(ctx, redfish.ResetGracefulShutdown); err != nil {
			return classify(fail("Reset GracefulShutdown: %w", err))
		}
	default:
		return provider.NewPermanentError(fail("power %q cannot be applied (want On or Off)", power))
	}
	return nil
}

// ---- 一度きりの操作（provider.Actioner、#139） ----

// PlanAction は op と args を現実を見ずに検査する。
func (p *Provider) PlanAction(localID, op string, args map[string]string) (provider.ActionPlan, error) {
	if _, ok := p.clients[localID]; !ok {
		return provider.ActionPlan{}, fmt.Errorf("%s %q: no BMC is configured for this node", Name, localID)
	}
	allow := func(keys ...string) error {
		for k := range args {
			if !contains(keys, k) {
				return fmt.Errorf("%s %q: action %s: unknown argument %q (want %s)", Name, localID, op, k, strings.Join(keys, ", "))
			}
		}
		return nil
	}
	switch op {
	case ActionBootOnce:
		if err := allow(ArgTarget); err != nil {
			return provider.ActionPlan{}, err
		}
		if strings.TrimSpace(args[ArgTarget]) == "" {
			return provider.ActionPlan{}, fmt.Errorf("%s %q: action %s needs args.target (e.g. Cd)", Name, localID, op)
		}
		return provider.ActionPlan{Description: fmt.Sprintf("%s %s (boot override Once)", op, args[ArgTarget])}, nil
	case ActionReset:
		if err := allow(ArgType); err != nil {
			return provider.ActionPlan{}, err
		}
		switch resetType(args) {
		case redfish.ResetGracefulRestart:
			return provider.ActionPlan{Description: op + " " + redfish.ResetGracefulRestart + " (On if powered off)"}, nil
		case redfish.ResetForceRestart:
			return provider.ActionPlan{Destructive: true, Description: op + " " + redfish.ResetForceRestart + " (On if powered off; destructive)"}, nil
		default:
			return provider.ActionPlan{}, fmt.Errorf("%s %q: action %s: type %q: want %s (default) or %s",
				Name, localID, op, args[ArgType], redfish.ResetGracefulRestart, redfish.ResetForceRestart)
		}
	default:
		return provider.ActionPlan{}, fmt.Errorf("%s %q: unknown action %q (want %s or %s)", Name, localID, op, ActionBootOnce, ActionReset)
	}
}

func resetType(args map[string]string) string {
	if t := args[ArgType]; t != "" {
		return t
	}
	return redfish.ResetGracefulRestart
}

// Action は op を 1 回だけ施す。送る前に BMC を読み、送れないと分かれば
// ErrActionNotSent を返す（送っていないので次の周に送り直せる）。
func (p *Provider) Action(ctx context.Context, localID, op string, args map[string]string) error {
	if _, err := p.PlanAction(localID, op, args); err != nil {
		return provider.NewPermanentError(err)
	}
	c := p.clients[localID]
	fail := func(format string, a ...any) error {
		return fmt.Errorf("%s: action %s on %q: "+format, append([]any{Name, op, localID}, a...)...)
	}
	notSent := func(format string, a ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{provider.ErrActionNotSent}, a...)...)
	}
	sys, err := c.GetSystem(ctx)
	if err != nil {
		return notSent("%s: action %s on %q: %v", Name, op, localID, err)
	}
	switch op {
	case ActionBootOnce:
		target := args[ArgTarget]
		if len(sys.Boot.AllowableTargets) > 0 && !contains(sys.Boot.AllowableTargets, target) {
			return provider.NewPermanentError(fail("boot target %q is not offered by the BMC (allowable: %s)", target, strings.Join(sys.Boot.AllowableTargets, ", ")))
		}
		if err := c.PatchBoot(ctx, redfish.BootOnce, target); err != nil {
			return classify(fail("PATCH Boot: %w", err))
		}
		after, err := c.GetSystem(ctx)
		if err != nil {
			return classify(fail("reading the boot override back: %w", err))
		}
		if after.Boot.Enabled != redfish.BootOnce || after.Boot.Target != target {
			return provider.NewPermanentError(fail("the BMC reports boot override %s %s after the PATCH, want %s %s",
				after.Boot.Enabled, after.Boot.Target, redfish.BootOnce, target))
		}
		return nil
	case ActionReset:
		t := resetType(args)
		switch sys.PowerState {
		case redfish.PowerOff:
			// 切れた台の再起動は電源オン。
			t = redfish.ResetOn
		case redfish.PowerOn:
		default:
			return notSent("%s: action %s on %q: the power is %s (another power operation is in progress)", Name, op, localID, sys.PowerState)
		}
		if len(sys.ResetTypes) > 0 && !contains(sys.ResetTypes, t) {
			return provider.NewPermanentError(fail("the BMC does not offer ResetType %s (allowable: %s)", t, strings.Join(sys.ResetTypes, ", ")))
		}
		p.forget(localID)
		if err := c.Reset(ctx, t); err != nil {
			return classify(fail("Reset %s: %w", t, err))
		}
		return nil
	}
	return provider.NewPermanentError(fail("unknown action"))
}

// ---- 下回り ----

// GracefulSentAt は node に最初の GracefulShutdown を送った時刻（S3）を返す。
func (p *Provider) GracefulSentAt(node string) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t, ok := p.gracefulSent[node]
	return t, ok
}

func (p *Provider) forget(node string) {
	p.mu.Lock()
	delete(p.gracefulSent, node)
	p.mu.Unlock()
}

// classify は Client のエラーを provider のエラー型にする。届かない・5xx・
// 時間切れは一時的、認証・証明書・BMC に拒まれた要求は設定を直すまで治らない。
func classify(err error) error {
	if redfish.IsUnreachable(err) {
		return provider.NewTransientError(err)
	}
	return provider.NewPermanentError(err)
}

func observedList(s core.State, field string) []string {
	tv, ok := s[field]
	if !ok {
		return nil
	}
	v := fmt.Sprintf("%v", tv.Value)
	if v == "" {
		return nil
	}
	return strings.Split(v, ListSep)
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
