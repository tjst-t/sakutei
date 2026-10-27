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

//go:build integration_ansible

package ansible

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// wantCore は make integration-ansible が venv に入れる ansible-core の版。
const wantCore = "2.21.5"

var lab struct {
	ansible string // ansible の実行ファイル
	python  string // venv の python（local 接続の interpreter）
	dir     string // 使い捨ての作業場所（ファイル・intent・設定）
	short   string // ansible の TMPDIR と ssh の ControlPath（unix socket のパス長の上限のため短い場所）
	bin     string // sakutei
	cfg     string // --ansible-config
	version string
}

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	if err := setup(); err != nil {
		fmt.Fprintln(os.Stderr, "integration-ansible: setup:", err)
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
		return fmt.Errorf("SAKUTEI_IT_ANSIBLE is not set (run make integration-ansible)")
	}
	lab.python = filepath.Join(filepath.Dir(lab.ansible), "python3")
	if _, err := os.Stat(lab.python); err != nil {
		return fmt.Errorf("the venv's python: %w", err)
	}
	var err error
	if lab.dir, err = os.MkdirTemp("", "sakutei-ansible-it-"); err != nil {
		return err
	}
	if lab.short, err = os.MkdirTemp("/tmp", "sk-ansit-"); err != nil {
		return err
	}
	out, err := exec.Command(lab.ansible, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("ansible --version: %v: %s", err, out)
	}
	m := regexp.MustCompile(`core ([0-9.]+)`).FindStringSubmatch(string(out))
	if m == nil {
		return fmt.Errorf("ansible --version: no core version in %q", out)
	}
	lab.version = m[1]
	if lab.version != wantCore && os.Getenv("SAKUTEI_IT_ANSIBLE_ANY") == "" {
		return fmt.Errorf("ansible-core %s, want %s (set SAKUTEI_IT_ANSIBLE_ANY=1 to run another version)", lab.version, wantCore)
	}
	fmt.Printf("integration-ansible: ansible-core %s (%s)\n", lab.version, lab.ansible)

	inv := fmt.Sprintf("target ansible_connection=local ansible_python_interpreter=%s\n"+
		"gone ansible_host=127.0.0.1 ansible_port=1 ansible_connection=ssh ansible_ssh_common_args='-o ConnectTimeout=3'\n", lab.python)
	secrets := `{"secrets": {"app/token": {"gen": 1, "value": "it-S3cret-v4lue"}}}`
	cfg := fmt.Sprintf("inventory: inventory.ini\nansible: %s\nsecret_store: secrets.json\ntimeout: 2m\nmax_parallel: 4\n"+
		"env:\n  TMPDIR: %s\n  ANSIBLE_SSH_CONTROL_PATH_DIR: %s\n  ANSIBLE_HOST_KEY_CHECKING: \"False\"\n  ANSIBLE_LOCAL_TEMP: %s\n",
		lab.ansible, lab.short, filepath.Join(lab.short, "cp"), filepath.Join(lab.short, "tmp"))
	for name, body := range map[string]string{"inventory.ini": inv, "secrets.json": secrets, "ansible.yaml": cfg} {
		if err := os.WriteFile(filepath.Join(lab.dir, name), []byte(body), 0o600); err != nil {
			return err
		}
	}
	lab.cfg = filepath.Join(lab.dir, "ansible.yaml")
	if err := os.MkdirAll(filepath.Join(lab.dir, "intent"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(lab.dir, "files"), 0o755); err != nil {
		return err
	}
	lab.bin = filepath.Join(lab.dir, "sakutei")
	build := exec.Command("go", "build", "-o", lab.bin, "./cmd/sakutei")
	build.Dir = filepath.Join("..", "..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("go build: %v: %s", err, out)
	}
	return nil
}

func teardown() {
	for _, d := range []string{lab.dir, lab.short} {
		if d != "" {
			_ = os.RemoveAll(d)
		}
	}
}

// ---- 道具 ----

func file(name string) string { return filepath.Join(lab.dir, "files", name) }

func read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(file(name))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

type res map[string]any

func call(id, host, module string, args map[string]any, extra ...string) res {
	b, _ := json.Marshal(args)
	f := map[string]string{"host": host, "module": module, "args": string(b)}
	for i := 0; i+1 < len(extra); i += 2 {
		f[extra[i]] = extra[i+1]
	}
	return res{"provider": "ansible", "id": id, "fields": f}
}

func writeIntent(t *testing.T, rs ...res) {
	t.Helper()
	b, _ := json.MarshalIndent(map[string]any{"resources": rs}, "", "  ")
	if err := os.WriteFile(filepath.Join(lab.dir, "intent", "ansible.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func cliArgs(verb string, extra ...string) []string {
	return append([]string{verb,
		"--intent", filepath.Join(lab.dir, "intent"),
		"--state", filepath.Join(lab.dir, "fake-state.json"),
		"--cause-state", filepath.Join(lab.dir, "cause-state.json"),
		"--ansible-config", lab.cfg,
	}, extra...)
}

func sakutei(t *testing.T, verb string, extra ...string) (string, int) {
	t.Helper()
	out, err := exec.Command(lab.bin, cliArgs(verb, extra...)...).CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		t.Fatalf("sakutei %s: %v", verb, err)
	}
	return string(out), 0
}

func mustReconcile(t *testing.T, extra ...string) string {
	t.Helper()
	out, code := sakutei(t, "reconcile", extra...)
	if code != 0 {
		t.Fatalf("reconcile exited %d:\n%s", code, out)
	}
	return out
}

type statusDoc struct {
	Summary struct {
		Conformant   int `json:"conformant"`
		Drift        int `json:"drift"`
		Unobservable int `json:"unobservable"`
	} `json:"summary"`
	Resources []struct {
		LocalID string `json:"local_id"`
		State   string `json:"state"`
	} `json:"resources"`
}

func status(t *testing.T) (statusDoc, string) {
	t.Helper()
	cmd := exec.Command(lab.bin, cliArgs("status", "--json")...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if _, isExit := err.(*exec.ExitError); err != nil && !isExit {
		t.Fatalf("sakutei status: %v", err)
	}
	var doc statusDoc
	if jerr := json.Unmarshal(out, &doc); jerr != nil {
		t.Fatalf("status --json: %v (%v)\n%s%s", jerr, err, out, stderr.String())
	}
	return doc, stderr.String()
}

func stateOf(doc statusDoc, id string) string {
	for _, r := range doc.Resources {
		if r.LocalID == id {
			return r.State
		}
	}
	return ""
}

func mtime(t *testing.T, name string) int64 {
	t.Helper()
	st, err := os.Stat(file(name))
	if err != nil {
		t.Fatal(err)
	}
	return st.ModTime().UnixNano()
}

// ---- テスト ----

func TestAnsibleIntegration(t *testing.T) {
	motd := call("motd", "target", "ansible.builtin.lineinfile",
		map[string]any{"path": file("motd"), "line": "managed by sakutei", "create": true, "mode": "0644"})
	braces := call("braces", "target", "ansible.builtin.lineinfile",
		map[string]any{"path": file("motd"), "line": "literal {{ not_a_var }} kept", "create": true})
	conf := call("app-conf", "target", "ansible.builtin.copy",
		map[string]any{"dest": file("app.conf"), "content": "listen = 8080\nworkers = 4\n", "mode": "0640"})
	token := call("app-token", "target", "ansible.builtin.copy",
		map[string]any{"dest": file("token"), "content": map[string]any{"$secret": "app/token", "backend": "file"}, "mode": "0600"})
	main := []res{motd, braces, conf, token}

	t.Run("create from empty", func(t *testing.T) {
		writeIntent(t, main...)
		doc, _ := status(t)
		if doc.Summary.Drift != 4 || doc.Summary.Conformant != 0 {
			t.Fatalf("before: summary %+v, want drift 4", doc.Summary)
		}
		plan, code := sakutei(t, "plan")
		if code != 0 || !strings.Contains(plan, "+managed by sakutei") {
			t.Fatalf("plan (code %d) does not carry the check-mode diff:\n%s", code, plan)
		}
		if _, err := os.Stat(file("motd")); !os.IsNotExist(err) {
			t.Fatal("status / plan changed the target")
		}
		out := mustReconcile(t)
		if strings.Contains(out, "it-S3cret") || strings.Contains(plan, "it-S3cret") {
			t.Fatalf("the secret value is in the output:\n%s\n%s", plan, out)
		}
		if got := read(t, "motd"); got != "managed by sakutei\nliteral {{ not_a_var }} kept\n" {
			t.Fatalf("motd = %q", got)
		}
		if got := read(t, "app.conf"); got != "listen = 8080\nworkers = 4\n" {
			t.Fatalf("app.conf = %q", got)
		}
		if got := read(t, "token"); got != "it-S3cret-v4lue" {
			t.Fatalf("token = %q", got)
		}
		doc, _ = status(t)
		if doc.Summary.Conformant != 4 || doc.Summary.Drift != 0 {
			t.Fatalf("after: summary %+v, want conformant 4", doc.Summary)
		}
	})

	t.Run("changed by hand is drift and comes back", func(t *testing.T) {
		if err := os.WriteFile(file("app.conf"), []byte("listen = 9999\n"), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file("motd"), []byte("literal {{ not_a_var }} kept\nsomeone else\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		doc, _ := status(t)
		if doc.Summary.Drift != 2 || stateOf(doc, "app-conf") != "drift" || stateOf(doc, "motd") != "drift" || stateOf(doc, "braces") != "conformant" {
			t.Fatalf("summary %+v resources %+v", doc.Summary, doc.Resources)
		}
		mustReconcile(t)
		if got := read(t, "app.conf"); got != "listen = 8080\nworkers = 4\n" {
			t.Fatalf("app.conf = %q", got)
		}
		if got := read(t, "motd"); got != "literal {{ not_a_var }} kept\nsomeone else\nmanaged by sakutei\n" {
			t.Fatalf("motd = %q", got)
		}
		if doc, _ := status(t); doc.Summary.Conformant != 4 {
			t.Fatalf("summary %+v", doc.Summary)
		}
	})

	t.Run("no drift however many cycles", func(t *testing.T) {
		before := map[string]int64{}
		for _, n := range []string{"motd", "app.conf", "token"} {
			before[n] = mtime(t, n)
		}
		for i := 0; i < 5; i++ {
			out := mustReconcile(t)
			if strings.Contains(out, "UPDATE") || strings.Contains(out, "DriftEvent") {
				t.Fatalf("cycle %d planned a change:\n%s", i, out)
			}
			if doc, _ := status(t); doc.Summary.Conformant != 4 || doc.Summary.Drift != 0 {
				t.Fatalf("cycle %d: summary %+v", i, doc.Summary)
			}
		}
		for n, m := range before {
			if mtime(t, n) != m {
				t.Fatalf("%s was rewritten by a converged cycle", n)
			}
		}
	})

	t.Run("an unreachable host and a module without check mode are unobservable and nothing is done", func(t *testing.T) {
		gone := call("gone-motd", "gone", "ansible.builtin.lineinfile", map[string]any{"path": "/tmp/sakutei-never", "line": "x", "create": true})
		cmd := call("touch", "target", "ansible.builtin.command", map[string]any{"cmd": "touch " + file("touched")})
		writeIntent(t, append(append([]res{}, main...), gone, cmd)...)
		doc, stderr := status(t)
		if doc.Summary.Unobservable != 2 || doc.Summary.Conformant != 4 || doc.Summary.Drift != 0 {
			t.Fatalf("summary %+v\n%s", doc.Summary, stderr)
		}
		if !strings.Contains(stderr, "unreachable") || !strings.Contains(stderr, "check mode") {
			t.Fatalf("status does not say why:\n%s", stderr)
		}
		out, code := sakutei(t, "reconcile")
		if code == 0 {
			t.Fatalf("reconcile with unobservable resources exited 0:\n%s", out)
		}
		if _, err := os.Stat(file("touched")); !os.IsNotExist(err) {
			t.Fatalf("the command ran although it cannot be observed:\n%s", out)
		}
		if strings.Contains(out, "UPDATE ansible//gone-motd") || strings.Contains(out, "UPDATE ansible//touch") {
			t.Fatalf("an unobservable resource was planned:\n%s", out)
		}
		writeIntent(t, main...)
	})

	t.Run("destructive applies are stopped by the budget", func(t *testing.T) {
		add := call("add", "target", "ansible.builtin.lineinfile", map[string]any{"path": file("additive"), "line": "a", "create": true})
		d1 := call("wipe-1", "target", "ansible.builtin.copy", map[string]any{"dest": file("d1"), "content": "wiped\n"}, "destructive", "true")
		d2 := call("wipe-2", "target", "ansible.builtin.copy", map[string]any{"dest": file("d2"), "content": "wiped\n"}, "destructive", "true")
		writeIntent(t, append(append([]res{}, main...), add, d1, d2)...)
		out, _ := sakutei(t, "reconcile", "--max-destructive-per-cycle", "1", "--on-exceed", "halt-destructive-only")
		if read(t, "additive") != "a\n" {
			t.Fatalf("the additive call was not applied:\n%s", out)
		}
		for _, n := range []string{"d1", "d2"} {
			if _, err := os.Stat(file(n)); !os.IsNotExist(err) {
				t.Fatalf("%s: a destructive call over the budget was applied:\n%s", n, out)
			}
		}
		if !strings.Contains(strings.ToLower(out), "budget") {
			t.Fatalf("reconcile does not report the budget:\n%s", out)
		}
		mustReconcile(t, "--max-destructive-per-cycle", "1", "--approve-large-change", "2")
		for _, n := range []string{"d1", "d2"} {
			if _, err := os.Stat(file(n)); err != nil {
				t.Fatalf("%s after the approval: %v", n, err)
			}
		}
		if doc, _ := status(t); doc.Summary.Conformant != 7 {
			t.Fatalf("summary %+v", doc.Summary)
		}
	})

	t.Run("clean up", func(t *testing.T) {
		// state: absent でファイルの行を消す（provider は Delete をしない）。
		absent := call("motd", "target", "ansible.builtin.lineinfile",
			map[string]any{"path": file("motd"), "line": "managed by sakutei", "state": "absent"})
		writeIntent(t, absent)
		mustReconcile(t)
		if strings.Contains(read(t, "motd"), "managed by sakutei") {
			t.Fatalf("motd = %q", read(t, "motd"))
		}
		if doc, _ := status(t); doc.Summary.Conformant != 1 {
			t.Fatalf("summary %+v", doc.Summary)
		}
	})
}
