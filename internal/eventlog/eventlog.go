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

// Package eventlog は core.Event を出す（--log-format text|json、§22.3, §27.2）。
//
// 知らせはすべて core.Event を作ってから Emit で出す。出力先は今の Out / ErrOut の
// io.Writer のまま（sink の分離は後の段）。
//
//   - text: Event.TextLine() をそのまま 1 行で書く（従来の文と同じ）。
//   - json: 1 Event 1 行の JSON lines。Event でないログ行（fmt.Fprintf で書く診断・
//     進捗）も、Writer を通せば Event と同じ形（kind=Log）の 1 行になる。
//
// Emit の先が Writer でない io.Writer（テストの bytes.Buffer など）なら text で書く。
package eventlog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// Format は出力の形。
type Format string

const (
	FormatText Format = "text"
	FormatJSON Format = "json"
)

// ParseFormat は --log-format の値を読む。
func ParseFormat(s string) (Format, error) {
	switch Format(s) {
	case FormatText, FormatJSON:
		return Format(s), nil
	}
	return "", fmt.Errorf("unknown --log-format %q (text|json)", s)
}

// Emitter は Event を受け取れる io.Writer。
type Emitter interface {
	io.Writer
	Emit(ev core.Event)
}

// Emit は ev を w に出す。w が Emitter ならその形で、そうでなければ text で書く。
func Emit(w io.Writer, ev core.Event) {
	if e, ok := w.(Emitter); ok {
		e.Emit(ev)
		return
	}
	fmt.Fprintln(w, ev.TextLine())
}

// Writer は Out / ErrOut を包む Emitter。Write と Emit は互いに排他（行が混ざらない）。
type Writer struct {
	mu       sync.Mutex
	w        io.Writer
	format   Format
	severity core.Severity
	buf      []byte
	now      func() time.Time
}

// New は w を format で包む。severity は Event でないログ行（kind=Log）の重さ
// （標準出力は info、標準エラーは warning）。
func New(w io.Writer, format Format, severity core.Severity) *Writer {
	return &Writer{w: w, format: format, severity: severity, now: time.Now}
}

// Format は出力の形を返す。
func (x *Writer) Format() Format { return x.format }

// Write は text ではそのまま書き、json では 1 行ずつ kind=Log の行にする。
func (x *Writer) Write(p []byte) (int, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.format != FormatJSON {
		return x.w.Write(p)
	}
	x.buf = append(x.buf, p...)
	for {
		i := bytes.IndexByte(x.buf, '\n')
		if i < 0 {
			break
		}
		line := string(x.buf[:i])
		x.buf = x.buf[i+1:]
		x.writeLogLocked(line)
	}
	return len(p), nil
}

// Flush は改行で終わっていない書きかけの行を出す。
func (x *Writer) Flush() {
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.buf) > 0 {
		line := string(x.buf)
		x.buf = nil
		x.writeLogLocked(line)
	}
}

func (x *Writer) writeLogLocked(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	x.writeEventLocked(core.Event{Kind: core.EventLog, Severity: x.severity, Message: line})
}

// Emit は ev を 1 行で出す。
func (x *Writer) Emit(ev core.Event) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.format != FormatJSON {
		fmt.Fprintln(x.w, ev.TextLine())
		return
	}
	x.writeEventLocked(ev)
}

func (x *Writer) writeEventLocked(ev core.Event) {
	if ev.Time.IsZero() {
		ev.Time = x.now()
	}
	b, err := json.Marshal(ev)
	if err != nil {
		// Event の JSON 化は失敗しない（文字列と数だけ）。万一のときも行は落とさない。
		fmt.Fprintln(x.w, ev.TextLine())
		return
	}
	b = append(b, '\n')
	_, _ = x.w.Write(b)
}

// Stamp は w に出す Event に周の番号と revision を付ける Emitter を返す（既に
// 値のある欄はそのまま）。Event でない書き込みは w にそのまま渡す。
func Stamp(w io.Writer, cycle int, rev core.RevisionInfo) io.Writer {
	return &stamped{w: w, cycle: cycle, rev: rev}
}

type stamped struct {
	w     io.Writer
	cycle int
	rev   core.RevisionInfo
}

func (s *stamped) Write(p []byte) (int, error) { return s.w.Write(p) }

func (s *stamped) Emit(ev core.Event) {
	if ev.Cycle == 0 {
		ev.Cycle = s.cycle
	}
	if ev.IntentRevision == "" {
		ev.IntentRevision = s.rev.IntentRevision
	}
	if ev.RenderedRevision == "" {
		ev.RenderedRevision = s.rev.RenderedRevision
	}
	Emit(s.w, ev)
}
