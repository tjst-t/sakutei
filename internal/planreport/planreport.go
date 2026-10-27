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

// Package planreport は "sakutei plan" の結果を 1 つの値にまとめる（#135）。
// 文の出力と "sakutei plan --json" は同じ Report から作るので、件数と中身が
// 一致する。形は docs/plan-json.md。
//
// 値は diff エンジンが出した Op のフィールドをそのまま使う。secret のフィールドは
// diff の段階で参照の形（secretref:…#genN）になっており、平文は入らない（§20.2）。
package planreport

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// JSONSchemaVersion は "sakutei plan --json" の出力の版。フィールドを足すだけなら
// 上げない。意味を変える・消すときに上げる。
const JSONSchemaVersion = 1

// 操作の種類（文の出力の CREATE / UPDATE / DELETE と同じ）。
const (
	KindCreate = "CREATE"
	KindUpdate = "UPDATE"
	KindDelete = "DELETE"
)

// 予算のレーン（§13.3）。
const (
	LaneNormal        = "normal"
	LanePlannedChange = "planned_change"
)

// 操作を止めた仕組み。
const (
	BlockedByBudget         = "budget"           // 変更予算（§13.1・§13.2）
	BlockedByCapacity       = "capacity"         // 容量ゲート（§13.4）
	BlockedByAlertOnlyScope = "alert_only_scope" // remediation: alert-only の scope（§14.3）
	// BlockedByAlertOnly は全体の remediation mode が alert-only（--mode・--freeze・
	// remediation schedule の窓の外、§14.1〜§14.3）で止めた操作。デーモンの
	// GET /plan だけが使う（#136。一回性の plan は remediation で示す）。
	BlockedByAlertOnly = "alert_only"
	// BlockedByPruneHalt は prune の安全装置（§12.4）が止めた宣言外の削除。
	// デーモンの GET /plan だけが使う（#136）。
	BlockedByPruneHalt = "prune_halt"
	// BlockedByObserveOnly は観測だけの provider（Capabilities.ObserveOnly、
	// §23.4。netconf など）の操作。どの mode でも Apply しない（知らせるだけ）。
	BlockedByObserveOnly = "observe_only"
)

// ObserveOnlyReason は観測だけの provider（§23.4）で止めた操作の理由。
const ObserveOnlyReason = "the provider observes only (Capabilities.ObserveOnly, §23.4) — reported as drift, never applied"

// AlertOnlyScopeReason は remediation: alert-only の scope（§14.3）で止めた操作の理由。
const AlertOnlyScopeReason = "scope declared remediation: alert-only — reported, not remediated (§14.3)"

// Report は "sakutei plan" 1 回分の結果である。
type Report struct {
	SchemaVersion    int    `json:"schema_version"`
	GeneratedAt      string `json:"generated_at"`
	IntentRevision   string `json:"intent_revision"`
	RenderedRevision string `json:"rendered_revision"`
	// Error は計画ができなかったときの理由（終了コード 1）。そのとき他の中身は
	// 分かったところまでしか入らない。
	Error string `json:"error,omitempty"`

	Claim       *Claim       `json:"claim,omitempty"`
	Remediation *Remediation `json:"remediation,omitempty"`

	Summary    Summary     `json:"summary"`
	Operations []Operation `json:"operations"`

	Budget   Gate `json:"budget"`
	Capacity Gate `json:"capacity"`
	// PlannedChangeEvents は planned_change の知らせ（レーンに入った・使い切った・
	// 冷却期間中など）。文の出力では標準エラーの "sakutei plan: PlannedChange…" の行。
	PlannedChangeEvents []string `json:"planned_change_events"`

	PruneHalts    []PruneHalt `json:"prune_halts"`
	PruneDeferred []Target    `json:"prune_deferred"`

	Workflow           []WorkflowTarget    `json:"workflow"`
	PendingAllocations []PendingAllocation `json:"pending_allocations"`
	Unobservable       []Unobservable      `json:"unobservable"`
	Composites         []Composite         `json:"composites"`
}

// Claim は --claim で絞った計画の範囲（§17.1）。
type Claim struct {
	Selector string `json:"selector"`
	InClaim  int    `json:"in_claim"`
	Excluded int    `json:"excluded"`
}

// Remediation は intent の remediation schedule を今の時刻で評価したもの（§14.2）。
type Remediation struct {
	Mode          string `json:"mode"` // "auto" | "alert-only"
	Reason        string `json:"reason"`
	OutsideWindow bool   `json:"outside_window"`
	Line          string `json:"line"`
}

// Summary は件数。
type Summary struct {
	Changes            int `json:"changes"` // operations の要素数（文の出力の "Plan: N change(s)"）
	Create             int `json:"create"`
	Update             int `json:"update"`
	Delete             int `json:"delete"`
	Destructive        int `json:"destructive"`
	Blocked            int `json:"blocked"`
	PlannedChangeLane  int `json:"planned_change_lane"`
	Task               int `json:"task"`
	Workflow           int `json:"workflow"`
	PendingAllocations int `json:"pending_allocations"`
	Unobservable       int `json:"unobservable"`
	PruneHalts         int `json:"prune_halts"`
}

// Target は合成 ID を分けて持つ。
type Target struct {
	ID       string            `json:"id"`
	Provider string            `json:"provider"`
	Scope    map[string]string `json:"scope"`
	LocalID  string            `json:"local_id"`
}

// NewTarget は id を Target にする。
func NewTarget(id core.ID) Target {
	scope := id.Scope.Labels
	if scope == nil {
		scope = map[string]string{}
	}
	return Target{ID: id.String(), Provider: id.Provider, Scope: scope, LocalID: id.Local}
}

// Operation は計画の操作 1 つ。
type Operation struct {
	Target
	Kind        string  `json:"kind"`
	Destructive bool    `json:"destructive"`
	Prune       bool    `json:"prune"` // 宣言外の削除（集合差 prune、§12）
	Cause       string  `json:"cause,omitempty"`
	Diagnostic  string  `json:"diagnostic,omitempty"`
	Fields      []Field `json:"fields,omitempty"`

	Lane          string `json:"lane"`
	PlannedChange string `json:"planned_change,omitempty"`

	Blocked     bool   `json:"blocked"`
	BlockedBy   string `json:"blocked_by,omitempty"`
	BlockReason string `json:"block_reason,omitempty"`

	Task *Task `json:"task,omitempty"`
}

// Field はフィールドの前と後。From が現実（無いときは空）、To が宣言（消すときは空）。
type Field struct {
	Field string `json:"field"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// Task は strategy: task のリソースの操作に付く（§6.2）。Apply されず、デーモンが
// Task を起こす。
type Task struct {
	Window         string `json:"window"`
	RollbackOnFail bool   `json:"rollback_on_fail"`
}

// NewOperation は op を Operation にする（レーンと止めた理由は呼ぶ側が入れる）。
func NewOperation(op core.Op, prune bool) Operation {
	o := Operation{
		Target:      NewTarget(op.Target),
		Kind:        KindName(op.Kind),
		Destructive: op.Destructive,
		Prune:       prune,
		Cause:       string(op.Cause),
		Diagnostic:  op.Diagnostic,
		Lane:        LaneNormal,
	}
	for _, fd := range op.Fields {
		o.Fields = append(o.Fields, Field{Field: fd.Field, From: fd.From, To: fd.To})
	}
	return o
}

// KindName は文の出力の操作名。
func KindName(k core.OpKind) string {
	switch k {
	case core.OpKindCreate:
		return KindCreate
	case core.OpKindUpdate:
		return KindUpdate
	case core.OpKindDelete:
		return KindDelete
	}
	return "UNKNOWN"
}

// Gate は予算・容量ゲートの判定。
type Gate struct {
	Halted bool `json:"halted"`
	// Halt は止める範囲："all"（すべての変更）/ "destructive"（破壊的な変更だけ）。
	Halt   string `json:"halt,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// GateOf はゲートの判定を Report の形にする。
func GateOf(haltAll, haltDestructive bool, reason string) Gate {
	g := Gate{Reason: reason}
	switch {
	case haltAll:
		g.Halted, g.Halt = true, "all"
	case haltDestructive:
		g.Halted, g.Halt = true, "destructive"
	}
	return g
}

// PruneHalt は prune の安全装置が止めた provider（§12.4）。その provider の
// 宣言外の削除はこの周に一つも出ない。
type PruneHalt struct {
	Provider    string `json:"provider"`
	Reason      string `json:"reason"`
	CountDrop   bool   `json:"count_drop"`
	HeldOrphans int    `json:"held_orphans"`
}

// WorkflowTarget は Workflow に任せている対象（§6.3）。
type WorkflowTarget struct {
	Target
	Workflow string `json:"workflow"`
	Stage    string `json:"stage"`
}

// PendingAllocation は採番待ちの要求（§19.8）。
type PendingAllocation struct {
	Target
	Field         string `json:"field"`
	Pool          string `json:"pool"`
	FreeKnown     bool   `json:"free_known"`
	Free          int    `json:"free"`
	PendingInPool int    `json:"pending_in_pool,omitempty"`
	Error         string `json:"error,omitempty"`
	Line          string `json:"line"`
}

// Unobservable は観測できず計画に入らなかったリソース（unknown ≠ absent、§3）。
type Unobservable struct {
	Target
	Availability string `json:"availability"`
}

// Composite は合成リソースの集約状態（§9.7）。
type Composite struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// New は空の Report を作る。配列は JSON で null にならないよう空にしておく。
func New(now time.Time) *Report {
	return &Report{
		SchemaVersion:       JSONSchemaVersion,
		GeneratedAt:         now.UTC().Format(time.RFC3339),
		Operations:          []Operation{},
		PlannedChangeEvents: []string{},
		PruneHalts:          []PruneHalt{},
		PruneDeferred:       []Target{},
		Workflow:            []WorkflowTarget{},
		PendingAllocations:  []PendingAllocation{},
		Unobservable:        []Unobservable{},
		Composites:          []Composite{},
	}
}

// Summarize は中身から Summary を数え直す。
func (r *Report) Summarize() {
	s := Summary{
		Changes:            len(r.Operations),
		Workflow:           len(r.Workflow),
		PendingAllocations: len(r.PendingAllocations),
		Unobservable:       len(r.Unobservable),
		PruneHalts:         len(r.PruneHalts),
	}
	for _, o := range r.Operations {
		switch o.Kind {
		case KindCreate:
			s.Create++
		case KindUpdate:
			s.Update++
		case KindDelete:
			s.Delete++
		}
		if o.Destructive {
			s.Destructive++
		}
		if o.Blocked {
			s.Blocked++
		}
		if o.Lane == LanePlannedChange {
			s.PlannedChangeLane++
		}
		if o.Task != nil {
			s.Task++
		}
	}
	r.Summary = s
}

// JSON は字下げつきの JSON にする（末尾の改行は含まない）。値の < > & は
// そのまま出す（採番待ちの "<pending allocation …>" を読めるように）。
func (r *Report) JSON() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
