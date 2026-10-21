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

package eventlog

// Event の sink（§22.3: 発生源と sink を分ける）。
//
// 収束コアは Event を io.Writer に Emit するだけで、その先を知らない。Route が
// その Emit を Sink に渡す。Sink は今のところ 2 つ:
//
//   - StreamSink: 標準エラー・標準出力（--log-format に従う。従来の出力）。
//   - FileSink:   JSON lines のファイル（--event-log <path>。追記・1 行ずつ書き切る）。
//
// ルーティング・エスカレーション・沈黙は Sakutei が持たず、sink の先
// （Alertmanager など）に委ねる。sink の失敗は収束を止めない（ログに 1 行出して
// 続ける。衛星の停止は変化を止めるが維持を止めない、§3 原則 8 と同じ考え）。
//
// S4 監査ログ（internal/audit）とは別物（監査は改竄耐性を持つ別の記録、§26.3）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/pkg/core"
)

// Sink は Event の consumer。Emit がエラーを返しても、呼び手は収束を止めない。
type Sink interface {
	Emit(ctx context.Context, ev core.Event) error
}

// SinkFunc は関数を Sink にする。
type SinkFunc func(ctx context.Context, ev core.Event) error

// Emit は f(ctx, ev) を返す。
func (f SinkFunc) Emit(ctx context.Context, ev core.Event) error { return f(ctx, ev) }

// FanOut は Event を並んだ順にすべての sink に配る。ある sink が失敗しても残りには
// 配り、失敗をまとめて返す。
type FanOut []Sink

// Emit は ev を各 sink に配る。
func (f FanOut) Emit(ctx context.Context, ev core.Event) error {
	var errs []error
	for _, s := range f {
		if s == nil {
			continue
		}
		if err := s.Emit(ctx, ev); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// IsNotice は ev が知らせ（Event）かを返す。周の要約（CycleSummary）と Event で
// ないログ行（Log）は、標準エラー・標準出力の行を同じ形にするための kind で、
// 知らせではない。
func IsNotice(ev core.Event) bool {
	return ev.Kind != core.EventCycleSummary && ev.Kind != core.EventLog
}

// NoticesOnly は知らせ（IsNotice）だけを s に渡す Sink を返す。
func NoticesOnly(s Sink) Sink {
	return SinkFunc(func(ctx context.Context, ev core.Event) error {
		if !IsNotice(ev) {
			return nil
		}
		return s.Emit(ctx, ev)
	})
}

// StreamSink は w（標準エラー・標準出力）に出す Sink。w が Writer なら
// --log-format の形で、そうでなければ text で書く（Emit と同じ）。
type StreamSink struct {
	W io.Writer
}

// Emit は ev を W に出す。
func (s StreamSink) Emit(_ context.Context, ev core.Event) error {
	Emit(s.W, ev)
	return nil
}

// FileSink は Event を JSON lines でファイルに追記する Sink（--event-log）。
//
// 1 Event を 1 回の write で 1 行書き切る（O_APPEND。ほかの書き手と行が混ざらない）。
// 書けなかったときはファイルを閉じ、次の Emit で開き直す（ログの回転にも追従する）。
type FileSink struct {
	mu   sync.Mutex
	path string
	f    *os.File
	now  func() time.Time
}

// OpenFileSink は path を追記で開く（無ければ作る）。開けなければエラー。
func OpenFileSink(path string) (*FileSink, error) {
	s := &FileSink{path: path, now: time.Now}
	if err := s.openLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *FileSink) openLocked() error {
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o640)
	if err != nil {
		return fmt.Errorf("event log %s: %w", s.path, err)
	}
	s.f = f
	return nil
}

// Path は書き先のファイルを返す。
func (s *FileSink) Path() string { return s.path }

// Emit は ev を JSON の 1 行で追記する。
func (s *FileSink) Emit(_ context.Context, ev core.Event) error {
	if ev.Time.IsZero() {
		ev.Time = s.now()
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("event log %s: %w", s.path, err)
	}
	b = append(b, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		if err := s.openLocked(); err != nil {
			return err
		}
	}
	if _, err := s.f.Write(b); err != nil {
		_ = s.f.Close()
		s.f = nil
		return fmt.Errorf("event log %s: %w", s.path, err)
	}
	return nil
}

// Close はファイルを閉じる。
func (s *FileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// Route は w に Emit された Event を、w 自身（StreamSink）と extra の sink に配る
// Emitter を返す。extra には知らせ（IsNotice）だけを渡す。Event でない書き込みは
// w にそのまま渡す。
//
// extra の sink が失敗したら、errOut に 1 行書いて続ける（収束を止めない）。
// extra が空なら w をそのまま返す。
func Route(ctx context.Context, w io.Writer, extra []Sink, errOut io.Writer) io.Writer {
	if len(extra) == 0 {
		return w
	}
	if errOut == nil {
		errOut = io.Discard
	}
	return &routed{ctx: ctx, w: w, extra: FanOut(extra), errOut: errOut, now: time.Now}
}

type routed struct {
	ctx    context.Context
	w      io.Writer
	extra  FanOut
	errOut io.Writer
	now    func() time.Time
}

func (r *routed) Write(p []byte) (int, error) { return r.w.Write(p) }

func (r *routed) Emit(ev core.Event) {
	// どの sink にも同じ時刻で出す。
	if ev.Time.IsZero() {
		ev.Time = r.now()
	}
	_ = StreamSink{W: r.w}.Emit(r.ctx, ev)
	if !IsNotice(ev) {
		return
	}
	if err := r.extra.Emit(r.ctx, ev); err != nil {
		fmt.Fprintf(r.errOut, "sakutei: warn: event sink: %s not delivered: %v (convergence continues)\n", ev.Kind, err)
	}
}
