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
//     と、intent が base chain として宣言した chain（Backlog #173、basechain.go）
//     だけ。宣言していない base chain（type/hook を持つ）・set・map・名指し
//     していない table は読みもせず書きもしない（§7、Backlog #68 と同じ管理
//     境界）。List・BulkObserve は base chain を出さない（宣言したものは Read が
//     観測する）。
//   - base chain を変える Apply は締め出しの守り（guard.go）を通す: 写しを取り、
//     ホストの側に時間切れの巻き戻しを仕掛けてから流し、新しい接続で到達を
//     確かめてから仕掛けを取り消す。確かめられなければ巻き戻しに任せて
//     provider.RolledBackError を返す。
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
//   - 離れたホスト（Backlog #130）: LiveConfig.Hosts に名前付きの Runner（ssh
//     越しの実行の層、internal/remote）を渡すと、1 つの Provider が複数の
//     ホストの ruleset を扱う。そのときの local id は "<ホスト>/<family>/<table>/
//     <chain>"（2 台に同じ table・chain があってもぶつからない）。Hosts が空
//     なら今までどおり手元の nft で、id も "<family>/<table>/<chain>" のまま。
//     ホストに届かない・認証の失敗は unknown（TransientError）、ホスト鍵の
//     不一致は PermanentError。BulkObserve は観測できないホストの分を返さない
//     （その chain は Read が unknown と答える）。List は全ホストを読めたとき
//     だけ complete。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/remote"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// liveProviderVersion は live 版の正規化のソースハッシュ。canonRule や読み方を
// 変えたら増分すること（cause-tag C（ghost diff）検出のため）。
const liveProviderVersion = "nftables-live-v1.1.0" // v1.1.0: base chain の宣言（Backlog #173）

// 対象（config の ruleset_target、CLI の --ruleset-target）。
const (
	// TargetFile は管理された ruleset ファイル（New、既定）。
	TargetFile = "file"
	// TargetLive はカーネルの実際の ruleset（NewLive）。
	TargetLive = "live"
)

// liveApplyMu は同じホストの live の Apply をプロセスの中で直列化する
// （ホストの名前ごと。手元は ""）。
var liveApplyMu sync.Map // string → *sync.Mutex

func liveApplyLock(host string) *sync.Mutex {
	mu, _ := liveApplyMu.LoadOrStore(host, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

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
	// Hosts は扱う離れたホスト（Backlog #130）。空なら手元（Runner / NFT）。
	// 空でなければ Runner と NFT は使わず、id の先頭にホストの名前が付く。
	Hosts []LiveHost
	// Guard は手元の締め出しの守り（Backlog #173）。nil なら手元の base chain は
	// 変えない（Apply は PermanentError。確かめの宛先を設定していない）。
	Guard Guard
	// RollbackAfter は base chain を変えた後、確かめられなければ写しに戻すまでの
	// 時間（0 は DefaultRollbackAfter）。確かめはその半分まで。
	RollbackAfter time.Duration
}

// LiveHost は live が扱う 1 つのホスト。
type LiveHost struct {
	// Name はホストの名前（id の先頭。"/" を含まない）。
	Name string
	// Runner はそのホストで nft を走らせる（例 RemoteRunner）。
	Runner Runner
	// Guard はそのホストの締め出しの守り（Backlog #173）。nil ならそのホストの
	// base chain は変えない。
	Guard Guard
}

// liveHost は Provider の中の 1 つのホスト（手元なら name ""）。
type liveHost struct {
	name   string
	runner Runner
	guard  Guard
}

// LiveProvider manages the regular chains of the named tables in the kernel's
// nftables ruleset.  provider.Provider・Lister・BulkObserver・IntentValidator・
// PlanRefiner を実装する。
type LiveProvider struct {
	hosts []liveHost
	// multi は id がホストの名前を含むかどうか（Hosts を設定したとき）。
	multi  bool
	tables []TableRef
	scope  core.Scope
	// rollbackAfter は時間切れの巻き戻しの上限（Backlog #173）。
	rollbackAfter time.Duration
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
	tables := append([]TableRef(nil), cfg.Tables...)
	p := &LiveProvider{tables: tables, scope: s.scope, rollbackAfter: cfg.RollbackAfter}
	if p.rollbackAfter <= 0 {
		p.rollbackAfter = DefaultRollbackAfter
	}
	if len(cfg.Hosts) == 0 {
		r := cfg.Runner
		if r == nil {
			r = ExecRunner{Path: cfg.NFT}
		}
		p.hosts = []liveHost{{runner: r, guard: cfg.Guard}}
		return p, nil
	}
	p.multi = true
	seen := map[string]bool{}
	for _, h := range cfg.Hosts {
		if h.Name == "" || strings.ContainsAny(h.Name, "/ \t\n") || h.Runner == nil {
			return nil, fmt.Errorf("nftables: live: host %q: want a name without '/' and a runner", h.Name)
		}
		if seen[h.Name] {
			return nil, fmt.Errorf("nftables: live: host %q is listed twice", h.Name)
		}
		seen[h.Name] = true
		p.hosts = append(p.hosts, liveHost{name: h.Name, runner: h.Runner, guard: h.Guard})
	}
	return p, nil
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
// 走らない・出力が読めないのは unknown（TransientError）、管理しない table・
// 読み分けられないルールを持つ chain は PermanentError。base chain は type・
// hook・priority・policy 付きで返す（Read は宣言したものにだけ呼ばれる。宣言が
// regular chain なら PlanRefiner が断る、Backlog #173）。
func (p *LiveProvider) Read(ctx context.Context, localID string) (core.Observation, error) {
	h, ref, chain, err := p.coords(localID)
	if err != nil {
		return unknownObs(), provider.NewPermanentError(fmt.Errorf("nftables: Read: %w", err))
	}
	tbl, err := h.observeTable(ctx, ref)
	if err != nil {
		return unknownObs(), observeErr(fmt.Errorf("nftables: Read: %w", err))
	}
	c := tbl.find(chain)
	if c == nil {
		return core.Observation{Availability: core.AvailabilityOk, ObservedAt: time.Now()}, nil
	}
	if err := c.refuse(ref, h.name); err != nil {
		return unknownObs(), provider.NewPermanentError(fmt.Errorf("nftables: Read: %w", err))
	}
	return core.Observation{Availability: core.AvailabilityOk, State: c.state(ref), ObservedAt: time.Now()}, nil
}

// List は管理する table の regular chain をすべて返す（base chain は出さない。
// 宣言していない base chain を status の宣言外に出さない、Backlog #173）。全 table を読めたとき
// だけ ListComplete で、1 つでも読めなければ error（complete-or-error、§5.4）。
func (p *LiveProvider) List(ctx context.Context, _ core.Scope) (core.ListResult, error) {
	var ids []core.ID
	for _, h := range p.hosts {
		for _, ref := range p.tables {
			tbl, err := h.observeTable(ctx, ref)
			if err != nil {
				return core.ListResult{}, observeErr(fmt.Errorf("nftables: List: %w", err))
			}
			for _, c := range tbl.chains {
				if !c.base && c.refuse(ref, h.name) == nil {
					ids = append(ids, core.NewID(ProviderName, p.scope, hostLocalID(h.name, ref, c.name)))
				}
			}
		}
	}
	return core.ListResult{IDs: ids, Status: core.ListComplete}, nil
}

// BulkObserve は管理する table ごとに nft を 2 回だけ呼んで全 chain を観測する
// （§5.7）。各エントリは Read が返すものと同じ。ホストごとに complete-or-error:
// ホストが 1 つだけ（手元を含む）ならその失敗は全体の error、複数なら読めない
// ホストの分は返さない（その chain は Read が決める＝unknown）。全部読めなければ
// error。
func (p *LiveProvider) BulkObserve(ctx context.Context, _ core.Scope) (provider.BulkObservation, error) {
	obs := map[core.IDKey]core.Observation{}
	now := time.Now()
	var firstErr error
	failed := 0
	for _, h := range p.hosts {
		got, err := h.bulk(ctx, p, now)
		if err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for k, o := range got {
			obs[k] = o
		}
	}
	if failed == len(p.hosts) {
		return provider.BulkObservation{}, observeErr(fmt.Errorf("nftables: BulkObserve: %w", firstErr))
	}
	return provider.BulkObservation{Observations: obs}, nil
}

// bulk は 1 つのホストの管理する chain を全部観測する（1 つの table でも
// 読めなければ error）。
func (h liveHost) bulk(ctx context.Context, p *LiveProvider, now time.Time) (map[core.IDKey]core.Observation, error) {
	obs := map[core.IDKey]core.Observation{}
	for _, ref := range p.tables {
		tbl, err := h.observeTable(ctx, ref)
		if err != nil {
			return nil, err
		}
		for _, c := range tbl.chains {
			if c.base || c.refuse(ref, h.name) != nil {
				continue
			}
			obs[core.NewID(ProviderName, p.scope, hostLocalID(h.name, ref, c.name)).Key()] = core.Observation{
				Availability: core.AvailabilityOk,
				State:        c.state(ref),
				ObservedAt:   now,
			}
		}
	}
	return obs, nil
}

// observeErr は観測の失敗を provider の error にする: ホスト鍵の不一致は
// PermanentError、それ以外は TransientError（どちらも unknown）。
func observeErr(err error) error { return remote.ProviderError(err) }

// ValidateIntent は読み込みの時点で、管理しない table の chain・識別子として
// 受けない名前・nft のスクリプトに埋め込めないルール文を断る（§28.4）。
func (p *LiveProvider) ValidateIntent(localID string, fields map[string]string) error {
	_, ref, chain, err := p.coords(localID)
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
	return validateBase(ref.Family, fields, localID)
}

// RefinePlan は rules の差分のうち、正規形にすると観測と同じもの（空白や
// カウンタの値だけの違い）と、priority の差分のうち数に直すと観測と同じもの
// （"filter" と "0"、Backlog #173）を落とす。現実が base chain なのに宣言が
// regular chain のものは断る（Backlog #173）。I/O はしない。
func (p *LiveProvider) RefinePlan(localID string, observed core.State, desired map[string]string, ops []core.Op) ([]core.Op, error) {
	family := ""
	if _, ref, _, err := p.coords(localID); err == nil {
		family = ref.Family
	}
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
	return refineBase(family, localID, observed, desired, out)
}

// chainCheck は Apply の後に読み戻して確かめる 1 つの chain。
type chainCheck struct {
	host    liveHost
	ref     TableRef
	chain   string
	rules   []string // 宣言のルール（正規形）
	deleted bool
	// def は名指しした base chain の宣言（base のときだけ）。
	base bool
	def  baseDef
}

// Apply は plan の op を 1 つの nft スクリプトにし、`nft -c -f -` で検査して
// から `nft -f -` で 1 つのトランザクションとして流す。流した後に読み戻し、
// 宣言の正規形と違えば PermanentError。base chain を変えるときは締め出しの守り
// （applyGuarded）を通す。
func (p *LiveProvider) Apply(ctx context.Context, plan core.Plan) error {
	// One transaction per host, in the order the hosts first appear.
	var order []string
	byHost := map[string][]core.Op{}
	hosts := map[string]liveHost{}
	for _, op := range plan.Ops {
		h, _, _, err := p.coords(op.Target.Local)
		if err != nil {
			return provider.NewPermanentError(fmt.Errorf("nftables: Apply: %w", err))
		}
		if _, ok := byHost[h.name]; !ok {
			order = append(order, h.name)
			hosts[h.name] = h
		}
		byHost[h.name] = append(byHost[h.name], op)
	}
	for _, name := range order {
		if err := p.applyHost(ctx, hosts[name], byHost[name]); err != nil {
			return err
		}
	}
	return nil
}

// applyHost は 1 つのホストの op を 1 つの nft スクリプトにして流す。
func (p *LiveProvider) applyHost(ctx context.Context, h liveHost, ops []core.Op) error {
	mu := liveApplyLock(h.name)
	mu.Lock()
	defer mu.Unlock()

	tables := map[TableRef]*liveTable{}
	var touched []TableRef // 変える table（写しを取る順）
	var script strings.Builder
	var checks []chainCheck
	var baseChanged []string // 変える base chain の id
	for _, op := range ops {
		_, ref, chain, err := p.coords(op.Target.Local)
		if err != nil {
			return provider.NewPermanentError(fmt.Errorf("nftables: Apply: %w", err))
		}
		tbl, ok := tables[ref]
		if !ok {
			if tbl, err = h.observeTable(ctx, ref); err != nil {
				return observeErr(fmt.Errorf("nftables: Apply: %w", err))
			}
			tables[ref] = tbl
		}
		cur := tbl.find(chain)
		if cur != nil {
			if err := cur.refuse(ref, h.name); err != nil {
				return provider.NewPermanentError(fmt.Errorf("nftables: Apply: %w", err))
			}
		}
		target := ref.Family + " " + ref.Name + " " + chain
		before := script.Len()

		switch op.Kind {
		case core.OpKindCreate, core.OpKindUpdate:
			to := map[string]string{}
			for _, f := range op.Fields {
				to[f.Field] = f.To
			}
			enc, has := to["rules"]
			if !has && cur != nil {
				enc = strings.Join(cur.rules, RuleSep) // rules は変わらない
			}
			rules := splitRules(enc)
			for i, r := range rules {
				if err := checkRuleText(r); err != nil {
					return provider.NewPermanentError(fmt.Errorf("nftables: Apply: %s rules[%d]: %w", op.Target.Local, i, err))
				}
			}
			// A base chain: declared as one (the op carries what changed, the
			// rest is as observed), or one in reality whose declaration the
			// PlanRefiner let through.
			named := declaresBase(to) || (cur != nil && cur.base)
			var def baseDef
			if named {
				fields := map[string]string{}
				if cur != nil && cur.base {
					fields["type"], fields["hook"], fields["priority"], fields["policy"] = cur.def.typ, cur.def.hook, cur.def.priority, cur.def.policy
				}
				for _, f := range baseFields {
					if v, ok := to[f]; ok {
						fields[f] = v
					}
				}
				if def, err = checkBaseFields(ref.Family, fields, op.Target.Local); err != nil {
					return provider.NewPermanentError(fmt.Errorf("nftables: Apply: %w", err))
				}
			}
			fresh := true // 空の chain から（ルールを足し直す）
			switch {
			case cur == nil:
				if !tbl.exists {
					fmt.Fprintf(&script, "add table %s %s\n", ref.Family, ref.Name)
					tbl.exists = true
				}
				writeAddChain(&script, target, named, def)
			case named && (!cur.base || !cur.def.sameHook(def)):
				// type・hook・priority は今ある chain の上では変えられない（regular
				// から base にもできない）: 同じトランザクションで作り直す。
				fmt.Fprintf(&script, "flush chain %s\ndelete chain %s\n", target, target)
				writeAddChain(&script, target, true, def)
			case named && cur.def.policy != def.policy:
				writeAddChain(&script, target, true, def) // policy だけを入れ替える
				fresh = has
			default:
				fresh = has
			}
			canon := make([]string, 0, len(rules))
			if fresh {
				fmt.Fprintf(&script, "flush chain %s\n", target)
				for _, r := range rules {
					fmt.Fprintf(&script, "add rule %s %s\n", target, r)
				}
			}
			for _, r := range rules {
				canon = append(canon, canonRule(r))
			}
			if script.Len() == before {
				continue // 何も変わらない
			}
			checks = append(checks, chainCheck{host: h, ref: ref, chain: chain, rules: canon, base: named, def: def})

		case core.OpKindDelete:
			if cur == nil {
				continue // 既に無い（idempotent）
			}
			fmt.Fprintf(&script, "flush chain %s\ndelete chain %s\n", target, target)
			checks = append(checks, chainCheck{host: h, ref: ref, chain: chain, deleted: true})

		default:
			return provider.NewPermanentError(fmt.Errorf("nftables: Apply: unknown op kind %v for %s", op.Kind, op.Target.Local))
		}
		if script.Len() > before {
			if !containsRef(touched, ref) {
				touched = append(touched, ref)
			}
			if declaresBase(opFields(op)) || (cur != nil && cur.base) {
				baseChanged = append(baseChanged, hostLocalID(h.name, ref, chain))
			}
		}
	}
	if script.Len() == 0 {
		return nil
	}

	body := []byte(script.String())
	if len(baseChanged) > 0 {
		return p.applyGuarded(ctx, h, touched, baseChanged, body, checks)
	}
	if _, err := h.runner.Run(ctx, []string{"-c", "-f", "-"}, body); err != nil {
		return classifyApplyErr(h.what("nft -c (check)"), err, true)
	}
	if _, err := h.runner.Run(ctx, []string{"-f", "-"}, body); err != nil {
		return classifyApplyErr(h.what("nft -f"), err, false)
	}
	return p.verify(ctx, checks)
}

// opFields は op の To を map にする。
func opFields(op core.Op) map[string]string {
	m := make(map[string]string, len(op.Fields))
	for _, f := range op.Fields {
		m[f.Field] = f.To
	}
	return m
}

// writeAddChain は add chain の行を書く（base chain なら { type ... policy ... ; }）。
func writeAddChain(w *strings.Builder, target string, base bool, def baseDef) {
	if base {
		fmt.Fprintf(w, "add chain %s { %s }\n", target, def.spec())
		return
	}
	fmt.Fprintf(w, "add chain %s\n", target)
}

func containsRef(refs []TableRef, ref TableRef) bool {
	for _, r := range refs {
		if r == ref {
			return true
		}
	}
	return false
}

// applyGuarded は base chain を変えるトランザクションを締め出しの守りを通して
// 流す（guard.go の 1〜6）。確かめられなかった・巻き戻しが先に動いたときは
// provider.RolledBackError。
func (p *LiveProvider) applyGuarded(ctx context.Context, h liveHost, touched []TableRef, changed []string, body []byte, checks []chainCheck) error {
	if h.guard == nil {
		what := "set ruleset_confirm_address (--ruleset-confirm-address) to a host:port a new connection must reach after the change"
		if h.name != "" {
			what = "the host has no way to confirm reaching it"
		}
		return provider.NewPermanentError(fmt.Errorf("nftables: Apply: refusing to change base chain(s) %s without the lockout guard: %s", strings.Join(changed, ", "), what))
	}
	// 1. The tables as they are now.
	var restore bytes.Buffer
	for _, ref := range touched {
		fmt.Fprintf(&restore, "add table %s %s\ndelete table %s %s\n", ref.Family, ref.Name, ref.Family, ref.Name)
		out, err := h.runner.Run(ctx, []string{"list", "table", ref.Family, ref.Name}, nil)
		if err != nil {
			if noSuchTable(err) {
				continue
			}
			return observeErr(fmt.Errorf("nftables: Apply: copy table %s before changing it: %w", ref, err))
		}
		restore.Write(out)
		if len(out) > 0 && out[len(out)-1] != '\n' {
			restore.WriteByte('\n')
		}
	}
	// 2. Check.
	if _, err := h.runner.Run(ctx, []string{"-c", "-f", "-"}, body); err != nil {
		return classifyApplyErr(h.what("nft -c (check)"), err, true)
	}
	// 3. Arm the rollback on the host.
	token, err := h.guard.Arm(ctx, restore.Bytes(), p.rollbackAfter)
	if err != nil {
		return remote.ProviderError(fmt.Errorf("nftables: Apply: %s: %w", h.what("lockout guard"), err))
	}
	fireAt := time.Now().Add(p.rollbackAfter)
	// 4. Apply.
	if _, err := h.runner.Run(ctx, []string{"-f", "-"}, body); err != nil {
		if derr := h.guard.Disarm(ctx, token); derr != nil && !errors.Is(derr, ErrRollbackFired) {
			// The rollback restores what was there (nothing was changed).
			err = fmt.Errorf("%w (and the rollback could not be cancelled: %v; it restores the tables as they were)", err, derr)
		}
		return classifyApplyErr(h.what("nft -f"), err, false)
	}
	// 5. Confirm over a new connection, until half the time is gone.
	confirmErr := p.confirm(ctx, h, fireAt.Add(-p.rollbackAfter/2))
	tables := refsText(touched)
	who := "the host"
	if h.name != "" {
		who = "host " + h.name
	}
	if confirmErr == nil {
		// 6. Cancel the rollback.
		derr := h.guard.Disarm(ctx, token)
		if derr == nil {
			return p.verify(ctx, checks)
		}
		if !errors.Is(derr, ErrRollbackFired) {
			waitUntil(ctx, fireAt.Add(rollbackGrace(p.rollbackAfter)))
		}
		return provider.NewRolledBackError(fmt.Errorf(
			"nftables: Apply: %s was reached after changing base chain(s) %s, but the rollback could not be cancelled (%v): it restored tables %s as they were before",
			who, strings.Join(changed, ", "), derr, tables))
	}
	// Not confirmed: the rollback armed on the host puts the tables back.
	waitUntil(ctx, fireAt.Add(rollbackGrace(p.rollbackAfter)))
	return provider.NewRolledBackError(fmt.Errorf(
		"nftables: Apply: %s could not be reached over a new connection within %s after changing base chain(s) %s (%v): the rollback armed on the host restored tables %s as they were before, after %s. Declare a rule that keeps the management path open (e.g. jump to the minimal chain kept outside Sakutei) and apply again",
		who, p.rollbackAfter/2, strings.Join(changed, ", "), confirmErr, tables, p.rollbackAfter))
}

// confirm は until まで新しい接続での確かめを張り直す。届けば nil、届かなければ
// 最後の error。
func (p *LiveProvider) confirm(ctx context.Context, h liveHost, until time.Time) error {
	interval := p.rollbackAfter / 20
	if interval > time.Second {
		interval = time.Second
	}
	for {
		cctx, cancel := context.WithDeadline(ctx, until)
		err := h.guard.Confirm(cctx)
		cancel()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil || time.Now().Add(interval).After(until) {
			return err
		}
		waitUntil(ctx, time.Now().Add(interval))
	}
}

// rollbackGrace は巻き戻しの上限の後、写しを流し終えるのを待つ余り。
func rollbackGrace(after time.Duration) time.Duration {
	g := after / 4
	if g > 2*time.Second {
		g = 2 * time.Second
	}
	return g
}

// waitUntil は t まで（ctx が終われば早く）待つ。
func waitUntil(ctx context.Context, t time.Time) {
	d := time.Until(t)
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

func refsText(refs []TableRef) string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.String()
	}
	return strings.Join(out, ", ")
}

// verify は Apply の後に読み戻し、各 chain が宣言どおり（正規形で）かを確かめる。
func (p *LiveProvider) verify(ctx context.Context, checks []chainCheck) error {
	tables := map[TableRef]*liveTable{}
	for _, ck := range checks {
		tbl, ok := tables[ck.ref]
		if !ok {
			var err error
			if tbl, err = ck.host.observeTable(ctx, ck.ref); err != nil {
				return observeErr(fmt.Errorf("nftables: Apply: read back: %w", err))
			}
			tables[ck.ref] = tbl
		}
		id := hostLocalID(ck.host.name, ck.ref, ck.chain)
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
		if ck.base && (!c.base || c.def != ck.def) {
			return provider.NewTransientError(fmt.Errorf("nftables: Apply: %s is %q after apply, not %q", id, c.def.spec(), ck.def.spec()))
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
		return remote.ProviderError(err)
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
	// def は base chain の type・hook・priority・policy（base のときだけ）。
	def baseDef
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

// refuse は chain が管理境界の外なら理由を返す（base chain は宣言したときだけ
// 扱う。List・BulkObserve が別に除く、Backlog #173）。
func (c *liveChain) refuse(ref TableRef, host string) error {
	id := hostLocalID(host, ref, c.name)
	switch {
	case c.opaque:
		return fmt.Errorf("refusing to manage chain %q (a rule contains %q)", id, RuleSep)
	case !identRe.MatchString(c.name):
		return fmt.Errorf("refusing to manage chain %q (name is not a plain identifier)", id)
	}
	return nil
}

func (c *liveChain) state(ref TableRef) core.State {
	st := core.State{
		"family": {Kind: core.FieldKindString, Value: ref.Family},
		"table":  {Kind: core.FieldKindString, Value: ref.Name},
		"chain":  {Kind: core.FieldKindString, Value: c.name},
		"rules":  {Kind: core.FieldKindStringList, Value: strings.Join(c.rules, RuleSep)},
	}
	if c.base {
		c.def.put(st)
	}
	return st
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
	Prio   *int   `json:"prio"`
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
func (h liveHost) observeTable(ctx context.Context, ref TableRef) (*liveTable, error) {
	out, err := h.runner.Run(ctx, []string{"-j", "list", "table", ref.Family, ref.Name}, nil)
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
	txt, err := h.runner.Run(ctx, []string{"-a", "-s", "list", "table", ref.Family, ref.Name}, nil)
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
				lc := liveChain{
					name:   c.Name,
					handle: c.Handle,
					base:   c.Type != "" || c.Hook != "" || c.Policy != "",
				}
				if lc.base {
					lc.def = baseDef{typ: c.Type, hook: c.Hook, policy: c.Policy}
					if c.Prio != nil {
						lc.def.priority = strconv.Itoa(*c.Prio)
					}
					if lc.def.policy == "" {
						lc.def.policy = "accept" // nft の既定
					}
				}
				tbl.chains = append(tbl.chains, lc)
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

// coords は local id をホスト・管理する table・chain に分ける。形が違う・
// 知らないホスト・管理しない table・識別子として受けない名前は error。
func (p *LiveProvider) coords(localID string) (liveHost, TableRef, string, error) {
	h := p.hosts[0]
	rest := localID
	if p.multi {
		name, after, ok := strings.Cut(localID, "/")
		if !ok {
			return liveHost{}, TableRef{}, "", fmt.Errorf("invalid local id %q: want <host>/<family>/<table>/<chain>", localID)
		}
		found := false
		for _, c := range p.hosts {
			if c.name == name {
				h, found = c, true
			}
		}
		if !found {
			return liveHost{}, TableRef{}, "", fmt.Errorf("invalid local id %q: host %q is not one of this provider's hosts", localID, name)
		}
		rest = after
	}
	ref, chain, err := p.tableChain(localID, rest)
	return h, ref, chain, err
}

// tableChain は "<family>/<table>/<chain>" を管理する table と chain に分ける。
func (p *LiveProvider) tableChain(localID, rest string) (TableRef, string, error) {
	family, table, chain, err := splitLocalID(rest)
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

// hostLocalID は id（ホストがあれば "<ホスト>/" を前に付ける）。
func hostLocalID(host string, ref TableRef, chain string) string {
	if host == "" {
		return chainLocalID(ref, chain)
	}
	return host + "/" + chainLocalID(ref, chain)
}

// what は文言にホストを添える。
func (h liveHost) what(s string) string {
	if h.name == "" {
		return s
	}
	return s + " on " + h.name
}

func unknownObs() core.Observation {
	return core.Observation{Availability: core.AvailabilityUnknown, ObservedAt: time.Now()}
}
