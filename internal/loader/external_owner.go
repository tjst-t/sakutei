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

package loader

import (
	"fmt"
	"strings"

	"github.com/tjst-t/sakutei/v2/internal/compat"
	"github.com/tjst-t/sakutei/v2/internal/extowner"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// parseExternalOwners は intent のトップレベルの external_owners:（触らない
// 宣言、§7.3・Backlog #158）を読む:
//
//	external_owners:
//	  - owner: drs                  # 持ち主（必須）
//	    scope: { cluster: prod }    # その scope と、その下
//	  - owner: legacy-team
//	    provider: proxmox-vm
//	    id_prefix: lab/legacy-      # id（完全一致）か id_prefix（前方一致）
//	  - owner: stage0
//	    provider: proxmox-vm
//	    id_prefix: lab/
//	    fields: { vmid: 100 }       # 観測した値（provider が申告したフィールドだけ）
//
// 選ばれたものは観測しても Apply・Delete・prune をしない。
func parseExternalOwners(v any) (extowner.Set, error) {
	raw, _ := v.([]any)
	out := make(extowner.Set, 0, len(raw))
	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("external_owners[%d]: オブジェクトでなければなりません", i)
		}
		s := extowner.Selector{Source: "intent"}
		s.Owner, _ = m["owner"].(string)
		s.Provider, _ = m["provider"].(string)
		s.ID, _ = m["id"].(string)
		s.IDPrefix, _ = m["id_prefix"].(string)
		s.Owner = strings.TrimSpace(s.Owner)
		s.Provider = strings.TrimSpace(s.Provider)
		s.Scope = core.NewScope(stringifyStringMap(m["scope"]))
		if f := stringifyStringMap(m["fields"]); len(f) > 0 {
			s.Fields = f
		}
		if err := s.Validate(); err != nil {
			return nil, fmt.Errorf("external_owners[%d]: %w", i, err)
		}
		out = append(out, s)
	}
	return out, nil
}

// validateCompatibility は互換の表（§29.1、#151）の名前が intent のファイルを
// またいで一意かを確かめる。
func validateCompatibility(tables compat.Set) error {
	seen := map[string]bool{}
	for _, t := range tables {
		if seen[t.Name] {
			return fmt.Errorf("compatibility: name %q is declared twice", t.Name)
		}
		seen[t.Name] = true
	}
	return nil
}
