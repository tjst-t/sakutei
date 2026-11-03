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

// Package ansible は専用の provider の無い雑多な設定を Ansible のモジュール 1 つで
// 覆う最小階級の provider（module 名 `ansible`、設計 §7.1 の ansible、E-E3・N-E5）。
// v2 の internal/ansible を移した。Ansible の「モジュール」だけを借り、playbook と
// 一回 push の実行モデルは借りない：いつ・何を流すかは収束の周が決め、どう
// 変えるかはモジュールが行う。playbook を手順の一段として流す（N-T3）のはこの
// provider でなく、コアの手順の `exec` の段（ansible-playbook を外のコマンドとして
// 走らせる）。
//
//	providers:                                   # つなぎ方の設定（つなぎ方だけ）
//	  ans:
//	    module: ansible
//	    settings:
//	      inventory: /etc/sakutei/ansible/inventory.ini   # 必須。絶対パス
//	      ansible: /opt/ansible/bin/ansible               # 任意。既定 "ansible"（PATH から）
//	      env: {ANSIBLE_CONFIG: /etc/sakutei/ansible/ansible.cfg}
//	      timeout: 5m                                     # 任意。1 回の上限（既定 5m）
//	      max_parallel: 4                                 # 任意。同時に走らせる数（既定 4）
//
//	resources:                                   # intent
//	  - type: ans/call
//	    id: motd-web1                            # 利用者が付ける名前
//	    fields:
//	      host: web1                             # inventory のホスト 1 台
//	      module: ansible.builtin.lineinfile     # FQCN
//	      args: {path: /etc/motd, line: managed by sakutei, create: true}
//	  - type: ans/call
//	    id: wipe-cache-web1
//	    fields:
//	      host: web1
//	      module: ansible.builtin.copy
//	      args: {dest: /var/cache/app/state, content: "reset\n"}
//	      destructive: true                      # 当てる変更を壊す変更として予算に数える
//
// 種類 call（ID は利用者が付ける名前）。項目：host（inventory のホスト名）・
// module（モジュールの FQCN）・args（モジュールの引数、Map）・destructive（Bool、
// 既定 false）。
//
// 決まり：
//
//   - 観測はモジュールを --check --diff で流す（宣言を見ないと観測できない縮退、
//     設計 §3.3 の DesiredObserver の場合）。コアは宣言を Observe に渡さないので、
//     Validate が宣言の項目を ID ごとに覚え、Observe がそれで check mode を流す
//     （guest-os の目印と同じ）。宣言の無い ID は観測できない（Unknown）（E-E3）。
//   - check mode が changed=false なら Present で、項目は宣言の写し（宣言どおり）。
//     changed=true なら Present で、項目 module だけを「<FQCN>（check mode で
//     changed）」にした写しを返す。比べるのは宣言した項目だけなので、plan は
//     module の Update（観測 → 宣言）として出し、ansible の --diff の差分は観測の
//     Note に置いて Classifier の理由（plan の「provider:」の行）として出す。
//     モジュールの中の今の状態は返さない（check mode は与えた引数に対して changed
//     かしか答えない）ので、項目ごとの差分は無い（E-E3）。
//   - Absent にしない。ansible に「無い」は無く、消すのはモジュールの引数
//     （state: absent など）で宣言する。ensure: absent は読み込みで断り、Delete は
//     断る（*core.RefuseError）（E-E3）。
//   - List と prune を持たない（「どの呼び出しが現実にあるか」を列挙できない）。
//     範囲は読み込みで断り、List は誤りを返す（E-E3）。
//   - 届かない（unreachable）・モジュールの失敗・check mode を持たないモジュール
//     （skipped。例 ansible.builtin.command）・ansible が走らない・時間切れ・
//     inventory に無いホストは Unknown。Absent にせず、当てない（不変条件 3、E-K1）。
//   - 適用は同じ呼び出しを通常の実行で流す（冪等なのはモジュール）。流す前に
//     host が inventory のちょうど 1 台を指すことを ansible --list-hosts で確かめる
//     （グループ名なら全員に流れてしまう。観測の check mode でも 2 台以上の結果は
//     Unknown にする）。失敗は ansible の知らせを添えて返す（E-E3）。
//   - 壊す変更の重さはコアには分からない（今の状態が無い）ので、利用者が
//     destructive: true で宣言した呼び出しの Update を Classifier が壊す変更と言う
//     （予算の壊す変更に数える、不変条件 9）。既定は壊さない（E-E3）。
//   - 原因の区別（意図の変更か手の変更か）は粗くなる：比べる今の状態が無く、
//     ずれは module の 1 項目に寄る（E-E3）。
//   - 秘密：check mode は適用と同じ引数で流す必要があるが、秘密の値は
//     WriteContext（Apply・Delete）の中でしか取り出せない。観測で値を使えないので、
//     args の中の秘密の参照（{secret: "<backend>:<在りか>"}）は読み込みで断る。
//     秘密は ansible の側（inventory の変数・ansible-vault）に置く（接続・become
//     の資格など）。値が引数に要るモジュールは専用の provider で扱う（E-E3・
//     不変条件 27）。観測で書き込み用の資格は読まない（E-G5）。
//   - 引数はコマンドラインに載せない。0600 の一時ファイルの extra vars
//     （-e @file）で渡し、文字列はテンプレートとして評価しない（!unsafe。
//     "{{ … }}" はそのまま渡る）。結果は自前の stdout callback（callback/
//     sakutei_json.py を埋め込む）の 1 行の JSON で読む。シェルは通さない。
//     時間切れはプロセスグループごと（ansible が開いた ssh も）止める（E-E3）。
//   - 観測は 1 回ごとに ansible の起動（Python・接続・モジュールの転送）で高い。
//     同時に走らせる数は max_parallel で抑える（観測と適用で分け合う）。
//   - settings はつなぎ方だけ（inventory・ansible・env・timeout・max_parallel）。
//     settings は設定ファイルの場所を知らないので、inventory と、/ を含む ansible
//     は絶対パスで書く。env で provider が決める変数（ANSIBLE_STDOUT_CALLBACK
//     など）は変えられない。
package ansible

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
)

// Version は Schema の版。比べ方・観測の形を変えたら上げる（E-J12）。
const Version = "ansible/1"

// Kind は種類の名前。
const Kind = "call"

// 項目。
const (
	FieldHost        = "host"
	FieldModule      = "module"
	FieldArgs        = "args"
	FieldDestructive = "destructive"
)

// ChangedMark は check mode が changed と答えたとき、観測の module の値の後ろに
// 付ける印（宣言の FQCN と違う値にして、plan に Update として出す）。
const ChangedMark = "（check mode で changed）"

// 既定値。
const (
	DefaultTimeout     = 5 * time.Minute
	DefaultMaxParallel = 4
)

// maxDiffLines・maxDiffBytes は観測の Note に載せる差分の上限。
const (
	maxDiffLines = 40
	maxDiffBytes = 4000
)

var (
	// validHost は inventory のホスト名。ansible のパターンの記号（* ? , : ! & [ ] ~ など）は
	// 受けない（1 台だけを指す。IPv6 などは inventory の別名で書く）。
	validHost = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
	// validModule はモジュールの FQCN（<namespace>.<collection>.<module>）。
	validModule = regexp.MustCompile(`^[A-Za-z0-9_]+(\.[A-Za-z0-9_]+){2,}$`)
	// validArgKey はモジュールの引数の名前（-a のテンプレートにそのまま埋める）。
	validArgKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// groupNames は ansible が必ず持つグループ（ホストではない）。
	groupNames = map[string]bool{"all": true, "ungrouped": true}
)

// Provider は ansible の provider。
type Provider struct {
	runner Runner
	slots  chan struct{}
	mu     sync.Mutex
	// decls は Validate が覚えた ID（名前）ごとの宣言の項目（正規形の写し）。
	decls map[string]core.Fields
}

var (
	_ core.Provider   = (*Provider)(nil)
	_ core.Validator  = (*Provider)(nil)
	_ core.Classifier = (*Provider)(nil)
)

// New は runner を使う provider を作る（テスト・自前のバイナリ用）。maxParallel
// が 0 以下なら DefaultMaxParallel。
func New(runner Runner, maxParallel int) *Provider {
	if maxParallel <= 0 {
		maxParallel = DefaultMaxParallel
	}
	return &Provider{runner: runner, slots: make(chan struct{}, maxParallel), decls: map[string]core.Fields{}}
}

// Settings は settings を読んだもの（つなぎ方だけ）。
type Settings struct {
	Ansible     string
	Inventory   string
	Env         map[string]string
	Timeout     time.Duration
	MaxParallel int
}

// ParseSettings は settings を読む。
func ParseSettings(settings map[string]any) (Settings, error) {
	s := Settings{Timeout: DefaultTimeout, MaxParallel: DefaultMaxParallel}
	keys := make([]string, 0, len(settings))
	for k := range settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := settings[k]
		switch k {
		case "inventory", "ansible":
			str, ok := v.(string)
			if !ok || strings.TrimSpace(str) == "" {
				return Settings{}, fmt.Errorf("settings.%s は文字列で書く", k)
			}
			str = strings.TrimSpace(str)
			if k == "inventory" {
				if !filepath.IsAbs(str) {
					return Settings{}, fmt.Errorf("settings.inventory は絶対パスで書く（settings は設定ファイルの場所を知らない）: %q", str)
				}
				s.Inventory = str
				continue
			}
			if strings.ContainsRune(str, '/') && !filepath.IsAbs(str) {
				return Settings{}, fmt.Errorf("settings.ansible は名前だけ（PATH から探す）か絶対パスで書く: %q", str)
			}
			s.Ansible = str
		case "env":
			m, ok := v.(map[string]any)
			if !ok {
				return Settings{}, errors.New("settings.env は変数の名前 → 値のマップで書く")
			}
			s.Env = make(map[string]string, len(m))
			for name, val := range m {
				for _, f := range forcedEnv {
					if name == f {
						return Settings{}, fmt.Errorf("settings.env.%s は provider が決める（自前の stdout callback で結果を読む）ので変えられない", name)
					}
				}
				if name == "" || strings.ContainsAny(name, "=\x00") {
					return Settings{}, fmt.Errorf("settings.env: %q は変数の名前でない", name)
				}
				str, isStr := val.(string)
				if !isStr {
					return Settings{}, fmt.Errorf("settings.env.%s は文字列で書く", name)
				}
				s.Env[name] = str
			}
		case "timeout":
			str, _ := v.(string)
			d, err := time.ParseDuration(str)
			if err != nil || d <= 0 {
				return Settings{}, errors.New("settings.timeout は正の長さ（5m など）で書く")
			}
			s.Timeout = d
		case "max_parallel":
			n, ok := wholeNumber(v)
			if !ok || n < 1 {
				return Settings{}, errors.New("settings.max_parallel は 1 以上の整数で書く")
			}
			s.MaxParallel = n
		default:
			return Settings{}, fmt.Errorf("settings に知らない項目 %q（書ける項目: ansible, env, inventory, max_parallel, timeout。呼び出しは Resource の項目に書く）", k)
		}
	}
	if s.Inventory == "" {
		return Settings{}, errors.New("settings.inventory を書く（ansible の -i に渡す inventory のファイル、絶対パス）")
	}
	return s, nil
}

func wholeNumber(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n == math.Trunc(n) && !math.IsInf(n, 0) {
			return int(n), true
		}
	}
	return 0, false
}

// Module は settings から provider を作る（load.Module）。
func Module(settings map[string]any) (core.Provider, error) {
	s, err := ParseSettings(settings)
	if err != nil {
		return nil, err
	}
	return New(ExecRunner{Path: s.Ansible, Inventory: s.Inventory, Env: s.Env, Timeout: s.Timeout}, s.MaxParallel), nil
}

// Schema は種類 call（E-E3）。host と module を identity にしない（作り直しは
// 削除を伴うが、この provider は消さない。違う呼び出しは違う宣言として書く）。
func (p *Provider) Schema() core.Schema {
	str := core.FieldSpec{Type: core.String}
	return core.Schema{Version: Version, Kinds: map[string]core.KindSchema{
		Kind: {Fields: map[string]core.FieldSpec{
			FieldHost: str, FieldModule: str,
			FieldArgs:        {Type: core.Map},
			FieldDestructive: {Type: core.Bool},
		}},
	}}
}

// call は宣言の項目を読んだもの。
type call struct {
	host, module string
	args         map[string]any
	destructive  bool
}

// callOf は宣言の項目（正規形）を読む。誤りの文に args の値は出さない。
func callOf(f core.Fields) (call, error) {
	host, _ := f[FieldHost].(string)
	module, _ := f[FieldModule].(string)
	c := call{host: host, module: module}
	switch {
	case host == "":
		return call{}, errors.New("host を書く（inventory のホスト名）")
	case groupNames[host] || !validHost.MatchString(host):
		return call{}, fmt.Errorf("host %q は inventory のホスト 1 台の名前で書く（パターン・グループは書けない）", host)
	case module == "":
		return call{}, errors.New("module を書く（FQCN、例 ansible.builtin.lineinfile）")
	case !validModule.MatchString(module):
		return call{}, fmt.Errorf("module %q は FQCN（<namespace>.<collection>.<module>、例 ansible.builtin.lineinfile）で書く", module)
	}
	if v, ok := f[FieldDestructive]; ok {
		b, isBool := v.(bool)
		if !isBool {
			return call{}, errors.New("destructive は true か false")
		}
		c.destructive = b
	}
	if v, ok := f[FieldArgs]; ok && v != nil {
		var m map[string]any
		switch x := v.(type) {
		case map[string]any:
			m = x
		case core.Fields:
			m = x
		default:
			return call{}, errors.New("args はモジュールの引数のマップで書く（例 {path: /etc/motd, line: hello}）")
		}
		for _, k := range sortedKeys(m) {
			if !validArgKey.MatchString(k) {
				return call{}, fmt.Errorf("args の引数の名前 %q は英字・数字・_ で書く（モジュールの引数）", k)
			}
		}
		if path, found := secretRefIn(m, FieldArgs); found {
			return call{}, fmt.Errorf("%s に秘密の参照を書けない（check mode の観測で値を取り出せない）。秘密は ansible の側（inventory の変数・ansible-vault）に置く", path)
		}
		c.args = m
	}
	return c, nil
}

// secretRefIn は v の中の秘密の参照（{secret: "<backend>:<在りか>"}）の場所を返す。
func secretRefIn(v any, at string) (string, bool) {
	switch x := v.(type) {
	case map[string]any:
		if s, ok := x["secret"].(string); ok && len(x) == 1 {
			if _, err := core.ParseSecretRef(s); err == nil {
				return at, true
			}
		}
		for _, k := range sortedKeys(x) {
			if p, ok := secretRefIn(x[k], at+"."+k); ok {
				return p, true
			}
		}
	case core.Fields:
		return secretRefIn(map[string]any(x), at)
	case []any:
		for i, e := range x {
			if p, ok := secretRefIn(e, fmt.Sprintf("%s[%d]", at, i)); ok {
				return p, true
			}
		}
	}
	return "", false
}

// Validate は宣言を確かめ、ID ごとの項目を覚える（Observe が check mode で流す、
// E-E3・E-M3）。範囲・ensure: absent は書けない（List と削除が無い）。
func (p *Provider) Validate(res []core.Resource, ranges []core.Range) []core.Diagnostic {
	var diags []core.Diagnostic
	rej := func(o core.Origin, target, format string, a ...any) {
		diags = append(diags, core.Diagnostic{Severity: core.Reject, Origin: o, Target: target, Message: fmt.Sprintf(format, a...)})
	}
	for _, rg := range ranges {
		rej(rg.Origin, "ranges", "ansible は範囲を持たない（List と prune が無い。消すのはモジュールの引数 state: absent などで宣言する）")
	}
	decls := map[string]core.Fields{}
	for _, r := range res {
		target := r.ID.String()
		if r.ID.Kind() != Kind {
			rej(r.Origin, target, "ansible の種類は %s だけ", Kind)
			continue
		}
		if strings.TrimSpace(r.ID.Name) == "" || strings.ContainsAny(r.ID.Name, " \t\r\n") {
			rej(r.Origin, target, "id は空白を含まない名前で書く（利用者が付ける名前）")
			continue
		}
		if r.Ensure == core.EnsureAbsent {
			rej(r.Origin, target, "ansible は消さない（ensure: absent は書けない。消すのはモジュールの引数 state: absent などで宣言する）")
			continue
		}
		if _, err := callOf(r.Fields); err != nil {
			rej(r.Origin, target, "%v", err)
			continue
		}
		decls[r.ID.Name] = copyFields(r.Fields)
	}
	if len(diags) == 0 {
		p.mu.Lock()
		p.decls = decls
		p.mu.Unlock()
	}
	return diags
}

// acquire は同時に走らせる ansible の枠を 1 つ取る。
func (p *Provider) acquire(ctx context.Context) (func(), error) {
	select {
	case p.slots <- struct{}{}:
		return func() { <-p.slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Observe は ID ごとにモジュールを check mode で流す（max_parallel まで同時に）。
// 誤りは ID ごとの Unknown にする（E-K1）。
func (p *Provider) Observe(ctx context.Context, ids []core.ID) (map[core.ID]core.Observation, error) {
	out := make(map[core.ID]core.Observation, len(ids))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, id := range ids {
		id := id
		wg.Add(1)
		go func() {
			defer wg.Done()
			o := p.observe(ctx, id)
			mu.Lock()
			out[id] = o
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out, nil
}

func (p *Provider) observe(ctx context.Context, id core.ID) core.Observation {
	unknown := func(format string, a ...any) core.Observation {
		return core.Observation{Availability: core.Unknown, Note: fmt.Sprintf(format, a...)}
	}
	if id.Kind() != Kind {
		return unknown("ansible に種類 %q は無い", id.Kind())
	}
	p.mu.Lock()
	decl, ok := p.decls[id.Name]
	p.mu.Unlock()
	if !ok {
		return unknown("宣言が無い（ansible は宣言の引数で check mode を流して観測するので、宣言の無い呼び出しは観測できない）")
	}
	c, err := callOf(decl)
	if err != nil {
		return unknown("%v", err)
	}
	release, err := p.acquire(ctx)
	if err != nil {
		return unknown("観測を待つあいだに止められた: %v", err)
	}
	res, err := p.runner.Run(ctx, Call{Host: c.host, Module: c.module, Args: c.args, Check: true})
	release()
	if err != nil {
		return unknown("%s を check mode で流せない: %v", c.module, err)
	}
	switch res.Status {
	case StatusOK:
	case StatusSkipped:
		return unknown("%s（%s）は check mode を持たないので観測できない: %s", c.module, c.host, orNone(res.Msg))
	case StatusUnreachable:
		return unknown("%s に届かない: %s", c.host, orNone(res.Msg))
	default:
		return unknown("%s（%s）が check mode で失敗した: %s", c.module, c.host, orNone(res.Msg))
	}
	f := copyFields(decl)
	if !res.Changed {
		return core.Observation{Availability: core.Present, Fields: f}
	}
	f[FieldModule] = c.module + ChangedMark
	note := "check mode で changed"
	if d := summarize(res.Diff); d != "" {
		note += "（ansible の --diff）:\n" + d
	} else {
		note += "（ansible の --diff は無い）"
	}
	return core.Observation{Availability: core.Present, Fields: f, Note: note}
}

// summarize は差分を Note に載せる長さに切る。
func summarize(diff string) string {
	d := strings.TrimRight(diff, "\n")
	if strings.TrimSpace(d) == "" {
		return ""
	}
	lines := strings.Split(d, "\n")
	cut := false
	if len(lines) > maxDiffLines {
		lines, cut = lines[:maxDiffLines], true
	}
	d = strings.Join(lines, "\n")
	if len(d) > maxDiffBytes {
		d, cut = strings.ToValidUTF8(d[:maxDiffBytes], ""), true
	}
	if cut {
		d += "\n…（以下略）"
	}
	return d
}

// Classify は check mode の差分を理由として出し、destructive: true の呼び出しの
// 変更を壊す変更と言う（§3.3 の Classifier、E-E3・不変条件 9）。現実に触れない。
func (p *Provider) Classify(id core.ID, current core.Observation, desired core.Fields) []core.Hint {
	if id.Kind() != Kind {
		return nil
	}
	var hints []core.Hint
	if current.Availability == core.Present && current.Note != "" {
		hints = append(hints, core.Hint{Field: FieldModule, Reason: strings.ReplaceAll(current.Note, "\n", "\n      ")})
	}
	if d, _ := desired[FieldDestructive].(bool); d {
		hints = append(hints, core.Hint{Field: FieldModule, Destructive: true,
			Reason: "destructive: true の呼び出し（利用者が壊す変更と宣言した）"})
	}
	return hints
}

// Apply は同じ呼び出しを通常の実行で流す（冪等なのはモジュール、E-E3）。流す前に
// host が inventory のちょうど 1 台を指すことを確かめる。
func (p *Provider) Apply(ctx context.Context, _ core.WriteContext, id core.ID, desired core.Fields, _ core.Observation) error {
	if id.Kind() != Kind {
		return fmt.Errorf("ansible に種類 %q は無い", id.Kind())
	}
	c, err := callOf(desired)
	if err != nil {
		return err
	}
	release, err := p.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	hosts, err := p.runner.Hosts(ctx, c.host)
	if err != nil {
		return err
	}
	if len(hosts) != 1 || hosts[0] != c.host {
		return fmt.Errorf("host %q は inventory の 1 台でない（%d 台に当たる: %s）。1 つの宣言は 1 台", c.host, len(hosts), strings.Join(hosts, ", "))
	}
	res, err := p.runner.Run(ctx, Call{Host: c.host, Module: c.module, Args: c.args})
	if err != nil {
		return err
	}
	switch res.Status {
	case StatusOK:
		return nil
	case StatusSkipped:
		return fmt.Errorf("%s（%s）が流されなかった（skipped）: %s", c.module, c.host, orNone(res.Msg))
	case StatusUnreachable:
		return fmt.Errorf("%s に届かない: %s", c.host, orNone(res.Msg))
	default:
		return fmt.Errorf("%s（%s）が失敗した: %s", c.module, c.host, orNone(res.Msg))
	}
}

// Delete は断る。消すのはモジュールの引数（state: absent など）で宣言する（E-E3）。
func (p *Provider) Delete(context.Context, core.WriteContext, core.ID, core.Observation) error {
	return &core.RefuseError{Reason: "ansible は消さない（消すのはモジュールの引数 state: absent などで宣言する）"}
}

// List は持たない（prune しない。範囲は Validate が断る、E-E3）。
func (p *Provider) List(context.Context, core.Range) (core.Listing, error) {
	return core.Listing{}, errors.New("ansible は List を持たない（どの呼び出しが現実にあるかを列挙できない）")
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "知らせなし"
	}
	return strings.TrimSpace(s)
}

// copyFields は項目の深い写し（観測の Fields を宣言と分ける）。
func copyFields(f core.Fields) core.Fields {
	out := make(core.Fields, len(f))
	for k, v := range f {
		out[k] = copyValue(v)
	}
	return out
}

func copyValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = copyValue(e)
		}
		return out
	case core.Fields:
		return map[string]any(copyFields(x))
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = copyValue(e)
		}
		return out
	}
	return v
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
