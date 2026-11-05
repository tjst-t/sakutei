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

// rules.go — ルール文・base chain の値の正規形と検査（E-B2・E-I7）。宣言と観測に
// 同じ関数を使う。

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	families = map[string]bool{"ip": true, "ip6": true, "inet": true, "arp": true, "bridge": true, "netdev": true}
	identRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)
	types    = map[string]bool{"filter": true, "nat": true, "route": true}
	hooks    = map[string]bool{"prerouting": true, "input": true, "forward": true, "output": true, "postrouting": true}
	policies = map[string]bool{"accept": true, "drop": true}
)

// priorityNames は nft の priority の名前（nft(8)）。ファイルを読むときに数値に直す。
var priorityNames = map[string]int64{"raw": -300, "mangle": -150, "dstnat": -100, "filter": 0, "security": 50, "srcnat": 100}

// parsePriority は "0"・"-150"・"filter"・"filter + 10"・"mangle - 5" を数値にする。
func parsePriority(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	f := strings.Fields(s)
	base, ok := priorityNames[strings.ToLower(f[0])]
	if !ok {
		return 0, fmt.Errorf("priority %q を読めない", s)
	}
	switch {
	case len(f) == 1:
		return base, nil
	case len(f) == 3 && (f[1] == "+" || f[1] == "-"):
		n, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("priority %q を読めない", s)
		}
		if f[1] == "-" {
			n = -n
		}
		return base + n, nil
	}
	return 0, fmt.Errorf("priority %q を読めない", s)
}

// tokens は引用符の外の空白で区切る。
func tokens(s string) []string {
	var out []string
	var cur strings.Builder
	in := false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, c := range s {
		switch {
		case c == '"':
			in = !in
			cur.WriteRune(c)
		case !in && (c == ' ' || c == '\t'):
			flush()
		default:
			cur.WriteRune(c)
		}
	}
	flush()
	return out
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// canonRule はルール文の正規形：引用符の外の空白を 1 つにそろえ、変わり続ける
// counter の packets・bytes と quota の used を落とす（宣言の一部でない）。
func canonRule(s string) string {
	toks := tokens(s)
	out := make([]string, 0, len(toks))
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t == "counter" && i+4 < len(toks) && toks[i+1] == "packets" && isDigits(toks[i+2]) && toks[i+3] == "bytes" && isDigits(toks[i+4]) {
			out = append(out, "counter")
			i += 4
			continue
		}
		if t == "used" && i+2 < len(toks) && isDigits(toks[i+1]) && afterQuota(out) {
			i += 2
			continue
		}
		out = append(out, t)
	}
	return strings.Join(out, " ")
}

func afterQuota(out []string) bool {
	for i := len(out) - 1; i >= 0 && i >= len(out)-4; i-- {
		if out[i] == "quota" {
			return true
		}
	}
	return false
}

// checkRule は nft のスクリプトの 1 行に埋め込めるルール文かを確かめる（専用ツールの
// 検査と同じ：引用符の外の ; # $ \ と改行・閉じていない引用符と括弧を受け付けない）。
func checkRule(r string) error {
	if strings.TrimSpace(r) == "" {
		return errors.New("空のルール")
	}
	in, d := false, 0
	for _, c := range r {
		switch {
		case c == '\n' || c == '\r' || (c < 0x20 && c != '\t'):
			return errors.New("ルールは 1 行で書く")
		case c == '\\':
			return errors.New("ルールに \\ は書けない")
		case c == '"':
			in = !in
		case in:
		case c == ';':
			return errors.New("引用符の外の ; は書けない（次の命令になる）")
		case c == '#':
			return errors.New("引用符の外の # は書けない（コメントになる）")
		case c == '$':
			return errors.New("引用符の外の $ は書けない（変数は使わない）")
		case c == '{':
			d++
		case c == '}':
			d--
			if d < 0 {
				return errors.New("閉じる } が多い")
			}
		}
	}
	if in {
		return errors.New("引用符が閉じていない")
	}
	if d != 0 {
		return errors.New("{ が閉じていない")
	}
	return nil
}

// baseDef は base chain の type・hook・priority・policy。
type baseDef struct {
	typ, hook, policy string
	priority          int64
}

// spec は add chain の { } の中身（専用ツールが受け付ける形）。
func (d baseDef) spec() string {
	return fmt.Sprintf("{ type %s hook %s priority %d ; policy %s ; }", d.typ, d.hook, d.priority, d.policy)
}

func (d baseDef) sameHook(o baseDef) bool {
	return d.typ == o.typ && d.hook == o.hook && d.priority == o.priority
}
