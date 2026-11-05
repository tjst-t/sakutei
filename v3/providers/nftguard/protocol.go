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

package nftguard

// protocol.go — 専用ツールと provider の取り決め（build tag なし。provider の側も使う）。

// Name は専用ツールの名前。
const Name = "sakutei-nft-guard"

// Protocol は取り決めのバージョン（status の protocol）。引数・出力の形を変えたら上げる。
const Protocol = 2

// DefaultConfigPath は専用ツールの設定ファイルの既定の場所。
const DefaultConfigPath = "/etc/sakutei/nft-guard.json"

// DefaultPath は専用ツールを置く既定の場所（provider の hosts.<名前>.guard の既定値）。
const DefaultPath = "/usr/local/libexec/sakutei-nft-guard"

// 終了コード。
const (
	ExitOK       = 0
	ExitRefused  = 2  // 引数・入力を拒否した（何もしていない）
	ExitConfig   = 3  // 設定ファイル・state ディレクトリが使えない
	ExitInternal = 4  // それ以外
	ExitNFT      = 10 // nft が 0 以外で終わった（標準エラーは nft のもの）
)

// Status は status の出力。
type Status struct {
	Tool      string     `json:"tool"`
	Protocol  int        `json:"protocol"`
	Tables    []string   `json:"tables"`
	MaxSecs   int        `json:"max_rollback_seconds"`
	Rollbacks []Rollback `json:"rollbacks"`
}

// Rollback は 1 つの予約の状態。
type Rollback struct {
	Token  string   `json:"token"`
	State  string   `json:"state"` // armed・fired
	FireAt int64    `json:"fire_at"`
	Tables []string `json:"tables"`
}
