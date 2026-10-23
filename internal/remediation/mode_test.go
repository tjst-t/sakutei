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

import "testing"

// --mode の値: auto と alert-only だけを受け取る。空の値は auto とみなさずに拒否する
// （`--mode=$MODE` で MODE が空のとき、黙って修復を開けない。§3。Backlog #102 の試用で見つけた）。
func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"auto": ModeAuto, "alert-only": ModeAlertOnly} {
		if got, err := ParseMode(in); err != nil || got != want {
			t.Errorf("ParseMode(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "Auto", "alertonly", "freeze"} {
		if _, err := ParseMode(in); err == nil {
			t.Errorf("ParseMode(%q) must be rejected", in)
		}
	}
}
