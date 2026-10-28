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

// Package apicred は HTTP の管理 API などを使う provider（proxmox-vm・
// redfish・ldap・postgresql・s3-bucket）の資格を、観測用と書き込み用に分ける
// 欄 observe:・write: の読み方と、呼び出しごとの資格の選び方（§20.7、Backlog
// #186）。ssh 越しの provider の observe:・write:（internal/remote、#185）と
// 同じ決まり:
//
//   - 対象（クラスタ・BMC・サーバ・endpoint）の設定に observe:・write: を
//     書ける。どちらも資格のキー（provider ごとに決まる。proxmox-vm なら
//     token_id・token_secret）だけを持ち、書かないキーは対象の上の値を
//     受け継ぐ。どちらも書かなければ今までどおり 1 本の資格。
//   - 観測（Read・List・BulkObserve。plan・status・読み取りの口）は観測用の
//     資格だけを、書き込み（provider.Writing の ctx。Apply・Action・Task の
//     実行）は書き込み用の資格だけを使う。
//   - この回が書かない（provider.AccessObserve）なら、書き込み用の資格は
//     secret store にあるかを確かめず・取り出さず、書き込みを
//     provider.ErrNoWriteAccess で断る。
package apicred

import (
	"context"
	"fmt"
	"sort"

	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// The keys of a target's config that hold the two credentials.
const (
	KeyObserve = "observe"
	KeyWrite   = "write"
)

// Sections は 1 つの対象の資格のキーを、観測用・書き込み用に分けたもの。
// どちらも資格のキーだけを持ち、observe:・write: に書かないキーは対象の上の
// 値（書いてあれば）を受け継いでいる。
type Sections struct {
	Observe map[string]any
	Write   map[string]any
	// Split は observe:・write: のどちらかが書いてある。
	Split bool
}

// Read は対象の設定 m（path p）から、keys（資格のキー）の観測用・書き込み用を
// 読む。observe:・write: が map でない・keys の外のキーを持つは error。呼ぶ側は
// KeyObserve・KeyWrite を自分の既知のキーに足しておく。
func Read(p string, m map[string]any, keys ...string) (Sections, error) {
	allowed := map[string]bool{}
	for _, k := range keys {
		allowed[k] = true
	}
	base := map[string]any{}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			base[k] = v
		}
	}
	s := Sections{}
	for _, sec := range []string{KeyObserve, KeyWrite} {
		out := make(map[string]any, len(base))
		for k, v := range base {
			out[k] = v
		}
		if raw, ok := m[sec]; ok {
			s.Split = true
			sm, isMap := raw.(map[string]any)
			if !isMap {
				return Sections{}, fmt.Errorf("%s.%s: must be a map (%s)", p, sec, joinKeys(keys))
			}
			names := make([]string, 0, len(sm))
			for k := range sm {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, k := range names {
				if !allowed[k] {
					return Sections{}, fmt.Errorf("unknown key %q (%s.%s holds %s only)", p+"."+sec+"."+k, p, sec, joinKeys(keys))
				}
				out[k] = sm[k]
			}
		}
		if sec == KeyObserve {
			s.Observe = out
		} else {
			s.Write = out
		}
	}
	return s, nil
}

func joinKeys(keys []string) string {
	out := ""
	for i, k := range keys {
		if i > 0 {
			out += ", "
		}
		out += k
	}
	return out
}

// Pair は 1 つの対象の観測用と書き込み用の資格（T は provider ごとの資格の型。
// 値そのものでなく、値を要求の直前に取り出す関数を持たせる、§20.2）。
type Pair[T any] struct {
	Observe T
	// Write は書き込み用の資格。分けない設定では Observe と同じ。
	Write T
	// NoWrite はこの回が書かない（provider.AccessObserve）: 書き込みを断る。
	NoWrite bool
}

// Single は分けない 1 本の資格の Pair。
func Single[T any](c T) Pair[T] { return Pair[T]{Observe: c, Write: c} }

// For は ctx に合う資格: 書き込み（provider.Writing）なら書き込み用（この回が
// 書かないなら provider.ErrNoWriteAccess を包んだ error。what は文言の主語）、
// ほかは観測用。
func (p Pair[T]) For(ctx context.Context, what string) (T, error) {
	if !provider.Writing(ctx) {
		return p.Observe, nil
	}
	if p.NoWrite {
		var zero T
		return zero, fmt.Errorf("%s: %w", what, provider.ErrNoWriteAccess)
	}
	return p.Write, nil
}
