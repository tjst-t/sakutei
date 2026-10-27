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

// Package ansible は ansible provider（設計 §28.7 の 2 層 provider 戦略の
// ロングテール層、Backlog #127）。Ansible の「モジュール」だけを借り、playbook
// と一回 push の実行モデルは借りない：いつ・何を流すかは収束ループが決め、
// どう変えるかはモジュールが行う。
//
// リソースは「1 つのホストに対する 1 つのモジュールの呼び出し」。ID は利用者が
// 付ける名前で、フィールドは host（inventory のホスト名）・module（FQCN）・
// args（モジュールの引数。JSON のオブジェクト）・destructive（適用を破壊的として
// 予算に数えるか。既定 false）。
//
// 最小階級（§5.6）：
//
//   - 観測は provider.CheckReader：モジュールを --check --diff で流し、
//     changed=false なら適合、changed=true なら drift（ansible の差分を添える）。
//     State は返さない（Capabilities.TrueObserve=false、§28.7）。
//   - check mode を持たないモジュール（skipped）・届かない（unreachable）・失敗は
//     観測できない（Unknown。何もしない）。
//   - 適用は同じ呼び出しを通常の実行で。失敗は ansible の知らせを添えて返す。
//   - List を持たない（prune しない。open-world のみ）。Delete もしない（消すのは
//     モジュールの引数、例えば state: absent で書く）。
//
// ansible は Runner 越しにだけ呼ぶ（本物は ExecRunner、テストは ansiblefake）。
package ansible

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

const (
	// Name は provider 名。
	Name = "ansible"
	// ProviderVersion は diff / 正規化の版（§10.4）。
	ProviderVersion = "ansible-v1"
)

// フィールド名。
const (
	FieldHost        = "host"
	FieldModule      = "module"
	FieldArgs        = "args"
	FieldDestructive = "destructive"
)

var (
	// validHost は inventory のホスト名。ansible のパターンの記号（* ? , : ! & [ ] ~ など）は
	// 受けない（1 台だけを指す。IPv6 などは inventory の別名で書く）。
	validHost = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
	// validModule はモジュールの FQCN（<namespace>.<collection>.<module>）。
	validModule = regexp.MustCompile(`^[A-Za-z0-9_]+(\.[A-Za-z0-9_]+){2,}$`)
	// groupNames は ansible が必ず持つグループ（ホストではない）。
	groupNames = map[string]bool{"all": true, "ungrouped": true}
)

// Options は Provider の設定。
type Options struct {
	// Secrets は args の SecretRef を実行の直前に解決する（nil なら SecretRef は
	// 書けない）。
	Secrets core.SecretResolver
	// SecretPaths は store にある path の一覧（読み込みの時点で SecretRef の path
	// があるかだけを確かめる。値は読まない）。nil なら確かめない。
	SecretPaths func() ([]string, error)
	// MaxParallel は同時に走らせる ansible の数の上限（0 なら DefaultMaxParallel）。
	MaxParallel int
}

// Provider は provider.Provider・provider.CheckReader・provider.PlanRefiner・
// provider.IntentValidator を実装する。
type Provider struct {
	runner Runner
	opts   Options
	slots  chan struct{}
}

var (
	_ provider.Provider        = (*Provider)(nil)
	_ provider.CheckReader     = (*Provider)(nil)
	_ provider.PlanRefiner     = (*Provider)(nil)
	_ provider.IntentValidator = (*Provider)(nil)
)

// New は runner を使う Provider を返す。
func New(runner Runner, opts Options) *Provider {
	n := opts.MaxParallel
	if n <= 0 {
		n = DefaultMaxParallel
	}
	return &Provider{runner: runner, opts: opts, slots: make(chan struct{}, n)}
}

// Schema はフィールドを宣言する（§28.4）。host と module が呼び出しの相手
// （identity）。args は比べない（観測は State を返さないので、差は ansible の
// check mode だけが知る）。
func (p *Provider) Schema() provider.Schema {
	return provider.Schema{Fields: map[string]provider.FieldSpec{
		FieldHost:        {Kind: core.FieldKindString, Required: true, Identity: true},
		FieldModule:      {Kind: core.FieldKindString, Required: true, Identity: true},
		FieldArgs:        {Kind: core.FieldKindString},
		FieldDestructive: {Kind: core.FieldKindBool},
	}}
}

// Capabilities は最小階級：State を返さず（TrueObserve:false）、列挙もしない。
func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{ProviderVersion: ProviderVersion}
}

// ValidateIntent は読み込みの時点で宣言を確かめる（現実は見ない）。文言に
// args の値は出さない（秘密を含みうる）。
func (p *Provider) ValidateIntent(localID string, fields map[string]string) error {
	if strings.TrimSpace(localID) == "" || strings.ContainsAny(localID, " \t\n") {
		return fmt.Errorf("%s: id %q: want a name without spaces (the id is yours to choose)", Name, localID)
	}
	names := make([]string, 0, len(fields))
	for k := range fields {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		switch k {
		case FieldHost, FieldModule, FieldArgs, FieldDestructive:
		default:
			return fmt.Errorf("%s: unknown field %q (want host, module, args, destructive)", Name, k)
		}
	}
	c, err := callOf(fields)
	if err != nil {
		return fmt.Errorf("%s: %w", Name, err)
	}
	refs, err := secretRefs(c.args)
	if err != nil {
		return fmt.Errorf("%s: %w", Name, err)
	}
	if len(refs) == 0 {
		return nil
	}
	if p.opts.Secrets == nil {
		return fmt.Errorf("%s: args has a SecretRef (%s) but no secret_store is configured", Name, refs[0])
	}
	if p.opts.SecretPaths == nil {
		return nil
	}
	paths, err := p.opts.SecretPaths()
	if err != nil {
		return fmt.Errorf("%s: secret store: %w", Name, err)
	}
	have := make(map[string]bool, len(paths))
	for _, s := range paths {
		have[s] = true
	}
	for _, r := range refs {
		if !have[r.Path()] {
			return fmt.Errorf("%s: args: %s is not in the secret store", Name, r)
		}
	}
	return nil
}

// call は宣言のフィールドを読んだもの（SecretRef は未解決）。
type call struct {
	host, module string
	args         map[string]any
	destructive  bool
}

func callOf(fields map[string]string) (call, error) {
	c := call{host: strings.TrimSpace(fields[FieldHost]), module: strings.TrimSpace(fields[FieldModule])}
	switch {
	case c.host == "":
		return call{}, errors.New("host is required (an inventory host name)")
	case groupNames[c.host] || !validHost.MatchString(c.host):
		return call{}, fmt.Errorf("host %q: want one inventory host name, not a pattern or a group", c.host)
	case c.module == "":
		return call{}, errors.New("module is required (its FQCN, e.g. ansible.builtin.lineinfile)")
	case !validModule.MatchString(c.module):
		return call{}, fmt.Errorf("module %q: want the FQCN (<namespace>.<collection>.<module>, e.g. ansible.builtin.lineinfile)", c.module)
	}
	switch fields[FieldDestructive] {
	case "", "false":
	case "true":
		c.destructive = true
	default:
		return call{}, errors.New("destructive: want true or false")
	}
	args, err := parseArgs(fields[FieldArgs])
	if err != nil {
		return call{}, err
	}
	c.args = args
	return c, nil
}

// run は c を 1 回流す（check なら観測）。SecretRef は直前に解決し、結果と
// エラーの文言の平文は伏せる。secret は SecretRef を使ったか。
func (p *Provider) run(ctx context.Context, c call, check bool) (res Result, secret bool, err error) {
	args, secrets, err := resolveArgs(ctx, c.args, p.opts.Secrets)
	if err != nil {
		return Result{}, false, provider.NewPermanentError(err)
	}
	m := newMasker(secrets)
	secret = len(secrets) > 0
	select {
	case p.slots <- struct{}{}:
	case <-ctx.Done():
		return Result{}, secret, provider.NewTransientError(ctx.Err())
	}
	defer func() { <-p.slots }()
	if !check {
		// 適用の前に、host が inventory のちょうど 1 台を指すことを確かめる
		// （グループ名なら全員に流れてしまう）。
		hosts, herr := p.runner.Hosts(ctx, c.host)
		if herr != nil {
			return Result{}, secret, provider.NewTransientError(errors.New(m.mask(herr.Error())))
		}
		if len(hosts) != 1 || hosts[0] != c.host {
			return Result{}, secret, provider.NewPermanentError(fmt.Errorf(
				"host %q is not one inventory host (it matches %d: %s); a resource is one host", c.host, len(hosts), strings.Join(hosts, ", ")))
		}
	}
	res, err = p.runner.Run(ctx, Call{Host: c.host, Module: c.module, Args: args, Check: check})
	if err != nil {
		return Result{}, secret, provider.NewTransientError(errors.New(m.mask(err.Error())))
	}
	res.Msg = m.mask(res.Msg)
	res.Diff = m.mask(res.Diff)
	return res, secret, nil
}

func unknownObs() core.Observation {
	return core.Observation{Availability: core.AvailabilityUnknown, ObservedAt: time.Now()}
}

// Read は desired 無しでは観測できない（check mode は与えた引数に対する
// changed しか答えない、§28.7）。コアは provider.ReadFor で ReadCheck を呼ぶ。
func (p *Provider) Read(_ context.Context, localID string) (core.Observation, error) {
	return unknownObs(), provider.NewPermanentError(fmt.Errorf(
		"%s: Read %q: the minimal tier observes only by running the module in check mode against the desired (§28.7); there is no State to read", Name, localID))
}

// ReadCheck はモジュールを --check --diff で流して観測する（provider.CheckReader）。
func (p *Provider) ReadCheck(ctx context.Context, localID string, desired map[string]string) (core.Observation, error) {
	c, err := callOf(desired)
	if err != nil {
		return unknownObs(), provider.NewPermanentError(fmt.Errorf("%s: observe %q: %w", Name, localID, err))
	}
	res, secret, err := p.run(ctx, c, true)
	if err != nil {
		return unknownObs(), fmt.Errorf("%s: observe %q: %w", Name, localID, err)
	}
	switch res.Status {
	case StatusOK:
		check := &core.CheckResult{Changed: res.Changed}
		if res.Changed && !secret {
			check.Detail = strings.TrimRight(res.Diff, "\n")
		}
		return core.Observation{Availability: core.AvailabilityOk, Check: check, ObservedAt: time.Now()}, nil
	case StatusSkipped:
		return unknownObs(), provider.NewPermanentError(fmt.Errorf(
			"%s: observe %q: %s on %s cannot be observed: it does not run in check mode (%s)", Name, localID, c.module, c.host, orNone(res.Msg)))
	default:
		return unknownObs(), fmt.Errorf("%s: observe %q: %w", Name, localID, resultError(c, res))
	}
}

// Apply は同じ呼び出しを通常の実行で流す（冪等なのはモジュール）。
func (p *Provider) Apply(ctx context.Context, plan core.Plan) error {
	for _, op := range plan.Ops {
		id := op.Target.Local
		if op.Kind == core.OpKindDelete {
			return provider.NewPermanentError(fmt.Errorf(
				"%s: Apply %q: the ansible provider does not delete; write the removal into the module's arguments (e.g. state: absent)", Name, id))
		}
		fields := make(map[string]string, len(op.Fields))
		for _, f := range op.Fields {
			fields[f.Field] = f.To
		}
		c, err := callOf(fields)
		if err != nil {
			return provider.NewPermanentError(fmt.Errorf("%s: Apply %q: %w", Name, id, err))
		}
		res, _, err := p.run(ctx, c, false)
		if err != nil {
			return fmt.Errorf("%s: Apply %q: %w", Name, id, err)
		}
		switch res.Status {
		case StatusOK:
		case StatusSkipped:
			return provider.NewPermanentError(fmt.Errorf("%s: Apply %q: %s on %s was skipped (%s)", Name, id, c.module, c.host, orNone(res.Msg)))
		default:
			return fmt.Errorf("%s: Apply %q: %w", Name, id, resultError(c, res))
		}
	}
	return nil
}

// RefinePlan は destructive: true の呼び出しの適用を破壊的にする（予算に数える、
// §13）。コアは State を持たない最小階級の変更の重さを知れないので、利用者の
// 宣言に従う（§29.1）。フィールドは落とさない（Apply は呼び出しを組み立て直す）。
func (p *Provider) RefinePlan(_ string, _ core.State, desired map[string]string, ops []core.Op) ([]core.Op, error) {
	if desired[FieldDestructive] != "true" {
		return ops, nil
	}
	out := make([]core.Op, len(ops))
	for i, op := range ops {
		op.Destructive = true
		out[i] = op
	}
	return out, nil
}

// resultError は失敗・届かないの結果のエラー。届かないは transient、ansible が
// 引数やモジュールそのものを断ったものは permanent（宣言を直すまで同じ）、
// それ以外のモジュールの失敗は transient（対象が直れば通りうる）。
func resultError(c call, res Result) error {
	if res.Status == StatusUnreachable {
		return provider.NewTransientError(fmt.Errorf("%s is unreachable: %s", c.host, orNone(res.Msg)))
	}
	err := fmt.Errorf("%s on %s failed: %s", c.module, c.host, orNone(res.Msg))
	for _, s := range permanentFailures {
		if strings.Contains(res.Msg, s) {
			return provider.NewPermanentError(err)
		}
	}
	return provider.NewTransientError(err)
}

// permanentFailures は ansible が宣言そのものを断るときの知らせの一部。
var permanentFailures = []string{
	"Unsupported parameters",
	"missing required arguments",
	"Cannot resolve",
	"couldn't resolve module/action",
	"value of ",
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "no message"
	}
	return strings.TrimSpace(s)
}
