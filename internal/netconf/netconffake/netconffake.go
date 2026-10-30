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

// Package netconffake は Go の中で動く偽の NETCONF サーバ（Backlog #141・#175
// の単体・契約・受け入れテスト用）。base:1.0 だけのもの（"]]>]]>" の区切り）と
// base:1.1 のもの（chunked）になれる。
//
//   - running の設定を XML で持ち、get-config の subtree filter（RFC 6241 §6:
//     選ぶ・中に入る・値で絞る）を当てて返す。
//   - 受けた rpc の操作の名前を残す（edit-config などが来ていないことを確かめる）。
//     commit confirmed は "commit-confirmed" と残す。Calls はセッションの番号も残す。
//   - hello の Caps に :candidate があれば candidate を持つ（lock・unlock・
//     edit-config〈merge・replace・create・delete・remove、yang:insert="last"〉・
//     discard-changes・commit）。:validate があれば validate。:confirmed-commit
//     （1.0・1.1）があれば commit confirmed: confirm-timeout（秒、TimeUnit で
//     縮められる）が過ぎるか、persist の無い commit confirmed を送ったセッションが
//     終われば running を戻す（RFC 6241 §8.4）。1.1 なら persist・persist-id・
//     cancel-commit。戻した理由は Rollbacks に残る。
//   - YANG を持たないので、リストのエントリは子の name（無ければ最初の葉）で、
//     LeafLists で名指しした葉は値で突き合わせる。
//   - Scope・ScopeOps で、部分木だけに絞ったユーザー（NACM）のように振る舞う
//     （外の読みは黙って省き、外の書き込み・許さない操作は access-denied。
//     Denials に残す。scope.go）。
//   - TopAttrs・EmptyData で、get-config の返りの最上位の要素に（読むたびに
//     変わる）属性を付ける機器・部分木が無いときに空の <configuration/> を返す
//     機器のように振る舞う（#208）。
//   - 止める（Down）・セッションを切る（Drop）・hello を壊す・rpc-error を返す
//     （SetRPCError・SetOpError）・行う前・応答の前に切る（SetDropBefore・
//     SetDropAfter）・答える前に待つ（SetDelay）・running
//     しだいで新しい接続を断る（RefuseDialsWhen）、を切り替えられる。
package netconffake

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/netconf"
	"github.com/tjst-t/sakutei/v2/internal/remote"
)

// Server は偽の NETCONF サーバ。
type Server struct {
	// Base11 なら hello で base:1.1 も出す（相手も出せば chunked）。
	Base11 bool
	// Caps は hello に足す capability。
	Caps []string
	// RunningFile があれば、get-config のたびに running をこのファイルから読む
	// （別のプロセスのサーバを、テストがファイルで変える）。
	RunningFile string
	// OpsFile があれば、受けた rpc の操作の名前を 1 行ずつ足す。
	OpsFile string
	// TimeUnit は confirm-timeout の 1 の長さ（0 なら 1 秒）。
	TimeUnit time.Duration
	// LeafLists は値で突き合わせる葉（leaf-list）の名前（nil なら user-name と
	// group）。
	LeafLists []string
	// RefuseDialsWhen が true を返す running の間は、新しい接続を断る（届かなく
	// なる設定。開いているセッションは切らない）。
	RefuseDialsWhen func(running string) bool
	// Scope があれば、部分木だけに絞ったユーザー（NACM）のように、Scope（subtree
	// filter の形の XML）が選ぶ所だけを読み書きさせる（scope.go）。
	Scope string
	// ScopeOps があれば、その操作の名前だけを許す（ほかは access-denied）。
	ScopeOps []string
	// TopAttrs があれば、get-config の返りの最上位の要素ごとに、その返す
	// 属性（xmlns:p="…" p:x="…" の形の XML の断片）を付ける（commit の時刻
	// などの印を付ける機器、#208）。get-config のたびに呼ぶ。
	TopAttrs func() string
	// EmptyData があれば、filter が何も選ばないとき <data> の中にこれを返す
	// （部分木が無いときに空の <configuration/> を返す機器、#208）。
	EmptyData string

	mu        sync.Mutex
	running   string
	down      bool
	badHello  string
	rpcError  string
	ops       []string
	dials     int
	nextID    int
	conns     map[io.Closer]bool
	framings  []netconf.Framing
	lastHello string

	candidate  *string // nil: the same as running
	lockedBy   int     // session holding the candidate lock (0: none)
	pending    *confirmed
	rollbacks  []string
	calls      []Call
	opErrors   map[string]string
	dropAfter  map[string]int
	dropBefore map[string]int
	delays     map[string]time.Duration
	denials    []string
}

// Call は受けた rpc 1 つ（セッションの番号と操作の名前）。
type Call struct {
	Session int
	Op      string
}

// confirmed は確定を待つ commit confirmed。
type confirmed struct {
	session int
	persist string
	backup  string
	timer   *time.Timer
	gen     int
}

// New は running の設定（最上位の要素の列の XML）を持つサーバ。
func New(running string, base11 bool) *Server {
	return &Server{Base11: base11, running: running, nextID: 1, conns: map[io.Closer]bool{}}
}

// SetRunning は running を書き換える（「誰かが手で変えた」）。
func (s *Server) SetRunning(x string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = x
}

// SetDown で止める（つなげない・開いているセッションは切る）。false で戻す。
func (s *Server) SetDown(down bool) {
	s.mu.Lock()
	s.down = down
	s.mu.Unlock()
	if down {
		s.Drop()
	}
}

// SetBadHello は次からのセッションの hello を hello の代わりに送る（"" で戻す）。
func (s *Server) SetBadHello(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.badHello = msg
}

// SetRPCError は get-config に error-tag tag の rpc-error を返す（"" で戻す）。
func (s *Server) SetRPCError(tag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rpcError = tag
}

// SetOpError は操作 op（"edit-config"・"commit-confirmed" など）に error-tag
// tag の rpc-error を返す（"" で戻す）。
func (s *Server) SetOpError(op, tag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.opErrors == nil {
		s.opErrors = map[string]string{}
	}
	if tag == "" {
		delete(s.opErrors, op)
		return
	}
	s.opErrors[op] = tag
}

// SetDropAfter は、これから n 回目に受ける操作 op を行ったあと、応答を送らずに
// その接続を切る（応答の前に切れた）。
func (s *Server) SetDropAfter(op string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dropAfter == nil {
		s.dropAfter = map[string]int{}
	}
	s.dropAfter[op] = n
}

// SetDelay は op の rpc に答える前に d だけ待つ（遅い機器、#199）。0 で戻す。
func (s *Server) SetDelay(op string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.delays == nil {
		s.delays = map[string]time.Duration{}
	}
	s.delays[op] = d
}

// SetDropBefore は、これから n 回目に受ける操作 op を行わずに、その接続を
// 切る（送った要求が届かなかった）。
func (s *Server) SetDropBefore(op string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dropBefore == nil {
		s.dropBefore = map[string]int{}
	}
	s.dropBefore[op] = n
}

// countdown は m[op] を 1 つ減らし、0 になったら true（s.mu を持って呼ぶ）。
func countdown(m map[string]int, op string) bool {
	n, ok := m[op]
	if !ok {
		return false
	}
	if n <= 1 {
		delete(m, op)
		return true
	}
	m[op] = n - 1
	return false
}

// Running は今の running。
func (s *Server) Running() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Calls は受けた rpc（順に、セッションの番号つき）。
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// Rollbacks は commit confirmed を戻した理由（"timeout"・"session-end"・
// "cancel"）の列。
func (s *Server) Rollbacks() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.rollbacks...)
}

// Pending は確定を待つ commit confirmed があるか。
func (s *Server) Pending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending != nil
}

// Drop は開いているセッションを切る（機器が idle で切った）。
func (s *Server) Drop() {
	s.mu.Lock()
	conns := s.conns
	s.conns = map[io.Closer]bool{}
	s.mu.Unlock()
	for c := range conns {
		c.Close()
	}
}

// Ops は受けた rpc の操作の名前（順に）。
func (s *Server) Ops() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ops...)
}

// Dials はつないだ回数。
func (s *Server) Dials() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dials
}

// ClientHello は最後に受けた手元の hello。
func (s *Server) ClientHello() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastHello
}

// Framings はセッションごとの hello の後の区切り方。
func (s *Server) Framings() []netconf.Framing {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]netconf.Framing(nil), s.framings...)
}

// Dialer はこのサーバにつなぐ netconf.Dialer（net.Pipe）。止めていれば
// *remote.ConnError（届かない）。
func (s *Server) Dialer(hostName string) netconf.Dialer {
	return func(ctx context.Context) (io.ReadWriteCloser, error) {
		s.mu.Lock()
		if s.down || (s.RefuseDialsWhen != nil && s.RefuseDialsWhen(s.running)) {
			s.mu.Unlock()
			return nil, &remote.ConnError{Target: hostName, Kind: remote.Unreachable, Detail: "connection refused"}
		}
		s.dials++
		client, server := net.Pipe()
		s.conns[server] = true
		s.mu.Unlock()
		go s.serve(server)
		return client, nil
	}
}

// ServeStream は rw の上で 1 つのセッションに答え、終われば返る（別の
// プロセスのサーバの標準入出力など）。
func (s *Server) ServeStream(rw io.ReadWriteCloser) {
	s.mu.Lock()
	s.conns[rw] = true
	s.mu.Unlock()
	s.serve(rw)
}

func (s *Server) serve(c io.ReadWriteCloser) {
	f := netconf.NewFramer(c, c)
	s.mu.Lock()
	id := s.nextID
	s.nextID++
	bad := s.badHello
	s.mu.Unlock()
	defer func() {
		c.Close()
		s.mu.Lock()
		delete(s.conns, c)
		s.sessionEnded(id)
		s.mu.Unlock()
	}()
	hello := bad
	if hello == "" {
		caps := []string{netconf.CapBase10}
		if s.Base11 {
			caps = append(caps, netconf.CapBase11)
		}
		caps = append(caps, s.Caps...)
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><hello xmlns="` + netconf.BaseNS + `"><capabilities>`)
		for _, cp := range caps {
			b.WriteString("<capability>")
			_ = xml.EscapeText(&b, []byte(cp))
			b.WriteString("</capability>")
		}
		fmt.Fprintf(&b, "</capabilities><session-id>%d</session-id></hello>", id)
		hello = b.String()
	}
	// The hellos cross (RFC 6241 §8.1): ours is written while the client's is
	// read; the framing changes only after both.
	wrote := make(chan error, 1)
	go func() {
		_, err := c.Write([]byte(hello + "]]>]]>"))
		wrote <- err
	}()
	msg, err := f.ReadMessage()
	if err != nil {
		c.Close() // unblocks the write
		<-wrote
		return
	}
	if err := <-wrote; err != nil {
		return
	}
	var ch struct {
		Caps []string `xml:"capabilities>capability"`
	}
	if xml.Unmarshal(msg, &ch) != nil {
		return
	}
	framing := netconf.FramingEOM
	if s.Base11 {
		for _, cp := range ch.Caps {
			if strings.TrimSpace(cp) == netconf.CapBase11 {
				framing = netconf.FramingChunked
			}
		}
	}
	f.SetFraming(framing)
	s.mu.Lock()
	s.framings = append(s.framings, framing)
	s.lastHello = string(msg)
	s.mu.Unlock()
	for {
		msg, err := f.ReadMessage()
		if err != nil {
			return
		}
		reply, closeAfter := s.handle(id, msg)
		if reply == "" {
			return // dropped before the reply
		}
		if err := f.WriteMessage([]byte(reply)); err != nil || closeAfter {
			return
		}
	}
}

// sessionEnded は終わったセッションの lock を放し、persist の無い commit
// confirmed を戻す（RFC 6241 §7.5・§8.4）。s.mu を持って呼ぶ。
func (s *Server) sessionEnded(id int) {
	if s.lockedBy == id {
		s.lockedBy = 0
		s.candidate = nil
	}
	if p := s.pending; p != nil && p.session == id && p.persist == "" {
		s.rollbackLocked("session-end")
	}
}

// rollbackLocked は確定を待つ commit confirmed を戻す。s.mu を持って呼ぶ。
func (s *Server) rollbackLocked(why string) {
	p := s.pending
	if p == nil {
		return
	}
	p.timer.Stop()
	s.running = p.backup
	s.candidate = nil
	s.pending = nil
	s.rollbacks = append(s.rollbacks, why)
}

func (s *Server) hasCap(c string) bool {
	for _, x := range s.Caps {
		if x == c {
			return true
		}
	}
	return false
}

// handle は rpc 1 つに答える。"" は応答の前に切る。
func (s *Server) handle(sid int, msg []byte) (string, bool) {
	root, err := parse(msg)
	if err != nil || len(root) != 1 || root[0].Name.Local != "rpc" {
		return rpcError("", "rpc", "malformed-message", "not an rpc"), false
	}
	rpc := root[0]
	mid := ""
	for _, a := range rpc.Attr {
		if a.Name.Local == "message-id" {
			mid = a.Value
		}
	}
	if len(rpc.Children) != 1 {
		return rpcError(mid, "rpc", "missing-element", "want one operation"), false
	}
	op := rpc.Children[0]
	name := op.Name.Local
	if name == "commit" && op.child("confirmed") != nil {
		name = "commit-confirmed"
	}
	s.mu.Lock()
	s.ops = append(s.ops, name)
	s.calls = append(s.calls, Call{Session: sid, Op: name})
	running, rpcErr, opErr := s.running, s.rpcError, s.opErrors[name]
	dropBefore := countdown(s.dropBefore, name)
	drop := !dropBefore && countdown(s.dropAfter, name)
	denied := !dropBefore && s.opDenied(name)
	delay := s.delays[name]
	s.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if dropBefore {
		return "", true
	}
	if s.OpsFile != "" {
		if f, err := os.OpenFile(s.OpsFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			fmt.Fprintln(f, name)
			f.Close()
		}
	}
	if denied {
		return accessDenied(mid, "protocol", "/ietf-netconf:"+op.Name.Local, "the user may not execute "+name+" (the fake's ScopeOps)"), false
	}
	if opErr != "" {
		return rpcError(mid, "application", opErr, "refused by the fake"), false
	}
	if reply, ok := s.handleEdit(sid, mid, name, op); ok {
		if drop {
			return "", true
		}
		return reply, false
	}
	if s.RunningFile != "" {
		b, err := os.ReadFile(s.RunningFile)
		if err != nil {
			return rpcError(mid, "application", "operation-failed", err.Error()), false
		}
		running = string(b)
	}
	switch op.Name.Local {
	case "close-session":
		return okReply(mid), true
	case "get-config":
		if rpcErr != "" {
			return rpcError(mid, "application", rpcErr, "refused by the fake"), false
		}
		src := op.child("source")
		switch {
		case src != nil && src.child("running") != nil:
		case src != nil && src.child("candidate") != nil && s.hasCap(netconf.CapCandidate):
			s.mu.Lock()
			if s.candidate != nil {
				running = *s.candidate
			}
			s.mu.Unlock()
		default:
			return rpcError(mid, "protocol", "invalid-value", "only running (and candidate)"), false
		}
		data, err := parse([]byte(running))
		if err != nil {
			return rpcError(mid, "application", "operation-failed", "bad running: "+err.Error()), false
		}
		if fl := op.child("filter"); fl != nil {
			data = filterNodes(data, fl.Children)
		}
		data = s.readScoped(data)
		var b strings.Builder
		b.WriteString(`<rpc-reply message-id="` + mid + `" xmlns="` + netconf.BaseNS + `"><data>`)
		if len(data) == 0 {
			b.WriteString(s.EmptyData)
		}
		for _, n := range data {
			extra := ""
			if s.TopAttrs != nil {
				extra = " " + s.TopAttrs()
			}
			n.writeWith(&b, netconf.BaseNS, extra)
		}
		b.WriteString(`</data></rpc-reply>`)
		return b.String(), false
	}
	return rpcError(mid, "protocol", "operation-not-supported", op.Name.Local+" is not supported by the fake"), false
}

func okReply(mid string) string {
	return `<rpc-reply message-id="` + mid + `" xmlns="` + netconf.BaseNS + `"><ok/></rpc-reply>`
}

func rpcError(mid, typ, tag, msg string) string {
	var b strings.Builder
	b.WriteString(`<rpc-reply message-id="` + mid + `" xmlns="` + netconf.BaseNS + `"><rpc-error>`)
	b.WriteString(`<error-type>` + typ + `</error-type><error-tag>` + tag + `</error-tag><error-severity>error</error-severity><error-message>`)
	_ = xml.EscapeText(&b, []byte(msg))
	b.WriteString(`</error-message></rpc-error></rpc-reply>`)
	return b.String()
}

// elem は XML の要素（xmlns の宣言の属性もそのまま持つ）。
type elem struct {
	Name     xml.Name
	Attr     []xml.Attr
	Children []*elem
	Text     string
}

func (e *elem) child(local string) *elem {
	for _, c := range e.Children {
		if c.Name.Local == local {
			return c
		}
	}
	return nil
}

func (e *elem) leaf() bool { return len(e.Children) == 0 }

func parse(data []byte) ([]*elem, error) {
	d := xml.NewDecoder(bytes.NewReader(data))
	var top, stack []*elem
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			e := &elem{Name: t.Name, Attr: t.Copy().Attr}
			if len(stack) == 0 {
				top = append(top, e)
			} else {
				p := stack[len(stack)-1]
				p.Children = append(p.Children, e)
			}
			stack = append(stack, e)
		case xml.EndElement:
			e := stack[len(stack)-1]
			e.Text = strings.TrimSpace(e.Text)
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].Text += string(t)
			}
		}
	}
	return top, nil
}

// write は e を書く（名前空間は親と違うときだけ xmlns で）。
func (e *elem) write(b *strings.Builder, parentNS string) { e.writeWith(b, parentNS, "") }

// writeWith は write に、e の開始タグへ足す属性の断片 extra を付けたもの。
func (e *elem) writeWith(b *strings.Builder, parentNS, extra string) {
	b.WriteString("<" + e.Name.Local + extra)
	if e.Name.Space != parentNS {
		b.WriteString(` xmlns="` + e.Name.Space + `"`)
	}
	for _, a := range e.Attr {
		switch {
		case a.Name.Space == "" && a.Name.Local == "xmlns":
		case a.Name.Space == "xmlns":
			b.WriteString(" xmlns:" + a.Name.Local + `="` + a.Value + `"`)
		case a.Name.Space == "":
			b.WriteString(" " + a.Name.Local + `="` + a.Value + `"`)
		}
	}
	b.WriteString(">")
	_ = xml.EscapeText(b, []byte(e.Text))
	for _, c := range e.Children {
		c.write(b, e.Name.Space)
	}
	b.WriteString("</" + e.Name.Local + ">")
}

func matches(f, d *elem) bool {
	return f.Name.Local == d.Name.Local && (f.Name.Space == "" || f.Name.Space == d.Name.Space)
}

// filterNodes は data の兄弟に filter の兄弟を当てる（RFC 6241 §6）。
func filterNodes(data, filter []*elem) []*elem {
	var out []*elem
	for _, d := range data {
		for _, f := range filter {
			if !matches(f, d) {
				continue
			}
			if r := apply(d, f); r != nil {
				out = append(out, r)
				break
			}
		}
	}
	return out
}

// apply は 1 つの data の要素に 1 つの filter の要素を当てる。nil は選ばれない。
func apply(d, f *elem) *elem {
	if f.leaf() {
		if f.Text == "" || f.Text == d.Text { // selection / content match
			return d
		}
		return nil
	}
	var cms, others []*elem
	for _, c := range f.Children {
		if c.leaf() && c.Text != "" {
			cms = append(cms, c)
		} else {
			others = append(others, c)
		}
	}
	out := &elem{Name: d.Name, Attr: d.Attr, Text: d.Text}
	for _, cm := range cms {
		found := false
		for _, dc := range d.Children {
			if matches(cm, dc) && dc.Text == cm.Text {
				found = true
			}
		}
		if !found {
			return nil
		}
	}
	if len(others) == 0 {
		return d
	}
	for _, dc := range d.Children {
		isCM := false
		for _, cm := range cms {
			if matches(cm, dc) && dc.Text == cm.Text {
				isCM = true
			}
		}
		if isCM {
			out.Children = append(out.Children, dc)
			continue
		}
		for _, o := range others {
			if !matches(o, dc) {
				continue
			}
			if r := apply(dc, o); r != nil {
				out.Children = append(out.Children, r)
				break
			}
		}
	}
	if len(out.Children) == 0 {
		return nil
	}
	return out
}
