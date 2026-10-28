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

package eventnotify

// alertmanager.go — Event を Alertmanager の API v2（POST /api/v2/alerts）の
// alert に写して送る sink（--event-alertmanager）。
//
// 写し方（§22.3）:
//
//   - labels: alertname（= kind）・kind・severity・resource・scope・class・
//     provider（あれば）と、設定の labels。alert の同一性は labels の組なので、
//     同じ level のキー（eventlog.Key）は同じ labels になる。
//   - annotations: message・cause（あれば）・since・count。
//   - level の Event の firing: 続いている alert として覚え（S3、メモリだけ）、
//     startsAt = since、endsAt = 送る時刻 + 4 × resend_interval で送る。
//     Sakutei の level は続く間は毎周出さないので、ここで resend_interval ごとに
//     送り直し、Alertmanager の期限（endsAt）が切れないようにする。Sakutei が
//     止まったら送り直しも止まり、alert は endsAt で自然に消える。
//   - 更新の firing で labels が変わった（stuck に上がって severity・class が
//     変わった）とき: 前の labels の alert を resolved で送り、新しい labels で
//     firing にする。
//   - resolved: 覚えた alert を endsAt = その時刻で 1 回送って忘れる。覚えて
//     いない（起こし直した後など）resolved は送らない（前の alert は endsAt で
//     消える）。
//   - edge の Event（と、一回性の reconcile の state の無い Event）: severity が
//     warning 以上のものだけ、startsAt = 時刻、endsAt = 時刻 + edge_ttl の
//     alert として 1 回送る（送り直さない）。info の edge（Pruned・Workflow の
//     進み）は送らない（webhook・--event-log には出る）。
//
// 送れないときは、待ちを挙げながら（最大 resend_interval）その時点の全部を
// 送り直す。収束は止めない。

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/eventlog"
	"github.com/tjst-t/sakutei/v2/internal/statusreport"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// reservedLabels は Event から決める label（設定の labels で書けない）。
var reservedLabels = map[string]bool{
	"alertname": true, "kind": true, "severity": true, "resource": true,
	"scope": true, "class": true, "provider": true,
}

// Alert は Alertmanager の API v2 の postableAlert。
type Alert struct {
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	StartsAt     time.Time         `json:"startsAt,omitempty"`
	EndsAt       time.Time         `json:"endsAt,omitempty"`
	GeneratorURL string            `json:"generatorURL,omitempty"`
}

// Alertmanager は Event を Alertmanager に送る sink。
type Alertmanager struct {
	p      *poster
	o      worker
	name   string
	url    string
	resend time.Duration
	edge   time.Duration
	labels map[string]string

	mu sync.Mutex
	// active は firing の alert（level のキー → alert。endsAt は送るときに埋める）。
	active map[string]Alert
	// pending は 1 回だけ送る alert（resolved と edge）。
	pending []Alert
}

// NewAlertmanager は t の送り先（Alertmanager の base URL。/api/v2/alerts は
// 足す）に送る Alertmanager を作り、送る goroutine を始める。止めるのは Close。
func NewAlertmanager(t Target, secretStore string, o Options) (*Alertmanager, error) {
	if err := CheckURL(t.URL); err != nil {
		return nil, fmt.Errorf("event alertmanager: %w", err)
	}
	base := t.URL
	t.URL = strings.TrimRight(base, "/") + "/api/v2/alerts"
	p, err := newPoster(t, secretStore)
	if err != nil {
		return nil, fmt.Errorf("event alertmanager: %w", err)
	}
	a := &Alertmanager{p: p, name: "alertmanager " + redact(base), url: redact(base),
		resend: t.ResendInterval, edge: t.EdgeTTL, labels: t.Labels, active: map[string]Alert{}}
	if a.resend <= 0 {
		a.resend = DefaultResendInterval
	}
	if a.edge <= 0 {
		a.edge = DefaultEdgeTTL
	}
	if o.MaxBackoff <= 0 || o.MaxBackoff > a.resend {
		o.MaxBackoff = a.resend
	}
	a.o.start(o, a.run)
	return a, nil
}

// Emit は ev を alert に写して送り待ちにする。
func (a *Alertmanager) Emit(_ context.Context, ev core.Event) error {
	now := a.o.opt.Now()
	if ev.Time.IsZero() {
		ev.Time = now
	}
	a.mu.Lock()
	err := a.observe(ev)
	a.mu.Unlock()
	a.o.poke()
	return err
}

func (a *Alertmanager) observe(ev core.Event) error {
	tracked := ev.Kind.IsLevel() && ev.State != ""
	switch {
	case tracked && ev.State == core.EventFiring:
		k := eventlog.Key(ev)
		al := a.alertOf(ev)
		al.StartsAt = ev.Since
		if al.StartsAt.IsZero() {
			al.StartsAt = ev.Time
		}
		if prev, ok := a.active[k]; ok && !sameLabels(prev.Labels, al.Labels) {
			prev.EndsAt = ev.Time
			a.push(prev)
		}
		a.active[k] = al
	case tracked && ev.State == core.EventResolved:
		k := eventlog.Key(ev)
		prev, ok := a.active[k]
		if !ok {
			return nil
		}
		delete(a.active, k)
		prev.EndsAt = ev.Time
		a.push(prev)
	default:
		if ev.Severity != core.SeverityWarning && ev.Severity != core.SeverityCritical {
			return nil
		}
		al := a.alertOf(ev)
		al.StartsAt = ev.Time
		al.EndsAt = ev.Time.Add(a.edge)
		return a.push(al)
	}
	return nil
}

// push は 1 回だけ送る alert を積む（溢れたら古いものを捨ててエラー）。
func (a *Alertmanager) push(al Alert) error {
	a.pending = append(a.pending, al)
	if len(a.pending) <= a.o.opt.MaxQueue {
		return nil
	}
	dropped := a.pending[0]
	a.pending = a.pending[1:]
	a.o.st.drop()
	return fmt.Errorf("%s: %d alerts waiting; dropped the oldest (%s)", a.name, a.o.opt.MaxQueue, dropped.Labels["alertname"])
}

// alertOf は ev の labels と annotations。
func (a *Alertmanager) alertOf(ev core.Event) Alert {
	sev := ev.Severity
	if sev == "" {
		sev = core.SeverityInfo
	}
	l := map[string]string{}
	for k, v := range a.labels {
		l[k] = v
	}
	l["alertname"] = string(ev.Kind)
	l["kind"] = string(ev.Kind)
	l["severity"] = string(sev)
	scope := ev.Scope
	if ev.HasResource() {
		l["resource"] = ev.Resource.String()
		if len(scope.Labels) == 0 {
			scope = ev.Resource.Scope
		}
	}
	if len(scope.Labels) > 0 {
		l["scope"] = scope.String()
	}
	if ev.Class != "" {
		l["class"] = string(ev.Class)
	}
	if p := ev.Attrs["provider"]; p != "" {
		l["provider"] = p
	}
	ann := map[string]string{"message": ev.Message}
	if ev.Cause != "" {
		ann["cause"] = string(ev.Cause)
	}
	if !ev.Since.IsZero() {
		ann["since"] = ev.Since.UTC().Format(time.RFC3339)
	}
	if ev.Count > 0 {
		ann["count"] = strconv.Itoa(ev.Count)
	}
	return Alert{Labels: l, Annotations: ann}
}

func sameLabels(x, y map[string]string) bool {
	if len(x) != len(y) {
		return false
	}
	for k, v := range x {
		if w, ok := y[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// Active は firing の alert の数。
func (a *Alertmanager) Active() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.active)
}

// Pending は 1 回だけ送る alert のうち、まだ届いていない数。
func (a *Alertmanager) Pending() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.pending)
}

// Status はこの宛先への送りの状態（#154）。
func (a *Alertmanager) Status() statusreport.NotifyHealth {
	h := a.o.st.status()
	h.Sink, h.URL = "alertmanager", a.url
	a.mu.Lock()
	h.Pending, h.Active = len(a.pending), len(a.active)
	a.mu.Unlock()
	return h
}

// Close は届いていない alert（resolved・edge と、firing の今の endsAt）を
// ctx の間だけ送り切ろうとしてから止める。firing の alert は送り直しが止まる
// ので、endsAt で Alertmanager から消える。
func (a *Alertmanager) Close(ctx context.Context) error { return a.o.close(ctx, a.name, a.Pending) }

// batch はいま送る alert（firing の全部と、1 回だけ送るもの）と、そのうち
// 1 回だけ送るものの数。
func (a *Alertmanager) batch() ([]Alert, int) {
	now := a.o.opt.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Alert, 0, len(a.active)+len(a.pending))
	for _, k := range sortedKeys(a.active) {
		al := a.active[k]
		al.EndsAt = now.Add(4 * a.resend)
		out = append(out, al)
	}
	out = append(out, a.pending...)
	return out, len(a.pending)
}

// sent は 1 回だけ送るもののうち先頭の n 個を届いたとして外す。
func (a *Alertmanager) sent(n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pending = a.pending[n:]
}

func (a *Alertmanager) run() {
	b := backoff{min: a.o.opt.MinBackoff, max: a.o.opt.MaxBackoff}
	for {
		alerts, n := a.batch()
		if len(alerts) > 0 {
			body, err := json.Marshal(alerts)
			if err == nil {
				err = a.p.post(a.o.ctx, body)
			}
			switch {
			case err == nil:
				a.o.st.ok(a.o.opt.Now())
				a.sent(n)
				b.reset()
			case a.o.aborted():
				return
			case rejected(err):
				a.o.st.reject(a.o.opt.Now(), err)
				fmt.Fprintf(a.o.opt.ErrOut, "sakutei: warn: event sink: %s: %d alert(s) rejected, the one-shot ones dropped: %v (convergence continues)\n", a.name, len(alerts), err)
				a.sent(n)
			default:
				a.o.st.fail(a.o.opt.Now(), err)
				d := b.next()
				fmt.Fprintf(a.o.opt.ErrOut, "sakutei: warn: event sink: %s: %d alert(s) not delivered: %v; retrying in %s (convergence continues)\n", a.name, len(alerts), err, d)
				if !wait(d, a.o.abort) {
					return
				}
				continue
			}
		}
		if a.o.closingNow() {
			// Close: the one-shot alerts are out (or there were none).
			return
		}
		if !a.o.idle(a.resend) {
			// Close began while idle: one last round sends what is left.
			if a.Pending() == 0 {
				return
			}
		}
	}
}
