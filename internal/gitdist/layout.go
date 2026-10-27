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

// Package gitdist は claim の git 配布（§17.4）を実装する。
//
//   - リポジトリのレイアウト規約: intent/<scope>/...（scope は core.Scope.Path）と、
//     全シャードが read-only で参照する共有 intent/platform/、シャード割当の宣言
//     shards/<シャード名>.yaml（§17.1 claim-as-intent）。
//   - claim から取るべきパスが機械的に引けるので、各シャードは sparse checkout +
//     partial clone で「自 claim 分 + platform + shards」だけを取得する（Checkout）。
//   - 拠点は中央 git を直接叩かず、状態を持たない読み取り専用ミラー（Mirror）から
//     jitter 付きポーリングで pull する。
//   - Source は常駐ループ（reconcile.Source）に、取得した intent と git 上で宣言された
//     claim を渡す。claim 変更 PR がそのまま開店/閉店のハンドオフになる（§17.7）。
//
// ここで手元に置くもの（checkout）は git の写しにすぎず、消えても pull し直せば戻る。
// S2-free シャード（§17.5）が全損から「再イメージ → pull → 収束」で戻れるのはこのため。
package gitdist

import (
	"fmt"
	"path"
	"strings"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

const (
	// IntentRoot は intent の木の根。
	IntentRoot = "intent"
	// PlatformDir は全シャードが read-only で参照する共有 platform の intent（§7, §17.3）。
	PlatformDir = "intent/platform"
	// ShardsDir はシャード割当の宣言（shards/<シャード名>.yaml）を置くディレクトリ。
	ShardsDir = "shards"
)

// ScopeDir は claim が所有する intent のディレクトリ（intent/<scope>）を返す。
// 例: {site: tokyo} → "intent/site=tokyo"。core.Scope.Path と同じ規約。
func ScopeDir(claim core.Scope) (string, error) {
	if len(claim.Labels) == 0 {
		// 空の claim は「全世界」。git 配布のシャードが全世界を掴むことはない。
		return "", fmt.Errorf("gitdist: an empty claim (whole world) has no intent/<scope>/ directory")
	}
	for k, v := range claim.Labels {
		for _, s := range []string{k, v} {
			if s == "" || s == "." || s == ".." || strings.ContainsAny(s, "/\\,=") {
				return "", fmt.Errorf("gitdist: claim label %s=%s cannot form a directory name", k, v)
			}
		}
	}
	return claim.Path(), nil
}

// SparsePaths は claim のシャードが取得するパス（sparse checkout の cone）を返す:
// shards/ + intent/platform/ + intent/<scope>/（§17.4）。
func SparsePaths(claim core.Scope) ([]string, error) {
	dir, err := ScopeDir(claim)
	if err != nil {
		return nil, err
	}
	return []string{ShardsDir, PlatformDir, dir}, nil
}

// SecretPrefix は claim の拠点ローカル資格が置かれる secret パスの接頭辞を返す
// （intent と同じ規約で "site=tokyo/"）。拠点の file SecretBackend にはこの接頭辞の
// 資格だけを置く（§20.5 爆発半径 = 当該拠点）。
func SecretPrefix(claim core.Scope) (string, error) {
	dir, err := ScopeDir(claim)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(dir, IntentRoot+"/") + "/", nil
}

// shardFile は shards/ 配下のシャード割当ファイル名の候補を返す。
func shardFiles(name string) []string {
	return []string{
		path.Join(ShardsDir, name+".yaml"),
		path.Join(ShardsDir, name+".yml"),
		path.Join(ShardsDir, name+".json"),
	}
}
