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

package events_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/events"
	"github.com/tjst-t/sakutei/v3/record"
)

type sink struct {
	mu   sync.Mutex
	got  []uint64
	fail error
	hang bool
}

func (s *sink) Deliver(ctx context.Context, batch []core.Event) error {
	s.mu.Lock()
	fail, hang := s.fail, s.hang
	s.mu.Unlock()
	if hang {
		<-ctx.Done()
		return nil
	}
	if fail != nil {
		return fail
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range batch {
		s.got = append(s.got, e.Seq)
	}
	return nil
}

func (s *sink) seqs() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uint64(nil), s.got...)
}

func appendN(t *testing.T, rec *record.Record, from, n uint64) {
	t.Helper()
	var evs []core.Event
	for i := from; i < from+n; i++ {
		evs = append(evs, core.Event{Seq: i, Kind: events.KindCycleDone, Type: core.IncidentEvent})
	}
	if err := rec.AppendOutbox(evs); err != nil {
		t.Fatal(err)
	}
}

// 不変条件 28：送れない sink は位置を保ち、次の周に同じ位置から送り直す。ほかの
// sink は進む。止まった sink・panic する sink でも戻ってくる。
func TestDeliverRetriesFromSamePosition(t *testing.T) {
	rec, err := record.Open(filepath.Join(t.TempDir(), "record"))
	if err != nil {
		t.Fatal(err)
	}
	defer func(d time.Duration) { events.DeliverTimeout = d }(events.DeliverTimeout)
	events.DeliverTimeout = 100 * time.Millisecond

	good, bad, slow := &sink{}, &sink{fail: errors.New("届かない")}, &sink{hang: true}
	sinks := map[string]core.Sink{"good": good, "bad": bad, "slow": slow, "panic": panicSink{}}
	appendN(t, rec, 1, 3)
	start := time.Now()
	ds := events.Deliver(context.Background(), rec, sinks)
	if time.Since(start) > 2*time.Second {
		t.Fatal("止まった sink を待ち続けた")
	}
	byName := map[string]int{}
	for i, d := range ds {
		byName[d.Sink] = i
	}
	if d := ds[byName["good"]]; d.Sent != 3 || d.Cursor != 3 || d.Error != "" {
		t.Fatalf("good: %+v", d)
	}
	for _, n := range []string{"bad", "slow", "panic"} {
		if d := ds[byName[n]]; d.Cursor != 0 || d.Pending != 3 || d.Error == "" {
			t.Fatalf("%s: %+v", n, d)
		}
	}

	appendN(t, rec, 4, 2)
	bad.mu.Lock()
	bad.fail = nil
	bad.mu.Unlock()
	events.Deliver(context.Background(), rec, map[string]core.Sink{"good": good, "bad": bad})
	if got := bad.seqs(); len(got) != 5 || got[0] != 1 || got[4] != 5 {
		t.Fatalf("bad は 1 から送り直す: %v", got)
	}
	if got := good.seqs(); len(got) != 5 || got[3] != 4 {
		t.Fatalf("good は続きから: %v", got)
	}
}

type panicSink struct{}

func (panicSink) Deliver(context.Context, []core.Event) error { panic("壊れた") }

// 送り出しの列は回し、全部の sink が送ったファイルを消す。/events の読み手は
// 消えたファイルを飛ばして続きを読む。
func TestOutboxRotateAndPrune(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "record")
	rec, err := record.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func(n int64) { record.MaxOutboxFile = n }(record.MaxOutboxFile)
	record.MaxOutboxFile = 300
	for i := uint64(1); i <= 10; i++ {
		appendN(t, rec, i, 1)
	}
	files, _ := os.ReadDir(filepath.Join(dir, "events", "outbox"))
	if len(files) < 3 {
		t.Fatalf("回していない: %d ファイル", len(files))
	}
	if last, err := rec.LastOutboxSeq(); err != nil || last != 10 {
		t.Fatalf("last=%d err=%v", last, err)
	}
	evs, err := record.ReadOutbox(dir, 6, 0)
	if err != nil || len(evs) != 4 || evs[0].Seq != 7 {
		t.Fatalf("since 6: %v %v", evs, err)
	}
	a := &sink{}
	events.Deliver(context.Background(), rec, map[string]core.Sink{"a": a})
	after, _ := os.ReadDir(filepath.Join(dir, "events", "outbox"))
	if len(after) != 1 {
		t.Fatalf("送ったファイルを消していない: %d", len(after))
	}
	// 切れた最後の行は読まず、次は新しいファイルに書く。
	last := filepath.Join(dir, "events", "outbox", after[0].Name())
	f, _ := os.OpenFile(last, os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString(`{"seq": 11, "ki`)
	f.Close()
	if n, _ := rec.LastOutboxSeq(); n != 10 {
		t.Fatalf("切れた行を読んだ: %d", n)
	}
	appendN(t, rec, 11, 1)
	evs, err = record.ReadOutbox(dir, 10, 0)
	if err != nil || len(evs) != 1 || evs[0].Seq != 11 {
		t.Fatalf("%v %v", evs, err)
	}
}

func TestStdoutSink(t *testing.T) {
	var b bytes.Buffer
	s, err := events.StdoutModule(&b)(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Deliver(context.Background(), []core.Event{{Seq: 1, Kind: "Drift"}, {Seq: 2, Kind: "CycleDone"}}); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(b.String()), "\n"); len(lines) != 2 || !strings.HasPrefix(lines[0], `{"seq":1,"kind":"Drift"`) {
		t.Fatalf("%q", b.String())
	}
	if _, err := events.StdoutModule(&b)(map[string]any{"x": 1}); err == nil {
		t.Fatal("知らない settings を通した")
	}
}

type auditSink struct {
	mu   sync.Mutex
	got  []uint64
	fail error
}

func (s *auditSink) Deliver(context.Context, []core.Event) error { return nil }

func (s *auditSink) DeliverAudit(_ context.Context, batch []core.AuditLine) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	for _, l := range batch {
		if !bytes.Contains(l.Line, []byte(`"kind":"resource.apply"`)) {
			return errors.New("行がそのままでない: " + string(l.Line))
		}
		s.got = append(s.got, l.Seq)
	}
	return nil
}

// 監査の送り先（streams: [audit]、E-L1・N-L1）：送れたら位置を進め、送れなければ
// 位置を保って次に同じ位置から送り直す。回したファイルをまたいで続きから読む。
func TestDeliverAudit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "record")
	rec, err := record.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func(n int64) { record.MaxAuditFile = n }(record.MaxAuditFile)
	record.MaxAuditFile = 400
	a, err := rec.Audit()
	if err != nil {
		t.Fatal(err)
	}
	add := func(n int) {
		for i := 0; i < n; i++ {
			if _, err := a.Append(record.Entry{Kind: "resource.apply", Resource: "fake/thing/a", Result: "ok"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	add(5)
	good, bad := &auditSink{}, &auditSink{fail: errors.New("届かない")}
	sinks := map[string]core.AuditSink{"good": good, "bad": bad}
	ds := events.DeliverAudit(context.Background(), rec, sinks)
	if len(ds) != 2 || ds[0].Sink != "bad" || ds[0].Pending != 5 || ds[0].Cursor != 0 || ds[0].Stream != "audit" {
		t.Fatalf("bad: %+v", ds)
	}
	if ds[1].Sent != 5 || ds[1].Cursor != 5 {
		t.Fatalf("good: %+v", ds[1])
	}
	add(4)
	files, _ := os.ReadDir(filepath.Join(dir, "audit"))
	if len(files) < 2 {
		t.Fatalf("回していない: %d", len(files))
	}
	bad.mu.Lock()
	bad.fail = nil
	bad.mu.Unlock()
	events.DeliverAudit(context.Background(), rec, sinks)
	if len(good.got) != 9 || good.got[5] != 6 || good.got[8] != 9 {
		t.Fatalf("good は続きから: %v", good.got)
	}
	if len(bad.got) != 9 || bad.got[0] != 1 {
		t.Fatalf("bad は 1 から送り直す: %v", bad.got)
	}
	if n, err := record.Verify(dir); err != nil || n != 9 {
		t.Fatalf("送っても監査は変えない: %d %v", n, err)
	}
}

type stateSink struct {
	sink
	firing [][]string
}

func (s *stateSink) DeliverState(_ context.Context, firing []core.Event) error {
	var keys []string
	for _, e := range firing {
		keys = append(keys, e.Key())
	}
	s.firing = append(s.firing, keys)
	return nil
}

// 続いている状態を送り直す送り先（core.StateSink）は、列が空の周も、列を送れない
// 周も、今の firing の一覧を受け取る（E-H2・E-H3）。
func TestDeliverState(t *testing.T) {
	rec, err := record.Open(filepath.Join(t.TempDir(), "record"))
	if err != nil {
		t.Fatal(err)
	}
	since := time.Now()
	st := record.EventState{Seq: 1, Firing: []core.Event{{Seq: 1, Kind: "Drift", Type: core.StateEvent, State: core.Firing, Resource: "fake/thing/a", Since: &since}}}
	if err := rec.WriteEventState(st); err != nil {
		t.Fatal(err)
	}
	s := &stateSink{}
	events.Deliver(context.Background(), rec, map[string]core.Sink{"am": s})
	s.fail = errors.New("届かない")
	appendN(t, rec, 2, 1)
	ds := events.Deliver(context.Background(), rec, map[string]core.Sink{"am": s})
	if len(s.firing) != 2 || len(s.firing[1]) != 1 || s.firing[1][0] != "Drift fake/thing/a" {
		t.Fatalf("今の一覧: %v", s.firing)
	}
	if ds[0].Pending != 1 {
		t.Fatalf("列の送りの失敗を残す: %+v", ds[0])
	}
}
