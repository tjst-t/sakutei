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

// Package metrics は Event から Prometheus の値を作って /metrics で公開する sink
// （module: metrics、設計 §3.9・§5.2・§5.6、E-H3・E-T3・N-J6）。cycle の件数と時間・
// budget の残りは CycleDone から、今 firing の数は cycle ごとの今の一覧
// （core.StateSink）から作る。数えていない値は出さない（不変条件 29：まだ cycle が
// 無いうちは cycle の値を出さない）。
//
// listen は最初に送られたときに始める（plan・status は送らないので listen しない）。
// daemon は cycle ごとに設定を読み直すので、同じアドレス・同じ設定なら同じ sink を
// 返して値と listener を保つ。TLS・読み取り専用のトークン・クライアント証明書・
// 外向きの平文の明示は read-only HTTP API（listen.read）と同じ決まり（E-H5・N-L4）。
package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/tjst-t/sakutei/v3/config"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/providers/sinks/internal/settings"
	"github.com/tjst-t/sakutei/v3/readapi"
)

// Sink は /metrics の sink。
type Sink struct {
	key    string
	listen config.ReadListen

	mu        sync.Mutex
	srv       *http.Server
	ln        net.Listener
	closed    bool
	cycles    int
	last      *core.Event    // 最後の CycleDone
	firing    map[string]int // kind → 今 firing の数（DeliverState を受けてから）
	stuck     map[string]int
	haveState bool
	total     map[string]int // kind → 受け取った Event の数
}

var (
	openMu sync.Mutex
	open   = map[string]*Sink{} // listen アドレス → sink
)

// Module は接続設定の module: metrics。
//
//	settings:
//	  listen: 127.0.0.1:9464
//	  tls: {cert: /etc/sakutei/tls.crt, key: /etc/sakutei/tls.key, client_ca: …}  # 省略可
//	  auth: {tokens_file: /etc/sakutei/metrics-tokens}                          # 省略可
//	  plaintext: true    # loopback でないアドレスで TLS なしで listen するとき（明示）
func Module(m map[string]any) (core.Sink, error) {
	r := settings.New("metrics", m)
	rl := config.ReadListen{Address: r.Str("listen")}
	var subs []*settings.Reader
	if t := r.Sub("tls"); t != nil {
		rl.TLSCert, rl.TLSKey, rl.ClientCA = t.Str("cert"), t.Str("key"), t.Str("client_ca")
		subs = append(subs, t)
	}
	if a := r.Sub("auth"); a != nil {
		rl.TokensFile = a.Str("tokens_file")
		subs = append(subs, a)
	}
	rl.Plaintext = r.Bool("plaintext")
	tls := rl.TLSCert != "" || rl.TLSKey != ""
	switch {
	case rl.Address == "":
		r.Fail("listen に listen するアドレス（host:port）を書く")
	case (rl.TLSCert == "") != (rl.TLSKey == ""):
		r.Fail("tls に cert と key を両方書く")
	case rl.ClientCA != "" && !tls:
		r.Fail("tls.client_ca は cert と key と一緒に書く")
	case tls && rl.Plaintext:
		r.Fail("tls と plaintext: true は一緒に書けない")
	case !tls && !rl.Plaintext && !config.Loopback(rl.Address):
		r.Fail("loopback でないアドレス %q で TLS なしで listen するには plaintext: true を書く", rl.Address)
	}
	if err := r.Err(subs...); err != nil {
		return nil, err
	}
	key, _ := json.Marshal(m)
	openMu.Lock()
	defer openMu.Unlock()
	if s := open[rl.Address]; s != nil {
		if s.key == string(key) {
			return s, nil
		}
		// 設定が変わった：前の listener を止めて作り直す。
		_ = s.shutdown()
	}
	s := &Sink{key: string(key), listen: rl, firing: map[string]int{}, stuck: map[string]int{}, total: map[string]int{}}
	open[rl.Address] = s
	return s, nil
}

// Close は listener を止める（テスト・終了時）。
func (s *Sink) Close() error {
	openMu.Lock()
	if open[s.listen.Address] == s {
		delete(open, s.listen.Address)
	}
	openMu.Unlock()
	return s.shutdown()
}

// shutdown は listener を止める。openMu → s.mu の順にロックを取る。
func (s *Sink) shutdown() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.srv == nil {
		return nil
	}
	err := s.srv.Close()
	s.srv, s.ln = nil, nil
	return err
}

// Addr は listen しているアドレス（始めていなければ ""）。
func (s *Sink) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// start は listen を始める（始めていれば何もしない）。s.mu を持って呼ぶ。
func (s *Sink) start() error {
	if s.srv != nil {
		return nil
	}
	if s.closed {
		return errors.New("閉じた sink")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", s.serve)
	rl := s.listen
	srv, ln, err := readapi.Listen(&rl, mux)
	if err != nil {
		return fmt.Errorf("metrics の listen %s: %w", rl.Address, err)
	}
	s.srv, s.ln = srv, ln
	go func() { _ = srv.Serve(ln) }()
	return nil
}

// Deliver は CycleDone の値と Event の数を覚える。
func (s *Sink) Deliver(_ context.Context, batch []core.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range batch {
		e := batch[i]
		s.total[e.Kind]++
		if e.Kind == "CycleDone" {
			s.cycles++
			s.last = &e
		}
	}
	return s.start()
}

// DeliverState は今 firing の数を kind ごとに数え直す。
func (s *Sink) DeliverState(_ context.Context, firing []core.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.firing, s.stuck, s.haveState = map[string]int{}, map[string]int{}, true
	for _, e := range firing {
		s.firing[e.Kind]++
		if e.Stuck {
			s.stuck[e.Kind]++
		}
	}
	return s.start()
}

func (s *Sink) serve(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	body := s.text()
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(body))
}

// countKeys は CycleDone の件数のうち、無ければ 0 と数えたもの（cycle の中で全部の
// 宣言を数えている）。
var countKeys = []string{"drift", "unknown", "converging", "unmanaged"}

// text は Prometheus のテキスト形式。s.mu を持って呼ぶ。
func (s *Sink) text() string {
	var b strings.Builder
	family := func(name, typ, help string, samples ...string) {
		if len(samples) == 0 {
			return
		}
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		for _, x := range samples {
			b.WriteString(x)
			b.WriteByte('\n')
		}
	}
	sample := func(name string, labels map[string]string, v float64) string {
		return name + labelText(labels) + " " + num(v)
	}
	family("sakutei_events_total", "counter", "Events this sink has received, by kind.", byKind("sakutei_events_total", "kind", s.total)...)
	if s.haveState {
		family("sakutei_firing", "gauge", "State events firing now, by kind.", byKind("sakutei_firing", "kind", s.firing)...)
		family("sakutei_firing_stuck", "gauge", "State events firing past cycle.stuck_after, by kind.", byKind("sakutei_firing_stuck", "kind", s.stuck)...)
	}
	if s.last == nil {
		return b.String()
	}
	v := s.last.Values
	family("sakutei_cycles_total", "counter", "Convergence cycles this sink has seen.", sample("sakutei_cycles_total", nil, float64(s.cycles)))
	family("sakutei_last_cycle_timestamp_seconds", "gauge", "Unix time the last cycle ended.",
		sample("sakutei_last_cycle_timestamp_seconds", nil, float64(s.last.At.UnixNano())/1e9))
	if d, ok := v["duration_ms"]; ok {
		family("sakutei_cycle_duration_seconds", "gauge", "Duration of the last cycle.", sample("sakutei_cycle_duration_seconds", nil, float64(d)/1000))
	}
	var states []string
	for _, k := range countKeys {
		states = append(states, sample("sakutei_resources", map[string]string{"state": k}, float64(v[k])))
	}
	family("sakutei_resources", "gauge", "Declared resources by state in the last cycle.", states...)
	for _, g := range []struct{ key, name, help string }{
		{"resources", "sakutei_declared_resources", "Declared resources in the last cycle."},
		{"applied", "sakutei_cycle_applied", "Applies and deletes that succeeded in the last cycle."},
		{"failed", "sakutei_cycle_failed", "Applies and deletes that failed in the last cycle."},
		{"halts", "sakutei_halts", "Halts in the last cycle."},
	} {
		family(g.name, "gauge", g.help, sample(g.name, nil, float64(v[g.key])))
	}
	budget := map[string]int{}
	window := map[string]int{}
	for k, n := range v {
		if p, ok := strings.CutPrefix(k, "budget_remaining."); ok {
			budget[p] = n
		}
		if p, ok := strings.CutPrefix(k, "budget_window_remaining."); ok {
			window[p] = n
		}
	}
	family("sakutei_budget_remaining", "gauge", "Changes left in the per-cycle budget, by policy.", byKind("sakutei_budget_remaining", "policy", budget)...)
	family("sakutei_budget_window_remaining", "gauge", "Destructive changes left in the window, by policy.", byKind("sakutei_budget_window_remaining", "policy", window)...)
	return b.String()
}

func byKind(name, label string, m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, name+labelText(map[string]string{label: k})+" "+num(float64(m[k])))
	}
	return out
}

func labelText(l map[string]string) string {
	if len(l) == 0 {
		return ""
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	for _, k := range keys {
		parts = append(parts, k+`="`+r.Replace(l[k])+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func num(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
