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

// protocol.go — 道具と Sakutei の取り決め（build tag なし。Sakutei の側
// 〈internal/nftables の ToolGuard〉もこれを使う）。

// Name は道具の名前（status の tool）。
const Name = "sakutei-nft-guard"

// Protocol は道具と Sakutei の取り決めの版（status の protocol）。引数の形・
// 出力の形を変えたら上げる。Sakutei は版の合わない道具では base chain を変えない。
const Protocol = 1

// DefaultConfigPath は道具の設定ファイルの決まった場所。
const DefaultConfigPath = "/etc/sakutei/nft-guard.json"

// DefaultPath は道具を置く決まった場所（provider の設定 ruleset_guard_tool の既定）。
const DefaultPath = "/usr/local/libexec/sakutei-nft-guard"

// 終了コード。
const (
	ExitOK       = 0
	ExitRefused  = 2
	ExitConfig   = 3
	ExitInternal = 4
	ExitNFT      = 10
)

// Status は status の出力。
type Status struct {
	Tool      string     `json:"tool"`
	Protocol  int        `json:"protocol"`
	Tables    []string   `json:"tables"`
	MaxSecs   int        `json:"max_rollback_seconds"`
	Rollbacks []Rollback `json:"rollbacks"`
}

// Rollback は 1 つの予約の様子。
type Rollback struct {
	Token  string   `json:"token"`
	State  string   `json:"state"` // armed・fired
	FireAt int64    `json:"fire_at"`
	Tables []string `json:"tables"`
}
