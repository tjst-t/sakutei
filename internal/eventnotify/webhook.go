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

// webhook.go — Event を JSON で 1 件ずつ POST する sink（--event-webhook）。
//
// 本文は core.Event の JSON（--event-log の 1 行と同じ形）。Emit は待ち行列に
// 積むだけで戻り、送るのは 1 本の goroutine（出た順に送る）。送れないときは
// 先頭を待ちを挙げながら送り直す。408・429 を除く 4xx は送り直しても通らない
// ので、ログに出して捨てる。待ち行列が溢れたら古いものから捨てる。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sync"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// Webhook は Event を webhook に送る sink。
type Webhook struct {
	p    *poster
	o    worker
	name string

	mu    sync.Mutex
	queue []core.Event
}

// NewWebhook は t の送り先に送る Webhook を作り、送る goroutine を始める。
// 止めるのは Close。
func NewWebhook(t Target, secretStore string, o Options) (*Webhook, error) {
	p, err := newPoster(t, secretStore)
	if err != nil {
		return nil, fmt.Errorf("event webhook: %w", err)
	}
	w := &Webhook{p: p, name: "webhook " + redact(t.URL)}
	w.o.start(o, w.run)
	return w, nil
}

// Emit は ev を送り待ちに積む。溢れて古いものを捨てたときはエラー（収束は
// 止めない。Route がログに出す）。
func (w *Webhook) Emit(_ context.Context, ev core.Event) error {
	if ev.Time.IsZero() {
		ev.Time = w.o.opt.Now()
	}
	w.mu.Lock()
	w.queue = append(w.queue, ev)
	var dropped core.Event
	over := len(w.queue) > w.o.opt.MaxQueue
	if over {
		dropped = w.queue[0]
		w.queue = w.queue[1:]
	}
	w.mu.Unlock()
	w.o.poke()
	if over {
		return fmt.Errorf("%s: %d Events waiting; dropped the oldest (%s)", w.name, w.o.opt.MaxQueue, dropped.Kind)
	}
	return nil
}

// Pending は送り待ちの Event の数。
func (w *Webhook) Pending() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.queue)
}

// Close は送り待ちを ctx の間だけ送り切ろうとしてから止める。
func (w *Webhook) Close(ctx context.Context) error { return w.o.close(ctx, w.name, w.Pending) }

func (w *Webhook) head() (core.Event, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.queue) == 0 {
		return core.Event{}, false
	}
	return w.queue[0], true
}

func (w *Webhook) pop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.queue) > 0 {
		w.queue = w.queue[1:]
	}
}

func (w *Webhook) run() {
	b := backoff{min: w.o.opt.MinBackoff, max: w.o.opt.MaxBackoff}
	for {
		ev, ok := w.head()
		if !ok {
			if !w.o.idle(0) {
				return
			}
			continue
		}
		body, err := json.Marshal(ev)
		if err == nil {
			err = w.p.post(w.o.ctx, body)
		}
		if err == nil {
			w.pop()
			b.reset()
			continue
		}
		if w.o.aborted() {
			return
		}
		if rejected(err) {
			fmt.Fprintf(w.o.opt.ErrOut, "sakutei: warn: event sink: %s: %s rejected, dropped: %v (convergence continues)\n", w.name, ev.Kind, err)
			w.pop()
			continue
		}
		d := b.next()
		fmt.Fprintf(w.o.opt.ErrOut, "sakutei: warn: event sink: %s: %s not delivered: %v; retrying in %s (convergence continues)\n", w.name, ev.Kind, err, d)
		if !wait(d, w.o.abort) {
			return
		}
	}
}

// redact は URL の資格（userinfo）と query を伏せる。
func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(bad url)"
	}
	u.User = nil
	if u.RawQuery != "" {
		u.RawQuery = "…"
	}
	return u.String()
}
