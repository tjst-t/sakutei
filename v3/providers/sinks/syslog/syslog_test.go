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

package syslog_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/providers/internal/testcert"
	"github.com/tjst-t/sakutei/v3/providers/sinks/syslog"
)

// receiver は偽の syslog の受信側（octet-counting の TCP か TLS）。
type receiver struct {
	ln   net.Listener
	mu   sync.Mutex
	msgs []string
}

func listen(t *testing.T, tc *tls.Config) *receiver {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if tc != nil {
		ln = tls.NewListener(ln, tc)
	}
	r := &receiver{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go r.read(c)
		}
	}()
	return r
}

func (r *receiver) read(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	for {
		n, err := br.ReadString(' ')
		if err != nil {
			return
		}
		size, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil {
			return
		}
		buf := make([]byte, size)
		if _, err := io.ReadFull(br, buf); err != nil {
			return
		}
		r.mu.Lock()
		r.msgs = append(r.msgs, string(buf))
		r.mu.Unlock()
	}
}

func (r *receiver) wait(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		if len(r.msgs) >= n {
			out := append([]string(nil), r.msgs...)
			r.msgs = nil
			r.mu.Unlock()
			return out
		}
		r.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%d 件届かない", n)
	return nil
}

func open(t *testing.T, settings map[string]any) *syslog.Sink {
	t.Helper()
	s, err := syslog.Module(settings)
	if err != nil {
		t.Fatal(err)
	}
	return s.(*syslog.Sink)
}

// E-L1・N-L1：監査の行を RFC 5424 で 1 件ずつ、行をそのまま MSG にして送る
// （受信側でハッシュチェーンをたどれる）。Events も送る。受信側が落ちていれば
// エラー（コアが位置を保って送り直す）。
func TestSyslogTCP(t *testing.T) {
	r := listen(t, nil)
	s := open(t, map[string]any{"address": "tcp://" + r.ln.Addr().String(), "hostname": "sakutei-tokyo"})
	ctx := context.Background()
	lines := []core.AuditLine{
		{Seq: 1, Line: []byte(`{"seq":1,"prev":"","at":"2026-11-05T13:41:00Z","kind":"resource.apply","resource":"dns/record/www","result":"ok"}`)},
		{Seq: 2, Line: []byte(`{"seq":2,"prev":"sha256:ab","at":"2026-11-05T13:41:01Z","kind":"resource.prune","result":"error"}`)},
	}
	if err := s.DeliverAudit(ctx, lines); err != nil {
		t.Fatal(err)
	}
	got := r.wait(t, 2)
	want0 := `<109>1 2026-11-05T13:41:00.000000Z sakutei-tokyo sakutei - resource.apply [meta sequenceId="1"] ` + string(lines[0].Line)
	if got[0] != want0 {
		t.Errorf("監査 1:\n%s\n%s", got[0], want0)
	}
	if !strings.HasPrefix(got[1], "<108>1 ") || !strings.HasSuffix(got[1], string(lines[1].Line)) {
		t.Errorf("失敗した行は warning: %s", got[1])
	}

	at := time.Date(2026, 11, 5, 13, 41, 0, 0, time.UTC)
	if err := s.Deliver(ctx, []core.Event{
		{Seq: 5, Kind: "Drift", Type: core.StateEvent, State: core.Firing, Resource: "dns/record/www", At: at},
		{Seq: 6, Kind: "CycleDone", Type: core.IncidentEvent, At: at},
	}); err != nil {
		t.Fatal(err)
	}
	got = r.wait(t, 2)
	if !strings.HasPrefix(got[0], `<12>1 2026-11-05T13:41:00.000000Z sakutei-tokyo sakutei - Drift [meta sequenceId="5"] {"seq":5,"kind":"Drift"`) {
		t.Errorf("Event: %s", got[0])
	}
	if !strings.HasPrefix(got[1], "<14>1 ") {
		t.Errorf("edge Event は info: %s", got[1])
	}

	r.ln.Close()
	if err := s.DeliverAudit(ctx, lines); err == nil || !strings.Contains(err.Error(), "接続できない") {
		t.Fatalf("受信側が落ちている: %v", err)
	}
}

func TestSyslogTLS(t *testing.T) {
	f := testcert.Write(t, t.TempDir())
	cert, err := tls.LoadX509KeyPair(f.ServerCert, f.ServerKey)
	if err != nil {
		t.Fatal(err)
	}
	r := listen(t, &tls.Config{Certificates: []tls.Certificate{cert}})
	_, port, _ := net.SplitHostPort(r.ln.Addr().String())
	s := open(t, map[string]any{"address": "tls://localhost:" + port, "tls": map[string]any{"ca": f.CA}})
	if err := s.DeliverAudit(context.Background(), []core.AuditLine{{Seq: 1, Line: []byte(`{"seq":1,"kind":"approval.approve"}`)}}); err != nil {
		t.Fatal(err)
	}
	if got := r.wait(t, 1); !strings.Contains(got[0], `approval.approve [meta sequenceId="1"]`) {
		t.Errorf("%s", got[0])
	}
	// 信頼していない CA の受信側には送らない。
	other := testcert.Write(t, t.TempDir())
	s = open(t, map[string]any{"address": "tls://localhost:" + port, "tls": map[string]any{"ca": other.CA}})
	if err := s.DeliverAudit(context.Background(), []core.AuditLine{{Seq: 1, Line: []byte(`{}`)}}); err == nil {
		t.Fatal("検証できない証明書の受信側に送った")
	}
}

func TestSyslogRejectsSettings(t *testing.T) {
	for _, c := range []struct {
		settings map[string]any
		want     string
	}{
		{map[string]any{}, "address に"},
		{map[string]any{"address": "udp://log:514"}, "tcp://host:port か tls://host:port"},
		{map[string]any{"address": "tcp://log"}, "tcp://host:port か tls://host:port"},
		{map[string]any{"address": "tcp://log:514", "tls": map[string]any{"ca": "x"}}, "tls は address が tls://"},
		{map[string]any{"address": "tls://log:6514", "tls": map[string]any{"cert": "x"}}, "cert と key を両方"},
		{map[string]any{"address": "tcp://log:514", "facility": 3}, "不明な field facility"},
	} {
		if _, err := syslog.Module(c.settings); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v（%q を含むはず）", c.settings, err, c.want)
		}
	}
}
