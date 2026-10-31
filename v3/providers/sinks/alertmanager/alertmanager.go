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

// Package alertmanager は Event を Alertmanager の API v2（POST /api/v2/alerts）に
// 送る送り先（module: alertmanager、設計 §3.9・§5.6、E-H2・E-T3）。v2 の
// --event-alertmanager（複数の宛先・送り直し）を移した。
//
// 写し方：
//
//   - labels：alertname（＝ Event の種類）・resource か scope・stuck（stuck に
//     上がったときだけ "true"）・宣言の labels（Prometheus の名前に直す）・設定の
//     labels。同じキーの Event は同じ labels になる。
//   - annotations：message・since・count。
//   - 続いている状態（firing）は周ごとに今の一覧（core.StateSink）を
//     startsAt = since・endsAt = 送る時刻 + ttl で送り直す。Sakutei が止まれば
//     送り直しも止まり、alert は endsAt で消える。
//   - 送り出しの列の resolved は endsAt = その時刻で 1 回送る。stuck に上がった
//     firing は、stuck の印の無い前の alert を resolved にする。
//   - 出来事型は設定の incidents に書いた種類だけ、startsAt = 時刻・
//     endsAt = 時刻 + incident_ttl で 1 回送る。
//
// 宛先（urls）は全部に同じものを送る（Alertmanager を複数台で動かす構成。台の
// 間は gossip で揃う）。1 台でも受け取れば送れたとし、全部に届かないときだけ
// 誤りを返して次の周に同じ位置から送り直す（不変条件 28）。
package alertmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/providers/sinks/internal/settings"
)

// 既定値。
const (
	DefaultTTL         = 10 * time.Minute
	DefaultIncidentTTL = time.Hour
)

// Alert は Alertmanager の API v2 の postableAlert。
type Alert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations,omitempty"`
	StartsAt    time.Time         `json:"startsAt,omitempty"`
	EndsAt      time.Time         `json:"endsAt,omitempty"`
}

// Sink は Alertmanager に送る送り先。
type Sink struct {
	urls        []string
	client      *settings.Client
	labels      map[string]string
	ttl         time.Duration
	incidentTTL time.Duration
	incidents   map[string]bool
	// Now は送り直しの endsAt の時刻（テストで差し替える）。
	Now func() time.Time
}

// reserved は Event から決める label（設定の labels・宣言の labels で書けない）。
var reserved = map[string]bool{"alertname": true, "resource": true, "scope": true, "stuck": true}

var labelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// Module はつなぎ方の設定の module: alertmanager。
//
//	settings:
//	  urls: [https://am1.example.com:9093, https://am2.example.com:9093]  # base URL
//	  tls: {ca: /etc/sakutei/am-ca.pem, cert: …, key: …}               # 省略可
//	  token_file: /etc/sakutei/am.token                               # 省略可（Bearer）
//	  plaintext: true        # loopback でない宛先に http で送るとき（明示）
//	  timeout: 10s
//	  ttl: 10m               # firing の endsAt（周の間隔より長く）
//	  incident_ttl: 1h       # 出来事型の alert の長さ
//	  incidents: [Applied]   # alert にする出来事型（既定は無し）
//	  labels: {instance: sakutei-tokyo}
func Module(m map[string]any) (core.Sink, error) {
	r := settings.New("alertmanager", m)
	urls := r.Strs("urls")
	t, tr := r.ReadTLS()
	tokenFile := r.Str("token_file")
	plaintext := r.Bool("plaintext")
	timeout := r.Duration("timeout", settings.DefaultTimeout)
	s := &Sink{labels: r.StrMap("labels"), ttl: r.Duration("ttl", DefaultTTL),
		incidentTTL: r.Duration("incident_ttl", DefaultIncidentTTL), incidents: map[string]bool{}, Now: time.Now}
	for _, k := range r.Strs("incidents") {
		s.incidents[k] = true
	}
	if len(urls) == 0 {
		r.Fail("urls に Alertmanager の base URL を 1 つ以上書く")
	}
	for _, u := range urls {
		pu, err := settings.CheckURL("urls", u, plaintext)
		if err != nil {
			r.Fail("%v", err)
			continue
		}
		s.urls = append(s.urls, strings.TrimRight(pu.String(), "/")+"/api/v2/alerts")
	}
	for k := range s.labels {
		if !labelName.MatchString(k) || reserved[k] {
			r.Fail("labels.%s は使えない名前（英数字と _、%s は Event から決める）", k, "alertname・resource・scope・stuck")
		}
	}
	if err := r.Err(tr); err != nil {
		return nil, err
	}
	c, err := settings.NewClient(t, timeout, tokenFile)
	if err != nil {
		return nil, fmt.Errorf("alertmanager の送り先: %w", err)
	}
	s.client = c
	return s, nil
}

// Deliver は送り出しの列の resolved と、設定した出来事型を 1 回送る。firing は
// DeliverState が今の一覧から送る。
func (s *Sink) Deliver(ctx context.Context, batch []core.Event) error {
	var alerts []Alert
	for _, e := range batch {
		switch {
		case e.Type == core.StateEvent && e.State == core.Resolved:
			al := s.alertOf(e)
			al.StartsAt, al.EndsAt = since(e), e.At
			alerts = append(alerts, al)
		case e.Type == core.StateEvent && e.Stuck:
			// stuck に上がった：印の無い前の alert を閉じる（新しい alert は
			// 今の一覧から送る）。
			prev := e
			prev.Stuck = false
			al := s.alertOf(prev)
			al.StartsAt, al.EndsAt = since(e), e.At
			alerts = append(alerts, al)
		case e.Type == core.IncidentEvent && s.incidents[e.Kind]:
			al := s.alertOf(e)
			al.StartsAt, al.EndsAt = e.At, e.At.Add(s.incidentTTL)
			alerts = append(alerts, al)
		}
	}
	return s.post(ctx, alerts)
}

// DeliverState は今 firing の全部を endsAt = 今 + ttl で送り直す（E-H2）。
func (s *Sink) DeliverState(ctx context.Context, firing []core.Event) error {
	ends := s.Now().Add(s.ttl)
	alerts := make([]Alert, 0, len(firing))
	for _, e := range firing {
		al := s.alertOf(e)
		al.StartsAt, al.EndsAt = since(e), ends
		alerts = append(alerts, al)
	}
	return s.post(ctx, alerts)
}

func since(e core.Event) time.Time {
	if e.Since != nil {
		return *e.Since
	}
	return e.At
}

func (s *Sink) alertOf(e core.Event) Alert {
	l := map[string]string{}
	for k, v := range e.Labels {
		if n := promName(k); n != "" && !reserved[n] {
			l[n] = v
		}
	}
	for k, v := range s.labels {
		l[k] = v
	}
	l["alertname"] = e.Kind
	if e.Resource != "" {
		l["resource"] = e.Resource
	} else if e.Scope != "" {
		l["scope"] = e.Scope
	}
	if e.Stuck {
		l["stuck"] = "true"
	}
	ann := map[string]string{}
	if e.Message != "" {
		ann["message"] = e.Message
	}
	if e.Since != nil {
		ann["since"] = e.Since.UTC().Format(time.RFC3339)
	}
	if e.Count > 0 {
		ann["count"] = strconv.Itoa(e.Count)
	}
	return Alert{Labels: l, Annotations: ann}
}

// promName は宣言の labels の名前を Prometheus の label の名前に直す。
func promName(k string) string {
	b := []byte(k)
	for i, c := range b {
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9') {
			b[i] = '_'
		}
	}
	return string(b)
}

// post は全部の宛先に送る。1 台でも受け取れば nil。
func (s *Sink) post(ctx context.Context, alerts []Alert) error {
	if len(alerts) == 0 {
		return nil
	}
	body, err := json.Marshal(alerts)
	if err != nil {
		return err
	}
	type result struct {
		i   int
		err error
	}
	ch := make(chan result, len(s.urls))
	for i, u := range s.urls {
		go func(i int, u string) { ch <- result{i, s.client.PostJSON(ctx, u, body)} }(i, u)
	}
	errs := make([]error, len(s.urls))
	ok := false
	for range s.urls {
		r := <-ch
		errs[r.i] = r.err
		ok = ok || r.err == nil
	}
	if ok {
		return nil
	}
	return fmt.Errorf("どの Alertmanager にも届かない（%d 件）: %w", len(alerts), errors.Join(errs...))
}
