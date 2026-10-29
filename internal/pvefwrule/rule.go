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

// Package pvefwrule は Proxmox VE の VM の FW の規則（/nodes/<n>/qemu/<id>/
// firewall/rules の 1 行）の文の形（Backlog #197）。proxmox-fw provider
// （internal/proxmox）と、網の間の許可の実現先 proxmox-fw（pkg/netpolicy）が
// 共に使う葉の package。
//
// 1 つの規則の文:
//
//	<in|out> <ACCEPT|DROP|REJECT> [iface=netN] [source=..] [dest=..] [proto=..]
//	  [dport=..] [sport=..] [icmp-type=..] [macro=..] [log=..] [enable=0] [comment=<文>]
//
// キーはこの順に並べたものが正規形（String）。comment は最後に置き、行の
// 終わりまでがその値（空白を含めてよい）。規則の並びは Sep（" ; "）でつなぐ。
// comment の ";" は Comment が "," に変える（区切りを作らない）。
//
// Sakutei が置いた規則は、Proxmox の comment の頭に印（Marker）を付けて見分ける
// （Mark・Unmark）。宣言の文の comment には印を書かない（provider が付け外す）。
package pvefwrule

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Sep は規則の並びの区切り。
const Sep = " ; "

// Marker は Sakutei が置いた規則の comment の頭の印（"sakutei" か
// "sakutei: <comment>"）。
const Marker = "sakutei"

// 規則の向き（type）。
const (
	TypeIn  = "in"
	TypeOut = "out"
)

// Rule は 1 つの規則。空の欄は書かない（Proxmox に渡さない）。
type Rule struct {
	Type     string
	Action   string
	Iface    string
	Source   string
	Dest     string
	Proto    string
	Dport    string
	Sport    string
	ICMPType string
	Macro    string
	Log      string
	// Disabled は enable=0（既定は効いている規則）。
	Disabled bool
	// Comment は印を除いた comment。
	Comment string
}

// keys は文のキーの正規の順（comment を除く）。
var keys = []string{"iface", "source", "dest", "proto", "dport", "sport", "icmp-type", "macro", "log", "enable"}

var (
	ifaceRe = regexp.MustCompile(`^net[0-9]+$`)
	addrRe  = regexp.MustCompile(`^[A-Za-z0-9+_.:/,-]+$`)
	protoRe = regexp.MustCompile(`^[a-z0-9-]+$`)
	portRe  = regexp.MustCompile(`^[A-Za-z0-9:,-]+$`)
	icmpRe  = regexp.MustCompile(`^[a-z0-9-]+$`)
	macroRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	logs    = map[string]bool{"emerg": true, "alert": true, "crit": true, "err": true, "warning": true, "notice": true, "info": true, "debug": true, "nolog": true}
)

func (r *Rule) field(k string) *string {
	switch k {
	case "iface":
		return &r.Iface
	case "source":
		return &r.Source
	case "dest":
		return &r.Dest
	case "proto":
		return &r.Proto
	case "dport":
		return &r.Dport
	case "sport":
		return &r.Sport
	case "icmp-type":
		return &r.ICMPType
	case "macro":
		return &r.Macro
	case "log":
		return &r.Log
	}
	return nil
}

// Parse は 1 つの規則の文を読む（向きと action は大文字・小文字を問わない）。
func Parse(s string) (Rule, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Rule{}, fmt.Errorf("an empty rule")
	}
	if strings.ContainsAny(s, "\n\r") {
		return Rule{}, fmt.Errorf("rule %q: one rule per line", s)
	}
	var r Rule
	head, comment, hasComment := strings.Cut(s, "comment=")
	if hasComment {
		if head != "" && !strings.HasSuffix(head, " ") {
			return Rule{}, fmt.Errorf("rule %q: comment= must be a key of its own", s)
		}
		r.Comment = strings.TrimSpace(comment)
		if strings.Contains(r.Comment, ";") {
			return Rule{}, fmt.Errorf("rule %q: a comment cannot hold ';' (it separates rules)", s)
		}
	}
	toks := strings.Fields(head)
	if len(toks) < 2 {
		return Rule{}, fmt.Errorf("rule %q: want <in|out> <ACCEPT|DROP|REJECT> [key=value ...]", s)
	}
	r.Type, r.Action = strings.ToLower(toks[0]), strings.ToUpper(toks[1])
	seen := map[string]bool{}
	for _, tok := range toks[2:] {
		k, v, ok := strings.Cut(tok, "=")
		if !ok || v == "" {
			return Rule{}, fmt.Errorf("rule %q: %q: want key=value", s, tok)
		}
		if seen[k] {
			return Rule{}, fmt.Errorf("rule %q: %s is written twice", s, k)
		}
		seen[k] = true
		if k == "enable" {
			switch v {
			case "0":
				r.Disabled = true
			case "1":
			default:
				return Rule{}, fmt.Errorf("rule %q: enable=%s: want 0 or 1", s, v)
			}
			continue
		}
		p := r.field(k)
		if p == nil {
			return Rule{}, fmt.Errorf("rule %q: unknown key %q (want %s or comment)", s, k, strings.Join(keys, ", "))
		}
		*p = v
	}
	if err := r.Validate(); err != nil {
		return Rule{}, fmt.Errorf("rule %q: %w", s, err)
	}
	return r, nil
}

// Validate は値の形を確かめる。
func (r Rule) Validate() error {
	switch r.Type {
	case TypeIn, TypeOut:
	default:
		return fmt.Errorf("type %q: want in or out", r.Type)
	}
	switch r.Action {
	case "ACCEPT", "DROP", "REJECT":
	default:
		return fmt.Errorf("action %q: want ACCEPT, DROP or REJECT", r.Action)
	}
	checks := []struct {
		k, v string
		re   *regexp.Regexp
	}{
		{"iface", r.Iface, ifaceRe}, {"source", r.Source, addrRe}, {"dest", r.Dest, addrRe},
		{"proto", r.Proto, protoRe}, {"dport", r.Dport, portRe}, {"sport", r.Sport, portRe},
		{"icmp-type", r.ICMPType, icmpRe}, {"macro", r.Macro, macroRe},
	}
	for _, c := range checks {
		if c.v != "" && !c.re.MatchString(c.v) {
			return fmt.Errorf("%s=%s: not a valid value", c.k, c.v)
		}
	}
	if r.Log != "" && !logs[r.Log] {
		return fmt.Errorf("log=%s: want emerg, alert, crit, err, warning, notice, info, debug or nolog", r.Log)
	}
	if (r.Dport != "" || r.Sport != "") && r.Proto == "" && r.Macro == "" {
		return fmt.Errorf("dport and sport need proto")
	}
	if strings.ContainsAny(r.Comment, "\n\r;") {
		return fmt.Errorf("comment %q: no newline and no ';'", r.Comment)
	}
	return nil
}

// String は正規形の文。
func (r Rule) String() string {
	parts := []string{r.Type, r.Action}
	for _, k := range keys {
		if k == "enable" {
			if r.Disabled {
				parts = append(parts, "enable=0")
			}
			continue
		}
		if v := *r.field(k); v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	if r.Comment != "" {
		parts = append(parts, "comment="+r.Comment)
	}
	return strings.Join(parts, " ")
}

// ParseList は Sep でつないだ規則の並びを読む（空の文字列は規則 0 個）。
func ParseList(s string) ([]Rule, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []Rule
	for i, part := range strings.Split(s, Sep) {
		r, err := Parse(part)
		if err != nil {
			return nil, fmt.Errorf("rules[%d]: %w", i, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// Join は規則の並びを正規形の文にする。
func Join(rules []Rule) string {
	parts := make([]string, len(rules))
	for i, r := range rules {
		parts[i] = r.String()
	}
	return strings.Join(parts, Sep)
}

// Canonical は規則の並びの文を正規形にする。
func Canonical(s string) (string, error) {
	rules, err := ParseList(s)
	if err != nil {
		return "", err
	}
	return Join(rules), nil
}

// CommentMax は comment に入れる長さの上限（バイト、印を除く）。
const CommentMax = 200

// Comment は文（reason など）を規則の comment に入る形にする: 改行・タブ・
// 続く空白は 1 つの空白に、";" は ","（規則の区切りを作らない）に、CommentMax
// バイトを超えれば文字の境目で切って "…" を付ける。
func Comment(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case r == ';':
			r = ','
		case unicode.IsSpace(r) || unicode.IsControl(r) || r == utf8.RuneError:
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	out := b.String()
	if len(out) <= CommentMax {
		return out
	}
	const ell = "…"
	cut := CommentMax - len(ell)
	for cut > 0 && !utf8.RuneStart(out[cut]) {
		cut--
	}
	return strings.TrimRight(out[:cut], " ") + ell
}

// Mark は印を付けた Proxmox の comment。
func Mark(comment string) string {
	if comment == "" {
		return Marker
	}
	return Marker + ": " + comment
}

// Unmark は Proxmox の comment から印を外す。印が無ければ false。
func Unmark(comment string) (string, bool) {
	comment = strings.TrimSpace(comment)
	if comment == Marker {
		return "", true
	}
	if rest, ok := strings.CutPrefix(comment, Marker+":"); ok {
		return strings.TrimSpace(rest), true
	}
	return comment, false
}

// Params は規則を Proxmox の API の引数にする（comment には印を付ける。
// enable はいつも渡す。作るときの既定は 0 のため）。
func (r Rule) Params() url.Values {
	v := url.Values{"type": {r.Type}, "action": {r.Action}, "comment": {Mark(r.Comment)}}
	for _, k := range keys {
		if k == "enable" {
			if r.Disabled {
				v.Set("enable", "0")
			} else {
				v.Set("enable", "1")
			}
			continue
		}
		if f := *r.field(k); f != "" {
			v.Set(k, f)
		}
	}
	return v
}

// Unset は r が書かず、was が書いている欄（書き換えで消すもの。Proxmox の
// PUT の delete に渡す名前）。
func (r Rule) Unset(was Rule) []string {
	var out []string
	for _, k := range keys {
		if k == "enable" {
			continue
		}
		if *r.field(k) == "" && *was.field(k) != "" {
			out = append(out, k)
		}
	}
	return out
}

// FromAPI は GET の 1 つの項目（キー → 文字列）を規則にする。marked は印の
// 有無（comment から外す）。
func FromAPI(m map[string]string) (r Rule, marked bool) {
	r = Rule{Type: m["type"], Action: m["action"]}
	for _, k := range keys {
		if k == "enable" {
			r.Disabled = m["enable"] != "1"
			continue
		}
		*r.field(k) = m[k]
	}
	r.Comment, marked = Unmark(m["comment"])
	r.Comment = Comment(r.Comment)
	return r, marked
}
