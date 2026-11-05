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

package events

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/record"
)

// DeliverTimeout は 1 回の送信で sink 1 つを待つ長さ。止まった sink で cycle を
// 止めない（不変条件 28、E-H2）。
var DeliverTimeout = 10 * time.Second

// BatchSize は 1 回の送信で送る Event の数の上限。残りは次の cycle に送る。
var BatchSize = 1000

// Deliver は sink ごとに、送り終えた位置の次から outbox を送り、送れたら
// 位置を進める（E-H2・N-H2）。送れなければ位置を保ち、次の cycle に同じ位置から
// 送り直す。全部の sink が送ったファイルは消す。ロックを持つ処理だけが呼ぶ。
func Deliver(ctx context.Context, rec *record.Record, sinks map[string]core.Sink) []plan.Delivery {
	names := make([]string, 0, len(sinks))
	for n := range sinks {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]plan.Delivery, 0, len(names))
	var low uint64
	lowSet := false
	for _, n := range names {
		d := deliverOne(ctx, rec, n, sinks[n])
		out = append(out, d)
		if !lowSet || d.Cursor < low {
			low, lowSet = d.Cursor, true
		}
	}
	if !lowSet {
		// sink が無ければ /events の読み手のために最後のファイルだけ残す。
		low = ^uint64(0)
	}
	_ = rec.PruneOutbox(low)
	return out
}

func deliverOne(ctx context.Context, rec *record.Record, name string, s core.Sink) plan.Delivery {
	d := plan.Delivery{Sink: name}
	cur, err := rec.Cursor(name)
	if err != nil {
		d.Error = err.Error()
		return d
	}
	d.Cursor = cur
	batch, err := record.ReadOutbox(rec.Dir(), cur, BatchSize)
	if err != nil {
		d.Error = err.Error()
		return d
	}
	if len(batch) > 0 {
		d = sent(d, len(batch), call(ctx, func(ctx context.Context) error { return s.Deliver(ctx, batch) }),
			batch[len(batch)-1].Seq, func(seq uint64) error { return rec.WriteCursor(name, seq) })
	}
	// 続いている状態を送り直す sink には、cycle ごとに今の firing の一覧も渡す
	// （E-H2・E-H3）。列の送りが失敗していても渡す（今の状態が正）。
	if ss, ok := s.(core.StateSink); ok {
		st, err := rec.EventState()
		if err == nil {
			err = call(ctx, func(ctx context.Context) error { return ss.DeliverState(ctx, st.Firing) })
		}
		if err != nil && d.Error == "" {
			d.Error = "今の状態を送れない: " + err.Error()
		}
	}
	return d
}

// AuditBatchSize は 1 回の送信で送る audit の行の数の上限。残りは次の cycle に送る。
var AuditBatchSize = 1000

// DeliverAudit は audit を送る sink ごとに、送り終えた位置の次から audit の
// ハッシュチェーンを送り、送れたら位置を進める（E-L1・N-L1）。送れなければ位置を
// 保ち、次の cycle に同じ位置から送り直す（届かない間は Record に貯まっている）。
// audit は消さない。
// ロックを持つ処理だけが呼ぶ。
func DeliverAudit(ctx context.Context, rec *record.Record, sinks map[string]core.AuditSink) []plan.Delivery {
	names := make([]string, 0, len(sinks))
	for n := range sinks {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]plan.Delivery, 0, len(names))
	for _, n := range names {
		d := plan.Delivery{Sink: n, Stream: "audit"}
		cur, err := rec.AuditCursor(n)
		if err != nil {
			d.Error = err.Error()
			out = append(out, d)
			continue
		}
		d.Cursor = cur
		batch, err := record.ReadAudit(rec.Dir(), cur, AuditBatchSize)
		switch {
		case err != nil:
			d.Error = err.Error()
		case len(batch) == 0:
		default:
			s := sinks[n]
			d = sent(d, len(batch), call(ctx, func(ctx context.Context) error { return s.DeliverAudit(ctx, batch) }),
				batch[len(batch)-1].Seq, func(seq uint64) error { return rec.WriteAuditCursor(n, seq) })
		}
		out = append(out, d)
	}
	return out
}

// sent は送った結果を d に書き、送れたら位置を last に進める。位置を書けなければ
// 次の cycle に同じものを送る（落とすより重ねる）。
func sent(d plan.Delivery, n int, err error, last uint64, write func(uint64) error) plan.Delivery {
	if err != nil {
		d.Error, d.Pending = err.Error(), n
		return d
	}
	if err := write(last); err != nil {
		d.Error, d.Pending = "位置を書けない: "+err.Error(), n
		return d
	}
	d.Sent, d.Cursor = n, last
	return d
}

// call は時間を区切って送る。sink が戻らなくても待たない。
func call(ctx context.Context, send func(ctx context.Context) error) (err error) {
	ctx, cancel := context.WithTimeout(ctx, DeliverTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- fmt.Errorf("sink が panic した: %v", p)
			}
		}()
		done <- send(ctx)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("sink が %s 以内に戻らない", DeliverTimeout)
	}
}

// Stdout は Event を 1 行 1 件の JSON で書く sink（§8 の Stage 3）。
type Stdout struct {
	mu sync.Mutex
	W  io.Writer
}

// Deliver は batch を 1 行ずつ書く。
func (s *Stdout) Deliver(_ context.Context, batch []core.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	enc := json.NewEncoder(s.W)
	for _, e := range batch {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return nil
}

// StdoutModule は接続設定（config ファイル）の module: stdout。settings は持たない。
func StdoutModule(w io.Writer) func(settings map[string]any) (core.Sink, error) {
	return func(settings map[string]any) (core.Sink, error) {
		for k := range settings {
			return nil, fmt.Errorf("stdout の sink の settings に知らない field %q（書ける field は無い）", k)
		}
		return &Stdout{W: w}, nil
	}
}
