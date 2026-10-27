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

package statusreport

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// JSONSchemaVersion は "sakutei status --json" の出力の版（#118）。フィールドを
// 足すだけなら上げない。意味を変える・消すときに上げる。形は docs/status-json.md。
const JSONSchemaVersion = 1

// リソースの状態（JSON の resources[].state）。文の出力の分類と同じ語。
const (
	StateConformant   = "conformant"   // 文の出力の converged
	StateDrift        = "drift"        // DRIFTED（missing: true は現実に無いもの）
	StateUnobservable = "unobservable" // UNOBSERVABLE
	StateUnmanaged    = "unmanaged"    // UNMANAGED
	StateWorkflow     = "workflow"     // WORKFLOW（Workflow に任せている）
)

// JSONReport は "sakutei status --json" の標準出力 1 つ分である（#118）。
// Report（文の出力と同じ情報源）から ToJSON で作る。
type JSONReport struct {
	SchemaVersion    int    `json:"schema_version"`
	GeneratedAt      string `json:"generated_at"`
	IntentRevision   string `json:"intent_revision"`
	RenderedRevision string `json:"rendered_revision"`
	// Complete は観測できないもの（リソース・List エラーの provider）が無いこと。
	// false のとき終了コードは 3。
	Complete bool        `json:"complete"`
	Summary  JSONSummary `json:"summary"`
	// Resources は ID 文字列（provider/scope/local）の昇順。
	Resources []JSONResource `json:"resources"`
	// UnmanagedUndetectable は宣言外の検出ができなかった provider（provider 名昇順）。
	UnmanagedUndetectable []JSONUndetectable `json:"unmanaged_undetectable"`
	// Daemon は --daemon-status を読めたときだけ出る。
	Daemon *JSONDaemon `json:"daemon,omitempty"`
	// DaemonNote は --daemon-status を読めなかったときの注記。
	DaemonNote string `json:"daemon_note,omitempty"`
}

// JSONSummary は件数の要約（文の出力の「要約:」の行と同じ数）。
type JSONSummary struct {
	Conformant   int `json:"conformant"`
	Drift        int `json:"drift"`
	Missing      int `json:"missing"` // drift のうち現実に無いもの（drift に含む）
	Unobservable int `json:"unobservable"`
	Unmanaged    int `json:"unmanaged"`
	Workflow     int `json:"workflow"`
	// ListErrors は List がエラーだった provider の数（観測できない側に数える）。
	ListErrors int `json:"list_errors"`
	// Total は conformant + drift + unobservable + unmanaged + workflow。
	Total int `json:"total"`
}

// JSONResource はリソース 1 つ。state に関係しないフィールドは出さない。
type JSONResource struct {
	ID       string            `json:"id"`
	Provider string            `json:"provider"`
	Scope    map[string]string `json:"scope"`
	LocalID  string            `json:"local_id"`
	State    string            `json:"state"`
	// drift のときだけ。
	Missing    bool        `json:"missing,omitempty"`
	Cause      string      `json:"cause,omitempty"`
	Fields     []JSONField `json:"fields,omitempty"`
	Diagnostic string      `json:"diagnostic,omitempty"`
	LastSeen   string      `json:"last_seen,omitempty"`
	Tier       *JSONTier   `json:"tier,omitempty"`
	// unobservable のときだけ。Read のエラー文、または "availability=..."。
	ObserveError string `json:"observe_error,omitempty"`
	// workflow のときだけ。
	Workflow      string `json:"workflow,omitempty"`
	WorkflowStage string `json:"workflow_stage,omitempty"`
}

// JSONField はずれたフィールド 1 つ（from=現実、to=宣言。無いときは空文字）。
type JSONField struct {
	Field string `json:"field"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// JSONTier は provider の Capabilities 階級。
type JSONTier struct {
	TrueObserve     bool   `json:"true_observe"`
	List            bool   `json:"list"`
	BulkObserve     bool   `json:"bulk_observe"`
	Pruneable       bool   `json:"pruneable"`
	ProviderVersion string `json:"provider_version"`
}

// JSONUndetectable は宣言外の検出ができなかった provider 1 つ。
type JSONUndetectable struct {
	Provider string `json:"provider"`
	Kind     string `json:"kind"`
	Reason   string `json:"reason,omitempty"`
	Note     string `json:"note"`
}

// JSONDaemon は常駐デーモンの健全性（#15）。
type JSONDaemon struct {
	DaemonHealth
	Prolonged bool `json:"prolonged"`
	Stale     bool `json:"stale"`
}

// ToJSON は Report を JSON の形にする。now は generated_at に入れる時刻。
func (r Report) ToJSON(now time.Time) JSONReport {
	out := JSONReport{
		SchemaVersion:         JSONSchemaVersion,
		GeneratedAt:           now.UTC().Format(time.RFC3339),
		IntentRevision:        r.IntentRevision,
		RenderedRevision:      r.RenderedRevision,
		Complete:              !r.Incomplete(),
		Resources:             []JSONResource{},
		UnmanagedUndetectable: []JSONUndetectable{},
		DaemonNote:            r.DaemonNote,
	}
	out.Summary = JSONSummary{
		Conformant:   r.ConvergedCount,
		Drift:        len(r.Drifted),
		Unobservable: r.UnobservableCount,
		Unmanaged:    len(r.Unmanaged),
		Workflow:     len(r.WorkflowHeld),
		ListErrors:   r.ListErrorCount,
	}
	out.Summary.Total = out.Summary.Conformant + out.Summary.Drift +
		out.Summary.Unobservable + out.Summary.Unmanaged + out.Summary.Workflow

	for _, it := range r.Drifted {
		jr := newJSONResource(it.ID, StateDrift)
		jr.Missing = it.Missing
		jr.Cause = string(it.Cause)
		jr.Diagnostic = it.Diagnostic
		jr.LastSeen = it.LastSeen
		jr.Tier = &JSONTier{
			TrueObserve:     it.Tier.TrueObserve,
			List:            it.Tier.List,
			BulkObserve:     it.Tier.BulkObserve,
			Pruneable:       it.Tier.Pruneable,
			ProviderVersion: it.Tier.ProviderVersion,
		}
		for _, fd := range it.Fields {
			jr.Fields = append(jr.Fields, JSONField{Field: fd.Field, From: fd.From, To: fd.To})
		}
		if it.Missing {
			out.Summary.Missing++
		}
		out.Resources = append(out.Resources, jr)
	}
	for _, id := range r.Converged {
		out.Resources = append(out.Resources, newJSONResource(id, StateConformant))
	}
	for _, it := range r.Unobservable {
		jr := newJSONResource(it.ID, StateUnobservable)
		jr.ObserveError = it.Reason
		out.Resources = append(out.Resources, jr)
	}
	for _, it := range r.WorkflowHeld {
		jr := newJSONResource(it.ID, StateWorkflow)
		jr.Workflow = it.Workflow
		jr.WorkflowStage = it.Stage
		out.Resources = append(out.Resources, jr)
	}
	for _, id := range r.Unmanaged {
		out.Resources = append(out.Resources, newJSONResource(id, StateUnmanaged))
	}
	sort.SliceStable(out.Resources, func(i, j int) bool { return out.Resources[i].ID < out.Resources[j].ID })

	for _, u := range r.Undetectable {
		out.UnmanagedUndetectable = append(out.UnmanagedUndetectable,
			JSONUndetectable{Provider: u.Provider, Kind: u.Kind, Reason: u.Reason, Note: u.Note})
	}
	if r.Daemon != nil {
		out.Daemon = &JSONDaemon{DaemonHealth: *r.Daemon, Prolonged: r.Daemon.Prolonged(), Stale: r.DaemonStale}
	}
	return out
}

// JSON は ToJSON の結果を字下げつきの JSON にする（末尾の改行は含まない）。
func (r Report) JSON(now time.Time) ([]byte, error) {
	return json.MarshalIndent(r.ToJSON(now), "", "  ")
}

func newJSONResource(id core.ID, state string) JSONResource {
	scope := id.Scope.Labels
	if scope == nil {
		scope = map[string]string{}
	}
	return JSONResource{
		ID:       id.String(),
		Provider: id.Provider,
		Scope:    scope,
		LocalID:  id.Local,
		State:    state,
	}
}
