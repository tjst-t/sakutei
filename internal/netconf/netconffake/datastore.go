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

// datastore.go — candidate・lock・edit-config・validate・commit（confirmed）・
// discard-changes・cancel-commit（Backlog #175）。

import (
	"strconv"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/netconf"
)

// handleEdit は candidate と commit の操作に答える。ok が false なら、この
// 操作はここでは扱わない。
func (s *Server) handleEdit(sid int, mid, name string, op *elem) (reply string, ok bool) {
	switch name {
	case "lock", "unlock", "edit-config", "validate", "commit", "commit-confirmed", "discard-changes", "cancel-commit":
	default:
		return "", false
	}
	if !s.hasCap(netconf.CapCandidate) {
		return rpcError(mid, "protocol", "operation-not-supported", name+" is not supported by the fake (no :candidate)"), true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch name {
	case "lock", "unlock":
		if t := op.child("target"); t == nil || t.child("candidate") == nil {
			return rpcError(mid, "protocol", "invalid-value", "the fake locks only the candidate"), true
		}
		if name == "lock" {
			if s.lockedBy != 0 {
				return rpcError(mid, "protocol", "lock-denied", "locked by session "+strconv.Itoa(s.lockedBy)), true
			}
			if s.candidate != nil && *s.candidate != s.running {
				return rpcError(mid, "protocol", "lock-denied", "the candidate has changes not committed"), true
			}
			s.lockedBy = sid
			return okReply(mid), true
		}
		if s.lockedBy != sid {
			return rpcError(mid, "protocol", "operation-failed", "not locked by this session"), true
		}
		s.lockedBy = 0
		return okReply(mid), true

	case "edit-config":
		if t := op.child("target"); t == nil || t.child("candidate") == nil {
			return rpcError(mid, "protocol", "invalid-value", "the fake edits only the candidate"), true
		}
		if s.lockedBy != 0 && s.lockedBy != sid {
			return rpcError(mid, "protocol", "in-use", "the candidate is locked by session "+strconv.Itoa(s.lockedBy)), true
		}
		cfg := op.child("config")
		if cfg == nil {
			return rpcError(mid, "protocol", "missing-element", "no config"), true
		}
		cur := s.running
		if s.candidate != nil {
			cur = *s.candidate
		}
		tree, err := parse([]byte(cur))
		if err != nil {
			return rpcError(mid, "application", "operation-failed", "bad candidate: "+err.Error()), true
		}
		before, _ := parse([]byte(cur))
		tree, tag, msg := s.editLevel(tree, cfg.Children, "merge")
		if tag != "" {
			return rpcError(mid, "application", tag, msg), true
		}
		if s.writesOutside(before, tree) {
			return accessDenied(mid, "application", "/", "the edit writes outside the user's scope (the fake's Scope)"), true
		}
		var b strings.Builder
		for _, n := range tree {
			n.write(&b, "")
		}
		x := b.String()
		s.candidate = &x
		return okReply(mid), true

	case "validate":
		if !s.hasCap(netconf.CapValidate10) && !s.hasCap(netconf.CapValidate11) {
			return rpcError(mid, "protocol", "operation-not-supported", "no :validate"), true
		}
		return okReply(mid), true

	case "discard-changes":
		s.candidate = nil
		return okReply(mid), true

	case "commit-confirmed":
		has11 := s.hasCap(netconf.CapConfirmedCommit11)
		if !has11 && !s.hasCap(netconf.CapConfirmedCommit10) {
			return rpcError(mid, "protocol", "operation-not-supported", "no :confirmed-commit"), true
		}
		persist := ""
		if p := op.child("persist"); p != nil {
			if !has11 {
				return rpcError(mid, "protocol", "unknown-element", "persist needs :confirmed-commit:1.1"), true
			}
			persist = p.Text
		}
		secs := 600
		if t := op.child("confirm-timeout"); t != nil {
			n, err := strconv.Atoi(t.Text)
			if err != nil || n < 1 {
				return rpcError(mid, "protocol", "invalid-value", "bad confirm-timeout"), true
			}
			secs = n
		}
		if s.pending != nil {
			// A follow-up commit confirmed: the same rules as a confirming one.
			if tag, msg := s.mayConfirm(sid, op); tag != "" {
				return rpcError(mid, "protocol", tag, msg), true
			}
			s.pending.timer.Stop()
		} else {
			s.pending = &confirmed{backup: s.running}
		}
		if s.candidate != nil {
			s.running = *s.candidate
			s.candidate = nil
		}
		s.pending.session, s.pending.persist = sid, persist
		s.pending.gen++
		gen, unit := s.pending.gen, s.TimeUnit
		if unit == 0 {
			unit = time.Second
		}
		s.pending.timer = time.AfterFunc(time.Duration(secs)*unit, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.pending != nil && s.pending.gen == gen {
				s.rollbackLocked("timeout")
			}
		})
		return okReply(mid), true

	case "commit":
		if s.pending != nil {
			if tag, msg := s.mayConfirm(sid, op); tag != "" {
				return rpcError(mid, "protocol", tag, msg), true
			}
			s.pending.timer.Stop()
			s.pending = nil
		}
		if s.candidate != nil {
			s.running = *s.candidate
			s.candidate = nil
		}
		return okReply(mid), true

	case "cancel-commit":
		if !s.hasCap(netconf.CapConfirmedCommit11) {
			return rpcError(mid, "protocol", "operation-not-supported", "cancel-commit needs :confirmed-commit:1.1"), true
		}
		if s.pending == nil {
			return rpcError(mid, "protocol", "invalid-value", "no commit confirmed is pending"), true
		}
		if tag, msg := s.mayConfirm(sid, op); tag != "" {
			return rpcError(mid, "protocol", tag, msg), true
		}
		s.rollbackLocked("cancel")
		return okReply(mid), true
	}
	return "", false
}

// mayConfirm は、確定を待つ commit confirmed を sid のこの操作が確定（取り消し）
// してよいか: persist で始めたものは persist-id が合うこと、そうでなければ同じ
// セッションであること（RFC 6241 §8.4.5.1）。
func (s *Server) mayConfirm(sid int, op *elem) (string, string) {
	p := s.pending
	if p.persist != "" {
		id := op.child("persist-id")
		if id == nil || id.Text != p.persist {
			return "invalid-value", "the persist-id does not match the pending commit confirmed"
		}
		return "", ""
	}
	if p.session != sid {
		return "in-use", "a commit confirmed of session " + strconv.Itoa(p.session) + " is pending"
	}
	return "", ""
}

// operation は e の nc:operation（無ければ inherited）と、yang:insert="last" か。
func operation(e *elem, inherited string) (string, bool) {
	op, last := inherited, false
	for _, a := range e.Attr {
		switch {
		case a.Name.Space == netconf.BaseNS && a.Name.Local == "operation":
			op = a.Value
		case a.Name.Space == netconf.YANGNS && a.Name.Local == "insert":
			last = a.Value == "last"
		}
	}
	return op, last
}

// strip は e から edit-config の属性と、その接頭辞の宣言を落とした写し（子も）。
func strip(e *elem) *elem {
	out := &elem{Name: e.Name, Text: e.Text}
	for _, a := range e.Attr {
		switch {
		case a.Name.Space == netconf.BaseNS || a.Name.Space == netconf.YANGNS:
		case a.Name.Space == "xmlns" && (a.Value == netconf.BaseNS || a.Value == netconf.YANGNS):
		default:
			out.Attr = append(out.Attr, a)
		}
	}
	for _, c := range e.Children {
		out.Children = append(out.Children, strip(c))
	}
	return out
}

func (s *Server) leafList(name string) bool {
	ll := s.LeafLists
	if ll == nil {
		ll = []string{"user-name", "group"}
	}
	for _, n := range ll {
		if n == name {
			return true
		}
	}
	return false
}

// sameEntry は既存の要素 x と edit の要素 e が同じものか（YANG なしの近似）。
func (s *Server) sameEntry(x, e *elem) bool {
	if x.Name != e.Name {
		return false
	}
	if e.leaf() {
		if s.leafList(e.Name.Local) {
			return x.Text == e.Text
		}
		return true
	}
	var key *elem
	if k := e.child("name"); k != nil && k.leaf() {
		key = k
	} else if len(e.Children) > 0 && e.Children[0].leaf() {
		key = e.Children[0]
	}
	if key == nil {
		return true // a container
	}
	for _, c := range x.Children {
		if c.Name == key.Name && c.Text == key.Text {
			return true
		}
	}
	return false
}

// editLevel は edits を兄弟の列 tree に当てる。tag が "" でなければ rpc-error。
func (s *Server) editLevel(tree []*elem, edits []*elem, inherited string) ([]*elem, string, string) {
	for _, e := range edits {
		op, last := operation(e, inherited)
		idx := -1
		for i, x := range tree {
			if s.sameEntry(x, e) {
				idx = i
				break
			}
		}
		switch op {
		case "remove", "delete":
			if idx < 0 {
				if op == "delete" {
					return nil, "data-missing", "no " + e.Name.Local + " to delete"
				}
				continue
			}
			tree = append(tree[:idx:idx], tree[idx+1:]...)
			continue
		case "create":
			if idx >= 0 {
				return nil, "data-exists", e.Name.Local + " exists"
			}
			fallthrough
		case "replace":
			n := strip(e)
			n.Children = nil
			var tag, msg string
			n.Children, tag, msg = s.editLevel(nil, e.Children, "merge")
			if tag != "" {
				return nil, tag, msg
			}
			if idx >= 0 {
				tree[idx] = n
			} else {
				tree = append(tree, n)
				idx = len(tree) - 1
			}
		case "merge":
			if idx < 0 {
				n := strip(e)
				var tag, msg string
				n.Children, tag, msg = s.editLevel(nil, e.Children, "merge")
				if tag != "" {
					return nil, tag, msg
				}
				tree = append(tree, n)
				idx = len(tree) - 1
			} else if e.leaf() {
				tree[idx].Text = e.Text
			} else {
				var tag, msg string
				tree[idx].Children, tag, msg = s.editLevel(tree[idx].Children, e.Children, "merge")
				if tag != "" {
					return nil, tag, msg
				}
			}
		default:
			return nil, "bad-attribute", "operation " + op
		}
		if last && idx >= 0 && idx < len(tree)-1 {
			n := tree[idx]
			tree = append(append(tree[:idx:idx], tree[idx+1:]...), n)
		}
	}
	return tree, "", ""
}
