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

// Package otlp は Event からトレースの span を作って OTLP/HTTP（JSON 形式）で送る
// sink（module: otlp、設計 §3.9・§5.2・§5.6、E-H3・N-H5）。v2 の
// --otlp-endpoint（#189）を移した。
//
// span の作り方：CycleDone 1 つが cycle の span（name sakutei.cycle、開始＝終了 −
// duration_ms、属性に cycle の件数と budget の残り）。その前に積まれた edge Event
// （Applied・Deleted・ProcedureStep…）は cycle の span の子（開始＝終了＝その時刻）。
// cycle の span が無いまま残った edge Event は root span にする。trace と span の
// ID は Event の番号から決める（送り直しても同じ ID）。field の値は載せない（属性は
// kind・対象・宣言の labels・件数だけ、不変条件 27）。level Event は span にしない。
package otlp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/providers/sinks/internal/settings"
)

// Sink は OTLP/HTTP に送る sink。
type Sink struct {
	url     string
	client  *settings.Client
	service string
	// Instance は trace の ID を決める名前（同じ Record を使う daemon が 1 つでも、
	// 別の Record の daemon と ID が重ならないように）。既定は service。
	instance string
}

// Module は接続設定の module: otlp。
//
//	settings:
//	  endpoint: https://otel.example.com:4318   # /v1/traces を足す（パスを書けばそのまま）
//	  tls: {ca: …, cert: …, key: …}            # 省略可
//	  token_file: /etc/sakutei/otlp.token       # 省略可（Bearer）
//	  plaintext: true   # loopback でない宛先に http で送るとき（明示）
//	  service_name: sakutei
//	  instance: tokyo   # 省略可。trace の ID の種（既定は service_name）
//	  timeout: 10s
func Module(m map[string]any) (core.Sink, error) {
	r := settings.New("otlp", m)
	endpoint := r.Str("endpoint")
	t, tr := r.ReadTLS()
	tokenFile := r.Str("token_file")
	plaintext := r.Bool("plaintext")
	timeout := r.Duration("timeout", settings.DefaultTimeout)
	s := &Sink{service: r.Str("service_name"), instance: r.Str("instance")}
	if s.service == "" {
		s.service = "sakutei"
	}
	if s.instance == "" {
		s.instance = s.service
	}
	if endpoint == "" {
		r.Fail("endpoint に OTLP/HTTP の宛先を書く")
	} else if u, err := settings.CheckURL("endpoint", endpoint, plaintext); err != nil {
		r.Fail("%v", err)
	} else {
		if u.Path == "" || u.Path == "/" {
			u.Path = "/v1/traces"
		}
		s.url = u.String()
	}
	if err := r.Err(tr); err != nil {
		return nil, err
	}
	c, err := settings.NewClient(t, timeout, tokenFile)
	if err != nil {
		return nil, fmt.Errorf("otlp の sink: %w", err)
	}
	s.client = c
	return s, nil
}

// Deliver は batch を span にして送る。span が無ければ送らない。
func (s *Sink) Deliver(ctx context.Context, batch []core.Event) error {
	spans := s.Spans(batch)
	if len(spans) == 0 {
		return nil
	}
	body, err := json.Marshal(request{ResourceSpans: []resourceSpans{{
		Resource:   resource{Attributes: []attr{strAttr("service.name", s.service)}},
		ScopeSpans: []scopeSpans{{Scope: scope{Name: "sakutei"}, Spans: spans}},
	}}})
	if err != nil {
		return err
	}
	return s.client.PostJSON(ctx, s.url, body)
}

// Span は OTLP の JSON の span。
type Span struct {
	TraceID      string `json:"traceId"`
	SpanID       string `json:"spanId"`
	ParentSpanID string `json:"parentSpanId,omitempty"`
	Name         string `json:"name"`
	Kind         int    `json:"kind"`
	Start        string `json:"startTimeUnixNano"`
	End          string `json:"endTimeUnixNano"`
	Attributes   []attr `json:"attributes,omitempty"`
	Status       status `json:"status"`
}

type status struct {
	Code int `json:"code,omitempty"` // 0 unset・2 error
}

type attr struct {
	Key   string    `json:"key"`
	Value attrValue `json:"value"`
}

type attrValue struct {
	String *string `json:"stringValue,omitempty"`
	Int    *string `json:"intValue,omitempty"` // OTLP の JSON は 64 bit の整数を文字列で書く
}

type request struct {
	ResourceSpans []resourceSpans `json:"resourceSpans"`
}
type resourceSpans struct {
	Resource   resource     `json:"resource"`
	ScopeSpans []scopeSpans `json:"scopeSpans"`
}
type resource struct {
	Attributes []attr `json:"attributes"`
}
type scopeSpans struct {
	Scope scope  `json:"scope"`
	Spans []Span `json:"spans"`
}
type scope struct {
	Name string `json:"name"`
}

func strAttr(k, v string) attr { return attr{Key: k, Value: attrValue{String: &v}} }
func intAttr(k string, v int) attr {
	s := strconv.Itoa(v)
	return attr{Key: k, Value: attrValue{Int: &s}}
}

const spanInternal = 1

// spanNames は edge Event の span の名前。
var spanNames = map[string]string{"Applied": "sakutei.apply", "Deleted": "sakutei.delete", "ProcedureStep": "sakutei.procedure_step"}

// Spans は batch の span（送る順）。
func (s *Sink) Spans(batch []core.Event) []Span {
	var out, pending []Span
	for _, e := range batch {
		if e.Type != core.IncidentEvent {
			continue
		}
		if e.Kind != "CycleDone" {
			name := spanNames[e.Kind]
			if name == "" {
				name = "sakutei." + strings.ToLower(e.Kind)
			}
			at := nanos(e.At)
			sp := Span{SpanID: s.id(e.Seq, 8), Name: name, Kind: spanInternal, Start: at, End: at, Attributes: eventAttrs(e)}
			pending = append(pending, sp)
			continue
		}
		trace := s.id(e.Seq, 16)
		start := e.At
		if d, ok := e.Values["duration_ms"]; ok {
			start = e.At.Add(-time.Duration(d) * time.Millisecond)
		}
		cyc := Span{TraceID: trace, SpanID: s.id(e.Seq, 8), Name: "sakutei.cycle", Kind: spanInternal,
			Start: nanos(start), End: nanos(e.At), Attributes: eventAttrs(e)}
		if e.Values["failed"] > 0 || e.Values["halts"] > 0 {
			cyc.Status.Code = 2
		}
		out = append(out, cyc)
		for _, c := range pending {
			c.TraceID, c.ParentSpanID = trace, cyc.SpanID
			out = append(out, c)
		}
		pending = nil
	}
	for _, c := range pending {
		c.TraceID = s.idOf("root-"+c.SpanID, 16)
		out = append(out, c)
	}
	return out
}

func eventAttrs(e core.Event) []attr {
	out := []attr{strAttr("sakutei.kind", e.Kind), intAttr("sakutei.seq", int(e.Seq))}
	if e.Resource != "" {
		out = append(out, strAttr("sakutei.resource", e.Resource))
	}
	if e.Scope != "" {
		out = append(out, strAttr("sakutei.scope", e.Scope))
	}
	keys := make([]string, 0, len(e.Labels))
	for k := range e.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, strAttr("sakutei.label."+k, e.Labels[k]))
	}
	vkeys := make([]string, 0, len(e.Values))
	for k := range e.Values {
		vkeys = append(vkeys, k)
	}
	sort.Strings(vkeys)
	for _, k := range vkeys {
		out = append(out, intAttr("sakutei."+k, e.Values[k]))
	}
	return out
}

// id は Event の番号から決まる ID（n バイト）。trace と span で種を分ける。
func (s *Sink) id(seq uint64, n int) string {
	kind := "span-"
	if n == 16 {
		kind = "trace-"
	}
	return s.idOf(kind+strconv.FormatUint(seq, 10), n)
}

func (s *Sink) idOf(key string, n int) string {
	sum := sha256.Sum256([]byte(s.instance + "\x00" + key))
	return hex.EncodeToString(sum[:n])
}

func nanos(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }
