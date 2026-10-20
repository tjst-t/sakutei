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
	"testing"
	"time"
)

func TestWindowContains(t *testing.T) {
	// 2026-10-24 は土曜。
	at := func(day, h, m int) time.Time { return time.Date(2026, 10, day, h, m, 0, 0, time.UTC) }
	cases := []struct {
		spec string
		t    time.Time
		want bool
	}{
		{"", at(21, 12, 0), true},
		{"02:00-04:00 UTC", at(21, 2, 0), true},
		{"02:00-04:00 UTC", at(21, 4, 0), false},
		{"Sat 02:00-04:00 UTC", at(24, 3, 59), true},
		{"Sat 02:00-04:00 UTC", at(23, 3, 0), false},
		{"Mon-Fri 22:00-06:00 UTC", at(23, 23, 0), true}, // 金曜の夜
		{"Mon-Fri 22:00-06:00 UTC", at(24, 5, 59), true}, // 金曜に始まった窓が土曜の朝に残る
		{"Mon-Fri 22:00-06:00 UTC", at(24, 22, 0), false},
		{"Mon-Fri 22:00-06:00 UTC", at(19, 3, 0), false}, // 月曜の朝は日曜始まりなので外
		{"Fri-Mon 00:00-24:00 UTC", at(25, 12, 0), true}, // 曜日の範囲は週を跨げる
		{"Fri-Mon 00:00-24:00 UTC", at(21, 12, 0), false},
		{"Sat,Sun 02:00-04:00 Asia/Tokyo", time.Date(2026, 10, 24, 17, 30, 0, 0, time.UTC), true}, // JST 日曜 02:30
	}
	for _, c := range cases {
		w, err := ParseWindow(c.spec)
		if err != nil {
			t.Fatalf("ParseWindow(%q): %v", c.spec, err)
		}
		if got := w.Contains(c.t); got != c.want {
			t.Errorf("%q.Contains(%s) = %v, want %v", c.spec, c.t.Format("Mon 15:04"), got, c.want)
		}
	}
}

func TestParseWindowRejects(t *testing.T) {
	for _, spec := range []string{"Sat", "Sat 2-4", "Sat 02:00", "Sat 02:60-04:00", "Xyz 02:00-04:00", "02:00-04:00 Nowhere/Zone", "Sat 02:00-04:00 UTC extra"} {
		if _, err := ParseWindow(spec); err == nil {
			t.Errorf("ParseWindow(%q) must fail", spec)
		}
	}
}
