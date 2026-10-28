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

// Package freeze は global freeze（§14.3）の始まり・終わり・期限切れを S4 監査に
// 残す（Backlog #184）。
//
// freeze には 2 つの入口がある。
//
//   - 起動の旗 --freeze（その実行・その常駐のあいだ効く）。始まりは起動のとき、
//     終わりは実行（常駐）が終わるとき、または --freeze-until を過ぎたとき。
//   - freeze の状態のファイル（--freeze-state）。動いている常駐を止めずに
//     `sakutei freeze start` / `end` で書き換える口。常駐は毎周これを読む。
//
// どちらも「誰が（By）・理由（Reason、必須）・期限（Until、任意）」を持ち、
// 始まり・終わり・期限切れを監査の記録に 1 件ずつ書く。期限を過ぎた freeze は
// 次に読んだ者（常駐の周・一回性の reconcile・sakutei freeze）が解き、それを
// 記録する。
package freeze

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/audit"
	"github.com/tjst-t/sakutei/v2/internal/filelock"
)

// Source は freeze の入口。
type Source string

const (
	// SourceFlag は起動の旗 --freeze。
	SourceFlag Source = "flag"
	// SourceCommand は freeze の状態のファイル（sakutei freeze start）。
	SourceCommand Source = "command"
)

// Freeze は効いている freeze 1 つ。
type Freeze struct {
	Source Source    `json:"source"`
	By     string    `json:"by"`
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
	// Until は期限（零なら期限なし）。過ぎたら解ける。
	Until time.Time `json:"until,omitempty"`
}

// Validate は理由と誰がを確かめる（理由の無い freeze は断る）。
func (f Freeze) Validate() error {
	if strings.TrimSpace(f.Reason) == "" {
		return errors.New("a freeze needs a reason (recorded in the audit log, §14.3)")
	}
	if strings.TrimSpace(f.By) == "" {
		return errors.New("a freeze needs who freezes it (recorded in the audit log)")
	}
	if !f.Until.IsZero() && !f.Until.After(f.Since) {
		return fmt.Errorf("the freeze's deadline %s is not after its start %s", f.Until.UTC().Format(time.RFC3339), f.Since.UTC().Format(time.RFC3339))
	}
	return nil
}

// Expired は now に期限を過ぎているか。
func (f Freeze) Expired(now time.Time) bool {
	return !f.Until.IsZero() && !now.Before(f.Until)
}

// Text は 1 行の説明。
func (f Freeze) Text() string {
	s := fmt.Sprintf("frozen by %s since %s (%s): %s", f.By, f.Since.UTC().Format(time.RFC3339), f.Source, f.Reason)
	if !f.Until.IsZero() {
		s += "; until " + f.Until.UTC().Format(time.RFC3339)
	}
	return s
}

// Attrs は Event の attrs。
func (f Freeze) Attrs() map[string]string {
	a := map[string]string{"source": string(f.Source), "by": f.By, "reason": f.Reason,
		"since": f.Since.UTC().Format(time.RFC3339)}
	if !f.Until.IsZero() {
		a["until"] = f.Until.UTC().Format(time.RFC3339)
	}
	return a
}

func (f Freeze) detail(what string) string {
	d := fmt.Sprintf("%s (%s): %s", what, f.Source, f.Reason)
	if !f.Until.IsZero() {
		d += "; until " + f.Until.UTC().Format(time.RFC3339)
	}
	return d
}

// RecordStart は始まりを監査に書く。
func RecordStart(log *audit.Log, f Freeze) error {
	return log.Append(audit.Record{Time: f.Since, Kind: audit.KindFreezeStarted, Actor: f.By,
		Detail: f.detail("global freeze started: alert-only, drift is still detected (§14.3)")})
}

// RecordEnd は人（または実行の終わり）による終わりを監査に書く。by が空なら
// 始めた人の名で書く。why は終わりの理由（任意）。
func RecordEnd(log *audit.Log, f Freeze, by, why string, now time.Time) error {
	if by == "" {
		by = f.By
	}
	d := f.detail("global freeze ended")
	if why != "" {
		d += "; ended: " + why
	}
	return log.Append(audit.Record{Time: now, Kind: audit.KindFreezeEnded, Actor: by, Detail: d})
}

// RecordExpired は期限切れで解けたことを監査に書く。
func RecordExpired(log *audit.Log, f Freeze, now time.Time) error {
	return log.Append(audit.Record{Time: now, Kind: audit.KindFreezeExpired, Actor: f.By,
		Detail: f.detail("global freeze expired: its deadline passed, remediation resumes")})
}

// StateVersion は状態のファイルの版。
const StateVersion = 1

// State は freeze の状態のファイル（--freeze-state）の中身。
type State struct {
	Version int     `json:"version"`
	Freeze  *Freeze `json:"freeze,omitempty"`
}

// Read は状態のファイルの freeze を返す（無ければ nil）。期限を過ぎていても
// そのまま返す（解くのは Check）。
func Read(path string) (*Freeze, error) {
	unlock := filelock.RLock(path)
	defer unlock()
	st, err := read(path)
	return st.Freeze, err
}

func read(path string) (State, error) {
	var st State
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("freeze state: %w", err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return st, nil
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("freeze state %s: %w", path, err)
	}
	if st.Version != StateVersion {
		return st, fmt.Errorf("freeze state %s: unknown version %d (want %d)", path, st.Version, StateVersion)
	}
	return st, nil
}

func write(path string, st State) error {
	st.Version = StateVersion
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".freeze-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

func update(path string, fn func(*State) error) error {
	unlock, err := filelock.Lock(path)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := read(path)
	if err != nil {
		return err
	}
	if err := fn(&st); err != nil {
		return err
	}
	return write(path, st)
}

// Start は状態のファイルに freeze を書く。監査に書いてからファイルを書く（記録の
// 無い freeze を作らない）。理由の無いもの・すでに効いているものは断る。
func Start(path string, log *audit.Log, f Freeze) error {
	f.Source = SourceCommand
	if err := f.Validate(); err != nil {
		return err
	}
	return update(path, func(st *State) error {
		if cur := st.Freeze; cur != nil {
			if !cur.Expired(f.Since) {
				return fmt.Errorf("already %s; end it first (sakutei freeze end)", cur.Text())
			}
			if err := RecordExpired(log, *cur, f.Since); err != nil {
				return err
			}
		}
		if err := RecordStart(log, f); err != nil {
			return err
		}
		st.Freeze = &f
		return nil
	})
}

// End は状態のファイルの freeze を解き、終わりを監査に書く。効いている freeze が
// 無ければ断る。期限を過ぎていたものは期限切れとして記録し、それも断る。
func End(path string, log *audit.Log, by, why string, now time.Time) (Freeze, error) {
	var ended Freeze
	wasExpired := false
	err := update(path, func(st *State) error {
		cur := st.Freeze
		if cur == nil {
			return fmt.Errorf("no freeze is in effect in %s", path)
		}
		st.Freeze = nil
		if cur.Expired(now) {
			if err := RecordExpired(log, *cur, now); err != nil {
				return err
			}
			// The expiry is the record; the file is cleared all the same.
			ended, wasExpired = *cur, true
			return nil
		}
		if err := RecordEnd(log, *cur, by, why, now); err != nil {
			return err
		}
		ended = *cur
		return nil
	})
	if err == nil && wasExpired {
		return ended, fmt.Errorf("the freeze had already expired at %s (recorded as expired)", ended.Until.UTC().Format(time.RFC3339))
	}
	return ended, err
}

// Check は状態のファイルの freeze を now で確かめる。期限を過ぎていれば解いて
// 期限切れを監査に書き、expired に返す。効いていれば active に返す。
func Check(path string, log *audit.Log, now time.Time) (active, expired *Freeze, err error) {
	cur, err := Read(path)
	if err != nil || cur == nil {
		return nil, nil, err
	}
	if !cur.Expired(now) {
		return cur, nil, nil
	}
	err = update(path, func(st *State) error {
		c := st.Freeze
		if c == nil || !c.Expired(now) {
			// Someone ended or replaced it in between: take what is there.
			active = c
			return nil
		}
		if err := RecordExpired(log, *c, now); err != nil {
			return err
		}
		expired = c
		st.Freeze = nil
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return active, expired, nil
}
