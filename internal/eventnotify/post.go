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

// post.go — JSON を POST する口（TLS・SecretRef のヘッダ）と、待ちを挙げる
// 送り直しの間隔。

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/render"
	"github.com/tjst-t/sakutei/v2/internal/statusreport"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// Options は sink の動きの調整（テスト用の値を差す口）。ゼロ値は既定。
type Options struct {
	// ErrOut は送れなかったことを書く先（nil なら捨てる）。
	ErrOut io.Writer
	// Now は時刻（nil なら time.Now）。
	Now func() time.Time
	// MinBackoff・MaxBackoff は送り直しの待ちの最小・最大（既定 1s・1m。
	// Alertmanager の最大は resend_interval を超えない）。
	MinBackoff, MaxBackoff time.Duration
	// MaxQueue は送り待ちの上限（既定 1000）。溢れたら古いものから捨てる。
	MaxQueue int
}

const (
	defaultMinBackoff = time.Second
	defaultMaxBackoff = time.Minute
	defaultMaxQueue   = 1000
)

func (o Options) withDefaults() Options {
	if o.ErrOut == nil {
		o.ErrOut = io.Discard
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MinBackoff <= 0 {
		o.MinBackoff = defaultMinBackoff
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = defaultMaxBackoff
	}
	if o.MaxBackoff < o.MinBackoff {
		o.MaxBackoff = o.MinBackoff
	}
	if o.MaxQueue <= 0 {
		o.MaxQueue = defaultMaxQueue
	}
	return o
}

// backoff は失敗が続くたびに倍にする待ち（min から max まで）。
type backoff struct {
	min, max, cur time.Duration
}

func (b *backoff) next() time.Duration {
	if b.cur == 0 {
		b.cur = b.min
	} else if b.cur *= 2; b.cur > b.max {
		b.cur = b.max
	}
	return b.cur
}

func (b *backoff) reset() { b.cur = 0 }

// poster は 1 つの送り先に JSON を POST する。
type poster struct {
	url     string
	client  *http.Client
	headers map[string]core.SecretRef
	secrets *render.FileSecretBackend
}

func newPoster(t Target, secretStore string) (*poster, error) {
	if err := CheckURL(t.URL); err != nil {
		return nil, err
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	switch {
	case t.CAFile != "":
		pem, err := os.ReadFile(t.CAFile)
		if err != nil {
			return nil, fmt.Errorf("ca file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca file %s: no PEM certificate", t.CAFile)
		}
		tlsCfg.RootCAs = pool
	case t.InsecureSkipVerify:
		tlsCfg.InsecureSkipVerify = true
	}
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	p := &poster{url: t.URL, client: &http.Client{Transport: tr, Timeout: timeout}, headers: t.Headers}
	if len(t.Headers) > 0 {
		if secretStore == "" {
			return nil, fmt.Errorf("headers need a secret_store")
		}
		p.secrets = render.NewFileSecretBackend(secretStore)
	}
	return p, nil
}

// post は body を POST する。2xx でなければエラー。ヘッダの値は要求の直前に
// 解決し（JIT、§20.2）、エラーの文言には出さない。
func (p *poster) post(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "sakutei")
	for _, name := range sortedKeys(p.headers) {
		ref := p.headers[name]
		v, err := p.secrets.ResolveValue(ctx, ref.Backend(), ref.Path(), ref.Generation())
		if err != nil {
			return fmt.Errorf("header %s (%s): %w", name, ref, err)
		}
		req.Header.Set(name, v)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &statusError{code: resp.StatusCode, body: strings.Join(strings.Fields(string(snippet)), " ")}
	}
	return nil
}

// statusError は 2xx でない応答。
type statusError struct {
	code int
	body string
}

func (e *statusError) Error() string {
	if e.body == "" {
		return fmt.Sprintf("HTTP %d", e.code)
	}
	return fmt.Sprintf("HTTP %d: %s", e.code, e.body)
}

// rejected は err が送り直しても通らない応答（408・429 を除く 4xx）かを返す。
// 送り先が中身を受け付けないものは、待ち行列の先頭で後ろを堰き止めないように
// 捨てる（ログには出す）。
func rejected(err error) bool {
	var se *statusError
	if !errors.As(err, &se) {
		return false
	}
	return se.code >= 400 && se.code < 500 && se.code != http.StatusRequestTimeout && se.code != http.StatusTooManyRequests
}

// wait は d だけ待つ。stop が閉じたら早めに false で戻る。
func wait(d time.Duration, stop <-chan struct{}) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-stop:
		return false
	}
}

// worker は送る goroutine の始まりと終わり（Close で送り待ちを送り切る）と、
// 送りの数（宛先ごとの状態、#154）。
type worker struct {
	opt    Options
	st     stats
	ctx    context.Context
	cancel context.CancelFunc
	// wake は送るものが増えた知らせ（1 つだけ溜まる）。
	wake chan struct{}
	// closing は Close が始まった（送り待ちが空になったら終わる）。abort は
	// Close の ctx が切れた（送り待ちを残して終わる）。
	closing, abort, done chan struct{}
	closeOnce, abortOnce sync.Once
}

func (k *worker) start(o Options, run func()) {
	k.opt = o.withDefaults()
	k.ctx, k.cancel = context.WithCancel(context.Background())
	k.wake = make(chan struct{}, 1)
	k.closing, k.abort, k.done = make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(k.done)
		run()
	}()
}

func (k *worker) poke() {
	select {
	case k.wake <- struct{}{}:
	default:
	}
}

// idle は送るものが増えるまで（d > 0 なら最長 d）待つ。Close が始まったら
// false。
func (k *worker) idle(d time.Duration) bool {
	var tick <-chan time.Time
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		tick = t.C
	}
	select {
	case <-k.wake:
		return true
	case <-tick:
		return true
	case <-k.closing:
		return false
	case <-k.abort:
		return false
	}
}

func (k *worker) closingNow() bool {
	select {
	case <-k.closing:
		return true
	default:
		return false
	}
}

func (k *worker) aborted() bool {
	select {
	case <-k.abort:
		return true
	default:
		return false
	}
}

// close は goroutine に送り切らせて止める。ctx が先に切れたら諦め、残った
// 数をエラーで返す。
func (k *worker) close(ctx context.Context, name string, pending func() int) error {
	k.closeOnce.Do(func() { close(k.closing) })
	select {
	case <-k.done:
	case <-ctx.Done():
		k.abortOnce.Do(func() { close(k.abort); k.cancel() })
		<-k.done
	}
	k.cancel()
	if n := pending(); n > 0 {
		return fmt.Errorf("%s: %d not delivered before shutdown", name, n)
	}
	return nil
}

// stats は 1 つの宛先への送りの数（#154: /metrics と status に宛先ごとに出す）。
// 数は要求（POST）の単位。webhook は 1 要求 = 1 Event、Alertmanager は
// 1 要求 = その時点の alert の全部。
type stats struct {
	mu                    sync.Mutex
	delivered, failed     int
	rejected, dropped     int
	consecutive           int
	lastSuccess, lastFail time.Time
	lastError             string
}

func (s *stats) ok(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delivered++
	s.consecutive = 0
	s.lastSuccess = now
}

// fail は送れなかった（送り直す）要求。
func (s *stats) fail(now time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed++
	s.consecutive++
	s.lastFail, s.lastError = now, err.Error()
}

// reject は送り先が受け付けなかった（捨てた）要求。届いてはいるので続けての
// 失敗には数えない。
func (s *stats) reject(now time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejected++
	s.consecutive = 0
	s.lastFail, s.lastError = now, err.Error()
}

// drop は待ち行列が溢れて捨てたもの。
func (s *stats) drop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropped++
}

// status は数を statusreport.NotifyHealth に写す（Sink・URL・Pending・Active は
// 呼び手が埋める）。
func (s *stats) status() statusreport.NotifyHealth {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := statusreport.NotifyHealth{
		Delivered: s.delivered, Failed: s.failed, Rejected: s.rejected, Dropped: s.dropped,
		ConsecutiveFailures: s.consecutive, LastError: s.lastError,
	}
	if !s.lastSuccess.IsZero() {
		t := s.lastSuccess.UTC()
		h.LastSuccess = &t
	}
	if !s.lastFail.IsZero() {
		t := s.lastFail.UTC()
		h.LastFailure = &t
	}
	return h
}
