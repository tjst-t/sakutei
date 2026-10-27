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

package statusreport

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// #15: デーモン健全性の往復と、レポートへの連続スキップ回数の表示。
func TestDaemonHealth_RoundTripAndReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon-status.json")
	want := DaemonHealth{Cycle: 7, ConsecutiveUnreachableSkips: 4, UnreachableSkipLimit: 3,
		UpdatedAt: time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)}
	if err := WriteDaemonHealth(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDaemonHealth(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip: got %+v want %+v", got, want)
	}

	b := NewBuilder("r1", "r1", nil)
	b.SetDaemonHealth(got)
	out := b.Build().String()
	if !strings.Contains(out, "consecutive_unreachable_skips=4（上限 3）") || !strings.Contains(out, "ExternalInputUnreachableProlonged") {
		t.Errorf("report:\n%s", out)
	}

	got.ConsecutiveUnreachableSkips = 1
	b = NewBuilder("r1", "r1", nil)
	b.SetDaemonHealth(got)
	if out := b.Build().String(); strings.Contains(out, "ExternalInputUnreachableProlonged") {
		t.Errorf("below the limit must not warn:\n%s", out)
	}

	b = NewBuilder("r1", "r1", nil)
	if out := b.Build().String(); strings.Contains(out, "デーモン") {
		t.Errorf("no daemon section without --daemon-status:\n%s", out)
	}
}

// #104: エラーで終わり続ける周と、更新が途絶えたファイルは健全に見せない。
func TestDaemonHealth_ErrorCyclesAndStale(t *testing.T) {
	at := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	h := DaemonHealth{Cycle: 9, UnreachableSkipLimit: 3, ConsecutiveErrorCycles: 4,
		LastError: "load intent: broken", Interval: "1s", UpdatedAt: at}

	b := NewBuilder("r1", "r1", nil)
	b.SetDaemonHealth(h)
	out := b.Build().String()
	if !strings.Contains(out, "consecutive_error_cycles=4") || !strings.Contains(out, "load intent: broken") {
		t.Errorf("error cycles not reported:\n%s", out)
	}
	if strings.Contains(out, "更新されていない") {
		t.Errorf("stale warning without MarkDaemonStale:\n%s", out)
	}

	if h.Stale(at.Add(3 * time.Second)) {
		t.Errorf("3 intervals old must not be stale yet")
	}
	if !h.Stale(at.Add(4 * time.Second)) {
		t.Errorf("4 intervals old must be stale")
	}
	h.Interval = ""
	if h.Stale(at.Add(time.Hour)) {
		t.Errorf("unknown interval must never be stale")
	}

	h.ConsecutiveErrorCycles, h.LastError, h.Interval = 0, "", "1s"
	b = NewBuilder("r1", "r1", nil)
	b.SetDaemonHealth(h)
	b.MarkDaemonStale()
	out = b.Build().String()
	if strings.Contains(out, "consecutive_error_cycles=") {
		t.Errorf("no error-cycle warning at 0:\n%s", out)
	}
	if !strings.Contains(out, "周期（1s）の数倍のあいだ更新されていない") {
		t.Errorf("stale warning missing:\n%s", out)
	}
}

// #136: the claim, the fencing generations and the intent source's fetch.
func TestDaemonHealth_Placement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon-status.json")
	fetched := time.Date(2026, 10, 27, 1, 2, 3, 0, time.UTC)
	want := DaemonHealth{Cycle: 3, UpdatedAt: fetched, Claim: "site=a", Shard: "shard-1",
		Fencing: []FencingGeneration{{Scope: "site=a", Generation: 2, Holder: "shard-1"},
			{Scope: "site=b", Generation: 5, Holder: "shard-2", Fenced: true}},
		Mirror: &MirrorHealth{Remote: "file:///m.git", Revision: "abc", FetchedAt: &fetched, Error: "unreachable"}}
	if err := WriteDaemonHealth(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDaemonHealth(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip: got %+v want %+v", got, want)
	}
	b := NewBuilder("r1", "r1", nil)
	b.SetDaemonHealth(got)
	out := b.Build().String()
	for _, line := range []string{"claim: site=a", "shard: shard-1", "fencing: site=a generation=2 holder=shard-1",
		"fencing: site=b generation=5 holder=shard-2 — fence されている", "mirror: file:///m.git revision=abc 最後に取れた時刻=2026-10-27T01:02:03Z",
		"intent の取得先に届いていない"} {
		if !strings.Contains(out, line) {
			t.Errorf("report lacks %q:\n%s", line, out)
		}
	}
	// Absent fields stay out of the file.
	if err := WriteDaemonHealth(path, DaemonHealth{Cycle: 1}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	for _, k := range []string{"claim", "shard", "fencing", "mirror", "idle"} {
		if strings.Contains(string(raw), `"`+k+`"`) {
			t.Errorf("%q written without a value:\n%s", k, raw)
		}
	}
}
