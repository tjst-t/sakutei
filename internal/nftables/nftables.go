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

// Package nftables provides a provider.Provider and provider.Lister
// implementation that manages firewall chains in a managed nftables-style
// ruleset file (§27 の「FW（順序付き）」, ROADMAP Sc7a647-1). これは DNS に
// 続く 2 つ目の実 provider であり、DNS が使わなかった Schema の `ordered`
// フラグ（順序が意味を持つ list フィールド）を end-to-end で行使することを
// 主眼とする。
//
// 設計判断（dnszone.go に揃える）:
//
//   - stdlib only: bufio.Scanner + strings による nftables サブセットの
//     parse。`nft` バイナリは呼び出さない（offline/CI 検証可）。宣言する
//     現実は「管理された ruleset ファイルそのもの」であり、Read は parse・
//     Apply は atomic write を行う。
//   - resource = 1 つの chain。identity は {table, chain}。ルール列（rules）
//     は `Ordered:true` の list フィールドで、順序そのものが意味を持つ。
//     firewall の chain はルールの順序評価（先頭マッチ勝ち）なので、同じ
//     ルール集合でも並び順が違えば別の現実であり、並べ替えは diff（Update）
//     として扱われる。これが本 provider の主たる実証点である。
//   - identity は rules の内容ではなく chain の座標 {table, chain}。ルールを
//     並べ替えても identity は不変であり、同一 chain の Update として現れる。
//   - Base chain の除外: `type ... hook ... priority ... policy ...` を宣言
//     する base chain（NAT/filter hook を持つ）は、hook/priority/policy を
//     この provider が管理しないため、管理境界外として PermanentError を
//     返す（dnszone の SOA/NS skip の類推）。regular chain（hook を持たない）
//     のみを管理する。
//   - Atomic write: Apply は os.CreateTemp + os.Rename でファイル全体を
//     置換するので、途中クラッシュでも ruleset が truncate されない
//     （S2 progress class, §4.9）。
//   - Pruneable: false — firewall chain は破壊的型である（chain を消すと
//     その chain のルールが一括で失われ、通信断・穴あきの outage に直結）。
//     ADR-0005「破壊的型は pruneable:false 既定」に従い、orphan の自動削除は
//     行わない（明示 opt-in が必要）。
//
// Ruleset ファイル形式（管理サブセット）:
//
//	table <family> <table> {
//	    chain <chain> {
//	        <rule text>
//	        <rule text>
//	        ...
//	    }
//	}
//
// - table 行の <family> は "ip"/"ip6"/"inet" 等。table 名・chain 名は識別子。
// - chain ブロック内の各行が 1 ルール（順序保存）。
// - `type ... hook ...` を含む chain 行は base chain（管理対象外）として扱う。
// - `#` から始まる行はコメント。空行は無視。
// - このサブセット外の構文（set/map/named counter 等）は parse しないので、
//   本 provider は「自分が emit する構文のみを round-trip する」ことを honest
//   な限界として明記する（想定外行は当該 chain のルールとして素通しせず、
//   ファイル全体を書き換える Apply が既存の未知ブロックを消し得る点に注意
//   ——本 provider が管理するのは自分の書いた ruleset ファイルのみである）。
//
// Local ID convention: "<family>/<table>/<chain>"（例 "ip/filter/input"）。
// これは合成 identity {family, table, chain} を収束コアが Read/Apply に渡す
// 単一の不透明文字列へ符号化したもの。
//
// rules フィールドのエンコード: intent/diff レイヤでは string→string マップ
// なので、順序付きルール列は区切り文字 " ; " で join した 1 本の文字列として
// 宣言/観測する（RuleSep）。文字列比較で順序差がそのまま diff になる。
package nftables

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// ProviderName is the canonical name for this provider in the registry.
const ProviderName = "nftables"

// providerVersion is a hard-coded source hash for this provider version.
// 正規化/diff ロジック（RuleSep やルール正規化）を変えたら増分すること
// （cause-tag C（ghost diff）検出のため）。
const providerVersion = "nftables-v1.0.0"

// RuleSep はルール列を 1 本の文字列へ join/split する区切り。
// intent の fields は string→string なので、順序付き rules をこの区切りで
// エンコードする。ルール本文に含まれない安全な区切りとして " ; " を用いる。
const RuleSep = " ; "

// chainRes は ruleset ファイルから parse した 1 つの chain。
type chainRes struct {
	// family は table family（"ip"/"ip6"/"inet"/"arp"/"bridge"/"netdev"）。
	family string
	// table は table 名。
	table string
	// chain は chain 名。
	chain string
	// rules は chain 内のルール列（宣言順を保存する ordered list）。
	rules []string
	// base は base chain（type/hook を持つ）かどうか。base chain は管理対象外。
	base bool
}

// localID は "<family>/<table>/<chain>" を返す。
func (c chainRes) localID() string {
	return c.family + "/" + c.table + "/" + c.chain
}

// rulesEncoded は rules を RuleSep で join した 1 本の文字列を返す。
func (c chainRes) rulesEncoded() string {
	return strings.Join(c.rules, RuleSep)
}

// Provider manages nftables chains in a managed ruleset file.
// provider.Provider と provider.Lister を実装する。
type Provider struct {
	// rulesetPath は ruleset ファイルのパス。
	rulesetPath string
}

// New creates a new nftables Provider backed by the ruleset file at path.
// ファイルはまだ存在しなくてよい（Apply が作成する）。
func New(rulesetPath string) *Provider {
	return &Provider{rulesetPath: rulesetPath}
}

// Schema returns the field declarations for a firewall chain.
//
// Fields:
//   - family (string, identity, required): table family（ip/ip6/inet…）。
//   - table  (string, identity, required): table 名。
//   - chain  (string, identity, required): chain 名。
//   - rules  (string-list, ORDERED, required): ルール列。順序が意味を持つ。
//
// identity は {family, table, chain} = chain の座標。rules は identity では
// ない（並べ替えても同じ chain の Update として現れる）。rules に
// Ordered:true を宣言するのが本 provider の主眼。
func (p *Provider) Schema() provider.Schema {
	return provider.Schema{
		Fields: map[string]provider.FieldSpec{
			"family": {
				Kind:     core.FieldKindString,
				Required: true,
				Identity: true,
			},
			"table": {
				Kind:     core.FieldKindString,
				Required: true,
				Identity: true,
			},
			"chain": {
				Kind:     core.FieldKindString,
				Required: true,
				Identity: true,
			},
			"rules": {
				Kind:     core.FieldKindStringList,
				Required: true,
				Ordered:  true,
				// ListSep は diff の set 正規化（Ordered:false 時）用の区切り
				// 宣言。rules は Ordered:true なので正規化はされず verbatim
				// 比較（並べ替え=diff）だが、Schema を self-describing にするため
				// provider の canonical separator を宣言しておく。
				ListSep: RuleSep,
			},
		},
	}
}

// Capabilities declares this provider's capabilities（honest 宣言）。
//
// TrueObserve: true — ruleset ファイルが真実源で、Read は実状態を返す。
// List: true — ファイル内の全 chain を列挙できる。
// Pruneable: false — firewall chain は破壊的型（消すと通信断）。ADR-0005 に
// より orphan の自動削除は行わない。
// ProviderVersion: fail-closed な ghost-diff 検出用の固定ソースハッシュ。
func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		TrueObserve:     true,
		List:            true,
		Pruneable:       false,
		ProviderVersion: providerVersion,
	}
}

// Read observes the current state of the chain identified by localID.
//
// localID format: "<family>/<table>/<chain>"（例 "ip/filter/input"）。
//
// Constraints (ADR-0002):
//   - desired を一切参照しない。
//   - ruleset ファイルが存在しなければ absent（Ok, nil State）。
//   - parse 不能なら AvailabilityUnknown（transient・unknown≠absent）。
//   - base chain（type/hook を持つ）は管理境界外で PermanentError。
func (p *Provider) Read(_ context.Context, localID string) (core.Observation, error) {
	family, table, chain, err := splitLocalID(localID)
	if err != nil {
		return core.Observation{
			Availability: core.AvailabilityUnknown,
			ObservedAt:   time.Now(),
		}, provider.NewPermanentError(fmt.Errorf("nftables: Read: invalid local id %q: %w", localID, err))
	}

	chains, err := p.parseRuleset()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// ファイル非存在 → chain は absent。
			return core.Observation{
				Availability: core.AvailabilityOk,
				State:        nil,
				ObservedAt:   time.Now(),
			}, nil
		}
		// parse/read エラー → unknown（transient: 書込中/ロック中かもしれない）。
		return core.Observation{
			Availability: core.AvailabilityUnknown,
			ObservedAt:   time.Now(),
		}, provider.NewTransientError(fmt.Errorf("nftables: Read: parse ruleset %s: %w", p.rulesetPath, err))
	}

	for _, c := range chains {
		if !sameChain(c, family, table, chain) {
			continue
		}
		if c.base {
			// base chain は管理境界外。silently 誤管理せず PermanentError。
			return core.Observation{
				Availability: core.AvailabilityUnknown,
				ObservedAt:   time.Now(),
			}, provider.NewPermanentError(fmt.Errorf(
				"nftables: Read: refusing to manage base chain %q (has type/hook/policy; management boundary)", localID))
		}
		st := core.State{
			"family": {Kind: core.FieldKindString, Value: c.family},
			"table":  {Kind: core.FieldKindString, Value: c.table},
			"chain":  {Kind: core.FieldKindString, Value: c.chain},
			"rules":  {Kind: core.FieldKindStringList, Value: c.rulesEncoded()},
		}
		return core.Observation{
			Availability: core.AvailabilityOk,
			State:        st,
			ObservedAt:   time.Now(),
		}, nil
	}

	// chain がファイルに無い → absent。
	return core.Observation{
		Availability: core.AvailabilityOk,
		State:        nil,
		ObservedAt:   time.Now(),
	}, nil
}

// Apply converges the chain to the desired state described in plan.
//
// Create/Update/Delete を扱う。base chain は決して変更しない。
// 書込は atomic（temp + rename）。
//
// idempotent: 同一 plan を二度適用しても同じファイル内容になる。
func (p *Provider) Apply(_ context.Context, plan core.Plan) error {
	for _, op := range plan.Ops {
		if err := p.applyOp(op); err != nil {
			return err
		}
	}
	return nil
}

// applyOp applies a single Op to the ruleset file.
func (p *Provider) applyOp(op core.Op) error {
	localID := op.Target.Local
	family, table, chain, err := splitLocalID(localID)
	if err != nil {
		return provider.NewPermanentError(fmt.Errorf("nftables: Apply: invalid local id %q: %w", localID, err))
	}

	// Guard: 既存が base chain なら触らない。
	if existing, gerr := p.readChain(family, table, chain); gerr == nil && existing != nil && existing.base {
		return provider.NewPermanentError(fmt.Errorf(
			"nftables: Apply: refusing to manage base chain %q (management boundary)", localID))
	}

	desired := make(map[string]string, len(op.Fields))
	for _, fd := range op.Fields {
		desired[fd.Field] = fd.To
	}

	switch op.Kind {
	case core.OpKindCreate, core.OpKindUpdate:
		rulesEnc := desired["rules"]
		// Update で rules フィールドが FieldDiff に無い場合は現状を保持する
		// （変わったフィールドだけが FieldDiff に現れるため）。
		if op.Kind == core.OpKindUpdate {
			if _, ok := desired["rules"]; !ok {
				if cur, rerr := p.readChain(family, table, chain); rerr == nil && cur != nil {
					rulesEnc = cur.rulesEncoded()
				}
			}
		}
		newChain := chainRes{
			family: family,
			table:  table,
			chain:  chain,
			rules:  splitRules(rulesEnc),
			base:   false,
		}
		return p.writeChain(newChain)

	case core.OpKindDelete:
		return p.deleteChain(family, table, chain)

	default:
		return provider.NewPermanentError(fmt.Errorf("nftables: Apply: unknown op kind %v for %s", op.Kind, localID))
	}
}

// List returns all managed chain local IDs in the ruleset file.
//
// Completeness contract: ファイル全体を読み、base chain を除く全 chain を返す。
// ファイル非存在は ListComplete + 空（現実が空）。parse 失敗は error
// （truncated な ListComplete は返さない — complete-or-error, §5.4）。
func (p *Provider) List(_ context.Context, _ core.Scope) (core.ListResult, error) {
	chains, err := p.parseRuleset()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return core.ListResult{IDs: nil, Status: core.ListComplete}, nil
		}
		return core.ListResult{}, provider.NewTransientError(
			fmt.Errorf("nftables: List: parse ruleset %s: %w", p.rulesetPath, err))
	}

	scope := core.NewScope(nil)
	ids := make([]core.ID, 0, len(chains))
	for _, c := range chains {
		if c.base {
			// base chain は管理対象外 → List にも出さない。
			continue
		}
		ids = append(ids, core.NewID(ProviderName, scope, c.localID()))
	}
	return core.ListResult{IDs: ids, Status: core.ListComplete}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ─────────────────────────────────────────────────────────────────────────────

// splitLocalID splits "<family>/<table>/<chain>" into its three parts.
func splitLocalID(localID string) (family, table, chain string, err error) {
	parts := strings.Split(localID, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", fmt.Errorf("expected '<family>/<table>/<chain>' format")
	}
	return parts[0], parts[1], parts[2], nil
}

// sameChain reports whether c matches the given coordinates (case-sensitive:
// nftables 識別子は case-sensitive)。
func sameChain(c chainRes, family, table, chain string) bool {
	return c.family == family && c.table == table && c.chain == chain
}

// splitRules は RuleSep で join されたルール列文字列を []string に戻す。
// 空文字列は空スライス（ルールなしの chain）を返す。
func splitRules(enc string) []string {
	if strings.TrimSpace(enc) == "" {
		return nil
	}
	parts := strings.Split(enc, RuleSep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t == "" {
			continue
		}
		out = append(out, t)
	}
	return out
}

// readChain は ruleset ファイルから 1 つの chain を {family,table,chain} で読む。
// 見つからない/ファイル非存在なら nil を返す。
func (p *Provider) readChain(family, table, chain string) (*chainRes, error) {
	chains, err := p.parseRuleset()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	for i, c := range chains {
		if sameChain(c, family, table, chain) {
			return &chains[i], nil
		}
	}
	return nil, nil
}

// writeChain inserts or replaces a chain in the ruleset file (Create/Update)。
// 同一 {family,table,chain} の既存ブロックは差し替える。atomic write。
func (p *Provider) writeChain(newChain chainRes) error {
	chains, err := p.parseRuleset()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return provider.NewTransientError(fmt.Errorf("nftables: writeChain: parse ruleset: %w", err))
	}

	// 既存を差し替え or 追加。
	replaced := false
	out := make([]chainRes, 0, len(chains)+1)
	for _, c := range chains {
		if sameChain(c, newChain.family, newChain.table, newChain.chain) {
			out = append(out, newChain)
			replaced = true
			continue
		}
		out = append(out, c)
	}
	if !replaced {
		out = append(out, newChain)
	}

	return p.writeRuleset(out)
}

// deleteChain removes a chain from the ruleset file。atomic write。
func (p *Provider) deleteChain(family, table, chain string) error {
	chains, err := p.parseRuleset()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// ファイル非存在 → 削除は no-op（idempotent）。
			return nil
		}
		return provider.NewTransientError(fmt.Errorf("nftables: deleteChain: parse ruleset: %w", err))
	}

	out := make([]chainRes, 0, len(chains))
	for _, c := range chains {
		if sameChain(c, family, table, chain) {
			continue
		}
		out = append(out, c)
	}
	return p.writeRuleset(out)
}

// parseRuleset reads and parses the ruleset file into chains。
// os.ErrNotExist を保持して返す（ファイル非存在時）。
func (p *Provider) parseRuleset() ([]chainRes, error) {
	f, err := os.Open(p.rulesetPath)
	if err != nil {
		return nil, err // os.ErrNotExist は errors.Is で保たれる
	}
	defer f.Close()

	var chains []chainRes
	var cur *chainRes
	var curFamily, curTable string
	inTable := false

	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		switch {
		case strings.HasPrefix(line, "table "):
			// "table <family> <table> {"
			fields := strings.Fields(strings.TrimSuffix(line, "{"))
			if len(fields) < 3 {
				return nil, fmt.Errorf("line %d: malformed table header %q", lineNo, line)
			}
			curFamily = fields[1]
			curTable = fields[2]
			inTable = true

		case strings.HasPrefix(line, "chain "):
			// "chain <name> {"（regular chain）
			if !inTable {
				return nil, fmt.Errorf("line %d: chain outside table: %q", lineNo, line)
			}
			fields := strings.Fields(strings.TrimSuffix(line, "{"))
			if len(fields) < 2 {
				return nil, fmt.Errorf("line %d: malformed chain header %q", lineNo, line)
			}
			c := chainRes{family: curFamily, table: curTable, chain: fields[1]}
			chains = append(chains, c)
			cur = &chains[len(chains)-1]

		case line == "}":
			// chain または table ブロックの終端。
			if cur != nil {
				cur = nil
			} else {
				inTable = false
			}

		case strings.HasPrefix(line, "type ") && cur != nil:
			// base chain の宣言（"type ... hook ... priority ...; policy ...;"）。
			// hook を持つ chain は base chain として印を付け、管理対象外にする。
			if strings.Contains(line, "hook ") {
				cur.base = true
			}
			// type 行自体はルールとして扱わない。

		default:
			if cur != nil {
				// chain ブロック内の行 = 1 ルール（順序保存）。
				cur.rules = append(cur.rules, line)
			}
			// table 直下・ブロック外の未知行は無視（honest な限界: 本 provider
			// が管理するのは自分の書いた ruleset のみ）。
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan ruleset file: %w", err)
	}
	return chains, nil
}

// writeRuleset writes chains to the ruleset file atomically（temp + rename）。
// family/table でグループ化して table ブロックを emit する。
func (p *Provider) writeRuleset(chains []chainRes) error {
	// family+table 単位でグループ化（安定順: 出現順を保つため順序リストを使う）。
	type tableKey struct{ family, table string }
	order := make([]tableKey, 0, len(chains))
	seen := make(map[tableKey]bool)
	grouped := make(map[tableKey][]chainRes)
	for _, c := range chains {
		k := tableKey{c.family, c.table}
		if !seen[k] {
			seen[k] = true
			order = append(order, k)
		}
		grouped[k] = append(grouped[k], c)
	}

	var b strings.Builder
	b.WriteString("# Managed by Sakutei nftables provider — do not edit by hand.\n")
	for _, k := range order {
		fmt.Fprintf(&b, "table %s %s {\n", k.family, k.table)
		for _, c := range grouped[k] {
			fmt.Fprintf(&b, "\tchain %s {\n", c.chain)
			for _, r := range c.rules {
				fmt.Fprintf(&b, "\t\t%s\n", r)
			}
			b.WriteString("\t}\n")
		}
		b.WriteString("}\n")
	}

	dir := filepath.Dir(p.rulesetPath)
	tmp, err := os.CreateTemp(dir, "sakutei-nftables-*.tmp")
	if err != nil {
		return provider.NewTransientError(fmt.Errorf("nftables: writeRuleset: create temp: %w", err))
	}
	tmpPath := tmp.Name()

	if _, werr := tmp.WriteString(b.String()); werr != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return provider.NewTransientError(fmt.Errorf("nftables: writeRuleset: write: %w", werr))
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return provider.NewTransientError(fmt.Errorf("nftables: writeRuleset: close temp: %w", err))
	}

	// Atomic rename over the target path.
	if err := os.Rename(tmpPath, p.rulesetPath); err != nil {
		os.Remove(tmpPath)
		return provider.NewTransientError(fmt.Errorf("nftables: writeRuleset: rename: %w", err))
	}
	return nil
}
