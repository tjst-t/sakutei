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

package audit

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/filelock"
)

// Syslog は監査の記録を 1 件ずつ送る syslog の受け手（RFC 5424 の書式、TCP の上は
// RFC 6587 の octet-counting、TLS は RFC 5425）。
type Syslog struct {
	// Addr は host:port。
	Addr string
	// TLS が nil でなければ TLS でつなぐ（nil は平文の TCP）。
	TLS *tls.Config
	// Hostname と AppName は HEADER に出す（空なら os.Hostname と "sakutei"）。
	Hostname, AppName string
	// Timeout はつなぐ・1 件書くときの時間の上限（0 は 10 秒）。
	Timeout time.Duration
}

func (s *Syslog) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return 10 * time.Second
}

func (s *Syslog) dial(ctx context.Context) (net.Conn, error) {
	d := &net.Dialer{Timeout: s.timeout()}
	if s.TLS != nil {
		return (&tls.Dialer{NetDialer: d, Config: s.TLS}).DialContext(ctx, "tcp", s.Addr)
	}
	return d.DialContext(ctx, "tcp", s.Addr)
}

// syslog の PRI：facility 13（log audit）。失敗した Apply・prune は warning（4）、
// ほかは notice（5）。
const (
	facilityLogAudit = 13
	severityWarning  = 4
	severityNotice   = 5
)

// Format は 1 件の記録を RFC 5424 の 1 メッセージにする。MSG は書かれたとおりの
// 行（JSON）なので、受け手の側でも seq と prev で鎖をたどれる。seq は登録済みの
// SD-ID meta の sequenceId にも出す。
func (s *Syslog) Format(raw []byte, rec Record) []byte {
	sev := severityNotice
	if rec.Result == ResultFailed {
		sev = severityWarning
	}
	host := s.Hostname
	if host == "" {
		host, _ = os.Hostname()
	}
	app := s.AppName
	if app == "" {
		app = "sakutei"
	}
	ts := "-"
	if !rec.Time.IsZero() {
		ts = rec.Time.UTC().Format("2006-01-02T15:04:05.000000Z07:00")
	}
	msgid := sdName(string(rec.Kind), 32)
	sd := "-"
	if rec.Seq > 0 && rec.Seq <= 2147483647 {
		sd = `[meta sequenceId="` + strconv.FormatInt(rec.Seq, 10) + `"]`
	}
	head := fmt.Sprintf("<%d>1 %s %s %s - %s %s ", facilityLogAudit*8+sev, ts,
		sdName(host, 255), sdName(app, 48), msgid, sd)
	return append([]byte(head), raw...)
}

// sdName は HEADER の欄に使える形（印字できる ASCII、空白なし、長さの上限）にする。
func sdName(v string, max int) string {
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

// ForwardState は送り先へ送り終えた所（S4 の外の、書き換える小さなファイル）。
// Seq と Hash は送った最後の行で、verify の Anchor になる（ログがそこまで無い・
// その行が違うなら、送ったあとに切り詰め・書き換えられた）。
type ForwardState struct {
	Seq     int64  `json:"seq"`
	Hash    string `json:"hash"`
	Segment int    `json:"segment"`
	Offset  int64  `json:"offset"`
}

// LoadForwardState は path の送り先の状態を読む（無ければ零）。
func LoadForwardState(path string) (ForwardState, error) {
	var st ForwardState
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("audit: forward state: %w", err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("audit: forward state %s: %w", path, err)
	}
	return st, nil
}

func saveForwardState(path string, st ForwardState) error {
	b, _ := json.Marshal(st)
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("audit: forward state: %w", err)
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("audit: forward state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("audit: forward state: %w", err)
	}
	tmp.Close()
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("audit: forward state: %w", err)
	}
	return nil
}

// Forwarder は監査のログを外（syslog）へすぐ送る（#187、§26.3 外部即時 push）。
//
// 貯め場はログそのもの：送り終えた所（ForwardState）を StatePath に持ち、そこより
// あとの行を順に送る。受け手が止まっている間は送れなかった行がログに残り、次に
// 送る（Run の間は Retry ごと、Run していなければ次の Append のとき）。送り先の
// 状態は複数のプロセスで分け合う（<StatePath>.lock の flock）。届けるのは「少なく
// とも 1 回」で、途中で落ちれば同じ行をもう一度送ることがある（受け手は meta の
// sequenceId で重なりを見分けられる）。
type Forwarder struct {
	Syslog *Syslog
	// StatePath は送り終えた所のファイル。
	StatePath string
	// Retry は送れなかったあとに送り直すまでの間（0 は 30 秒）。
	Retry time.Duration
	// ErrOut に送れないことを知らせる（nil は捨てる）。
	ErrOut io.Writer

	mu        sync.Mutex
	kick      chan struct{}
	running   bool
	downUntil time.Time
	lastErr   string
}

func (f *Forwarder) retry() time.Duration {
	if f.Retry > 0 {
		return f.Retry
	}
	return 30 * time.Second
}

func (f *Forwarder) warn(err error) {
	f.mu.Lock()
	msg := err.Error()
	same := msg == f.lastErr
	f.lastErr = msg
	f.mu.Unlock()
	if f.ErrOut != nil && !same {
		fmt.Fprintf(f.ErrOut, "sakutei: warn: audit: cannot forward to syslog %s: %v; the records stay in the audit log and are sent again\n", f.Syslog.Addr, err)
	}
}

// notify は Append のあとに呼ばれる。Run の間は送る側を起こすだけ。Run していない
// （一回性のコマンド）ならその場で送る。ただし送れなかったあと Retry の間は試さない
// （止まっている受け手で Append ごとに待たない）。
func (f *Forwarder) notify(l *Log) {
	f.mu.Lock()
	if f.running {
		select {
		case f.kick <- struct{}{}:
		default:
		}
		f.mu.Unlock()
		return
	}
	if time.Now().Before(f.downUntil) {
		f.mu.Unlock()
		return
	}
	f.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*f.Syslog.timeout())
	defer cancel()
	if _, err := f.Flush(ctx, l.Path); err != nil {
		f.mu.Lock()
		f.downUntil = time.Now().Add(f.retry())
		f.mu.Unlock()
		f.warn(err)
	}
}

// Run は ctx が終わるまで、Append で起こされるたび・Retry ごとに送り残しを送る
// （ほかのプロセスが書いた行も送る）。常駐で使う。
func (f *Forwarder) Run(ctx context.Context, path string) {
	f.mu.Lock()
	f.running = true
	if f.kick == nil {
		f.kick = make(chan struct{}, 1)
	}
	kick := f.kick
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.running = false
		f.mu.Unlock()
	}()
	for {
		if _, err := f.Flush(ctx, path); err != nil && ctx.Err() == nil {
			f.warn(err)
		} else if err == nil {
			f.mu.Lock()
			f.lastErr = ""
			f.mu.Unlock()
		}
		t := time.NewTimer(f.retry())
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-kick:
		case <-t.C:
		}
		t.Stop()
	}
}

// Flush は送り終えた所よりあとの行を順に送り、送れた件数を返す。途中で送れなく
// なれば、そこまでを送り終えた所として残して error を返す。改行で終わっていない
// 最後の行（書いている途中）は送らない。
func (f *Forwarder) Flush(ctx context.Context, path string) (int, error) {
	unlock, err := filelock.Lock(f.StatePath)
	if err != nil {
		return 0, fmt.Errorf("audit: %w", err)
	}
	defer unlock()
	st, err := LoadForwardState(f.StatePath)
	if err != nil {
		return 0, err
	}
	var (
		conn net.Conn
		sent int
		done = st
	)
	stop := errors.New("stop")
	err = scan(path, st.Segment, st.Offset, func(l rawLine) error {
		if l.torn {
			return stop
		}
		if conn == nil {
			c, err := f.Syslog.dial(ctx)
			if err != nil {
				return err
			}
			conn = c
		}
		msg := f.Syslog.Format(l.raw, l.rec)
		frame := append([]byte(strconv.Itoa(len(msg))+" "), msg...)
		_ = conn.SetWriteDeadline(time.Now().Add(f.Syslog.timeout()))
		if _, err := conn.Write(frame); err != nil {
			return err
		}
		sent++
		seq := l.rec.Seq
		if seq == 0 {
			seq = done.Seq + 1
		}
		done = ForwardState{Seq: seq, Hash: lineHash(l.raw), Segment: l.segment, Offset: l.end}
		return ctx.Err()
	})
	if conn != nil {
		if cerr := conn.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	if errors.Is(err, stop) {
		err = nil
	}
	if done != st {
		if serr := saveForwardState(f.StatePath, done); serr != nil && err == nil {
			err = serr
		}
	}
	return sent, err
}
