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

// live.go — カーネルの実際の ruleset を観測・適用する live 版（Backlog #121）。
//
// 設計判断:
//
//   - 宣言する現実はカーネルの ruleset。file 版と同じ Schema・同じ identity
//     {family, table, chain}・同じ local id・同じ Ordered の rules を使うので、
//     intent は対象（--ruleset-target file|live）を変えてもそのまま使える。
//   - 管理するのは設定で名指しした table（TableRef、必須）の regular chain
//     だけ。base chain（type/hook を持つ）・set・map・名指ししていない table は
//     読みもせず書きもしない（§7、Backlog #68 と同じ管理境界）。
//   - 観測は table ごとに nft を 2 回: `nft -j list table <family> <name>` の
//     JSON で構造（chain の並び・base かどうか・ルールの handle の並び）を、
//     `nft -a -s list table <family> <name>` のテキストで handle ごとのルール文を
//     読む。2 つは handle で突き合わせ、合わなければ（読む間に変わった）その周は
//     観測できないとする。table が無いのは「空」、nft が走らない・権限が無い・
//     出力が読めないのは「観測できない」（unknown。absent にしない、§3・§5.4）。
//   - 正規形（canonRule）: nft が書き戻すテキスト（-s でカウンタと quota の
//     used を落とした形）の空白を 1 つにそろえたもの。宣言も同じ関数を通して
//     比べる（PlanRefiner）。nft は宣言を自分の形に書き換えることがある（例
//     `ct state new,established` → `established,new`）ので、Apply は流した後に
//     読み戻し、正規形が宣言と違えば「nft の形で宣言せよ」という PermanentError
//     を返す（永久に drift が出続ける宣言を黙って受けない）。
//   - Apply は対象の chain の中身を入れ替えるスクリプト（flush chain + add
//     rule の列。Create は add table・add chain、Delete は flush + delete chain）
//     を作り、`nft -c -f -` で検査してから `nft -f -` で 1 つのトランザクション
//     として流す（途中で失敗すれば何も変わらない）。同じホストの Apply は
//     プロセス内で直列化する（liveApplyMu）。
//   - ルール文は nft のスクリプトにそのまま埋め込むので、引用符の外の ';'・
//     '#'・改行（コマンドの区切りとコメント）は受けない（管理しない table を
//     書き換えるコマンドを注入させない）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// liveProviderVersion は live 版の正規化のソースハッシュ。canonRule や読み方を
// 変えたら増分すること（cause-tag C（ghost diff）検出のため）。
const liveProviderVersion = "nftables-live-v1.0.0"

// 対象（config の ruleset_target、CLI の --ruleset-target）。
const (
	// TargetFile は管理された ruleset ファイル（New、既定）。
	TargetFile = "file"
	// TargetLive はカーネルの実際の ruleset（NewLive）。
	TargetLive = "live"
)

// liveApplyMu は同じホスト（= このプロセス）の live の Apply を直列化する。
var liveApplyMu sync.Mutex

// families は nft の table family。
var families = map[string]bool{"ip": true, "ip6": true, "inet": true, "arp": true, "bridge": true, "netdev": true}

// identRe は管理する table・chain の名前として受ける識別子。
var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)

// TableRef は live が管理する 1 つの table（family と名前）。
type TableRef struct {
	Family string
	Name   string
}

// String は "<family>/<name>" を返す。
func (t TableRef) String() string { return t.Family + "/" + t.Name }

// ParseTables は "<family>/<name>,<family>/<name>,..."（空白は無視）を読む。
// 1 つも無い・形が違う・同じ table が 2 回は error。
func ParseTables(s string) ([]TableRef, error) {
	var out []TableRef
	seen := map[TableRef]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		family, name, ok := strings.Cut(part, "/")
		t := TableRef{Family: family, Name: name}
		if !ok || !families[family] || !identRe.MatchString(name) {
			return nil, fmt.Errorf("table %q: want <family>/<name> (family: ip, ip6, inet, arp, bridge, netdev)", part)
		}
		if seen[t] {
			return nil, fmt.Errorf("table %q is listed twice", part)
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil, errors.New("no table to manage: list them as <family>/<name>,...")
	}
	return out, nil
}

// LiveConfig は LiveProvider の設定。
type LiveConfig struct {
	// NFT は nft のパス（既定 "nft"）。Runner が nil のときに使う。
	NFT string
	// Runner は nft を走らせる seam。nil なら ExecRunner{Path: NFT}。
	Runner Runner
	// Tables は管理する table（必須）。
	Tables []TableRef
}

// LiveProvider manages the regular chains of the named tables in the kernel's
// nftables ruleset.  provider.Provider・Lister・BulkObserver・IntentValidator・
// PlanRefiner を実装する。
type LiveProvider struct {
	runner Runner
	tables []TableRef
	scope  core.Scope
}

// NewLive は live の Provider を作る。Tables が空なら error。
func NewLive(cfg LiveConfig, opts ...Option) (*LiveProvider, error) {
	if len(cfg.Tables) == 0 {
		return nil, errors.New("nftables: live: no table to manage")
	}
	var s settings
	for _, opt := range opts {
		opt(&s)
	}
	r := cfg.Runner
	if r == nil {
		r = ExecRunner{Path: cfg.NFT}
	}
	tables := append([]TableRef(nil), cfg.Tables...)
	return &LiveProvider{runner: r, tables: tables, scope: s.scope}, nil
}

// Schema は file 版と同じ（intent を対象によらず使えるように）。
func (p *LiveProvider) Schema() provider.Schema { return new(Provider).Schema() }

// Capabilities: TrueObserve（カーネルが真実源）・List・BulkObserve。
// Pruneable は false（file 版と同じく破壊的型、ADR-0005）。
func (p *LiveProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		TrueObserve:     true,
		List:            true,
		Pruneable:       false,
		BulkObserve:     true,
		ProviderVersion: liveProviderVersion,
	}
}

// Compile-time assertions.
var (
	_ provider.Provider        = (*LiveProvider)(nil)
	_ provider.Lister          = (*LiveProvider)(nil)
	_ provider.BulkObserver    = (*LiveProvider)(nil)
	_ provider.IntentValidator = (*LiveProvider)(nil)
	_ provider.PlanRefiner     = (*LiveProvider)(nil)
)

// Read observes one chain.  table が無い・chain が無いのは absent、nft が
// 走らない・出力が読めないのは unknown（TransientError）、base chain・
// 管理しない table・読み分けられないルールを持つ chain は PermanentError。
func (p *LiveProvider) Read(ctx context.Context, localID string) (core.Observation, error) {
	ref, chain, err := p.coords(localID)
	if err != nil {
		return unknownObs(), provider.NewPermanentError(fmt.Errorf("nftables: Read: %w", err))
	}
	tbl, err := p.observeTable(ctx, ref)
	if err != nil {
		return unknownObs(), provider.NewTransientError(fmt.Errorf("nftables: Read: %w", err))
	}
	c := tbl.find(chain)
	if c == nil {
		return core.Observation{Availability: core.AvailabilityOk, ObservedAt: time.Now()}, nil
	}
	if err := c.refuse(ref); err != nil {
		return unknownObs(), provider.NewPermanentError(fmt.Errorf("nftables: Read: %w", err))
	}
	return core.Observation{Availability: core.AvailabilityOk, State: c.state(ref), ObservedAt: time.Now()}, nil
}

// List は管理する table の regular chain をすべて返す。全 table を読めたとき
// だけ ListComplete で、1 つでも読めなければ error（complete-or-error、§5.4）。
func (p *LiveProvider) List(ctx context.Context, _ core.Scope) (core.ListResult, error) {
	var ids []core.ID
	for _, ref := range p.tables {
		tbl, err := p.observeTable(ctx, ref)
		if err != nil {
			return core.ListResult{}, provider.NewTransientError(fmt.Errorf("nftables: List: %w", err))
		}
		for _, c := range tbl.chains {
			if c.refuse(ref) == nil {
				ids = append(ids, core.NewID(ProviderName, p.scope, chainLocalID(ref, c.name)))
			}
		}
	}
	return core.ListResult{IDs: ids, Status: core.ListComplete}, nil
}

// BulkObserve は管理する table ごとに nft を 2 回だけ呼んで全 chain を観測する
// （§5.7）。各エントリは Read が返すものと同じ。complete-or-error。
func (p *LiveProvider) BulkObserve(ctx context.Context, _ core.Scope) (provider.BulkObservation, error) {
	obs := map[core.IDKey]core.Observation{}
	now := time.Now()
	for _, ref := range p.tables {
		tbl, err := p.observeTable(ctx, ref)
		if err != nil {
			return provider.BulkObservation{}, provider.NewTransientError(fmt.Errorf("nftables: BulkObserve: %w", err))
		}
		for _, c := range tbl.chains {
			if c.refuse(ref) != nil {
				continue
			}
			obs[core.NewID(ProviderName, p.scope, chainLocalID(ref, c.name)).Key()] = core.Observation{
				Availability: core.AvailabilityOk,
				State:        c.state(ref),
				ObservedAt:   now,
			}
		}
	}
	return provider.BulkObservation{Observations: obs}, nil
}

// ValidateIntent は読み込みの時点で、管理しない table の chain・識別子として
// 受けない名前・nft のスクリプトに埋め込めないルール文を断る（§28.4）。
func (p *LiveProvider) ValidateIntent(localID string, fields map[string]string) error {
	ref, chain, err := p.coords(localID)
	if err != nil {
		return err
	}
	want := map[string]string{"family": ref.Family, "table": ref.Name, "chain": chain}
	for k, v := range want {
		if got, ok := fields[k]; ok && got != v {
			return fmt.Errorf("field %s is %q but the id says %q", k, got, v)
		}
	}
	for i, r := range splitRules(fields["rules"]) {
		if err := checkRuleText(r); err != nil {
			return fmt.Errorf("rules[%d]: %w", i, err)
		}
	}
	return nil
}

// RefinePlan は rules の差分のうち、正規形にすると観測と同じもの（空白や
// カウンタの値だけの違い）を落とす。I/O はしない。
func (p *LiveProvider) RefinePlan(_ string, _ core.State, _ map[string]string, ops []core.Op) ([]core.Op, error) {
	out := make([]core.Op, 0, len(ops))
	for _, op := range ops {
		if op.Kind == core.OpKindUpdate {
			var fields []core.FieldDiff
			for _, f := range op.Fields {
				if f.Field == "rules" && canonRules(f.To) == f.From {
					continue
				}
				fields = append(fields, f)
			}
			op.Fields = fields
		}
		out = append(out, op)
	}
	return out, nil
}

// chainCheck は Apply の後に読み戻して確かめる 1 つの chain。
type chainCheck struct {
	ref     TableRef
	chain   string
	rules   []string // 宣言のルール（正規形）
	deleted bool
}

// Apply は plan の op を 1 つの nft スクリプトにし、`nft -c -f -` で検査して
// から `nft -f -` で 1 つのトランザクションとして流す。流した後に読み戻し、
// 宣言の正規形と違えば PermanentError。
func (p *LiveProvider) Apply(ctx context.Context, plan core.Plan) error {
	liveApplyMu.Lock()
	defer liveApplyMu.Unlock()

	tables := map[TableRef]*liveTable{}
	var script strings.Builder
	var checks []chainCheck
	for _, op := range plan.Ops {
		ref, chain, err := p.coords(op.Target.Local)
		if err != nil {
			return provider.NewPermanentError(fmt.Errorf("nftables: Apply: %w", err))
		}
		tbl, ok := tables[ref]
		if !ok {
			if tbl, err = p.observeTable(ctx, ref); err != nil {
				return provider.NewTransientError(fmt.Errorf("nftables: Apply: %w", err))
			}
			tables[ref] = tbl
		}
		cur := tbl.find(chain)
		if cur != nil {
			if err := cur.refuse(ref); err != nil {
				return provider.NewPermanentError(fmt.Errorf("nftables: Apply: %w", err))
			}
		}
		target := ref.Family + " " + ref.Name + " " + chain

		switch op.Kind {
		case core.OpKindCreate, core.OpKindUpdate:
			var enc string
			has := false
			for _, f := range op.Fields {
				if f.Field == "rules" {
					enc, has = f.To, true
				}
			}
			if op.Kind == core.OpKindUpdate && !has && cur != nil {
				continue // rules は変わらない
			}
			rules := splitRules(enc)
			for i, r := range rules {
				if err := checkRuleText(r); err != nil {
					return provider.NewPermanentError(fmt.Errorf("nftables: Apply: %s rules[%d]: %w", op.Target.Local, i, err))
				}
			}
			if cur == nil {
				if !tbl.exists {
					fmt.Fprintf(&script, "add table %s %s\n", ref.Family, ref.Name)
				}
				fmt.Fprintf(&script, "add chain %s\n", target)
			}
			fmt.Fprintf(&script, "flush chain %s\n", target)
			canon := make([]string, 0, len(rules))
			for _, r := range rules {
				fmt.Fprintf(&script, "add rule %s %s\n", target, r)
				canon = append(canon, canonRule(r))
			}
			checks = append(checks, chainCheck{ref: ref, chain: chain, rules: canon})

		case core.OpKindDelete:
			if cur == nil {
				continue // 既に無い（idempotent）
			}
			fmt.Fprintf(&script, "flush chain %s\ndelete chain %s\n", target, target)
			checks = append(checks, chainCheck{ref: ref, chain: chain, deleted: true})

		default:
			return provider.NewPermanentError(fmt.Errorf("nftables: Apply: unknown op kind %v for %s", op.Kind, op.Target.Local))
		}
	}
	if script.Len() == 0 {
		return nil
	}

	body := []byte(script.String())
	if _, err := p.runner.Run(ctx, []string{"-c", "-f", "-"}, body); err != nil {
		return classifyApplyErr("nft -c (check)", err, true)
	}
	if _, err := p.runner.Run(ctx, []string{"-f", "-"}, body); err != nil {
		return classifyApplyErr("nft -f", err, false)
	}
	return p.verify(ctx, checks)
}

// verify は Apply の後に読み戻し、各 chain が宣言どおり（正規形で）かを確かめる。
func (p *LiveProvider) verify(ctx context.Context, checks []chainCheck) error {
	tables := map[TableRef]*liveTable{}
	for _, ck := range checks {
		tbl, ok := tables[ck.ref]
		if !ok {
			var err error
			if tbl, err = p.observeTable(ctx, ck.ref); err != nil {
				return provider.NewTransientError(fmt.Errorf("nftables: Apply: read back: %w", err))
			}
			tables[ck.ref] = tbl
		}
		id := chainLocalID(ck.ref, ck.chain)
		c := tbl.find(ck.chain)
		if ck.deleted {
			if c != nil {
				return provider.NewTransientError(fmt.Errorf("nftables: Apply: %s is still there after delete", id))
			}
			continue
		}
		if c == nil {
			return provider.NewTransientError(fmt.Errorf("nftables: Apply: %s is not there after apply", id))
		}
		for i := 0; i < len(ck.rules) || i < len(c.rules); i++ {
			var want, got string
			if i < len(ck.rules) {
				want = ck.rules[i]
			}
			if i < len(c.rules) {
				got = c.rules[i]
			}
			if want != got {
				return provider.NewPermanentError(fmt.Errorf(
					"nftables: Apply: nft lists rule %d of %s as %q, not as declared %q: declare it in the form nft lists it (it would show as drift every cycle otherwise)",
					i, id, got, want))
			}
		}
	}
	return nil
}

// classifyApplyErr は nft の失敗を分ける。走らせられない・権限が無い・
// 一時的に使えない（EBUSY 等）は TransientError、検査（-c）で断られたのは
// この現実にこの宣言を流せないので PermanentError。
func classifyApplyErr(stage string, err error, check bool) error {
	err = fmt.Errorf("nftables: Apply: %s: %w", stage, err)
	var ee *ExitError
	if !errors.As(err, &ee) || !check {
		return provider.NewTransientError(err)
	}
	for _, s := range []string{"Operation not permitted", "Permission denied", "Device or resource busy", "Resource temporarily unavailable"} {
		if strings.Contains(ee.Stderr, s) {
			return provider.NewTransientError(err)
		}
	}
	return provider.NewPermanentError(err)
}

// ─────────────────────────────────────────────────────────────────────────────
// 観測
// ─────────────────────────────────────────────────────────────────────────────

// liveTable は 1 つの table を読んだ結果。
type liveTable struct {
	exists bool
	chains []liveChain
}

// liveChain は 1 つの chain（ルールは正規形）。
type liveChain struct {
	name   string
	handle int
	base   bool
	// opaque は rules を 1 本の文字列に符号化できない（RuleSep を含む）ルールを
	// 持つかどうか。
	opaque bool
	rules  []string
}

func (t *liveTable) find(chain string) *liveChain {
	for i := range t.chains {
		if t.chains[i].name == chain {
			return &t.chains[i]
		}
	}
	return nil
}

// refuse は chain が管理境界の外なら理由を返す。
func (c *liveChain) refuse(ref TableRef) error {
	id := chainLocalID(ref, c.name)
	switch {
	case c.base:
		return fmt.Errorf("refusing to manage base chain %q (has type/hook/policy; management boundary)", id)
	case c.opaque:
		return fmt.Errorf("refusing to manage chain %q (a rule contains %q)", id, RuleSep)
	case !identRe.MatchString(c.name):
		return fmt.Errorf("refusing to manage chain %q (name is not a plain identifier)", id)
	}
	return nil
}

func (c *liveChain) state(ref TableRef) core.State {
	return core.State{
		"family": {Kind: core.FieldKindString, Value: ref.Family},
		"table":  {Kind: core.FieldKindString, Value: ref.Name},
		"chain":  {Kind: core.FieldKindString, Value: c.name},
		"rules":  {Kind: core.FieldKindStringList, Value: strings.Join(c.rules, RuleSep)},
	}
}

// nftDoc は `nft -j list table` の出力。
type nftDoc struct {
	Nftables []map[string]json.RawMessage `json:"nftables"`
}

type nftChain struct {
	Family string `json:"family"`
	Table  string `json:"table"`
	Name   string `json:"name"`
	Handle int    `json:"handle"`
	Type   string `json:"type"`
	Hook   string `json:"hook"`
	Policy string `json:"policy"`
}

type nftRule struct {
	Family string `json:"family"`
	Table  string `json:"table"`
	Chain  string `json:"chain"`
	Handle int    `json:"handle"`
}

// observeTable は 1 つの table を読む。table が無ければ exists=false で error
// なし。それ以外の失敗（nft が走らない・権限が無い・出力が読めない・2 回の
// 読みが合わない）は error。
func (p *LiveProvider) observeTable(ctx context.Context, ref TableRef) (*liveTable, error) {
	out, err := p.runner.Run(ctx, []string{"-j", "list", "table", ref.Family, ref.Name}, nil)
	if err != nil {
		if noSuchTable(err) {
			return &liveTable{}, nil
		}
		return nil, fmt.Errorf("list table %s: %w", ref, err)
	}
	tbl, rules, err := parseTableJSON(out, ref)
	if err != nil {
		return nil, fmt.Errorf("list table %s: %w", ref, err)
	}
	txt, err := p.runner.Run(ctx, []string{"-a", "-s", "list", "table", ref.Family, ref.Name}, nil)
	if err != nil {
		return nil, fmt.Errorf("list table %s (text): %w", ref, err)
	}
	texts, err := parseTableText(txt)
	if err != nil {
		return nil, fmt.Errorf("list table %s (text): %w", ref, err)
	}
	if len(texts) != len(rules) {
		return nil, fmt.Errorf("list table %s: the ruleset changed while reading (%d rules in JSON, %d in text)", ref, len(rules), len(texts))
	}
	for _, r := range rules {
		c := tbl.find(r.Chain)
		t, ok := texts[r.Handle]
		if c == nil || !ok || t.chain != c.handle {
			return nil, fmt.Errorf("list table %s: the ruleset changed while reading (rule handle %d)", ref, r.Handle)
		}
		rule := canonRule(t.text)
		if strings.Contains(rule, RuleSep) {
			c.opaque = true
		}
		c.rules = append(c.rules, rule)
	}
	return tbl, nil
}

// noSuchTable は nft が「table が無い」で終わったかどうか。
func noSuchTable(err error) bool {
	var ee *ExitError
	return errors.As(err, &ee) && ee.Code == 1 && strings.Contains(ee.Stderr, "No such file or directory")
}

// parseTableJSON は `nft -j list table` の JSON から chain（並び順）とルール
// （並び順）を読む。ref 以外の table のものは無視する。
func parseTableJSON(out []byte, ref TableRef) (*liveTable, []nftRule, error) {
	var doc nftDoc
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, nil, fmt.Errorf("parse JSON: %w", err)
	}
	tbl := &liveTable{}
	var rules []nftRule
	for _, obj := range doc.Nftables {
		for kind, raw := range obj {
			switch kind {
			case "table":
				var t struct {
					Family string `json:"family"`
					Name   string `json:"name"`
				}
				if err := json.Unmarshal(raw, &t); err != nil {
					return nil, nil, fmt.Errorf("parse JSON table: %w", err)
				}
				if t.Family == ref.Family && t.Name == ref.Name {
					tbl.exists = true
				}
			case "chain":
				var c nftChain
				if err := json.Unmarshal(raw, &c); err != nil {
					return nil, nil, fmt.Errorf("parse JSON chain: %w", err)
				}
				if c.Family != ref.Family || c.Table != ref.Name {
					continue
				}
				if tbl.find(c.Name) != nil {
					return nil, nil, fmt.Errorf("chain %q is listed twice", c.Name)
				}
				tbl.chains = append(tbl.chains, liveChain{
					name:   c.Name,
					handle: c.Handle,
					base:   c.Type != "" || c.Hook != "" || c.Policy != "",
				})
			case "rule":
				var r nftRule
				if err := json.Unmarshal(raw, &r); err != nil {
					return nil, nil, fmt.Errorf("parse JSON rule: %w", err)
				}
				if r.Family != ref.Family || r.Table != ref.Name {
					continue
				}
				rules = append(rules, r)
			}
		}
	}
	if !tbl.exists {
		return nil, nil, fmt.Errorf("the output does not have table %s", ref)
	}
	return tbl, rules, nil
}

// textRule は `nft -a list table` の 1 ルール（属する chain の handle と本文）。
type textRule struct {
	chain int
	text  string
}

// parseTableText は `nft -a -s list table` のテキストから、handle ごとの
// ルール文を読む。chain のブロックの中で末尾が `# handle <n>` の行が 1 ルール
// （nft はルールを必ず 1 行で書く）。末尾が `{ # handle <n>` の行はブロックの
// 頭（chain なら以降がその chain の中）、`}` だけの行はブロックの終わり。
func parseTableText(out []byte) (map[int]textRule, error) {
	rules := map[int]textRule{}
	cur := -1
	for _, raw := range strings.Split(string(out), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if line == "}" {
			cur = -1
			continue
		}
		h, body, ok := cutHandle(line)
		if !ok {
			continue // base chain の type 行・set の中身など
		}
		if strings.HasSuffix(body, "{") {
			cur = -1
			if strings.HasPrefix(body, "chain ") {
				cur = h
			}
			continue
		}
		if cur < 0 {
			continue
		}
		if _, dup := rules[h]; dup {
			return nil, fmt.Errorf("rule handle %d is listed twice", h)
		}
		rules[h] = textRule{chain: cur, text: body}
	}
	return rules, nil
}

// cutHandle は行末の " # handle <n>" を切り分ける。
func cutHandle(line string) (int, string, bool) {
	i := strings.LastIndex(line, "# handle ")
	if i < 0 {
		return 0, "", false
	}
	h, err := strconv.Atoi(line[i+len("# handle "):])
	if err != nil {
		return 0, "", false
	}
	return h, strings.TrimSpace(line[:i]), true
}

// ─────────────────────────────────────────────────────────────────────────────
// 正規形とルール文の検査
// ─────────────────────────────────────────────────────────────────────────────

// canonRule はルール文の正規形を返す: 引用符の外の空白を 1 つにそろえ、
// `counter packets <n> bytes <n>` を `counter` に、quota の `used <n> <unit>` を
// 落とす（変わり続ける状態で、宣言の一部ではない）。観測（nft -s の出力）と
// 宣言の両方に同じものを使う。
func canonRule(s string) string {
	toks := ruleTokens(s)
	out := make([]string, 0, len(toks))
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t == "counter" && i+4 < len(toks) && toks[i+1] == "packets" && isDigits(toks[i+2]) && toks[i+3] == "bytes" && isDigits(toks[i+4]) {
			out = append(out, "counter")
			i += 4
			continue
		}
		if t == "used" && i+2 < len(toks) && isDigits(toks[i+1]) && afterQuota(out) {
			i += 2
			continue
		}
		out = append(out, t)
	}
	return strings.Join(out, " ")
}

// canonRules は RuleSep で join したルール列の各ルールを正規形にする。
func canonRules(enc string) string {
	rules := splitRules(enc)
	for i, r := range rules {
		rules[i] = canonRule(r)
	}
	return strings.Join(rules, RuleSep)
}

// afterQuota は直前の数語に quota があるかどうか（"quota [over|until] <n> <unit>"）。
func afterQuota(out []string) bool {
	for i := len(out) - 1; i >= 0 && i >= len(out)-4; i-- {
		if out[i] == "quota" {
			return true
		}
	}
	return false
}

// ruleTokens は引用符の外の空白でルール文を区切る（引用符の中はそのまま）。
func ruleTokens(s string) []string {
	var toks []string
	var b strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			b.WriteRune(r)
		case !inQuote && (r == ' ' || r == '\t' || r == '\n' || r == '\r'):
			if b.Len() > 0 {
				toks = append(toks, b.String())
				b.Reset()
			}
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		toks = append(toks, b.String())
	}
	return toks
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// checkRuleText は nft のスクリプトに 1 行として埋め込めるルール文かを見る。
// 引用符の外の ';'・'#'（コマンドの区切り・コメント）と改行、閉じない引用符は
// 受けない。エラーにルール文そのものは書かない。
func checkRuleText(r string) error {
	inQuote := false
	for _, c := range r {
		switch {
		case c == '\n' || c == '\r':
			return errors.New("a rule must be one line")
		case c == '"':
			inQuote = !inQuote
		case inQuote:
		case c == ';':
			return errors.New("a rule must not contain ';' outside quotes (it would end the nft command)")
		case c == '#':
			return errors.New("a rule must not contain '#' outside quotes (nft would read the rest as a comment)")
		}
	}
	if inQuote {
		return errors.New("a rule has an unclosed quote")
	}
	return nil
}

// coords は local id を管理する table と chain に分ける。形が違う・管理しない
// table・識別子として受けない名前は error。
func (p *LiveProvider) coords(localID string) (TableRef, string, error) {
	family, table, chain, err := splitLocalID(localID)
	if err != nil {
		return TableRef{}, "", fmt.Errorf("invalid local id %q: %w", localID, err)
	}
	ref := TableRef{Family: family, Name: table}
	managed := false
	for _, t := range p.tables {
		if t == ref {
			managed = true
		}
	}
	if !managed {
		return TableRef{}, "", fmt.Errorf("refusing to manage %q: table %s is not in the managed tables (management boundary)", localID, ref)
	}
	if !identRe.MatchString(chain) {
		return TableRef{}, "", fmt.Errorf("invalid local id %q: chain name must match %s", localID, identRe)
	}
	return ref, chain, nil
}

func chainLocalID(ref TableRef, chain string) string {
	return ref.Family + "/" + ref.Name + "/" + chain
}

func unknownObs() core.Observation {
	return core.Observation{Availability: core.AvailabilityUnknown, ObservedAt: time.Now()}
}
