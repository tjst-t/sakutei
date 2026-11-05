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

// Package record は cycle をまたいで覚えるものの置き場（設計 §3.9・§5.1）。ディレクトリ
// 1 つに、ロック・前の cycle の marks（marks.json）・audit のハッシュチェーン（audit/）を
// 置く（E-S2・N-S5）。観測の値は置かない（不変条件 1）。Stage 2 はロック・marks・
// audit のハッシュチェーンだけを持つ。
//
// 書き込みはどれも一時ファイルに書いて rename する（監査は追記だけ）。
package record

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Format は Record の format のバージョン。知らないバージョンなら読まずに止める（§5.1）。
const Format = "1"

const (
	formatFile = "format"
	lockFile   = "LOCK"
	marksFile  = "marks.json"
	auditDir   = "audit"
)

// Record は Record のディレクトリ。
type Record struct {
	dir string
}

// Open は dir を Record として開く。無ければ作って format を置く。知らない format の
// バージョンならエラーにする。
func Open(dir string) (*Record, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := checkFormat(dir); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err := writeFile(filepath.Join(dir, formatFile), []byte(Format+"\n")); err != nil {
			return nil, err
		}
	}
	return &Record{dir: dir}, nil
}

// Dir は Record のディレクトリ。
func (r *Record) Dir() string { return r.dir }

func checkFormat(dir string) error {
	data, err := os.ReadFile(filepath.Join(dir, formatFile))
	if err != nil {
		return err
	}
	if v := strings.TrimSpace(string(data)); v != Format {
		return fmt.Errorf("Record %s の format のバージョン %q を知らない（読めるのは %q）", dir, v, Format)
	}
	return nil
}

// Lock は進める処理を 1 つにするロックを取る（E-M5・N-M7・N-S3）。ほかが持って
// いれば held は false で、release は何もしない。
func (r *Record) Lock() (release func(), held bool, err error) {
	return lock(filepath.Join(r.dir, lockFile))
}

// Marks は前の cycle の marks（§5.1 の marks.json）。観測の値は置かない（不変条件 1）。
type Marks struct {
	Cycle uint64    `json:"cycle"`
	At    time.Time `json:"at"`
	// RangeCounts は Range（core.Range.Key）ごとの、前の cycle の List の件数から自分が
	// 消した分を差し引いた数（E-K2・N-K2）。急減で halt した cycle は前の値を保つ。
	RangeCounts map[string]int `json:"range_counts,omitempty"`
	// BudgetWindows は Policy ごとの、cycle をまたいだ壊す変更の使用量（E-J1）。
	BudgetWindows map[string]WindowUse `json:"budget_windows,omitempty"`
	// Ineffective は Resource ごとの、続いた ineffective Apply の数（E-J6・E-K10・
	// 不変条件 14）。宣言が変われば数え直す。再観測して宣言どおりなら消す。
	Ineffective map[string]IneffectiveMark `json:"ineffective,omitempty"`
	// InputCounts は外部入力ごとの前の cycle の件数（E-K3）。欠けていた cycle は前の値を保つ。
	InputCounts map[string]int `json:"input_counts,omitempty"`
	// Lanes は allow の Policy ごとの使用量と、宣言を初めて見た時刻（cooling-off の
	// 時計、E-J2・N-J2）。allow の枠は実行の直前に使い、失敗しても戻さない。
	Lanes map[string]LaneMark `json:"lanes,omitempty"`
}

// LaneMark は allow 1 つの marks。Fingerprint（select・kind・window）が変われば別の宣言
// として数え直す。max_count を変えただけなら使った分を保つ。
type LaneMark struct {
	Fingerprint string    `json:"fingerprint"`
	Used        int       `json:"used"`
	Seen        time.Time `json:"seen"`
}

// Lane は Policy name の allow の marks（fingerprint が違えば空）。
func (m Marks) Lane(name, fingerprint string) (LaneMark, bool) {
	l, ok := m.Lanes[name]
	if !ok || l.Fingerprint != fingerprint {
		return LaneMark{Fingerprint: fingerprint}, false
	}
	return l, true
}

// IneffectiveMark は ineffective Apply の数と、数えたときの宣言のハッシュ（宣言を
// 直したら上限から外す、E-J6）。観測の値は持たない（不変条件 1）。
type IneffectiveMark struct {
	Count   int    `json:"count"`
	Desired string `json:"desired"`
}

// WindowUse は budget の window 1 つの使用量。window は Start から長さ分続き、過ぎれば
// 0 から数え直す。
type WindowUse struct {
	Start       time.Time `json:"start"`
	Destructive int       `json:"destructive"`
}

// Window は Policy name の今の window の使用量。無いか過ぎていれば now から始まる空の
// window を返す。時計が window の始まりより前に戻ったときは使用量を保つ（数え直して
// budget を緩めない）。
func (m Marks) Window(name string, length time.Duration, now time.Time) WindowUse {
	w, ok := m.BudgetWindows[name]
	if !ok || length <= 0 || !now.Before(w.Start.Add(length)) {
		return WindowUse{Start: now}
	}
	return w
}

// Marks は marks.json を読む。無ければ空の marks（初めての cycle）。
func (r *Record) Marks() (Marks, error) { return readMarks(r.dir) }

// WriteMarks は marks.json を書く。ロックを持つ処理だけが呼ぶ。
func (r *Record) WriteMarks(m Marks) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(r.dir, marksFile), append(data, '\n'))
}

// ReadMarks は Record を作らず・ロックを取らずに marks だけを読む（plan、不変条件 30）。
// ディレクトリが無ければ空の marks。
func ReadMarks(dir string) (Marks, error) {
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return Marks{}, nil
	}
	if err := checkFormat(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Marks{}, err
	}
	return readMarks(dir)
}

func readMarks(dir string) (Marks, error) {
	path := filepath.Join(dir, marksFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Marks{}, nil
	}
	if err != nil {
		return Marks{}, err
	}
	var m Marks
	if err := json.Unmarshal(data, &m); err != nil {
		return Marks{}, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// WriteAtomic は一時ファイルに書いて rename する（Record の外のファイル：採番の
// intent のファイル、N-B13）。
func WriteAtomic(path string, data []byte) error { return writeFile(path, data) }

// writeFile は一時ファイルに書いて rename する（途中で止まっても半端なファイルを
// 残さない）。
func writeFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}
