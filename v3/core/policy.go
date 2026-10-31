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

import "time"

// Policy は「この範囲で、いつ、どこまで変えてよいか」（設計 §3.6）。段 2 は
// select・mode・budget を持つ。窓・承認・別枠・一時的な Policy は段 5 で足す。
type Policy struct {
	Name   string
	Select Selector // E-M2・N-M2・E-M1
	Mode   Mode     // E-I3・E-Q1・N-Q2・N-I1
	Budget *Budget  // E-J1
	Origin Origin
}

// Selector は Policy の当たり先。書いた条件を全部満たすものに当たる（E-M2）。
type Selector struct {
	All    bool
	Types  []string          // "<provider>/<種類>"
	Labels map[string]string // 宣言の labels。宣言外は Range の labels（E-D4・N-I3）
}

// Matches は種類 typ・ラベル labels のものに当たるか。
func (s Selector) Matches(typ string, labels map[string]string) bool {
	if s.All {
		return true
	}
	if len(s.Types) == 0 && len(s.Labels) == 0 {
		return false
	}
	if len(s.Types) > 0 {
		hit := false
		for _, t := range s.Types {
			if t == typ {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	for k, v := range s.Labels {
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// Mode は当てるか知らせるだけか。
type Mode int

const (
	// ModeApply は当てる（既定）。
	ModeApply Mode = iota
	// ModeReport は差分を知らせるだけ。観測と知らせは続ける（E-I3・E-Q1）。
	ModeReport
)

func (m Mode) String() string {
	if m == ModeReport {
		return "report"
	}
	return "apply"
}

// Unlimited は予算の上限を書かないときの値。
const Unlimited = -1

// Budget は変更の予算（E-J1）。超えたら超過分の保留でなく周全体を止める
// （不変条件 20）。上限を書かない項目は Unlimited。
type Budget struct {
	PerCycle             int           // 1 周の変更の数
	DestructivePerCycle  int           // 1 周の壊す変更の数
	DestructivePerWindow int           // 周をまたいだ壊す変更の累積（使用量は Record）
	Window               time.Duration // DestructivePerWindow の窓
}
