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
	"sort"
	"strings"
	"time"
)

// Policy は「この範囲で、いつ、どこまで変えてよいか」（設計 §3.6）。段 2 は
// select・mode・budget を持ち、段 5 で承認と一時的な Policy（期限・理由・誰が）を、
// 段 9 で窓と別枠を足した。
type Policy struct {
	Name   string
	Select Selector // E-M2・N-M2・E-M1
	Mode   Mode     // E-I3・E-Q1・N-Q2・N-I1
	Budget *Budget  // E-J1
	// Approval は当たる変更を承認まで流さない（E-L2・N-L2・E-J10・N-G10）。
	Approval *Approval
	// Applicant は承認を頼む人。承認者に数えない（不変条件 21、E-L2）。
	Applicant string
	// Windows は窓（E-J3・E-R1・N-J1・N-R3）。別枠でない Policy は窓の外で
	// report（当たる変更を知らせるだけ）。別枠の Policy は窓の中でだけ開く。
	// 書かなければいつでも。
	Windows []Window
	// Allow は大量の正当な作業の別枠（E-J2・E-A9・N-J2）。窓の中で承認が揃えば、
	// select と kind に合う変更を max_count まで通常の予算の外で通す。別枠は
	// 予算の免除でない（数えて max_count で止まる、不変条件 9）。
	Allow *Allow
	// Until・Reason・By は一時的な Policy（freeze・その場の report・手順の段の
	// policy）。Until を過ぎれば消える（不変条件 22、E-J4・N-Q1・E-R2・N-R1）。
	Until  time.Time
	Reason string
	By     string
	Origin Origin
}

// Approval は承認の要件（§3.6・§3.7）。Policy と手順の gate で同じ型
// （E-L2・N-L2・E-A5・E-J8）。
type Approval struct {
	// N は数える承認者の数。Of に入る、申請者でない、異なる人だけを数える
	// （N-of-M、不変条件 21）。
	N  int
	Of []string
	// Separation は申請者が分からない依頼を承認させない（申請者≠承認者を確かめ
	// られないものを通さない、E-L2・N-L2）。
	Separation bool
	// Timeout を過ぎた依頼は期限切れ（E-L2・E-L6）。0 は期限なし。
	Timeout time.Duration
	// CoolingOff は別枠の宣言を初めて見てから開くまで待つ長さ（E-J2・N-J2。
	// 承認者を置かない運用で、宣言の取り消しに間を残す）。別枠の Policy だけ。
	// N が 0 なら承認者は要らず、CoolingOff だけで開く。
	CoolingOff time.Duration
}

// AllowKind は別枠に入れる変更の種類（E-J2・E-A9）。
type AllowKind string

const (
	AllowDelete AllowKind = "delete" // 削除（ensure: absent と prune）
	AllowUpdate AllowKind = "update" // 変更
	AllowCreate AllowKind = "create" // 作成
	// AllowReimage は同じ種類・同じラベルの削除と作成の対（入れ直し）。対を 1 と
	// 数え、作成の側は数えない。
	AllowReimage AllowKind = "reimage"
	// AllowDestructive は壊す変更（削除・作り直し・provider が壊すと言った変更）。
	AllowDestructive AllowKind = "destructive"
)

// AllowKinds は書ける種類。
var AllowKinds = []AllowKind{AllowDelete, AllowUpdate, AllowCreate, AllowReimage, AllowDestructive}

// Allow は別枠（§3.6 の allow）。
type Allow struct {
	Kind     AllowKind
	MaxCount int // 窓の全体で通す数（Record に使用量を置く）
}

// Selector は Policy の当たり先。書いた条件を全部満たすものに当たる（E-M2）。
type Selector struct {
	All    bool              `json:"all,omitempty"`
	Types  []string          `json:"types,omitempty"`  // "<provider>/<種類>"
	Labels map[string]string `json:"labels,omitempty"` // 宣言の labels。宣言外は Range の labels（E-D4・N-I3）
}

// String は人が読む形（freeze の一覧・Event）。
func (s Selector) String() string {
	if s.All {
		return "all"
	}
	var parts []string
	for _, t := range s.Types {
		parts = append(parts, "type="+t)
	}
	keys := make([]string, 0, len(s.Labels))
	for k := range s.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, "labels."+k+"="+s.Labels[k])
	}
	return strings.Join(parts, ",")
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
	// MaxIneffectiveApplies は効かない Apply（当てて成功したのに読み直すとずれた
	// まま）がこの回数続いたら、その Resource を止める（E-J6・E-K10・N-J12・N-Q5）。
	// 書かなければ Unlimited で、どの Policy も書かなければ
	// DefaultMaxIneffectiveApplies。
	MaxIneffectiveApplies int
}

// DefaultMaxIneffectiveApplies は当たる Policy のどれも max_ineffective_applies を
// 書かないときの上限（E-J6：同じ Apply を繰り返さず止める）。
const DefaultMaxIneffectiveApplies = 3
