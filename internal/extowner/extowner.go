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

// Package extowner は触らない宣言（external_owner、§7.3、Backlog #158）の
// 選び方と照合を持つ。
//
// intent の external_owners: の 1 件が 1 つの Selector になる。選ばれたものは
// Sakutei が観測しても Apply・Delete・prune をしない。選び方は provider に
// 依らない（scope・provider・ID の完全一致か前方一致）に加えて、観測した
// フィールドの値（例 proxmox-vm の vmid）で選べる。フィールドで選べるのは、
// provider が provider.OwnerFieldSelector で申告したフィールドだけ。
//
// provider の設定にある除外（provider.ConfigExcluder、例 proxmox-vm の
// protected_vmids・protected_names）は最後の安全の網として残り、同じ Selector
// の形に直して intent と突き合わせる（Check）。
package extowner

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// Selector は触らない範囲の 1 つ。空の項目は「何でもよい」。どの項目も
// 満たすものが選ばれる（AND）。
type Selector struct {
	// Owner は持ち主の名前（drs・mha・人の名前など）。知らせの文言に出る。
	Owner string
	// Provider は provider の名前。空はどの provider でも。
	Provider string
	// Scope は ID の scope を選ぶ（その scope と、その下の scope）。
	Scope core.Scope
	// ID は provider の中の ID の完全一致、IDPrefix は前方一致。どちらか一方。
	ID       string
	IDPrefix string
	// Fields は観測したフィールドの値の完全一致（文字列で比べる）。
	Fields map[string]string
	// Source はどこの宣言か（"intent" か "provider config <設定のキー>"）。
	Source string
}

// FromIntent は intent に書かれた Selector か。
func (s Selector) FromIntent() bool { return s.Source == "" || s.Source == "intent" }

// MatchesID は ID の側（provider・scope・ID）が合うか。Fields は見ない。
func (s Selector) MatchesID(id core.ID) bool {
	if s.Provider != "" && id.Provider != s.Provider {
		return false
	}
	if !id.Scope.Matches(s.Scope) {
		return false
	}
	if s.ID != "" && id.Local != s.ID {
		return false
	}
	if s.IDPrefix != "" && !strings.HasPrefix(id.Local, s.IDPrefix) {
		return false
	}
	return true
}

// MatchesFields は Fields がみな values と同じ値か（values に無いフィールドは
// 合わない）。Fields が空なら真。
func (s Selector) MatchesFields(values map[string]string) bool {
	for k, v := range s.Fields {
		got, ok := values[k]
		if !ok || got != v {
			return false
		}
	}
	return true
}

// Describe は知らせの文言に使う短い形。
func (s Selector) Describe() string {
	var parts []string
	if s.Provider != "" {
		parts = append(parts, "provider "+s.Provider)
	}
	if len(s.Scope.Labels) > 0 {
		parts = append(parts, "scope "+s.Scope.String())
	}
	if s.ID != "" {
		parts = append(parts, fmt.Sprintf("id %q", s.ID))
	}
	if s.IDPrefix != "" {
		parts = append(parts, fmt.Sprintf("id_prefix %q", s.IDPrefix))
	}
	if len(s.Fields) > 0 {
		keys := make([]string, 0, len(s.Fields))
		for k := range s.Fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fs := make([]string, 0, len(keys))
		for _, k := range keys {
			fs = append(fs, k+"="+s.Fields[k])
		}
		parts = append(parts, "fields "+strings.Join(fs, ","))
	}
	owner := s.Owner
	if owner == "" {
		owner = "?"
	}
	src := s.Source
	if src == "" {
		src = "intent"
	}
	return fmt.Sprintf("external_owner %s (%s; %s)", owner, strings.Join(parts, ", "), src)
}

// Validate は 1 件の書き方を検査する（読み込みの時点、provider を見ずに決まる分）。
func (s Selector) Validate() error {
	if strings.TrimSpace(s.Owner) == "" {
		return fmt.Errorf("owner is empty; name who owns the range (§7.3)")
	}
	if s.ID != "" && s.IDPrefix != "" {
		return fmt.Errorf("id and id_prefix cannot both be declared")
	}
	if s.Provider == "" && len(s.Scope.Labels) == 0 {
		// 何も選ばない宣言は世界の全部を外の持ち主に渡してしまう（全収束が止まる）。
		return fmt.Errorf("declare provider or a non-empty scope (an entry selecting everything would hand the whole world to the owner)")
	}
	if (s.ID != "" || s.IDPrefix != "" || len(s.Fields) > 0) && s.Provider == "" {
		return fmt.Errorf("id / id_prefix / fields are provider-local: declare provider with them")
	}
	for k := range s.Fields {
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("fields: an empty field name")
		}
	}
	return nil
}

// Set は触らない宣言の全部。
type Set []Selector

// ByID は Fields を持たず ID だけで選ぶ Selector のうち、id に合う最初のもの。
// 観測しなくても選ばれると決まる（prune の候補を Read する前に外せる）。
func (set Set) ByID(id core.ID) (Selector, bool) {
	for _, s := range set {
		if len(s.Fields) == 0 && s.MatchesID(id) {
			return s, true
		}
	}
	return Selector{}, false
}

// ByState は観測した state で選ぶ：ID の側が合い、Fields が state の値と
// 合う最初の Selector（Fields の無いものも含む）。
func (set Set) ByState(id core.ID, state core.State) (Selector, bool) {
	var values map[string]string
	for _, s := range set {
		if !s.MatchesID(id) {
			continue
		}
		if len(s.Fields) == 0 {
			return s, true
		}
		if state == nil {
			continue
		}
		if values == nil {
			values = StateStrings(state)
		}
		if s.MatchesFields(values) {
			return s, true
		}
	}
	return Selector{}, false
}

// HasFieldSelectors は観測の値で選ぶ Selector があるか（ある provider の分）。
func (set Set) HasFieldSelectors(providerName string) bool {
	for _, s := range set {
		if len(s.Fields) > 0 && (s.Provider == "" || s.Provider == providerName) {
			return true
		}
	}
	return false
}

// StateStrings は観測した値を文字列にする（diff と同じ %v の形）。
func StateStrings(state core.State) map[string]string {
	out := make(map[string]string, len(state))
	for k, tv := range state {
		out[k] = fmt.Sprintf("%v", tv.Value)
	}
	return out
}

// Declares は宣言 r が s の範囲のものを管理する（か absent で消す）と、
// 読み込みの時点で分かるか。Fields を持つ s は、r がその全部を同じ値で
// 宣言しているときだけ真（採番で決まる値・宣言しない値は観測まで分からない。
// その分は観測の時点で止める、ByState）。
func Declares(s Selector, r diff.DesiredResource) bool {
	if !s.MatchesID(r.ID) {
		return false
	}
	if len(s.Fields) == 0 {
		return true
	}
	if r.Absent {
		return false
	}
	return s.MatchesFields(r.Fields)
}

// ValidateDesired は intent の触らない宣言の範囲のものを、intent が管理すると
// 宣言していないかを検査する。管理していれば断る（§7.3、Backlog #158）。
func ValidateDesired(set Set, desired []diff.DesiredResource) error {
	for _, r := range desired {
		for _, s := range set {
			if Declares(s, r) {
				verb := "declares"
				if r.Absent {
					verb = "declares absent"
				}
				return fmt.Errorf("resource %s: the intent %s it, but it is in %s: Sakutei observes it and never applies, deletes or prunes it (§7.3); take it out of one of them", r.ID, verb, s.Describe())
			}
		}
	}
	return nil
}

// FromConfig は provider の設定の除外を Selector の形にする。
func FromConfig(providerName string, ex provider.ConfigExclusion) Selector {
	return Selector{
		Owner:    "provider config",
		Provider: providerName,
		Scope:    ex.Scope,
		ID:       ex.ID,
		IDPrefix: ex.IDPrefix,
		Fields:   ex.Fields,
		Source:   "provider config " + ex.Setting,
	}
}

// Covers は intent の s が、provider の設定の除外 c の選ぶものを全部選ぶか。
// 判らないとき（s がもっと狭い・別のフィールドで選ぶ）は偽（知らせる側に倒す）。
func Covers(s, c Selector) bool {
	if s.Provider != "" && s.Provider != c.Provider {
		return false
	}
	// s の scope が c の scope と同じか広いこと。
	if !c.Scope.Matches(s.Scope) {
		return false
	}
	switch {
	case s.ID != "":
		if c.ID != s.ID {
			return false
		}
	case s.IDPrefix != "":
		switch {
		case c.ID != "":
			if !strings.HasPrefix(c.ID, s.IDPrefix) {
				return false
			}
		case c.IDPrefix != "":
			if !strings.HasPrefix(c.IDPrefix, s.IDPrefix) {
				return false
			}
		default:
			return false
		}
	}
	// s の Fields は c の Fields に含まれる（c が同じ値で絞っている）こと。
	for k, v := range s.Fields {
		if c.Fields[k] != v {
			return false
		}
	}
	return true
}

// Check は provider を組み立てたあと（各経路の読み込みの入口）に行う検査：
//
//   - intent の Fields で選ぶ Selector は、その provider が
//     provider.OwnerFieldSelector で申告したフィールドだけを使う（断る）。
//   - provider の設定の除外（provider.ConfigExcluder）のものを intent が
//     管理すると宣言していれば断る（弱い側に寄せない）。
//   - provider の設定にだけある除外は、intent にも書くよう知らせる（warnings）。
//   - intent の触らない宣言の範囲を intent が管理していれば断る（ValidateDesired。
//     render のあと＝inventory・採番・合成を足したあとの desired で）。
//
// providers は名前 → provider。組み立てていない provider の Selector は
// 検査しない（その provider のものは今回は観測も Apply もしない）。
func Check(set Set, desired []diff.DesiredResource, providers map[string]provider.Provider) (warnings []string, err error) {
	if err := ValidateDesired(set, desired); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(providers))
	for n := range providers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, s := range set {
		if len(s.Fields) == 0 {
			continue
		}
		p, ok := providers[s.Provider]
		if !ok {
			continue
		}
		allowed := map[string]bool{}
		if f, ok := p.(provider.OwnerFieldSelector); ok {
			for _, k := range f.OwnerSelectFields() {
				allowed[k] = true
			}
		}
		keys := make([]string, 0, len(s.Fields))
		for k := range s.Fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !allowed[k] {
				return nil, fmt.Errorf("%s: provider %s does not offer field %q to select by (it offers %s, §7.3)", s.Describe(), s.Provider, k, offered(allowed))
			}
		}
	}
	for _, name := range names {
		ce, ok := providers[name].(provider.ConfigExcluder)
		if !ok {
			continue
		}
		for _, ex := range ce.ConfigExclusions() {
			c := FromConfig(name, ex)
			for _, r := range desired {
				if r.ID.Provider == name && Declares(c, r) {
					return nil, fmt.Errorf("resource %s: the intent manages it, but the provider config excludes it (%s); the two disagree — declare it in external_owners and take it out of the resources, or take it out of the provider config (§7.3, §29.1)", r.ID, c.Describe())
				}
			}
			covered := false
			for _, s := range set {
				if Covers(s, c) {
					covered = true
					break
				}
			}
			if !covered {
				warnings = append(warnings, fmt.Sprintf("%s is only in the provider config: write it in the intent too (external_owners, §7.3; the provider config keeps it as the last safety net, §29.1)", c.Describe()))
			}
		}
	}
	return warnings, nil
}

func offered(allowed map[string]bool) string {
	if len(allowed) == 0 {
		return "none"
	}
	ks := make([]string, 0, len(allowed))
	for k := range allowed {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ", ")
}
