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

package netconf

// confirm.go — 変えたあとの確かめの先（Backlog #200、§23.4）。
//
// 宣言の欄 confirm に、確かめる先を「, 」で区切って書く:
//
//	confirm: 'self, observe:netconf//sw2/if-uplink expect:{"config":"<interfaces …/>"}, check:route-seen'
//
//   - self: 機器そのもの（新しい ssh 接続で届くこと・部分木が宣言どおりな
//     こと、apply.go の verify）。書かなければ self だけ（今までどおり）。
//   - observe:<provider>/<scope>/<id> [expect:{…}]: 別のリソースを観測し
//     直し、expect の欄（JSON の object。欄 → 値。その部分集合だけを比べる）が
//     期待どおりになるまで繰り返し読む。expect を書かなければ、観測できて
//     在ることだけを確かめる。読むのはコア（provider.Confirmer）で、その
//     リソースの provider の Read を観測用の資格で、ふだんの範囲のまま
//     （intent が宣言するリソースだけ。netconf なら部分木の filter と機器の
//     NACM）。ID は、最初の「/」までが provider、次の「/」までが scope
//     （"site=a,rack=1" の形。無ければ空）、残りがリソースの ID。ID の中に
//     空白は書けない（expect との区切り）。次の先とは「, 」で区切る（ID の
//     直後の「,」は、次の先が self・after:・observe:・check: で始まるときだけ
//     区切りと読む。scope の「,」はそのまま）。
//   - check:<name>: 名前で登録された確かめの関数（pkg/cli.WithConfirmCheck・
//     network_policy の realizer の Output.Checks）を呼ぶ。何をどう読んで
//     判定するかは関数を渡す側が持つ。
//
// observe: と check: は、いつも変えたあとだけ確かめる（commit confirmed の
// あと。after: の印は書いても書かなくても同じ）。到達の先（self）が 1 つも
// 無い宣言は読み込みで断る（変えたあとの確かめだけでは、機器に届くことを
// 誰も確かめない）。self に after: は付けられない。
//
// 確かめの時間の上限は self と同じ（commit confirmed から confirm-timeout の
// 半分、commit_group では一番短いものの半分）。commit_group では、全部の台が
// commit confirmed を終え、全部の台の self が通ってから、全部の台の observe:
// と check: を待つ。1 つでも通らなければ確定しない（self が落ちたときと同じ
// 扱い: 機器の巻き戻しに任せ、同じ宣言は変わるまで当て直さない）。
//
// confirm は機器に無い「どう確かめるか」の宣言なので、RefinePlan はほかの
// 差分があるときだけ op に残す。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// FieldConfirm は確かめる先の一覧（宣言の欄。機器には無い）。
const FieldConfirm = "confirm"

// 確かめる先の種類。
const (
	ConfirmSelf    = "self"
	ConfirmObserve = "observe"
	ConfirmCheck   = "check"
)

// confirmAfter は「変えたあとだけ確かめる」印（observe: と check: はいつも）。
const confirmAfter = "after:"

// ConfirmTarget は確かめる 1 つの先。
type ConfirmTarget struct {
	Kind string
	// ID・Expect は observe: の先と期待する欄の値。
	ID     core.ID
	Expect map[string]string
	// Check は check: の関数の名前。
	Check string
}

// String は宣言に書く形（expect は欄の名前の順の JSON）。
func (t ConfirmTarget) String() string {
	switch t.Kind {
	case ConfirmObserve:
		s := "observe:" + t.ID.String()
		if len(t.Expect) > 0 {
			var b strings.Builder
			enc := json.NewEncoder(&b)
			enc.SetEscapeHTML(false)
			_ = enc.Encode(t.Expect)
			s += " expect:" + strings.TrimSpace(b.String())
		}
		return s
	case ConfirmCheck:
		return "check:" + t.Check
	}
	return ConfirmSelf
}

// label は知らせに出す短い形（expect の値は出さない）。
func (t ConfirmTarget) label() string {
	if t.Kind == ConfirmObserve {
		return "observe:" + t.ID.String()
	}
	return t.String()
}

// ParseConfirm は confirm の宣言を読む。空なら nil（self だけ）。
func ParseConfirm(s string) ([]ConfirmTarget, error) {
	rest := strings.TrimSpace(s)
	if rest == "" {
		return nil, nil
	}
	var out []ConfirmTarget
	seen := map[string]bool{}
	for {
		rest = strings.TrimLeft(rest, " \t\r\n")
		after := strings.HasPrefix(rest, confirmAfter)
		rest = strings.TrimLeft(strings.TrimPrefix(rest, confirmAfter), " \t\r\n")
		var t ConfirmTarget
		var err error
		switch {
		case strings.HasPrefix(rest, "observe:"):
			t, rest, err = parseObserve(strings.TrimPrefix(rest, "observe:"))
		case strings.HasPrefix(rest, "check:"):
			name, r := token(strings.TrimPrefix(rest, "check:"))
			if err = provider.ValidConfirmCheckName(name); err != nil {
				err = fmt.Errorf("check:%s: %v", name, err)
			}
			t, rest = ConfirmTarget{Kind: ConfirmCheck, Check: name}, r
		default:
			name, r := token(rest)
			if name != ConfirmSelf {
				return nil, fmt.Errorf("%q: want self, observe:<provider>/<scope>/<id> [expect:{…}] or check:<name>, separated by commas", name)
			}
			if after {
				return nil, fmt.Errorf("after:self: self is the device itself over a new connection, confirmed after every change anyway; observe: and check: are always confirmed after the change, after: is not needed")
			}
			t, rest = ConfirmTarget{Kind: ConfirmSelf}, r
		}
		if err != nil {
			return nil, err
		}
		key := t.String()
		if seen[key] {
			return nil, fmt.Errorf("%s is listed twice", t.label())
		}
		seen[key] = true
		out = append(out, t)
		rest = strings.TrimLeft(rest, " \t\r\n")
		if rest == "" {
			break
		}
		if rest[0] != ',' {
			return nil, fmt.Errorf("%q: want a comma between the targets", rest)
		}
		rest = rest[1:]
	}
	if !hasSelf(out) {
		return nil, fmt.Errorf("%q: no target is reached: observe: and check: are confirmed only after the change; write self too (the device over a new connection)", s)
	}
	return out, nil
}

func hasSelf(ts []ConfirmTarget) bool {
	for _, t := range ts {
		if t.Kind == ConfirmSelf {
			return true
		}
	}
	return false
}

// token は次の「,」か空白までを切り出す。
func token(s string) (string, string) {
	i := strings.IndexAny(s, ", \t\r\n")
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i:]
}

// parseObserve は observe: の後ろ（ID と、あれば expect:）を読む。
func parseObserve(s string) (ConfirmTarget, string, error) {
	i := strings.IndexAny(s, " \t\r\n")
	raw, rest := s, ""
	if i >= 0 {
		raw, rest = s[:i], s[i:]
	}
	// The id ends at a blank, or at a comma followed by the next target (a
	// scope's own commas are followed by a label).
	for j := 0; j < len(raw); j++ {
		if raw[j] == ',' && nextTarget(raw[j+1:]) {
			raw, rest = raw[:j], raw[j:]+rest
			break
		}
	}
	id, err := parseObserveID(raw)
	if err != nil {
		return ConfirmTarget{}, "", err
	}
	t := ConfirmTarget{Kind: ConfirmObserve, ID: id}
	r := strings.TrimLeft(rest, " \t\r\n")
	if !strings.HasPrefix(r, "expect:") {
		return t, rest, nil
	}
	r = strings.TrimPrefix(r, "expect:")
	dec := json.NewDecoder(strings.NewReader(r))
	var raw2 map[string]any
	if err := dec.Decode(&raw2); err != nil || raw2 == nil {
		return ConfirmTarget{}, "", fmt.Errorf("observe:%s: expect: want a JSON object of field → value such as {\"config\":\"…\"}", id)
	}
	t.Expect = map[string]string{}
	for k, v := range raw2 {
		sv, ok := v.(string)
		if !ok {
			return ConfirmTarget{}, "", fmt.Errorf("observe:%s: expect: field %q: want a string value", id, k)
		}
		t.Expect[k] = sv
	}
	if len(t.Expect) == 0 {
		return ConfirmTarget{}, "", fmt.Errorf("observe:%s: expect: {} expects nothing (leave expect: out to confirm only that it is observed present)", id)
	}
	return t, r[dec.InputOffset():], nil
}

// nextTarget は s が次の確かめる先で始まるか（空も）。
func nextTarget(s string) bool {
	if s == "" {
		return true
	}
	if tok, _ := token(s); tok == ConfirmSelf {
		return true
	}
	for _, p := range []string{confirmAfter, "observe:", "check:"} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// parseObserveID は "<provider>/<scope>/<id>" を読む。
func parseObserveID(s string) (core.ID, error) {
	prov, rest, ok1 := strings.Cut(s, "/")
	scope, local, ok2 := strings.Cut(rest, "/")
	if !ok1 || !ok2 || prov == "" || local == "" {
		return core.ID{}, fmt.Errorf("observe:%s: want observe:<provider>/<scope>/<id> (the scope may be empty: netconf//sw2/if-uplink)", s)
	}
	sc, err := core.ParseScope(scope)
	if err != nil {
		return core.ID{}, fmt.Errorf("observe:%s: scope: %v", s, err)
	}
	return core.NewID(prov, sc, local), nil
}

// validateConfirmField は読み込みの時点の confirm の確かめ（ValidateIntent
// から）: 書式と、observe: の先・check: の名前を Confirmer に確かめる。
func (p *Provider) validateConfirmField(localID string, fields map[string]string) error {
	ts, err := ParseConfirm(fields[FieldConfirm])
	if err != nil {
		return fmt.Errorf("%s %q: confirm: %w", Name, localID, err)
	}
	c := p.confirmer()
	for _, t := range ts {
		if t.Kind == ConfirmSelf {
			continue
		}
		if c == nil {
			return fmt.Errorf("%s %q: confirm: %s: this run checks nothing after a change (no confirmer is given to the provider)", Name, localID, t.label())
		}
		switch t.Kind {
		case ConfirmObserve:
			if t.ID.Provider == Name && t.ID.Local == localID {
				return fmt.Errorf("%s %q: confirm: %s is the resource itself (self reads it)", Name, localID, t.label())
			}
			if err := c.CheckObserve(t.ID, t.Expect); err != nil {
				return fmt.Errorf("%s %q: confirm: %w", Name, localID, err)
			}
		case ConfirmCheck:
			if !c.HasCheck(t.Check) {
				return fmt.Errorf("%s %q: confirm: check:%s: no check function is registered under that name (cli.WithConfirmCheck, or the Checks of a network_policy realizer's Output)", Name, localID, t.Check)
			}
		}
	}
	return nil
}

// UseConfirmer は読み込んだ intent の Confirmer を受ける（provider.ConfirmUser）。
func (p *Provider) UseConfirmer(c provider.Confirmer) {
	p.confMu.Lock()
	defer p.confMu.Unlock()
	p.conf = c
}

func (p *Provider) confirmer() provider.Confirmer {
	p.confMu.RLock()
	defer p.confMu.RUnlock()
	return p.conf
}

// afterTargets は変えたあとだけ確かめる先（observe: と check:）。
func afterTargets(ts []ConfirmTarget) []ConfirmTarget {
	var out []ConfirmTarget
	for _, t := range ts {
		if t.Kind != ConfirmSelf {
			out = append(out, t)
		}
	}
	return out
}

// afterChecks は observe: と check: を、それぞれ通るまで deadline の中で
// 繰り返す。全部が通れば ""、そうでなければ通らなかった先と最後に見た理由。
// 確かめは観測の文脈で（Confirmer が観測用の資格に切り替える）。
func (a *applyRun) afterChecks(ctx context.Context, deadline time.Time) string {
	ts := afterTargets(a.confirm)
	if len(ts) == 0 {
		return ""
	}
	c := a.p.confirmer()
	if c == nil {
		return "no confirmer is given to the provider, so " + ts[0].label() + " cannot be checked"
	}
	target := provider.ConfirmTarget{ID: a.target, Fields: a.fields}
	for _, t := range ts {
		last := ""
		for {
			vctx, cancel := context.WithDeadline(ctx, deadline)
			var err error
			if t.Kind == ConfirmObserve {
				err = c.Observe(vctx, t.ID, t.Expect)
			} else {
				err = c.Check(vctx, t.Check, target)
			}
			cut := vctx.Err() != nil
			cancel()
			if err == nil {
				break
			}
			if !cut || last == "" {
				last = err.Error()
			}
			wait := a.pollInterval()
			if rest := time.Until(deadline); rest < wait {
				wait = rest
			}
			if wait <= 0 || ctx.Err() != nil {
				return t.label() + ": " + last
			}
			select {
			case <-ctx.Done():
				return t.label() + ": " + last
			case <-time.After(wait):
			}
		}
	}
	return ""
}

// confirmFields は op の宣言の値（確かめの関数に渡す。confirm 自身は除く）。
func confirmFields(op core.Op) map[string]string {
	out := map[string]string{}
	for _, f := range op.Fields {
		if f.Field != FieldConfirm {
			out[f.Field] = f.To
		}
	}
	return out
}
