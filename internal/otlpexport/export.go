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

// Package otlpexport はトレースの出口の送り手（Backlog #189、§26.8）。閉じた
// span（internal/tracing）を束にし、OTLP/HTTP の JSON の形
// （Content-Type: application/json、POST <endpoint>/v1/traces）で Collector へ
// 送る。protobuf も OpenTelemetry の SDK も使わない（標準のライブラリだけ）。
//
// 送れなくても収束を止めない・待たない：span は上限つきの列に積み、1 つの
// goroutine が束にして送る。列が溢れた span・送れなかった束は捨てて数え、
// 送れない間は一度だけ知らせる（戻れば一度だけ知らせる）。
package otlpexport

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/render"
	"github.com/tjst-t/sakutei/v2/internal/tracing"
)

const (
	// QueueSize は送る前の span の列の上限。溢れたら捨てて数える。
	QueueSize = 4096
	// BatchSize は 1 回に送る span の上限。
	BatchSize = 512
	// DefaultBatchInterval は束を送る間隔の既定。
	DefaultBatchInterval = time.Second
	// ReloadInterval は金庫（クライアント証明書・トークン）を読み直す間隔。
	ReloadInterval = 30 * time.Second
)

// Resource は span を出したもの（OTLP の resource の属性）。
type Resource struct {
	ServiceName, ServiceVersion, HostName string
	// Attrs は足す属性（例 sakutei.command）。
	Attrs []tracing.Attr
}

// Exporter は span を束にして送る。tracing.Sink を満たす。
type Exporter struct {
	cfg      Config
	res      Resource
	client   *http.Client
	secrets  *secrets
	log      io.Writer
	interval time.Duration

	q    chan tracing.SpanData
	stop chan struct{}
	done chan struct{}
	once sync.Once

	sent, dropped atomic.Int64
	failing       bool // run の goroutine だけが触る
}

// New は cfg の宛先へ送る Exporter を作り、送る goroutine を始める。log には
// 送れない・戻ったの知らせを書く。
func New(cfg Config, res Resource, log io.Writer) (*Exporter, error) {
	if cfg.URL == nil {
		return nil, errors.New("otlp: no endpoint")
	}
	if res.ServiceName == "" {
		res.ServiceName = cfg.ServiceName
	}
	if res.HostName == "" {
		res.HostName, _ = os.Hostname()
	}
	e := &Exporter{cfg: cfg, res: res, log: log, interval: DefaultBatchInterval,
		q: make(chan tracing.SpanData, QueueSize), stop: make(chan struct{}), done: make(chan struct{})}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil // 宛先は明示したものだけ（環境の proxy を通さない）
	if cfg.URL.Scheme == "https" {
		tc := &tls.Config{MinVersion: cfg.MinVersion, ServerName: cfg.ServerName}
		if cfg.CAFile != "" {
			pool, err := loadCAPool(cfg.CAFile)
			if err != nil {
				return nil, fmt.Errorf("otlp: tls.ca_file: %w", err)
			}
			tc.RootCAs = pool
		}
		if cfg.ClientCert != nil {
			tc.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
				c, _ := e.secrets.get()
				if c == nil {
					return &tls.Certificate{}, nil
				}
				return c, nil
			}
		}
		tr.TLSClientConfig = tc
	}
	e.client = &http.Client{Transport: tr, Timeout: cfg.Timeout,
		// 宛先は明示したものだけ：転送には従わない（資格を別の宛先へ出さない）。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if cfg.Authorizes() {
		e.secrets = newSecrets(cfg)
		if _, err := e.secrets.load(); err != nil {
			return nil, fmt.Errorf("otlp: %w", err)
		}
	}
	go e.run()
	return e, nil
}

// Enqueue は span を送る列に積む。列が一杯なら捨てて数える（待たない）。
func (e *Exporter) Enqueue(d tracing.SpanData) {
	select {
	case e.q <- d:
	default:
		e.dropped.Add(1)
	}
}

// Sent・Dropped は送った・捨てた span の数。
func (e *Exporter) Sent() int64    { return e.sent.Load() }
func (e *Exporter) Dropped() int64 { return e.dropped.Load() }

// Shutdown は列に残った span を送り終えるか timeout まで待って止める。
func (e *Exporter) Shutdown(timeout time.Duration) {
	e.once.Do(func() { close(e.stop) })
	select {
	case <-e.done:
	case <-time.After(timeout):
	}
}

func (e *Exporter) run() {
	defer close(e.done)
	t := time.NewTicker(e.interval)
	defer t.Stop()
	var batch []tracing.SpanData
	flush := func() {
		if len(batch) > 0 {
			e.send(batch)
			batch = nil
		}
	}
	for {
		select {
		case d := <-e.q:
			batch = append(batch, d)
			if len(batch) >= BatchSize {
				flush()
			}
		case <-t.C:
			flush()
		case <-e.stop:
			for {
				select {
				case d := <-e.q:
					batch = append(batch, d)
					if len(batch) >= BatchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

// send は束を送る。届かない・5xx・429 は少し待って 2 度まで送り直し、それでも
// 駄目なら捨てる（収束を待たせない）。
func (e *Exporter) send(batch []tracing.SpanData) {
	body, err := json.Marshal(e.encode(batch))
	if err != nil {
		e.dropped.Add(int64(len(batch)))
		return
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			case <-e.stop:
			}
		}
		retry, err := e.post(body)
		if err == nil {
			e.sent.Add(int64(len(batch)))
			if e.failing {
				e.failing = false
				fmt.Fprintf(e.log, "sakutei: otlp: sending traces to %s again\n", e.cfg.URL.Redacted())
			}
			return
		}
		last = err
		if !retry {
			break
		}
	}
	e.dropped.Add(int64(len(batch)))
	if !e.failing {
		e.failing = true
		fmt.Fprintf(e.log, "sakutei: otlp: cannot send %d span(s) to %s: %v; dropped (traces never hold back convergence; told once until sending works again)\n",
			len(batch), e.cfg.URL.Redacted(), last)
	}
}

// post は 1 回送る。retry は送り直す意味があるか。
func (e *Exporter) post(body []byte) (retry bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), e.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.URL.String(), bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "sakutei/"+e.res.ServiceVersion)
	if e.cfg.Token != nil {
		_, tok := e.secrets.get()
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return true, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 == 2 {
		return false, nil
	}
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return false, fmt.Errorf("HTTP %d", resp.StatusCode)
}

// ── OTLP/HTTP の JSON の形（opentelemetry-proto の JSON の写像）──

type exportRequest struct {
	ResourceSpans []resourceSpans `json:"resourceSpans"`
}

type resourceSpans struct {
	Resource   otlpResource `json:"resource"`
	ScopeSpans []scopeSpans `json:"scopeSpans"`
}

type otlpResource struct {
	Attributes []keyValue `json:"attributes"`
}

type scopeSpans struct {
	Scope instrScope `json:"scope"`
	Spans []span     `json:"spans"`
}

type instrScope struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type span struct {
	TraceID           string     `json:"traceId"`
	SpanID            string     `json:"spanId"`
	ParentSpanID      string     `json:"parentSpanId,omitempty"`
	Name              string     `json:"name"`
	Kind              int        `json:"kind"`
	StartTimeUnixNano string     `json:"startTimeUnixNano"`
	EndTimeUnixNano   string     `json:"endTimeUnixNano"`
	Attributes        []keyValue `json:"attributes,omitempty"`
	Status            status     `json:"status"`
}

type status struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type keyValue struct {
	Key   string   `json:"key"`
	Value anyValue `json:"value"`
}

type anyValue struct {
	StringValue *string     `json:"stringValue,omitempty"`
	BoolValue   *bool       `json:"boolValue,omitempty"`
	IntValue    *string     `json:"intValue,omitempty"`
	DoubleValue *float64    `json:"doubleValue,omitempty"`
	ArrayValue  *arrayValue `json:"arrayValue,omitempty"`
}

type arrayValue struct {
	Values []anyValue `json:"values"`
}

const (
	spanKindInternal = 1
	statusOK         = 1
	statusError      = 2
)

func (e *Exporter) encode(batch []tracing.SpanData) exportRequest {
	resAttrs := []tracing.Attr{
		tracing.String("service.name", e.res.ServiceName),
		tracing.String("service.version", e.res.ServiceVersion),
		tracing.String("host.name", e.res.HostName),
	}
	resAttrs = append(resAttrs, e.res.Attrs...)
	spans := make([]span, 0, len(batch))
	for _, d := range batch {
		s := span{
			TraceID: d.TraceID, SpanID: d.SpanID, ParentSpanID: d.ParentID, Name: d.Name, Kind: spanKindInternal,
			StartTimeUnixNano: strconv.FormatInt(d.Start.UnixNano(), 10),
			EndTimeUnixNano:   strconv.FormatInt(d.End.UnixNano(), 10),
			Attributes:        attributes(d.Attrs),
			Status:            status{Code: statusOK},
		}
		if d.Failed {
			s.Status = status{Code: statusError, Message: d.ErrMsg}
		}
		spans = append(spans, s)
	}
	return exportRequest{ResourceSpans: []resourceSpans{{
		Resource:   otlpResource{Attributes: attributes(resAttrs)},
		ScopeSpans: []scopeSpans{{Scope: instrScope{Name: "sakutei", Version: e.res.ServiceVersion}, Spans: spans}},
	}}}
}

// attributes は属性を OTLP の形にする。同じ名前は後のものが勝ち、順は最初に
// 出た順。
func attributes(attrs []tracing.Attr) []keyValue {
	idx := map[string]int{}
	var out []keyValue
	for _, a := range attrs {
		v, ok := value(a.Value)
		if !ok || a.Key == "" {
			continue
		}
		if i, seen := idx[a.Key]; seen {
			out[i].Value = v
			continue
		}
		idx[a.Key] = len(out)
		out = append(out, keyValue{Key: a.Key, Value: v})
	}
	return out
}

func value(x any) (anyValue, bool) {
	switch v := x.(type) {
	case string:
		return anyValue{StringValue: &v}, true
	case bool:
		return anyValue{BoolValue: &v}, true
	case int:
		s := strconv.Itoa(v)
		return anyValue{IntValue: &s}, true
	case int64:
		s := strconv.FormatInt(v, 10)
		return anyValue{IntValue: &s}, true
	case float64:
		return anyValue{DoubleValue: &v}, true
	case []string:
		arr := &arrayValue{Values: make([]anyValue, 0, len(v))}
		for i := range v {
			arr.Values = append(arr.Values, anyValue{StringValue: &v[i]})
		}
		return anyValue{ArrayValue: arr}, true
	}
	return anyValue{}, false
}

// ── 金庫（クライアント証明書・トークン）──

// secrets は金庫から読んだクライアント証明書とトークン。ReloadInterval ごとに
// 読み直し、読めなければ前のまま。
type secrets struct {
	cfg     Config
	backend *render.FileSecretBackend

	mu     sync.Mutex
	cert   *tls.Certificate
	token  string
	loaded time.Time
}

func newSecrets(cfg Config) *secrets {
	return &secrets{cfg: cfg, backend: render.NewFileSecretBackend(cfg.SecretStore)}
}

// load は金庫を読む（値は誤りの文に出さない）。
func (s *secrets) load() (*secrets, error) {
	var cert *tls.Certificate
	if s.cfg.ClientCert != nil {
		c, err := s.resolve(*s.cfg.ClientCert)
		if err != nil {
			return nil, fmt.Errorf("tls.client_cert: %w", err)
		}
		k, err := s.resolve(*s.cfg.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("tls.client_key: %w", err)
		}
		pair, err := tls.X509KeyPair([]byte(c), []byte(k))
		if err != nil {
			return nil, fmt.Errorf("tls.client_cert / tls.client_key (%s, %s) are not a PEM certificate and its key", s.cfg.ClientCert, s.cfg.ClientKey)
		}
		cert = &pair
	}
	var token string
	if s.cfg.Token != nil {
		t, err := s.resolve(*s.cfg.Token)
		if err != nil {
			return nil, fmt.Errorf("bearer_token: %w", err)
		}
		if t == "" {
			return nil, fmt.Errorf("bearer_token: %s is empty", s.cfg.Token)
		}
		token = t
	}
	s.mu.Lock()
	s.cert, s.token, s.loaded = cert, token, time.Now()
	s.mu.Unlock()
	return s, nil
}

func (s *secrets) resolve(ref interface {
	Backend() string
	Path() string
	Generation() int64
}) (string, error) {
	return s.backend.ResolveValue(context.Background(), ref.Backend(), ref.Path(), ref.Generation())
}

// get は今のクライアント証明書とトークン。古ければ読み直す（読めなければ前のまま）。
func (s *secrets) get() (*tls.Certificate, string) {
	s.mu.Lock()
	stale := time.Since(s.loaded) >= ReloadInterval
	s.mu.Unlock()
	if stale {
		if _, err := s.load(); err != nil {
			s.mu.Lock()
			s.loaded = time.Now() // 次の読み直しまで前のものを使う
			s.mu.Unlock()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cert, s.token
}
