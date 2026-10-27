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

// Package nftables provides a provider.Provider, provider.Lister and
// provider.BulkObserver implementation that manages firewall chains in a
// managed nftables-style ruleset file (§27 の「FW（順序付き）」, 旧 ROADMAP Sc7a647-1). これは DNS に
// 続く 2 つ目の実 provider であり、DNS が使わなかった Schema の `ordered`
// フラグ（順序が意味を持つ list フィールド）を end-to-end で行使することを
// 主眼とする。
//
// 設計判断（dnszone.go に揃える）:
//
//   - stdlib only: strings による nftables サブセットの
//     parse。`nft` バイナリは呼び出さない（offline/CI 検証可）。宣言する
//     現実は「管理された ruleset ファイルそのもの」であり、Read は parse・
//     Apply は atomic write を行う。カーネルの実際の ruleset を扱うのは
//     live 版（LiveProvider、live.go、Backlog #121）で、Schema・identity・
//     local id は共通。
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
//   - 管理境界の外は壊さない（§7, Backlog #68）: parse はファイルの各行を
//     そのまま持ち、Apply は対象の chain の行（中身、または Create/Delete
//     ならブロック全体）だけを差し替えて書き戻す。base chain の宣言行と中身・
//     set/map・コメント・他の table はバイト単位で変わらない。
//   - Atomic write: Apply は os.CreateTemp + os.Rename でファイル全体を
//     置換するので、途中クラッシュでも ruleset が truncate されない
//     （S2 progress class, §4.9）。
//   - Apply の排他（Backlog #72）: Apply はファイル全体を読み、書き換え、
//     rename で置き換えるまでを ruleset のパスの filelock.Lock（プロセス内の
//     mutex + "<ruleset>.lock" の flock）の中で行う。デーモンは Apply を並行に
//     走らせ、plan・reconcile は別プロセスでも動くので、同じ古い中身を読んだ
//     2 つの Apply が片方の変更を消さないようにする。Read・List・BulkObserve は
//     共有の filelock.RLock を取る。
//   - Pruneable: false — firewall chain は破壊的型である（chain を消すと
//     その chain のルールが一括で失われ、通信断・穴あきの outage に直結）。
//     ADR-0005「破壊的型は pruneable:false 既定」に従い、orphan の自動削除は
//     行わない（明示 opt-in が必要）。
//   - BulkObserve: true — ruleset ファイルを 1 回読めば全 chain が分かるので、
//     1 周のファイル読み込みは chain 数によらず 1 回で済む（§5.7, Backlog #27）。
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
//   - table 行の <family> は "ip"/"ip6"/"inet" 等。table 名・chain 名は識別子。
//   - chain ブロック内の各行が 1 ルール（順序保存）。
//   - `type ... hook ...`（または `policy ...`）を含む chain は base chain
//     （管理対象外）として扱う。
//   - `#` から始まる行はコメント。空行は無視。
//   - table 直下の chain 以外のブロック（set/map/named counter 等）と table の
//     外の行は parse せず、Apply でもそのまま残す。
//   - chain の中の複数行にわたる構文（複数行の匿名 set 等）や 1 行で書いた
//     chain は書き直せないので opaque として管理対象外にし、Read・Apply は
//     PermanentError を返す（黙って書き換えない）。
//   - 管理する chain の中のコメント行は、その chain の Update/Delete で消える。
//   - 括弧が閉じきらない/閉じすぎのファイルは parse エラー（unknown）。
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

	"github.com/tjst-t/sakutei/v2/internal/filelock"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// Test seams (Backlog #72): the concurrency test swaps lockRuleset for a no-op to
// show that without the lock concurrent Applies lose changes, and widens the
// read-to-write window with beforeWrite.
var (
	lockRuleset    = filelock.Lock
	beforeWrite = func() {}
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
	// opaque は chain の中に管理サブセット外の構文（複数行の匿名 set 等）を
	// 含むかどうか。opaque な chain は書き直せないので管理対象外。
	opaque bool
	// start・end はファイル内の chain 行と閉じ括弧の行の番号（0 始まり）。
	start, end int
}

// unmanaged は chain が管理境界の外（base chain か opaque）かどうか。
func (c chainRes) unmanaged() bool {
	return c.base || c.opaque
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
// provider.Provider・provider.Lister・provider.BulkObserver を実装する。
type Provider struct {
	// rulesetPath は ruleset ファイルのパス。
	rulesetPath string
	// scope は List・BulkObserve が返す ID に付ける固定の scope
	// (WithScope, Backlog #106, Option A)。空（root scope）が既定。
	scope core.Scope
}

// Option configures optional Provider / LiveProvider behavior at construction
// time.
type Option func(*settings)

// settings は Option が設定する値（file 版と live 版で共通）。
type settings struct {
	scope core.Scope
}

// WithScope sets the fixed scope that List and BulkObserve tag every
// returned ID with（既定は root scope = core.NewScope(nil)）。
//
// 設計判断（Backlog #106, Option A, §27.5）: 1 つの Provider インスタンス
// = 1 つの ruleset ファイル = Sakutei の静的 claim シャーディング（§17）で
// いう 1 つの claim 分のリソース、という既存アーキテクチャにそのまま乗る
// 形で、scope は provider 設定（CLI の --ruleset-scope）で固定する。
// これにより:
//   - Sync Mode（generated/ への書き出し）が non-root scope の静的 scope
//     設定下でも空にならず、正しく scope 付きの snapshot を書ける。
//   - PruneWithinScope が non-root scope の orphan を取り落とさず見える。
//   - planned_change の削除数が、宣言と同じ scope の DELETE op に対して
//     正しく数えられる（root scope 固定だった旧挙動は scope がズレて
//     数え落としていた）。
func WithScope(scope core.Scope) Option {
	return func(s *settings) { s.scope = scope }
}

// New creates a new nftables Provider backed by the ruleset file at path.
// ファイルはまだ存在しなくてよい（Apply が作成する）。
func New(rulesetPath string, opts ...Option) *Provider {
	var s settings
	for _, opt := range opts {
		opt(&s)
	}
	return &Provider{rulesetPath: rulesetPath, scope: s.scope}
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
// BulkObserve: true — BulkObserve がファイル 1 回の読み込みで全 chain を観測する。
// ProviderVersion: fail-closed な ghost-diff 検出用の固定ソースハッシュ。
func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		TrueObserve:     true,
		List:            true,
		Pruneable:       false,
		BulkObserve:     true,
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
	defer filelock.RLock(p.rulesetPath)()
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
		if c.opaque {
			return core.Observation{
				Availability: core.AvailabilityUnknown,
				ObservedAt:   time.Now(),
			}, provider.NewPermanentError(fmt.Errorf(
				"nftables: Read: refusing to manage chain %q (contains syntax outside the managed subset)", localID))
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
// Create/Update/Delete を扱う。base chain・opaque な chain は決して変更せず
// PermanentError を返す。書き直すのは対象の chain の行だけで、他の行は
// バイト単位でそのまま残す。書込は atomic（temp + rename）。
//
// idempotent: 同一 plan を二度適用しても同じファイル内容になる。
func (p *Provider) Apply(_ context.Context, plan core.Plan) error {
	// 各 op の読み → 書き換え → rename を 1 つの排他区間で行う（Backlog #72）。
	unlock, err := lockRuleset(p.rulesetPath)
	if err != nil {
		return provider.NewTransientError(fmt.Errorf("nftables: Apply: %w", err))
	}
	defer unlock()
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
	defer filelock.RLock(p.rulesetPath)()
	chains, err := p.parseRuleset()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return core.ListResult{IDs: nil, Status: core.ListComplete}, nil
		}
		return core.ListResult{}, provider.NewTransientError(
			fmt.Errorf("nftables: List: parse ruleset %s: %w", p.rulesetPath, err))
	}

	// 同じ座標の chain が 2 回書かれていても ID は 1 回だけ出す（List は集合）。
	// Read・BulkObserve と同じく最初の出現で決め、それが base chain・opaque なら出さない。
	// scope は provider 設定の固定値（既定 root, WithScope で変えられる、Backlog #106）。
	scope := p.scope
	ids := make([]core.ID, 0, len(chains))
	seen := make(map[string]bool, len(chains))
	for _, c := range chains {
		if seen[c.localID()] {
			continue
		}
		seen[c.localID()] = true
		if c.unmanaged() {
			// base chain・opaque な chain は管理対象外 → List にも出さない。
			continue
		}
		ids = append(ids, core.NewID(ProviderName, scope, c.localID()))
	}
	return core.ListResult{IDs: ids, Status: core.ListComplete}, nil
}

// BulkObserve observes every managed chain in the ruleset file with ONE read of
// the file (§5.7)。各エントリは、その chain の local id に Read が返すものと同じ
// で、キーは List と同じ（provider 設定の固定 scope、"<family>/<table>/<chain>"）。
//
// Read は座標の一致する最初の chain を返すので、各エントリも最初の出現から作る。
// 最初の出現が base chain・opaque な chain の座標は返さない（Read はそこで PermanentError を返す
// ので、収束の経路はその chain を Read に回して同じエラーを得る）。
//
// complete-or-error: 読めない・解釈できないファイルは呼び出し全体のエラーにし、
// 一部だけを返さない。ファイル非存在は空の現実（エントリなし・エラーなし）で、
// 各リソースは Read に回り absent になる。結果は「何が無いか」を言わず、prune の
// 入力にもならない（core.ListResult ではない、ADR-0011）。
func (p *Provider) BulkObserve(_ context.Context, _ core.Scope) (provider.BulkObservation, error) {
	defer filelock.RLock(p.rulesetPath)()
	chains, err := p.parseRuleset()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return provider.BulkObservation{Observations: map[core.IDKey]core.Observation{}}, nil
		}
		return provider.BulkObservation{}, provider.NewTransientError(
			fmt.Errorf("nftables: BulkObserve: parse ruleset %s: %w", p.rulesetPath, err))
	}

	now := time.Now()
	scope := p.scope
	seen := make(map[string]bool, len(chains))
	obs := make(map[core.IDKey]core.Observation, len(chains))
	for _, c := range chains {
		if seen[c.localID()] {
			continue
		}
		seen[c.localID()] = true
		if c.unmanaged() {
			continue
		}
		obs[core.NewID(ProviderName, scope, c.localID()).Key()] = core.Observation{
			Availability: core.AvailabilityOk,
			State: core.State{
				"family": {Kind: core.FieldKindString, Value: c.family},
				"table":  {Kind: core.FieldKindString, Value: c.table},
				"chain":  {Kind: core.FieldKindString, Value: c.chain},
				"rules":  {Kind: core.FieldKindStringList, Value: c.rulesEncoded()},
			},
			ObservedAt: now,
		}
	}
	return provider.BulkObservation{Observations: obs}, nil
}

// Compile-time assertions.
var (
	_ provider.Provider     = (*Provider)(nil)
	_ provider.Lister       = (*Provider)(nil)
	_ provider.BulkObserver = (*Provider)(nil)
)

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
//
// 同一 {family,table,chain} の既存ブロックがあれば、その中身の行（chain 行と
// 閉じ括弧の間）だけを差し替える。無ければ同じ table の閉じ括弧の直前に chain
// ブロックを足し、table も無ければファイル末尾に table ブロックを足す。
// それ以外の行（base chain・set/map・コメント・他の table）はバイト単位で
// そのまま残す（§7 の管理境界）。atomic write。
func (p *Provider) writeChain(newChain chainRes) error {
	doc, err := p.parseDoc()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return provider.NewTransientError(fmt.Errorf("nftables: writeChain: parse ruleset: %w", err))
		}
		// ファイル非存在 → ヘッダコメント付きの新しいファイルを作る。
		doc = &rulesetDoc{lines: []string{"# Managed by Sakutei nftables provider — do not edit by hand.", ""}}
	}

	matches, err := doc.matching(newChain.family, newChain.table, newChain.chain)
	if err != nil {
		return err
	}

	lines := doc.lines
	if len(matches) > 0 {
		// 後ろから差し替えて、前のブロックの行番号をずらさない。
		for i := len(matches) - 1; i >= 0; i-- {
			lines = spliceLines(lines, matches[i].start, matches[i].end+1, rewriteChain(lines, matches[i], newChain.rules))
		}
		return p.writeLines(lines)
	}

	// 同じ table の最後のブロックに足す（閉じ括弧が単独行のものに限る）。
	for i := len(doc.tables) - 1; i >= 0; i-- {
		t := doc.tables[i]
		if t.family != newChain.family || t.table != newChain.table {
			continue
		}
		if t.end == t.start || strings.TrimSpace(lines[t.end]) != "}" {
			continue
		}
		indent := leadingSpace(lines[t.start]) + "\t"
		block := chainBlock(newChain, indent)
		return p.writeLines(spliceLines(lines, t.end, t.end, block))
	}

	// table が無い → 末尾に table ブロックを足す。
	block := []string{fmt.Sprintf("table %s %s {", newChain.family, newChain.table)}
	block = append(block, chainBlock(newChain, "\t")...)
	block = append(block, "}")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		// 末尾の改行の手前に足す。
		lines = spliceLines(lines, n-1, n-1, block)
	} else {
		lines = append(append(lines, block...), "")
	}
	return p.writeLines(lines)
}

// deleteChain removes a chain from the ruleset file。
// その chain のブロックの行だけを消し、他の行はそのまま残す。atomic write。
func (p *Provider) deleteChain(family, table, chain string) error {
	doc, err := p.parseDoc()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// ファイル非存在 → 削除は no-op（idempotent）。
			return nil
		}
		return provider.NewTransientError(fmt.Errorf("nftables: deleteChain: parse ruleset: %w", err))
	}

	matches, err := doc.matching(family, table, chain)
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		return nil // 既に無い（idempotent）。
	}
	lines := doc.lines
	for i := len(matches) - 1; i >= 0; i-- {
		lines = spliceLines(lines, matches[i].start, matches[i].end+1, nil)
	}
	return p.writeLines(lines)
}

// rulesetDoc は parse した ruleset ファイル。lines はファイルの各行（改行で
// 分割したそのまま。末尾が改行なら最後の要素は ""）で、Apply は管理する chain
// の行だけを差し替えて lines を書き戻す。
type rulesetDoc struct {
	lines  []string
	chains []chainRes
	tables []tableSpan
}

// tableSpan は 1 つの table ブロックの位置（start は table 行、end は閉じ括弧の行）。
type tableSpan struct {
	family, table string
	start, end    int
}

// matching は座標の一致する chain をファイル内の出現順に返す。一つでも base
// chain か管理サブセット外の構文を含む chain があれば、Apply を止める
// PermanentError を返す（黙って書き換えない）。
func (d *rulesetDoc) matching(family, table, chain string) ([]chainRes, error) {
	var out []chainRes
	for _, c := range d.chains {
		if !sameChain(c, family, table, chain) {
			continue
		}
		if c.base {
			return nil, provider.NewPermanentError(fmt.Errorf(
				"nftables: Apply: refusing to manage base chain %q (management boundary)", c.localID()))
		}
		if c.opaque {
			return nil, provider.NewPermanentError(fmt.Errorf(
				"nftables: Apply: refusing to rewrite chain %q (contains syntax outside the managed subset)", c.localID()))
		}
		out = append(out, c)
	}
	return out, nil
}

// parseRuleset reads and parses the ruleset file into chains。
// os.ErrNotExist を保持して返す（ファイル非存在時）。
func (p *Provider) parseRuleset() ([]chainRes, error) {
	doc, err := p.parseDoc()
	if err != nil {
		return nil, err
	}
	return doc.chains, nil
}

// parseDoc reads and parses the ruleset file, keeping every line verbatim.
//
// 括弧の深さを数えてブロックを追う。深さ 0 の `table` 行が table を、table
// 直下（深さ 1）の `chain` 行が chain を開く。table 直下のそれ以外のブロック
// （set/map 等）と table の外の行は parse せず、lines にそのまま残る。chain の
// 中で 1 行に収まらない構文（複数行の匿名 set 等）や 1 行の chain は opaque
// として印を付け、管理しない。括弧が閉じきらない/閉じすぎのファイルはエラー。
// os.ErrNotExist を保持して返す（ファイル非存在時）。
func (p *Provider) parseDoc() (*rulesetDoc, error) {
	data, err := os.ReadFile(p.rulesetPath)
	if err != nil {
		return nil, err // os.ErrNotExist は errors.Is で保たれる
	}

	doc := &rulesetDoc{lines: strings.Split(string(data), "\n")}
	depth := 0
	tableIdx, chainIdx := -1, -1
	var curFamily, curTable string

	for i, raw := range doc.lines {
		lineNo := i + 1
		if len(raw) > bufio.MaxScanTokenSize {
			return nil, fmt.Errorf("line %d: too long (%d bytes)", lineNo, len(raw))
		}
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		net := netBraces(line)
		next := depth + net
		if next < 0 {
			return nil, fmt.Errorf("line %d: unbalanced '}'", lineNo)
		}

		switch {
		case depth == 0 && strings.HasPrefix(line, "table "):
			// "table <family> <table> {"
			fields := strings.Fields(strings.TrimSuffix(line, "{"))
			if len(fields) < 3 {
				return nil, fmt.Errorf("line %d: malformed table header %q", lineNo, line)
			}
			curFamily = fields[1]
			curTable = fields[2]
			doc.tables = append(doc.tables, tableSpan{family: curFamily, table: curTable, start: i, end: i})
			tableIdx = len(doc.tables) - 1

		case strings.HasPrefix(line, "chain ") && (depth == 0 || tableIdx < 0):
			return nil, fmt.Errorf("line %d: chain outside table: %q", lineNo, line)

		case depth == 1 && tableIdx >= 0 && strings.HasPrefix(line, "chain "):
			// "chain <name> {"
			fields := strings.Fields(strings.TrimSuffix(line, "{"))
			if len(fields) < 2 {
				return nil, fmt.Errorf("line %d: malformed chain header %q", lineNo, line)
			}
			c := chainRes{family: curFamily, table: curTable, chain: fields[1], start: i, end: i}
			if net != 1 {
				// 1 行の chain（"chain x { ... }"）等は管理サブセット外。
				c.opaque = true
			} else if _, rest, ok := strings.Cut(line, "{"); ok {
				// "chain x { <rule>" のように '{' の後ろに書いた行も中身として読む。
				c.addLine(rest)
			}
			doc.chains = append(doc.chains, c)
			if next > depth {
				chainIdx = len(doc.chains) - 1
			}

		case chainIdx >= 0:
			cur := &doc.chains[chainIdx]
			switch {
			case next == 1 && depth == 2 && net == -1:
				// chain ブロックの終端。"<rule> }" のように '}' の前に書いた行も
				// 中身として読む。
				cur.addLine(line[:strings.LastIndex(line, "}")])
				cur.end = i
				chainIdx = -1
			case next == 1:
				// 複数行にわたる構文の終わりと chain の終端が同じ行にある。
				cur.opaque = true
				cur.end = i
				chainIdx = -1
			case depth != 2 || net != 0:
				// 複数行にわたる構文は管理サブセット外。
				cur.opaque = true
			default:
				cur.addLine(line)
			}
		}

		depth = next
		if depth == 0 && tableIdx >= 0 {
			doc.tables[tableIdx].end = i
			tableIdx = -1
		}
	}
	if depth != 0 {
		return nil, fmt.Errorf("unbalanced '{' at end of file")
	}
	return doc, nil
}

// addLine は chain ブロック内の 1 行（前後の空白は除いてよい）を読む。
func (c *chainRes) addLine(text string) {
	line := strings.TrimSpace(text)
	switch {
	case line == "" || strings.HasPrefix(line, "#"):
		// 空行・コメントはルールではない。
	case strings.HasPrefix(line, "type "):
		// base chain の宣言（"type ... hook ... priority ...; policy ...;"）。
		// hook を持つ chain は base chain として印を付け、管理対象外にする。
		// type 行自体はルールとして扱わない。
		if strings.Contains(line, "hook ") {
			c.base = true
		}
	case strings.HasPrefix(line, "policy "):
		// policy は base chain にしか書けない。
		c.base = true
	default:
		// chain ブロック内の行 = 1 ルール（順序保存）。
		c.rules = append(c.rules, line)
	}
}

// netBraces は 1 行の中の '{' の数から '}' の数を引いたものを返す。
// 引用符の中と '#' 以降のコメントは数えない。
func netBraces(line string) int {
	n := 0
	inQuote := false
	for _, r := range line {
		switch {
		case r == '"':
			inQuote = !inQuote
		case inQuote:
		case r == '#':
			return n
		case r == '{':
			n++
		case r == '}':
			n--
		}
	}
	return n
}

// leadingSpace は行頭の空白（インデント）を返す。
func leadingSpace(s string) string {
	return s[:len(s)-len(strings.TrimLeft(s, " \t"))]
}

// rewriteChain は既存の chain ブロック c（lines[c.start..c.end]）を rules に
// 書き直した行を返す。chain 行・閉じ括弧の行は、中身を含まなければ元のまま
// 残す。ルールのインデントは既存の最初のルール行に揃える。
func rewriteChain(lines []string, c chainRes, rules []string) []string {
	indent := leadingSpace(lines[c.start])
	header := lines[c.start]
	if !strings.HasSuffix(strings.TrimSpace(header), "{") {
		header = fmt.Sprintf("%schain %s {", indent, c.chain)
	}
	closing := lines[c.end]
	if strings.TrimSpace(closing) != "}" {
		closing = indent + "}"
	}
	ruleIndent := indent + "\t"
	for _, l := range lines[c.start+1 : c.end] {
		if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "#") {
			ruleIndent = leadingSpace(l)
			break
		}
	}
	out := []string{header}
	out = append(out, ruleLines(rules, ruleIndent)...)
	return append(out, closing)
}

// ruleLines はルールを indent 付きの行にする。
func ruleLines(rules []string, indent string) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, indent+r)
	}
	return out
}

// chainBlock は chain ブロック全体（chain 行・ルール・閉じ括弧）の行を返す。
func chainBlock(c chainRes, indent string) []string {
	out := []string{fmt.Sprintf("%schain %s {", indent, c.chain)}
	out = append(out, ruleLines(c.rules, indent+"\t")...)
	return append(out, indent+"}")
}

// spliceLines は lines[from:to] を repl で置き換えた新しいスライスを返す。
func spliceLines(lines []string, from, to int, repl []string) []string {
	out := make([]string, 0, len(lines)-(to-from)+len(repl))
	out = append(out, lines[:from]...)
	out = append(out, repl...)
	return append(out, lines[to:]...)
}

// writeLines joins lines with "\n" and writes the ruleset file atomically.
func (p *Provider) writeLines(lines []string) error {
	return p.writeFile(strings.Join(lines, "\n"))
}

// writeFile writes content to the ruleset file atomically（temp + rename）。
func (p *Provider) writeFile(content string) error {
	beforeWrite()
	dir := filepath.Dir(p.rulesetPath)
	tmp, err := os.CreateTemp(dir, "sakutei-nftables-*.tmp")
	if err != nil {
		return provider.NewTransientError(fmt.Errorf("nftables: writeRuleset: create temp: %w", err))
	}
	tmpPath := tmp.Name()

	if _, werr := tmp.WriteString(content); werr != nil {
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
