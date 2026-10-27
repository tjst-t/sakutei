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

// Command fakeansible は受け入れテスト（tests/acceptance/ansible_test.go）が
// sakutei の CLI に ansible として渡す偽の ad-hoc の ansible。本物と同じ
// 引数（<host> -i <inventory> -m <module> [-e @<vars> -a <tmpl>] [--check --diff]、
// または <pattern> -i <inventory> --list-hosts）を受け、local 接続のように
// この機械のファイルを相手にする:
//
//   - ansible.builtin.lineinfile（path・line・state・create）と
//     ansible.builtin.copy（dest・content）。check mode では変えずに changed を返す。
//   - ansible.builtin.command は check mode を持たない（skipped）。
//   - inventory は 1 行 1 ホスト（[name] はグループ）。行に fake_unreachable=1 が
//     あれば、そのホストには届かない（終了コード 4、本物と同じ）。
//
// 結果は provider の stdout callback と同じ 1 行の JSON で出す（ANSIBLE_STDOUT_CALLBACK
// が sakutei_json でなければ断る）。FAKE_ANSIBLE_LOG があれば、走るたびに
// "<check|apply> <host> <module>" を追記する。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const prefix = "SAKUTEI-RESULT "

type result struct {
	Host    string `json:"host"`
	Status  string `json:"status"`
	Changed bool   `json:"changed"`
	Msg     string `json:"msg"`
	Diff    string `json:"diff"`
}

func main() { os.Exit(run(os.Args[1:])) }

func run(argv []string) int {
	if len(argv) < 3 || argv[1] != "-i" {
		fmt.Fprintln(os.Stderr, "fakeansible: usage: <pattern> -i <inventory> ...")
		return 5
	}
	pattern := argv[0]
	hosts, groups, unreachable, err := inventory(argv[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakeansible:", err)
		return 1
	}
	matched := groups[pattern]
	if matched == nil && hosts[pattern] {
		matched = []string{pattern}
	}
	rest := argv[3:]
	if len(rest) == 1 && rest[0] == "--list-hosts" {
		fmt.Printf("  hosts (%d):\n", len(matched))
		for _, h := range matched {
			fmt.Println("    " + h)
		}
		return 0
	}
	if os.Getenv("ANSIBLE_STDOUT_CALLBACK") != "sakutei_json" {
		fmt.Fprintln(os.Stderr, "fakeansible: ANSIBLE_STDOUT_CALLBACK is not sakutei_json")
		return 5
	}
	var module, varsFile string
	check := false
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "-m":
			i++
			module = rest[i]
		case "-e":
			i++
			varsFile = strings.TrimPrefix(rest[i], "@")
		case "-a":
			i++
		case "--check":
			check = true
		}
	}
	args := map[string]any{}
	if varsFile != "" {
		b, err := os.ReadFile(varsFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fakeansible:", err)
			return 1
		}
		// provider の書く flow 形式の YAML は、!unsafe を落とせば JSON。
		var doc map[string]map[string]any
		if err := json.Unmarshal([]byte(strings.ReplaceAll(string(b), "!unsafe ", "")), &doc); err != nil {
			fmt.Fprintln(os.Stderr, "fakeansible: vars:", err)
			return 1
		}
		args = doc["__sakutei_args"]
	}
	if len(matched) == 0 {
		fmt.Fprintln(os.Stderr, "[WARNING]: Could not match supplied host pattern, ignoring: "+pattern)
		return 0
	}
	code := 0
	for _, h := range matched {
		if p := os.Getenv("FAKE_ANSIBLE_LOG"); p != "" {
			verb := "apply"
			if check {
				verb = "check"
			}
			if f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				fmt.Fprintf(f, "%s %s %s\n", verb, h, module)
				f.Close()
			}
		}
		r := result{Host: h, Status: "ok"}
		switch {
		case unreachable[h]:
			r.Status, r.Msg = "unreachable", "Task failed: Failed to connect to the host via ssh: Connection refused"
			code = 4
		case module == "ansible.builtin.lineinfile":
			r = lineinfile(r, args, check)
		case module == "ansible.builtin.copy":
			r = copyFile(r, args, check)
		case module == "ansible.builtin.command" && check:
			r.Status, r.Msg = "skipped", "Command would have run if not in check mode"
		default:
			r.Status, r.Msg = "failed", "Task failed: Cannot resolve '"+module+"' to an action or module."
		}
		if r.Status == "failed" && code == 0 {
			code = 2
		}
		b, _ := json.Marshal(r)
		fmt.Println(prefix + string(b))
	}
	return code
}

func inventory(path string) (hosts map[string]bool, groups map[string][]string, unreachable map[string]bool, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, nil, err
	}
	hosts, groups, unreachable = map[string]bool{}, map[string][]string{}, map[string]bool{}
	group := ""
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			group = strings.Trim(line, "[]")
			continue
		}
		f := strings.Fields(line)
		hosts[f[0]] = true
		if group != "" {
			groups[group] = append(groups[group], f[0])
		}
		if strings.Contains(line, "fake_unreachable=1") {
			unreachable[f[0]] = true
		}
	}
	return hosts, groups, unreachable, nil
}

func str(args map[string]any, k string) string {
	s, _ := args[k].(string)
	return s
}

func lineinfile(r result, args map[string]any, check bool) result {
	path, line := str(args, "path"), str(args, "line")
	absent := str(args, "state") == "absent"
	create, _ := args["create"].(bool)
	b, err := os.ReadFile(path)
	if err != nil && !(os.IsNotExist(err) && (create || absent)) {
		r.Status, r.Msg = "failed", fmt.Sprintf("Destination %s does not exist !", path)
		return r
	}
	var out []string
	found := false
	for _, l := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if l == "" && len(b) == 0 {
			continue
		}
		if l == line {
			found = true
			if absent {
				continue
			}
		}
		out = append(out, l)
	}
	switch {
	case absent && !found, !absent && found:
		return r
	case !absent:
		out = append(out, line)
		r.Diff = fmt.Sprintf("--- before: %s\n+++ after: %s\n+%s\n", path, path, line)
	default:
		r.Diff = fmt.Sprintf("--- before: %s\n+++ after: %s\n-%s\n", path, path, line)
	}
	r.Changed = true
	if !check {
		content := strings.Join(out, "\n")
		if content != "" {
			content += "\n"
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			r.Status, r.Msg = "failed", err.Error()
		}
	}
	return r
}

func copyFile(r result, args map[string]any, check bool) result {
	dest, content := str(args, "dest"), str(args, "content")
	if b, err := os.ReadFile(dest); err == nil && string(b) == content {
		return r
	}
	r.Changed = true
	if !check {
		if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
			r.Status, r.Msg = "failed", err.Error()
		}
	}
	return r
}
