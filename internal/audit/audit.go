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

// Package audit は S4 監査ログの最小形（§4.9 S4, §6.4, §26）。
//
// 1 行 1 件の JSON（JSON Lines）をファイルに追記するだけの append-only ログ。
// 既存の行を書き換える API は持たない。追記は O_APPEND で開いて 1 回の write で
// 行を書き、fsync してから返す（返ったら記録は残っている）。WORM・外部への即時
// push は最小形の外（§29）。
package audit

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Kind は監査記録の種別。
type Kind string

const (
	// KindGateApproved は承認ゲートの承認（§6.4「全承認者を S4 監査に記録」）。
	// N-of-M のゲートでは承認者 1 人ごとに 1 件。
	KindGateApproved Kind = "workflow.gate.approved"
	// KindGateRejected は承認ゲートの却下（= halt）。
	KindGateRejected Kind = "workflow.gate.rejected"
	// KindGateTimedOut はゲートの timeout による halt。
	KindGateTimedOut Kind = "workflow.gate.timed_out"
	// KindWorkflowCancelled は人による Workflow の実行の取り消し（sakutei workflow cancel）。
	KindWorkflowCancelled Kind = "workflow.cancelled"

	// KindBreakGlassWrite は書込 break-glass の使用（§25.1 (b)）: git の正に届かない間に
	// ローカルミラーへ書いた署名付きコミット。コミットより先に記録する。
	KindBreakGlassWrite Kind = "breakglass.write"
	// KindBreakGlassMergeBack は事後合流のブランチを中央へ push し PR の下書きを作ったこと。
	KindBreakGlassMergeBack Kind = "breakglass.merge_back.opened"
	// KindBreakGlassMerged は中央の正が break-glass コミットを取り込み、ミラーが中央の
	// 追従に戻ったこと（session の終わり）。
	KindBreakGlassMerged Kind = "breakglass.merged"
	// KindBreakGlassKeyRetired は session で使った break-glass 鍵の失効（使い捨て原則）。
	// 失効した鍵での break-glass 書込は拒否される。
	KindBreakGlassKeyRetired Kind = "breakglass.key.retired"
)

// Record は監査の 1 件。
type Record struct {
	Time     time.Time `json:"time"`
	Kind     Kind      `json:"kind"`
	Workflow string    `json:"workflow,omitempty"`
	RunID    string    `json:"run_id,omitempty"`
	Gate     string    `json:"gate,omitempty"`
	// Actor は承認者 ID。コアは認可を持たないので、記録するのは名乗られた ID と
	// その署名である（誰が叩けるかの判定は上位 IdP、§7）。
	Actor string `json:"actor,omitempty"`
	// Roles は承認者が名乗った役割（真偽は上位 IdP、§7）。
	Roles     []string `json:"roles,omitempty"`
	Signature string   `json:"signature,omitempty"`
	// Session・Commit・Key は書込 break-glass の記録（session ID・コミット・鍵の指紋）。
	Session string `json:"session,omitempty"`
	Commit  string `json:"commit,omitempty"`
	Key     string `json:"key,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// Log は1つのファイルへの append-only ログ。複数プロセスから追記してよい
// （O_APPEND の 1 回の write は行単位で混ざらない）。
type Log struct {
	Path string
	mu   sync.Mutex
}

// NewLog は path に追記する Log を返す。
func NewLog(path string) *Log { return &Log{Path: path} }

// Append は rec を 1 行追記し、fsync してから返す。Time が零なら今の時刻を入れる。
func (l *Log) Append(rec Record) error {
	if rec.Time.IsZero() {
		rec.Time = time.Now()
	}
	rec.Time = rec.Time.UTC()
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	line = append(line, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("audit: open %s: %w", l.Path, err)
	}
	if _, err := f.Write(line); err != nil {
		f.Close()
		return fmt.Errorf("audit: append %s: %w", l.Path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("audit: sync %s: %w", l.Path, err)
	}
	return f.Close()
}

// ReadAll は path の全記録を書かれた順に返す。ファイルが無ければ空。
func ReadAll(path string) ([]Record, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	defer f.Close()
	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("audit: %s line %d: %w", path, n, err)
		}
		out = append(out, rec)
	}
	return out, sc.Err()
}
