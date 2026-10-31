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

// Package nftables は nftables の chain（順序のあるルール列）を宣言どおりに保つ
// provider（設計 §7.1、E-B2・E-B3・E-B4・N-B9）。コアの外のモジュールで、コアの口
// core.Provider・core.Validator・core.Transactor を持つ。
//
//	providers:                                  # つなぎ方の設定
//	  fw:
//	    module: nftables
//	    settings:
//	      hosts:
//	        lab: {ruleset: /srv/fw/lab.rules}   # file：ruleset のファイル（FW には反映しない）
//	        fw1:                                # live：カーネルの ruleset。root で走るのは道具だけ
//	          ssh: {address: 192.0.2.1, user: sakutei, key: /etc/sakutei/id_ed25519, known_hosts: /etc/sakutei/known_hosts}
//	          sudo: true
//	          guard: [/usr/local/libexec/sakutei-nft-guard]
//
//	ranges:                                     # intent（範囲は intent にだけ書く）
//	  - provider: fw
//	    args: {host: fw1, tables: "inet/filter"}
//	resources:
//	  - type: fw/chain
//	    id: fw1/inet/filter/input               # <host>/<family>/<table>/<chain>
//	    fields: {type: filter, hook: input, priority: 0, policy: drop, rules: ["ct state established,related accept", "jump mgmt"]}
//	    apply:
//	      transaction:
//	        timeout: 30s
//	        confirm:
//	          - {phase: before, when: 'observe(probe/tcp/fw1:22).reachable == true'}
//	          - {phase: after,  when: 'observe(probe/tcp/fw1:22).reachable == true'}
//
// 決まり：
//
//   - 扱う table は Range の args.tables（E-B2）。範囲の外の table・宣言していない
//     base chain・set・map は読まず書かない。List は範囲の table の regular chain
//     だけを出す（E-B4）。
//   - rules は順序のあるルール文のリスト（全置換、E-B12）。宣言は正規形（引用符の外の
//     空白 1 つ）で書く（E-I7）。
//   - type・hook・priority・policy の 4 つを書いた宣言が base chain の宣言（4 つとも
//     要る。policy を書き忘れて既定の accept にしない）。宣言が regular chain なのに
//     現実が base chain なら Unknown（既定の拒否の chain のルールを policy を言わずに
//     変えない、E-B3）。
//   - live の base chain の宣言には apply.transaction（2 段の Apply）を書く（締め出しの
//     守り、E-B3）。Prepare は道具の arm（写しとホストの側の時間切れの戻し）と apply、
//     Commit は disarm、Rollback は道具の rollback。Sakutei が落ちても接続が切れても、
//     ホストの側で戻る（不変条件 17）。file の宣言には書けない（FW を変えない）。
package nftables

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/providers/nftguard"
	"github.com/tjst-t/sakutei/v3/sdk/hostexec"
)

// Version は Schema の版。
const Version = "nftables/1"

// Kind は chain の種類の名前。
const Kind = "chain"

// host は 1 つのホスト：file か live。
type host struct {
	name    string
	ruleset string // file の対象
	guard   *guard // live の対象
}

// Provider は nftables の provider。
type Provider struct {
	mu    sync.Mutex
	hosts map[string]*host
	// tables はホスト → 範囲の table（Validate で受け取る）。
	tables map[string]map[string]bool
	// regular は regular chain として宣言した ID（現実が base chain なら Unknown）。
	regular map[core.ID]bool
}

var (
	_ core.Provider   = (*Provider)(nil)
	_ core.Validator  = (*Provider)(nil)
	_ core.Transactor = (*Provider)(nil)
)

// Module は settings から provider を作る（load.Module）。
func Module(settings map[string]any) (core.Provider, error) {
	for k := range settings {
		if k != "hosts" {
			return nil, fmt.Errorf("settings に知らない項目 %q（書ける項目: hosts）", k)
		}
	}
	hs, err := hostexec.ParseHosts(settings["hosts"], "ruleset", "guard")
	if err != nil {
		return nil, err
	}
	p := &Provider{hosts: map[string]*host{}, tables: map[string]map[string]bool{}, regular: map[core.ID]bool{}}
	for name, h := range hs {
		ho := &host{name: name}
		if v, ok := h.Extra["ruleset"]; ok {
			s, isStr := v.(string)
			if !isStr || s == "" {
				return nil, fmt.Errorf("settings.hosts.%s.ruleset にファイルを書く", name)
			}
			if h.Runner != nil || h.Extra["guard"] != nil {
				return nil, fmt.Errorf("settings.hosts.%s: ruleset（file）と local・ssh・guard（live）はどちらか", name)
			}
			ho.ruleset = s
		} else {
			if h.Runner == nil {
				return nil, fmt.Errorf("settings.hosts.%s: ruleset か local・ssh を書く", name)
			}
			argv := []string{nftguard.DefaultPath}
			if v, ok := h.Extra["guard"]; ok {
				if argv, err = hostexec.StringList(v); err != nil {
					return nil, fmt.Errorf("settings.hosts.%s.guard: %w", name, err)
				}
			}
			ho.guard = &guard{runner: h.Runner, argv: argv}
		}
		p.hosts[name] = ho
	}
	return p, nil
}

// Schema は chain の項目。
func (p *Provider) Schema() core.Schema {
	return core.Schema{Version: Version, Kinds: map[string]core.KindSchema{
		Kind: {Fields: map[string]core.FieldSpec{
			"rules":    {Type: core.List},
			"type":     {Type: core.String, Identity: true},
			"hook":     {Type: core.String, Identity: true},
			"priority": {Type: core.Int, Identity: true},
			"policy":   {Type: core.String},
		}},
	}}
}

// coords は ID の名前を host・family・table・chain に分ける。
func coords(id core.ID) (h, family, table, chain string, err error) {
	f := strings.Split(id.Name, "/")
	if len(f) != 4 || f[0] == "" || !families[f[1]] || !identRe.MatchString(f[2]) || !identRe.MatchString(f[3]) {
		return "", "", "", "", fmt.Errorf("id %q は <host>/<family>/<table>/<chain>（family は ip・ip6・inet・arp・bridge・netdev）", id.Name)
	}
	return f[0], f[1], f[2], f[3], nil
}

var baseFields = []string{"type", "hook", "priority", "policy"}

// Validate は範囲と宣言を確かめ、範囲の table と regular chain の宣言を覚える
// （E-B2・E-B3・E-I6・E-I7・E-K8）。
func (p *Provider) Validate(res []core.Resource, ranges []core.Range) []core.Diagnostic {
	var diags []core.Diagnostic
	rej := func(o core.Origin, target, format string, a ...any) {
		diags = append(diags, core.Diagnostic{Severity: core.Reject, Origin: o, Target: target, Message: fmt.Sprintf(format, a...)})
	}
	tables := map[string]map[string]bool{}
	for _, rg := range ranges {
		for k := range rg.Args {
			if k != "host" && k != "tables" {
				rej(rg.Origin, "ranges", "nftables の範囲の引数は host と tables（%q が書かれている）", k)
			}
		}
		h, list := rg.Args["host"], rg.Args["tables"]
		if p.hosts[h] == nil {
			rej(rg.Origin, "ranges", "host %q がつなぎ方の設定の hosts に無い", h)
			continue
		}
		if list == "" {
			rej(rg.Origin, "ranges", "tables に扱う table を書く（例 \"inet/filter,ip/nat\"。推測しない）")
			continue
		}
		if tables[h] == nil {
			tables[h] = map[string]bool{}
		}
		for _, t := range strings.Split(list, ",") {
			t = strings.TrimSpace(t)
			f, n, ok := strings.Cut(t, "/")
			if !ok || !families[f] || !identRe.MatchString(n) {
				rej(rg.Origin, "ranges", "table %q は <family>/<name>", t)
				continue
			}
			if tables[h][t] {
				rej(rg.Origin, "ranges", "host %s の table %s が 2 つの範囲に入る", h, t)
			}
			tables[h][t] = true
		}
	}
	regular := map[core.ID]bool{}
	for _, r := range res {
		target := r.ID.String()
		h, family, table, _, err := coords(r.ID)
		if err != nil {
			rej(r.Origin, target, "%v", err)
			continue
		}
		ho := p.hosts[h]
		switch {
		case ho == nil:
			rej(r.Origin, target, "host %q がつなぎ方の設定の hosts に無い", h)
			continue
		case !tables[h][family+"/"+table]:
			rej(r.Origin, target, "table %s/%s は host %s の範囲（ranges の tables）に無い", family, table, h)
			continue
		}
		if r.Ensure == core.EnsureAbsent {
			continue
		}
		n := 0
		for _, k := range baseFields {
			if _, ok := r.Fields[k]; ok {
				n++
			}
		}
		switch {
		case n == 0:
			regular[r.ID] = true
		case n != len(baseFields):
			rej(r.Origin, target, "base chain の宣言は type・hook・priority・policy の 4 つを書く（policy を書き忘れて既定の accept にしない）")
		default:
			if s, _ := r.Fields["type"].(string); !types[s] {
				rej(r.Origin, target, "type は filter・nat・route")
			}
			if s, _ := r.Fields["hook"].(string); !hooks[s] {
				rej(r.Origin, target, "hook は prerouting・input・forward・output・postrouting")
			}
			if s, _ := r.Fields["policy"].(string); !policies[s] {
				rej(r.Origin, target, "policy は accept か drop")
			}
			if ho.guard != nil && r.Txn() == nil {
				rej(r.Origin, target, "live の base chain は apply.transaction（2 段の Apply）で当てる（締め出しの守り、E-B3）")
			}
		}
		if ho.ruleset != "" && r.Txn() != nil {
			rej(r.Origin, target, "file の対象（ruleset）には apply.transaction を書けない（FW を変えない）")
		}
		if l, ok := r.Fields["rules"].([]any); ok {
			for i, e := range l {
				s, isStr := e.(string)
				if !isStr {
					rej(r.Origin, target, "rules[%d] は文字列", i)
					continue
				}
				if err := checkRule(s); err != nil {
					rej(r.Origin, target, "rules[%d]: %v", i, err)
				} else if c := canonRule(s); c != s {
					rej(r.Origin, target, "rules[%d] は正規形 %q で書く（E-I7）", i, c)
				}
			}
		}
	}
	p.mu.Lock()
	p.tables, p.regular = tables, regular
	p.mu.Unlock()
	return diags
}

func (p *Provider) managed(h, family, table string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tables[h][family+"/"+table]
}

// readTables はホストの table を 1 回ずつ読む。
type reader struct {
	p     *Provider
	files map[string]*rulesetFile
	errs  map[string]error
	live  map[string]*tableState
}

func (p *Provider) newReader() *reader {
	return &reader{p: p, files: map[string]*rulesetFile{}, errs: map[string]error{}, live: map[string]*tableState{}}
}

func (rd *reader) table(ctx context.Context, h *host, family, name string) (*tableState, error) {
	key := h.name + "/" + family + "/" + name
	if err := rd.errs[key]; err != nil {
		return nil, err
	}
	if t, ok := rd.live[key]; ok {
		return t, nil
	}
	var t *tableState
	var err error
	if h.ruleset != "" {
		rf, ok := rd.files[h.name]
		if !ok {
			rf, err = readRuleset(h.ruleset)
			if err != nil {
				rd.errs[key] = err
				return nil, err
			}
			rd.files[h.name] = rf
		}
		t = rf.tables[family+"/"+name]
		if t == nil {
			t = &tableState{}
		}
	} else {
		t, err = h.guard.readTable(ctx, family, name)
		if err != nil {
			rd.errs[key] = err
			return nil, err
		}
	}
	rd.live[key] = t
	return t, nil
}

func unknown(note string) core.Observation {
	return core.Observation{Availability: core.Unknown, Note: note}
}

// Observe は table ごとに 1 回読む（E-D8）。
func (p *Provider) Observe(ctx context.Context, ids []core.ID) (map[core.ID]core.Observation, error) {
	out := map[core.ID]core.Observation{}
	rd := p.newReader()
	for _, id := range ids {
		out[id] = p.observe(ctx, rd, id)
	}
	return out, nil
}

func (p *Provider) observe(ctx context.Context, rd *reader, id core.ID) core.Observation {
	h, family, table, chain, err := coords(id)
	if err != nil {
		return unknown(err.Error())
	}
	ho := p.hosts[h]
	if ho == nil || !p.managed(h, family, table) {
		return unknown("範囲（ranges の tables）の外")
	}
	t, err := rd.table(ctx, ho, family, table)
	if err != nil {
		return unknown(err.Error())
	}
	c := t.find(chain)
	if !t.exists || c == nil {
		return core.Observation{Availability: core.Absent}
	}
	if c.opaque != "" {
		return unknown("chain を管理できない: " + c.opaque)
	}
	p.mu.Lock()
	regular := p.regular[id]
	p.mu.Unlock()
	if c.base && regular {
		return unknown("現実は base chain（policy " + c.def.policy + "）。type・hook・priority・policy も宣言する（E-B3）")
	}
	f := core.Fields{"rules": toAny(c.rules)}
	if c.base {
		f["type"], f["hook"], f["priority"], f["policy"] = c.def.typ, c.def.hook, c.def.priority, c.def.policy
	}
	return core.Observation{Availability: core.Present, Fields: f}
}

func toAny(l []string) []any {
	out := make([]any, len(l))
	for i, s := range l {
		out[i] = s
	}
	return out
}

// List は範囲の table の regular chain（宣言していない base chain は出さない、E-B4）。
func (p *Provider) List(ctx context.Context, r core.Range) (core.Listing, error) {
	ho := p.hosts[r.Args["host"]]
	if ho == nil {
		return core.Listing{}, fmt.Errorf("host %q が無い", r.Args["host"])
	}
	rd := p.newReader()
	var ids []core.ID
	for _, t := range strings.Split(r.Args["tables"], ",") {
		family, name, _ := strings.Cut(strings.TrimSpace(t), "/")
		tbl, err := rd.table(ctx, ho, family, name)
		if err != nil {
			return core.Listing{}, err
		}
		for _, c := range tbl.chains {
			if c.base || c.opaque != "" {
				continue
			}
			ids = append(ids, core.ID{Type: r.Provider + "/" + Kind, Name: strings.Join([]string{ho.name, family, name, c.name}, "/")})
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].Name < ids[j].Name })
	return core.Listing{IDs: ids, Complete: true}, nil
}

// chainOp は chain 1 つの変更。
type chainOp struct {
	family, table, chain string
	delete               bool
	base                 *baseDef
	rules                []string
	cur                  *chainState
}

// op は宣言から変更を作る（今の chain を読み直して、作り直しが要るかを決める）。
func (p *Provider) op(ctx context.Context, rd *reader, ch core.Change) (*host, chainOp, error) {
	h, family, table, chain, err := coords(ch.ID)
	if err != nil {
		return nil, chainOp{}, err
	}
	ho := p.hosts[h]
	if ho == nil || !p.managed(h, family, table) {
		return nil, chainOp{}, fmt.Errorf("%s は範囲（ranges の tables）の外", ch.ID)
	}
	t, err := rd.table(ctx, ho, family, table)
	if err != nil {
		return nil, chainOp{}, err
	}
	op := chainOp{family: family, table: table, chain: chain, delete: ch.Delete, cur: t.find(chain)}
	if op.cur != nil && op.cur.opaque != "" {
		return nil, chainOp{}, fmt.Errorf("chain %s を管理できない: %s", ch.ID, op.cur.opaque)
	}
	if ch.Delete {
		return ho, op, nil
	}
	if l, ok := ch.Desired["rules"].([]any); ok {
		for _, e := range l {
			op.rules = append(op.rules, fmt.Sprint(e))
		}
	} else if op.cur != nil {
		op.rules = op.cur.rules // rules を宣言しない：今のルールを保つ
	}
	if typ, ok := ch.Desired["type"].(string); ok {
		d := baseDef{typ: typ}
		d.hook, _ = ch.Desired["hook"].(string)
		d.priority, _ = ch.Desired["priority"].(int64)
		d.policy, _ = ch.Desired["policy"].(string)
		op.base = &d
	} else if op.cur != nil && op.cur.base {
		return nil, chainOp{}, fmt.Errorf("%s の現実は base chain。type・hook・priority・policy も宣言する", ch.ID)
	}
	return ho, op, nil
}

// Apply は 1 つの chain を当てる（2 段でない宣言）。
func (p *Provider) Apply(ctx context.Context, _ core.WriteContext, id core.ID, desired core.Fields, _ core.Observation) error {
	return p.write(ctx, []core.Change{{ID: id, Desired: desired}})
}

// Delete は chain を消す（flush と delete を 1 つのトランザクションで）。
func (p *Provider) Delete(ctx context.Context, _ core.WriteContext, id core.ID, _ core.Observation) error {
	return p.write(ctx, []core.Change{{ID: id, Delete: true}})
}

func (p *Provider) write(ctx context.Context, changes []core.Change) error {
	rd := p.newReader()
	byHost, order, err := p.ops(ctx, rd, changes)
	if err != nil {
		return err
	}
	for _, name := range order {
		ho := p.hosts[name]
		if ho.ruleset != "" {
			if err := p.writeFile(ho, byHost[name]); err != nil {
				return err
			}
			continue
		}
		if err := ho.guard.apply(ctx, script(byHost[name])); err != nil {
			return err
		}
	}
	return nil
}

func (p *Provider) ops(ctx context.Context, rd *reader, changes []core.Change) (map[string][]chainOp, []string, error) {
	byHost := map[string][]chainOp{}
	var order []string
	for _, ch := range changes {
		ho, op, err := p.op(ctx, rd, ch)
		if err != nil {
			return nil, nil, err
		}
		if byHost[ho.name] == nil {
			order = append(order, ho.name)
		}
		byHost[ho.name] = append(byHost[ho.name], op)
	}
	sort.Strings(order)
	return byHost, order, nil
}

func (p *Provider) writeFile(ho *host, ops []chainOp) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	rf, err := readRuleset(ho.ruleset)
	if err != nil {
		return err
	}
	for _, op := range ops {
		rf.set(op)
	}
	return writeAtomic(ho.ruleset, []byte(rf.text()))
}

// Prepare は 2 段の Apply の準備（E-B3・E-B8・不変条件 17）。ホストごとに、道具で
// 確かめ（check）、変える table の写しとホストの側の時間切れの戻しを置き（arm）、
// 1 つのトランザクションで流す（apply）。流せなければ置いた戻しをすぐ使う。
func (p *Provider) Prepare(ctx context.Context, _ core.WriteContext, changes []core.Change, opt core.TxnOptions) (core.Txn, error) {
	rd := p.newReader()
	byHost, order, err := p.ops(ctx, rd, changes)
	if err != nil {
		return nil, err
	}
	txn := &txn{}
	for _, name := range order {
		ho := p.hosts[name]
		if ho.guard == nil {
			txn.Rollback(ctx)
			return nil, fmt.Errorf("host %s は file の対象（2 段の Apply は live だけ）", name)
		}
		body := script(byHost[name])
		if _, err := ho.guard.run(ctx, body, "check"); err != nil {
			txn.Rollback(ctx)
			return nil, fmt.Errorf("host %s: nft -c に落ちた: %w", name, err)
		}
		var tables []string
		seen := map[string]bool{}
		for _, op := range byHost[name] {
			t := op.family + "/" + op.table
			if !seen[t] {
				seen[t] = true
				tables = append(tables, t)
			}
		}
		token, err := ho.guard.arm(ctx, rollbackSeconds(opt.Timeout), tables)
		if err != nil {
			txn.Rollback(ctx)
			return nil, fmt.Errorf("host %s: %w", name, err)
		}
		txn.armed = append(txn.armed, armed{host: ho, token: token})
		if _, err := ho.guard.run(ctx, body, "apply"); err != nil {
			txn.Rollback(ctx)
			return nil, fmt.Errorf("host %s: nft -f に落ちた: %w", name, err)
		}
	}
	return txn, nil
}

type armed struct {
	host  *host
	token string
}

// txn はホストごとに置いた戻しの仕掛け。
type txn struct {
	armed []armed
}

// Commit は仕掛けを外す（disarm）。もう戻っていれば誤り。
func (t *txn) Commit(ctx context.Context) error {
	var errs []string
	for _, a := range t.armed {
		out, err := a.host.guard.run(ctx, nil, "disarm", a.token)
		switch {
		case err != nil:
			errs = append(errs, fmt.Sprintf("host %s: 仕掛けを外せない（時間切れで戻るかもしれない）: %v", a.host.name, err))
		case strings.TrimSpace(string(out)) != "disarmed":
			errs = append(errs, fmt.Sprintf("host %s: 外す前に時間切れで戻った", a.host.name))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "・"))
	}
	return nil
}

// Rollback は仕掛けをすぐ使って写しに戻す（呼ばなくても時間切れで戻る）。
func (t *txn) Rollback(ctx context.Context) error {
	var errs []string
	for _, a := range t.armed {
		if _, err := a.host.guard.run(ctx, nil, "rollback", a.token); err != nil {
			errs = append(errs, fmt.Sprintf("host %s: %v", a.host.name, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "・"))
	}
	return nil
}
