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

package s3bucket

// objectlock.go — Object Lock（Backlog #205）。宣言は object_lock（JSON の
// オブジェクト）:
//
//	{"enable_on_create": true, "mode": "COMPLIANCE", "years": 7}
//
//   - 宣言すれば Object Lock を有効にする。"" は「Object Lock が無い」。
//   - enable_on_create: 作るときに x-amz-bucket-object-lock-enabled を送る
//     （書いたときだけ。持たない実装でも宣言どおりにできるように、既定は送らず
//     PutObjectLockConfiguration で有効にする）。作る手順の引数で、比べない。
//     既にある Object Lock の無いバケットに true を宣言すると食い違いとして
//     止める（作り直さない）。
//   - mode（GOVERNANCE・COMPLIANCE）と days か years: バケットの既定の保持。
//     書かなければ既定の保持は無い（有るなら外す）。
//
// 正規形は期間を Days にする（years は 365 倍。Years で PUT しても Get で Days を
// 返す実装があるため）: Object Lock が無ければ ""、有れば {"days":N,"mode":M}
// か、既定の保持が無ければ {}。観測（GetObjectLockConfiguration）も同じ形にする。
//
// 既にあるバケットの保持の設定を変える Update は、向きによらず承認の要る壊す
// 変更（ApprovalRequired）。新しく作るバケットに初めて掛けるのはふつうの作成。

import (
	"encoding/json"
	"errors"
	"fmt"
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

// LockSpec は object_lock の宣言。
type LockSpec struct {
	// Enabled は Object Lock を有効にするか（"" の宣言だけが false）。
	Enabled bool
	// EnableOnCreate は作るときに有効化のヘッダを送るか。
	EnableOnCreate bool
	// Mode は既定の保持の mode（"" は既定の保持が無い）。
	Mode string
	// Days・Years は宣言した期間（どちらか一方）。
	Days, Years int
}

// days は期間の正規形（Days）。
func (l LockSpec) days() int {
	if l.Years > 0 {
		return l.Years * daysPerYear
	}
	return l.Days
}

// canon は正規形の文字列。
func (l LockSpec) canon() string {
	if !l.Enabled {
		return ""
	}
	if l.Mode == "" {
		return "{}"
	}
	return canonJSON(map[string]any{"mode": l.Mode, "days": l.days()})
}

// ParseObjectLock は object_lock の宣言を読み、検査する。"" は Object Lock が
// 無いこと。
func ParseObjectLock(s string) (LockSpec, error) {
	if strings.TrimSpace(s) == "" {
		return LockSpec{}, nil
	}
	v, err := decodeJSON(s)
	if err != nil {
		return LockSpec{}, fmt.Errorf("object_lock is not valid JSON: %v", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return LockSpec{}, errors.New(`object_lock must be a JSON object such as {"mode": "COMPLIANCE", "days": 30}`)
	}
	out := LockSpec{Enabled: true}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	period := func(k string, x any, max int) (int, error) {
		n, ok := x.(json.Number)
		if !ok {
			return 0, fmt.Errorf("object_lock: %s must be a number", k)
		}
		i, err := n.Int64()
		if err != nil || i <= 0 || i > int64(max) {
			return 0, fmt.Errorf("object_lock: %s %s: want a whole number from 1 to %d", k, n, max)
		}
		return int(i), nil
	}
	for _, k := range keys {
		x := m[k]
		switch k {
		case "enable_on_create":
			b, ok := x.(bool)
			if !ok {
				return LockSpec{}, errors.New("object_lock: enable_on_create must be true or false")
			}
			out.EnableOnCreate = b
		case "mode":
			s, _ := x.(string)
			if s != ObjectLockGovernance && s != ObjectLockCompliance {
				return LockSpec{}, fmt.Errorf("object_lock: mode %v: want %s or %s", x, ObjectLockGovernance, ObjectLockCompliance)
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
			return LockSpec{}, fmt.Errorf("object_lock: unknown key %q (want enable_on_create, mode, days or years)", k)
		}
	}
	switch {
	case out.Days > 0 && out.Years > 0:
		return LockSpec{}, errors.New("object_lock: write either days or years, not both")
	case out.Mode != "" && out.Days == 0 && out.Years == 0:
		return LockSpec{}, errors.New("object_lock: mode needs a retention period (days or years)")
	case out.Mode == "" && (out.Days > 0 || out.Years > 0):
		return LockSpec{}, errors.New("object_lock: a retention period needs a mode (GOVERNANCE or COMPLIANCE)")
	}
	return out, nil
}

// CanonObjectLock は object_lock の宣言の正規形を返す。
func CanonObjectLock(s string) (string, error) {
	l, err := ParseObjectLock(s)
	if err != nil {
		return "", err
	}
	return l.canon(), nil
}

// lockFromObserved は GetObjectLockConfiguration の結果を LockSpec にする。
func lockFromObserved(ol ObjectLock) LockSpec {
	if !ol.Enabled {
		return LockSpec{}
	}
	return LockSpec{Enabled: true, Mode: ol.Mode, Days: ol.Days, Years: ol.Years}
}

// lockFromCanon は正規形（観測の State）を LockSpec に戻す。読めなければ ok が
// false。
func lockFromCanon(s string) (LockSpec, bool) {
	if s == "" {
		return LockSpec{}, true
	}
	var doc struct {
		Mode string `json:"mode"`
		Days int    `json:"days"`
	}
	if json.Unmarshal([]byte(s), &doc) != nil {
		return LockSpec{}, false
	}
	return LockSpec{Enabled: true, Mode: doc.Mode, Days: doc.Days}, true
}

// lockChange は既にあるバケットの保持の設定の from → to の変更を文にする
// （承認の理由）。変わらなければ ""。
func lockChange(from, to LockSpec) string {
	describe := func(l LockSpec) string {
		if l.Mode == "" {
			return "none"
		}
		return fmt.Sprintf("%s %d days", l.Mode, l.days())
	}
	var parts []string
	if !from.Enabled && to.Enabled {
		parts = append(parts, "turns Object Lock on for an existing bucket (it cannot be turned off again)")
	}
	switch {
	case from.Mode == to.Mode && from.days() == to.days():
	case to.Mode == "":
		parts = append(parts, fmt.Sprintf("removes the default retention (%s): versions written from now on are not protected by default", describe(from)))
	case from.Mode == "":
		parts = append(parts, fmt.Sprintf("sets the default retention %s: versions written from now on cannot be removed before it ends", describe(to)))
	default:
		if from.Mode != to.Mode {
			p := fmt.Sprintf("changes the default retention mode %s → %s", from.Mode, to.Mode)
			if to.Mode == ObjectLockCompliance {
				p += " (no one can shorten or lift COMPLIANCE retention on the versions it protects)"
			} else {
				p += " (versions written from now on can be unlocked with the bypass permission)"
			}
			parts = append(parts, p)
		}
		switch d0, d1 := from.days(), to.days(); {
		case d1 > d0:
			parts = append(parts, fmt.Sprintf("lengthens the default retention %d → %d days (versions written from now on cannot be shortened back)", d0, d1))
		case d1 < d0:
			parts = append(parts, fmt.Sprintf("shortens the default retention %d → %d days (versions written from now on are protected for less)", d0, d1))
		}
	}
	return strings.Join(parts, "; ")
}

// lockPutErr は PutObjectLockConfiguration のエラーを理由つきの provider の
// エラーにする。endpoint が断った（受けない mode・期間、既にあるバケットに
// 有効にできない）のは PermanentError（黙って流さない）。
func lockPutErr(err error) error {
	if err == nil {
		return nil
	}
	if KindOf(err) == KindNotImplemented {
		return putErr(err)
	}
	if KindOf(err) != KindRequest {
		return classify(err)
	}
	hint := "the endpoint refused the Object Lock configuration (a mode, period or form it does not accept); declare what the endpoint accepts"
	if CodeOf(err) == "InvalidBucketState" {
		hint = "the endpoint refused to turn Object Lock on for this existing bucket (many endpoints allow it only when the bucket is created, with enable_on_create, or with versioning Enabled first); Sakutei does not re-create the bucket"
	}
	return classify(fmt.Errorf("%s: %w", hint, err))
}
