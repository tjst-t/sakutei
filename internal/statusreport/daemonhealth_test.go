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
	"path/filepath"
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
	if got != want {
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
