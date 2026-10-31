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

// Package record は周をまたいで覚えるものの置き場（設計 §3.9・§5.1）。ディレクトリ
// 1 つに、ロック・前の周の目印（marks.json）・監査の鎖（audit/）を置く（E-S2・N-S5）。
// 観測の値は置かない（不変条件 1）。段 2 はロック・目印・監査の鎖だけを持つ。
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

// Format は Record の形の版。知らない版なら読まずに止める（§5.1）。
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

// Open は dir を Record として開く。無ければ作って format を置く。知らない形の
// 版なら断る。
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
		return fmt.Errorf("Record %s の形の版 %q を知らない（読めるのは %q）", dir, v, Format)
	}
	return nil
}

// Lock は進める処理を 1 つにするロックを取る（E-M5・N-M7・N-S3）。ほかが持って
// いれば held は false で、release は何もしない。
func (r *Record) Lock() (release func(), held bool, err error) {
	return lock(filepath.Join(r.dir, lockFile))
}

// Marks は前の周の目印（§5.1 の marks.json）。観測の値は置かない（不変条件 1）。
type Marks struct {
	Cycle uint64    `json:"cycle"`
	At    time.Time `json:"at"`
	// RangeCounts は範囲（core.Range.Key）ごとの、前の周の List の件数から自分が
	// 消した分を差し引いた数（E-K2・N-K2）。急減で止めた周は前の値を保つ。
	RangeCounts map[string]int `json:"range_counts,omitempty"`
	// BudgetWindows は Policy ごとの、周をまたいだ壊す変更の使用量（E-J1）。
	BudgetWindows map[string]WindowUse `json:"budget_windows,omitempty"`
	// Ineffective は Resource ごとの、続いた効かない Apply の数（E-J6・E-K10・
	// 不変条件 14）。宣言が変われば数え直す。読み直して宣言どおりなら消す。
	Ineffective map[string]IneffectiveMark `json:"ineffective,omitempty"`
}

// IneffectiveMark は効かない Apply の数と、数えたときの宣言のハッシュ（宣言を
// 直したら上限から外す、E-J6）。観測の値は持たない（不変条件 1）。
type IneffectiveMark struct {
	Count   int    `json:"count"`
	Desired string `json:"desired"`
}

// WindowUse は予算の窓 1 つの使用量。窓は Start から長さ分続き、過ぎれば 0 から
// 数え直す。
type WindowUse struct {
	Start       time.Time `json:"start"`
	Destructive int       `json:"destructive"`
}

// Window は Policy name の今の窓の使用量。無いか過ぎていれば now から始まる空の
// 窓を返す。時計が窓の始まりより前に戻ったときは使用量を保つ（数え直して予算を
// 緩めない）。
func (m Marks) Window(name string, length time.Duration, now time.Time) WindowUse {
	w, ok := m.BudgetWindows[name]
	if !ok || length <= 0 || !now.Before(w.Start.Add(length)) {
		return WindowUse{Start: now}
	}
	return w
}

// Marks は marks.json を読む。無ければ空の目印（初めての周）。
func (r *Record) Marks() (Marks, error) { return readMarks(r.dir) }

// WriteMarks は marks.json を書く。ロックを持つ処理だけが呼ぶ。
func (r *Record) WriteMarks(m Marks) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(r.dir, marksFile), append(data, '\n'))
}

// ReadMarks は Record を作らず・ロックを取らずに目印だけを読む（plan、不変条件 30）。
// ディレクトリが無ければ空の目印。
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
