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
	"fmt"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // tz: の解決をホストの zoneinfo に依存させない
)

// Schedule is the intent's `remediation:` declaration (§14.2): a mode per
// time window.
//
//	remediation:
//	  default: alert-only
//	  schedule:
//	    - window: "Sat 02:00-04:00"
//	      mode: auto
//	    - window: "*"
//	      mode: alert-only
//	  tz: Asia/Tokyo
//
// The first window that contains the current time decides; when none does,
// Default applies.  A schedule is one decision source among several — it can
// only open auto as far as every other source allows (most-restrictive-wins,
// §14.3).
type Schedule struct {
	// Default is the mode outside every window.  An omitted default is
	// alert-only: the most conservative side (§14.2).
	Default Mode
	// Entries are the windows in declaration order (first match wins).
	Entries []Entry
	// Location is the time zone the windows are written in (tz:).  An omitted
	// tz is UTC, so the schedule does not shift with the host's zone.
	Location *time.Location
}

// Entry is one `schedule:` item: a window and the mode inside it.
type Entry struct {
	Window Window
	Mode   Mode
}

// ModeAt returns the mode the schedule puts at t, with a human-readable
// reason naming the window (or the default) that decided it.
func (s *Schedule) ModeAt(t time.Time) (Mode, string) {
	loc := s.Location
	if loc == nil {
		loc = time.UTC
	}
	lt := t.In(loc)
	for _, e := range s.Entries {
		if e.Window.Contains(lt) {
			return e.Mode, fmt.Sprintf("schedule window %q", e.Window.Spec)
		}
	}
	return s.Default, "schedule default"
}

// Window is a weekly time window: "*" (always), "HH:MM-HH:MM" (every day) or
// "<days> HH:MM-HH:MM" where days is a comma-separated list of day names or
// day ranges ("Sat", "Mon-Fri", "Sat,Sun").  An end at or before the start
// crosses midnight: "Fri 22:00-02:00" runs from Friday 22:00 to Saturday
// 02:00 (the days name the day the window opens).  "24:00" is allowed as an
// end.
type Window struct {
	// Spec is the window as written, for logs.
	Spec string
	// always is "*".
	always bool
	// days[w] is true when the window opens on weekday w.
	days [7]bool
	// start/end are minutes since midnight; end may be 1440 ("24:00").
	start, end int
}

var dayNames = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// ParseWindow parses a window spec (see Window).
func ParseWindow(spec string) (Window, error) {
	w := Window{Spec: spec}
	s := strings.TrimSpace(spec)
	if s == "*" {
		w.always = true
		return w, nil
	}
	fields := strings.Fields(s)
	var dayPart, timePart string
	switch len(fields) {
	case 1:
		timePart = fields[0]
		for i := range w.days {
			w.days[i] = true
		}
	case 2:
		dayPart, timePart = fields[0], fields[1]
		if err := parseDays(dayPart, &w.days); err != nil {
			return Window{}, fmt.Errorf("window %q: %w", spec, err)
		}
	default:
		return Window{}, fmt.Errorf("window %q: want \"*\", \"HH:MM-HH:MM\" or \"<days> HH:MM-HH:MM\"", spec)
	}
	from, to, ok := strings.Cut(timePart, "-")
	if !ok {
		return Window{}, fmt.Errorf("window %q: time range must be HH:MM-HH:MM", spec)
	}
	var err error
	if w.start, err = parseClock(from, false); err != nil {
		return Window{}, fmt.Errorf("window %q: %w", spec, err)
	}
	if w.end, err = parseClock(to, true); err != nil {
		return Window{}, fmt.Errorf("window %q: %w", spec, err)
	}
	if w.start == w.end {
		return Window{}, fmt.Errorf("window %q: empty range (start equals end); use \"*\" for always", spec)
	}
	return w, nil
}

func parseDays(s string, days *[7]bool) error {
	for _, part := range strings.Split(s, ",") {
		from, to, isRange := strings.Cut(part, "-")
		a, ok := dayNames[strings.ToLower(from)]
		if !ok {
			return fmt.Errorf("unknown day %q (use Sun Mon Tue Wed Thu Fri Sat)", from)
		}
		if !isRange {
			days[a] = true
			continue
		}
		b, ok := dayNames[strings.ToLower(to)]
		if !ok {
			return fmt.Errorf("unknown day %q (use Sun Mon Tue Wed Thu Fri Sat)", to)
		}
		// A range may wrap the week ("Fri-Mon").
		for d := a; ; d = (d + 1) % 7 {
			days[d] = true
			if d == b {
				break
			}
		}
	}
	return nil
}

func parseClock(s string, allow24 bool) (int, error) {
	hh, mm, ok := strings.Cut(s, ":")
	if !ok || len(hh) != 2 || len(mm) != 2 {
		return 0, fmt.Errorf("time %q must be HH:MM", s)
	}
	h, err1 := strconv.Atoi(hh)
	m, err2 := strconv.Atoi(mm)
	if err1 != nil || err2 != nil || m < 0 || m > 59 || h < 0 || h > 24 || (h == 24 && (m != 0 || !allow24)) {
		return 0, fmt.Errorf("time %q is out of range", s)
	}
	return h*60 + m, nil
}

// Contains reports whether t (already in the schedule's time zone) falls in
// the window.  The start is inclusive, the end exclusive.
func (w Window) Contains(t time.Time) bool {
	if w.always {
		return true
	}
	min := t.Hour()*60 + t.Minute()
	wd := t.Weekday()
	if w.start < w.end {
		return w.days[wd] && min >= w.start && min < w.end
	}
	// Crosses midnight: the opening day's tail, or the next day's head.
	prev := (wd + 6) % 7
	return (w.days[wd] && min >= w.start) || (w.days[prev] && min < w.end)
}
