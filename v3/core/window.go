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

package core

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // 窓の tz（Asia/Tokyo・Etc/GMT-3）をホストの zoneinfo に頼らず読む
)

// Window は Policy の窓 1 つ（§3.6、E-J3・E-R1・N-J1・N-R3）。2 つの形を書ける。
//
//	"2026-11-05 22:00 .. 2026-11-06 06:00 Asia/Tokyo"   # 決まった期間（tz を省けば UTC。RFC 3339 も書ける）
//	"Sat 02:00-04:00 Asia/Tokyo"                        # 毎週（曜日は Mon-Fri・Sat,Sun。省けば毎日）
//	"22:00-02:00"                                       # 終わりが始めより前なら日をまたぐ（曜日は開く日）
//	"*"                                                 # いつでも
type Window struct {
	Text string
	// 決まった期間 [Start, End)。
	Start, End time.Time
	// 毎週の窓。Days[time.Weekday] が開く日、From・To は 0 時からの分。
	Weekly   bool
	Days     [7]bool
	From, To int
	Loc      *time.Location
	Always   bool
}

// String は書いたままの窓。
func (w Window) String() string { return w.Text }

// Contains は t が窓の中か。
func (w Window) Contains(t time.Time) bool {
	switch {
	case w.Always:
		return true
	case !w.Weekly:
		return !t.Before(w.Start) && t.Before(w.End)
	}
	lt := t.In(w.Loc)
	min := lt.Hour()*60 + lt.Minute()
	day := lt.Weekday()
	if w.From < w.To {
		return w.Days[day] && min >= w.From && min < w.To
	}
	// 日をまたぐ窓：開く日の From 以降か、開いた日の翌日の To より前。
	prev := (day + 6) % 7
	return (w.Days[day] && min >= w.From) || (w.Days[prev] && min < w.To)
}

// Ended は窓が二度と開かないか（決まった期間が過ぎた）。毎週の窓は終わらない。
func (w Window) Ended(t time.Time) bool { return !w.Weekly && !w.Always && !t.Before(w.End) }

// InWindows は ws のどれかの中か。窓を書かなければいつでも中（E-J3）。
func InWindows(ws []Window, t time.Time) bool {
	if len(ws) == 0 {
		return true
	}
	for _, w := range ws {
		if w.Contains(t) {
			return true
		}
	}
	return false
}

var weekdays = map[string]time.Weekday{"Sun": time.Sunday, "Mon": time.Monday, "Tue": time.Tuesday, "Wed": time.Wednesday,
	"Thu": time.Thursday, "Fri": time.Friday, "Sat": time.Saturday}

var windowLayouts = []string{time.RFC3339, "2006-01-02T15:04Z07:00", "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04:05", "2006-01-02T15:04"}

// ParseWindow は窓の文字列を読む。
func ParseWindow(s string) (Window, error) {
	w := Window{Text: s, Loc: time.UTC}
	s = strings.TrimSpace(s)
	if s == "*" {
		w.Always = true
		return w, nil
	}
	if a, b, ok := strings.Cut(s, ".."); ok {
		// 決まった期間。最後の語が tz なら両端に当てる。
		b = strings.TrimSpace(b)
		if i := strings.LastIndex(b, " "); i > 0 && (strings.Contains(b[i+1:], "/") || b[i+1:] == "UTC") {
			loc, err := time.LoadLocation(b[i+1:])
			if err != nil {
				return w, fmt.Errorf("窓 %q の tz %q が分からない", w.Text, b[i+1:])
			}
			w.Loc, b = loc, strings.TrimSpace(b[:i])
		}
		start, err := parseWindowTime(a, w.Loc)
		if err != nil {
			return w, fmt.Errorf("窓 %q の始め: %v", w.Text, err)
		}
		end, err := parseWindowTime(b, w.Loc)
		if err != nil {
			return w, fmt.Errorf("窓 %q の終わり: %v", w.Text, err)
		}
		if !end.After(start) {
			return w, fmt.Errorf("窓 %q の終わりが始めより後でない", w.Text)
		}
		w.Start, w.End = start, end
		return w, nil
	}
	w.Weekly = true
	fields := strings.Fields(s)
	if len(fields) == 0 || len(fields) > 3 {
		return w, fmt.Errorf("窓 %q は \"[曜日] HH:MM-HH:MM [tz]\" か \"<始め> .. <終わり> [tz]\"", w.Text)
	}
	days := ""
	if len(fields) > 1 && !strings.Contains(fields[0], ":") {
		days, fields = fields[0], fields[1:]
	}
	if len(fields) == 2 {
		loc, err := time.LoadLocation(fields[1])
		if err != nil {
			return w, fmt.Errorf("窓 %q の tz %q が分からない", w.Text, fields[1])
		}
		w.Loc = loc
	} else if len(fields) != 1 {
		return w, fmt.Errorf("窓 %q は \"[曜日] HH:MM-HH:MM [tz]\"", w.Text)
	}
	if days == "" {
		for i := range w.Days {
			w.Days[i] = true
		}
	} else {
		for _, part := range strings.Split(days, ",") {
			a, b, isRange := strings.Cut(part, "-")
			da, ok := weekdays[a]
			if !ok {
				return w, fmt.Errorf("窓 %q の曜日 %q（Mon・Tue・Wed・Thu・Fri・Sat・Sun）", w.Text, a)
			}
			db := da
			if isRange {
				if db, ok = weekdays[b]; !ok {
					return w, fmt.Errorf("窓 %q の曜日 %q（Mon・Tue・Wed・Thu・Fri・Sat・Sun）", w.Text, b)
				}
			}
			for d := da; ; d = (d + 1) % 7 {
				w.Days[d] = true
				if d == db {
					break
				}
			}
		}
	}
	from, to, ok := strings.Cut(fields[0], "-")
	if !ok {
		return w, fmt.Errorf("窓 %q の時刻は HH:MM-HH:MM", w.Text)
	}
	var err error
	if w.From, err = clock(from); err != nil {
		return w, fmt.Errorf("窓 %q: %v", w.Text, err)
	}
	if w.To, err = clock(to); err != nil {
		return w, fmt.Errorf("窓 %q: %v", w.Text, err)
	}
	if w.From == w.To {
		return w, fmt.Errorf("窓 %q が空（始めと終わりが同じ）", w.Text)
	}
	return w, nil
}

func parseWindowTime(s string, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range windowLayouts {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q は時刻でない（RFC 3339 か \"YYYY-MM-DD HH:MM\"）", s)
}

func clock(s string) (int, error) {
	h, m, ok := strings.Cut(s, ":")
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if !ok || err1 != nil || err2 != nil || len(m) != 2 || hh < 0 || hh > 24 || mm < 0 || mm > 59 || (hh == 24 && mm != 0) {
		return 0, fmt.Errorf("時刻 %q は HH:MM（00:00〜24:00）", s)
	}
	return hh*60 + mm, nil
}
