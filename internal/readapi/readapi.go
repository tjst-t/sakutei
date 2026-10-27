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

// Package readapi はデーモンの読み取り専用の HTTP の口（#136）。
//
//	GET /status  直前に観測した周の status（"sakutei status --json" と同じ形 + 周の番号と時刻）
//	GET /plan    直前に観測した周の計画（"sakutei plan --json" と同じ形 + 周の番号と時刻）
//	GET /events  Event の流し出し（Server-Sent Events）。接続した時点で firing 中の
//	             level の Event を先に送る
//	GET /graph   依存の DAG（"sakutei graph --json" と同じ形 + 周の番号と時刻）
//
// 書き込みは受けない（GET・HEAD 以外は 405）。認証は持たない：手元（127.0.0.1）で
// 待ち受ける前提で、外に出すなら前段に置く道具で守る（docs/read-api.md）。
//
// 中身は収束ループが周の終わりに渡す見え方（reconcile.CycleView）で、ここは独自の
// データを持たない（§2 の catalog の原則：UI は薄いフロントエンド）。履歴も持たない：
// 貯めるのは外の道具。遅い読み手は収束を止めない：送れない Event は落として数える。
package readapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/eventlog"
	"github.com/tjst-t/sakutei/v2/internal/graphreport"
	"github.com/tjst-t/sakutei/v2/internal/planreport"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/internal/statusreport"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// SubscriberBuffer は読み手 1 人あたりに貯めておける Event の数。これを超えた分は
// 落として数える（収束ループは待たない）。
const SubscriberBuffer = 256

// Server は読み取り専用の口。reconcile.ViewSink（周の見え方を受け取る）と
// eventlog.Sink（Event を受け取る）を兼ねる。
type Server struct {
	mu sync.Mutex
	// last は最後の周、observed は最後に Observe まで進んだ周。
	last, observed *reconcile.CycleView
	// firing は firing 中の level の Event（キー → Event）。周の終わりに収束ループの
	// 持つもので置き換え、周の途中は流れてくる Event で更新する。
	firing map[string]core.Event
	subs   map[*subscriber]bool

	dropped atomic.Int64
	now     func() time.Time
}

var (
	_ reconcile.ViewSink = (*Server)(nil)
	_ eventlog.Sink      = (*Server)(nil)
)

// New は空の Server を返す。
func New() *Server {
	return &Server{firing: map[string]core.Event{}, subs: map[*subscriber]bool{}, now: time.Now}
}

// RecordCycle は周の見え方を受け取る（収束ループから）。
func (s *Server) RecordCycle(v reconcile.CycleView) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = &v
	if v.Observed {
		s.observed = &v
	}
	s.firing = map[string]core.Event{}
	for _, ev := range v.Firing {
		s.firing[eventlog.Key(ev)] = ev
	}
}

// Emit は Event を受け取り、つながっている読み手に配る（eventlog.Sink）。知らせ
// （eventlog.IsNotice）だけが来る。読み手の箱が一杯なら落として数える。
func (s *Server) Emit(_ context.Context, ev core.Event) error {
	if ev.Time.IsZero() {
		ev.Time = s.now()
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("read api: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ev.Kind.IsLevel() {
		switch ev.State {
		case core.EventFiring:
			s.firing[eventlog.Key(ev)] = ev
		case core.EventResolved:
			delete(s.firing, eventlog.Key(ev))
		}
	}
	for sub := range s.subs {
		select {
		case sub.ch <- data:
		default:
			sub.dropped.Add(1)
			s.dropped.Add(1)
		}
	}
	return nil
}

// Dropped は遅い読み手に送れず落とした Event の数（全読み手の合計）。
func (s *Server) Dropped() int64 { return s.dropped.Load() }

type subscriber struct {
	ch      chan []byte
	dropped atomic.Int64
}

// Handler は口の http.Handler を返す。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", s.readOnly(s.serveStatus))
	mux.HandleFunc("/plan", s.readOnly(s.servePlan))
	mux.HandleFunc("/graph", s.readOnly(s.serveGraph))
	mux.HandleFunc("/events", s.readOnly(s.serveEvents))
	mux.HandleFunc("/", s.readOnly(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not found: the read-only API serves GET /status, /plan, /events and /graph")
	}))
	return mux
}

// readOnly は GET・HEAD 以外を 405 で断る。
func (s *Server) readOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed: the read-only API takes no writes")
			return
		}
		h(w, r)
	}
}

// CycleInfo は応答に足す周の番号と時刻。
type CycleInfo struct {
	// Cycle は中身を作った（観測した）周の番号。CycleTime はその周が終わった時刻。
	Cycle     int    `json:"cycle"`
	CycleTime string `json:"cycle_time"`
	// LastCycle はデーモンが最後に回した周の番号（Observe まで進まなかった周を含む）。
	// Cycle より大きければ、その後の周は現実を確かめていない（飛ばした・エラー）。
	LastCycle int `json:"last_cycle"`
}

// StatusResponse は GET /status の応答。
type StatusResponse struct {
	statusreport.JSONReport
	CycleInfo
}

// PlanResponse は GET /plan の応答。
type PlanResponse struct {
	*planreport.Report
	CycleInfo
}

// GraphResponse は GET /graph の応答。
type GraphResponse struct {
	*graphreport.Report
	CycleInfo
}

// snapshot は最後に観測した周と、最後の周の情報を返す。まだ無ければ ok=false。
func (s *Server) snapshot() (v reconcile.CycleView, info CycleInfo, health *statusreport.DaemonHealth, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last != nil {
		h := s.last.Health
		health = &h
		info.LastCycle = s.last.Cycle
	}
	if s.observed == nil {
		return v, info, health, false
	}
	v = *s.observed
	info.Cycle = v.Cycle
	info.CycleTime = v.End.UTC().Format(time.RFC3339)
	return v, info, health, true
}

func (s *Server) serveStatus(w http.ResponseWriter, r *http.Request) {
	v, info, health, ok := s.snapshot()
	if !ok {
		writeNotYet(w)
		return
	}
	rep := *v.Status
	rep.Daemon = health
	writeJSON(w, StatusResponse{JSONReport: rep.ToJSON(s.now()), CycleInfo: info})
}

func (s *Server) servePlan(w http.ResponseWriter, r *http.Request) {
	v, info, _, ok := s.snapshot()
	if !ok {
		writeNotYet(w)
		return
	}
	writeJSON(w, PlanResponse{Report: v.Plan, CycleInfo: info})
}

func (s *Server) serveGraph(w http.ResponseWriter, r *http.Request) {
	v, info, _, ok := s.snapshot()
	if !ok {
		writeNotYet(w)
		return
	}
	writeJSON(w, GraphResponse{Report: v.Graph, CycleInfo: info})
}

// serveEvents は Event を Server-Sent Events で流す。1 Event が 1 つの SSE の
// event（"data: <Event の JSON>"）。接続した時点で firing 中の level の Event を
// 先に送る。読み手が遅く落とした Event があれば、次に送るときに
// "event: dropped" で落とした数を知らせる。
func (s *Server) serveEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodHead {
		return
	}

	sub := &subscriber{ch: make(chan []byte, SubscriberBuffer)}
	s.mu.Lock()
	var initial [][]byte
	for _, ev := range sortedEvents(s.firing) {
		if data, err := json.Marshal(ev); err == nil {
			initial = append(initial, data)
		}
	}
	s.subs[sub] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, sub)
		s.mu.Unlock()
	}()

	w.WriteHeader(http.StatusOK)
	// 最初のコメントで、ヘッダと接続ができたことを読み手に知らせる。
	if _, err := fmt.Fprint(w, ": sakutei events\n\n"); err != nil {
		return
	}
	for _, data := range initial {
		if writeEvent(w, data) != nil {
			return
		}
	}
	flusher.Flush()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	var reported int64
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case data := <-sub.ch:
			if n := sub.dropped.Load(); n > reported {
				if _, err := fmt.Fprintf(w, "event: dropped\ndata: {\"dropped\":%d}\n\n", n); err != nil {
					return
				}
				reported = n
			}
			if writeEvent(w, data) != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeEvent(w http.ResponseWriter, data []byte) error {
	// json.Marshal の出力は改行を含まないので、data の行は 1 つ。
	_, err := fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}

func sortedEvents(m map[string]core.Event) []core.Event {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]core.Event, 0, len(keys))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}

func writeNotYet(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	writeError(w, http.StatusServiceUnavailable, "no cycle has observed reality yet")
}

func writeJSON(w http.ResponseWriter, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(buf.Bytes())
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(map[string]string{"error": msg})
	_, _ = w.Write(append(b, '\n'))
}
