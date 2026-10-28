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

// set.go — 通知の口の全部（#154）。宛先が複数あるとき（Alertmanager を複数台で
// 動かす構成では、送る側がすべての Alertmanager に同じ alert を送り、重複は
// Alertmanager どうしが消す）、宛先ごとに 1 つの sink を作る。sink はそれぞれ
// 送る goroutine・送り直し（resend_interval）・待ち（backoff）・待ち行列を
// 持つので、1 台が落ちていても他の宛先へは送り続け、戻った宛先には待って
// いた分と firing の今を送り直す。

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/tjst-t/sakutei/v2/internal/eventlog"
	"github.com/tjst-t/sakutei/v2/internal/statusreport"
)

// Destination は 1 つの宛先の sink（*Webhook・*Alertmanager）。
type Destination interface {
	eventlog.Sink
	Status() statusreport.NotifyHealth
	Close(ctx context.Context) error
}

// Set は通知の口の宛先の全部。
type Set struct {
	dests []Destination
}

// SplitURLs は CLI の宛先の値（フラグを繰り返したものと、それぞれの "," で
// つないだもの）を 1 つずつに分ける。空の要素は捨てる。
func SplitURLs(values []string) []string {
	var out []string
	for _, v := range values {
		for _, u := range strings.Split(v, ",") {
			if u = strings.TrimSpace(u); u != "" {
				out = append(out, u)
			}
		}
	}
	return out
}

// Open は webhooks・alertmanagers（CLI の宛先）と cfg の urls を合わせた宛先
// ごとに sink を作り、送り始める。同じ宛先は 1 つにまとめる。宛先が 1 つでも
// 作れなければ、作ったものを止めてエラー。宛先が無ければ空の Set。
func Open(cfg Config, webhooks, alertmanagers []string, o Options) (*Set, error) {
	s := &Set{}
	fail := func(err error) (*Set, error) {
		_ = s.Close(canceled())
		return nil, err
	}
	for _, u := range merge(webhooks, cfg.Webhook.URLs, func(u string) string { return u }) {
		t := cfg.Webhook
		t.URL, t.URLs = u, nil
		w, err := NewWebhook(t, cfg.SecretStore, o)
		if err != nil {
			return fail(err)
		}
		s.dests = append(s.dests, w)
	}
	amKey := func(u string) string { return strings.TrimRight(u, "/") }
	for _, u := range merge(alertmanagers, cfg.Alertmanager.URLs, amKey) {
		t := cfg.Alertmanager
		t.URL, t.URLs = u, nil
		a, err := NewAlertmanager(t, cfg.SecretStore, o)
		if err != nil {
			return fail(err)
		}
		s.dests = append(s.dests, a)
	}
	return s, nil
}

// merge は CLI の宛先のあとに設定の宛先を並べ、key が同じものを 1 つにする。
func merge(cli, conf []string, key func(string) string) []string {
	seen := map[string]bool{}
	var out []string
	for _, u := range append(append([]string(nil), cli...), conf...) {
		if k := key(u); !seen[k] {
			seen[k] = true
			out = append(out, u)
		}
	}
	return out
}

func canceled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// Len は宛先の数。
func (s *Set) Len() int { return len(s.dests) }

// Sinks は宛先ごとの sink（eventlog.Route・reconcile.Config.EventSinks に渡す）。
func (s *Set) Sinks() []eventlog.Sink {
	out := make([]eventlog.Sink, len(s.dests))
	for i, d := range s.dests {
		out[i] = d
	}
	return out
}

// Status は宛先ごとの送りの状態（開いた順）。
func (s *Set) Status() []statusreport.NotifyHealth {
	out := make([]statusreport.NotifyHealth, len(s.dests))
	for i, d := range s.dests {
		out[i] = d.Status()
	}
	return out
}

// Close は宛先の全部を同時に閉じる（ctx の間だけ送り切ろうとする）。落ちて
// いる宛先が ctx を使い切っても、届く宛先の送り切りは妨げない。
func (s *Set) Close(ctx context.Context) error {
	errs := make([]error, len(s.dests))
	var wg sync.WaitGroup
	for i, d := range s.dests {
		wg.Add(1)
		go func(i int, d Destination) {
			defer wg.Done()
			errs[i] = d.Close(ctx)
		}(i, d)
	}
	wg.Wait()
	return errors.Join(errs...)
}
