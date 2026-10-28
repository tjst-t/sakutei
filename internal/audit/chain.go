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
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// segmentPath は回した n 番目のファイルの名前（0 は path そのもの）。
func segmentPath(path string, n int) string {
	if n == 0 {
		return path
	}
	return fmt.Sprintf("%s.%06d", path, n)
}

// segments は path の今あるファイルの番号を順に返す（無ければ空）。
func segments(path string) ([]int, error) {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	var out []int
	for _, e := range ents {
		name := e.Name()
		if name == base {
			out = append(out, 0)
			continue
		}
		rest, ok := strings.CutPrefix(name, base+".")
		if !ok || len(rest) != 6 {
			continue
		}
		if n, err := strconv.Atoi(rest); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out, nil
}

// lineHash は 1 行（書かれたとおりのバイト列、改行を除く）の SHA-256。
func lineHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// tail は追記の前に要る、ログの末尾の様子。
type tail struct {
	segment int    // 追記するファイルの番号
	size    int64  // そのファイルの大きさ
	seq     int64  // 最後の行の通し番号（無ければ 0）
	hash    string // 最後の行のハッシュ（無ければ空）
	torn    bool   // 追記するファイルが改行で終わっていない
}

func readTail(path string) (tail, error) {
	segs, err := segments(path)
	if err != nil || len(segs) == 0 {
		return tail{}, err
	}
	t := tail{segment: segs[len(segs)-1]}
	for i := len(segs) - 1; i >= 0; i-- {
		raw, size, torn, err := lastLine(segmentPath(path, segs[i]))
		if err != nil {
			return tail{}, err
		}
		if i == len(segs)-1 {
			t.size, t.torn = size, torn
		}
		if raw == nil {
			continue
		}
		t.hash = lineHash(raw)
		var rec Record
		if !torn && json.Unmarshal(raw, &rec) == nil && rec.Seq > 0 {
			t.seq = rec.Seq
			return t, nil
		}
		// 鎖より前の行・切れた行：通し番号は数えて出す。
		n, err := countLines(path)
		if err != nil {
			return tail{}, err
		}
		t.seq = n
		return t, nil
	}
	return t, nil
}

// lastLine は file の最後の空でない行（改行を除く）と、ファイルの大きさ・改行で
// 終わっていないかを返す。
func lastLine(file string) (raw []byte, size int64, torn bool, err error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, 0, false, fmt.Errorf("audit: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, false, fmt.Errorf("audit: %w", err)
	}
	size = st.Size()
	if size > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, size-1); err != nil {
			return nil, 0, false, fmt.Errorf("audit: %w", err)
		}
		torn = last[0] != '\n'
	}
	const chunk = 64 * 1024
	var buf []byte
	for off := size; off > 0; {
		n := int64(chunk)
		if off < n {
			n = off
		}
		off -= n
		b := make([]byte, n)
		if _, err := f.ReadAt(b, off); err != nil && err != io.EOF {
			return nil, 0, false, fmt.Errorf("audit: %w", err)
		}
		buf = append(b, buf...)
		trimmed := bytes.TrimRight(buf, "\r\n")
		if i := bytes.LastIndexByte(trimmed, '\n'); i >= 0 {
			return bytes.TrimRight(trimmed[i+1:], "\r"), size, torn, nil
		}
		if off == 0 && len(trimmed) > 0 {
			return trimmed, size, torn, nil
		}
	}
	return nil, size, torn, nil
}

func countLines(path string) (int64, error) {
	var n int64
	err := scan(path, 0, 0, func(rawLine) error { n++; return nil })
	return n, err
}

// rawLine は scan が渡す 1 行。
type rawLine struct {
	file    string
	segment int
	n       int   // ファイルの中の行番号（1 から。途中から読んだときは読み始めから）
	end     int64 // この行の終わり（改行の次）のファイルの中の位置
	raw     []byte
	rec     Record
	err     error // JSON として読めない
	torn    bool  // 改行で終わっていない（最後の行）
}

// scan は path のファイルを番号順に、fromSeg の fromOff から 1 行ずつ fn に渡す。
// 空の行は飛ばす。
func scan(path string, fromSeg int, fromOff int64, fn func(rawLine) error) error {
	segs, err := segments(path)
	if err != nil {
		return err
	}
	for _, sg := range segs {
		if sg < fromSeg {
			continue
		}
		file := segmentPath(path, sg)
		f, err := os.Open(file)
		if err != nil {
			return fmt.Errorf("audit: %w", err)
		}
		off := int64(0)
		if sg == fromSeg && fromOff > 0 {
			if off, err = f.Seek(fromOff, io.SeekStart); err != nil {
				f.Close()
				return fmt.Errorf("audit: %w", err)
			}
		}
		r := bufio.NewReader(f)
		for n := 1; ; n++ {
			b, rerr := r.ReadBytes('\n')
			off += int64(len(b))
			if len(b) > 0 {
				l := rawLine{file: file, segment: sg, n: n, end: off, torn: b[len(b)-1] != '\n'}
				l.raw = bytes.TrimRight(b, "\r\n")
				if len(bytes.TrimSpace(l.raw)) > 0 {
					l.err = json.Unmarshal(l.raw, &l.rec)
					if err := fn(l); err != nil {
						f.Close()
						return err
					}
				}
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				f.Close()
				return fmt.Errorf("audit: %s: %w", file, rerr)
			}
		}
		f.Close()
	}
	return nil
}
