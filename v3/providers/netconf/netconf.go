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

// Package netconf は NETCONF の機器の設定の部分木を観測し、設定で許したときは
// commit confirmed の 2 段で当てる provider（設計 §7.1、E-B7・E-B8・E-B9・E-B10・
// N-B1・N-B4）。コアの外のモジュールで、コアの口 core.Provider・core.Validator・
// core.Transactor を持つ。v2 の netconf provider（#141・#175・#176・#199・#200・
// #208）を移した。
//
//	providers:                                  # つなぎ方の設定
//	  nc:
//	    module: netconf
//	    settings:
//	      apply: true                           # 2 段の Apply を許す（既定は観測だけ）
//	      commit_flow: "1.0"                    # "1.1" なら :confirmed-commit:1.1 の persist
//	      hosts:
//	        sw1:
//	          ssh: {address: 192.0.2.1, port: 830, user: sakutei-ro, key: /etc/sakutei/id_ro, known_hosts: /etc/sakutei/known_hosts}
//	          write:                            # 省略可。書き込み用の資格（Prepare・Commit だけで使う）
//	            ssh: {address: 192.0.2.1, port: 830, user: sakutei, key: /etc/sakutei/id_rw, known_hosts: /etc/sakutei/known_hosts}
//
//	ranges:                                     # intent（機器と部分木は範囲）
//	  - provider: nc
//	    args:
//	      host: sw1
//	      filter.acl: '<nacm xmlns="urn:ietf:params:xml:ns:yang:ietf-netconf-acm"><rule-list><name>edge</name></rule-list></nacm>'
//	      ordered.acl: nacm/rule-list/rule      # 省略可。順に意味のある列の道
//	resources:
//	  - type: nc/subtree
//	    id: sw1/acl                             # <host>/<部分木>
//	    fields: {config: '<nacm xmlns="…"><rule-list><name>edge</name>…</rule-list></nacm>'}
//	    apply:
//	      transaction:
//	        group: core                         # 省略可。同じ名前の台を 1 回にまとめる（commit_group）
//	        timeout: 60s                        # confirm-timeout
//	        confirm:
//	          - {phase: after, when: 'check(mgmt-path)'}
//
// 決まり：
//
//   - リソースは設定の部分木（種類 subtree、ID <host>/<部分木>）。どこ（subtree filter）は
//     Range の filter.<部分木>、中身（宣言する XML）は項目 config。観測は
//     <get-config source="running"> を filter で絞って取る（部分木の外を読まない、
//     NACM で部分木だけに絞ったユーザーでも動く、E-B10）。
//   - 比べ方は名前空間・空白・属性の順をそろえた正規形（canon.go）。兄弟の順は問わず、
//     Range の ordered.<部分木> で名指しした道だけ順を比べる。属性は宣言に書いたもの
//     だけを比べる（機器が付ける commit の時刻などで drift にしない）。観測の config は、
//     宣言と意味が同じなら宣言の書き方のまま、違えば機器の正規形を返す（E-I7・E-B7）。
//   - 届かない・認証・hello が合わない・区切りが壊れた・rpc-error・access-denied は
//     Unknown（Absent にしない、E-K1）。中身の無い要素だけの <data> は Absent。
//   - 既定は観測だけ（KindSchema.ObserveOnly）。settings の apply: true のときだけ当て、
//     いつも commit confirmed の 2 段で当てる（E-B8）。Prepare は新しいセッションで
//     candidate を lock し、部分木の置き換えを edit-config・validate し、同じセッションで
//     commit confirmed（confirm-timeout は transaction.timeout）を送る。Commit は同じ
//     セッションで確定の commit、Rollback は discard と cancel-commit（1.1）かセッションを
//     閉じる（1.0 はセッションの終わりで戻る）。どちらも呼ばれなければ機器が
//     confirm-timeout で戻す（不変条件 17）。準備のあと確定までの観測は新しい接続で
//     張る（変えたあとも届くことを確かめる、E-B8）。apply.transaction を書かない宣言の
//     Apply も同じ 2 段で、新しい接続で読み直して宣言どおりなら確定する（settings の
//     timeout、既定 30s）。
//   - transaction.group（commit_group）の台は、コアが 1 回の Prepare にまとめて渡す。
//     全部の台で commit confirmed を送ってから確かめ、全部が通ったときだけ確定する
//     （E-B9）。confirm の observe(…)・check(…) はコアの Condition。
//   - List は範囲の部分木のうち機器に在るもの（宣言外の報告）。部分木は消さない
//     （prune: true の範囲・ensure: absent は読み込みで断り、Delete は RefuseError）。
package netconf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/providers/internal/sshstream"
	"github.com/tjst-t/sakutei/v3/sdk/hostexec"
)

// Version は Schema の版（正規形・項目を変えたら上げる）。
const Version = "netconf/1"

// Kind は部分木の種類の名前。
const Kind = "subtree"

// Subsystem は ssh のサブシステムの名前（RFC 6242 §3）。
const Subsystem = "netconf"

// 項目。
const (
	// FieldConfig は部分木の中身の XML（宣言、必須）。
	FieldConfig = "config"
	// FieldCapabilities は hello の NETCONF の capability の短い名前（観測だけ）。
	FieldCapabilities = "capabilities"
)

// DefaultTimeout は ctx に期限が無いときの 1 回の観測の上限。
const DefaultTimeout = 60 * time.Second

// Dialer はホストへの流れ（netconf サブシステム）を開く。
type Dialer func(ctx context.Context) (io.ReadWriteCloser, error)

// Host は 1 台の機器へのつなぎ方。Write が nil なら Observe と同じものを使う。
type Host struct {
	Observe Dialer
	Write   Dialer
}

// Options は 2 段の Apply の設定。
type Options struct {
	// Apply は 2 段の Apply を許す（既定は観測だけ）。
	Apply bool
	// Persist は :confirmed-commit:1.1 を持つ機器で persist の流れを使う。
	Persist bool
	// Timeout は apply.transaction を書かない宣言の confirm-timeout（0 なら
	// core.DefaultTxnTimeout）。確かめはその半分まで。
	Timeout time.Duration
}

// Provider は netconf の provider。
type Provider struct {
	hosts map[string]*host
	opt   Options

	mu sync.Mutex
	// subtrees はホスト → 部分木の名前 → 部分木（Validate で範囲から受け取る）。
	subtrees map[string]map[string]*subtree
	// decl は宣言の比べ方（Validate で受け取る）。
	decl map[core.ID]*decl
}

var (
	_ core.Provider   = (*Provider)(nil)
	_ core.Validator  = (*Provider)(nil)
	_ core.Transactor = (*Provider)(nil)
)

// host は 1 台の機器へのつなぎ方。観測はいつも新しいセッションを開いて閉じる
// （使い回さない：確定の前の確かめが新しい接続で届くことを見る、E-B8。コアは
// provider を閉じないので、開いたままのセッションを残さない）。
type host struct {
	name    string
	observe Dialer
	write   Dialer
}

// subtree は範囲の部分木 1 つ。
type subtree struct {
	name    string
	filter  string
	fnodes  []*node
	top     []nameKey
	ordered orderedSet
}

type nameKey struct{ space, local string }

// decl は宣言 1 件の比べ方。
type decl struct {
	raw   string
	want  []*node
	attrs attrSet
	canon string
}

// New は機器のつなぎ方から provider を作る（テスト・自前のバイナリ用）。
func New(hosts map[string]Host, opt Options) (*Provider, error) {
	if len(hosts) == 0 {
		return nil, errors.New("netconf: hosts に機器が無い")
	}
	p := &Provider{hosts: map[string]*host{}, opt: opt, subtrees: map[string]map[string]*subtree{}, decl: map[core.ID]*decl{}}
	for name, h := range hosts {
		if h.Observe == nil {
			return nil, fmt.Errorf("netconf: host %s のつなぎ方が無い", name)
		}
		w := h.Write
		if w == nil {
			w = h.Observe
		}
		p.hosts[name] = &host{name: name, observe: h.Observe, write: w}
	}
	return p, nil
}

// Module は settings から provider を作る（load.Module）。
func Module(settings map[string]any) (core.Provider, error) {
	var opt Options
	for k, v := range settings {
		switch k {
		case "hosts":
		case "apply":
			b, ok := v.(bool)
			if !ok {
				return nil, errors.New("settings.apply に true か false を書く")
			}
			opt.Apply = b
		case "timeout":
			str, _ := v.(string)
			d, err := time.ParseDuration(str)
			if err != nil || d < 2*time.Second {
				return nil, errors.New(`settings.timeout に 2s 以上の長さを書く（例 "60s"）`)
			}
			opt.Timeout = d
		case "commit_flow":
			switch v {
			case "1.0":
			case "1.1":
				opt.Persist = true
			default:
				return nil, errors.New(`settings.commit_flow は "1.0" か "1.1"`)
			}
		default:
			return nil, fmt.Errorf("settings に知らない項目 %q（書ける項目: apply, commit_flow, hosts, timeout）", k)
		}
	}
	hs, err := hostexec.ParseHosts(settings["hosts"], "write")
	if err != nil {
		return nil, err
	}
	hosts := map[string]Host{}
	for name, h := range hs {
		if h.Runner == nil {
			return nil, fmt.Errorf("settings.hosts.%s に ssh を書く", name)
		}
		if _, ok := h.Runner.(hostexec.SSH); !ok {
			return nil, fmt.Errorf("settings.hosts.%s: netconf は ssh のつなぎ方だけ（local は使えない）", name)
		}
		hd := Host{Observe: sshDialer(h.Runner)}
		if w, ok := h.Extra["write"]; ok {
			wh, err := hostexec.ParseHosts(map[string]any{name: w})
			if err != nil {
				return nil, fmt.Errorf("settings.hosts.%s.write: %w", name, err)
			}
			wr := wh[name].Runner
			if _, ok := wr.(hostexec.SSH); !ok {
				return nil, fmt.Errorf("settings.hosts.%s.write に ssh を書く", name)
			}
			hd.Write = sshDialer(wr)
		}
		hosts[name] = hd
	}
	return New(hosts, opt)
}

func sshDialer(r hostexec.Runner) Dialer {
	return func(ctx context.Context) (io.ReadWriteCloser, error) {
		return sshstream.Subsystem(ctx, r, Subsystem)
	}
}

// Schema は部分木の項目。
func (p *Provider) Schema() core.Schema {
	return core.Schema{Version: Version, Kinds: map[string]core.KindSchema{
		Kind: {
			Fields: map[string]core.FieldSpec{
				FieldConfig:       {Type: core.String},
				FieldCapabilities: {Type: core.Set},
			},
			ObserveOnly: !p.opt.Apply,
		},
	}}
}

var subtreeName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

// newSubtree は名前と filter から部分木を作り、検査する。
func newSubtree(name, filter string) (*subtree, error) {
	if !subtreeName.MatchString(name) {
		return nil, fmt.Errorf("部分木の名前 %q は %s に合わせる", name, subtreeName)
	}
	top, err := parseXML([]byte(filter))
	if err != nil {
		return nil, fmt.Errorf("部分木 %s の filter が XML でない: %v", name, err)
	}
	if len(top) == 0 {
		return nil, fmt.Errorf("部分木 %s の filter に要素が無い", name)
	}
	st := &subtree{name: name, filter: strings.TrimSpace(filter), fnodes: top, ordered: orderedSet{}}
	for _, n := range top {
		if n.name.Space == "" {
			// 名前空間の無い subtree filter はすべての名前空間に当たる（RFC 6241 §6.2.1）。
			return nil, fmt.Errorf("部分木 %s の filter の <%s> に名前空間（xmlns）を書く", name, n.name.Local)
		}
		st.top = append(st.top, nameKey{n.name.Space, n.name.Local})
	}
	return st, nil
}

// coords は ID の名前を host と部分木に分ける。
func coords(id core.ID) (string, string, error) {
	h, s, ok := strings.Cut(id.Name, "/")
	if !ok || h == "" || s == "" || strings.Contains(s, "/") {
		return "", "", fmt.Errorf("id %q は <host>/<部分木>", id.Name)
	}
	return h, s, nil
}

// Validate は範囲（機器・部分木の filter・ordered）と宣言を確かめ、覚える
// （E-B7・E-B8・E-B10・E-I6・E-I7・E-K8）。
func (p *Provider) Validate(res []core.Resource, ranges []core.Range) []core.Diagnostic {
	var diags []core.Diagnostic
	rej := func(o core.Origin, target, format string, a ...any) {
		diags = append(diags, core.Diagnostic{Severity: core.Reject, Origin: o, Target: target, Message: fmt.Sprintf(format, a...)})
	}
	subtrees := map[string]map[string]*subtree{}
	for _, rg := range ranges {
		h := rg.Args["host"]
		if p.hosts[h] == nil {
			rej(rg.Origin, "ranges", "host %q がつなぎ方の設定の hosts に無い", h)
			continue
		}
		if rg.Prune {
			rej(rg.Origin, "ranges", "netconf は部分木を消さない（prune: true は書けない）")
		}
		if subtrees[h] == nil {
			subtrees[h] = map[string]*subtree{}
		}
		ordered := map[string]string{}
		n := 0
		for _, k := range sortedKeys(rg.Args) {
			v := rg.Args[k]
			switch {
			case k == "host":
			case strings.HasPrefix(k, "filter."):
				name := strings.TrimPrefix(k, "filter.")
				st, err := newSubtree(name, v)
				if err != nil {
					rej(rg.Origin, "ranges", "%v", err)
					continue
				}
				if subtrees[h][name] != nil {
					rej(rg.Origin, "ranges", "host %s の部分木 %s が 2 つの範囲に入る", h, name)
				}
				subtrees[h][name] = st
				n++
			case strings.HasPrefix(k, "ordered."):
				ordered[strings.TrimPrefix(k, "ordered.")] = v
			default:
				rej(rg.Origin, "ranges", "netconf の範囲の引数は host・filter.<部分木>・ordered.<部分木>（%q が書かれている）", k)
			}
		}
		if n == 0 {
			rej(rg.Origin, "ranges", "filter.<部分木> に観測する部分木の subtree filter を書く（推測しない）")
		}
		for name, v := range ordered {
			st := subtrees[h][name]
			if st == nil {
				rej(rg.Origin, "ranges", "ordered.%s: 同じ範囲に filter.%s が無い", name, name)
				continue
			}
			o, err := ParseOrdered(v)
			if err != nil {
				rej(rg.Origin, "ranges", "ordered.%s: %v", name, err)
				continue
			}
			st.ordered = o
		}
	}
	decls := map[core.ID]*decl{}
	for _, r := range res {
		target := r.ID.String()
		if r.ID.Kind() != Kind {
			rej(r.Origin, target, "netconf の種類は %s だけ", Kind)
			continue
		}
		h, name, err := coords(r.ID)
		if err != nil {
			rej(r.Origin, target, "%v", err)
			continue
		}
		st := subtrees[h][name]
		if st == nil {
			rej(r.Origin, target, "部分木 %s は host %s の範囲（ranges の filter.%s）に無い", name, h, name)
			continue
		}
		if r.Ensure == core.EnsureAbsent {
			rej(r.Origin, target, "netconf は部分木を消さない（ensure: absent は書けない）")
			continue
		}
		for k := range r.Fields {
			switch k {
			case FieldConfig:
			case FieldCapabilities:
				rej(r.Origin, target, "capabilities は観測だけの項目で宣言できない")
			default:
				rej(r.Origin, target, "知らない項目 %q（宣言できるのは config だけ）", k)
			}
		}
		d, err := declOf(st, r.Fields[FieldConfig])
		if err != nil {
			rej(r.Origin, target, "config: %v", err) // 中身は出さない
			continue
		}
		decls[r.ID] = d
	}
	p.mu.Lock()
	p.subtrees, p.decl = subtrees, decls
	p.mu.Unlock()
	return diags
}

// declOf は宣言の config を確かめ、比べ方を作る。
func declOf(st *subtree, v any) (*decl, error) {
	raw, ok := v.(string)
	if !ok {
		return nil, errors.New("部分木の中身の XML を文字列で書く")
	}
	want, err := parseXML([]byte(raw))
	if err != nil {
		return nil, fmt.Errorf("XML でない: %v", err)
	}
	if len(want) == 0 {
		return nil, errors.New("要素が無い（空の部分木は宣言できない）")
	}
	allowed := map[nameKey]bool{}
	for _, k := range st.top {
		allowed[k] = true
	}
	for _, n := range want {
		if !hasContent(n) {
			// 中身の無い要素は「部分木が無い」と観測する（#208）。
			return nil, fmt.Errorf("<%s> に中身が無い（中身の無い要素は部分木が無いと観測する）", n.name.Local)
		}
		if !allowed[nameKey{n.name.Space, n.name.Local}] {
			return nil, fmt.Errorf("<%s xmlns=%q> は部分木 %s の filter が選ばない", n.name.Local, n.name.Space, st.name)
		}
	}
	if err := noEditAttrs(want); err != nil {
		return nil, err
	}
	// filter の外を書く宣言は edit を組めない（送れば部分木の外を書く）。
	if _, err := buildEdit(st.fnodes, want, nil, st.ordered); err != nil {
		return nil, err
	}
	attrs := declaredAttrs(want)
	return &decl{raw: raw, want: want, attrs: attrs, canon: canonical(want, true, st.ordered)}, nil
}

// noEditAttrs は宣言に edit-config の属性（nc:operation・yang:insert など）が無い
// ことを確かめる：どう当てるかは provider が決める。
func noEditAttrs(nodes []*node) error {
	for _, n := range nodes {
		for _, a := range n.attrs {
			if a.Name.Space == BaseNS || a.Name.Space == YANGNS {
				return fmt.Errorf("<%s> に属性 %s がある（当て方は provider が決める。中身だけを書く）", n.name.Local, a.Name.Local)
			}
		}
		if err := noEditAttrs(n.children); err != nil {
			return err
		}
	}
	return nil
}

func (p *Provider) lookup(id core.ID) (*host, *subtree, *decl, error) {
	h, name, err := coords(id)
	if err != nil {
		return nil, nil, nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	ho := p.hosts[h]
	st := p.subtrees[h][name]
	if ho == nil || st == nil {
		return nil, nil, nil, fmt.Errorf("%s は範囲（ranges の host・filter）の外", id)
	}
	return ho, st, p.decl[id], nil
}

func unknown(note string) core.Observation {
	return core.Observation{Availability: core.Unknown, Note: note}
}

// readDeniedHint は get-config が access-denied で断られたときの説明。
const readDeniedHint = "機器が get-config を断った（access-denied）。利用者に get-config の実行と部分木の読みを許す（NACM、RFC 8341）"

// Observe は部分木を観測する（ホストごとに 1 本の新しいセッション、E-D8）。
func (p *Provider) Observe(ctx context.Context, ids []core.ID) (map[core.ID]core.Observation, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
	}
	out := map[core.ID]core.Observation{}
	type item struct {
		id core.ID
		st *subtree
		d  *decl
	}
	byHost := map[string][]item{}
	for _, id := range ids {
		ho, st, d, err := p.lookup(id)
		if err != nil {
			out[id] = unknown(err.Error())
			continue
		}
		byHost[ho.name] = append(byHost[ho.name], item{id, st, d})
	}
	for _, name := range sortedKeys(byHost) {
		items := byHost[name]
		s, err := p.hosts[name].open(ctx, p.hosts[name].observe)
		if err != nil {
			for _, it := range items {
				out[it.id] = unknown(err.Error())
			}
			continue
		}
		for _, it := range items {
			out[it.id] = observeWith(ctx, s, it.st, it.d)
		}
		closeQuietly(s)
	}
	return out, nil
}

// observeWith は開いたセッションで部分木を観測する。
func observeWith(ctx context.Context, s *Session, st *subtree, d *decl) core.Observation {
	data, err := s.GetConfig(ctx, st.filter)
	if err != nil {
		if IsAccessDenied(err) {
			return unknown(fmt.Sprintf("%v; %s", err, readDeniedHint))
		}
		return unknown(err.Error())
	}
	top, err := parseXML(data)
	if err != nil {
		return unknown(fmt.Sprintf("機器の返した data が XML でない: %v", err))
	}
	// 中身の無い要素（<configuration/>）は部分木が無い（#208）。
	top = contentNodes(top)
	if len(top) == 0 {
		return core.Observation{Availability: core.Absent}
	}
	var attrs attrSet
	if d != nil {
		attrs = d.attrs
	}
	got := onlyDeclaredAttrs(top, attrs)
	cfg := canonical(got, false, nil)
	if d != nil && canonical(got, true, st.ordered) == d.canon {
		cfg = d.raw // 意味が同じなら宣言の書き方で返す（E-I7）
	}
	caps := s.ShortCapabilities()
	capList := make([]any, len(caps))
	for i, c := range caps {
		capList[i] = c
	}
	return core.Observation{Availability: core.Present, Fields: core.Fields{FieldConfig: cfg, FieldCapabilities: capList}}
}

func (h *host) open(ctx context.Context, d Dialer) (*Session, error) {
	rw, err := d(ctx)
	if err != nil {
		return nil, err
	}
	return Open(ctx, rw)
}

func closeQuietly(s *Session) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.Close(ctx)
}

// List は範囲の部分木のうち機器に在るもの（宣言外の報告。消さない）。
func (p *Provider) List(ctx context.Context, r core.Range) (core.Listing, error) {
	name := r.Args["host"]
	p.mu.Lock()
	ho := p.hosts[name]
	sts := p.subtrees[name]
	p.mu.Unlock()
	if ho == nil {
		return core.Listing{}, fmt.Errorf("host %q が無い", name)
	}
	s, err := ho.open(ctx, ho.observe)
	if err != nil {
		return core.Listing{}, err
	}
	defer closeQuietly(s)
	var ids []core.ID
	for _, k := range sortedKeys(r.Args) {
		if !strings.HasPrefix(k, "filter.") {
			continue
		}
		st := sts[strings.TrimPrefix(k, "filter.")]
		if st == nil {
			return core.Listing{}, fmt.Errorf("部分木 %s を受け取っていない（Validate の前）", k)
		}
		o := observeWith(ctx, s, st, nil)
		switch o.Availability {
		case core.Present:
			ids = append(ids, core.ID{Type: r.Provider + "/" + Kind, Name: name + "/" + st.name})
		case core.Absent:
		default:
			return core.Listing{}, fmt.Errorf("host %s の部分木 %s を読めない: %s", name, st.name, o.Note)
		}
	}
	return core.Listing{IDs: ids, Complete: true}, nil
}

// Apply は apply.transaction を書かない宣言を 2 段で当てる：commit confirmed を送り、
// 新しい接続で読み直して宣言どおりなら確定し、timeout の半分までに確かめられなければ
// 戻す（v2 の既定の流れ、E-B8）。
func (p *Provider) Apply(ctx context.Context, w core.WriteContext, id core.ID, desired core.Fields, current core.Observation) error {
	if !p.opt.Apply {
		return fmt.Errorf("%s: netconf は観測だけ（settings の apply が無い）。何も当てていない", id)
	}
	ho, st, d, err := p.lookup(id)
	if err != nil {
		return err
	}
	if d == nil {
		if d, err = declOf(st, desired[FieldConfig]); err != nil {
			return fmt.Errorf("%s: config: %v", id, err)
		}
	}
	timeout := p.opt.Timeout
	if timeout <= 0 {
		timeout = core.DefaultTxnTimeout
	}
	t, err := p.Prepare(ctx, w, []core.Change{{ID: id, Desired: desired, Current: current}}, core.TxnOptions{Timeout: timeout})
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout / 2)
	last := ""
	for {
		o := core.Observation{Availability: core.Unknown}
		if s, err := ho.open(ctx, ho.observe); err != nil { // 新しい接続
			o.Note = err.Error()
		} else {
			o = observeWith(ctx, s, st, d)
			closeQuietly(s)
		}
		switch {
		case o.Availability == core.Present && o.Fields[FieldConfig] == d.raw:
			return t.Commit(ctx)
		case o.Availability == core.Present:
			last = "running の部分木が宣言どおりでない"
		default:
			last = "新しい接続で部分木を読めない（" + o.Availability.String() + " " + o.Note + "）"
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			note := "戻した"
			if err := t.Rollback(ctx); err != nil {
				note = err.Error()
			}
			return fmt.Errorf("%s: confirm-timeout %s の半分のうちに確かめられない（%s）。確定していない（%s）", id, timeout, last, note)
		}
		select {
		case <-ctx.Done():
		case <-time.After(applyPoll):
		}
	}
}

// applyPoll は Apply の確かめの間隔。
const applyPoll = 500 * time.Millisecond

// Delete は部分木を消さない（E-C3）。
func (p *Provider) Delete(_ context.Context, _ core.WriteContext, _ core.ID, _ core.Observation) error {
	return &core.RefuseError{Reason: "netconf は部分木を消さない"}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
