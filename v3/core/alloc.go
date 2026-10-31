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

import "fmt"

// Pool は採番のプール（intent の pools:、§3.8、N-B13・N-B12・N-A14）。値は整数で、
// 文字列への埋め込みは要求の template（"192.0.2.{n}"）。
type Pool struct {
	Name   string
	Lo, Hi int64
	// Blocks は区切った範囲（N-B13）。要求は block で名指しする。
	Blocks []Block
	// Reuse が false なら、一度払った値は返しても二度と払わない。block ごとの
	// 最後の値を採番のファイルの last に残す（N-B13・不変条件 11）。
	Reuse  bool
	Origin Origin
}

// BlockOf は値 v の入る block の名前（どれにも入らなければ ""）。
func (p Pool) BlockOf(v int64) string {
	for _, b := range p.Blocks {
		if v >= b.Lo && v <= b.Hi {
			return b.Name
		}
	}
	return ""
}

// Block はプールの中の区切った範囲。
type Block struct {
	Name   string
	Lo, Hi int64
}

// Bounds は block（空ならプール全体）の範囲。無い block なら ok は false。
func (p Pool) Bounds(block string) (lo, hi int64, ok bool) {
	if block == "" {
		return p.Lo, p.Hi, true
	}
	for _, b := range p.Blocks {
		if b.Name == block {
			return b.Lo, b.Hi, true
		}
	}
	return 0, 0, false
}

// Allocation は採番の結果（intent のファイル allocations/<pool>.yaml の
// allocations:）。intent のファイルが正で、Record の alloc/<pool>.json はその写し
// （P5、N-B13・E-K4）。
type Allocation struct {
	Pool string `json:"pool"`
	// Values は要求の鍵（"<type>/<id>#<項目>"）→ 払った値。宣言から消えても返さない
	// （N-B13・N-J4）。
	Values map[string]int64 `json:"values"`
	// Last は block（"" はプール全体）→ 払った最後の値。reuse: false のプールは
	// これより大きい値だけを払う（不変条件 11）。
	Last   map[string]int64 `json:"last,omitempty"`
	Origin Origin           `json:"-"`
}

// AllocRequest は採番の要求（宣言の項目の {from_pool: <pool>, block: …, template: …}）。
type AllocRequest struct {
	Resource ID
	Field    string
	Pool     string
	Block    string
	Template string // "{n}" を値に置き換える。空なら値は整数
}

// Key は採番のファイルの鍵（"<type>/<id>#<項目>"）。
func (r AllocRequest) Key() string { return fmt.Sprintf("%s#%s", r.Resource, r.Field) }

// Allocator は空きから値を選ぶ（§3.8。差し替えれば外の採番権威を使える、E-T4・N-T5）。
// taken は払えない値（払って返していない値と、reuse: false のプールで一度払った
// 値）。範囲は pool.Bounds(req.Block)。
type Allocator interface {
	Pick(pool Pool, taken func(int64) bool, req AllocRequest) (int64, error)
}

// FirstFree は既定の Allocator：範囲の中で使われていない最も小さい値。
type FirstFree struct{}

// Pick は空きの先頭を返す。空きが無ければ誤り（理由つきで止める、N-B13）。
func (FirstFree) Pick(pool Pool, taken func(int64) bool, req AllocRequest) (int64, error) {
	lo, hi, ok := pool.Bounds(req.Block)
	if !ok {
		return 0, fmt.Errorf("プール %s に block %q が無い", pool.Name, req.Block)
	}
	for v := lo; v <= hi; v++ {
		if !taken(v) {
			return v, nil
		}
	}
	where := "プール " + pool.Name
	if req.Block != "" {
		where += " の block " + req.Block
	}
	return 0, fmt.Errorf("%s に空きが無い", where)
}
