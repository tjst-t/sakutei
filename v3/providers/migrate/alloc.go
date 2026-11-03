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

package migrate

// alloc.go — v2 の採番の記録（--alloc-store のファイル）を新しいコアの採番の
// ファイル（intent の allocations/<pool>.yaml、§3.8）にする。払った値を書き
// 写さないと、新しいコアが同じ鍵に別の値を払い直す（不変条件 11）。

import (
	"encoding/json"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/v3/alloc"
	"github.com/tjst-t/sakutei/v3/core"
)

// allocations は v2 の採番の記録を読み、プールごとのファイルを intent に足す。
// v2 の鍵 <provider>/<local>#<項目> は新しい鍵 <ID>#<項目> にする。
func (c *conv) allocations(intent map[string]string) {
	file := c.in.Flags["alloc-store"]
	if file == "" {
		return
	}
	data, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return // v2 もまだ何も払っていない
	}
	if err != nil {
		c.refuse("alloc-store: %v", err)
		return
	}
	var store struct {
		Pools map[string]struct {
			Allocations map[string]struct {
				Value int    `json:"value"`
				State string `json:"state"`
			} `json:"allocations"`
			NoReuse bool           `json:"no_reuse"`
			Last    map[string]int `json:"last"`
		} `json:"pools"`
	}
	if err := json.Unmarshal(data, &store); err != nil {
		c.refuse("alloc-store %s: %v", file, err)
		return
	}
	var pools []string
	for p := range store.Pools {
		pools = append(pools, p)
	}
	sort.Strings(pools)
	for _, p := range pools {
		v2 := store.Pools[p]
		a := core.Allocation{Pool: p, Values: map[string]int64{}, Last: map[string]int64{}}
		for owner, e := range v2.Allocations {
			if e.State != "allocated" {
				c.note("alloc-store: プール %s の %s は予約（%s）のまま。書き写さない", p, owner, e.State)
				continue
			}
			key, ok := c.allocKey(owner)
			if !ok {
				c.refuse("alloc-store: プール %s の %s の宣言が無い（値 %d を書き写せない）", p, owner, e.Value)
				continue
			}
			a.Values[key] = int64(e.Value)
		}
		if v2.NoReuse {
			for b, v := range v2.Last {
				a.Last[b] = int64(v)
			}
		}
		intent[path.Join(alloc.Dir, p+".yaml")] = string(alloc.Format(a))
	}
}

// allocKey は v2 の持ち主（<provider>/<local>#<項目>、scope を含めない）を
// 新しい鍵（<provider>/<種類>/<名前>#<項目>）にする。
func (c *conv) allocKey(owner string) (string, bool) {
	ref, field, ok := strings.Cut(owner, "#")
	if !ok {
		return "", false
	}
	provider, local, ok := strings.Cut(ref, "/")
	if !ok {
		return "", false
	}
	for _, r := range c.resources {
		if r.provider == provider && r.local == local && r.out != nil {
			return r.ref() + "#" + field, true
		}
	}
	return "", false
}
