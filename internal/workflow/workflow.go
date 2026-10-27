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

// Package workflow は最小 Workflow（v1.5、§6.3 / §6.4 / §14.4）。
//
// 直列（one-at-a-time）+ 不変条件（invariant）+ 手動承認ゲートだけを持つ。
// 動的 select・自動 canary・自動段階ロールアウトは v2。
//
//	workflows:
//	  - workflow: rolling-upgrade
//	    applicant: alice                        # 申請者 ID（separation 用、PR の作者など）
//	    target:                                 # 対象の provider（と scope）
//	      provider: fake
//	    invariant:
//	      min_active: 2
//	      active_when:
//	        active: "true"
//	    strategy:
//	      order: one-at-a-time
//	    stages:                                 # ステージは宣言順に 1 つずつ
//	      - name: n1
//	        targets:                            # ステージ内の対象も 1 つずつ
//	          - n1
//	        gate:
//	          manual_approval: required
//	          timeout: 24h
//	      - name: n2
//	        targets:
//	          - n2
//	        gate:
//	          manual_approval:
//	            requirements:                   # §14.4（無ければ単一承認）
//	              quorum:
//	                n: 2
//	                of: 3
//	              roles:
//	                - platform-admin
//	              separation: applicant_ne_approver
//	    steps:                                  # 対象ごとに実行する手順
//	      - apply:                              # drain（冪等な Apply）
//	          active: "false"
//	      - task:                               # 単純 Task（§6.2）を呼ぶ
//	          version: "2"
//	      - wait:
//	          fields:
//	            version: "2"
//	          timeout: 5m
//	      - apply:                              # undrain
//	          active: "true"
//	      - verify: invariant
//	    on_failure:                             # 既定 halt（v1.5 は halt のみ）
//	      policy: halt
//
// intent の YAML はブロックスタイルだけを受け付ける（{ … } / [ … ] のフロー
// スタイルは読み込めない）。この例はそのまま loader で読める（loader のテスト
// TestWorkflowDocExampleLoads が確かめる）。
//
// 規則:
//   - invariant は各ステップの前後で検証する。前（その手順を施した後の見込み）で
//     破れ、かつその手順が active を減らすなら、そのステップに留まって進まない
//     （level-triggered に毎周再評価し、現実が戻れば進む）。後で破れていれば
//     失敗＝on_failure（halt）。
//   - ステージを終えると、ゲートがあれば StageAwaitingApproval を出して「待ち」
//     フェーズで止まる。進行位置は S2（Store）にあるので再起動しても待ちに留まる。
//     承認で次のステージへ、却下と timeout（既定 24h）で halt。
//   - 承認の事実は S4 監査（audit.Log）に append-only で記録する。承認 API 自体は
//     認可を持たない（§7）。
//   - ゲートは requirements（§14.4）を持てる。quorum: { n, of } は異なる承認者 n 人で
//     通過（同じ承認者の二重承認は 1 と数える）、roles はそのいずれかを名乗る承認
//     だけを数え、separation: applicant_ne_approver は申請者（workflow の applicant）
//     本人の承認を拒否する。役割の真偽は上位 IdP が判定する（承認ボディの roles を
//     数えるだけ、§7）。requirements の無いゲートは単一承認。
//   - 実行中・待ち・halt の Workflow の対象は収束ループが Apply しない（converging
//     扱い、§6）。完了（done）すると収束に戻る。
//   - 手順は対象と別の provider のリソースを触れる（#87）。apply・wait・require に
//     resource: "<provider>/<id>" を書くと、その手順はそのリソースを Observe・Diff・
//     Apply する（id の {target} は今の対象の Local ID）。apply は { resource,
//     fields } の形、wait / require は { resource, fields, timeout }。require は
//     前提条件で、観測で満たすまで待ち、timeout（必須）で halt する。手順が触る
//     リソースも対象と同じく、done まで収束ループが Apply しない（Resources）。
//     invariant（min_active）は対象だけで数える。
//   - action は provider の一度きりの操作（#139、provider.Actioner）: { resource,
//     op, args }（resource は省けば対象）。収束の宣言にできない操作（Redfish の
//     「次の 1 回だけ CD から起動」〈op boot_once〉・再起動〈op reset〉）を、その
//     位置で 1 回だけ施して次へ進む。効いたかは後の wait / require で確かめる。
//     送る直前に「送りかけ」を S2 に書き、送れたら消して進む。書いたまま残って
//     いれば（送ったか分からないまま止まった）、二度送らずに halt する。provider
//     が「送っていない」と言う失敗（届かない・別の操作の途中）は待って送り直し、
//     それ以外の失敗は halt。破壊的な操作（ForceRestart）は予算（§13）を通し、
//     止められれば待つ。invariant は action では見込みを変えない（止まる間の
//     active は後の手順で確かめる）。
//   - trigger: reimage（#88）の Workflow は、対象の scope（target.scope、必須）を
//     覆う planned_change の kind: reimage（§13.3）のレーンが開いている（窓の中・
//     承認・cooling-off）ときだけ、その宣言ごとに 1 回走る（Run.Trigger に宣言の
//     Key）。宣言が無い間は run を作らず、対象は収束ループに任せる（Holds が
//     返さない）。対象ごとに手を付ける前に宣言の max_count を 1 つ使い
//     （Run.Reserved、ReimageLanes）、使い切った・取り下げられた・窓を過ぎたなら
//     その対象の手前で halt する。入れ直しの中の破壊的な action はレーンの 1 回分に
//     含まれ、通常予算に通さない。
//
// 別の provider を触る手順と一度きりの操作の例（OS の導入: CD を差す → 差さった
// ことを確かめる → 次の 1 回だけ CD から起動 → 再起動〈切れていれば電源 On〉→
// インストーラが電源を切るのを待つ → 電源 On〈上書きは使い切ったのでディスク
// から〉→ ゲスト OS の目印を確かめる → CD を抜く。全体は
// docs/trial/redfish-os-install/intent/os.yaml）:
//
//	steps:
//	  - apply:
//	      resource: redfish-virtual-media/{target}/cd
//	      fields:
//	        image: /iso/installer.iso
//	  - require:
//	      resource: redfish-virtual-media/{target}/cd
//	      fields:
//	        image: /iso/installer.iso
//	      timeout: 5m
//	  - action:
//	      op: boot_once
//	      args:
//	        target: Cd
//	  - action:
//	      op: reset
//	  - wait:
//	      fields:
//	        power: "Off"
//	      timeout: 15m
//	  - apply:
//	      power: "On"
//	  - require:
//	      resource: guest-os/{target}
//	      fields:
//	        marker.installed: install-1
//	      timeout: 10m
//	  - apply:
//	      resource: redfish-virtual-media/{target}/cd
//	      fields:
//	        image: ""
package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/pkg/core"
)

// DefaultGateTimeout はゲートの timeout の既定（§6.4）。
const DefaultGateTimeout = 24 * time.Hour

// DefaultActiveField / DefaultActiveValue は invariant.active_when を省いたときの
// 「active」の判定（fields.active == "true"）。
const (
	DefaultActiveField = "active"
	DefaultActiveValue = "true"
)

// StepKind は手順の種別。
type StepKind string

const (
	// StepApply は対象に fields を冪等に Apply する（drain / undrain など）。
	StepApply StepKind = "apply"
	// StepTask は対象に単純 Task（§6.2）を起動し、完了印（fields）を Observe するまで待つ。
	StepTask StepKind = "task"
	// StepWait は対象の fields が観測されるまで待つ。timeout を過ぎたら失敗。
	StepWait StepKind = "wait"
	// StepVerify は invariant を検証する。破れていれば失敗。
	StepVerify StepKind = "verify"
	// StepRequire は手順の前提条件（#87）。fields が観測されるまで待ち、timeout
	// （必須）を過ぎたら失敗。wait と同じく観測で判定するが、満たされないまま
	// 永遠に待たないよう timeout を省けない。
	StepRequire StepKind = "require"
	// StepAction は provider の一度きりの操作（#139、provider.Actioner）を 1 回だけ
	// 施す（Redfish の boot_once・reset など）。収束の宣言ではなく手順なので、
	// 観測で済んだかを確かめない（後の wait / require で確かめる）。
	StepAction StepKind = "action"
)

// TriggerReimage は planned_change の kind: reimage（§13.3）で起こす Workflow の
// 引き金（#88）。Definition.Trigger。
const TriggerReimage = "reimage"

// TargetPlaceholder は Step.Resource の中で今の対象の Local ID に置き換わる文字列。
const TargetPlaceholder = "{target}"

// Step は対象ごとの手順の 1 つ。
type Step struct {
	Kind StepKind `json:"kind"`
	// Resource は手順が触る（観測する）リソース "<provider>/<id>"（#87）。空なら
	// 今の対象。id の中の {target} は今の対象の Local ID に置き換わる（例
	// "redfish-virtual-media/{target}/cd"）。scope は Workflow の target.scope。
	// apply / wait / require / action が持てる（task は対象だけ。多相 Task は v2）。
	Resource string `json:"resource,omitempty"`
	// Fields は apply / task が現実に施す値、wait / require が待つ値。
	Fields map[string]string `json:"fields,omitempty"`
	// Timeout は wait / require の上限（wait は 0 なら無期限、require は必須）。
	Timeout time.Duration `json:"timeout,omitempty"`
	// Op と Args は action の操作の名前と引数（#139、例 op boot_once・args
	// { target: Cd }）。意味は provider が決める（provider.Actioner）。
	Op   string            `json:"op,omitempty"`
	Args map[string]string `json:"args,omitempty"`
}

// ResourceFor は対象 target のときに手順が触るリソースの ID を返す。
func (s Step) ResourceFor(target core.ID) core.ID {
	if s.Resource == "" {
		return target
	}
	ref := strings.ReplaceAll(s.Resource, TargetPlaceholder, target.Local)
	prov, local, _ := strings.Cut(ref, "/")
	return core.NewID(prov, target.Scope, local)
}

// validateResource は Resource の書き方の誤りを返す。
func (s Step) validateResource() error {
	if s.Resource == "" {
		return nil
	}
	if s.Kind != StepApply && s.Kind != StepWait && s.Kind != StepRequire && s.Kind != StepAction {
		return fmt.Errorf("resource is only for apply / wait / require / action (a task runs on the target; polymorphic Task is v2)")
	}
	prov, local, ok := strings.Cut(s.Resource, "/")
	if !ok || strings.TrimSpace(prov) == "" || strings.TrimSpace(local) == "" {
		return fmt.Errorf("resource %q: want \"<provider>/<id>\" (e.g. \"redfish-virtual-media/%s/cd\")", s.Resource, TargetPlaceholder)
	}
	if rest := strings.ReplaceAll(local, TargetPlaceholder, ""); strings.ContainsAny(rest, "{}") {
		return fmt.Errorf("resource %q: only %s can be substituted", s.Resource, TargetPlaceholder)
	}
	return nil
}

// Gate はステージ末尾の手動承認ゲート（§6.4）。
type Gate struct {
	// Timeout を過ぎても承認されなければ halt する。既定 24h。
	Timeout time.Duration `json:"timeout"`
	// Requirements は承認者要件（§14.4）。nil なら単一承認。
	Requirements *Requirements `json:"requirements,omitempty"`
}

// SeparationApplicantNeApprover は申請者≠承認者（4-eyes の中核、§14.4）。
const SeparationApplicantNeApprover = "applicant_ne_approver"

// Quorum は N-of-M（承認者候補 Of 人のうち N 人の承認で通過）。
type Quorum struct {
	N  int `json:"n"`
	Of int `json:"of"`
}

// Requirements はゲートの承認者要件（§14.4）。コアは認可を持たない（§7）: 役割は
// 承認ボディに名乗られたものを数えるだけで、真偽は上位 IdP が判定する。
type Requirements struct {
	// Quorum が nil なら 1 人。
	Quorum *Quorum `json:"quorum,omitempty"`
	// Roles を宣言すると、そのいずれかの役割を名乗る承認だけを数える。
	Roles []string `json:"roles,omitempty"`
	// Separation が applicant_ne_approver なら申請者本人の承認を拒否する。
	Separation string `json:"separation,omitempty"`
}

// Needed は通過に要る異なる承認者の数。
func (r *Requirements) Needed() int {
	if r == nil || r.Quorum == nil {
		return 1
	}
	return r.Quorum.N
}

// qualify は承認 a が要件を満たさない理由を返す（満たすなら空）。
func (r *Requirements) qualify(a Approval, applicant string) string {
	if r == nil {
		return ""
	}
	if r.Separation == SeparationApplicantNeApprover && a.Approver == applicant {
		return fmt.Sprintf("separation %s: %s is the applicant and cannot approve", r.Separation, a.Approver)
	}
	if len(r.Roles) > 0 && !hasAny(a.Roles, r.Roles) {
		return fmt.Sprintf("approver %s holds none of the required roles %v (asserted roles %v)", a.Approver, r.Roles, a.Roles)
	}
	return ""
}

// Count は approvals のうち要件を満たす承認の異なる承認者を順に返す（同じ承認者の
// 二重承認は 1 と数える）。rejected は要件を満たさず数えなかった承認の理由。
// ゲートの数え上げ（Run.Tally）と planned_change の approval の検証（§13.3）が使う。
func (r *Requirements) Count(approvals []Approval, applicant string) (approvers, rejected []string) {
	seen := map[string]bool{}
	for _, a := range approvals {
		if a.Decision != DecisionApprove || seen[a.Approver] {
			continue
		}
		if why := r.qualify(a, applicant); why != "" {
			rejected = append(rejected, why)
			continue
		}
		seen[a.Approver] = true
		approvers = append(approvers, a.Approver)
	}
	return approvers, rejected
}

// Validate は要件の宣言を検証する（ロード時）。
func (r *Requirements) Validate() error { return r.validate() }

func hasAny(have, want []string) bool {
	for _, h := range have {
		for _, w := range want {
			if h == w {
				return true
			}
		}
	}
	return false
}

func (r *Requirements) validate() error {
	if r == nil {
		return nil
	}
	if q := r.Quorum; q != nil && (q.N < 1 || q.Of < q.N) {
		return fmt.Errorf("requirements.quorum must satisfy 1 <= n <= of (n=%d, of=%d)", q.N, q.Of)
	}
	if r.Separation != "" && r.Separation != SeparationApplicantNeApprover {
		return fmt.Errorf("requirements.separation %q is not supported (%s)", r.Separation, SeparationApplicantNeApprover)
	}
	for _, role := range r.Roles {
		if role == "" {
			return fmt.Errorf("requirements.roles: empty role")
		}
	}
	return nil
}

// Stage は 1 つのステージ。Targets を宣言順に 1 つずつ処理する。
type Stage struct {
	Name    string    `json:"name"`
	Targets []core.ID `json:"targets"`
	// Gate が nil ならゲートなしで次のステージへ進む。
	Gate *Gate `json:"gate,omitempty"`
}

// Invariant は全体の不変条件。
type Invariant struct {
	// MinActive は Workflow の全対象のうち active でなければならない数の下限（0 は検証しない）。
	MinActive int `json:"min_active,omitempty"`
	// ActiveWhen は「active」とみなす観測値（全 field が一致）。既定 { active: "true" }。
	ActiveWhen map[string]string `json:"active_when,omitempty"`
}

// Definition は intent の workflows: の 1 件。
type Definition struct {
	Name string `json:"workflow"`
	// Applicant は申請者 ID（PR の作者など）。separation: applicant_ne_approver の
	// ゲートはこの ID の承認を拒否する。
	Applicant string `json:"applicant,omitempty"`
	// Trigger が空なら、Workflow は宣言されたときに 1 回走る（done のあとは定義が
	// 変わったときだけ）。TriggerReimage なら、対象の scope を覆う planned_change
	// kind: reimage（窓の中・承認済み、§13.3）があるときだけ、その宣言ごとに 1 回
	// 走る（#88）。対象ごとに宣言の max_count を 1 つ使う。
	Trigger   string    `json:"trigger,omitempty"`
	Invariant Invariant `json:"invariant"`
	Stages    []Stage   `json:"stages"`
	Steps     []Step    `json:"steps"`
}

// Scope は対象の scope（target.scope。全対象で同じ）。
func (d Definition) Scope() core.Scope {
	for _, st := range d.Stages {
		for _, id := range st.Targets {
			return id.Scope
		}
	}
	return core.NewScope(nil)
}

// Targets は全ステージの対象を宣言順に返す。
func (d Definition) Targets() []core.ID {
	var out []core.ID
	for _, st := range d.Stages {
		out = append(out, st.Targets...)
	}
	return out
}

// Resources は Workflow が触るリソース（全対象と、手順の resource を対象ごとに
// 展開したもの）を重複なしに宣言順で返す（#87）。done でない Workflow の
// Resources は収束ループが Apply しない。
func (d Definition) Resources() []core.ID {
	var out []core.ID
	seen := map[core.IDKey]bool{}
	add := func(id core.ID) {
		if !seen[id.Key()] {
			seen[id.Key()] = true
			out = append(out, id)
		}
	}
	targets := d.Targets()
	for _, id := range targets {
		add(id)
	}
	for _, step := range d.Steps {
		if step.Resource == "" {
			continue
		}
		for _, id := range targets {
			add(step.ResourceFor(id))
		}
	}
	return out
}

// Hash は定義のハッシュ。実行中に定義が変わったことを見分けるのに使う。
func (d Definition) Hash() string {
	type target struct {
		Provider string `json:"provider"`
		Scope    string `json:"scope,omitempty"`
		Local    string `json:"local"`
	}
	type stage struct {
		Name    string   `json:"name"`
		Targets []target `json:"targets"`
		Gate    *Gate    `json:"gate,omitempty"`
	}
	v := struct {
		Name      string    `json:"name"`
		Applicant string    `json:"applicant,omitempty"`
		Trigger   string    `json:"trigger,omitempty"`
		Invariant Invariant `json:"invariant"`
		Stages    []stage   `json:"stages"`
		Steps     []Step    `json:"steps"`
	}{Name: d.Name, Applicant: d.Applicant, Trigger: d.Trigger, Invariant: d.Invariant, Steps: d.Steps}
	for _, st := range d.Stages {
		s := stage{Name: st.Name, Gate: st.Gate}
		for _, id := range st.Targets {
			s.Targets = append(s.Targets, target{Provider: id.Provider, Scope: id.Scope.String(), Local: id.Local})
		}
		v.Stages = append(v.Stages, s)
	}
	data, _ := json.Marshal(v)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:16]
}

// activeWhen は既定を補った active の判定。
func (d Definition) activeWhen() map[string]string {
	if len(d.Invariant.ActiveWhen) > 0 {
		return d.Invariant.ActiveWhen
	}
	return map[string]string{DefaultActiveField: DefaultActiveValue}
}

// Validate は定義の誤りを返す。
func (d Definition) Validate() error {
	if d.Name == "" {
		return fmt.Errorf("workflow: name is required")
	}
	if d.Invariant.MinActive < 0 {
		return fmt.Errorf("workflow %s: invariant.min_active must be >= 0", d.Name)
	}
	if len(d.Stages) == 0 {
		return fmt.Errorf("workflow %s: at least one stage is required", d.Name)
	}
	if len(d.Steps) == 0 {
		return fmt.Errorf("workflow %s: at least one step is required", d.Name)
	}
	stages := map[string]bool{}
	targets := map[string]bool{}
	for i, st := range d.Stages {
		if st.Name == "" {
			return fmt.Errorf("workflow %s: stages[%d]: name is required", d.Name, i)
		}
		if stages[st.Name] {
			return fmt.Errorf("workflow %s: stage %q is declared twice (the stage name is the gate name)", d.Name, st.Name)
		}
		stages[st.Name] = true
		if len(st.Targets) == 0 {
			return fmt.Errorf("workflow %s: stage %s: at least one target is required", d.Name, st.Name)
		}
		for _, id := range st.Targets {
			if id.Local == "" {
				return fmt.Errorf("workflow %s: stage %s: empty target id", d.Name, st.Name)
			}
			if targets[id.String()] {
				return fmt.Errorf("workflow %s: target %s appears twice", d.Name, id)
			}
			targets[id.String()] = true
		}
		if st.Gate != nil && st.Gate.Timeout <= 0 {
			return fmt.Errorf("workflow %s: stage %s: gate timeout must be positive", d.Name, st.Name)
		}
		if st.Gate != nil {
			if err := st.Gate.Requirements.validate(); err != nil {
				return fmt.Errorf("workflow %s: stage %s: gate %w", d.Name, st.Name, err)
			}
			if st.Gate.Requirements != nil && st.Gate.Requirements.Separation != "" && d.Applicant == "" {
				return fmt.Errorf("workflow %s: stage %s: separation %s needs the workflow's applicant", d.Name, st.Name, st.Gate.Requirements.Separation)
			}
		}
	}
	switch d.Trigger {
	case "":
	case TriggerReimage:
		if len(d.Scope().Labels) == 0 {
			return fmt.Errorf("workflow %s: trigger %s needs target.scope (a planned_change covers a scope; a target without one is never covered)", d.Name, d.Trigger)
		}
	default:
		return fmt.Errorf("workflow %s: unknown trigger %q (%s)", d.Name, d.Trigger, TriggerReimage)
	}
	if d.Invariant.MinActive > len(targets) {
		return fmt.Errorf("workflow %s: invariant.min_active %d exceeds the %d target(s)", d.Name, d.Invariant.MinActive, len(targets))
	}
	for i, s := range d.Steps {
		switch s.Kind {
		case StepApply, StepTask, StepWait:
			if len(s.Fields) == 0 {
				return fmt.Errorf("workflow %s: steps[%d] (%s): fields are required", d.Name, i, s.Kind)
			}
		case StepRequire:
			if len(s.Fields) == 0 {
				return fmt.Errorf("workflow %s: steps[%d] (%s): fields are required", d.Name, i, s.Kind)
			}
			if s.Timeout <= 0 {
				return fmt.Errorf("workflow %s: steps[%d] (require): timeout is required (a precondition that never holds halts instead of waiting forever)", d.Name, i)
			}
		case StepAction:
			if strings.TrimSpace(s.Op) == "" {
				return fmt.Errorf("workflow %s: steps[%d] (action): op is required (e.g. boot_once, reset)", d.Name, i)
			}
			if len(s.Fields) > 0 || s.Timeout != 0 {
				return fmt.Errorf("workflow %s: steps[%d] (action): an action takes op and args only (confirm its effect with a following wait / require)", d.Name, i)
			}
		case StepVerify:
		default:
			return fmt.Errorf("workflow %s: steps[%d]: unknown step kind %q (apply | task | wait | require | action | verify)", d.Name, i, s.Kind)
		}
		if s.Kind != StepAction && (s.Op != "" || len(s.Args) > 0) {
			return fmt.Errorf("workflow %s: steps[%d] (%s): op and args are only for action", d.Name, i, s.Kind)
		}
		if s.Timeout < 0 {
			return fmt.Errorf("workflow %s: steps[%d]: timeout must not be negative", d.Name, i)
		}
		if err := s.validateResource(); err != nil {
			return fmt.Errorf("workflow %s: steps[%d] (%s): %w", d.Name, i, s.Kind, err)
		}
	}
	return nil
}
