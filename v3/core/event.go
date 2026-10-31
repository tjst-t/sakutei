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

package core

import (
	"context"
	"time"
)

// EventType は Event の型（設計 §5.2）。
type EventType string

const (
	// StateEvent は今の状態。キー（種類＋対象）ごとに firing になった周と
	// resolved になった周にだけ送り出しの列へ積む（E-H1・N-H2）。
	StateEvent EventType = "state"
	// IncidentEvent は起きたとき 1 回の出来事（E-H1）。
	IncidentEvent EventType = "incident"
)

// 状態型の Event の State。
const (
	Firing   = "firing"
	Resolved = "resolved"
)

// Event は外への知らせ 1 件（§3.9・§5.2）。観測の値・秘密の値は載せない
// （不変条件 1・27）。Seq は送り出しの列の位置で、Record が付ける。
type Event struct {
	Seq      uint64            `json:"seq"`
	Kind     string            `json:"kind"` // Drift・Unknown・Halt・Applied・CycleDone…
	Type     EventType         `json:"type"`
	State    string            `json:"state,omitempty"`    // firing | resolved（状態型のとき）
	Resource string            `json:"resource,omitempty"` // 対象の Resource（E-H1）
	Scope    string            `json:"scope,omitempty"`    // 対象が Resource でないとき（範囲・Policy、E-H1）
	Labels   map[string]string `json:"labels,omitempty"`   // 宣言の labels（E-M1・E-H2）
	At       time.Time         `json:"at"`
	Since    *time.Time        `json:"since,omitempty"` // firing になった時刻（状態型、E-H1）
	Count    int               `json:"count,omitempty"` // 確かめて続いた周の数（E-H1）
	Stuck    bool              `json:"stuck,omitempty"` // cycle.stuck_after の周数続いた（E-H1）
	Message  string            `json:"message,omitempty"`
	// Values は CycleDone の件数と周の時間（E-H3・N-H4）。数えたものだけを
	// 載せる（観測しなかった値を 0 として出さない、不変条件 29）。
	Values map[string]int `json:"values,omitempty"`
}

// Key は状態型の Event のキー（種類＋対象）。
func (e Event) Key() string {
	if e.Resource != "" {
		return e.Kind + " " + e.Resource
	}
	return e.Kind + " " + e.Scope
}

// Sink は Event の送り先（Alertmanager・webhook・syslog・stdout…、E-H2・E-H3）。
// コアは口だけを持ち、送り先はモジュールが作る。
type Sink interface {
	// Deliver は batch を送る。失敗しても周を止めない。次の周に同じ位置から
	// 送り直す（不変条件 28、E-H2・N-H2）。
	Deliver(ctx context.Context, batch []Event) error
}

// StateSink は周ごとに今 firing の Event の一覧も受け取る送り先（任意）。
// 送り出しの列は状態の変わり目だけなので、続いている状態を送り直す送り先
// （Alertmanager の期限・metrics のゲージ）が使う（E-H2・E-H3）。Deliver の
// あとに呼ぶ。失敗しても周を止めない（不変条件 28）。
type StateSink interface {
	DeliverState(ctx context.Context, firing []Event) error
}

// AuditLine は監査の鎖の 1 行（Record に書いたままの JSON と番号、E-L1・N-L1）。
// 監査の行は値を書かない（不変条件 1）。
type AuditLine struct {
	Seq  uint64
	Line []byte
}

// AuditSink は監査の鎖も送る送り先（つなぎ方の設定の streams: [audit]、syslog
// など、E-L1・N-L1）。送り終えた位置は sink ごとに Record が覚え、送れなければ
// 次の周に同じ位置から送り直す（不変条件 28）。
type AuditSink interface {
	DeliverAudit(ctx context.Context, batch []AuditLine) error
}

// CycleSettings は常駐の周の設定（intent の cycle:、§3.6）。
type CycleSettings struct {
	Interval   time.Duration // 常駐の周の間隔（E-J11）
	StuckAfter int           // 状態型の Event がこの周数続けば stuck に上げる。0 は上げない（E-H1）
	Origin     Origin
}

// DefaultInterval は cycle.interval を書かないときの周の間隔。
const DefaultInterval = 60 * time.Second
