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

package task

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Window はメンテ窓（update_policy.window、§6.2）。窓の外では Task を起動しない。
//
// 書式: "[<曜日>] HH:MM-HH:MM [<タイムゾーン>]"
//
//	"02:00-04:00"                 毎日
//	"Sat 02:00-04:00"             土曜
//	"Mon-Fri 22:00-06:00"         平日の夜（終わりが始まり以前なら翌日に跨ぐ。曜日は始まりの日で判定）
//	"Sat,Sun 02:00-04:00 UTC"     タイムゾーン名（time.LoadLocation）。省略時はデーモンのローカル時刻
//
// 始まりは含み、終わりは含まない。
type Window struct {
	spec  string
	days  [7]bool // time.Weekday で引く
	start int     // 0:00 からの分
	end   int
	loc   *time.Location
}

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// ParseWindow は窓の文字列を読む。空文字は nil（常時）を返す。
func ParseWindow(spec string) (*Window, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	w := &Window{spec: spec, loc: time.Local}
	parts := strings.Fields(spec)
	i := 0
	if i < len(parts) && !strings.Contains(parts[i], ":") {
		if err := w.parseDays(parts[i]); err != nil {
			return nil, fmt.Errorf("window %q: %w", spec, err)
		}
		i++
	} else {
		for d := range w.days {
			w.days[d] = true
		}
	}
	if i >= len(parts) {
		return nil, fmt.Errorf("window %q: missing HH:MM-HH:MM", spec)
	}
	from, to, ok := strings.Cut(parts[i], "-")
	if !ok {
		return nil, fmt.Errorf("window %q: time range must be HH:MM-HH:MM", spec)
	}
	var err error
	if w.start, err = parseClock(from); err != nil {
		return nil, fmt.Errorf("window %q: %w", spec, err)
	}
	if w.end, err = parseClock(to); err != nil {
		return nil, fmt.Errorf("window %q: %w", spec, err)
	}
	i++
	if i < len(parts) {
		if w.loc, err = time.LoadLocation(parts[i]); err != nil {
			return nil, fmt.Errorf("window %q: time zone: %w", spec, err)
		}
		i++
	}
	if i < len(parts) {
		return nil, fmt.Errorf("window %q: unexpected %q", spec, parts[i])
	}
	return w, nil
}

func (w *Window) parseDays(s string) error {
	for _, item := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(item, "-")
		a, ok := weekdays[strings.ToLower(lo)]
		if !ok {
			return fmt.Errorf("unknown weekday %q (use Sun..Sat)", lo)
		}
		b := a
		if isRange {
			if b, ok = weekdays[strings.ToLower(hi)]; !ok {
				return fmt.Errorf("unknown weekday %q (use Sun..Sat)", hi)
			}
		}
		for d := a; ; d = (d + 1) % 7 {
			w.days[d] = true
			if d == b {
				break
			}
		}
	}
	return nil
}

func parseClock(s string) (int, error) {
	hh, mm, ok := strings.Cut(s, ":")
	h, errH := strconv.Atoi(hh)
	m, errM := strconv.Atoi(mm)
	if !ok || errH != nil || errM != nil || h < 0 || h > 24 || m < 0 || m > 59 || (h == 24 && m != 0) {
		return 0, fmt.Errorf("invalid time %q (want HH:MM)", s)
	}
	return h*60 + m, nil
}

// Contains は t が窓の中か。nil の窓は常に中。
func (w *Window) Contains(t time.Time) bool {
	if w == nil {
		return true
	}
	t = t.In(w.loc)
	min := t.Hour()*60 + t.Minute()
	today := t.Weekday()
	if w.end > w.start {
		return w.days[today] && min >= w.start && min < w.end
	}
	// 翌日に跨ぐ窓（end <= start）: 今日始まった分か、昨日始まって今日に残る分。
	yesterday := (today + 6) % 7
	return (w.days[today] && min >= w.start) || (w.days[yesterday] && min < w.end)
}

// String は宣言どおりの窓の文字列。
func (w *Window) String() string {
	if w == nil {
		return "(always)"
	}
	return w.spec
}
