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

// Package clidevice は CLI でしか設定できない装置を扱う汎用の provider
// "cli-device"（Backlog #159、§29.1）。
//
// 管理 API に設定の口が無い装置（syslog の宛先・ソフトの更新など）に、ssh 越しの
// 実行の層（internal/remote、#130）で CLI を打って観測し、コマンド列で変える。
// 機種ごとの中身（どのコマンドで観測し、どう読み、どのコマンドで変えるか）は
// 機種の定義のファイル（definition.go）で与え、この本体は機種を知らない。
//
//	resources:
//	  - provider: cli-device
//	    id: sw1/syslog                  # <装置>/<定義の項目>
//	    fields: {host: 192.0.2.53, port: "514"}
//
// 設計判断:
//
//   - 標準階級（TrueObserve）。List も BulkObserve も持たず、Pruneable にしない：
//     管理の範囲は intent に書いた項目だけ（A3：範囲は intent、つなぎ方と機種の
//     定義は設定）。
//   - 出力が定義に合わない（読み方が当たらない・当たりが 2 つ以上・要る項目が欠ける・
//     エラーの印が出る・プロンプトが来ない）ときは観測できない（unknown）。absent
//     と取り違えない（§3）。absent は定義の observe.absent が当たったときだけ。
//   - Apply は 1 つの項目のコマンド列を順に流し、エラーの印が出たらそこで止める。
//     流す前に同じやりとりで観測し、出力が定義に合わなければ何も流さない。すでに
//     宣言どおりなら流さない。流した後は必ず観測し直し、宣言どおりでなければ失敗
//     （TransientError で backoff。同じ失敗が PermanentAfter 回続けば PermanentError）。
//   - 部分的に失敗したとき：どのコマンドまで流れたかをエラー（Event・知らせ）に
//     出す。定義に確定・取り消し（config.commit・rollback）がある装置は取り消して
//     確定しない。無い装置は観測し直した現実を正として次の周で直す（level）。
//   - 秘密のフィールド（定義の secret: true）は SecretRef で宣言し、Apply の
//     直前に解決する（JIT、§20）。値はログ・エラー・plan・観測に出さない：観測は
//     「このプロセスが書いた値と同じなら、その SecretRef」だけを返す（値は覚えず、
//     プロセスごとの鍵の HMAC だけを覚える）。起こし直した後の最初の Apply は、
//     装置の値が宣言の値と同じなら書かずに覚えるだけ。エラーに載せる出力の行・
//     コマンドからは秘密の値を伏せる。
//   - Apply を破壊的として予算（§13）に数えるかは項目ごとに定義（disruptive）で書く。
//   - 一度きりの操作（provider.Actioner、Workflow の action、#139）：定義の
//     actions を同じ口で流す（ソフトの更新の手順など。承認ゲートつきの Workflow
//     から使う）。
//   - 1 つの装置へは同時に 1 つのやりとりだけ（装置の受けるセッションは少ない。
//     観測と設定が交ざらない）。
package clidevice

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/remote"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

const (
	// Name は provider の名前。
	Name = "cli-device"
	// ProviderVersion は観測の正規化の版（§10.4）。
	ProviderVersion = Name + "-v1"
	// FieldItem は項目の名前（identity。書くなら ID の項目と同じ）。
	FieldItem = "item"
	// DefaultPermanentAfter は同じ失敗が何回続けば PermanentError にするかの既定。
	DefaultPermanentAfter = 3
)

// Host は 1 つの装置: つなぎ方と機種の定義。
type Host struct {
	Conn Conn
	Def  *Definition
}

// Options は provider の設定。
type Options struct {
	// Secrets は秘密のフィールド・引数の SecretRef を解決する（nil なら解決できない）。
	Secrets core.SecretResolver
	// PermanentAfter は同じ Apply の失敗が何回続けば PermanentError にするか
	// （0 は DefaultPermanentAfter）。
	PermanentAfter int
}

type hostState struct {
	Host
	// lock は 1 つの装置へのやりとりを 1 つにする（ctx で待ちをやめられるよう
	// チャネル）。
	lock chan struct{}
}

type failure struct {
	sig string
	n   int
}

// Provider は cli-device の provider。
type Provider struct {
	hosts          map[string]*hostState
	secrets        core.SecretResolver
	permanentAfter int
	schema         provider.Schema
	key            []byte

	mu       sync.Mutex
	written  map[string]string // HMAC(値) → 書いた SecretRef（フィールドごと）
	failures map[string]failure
}

var (
	_ provider.Provider        = (*Provider)(nil)
	_ provider.IntentValidator = (*Provider)(nil)
	_ provider.AbsentValidator = (*Provider)(nil)
	_ provider.PlanRefiner     = (*Provider)(nil)
	_ provider.Actioner        = (*Provider)(nil)
)

// New は hosts の装置を扱う provider を作る。
func New(hosts map[string]Host, opts Options) (*Provider, error) {
	if len(hosts) == 0 {
		return nil, errors.New("cli-device: no device is named (cli_hosts)")
	}
	p := &Provider{hosts: map[string]*hostState{}, secrets: opts.Secrets, permanentAfter: opts.PermanentAfter,
		written: map[string]string{}, failures: map[string]failure{}}
	if p.permanentAfter <= 0 {
		p.permanentAfter = DefaultPermanentAfter
	}
	p.key = make([]byte, 32)
	if _, err := rand.Read(p.key); err != nil {
		return nil, err
	}
	fields := map[string]provider.FieldSpec{FieldItem: {Kind: core.FieldKindString, Identity: true}}
	owner := map[string]string{}
	for _, name := range sortedKeys(hosts) {
		h := hosts[name]
		if !remote.ValidHostName(name) {
			return nil, fmt.Errorf("cli-device: device %q: not a host name", name)
		}
		if h.Conn == nil || h.Def == nil {
			return nil, fmt.Errorf("cli-device: device %q: a connection and a device definition are required", name)
		}
		for _, in := range sortedKeys(h.Def.Items) {
			for fn, f := range h.Def.Items[in].Fields {
				if fn == FieldItem {
					return nil, fmt.Errorf("cli-device: %s: items.%s: the field name %q is reserved", h.Def.Name, in, fn)
				}
				spec := provider.FieldSpec{Kind: core.FieldKindString}
				if f.Secret {
					spec = provider.FieldSpec{Kind: core.FieldKindSecretRef, Secret: true}
				}
				if prev, ok := fields[fn]; ok && prev.Secret != spec.Secret {
					return nil, fmt.Errorf("cli-device: field %q is secret in %s but not in %s: a field name is secret in every definition or in none", fn, ownerOf(owner, fn, f.Secret, h.Def.Name), ownerOf(owner, fn, !f.Secret, h.Def.Name))
				}
				fields[fn] = spec
				owner[fn+fmt.Sprint(f.Secret)] = h.Def.Name
			}
		}
		p.hosts[name] = &hostState{Host: h, lock: make(chan struct{}, 1)}
	}
	p.schema = provider.Schema{Fields: fields}
	return p, nil
}

func ownerOf(owner map[string]string, fn string, secret bool, fallback string) string {
	if o, ok := owner[fn+fmt.Sprint(secret)]; ok {
		return o
	}
	return fallback
}

// Schema はフィールドの宣言（どの定義の項目のフィールドも合わせたもの）。
func (p *Provider) Schema() provider.Schema { return p.schema }

// Capabilities は標準階級：List も BulkObserve も無く、消さない（prune しない）。
func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{TrueObserve: true, ProviderVersion: ProviderVersion}
}

// locate は ID の装置と項目。
func (p *Provider) locate(id string) (*hostState, *Item, error) {
	host, item, ok := strings.Cut(id, "/")
	if !ok || host == "" || item == "" {
		return nil, nil, fmt.Errorf("want <device>/<item> (got %q)", id)
	}
	h, ok := p.hosts[host]
	if !ok {
		return nil, nil, fmt.Errorf("device %q is not in cli_hosts", host)
	}
	it, ok := h.Def.Items[item]
	if !ok {
		return nil, nil, fmt.Errorf("item %q is not in the device definition %s (%s)", item, h.Def.Name, strings.Join(sortedKeys(h.Def.Items), " | "))
	}
	return h, it, nil
}

// acquire は装置のやりとりの番を取る。
func (h *hostState) acquire(ctx context.Context) (func(), error) {
	select {
	case h.lock <- struct{}{}:
		return func() { <-h.lock }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 観測
// ─────────────────────────────────────────────────────────────────────────────

// observed は 1 回の観測の結果。
type observed struct {
	absent bool
	values map[string]string
}

// mismatch は出力が定義に合わない（観測できない）。
type mismatch struct{ msg string }

func (e *mismatch) Error() string { return e.msg }

// observe は開いたやりとりで項目を観測する。出力は文言に載せない（秘密が
// 含まれうる）。エラーの印の行だけは、秘密のフィールドの無い項目なら載せる。
func (p *Provider) observe(ctx context.Context, d *Definition, s session, it *Item) (observed, error) {
	out, err := s.do(ctx, it.Command)
	if err != nil {
		var cf *cmdFailed
		if errors.As(err, &cf) {
			return observed{}, &mismatch{fmt.Sprintf("the observe command %q %s", it.Command, cf.reason)}
		}
		return observed{}, err
	}
	if n, line, bad := d.errorLine(out); bad {
		msg := fmt.Sprintf("the output of %q has an error marker at line %d", it.Command, n)
		if !it.hasSecret() {
			msg += fmt.Sprintf(": %q", clip(line))
		}
		return observed{}, &mismatch{msg}
	}
	vals, found, err := it.parse.read(out)
	if err != nil {
		return observed{}, &mismatch{fmt.Sprintf("the output of %q does not match the device definition: %v", it.Command, err)}
	}
	gone := it.Absent != nil && it.Absent.MatchString(out)
	switch {
	case found && gone:
		return observed{}, &mismatch{fmt.Sprintf("the output of %q matches both the item and its absent pattern", it.Command)}
	case gone:
		return observed{absent: true}, nil
	case !found:
		return observed{}, &mismatch{fmt.Sprintf("the output of %q does not match the device definition (the item is not found, and the absent pattern does not match either)", it.Command)}
	}
	return observed{values: vals}, nil
}

func (it *Item) hasSecret() bool {
	for _, f := range it.Fields {
		if f.Secret {
			return true
		}
	}
	return false
}

// mac は秘密の値の HMAC（このプロセスだけの鍵。値は覚えない）。
func (p *Provider) mac(field, v string) string {
	h := hmac.New(sha256.New, p.key)
	h.Write([]byte(field))
	h.Write([]byte{0})
	h.Write([]byte(v))
	return hex.EncodeToString(h.Sum(nil))
}

func (p *Provider) state(it *Item, o observed) core.State {
	st := core.State{FieldItem: strVal(it.Name)}
	p.mu.Lock()
	defer p.mu.Unlock()
	for fn, v := range o.values {
		if it.Fields[fn].Secret {
			// 宣言に書かれたとおりの参照の文字列（Diff は参照の文字列をそのまま比べる）。
			if ref, ok := p.written[p.mac(fn, v)]; ok {
				st[fn] = strVal(ref)
			}
			continue
		}
		st[fn] = strVal(v)
	}
	return st
}

// Read は項目を観測する。desired を見ない（ADR-0002）。
func (p *Provider) Read(ctx context.Context, id string) (core.Observation, error) {
	h, it, err := p.locate(id)
	if err != nil {
		return unknownObs(), provider.NewPermanentError(fmt.Errorf("%s: Read %q: %w", Name, id, err))
	}
	release, err := h.acquire(ctx)
	if err != nil {
		return unknownObs(), provider.NewTransientError(fmt.Errorf("%s: Read %q: %w", Name, id, err))
	}
	defer release()
	s, err := h.Def.open(ctx, h.Conn)
	if err != nil {
		return unknownObs(), connError(fmt.Sprintf("%s: Read %q", Name, id), err)
	}
	o, err := p.observe(ctx, h.Def, s, it)
	s.close()
	if err != nil {
		return unknownObs(), connError(fmt.Sprintf("%s: Read %q", Name, id), err)
	}
	if o.absent {
		return core.Observation{Availability: core.AvailabilityOk, ObservedAt: time.Now()}, nil
	}
	return core.Observation{Availability: core.AvailabilityOk, State: p.state(it, o), ObservedAt: time.Now()}, nil
}

// connError は観測・やりとりの失敗を provider の error にする（どれも
// 観測できない。ホスト鍵の不一致だけは PermanentError）。
func connError(prefix string, err error) error {
	var ce *remote.ConnError
	if errors.As(err, &ce) {
		return remote.ProviderError(fmt.Errorf("%s: %w", prefix, err))
	}
	return provider.NewTransientError(fmt.Errorf("%s: %w", prefix, err))
}

// ─────────────────────────────────────────────────────────────────────────────
// 読み込みの検査・plan
// ─────────────────────────────────────────────────────────────────────────────

// ValidateIntent は書き方の誤りと、形に合わない値（コマンドへの差し込みになり
// うる値）を読み込みの時点で断る。値そのものは文言に出さない。
func (p *Provider) ValidateIntent(id string, fields map[string]string) error {
	_, it, err := p.locate(id)
	if err != nil {
		return fmt.Errorf("%s %q: %w", Name, id, err)
	}
	for _, k := range sortedKeys(fields) {
		if k == FieldItem {
			if fields[k] != it.Name {
				return fmt.Errorf("%s %q: item must equal the item in the id (%s)", Name, id, it.Name)
			}
			continue
		}
		if it.Fields[k] == nil {
			return fmt.Errorf("%s %q: unknown field %q (%s)", Name, id, k, strings.Join(sortedKeys(it.Fields), " | "))
		}
	}
	for _, fn := range sortedKeys(it.Fields) {
		f := it.Fields[fn]
		v, ok := fields[fn]
		if !ok {
			return fmt.Errorf("%s %q: field %q is required (an item is declared as a whole)", Name, id, fn)
		}
		if f.Secret {
			if _, err := core.ParseSecretRef(v); err != nil {
				return fmt.Errorf("%s %q: %s is secret: write a SecretRef (secretref:file/<path>), never the value (§20)", Name, id, fn)
			}
			continue
		}
		if err := f.check(v); err != nil {
			return fmt.Errorf("%s %q: %w", Name, id, err)
		}
	}
	return nil
}

// ValidateAbsent は定義に消し方（unset）と、消えたことの観測（observe.absent）が
// ある項目だけ ensure: absent を受ける。
func (p *Provider) ValidateAbsent(id string) error {
	h, it, err := p.locate(id)
	if err != nil {
		return fmt.Errorf("%s %q: %w", Name, id, err)
	}
	if len(it.Unset) == 0 {
		return fmt.Errorf("%s %q: the device definition %s has no unset for item %s, so it cannot be declared absent", Name, id, h.Def.Name, it.Name)
	}
	return nil
}

// RefinePlan は定義の disruptive を Op の Destructive にし、秘密のフィールドの
// 差分を値なしで書く。I/O はしない。
func (p *Provider) RefinePlan(localID string, _ core.State, _ map[string]string, ops []core.Op) ([]core.Op, error) {
	_, it, err := p.locate(localID)
	if err != nil {
		return nil, provider.NewPermanentError(fmt.Errorf("%s: %q: %w", Name, localID, err))
	}
	out := make([]core.Op, 0, len(ops))
	for _, op := range ops {
		var notes []string
		for _, f := range op.Fields {
			switch fd := it.Fields[f.Field]; {
			case fd == nil:
			case fd.Secret:
				notes = append(notes, f.Field+" (secret) differs")
			default:
				notes = append(notes, fmt.Sprintf("%s %q → %q", f.Field, f.From, f.To))
			}
		}
		if it.Disruptive {
			op.Destructive = true
			notes = append(notes, "disruptive: counted as destructive")
		}
		if len(notes) > 0 {
			op.Diagnostic = fmt.Sprintf("resource %s: %s", op.Target, strings.Join(notes, "; "))
		}
		out = append(out, op)
	}
	return out, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// 適用
// ─────────────────────────────────────────────────────────────────────────────

// Apply は plan を適用する。
func (p *Provider) Apply(ctx context.Context, plan core.Plan) error {
	for _, op := range plan.Ops {
		if err := p.applyOp(ctx, op); err != nil {
			return err
		}
	}
	return nil
}

// secretValues は値を解決し、雛形に入れる値と伏せる値を返す。
type resolved struct {
	values map[string]string
	refs   map[string]string // 秘密のフィールド → SecretRef の文字列
	hide   []string          // 伏せる値
	mask   map[string]bool
}

// fill なら want に無いフィールドは飛ばす（Apply が観測の値で埋める）。
func (p *Provider) resolve(ctx context.Context, want map[string]string, fields map[string]*Field, fill bool) (resolved, error) {
	r := resolved{values: map[string]string{}, refs: map[string]string{}, mask: map[string]bool{}}
	for _, fn := range sortedKeys(fields) {
		f := fields[fn]
		v, ok := want[fn]
		if !ok {
			if fill {
				continue
			}
			return r, provider.NewPermanentError(fmt.Errorf("field %q has no value", fn))
		}
		if f.Secret {
			ref, err := core.ParseSecretRef(v)
			if err != nil {
				return r, provider.NewPermanentError(fmt.Errorf("%s is not a SecretRef", fn))
			}
			if p.secrets == nil {
				return r, provider.NewPermanentError(fmt.Errorf("%s: %s: no secret store is configured (secret_store in remote_config or the provider config)", fn, ref))
			}
			sv, err := p.secrets.ResolveValue(ctx, ref.Backend(), ref.Path(), ref.Generation())
			if err != nil {
				return r, provider.NewTransientError(fmt.Errorf("%s: resolve %s: cannot read the secret", fn, ref))
			}
			if err := f.check(sv); err != nil {
				return r, provider.NewPermanentError(fmt.Errorf("%s: the value of %s does not match the pattern of the device definition", fn, ref))
			}
			r.values[fn], r.refs[fn], r.mask[fn] = sv, strings.TrimSpace(v), true
			if sv != "" {
				r.hide = append(r.hide, sv)
			}
			continue
		}
		if err := f.check(v); err != nil {
			return r, provider.NewPermanentError(err)
		}
		r.values[fn] = v
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

// runner は開いたやりとりでコマンドを順に流し、どこまで流れたかを数える。
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
		return fmt.Errorf("%q: %s", shown, x.r.redact(err.Error()))
	}
	if n, line, bad := x.d.errorLine(out); bad {
		return fmt.Errorf("%q: error marker at line %d: %q", shown, n, x.r.redact(clip(line)))
	}
	return nil
}

// sequence はコマンドの列を流す（設定の入り方・確定・出方で囲む）。失敗したら
// 取り消し（あれば）を流し、どこまで流れたかを書いた error を返す。
// sentAny は装置に何か 1 つでも打ったか。
func (x runner) sequence(lines, shown []string, config bool) (sentAny bool, err error) {
	cfg := x.d.Config
	if config {
		for _, c := range cfg.Enter {
			sentAny = true
			if err := x.run(c, c); err != nil {
				return sentAny, fmt.Errorf("entering the configuration mode failed: %w; none of the %d commands ran", err, len(lines))
			}
		}
	}
	for i, c := range lines {
		sentAny = true
		if err := x.run(c, shown[i]); err != nil {
			msg := fmt.Sprintf("command %d of %d failed: %v; %s", i+1, len(lines), err, ranNote(shown[:i]))
			return sentAny, errors.New(msg + x.undo(config))
		}
	}
	if config {
		for _, c := range cfg.Commit {
			if err := x.run(c, c); err != nil {
				return sentAny, fmt.Errorf("all %d commands ran but the commit failed: %v%s", len(lines), err, x.undo(config))
			}
		}
		for _, c := range cfg.Leave {
			if err := x.run(c, c); err != nil {
				return sentAny, fmt.Errorf("leaving the configuration mode failed: %w", err)
			}
		}
	}
	return sentAny, nil
}

// undo は失敗の後始末: 取り消しがあれば取り消して（確定せずに）設定を出る。
// 無ければ設定を出るだけ（流れたところまでは装置に残る。観測し直した現実を
// 正として次の周で直す）。
func (x runner) undo(config bool) string {
	if !config {
		return "; the device has no rollback in its definition: what ran stays, and the next cycle observes it again (level)"
	}
	cfg := x.d.Config
	note := ""
	if len(cfg.Rollback) > 0 {
		note = "; rolled back (" + strings.Join(cfg.Rollback, ", ") + "), not committed"
		for _, c := range cfg.Rollback {
			if err := x.run(c, c); err != nil {
				note = fmt.Sprintf("; the rollback failed: %v", err)
				break
			}
		}
	} else {
		note = "; the device has no rollback in its definition: what ran stays, and the next cycle observes it again (level)"
	}
	for _, c := range cfg.Leave {
		_ = x.run(c, c)
	}
	return note
}

func ranNote(done []string) string {
	if len(done) == 0 {
		return "no command before it ran"
	}
	q := make([]string, len(done))
	for i, d := range done {
		q[i] = fmt.Sprintf("%q", d)
	}
	return fmt.Sprintf("ran %d before it: %s", len(done), strings.Join(q, ", "))
}

// equal は観測が宣言（解決した値）と同じか。違うフィールドの名前を返す。
func (r resolved) differs(o observed) []string {
	var out []string
	for _, fn := range sortedKeys(r.values) {
		if o.values[fn] != r.values[fn] {
			out = append(out, fn)
		}
	}
	return out
}

func (p *Provider) applyOp(ctx context.Context, op core.Op) error {
	id := op.Target.Local
	prefix := fmt.Sprintf("%s: Apply %q", Name, id)
	h, it, err := p.locate(id)
	if err != nil {
		return provider.NewPermanentError(fmt.Errorf("%s: %w", prefix, err))
	}
	del := op.Kind == core.OpKindDelete
	var cmds []Template
	var r resolved
	if del {
		if len(it.Unset) == 0 {
			return provider.NewPermanentError(fmt.Errorf("%s: the device definition has no unset for item %s", prefix, it.Name))
		}
		cmds = it.Unset
		r = resolved{values: map[string]string{}, mask: map[string]bool{}}
	} else {
		if len(it.Set) == 0 {
			return provider.NewPermanentError(fmt.Errorf("%s: item %s is observe only in the device definition %s (no set): change it by a Workflow action or a manual Task", prefix, it.Name, h.Def.Name))
		}
		cmds = it.Set
		want := map[string]string{}
		for _, f := range op.Fields {
			want[f.Field] = f.To
		}
		if r, err = p.resolve(ctx, want, it.Fields, true); err != nil {
			return wrap(prefix, err)
		}
	}

	release, err := h.acquire(ctx)
	if err != nil {
		return provider.NewTransientError(fmt.Errorf("%s: %w", prefix, err))
	}
	defer release()
	s, err := h.Def.open(ctx, h.Conn)
	if err != nil {
		return connError(prefix, errors.New(r.redact(err.Error())))
	}
	defer s.close()

	// 流す前に観測する：定義に合わなければ何もしない。宣言どおりなら流さない。
	before, err := p.observe(ctx, h.Def, s, it)
	if err != nil {
		return connError(prefix+": nothing was sent: the observation before the change failed", err)
	}
	// Op に無いフィールド（変わらないもの）は、いまの値で雛形を埋める（Diff は
	// 変わるフィールドだけを渡す）。秘密なら、それは宣言と同じ値として観測した
	// もの（このプロセスが書いた値）。装置から読んだ値も宣言の値と同じく定義の
	// 値の形で確かめ、合わなければ観測できない（定義に合わない出力）として何も
	// 流さない（コマンドへの差し込みを防ぐ。値は文言に出さない）。
	for _, fn := range sortedKeys(it.Fields) {
		if _, ok := r.values[fn]; ok || del {
			continue
		}
		v, ok := before.values[fn]
		if !ok {
			return provider.NewPermanentError(fmt.Errorf("%s: field %q has no value (the item is absent, and the plan does not carry it)", prefix, fn))
		}
		if err := it.Fields[fn].check(v); err != nil {
			return connError(prefix+": nothing was sent", &mismatch{fmt.Sprintf("the value read from the device for the unchanged field %s does not match the device definition, so it is not put into a command: %v", fn, err)})
		}
		r.values[fn] = v
		if it.Fields[fn].Secret {
			r.mask[fn] = true
			if v != "" {
				r.hide = append(r.hide, v)
			}
		}
	}
	if done := p.matches(it, r, before, del); done {
		p.ok(id)
		return nil
	}
	lines := make([]string, len(cmds))
	shown := make([]string, len(cmds))
	for i, t := range cmds {
		lines[i] = t.render(r.values, nil)
		shown[i] = t.render(r.values, r.mask)
	}

	x := runner{d: h.Def, s: s, r: r, ctx: ctx}
	config := len(h.Def.Config.Enter) > 0 || len(h.Def.Config.Commit) > 0
	if _, err := x.sequence(lines, shown, config); err != nil {
		return p.fail(id, fmt.Sprintf("%s: %s", prefix, r.redact(err.Error())))
	}

	// 流した後は必ず観測し直す。
	after, err := p.observe(ctx, h.Def, s, it)
	if err != nil {
		return p.fail(id, fmt.Sprintf("%s: all %d commands ran, but the observation afterwards failed: %s", prefix, len(lines), r.redact(err.Error())))
	}
	if !p.matches(it, r, after, del) {
		if del {
			return p.fail(id, fmt.Sprintf("%s: all %d commands ran, but the item is still there", prefix, len(lines)))
		}
		if after.absent {
			return p.fail(id, fmt.Sprintf("%s: all %d commands ran, but the item is observed absent", prefix, len(lines)))
		}
		var notes []string
		for _, fn := range r.differs(after) {
			if it.Fields[fn].Secret {
				notes = append(notes, fn+" (secret) differs")
			} else {
				notes = append(notes, fmt.Sprintf("%s is %q, want %q", fn, after.values[fn], r.values[fn]))
			}
		}
		return p.fail(id, fmt.Sprintf("%s: all %d commands ran, but the device does not show the declared values: %s", prefix, len(lines), strings.Join(notes, "; ")))
	}
	p.ok(id)
	return nil
}

// matches は観測が宣言どおりか。秘密の値が同じなら、その SecretRef を覚える
// （観測で返すため）。
func (p *Provider) matches(it *Item, r resolved, o observed, del bool) bool {
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

func (p *Provider) ok(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.failures, id)
}

// fail は Apply の失敗を数え、同じ失敗が permanentAfter 回続けば PermanentError に
// する（それまでは TransientError で backoff）。
func (p *Provider) fail(id, msg string) error {
	p.mu.Lock()
	f := p.failures[id]
	if f.sig == msg {
		f.n++
	} else {
		f = failure{sig: msg, n: 1}
	}
	p.failures[id] = f
	p.mu.Unlock()
	if f.n >= p.permanentAfter {
		return provider.NewPermanentError(fmt.Errorf("%s (the same failure %d times in a row)", msg, f.n))
	}
	return provider.NewTransientError(errors.New(msg))
}

func wrap(prefix string, err error) error {
	var pe *provider.PermanentError
	if errors.As(err, &pe) {
		return provider.NewPermanentError(fmt.Errorf("%s: %w", prefix, pe.Cause))
	}
	var te *provider.TransientError
	if errors.As(err, &te) {
		err = te.Cause
	}
	return provider.NewTransientError(fmt.Errorf("%s: %w", prefix, err))
}

// ─────────────────────────────────────────────────────────────────────────────
// 一度きりの操作（provider.Actioner、Workflow の action、#139）
// ─────────────────────────────────────────────────────────────────────────────

// actionOf は localID（"<装置>" か "<装置>/<項目>"）の装置と op の定義。
func (p *Provider) actionOf(localID, op string) (*hostState, *Action, error) {
	host, _, _ := strings.Cut(localID, "/")
	h, ok := p.hosts[host]
	if !ok {
		return nil, nil, fmt.Errorf("device %q is not in cli_hosts", host)
	}
	a, ok := h.Def.Actions[op]
	if !ok {
		names := sortedKeys(h.Def.Actions)
		if len(names) == 0 {
			return nil, nil, fmt.Errorf("the device definition %s has no actions", h.Def.Name)
		}
		return nil, nil, fmt.Errorf("the device definition %s has no action %q (%s)", h.Def.Name, op, strings.Join(names, " | "))
	}
	return h, a, nil
}

// PlanAction は引数を定義の形で確かめる（現実は見ない）。
func (p *Provider) PlanAction(localID, op string, args map[string]string) (provider.ActionPlan, error) {
	h, a, err := p.actionOf(localID, op)
	if err != nil {
		return provider.ActionPlan{}, err
	}
	for _, k := range sortedKeys(args) {
		if a.Args[k] == nil {
			return provider.ActionPlan{}, fmt.Errorf("action %s: unknown argument %q (%s)", op, k, strings.Join(sortedKeys(a.Args), " | "))
		}
	}
	for _, k := range sortedKeys(a.Args) {
		f := a.Args[k]
		v, ok := args[k]
		if !ok {
			return provider.ActionPlan{}, fmt.Errorf("action %s: argument %q is required", op, k)
		}
		if f.Secret {
			if _, err := core.ParseSecretRef(v); err != nil {
				return provider.ActionPlan{}, fmt.Errorf("action %s: %s is secret: write a SecretRef, never the value (§20)", op, k)
			}
			continue
		}
		if err := f.check(v); err != nil {
			return provider.ActionPlan{}, fmt.Errorf("action %s: %w", op, err)
		}
	}
	host, _, _ := strings.Cut(localID, "/")
	return provider.ActionPlan{Destructive: a.Destructive, Description: fmt.Sprintf("%s on %s (%s)", op, host, h.Def.Name)}, nil
}

// Action は操作のコマンド列を 1 回流す。つなげないときは何も送っていない
// （ErrActionNotSent）。
func (p *Provider) Action(ctx context.Context, localID, op string, args map[string]string) error {
	if _, err := p.PlanAction(localID, op, args); err != nil {
		return err
	}
	h, a, _ := p.actionOf(localID, op)
	prefix := fmt.Sprintf("%s: action %s on %q", Name, op, localID)
	r, err := p.resolve(ctx, args, a.Args, false)
	if err != nil {
		return fmt.Errorf("%w: %v", provider.ErrActionNotSent, wrap(prefix, err))
	}
	lines := make([]string, len(a.Commands))
	shown := make([]string, len(a.Commands))
	for i, t := range a.Commands {
		lines[i] = t.render(r.values, nil)
		shown[i] = t.render(r.values, r.mask)
	}
	release, err := h.acquire(ctx)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", provider.ErrActionNotSent, prefix, err)
	}
	defer release()
	s, err := h.Def.open(ctx, h.Conn)
	if err != nil {
		return fmt.Errorf("%w: %s: %s", provider.ErrActionNotSent, prefix, r.redact(err.Error()))
	}
	defer s.close()
	x := runner{d: h.Def, s: s, r: r, ctx: ctx}
	if sent, err := x.sequence(lines, shown, a.Config); err != nil {
		if !sent {
			return fmt.Errorf("%w: %s: %s", provider.ErrActionNotSent, prefix, r.redact(err.Error()))
		}
		return fmt.Errorf("%s: %s", prefix, r.redact(err.Error()))
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────

func unknownObs() core.Observation {
	return core.Observation{Availability: core.AvailabilityUnknown, ObservedAt: time.Now()}
}

func strVal(v string) core.TypedValue { return core.TypedValue{Kind: core.FieldKindString, Value: v} }

// Devices は装置の名前（並べたもの）。
func (p *Provider) Devices() []string {
	out := make([]string, 0, len(p.hosts))
	for n := range p.hosts {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
