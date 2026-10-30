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

// Package confirmafter は、confirm の先のうち変えたあとだけ確かめるもの
// （observe: と check:、Backlog #200・#214、§23.4）の provider に共通の口:
// 宣言の読み方・読み込みの時点の確かめ・時間の上限の中で繰り返す確かめ・
// 知らせの形。netconf（commit confirmed のあと）と nftables（base chain を
// 変えて時間切れの巻き戻しを仕掛けたあと）が使う。
//
//   - observe:<provider>/<scope>/<id> [expect:{…}]: 別のリソースを、コア
//     （provider.Confirmer）がその provider の Read で観測用の資格のまま、
//     ふだんの範囲で読み直し、expect の欄（JSON の object。欄 → 値）の部分
//     集合が期待どおりになるまで繰り返す。expect を書かなければ、観測できて
//     在ることだけを確かめる。ID は、最初の「/」までが provider、次の「/」
//     までが scope（"site=a,rack=1" の形。無ければ空）、残りがリソースの ID。
//     ID の中に空白は書けない（expect との区切り）。ID の直後の「,」は、
//     次の先が始まる（provider が渡す next が真）ときだけ区切りと読む。
//   - check:<name>: 名前で登録された確かめの関数（pkg/cli.WithConfirmCheck・
//     network_policy の realizer の Output.Checks）を呼ぶ。
//
// どちらもいつも変えたあとだけ確かめる（after: の印は書いても同じ）。到達の
// 先（provider ごと: netconf の self、nftables の self・ssh://・tcp://）が
// 1 つも無い宣言を断るのは provider の受け持ち（変えたあとの確かめだけでは、
// 変えた先に届くことを誰も確かめない）。
package confirmafter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// 先の種類。
const (
	Observe = "observe"
	Check   = "check"
)

// Target は変えたあとだけ確かめる 1 つの先。
type Target struct {
	// Kind は Observe か Check。
	Kind string
	// ID・Expect は observe: の先と期待する欄の値（nil なら在ることだけ）。
	ID     core.ID
	Expect map[string]string
	// Check は check: の関数の名前。
	Check string
}

// String は宣言に書く形（expect は欄の名前の順の JSON）。
func (t Target) String() string {
	if t.Kind == Check {
		return "check:" + t.Check
	}
	s := "observe:" + t.ID.String()
	if len(t.Expect) > 0 {
		var b strings.Builder
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(t.Expect)
		s += " expect:" + strings.TrimSpace(b.String())
	}
	return s
}

// Label は知らせに出す短い形（expect の値は出さない）。
func (t Target) Label() string {
	if t.Kind == Observe {
		return "observe:" + t.ID.String()
	}
	return t.String()
}

// Starts は s が observe: か check: で始まるか。
func Starts(s string) bool {
	return strings.HasPrefix(s, "observe:") || strings.HasPrefix(s, "check:")
}

// Parse は s の先頭の observe: か check: の先を読み、残りを返す。s がどちらでも
// 始まらなければ ok は false。next は「,」の後ろが次の先で始まるか（空も真）。
func Parse(s string, next func(string) bool) (t Target, rest string, ok bool, err error) {
	switch {
	case strings.HasPrefix(s, "observe:"):
		t, rest, err = parseObserve(strings.TrimPrefix(s, "observe:"), next)
		return t, rest, true, err
	case strings.HasPrefix(s, "check:"):
		name, r := Token(strings.TrimPrefix(s, "check:"))
		if err := provider.ValidConfirmCheckName(name); err != nil {
			return Target{}, "", true, fmt.Errorf("check:%s: %v", name, err)
		}
		return Target{Kind: Check, Check: name}, r, true, nil
	}
	return Target{}, s, false, nil
}

// Token は次の「,」か空白までを切り出す。
func Token(s string) (string, string) {
	i := strings.IndexAny(s, ", \t\r\n")
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i:]
}

// parseObserve は observe: の後ろ（ID と、あれば expect:）を読む。
func parseObserve(s string, next func(string) bool) (Target, string, error) {
	i := strings.IndexAny(s, " \t\r\n")
	raw, rest := s, ""
	if i >= 0 {
		raw, rest = s[:i], s[i:]
	}
	// The id ends at a blank, or at a comma followed by the next target (a
	// scope's own commas are followed by a label).
	for j := 0; j < len(raw); j++ {
		if raw[j] == ',' && next(strings.TrimLeft(raw[j+1:], " \t\r\n")) {
			raw, rest = raw[:j], raw[j:]+rest
			break
		}
	}
	id, err := parseID(raw)
	if err != nil {
		return Target{}, "", err
	}
	t := Target{Kind: Observe, ID: id}
	r := strings.TrimLeft(rest, " \t\r\n")
	if !strings.HasPrefix(r, "expect:") {
		return t, rest, nil
	}
	r = strings.TrimPrefix(r, "expect:")
	dec := json.NewDecoder(strings.NewReader(r))
	var raw2 map[string]any
	if err := dec.Decode(&raw2); err != nil || raw2 == nil {
		return Target{}, "", fmt.Errorf("observe:%s: expect: want a JSON object of field → value such as {\"state\":\"…\"}", id)
	}
	t.Expect = map[string]string{}
	for k, v := range raw2 {
		sv, ok := v.(string)
		if !ok {
			return Target{}, "", fmt.Errorf("observe:%s: expect: field %q: want a string value", id, k)
		}
		t.Expect[k] = sv
	}
	if len(t.Expect) == 0 {
		return Target{}, "", fmt.Errorf("observe:%s: expect: {} expects nothing (leave expect: out to confirm only that it is observed present)", id)
	}
	return t, r[dec.InputOffset():], nil
}

// parseID は "<provider>/<scope>/<id>" を読む。
func parseID(s string) (core.ID, error) {
	prov, rest, ok1 := strings.Cut(s, "/")
	scope, local, ok2 := strings.Cut(rest, "/")
	if !ok1 || !ok2 || prov == "" || local == "" {
		return core.ID{}, fmt.Errorf("observe:%s: want observe:<provider>/<scope>/<id> (the scope may be empty: probe//p1)", s)
	}
	sc, err := core.ParseScope(scope)
	if err != nil {
		return core.ID{}, fmt.Errorf("observe:%s: scope: %v", s, err)
	}
	return core.NewID(prov, sc, local), nil
}

// Validate は読み込みの時点の確かめ（I/O をしない）: observe: の先が intent の
// 宣言で、比べられる欄であること・check: の名前が登録されていること。self は
// 変えたリソースそのもの（providerName・localID）で、observe: に書けない。
func Validate(c provider.Confirmer, providerName, localID string, ts []Target) error {
	for _, t := range ts {
		if c == nil {
			return fmt.Errorf("%s: this run checks nothing after a change (no confirmer is given to the provider)", t.Label())
		}
		switch t.Kind {
		case Observe:
			if t.ID.Provider == providerName && t.ID.Local == localID {
				return fmt.Errorf("%s is the resource itself (self reads it)", t.Label())
			}
			if err := c.CheckObserve(t.ID, t.Expect); err != nil {
				return err
			}
		case Check:
			if !c.HasCheck(t.Check) {
				return fmt.Errorf("check:%s: no check function is registered under that name (cli.WithConfirmCheck, or the Checks of a network_policy realizer's Output)", t.Check)
			}
		}
	}
	return nil
}

// Holder は読み込んだ intent の Confirmer を持つ（provider.ConfirmUser の
// 受け口。Apply の最中にも差し替わるので鍵の下に置く）。
type Holder struct {
	mu sync.RWMutex
	c  provider.Confirmer
}

// UseConfirmer implements provider.ConfirmUser.
func (h *Holder) UseConfirmer(c provider.Confirmer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.c = c
}

// Confirmer は今の Confirmer（無ければ nil）。
func (h *Holder) Confirmer() provider.Confirmer {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.c
}

// Item は確かめる 1 つの先と、check: の関数に渡す変えたリソース。
type Item struct {
	Target Target
	Of     provider.ConfirmTarget
}

// Interval は確かめの間隔: 時間の上限の 1/20 を 100ms〜5s に収める。
func Interval(limit time.Duration) time.Duration {
	d := limit / 20
	switch {
	case d < 100*time.Millisecond:
		return 100 * time.Millisecond
	case d > 5*time.Second:
		return 5 * time.Second
	}
	return d
}

// Wait は items を順に、それぞれ通るまで deadline の中で interval ごとに
// 繰り返す。全部が通れば ""、そうでなければ通らなかった先と最後に見た理由
// （"observe:… : is absent" の形）。確かめは観測の文脈で（Confirmer が観測用の
// 資格に切り替える）。
func Wait(ctx context.Context, c provider.Confirmer, items []Item, deadline time.Time, interval time.Duration) string {
	if len(items) == 0 {
		return ""
	}
	if c == nil {
		return "no confirmer is given to the provider, so " + items[0].Target.Label() + " cannot be checked"
	}
	for _, it := range items {
		t := it.Target
		last := ""
		for {
			vctx, cancel := context.WithDeadline(ctx, deadline)
			var err error
			if t.Kind == Observe {
				err = c.Observe(vctx, t.ID, t.Expect)
			} else {
				err = c.Check(vctx, t.Check, it.Of)
			}
			cut := vctx.Err() != nil
			cancel()
			if err == nil {
				break
			}
			if !cut || last == "" {
				last = err.Error()
			}
			wait := interval
			if rest := time.Until(deadline); rest < wait {
				wait = rest
			}
			if wait <= 0 || ctx.Err() != nil {
				return t.Label() + ": " + last
			}
			select {
			case <-ctx.Done():
				return t.Label() + ": " + last
			case <-time.After(wait):
			}
		}
	}
	return ""
}

// Fields は op の宣言の値（確かめの関数に渡す。confirm の欄 skip は除く）。
func Fields(op core.Op, skip string) map[string]string {
	out := map[string]string{}
	for _, f := range op.Fields {
		if f.Field != skip {
			out[f.Field] = f.To
		}
	}
	return out
}
