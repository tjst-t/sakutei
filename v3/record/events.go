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

// MaxOutboxFile は送り出しの列のファイル 1 つの大きさの目安。超えれば新しい
// ファイルへ回す。全部の sink が送ったファイルは消す（§5.1）。
var MaxOutboxFile int64 = 1 << 20

const (
	eventsDir  = "events"
	stateFile  = "state.json"
	outboxDir  = "outbox"
	cursorsDir = "cursors"
	healthFile = "health.json"
	wakeFile   = "wake"
)

// EventState は通知の状態（§3.9、events/state.json）：firing の Event の一覧と、
// 最後に積んだ Event の番号（E-H1・E-O3）。観測の値は置かない（不変条件 1）。
type EventState struct {
	Seq    uint64       `json:"seq"`
	Firing []core.Event `json:"firing"`
}

// EventState は events/state.json を読む。無ければ空。
func (r *Record) EventState() (EventState, error) {
	path := filepath.Join(r.dir, eventsDir, stateFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return EventState{}, nil
	}
	if err != nil {
		return EventState{}, err
	}
	var st EventState
	if err := json.Unmarshal(data, &st); err != nil {
		return EventState{}, fmt.Errorf("%s: %w", path, err)
	}
	return st, nil
}

// WriteEventState は events/state.json を書く。ロックを持つ処理だけが呼ぶ。
func (r *Record) WriteEventState(st EventState) error {
	if st.Firing == nil {
		st.Firing = []core.Event{}
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(r.dir, eventsDir), 0o755); err != nil {
		return err
	}
	return writeFile(filepath.Join(r.dir, eventsDir, stateFile), append(data, '\n'))
}

// LastOutboxSeq は送り出しの列の最後の Event の番号（無ければ 0）。state.json を
// 書く前に止まっても番号を二度使わないよう、次の番号はこれと state.json の大きい
// ほうから数える。
func (r *Record) LastOutboxSeq() (uint64, error) {
	dir := filepath.Join(r.dir, eventsDir, outboxDir)
	files, err := outboxFiles(dir)
	if err != nil || len(files) == 0 {
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		return 0, err
	}
	for i := len(files) - 1; i >= 0; i-- {
		evs, err := readOutboxFile(filepath.Join(dir, outboxName(files[i])))
		if err != nil {
			return 0, err
		}
		if len(evs) > 0 {
			return evs[len(evs)-1].Seq, nil
		}
	}
	return 0, nil
}

// AppendOutbox は番号を付けた Event を送り出しの列に足す（§5.1、E-H2・N-H2）。
// 最後のファイルが大きいか、最後の行が切れていれば新しいファイルに書く。
func (r *Record) AppendOutbox(evs []core.Event) error {
	if len(evs) == 0 {
		return nil
	}
	dir := filepath.Join(r.dir, eventsDir, outboxDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	files, err := outboxFiles(dir)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	for _, e := range evs {
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	name := outboxName(evs[0].Seq)
	if len(files) > 0 {
		last := filepath.Join(dir, outboxName(files[len(files)-1]))
		data, err := os.ReadFile(last)
		if err != nil {
			return err
		}
		if (len(data) == 0 || data[len(data)-1] == '\n') && int64(len(data)+buf.Len()) <= MaxOutboxFile {
			name = outboxName(files[len(files)-1])
		}
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ReadOutbox は送り出しの列から番号が since より大きい Event を、max 件まで
// 番号の順に読む（max が 0 以下なら全部）。ロックを取らず、Record を作らない
// （読み取り HTTP の /events が使う。遅い読み手で周を止めない、E-H5）。
func ReadOutbox(dir string, since uint64, max int) ([]core.Event, error) {
	odir := filepath.Join(dir, eventsDir, outboxDir)
	files, err := outboxFiles(odir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []core.Event
	for i, first := range files {
		// 次のファイルの頭が since 以下なら、このファイルは全部読み済み。
		if i+1 < len(files) && files[i+1] <= since+1 {
			continue
		}
		evs, err := readOutboxFile(filepath.Join(odir, outboxName(first)))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // 読む間に消された（全部の sink が送った）
			}
			return nil, err
		}
		for _, e := range evs {
			if e.Seq <= since {
				continue
			}
			out = append(out, e)
			if max > 0 && len(out) >= max {
				return out, nil
			}
		}
	}
	return out, nil
}

// readOutboxFile は 1 つのファイルを読む。最後の切れた行は読まない（書く途中で
// 止まった行は無かったことにする）。
func readOutboxFile(path string) ([]core.Event, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if i := bytes.LastIndexByte(data, '\n'); i < len(data)-1 {
		data = data[:i+1]
	}
	var out []core.Event
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), int(MaxOutboxFile)+(1<<20))
	for sc.Scan() {
		var e core.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// PruneOutbox は、番号が upTo 以下の Event だけのファイルを消す（全部の sink が
// 送ったもの）。最後のファイルは残す。
func (r *Record) PruneOutbox(upTo uint64) error {
	dir := filepath.Join(r.dir, eventsDir, outboxDir)
	files, err := outboxFiles(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for i := 0; i+1 < len(files); i++ {
		// files[i] の最後の番号は次のファイルの頭の 1 つ前。
		if files[i+1]-1 > upTo {
			break
		}
		if err := os.Remove(filepath.Join(dir, outboxName(files[i]))); err != nil {
			return err
		}
	}
	return nil
}

func outboxName(first uint64) string { return fmt.Sprintf("%012d.jsonl", first) }

// outboxFiles は送り出しの列のファイルの頭の番号を小さい順に返す。
func outboxFiles(dir string) ([]uint64, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []uint64
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSuffix(name, ".jsonl"), 10, 64)
		if err != nil || outboxName(n) != name {
			return nil, fmt.Errorf("送り出しの列のディレクトリに知らないファイル %s", name)
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// Cursor は sink name が送り終えた最後の番号（無ければ 0、§5.1 の cursors/）。
func (r *Record) Cursor(name string) (uint64, error) {
	data, err := os.ReadFile(filepath.Join(r.dir, eventsDir, cursorsDir, name+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var c struct {
		Seq uint64 `json:"seq"`
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return 0, fmt.Errorf("sink %s の位置: %w", name, err)
	}
	return c.Seq, nil
}

// WriteCursor は sink name が送り終えた最後の番号を書く。
func (r *Record) WriteCursor(name string, seq uint64) error {
	dir := filepath.Join(r.dir, eventsDir, cursorsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, _ := json.Marshal(struct {
		Seq uint64 `json:"seq"`
	}{seq})
	return writeFile(filepath.Join(dir, name+".json"), append(data, '\n'))
}

// AuditCursor は監査を送る sink name が送り終えた最後の監査の番号（無ければ 0、
// events/cursors/audit/<sink>.json、E-L1）。
func (r *Record) AuditCursor(name string) (uint64, error) {
	return r.Cursor(filepath.Join(auditDir, name))
}

// WriteAuditCursor は監査を送る sink name が送り終えた最後の番号を書く。
func (r *Record) WriteAuditCursor(name string, seq uint64) error {
	if err := os.MkdirAll(filepath.Join(r.dir, eventsDir, cursorsDir, auditDir), 0o755); err != nil {
		return err
	}
	return r.WriteCursor(filepath.Join(auditDir, name), seq)
}

// Health は健全性（§3.9、health.json）：最後の周の時刻・続いた誤りの数・最後の
// 誤り（E-K11・N-H4）。
type Health struct {
	LastCycle         time.Time `json:"last_cycle"`
	ConsecutiveErrors int       `json:"consecutive_errors"`
	LastError         string    `json:"last_error,omitempty"`
	LastErrorAt       time.Time `json:"last_error_at,omitempty"`
}

// Health は health.json を読む。無ければ空。
func (r *Record) Health() (Health, error) { return ReadHealth(r.dir) }

// ReadHealth は Record を作らず・ロックを取らずに health.json を読む。
func ReadHealth(dir string) (Health, error) {
	var h Health
	data, err := os.ReadFile(filepath.Join(dir, healthFile))
	if errors.Is(err, os.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return h, err
	}
	if err := json.Unmarshal(data, &h); err != nil {
		return h, fmt.Errorf("%s: %w", healthFile, err)
	}
	return h, nil
}

// RecordCycle は周の終わりに健全性を書く。cycleErr が空でなければ続いた誤りを
// 1 つ増やし、空なら 0 に戻す。ロックを持つ処理だけが呼ぶ。
func (r *Record) RecordCycle(now time.Time, cycleErr string) (Health, error) {
	h, err := r.Health()
	if err != nil {
		h = Health{} // 読めない健全性は作り直す（判定に使わない）
	}
	h.LastCycle = now
	if cycleErr == "" {
		h.ConsecutiveErrors = 0
	} else {
		h.ConsecutiveErrors++
		h.LastError, h.LastErrorAt = cycleErr, now
	}
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return h, err
	}
	return h, writeFile(filepath.Join(r.dir, healthFile), append(data, '\n'))
}

// Wake は常駐に次の周を今すぐ始めるよう頼む（sakutei wake、E-N5）。Record の
// wake のファイルに時刻を書くだけで、常駐はその変化を見て起きる。ロックは取らない。
func Wake(dir string, now time.Time) error {
	if err := checkFormat(dir); err != nil {
		return fmt.Errorf("Record %s: %w", dir, err)
	}
	return writeFile(filepath.Join(dir, wakeFile), []byte(now.Format(time.RFC3339Nano)+"\n"))
}

// WakeMark は wake のファイルの今の中身（無ければ空）。常駐が前と比べて起きる。
func WakeMark(dir string) string {
	data, _ := os.ReadFile(filepath.Join(dir, wakeFile))
	return string(data)
}
