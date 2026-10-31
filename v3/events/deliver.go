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

// DeliverTimeout は sink 1 つに 1 回の送り出しで待つ長さ。止まった送り先で周を
// 止めない（不変条件 28、E-H2）。
var DeliverTimeout = 10 * time.Second

// BatchSize は 1 回の送り出しで送る Event の数の上限。残りは次の周に送る。
var BatchSize = 1000

// Deliver は sink ごとに、送り終えた位置の次から送り出しの列を送り、送れたら
// 位置を進める（E-H2・N-H2）。送れなければ位置を保ち、次の周に同じ位置から
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
	if len(batch) == 0 {
		return d
	}
	if err := call(ctx, s, batch); err != nil {
		d.Error, d.Pending = err.Error(), len(batch)
		return d
	}
	last := batch[len(batch)-1].Seq
	if err := rec.WriteCursor(name, last); err != nil {
		// 位置を書けなければ次の周に同じものを送る（落とすより重ねる）。
		d.Error, d.Pending = "位置を書けない: "+err.Error(), len(batch)
		return d
	}
	d.Sent, d.Cursor = len(batch), last
	return d
}

// call は時間を区切って送る。送り先が戻らなくても待たない。
func call(ctx context.Context, s core.Sink, batch []core.Event) (err error) {
	ctx, cancel := context.WithTimeout(ctx, DeliverTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- fmt.Errorf("送り先が panic した: %v", p)
			}
		}()
		done <- s.Deliver(ctx, batch)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("送り先が %s のうちに戻らない", DeliverTimeout)
	}
}

// Stdout は Event を 1 行 1 件の JSON で書く送り先（§8 の段 3）。
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

// StdoutModule はつなぎ方の設定の module: stdout。settings は持たない。
func StdoutModule(w io.Writer) func(settings map[string]any) (core.Sink, error) {
	return func(settings map[string]any) (core.Sink, error) {
		for k := range settings {
			return nil, fmt.Errorf("stdout の送り先の settings に知らない項目 %q（書ける項目は無い）", k)
		}
		return &Stdout{W: w}, nil
	}
}
