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

package s3

// objectlock.go — Object Lock（v2 の internal/s3bucket から移した、E-C1・N-C7）。
// 宣言は object_lock（マップ）：
//
//	object_lock: {enable_on_create: true, mode: COMPLIANCE, years: 7}
//
//   - 宣言すれば Object Lock を有効にする。{enabled: false} は「Object Lock が
//     無い」（有効なバケットには宣言どおりにできない。外せないので Apply が
//     何も書かずに断る）。
//   - enable_on_create：作るときに x-amz-bucket-object-lock-enabled を送る
//     （書いたときだけ。持たない実装でも宣言どおりにできるよう、既定は送らず
//     PutObjectLockConfiguration で有効にする）。作る手順の引数で、比べない。
//     既にある Object Lock の無いバケットに true を宣言すると、作り直さずに
//     Apply が理由つきで断る。
//   - mode（GOVERNANCE・COMPLIANCE）と days か years：バケットの既定の保持。
//     書かなければ既定の保持は無い（有れば外す）。
//
// 正規形（観測の値）は期間を Days にする（years は 365 倍。Years で PUT しても
// Days で返す実装があるため）：{enabled: false}・{enabled: true}（既定の保持が
// 無い）・{enabled: true, mode: M, days: N}。
//
// 既にあるバケットの保持の設定を変える変更は、向きによらず承認の要る壊す変更
// （Classify）。新しく作るバケットに初めて掛けるのはふつうの作成。

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Object Lock の既定の保持の mode。
const (
	ObjectLockGovernance = "GOVERNANCE"
	ObjectLockCompliance = "COMPLIANCE"
)

// daysPerYear は years を Days に直す倍率。
const daysPerYear = 365

// 期間の上限（S3 の決まり。Days は 36500 日、Years は 100 年まで）。
const (
	maxRetentionDays  = 36500
	maxRetentionYears = 100
)

// LockSpec は object_lock の宣言か観測。
type LockSpec struct {
	// Enabled は Object Lock を有効にするか（{enabled: false} の宣言だけが false）。
	Enabled bool
	// EnableOnCreate は作るときに有効化のヘッダを送るか。
	EnableOnCreate bool
	// Mode は既定の保持の mode（"" は既定の保持が無い）。
	Mode string
	// Days・Years は期間（どちらか一方）。
	Days, Years int
}

// days は期間の正規形（Days）。
func (l LockSpec) days() int {
	if l.Years > 0 {
		return l.Years * daysPerYear
	}
	return l.Days
}

// value は正規形（観測の値の形）。
func (l LockSpec) value() map[string]any {
	if !l.Enabled {
		return map[string]any{"enabled": false}
	}
	if l.Mode == "" {
		return map[string]any{"enabled": true}
	}
	return map[string]any{"enabled": true, "mode": l.Mode, "days": int64(l.days())}
}

// wholeNumber は宣言の数（YAML の読み方・JSON による違いを問わない）。
func wholeNumber(x any) (int64, bool) {
	switch n := x.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		if n == math.Trunc(n) && !math.IsInf(n, 0) {
			return int64(n), true
		}
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
	}
	return 0, false
}

// ParseObjectLock は object_lock の宣言（マップ）を読み、確かめる。
func ParseObjectLock(v any) (LockSpec, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return LockSpec{}, errors.New("object_lock はマップで書く（{mode: COMPLIANCE, days: 30} など）")
	}
	out := LockSpec{Enabled: true}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	period := func(k string, x any, max int) (int, error) {
		n, ok := wholeNumber(x)
		if !ok || n <= 0 || n > int64(max) {
			return 0, fmt.Errorf("object_lock の %s は 1 から %d までの整数", k, max)
		}
		return int(n), nil
	}
	var err error
	for _, k := range keys {
		x := m[k]
		switch k {
		case "enabled":
			b, ok := x.(bool)
			if !ok {
				return LockSpec{}, errors.New("object_lock の enabled は true か false")
			}
			out.Enabled = b
		case "enable_on_create":
			b, ok := x.(bool)
			if !ok {
				return LockSpec{}, errors.New("object_lock の enable_on_create は true か false")
			}
			out.EnableOnCreate = b
		case "mode":
			s, _ := x.(string)
			if s != ObjectLockGovernance && s != ObjectLockCompliance {
				return LockSpec{}, fmt.Errorf("object_lock の mode %v は %s か %s", x, ObjectLockGovernance, ObjectLockCompliance)
			}
			out.Mode = s
		case "days":
			if out.Days, err = period(k, x, maxRetentionDays); err != nil {
				return LockSpec{}, err
			}
		case "years":
			if out.Years, err = period(k, x, maxRetentionYears); err != nil {
				return LockSpec{}, err
			}
		default:
			return LockSpec{}, fmt.Errorf("object_lock に知らない項目 %q（書ける項目: days, enable_on_create, enabled, mode, years）", k)
		}
	}
	switch {
	case !out.Enabled && len(m) > 1:
		return LockSpec{}, errors.New("object_lock の enabled: false にはほかの項目を書けない")
	case out.Days > 0 && out.Years > 0:
		return LockSpec{}, errors.New("object_lock には days か years のどちらか一方を書く")
	case out.Mode != "" && out.Days == 0 && out.Years == 0:
		return LockSpec{}, errors.New("object_lock の mode には保持の期間（days か years）が要る")
	case out.Mode == "" && (out.Days > 0 || out.Years > 0):
		return LockSpec{}, errors.New("object_lock の保持の期間には mode（GOVERNANCE か COMPLIANCE）が要る")
	}
	return out, nil
}

// lockFromObserved は GetObjectLockConfiguration の結果を LockSpec にする。
func lockFromObserved(ol ObjectLock) LockSpec {
	if !ol.Enabled {
		return LockSpec{}
	}
	return LockSpec{Enabled: true, Mode: ol.Mode, Days: ol.Days, Years: ol.Years}
}

// lockFromValue は観測の値（正規形）を LockSpec に戻す。読めなければ ok が false。
func lockFromValue(v any) (LockSpec, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return LockSpec{}, false
	}
	en, ok := m["enabled"].(bool)
	if !ok {
		return LockSpec{}, false
	}
	l := LockSpec{Enabled: en}
	if s, isStr := m["mode"].(string); isStr {
		l.Mode = s
		d, ok := wholeNumber(m["days"])
		if !ok {
			return LockSpec{}, false
		}
		l.Days = int(d)
	}
	return l, true
}

// matchLock は宣言と観測の object_lock を正規形で比べる（enable_on_create は比べ
// ない、years は Days で比べる）。
func matchLock(desired, observed any) bool {
	if observed == nil {
		return false
	}
	want, err := ParseObjectLock(desired)
	if err != nil {
		return false
	}
	got, ok := lockFromValue(observed)
	return ok && sameLock(want, got)
}

func sameLock(a, b LockSpec) bool {
	return a.Enabled == b.Enabled && a.Mode == b.Mode && a.days() == b.days()
}

// lockConflict は既にあるバケットの Object Lock（観測 from）を宣言 to にでき
// ないときの理由を返す（作り直さない）。できるなら ""。
func lockConflict(from, to LockSpec) string {
	switch {
	case from.Enabled && !to.Enabled:
		return "object_lock: このバケットは Object Lock が有効で、外せない（バケットの Object Lock を宣言するか、object_lock を書かない）"
	case !from.Enabled && to.Enabled && to.EnableOnCreate:
		return "object_lock: enable_on_create は作るときだけで、このバケットは Object Lock の無いまま既にある。Sakutei は作り直さない（enable_on_create を外せば PutObjectLockConfiguration で有効にする（承認の要る変更）。か、新しいバケットを作る）"
	}
	return ""
}

// lockChange は既にあるバケットの保持の設定の from → to の変更を文にする
// （承認の理由）。変わらなければ ""。
func lockChange(from, to LockSpec) string {
	describe := func(l LockSpec) string {
		if l.Mode == "" {
			return "無し"
		}
		return fmt.Sprintf("%s %d 日", l.Mode, l.days())
	}
	var parts []string
	if !from.Enabled && to.Enabled {
		parts = append(parts, "既にあるバケットの Object Lock を有効にする（もう外せない）")
	}
	switch {
	case from.Mode == to.Mode && from.days() == to.days():
	case to.Mode == "":
		parts = append(parts, fmt.Sprintf("既定の保持（%s）を外す：これから書く版は既定では守られない", describe(from)))
	case from.Mode == "":
		parts = append(parts, fmt.Sprintf("既定の保持 %s を掛ける：これから書く版は保持が終わるまで消せない", describe(to)))
	default:
		if from.Mode != to.Mode {
			p := fmt.Sprintf("既定の保持の mode を %s → %s に変える", from.Mode, to.Mode)
			if to.Mode == ObjectLockCompliance {
				p += "（COMPLIANCE の保持は誰も短くも外しもできない）"
			} else {
				p += "（これから書く版は bypass の権限で外せる）"
			}
			parts = append(parts, p)
		}
		switch d0, d1 := from.days(), to.days(); {
		case d1 > d0:
			parts = append(parts, fmt.Sprintf("既定の保持を %d → %d 日に延ばす（これから書く版は短くし直せない）", d0, d1))
		case d1 < d0:
			parts = append(parts, fmt.Sprintf("既定の保持を %d → %d 日に縮める（これから書く版の守りが弱まる）", d0, d1))
		}
	}
	return strings.Join(parts, "。")
}

// lockPutErr は PutObjectLockConfiguration の誤りに理由を足す。
func lockPutErr(err error) error {
	if err == nil {
		return nil
	}
	if KindOf(err) == KindNotImplemented {
		return putErr(err)
	}
	if KindOf(err) != KindRequest {
		return err
	}
	hint := "endpoint が Object Lock の設定を断った（受けない mode・期間・形）。endpoint の受ける形で宣言する"
	if CodeOf(err) == "InvalidBucketState" {
		hint = "endpoint が既にあるバケットの Object Lock の有効化を断った（多くの endpoint は作るとき（enable_on_create）か、先に versioning を Enabled にしたときだけ許す）。Sakutei はバケットを作り直さない"
	}
	return fmt.Errorf("%s: %w", hint, err)
}
