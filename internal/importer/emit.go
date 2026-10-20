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

package importer

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// bareKey is the key spelling the loader's YAML subset accepts unquoted.
var bareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// EmitYAML renders the resources of one provider as an intent file in the
// loader's YAML subset (internal/loader/yaml.go).
//
// Every scalar value is double-quoted, so nothing is re-typed on the way back
// in (no Norway problem, "1.10" stays "1.10").  Output is deterministic: the
// same Result gives the same bytes.  samples is only used in the header.
//
// Each resource is written with deletion_policy: ExplicitAbsentOnly — an
// imported intent is a snapshot and must never enable closed-world prune
// before a person has reviewed it (§24.1).
func EmitYAML(resources []Resource, samples int) []byte {
	var b strings.Builder
	b.WriteString("# sakutei import で生成（reverse-render, 設計 §24.4）。\n")
	b.WriteString("#\n")
	b.WriteString("# これは「理想」ではなく「現状のスナップショット」です。設定ミスやレガシーも\n")
	b.WriteString("# そのまま入っています。人が読んで意図を反映してから intent として使ってください。\n")
	b.WriteString("# 導入初期は prune を禁止するため、全リソースを deletion_policy: ExplicitAbsentOnly に\n")
	b.WriteString("# しています（§24.1）。まずは --mode alert-only で動かして観察してください（§24.2）。\n")
	if samples > 1 {
		fmt.Fprintf(&b, "# managed 候補: 現実を %d 回観測して値が変わったフィールドに印を付けています。\n", samples)
		b.WriteString("# 外部（DRS・DHCP など）が動かす値なら Schema で managed にするか、その行を消してください。\n")
	} else {
		b.WriteString("# 観測が 1 回だけなので、managed 候補（観測のたびに値が変わるフィールド）は調べていません。\n")
	}
	b.WriteString("resources:\n")

	for _, r := range resources {
		fmt.Fprintf(&b, "  - provider: %s\n", quote(r.Provider))
		fmt.Fprintf(&b, "    id: %s\n", quote(r.ID.Local))
		if len(r.ID.Scope.Labels) > 0 {
			b.WriteString("    scope:\n")
			for _, k := range sortedKeys(r.ID.Scope.Labels) {
				fmt.Fprintf(&b, "      %s: %s\n", key(k), quote(r.ID.Scope.Labels[k]))
			}
		}
		b.WriteString("    deletion_policy: \"ExplicitAbsentOnly\"\n")
		if len(r.Fields) > 0 {
			b.WriteString("    fields:\n")
			for _, f := range sortedKeys(r.Fields) {
				if vals, ok := r.ManagedCandidates[f]; ok {
					q := make([]string, len(vals))
					for i, v := range vals {
						q[i] = quote(v)
					}
					fmt.Fprintf(&b, "      # managed 候補: 観測のたびに値が変化（%s）\n", strings.Join(q, " → "))
				}
				fmt.Fprintf(&b, "      %s: %s\n", key(f), quote(r.Fields[f]))
			}
		}
		for _, f := range sortedKeys(r.Omitted) {
			fmt.Fprintf(&b, "    # 書き出していないフィールド %s: %s\n", f, oneLine(r.Omitted[f]))
		}
	}
	return []byte(b.String())
}

// quote renders s as a double-quoted scalar using only the escapes the
// loader's unquoteDouble understands.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, c := range s {
		switch c {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func key(k string) string {
	if bareKey.MatchString(k) {
		return k
	}
	return quote(k)
}

func oneLine(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ").Replace(s)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
