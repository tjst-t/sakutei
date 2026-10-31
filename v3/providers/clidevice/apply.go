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

package clidevice

// apply.go — 変え方：Apply・Delete（コマンド列）、2 段の Apply（core.Transactor）、
// 一度きりの操作（core.Invoker）。

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/sdk/hostexec"
)

// resolved は雛形に入れる値と、伏せる値。
type resolved struct {
	values map[string]string
	refs   map[string]string // 秘密のフィールド → 参照（世代つき）
	hide   []string          // 伏せる値
	mask   map[string]bool
}

func newResolved() resolved {
	return resolved{values: map[string]string{}, refs: map[string]string{}, mask: map[string]bool{}}
}

// resolve は宣言の値を確かめ、秘密は書き込みの文脈で取り出す（E-G2・E-G5）。値は
// 文言に出さない。
func resolve(ctx context.Context, w core.WriteContext, want core.Fields, fields map[string]*Field) (resolved, error) {
	r := newResolved()
	for _, fn := range sortedKeys(fields) {
		f := fields[fn]
		v, ok := want[fn]
		if !ok {
			return r, fmt.Errorf("フィールド %q に値が無い", fn)
		}
		if f.Secret {
			ref, err := core.Normalize(core.Secret, v)
			if err != nil {
				return r, fmt.Errorf("%s は秘密の参照でない", fn)
			}
			sv, err := w.Secret(ctx, ref)
			if err != nil {
				return r, fmt.Errorf("%s: 秘密を取り出せない: %v", fn, err)
			}
			if err := f.check(sv); err != nil {
				return r, fmt.Errorf("%s: 秘密の値が機種の定義の形に合わない", fn)
			}
			r.values[fn], r.refs[fn], r.mask[fn] = sv, ref.(string), true
			if sv != "" {
				r.hide = append(r.hide, sv)
			}
			continue
		}
		s, isStr := v.(string)
		if !isStr {
			return r, fmt.Errorf("%s は文字列で書く", fn)
		}
		if err := f.check(s); err != nil {
			return r, err
		}
		r.values[fn] = s
	}
	return r, nil
}

// redact は s の中の秘密の値を伏せる。
func (r resolved) redact(s string) string {
	for _, v := range r.hide {
		s = strings.ReplaceAll(s, v, "***")
	}
	return s
}

// differs は観測が宣言（取り出した値）と違うフィールドの名前。
func (r resolved) differs(o observed) []string {
	var out []string
	for _, fn := range sortedKeys(r.values) {
		if o.values[fn] != r.values[fn] {
			out = append(out, fn)
		}
	}
	return out
}

// runner は開いたやりとりでコマンドを順に流す。
type runner struct {
	d   *Definition
	s   session
	r   resolved
	ctx context.Context
}

// run は cmd を打ち、失敗（エラーの印・0 以外の終了・やりとりの失敗）を返す。
// shown は文言に載せる形（秘密は伏せる）。
func (x runner) run(cmd, shown string) error {
	out, err := x.s.do(x.ctx, cmd)
	if err != nil {
		var cf *cmdFailed
		if errors.As(err, &cf) {
			reason := cf.reason
			if n, line, bad := x.d.errorLine(cf.out); bad {
				reason += fmt.Sprintf("; error marker at line %d: %q", n, x.r.redact(clip(line)))
			}
			return fmt.Errorf("%q %s", shown, reason)
		}
		if hostexec.IsUnreachable(err) {
			return &unreachable{fmt.Sprintf("%q: %s", shown, x.r.redact(err.Error()))}
		}
		return fmt.Errorf("%q: %s", shown, x.r.redact(err.Error()))
	}
	if n, line, bad := x.d.errorLine(out); bad {
		return fmt.Errorf("%q: error marker at line %d: %q", shown, n, x.r.redact(clip(line)))
	}
	return nil
}

// unreachable は装置に届かず、コマンドを打てていない。
type unreachable struct{ msg string }

func (e *unreachable) Error() string { return e.msg }

// first は最初のコマンドが届かずに落ちたか（何も送れていない）。
func first(i int, err error) bool {
	var u *unreachable
	return i == 0 && errors.As(err, &u)
}

// lines は流すコマンド（そのままと、文言に載せる形）。
type lines struct{ raw, shown []string }

func (l *lines) add(t Template, r resolved) {
	l.raw = append(l.raw, t.render(r.values, nil))
	l.shown = append(l.shown, t.render(r.values, r.mask))
}

// sequence はコマンドの列を流す（config なら設定の入り方・確定・出方で囲む。
// commitWith があれば確定の代わりにそれを流す）。失敗したら取り消し（あれば）を流し、
// どこまで流れたかを書いた error を返す。sentAny は装置に何か 1 つでも打ったか。
func (x runner) sequence(l lines, config bool, commitWith []string) (sentAny bool, err error) {
	cfg := x.d.Config
	n := 0
	if config {
		for _, c := range cfg.Enter {
			err := x.run(c, c)
			sentAny = sentAny || !first(n, err)
			n++
			if err != nil {
				return sentAny, fmt.Errorf("設定に入れない: %w; %d 個のコマンドはどれも流していない", err, len(l.raw))
			}
		}
	}
	for i, c := range l.raw {
		err := x.run(c, l.shown[i])
		sentAny = sentAny || !first(n, err)
		n++
		if err != nil {
			msg := fmt.Sprintf("%d 個のうち %d 個目のコマンドが失敗した: %v; %s", len(l.raw), i+1, err, ranNote(l.shown[:i]))
			return sentAny, errors.New(msg + x.undo(config))
		}
	}
	if config {
		commit := cfg.Commit
		if commitWith != nil {
			commit = commitWith
		}
		for _, c := range commit {
			if err := x.run(c, c); err != nil {
				return sentAny, fmt.Errorf("%d 個のコマンドは流れたが確定に落ちた: %v%s", len(l.raw), err, x.undo(config))
			}
		}
		for _, c := range cfg.Leave {
			if err := x.run(c, c); err != nil {
				return sentAny, fmt.Errorf("設定を出られない: %w", err)
			}
		}
	}
	return sentAny, nil
}

// undo は失敗の後始末：取り消しがあれば取り消して（確定せずに）設定を出る。無ければ
// 設定を出るだけ（流れたところまでは装置に残る。次の周が観測し直す）。
func (x runner) undo(config bool) string {
	none := "; 機種の定義に取り消しが無い：流れたところは装置に残り、次の周が観測し直す"
	if !config {
		return none
	}
	cfg := x.d.Config
	note := none
	if len(cfg.Rollback) > 0 {
		note = "; 取り消した（" + strings.Join(cfg.Rollback, ", ") + "）。確定していない"
		for _, c := range cfg.Rollback {
			if err := x.run(c, c); err != nil {
				note = fmt.Sprintf("; 取り消しに落ちた: %v", err)
				break
			}
		}
	}
	for _, c := range cfg.Leave {
		_ = x.run(c, c)
	}
	return note
}

func ranNote(done []string) string {
	if len(done) == 0 {
		return "その前に流したコマンドは無い"
	}
	q := make([]string, len(done))
	for i, d := range done {
		q[i] = fmt.Sprintf("%q", d)
	}
	return fmt.Sprintf("その前に %d 個流した: %s", len(done), strings.Join(q, ", "))
}

// matches は観測が宣言どおりか。秘密の値が同じなら、その参照を覚える（観測で
// 返すため）。
func (p *Provider) matches(r resolved, o observed, del bool) bool {
	if del {
		return o.absent
	}
	if o.absent || len(r.differs(o)) > 0 {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for fn, ref := range r.refs {
		p.written[p.mac(fn, r.values[fn])] = ref
	}
	return true
}

// change は 1 つの項目の変更（Apply・Delete・Prepare）。
type change struct {
	id  core.ID
	it  *Item
	del bool
	r   resolved
}

func (p *Provider) changeOf(ctx context.Context, w core.WriteContext, id core.ID, desired core.Fields, del bool) (*hostState, change, error) {
	h, it, err := p.locate(id)
	if err != nil {
		return nil, change{}, err
	}
	c := change{id: id, it: it, del: del, r: newResolved()}
	if del {
		if len(it.Unset) == 0 {
			return nil, change{}, &core.RefuseError{Reason: fmt.Sprintf("機種の定義 %s の項目 %s に unset が無い", h.def.Name, it.Name)}
		}
		return h, c, nil
	}
	if len(it.Set) == 0 {
		return nil, change{}, fmt.Errorf("%s: 項目 %s は機種の定義 %s で観測だけ（set が無い）。手順の invoke か人の手で変える", id, it.Name, h.def.Name)
	}
	if c.r, err = resolve(ctx, w, desired, it.Fields); err != nil {
		return nil, change{}, fmt.Errorf("%s: %w", id, err)
	}
	return h, c, nil
}

// commands は変更のコマンド列。
func (c change) commands() []Template {
	if c.del {
		return c.it.Unset
	}
	return c.it.Set
}

// Apply は 1 つの項目を宣言どおりにする。
func (p *Provider) Apply(ctx context.Context, w core.WriteContext, id core.ID, desired core.Fields, _ core.Observation) error {
	h, c, err := p.changeOf(ctx, w, id, desired, false)
	if err != nil {
		return err
	}
	return p.write(ctx, h, c)
}

// Delete は項目を unset で消す。unset の無い項目は断る（E-F1）。
func (p *Provider) Delete(ctx context.Context, w core.WriteContext, id core.ID, _ core.Observation) error {
	h, c, err := p.changeOf(ctx, w, id, nil, true)
	if err != nil {
		return err
	}
	return p.write(ctx, h, c)
}

func (p *Provider) write(ctx context.Context, h *hostState, c change) error {
	prefix := c.id.String()
	release, err := h.acquire(ctx)
	if err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	defer release()
	s, err := h.def.open(ctx, h.write)
	if err != nil {
		return fmt.Errorf("%s: 何も流していない: %s", prefix, c.r.redact(err.Error()))
	}
	defer s.close()
	rd := &reader{d: h.def, s: s}

	// 流す前に観測する：定義に合わなければ何もしない。宣言どおりなら流さない。
	before, err := rd.observe(ctx, c.it)
	if err != nil {
		return fmt.Errorf("%s: 何も流していない：変える前の観測に落ちた: %v", prefix, err)
	}
	if p.matches(c.r, before, c.del) {
		return nil
	}
	var l lines
	for _, t := range c.commands() {
		l.add(t, c.r)
	}
	x := runner{d: h.def, s: s, r: c.r, ctx: ctx}
	config := len(h.def.Config.Enter) > 0 || len(h.def.Config.Commit) > 0
	if _, err := x.sequence(l, config, nil); err != nil {
		return fmt.Errorf("%s: %s", prefix, c.r.redact(err.Error()))
	}
	// 流した後は必ず観測し直す。
	after, err := rd.observe(ctx, c.it)
	if err != nil {
		return fmt.Errorf("%s: %d 個のコマンドは流れたが、あとの観測に落ちた: %s", prefix, len(l.raw), c.r.redact(err.Error()))
	}
	if !p.matches(c.r, after, c.del) {
		return fmt.Errorf("%s: %d 個のコマンドは流れたが、装置が宣言どおりでない: %s", prefix, len(l.raw), c.r.explain(c.it, after, c.del))
	}
	return nil
}

func (r resolved) explain(it *Item, o observed, del bool) string {
	switch {
	case del:
		return "項目がまだ在る"
	case o.absent:
		return "項目が無いと観測した"
	}
	var notes []string
	for _, fn := range r.differs(o) {
		if it.Fields[fn].Secret {
			notes = append(notes, fn+"（秘密）が違う")
		} else {
			notes = append(notes, fmt.Sprintf("%s は %q（宣言 %q）", fn, o.values[fn], r.values[fn]))
		}
	}
	return strings.Join(notes, "; ")
}

// ─────────────────────────────────────────────────────────────────────────────
// 2 段の Apply（core.Transactor、E-B8・不変条件 17）
// ─────────────────────────────────────────────────────────────────────────────

// Prepare は装置ごとに、設定に入ってコマンド列を流し、commit_confirmed（時間切れで
// 装置が戻す確定）で終える。どこかで落ちれば、準備した装置を戻す。
func (p *Provider) Prepare(ctx context.Context, w core.WriteContext, changes []core.Change, opt core.TxnOptions) (core.Txn, error) {
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = core.DefaultTxnTimeout
	}
	byHost := map[string][]change{}
	for _, ch := range changes {
		h, c, err := p.changeOf(ctx, w, ch.ID, ch.Desired, ch.Delete)
		if err != nil {
			return nil, err
		}
		if len(h.def.Config.CommitConfirmed) == 0 {
			return nil, fmt.Errorf("%s: 機種の定義 %s に config.commit_confirmed が無い（2 段の Apply を持たない）", ch.ID, h.def.Name)
		}
		byHost[h.name] = append(byHost[h.name], c)
	}
	t := &txn{p: p}
	for _, name := range sortedKeys(byHost) {
		h := p.hosts[name]
		ok, err := p.stage(ctx, h, byHost[name], timeout)
		if err != nil {
			_ = t.Rollback(ctx)
			return nil, fmt.Errorf("host %s: %w", name, err)
		}
		if ok {
			t.hosts = append(t.hosts, h)
		}
	}
	return t, nil
}

// stage は 1 台に変更と commit_confirmed を流す。どの項目も宣言どおりなら何も流さず
// false。
func (p *Provider) stage(ctx context.Context, h *hostState, cs []change, timeout time.Duration) (bool, error) {
	release, err := h.acquire(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	all := newResolved()
	for _, c := range cs {
		all.hide = append(all.hide, c.r.hide...)
	}
	s, err := h.def.open(ctx, h.write)
	if err != nil {
		return false, fmt.Errorf("何も流していない: %s", all.redact(err.Error()))
	}
	defer s.close()
	rd := &reader{d: h.def, s: s}
	var l lines
	for _, c := range cs {
		before, err := rd.observe(ctx, c.it)
		if err != nil {
			return false, fmt.Errorf("何も流していない：%s の変える前の観測に落ちた: %v", c.id, err)
		}
		if p.matches(c.r, before, c.del) {
			continue
		}
		for _, t := range c.commands() {
			l.add(t, c.r)
		}
	}
	if len(l.raw) == 0 {
		return false, nil
	}
	secs := int((timeout + time.Second - 1) / time.Second)
	tv := map[string]string{"seconds": strconv.Itoa(secs), "minutes": strconv.Itoa((secs + 59) / 60)}
	var commit []string
	for _, t := range h.def.Config.CommitConfirmed {
		commit = append(commit, t.render(tv, nil))
	}
	x := runner{d: h.def, s: s, r: all, ctx: ctx}
	if _, err := x.sequence(l, true, commit); err != nil {
		return false, errors.New(all.redact(err.Error()))
	}
	return true, nil
}

// txn は commit_confirmed を流した装置。
type txn struct {
	p     *Provider
	hosts []*hostState
}

// Commit は装置ごとに新しいやりとりで confirm を流す。
func (t *txn) Commit(ctx context.Context) error {
	return t.each(ctx, func(d *Definition) []string { return d.Config.Confirm })
}

// Rollback は cancel のある装置にそれを流す（無ければ装置の時間切れで戻る）。
func (t *txn) Rollback(ctx context.Context) error {
	return t.each(ctx, func(d *Definition) []string { return d.Config.Cancel })
}

func (t *txn) each(ctx context.Context, cmds func(*Definition) []string) error {
	var errs []string
	for _, h := range t.hosts {
		c := cmds(h.def)
		if len(c) == 0 {
			continue
		}
		if err := t.p.runConfig(ctx, h, c); err != nil {
			errs = append(errs, fmt.Sprintf("host %s: %v（装置は時間切れで戻るかもしれない）", h.name, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "・"))
	}
	return nil
}

// runConfig は設定に入って cmds を流し、出る。
func (p *Provider) runConfig(ctx context.Context, h *hostState, cmds []string) error {
	release, err := h.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	s, err := h.def.open(ctx, h.write)
	if err != nil {
		return err
	}
	defer s.close()
	x := runner{d: h.def, s: s, r: newResolved(), ctx: ctx}
	for _, c := range append(append(append([]string(nil), h.def.Config.Enter...), cmds...), h.def.Config.Leave...) {
		if err := x.run(c, c); err != nil {
			return err
		}
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// 一度きりの操作（core.Invoker、E-B11・E-A2）
// ─────────────────────────────────────────────────────────────────────────────

// Ops は項目 kind を持つ機種の定義の actions。
func (p *Provider) Ops(kind string) map[string]core.OpSpec {
	out := map[string]core.OpSpec{}
	for _, h := range p.hosts {
		if h.def.Items[kind] == nil {
			continue
		}
		for name, a := range h.def.Actions {
			spec := out[name]
			spec.Destructive = spec.Destructive || a.Destructive
			out[name] = spec
		}
	}
	return out
}

// Invoke は装置 id.Name で操作 op のコマンド列を 1 回流す。つなげない・1 つも
// コマンドを打てていないときだけ core.ErrNotSent（不変条件 16）。
func (p *Provider) Invoke(ctx context.Context, w core.WriteContext, id core.ID, op string, args core.Fields) error {
	h, ok := p.hosts[id.Name]
	if !ok {
		return fmt.Errorf("%w: 装置 %q が hosts に無い", core.ErrNotSent, id.Name)
	}
	a, ok := h.def.Actions[op]
	if !ok {
		return fmt.Errorf("%w: 機種の定義 %s に操作 %q が無い", core.ErrNotSent, h.def.Name, op)
	}
	for k := range args {
		if a.Args[k] == nil {
			return fmt.Errorf("%w: 操作 %s に知らない引数 %q", core.ErrNotSent, op, k)
		}
	}
	r, err := resolve(ctx, w, args, a.Args)
	if err != nil {
		return fmt.Errorf("%w: 操作 %s: %v", core.ErrNotSent, op, err)
	}
	var l lines
	for _, t := range a.Commands {
		l.add(t, r)
	}
	prefix := fmt.Sprintf("操作 %s（装置 %s）", op, h.name)
	release, err := h.acquire(ctx)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", core.ErrNotSent, prefix, err)
	}
	defer release()
	s, err := h.def.open(ctx, h.write)
	if err != nil {
		return fmt.Errorf("%w: %s: %s", core.ErrNotSent, prefix, r.redact(err.Error()))
	}
	defer s.close()
	x := runner{d: h.def, s: s, r: r, ctx: ctx}
	if sent, err := x.sequence(l, a.Config, nil); err != nil {
		if !sent {
			return fmt.Errorf("%w: %s: %s", core.ErrNotSent, prefix, r.redact(err.Error()))
		}
		return fmt.Errorf("%s: %s", prefix, r.redact(err.Error()))
	}
	return nil
}
