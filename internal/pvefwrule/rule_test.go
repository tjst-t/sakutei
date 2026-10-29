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

package pvefwrule

import (
	"strings"
	"testing"
)

func TestParseAndString(t *testing.T) {
	cases := map[string]string{
		"in ACCEPT iface=net1 source=172.16.1.0/24 dest=172.16.0.11 proto=tcp dport=443 comment=サービス側は 443 だけ": "",
		"OUT accept   proto=udp iface=net0  dport=53,123 comment=  DNS  と NTP ":                                "out ACCEPT iface=net0 proto=udp dport=53,123 comment=DNS と NTP",
		"in DROP enable=0":                      "",
		"in ACCEPT enable=1 macro=SSH log=info": "in ACCEPT macro=SSH log=info",
	}
	for in, want := range cases {
		r, err := Parse(in)
		if err != nil {
			t.Errorf("Parse(%q): %v", in, err)
			continue
		}
		if want == "" {
			want = in
		}
		if got := r.String(); got != want {
			t.Errorf("Parse(%q).String() = %q, want %q", in, got, want)
		}
		again, err := Parse(r.String())
		if err != nil || again != r {
			t.Errorf("round trip of %q: %+v, %v", in, again, err)
		}
	}
	for _, bad := range []string{"", "in", "forward ACCEPT", "in ALLOW", "in ACCEPT iface=eth0", "in ACCEPT dport=22",
		"in ACCEPT proto=tcp proto=udp", "in ACCEPT foo=1", "in ACCEPT enable=2", "in ACCEPT log=loud", "in ACCEPT source=10.0.0.0/8;x",
		"in ACCEPTcomment=x", "in ACCEPT dport"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}

func TestParseList(t *testing.T) {
	rules, err := ParseList("in ACCEPT proto=tcp dport=22 comment=a ; out ACCEPT proto=udp dport=53")
	if err != nil || len(rules) != 2 || rules[0].Comment != "a" || rules[1].Type != TypeOut {
		t.Fatalf("ParseList = %+v, %v", rules, err)
	}
	if rules, err := ParseList("  "); err != nil || rules != nil {
		t.Errorf("ParseList(blank) = %v, %v", rules, err)
	}
	if _, err := ParseList("in ACCEPT ; in ALLOW"); err == nil || !strings.Contains(err.Error(), "rules[1]") {
		t.Errorf("ParseList with a bad second rule: %v", err)
	}
	if got, _ := Canonical("IN accept dport=1 proto=tcp"); got != "in ACCEPT proto=tcp dport=1" {
		t.Errorf("Canonical = %q", got)
	}
}

func TestCommentAndMark(t *testing.T) {
	if got := Comment("a;b\n\t c  d"); got != "a,b c d" {
		t.Errorf("Comment = %q", got)
	}
	long := Comment(strings.Repeat("関", 100))
	if len(long) > CommentMax || !strings.HasSuffix(long, "…") {
		t.Errorf("long comment: %d bytes %q", len(long), long)
	}
	for _, c := range []string{"", "web から DB"} {
		got, ok := Unmark(Mark(c))
		if !ok || got != c {
			t.Errorf("Unmark(Mark(%q)) = %q, %v", c, got, ok)
		}
	}
	for _, c := range []string{"hand rule", "sakuteix", "not sakutei: x"} {
		if _, ok := Unmark(c); ok {
			t.Errorf("Unmark(%q) found the marker", c)
		}
	}
}

func TestParamsAndFromAPI(t *testing.T) {
	r, _ := Parse("out ACCEPT iface=net0 source=10.0.0.1 dest=10.0.0.10,10.0.0.11 proto=udp dport=53 comment=DNS")
	p := r.Params()
	if p.Get("enable") != "1" || p.Get("comment") != "sakutei: DNS" || p.Get("iface") != "net0" || p.Get("type") != "out" || p.Has("sport") {
		t.Errorf("Params = %v", p)
	}
	m := map[string]string{}
	for k := range p {
		m[k] = p.Get(k)
	}
	back, marked := FromAPI(m)
	if !marked || back != r {
		t.Errorf("FromAPI(Params) = %+v, %v; want %+v", back, marked, r)
	}
	hand, marked := FromAPI(map[string]string{"type": "in", "action": "DROP", "enable": "0", "comment": "a;b"})
	if marked || !hand.Disabled || hand.String() != "in DROP enable=0 comment=a,b" {
		t.Errorf("FromAPI(hand) = %q, %v", hand.String(), marked)
	}
	was, _ := Parse("in ACCEPT iface=net1 proto=tcp dport=443")
	now, _ := Parse("in ACCEPT proto=tcp")
	if got := now.Unset(was); strings.Join(got, ",") != "iface,dport" {
		t.Errorf("Unset = %v", got)
	}
}

// 宣言の comment は観測の側と同じに正規化される：宣言どおりに置いて読み戻しても
// 同じ文になる（drift にならない）。
func TestCommentSameOnBothSides(t *testing.T) {
	long := strings.Repeat("長い理由", 30)
	for _, in := range []string{
		"in ACCEPT proto=tcp dport=22 comment=管理\tの  口",
		"in ACCEPT proto=tcp dport=22 comment=  前後 と  中の   空白  ",
		"in ACCEPT proto=tcp dport=22 comment=" + long,
	} {
		r, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		if r.Comment != Comment(r.Comment) || len(r.Comment) > CommentMax {
			t.Errorf("Parse(%q).Comment = %q, not normalized", in, r.Comment)
		}
		// Proxmox に書く値（Params）をそのまま読み戻す。
		p := r.Params()
		m := map[string]string{}
		for k := range p {
			m[k] = p.Get(k)
		}
		back, marked := FromAPI(m)
		if !marked || back.String() != r.String() {
			t.Errorf("declared %q, read back %q", r.String(), back.String())
		}
		if c, _ := Canonical(in); c != r.String() {
			t.Errorf("Canonical(%q) = %q, want %q", in, c, r.String())
		}
	}
	if err := (Rule{Type: "in", Action: "ACCEPT", Comment: "a  b"}).Validate(); err == nil {
		t.Error("Validate accepted a comment that is not in the normal form")
	}
}
