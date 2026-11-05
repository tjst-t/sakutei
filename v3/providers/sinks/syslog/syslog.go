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

// Package syslog は Event と audit のハッシュチェーンの行を syslog に 1 件ずつ送る
// sink（module: syslog、設計 §3.9・§5.6、E-L1・N-L1・E-H2）。書式は RFC 5424、TCP の上は
// RFC 6587 の octet-counting、TLS は RFC 5425。v2 の監査の syslog（#187）を移した。
//
// 監査（streams: [audit]）の MSG は Record に書いたままの行（JSON）なので、受信側
// でも seq と prev でハッシュチェーンをたどれる。facility は 13（log audit）、失敗した
// 行は warning、ほかは notice。Events（streams: [events]）の MSG は Event の JSON で、
// facility は 1（user）、firing は warning・resolved は notice・edge Event は info。
// どちらも番号を SD-ID meta の sequenceId に出す（受信側が重複を見分けられる）。
//
// 送信 1 回ごとに接続し、全部を書いて閉じる。届かなければエラーを返し、コアが
// 次の cycle に同じ位置から送り直す（届かない間は Record に貯まる、不変条件 28）。
package syslog

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/providers/sinks/internal/settings"
)

// syslog の PRI。
const (
	facilityUser     = 1
	facilityLogAudit = 13
	severityWarning  = 4
	severityNotice   = 5
	severityInfo     = 6
)

// CloseWait は書き終えたあと受信側が閉じるのを待つ時間の上限。
var CloseWait = 2 * time.Second

// Sink は syslog に送る sink。
type Sink struct {
	addr     string
	tls      *tls.Config // nil は平文の TCP
	hostname string
	app      string
	timeout  time.Duration
}

// Module は接続設定の module: syslog。
//
//	settings:
//	  address: tls://log.example.com:6514   # tcp://… か tls://…
//	  tls: {ca: /etc/sakutei/log-ca.pem, cert: …, key: …, server_name: …}  # 省略可
//	  hostname: sakutei-tokyo                # 省略可（既定はホスト名）
//	  app_name: sakutei                      # 省略可
//	  timeout: 10s
//	streams: [audit]                         # 監査を送るなら（sinks.<名前>.streams）
func Module(m map[string]any) (core.Sink, error) {
	r := settings.New("syslog", m)
	addr := r.Str("address")
	t, tr := r.ReadTLS()
	s := &Sink{hostname: r.Str("hostname"), app: r.Str("app_name"), timeout: r.Duration("timeout", settings.DefaultTimeout)}
	u, err := url.Parse(addr)
	switch {
	case addr == "":
		r.Fail("address に tcp://host:port か tls://host:port を書く")
	case err != nil || u.Host == "" || u.Port() == "" || (u.Scheme != "tcp" && u.Scheme != "tls") || u.Path != "":
		r.Fail("address %q: tcp://host:port か tls://host:port を書く", addr)
	case u.Scheme == "tcp" && t != nil:
		r.Fail("tls は address が tls:// のときだけ書く")
	default:
		s.addr = u.Host
	}
	if err := r.Err(tr); err != nil {
		return nil, err
	}
	if u.Scheme == "tls" {
		c, err := t.Config()
		if err != nil {
			return nil, fmt.Errorf("syslog の sink: %w", err)
		}
		if c.ServerName == "" {
			c.ServerName = u.Hostname()
		}
		s.tls = c
	}
	if s.hostname == "" {
		s.hostname, _ = os.Hostname()
	}
	if s.app == "" {
		s.app = "sakutei"
	}
	return s, nil
}

// Deliver は Event を 1 件ずつ送る。
func (s *Sink) Deliver(ctx context.Context, batch []core.Event) error {
	msgs := make([][]byte, 0, len(batch))
	for _, e := range batch {
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		sev := severityInfo
		switch {
		case e.Type == core.StateEvent && e.State == core.Firing:
			sev = severityWarning
		case e.Type == core.StateEvent:
			sev = severityNotice
		}
		msgs = append(msgs, s.format(facilityUser, sev, e.At, e.Kind, e.Seq, line))
	}
	return s.send(ctx, msgs)
}

// DeliverAudit は監査の行を 1 件ずつ送る（E-L1・N-L1）。
func (s *Sink) DeliverAudit(ctx context.Context, batch []core.AuditLine) error {
	msgs := make([][]byte, 0, len(batch))
	for _, l := range batch {
		var e struct {
			At     time.Time `json:"at"`
			Kind   string    `json:"kind"`
			Result string    `json:"result"`
		}
		_ = json.Unmarshal(l.Line, &e) // 読めなくても行はそのまま送る
		sev := severityNotice
		if e.Result == "error" {
			sev = severityWarning
		}
		msgs = append(msgs, s.format(facilityLogAudit, sev, e.At, e.Kind, l.Seq, l.Line))
	}
	return s.send(ctx, msgs)
}

// format は 1 件を RFC 5424 の 1 メッセージにする。
func (s *Sink) format(facility, sev int, at time.Time, kind string, seq uint64, msg []byte) []byte {
	ts := "-"
	if !at.IsZero() {
		ts = at.UTC().Format("2006-01-02T15:04:05.000000Z07:00")
	}
	sd := "-"
	if seq > 0 && seq <= 2147483647 {
		sd = `[meta sequenceId="` + strconv.FormatUint(seq, 10) + `"]`
	}
	head := fmt.Sprintf("<%d>1 %s %s %s - %s %s ", facility*8+sev, ts, field(s.hostname, 255), field(s.app, 48), field(kind, 32), sd)
	return append([]byte(head), msg...)
}

// field は HEADER の field に使える形（印字できる ASCII、空白なし、長さの上限）にする。
func field(v string, max int) string {
	b := make([]byte, 0, len(v))
	for i := 0; i < len(v) && len(b) < max; i++ {
		if c := v[i]; c > 32 && c < 127 {
			b = append(b, c)
		}
	}
	if len(b) == 0 {
		return "-"
	}
	return string(b)
}

// send は接続して octet-counting で全部を書き、閉じる。
func (s *Sink) send(ctx context.Context, msgs [][]byte) error {
	if len(msgs) == 0 {
		return nil
	}
	d := &net.Dialer{Timeout: s.timeout}
	var conn net.Conn
	var err error
	if s.tls != nil {
		conn, err = (&tls.Dialer{NetDialer: d, Config: s.tls}).DialContext(ctx, "tcp", s.addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", s.addr)
	}
	if err != nil {
		return fmt.Errorf("syslog %s に接続できない: %w", s.addr, err)
	}
	defer conn.Close()
	deadline := time.Now().Add(s.timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetWriteDeadline(deadline)
	for i, m := range msgs {
		if _, err := conn.Write(append([]byte(strconv.Itoa(len(m))+" "), m...)); err != nil {
			return fmt.Errorf("syslog %s に書けない（%d/%d 件目）: %w", s.addr, i+1, len(msgs), err)
		}
	}
	// 閉じる前に書き込みの終わりを伝え、受信側が読み終えて閉じるのを短く待つ
	// （RST で末尾を落とさない）。
	if tc, ok := conn.(interface{ CloseWrite() error }); ok {
		if err := tc.CloseWrite(); err != nil {
			return fmt.Errorf("syslog %s: %w", s.addr, err)
		}
		if linger := time.Now().Add(CloseWait); linger.Before(deadline) {
			deadline = linger
		}
		_ = conn.SetReadDeadline(deadline)
		var buf [1]byte
		_, _ = conn.Read(buf[:])
	}
	return nil
}
