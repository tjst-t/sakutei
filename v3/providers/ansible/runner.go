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

package ansible

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Call は 1 つのホストに対する 1 つのモジュールの呼び出し（ad-hoc の ansible の
// 1 回、E-E3）。
type Call struct {
	// Host は inventory のホスト名（パターンではない。1 台だけに当たる）。
	Host string
	// Module はモジュールの FQCN（例 ansible.builtin.lineinfile）。
	Module string
	// Args はモジュールの引数（コマンドラインにもログにも出さない）。
	Args map[string]any
	// Check なら --check --diff で流す（観測）。false なら通常の実行（適用）。
	Check bool
}

// 結果の status（ansible の callback の v2_runner_on_* に対応）。
const (
	StatusOK          = "ok"
	StatusFailed      = "failed"
	StatusUnreachable = "unreachable"
	StatusSkipped     = "skipped"
)

// Result はホスト 1 台の結果。
type Result struct {
	Host    string `json:"host"`
	Status  string `json:"status"`
	Changed bool   `json:"changed"`
	// Msg はモジュールか ansible の知らせ（失敗の理由など）。
	Msg string `json:"msg"`
	// Diff は --diff の差分（ansible の形の文字列）。無ければ空。
	Diff string `json:"diff"`
}

// Runner は ansible を走らせる口。Provider は ansible をこの口越しにだけ呼ぶので、
// ふだんのテストは偽の ansible（ansiblefake）で回り、本物の ansible-core は
// 統合テスト（build tag integration_v3_ansible）だけが使う（E-P2）。
//
// Run は call を 1 回流し、call.Host の結果を返す。ansible を走らせられない・
// 結果が無い（ホストが inventory に無い）・ほかのホストの結果が混じるときは
// 誤りを返す。モジュールの失敗・届かない・check mode を持たないは誤りではなく
// Result.Status で返す（E-K1）。
//
// Hosts は pattern に当たる inventory のホスト名を返す（ansible --list-hosts）。
// 適用の前に、Host が 1 台だけを指すことを確かめるのに使う。
type Runner interface {
	Run(ctx context.Context, call Call) (Result, error)
	Hosts(ctx context.Context, pattern string) ([]string, error)
}

//go:embed callback/sakutei_json.py
var callbackPlugin []byte

const (
	// callbackName は自前の stdout callback の名前（callback/sakutei_json.py）。
	callbackName = "sakutei_json"
	// resultPrefix は callback が結果の行の頭に付ける印。
	resultPrefix = "SAKUTEI-RESULT "
	// argsVar は引数を渡す extra var の名前。
	argsVar = "__sakutei_args"
)

// forcedEnv は provider が決める環境変数（settings の env では変えられない）。
// ANSIBLE_LOAD_CALLBACK_PLUGINS は ad-hoc に stdout callback を読ませる。
var forcedEnv = []string{
	"ANSIBLE_STDOUT_CALLBACK", "ANSIBLE_LOAD_CALLBACK_PLUGINS", "ANSIBLE_CALLBACK_PLUGINS", "ANSIBLE_NOCOLOR",
}

// ExecRunner は本物の ansible（ad-hoc）を os/exec で走らせる Runner。シェルは
// 通さない。引数はコマンドラインに載せず、0600 の一時ファイルの extra vars
// （-e @file、文字列の値はすべて !unsafe＝テンプレートとして評価しない）で渡し、
// -a はその変数を指すだけにする。結果は自前の stdout callback が出す 1 行の
// JSON で読む。
type ExecRunner struct {
	// Path は ansible の実行ファイル（既定 "ansible"、PATH から）。
	Path string
	// Inventory は inventory のファイル（-i）。
	Inventory string
	// Env は足す・上書きする環境変数（forcedEnv は除く）。
	Env map[string]string
	// Timeout は 1 回の上限（0 なら DefaultTimeout）。
	Timeout time.Duration
}

var _ Runner = ExecRunner{}

// Run は Runner の Run。
func (r ExecRunner) Run(ctx context.Context, call Call) (Result, error) {
	dir, err := os.MkdirTemp("", "sakutei-ansible-")
	if err != nil {
		return Result{}, fmt.Errorf("ansible: 一時ディレクトリを作れない: %w", err)
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, callbackName+".py"), callbackPlugin, 0o600); err != nil {
		return Result{}, fmt.Errorf("ansible: callback を置けない: %w", err)
	}
	argv := []string{call.Host, "-i", r.Inventory, "-m", call.Module}
	if len(call.Args) > 0 {
		vars := filepath.Join(dir, "args.yml")
		doc := map[string]any{argsVar: call.Args}
		if err := os.WriteFile(vars, []byte(unsafeYAML(doc)+"\n"), 0o600); err != nil {
			return Result{}, fmt.Errorf("ansible: 引数のファイルを書けない: %w", err)
		}
		tmpl, err := argsTemplate(call.Args)
		if err != nil {
			return Result{}, err
		}
		argv = append(argv, "-e", "@"+vars, "-a", tmpl)
	}
	if call.Check {
		argv = append(argv, "--check", "--diff")
	}
	stdout, stderr, runErr := r.exec(ctx, argv, dir)
	results, perr := parseResults(stdout)
	if perr != nil {
		return Result{}, fmt.Errorf("ansible: %w", perr)
	}
	switch {
	case len(results) == 0 && runErr != nil:
		return Result{}, fmt.Errorf("ansible: %w%s", runErr, stderrTail(stderr))
	case len(results) == 0:
		return Result{}, fmt.Errorf("ansible: ホスト %q の結果が無い（inventory に無い）%s", call.Host, stderrTail(stderr))
	case len(results) > 1:
		return Result{}, fmt.Errorf("ansible: ホスト %q が %d 台に当たった（%s）。1 つの宣言は 1 台", call.Host, len(results), resultHosts(results))
	case results[0].Host != call.Host:
		return Result{}, fmt.Errorf("ansible: ホスト %q がほかのホスト %q に当たった。1 つの宣言は inventory の 1 台", call.Host, results[0].Host)
	}
	return results[0], nil
}

// Hosts は Runner の Hosts。
func (r ExecRunner) Hosts(ctx context.Context, pattern string) ([]string, error) {
	stdout, stderr, err := r.exec(ctx, []string{pattern, "-i", r.Inventory, "--list-hosts"}, "")
	if err != nil {
		return nil, fmt.Errorf("ansible --list-hosts: %w%s", err, stderrTail(stderr))
	}
	return parseListHosts(stdout)
}

// exec は ansible を argv で走らせる（シェルを通さない）。callbackDir が空で
// なければ、そこの自前の stdout callback を使わせる。時間切れはプロセスグループ
// ごと（ansible が開いた ssh も）止める。
func (r ExecRunner) exec(ctx context.Context, argv []string, callbackDir string) (stdout, stderr []byte, err error) {
	path := r.Path
	if path == "" {
		path = "ansible"
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, argv...)
	cmd.Env = r.environ(callbackDir)
	cmd.WaitDelay = 5 * time.Second
	killGroupOnCancel(cmd)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err = cmd.Run()
	if ctx.Err() != nil {
		return out.Bytes(), errb.Bytes(), fmt.Errorf("%s が %s のうちに終わらなかった: %w", path, timeout, ctx.Err())
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out.Bytes(), errb.Bytes(), fmt.Errorf("%s が終了コード %d で終わった", path, ee.ExitCode())
	}
	if err != nil {
		return out.Bytes(), errb.Bytes(), fmt.Errorf("%s を走らせられない: %w", path, err)
	}
	return out.Bytes(), errb.Bytes(), nil
}

// environ は子の環境：自分の環境に Env を重ね、callback の設定を最後に置く。
func (r ExecRunner) environ(callbackDir string) []string {
	env := os.Environ()
	keys := make([]string, 0, len(r.Env))
	for k := range r.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+r.Env[k])
	}
	env = append(env, "ANSIBLE_NOCOLOR=1")
	if callbackDir != "" {
		env = append(env,
			"ANSIBLE_STDOUT_CALLBACK="+callbackName,
			"ANSIBLE_LOAD_CALLBACK_PLUGINS=1",
			"ANSIBLE_CALLBACK_PLUGINS="+callbackDir)
	}
	return env
}

// argsTemplate は -a の値：引数の各キーを extra var の同じキーで置いた JSON の
// オブジェクト。ansible はキーごとに 1 つの式として評価し、値の型（真偽・数・
// 入れ子）をそのまま渡す。キーは validArgKey に合うものだけ（Validate）。
func argsTemplate(args map[string]any) (string, error) {
	m := make(map[string]string, len(args))
	for k := range args {
		if !validArgKey.MatchString(k) {
			return "", fmt.Errorf("ansible: 引数の名前 %q は英字・数字・_ で書く", k)
		}
		m[k] = "{{ " + argsVar + "['" + k + "'] }}"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// unsafeYAML は v を YAML の flow 形式で書く。文字列はすべて !unsafe を付けた
// JSON の文字列（YAML の二重引用符の scalar として読める）にして、ansible が
// テンプレートとして評価しないようにする（"{{" を含む値もそのまま渡る）。
func unsafeYAML(v any) string {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			kb, _ := json.Marshal(k)
			parts = append(parts, string(kb)+": "+unsafeYAML(t[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, unsafeYAML(e))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case string:
		b, _ := json.Marshal(t)
		return "!unsafe " + string(b)
	case nil:
		return "null"
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// parseResults は stdout の中の結果の行（resultPrefix で始まる）を読む。
func parseResults(stdout []byte) ([]Result, error) {
	var out []Result
	sc := bufio.NewScanner(bytes.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 64*1024), 64<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, resultPrefix) {
			continue
		}
		var r Result
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, resultPrefix)), &r); err != nil {
			return nil, fmt.Errorf("callback の結果の行が JSON でない: %w", err)
		}
		switch r.Status {
		case StatusOK, StatusFailed, StatusUnreachable, StatusSkipped:
		default:
			return nil, fmt.Errorf("callback の結果の行の status %q を知らない", r.Status)
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("出力を読めない: %w", err)
	}
	return out, nil
}

// parseListHosts は `ansible <pattern> --list-hosts` の出力を読む:
//
//	hosts (2):
//	  web1
//	  web2
func parseListHosts(stdout []byte) ([]string, error) {
	var hosts []string
	seen := false
	for _, line := range strings.Split(string(stdout), "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "hosts (") && strings.HasSuffix(t, "):") {
			seen = true
			continue
		}
		if seen {
			hosts = append(hosts, t)
		}
	}
	if !seen {
		return nil, errors.New(`ansible --list-hosts: 出力に "hosts (N):" の行が無い`)
	}
	return hosts, nil
}

func resultHosts(rs []Result) string {
	hs := make([]string, len(rs))
	for i, r := range rs {
		hs[i] = r.Host
	}
	return strings.Join(hs, ", ")
}

// stderrTail は stderr の最後の数行（知らせに添える）。
func stderrTail(stderr []byte) string {
	lines := strings.Split(strings.TrimSpace(string(stderr)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return ""
	}
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	return ": " + strings.Join(lines, " / ")
}
