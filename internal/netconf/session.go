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

package netconf

// session.go — NETCONF のセッション（RFC 6241、手元の側）。
//
//   - hello: こちらは base:1.0 と base:1.1 を出す。相手の hello に session-id と
//     base:1.0 か base:1.1 が無ければ「hello が合わない」（ErrHello）。両方が
//     base:1.1 なら、hello の後は chunked の区切り（RFC 6242 §4.1）。
//   - 観測で送る rpc は get-config（running、subtree filter）と close-session
//     だけ。2 段の Apply（apply.go、#175）は lock・unlock・edit-config・
//     validate・commit（confirmed）・discard-changes を足す。
//   - rpc-reply の rpc-error（error-severity が error のもの）は *RPCError。
//     warning だけなら成功として扱う。
//   - 1 つのセッションは 1 度に 1 つの rpc（Session は並行に使わない）。

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// 名前空間と capability。
const (
	BaseNS = "urn:ietf:params:xml:ns:netconf:base:1.0"
	// CapBase10・CapBase11 は base の capability。
	CapBase10 = "urn:ietf:params:netconf:base:1.0"
	CapBase11 = "urn:ietf:params:netconf:base:1.1"
	// capPrefix は NETCONF 自身の capability の頭（短い名前にして記録する）。
	capPrefix = "urn:ietf:params:netconf:"
	// 2 段の Apply が見る capability（RFC 6241 §8）。
	CapCandidate         = "urn:ietf:params:netconf:capability:candidate:1.0"
	CapConfirmedCommit10 = "urn:ietf:params:netconf:capability:confirmed-commit:1.0"
	CapConfirmedCommit11 = "urn:ietf:params:netconf:capability:confirmed-commit:1.1"
	CapValidate10        = "urn:ietf:params:netconf:capability:validate:1.0"
	CapValidate11        = "urn:ietf:params:netconf:capability:validate:1.1"
)

// ErrHello は相手の hello が NETCONF のものとして合わないことを表す。
var ErrHello = errors.New("netconf: the server's hello does not match")

// RPCError は rpc-reply の rpc-error。
type RPCError struct {
	Type, Tag, Severity, Message string
}

func (e *RPCError) Error() string {
	msg := fmt.Sprintf("netconf: rpc-error: %s %s (%s)", e.Type, e.Tag, e.Severity)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

// Session は開いた NETCONF のセッション。
type Session struct {
	rw        io.ReadWriteCloser
	f         *Framer
	sessionID string
	caps      []string
	nextID    int
}

// helloMsg は手元の hello。
const helloMsg = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<hello xmlns="` + BaseNS + `"><capabilities>` +
	`<capability>` + CapBase10 + `</capability>` +
	`<capability>` + CapBase11 + `</capability>` +
	`</capabilities></hello>`

// Open は rw の上で hello を交わす。ctx が終われば rw を閉じて止める。
func Open(ctx context.Context, rw io.ReadWriteCloser) (*Session, error) {
	s := &Session{rw: rw, f: NewFramer(rw, rw), nextID: 1}
	err := s.guard(ctx, func() error {
		// Both hellos are sent at once (RFC 6241 §8.1): write ours first, so a
		// server that waits for it is not blocked.
		if err := s.f.WriteMessage([]byte(helloMsg)); err != nil {
			return err
		}
		msg, err := s.f.ReadMessage()
		if err != nil {
			return err
		}
		return s.readHello(msg)
	})
	if err != nil {
		rw.Close()
		return nil, err
	}
	return s, nil
}

func (s *Session) readHello(msg []byte) error {
	var h struct {
		XMLName      xml.Name `xml:"hello"`
		Capabilities []string `xml:"capabilities>capability"`
		SessionID    string   `xml:"session-id"`
	}
	if err := xml.Unmarshal(msg, &h); err != nil {
		return fmt.Errorf("%w: %v", ErrHello, err)
	}
	if h.XMLName.Space != BaseNS {
		return fmt.Errorf("%w: the hello is not in the NETCONF namespace", ErrHello)
	}
	if _, err := strconv.ParseUint(strings.TrimSpace(h.SessionID), 10, 32); err != nil {
		return fmt.Errorf("%w: no session-id", ErrHello)
	}
	s.sessionID = strings.TrimSpace(h.SessionID)
	has := map[string]bool{}
	for _, c := range h.Capabilities {
		c = strings.TrimSpace(c)
		s.caps = append(s.caps, c)
		has[c] = true
	}
	switch {
	case has[CapBase11]:
		s.f.SetFraming(FramingChunked)
	case has[CapBase10]:
	default:
		return fmt.Errorf("%w: it offers neither base:1.0 nor base:1.1", ErrHello)
	}
	return nil
}

// SessionID は相手が付けたセッションの番号。
func (s *Session) SessionID() string { return s.sessionID }

// Framing はこのセッションの区切り方。
func (s *Session) Framing() Framing { return s.f.Framing() }

// Capabilities は相手の hello の capability（そのまま）。
func (s *Session) Capabilities() []string { return append([]string(nil), s.caps...) }

// Has は相手の hello に capability c があるか（? 以下は比べない）。
func (s *Session) Has(c string) bool {
	for _, x := range s.caps {
		if i := strings.IndexByte(x, '?'); i >= 0 {
			x = x[:i]
		}
		if x == c {
			return true
		}
	}
	return false
}

// ShortCapabilities は NETCONF 自身の capability を短い名前（"base:1.1"・
// "candidate:1.0"・"confirmed-commit:1.1"・"validate:1.1" など。? 以下は落とす）
// にして、並べて返す。YANG のモジュールの capability は含めない。
func (s *Session) ShortCapabilities() []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range s.caps {
		if !strings.HasPrefix(c, capPrefix) {
			continue
		}
		short := strings.TrimPrefix(c, capPrefix)
		short = strings.TrimPrefix(short, "capability:")
		if i := strings.IndexByte(short, '?'); i >= 0 {
			short = short[:i]
		}
		if short != "" && !seen[short] {
			seen[short] = true
			out = append(out, short)
		}
	}
	sort.Strings(out)
	return out
}

// GetConfig は running の、filter（subtree filter の中身の XML。空なら全体）で
// 絞った設定を返す: <data> の中身（最上位の要素の列）。
func (s *Session) GetConfig(ctx context.Context, filter string) ([]byte, error) {
	var body strings.Builder
	body.WriteString(`<get-config><source><running/></source>`)
	if strings.TrimSpace(filter) != "" {
		body.WriteString(`<filter type="subtree">`)
		body.WriteString(filter)
		body.WriteString(`</filter>`)
	}
	body.WriteString(`</get-config>`)
	reply, err := s.rpc(ctx, body.String())
	if err != nil {
		return nil, err
	}
	return dataOf(reply)
}

// Lock・Unlock は datastore（"candidate" など）を締める・放す。
func (s *Session) Lock(ctx context.Context, target string) error {
	_, err := s.rpc(ctx, `<lock><target><`+target+`/></target></lock>`)
	return err
}

// Unlock は Lock の逆。
func (s *Session) Unlock(ctx context.Context, target string) error {
	_, err := s.rpc(ctx, `<unlock><target><`+target+`/></target></unlock>`)
	return err
}

// EditConfig は candidate に config（<config> の中身の XML）を当てる。
// default-operation は既定（merge）のまま: 置き換え・消すは config の中の
// operation 属性で書く。
func (s *Session) EditConfig(ctx context.Context, config string) error {
	_, err := s.rpc(ctx, `<edit-config><target><candidate/></target><config>`+config+`</config></edit-config>`)
	return err
}

// Validate は candidate を確かめる（:validate）。
func (s *Session) Validate(ctx context.Context) error {
	_, err := s.rpc(ctx, `<validate><source><candidate/></source></validate>`)
	return err
}

// CommitConfirmed は commit confirmed（RFC 6241 §8.4）を送る。timeout は秒。
// persist が空でなければ :confirmed-commit:1.1 の <persist>（セッションが
// 切れても残り、persist-id で確定できる）。
func (s *Session) CommitConfirmed(ctx context.Context, timeout int, persist string) error {
	body := `<commit><confirmed/><confirm-timeout>` + strconv.Itoa(timeout) + `</confirm-timeout>`
	if persist != "" {
		body += `<persist>` + persist + `</persist>`
	}
	_, err := s.rpc(ctx, body+`</commit>`)
	return err
}

// Commit は確定の commit。persistID が空でなければ <persist-id> を付ける
// （:confirmed-commit:1.1 の persist で始めた commit confirmed を確定する）。
func (s *Session) Commit(ctx context.Context, persistID string) error {
	body := `<commit/>`
	if persistID != "" {
		body = `<commit><persist-id>` + persistID + `</persist-id></commit>`
	}
	_, err := s.rpc(ctx, body)
	return err
}

// DiscardChanges は candidate を running に戻す。
func (s *Session) DiscardChanges(ctx context.Context) error {
	_, err := s.rpc(ctx, `<discard-changes/>`)
	return err
}

// Close は close-session を送って閉じる（返事は待つが、失敗しても閉じる）。
func (s *Session) Close(ctx context.Context) error {
	_, err := s.rpc(ctx, `<close-session/>`)
	cerr := s.rw.Close()
	if err != nil {
		return err
	}
	return cerr
}

// Abort は何も送らずに閉じる。
func (s *Session) Abort() { _ = s.rw.Close() }

// rpc は body を rpc として送り、対応する rpc-reply を返す。
func (s *Session) rpc(ctx context.Context, body string) ([]byte, error) {
	id := strconv.Itoa(s.nextID)
	s.nextID++
	msg := `<rpc message-id="` + id + `" xmlns="` + BaseNS + `">` + body + `</rpc>`
	var reply []byte
	err := s.guard(ctx, func() error {
		if err := s.f.WriteMessage([]byte(msg)); err != nil {
			return err
		}
		for {
			m, err := s.f.ReadMessage()
			if err != nil {
				return err
			}
			kind, mid, err := messageKind(m)
			if err != nil {
				return err
			}
			if kind == "notification" {
				continue // not subscribed, but harmless
			}
			if kind != "rpc-reply" {
				return fmt.Errorf("netconf: unexpected message <%s>", kind)
			}
			if mid != id {
				return fmt.Errorf("netconf: rpc-reply for message-id %q, want %q", mid, id)
			}
			reply = m
			return nil
		}
	})
	if err != nil {
		return nil, err
	}
	if err := rpcErrorOf(reply); err != nil {
		return nil, err
	}
	return reply, nil
}

// guard runs f; when ctx ends first the stream is closed (f then fails).
func (s *Session) guard(ctx context.Context, f func() error) error {
	stop := context.AfterFunc(ctx, func() { _ = s.rw.Close() })
	err := f()
	if !stop() && ctx.Err() != nil {
		return fmt.Errorf("netconf: %w", ctx.Err())
	}
	return err
}

// messageKind は最上位の要素の名前と message-id。
func messageKind(m []byte) (string, string, error) {
	d := xml.NewDecoder(bytes.NewReader(m))
	for {
		tok, err := d.Token()
		if err != nil {
			return "", "", fmt.Errorf("netconf: unreadable message: %v", err)
		}
		if se, ok := tok.(xml.StartElement); ok {
			mid := ""
			for _, a := range se.Attr {
				if a.Name.Local == "message-id" && (a.Name.Space == "" || a.Name.Space == BaseNS) {
					mid = a.Value
				}
			}
			if se.Name.Space != BaseNS && se.Name.Local != "notification" {
				return "", "", fmt.Errorf("netconf: <%s> is not in the NETCONF namespace", se.Name.Local)
			}
			return se.Name.Local, mid, nil
		}
	}
}

// rpcErrorOf は reply の最初の rpc-error（severity error）を返す。
func rpcErrorOf(reply []byte) error {
	var r struct {
		Errors []struct {
			Type     string `xml:"error-type"`
			Tag      string `xml:"error-tag"`
			Severity string `xml:"error-severity"`
			Message  string `xml:"error-message"`
		} `xml:"rpc-error"`
	}
	if err := xml.Unmarshal(reply, &r); err != nil {
		return fmt.Errorf("netconf: unreadable rpc-reply: %v", err)
	}
	for _, e := range r.Errors {
		sev := strings.TrimSpace(e.Severity)
		if sev == "warning" {
			continue
		}
		return &RPCError{Type: strings.TrimSpace(e.Type), Tag: strings.TrimSpace(e.Tag),
			Severity: sev, Message: strings.TrimSpace(e.Message)}
	}
	return nil
}

// dataOf は rpc-reply の <data> の中身（そのままのバイト。名前空間の宣言は
// <data> と rpc-reply から引き継ぐので、中身だけ切り出さず木にして返す）。
func dataOf(reply []byte) ([]byte, error) {
	top, err := parseXML(reply)
	if err != nil {
		return nil, fmt.Errorf("netconf: unreadable rpc-reply: %v", err)
	}
	if len(top) != 1 {
		return nil, errors.New("netconf: the rpc-reply is not one element")
	}
	for _, c := range top[0].children {
		if c.name.Space == BaseNS && c.name.Local == "data" {
			return []byte(canonical(c.children, false, nil)), nil
		}
	}
	return nil, errors.New("netconf: the rpc-reply has no <data>")
}
