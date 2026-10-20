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

package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/internal/audit"
)

// Decide はゲートへの判断（承認 / 却下）を受け付ける。承認 API と CLI の共通の入口。
//
// 判断は S4 監査に append してから S2 に書く（同じロックの中）。S4 に残せない判断は
// 効かせない。コアは認可を持たない（§7）: approver と signature は記録するだけで、
// 誰が叩けるか・署名が正しいかは上位（IdP / プロキシ）が判定する。
func Decide(ctx context.Context, store Store, log *audit.Log, workflow, gate string, a Approval, now time.Time) (Run, error) {
	if log == nil {
		return Run{}, fmt.Errorf("an S4 audit log is required to record approvals (--audit-log)")
	}
	a.Gate = gate
	return store.Decide(ctx, workflow, a, now, func(run Run) error {
		kind := audit.KindGateApproved
		detail := fmt.Sprintf("stage %s approved; proceeding to the next stage", gate)
		if a.Decision == DecisionReject {
			kind = audit.KindGateRejected
			detail = fmt.Sprintf("stage %s rejected; workflow halted", gate)
		}
		return log.Append(audit.Record{
			Time: now, Kind: kind, Workflow: workflow, RunID: run.RunID, Gate: gate,
			Actor: a.Approver, Signature: a.Signature, Detail: detail,
		})
	})
}

// API は承認ゲートの HTTP API（§6.4）。
//
//	POST /workflows/{id}/gates/{gate}/approve   {"approver": "...", "signature": "...", "decision": "approve"|"reject"}
//	POST /workflows/{id}/gates/{gate}/reject    {"approver": "...", "signature": "..."}
//	GET  /workflows                              全 Workflow の進行位置
//	GET  /workflows/{id}
//
// approve の decision を省くと承認、"reject" なら却下（= halt）。認可は持たない。
type API struct {
	Store Store
	Audit *audit.Log
	// Now は時刻。nil なら time.Now。
	Now func() time.Time
}

type decisionBody struct {
	Approver  string   `json:"approver"`
	Signature string   `json:"signature"`
	Decision  Decision `json:"decision,omitempty"`
}

// Handler は API の http.Handler を返す。
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /workflows/{id}/gates/{gate}/approve", a.decide(DecisionApprove))
	mux.HandleFunc("POST /workflows/{id}/gates/{gate}/reject", a.decide(DecisionReject))
	mux.HandleFunc("GET /workflows", func(w http.ResponseWriter, r *http.Request) {
		runs, err := a.Store.List(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if runs == nil {
			runs = []Run{}
		}
		writeJSON(w, http.StatusOK, runs)
	})
	mux.HandleFunc("GET /workflows/{id}", func(w http.ResponseWriter, r *http.Request) {
		run, ok, err := a.Store.Get(r.Context(), r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, fmt.Errorf("workflow %q has no run", r.PathValue("id")))
			return
		}
		writeJSON(w, http.StatusOK, run)
	})
	return mux
}

func (a *API) decide(def Decision) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body decisionBody
		data, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
		if err == nil && len(strings.TrimSpace(string(data))) > 0 {
			err = json.Unmarshal(data, &body)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("body: %w", err))
			return
		}
		decision := def
		if body.Decision != "" {
			if def == DecisionReject && body.Decision != DecisionReject {
				writeError(w, http.StatusBadRequest, fmt.Errorf("decision %q on the reject endpoint", body.Decision))
				return
			}
			decision = body.Decision
		}
		now := time.Now()
		if a.Now != nil {
			now = a.Now()
		}
		run, err := Decide(r.Context(), a.Store, a.Audit, r.PathValue("id"), r.PathValue("gate"),
			Approval{Approver: body.Approver, Signature: body.Signature, Decision: decision}, now)
		switch {
		case err == nil:
			writeJSON(w, http.StatusOK, run)
		case errors.Is(err, ErrNotAwaiting):
			writeError(w, http.StatusConflict, err)
		case errors.Is(err, ErrInvalidDecision):
			writeError(w, http.StatusBadRequest, err)
		default:
			writeError(w, http.StatusInternalServerError, err)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
