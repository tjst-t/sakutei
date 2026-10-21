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

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/pkg/core"
)

type recordSink struct{ got []core.Event }

func (r *recordSink) Emit(_ context.Context, ev core.Event) error {
	r.got = append(r.got, ev)
	return nil
}

func TestFanOut_DeliversToAllDespiteFailure(t *testing.T) {
	a, b := &recordSink{}, &recordSink{}
	boom := errors.New("boom")
	f := FanOut{a, SinkFunc(func(context.Context, core.Event) error { return boom }), b}
	err := f.Emit(context.Background(), core.Event{Kind: core.EventDrift})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if len(a.got) != 1 || len(b.got) != 1 {
		t.Fatalf("delivered a=%d b=%d, want 1 and 1", len(a.got), len(b.got))
	}
}

func TestFileSink_AppendsJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte(`{"kind":"Earlier"}`+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	s, err := OpenFileSink(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []core.EventKind{core.EventDrift, core.EventPruned} {
		if err := s.Emit(context.Background(), core.Event{Kind: k, Severity: core.SeverityInfo, Message: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	// A closed file is reopened on the next Emit.
	_ = s.Close()
	if err := s.Emit(context.Background(), core.Event{Kind: core.EventBudgetHalted, Message: "m"}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	want := []string{"Earlier", "Drift", "Pruned", "BudgetHalted"}
	if len(lines) != len(want) {
		t.Fatalf("lines = %q, want %d", lines, len(want))
	}
	for i, line := range lines {
		var ev struct{ Kind, Time string }
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line %d not JSON: %q", i, line)
		}
		if ev.Kind != want[i] || (i > 0 && ev.Time == "") {
			t.Errorf("line %d = %q, want kind %s with a time", i, line, want[i])
		}
	}
}

func TestOpenFileSink_Unopenable(t *testing.T) {
	if _, err := OpenFileSink(filepath.Join(t.TempDir(), "no", "such", "dir", "e.jsonl")); err == nil {
		t.Fatal("want an error")
	}
}

func TestRoute(t *testing.T) {
	var stream, errOut bytes.Buffer
	rec := &recordSink{}
	fail := SinkFunc(func(context.Context, core.Event) error { return errors.New("disk full") })
	w := Route(context.Background(), &stream, []Sink{rec, fail}, &errOut)

	Emit(w, core.Event{Kind: core.EventDrift, Message: "x", Text: "sakutei: DriftEvent: x"})
	Emit(w, core.Event{Kind: core.EventCycleSummary, Message: "s", Text: "sakutei: cycle 1: s"})
	_, _ = w.Write([]byte("plain line\n"))

	if got := stream.String(); got != "sakutei: DriftEvent: x\nsakutei: cycle 1: s\nplain line\n" {
		t.Errorf("stream = %q", got)
	}
	if len(rec.got) != 1 || rec.got[0].Kind != core.EventDrift || rec.got[0].Time.IsZero() {
		t.Errorf("sink got %+v, want the one Drift with a time (no CycleSummary, no plain line)", rec.got)
	}
	if got := errOut.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "event sink: Drift not delivered: disk full") {
		t.Errorf("errOut = %q, want one warn line", got)
	}
	if Route(context.Background(), &stream, nil, &errOut) != &stream {
		t.Error("Route without sinks should return w")
	}
}
