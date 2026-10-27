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

package netconf

// config.go — 部分木の一覧（--provider-config の netconf_subtrees が指す
// ファイル）。書式は intent と同じ JSON / YAML サブセット（loader.ReadTree）:
//
//	subtrees:
//	  if-uplink:                      # 部分木の名前（ID の後ろ。sw1/if-uplink）
//	    filter: '<interfaces xmlns="urn:ietf:params:xml:ns:yang:ietf-interfaces"><interface><name>uplink</name></interface></interfaces>'
//	  acl-mgmt:
//	    filter_file: filters/acl-mgmt.xml   # 長いものはファイルで（このファイルのある所から）
//
// 「どこ」（subtree filter）は観測が知るべきことなので、ここ（provider の側）
// に書く: Read は desired を見ない（ADR-0002）。「中身」は intent の config。
//
// 決まり:
//
//   - filter・filter_file のどちらか 1 つ。最上位の要素は 1 つ以上で、どれも
//     名前空間を持つ（名前空間の無い subtree filter はすべての名前空間に
//     当たる、RFC 6241 §6.2.1。どこを見るかをあいまいにしない）。
//   - 未知のキーはエラー（書き間違いを黙って無視しない）。

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/v2/internal/loader"
)

// Subtree は設定の部分木の「どこ」。
type Subtree struct {
	Name string
	// Filter は subtree filter の中身の XML（<filter> の子）。
	Filter string
	// top は filter の最上位の要素（名前空間と名前）。
	top []nameKey
}

type nameKey struct{ space, local string }

var subtreeName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

// NewSubtree は名前と filter から Subtree を作り、検査する。
func NewSubtree(name, filter string) (Subtree, error) {
	if !subtreeName.MatchString(name) {
		return Subtree{}, fmt.Errorf("subtree %q: the name must match %s", name, subtreeName)
	}
	top, err := parseXML([]byte(filter))
	if err != nil {
		return Subtree{}, fmt.Errorf("subtree %s: filter: %v", name, err)
	}
	if len(top) == 0 {
		return Subtree{}, fmt.Errorf("subtree %s: filter: no element", name)
	}
	st := Subtree{Name: name, Filter: strings.TrimSpace(filter)}
	for _, n := range top {
		if n.name.Space == "" {
			return Subtree{}, fmt.Errorf("subtree %s: filter: <%s> has no namespace (write xmlns=\"…\": a filter without one matches every namespace)", name, n.name.Local)
		}
		st.top = append(st.top, nameKey{n.name.Space, n.name.Local})
	}
	return st, nil
}

var (
	subtreesTopKeys = map[string]bool{"subtrees": true}
	subtreeKeys     = map[string]bool{"filter": true, "filter_file": true}
)

// LoadSubtrees は path の部分木の一覧を読み、検査する。
func LoadSubtrees(path string) (map[string]Subtree, error) {
	tree, err := loader.ReadTree(path)
	if err != nil {
		return nil, fmt.Errorf("netconf subtrees: %w", err)
	}
	out, err := parseSubtrees(tree, filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("netconf subtrees %s: %w", path, err)
	}
	return out, nil
}

func parseSubtrees(tree map[string]any, dir string) (map[string]Subtree, error) {
	if err := unknownKeys("", tree, subtreesTopKeys); err != nil {
		return nil, err
	}
	raw, ok := tree["subtrees"].(map[string]any)
	if !ok || len(raw) == 0 {
		return nil, errors.New(`"subtrees" must be a non-empty map of name → {filter | filter_file}`)
	}
	names := make([]string, 0, len(raw))
	for n := range raw {
		names = append(names, n)
	}
	sort.Strings(names)
	out := map[string]Subtree{}
	for _, name := range names {
		p := "subtrees." + name
		m, ok := raw[name].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: must be a map", p)
		}
		if err := unknownKeys(p+".", m, subtreeKeys); err != nil {
			return nil, err
		}
		filter, hasFilter := m["filter"].(string)
		file, hasFile := m["filter_file"].(string)
		if _, ok := m["filter"]; ok && !hasFilter {
			return nil, fmt.Errorf("%s.filter: want a string", p)
		}
		if _, ok := m["filter_file"]; ok && !hasFile {
			return nil, fmt.Errorf("%s.filter_file: want a string", p)
		}
		switch {
		case hasFilter == hasFile:
			return nil, fmt.Errorf("%s: write exactly one of filter and filter_file", p)
		case hasFile:
			if !filepath.IsAbs(file) {
				file = filepath.Join(dir, file)
			}
			data, err := os.ReadFile(file)
			if err != nil {
				return nil, fmt.Errorf("%s.filter_file: %w", p, err)
			}
			filter = string(data)
		}
		st, err := NewSubtree(name, filter)
		if err != nil {
			return nil, err
		}
		out[name] = st
	}
	return out, nil
}

func unknownKeys(prefix string, m map[string]any, allowed map[string]bool) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !allowed[k] {
			return fmt.Errorf("unknown key %q", prefix+k)
		}
	}
	return nil
}
