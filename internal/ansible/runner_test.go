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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain は、SAKUTEI_FAKE_ANSIBLE が立っていれば偽の ansible として振る舞う
// （ExecRunner.Path にこのテストのバイナリを渡す）。
func TestMain(m *testing.M) {
	if mode := os.Getenv("SAKUTEI_FAKE_ANSIBLE"); mode != "" {
		os.Exit(fakeAnsible(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeAnsible は ExecRunner が渡したものを確かめ、受け取ったものを msg に入れて
// 結果の行を出す。
func fakeAnsible(mode string, argv []string) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(os.Stderr, format+"\n", a...)
		return 99
	}
	if os.Getenv("ANSIBLE_NOCOLOR") != "1" {
		return fail("ANSIBLE_NOCOLOR is not set")
	}
	if len(argv) >= 4 && argv[len(argv)-1] == "--list-hosts" {
		fmt.Println("  hosts (2):\n    web1\n    web2")
		return 0
	}
	if os.Getenv("ANSIBLE_STDOUT_CALLBACK") != "sakutei_json" || os.Getenv("ANSIBLE_LOAD_CALLBACK_PLUGINS") != "1" {
		return fail("the callback is not configured")
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("ANSIBLE_CALLBACK_PLUGINS"), "sakutei_json.py")); err != nil {
		return fail("callback plugin: %v", err)
	}
	got := map[string]any{"argv": argv, "custom": os.Getenv("SAKUTEI_CUSTOM")}
	for i, a := range argv {
		if a == "-e" && i+1 < len(argv) {
			b, err := os.ReadFile(strings.TrimPrefix(argv[i+1], "@"))
			if err != nil {
				return fail("vars: %v", err)
			}
			st, _ := os.Stat(strings.TrimPrefix(argv[i+1], "@"))
			got["vars"] = string(b)
			got["mode"] = st.Mode().Perm().String()
		}
	}
	msg, _ := json.Marshal(got)
	line := func(host, status string, changed bool) {
		b, _ := json.Marshal(Result{Host: host, Status: status, Changed: changed, Msg: string(msg), Diff: "+x\n"})
		fmt.Println("[WARNING]: noise on stdout")
		fmt.Println(resultPrefix + string(b))
	}
	switch mode {
	case "ok":
		line(argv[0], StatusOK, true)
	case "unreachable":
		line(argv[0], StatusUnreachable, false)
		return 4
	case "none":
		fmt.Fprintln(os.Stderr, "[WARNING]: Could not match supplied host pattern, ignoring: "+argv[0])
	case "two":
		line("web1", StatusOK, false)
		line("web2", StatusOK, false)
	case "other":
		line("web2", StatusOK, false)
	case "crash":
		fmt.Fprintln(os.Stderr, "ERROR! Unexpected Exception")
		return 250
	case "slow":
		time.Sleep(10 * time.Second)
	case "garbage":
		fmt.Println(resultPrefix + "{not json")
	}
	return 0
}

func fakeRunner(t *testing.T, mode string) ExecRunner {
	t.Helper()
	return ExecRunner{Path: os.Args[0], Inventory: "/inv.ini", Timeout: 5 * time.Second,
		Env: map[string]string{"SAKUTEI_FAKE_ANSIBLE": mode, "SAKUTEI_CUSTOM": "yes"}}
}

func TestExecRunnerArgv(t *testing.T) {
	args := map[string]any{"path": "/etc/motd", "line": "{{ 7*6 }} secret-value", "create": true, "mode": json.Number("420")}
	res, err := fakeRunner(t, "ok").Run(context.Background(), Call{Host: "web1", Module: "ansible.builtin.lineinfile", Args: args, Check: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusOK || !res.Changed || res.Host != "web1" || res.Diff != "+x\n" {
		t.Fatalf("res = %+v", res)
	}
	var got struct {
		Argv   []string
		Custom string
		Vars   string
		Mode   string
	}
	if err := json.Unmarshal([]byte(res.Msg), &got); err != nil {
		t.Fatal(err)
	}
	argv := strings.Join(got.Argv, " ")
	if strings.Contains(argv, "secret-value") {
		t.Fatalf("an argument value is on the command line: %s", argv)
	}
	for _, want := range []string{"web1 -i /inv.ini -m ansible.builtin.lineinfile -e @", "--check --diff"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("argv %q lacks %q", argv, want)
		}
	}
	if got.Custom != "yes" {
		t.Fatal("env from the config was not passed")
	}
	if got.Mode != "-rw-------" {
		t.Fatalf("args file mode = %s", got.Mode)
	}
	want := `{"__sakutei_args": {"create": true, "line": !unsafe "{{ 7*6 }} secret-value", "mode": 420, "path": !unsafe "/etc/motd"}}` + "\n"
	if got.Vars != want {
		t.Fatalf("vars file =\n%s\nwant\n%s", got.Vars, want)
	}
	i := len(got.Argv) - 3
	var tmpl map[string]string
	if err := json.Unmarshal([]byte(got.Argv[i]), &tmpl); err != nil || tmpl["line"] != "{{ __sakutei_args['line'] }}" || len(tmpl) != 4 {
		t.Fatalf("-a = %q (%v)", got.Argv[i], err)
	}

	// 適用は --check を付けない。引数が無ければ -e も -a も付けない。
	res, err = fakeRunner(t, "ok").Run(context.Background(), Call{Host: "web1", Module: "ansible.builtin.ping"})
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(res.Msg), &got)
	if a := strings.Join(got.Argv, " "); a != "web1 -i /inv.ini -m ansible.builtin.ping" {
		t.Fatalf("argv = %q", a)
	}
}

func TestExecRunnerOutcomes(t *testing.T) {
	c := Call{Host: "web1", Module: "ansible.builtin.ping", Check: true}
	res, err := fakeRunner(t, "unreachable").Run(context.Background(), c)
	if err != nil || res.Status != StatusUnreachable {
		t.Fatalf("unreachable: %+v %v", res, err)
	}
	for mode, want := range map[string]string{
		"none":    "not in the inventory",
		"two":     "matched 2 hosts",
		"other":   `matched the host "web2"`,
		"crash":   "exited with code 250: ERROR! Unexpected Exception",
		"garbage": "not JSON",
	} {
		if _, err := fakeRunner(t, mode).Run(context.Background(), c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", mode, err, want)
		}
	}
	r := fakeRunner(t, "slow")
	r.Timeout = 200 * time.Millisecond
	start := time.Now()
	if _, err := r.Run(context.Background(), c); err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("slow: err = %v", err)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatalf("the timeout did not stop ansible (%s)", time.Since(start))
	}
	hosts, err := fakeRunner(t, "ok").Hosts(context.Background(), "web")
	if err != nil || strings.Join(hosts, ",") != "web1,web2" {
		t.Fatalf("hosts = %v %v", hosts, err)
	}
	if _, err := (ExecRunner{Path: filepath.Join(t.TempDir(), "no-ansible"), Inventory: "x"}).Run(context.Background(), c); err == nil {
		t.Fatal("a missing ansible: no error")
	}
}

func TestParseListHosts(t *testing.T) {
	h, err := parseListHosts([]byte("  hosts (0):\n"))
	if err != nil || len(h) != 0 {
		t.Fatalf("%v %v", h, err)
	}
	if _, err := parseListHosts([]byte("garbage")); err == nil {
		t.Fatal("no error")
	}
}

func TestParseConfig(t *testing.T) {
	cfg, err := parseConfig(map[string]any{"inventory": "inv.ini", "ansible": "venv/bin/ansible", "env": map[string]any{"TMPDIR": "/tmp"},
		"timeout": "30s", "max_parallel": 2, "secret_store": "s.json"}, "/etc/sakutei")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Inventory != "/etc/sakutei/inv.ini" || cfg.Ansible != "/etc/sakutei/venv/bin/ansible" || cfg.SecretStore != "/etc/sakutei/s.json" ||
		cfg.Timeout != 30*time.Second || cfg.MaxParallel != 2 || cfg.Env["TMPDIR"] != "/tmp" {
		t.Fatalf("cfg = %+v", cfg)
	}
	cfg, err = parseConfig(map[string]any{"inventory": "/inv", "ansible": "ansible"}, "/etc")
	if err != nil || cfg.Ansible != "ansible" || cfg.Timeout != DefaultTimeout || cfg.MaxParallel != DefaultMaxParallel {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
	for name, tree := range map[string]map[string]any{
		"no inventory": {},
		"unknown key":  {"inventory": "x", "invetory": "y"},
		"forced env":   {"inventory": "x", "env": map[string]any{"ANSIBLE_STDOUT_CALLBACK": "json"}},
		"bad timeout":  {"inventory": "x", "timeout": "soon"},
		"bad parallel": {"inventory": "x", "max_parallel": 0},
	} {
		if _, err := parseConfig(tree, "/"); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
