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
	"testing"
	"time"
)

// 窓の形（E-J3・E-R1）：毎週・曜日の範囲・日をまたぐ窓（曜日は開く日）・決まった期間・tz。
func TestWindowContains(t *testing.T) {
	at := func(s string) time.Time {
		x, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	cases := []struct {
		w    string
		in   []string
		out  []string
		ends string
	}{
		{"02:00-04:00", []string{"2026-11-05T02:00:00Z", "2026-11-05T03:59:00Z"}, []string{"2026-11-05T04:00:00Z", "2026-11-05T01:59:00Z"}, ""},
		{"Mon-Fri 09:00-17:00 Asia/Tokyo", []string{"2026-11-06T00:00:00Z"}, []string{"2026-11-07T00:00:00Z", "2026-11-05T09:00:00Z"}, ""}, // 金 09:00 JST・土・木 18:00 JST
		{"Fri 23:00-01:00", []string{"2026-11-06T23:30:00Z", "2026-11-07T00:30:00Z"}, []string{"2026-11-06T00:30:00Z", "2026-11-07T23:30:00Z"}, ""},
		{"Sat,Sun 00:00-24:00", []string{"2026-11-07T12:00:00Z", "2026-11-08T23:59:00Z"}, []string{"2026-11-09T00:00:00Z"}, ""},
		{"2026-11-05 22:00 .. 2026-11-06 06:00 Asia/Tokyo", []string{"2026-11-05T13:00:00Z"}, []string{"2026-11-05T21:00:00Z", "2026-11-05T12:59:00Z"}, "2026-11-05T21:00:00Z"},
		{"2026-11-05T22:00:00+09:00 .. 2026-11-06T06:00:00+09:00", []string{"2026-11-05T13:00:00Z"}, []string{"2026-11-05T21:00:00Z"}, "2026-11-05T21:00:00Z"},
		{"*", []string{"2026-11-05T13:00:00Z"}, nil, ""},
	}
	for _, tc := range cases {
		w, err := ParseWindow(tc.w)
		if err != nil {
			t.Fatalf("%s: %v", tc.w, err)
		}
		for _, s := range tc.in {
			if !w.Contains(at(s)) {
				t.Errorf("%s は %s を含むはず", tc.w, s)
			}
		}
		for _, s := range tc.out {
			if w.Contains(at(s)) {
				t.Errorf("%s は %s を含まないはず", tc.w, s)
			}
		}
		if tc.ends != "" && (!w.Ended(at(tc.ends)) || w.Ended(at(tc.in[0]))) {
			t.Errorf("%s の終わり", tc.w)
		}
	}
	if !InWindows(nil, time.Now()) {
		t.Fatal("窓を書かなければいつでも")
	}
	for _, bad := range []string{"25:00-26:00", "02:00-02:00", "Satu 02:00-04:00", "02:00", "02:00-04:00 Asia/Tokio", "2026-11-06 00:00 .. 2026-11-05 00:00", "x .. y"} {
		if _, err := ParseWindow(bad); err == nil {
			t.Errorf("%q を断らない", bad)
		}
	}
}
