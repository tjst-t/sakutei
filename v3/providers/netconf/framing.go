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

// framing.go — NETCONF のメッセージの区切り（RFC 6242 §4）。
//
//   - 1.0（end-of-message）: メッセージのあとに "]]>]]>"。hello は常にこれ。
//   - 1.1（chunked）: "\n#<大きさ>\n<中身>" をいくつか並べ、"\n##\n" で終わる。
//     両方が hello で base:1.1 を出したときだけ、hello の後から使う。
//
// 読むメッセージの大きさは MaxMessageBytes まで（越えれば error。機器が
// 際限なく返しても手元のメモリを使い切らない）。

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// MaxMessageBytes は 1 つのメッセージの大きさの上限。
const MaxMessageBytes = 32 << 20

// endOfMessage は 1.0 の区切り。
const endOfMessage = "]]>]]>"

// Framing はメッセージの区切り方。
type Framing int

const (
	// FramingEOM は 1.0 の "]]>]]>"。
	FramingEOM Framing = iota
	// FramingChunked は 1.1 の chunked。
	FramingChunked
)

func (f Framing) String() string {
	if f == FramingChunked {
		return "chunked"
	}
	return "end-of-message"
}

// ErrFraming は区切りが壊れている（相手が NETCONF を話していない・途中で
// 切れた）ことを表す。
var ErrFraming = errors.New("netconf: malformed message framing")

// Framer は 1 本の流れの上でメッセージを読み書きする。
type Framer struct {
	r       *bufio.Reader
	w       io.Writer
	framing Framing
}

// NewFramer は rw の上の Framer（1.0 の区切りから始める）。
func NewFramer(r io.Reader, w io.Writer) *Framer {
	return &Framer{r: bufio.NewReaderSize(r, 64*1024), w: w}
}

// SetFraming は区切り方を変える（hello のあと）。
func (f *Framer) SetFraming(fr Framing) { f.framing = fr }

// Framing は今の区切り方。
func (f *Framer) Framing() Framing { return f.framing }

// WriteMessage は msg を 1 つのメッセージとして書く。
func (f *Framer) WriteMessage(msg []byte) error {
	var buf bytes.Buffer
	if f.framing == FramingChunked {
		if len(msg) == 0 {
			return errors.New("netconf: an empty message cannot be chunked")
		}
		fmt.Fprintf(&buf, "\n#%d\n", len(msg))
		buf.Write(msg)
		buf.WriteString("\n##\n")
	} else {
		if bytes.Contains(msg, []byte(endOfMessage)) {
			return errors.New("netconf: the message contains the end-of-message marker")
		}
		buf.Write(msg)
		buf.WriteString(endOfMessage)
	}
	_, err := f.w.Write(buf.Bytes())
	return err
}

// ReadMessage は次のメッセージを読む。
func (f *Framer) ReadMessage() ([]byte, error) {
	if f.framing == FramingChunked {
		return f.readChunked()
	}
	return f.readEOM()
}

func (f *Framer) readEOM() ([]byte, error) {
	var msg []byte
	for {
		b, err := f.r.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) && len(bytes.TrimSpace(msg)) > 0 {
				return nil, fmt.Errorf("%w: the stream ended in the middle of a message", ErrFraming)
			}
			return nil, err
		}
		msg = append(msg, b)
		if b == '>' && bytes.HasSuffix(msg, []byte(endOfMessage)) {
			return msg[:len(msg)-len(endOfMessage)], nil
		}
		if len(msg) > MaxMessageBytes+len(endOfMessage) {
			return nil, fmt.Errorf("netconf: a message is larger than %d bytes", MaxMessageBytes)
		}
	}
}

func (f *Framer) readChunked() ([]byte, error) {
	var msg []byte
	for first := true; ; first = false {
		// Each chunk (and the end) starts with LF '#'.
		lf, err := f.r.ReadByte()
		if err != nil {
			if !first || !errors.Is(err, io.EOF) {
				return nil, eofInMessage(err)
			}
			return nil, err
		}
		hash, err := f.r.ReadByte()
		if err != nil {
			return nil, eofInMessage(err)
		}
		if lf != '\n' || hash != '#' {
			return nil, fmt.Errorf("%w: want a chunk header", ErrFraming)
		}
		next, err := f.r.ReadByte()
		if err != nil {
			return nil, eofInMessage(err)
		}
		if next == '#' {
			end, err := f.r.ReadByte()
			if err != nil {
				return nil, eofInMessage(err)
			}
			if end != '\n' || first {
				return nil, fmt.Errorf("%w: bad end of chunks", ErrFraming)
			}
			return msg, nil
		}
		// chunk-size = [1-9][0-9]*, at most 4294967295, then LF.
		digits := []byte{next}
		for {
			c, err := f.r.ReadByte()
			if err != nil {
				return nil, eofInMessage(err)
			}
			if c == '\n' {
				break
			}
			digits = append(digits, c)
			if len(digits) > 10 {
				return nil, fmt.Errorf("%w: chunk size too long", ErrFraming)
			}
		}
		if digits[0] < '1' || digits[0] > '9' {
			return nil, fmt.Errorf("%w: bad chunk size %q", ErrFraming, digits)
		}
		n, err := strconv.ParseUint(string(digits), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("%w: bad chunk size %q", ErrFraming, digits)
		}
		if uint64(len(msg))+n > MaxMessageBytes {
			return nil, fmt.Errorf("netconf: a message is larger than %d bytes", MaxMessageBytes)
		}
		start := len(msg)
		msg = append(msg, make([]byte, n)...)
		if _, err := io.ReadFull(f.r, msg[start:]); err != nil {
			return nil, eofInMessage(err)
		}
	}
}

func eofInMessage(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: the stream ended in the middle of a message", ErrFraming)
	}
	return err
}
