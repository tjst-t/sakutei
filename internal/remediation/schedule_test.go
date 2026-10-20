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

package remediation

import (
	"testing"
	"time"
)

// 2026-10-24 は土曜日（UTC で評価する）。
func at(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestWindowContains(t *testing.T) {
	cases := []struct {
		spec string
		at   string
		want bool
	}{
		{"*", "2026-10-21 12:00", true},
		{"Sat 02:00-04:00", "2026-10-24 02:00", true},
		{"Sat 02:00-04:00", "2026-10-24 03:59", true},
		{"Sat 02:00-04:00", "2026-10-24 04:00", false},
		{"Sat 02:00-04:00", "2026-10-24 01:59", false},
		{"Sat 02:00-04:00", "2026-10-31 02:30", true},
		{"Sat 02:00-04:00", "2026-10-25 02:30", false},
		{"02:00-04:00", "2026-10-21 03:00", true},
		{"Mon-Fri 09:00-18:00", "2026-10-23 17:59", true},
		{"Mon-Fri 09:00-18:00", "2026-10-24 10:00", false},
		{"Sat,Sun 00:00-24:00", "2026-10-25 23:59", true},
		{"Sat,Sun 00:00-24:00", "2026-10-26 00:00", false},
		{"Fri-Mon 01:00-02:00", "2026-10-25 01:30", true}, // 週をまたぐ範囲
		{"Fri-Mon 01:00-02:00", "2026-10-21 01:30", false},
		// 日をまたぐ窓: 曜日は開く日。
		{"Fri 22:00-02:00", "2026-10-23 23:00", true},
		{"Fri 22:00-02:00", "2026-10-24 01:59", true},
		{"Fri 22:00-02:00", "2026-10-24 02:00", false},
		{"Fri 22:00-02:00", "2026-10-23 01:00", false},
	}
	for _, c := range cases {
		w, err := ParseWindow(c.spec)
		if err != nil {
			t.Fatalf("ParseWindow(%q): %v", c.spec, err)
		}
		if got := w.Contains(at(c.at)); got != c.want {
			t.Errorf("%q contains %s = %v, want %v", c.spec, c.at, got, c.want)
		}
	}
}

func TestParseWindowRejects(t *testing.T) {
	for _, spec := range []string{"", "Sat", "Sab 02:00-04:00", "Sat 2:00-04:00", "Sat 02:00", "Sat 02:00-02:00",
		"Sat 24:00-02:00", "Sat 02:00-24:30", "Sat 02:60-04:00", "Sat Sun 02:00-04:00"} {
		if _, err := ParseWindow(spec); err == nil {
			t.Errorf("ParseWindow(%q) は失敗するはず", spec)
		}
	}
}

func TestScheduleModeAtFirstMatchAndTZ(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	win, _ := ParseWindow("Sat 02:00-04:00")
	all, _ := ParseWindow("*")
	s := &Schedule{Default: ModeAuto, Location: tokyo, Entries: []Entry{{win, ModeAuto}, {all, ModeAlertOnly}}}
	// 土曜 02:30 JST = 金曜 17:30 UTC。
	if m, _ := s.ModeAt(at("2026-10-23 17:30")); m != ModeAuto {
		t.Errorf("土曜 02:30 JST は auto のはず: %v", m)
	}
	// 土曜 02:30 UTC は JST では 11:30 で窓の外。
	if m, why := s.ModeAt(at("2026-10-24 02:30")); m != ModeAlertOnly || why != `schedule window "*"` {
		t.Errorf("土曜 02:30 UTC は alert-only のはず: %v (%s)", m, why)
	}
	// どの窓にも入らなければ default。
	s2 := &Schedule{Default: ModeAlertOnly, Entries: []Entry{{win, ModeAuto}}}
	if m, why := s2.ModeAt(at("2026-10-21 12:00")); m != ModeAlertOnly || why != "schedule default" {
		t.Errorf("窓の外は default のはず: %v (%s)", m, why)
	}
}

func TestDecideMostRestrictiveWins(t *testing.T) {
	all, _ := ParseWindow("*")
	open := &Schedule{Entries: []Entry{{all, ModeAuto}}}
	closed := &Schedule{Entries: []Entry{{all, ModeAlertOnly}}}
	cases := []struct {
		name string
		cfg  Config
		s    *Schedule
		want Mode
	}{
		{"no sources restrict", Config{}, nil, ModeAuto},
		{"schedule auto", Config{}, open, ModeAuto},
		{"schedule alert-only beats --mode auto", Config{Mode: ModeAuto}, closed, ModeAlertOnly},
		{"--mode alert-only beats schedule auto", Config{Mode: ModeAlertOnly}, open, ModeAlertOnly},
		{"freeze beats schedule auto", Config{Freeze: true}, open, ModeAlertOnly},
		{"freeze beats everything auto", Config{Mode: ModeAuto, Freeze: true}, nil, ModeAlertOnly},
	}
	for _, c := range cases {
		if got := c.cfg.Decide(c.s); got.Mode != c.want {
			t.Errorf("%s: got %v (%s), want %v", c.name, got.Mode, got.Reason, c.want)
		}
	}
	d := Config{Mode: ModeAlertOnly, Freeze: true}.Decide(closed)
	if d.Reason != `freeze, --mode alert-only, schedule window "*" (alert-only)` {
		t.Errorf("alert-only を言った決定源がすべて理由に並ぶはず: %q", d.Reason)
	}
	// Now の注入。
	sat, _ := ParseWindow("Sat 02:00-04:00")
	s := &Schedule{Default: ModeAlertOnly, Entries: []Entry{{sat, ModeAuto}}}
	cfg := Config{Now: func() time.Time { return at("2026-10-24 03:00") }}
	if got := cfg.Decide(s); got.Mode != ModeAuto {
		t.Errorf("注入した時刻（窓の中）で auto のはず: %+v", got)
	}
}
