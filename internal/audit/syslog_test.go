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

package audit_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/audit"
	"github.com/tjst-t/sakutei/v2/internal/audit/syslogtest"
)

func forwardingLog(t *testing.T, s *audit.Syslog) (*audit.Log, string) {
	t.Helper()
	dir := t.TempDir()
	l := audit.NewLog(filepath.Join(dir, "audit.jsonl"))
	var errb bytes.Buffer
	l.Forwarder = &audit.Forwarder{Syslog: s, StatePath: filepath.Join(dir, "audit.jsonl.syslog-state"),
		Retry: 50 * time.Millisecond, ErrOut: &errb}
	return l, l.Forwarder.StatePath
}

func bodies(t *testing.T, msgs []syslogtest.Message) []audit.Record {
	t.Helper()
	var out []audit.Record
	for _, m := range msgs {
		var r audit.Record
		if err := json.Unmarshal([]byte(m.Body), &r); err != nil {
			t.Fatalf("message %q: %v", m.Raw, err)
		}
		if m.MsgID != string(r.Kind) {
			t.Errorf("MSGID %q, kind %q", m.MsgID, r.Kind)
		}
		out = append(out, r)
	}
	return out
}

func TestForward_TCP_EachRecordAtOnce(t *testing.T) {
	srv, err := syslogtest.Start("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	l, _ := forwardingLog(t, &audit.Syslog{Addr: srv.Addr, Hostname: "h1"})
	for i := 0; i < 3; i++ {
		if err := l.Append(audit.Record{Kind: audit.KindApply, Resource: fmt.Sprintf("fake//r%d", i), Result: audit.ResultOK}); err != nil {
			t.Fatal(err)
		}
		// 一回性のコマンド（Run していない）でも、Append のたびにその場で届く。
		if got := len(srv.Messages()); got != i+1 {
			t.Fatalf("after append %d the receiver has %d message(s)", i+1, got)
		}
	}
	recs := bodies(t, srv.Messages())
	for i, r := range recs {
		if r.Seq != int64(i+1) || r.Resource != fmt.Sprintf("fake//r%d", i) {
			t.Errorf("message %d = %+v", i, r)
		}
	}
	m := srv.Messages()[0].Raw
	if !strings.HasPrefix(m, "<109>1 ") || !strings.Contains(m, " h1 sakutei - resource.apply [meta sequenceId=\"1\"] {") {
		t.Errorf("RFC 5424 message = %q", m)
	}
}

func TestForward_TLS(t *testing.T) {
	cfg, caPEM, err := syslogtest.SelfSigned()
	if err != nil {
		t.Fatal(err)
	}
	srv, err := syslogtest.Start("127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	l, _ := forwardingLog(t, &audit.Syslog{Addr: srv.Addr, TLS: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"}})
	if err := l.Append(audit.Record{Kind: audit.KindFreezeStarted, Actor: "alice"}); err != nil {
		t.Fatal(err)
	}
	recs := bodies(t, srv.Wait(1, 5*time.Second))
	if len(recs) != 1 || recs[0].Actor != "alice" {
		t.Fatalf("received %+v", recs)
	}

	// 信じない CA の受け手には送らない（記録は手元に残る）。
	other, _, _ := syslogtest.SelfSigned()
	bad, err := syslogtest.Start("127.0.0.1:0", other)
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Stop()
	l2, _ := forwardingLog(t, &audit.Syslog{Addr: bad.Addr, TLS: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"}})
	_ = l2.Append(audit.Record{Kind: audit.KindApply})
	if n, err := l2.Forwarder.Flush(context.Background(), l2.Path); err == nil || n != 0 {
		t.Fatalf("flush to an untrusted receiver: %d, %v", n, err)
	}
	if len(bad.Messages()) != 0 {
		t.Fatal("a record reached an untrusted receiver")
	}
}

// 受け手が止まっている間は手元に貯め、戻ったら順に送り直す。
func TestForward_KeepsAndResendsWhileDown(t *testing.T) {
	srv, err := syslogtest.Start("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	l, statePath := forwardingLog(t, &audit.Syslog{Addr: srv.Addr, Timeout: time.Second})
	_ = l.Append(audit.Record{Kind: audit.KindApply, Resource: "fake//a"})
	if got := len(srv.Wait(1, 5*time.Second)); got != 1 {
		t.Fatalf("received %d", got)
	}
	srv.Stop()
	for _, r := range []string{"fake//b", "fake//c", "fake//d"} {
		if err := l.Append(audit.Record{Kind: audit.KindApply, Resource: r}); err != nil {
			t.Fatalf("append while the receiver is down: %v", err)
		}
	}
	st, _ := audit.LoadForwardState(statePath)
	if st.Seq != 1 {
		t.Fatalf("state while down = %+v, want sent up to seq 1", st)
	}
	if err := srv.Restart(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Forwarder.Run(ctx, l.Path); close(done) }()
	recs := bodies(t, srv.Wait(4, 5*time.Second))
	if len(recs) != 4 {
		t.Fatalf("received %d after the restart, want 4", len(recs))
	}
	for i, want := range []string{"fake//a", "fake//b", "fake//c", "fake//d"} {
		if recs[i].Resource != want || recs[i].Seq != int64(i+1) {
			t.Errorf("message %d = %s seq %d, want %s seq %d", i, recs[i].Resource, recs[i].Seq, want, i+1)
		}
	}
	// Run の間は Append で起こされて届く。
	_ = l.Append(audit.Record{Kind: audit.KindPrune, Resource: "fake//e"})
	if recs := bodies(t, srv.Wait(5, 5*time.Second)); len(recs) != 5 || recs[4].Kind != audit.KindPrune {
		t.Fatalf("after Run: %+v", recs)
	}
	cancel()
	<-done
	srv.Stop()
	st, _ = audit.LoadForwardState(statePath)
	if st.Seq != 5 {
		t.Fatalf("state = %+v, want seq 5", st)
	}
	// 送った所は verify の anchor になる。
	r, err := audit.Verify(l.Path, audit.Anchor{Seq: st.Seq, Hash: st.Hash})
	if err != nil || !r.OK() {
		t.Fatalf("verify with the state: %+v %v", r, err)
	}
}
