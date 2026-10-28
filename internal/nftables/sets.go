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

// sets.go — ルール文の中の無名の集合を比べるための正規形（Backlog #178）。
//
// 設計判断:
//
//   - nft は interval の集合の隣り合う・重なる範囲をまとめて書き戻す（例
//     `{ 10.0.0.0/24, 10.0.1.0/24 }` → `{ 10.0.0.0/23 }`、`{ 22, 23, 80-90 }` →
//     `{ 22-23, 80-90 }`）。要素が 1 つの集合は `{ }` を外して書き戻す。だから
//     宣言の形のままでは読み戻しと合わない。
//   - 比べる前に、ルール文の中の無名の集合 `{ ... }` の要素を範囲に展開し、
//     重なり・隣り合いを合わせて並べ替えた形（NormalizeSets）にする。宣言の側も
//     観測の側も同じ関数を通す（ruleKey。PlanRefiner と Apply の読み戻し）。
//     まとめた結果が 1 つの範囲なら `{ }` を外す（nft の 1 要素の集合と同じ）。
//   - 正規化するのは要素が全部「IPv4 のアドレス・範囲・CIDR」か「IPv6 の
//     アドレス・範囲・CIDR」か「数（ポート等）・数の範囲」のどれか 1 種の集合
//     だけ。名前（`{ established, related }`）・連結（`a . b`）・map/vmap
//     （`k : v`）・混ざったものは書いたままにする（比べ方は今までどおり）。
//   - 観測（state）は nft が書き戻した形のまま持つ（正規形は比べるときだけ）。
//     名前の付いた set・map は管理の外のまま（§29.1、Backlog #121）。

import (
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

// ruleKey はルール文を比べるための形: canonRule（空白・カウンタ）の上に
// 無名の集合の正規形（NormalizeSets）を重ねたもの。
func ruleKey(s string) string { return NormalizeSets(canonRule(s)) }

// rulesKey は RuleSep で join したルール列の各ルールを ruleKey にする。
func rulesKey(enc string) string {
	rules := splitRules(enc)
	for i, r := range rules {
		rules[i] = ruleKey(r)
	}
	return strings.Join(rules, RuleSep)
}

// NormalizeSets はルール文の中の無名の集合（アドレス・アドレスの範囲・CIDR・
// ポートとポートの範囲）を、要素を範囲に展開して重なり・隣り合いを合わせ、
// 並べ替えた形に書き直す（1 つの範囲にまとまれば `{ }` を外す）。それ以外の
// 集合と引用符の中はそのまま。結果の空白は 1 つにそろえる。nft が集合を
// まとめて書き戻しても宣言と同じ形になるので、ルールを作る側（網の間の許可の
// 変換、Backlog #174）も比べる側もこの形を使う。
func NormalizeSets(rule string) string {
	var b strings.Builder
	inQuote := false
	for i := 0; i < len(rule); i++ {
		c := rule[i]
		switch {
		case c == '"':
			inQuote = !inQuote
		case !inQuote && c == '{':
			end := strings.IndexAny(rule[i+1:], "{}\"")
			if end >= 0 && rule[i+1+end] == '}' {
				if repl, ok := normalizeSet(rule[i+1 : i+1+end]); ok {
					b.WriteString(" " + repl + " ")
					i += 1 + end
					continue
				}
			}
		}
		b.WriteByte(c)
	}
	return strings.Join(ruleTokens(b.String()), " ")
}

// interval は 1 つの範囲 [lo, hi]（両端を含む）。アドレスは netip.Addr、数は
// 下位 64 bit の IPv6 のアドレスとして持つ（比べ方と次の値を共通にする）。
type interval struct{ lo, hi netip.Addr }

// setKind は集合の要素の種類。
type setKind int

const (
	kindNone setKind = iota
	kindV4
	kindV6
	kindNum
)

// normalizeSet は集合の中身（`{` と `}` の間）を正規形にする。正規化できない
// 集合なら false。
func normalizeSet(body string) (string, bool) {
	kind := kindNone
	var ivs []interval
	for _, el := range strings.Split(body, ",") {
		el = strings.Join(strings.Fields(el), "")
		if el == "" {
			return "", false
		}
		k, iv, ok := parseElem(el)
		if !ok || (kind != kindNone && k != kind) {
			return "", false
		}
		kind = k
		ivs = append(ivs, iv)
	}
	if len(ivs) == 0 {
		return "", false
	}
	sort.Slice(ivs, func(i, j int) bool { return ivs[i].lo.Less(ivs[j].lo) })
	merged := []interval{ivs[0]}
	for _, iv := range ivs[1:] {
		last := &merged[len(merged)-1]
		// 重なる（lo <= hi）か隣り合う（lo == hi+1）なら合わせる。
		if iv.lo.Compare(last.hi) <= 0 || iv.lo == last.hi.Next() {
			if last.hi.Less(iv.hi) {
				last.hi = iv.hi
			}
			continue
		}
		merged = append(merged, iv)
	}
	out := make([]string, len(merged))
	for i, iv := range merged {
		out[i] = formatInterval(kind, iv)
	}
	if len(out) == 1 {
		return out[0], true
	}
	return "{ " + strings.Join(out, ", ") + " }", true
}

// parseElem は集合の 1 要素（空白を除いたもの）を範囲にする。
func parseElem(el string) (setKind, interval, bool) {
	if isDigits(el) {
		n, ok := numAddr(el)
		return kindNum, interval{n, n}, ok
	}
	if lo, hi, ok := strings.Cut(el, "-"); ok && isDigits(lo) && isDigits(hi) {
		a, ok1 := numAddr(lo)
		b, ok2 := numAddr(hi)
		if !ok1 || !ok2 || b.Less(a) {
			return kindNone, interval{}, false
		}
		return kindNum, interval{a, b}, true
	}
	if strings.Contains(el, "/") {
		p, err := netip.ParsePrefix(el)
		if err != nil {
			return kindNone, interval{}, false
		}
		p = p.Masked()
		return addrKind(p.Addr()), interval{p.Addr(), lastAddr(p)}, true
	}
	if lo, hi, ok := strings.Cut(el, "-"); ok {
		a, err1 := netip.ParseAddr(lo)
		b, err2 := netip.ParseAddr(hi)
		if err1 != nil || err2 != nil || a.Is4() != b.Is4() || a.Zone() != "" || b.Zone() != "" || b.Less(a) {
			return kindNone, interval{}, false
		}
		return addrKind(a), interval{a, b}, true
	}
	a, err := netip.ParseAddr(el)
	if err != nil || a.Zone() != "" {
		return kindNone, interval{}, false
	}
	return addrKind(a), interval{a, a}, true
}

func addrKind(a netip.Addr) setKind {
	if a.Is4() {
		return kindV4
	}
	return kindV6
}

// numAddr は 10 進の数を IPv6 のアドレスの下位 64 bit に置く。
func numAddr(s string) (netip.Addr, bool) {
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return netip.Addr{}, false
	}
	var b [16]byte
	for i := 0; i < 8; i++ {
		b[15-i] = byte(n >> (8 * i))
	}
	return netip.AddrFrom16(b), true
}

// addrNum は numAddr の逆。
func addrNum(a netip.Addr) string {
	b := a.As16()
	var n uint64
	for i := 8; i < 16; i++ {
		n = n<<8 | uint64(b[i])
	}
	return strconv.FormatUint(n, 10)
}

// lastAddr は prefix の最後のアドレス。
func lastAddr(p netip.Prefix) netip.Addr {
	a := p.Addr()
	if a.Is4() {
		b := a.As4()
		setHostBits(b[:], p.Bits())
		return netip.AddrFrom4(b)
	}
	b := a.As16()
	setHostBits(b[:], p.Bits())
	return netip.AddrFrom16(b)
}

func setHostBits(b []byte, bits int) {
	for i := range b {
		for j := 0; j < 8; j++ {
			if i*8+j >= bits {
				b[i] |= 0x80 >> j
			}
		}
	}
}

// formatInterval は範囲を書く: 数は "n" か "lo-hi"、アドレスは 1 つなら
// アドレス、CIDR にちょうど収まれば prefix、それ以外は "lo-hi"。
func formatInterval(kind setKind, iv interval) string {
	if kind == kindNum {
		if iv.lo == iv.hi {
			return addrNum(iv.lo)
		}
		return addrNum(iv.lo) + "-" + addrNum(iv.hi)
	}
	if iv.lo == iv.hi {
		return iv.lo.String()
	}
	for bits := 0; bits < iv.lo.BitLen(); bits++ {
		p := netip.PrefixFrom(iv.lo, bits)
		if p.Masked().Addr() == iv.lo && lastAddr(p) == iv.hi {
			return p.String()
		}
	}
	return iv.lo.String() + "-" + iv.hi.String()
}
