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

package hostconf

// unit.go — provider "systemd-unit"：ホストの上の systemd のユニット 1 つ。
//
//	resources:
//	  - provider: systemd-unit
//	    id: web1/app.container            # quadlet（/etc/containers/systemd/app.container → app.service）
//	    depends_on: [web1/etc/app/app.env]
//	    fields:
//	      content: |
//	        [Container]
//	        Image=docker.io/library/nginx:1.27
//	        EnvironmentFile=/etc/app/app.env
//	        [Install]
//	        WantedBy=multi-user.target
//	      state: active
//	      restart_on: /etc/app/app.env      # 依って立つファイル（同じホスト、"," 区切り）
//	      on_change: restart                # 省略可（restart | reload）
//	      disruptive: false                 # 省略可。true なら入れ替えを破壊的として予算に数える
//
// 決まり:
//
//   - 元のファイルは ID の名前で決まる：quadlet（.container・.volume・.network・
//     .kube・.pod・.image・.build）は quadlet_dir（既定 /etc/containers/systemd）、
//     それ以外は unit_dir（既定 /etc/systemd/system）。content を書かなければ
//     元のファイルには触れない（配布物のユニットの状態だけを合わせる）。
//   - 元のファイルを書き換えたら systemctl daemon-reload を 1 回だけ流す。
//   - enabled は通常のユニットだけ。quadlet が作るユニットは enable できないので、
//     起動時の立ち上げは元のファイルの [Install] に任せる（書くと断る）。
//   - 入れ替え（restart_on）：ユニットを最後に（再）起動したときの元のファイルと
//     依って立つファイルの hash を、ホストの印 <stamp_dir>/unit-stamps/<名前> に残す。
//     観測の restart_on は「印の hash といまの hash が同じファイル」の一覧なので、
//     どれかを書き換えると宣言と違って drift になり、Apply が入れ替え（restart、
//     on_change: reload なら reload-or-restart）を 1 回流して印を更新する。走って
//     いるユニットの元のファイルが印と違う（書いたあと入れ替える前に止まった）
//     ときも content の drift として入れ替える。
//   - 印が無いとき（初めて扱うユニット）は入れ替えない：印を書くだけ。走って
//     いなかったユニットは start するので、どのみち今のファイルで立ち上がる。
//     印にまだ無いファイルを restart_on に足したときも同じ（入れ替えない）。
//   - 入れ替えは通常の操作。disruptive: true なら、入れ替えと停止の Op を破壊的
//     （Destructive）にして予算（§13）に数える。
//   - Delete は断る（ユニットは消さない。prune もしない）。
//   - quadlet の ServiceName= で名前を変えたユニットは扱わない（動くユニットは
//     ファイルの名前から決める）。

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/remote"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

const (
	// UnitName は provider の名前。
	UnitName = "systemd-unit"
	// UnitProviderVersion は観測の正規化の版（§10.4）。
	UnitProviderVersion = UnitName + "-v1"
	// DefaultUnitDir・DefaultQuadletDir は元のファイルの置き場の既定。
	DefaultUnitDir    = "/etc/systemd/system"
	DefaultQuadletDir = "/etc/containers/systemd"
)

// systemd-unit のフィールド（content は host-file と同じ FieldContent）。
const (
	// FieldName はユニットのファイルの名前（identity。書くなら ID と同じ）。
	FieldName = "name"
	// FieldState は active | inactive。
	FieldState = "state"
	// FieldEnabled は true | false（通常のユニットだけ）。
	FieldEnabled = "enabled"
	// FieldRestartOn は依って立つファイル（同じホストの絶対パス、"," 区切り）。
	FieldRestartOn = "restart_on"
	// FieldOnChange は入れ替え方（restart | reload）。宣言だけ（観測しない）。
	FieldOnChange = "on_change"
	// FieldDisruptive は入れ替えを破壊的に数えるか。宣言だけ（観測しない）。
	FieldDisruptive = "disruptive"
	// 観測だけのフィールド。
	FieldUnit     = "unit"      // 動くユニットの名前（quadlet なら生成される .service）
	FieldSource   = "source"    // 元のファイルのパス
	FieldSubState = "sub_state" // systemd の SubState
)

// 状態と入れ替え方の値。
const (
	StateActive     = "active"
	StateInactive   = "inactive"
	OnChangeRestart = "restart"
	OnChangeReload  = "reload"
)

// RestartOnSep は restart_on の区切り。
const RestartOnSep = ","

var unitNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9:_.@-]*\.(service|socket|timer|path|mount|automount|swap|target|slice|container|volume|network|kube|pod|image|build)$`)

// quadletSuffix は quadlet の拡張子 → 生成される .service の名前の後ろ。
var quadletSuffix = map[string]string{
	".container": "", ".kube": "", ".volume": "-volume", ".network": "-network",
	".pod": "-pod", ".image": "-image", ".build": "-build",
}

// ServiceName はユニットのファイルの名前から、動くユニットの名前を返す
// （quadlet なら podman の generator が作る .service の名前）。
func ServiceName(file string) (unit string, quadlet bool) {
	for ext, suffix := range quadletSuffix {
		if base, ok := strings.CutSuffix(file, ext); ok {
			return base + suffix + ".service", true
		}
	}
	return file, false
}

// UnitOptions は systemd-unit の設定。
type UnitOptions struct {
	StampDir   string // 既定 DefaultStampDir
	UnitDir    string // 既定 DefaultUnitDir
	QuadletDir string // 既定 DefaultQuadletDir
}

// UnitProvider は systemd-unit の provider。
type UnitProvider struct {
	hosts Hosts
	opts  UnitOptions
}

var (
	_ provider.Provider        = (*UnitProvider)(nil)
	_ provider.IntentValidator = (*UnitProvider)(nil)
	_ provider.PlanRefiner     = (*UnitProvider)(nil)
)

// NewUnit は hosts のユニットを扱う provider を作る。
func NewUnit(hosts Hosts, opts UnitOptions) (*UnitProvider, error) {
	if len(hosts) == 0 {
		return nil, errors.New("systemd-unit: no host is named (unit_hosts)")
	}
	for _, d := range []struct {
		key string
		v   *string
		def string
	}{{"stamp_dir", &opts.StampDir, DefaultStampDir}, {"unit_dir", &opts.UnitDir, DefaultUnitDir}, {"quadlet_dir", &opts.QuadletDir, DefaultQuadletDir}} {
		if *d.v == "" {
			*d.v = d.def
		}
		if err := cleanAbs(*d.v); err != nil {
			return nil, fmt.Errorf("systemd-unit: %s: %w", d.key, err)
		}
	}
	return &UnitProvider{hosts: hosts, opts: opts}, nil
}

// Schema はフィールドの宣言。
func (p *UnitProvider) Schema() provider.Schema {
	s := provider.FieldSpec{Kind: core.FieldKindString}
	computed := provider.FieldSpec{Kind: core.FieldKindString, Computed: true}
	return provider.Schema{Fields: map[string]provider.FieldSpec{
		FieldName:       {Kind: core.FieldKindString, Identity: true},
		FieldContent:    s,
		FieldState:      s,
		FieldEnabled:    {Kind: core.FieldKindBool},
		FieldRestartOn:  {Kind: core.FieldKindStringList, ListSep: RestartOnSep},
		FieldOnChange:   s,
		FieldDisruptive: {Kind: core.FieldKindBool},
		FieldUnit:       computed,
		FieldSource:     computed,
		FieldSubState:   computed,
	}}
}

// Capabilities は標準階級：List も BulkObserve も無く、消さない。
func (p *UnitProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{TrueObserve: true, ProviderVersion: UnitProviderVersion}
}

// locate は ID のホストとユニットのファイルの名前。
func (p *UnitProvider) locate(id string) (string, string, error) {
	host, name, ok := splitID(id)
	if !ok {
		return "", "", fmt.Errorf("want <host>/<unit file name> (got %q)", id)
	}
	if _, ok := p.hosts[host]; !ok {
		return "", "", fmt.Errorf("host %q is not in unit_hosts", host)
	}
	if !unitNameRe.MatchString(name) {
		return "", "", fmt.Errorf("%q is not a unit file name (such as app.service or app.container)", name)
	}
	return host, name, nil
}

// SourcePath は名前の元のファイルのパス。
func (p *UnitProvider) SourcePath(name string) string {
	if _, quadlet := ServiceName(name); quadlet {
		return p.opts.QuadletDir + "/" + name
	}
	return p.opts.UnitDir + "/" + name
}

func (p *UnitProvider) stampPath(name string) string {
	return p.opts.StampDir + "/unit-stamps/" + name
}

// unitStamp は印のファイル：最後に（再）起動したときの hash。
type unitStamp struct {
	source string            // 元のファイルの hash（無ければ "absent"）
	deps   map[string]string // 依って立つファイル → hash
	order  []string
}

func parseStamp(lines []string) *unitStamp {
	st := &unitStamp{deps: map[string]string{}}
	for _, l := range lines {
		kind, rest, _ := strings.Cut(l, " ")
		switch kind {
		case "source":
			st.source = rest
		case "dep":
			h, path, ok := strings.Cut(rest, " ")
			if ok && path != "" {
				if _, dup := st.deps[path]; !dup {
					st.order = append(st.order, path)
				}
				st.deps[path] = h
			}
		}
	}
	return st
}

func formatStamp(source string, deps []string, hashes map[string]string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "source %s\n", source)
	for _, d := range deps {
		fmt.Fprintf(&b, "dep %s %s\n", hashes[d], d)
	}
	return []byte(b.String())
}

// unitObs はユニットの観測。
type unitObs struct {
	load, active, sub, fileState string
	needReload                   bool
	srcHash                      string // 元のファイルの hash（無ければ ""）
	stamp                        *unitStamp
	depNow                       map[string]string
}

func (o unitObs) isActive() bool {
	switch o.active {
	case "active", "reloading", "activating", "refreshing":
		return true
	}
	return false
}

// unitReadScript は元のファイル $1 の hash と印 $2 の行を書く。
const unitReadScript = knownAbsent + `src=$1; st=$2
if [ -f "$src" ]; then
  h=$(sha256sum < "$src") || exit 3
  echo "source ${h%% *}"
elif [ -e "$src" ] || [ -L "$src" ]; then echo "source notfile"
else known_absent "$src"; echo "source absent"
fi
if [ -f "$st" ]; then
  while IFS= read -r l || [ -n "$l" ]; do echo "stamp $l"; done < "$st" || exit 3
elif [ ! -e "$st" ]; then known_absent "$st"
fi
echo end
`

// observe はユニットを読む：systemctl show、元のファイルと印、印にある
// 依って立つファイルの hash。どれかが答えなければ error（unknown）。
func (p *UnitProvider) observe(ctx context.Context, h remote.Executor, name string) (unitObs, error) {
	unit, _ := ServiceName(name)
	raw, err := h.Run(ctx, []string{"systemctl", "show", "--property=LoadState,ActiveState,SubState,UnitFileState,NeedDaemonReload", unit}, nil)
	if err != nil {
		return unitObs{}, fmt.Errorf("systemctl show %s: %w", unit, err)
	}
	var o unitObs
	props := map[string]string{}
	for _, l := range strings.Split(string(raw), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
			props[k] = v
		}
	}
	o.load, o.active, o.sub, o.fileState = props["LoadState"], props["ActiveState"], props["SubState"], props["UnitFileState"]
	o.needReload = props["NeedDaemonReload"] == "yes"
	if o.load == "" || o.active == "" {
		return unitObs{}, fmt.Errorf("systemctl show %s: no LoadState / ActiveState in the answer", unit)
	}

	src := p.SourcePath(name)
	raw, err = h.Run(ctx, []string{"sh", "-c", unitReadScript, "sh", src, p.stampPath(name)}, nil)
	if err != nil {
		return unitObs{}, fmt.Errorf("read %s: %w", src, err)
	}
	var stampLines []string
	hasStamp, ended := false, false
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	for sc.Scan() {
		l := sc.Text()
		switch {
		case l == "end":
			ended = true
		case l == "source absent":
		case l == "source notfile":
			return unitObs{}, provider.NewPermanentError(fmt.Errorf("%s is not a regular file", src))
		case strings.HasPrefix(l, "source "):
			o.srcHash = strings.TrimPrefix(l, "source ")
		case strings.HasPrefix(l, "stamp "):
			hasStamp = true
			stampLines = append(stampLines, strings.TrimPrefix(l, "stamp "))
		default:
			return unitObs{}, fmt.Errorf("read %s: unexpected output line %q", src, l)
		}
	}
	if !ended {
		return unitObs{}, fmt.Errorf("read %s: the output ended before it was complete", src)
	}
	if hasStamp {
		o.stamp = parseStamp(stampLines)
		if o.depNow, err = hashFiles(ctx, h, o.stamp.order); err != nil {
			return unitObs{}, err
		}
	}
	return o, nil
}

// Read はユニットを観測する。desired を見ない（ADR-0002）。
func (p *UnitProvider) Read(ctx context.Context, id string) (core.Observation, error) {
	host, name, err := p.locate(id)
	if err != nil {
		return unknownObs(), provider.NewPermanentError(fmt.Errorf("%s: Read %q: %w", UnitName, id, err))
	}
	o, err := p.observe(ctx, p.hosts[host], name)
	if err != nil {
		var pe *provider.PermanentError
		if errors.As(err, &pe) {
			return unknownObs(), provider.NewPermanentError(fmt.Errorf("%s: Read %q: %w", UnitName, id, err))
		}
		return unknownObs(), readError(UnitName, id, err)
	}
	if o.load == "not-found" && o.srcHash == "" {
		return core.Observation{Availability: core.AvailabilityOk, ObservedAt: time.Now()}, nil
	}
	unit, _ := ServiceName(name)
	state := core.State{
		FieldName:     str(name),
		FieldUnit:     str(unit),
		FieldSource:   str(p.SourcePath(name)),
		FieldSubState: str(o.sub),
		FieldState:    str(StateInactive),
	}
	if o.isActive() {
		state[FieldState] = str(StateActive)
	}
	switch o.fileState {
	case "enabled", "enabled-runtime":
		state[FieldEnabled] = str("true")
	case "disabled":
		state[FieldEnabled] = str("false")
	case "":
	default:
		state[FieldEnabled] = str(o.fileState) // static・generated・masked など
	}
	if o.srcHash != "" {
		c := hashPrefix + o.srcHash
		switch {
		case o.needReload:
			c += " (systemd has not reloaded it)"
		case o.stamp != nil && o.isActive() && o.stamp.source != o.srcHash:
			c += " (changed since the unit was last (re)started)"
		}
		state[FieldContent] = str(c)
	}
	if o.stamp != nil {
		var same []string
		for _, d := range o.stamp.order {
			if now, ok := o.depNow[d]; ok && now == o.stamp.deps[d] {
				same = append(same, d)
			}
		}
		sort.Strings(same)
		state[FieldRestartOn] = str(strings.Join(same, RestartOnSep))
	}
	return core.Observation{Availability: core.AvailabilityOk, State: state, ObservedAt: time.Now()}, nil
}

// parseRestartOn は restart_on を読む（空白を落とし、重複を除き、並べる）。
func parseRestartOn(v string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range strings.Split(v, RestartOnSep) {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// ValidateIntent は書き方の誤りを読み込みの時点で断る。
func (p *UnitProvider) ValidateIntent(id string, fields map[string]string) error {
	_, name, err := p.locate(id)
	if err != nil {
		return fmt.Errorf("%s %q: %w", UnitName, id, err)
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s %q: "+format, append([]any{UnitName, id}, args...)...)
	}
	names := make([]string, 0, len(fields))
	for k := range fields {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		switch k {
		case FieldName, FieldContent, FieldState, FieldEnabled, FieldRestartOn, FieldOnChange, FieldDisruptive:
		default:
			return fail("unknown field %q (name | content | state | enabled | restart_on | on_change | disruptive)", k)
		}
	}
	if v, ok := fields[FieldName]; ok && v != name {
		return fail("name %q must equal the name in the id (%s)", v, name)
	}
	if len(fields[FieldContent]) > MaxContentBytes {
		return fail("content is %d bytes; the limit is %d", len(fields[FieldContent]), MaxContentBytes)
	}
	if v, ok := fields[FieldState]; ok && v != StateActive && v != StateInactive {
		return fail("state %q: want active or inactive", v)
	}
	if v, ok := fields[FieldEnabled]; ok {
		if _, quadlet := ServiceName(name); quadlet {
			return fail("enabled: a unit made by quadlet is generated and cannot be enabled; start it at boot with [Install] WantedBy=... in the file")
		}
		if v != "true" && v != "false" {
			return fail("enabled %q: want true or false", v)
		}
	}
	if v, ok := fields[FieldOnChange]; ok && v != OnChangeRestart && v != OnChangeReload {
		return fail("on_change %q: want restart or reload", v)
	}
	if v, ok := fields[FieldDisruptive]; ok && v != "true" && v != "false" {
		return fail("disruptive %q: want true or false", v)
	}
	deps := parseRestartOn(fields[FieldRestartOn])
	for _, d := range deps {
		if err := cleanAbs(d); err != nil {
			return fail("restart_on: %w", err)
		}
	}
	if len(deps) > 0 && fields[FieldState] != StateActive {
		return fail("restart_on needs state: active (a unit is only restarted while it runs)")
	}
	return nil
}

// RefinePlan は Diff を systemd の決まりで直す（§28.4）。I/O はしない:
//
//   - content は hash で比べる（観測に印が付いていれば、同じ hash でも残す）。
//   - restart_on は集合として比べる（空白・並び・重複は差にしない）。
//   - on_change・disruptive は観測しない宣言だけの値：ほかに直すものがある
//     ときだけ Apply に渡し、それだけなら落とす。
//   - disruptive: true なら、走っているユニットの入れ替えと停止を破壊的にする。
func (p *UnitProvider) RefinePlan(_ string, observed core.State, desired map[string]string, ops []core.Op) ([]core.Op, error) {
	out := make([]core.Op, 0, len(ops))
	active := obsString(observed, FieldState) == StateActive
	for _, op := range ops {
		var real, params []core.FieldDiff
		var notes []string
		swap := false
		for _, f := range op.Fields {
			switch f.Field {
			case FieldContent:
				want := hashPrefix + digest([]byte(f.To))
				got := obsString(observed, FieldContent)
				if got == want {
					continue
				}
				notes = append(notes, fmt.Sprintf("content %s → sha256 %s", describeHash(got), short(want)))
				swap = swap || active
			case FieldRestartOn:
				got := obsString(observed, FieldRestartOn)
				if strings.Join(parseRestartOn(f.To), RestartOnSep) == strings.Join(parseRestartOn(got), RestartOnSep) {
					continue
				}
				notes = append(notes, "restart_on: a file changed since the unit was last (re)started, or is not recorded yet")
				swap = swap || active
			case FieldOnChange, FieldDisruptive:
				params = append(params, f)
				continue
			case FieldState:
				if f.To == StateInactive && active {
					swap = true
				}
			}
			real = append(real, f)
		}
		if len(real) == 0 {
			continue
		}
		op.Fields = append(real, params...)
		if desired[FieldDisruptive] == "true" && swap {
			op.Destructive = true
			notes = append(notes, "disruptive: the restart / stop is counted as destructive")
		}
		if len(notes) > 0 {
			op.Diagnostic = fmt.Sprintf("resource %s: %s", op.Target, strings.Join(notes, "; "))
		}
		out = append(out, op)
	}
	return out, nil
}

func describeHash(obs string) string {
	if obs == "" {
		return "(no file)"
	}
	h, note, _ := strings.Cut(strings.TrimPrefix(obs, hashPrefix), " ")
	if note != "" {
		return "sha256 " + short(h) + " " + note
	}
	return "sha256 " + short(h)
}

// Apply は plan を適用する。
func (p *UnitProvider) Apply(ctx context.Context, plan core.Plan) error {
	// §20.7 (#185): what this Apply runs uses the write credential.
	ctx = provider.WithWrite(ctx)
	for _, op := range plan.Ops {
		if err := p.applyOp(ctx, op); err != nil {
			return err
		}
	}
	return nil
}

// applyOp は 1 つのユニットを合わせる。今の姿を読み直してから動くので、同じ
// Op を 2 回適用しても入れ替えは 1 回（印が更新されているから）。
func (p *UnitProvider) applyOp(ctx context.Context, op core.Op) error {
	id := op.Target.Local
	host, name, err := p.locate(id)
	if err != nil {
		return provider.NewPermanentError(fmt.Errorf("%s: Apply %q: %w", UnitName, id, err))
	}
	if op.Kind == core.OpKindDelete {
		return provider.NewPermanentError(fmt.Errorf("%s: Apply %q: the systemd-unit provider never deletes a unit; stop and remove it by hand", UnitName, id))
	}
	h := p.hosts[host]
	fail := func(err error) error { return applyError(UnitName, id, err) }
	want := map[string]string{}
	for _, f := range op.Fields {
		want[f.Field] = f.To
	}
	unit, quadlet := ServiceName(name)
	cur, err := p.observe(ctx, h, name)
	if err != nil {
		return fail(err)
	}
	systemctl := func(args ...string) error {
		if _, err := h.Run(ctx, append([]string{"systemctl"}, args...), nil); err != nil {
			return fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
		}
		return nil
	}

	// 1. 元のファイル。
	wrote := false
	if c, ok := want[FieldContent]; ok && digest([]byte(c)) != cur.srcHash {
		argv := []string{"sh", "-c", fileWriteScript, "sh", p.SourcePath(name), "", "", "", "0644", "", "", ""}
		if _, err := h.Run(ctx, argv, []byte(c)); err != nil {
			return fail(fmt.Errorf("write %s: %w", p.SourcePath(name), err))
		}
		cur.srcHash, wrote = digest([]byte(c)), true
	}
	if cur.srcHash == "" && cur.load == "not-found" {
		return provider.NewPermanentError(fmt.Errorf("%s: Apply %q: systemd does not know %s and no content is declared", UnitName, id, unit))
	}
	// 2. daemon-reload（書き換えたとき・systemd が読み直していないときに 1 回）。
	if wrote || cur.needReload || cur.load == "not-found" {
		if err := systemctl("daemon-reload"); err != nil {
			return fail(err)
		}
	}
	// 3. enabled。
	if e, ok := want[FieldEnabled]; ok {
		if quadlet {
			return provider.NewPermanentError(fmt.Errorf("%s: Apply %q: a unit made by quadlet cannot be enabled", UnitName, id))
		}
		verb := "disable"
		if e == "true" {
			verb = "enable"
		}
		if err := systemctl(verb, unit); err != nil {
			return fail(err)
		}
	}
	// 4. 状態と入れ替え。state を宣言していない・差が無ければ今の状態のまま。
	target := want[FieldState]
	if target == "" {
		target = StateInactive
		if cur.isActive() {
			target = StateActive
		}
	}
	if target == StateInactive {
		if cur.isActive() {
			if err := systemctl("stop", unit); err != nil {
				return fail(err)
			}
		}
		return nil
	}
	var deps []string
	if v, ok := want[FieldRestartOn]; ok {
		deps = parseRestartOn(v)
	} else if cur.stamp != nil {
		deps = append(deps, cur.stamp.order...)
	}
	now, err := hashFiles(ctx, h, deps)
	if err != nil {
		return fail(err)
	}
	if !cur.isActive() {
		if err := systemctl("start", unit); err != nil {
			return fail(err)
		}
	} else {
		swap := wrote
		if st := cur.stamp; st != nil {
			swap = swap || (st.source != "" && st.source != cur.srcHash && cur.srcHash != "")
			for _, d := range deps {
				// 印にまだ無いファイルでは入れ替えない（記録するだけ）。
				if old, ok := st.deps[d]; ok && old != now[d] {
					swap = true
				}
			}
		}
		if swap {
			verb := "restart"
			if want[FieldOnChange] == OnChangeReload {
				verb = "reload-or-restart"
			}
			if err := systemctl(verb, unit); err != nil {
				return fail(err)
			}
		}
	}
	src := cur.srcHash
	if src == "" {
		src = "absent"
	}
	if err := writeStamp(ctx, h, p.stampPath(name), formatStamp(src, deps, now)); err != nil {
		return fail(err)
	}
	return nil
}
