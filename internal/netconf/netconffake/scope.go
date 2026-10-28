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

package netconffake

// scope.go — 部分木だけに絞ったユーザー（機器の側の NACM、Backlog #176）の
// 近似。
//
//   - Scope（subtree filter の形の XML）があれば、そのユーザーが読み書き
//     できるのは Scope が選ぶ所だけ。get-config は Scope の外を黙って省き
//     （RFC 8341 §3.4.5）、Scope の外を変える edit-config は access-denied で
//     断る（candidate は変えない）。
//   - ScopeOps があれば、その操作の名前（"commit-confirmed" を含む）だけを
//     許し、ほかは access-denied（close-session はいつも許す、RFC 8341 §3.4.4）。
//   - 外を読もうとした・書こうとした・許されない操作を送った、は Denials に残る
//     （「外を触る操作が無い」をテストが確かめる）。

import (
	"encoding/xml"
	"strings"

	"github.com/tjst-t/sakutei/v2/internal/netconf"
)

// Denials は Scope・ScopeOps で断った（読みは省いた）ことの列。
func (s *Server) Denials() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.denials...)
}

// opDenied は ScopeOps が op を許さないか（s.mu を持って呼ぶ）。
func (s *Server) opDenied(op string) bool {
	if s.ScopeOps == nil || op == "close-session" {
		return false
	}
	for _, x := range s.ScopeOps {
		if x == op {
			return false
		}
	}
	s.denials = append(s.denials, "rpc "+op)
	return true
}

// scopeFilter は Scope の要素（無ければ nil）。
func (s *Server) scopeFilter() []*elem {
	if s.Scope == "" {
		return nil
	}
	f, err := parse([]byte(s.Scope))
	if err != nil {
		panic("netconffake: bad Scope: " + err.Error())
	}
	return f
}

// readScoped は get-config の data から Scope の外を省く。省いたら Denials に残す。
func (s *Server) readScoped(data []*elem) []*elem {
	scope := s.scopeFilter()
	if scope == nil {
		return data
	}
	in := filterNodes(data, scope)
	if serialize(in) != serialize(data) {
		s.mu.Lock()
		s.denials = append(s.denials, "read outside the scope: get-config")
		s.mu.Unlock()
	}
	return in
}

// writesOutside は before から after への変更が Scope の外を変えるか（s.mu を
// 持って呼ぶ）。変えるなら Denials に残す。
func (s *Server) writesOutside(before, after []*elem) bool {
	scope := s.scopeFilter()
	if scope == nil {
		return false
	}
	if serialize(without(before, scope)) == serialize(without(after, scope)) {
		return false
	}
	s.denials = append(s.denials, "write outside the scope: edit-config")
	return true
}

func serialize(nodes []*elem) string {
	var b strings.Builder
	for _, n := range nodes {
		n.write(&b, "")
	}
	return b.String()
}

// without は data から filter が選ぶ所を除いた残り。中身が無くなった入れ物
// （エントリを指す葉だけが残ったものも）は落とす。
func without(data, filter []*elem) []*elem {
	var out []*elem
	for _, d := range data {
		kept := d
		for _, f := range filter {
			if !matches(f, d) || apply(d, f) == nil {
				continue
			}
			var others []*elem
			for _, c := range f.Children {
				if !(c.leaf() && c.Text != "") {
					others = append(others, c)
				}
			}
			if len(others) == 0 {
				kept = nil // the whole element is in the scope
				break
			}
			var keys, rest []*elem
			for _, dc := range d.Children {
				key := false
				for _, cm := range f.Children {
					if cm.leaf() && cm.Text != "" && matches(cm, dc) && dc.Text == cm.Text {
						key = true
					}
				}
				if key {
					keys = append(keys, dc)
				} else {
					rest = append(rest, dc)
				}
			}
			rest = without(rest, others)
			if len(rest) == 0 {
				kept = nil
			} else {
				kept = &elem{Name: d.Name, Attr: d.Attr, Text: d.Text, Children: append(keys, rest...)}
			}
			break
		}
		if kept != nil {
			out = append(out, kept)
		}
	}
	return out
}

// accessDenied は access-denied の rpc-error（error-path つき）。
func accessDenied(mid, typ, path, msg string) string {
	var b strings.Builder
	b.WriteString(`<rpc-reply message-id="` + mid + `" xmlns="` + netconf.BaseNS + `"><rpc-error>`)
	b.WriteString(`<error-type>` + typ + `</error-type><error-tag>` + netconf.TagAccessDenied + `</error-tag><error-severity>error</error-severity><error-path>`)
	_ = xml.EscapeText(&b, []byte(path))
	b.WriteString(`</error-path><error-message>`)
	_ = xml.EscapeText(&b, []byte(msg))
	b.WriteString(`</error-message></rpc-error></rpc-reply>`)
	return b.String()
}
