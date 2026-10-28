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

// firmware.go は Redfish の BMC provider のリソース「FW の版」（provider
// redfish-firmware、FirmwareProvider）。設計書 §29.1「Redfish の FW の更新」、
// Backlog #148。システム・仮想メディア・BIOS と同じパッケージに別の型として
// 置き、ノードごとの redfish.Client を共有する。標準階級で List と BulkObserve を
// 持たない（部品は BMC に前からあり、Sakutei が作ったり消したりしない）。
//
// ID は "<ノード名>/firmware/<部品の Id>"（例 "node1/firmware/BMC"）。部品の Id は
// UpdateService の FirmwareInventory のメンバーの Id。"<ノード名>/<Id>" にしない
// のは、Local ID が intent 全体で一意なので、同じノードの redfish-bios
// （"<ノード名>/bios"）や仮想メディア（"<ノード名>/cd"）と Id が重なりうるため。
//
// 宣言するフィールド:
//
//   - name: ノード名（ID のノード名の部分と同じ）。component: 部品の Id（書くなら
//     ID の部品の部分と同じ）。
//   - version: 望む版（必須）。比べ方は前後の空白を除いた文字列の完全一致
//     （大文字と小文字も区別する）。BMC は版の書き方を変えないので、それ以上の
//     正規化（"v" の有無・数の比較）はしない。違えば更新する（上げるか下げるかは
//     問わない）。
//   - image_uri: イメージの在りか（必須、http / https。transfer が multipart なら
//     file:// も）。資格を URI に書かない（image_username・image_password）。
//   - sha256: イメージの SHA-256（16 進 64 文字。書けば、流す前に Sakutei が
//     イメージを取りに行って確かめる）。
//   - transfer: simple_update（既定。BMC が image_uri を取りに行く）/ multipart
//     （Sakutei がイメージを取りに行き、UpdateService の MultipartHttpPushUri に
//     送る。BMC がその口を持つときだけ）。
//   - apply_time: Immediate（既定）/ OnReset（@Redfish.OperationApplyTime）。BMC が
//     @Redfish.OperationApplyTimeSupport を報告していれば、その中の値だけ。
//   - reboot: none（既定）/ graceful / force。更新の後にホストを再起動するか
//     （Redfish の電源の決まり #48 と同じ: graceful は GracefulRestart、force は
//     ForceRestart、切れていれば On）。none でも OnReset の保留は、誰かが再起動
//     すれば効く（reboot_timeout まで版が合うのを待つ）。
//   - update_timeout: BMC の Task の完了を待つ上限（既定 1h）。reboot_timeout: 再起動
//     して版が合うのを待つ上限（既定 30m）。
//   - image_username・image_password: image_uri を取りに行く資格（BMC の
//     SimpleUpdate の Username・Password と、Sakutei が取りに行くときの Basic
//     認証）。image_password は SecretRef でしか書けず（§20）、使う直前に解決して
//     ログ・エラーに出さない。
//
// 観測（Read）: UpdateService の FirmwareInventory の部品の Version と、計画の
// 時点の検査に使う Updateable・UpdateService の口（multipart・ApplyTime・
// TaskService）。宣言の版は見ない（ADR-0002）。UpdateService や部品が無いのは
// Ok で present が false（版を宣言しているので計画の時点のエラー）。届かないのは
// Unknown。
//
// 変更は Task の実行体（task.PhasedRunner、firmware_task.go）だけが行う:
// update_policy.strategy は task 必須で、収束ループの Apply は拒む（§6.5
// RequiresTaskEvent）。版の違いの Op は破壊的（Destructive、予算 §13 に数える）。
// rollback_on_fail は書けない（FW は自動では戻せない）。
//
// 版以外の宣言（image_uri など）は「どう合わせるか」で BMC には無い。Read は
// 返さず、PlanRefiner（RefinePlan）が差分から落とす。

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/redfish"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

const (
	// FirmwareName は FW の版の provider 名（core.ID.Provider に入る）。
	FirmwareName = "redfish-firmware"

	// FirmwareProviderVersion は diff / 正規化の版（§10.4）。
	FirmwareProviderVersion = FirmwareName + "-v1"
)

// FirmwareIDInfix は FW の版の ID の中ほど（"<ノード名>/firmware/<部品の Id>"）。
const FirmwareIDInfix = "/firmware/"

// FirmwareID はノード node の部品 component の FW の版の ID を返す。
func FirmwareID(node, component string) string { return node + FirmwareIDInfix + component }

// firmwareParts は FW の版の ID からノード名と部品の Id を取り出す。
func firmwareParts(id string) (node, component string, err error) {
	node, component, ok := strings.Cut(id, FirmwareIDInfix)
	if !ok || node == "" || component == "" || strings.Contains(component, "/") {
		return "", "", fmt.Errorf("%s: id %q: want \"<node>%s<component Id>\" (e.g. %q)", FirmwareName, id, FirmwareIDInfix, FirmwareID("node1", "BMC"))
	}
	return node, component, nil
}

// FW の版のフィールド名（name は FieldName と同じ）。
const (
	FieldComponent     = "component"
	FieldVersion       = "version"
	FieldImageURI      = "image_uri"
	FieldSHA256        = "sha256"
	FieldTransfer      = "transfer"
	FieldApplyTime     = "apply_time"
	FieldReboot        = "reboot"
	FieldUpdateTimeout = "update_timeout"
	FieldRebootTimeout = "reboot_timeout"
	FieldImageUsername = "image_username"
	FieldImagePassword = "image_password"

	FieldPresent       = "present"
	FieldUpdateable    = "updateable"
	FieldSoftwareID    = "software_id"
	FieldUpdateService = "update_service"
	FieldTaskService   = "task_service"
	FieldMultipartPush = "multipart_push"
	FieldApplyTimes    = "apply_times"
)

// transfer・reboot の値。
const (
	TransferSimpleUpdate = "simple_update"
	TransferMultipart    = "multipart"

	RebootNone     = "none"
	RebootGraceful = "graceful"
	RebootForce    = "force"
)

// 既定の時間。
const (
	DefaultUpdateTimeout = time.Hour
	DefaultRebootTimeout = 30 * time.Minute
	// DefaultFirmwarePollInterval は BMC の Task と版を見に行く間隔。
	DefaultFirmwarePollInterval = 10 * time.Second
	// DefaultClockSkew は BMC とこのホストの時計のずれの許し（firmware_task.go）。
	DefaultClockSkew = 5 * time.Minute
)

// howFields は「どう合わせるか」の宣言（BMC には無い。RefinePlan が差分から落とす）。
var howFields = []string{
	FieldComponent, FieldImageURI, FieldSHA256, FieldTransfer, FieldApplyTime, FieldReboot,
	FieldUpdateTimeout, FieldRebootTimeout, FieldImageUsername, FieldImagePassword,
}

// FirmwareOptions は FirmwareProvider の設定。
type FirmwareOptions struct {
	// Secrets は image_password の SecretRef を解決する（無ければ image_password を
	// 宣言した更新は失敗する）。
	Secrets core.SecretResolver
	// PollInterval は BMC の Task と版を見に行く間隔（0 なら
	// DefaultFirmwarePollInterval）。
	PollInterval time.Duration
	// ImageClient はイメージを取りに行く HTTP クライアント（nil なら時間切れを
	// ctx に任せる既定のもの）。
	ImageClient *http.Client
	// TempDir は multipart で送るイメージを一時に置くディレクトリ（空なら
	// os.TempDir）。
	TempDir string
	// ClockSkew は BMC とこのホストの時計のずれの許し。Task の起動の時刻から
	// これを引いた時刻より前に始まった BMC の Task は、この更新のものと数えない
	// （0 なら DefaultClockSkew、firmware_task.go）。
	ClockSkew time.Duration
}

// FirmwareProvider は provider.Provider・IntentValidator・PlanRefiner・
// UpdatePolicyValidator と task.PhasedRunner（firmware_task.go）を実装する。
type FirmwareProvider struct {
	clients map[string]redfish.Client
	opts    FirmwareOptions
}

var (
	_ provider.Provider              = (*FirmwareProvider)(nil)
	_ provider.IntentValidator       = (*FirmwareProvider)(nil)
	_ provider.PlanRefiner           = (*FirmwareProvider)(nil)
	_ provider.UpdatePolicyValidator = (*FirmwareProvider)(nil)
)

// NewFirmware は clients（ノード名 → そのノードの BMC の Client）を使う
// FirmwareProvider を返す。
func NewFirmware(clients map[string]redfish.Client, opts FirmwareOptions) *FirmwareProvider {
	cs := make(map[string]redfish.Client, len(clients))
	for k, v := range clients {
		cs[k] = v
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultFirmwarePollInterval
	}
	if opts.ClockSkew <= 0 {
		opts.ClockSkew = DefaultClockSkew
	}
	if opts.ImageClient == nil {
		opts.ImageClient = &http.Client{}
	}
	return &FirmwareProvider{clients: cs, opts: opts}
}

// Schema は §29.1「Redfish の FW の更新」。identity はノード名と部品の Id。
func (p *FirmwareProvider) Schema() provider.Schema {
	str := provider.FieldSpec{Kind: core.FieldKindString}
	computed := func(k core.FieldKind) provider.FieldSpec { return provider.FieldSpec{Kind: k, Computed: true} }
	return provider.Schema{Fields: map[string]provider.FieldSpec{
		FieldName:          {Kind: core.FieldKindString, Required: true, Identity: true},
		FieldComponent:     {Kind: core.FieldKindString, Identity: true},
		FieldVersion:       {Kind: core.FieldKindString, Required: true},
		FieldImageURI:      str,
		FieldSHA256:        str,
		FieldTransfer:      str,
		FieldApplyTime:     str,
		FieldReboot:        str,
		FieldUpdateTimeout: str,
		FieldRebootTimeout: str,
		FieldImageUsername: str,
		FieldImagePassword: {Kind: core.FieldKindSecretRef, Secret: true},
		FieldPresent:       computed(core.FieldKindBool),
		FieldUpdateable:    computed(core.FieldKindBool),
		FieldSoftwareID:    computed(core.FieldKindString),
		FieldUpdateService: computed(core.FieldKindBool),
		FieldTaskService:   computed(core.FieldKindBool),
		FieldMultipartPush: computed(core.FieldKindBool),
		FieldApplyTimes:    computed(core.FieldKindString),
	}}
}

// Capabilities は標準階級: List と BulkObserve は無く、消さない。
func (p *FirmwareProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		TrueObserve:     true,
		List:            false,
		BulkObserve:     false,
		Pruneable:       false,
		ProviderVersion: FirmwareProviderVersion,
	}
}

// ---- Read ----

// Read は部品 id の版を観測する。desired を見ない（ADR-0002）。
func (p *FirmwareProvider) Read(ctx context.Context, id string) (core.Observation, error) {
	now := time.Now()
	unknown := core.Observation{Availability: core.AvailabilityUnknown, ObservedAt: now}
	node, component, err := firmwareParts(id)
	if err != nil {
		return unknown, provider.NewPermanentError(err)
	}
	c, ok := p.clients[node]
	if !ok {
		return unknown, provider.NewPermanentError(fmt.Errorf("%s: Read %q: no BMC is configured for this node", FirmwareName, id))
	}
	str := func(v string) core.TypedValue { return core.TypedValue{Kind: core.FieldKindString, Value: v} }
	boolean := func(v bool) core.TypedValue { return core.TypedValue{Kind: core.FieldKindBool, Value: v} }
	state := core.State{FieldName: str(node), FieldComponent: str(component)}
	ok200 := func() (core.Observation, error) {
		return core.Observation{Availability: core.AvailabilityOk, State: state, ObservedAt: now}, nil
	}
	u, err := c.GetUpdateService(ctx)
	if redfish.IsNoUpdateService(err) {
		state[FieldUpdateService], state[FieldPresent] = boolean(false), boolean(false)
		return ok200()
	}
	if err != nil {
		return unknown, classify(fmt.Errorf("%s: Read %q: %w", FirmwareName, id, err))
	}
	state[FieldUpdateService] = boolean(u.ServiceEnabled)
	state[FieldTaskService] = boolean(u.TaskService)
	state[FieldMultipartPush] = boolean(u.MultipartHTTPPushURI != "")
	state[FieldApplyTimes] = str(strings.Join(u.ApplyTimes, ListSep))
	inv, err := c.GetFirmwareInventory(ctx)
	if err != nil {
		return unknown, classify(fmt.Errorf("%s: Read %q: %w", FirmwareName, id, err))
	}
	state[FieldPresent] = boolean(false)
	for _, fc := range inv {
		if fc.ID != component {
			continue
		}
		state[FieldPresent] = boolean(true)
		state[FieldVersion] = str(fc.Version)
		state[FieldUpdateable] = boolean(fc.Updateable)
		state[FieldSoftwareID] = str(fc.SoftwareID)
	}
	return ok200()
}

// VersionsMatch は観測の版 observed が宣言の版 want と同じか（前後の空白を除いた
// 完全一致）。
func VersionsMatch(observed, want string) bool {
	return strings.TrimSpace(observed) == strings.TrimSpace(want)
}

// ---- 宣言の読み方 ----

// fwSpec は宣言を読んだもの（ValidateIntent が確かめた後のもの）。
type fwSpec struct {
	node, component string
	version         string
	imageURI        string
	sha256          string // 小文字の 16 進。空なら確かめない
	transfer        string
	applyTime       string
	reboot          string
	updateTimeout   time.Duration
	rebootTimeout   time.Duration
	username        string
	password        string // SecretRef の参照（値ではない）。空なら無し
	// startedAt は Task の起動の時刻（task.Request.StartedAt。宣言ではない）。
	startedAt time.Time
}

// parseFirmware は localID と fields を読み、誤りがあればエラーを返す。エラーは
// image_password の中身を出さない。
func parseFirmware(localID string, fields map[string]string) (fwSpec, error) {
	fail := func(format string, a ...any) (fwSpec, error) {
		return fwSpec{}, fmt.Errorf("%s %q: "+format, append([]any{FirmwareName, localID}, a...)...)
	}
	node, component, err := firmwareParts(localID)
	if err != nil {
		return fwSpec{}, err
	}
	s := fwSpec{
		node: node, component: component,
		version:  strings.TrimSpace(fields[FieldVersion]),
		imageURI: strings.TrimSpace(fields[FieldImageURI]),
		sha256:   strings.ToLower(strings.TrimSpace(fields[FieldSHA256])),
		transfer: fields[FieldTransfer], applyTime: fields[FieldApplyTime], reboot: fields[FieldReboot],
		username: fields[FieldImageUsername], password: strings.TrimSpace(fields[FieldImagePassword]),
		updateTimeout: DefaultUpdateTimeout, rebootTimeout: DefaultRebootTimeout,
	}
	if v, ok := fields[FieldName]; ok && v != node {
		return fail("name %q must equal the node name in the resource id (%q)", v, node)
	}
	if v, ok := fields[FieldComponent]; ok && v != component {
		return fail("component %q must equal the component Id in the resource id (%q)", v, component)
	}
	if s.version == "" {
		return fail("version is required (the firmware version to run)")
	}
	if s.transfer == "" {
		s.transfer = TransferSimpleUpdate
	}
	if s.transfer != TransferSimpleUpdate && s.transfer != TransferMultipart {
		return fail("transfer %q: want %s (default) or %s", s.transfer, TransferSimpleUpdate, TransferMultipart)
	}
	if s.imageURI == "" {
		return fail("image_uri is required (where the firmware image is)")
	}
	u, err := url.Parse(s.imageURI)
	if err != nil || u.Host == "" && u.Scheme != "file" {
		return fail("image_uri %q is not an absolute URI", redactURI(s.imageURI))
	}
	if u.User != nil {
		return fail("image_uri must not carry credentials; use image_username and image_password (a SecretRef)")
	}
	switch {
	case u.Scheme == "http" || u.Scheme == "https":
	case u.Scheme == "file" && s.transfer == TransferMultipart:
		if u.Path == "" {
			return fail("image_uri %q: a file URI needs a path", s.imageURI)
		}
	case u.Scheme == "file":
		return fail("image_uri %q: a file URI works only with transfer: %s (the BMC cannot read Sakutei's files)", s.imageURI, TransferMultipart)
	default:
		return fail("image_uri %q: want http or https (or file with transfer: %s)", redactURI(s.imageURI), TransferMultipart)
	}
	if s.sha256 != "" {
		if b, err := hex.DecodeString(s.sha256); err != nil || len(b) != 32 {
			return fail("sha256 must be 64 hexadecimal digits")
		}
	}
	switch s.applyTime {
	case "":
		s.applyTime = redfish.ApplyImmediate
	case redfish.ApplyImmediate, redfish.ApplyOnReset:
	default:
		return fail("apply_time %q: want %s (default) or %s", s.applyTime, redfish.ApplyImmediate, redfish.ApplyOnReset)
	}
	switch s.reboot {
	case "":
		s.reboot = RebootNone
	case RebootNone, RebootGraceful, RebootForce:
	default:
		return fail("reboot %q: want %s (default), %s or %s", s.reboot, RebootNone, RebootGraceful, RebootForce)
	}
	for _, d := range []struct {
		field string
		dst   *time.Duration
	}{{FieldUpdateTimeout, &s.updateTimeout}, {FieldRebootTimeout, &s.rebootTimeout}} {
		v, ok := fields[d.field]
		if !ok || v == "" {
			continue
		}
		t, err := time.ParseDuration(v)
		if err != nil || t <= 0 {
			return fail("%s %q: want a positive duration such as 30m", d.field, v)
		}
		*d.dst = t
	}
	if s.password != "" {
		if _, err := core.ParseSecretRef(s.password); err != nil {
			return fail("image_password must be a SecretRef (secretref:file/<path>[#gen<N>]); a password is never written in the intent (§20)")
		}
	}
	if s.password != "" && s.username == "" {
		return fail("image_password needs image_username")
	}
	return s, nil
}

// redactURI は URI の userinfo を伏せる（エラーの文言用）。
func redactURI(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	u.User = url.User("***")
	return u.String()
}

// ---- 計画の時点の検査 ----

// ValidateIntent は現実を見ずに決まる誤りを、ロードの時点で弾く。
func (p *FirmwareProvider) ValidateIntent(localID string, fields map[string]string) error {
	if _, _, err := firmwareParts(localID); err != nil {
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
			return fmt.Errorf("%s %q: unknown field %q", FirmwareName, localID, k)
		case spec.Computed:
			return fmt.Errorf("%s %q: field %q is observed only and cannot be declared", FirmwareName, localID, k)
		}
	}
	s, err := parseFirmware(localID, fields)
	if err != nil {
		return err
	}
	if _, ok := p.clients[s.node]; !ok {
		return fmt.Errorf("%s %q: no BMC is configured for node %q (add it to the BMC list, --redfish-config)", FirmwareName, localID, s.node)
	}
	return nil
}

// ValidateUpdatePolicy は strategy: task だけを受け付ける（FW の書込は中断できず、
// 冪等でない）。rollback_on_fail は FW を自動では戻せないので断る。
func (p *FirmwareProvider) ValidateUpdatePolicy(localID string, pol core.UpdatePolicy) error {
	if !pol.IsTask() {
		return fmt.Errorf("%s %q: update_policy.strategy must be task (a firmware update cannot be interrupted and reboots; Apply is refused)", FirmwareName, localID)
	}
	if pol.RollbackOnFail {
		return fmt.Errorf("%s %q: update_policy.rollback_on_fail is not supported (a firmware update is not rolled back automatically; declare the previous version to go back)", FirmwareName, localID)
	}
	return nil
}

// RefinePlan は差分を版だけに直し、版が違えば更新できるかを観測で確かめて、
// Op を破壊的にする（§13）。
func (p *FirmwareProvider) RefinePlan(localID string, observed core.State, desired map[string]string, ops []core.Op) ([]core.Op, error) {
	s, err := parseFirmware(localID, desired)
	if err != nil {
		return nil, err
	}
	fail := func(format string, a ...any) ([]core.Op, error) {
		return nil, fmt.Errorf("%s %q: "+format, append([]any{FirmwareName, localID}, a...)...)
	}
	b := func(field string) bool { v, _ := observed[field].Value.(bool); return v }
	if !b(FieldPresent) {
		if us, ok := observed[FieldUpdateService].Value.(bool); ok && !us {
			return fail("the BMC of node %q has no UpdateService (or it is disabled); its firmware cannot be observed or updated through Redfish", s.node)
		}
		return fail("the BMC of node %q reports no component %q in UpdateService/FirmwareInventory", s.node, s.component)
	}
	current := fmt.Sprintf("%v", observed[FieldVersion].Value)
	if VersionsMatch(current, s.version) {
		return nil, nil
	}
	switch {
	case !b(FieldUpdateable):
		return fail("component %q is not updateable (Updateable is false)", s.component)
	case !b(FieldTaskService):
		return fail("the BMC of node %q has no TaskService; the update could not be followed to its end", s.node)
	case s.transfer == TransferMultipart && !b(FieldMultipartPush):
		return fail("transfer: %s, but the UpdateService of node %q has no MultipartHttpPushUri (use transfer: %s)", TransferMultipart, s.node, TransferSimpleUpdate)
	}
	if times := observedList(observed, FieldApplyTimes); len(times) > 0 && !contains(times, s.applyTime) {
		return fail("apply_time %s is not supported by the BMC (supported: %s)", s.applyTime, strings.Join(times, ", "))
	}
	reboot := "no reboot by Sakutei"
	if s.reboot != RebootNone {
		reboot = "reboot " + s.reboot
	}
	out := make([]core.Op, 0, len(ops))
	for _, op := range ops {
		var fields []core.FieldDiff
		for _, f := range op.Fields {
			if f.Field == FieldVersion {
				fields = append(fields, f)
			}
		}
		op.Fields = fields
		op.Destructive = true
		op.Diagnostic = fmt.Sprintf("resource %s: firmware %s %q → %q; a phased Task updates it (%s, apply_time %s, %s; destructive, needs approval)",
			op.Target, s.component, current, s.version, s.transfer, s.applyTime, reboot)
		out = append(out, op)
	}
	return out, nil
}

// ---- Apply ----

// Apply は拒む: FW の更新は Task の実行体だけが行う（§6.5）。
func (p *FirmwareProvider) Apply(_ context.Context, plan core.Plan) error {
	ids := make([]string, 0, len(plan.Ops))
	for _, op := range plan.Ops {
		ids = append(ids, op.Target.Local)
	}
	return provider.NewPermanentError(fmt.Errorf("%s: Apply %v is refused: firmware is updated only by a phased Task (declare update_policy.strategy: task)", FirmwareName, ids))
}
