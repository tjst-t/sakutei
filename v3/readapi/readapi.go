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

// Package readapi は daemon の read-only HTTP API（設計 §5.3、E-H5・E-L4・N-L4・N-L6）。
// GET /status・/plan・/events（SSE）・/graph・/health を返す。書き込みの
// エンドポイントは持たない。
//
// 遅い読み手で cycle を止めない：/status は cycle の終わりに作った JSON の写しを返し、
// /events・/health は Record のファイルをロックを取らずに読む。/plan は同時に
// 1 つだけ計算する。
package readapi

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v3/config"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/daemon"
	"github.com/tjst-t/sakutei/v3/graph"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/record"
)

// PollInterval は /events が outbox の新しい Event を見る間隔。
var PollInterval = time.Second

// Server は read-only HTTP API の中身。
type Server struct {
	// RecordDir は Record のディレクトリ（/events・/health が読む）。
	RecordDir string
	// Plan は今の intent と観測で plan を計算する。何も消費しない（不変条件 30）。
	Plan func(ctx context.Context) (plan.Result, error)
	// Graph は今の intent の Resource と依存を返す。
	Graph func(ctx context.Context) (Graph, error)

	mu       sync.RWMutex
	status   []byte
	planning sync.Mutex
}

// Graph は Resource と依存（下流の表示、§5.3）。
type Graph struct {
	Nodes []string `json:"nodes"`
	Edges []Edge   `json:"edges"`
}

// Edge は from が on に依る（on が上流）。Kind は depends_on か when（§3.5）。
type Edge = graph.Edge

// GraphOf は Resource の依存を Graph にする（N-J11・E-H5）。
func GraphOf(res []core.Resource) Graph {
	g := Graph{Nodes: []string{}, Edges: graph.New(res).Edges()}
	for _, r := range res {
		g.Nodes = append(g.Nodes, r.ID.String())
	}
	return g
}

// Publish は cycle の結果を /status に置く。結果の無い cycle（読み込み時にエラーにした）は前の
// 結果を残す（誤りは /health に出る）。
func (s *Server) Publish(c daemon.Cycle) {
	if c.Result == nil {
		return
	}
	var b bytes.Buffer
	if err := c.Result.WriteJSON(&b); err != nil {
		return
	}
	s.mu.Lock()
	s.status = b.Bytes()
	s.mu.Unlock()
}

// Handler は GET だけを受ける。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/plan", s.handlePlan)
	mux.HandleFunc("/events", s.handleEvents)
	mux.HandleFunc("/graph", s.handleGraph)
	mux.HandleFunc("/health", s.handleHealth)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "読み取りだけ（GET）")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	body := s.status
	s.mu.RUnlock()
	if body == nil {
		writeError(w, http.StatusServiceUnavailable, "まだ cycle を回していない")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	if !s.planning.TryLock() {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "ほかの /plan を計算している")
		return
	}
	defer s.planning.Unlock()
	res, err := s.Plan(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var b bytes.Buffer
	if err := res.WriteJSON(&b); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b.Bytes())
}

func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	g, err := s.Graph(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, g)
}

// handleHealth は health（E-K11・N-H4）。続いた誤りがあれば 503。
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	h, err := record.ReadHealth(s.RecordDir)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "health が読めない: "+err.Error())
		return
	}
	code := http.StatusOK
	if h.ConsecutiveErrors > 0 || h.LastCycle.IsZero() {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, h)
}

// handleEvents は outbox を SSE で流す（?since=<seq>、Last-Event-ID）。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	var since uint64
	q := r.URL.Query().Get("since")
	if q == "" {
		q = r.Header.Get("Last-Event-ID")
	}
	if q != "" {
		n, err := strconv.ParseUint(q, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "since に番号を書く")
			return
		}
		since = n
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "流せない")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	t := time.NewTicker(PollInterval)
	defer t.Stop()
	for {
		evs, err := record.ReadOutbox(s.RecordDir, since, 1000)
		if err != nil {
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", strings.ReplaceAll(err.Error(), "\n", " "))
			flusher.Flush()
			return
		}
		for _, e := range evs {
			data, _ := json.Marshal(e)
			if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Kind, data); err != nil {
				return
			}
			since = e.Seq
		}
		if len(evs) > 0 {
			flusher.Flush()
			continue
		}
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// Listen は listen.read のとおりに待ち受けを作る（TLS・クライアント証明書・
// 読むだけのトークン、E-H5・E-L3・N-L4）。loopback でない宛先の平文は設定の
// 読み込みで plaintext: true を確かめ済み。
func Listen(rl *config.ReadListen, h http.Handler) (*http.Server, net.Listener, error) {
	if rl.TokensFile != "" {
		tokens, err := readTokens(rl.TokensFile)
		if err != nil {
			return nil, nil, err
		}
		h = requireToken(tokens, h)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", rl.Address)
	if err != nil {
		return nil, nil, err
	}
	if rl.TLSCert == "" {
		return srv, ln, nil
	}
	cert, err := tls.LoadX509KeyPair(rl.TLSCert, rl.TLSKey)
	if err != nil {
		ln.Close()
		return nil, nil, fmt.Errorf("listen.read.tls: %w", err)
	}
	tc := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	if rl.ClientCA != "" {
		pem, err := os.ReadFile(rl.ClientCA)
		if err != nil {
			ln.Close()
			return nil, nil, fmt.Errorf("listen.read.tls.client_ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			ln.Close()
			return nil, nil, errors.New("listen.read.tls.client_ca: 証明書が読めない")
		}
		tc.ClientCAs, tc.ClientAuth = pool, tls.RequireAndVerifyClientCert
	}
	srv.TLSConfig = tc
	return srv, tls.NewListener(ln, tc), nil
}

func readTokens(path string) ([][]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("listen.read.auth.tokens_file: %w", err)
	}
	var out [][]byte
	for _, line := range strings.Split(string(data), "\n") {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "#") {
			out = append(out, []byte(t))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("listen.read.auth.tokens_file %s にトークンが無い", path)
	}
	return out, nil
}

// requireToken は Authorization: Bearer <token> を求める。トークンの値は誤りの文に
// 出さない（不変条件 27）。
func requireToken(tokens [][]byte, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		match := 0
		for _, t := range tokens {
			match |= subtle.ConstantTimeCompare([]byte(got), t)
		}
		if !ok || match != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "読むだけのトークンが要る")
			return
		}
		h.ServeHTTP(w, r)
	})
}
