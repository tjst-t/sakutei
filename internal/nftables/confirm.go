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

package nftables

// confirm.go — 到達を確かめる先の宣言（Backlog #179）。
//
// 設計判断:
//
//   - 確かめる先は base chain の宣言の `confirm` に書く（正しい状態は宣言に
//     置く、§1.7）。「, 」で区切った一覧で、どの先も新しい接続で届いたときだけ
//     確かめられたとする:
//     self（Sakutei 自身の管理の接続。離れたホストはそのホストへの新しい ssh、
//     手元は ruleset_confirm_address への TCP。書かなければこれだけ）・
//     ssh://[user@]host[:port]（別の経路の ssh。鍵・known_hosts はそのホストの
//     remote の設定。user・port を書かなければそのホストの設定のまま。手元の
//     live には ssh の設定が無いので書けない）・tcp://host:port（Sakutei から
//     の TCP の接続）。
//   - confirm は現実に無い（観測に出ない）「どう変えるか」の宣言なので、
//     PlanRefiner はほかの差分があるときだけ op に残す（それだけの差分は
//     収束している、hostconf の on_change と同じ）。
//   - regular chain の宣言には書けない（守りを通さないので意味が無い）。
//   - 先の前に `after:` を書いた先（Backlog #192）は変えたあとだけ確かめる:
//     変える chain を通る経路の先（例: forward を通る別の経路の ssh）は、空から
//     の初回は変える前に届かないので、印が無ければ一度も変えられない。印の
//     付いた先も変えたあとはほかの先と同じに必ず届くことを確かめ、届かなけれ
//     ば巻き戻す。変える前に確かめる先が 1 つも無い宣言は読み込みで断る（前の
//     確かめが何も守らなくなる）。self には付けられない（自身の管理の接続は
//     変える前に届いている）。
//   - observe:<provider>/<scope>/<id> [expect:{…}] と check:<name>（Backlog
//     #214、netconf と同じ書き方と口、internal/confirmafter）: 別のリソースの
//     観測（コアがその provider で観測用の資格・ふだんの範囲のまま読む）と、
//     名前で登録した確かめの関数。どちらもいつも変えたあとだけ確かめる
//     （after: は書いても書かなくても同じ）。到達の先（self・ssh://・tcp://）
//     に届いてから、同じ時間の上限（巻き戻しの上限の半分）の中で通るまで
//     繰り返し、通らなければ仕掛けを取り消さず元の規則に戻す。「全部に after:
//     の印」の決まりは到達の先だけで数え、到達の先が 1 つも無い宣言は読み込み
//     で断る（締め出しの守りを弱めない。observe: と check: は管理の経路が
//     開いていることを確かめない）。

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/tjst-t/sakutei/v2/internal/confirmafter"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// FieldConfirm は base chain の宣言の確かめる先の一覧のフィールド。
const FieldConfirm = "confirm"

// 確かめる先の種類。
const (
	ConfirmSelf = "self"
	ConfirmSSH  = "ssh"
	ConfirmTCP  = "tcp"
	// ConfirmObserve・ConfirmCheck は変えたあとだけ確かめる先（Backlog #214）。
	ConfirmObserve = confirmafter.Observe
	ConfirmCheck   = confirmafter.Check
)

// ConfirmTarget は到達を確かめる 1 つの先。
type ConfirmTarget struct {
	// Kind は ConfirmSelf・ConfirmSSH・ConfirmTCP。
	Kind string
	// User・Host・Port は ssh・tcp の先（ssh の User・Port は空・0 ならその
	// ホストの設定のまま）。
	User string
	Host string
	Port int
	// ID・Expect は observe: の先と期待する欄の値、Check は check: の関数の
	// 名前（Backlog #214）。
	ID     core.ID
	Expect map[string]string
	Check  string
	// After は変えたあとだけ確かめる印（宣言の `after:`、Backlog #192）。
	// observe: と check: はいつも真。
	After bool
}

// reach は到達の先（self・ssh・tcp）かどうか。
func (t ConfirmTarget) reach() bool { return t.Kind != ConfirmObserve && t.Kind != ConfirmCheck }

// afterTarget は observe: と check: の先を共通の口の形にする。
func (t ConfirmTarget) afterTarget() confirmafter.Target {
	return confirmafter.Target{Kind: t.Kind, ID: t.ID, Expect: t.Expect, Check: t.Check}
}

// ConfirmAfterPrefix は変えたあとだけ確かめる先の印。
const ConfirmAfterPrefix = "after:"

// label は知らせに出す短い形（observe: の expect の値は出さない）。
func (t ConfirmTarget) label() string {
	if !t.reach() {
		return t.afterTarget().Label()
	}
	return t.String()
}

// String は宣言に書く形（"self"・"ssh://admin@192.0.2.10:2222"・
// "after:tcp://192.0.2.10:443"・"check:route-seen"。observe: と check: には
// after: を付けない）。
func (t ConfirmTarget) String() string {
	if !t.reach() {
		return t.afterTarget().String()
	}
	if t.After {
		u := t
		u.After = false
		return ConfirmAfterPrefix + u.String()
	}
	if t.Kind == ConfirmSelf {
		return ConfirmSelf
	}
	host := t.Host
	if t.Port != 0 {
		host = net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if t.User != "" {
		host = t.User + "@" + host
	}
	return t.Kind + "://" + host
}

// SelfTarget は書かなかったときの既定（Sakutei 自身の管理の接続）。
var SelfTarget = ConfirmTarget{Kind: ConfirmSelf}

// parseConfirm は confirm の宣言を読む。空なら nil（既定の self）。
func parseConfirm(s string) ([]ConfirmTarget, error) {
	rest := strings.TrimSpace(s)
	if rest == "" {
		return nil, nil
	}
	var out []ConfirmTarget
	seen := map[string]bool{}
	for {
		rest = strings.TrimLeft(rest, " \t\r\n")
		after := strings.HasPrefix(rest, ConfirmAfterPrefix)
		rest = strings.TrimLeft(strings.TrimPrefix(rest, ConfirmAfterPrefix), " \t\r\n")
		at, r, ok, err := confirmafter.Parse(rest, nextTarget)
		if err != nil {
			return nil, err
		}
		var t ConfirmTarget
		if ok {
			// Always confirmed after the change, marked or not.
			t = ConfirmTarget{Kind: at.Kind, ID: at.ID, Expect: at.Expect, Check: at.Check, After: true}
			rest = r
		} else {
			item := rest
			if k := strings.IndexByte(rest, ','); k >= 0 {
				item, rest = rest[:k], rest[k:]
			} else {
				rest = ""
			}
			item = strings.TrimSpace(item)
			if item == "" {
				return nil, fmt.Errorf("%q: an empty entry (want self, ssh://[user@]host[:port] or tcp://host:port, each optionally marked after:, or observe:<provider>/<scope>/<id> [expect:{…}] or check:<name>, separated by commas)", s)
			}
			if t, err = parseConfirmTarget(item); err != nil {
				return nil, err
			}
			if after && t.Kind == ConfirmSelf {
				return nil, fmt.Errorf("%q: self is Sakutei's own management connection and is always confirmed before the change; mark only another path's target after:", ConfirmAfterPrefix+item)
			}
			t.After = after
		}
		key := strings.TrimPrefix(t.String(), ConfirmAfterPrefix) // marked or not, the same target
		if seen[key] {
			return nil, fmt.Errorf("%q is listed twice", t.label())
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
	reach := reachTargets(out)
	if len(reach) == 0 {
		return nil, fmt.Errorf("%q: no target is reached: observe: and check: are confirmed only after the change and do not show the management path open; write self (or ssh://, tcp://) too", s)
	}
	if len(beforeTargets(reach)) == 0 {
		return nil, fmt.Errorf("%q: every target is marked after:, so nothing is confirmed before the change; keep self (or another target reached now) unmarked", s)
	}
	return out, nil
}

// nextTarget は s（「,」の後ろ）が次の確かめる先で始まるか（空も）。
func nextTarget(s string) bool {
	if s == "" || confirmafter.Starts(s) || strings.HasPrefix(s, ConfirmAfterPrefix) {
		return true
	}
	for _, p := range []string{ConfirmSSH + "://", ConfirmTCP + "://"} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	tok, _ := confirmafter.Token(s)
	return tok == ConfirmSelf
}

// reachTargets は到達の先（self・ssh・tcp）。
func reachTargets(ts []ConfirmTarget) []ConfirmTarget {
	var out []ConfirmTarget
	for _, t := range ts {
		if t.reach() {
			out = append(out, t)
		}
	}
	return out
}

// afterItems は observe: と check: の先（Of は check: の関数に渡す変えたリソース）。
func afterItems(ts []ConfirmTarget, of provider.ConfirmTarget) []confirmafter.Item {
	var out []confirmafter.Item
	for _, t := range ts {
		if !t.reach() {
			out = append(out, confirmafter.Item{Target: t.afterTarget(), Of: of})
		}
	}
	return out
}

// beforeTargets は変える前にも確かめる先（after: の印の無い先）。
func beforeTargets(ts []ConfirmTarget) []ConfirmTarget {
	var out []ConfirmTarget
	for _, t := range ts {
		if !t.After {
			out = append(out, t)
		}
	}
	return out
}

func parseConfirmTarget(item string) (ConfirmTarget, error) {
	if item == ConfirmSelf {
		return SelfTarget, nil
	}
	u, err := url.Parse(item)
	if err != nil || (u.Scheme != ConfirmSSH && u.Scheme != ConfirmTCP) {
		return ConfirmTarget{}, fmt.Errorf("%q: want self, ssh://[user@]host[:port] or tcp://host:port", item)
	}
	if u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Hostname() == "" {
		return ConfirmTarget{}, fmt.Errorf("%q: want ssh://[user@]host[:port] or tcp://host:port and nothing else", item)
	}
	t := ConfirmTarget{Kind: u.Scheme, Host: u.Hostname()}
	if strings.HasPrefix(t.Host, "-") || strings.ContainsAny(t.Host, " \t\r\n@") {
		return ConfirmTarget{}, fmt.Errorf("%q: host %q", item, t.Host)
	}
	if ps := u.Port(); ps != "" {
		n, err := strconv.Atoi(ps)
		if err != nil || n < 1 || n > 65535 {
			return ConfirmTarget{}, fmt.Errorf("%q: port %q", item, ps)
		}
		t.Port = n
	}
	if u.User != nil {
		if _, has := u.User.Password(); has || u.Scheme != ConfirmSSH {
			return ConfirmTarget{}, fmt.Errorf("%q: only an ssh target takes a user, and never a password", item)
		}
		t.User = u.User.Username()
		if t.User == "" || strings.HasPrefix(t.User, "-") || strings.ContainsAny(t.User, " \t\r\n@") {
			return ConfirmTarget{}, fmt.Errorf("%q: user %q", item, t.User)
		}
	}
	if t.Kind == ConfirmTCP && t.Port == 0 {
		return ConfirmTarget{}, fmt.Errorf("%q: a tcp target needs a port", item)
	}
	return t, nil
}

// validateConfirm は読み込みの時点の confirm の検査: base chain の宣言だけに
// 書け、形が読めること。local（手元の live）なら ssh の先は書けない。
// observe: の先と check: の名前は c（読み込んだ intent の Confirmer）に確かめる
// （Backlog #214。I/O はしない）。
func validateConfirm(fields map[string]string, id string, local bool, c provider.Confirmer) error {
	v := fields[FieldConfirm]
	if v == "" {
		return nil
	}
	if !declaresBase(fields) {
		return fmt.Errorf("%s: field %s is for a base chain declaration (type, hook, priority, policy): a regular chain is changed without the lockout guard", id, FieldConfirm)
	}
	ts, err := parseConfirm(v)
	if err != nil {
		return fmt.Errorf("%s: field %s: %w", id, FieldConfirm, err)
	}
	var after []confirmafter.Target
	for _, t := range ts {
		if !t.reach() {
			after = append(after, t.afterTarget())
			continue
		}
		if local && t.Kind == ConfirmSSH {
			return fmt.Errorf("%s: field %s: %s: the local nft has no ssh settings to reach another path; use tcp://host:port", id, FieldConfirm, t)
		}
	}
	if err := confirmafter.Validate(c, ProviderName, id, after); err != nil {
		return fmt.Errorf("%s: field %s: %w", id, FieldConfirm, err)
	}
	return nil
}

// ErrCannotConfirm は確かめの手立てが無いこと（手元で self の宛先を設定して
// いない・ssh の設定の無い先へ ssh で確かめる）。直すのは設定か宣言（PermanentError）。
var ErrCannotConfirm = errors.New("no way to confirm reaching it")

// mergeTargets は ts の到達の先を out に足す（同じ先は 1 つ。片方の宣言にだけ
// after: の印があれば、印の無い方（変える前にも確かめる）に寄せる）。observe:
// と check: は足さない（afterItems）。
func mergeTargets(out, ts []ConfirmTarget) []ConfirmTarget {
	for _, t := range ts {
		if !t.reach() {
			continue
		}
		dup := false
		for i, o := range out {
			if o.Kind == t.Kind && o.User == t.User && o.Host == t.Host && o.Port == t.Port {
				out[i].After = o.After && t.After
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, t)
		}
	}
	return out
}

// mergeItems は items を out に足す（同じ先を同じリソースが 2 度は書かない。
// 別の chain が同じ check: を書けば、それぞれの変更について確かめる）。
func mergeItems(out, items []confirmafter.Item) []confirmafter.Item {
	for _, it := range items {
		dup := false
		for _, o := range out {
			if o.Target.String() == it.Target.String() && o.Of.ID.Key() == it.Of.ID.Key() {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, it)
		}
	}
	return out
}

func targetsText(ts []ConfirmTarget) string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.String()
	}
	return strings.Join(out, ", ")
}

// afterHint は知らせに添える after: の印の説明（印の付いた先が無ければ ""）。
func afterHint(ts []ConfirmTarget) string {
	if len(beforeTargets(ts)) == len(ts) {
		return ""
	}
	return " (after: marks a target confirmed only after the change, not before it)"
}
