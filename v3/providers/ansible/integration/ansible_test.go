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

//go:build integration_v3_ansible

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/providers/ansible"
)

// wantCore は make integration-v3-ansible が venv に入れる ansible-core のバージョン
// （v2 の統合テストと同じ）。
const wantCore = "2.21.5"

var lab struct {
	ansible string // ansible の実行ファイル
	python  string // venv の python（local 接続の interpreter）
	dir     string // 使い捨ての作業ディレクトリ（ファイル・intent・設定・Record）
	short   string // ansible の TMPDIR と ssh の ControlPath（unix socket のパス長の上限のため短いパス）
	config  string // 接続設定
	version string
}

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	if err := setup(); err != nil {
		fmt.Fprintln(os.Stderr, "integration-v3-ansible:", err)
		teardown()
		return 1
	}
	code := m.Run()
	teardown()
	return code
}

func setup() error {
	lab.ansible = os.Getenv("SAKUTEI_IT_ANSIBLE")
	if lab.ansible == "" {
		return fmt.Errorf("SAKUTEI_IT_ANSIBLE（ansible の実行ファイル）が無い（make integration-v3-ansible で実行する）")
	}
	if !filepath.IsAbs(lab.ansible) {
		return fmt.Errorf("SAKUTEI_IT_ANSIBLE は絶対パスで渡す: %q", lab.ansible)
	}
	lab.python = filepath.Join(filepath.Dir(lab.ansible), "python3")
	if _, err := os.Stat(lab.python); err != nil {
		return fmt.Errorf("venv の python: %w", err)
	}
	out, err := exec.Command(lab.ansible, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("ansible --version: %v: %s", err, out)
	}
	m := regexp.MustCompile(`core ([0-9.]+)`).FindStringSubmatch(string(out))
	if m == nil {
		return fmt.Errorf("ansible --version の出力に core のバージョンが無い: %q", out)
	}
	lab.version = m[1]
	if lab.version != wantCore && os.Getenv("SAKUTEI_IT_ANSIBLE_ANY") == "" {
		return fmt.Errorf("ansible-core %s（want %s。ほかのバージョンで動かすなら SAKUTEI_IT_ANSIBLE_ANY=1）", lab.version, wantCore)
	}
	fmt.Printf("integration-v3-ansible: ansible-core %s（%s）\n", lab.version, lab.ansible)
	if lab.dir, err = os.MkdirTemp("", "sakutei-v3-ansible-it-"); err != nil {
		return err
	}
	if lab.short, err = os.MkdirTemp("/tmp", "sk-v3ans-"); err != nil {
		return err
	}
	inv := fmt.Sprintf("target ansible_connection=local ansible_python_interpreter=%s\n"+
		"gone ansible_host=127.0.0.1 ansible_port=1 ansible_connection=ssh ansible_ssh_common_args='-o ConnectTimeout=3'\n"+
		"[pair]\ntarget\ngone\n", lab.python)
	invPath := filepath.Join(lab.dir, "inventory.ini")
	if err := os.WriteFile(invPath, []byte(inv), 0o600); err != nil {
		return err
	}
	cfg := fmt.Sprintf(`providers:
  ans:
    module: ansible
    settings:
      inventory: %s
      ansible: %s
      timeout: 2m
      max_parallel: 4
      env:
        TMPDIR: %s
        ANSIBLE_SSH_CONTROL_PATH_DIR: %s
        ANSIBLE_HOST_KEY_CHECKING: "False"
        ANSIBLE_LOCAL_TEMP: %s
`, invPath, lab.ansible, lab.short, filepath.Join(lab.short, "cp"), filepath.Join(lab.short, "tmp"))
	lab.config = filepath.Join(lab.dir, "config.yaml")
	if err := os.WriteFile(lab.config, []byte(cfg), 0o600); err != nil {
		return err
	}
	return os.MkdirAll(filepath.Join(lab.dir, "files"), 0o755)
}

func teardown() {
	for _, d := range []string{lab.dir, lab.short} {
		if d != "" {
			_ = os.RemoveAll(d)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ヘルパー
// ─────────────────────────────────────────────────────────────────────────────

func file(name string) string { return filepath.Join(lab.dir, "files", name) }

func read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(file(name))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func exists(name string) bool {
	_, err := os.Stat(file(name))
	return err == nil
}

func mtime(t *testing.T, name string) int64 {
	t.Helper()
	st, err := os.Stat(file(name))
	if err != nil {
		t.Fatal(err)
	}
	return st.ModTime().UnixNano()
}

// call は宣言 1 つ（YAML の 1 行）。args は JSON（YAML の flow として読める）。
func call(name, host, module string, args map[string]any, extra string) string {
	b, _ := json.Marshal(args)
	return fmt.Sprintf("  - {type: ans/call, id: %s, fields: {host: %s, module: %s, args: %s%s}}\n", name, host, module, b, extra)
}

// intent は intent のディレクトリを書き直す。
func intent(t *testing.T, body string) string {
	t.Helper()
	d := filepath.Join(lab.dir, "intent")
	_ = os.RemoveAll(d)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "ansible.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return d
}

// run は v3 の CLI を 1 回動かし、JSON の結果と終了コードを返す。
func run(t *testing.T, verb, intentDir string) (plan.Result, int, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), []string{verb, "--intent", intentDir, "--config", lab.config,
		"--record", filepath.Join(lab.dir, "record"), "--json"},
		&stdout, &stderr, cli.WithModule("ansible", ansible.Module))
	var r plan.Result
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		if code == cli.ExitRejected {
			return r, code, stderr.String()
		}
		t.Fatalf("%s の出力を読めない（exit=%d）: %v\n%s\n%s", verb, code, err, stdout.String(), stderr.String())
	}
	return r, code, stderr.String()
}

func changeOf(r plan.Result, name string) plan.Change {
	for _, c := range r.Changes {
		if c.Resource == "ans/call/"+name {
			return c
		}
	}
	return plan.Change{}
}

func dump(r plan.Result) string {
	b, _ := json.MarshalIndent(r, "", "  ")
	return string(b)
}

func wantOps(t *testing.T, step string, r plan.Result, want map[string]plan.Op) {
	t.Helper()
	for name, op := range want {
		if c := changeOf(r, name); c.Op != op {
			t.Fatalf("%s: %s は %q（want %q）\n%s", step, name, c.Op, op, dump(r))
		}
	}
}

// settled は reconcile のあと（再観測を含めて）宣言どおりかを返す。
func settled(r plan.Result, name string) bool { return changeOf(r, name).Final().Settled() }

// ─────────────────────────────────────────────────────────────────────────────
// テスト
// ─────────────────────────────────────────────────────────────────────────────

func TestAnsible(t *testing.T) {
	motd := call("motd", "target", "ansible.builtin.lineinfile",
		map[string]any{"path": file("motd"), "line": "managed by sakutei", "create": true, "mode": "0644"}, "")
	braces := call("braces", "target", "ansible.builtin.lineinfile",
		map[string]any{"path": file("motd"), "line": "literal {{ not_a_var }} kept", "create": true}, "")
	conf := call("app-conf", "target", "ansible.builtin.copy",
		map[string]any{"dest": file("app.conf"), "content": "listen = 8080\nworkers = 4\n", "mode": "0640"}, "")
	main := "resources:\n" + motd + braces + conf
	names := []string{"motd", "braces", "app-conf"}
	all := func(op plan.Op) map[string]plan.Op {
		m := map[string]plan.Op{}
		for _, n := range names {
			m[n] = op
		}
		return m
	}

	t.Run("空の状態から：plan は check mode の差分を出して変えず、reconcile が Apply する", func(t *testing.T) {
		dir := intent(t, main)
		r, code, stderr := run(t, "plan", dir)
		if code != cli.ExitOK {
			t.Fatalf("plan: exit=%d\n%s\n%s", code, dump(r), stderr)
		}
		wantOps(t, "plan", r, all(plan.Update))
		c := changeOf(r, "motd")
		if len(c.Fields) != 1 || c.Fields[0].Name != "module" || !strings.Contains(strings.Join(c.Classified, "\n"), "+managed by sakutei") {
			t.Fatalf("plan が module の drift と check mode の差分を出さない:\n%s", dump(r))
		}
		if exists("motd") || exists("app.conf") {
			t.Fatal("plan が現実を変えた")
		}
		r, code, stderr = run(t, "reconcile", dir)
		if code != cli.ExitOK {
			t.Fatalf("reconcile: exit=%d\n%s\n%s", code, dump(r), stderr)
		}
		for _, n := range names {
			if !settled(r, n) {
				t.Fatalf("%s が再観測で宣言どおりでない:\n%s", n, dump(r))
			}
		}
		// 同じファイルの 2 つの行は名前の順に Apply される（並びは問わない）。
		if got := read(t, "motd"); got != "managed by sakutei\nliteral {{ not_a_var }} kept\n" && got != "literal {{ not_a_var }} kept\nmanaged by sakutei\n" {
			t.Fatalf("motd = %q（{{ }} はテンプレートとして評価しない）", got)
		}
		if got := read(t, "app.conf"); got != "listen = 8080\nworkers = 4\n" {
			t.Fatalf("app.conf = %q", got)
		}
		r, code, _ = run(t, "plan", dir)
		if code != cli.ExitOK {
			t.Fatalf("plan: exit=%d", code)
		}
		wantOps(t, "Apply したあと", r, all(plan.None))
	})

	t.Run("手で変えると drift し、reconcile が戻す", func(t *testing.T) {
		dir := intent(t, main)
		if err := os.WriteFile(file("app.conf"), []byte("listen = 9999\n"), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file("motd"), []byte("literal {{ not_a_var }} kept\nsomeone else\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		r, _, _ := run(t, "plan", dir)
		wantOps(t, "手で変えた", r, map[string]plan.Op{"app-conf": plan.Update, "motd": plan.Update, "braces": plan.None})
		if !strings.Contains(strings.Join(changeOf(r, "app-conf").Classified, "\n"), "listen = 8080") {
			t.Fatalf("差分が理由に無い:\n%s", dump(r))
		}
		r, code, stderr := run(t, "reconcile", dir)
		if code != cli.ExitOK {
			t.Fatalf("reconcile: exit=%d\n%s\n%s", code, dump(r), stderr)
		}
		if got := read(t, "app.conf"); got != "listen = 8080\nworkers = 4\n" {
			t.Fatalf("app.conf = %q", got)
		}
		if got := read(t, "motd"); got != "literal {{ not_a_var }} kept\nsomeone else\nmanaged by sakutei\n" {
			t.Fatalf("motd = %q", got)
		}
	})

	t.Run("何 cycle 回しても宣言どおりのまま書き換えない", func(t *testing.T) {
		dir := intent(t, main)
		before := map[string]int64{"motd": mtime(t, "motd"), "app.conf": mtime(t, "app.conf")}
		for i := 0; i < 5; i++ {
			r, code, stderr := run(t, "reconcile", dir)
			if code != cli.ExitOK {
				t.Fatalf("cycle %d: exit=%d\n%s\n%s", i, code, dump(r), stderr)
			}
			wantOps(t, fmt.Sprintf("cycle %d", i), r, all(plan.None))
		}
		for n, m := range before {
			if mtime(t, n) != m {
				t.Fatalf("宣言どおりの cycle が %s を書き換えた", n)
			}
		}
	})

	t.Run("到達できないホストと check mode を持たないモジュールは Unknown で、Apply しない", func(t *testing.T) {
		gone := call("gone-motd", "gone", "ansible.builtin.lineinfile", map[string]any{"path": "/tmp/sakutei-v3-never", "line": "x", "create": true}, "")
		cmd := call("touch", "target", "ansible.builtin.command", map[string]any{"cmd": "touch " + file("touched")}, "")
		dir := intent(t, main+gone+cmd)
		r, code, stderr := run(t, "reconcile", dir)
		if code != cli.ExitRemaining {
			t.Fatalf("reconcile: exit=%d（want %d）\n%s\n%s", code, cli.ExitRemaining, dump(r), stderr)
		}
		wantOps(t, "観測できない", r, map[string]plan.Op{"gone-motd": plan.Unknown, "touch": plan.Unknown, "motd": plan.None})
		if n := changeOf(r, "gone-motd").Note; !strings.Contains(n, "到達できない") {
			t.Fatalf("到達できない理由が無い: %q", n)
		}
		if n := changeOf(r, "touch").Note; !strings.Contains(n, "check mode を持たない") {
			t.Fatalf("check mode の理由が無い: %q", n)
		}
		if exists("touched") {
			t.Fatal("観測できないコマンドを流した")
		}
	})

	t.Run("destructive の呼び出しは budget の破壊的な変更に数える", func(t *testing.T) {
		add := call("add", "target", "ansible.builtin.lineinfile", map[string]any{"path": file("additive"), "line": "a", "create": true}, "")
		d1 := call("wipe-1", "target", "ansible.builtin.copy", map[string]any{"dest": file("d1"), "content": "wiped\n"}, ", destructive: true")
		d2 := call("wipe-2", "target", "ansible.builtin.copy", map[string]any{"dest": file("d2"), "content": "wiped\n"}, ", destructive: true")
		policy := func(n int) string {
			return fmt.Sprintf("policies:\n  - {name: wipe, select: {all: true}, budget: {destructive_per_cycle: %d}}\n", n)
		}
		dir := intent(t, policy(1)+main+add+d1+d2)
		r, code, stderr := run(t, "reconcile", dir)
		if code != cli.ExitHalted {
			t.Fatalf("reconcile: exit=%d（want %d）\n%s\n%s", code, cli.ExitHalted, dump(r), stderr)
		}
		// 破壊的な変更に数えるのは destructive: true の 2 つだけ（破壊的でない add は数えない）。
		if len(r.Budgets) != 1 || r.Budgets[0].Destructive != 2 || r.Budgets[0].Changes != 3 {
			t.Fatalf("budget の数え方:\n%s", dump(r))
		}
		for n, want := range map[string]bool{"add": false, "wipe-1": true, "wipe-2": true} {
			if c := changeOf(r, n); c.Destructive != want || c.Action != plan.ActHalt {
				t.Fatalf("%s: destructive=%v action=%s（want destructive=%v・halt）\n%s", n, c.Destructive, c.Action, want, dump(r))
			}
		}
		if exists("additive") || exists("d1") || exists("d2") {
			t.Fatalf("budget を超えて halt した cycle が Apply した:\n%s", dump(r))
		}
		dir = intent(t, policy(2)+main+add+d1+d2)
		r, code, stderr = run(t, "reconcile", dir)
		if code != cli.ExitOK {
			t.Fatalf("budget を上げた reconcile: exit=%d\n%s\n%s", code, dump(r), stderr)
		}
		for n, want := range map[string]string{"additive": "a\n", "d1": "wiped\n", "d2": "wiped\n"} {
			if got := read(t, n); got != want {
				t.Fatalf("%s = %q（want %q）", n, got, want)
			}
		}
	})

	t.Run("グループ名の host は Apply せず、secret の参照・ensure: absent・Range は読み込み時にエラーにする", func(t *testing.T) {
		pair := call("pair", "pair", "ansible.builtin.lineinfile", map[string]any{"path": file("pair"), "line": "x", "create": true}, "")
		// check mode の観測が 2 ホストの結果を受けて Unknown になり、Apply しない。
		r, code, _ := run(t, "reconcile", intent(t, "resources:\n"+pair))
		if c := changeOf(r, "pair"); code != cli.ExitRemaining || c.Op != plan.Unknown || !strings.Contains(c.Note, "2 ホストにマッチした") || exists("pair") {
			t.Fatalf("グループ名: exit=%d\n%s", code, dump(r))
		}
		for name, c := range map[string]struct{ body, want string }{
			"secret の参照": {"resources:\n  - {type: ans/call, id: tok, fields: {host: target, module: ansible.builtin.copy, args: {dest: /tmp/x, content: {secret: \"file:app/token\"}}}}\n", "secret の参照を書けない"},
			"absent":     {"resources:\n  - {type: ans/call, id: gone, ensure: absent}\n", "ensure: absent は書けない"},
			"Range":      {"ranges:\n  - {provider: ans, args: {host: target}}\n" + "resources:\n" + motd, "Range を持たない"},
		} {
			if _, code, stderr := run(t, "plan", intent(t, c.body)); code != cli.ExitRejected || !strings.Contains(stderr, c.want) {
				t.Fatalf("%s: exit=%d（want %d・%q）\n%s", name, code, cli.ExitRejected, c.want, stderr)
			}
		}
	})

	t.Run("消すのはモジュールの引数（state: absent）で宣言する", func(t *testing.T) {
		absent := call("motd", "target", "ansible.builtin.lineinfile",
			map[string]any{"path": file("motd"), "line": "managed by sakutei", "state": "absent"}, "")
		dir := intent(t, "resources:\n"+absent)
		r, code, stderr := run(t, "reconcile", dir)
		if code != cli.ExitOK {
			t.Fatalf("reconcile: exit=%d\n%s\n%s", code, dump(r), stderr)
		}
		if strings.Contains(read(t, "motd"), "managed by sakutei") {
			t.Fatalf("motd = %q", read(t, "motd"))
		}
		if r, _, _ := run(t, "plan", dir); changeOf(r, "motd").Op != plan.None {
			t.Fatalf("消したあと:\n%s", dump(r))
		}
	})
}
