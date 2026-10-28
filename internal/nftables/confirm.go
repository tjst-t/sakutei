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

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// FieldConfirm は base chain の宣言の確かめる先の一覧のフィールド。
const FieldConfirm = "confirm"

// 確かめる先の種類。
const (
	ConfirmSelf = "self"
	ConfirmSSH  = "ssh"
	ConfirmTCP  = "tcp"
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
}

// String は宣言に書く形（"self"・"ssh://admin@192.0.2.10:2222"・"tcp://192.0.2.10:443"）。
func (t ConfirmTarget) String() string {
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
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []ConfirmTarget
	seen := map[string]bool{}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			return nil, fmt.Errorf("%q: an empty entry (want self, ssh://[user@]host[:port] or tcp://host:port, separated by commas)", s)
		}
		t, err := parseConfirmTarget(item)
		if err != nil {
			return nil, err
		}
		if seen[t.String()] {
			return nil, fmt.Errorf("%q is listed twice", item)
		}
		seen[t.String()] = true
		out = append(out, t)
	}
	return out, nil
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
func validateConfirm(fields map[string]string, id string, local bool) error {
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
	if local {
		for _, t := range ts {
			if t.Kind == ConfirmSSH {
				return fmt.Errorf("%s: field %s: %s: the local nft has no ssh settings to reach another path; use tcp://host:port", id, FieldConfirm, t)
			}
		}
	}
	return nil
}

// ErrCannotConfirm は確かめの手立てが無いこと（手元で self の宛先を設定して
// いない・ssh の設定の無い先へ ssh で確かめる）。直すのは設定か宣言（PermanentError）。
var ErrCannotConfirm = errors.New("no way to confirm reaching it")

// mergeTargets は ts を out に足す（同じ先は 1 つ）。
func mergeTargets(out, ts []ConfirmTarget) []ConfirmTarget {
	for _, t := range ts {
		dup := false
		for _, o := range out {
			if o == t {
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

func targetsText(ts []ConfirmTarget) string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.String()
	}
	return strings.Join(out, ", ")
}
