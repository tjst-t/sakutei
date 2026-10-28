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

package nftables

// sets.go — ルール文の中の無名の集合を比べるための正規形（Backlog #178）。
// 正規形そのもの（NormalizeSets）は葉の package internal/nftrule にあり、網の間の
// 許可の変換（pkg/netpolicy、Backlog #174）も同じ関数でルールを作る。

import (
	"strings"

	"github.com/tjst-t/sakutei/v2/internal/nftrule"
)

// ruleKey はルール文を比べるための形: canonRule（空白・カウンタ）の上に
// 無名の集合の正規形（NormalizeSets）を重ねたもの。
func ruleKey(s string) string { return NormalizeSets(canonRule(s)) }

// rulesKey は RuleSep で join したルール列の各ルールを ruleKey にする。
func rulesKey(enc string) string {
	rules := splitRules(enc)
	for i, r := range rules {
		rules[i] = ruleKey(r)
	}
	return strings.Join(rules, RuleSep)
}

// NormalizeSets はルール文の中の無名の集合を正規形に書き直す（nftrule.NormalizeSets）。
func NormalizeSets(rule string) string { return nftrule.NormalizeSets(rule) }
