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

package record

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
)

// MaxAuditFile は監査のファイル 1 つの大きさの目安。超えれば新しいファイルへ回す
// （書き換え・切り詰めはしない、不変条件 28）。
var MaxAuditFile int64 = 8 << 20

// Entry は監査の 1 行（§5.1、E-L1・N-L1）。値は書かず、変えた field の名前だけ。
// Seq と Prev は Append が付ける。Prev は前の行のハッシュで、行をハッシュチェーンにする。
type Entry struct {
	Seq      uint64    `json:"seq"`
	Prev     string    `json:"prev"`
	At       time.Time `json:"at"`
	Kind     string    `json:"kind"` // resource.apply・resource.delete・resource.prune・approval.*・freeze.*・procedure.*
	Resource string    `json:"resource,omitempty"`
	Fields   []string  `json:"fields,omitempty"`
	Result   string    `json:"result,omitempty"` // ok・error・refused
	Error    string    `json:"error,omitempty"`
	Policies []string  `json:"policies,omitempty"`
	Reason   string    `json:"reason,omitempty"` // N-J7・N-L8
	// Stage 5：承認・一時的な Policy・Procedure（E-L1・E-L2・E-L6・N-L3・E-J4）。
	By         string   `json:"by,omitempty"`          // した人（承認者・freeze した人・start した人）
	Request    string   `json:"request,omitempty"`     // 承認の依頼の ID
	ApprovedBy []string `json:"approved_by,omitempty"` // 数えた承認者
	Procedure  string   `json:"procedure,omitempty"`
	Run        string   `json:"run_id,omitempty"`
	Step       string   `json:"step,omitempty"`
	Op         string   `json:"op,omitempty"`
	// Stage 7：採番（alloc.assign・alloc.release、N-B13）。払い出した・返した値はプールの
	// 番号で、secret ではない。
	Pool  string `json:"pool,omitempty"`
	Value *int64 `json:"value,omitempty"`
}

// Audit は audit のハッシュチェーンへの追記。
type Audit struct {
	dir  string
	seq  uint64
	prev string
	file int   // 今のファイルの番号（無ければ 0）
	size int64 // 今のファイルの大きさ
}

// Audit は audit のハッシュチェーンを開き、最後の行を読んで続きを書ける状態にする。
// 最後の行が切れている・読めないときはエラーにする（チェーンを書き換えて直さない）。
func (r *Record) Audit() (*Audit, error) {
	dir := filepath.Join(r.dir, auditDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	files, err := auditFiles(dir)
	if err != nil {
		return nil, err
	}
	a := &Audit{dir: dir}
	if len(files) == 0 {
		return a, nil
	}
	last := files[len(files)-1]
	data, err := os.ReadFile(filepath.Join(dir, fileName(last)))
	if err != nil {
		return nil, err
	}
	a.file, a.size = last, int64(len(data))
	if len(data) == 0 {
		// 回した直後で空のファイル：前のファイルの最後の行から続ける。
		if len(files) == 1 {
			return a, nil
		}
		prevData, err := os.ReadFile(filepath.Join(dir, fileName(files[len(files)-2])))
		if err != nil {
			return nil, err
		}
		data = prevData
	}
	if data[len(data)-1] != '\n' {
		return nil, fmt.Errorf("監査 %s の最後の行が切れている（audit verify で確認する）", fileName(last))
	}
	lines := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
	line := lines[len(lines)-1]
	var e Entry
	if err := json.Unmarshal(line, &e); err != nil {
		return nil, fmt.Errorf("監査 %s の最後の行が読めない: %w", fileName(last), err)
	}
	a.seq, a.prev = e.Seq, hashLine(line)
	return a, nil
}

// Append は 1 行を足す。書けたら Seq を返す。
func (a *Audit) Append(e Entry) (uint64, error) {
	e.Seq, e.Prev = a.seq+1, a.prev
	line, err := json.Marshal(e)
	if err != nil {
		return 0, err
	}
	file := a.file
	if file == 0 || (a.size > 0 && a.size+int64(len(line))+1 > MaxAuditFile) {
		file++
	}
	f, err := os.OpenFile(filepath.Join(a.dir, fileName(file)), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	if file != a.file {
		a.file, a.size = file, 0
	}
	a.size += int64(len(line)) + 1
	a.seq, a.prev = e.Seq, hashLine(line)
	return e.Seq, nil
}

// Verify は audit のハッシュチェーンを頭から辿り、行の数を返す。番号の飛び・ファイルの欠け・
// 書き換え（前の行のハッシュの不一致）・切れた行を見つけたらエラーを返す
// （E-L1・N-L1・不変条件 28）。Record を作らず、ロックも取らない。
func Verify(dir string) (int, error) {
	adir := filepath.Join(dir, auditDir)
	files, err := auditFiles(adir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	var seq uint64
	prev := ""
	for i, n := range files {
		if n != i+1 {
			return int(seq), fmt.Errorf("監査のファイル %s が無い（次は %s）", fileName(i+1), fileName(n))
		}
		data, err := os.ReadFile(filepath.Join(adir, fileName(n)))
		if err != nil {
			return int(seq), err
		}
		if len(data) > 0 && data[len(data)-1] != '\n' {
			return int(seq), fmt.Errorf("%s の最後の行が切れている", fileName(n))
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 0, 64<<10), int(MaxAuditFile)+(1<<20))
		lineNo := 0
		for sc.Scan() {
			lineNo++
			line := sc.Bytes()
			var e Entry
			if err := json.Unmarshal(line, &e); err != nil {
				return int(seq), fmt.Errorf("%s:%d: 読めない: %v", fileName(n), lineNo, err)
			}
			if e.Seq != seq+1 {
				return int(seq), fmt.Errorf("%s:%d: seq %d（%d のはず）", fileName(n), lineNo, e.Seq, seq+1)
			}
			if e.Prev != prev {
				return int(seq), fmt.Errorf("%s:%d: seq %d の prev が前の行のハッシュと合わない（前の行が書き換えられた）", fileName(n), lineNo, e.Seq)
			}
			seq, prev = e.Seq, hashLine(line)
		}
		if err := sc.Err(); err != nil {
			return int(seq), fmt.Errorf("%s: %w", fileName(n), err)
		}
	}
	return int(seq), nil
}

func hashLine(line []byte) string {
	sum := sha256.Sum256(line)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func fileName(n int) string { return fmt.Sprintf("%06d.jsonl", n) }

// auditFiles は監査のファイルの番号を小さい順に返す。
func auditFiles(dir string) ([]int, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []int
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSuffix(name, ".jsonl"))
		if err != nil || n <= 0 || fileName(n) != name {
			return nil, fmt.Errorf("監査のディレクトリに知らないファイル %s", name)
		}
		out = append(out, n)
	}
	sort.Ints(out)
	return out, nil
}

// ReadAudit は audit のハッシュチェーンの since より後の行を max 行まで順に返す（監査の sink、
// E-L1・N-L1）。ファイルの初めの番号で読み始めるファイルを選び、全部は読まない。
// 最後の切れた行（書いている途中）は返さない。Record を作らず、ロックも取らない。
func ReadAudit(dir string, since uint64, max int) ([]core.AuditLine, error) {
	adir := filepath.Join(dir, auditDir)
	files, err := auditFiles(adir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	// 初めの番号が since+1 以下のうち最後のファイルから読む。
	start := 0
	for i, n := range files {
		first, ok, err := firstAuditSeq(filepath.Join(adir, fileName(n)))
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if first > since+1 {
			break
		}
		start = i
	}
	var out []core.AuditLine
	for _, n := range files[start:] {
		data, err := os.ReadFile(filepath.Join(adir, fileName(n)))
		if err != nil {
			return nil, err
		}
		for len(data) > 0 {
			i := bytes.IndexByte(data, '\n')
			if i < 0 {
				break // 書いている途中の行
			}
			line := data[:i]
			data = data[i+1:]
			var e struct {
				Seq uint64 `json:"seq"`
			}
			if err := json.Unmarshal(line, &e); err != nil {
				return nil, fmt.Errorf("監査 %s の行が読めない: %w", fileName(n), err)
			}
			if e.Seq <= since {
				continue
			}
			out = append(out, core.AuditLine{Seq: e.Seq, Line: append([]byte(nil), line...)})
			if len(out) >= max {
				return out, nil
			}
		}
	}
	return out, nil
}

// firstAuditSeq はファイルの最初の行の番号。空なら false。
func firstAuditSeq(path string) (uint64, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	line, _ := r.ReadBytes('\n')
	if len(line) == 0 || line[len(line)-1] != '\n' {
		return 0, false, nil
	}
	var e struct {
		Seq uint64 `json:"seq"`
	}
	if err := json.Unmarshal(line, &e); err != nil {
		return 0, false, fmt.Errorf("監査 %s の最初の行が読めない: %w", filepath.Base(path), err)
	}
	return e.Seq, true, nil
}
