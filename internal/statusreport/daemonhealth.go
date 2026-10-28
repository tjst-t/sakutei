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

	// 以下は #136：デーモンの担当と配布の状態。あるときだけ出る。

	// Claim はこのデーモンの担当（scope-selector、§17.1）。--claim も git の
	// shards/ も無い（全体を収束する）ときは空。
	Claim string `json:"claim,omitempty"`
	// Idle は git の shards/ で担当が無い（何も収束しない）こと（§17.7）。
	Idle bool `json:"idle,omitempty"`
	// Shard は fencing の holder（--shard-id）。--fencing-store が無ければ空。
	Shard string `json:"shard,omitempty"`
	// Fencing は scope ごとの fencing の世代（§15.2）。--fencing-store があるときだけ。
	Fencing []FencingGeneration `json:"fencing,omitempty"`
	// Mirror は git の配布（--intent-repo）の取得の状態（§17.4）。git 配布のときだけ。
	Mirror *MirrorHealth `json:"mirror,omitempty"`

	// Notify は通知の口（--event-webhook・--event-alertmanager）の宛先ごとの送りの
	// 状態（#154、§22.3）。通知の口があるときだけ。
	Notify []NotifyHealth `json:"notify,omitempty"`
}

// NotifyHealth は通知の口の 1 つの宛先への送りの状態（#154、§22.3）。宛先ごとに
// 送り直し・待ち・待ち行列を持つので、1 台が落ちていても他の宛先の数は進む。
// 数は要求（POST）の単位（webhook は 1 要求 = 1 Event、Alertmanager は 1 要求 =
// その時点の alert の全部）。デーモンが起きてからの数（S3、メモリだけ）。
type NotifyHealth struct {
	// Sink は "webhook" か "alertmanager"。
	Sink string `json:"sink"`
	// URL は宛先（資格と query は伏せる）。
	URL string `json:"url"`
	// Delivered は届いた要求の数。
	Delivered int `json:"delivered"`
	// Failed は届かなかった（送り直した）要求の数。
	Failed int `json:"failed"`
	// Rejected は送り先が受け付けなかった（408・429 を除く 4xx。捨てた）要求の数。
	Rejected int `json:"rejected"`
	// Dropped は待ち行列が溢れて捨てた Event・alert の数。
	Dropped int `json:"dropped"`
	// Pending は送り待ち（webhook は Event、Alertmanager は 1 回だけ送る alert）の数。
	Pending int `json:"pending"`
	// Active は Alertmanager に送り直している firing の alert の数（webhook は 0）。
	Active int `json:"active,omitempty"`
	// ConsecutiveFailures は続けて届かなかった要求の数（届けば 0 に戻る）。
	ConsecutiveFailures int `json:"consecutive_failures"`
	// LastSuccess・LastFailure は最後に届いた・届かなかった（受け付けられなかった）時刻。
	LastSuccess *time.Time `json:"last_success,omitempty"`
	LastFailure *time.Time `json:"last_failure,omitempty"`
	// LastError は最後に届かなかった・受け付けられなかった理由。
	LastError string `json:"last_error,omitempty"`
}

// Down は宛先に今届いていない（最後の要求が届かなかった）ことを返す。
func (n NotifyHealth) Down() bool { return n.ConsecutiveFailures > 0 }

// FencingGeneration は 1 つの scope の fencing の世代（§15.2）。
type FencingGeneration struct {
	// Scope は scope のキー（ラベルなしは "_root"）。
	Scope string `json:"scope"`
	// Generation と Holder は、手元で持つ世代（Fenced が false）か、fence されたとき
	// はストアの現在の世代（別のシャードが持つ）。
	Generation int64  `json:"generation"`
	Holder     string `json:"holder"`
	// Fenced はこのデーモンがこの scope を fence されている（書かない）こと。
	Fenced bool `json:"fenced"`
}

// MirrorHealth は git の配布の取得の状態（§17.4・§25.2）。
type MirrorHealth struct {
	// Remote は取りに行く先（中央の intent リポジトリ、または拠点のミラー）。
	Remote string `json:"remote"`
	// Revision は最後に取れた（手元写しにある）コミット。
	Revision string `json:"revision,omitempty"`
	// FetchedAt は最後に取得が成功した時刻（UTC）。まだ一度も無ければ出ない。
	FetchedAt *time.Time `json:"fetched_at,omitempty"`
	// Reachable は直近の取得が成功したこと（中央に届いている）。false のとき
	// デーモンは手元写しで収束を続ける（§17.4）か、手元写しも無ければ周を飛ばす。
	Reachable bool `json:"reachable"`
	// Error は直近の取得の失敗（Reachable が true なら空）。
	Error string `json:"error,omitempty"`
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
