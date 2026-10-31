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

package dnszone

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
)

// rr はゾーンファイルの RR 1 つ。start〜end は行の番号（括弧の複数行）。
type rr struct {
	owner      string
	ttl        int64 // 実効の TTL（書いた TTL か $TTL。分からなければ -1）
	typ        string
	rdata      string // 正規形
	start, end int
	blankOwner bool
}

// zoneFile は読んだゾーンファイルと、書き換え（消す行・足す行）。
type zoneFile struct {
	zone     string
	lines    []string
	rrs      []rr
	removed  map[int]bool
	inserted map[int][]string // その行の前に足す（len(lines) は末尾）
	edits    map[int]string   // 行を置き換える（SOA のシリアル）
}

func readZone(path, zone string) (*zoneFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("ゾーンファイル %s が無い", path)
		}
		return nil, fmt.Errorf("ゾーンファイル %s を読めない: %v", path, err)
	}
	zf, err := parseZone(string(data), zone)
	if err != nil {
		return nil, fmt.Errorf("ゾーンファイル %s: %v", path, err)
	}
	return zf, nil
}

func parseZone(text, zone string) (*zoneFile, error) {
	text = strings.TrimSuffix(text, "\n")
	zf := &zoneFile{zone: zone, removed: map[int]bool{}, inserted: map[int][]string{}, edits: map[int]string{}}
	if text != "" {
		zf.lines = strings.Split(text, "\n")
	}
	origin, prev := zone, ""
	defTTL := int64(-1)
	for i := 0; i < len(zf.lines); i++ {
		start, raw := i, zf.lines[i]
		body := stripComment(raw)
		for depth(body) > 0 {
			if i+1 >= len(zf.lines) {
				return nil, fmt.Errorf("%d 行目: 括弧が閉じていない", start+1)
			}
			i++
			body += " " + stripComment(zf.lines[i])
		}
		if depth(body) < 0 {
			return nil, fmt.Errorf("%d 行目: 閉じる括弧が多い", start+1)
		}
		toks := tokens(strings.NewReplacer("(", " ", ")", " ").Replace(body))
		if len(toks) == 0 {
			continue
		}
		where := fmt.Sprintf("%d 行目", start+1)
		switch strings.ToUpper(toks[0]) {
		case "$ORIGIN":
			if len(toks) != 2 {
				return nil, fmt.Errorf("%s: $ORIGIN の形が違う", where)
			}
			origin = absName(toks[1], origin)
			continue
		case "$TTL":
			t, ok := parseTTL(toks[len(toks)-1])
			if len(toks) != 2 || !ok {
				return nil, fmt.Errorf("%s: $TTL の形が違う", where)
			}
			defTTL = t
			continue
		}
		if strings.HasPrefix(toks[0], "$") {
			return nil, fmt.Errorf("%s: %s は扱わない", where, toks[0])
		}
		r := rr{ttl: defTTL, start: start, end: i, blankOwner: raw[0] == ' ' || raw[0] == '\t'}
		if r.blankOwner {
			if prev == "" {
				return nil, fmt.Errorf("%s: owner を省いた最初の RR", where)
			}
			r.owner = prev
		} else {
			r.owner = absName(toks[0], origin)
			toks = toks[1:]
		}
		for n := 0; n < 2 && len(toks) > 0; n++ {
			if t, ok := parseTTL(toks[0]); ok {
				r.ttl = t
				toks = toks[1:]
				continue
			}
			switch strings.ToUpper(toks[0]) {
			case "IN":
				toks = toks[1:]
			case "CH", "HS", "CS":
				return nil, fmt.Errorf("%s: class %s は扱わない", where, toks[0])
			}
		}
		if len(toks) < 2 {
			return nil, fmt.Errorf("%s: 型と値が無い", where)
		}
		r.typ = strings.ToUpper(toks[0])
		rd, err := canonToks(r.typ, toks[1:], origin)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", where, err)
		}
		r.rdata = rd
		prev = r.owner
		zf.rrs = append(zf.rrs, r)
	}
	return zf, nil
}

// observe は name の typ の RRset の観測（正規形）。
func (zf *zoneFile) observe(name, typ string) core.Observation {
	var vals []string
	ttl, same := int64(-2), true
	for _, r := range zf.rrs {
		if r.owner != name || r.typ != typ {
			continue
		}
		vals = append(vals, r.rdata)
		if ttl == -2 {
			ttl = r.ttl
		} else if ttl != r.ttl {
			same = false
		}
	}
	if len(vals) == 0 {
		return core.Observation{Availability: core.Absent}
	}
	sort.Strings(vals)
	f := core.Fields{}
	l := make([]any, len(vals))
	for i, v := range vals {
		l[i] = v
	}
	f["values"] = l
	if len(vals) == 1 {
		f["value"] = vals[0]
	}
	if same && ttl >= 0 {
		f["ttl"] = ttl
	}
	return core.Observation{Availability: core.Present, Fields: f}
}

func (zf *zoneFile) ttlOf(name, typ string) int64 {
	if o := zf.observe(name, typ); o.Availability == core.Present {
		if t, ok := o.Fields["ttl"].(int64); ok {
			return t
		}
	}
	return -1
}

// replace は name の typ の RRset を lines で置き換える（nil なら消す）。新しい
// RRset は末尾に足す。
func (zf *zoneFile) replace(name, typ string, lines []string) {
	at := -1
	for _, r := range zf.rrs {
		if r.owner != name || r.typ != typ {
			continue
		}
		if at < 0 {
			at = r.start
		}
		for i := r.start; i <= r.end; i++ {
			zf.removed[i] = true
		}
	}
	if at < 0 {
		at = len(zf.lines)
	}
	zf.inserted[at] = append(zf.inserted[at], lines...)
}

// serialToken は SOA のシリアルの行・桁の始まりと終わり・値。
func (zf *zoneFile) serialToken() (line, from, to int, serial int64, err error) {
	for _, r := range zf.rrs {
		if r.typ != "SOA" || r.owner != zf.zone {
			continue
		}
		seen, after := false, 0
		for i := r.start; i <= r.end; i++ {
			text := zf.lines[i]
			if e, ok := zf.edits[i]; ok {
				text = e
			}
			body := stripComment(text)
			for _, t := range positions(body) {
				w := body[t[0]:t[1]]
				if !seen {
					seen = strings.EqualFold(w, "SOA")
					continue
				}
				after++
				if after == 3 {
					n, ok := atoi(w)
					if !ok {
						return 0, 0, 0, 0, fmt.Errorf("SOA のシリアル %q が数でない", w)
					}
					return i, t[0], t[1], n, nil
				}
			}
		}
		return 0, 0, 0, 0, errors.New("SOA のシリアルが読めない")
	}
	return 0, 0, 0, 0, fmt.Errorf("ゾーン %s の SOA が無い", zf.zone)
}

// bumpSerial は SOA のシリアルを上げる（E-B1）。YYYYMMDDnn の形なら今日の 00 か
// +1 の大きいほう。
func (zf *zoneFile) bumpSerial(now time.Time) error {
	line, from, to, serial, err := zf.serialToken()
	if err != nil {
		return err
	}
	next := serial + 1
	if serial >= 1970010100 && serial <= 9999123199 {
		day, _ := strconv.ParseInt(now.UTC().Format("20060102"), 10, 64)
		if day*100 > next {
			next = day * 100
		}
	}
	if next > 4294967295 {
		return fmt.Errorf("SOA のシリアル %d を上げられない", serial)
	}
	text := zf.lines[line]
	if e, ok := zf.edits[line]; ok {
		text = e
	}
	zf.edits[line] = text[:from] + strconv.FormatInt(next, 10) + text[to:]
	return nil
}

// render は書き換えたゾーンファイル。owner を省いた行の owner が、消した行の
// せいで変わるなら owner を書き足す。
func (zf *zoneFile) render() string {
	starts := map[int]rr{}
	for _, r := range zf.rrs {
		starts[r.start] = r
	}
	var b strings.Builder
	last := ""
	emitNew := func(i int) {
		for _, l := range zf.inserted[i] {
			b.WriteString(l + "\n")
			last = strings.Fields(l)[0]
		}
	}
	for i, l := range zf.lines {
		emitNew(i)
		if zf.removed[i] {
			continue
		}
		if e, ok := zf.edits[i]; ok {
			l = e
		}
		if r, ok := starts[i]; ok {
			if r.blankOwner && r.owner != last {
				l = r.owner + l
			}
			last = r.owner
		}
		b.WriteString(l + "\n")
	}
	emitNew(len(zf.lines))
	return b.String()
}

// canonRdata は宣言の値を観測と同じ正規形にする（E-I7）。
func canonRdata(typ, v, origin string) (string, error) {
	if strings.ContainsAny(v, "\n\r") {
		return "", errors.New("1 行で書く")
	}
	if strings.ContainsAny(stripComment(v), "()") || stripComment(v) != v {
		return "", errors.New("括弧と ; のコメントは書けない")
	}
	toks := tokens(v)
	if len(toks) == 0 {
		return "", errors.New("空の値")
	}
	return canonToks(typ, toks, origin)
}

// canonToks は値の語を正規形にする。名前を持つ型の名前は小文字の FQDN。
func canonToks(typ string, toks []string, origin string) (string, error) {
	out := append([]string(nil), toks...)
	names := map[string][]int{"NS": {0}, "CNAME": {0}, "PTR": {0}, "MX": {1}, "SRV": {3}}
	want := map[string]int{"NS": 1, "CNAME": 1, "PTR": 1, "MX": 2, "SRV": 4, "A": 1, "AAAA": 1}
	if n, ok := want[typ]; ok && len(out) != n {
		return "", fmt.Errorf("%s の値は %d 語（%d 語ある）", typ, n, len(out))
	}
	for _, i := range names[typ] {
		out[i] = absName(out[i], origin)
	}
	if typ == "AAAA" {
		out[0] = strings.ToLower(out[0])
	}
	return strings.Join(out, " "), nil
}

func absName(s, origin string) string {
	switch {
	case s == "@":
		return origin
	case strings.HasSuffix(s, "."):
		return strings.ToLower(s)
	}
	return strings.ToLower(s + "." + origin)
}

func parseTTL(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	if n, ok := atoi(s); ok {
		return n, n >= 0
	}
	var total, cur int64
	seenDigit := false
	for _, c := range strings.ToLower(s) {
		switch {
		case c >= '0' && c <= '9':
			cur = cur*10 + int64(c-'0')
			seenDigit = true
		case strings.ContainsRune("smhdw", c) && seenDigit:
			mult := map[rune]int64{'s': 1, 'm': 60, 'h': 3600, 'd': 86400, 'w': 604800}[c]
			total += cur * mult
			cur, seenDigit = 0, false
		default:
			return 0, false
		}
	}
	if seenDigit {
		return 0, false
	}
	return total, true
}

// stripComment は引用符の外の ; から後を落とす。
func stripComment(s string) string {
	in := false
	for i, c := range s {
		switch {
		case c == '"':
			in = !in
		case c == ';' && !in:
			return s[:i]
		}
	}
	return s
}

func depth(s string) int {
	d, in := 0, false
	for _, c := range s {
		switch {
		case c == '"':
			in = !in
		case in:
		case c == '(':
			d++
		case c == ')':
			d--
		}
	}
	return d
}

// tokens は引用符の外の空白で区切る。
func tokens(s string) []string {
	var out []string
	for _, p := range positions(s) {
		out = append(out, s[p[0]:p[1]])
	}
	return out
}

// positions は語の始まりと終わり（引用符の外の空白で区切り、括弧は 1 語）。
func positions(s string) [][2]int {
	var out [][2]int
	start, in := -1, false
	for i, c := range s {
		switch {
		case c == '"':
			if start < 0 {
				start = i
			}
			in = !in
		case in:
		case c == ' ' || c == '\t':
			if start >= 0 {
				out = append(out, [2]int{start, i})
				start = -1
			}
		case c == '(' || c == ')':
			if start >= 0 {
				out = append(out, [2]int{start, i})
				start = -1
			}
		default:
			if start < 0 {
				start = i
			}
		}
	}
	if start >= 0 {
		out = append(out, [2]int{start, len(s)})
	}
	return out
}
