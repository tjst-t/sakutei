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

package gitdist

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// ShardClaim はシャード割当の宣言（shards/<シャード名>.yaml）。
//
//	claim:
//	  site: tokyo
//
// claim が無い・空のシャードは idle（何も収束しない）。全世界を掴む意味にはしない。
// 開店前の店舗デーモンは idle で待ち、claim 変更 PR で scope を受け取る（§17.7）。
type ShardClaim struct {
	Name  string
	Claim core.Scope
	Idle  bool
}

// ReadShardClaim は checkout の dir から shards/<name>.{yaml,yml,json} を読む。
// ファイルが無いのはエラー（宣言されていないシャードは何も収束しない）。
func ReadShardClaim(dir, name string) (ShardClaim, error) {
	if name == "" || strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return ShardClaim{}, fmt.Errorf("gitdist: invalid shard name %q", name)
	}
	for _, rel := range shardFiles(name) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			continue
		}
		tree, err := loader.ReadTree(p)
		if err != nil {
			return ShardClaim{}, fmt.Errorf("gitdist: shard %q: %w", name, err)
		}
		return parseShardClaim(name, rel, tree)
	}
	return ShardClaim{}, fmt.Errorf("gitdist: shard %q is not declared (no %s.yaml under %s/)", name, name, ShardsDir)
}

func parseShardClaim(name, rel string, tree map[string]any) (ShardClaim, error) {
	keys := make([]string, 0, len(tree))
	for k := range tree {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k != "claim" {
			return ShardClaim{}, fmt.Errorf("gitdist: %s: unknown key %q (only \"claim\")", rel, k)
		}
	}
	sc := ShardClaim{Name: name}
	raw, ok := tree["claim"]
	if !ok || raw == nil {
		sc.Idle = true
		return sc, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		if s, isStr := raw.(string); isStr && s == "" {
			sc.Idle = true
			return sc, nil
		}
		return ShardClaim{}, fmt.Errorf("gitdist: %s: claim must be a map of scope labels", rel)
	}
	labels := map[string]string{}
	for k, v := range m {
		s, ok := v.(string)
		if !ok {
			s = fmt.Sprint(v)
		}
		labels[k] = s
	}
	if len(labels) == 0 {
		sc.Idle = true
		return sc, nil
	}
	sc.Claim = core.NewScope(labels)
	if _, err := ScopeDir(sc.Claim); err != nil {
		return ShardClaim{}, fmt.Errorf("gitdist: %s: %w", rel, err)
	}
	return sc, nil
}
