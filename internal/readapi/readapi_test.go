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

package readapi

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/eventlog"
	"github.com/tjst-t/sakutei/v2/internal/fakeprovider"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

func TestNotYetObserved(t *testing.T) {
	s := New()
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	s.RecordCycle(reconcile.CycleView{Cycle: 1}) // a cycle that did not observe
	for _, p := range []string{"/status", "/plan", "/graph"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("GET %s before any observing cycle: %d, want 503", p, resp.StatusCode)
		}
	}
}

// A reader that does not read never blocks Emit: what does not fit is dropped
// and counted.
func TestSlowReaderDoesNotBlock(t *testing.T) {
	s := New()
	sub := &subscriber{ch: make(chan []byte, SubscriberBuffer)}
	s.subs[sub] = true
	done := make(chan struct{})
	go func() {
		for i := 0; i < SubscriberBuffer+10; i++ {
			_ = s.Emit(context.Background(), core.Event{Kind: core.EventPruned, Message: "x"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Emit blocked on a slow reader")
	}
	if got := s.Dropped(); got != 10 {
		t.Errorf("Dropped = %d, want 10", got)
	}
	if got := sub.dropped.Load(); got != 10 {
		t.Errorf("subscriber dropped = %d, want 10", got)
	}
}

// A reader gets what is firing first; a resolved Event takes it out.
func TestEventsReplayFiring(t *testing.T) {
	s := New()
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	res := core.ID{Provider: "fake", Local: "web"}
	firing := core.Event{Kind: core.EventDrift, Resource: res, State: core.EventFiring, Message: "drift"}
	s.RecordCycle(reconcile.CycleView{Cycle: 1, Firing: []core.Event{firing}})

	read := func() (*bufio.Scanner, func()) {
		resp, err := http.Get(srv.URL + "/events")
		if err != nil {
			t.Fatal(err)
		}
		return bufio.NewScanner(resp.Body), func() { resp.Body.Close() }
	}
	nextData := func(sc *bufio.Scanner) string {
		for sc.Scan() {
			if l := sc.Text(); strings.HasPrefix(l, "data: ") {
				return l
			}
		}
		return ""
	}
	sc, closeA := read()
	if l := nextData(sc); !strings.Contains(l, `"kind":"Drift"`) || !strings.Contains(l, `"state":"firing"`) {
		t.Fatalf("first event %q, want the firing Drift", l)
	}
	closeA()

	_ = s.Emit(context.Background(), core.Event{Kind: core.EventDrift, Resource: res, State: core.EventResolved, Message: "resolved"})
	_ = s.Emit(context.Background(), core.Event{Kind: core.EventPruned, Message: "after"})
	sc, closeB := read()
	defer closeB()
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = s.Emit(context.Background(), core.Event{Kind: core.EventPruned, Message: "live"})
	}()
	if l := nextData(sc); !strings.Contains(l, `"message":"live"`) {
		t.Fatalf("first event after the resolve %q, want the live one (nothing firing)", l)
	}
}

// The loop records cycles and emits Events while readers read: run under
// -race (make test-race).
func TestLoopWithConcurrentReaders(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	intent := `{"resources":[{"provider":"fake","id":"a","fields":{"name":"a","size":"1"}},` +
		`{"provider":"fake","id":"b","depends_on":[{"id":"a"}],"fields":{"name":"b","size":"1"}}]}`
	if err := os.WriteFile(filepath.Join(intentDir, "r.json"), []byte(intent), 0o644); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	if err := os.WriteFile(statePath, []byte(`{"resources":{},"provider_version":"v1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New()
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- reconcile.Run(ctx, reconcile.Config{
			IntentDir: intentDir, GeneratedDir: filepath.Join(tmp, "generated"),
			Provider: fakeprovider.New(statePath), Interval: 10 * time.Millisecond,
			Out: io.Discard, ErrOut: io.Discard, EventSinks: []eventlog.Sink{s}, View: s,
		})
	}()
	defer func() { cancel(); <-done }()

	evResp, err := http.Get(srv.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer evResp.Body.Close()
	go func() { _, _ = io.Copy(io.Discard, evResp.Body) }()

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				for _, p := range []string{"/status", "/plan", "/graph"} {
					if resp, err := http.Get(srv.URL + p); err == nil {
						_, _ = io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
					}
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
	}
	wg.Wait()

	resp, err := http.Get(srv.URL + "/graph")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var g struct {
		Waves [][]string `json:"waves"`
		Cycle int        `json:"cycle"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&g); err != nil || g.Cycle < 1 {
		t.Fatalf("/graph: %v %+v", err, g)
	}
	if b, _ := json.Marshal(g.Waves); string(b) != `[["a"],["b"]]` {
		t.Errorf("waves %s", b)
	}
}
