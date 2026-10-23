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

package statusreport

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DaemonHealth は常駐デーモン（reconcile.Run）が周期ごとに書き出す健全性の要約で
// ある（#15, §9.2）。status は別プロセスなのでデーモンのメモリを覗けない。そこで
// デーモンが --daemon-status のファイルに書き、status が --daemon-status で読む。
//
// これは観測 state ではない（現実の写しを持たない・収束の判断に使わない）。デーモン
// 自身の進捗（S3 相当）を人に見せるための報告である。
type DaemonHealth struct {
	// Cycle は最後に書いた周期番号。
	Cycle int `json:"cycle"`
	// ConsecutiveUnreachableSkips は、外部入力が不達で前回値も無いために halt-safe
	// にスキップした連続周期数。収束が進む周期が来れば 0 に戻る。
	ConsecutiveUnreachableSkips int `json:"consecutive_unreachable_skips"`
	// UnreachableSkipLimit は通知（ExternalInputUnreachableProlonged）を上げる上限。
	UnreachableSkipLimit int `json:"unreachable_skip_limit"`
	// ConsecutiveErrorCycles は、エラーで終わった連続周期数（intent が読めない・
	// fencing のストアに届かない等）。これも収束が止まっている状態なので、健全に
	// 見せない。エラーなく終わる周が来れば 0 に戻る。
	ConsecutiveErrorCycles int `json:"consecutive_error_cycles"`
	// LastError は最後にエラーで終わった周のエラー（ConsecutiveErrorCycles が 0 なら空）。
	LastError string `json:"last_error,omitempty"`
	// Interval はデーモンの周期（time.Duration の文字列）。status はこれで
	// ファイルが更新されなくなった（デーモンが止まった）ことを見分ける。
	Interval string `json:"interval,omitempty"`
	// UpdatedAt は書いた時刻（UTC）。
	UpdatedAt time.Time `json:"updated_at"`
}

// Prolonged は連続スキップが上限に達しているかを返す。
func (h DaemonHealth) Prolonged() bool {
	return h.UnreachableSkipLimit > 0 && h.ConsecutiveUnreachableSkips >= h.UnreachableSkipLimit
}

// staleIntervals は、更新が周期の何倍途絶えたら「止まっているかもしれない」と
// みなすか。周期には 10% までの揺らぎと周の所要時間が乗るので、余裕を取る。
const staleIntervals = 3

// Stale は now の時点で、ファイルが周期の staleIntervals 倍より長く更新されて
// いないかを返す。周期が分からない（古いデーモンが書いた）ときは false。
func (h DaemonHealth) Stale(now time.Time) bool {
	iv, err := time.ParseDuration(h.Interval)
	if err != nil || iv <= 0 {
		return false
	}
	return now.Sub(h.UpdatedAt) > staleIntervals*iv
}

// WriteDaemonHealth は h を path に書く。読み手が途中まで書かれたファイルを見ない
// よう、同じディレクトリの一時ファイルに書いてから rename する。
func WriteDaemonHealth(path string, h DaemonHealth) error {
	b, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".daemon-status-*")
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

// ReadDaemonHealth は WriteDaemonHealth が書いたファイルを読む。
func ReadDaemonHealth(path string) (DaemonHealth, error) {
	var h DaemonHealth
	b, err := os.ReadFile(path)
	if err != nil {
		return h, err
	}
	if err := json.Unmarshal(b, &h); err != nil {
		return h, fmt.Errorf("parse daemon status %s: %w", path, err)
	}
	return h, nil
}
