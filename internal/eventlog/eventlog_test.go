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
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

func TestTextIsTheLegacyLine(t *testing.T) {
	var buf bytes.Buffer
	w := New(&buf, FormatText, core.SeverityWarning)
	fmt.Fprintf(w, "sakutei: plain %d\n", 1)
	Emit(w, core.Event{Kind: core.EventBudgetHalted, Message: "r", Text: "sakutei: cycle 1: BUDGET HALTED: r"})
	Emit(w, core.Event{Kind: core.EventPruned, Message: "m"})
	want := "sakutei: plain 1\nsakutei: cycle 1: BUDGET HALTED: r\nPruned: m\n"
	if buf.String() != want {
		t.Fatalf("text = %q, want %q", buf.String(), want)
	}
	// A plain io.Writer gets the text form too.
	var plain bytes.Buffer
	Emit(&plain, core.Event{Kind: core.EventPruned, Text: "x"})
	if plain.String() != "x\n" {
		t.Fatalf("plain = %q", plain.String())
	}
}

func TestJSONLines(t *testing.T) {
	var buf bytes.Buffer
	w := New(&buf, FormatJSON, core.SeverityWarning)
	id := core.NewID("fake", core.NewScope(map[string]string{"site": "tokyo"}), "vm-1")
	s := Stamp(w, 7, core.RevisionInfo{IntentRevision: "i1", RenderedRevision: "r1"})
	fmt.Fprint(s, "partial ")
	fmt.Fprint(s, "line\n\n")
	Emit(s, core.Event{Kind: core.EventDrift, Severity: core.SeverityWarning, Resource: id,
		Cause: core.CauseTagB, Message: "m", Attrs: map[string]string{"op": "UPDATE"}, Text: "not in json"})
	fmt.Fprint(s, "tail")
	w.Flush()

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 JSON lines (blank line dropped), got %d: %q", len(lines), buf.String())
	}
	var log map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &log); err != nil {
		t.Fatal(err)
	}
	if log["kind"] != "Log" || log["message"] != "partial line" || log["severity"] != "warning" {
		t.Errorf("log line = %v", log)
	}
	var ev map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &ev); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{
		"kind": "Drift", "cycle": float64(7), "resource": "fake/site=tokyo/vm-1", "scope": "site=tokyo",
		"intent_revision": "i1", "rendered_revision": "r1", "cause": "B", "message": "m",
	} {
		if ev[k] != want {
			t.Errorf("%s = %v, want %v", k, ev[k], want)
		}
	}
	if strings.Contains(lines[1], "not in json") {
		t.Errorf("Text must not be in the JSON line: %s", lines[1])
	}
	if !strings.Contains(lines[2], `"message":"tail"`) {
		t.Errorf("Flush did not write the partial line: %s", lines[2])
	}
}

func TestParseFormat(t *testing.T) {
	for _, s := range []string{"text", "json"} {
		if f, err := ParseFormat(s); err != nil || string(f) != s {
			t.Errorf("ParseFormat(%q) = %v, %v", s, f, err)
		}
	}
	if _, err := ParseFormat("yaml"); err == nil {
		t.Error("ParseFormat(yaml) must fail")
	}
}
