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
// commit confirmed の 2 段で当てる provider "netconf"（Backlog #141・#175、
// §23.4、§29.1）。
//
// 通信は NETCONF over SSH（RFC 6241・6242）。ssh 越しの実行の層
// （internal/remote、#130）の ssh で netconf サブシステムを開き、区切り
// （1.0 の "]]>]]>"・1.1 の chunked）と XML はこのパッケージ（stdlib）で扱う。
//
// リソースは「設定の部分木」: ID は "<ホスト名>/<部分木の名前>"。どこ（subtree
// filter）は部分木の一覧（config.go、provider の側）に、中身（宣言する XML）は
// intent の config に書く。観測は <get-config source="running"> を filter で
// 絞って取る。
//
// 設計判断:
//
//   - 既定は観測だけ（Capabilities.ObserveOnly）。差分はずれの知らせ
//     （DriftWouldApply、plan の blocked_by observe_only）になるだけで、コアは
//     Apply を呼ばない（呼ばれても PermanentError）。設定で許したとき
//     （AllowApply、--provider-config の netconf_apply）だけ CommitConfirm を
//     宣言して ObserveOnly を外し、commit confirmed の 2 段で当てる（apply.go。
//     §23.4「CommitConfirm が無ければ Apply 禁止」）。機器が :candidate と
//     :confirmed-commit を持たなければ、その機器には当てない。
//   - List・BulkObserve は持たない（prune しない）。
//   - 比べ方は名前空間・空白・属性の順をそろえた正規形（canon.go）。属性は
//     宣言に書いたものだけを比べる（機器が付ける commit の時刻などの印は比べ
//     ない、#208）。兄弟の順は
//     既定で問わず、intent の ordered で名指しした道だけ順を比べる。機種固有の
//     設定の型（YANG）は持たない: 利用者が機器の YANG に沿った XML を書く。
//     観測の config は機器の順のままの正規形（見せる形）で、比べるのは
//     RefinePlan（両側を並べ替えた正規形）。
//   - 届かない・認証・ホスト鍵・hello が合わない・区切りが壊れた・rpc-error は
//     観測できない（unknown）。absent にしない（§3）。部分木が機器に無い
//     （<data> が空）のは観測の事実（absent）。get-config が access-denied
//     （権限が無い）なら unknown の PermanentError（やり直しても変わらない）。
//     中身の無い要素だけの <data>（空の <configuration/> を返す機器がある。
//     名前空間の有無を問わない）も absent（#208）。
//   - 部分木だけに絞ったユーザー（機器の側の NACM、#176）で動く: 観測は
//     部分木の filter の get-config だけで、部分木の外を読まない。ただし NACM は
//     読めないデータを黙って省く（RFC 8341 §3.4.5）ので、読むのを許されて
//     いない部分木は機器に無い（absent）と見分けられない。その部分木を Apply
//     すると機器が access-denied で断り、理由つきの PermanentError になる。
//   - セッションはホストごとに 1 本を使い回し、1 度に 1 つの rpc。使い回した
//     セッションが切れていたら（機器が idle で切った等）1 度だけ開き直す。
//     rpc-error ではセッションを捨てない。
//   - hello の capability（base:1.0・1.1、candidate、confirmed-commit:1.0・1.1、
//     validate など）は観測の capabilities（Computed）に記録する。2 段の Apply
//     は Apply のセッションの hello で道を選ぶ。
package netconf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/remote"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// Name は provider の名前。
const Name = "netconf"

// ProviderVersion は観測の正規化の版（§10.4）。正規形・フィールドを変えたら上げる。
const ProviderVersion = Name + "-v2"

// Subsystem は ssh のサブシステムの名前（RFC 6242 §3）。
const Subsystem = "netconf"

// フィールド。
const (
	// FieldHost はホストの名前（ID の前、identity）。
	FieldHost = "host"
	// FieldSubtree は部分木の名前（ID の後ろ、identity）。
	FieldSubtree = "subtree"
	// FieldConfig は部分木の中身の XML（宣言、必須）。観測は正規形。
	FieldConfig = "config"
	// FieldOrdered は順に意味のある列の道（"acl/aces/ace,..."）。比べ方の宣言で、
	// 機器には無い（RefinePlan が差分から落とす）。
	FieldOrdered = "ordered"
	// FieldCapabilities は hello の NETCONF の capability（短い名前を空白で
	// 区切る。Computed）。
	FieldCapabilities = "capabilities"
)

// DefaultTimeout は ctx に期限が無いときの 1 回の観測の上限。
const DefaultTimeout = 60 * time.Second

// Dialer はホストへの流れ（netconf サブシステム）を開く。
type Dialer func(ctx context.Context) (io.ReadWriteCloser, error)

// Provider は netconf の provider。
type Provider struct {
	hosts    map[string]*host
	subtrees map[string]Subtree
	// apply は 2 段の Apply の設定（nil なら観測だけ）。
	apply *ApplyConfig
}

var (
	_ provider.Provider        = (*Provider)(nil)
	_ provider.IntentValidator = (*Provider)(nil)
	_ provider.PlanRefiner     = (*Provider)(nil)
)

// host は 1 台の機器とのセッション。
type host struct {
	name string
	dial Dialer

	mu   sync.Mutex
	sess *Session

	// applyMu は 1 台に 1 度に 1 つの Apply（candidate は機器に 1 つ）。
	applyMu sync.Mutex
}

// New は dialers（ホストの名前 → Dialer）の機器の、subtrees の部分木を観測する
// provider を作る。
func New(dialers map[string]Dialer, subtrees map[string]Subtree) (*Provider, error) {
	if len(dialers) == 0 {
		return nil, errors.New("netconf: no host is named")
	}
	if len(subtrees) == 0 {
		return nil, errors.New("netconf: no subtree is declared")
	}
	p := &Provider{hosts: map[string]*host{}, subtrees: subtrees}
	for name, d := range dialers {
		if !remote.ValidHostName(name) {
			return nil, fmt.Errorf("netconf: host %q: not a host name", name)
		}
		p.hosts[name] = &host{name: name, dial: d}
	}
	return p, nil
}

// SSHDialers は remote の Executor（ssh）の netconf サブシステムを開く Dialer。
// ssh でない Executor は error。
func SSHDialers(execs map[string]remote.Executor) (map[string]Dialer, error) {
	out := map[string]Dialer{}
	for name, e := range execs {
		o, ok := e.(remote.SubsystemOpener)
		if !ok {
			return nil, fmt.Errorf("netconf: host %q cannot open an ssh subsystem", name)
		}
		out[name] = func(ctx context.Context) (io.ReadWriteCloser, error) {
			return o.OpenSubsystem(ctx, Subsystem)
		}
	}
	return out, nil
}

// Schema は部分木のフィールド。
func (p *Provider) Schema() provider.Schema {
	return provider.Schema{Fields: map[string]provider.FieldSpec{
		FieldHost:         {Kind: core.FieldKindString, Identity: true},
		FieldSubtree:      {Kind: core.FieldKindString, Identity: true},
		FieldConfig:       {Kind: core.FieldKindString, Required: true},
		FieldOrdered:      {Kind: core.FieldKindString},
		FieldCapabilities: {Kind: core.FieldKindString, Computed: true},
	}}
}

// Capabilities は既定で観測だけ（§23.4）: CommitConfirm が無いので Apply
// させない。2 段の Apply を許したときだけ CommitConfirm を宣言し、ObserveOnly
// を外す。どちらでも List も BulkObserve も無く、消さない。
func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		TrueObserve:     true,
		ObserveOnly:     p.apply == nil,
		CommitConfirm:   p.apply != nil,
		ProviderVersion: ProviderVersion,
	}
}

// splitID は "<ホスト>/<部分木>" を分ける。
func (p *Provider) splitID(id string) (*host, Subtree, error) {
	hn, sn, ok := strings.Cut(id, "/")
	if !ok || hn == "" || sn == "" {
		return nil, Subtree{}, fmt.Errorf("%s: id %q: want <host>/<subtree>", Name, id)
	}
	h, ok := p.hosts[hn]
	if !ok {
		return nil, Subtree{}, fmt.Errorf("%s: id %q: no such host (netconf_hosts)", Name, id)
	}
	st, ok := p.subtrees[sn]
	if !ok {
		return nil, Subtree{}, fmt.Errorf("%s: id %q: no such subtree (netconf_subtrees)", Name, id)
	}
	return h, st, nil
}

// Read は部分木 id を観測する。desired を見ない（ADR-0002）。
func (p *Provider) Read(ctx context.Context, id string) (core.Observation, error) {
	unknown := core.Observation{Availability: core.AvailabilityUnknown, ObservedAt: time.Now()}
	h, st, err := p.splitID(id)
	if err != nil {
		return unknown, provider.NewPermanentError(err)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
	}
	data, caps, err := h.getConfig(ctx, st.Filter)
	if err != nil {
		err = fmt.Errorf("%s: Read %q: %w", Name, id, err)
		var ce *remote.ConnError
		if errors.As(err, &ce) {
			return unknown, remote.ProviderError(err)
		}
		if IsAccessDenied(err) {
			// Retrying does not help: the user lacks the right (NACM, §29.1).
			return unknown, provider.NewPermanentError(fmt.Errorf("%w; %s", err, readDeniedHint))
		}
		return unknown, provider.NewTransientError(err)
	}
	obs := core.Observation{Availability: core.AvailabilityOk, ObservedAt: time.Now()}
	top, err := parseXML(data)
	if err != nil {
		return unknown, provider.NewTransientError(fmt.Errorf("%s: Read %q: the data is not XML: %v", Name, id, err))
	}
	// An element with nothing in it (e.g. <configuration/>) is absent (#208).
	top = contentNodes(top)
	if len(top) == 0 {
		return obs, nil // the subtree is absent on the device
	}
	data = []byte(canonical(top, false, nil))
	str := func(v string) core.TypedValue { return core.TypedValue{Kind: core.FieldKindString, Value: v} }
	obs.State = core.State{
		FieldHost:         str(h.name),
		FieldSubtree:      str(st.Name),
		FieldConfig:       str(string(data)),
		FieldCapabilities: str(strings.Join(caps, " ")),
	}
	return obs, nil
}

// readDeniedHint は get-config が access-denied で断られたときの説明。
const readDeniedHint = "the device refused the user's get-config (access-denied): permit the user to execute get-config and to read the subtree (NACM, RFC 8341)"

// getConfig は filter の部分木を取る。使い回したセッションが切れていたら
// 1 度だけ開き直す。
func (h *host) getConfig(ctx context.Context, filter string) ([]byte, []string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	reused := h.sess != nil
	for {
		if h.sess == nil {
			rw, err := h.dial(ctx)
			if err != nil {
				return nil, nil, err
			}
			s, err := Open(ctx, rw)
			if err != nil {
				return nil, nil, err
			}
			h.sess = s
		}
		data, err := h.sess.GetConfig(ctx, filter)
		if err == nil {
			return data, h.sess.ShortCapabilities(), nil
		}
		var re *RPCError
		if errors.As(err, &re) {
			return nil, nil, err // the session is fine; the request was refused
		}
		h.sess.Abort()
		h.sess = nil
		if !reused || ctx.Err() != nil {
			return nil, nil, err
		}
		reused = false
	}
}

// Close は開いているセッションを閉じる（close-session）。
func (p *Provider) Close() error {
	var errs []error
	for _, h := range p.hosts {
		h.mu.Lock()
		if h.sess != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := h.sess.Close(ctx); err != nil {
				errs = append(errs, fmt.Errorf("%s: close %s: %w", Name, h.name, err))
			}
			cancel()
			h.sess = nil
		}
		h.mu.Unlock()
	}
	return errors.Join(errs...)
}

// ValidateIntent は読み込みの時点で宣言を確かめる: ID の形・ホストと部分木が
// あること・config が XML として読めて、最上位の要素が filter の最上位の要素の
// どれかであること・ordered の書式。エラーに config の中身は出さない。
func (p *Provider) ValidateIntent(localID string, fields map[string]string) error {
	_, st, err := p.splitID(localID)
	if err != nil {
		return err
	}
	cfg, ok := fields[FieldConfig]
	if !ok {
		return fmt.Errorf("%s %q: config is required (the XML of the subtree's content)", Name, localID)
	}
	top, err := parseXML([]byte(cfg))
	if err != nil {
		return fmt.Errorf("%s %q: config is not XML: %v", Name, localID, err)
	}
	if len(top) == 0 {
		return fmt.Errorf("%s %q: config has no element (an empty subtree cannot be declared; the subtree being absent is not declared here)", Name, localID)
	}
	for _, n := range top {
		if !hasContent(n) {
			// An element with nothing in it is observed as absent (#208).
			return fmt.Errorf("%s %q: config: <%s> has nothing in it (an element without children or a value is observed as the subtree being absent; declare its content)", Name, localID, n.name.Local)
		}
	}
	allowed := map[nameKey]bool{}
	for _, k := range st.top {
		allowed[k] = true
	}
	for _, n := range top {
		if !allowed[nameKey{n.name.Space, n.name.Local}] {
			return fmt.Errorf("%s %q: config: <%s xmlns=%q> is not selected by the filter of subtree %s", Name, localID, n.name.Local, n.name.Space, st.Name)
		}
	}
	if err := noEditAttrs(top); err != nil {
		return fmt.Errorf("%s %q: config: %w", Name, localID, err)
	}
	if _, err := ParseOrdered(fields[FieldOrdered]); err != nil {
		return fmt.Errorf("%s %q: %w", Name, localID, err)
	}
	for _, f := range []string{FieldHost, FieldSubtree} {
		if v, ok := fields[f]; ok {
			want := strings.SplitN(localID, "/", 2)[0]
			if f == FieldSubtree {
				want = st.Name
			}
			if v != want {
				return fmt.Errorf("%s %q: %s %q does not match the id", Name, localID, f, v)
			}
		}
	}
	return nil
}

// noEditAttrs は宣言に edit-config の属性（nc:operation・yang:insert など）が
// 無いことを確かめる: どう当てるかは Apply が決める。
func noEditAttrs(nodes []*node) error {
	for _, n := range nodes {
		for _, a := range n.attrs {
			if a.Name.Space == BaseNS || a.Name.Space == YANGNS {
				return fmt.Errorf("<%s> has the attribute %s (how to edit is the provider's; declare the content only)", n.name.Local, a.Name.Local)
			}
		}
		if err := noEditAttrs(n.children); err != nil {
			return err
		}
	}
	return nil
}

// RefinePlan は比べ方の宣言を差分に当てる:
//
//   - ordered（機器に無い「どう比べるか」）の差分は落とす。ただし Apply を
//     許していて config も違うときは残す（Apply が順を並べ直し、宣言どおりかを
//     同じ比べ方で確かめるのに使う）。
//   - config は両側を並べ替えた正規形で比べ、同じなら差分を落とす。違えば
//     どの行が違うかを Diagnostic に書く。
func (p *Provider) RefinePlan(localID string, observed core.State, desired map[string]string, ops []core.Op) ([]core.Op, error) {
	ordered, err := ParseOrdered(desired[FieldOrdered])
	if err != nil {
		return nil, fmt.Errorf("%s %q: %w", Name, localID, err)
	}
	want, err := parseXML([]byte(desired[FieldConfig]))
	if err != nil {
		return nil, fmt.Errorf("%s %q: config is not XML: %v", Name, localID, err)
	}
	attrs := declaredAttrs(want)
	var out []core.Op
	for _, op := range ops {
		var fields []core.FieldDiff
		var orderedDiff *core.FieldDiff
		diag := ""
		for _, f := range op.Fields {
			switch f.Field {
			case FieldOrdered:
				od := f
				orderedDiff = &od
				continue
			case FieldConfig:
				// The core refines only a present resource (observed != nil).
				obsCfg, _ := observed[FieldConfig].Value.(string)
				got, err := parseXML([]byte(obsCfg))
				if err != nil {
					return nil, fmt.Errorf("%s %q: the observed config is not XML: %v", Name, localID, err)
				}
				// Only the attributes written in the declaration are compared:
				// the device's own marks (commit time, user) are not drift (#208).
				got = onlyDeclaredAttrs(got, attrs)
				if canonical(got, true, ordered) == canonical(want, true, ordered) {
					continue
				}
				diag = explain(localID, got, want, ordered)
			}
			fields = append(fields, f)
		}
		if p.apply != nil && orderedDiff != nil && len(fields) > 0 {
			fields = append(fields, *orderedDiff)
		}
		op.Fields = fields
		op.Diagnostic = diag
		out = append(out, op)
	}
	return out, nil
}

// explain はずれを「道 = 値」の行で説明する（機器にだけある行・宣言にだけある
// 行を、それぞれ頭から 3 つまで）。行が同じで属性だけが違えば、「道 @属性 =
// 値」の行で、どの要素のどの属性が違うかを書く（got は宣言に書いた属性だけに
// 絞ったもの、#208）。
func explain(localID string, got, want []*node, ordered orderedSet) string {
	g, w := leafLines(got, ordered), leafLines(want, ordered)
	onlyDevice, onlyDeclared := minus(g, w), minus(w, g)
	var b strings.Builder
	if len(onlyDevice) == 0 && len(onlyDeclared) == 0 {
		ga, wa := attrLines(got, ordered), attrLines(want, ordered)
		onlyDevice, onlyDeclared = minus(ga, wa), minus(wa, ga)
		if len(onlyDevice) > 0 || len(onlyDeclared) > 0 {
			fmt.Fprintf(&b, "%s %s: the attributes of the device's subtree differ from the declaration (%d attribute(s) only on the device, %d only declared; only the attributes written in the declaration are compared)",
				Name, localID, len(onlyDevice), len(onlyDeclared))
		} else {
			fmt.Fprintf(&b, "%s %s: the device's subtree differs from the declaration (0 line(s) only on the device, 0 only declared): the order of an ordered list differs",
				Name, localID)
		}
	} else {
		fmt.Fprintf(&b, "%s %s: the device's subtree differs from the declaration (%d line(s) only on the device, %d only declared)",
			Name, localID, len(onlyDevice), len(onlyDeclared))
	}
	for _, part := range []struct {
		label string
		lines []string
	}{{"device", onlyDevice}, {"declared", onlyDeclared}} {
		for i, l := range part.lines {
			if i == 3 {
				fmt.Fprintf(&b, "\n  %s: … (%d more)", part.label, len(part.lines)-3)
				break
			}
			if len(l) > 200 {
				l = l[:200] + "…"
			}
			fmt.Fprintf(&b, "\n  %s: %s", part.label, l)
		}
	}
	return b.String()
}

// minus は a から b を（重複を数えて）引いた残り（a の順）。
func minus(a, b []string) []string {
	count := map[string]int{}
	for _, x := range b {
		count[x]++
	}
	var out []string
	for _, x := range a {
		if count[x] > 0 {
			count[x]--
			continue
		}
		out = append(out, x)
	}
	return out
}
