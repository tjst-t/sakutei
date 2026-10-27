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

// Package ansiblefake は ansible provider（internal/ansible）を通常のテストで
// 回すための偽の ansible（ansible.Runner、Backlog #127）。ホストごとにメモリの
// ファイルを持ち、本物のモジュールのうち次の 2 つを真似る（check mode では
// 何も変えずに changed と差分だけを返す）:
//
//   - ansible.builtin.lineinfile: path・line・state（present / absent）・create。
//     present は line と同じ行が無ければ末尾に足す。absent は同じ行を消す。
//     ファイルが無く create でなければ失敗（本物と同じ）。
//   - ansible.builtin.copy: dest・content。中身が違えば置き換える。
//
// ansible.builtin.command は check mode を持たない（本物と同じく skipped）。
// それ以外のモジュールは "Cannot resolve" で失敗する。Unreachable のホストは
// 届かない、Groups はグループ名（Hosts がその全員を返す）。
package ansiblefake

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/tjst-t/sakutei/internal/ansible"
)

// Ansible は偽の ansible。ゼロ値は使えない（New を使う）。
type Ansible struct {
	mu    sync.Mutex
	files map[string]map[string]string // host → path → 中身

	// Unreachable のホストにはどの呼び出しも届かない。
	Unreachable map[string]bool
	// Groups はグループ名 → ホスト名。
	Groups map[string][]string
	// Down が nil でなければ、どの呼び出しもこの error で失敗する（ansible が走らない）。
	Down error

	// Calls は Run の回数（check の数と適用の数）。
	Checks, Applies int
	// LastArgs は最後の Run の引数（解決済み）。
	LastArgs            map[string]any
	running, maxRunning int
	// Block が nil でなければ、Run はそれが閉じられるまで待つ（同時に走る数を見る）。
	Block chan struct{}
}

var _ ansible.Runner = (*Ansible)(nil)

// New は hosts を持つ偽の ansible を返す（どのホストもファイルは空）。
func New(hosts ...string) *Ansible {
	a := &Ansible{files: map[string]map[string]string{}, Unreachable: map[string]bool{}, Groups: map[string][]string{}}
	for _, h := range hosts {
		a.files[h] = map[string]string{}
	}
	return a
}

// File はホストのファイルの中身（無ければ ok=false）。
func (a *Ansible) File(host, path string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.files[host][path]
	return s, ok
}

// SetFile はホストのファイルを手で変える（sakutei の外の変更）。
func (a *Ansible) SetFile(host, path, content string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.files[host][path] = content
}

// MaxRunning は同時に走った Run の最大の数。
func (a *Ansible) MaxRunning() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.maxRunning
}

// Hosts implements ansible.Runner.
func (a *Ansible) Hosts(_ context.Context, pattern string) ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Down != nil {
		return nil, a.Down
	}
	if g, ok := a.Groups[pattern]; ok {
		return append([]string(nil), g...), nil
	}
	if _, ok := a.files[pattern]; ok {
		return []string{pattern}, nil
	}
	return nil, nil
}

// Run implements ansible.Runner.
func (a *Ansible) Run(ctx context.Context, call ansible.Call) (ansible.Result, error) {
	a.mu.Lock()
	a.running++
	if a.running > a.maxRunning {
		a.maxRunning = a.running
	}
	block := a.Block
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.running--
		a.mu.Unlock()
	}()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ansible.Result{}, ctx.Err()
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Down != nil {
		return ansible.Result{}, a.Down
	}
	if call.Check {
		a.Checks++
	} else {
		a.Applies++
	}
	a.LastArgs = call.Args
	if g, ok := a.Groups[call.Host]; ok {
		return ansible.Result{}, fmt.Errorf("ansible: host %q matched %d hosts (%s); a resource is one host", call.Host, len(g), strings.Join(g, ", "))
	}
	files, ok := a.files[call.Host]
	if !ok {
		return ansible.Result{}, fmt.Errorf("ansible: no result for host %q: it is not in the inventory", call.Host)
	}
	res := ansible.Result{Host: call.Host, Status: ansible.StatusOK}
	if a.Unreachable[call.Host] {
		res.Status = ansible.StatusUnreachable
		res.Msg = "Task failed: Failed to connect to the host via ssh: Connection refused"
		return res, nil
	}
	switch call.Module {
	case "ansible.builtin.lineinfile":
		return lineinfile(files, call, res), nil
	case "ansible.builtin.copy":
		return copyModule(files, call, res), nil
	case "ansible.builtin.command":
		if call.Check {
			res.Status = ansible.StatusSkipped
			res.Msg = "Command would have run if not in check mode"
			return res, nil
		}
		res.Changed = true
		return res, nil
	default:
		res.Status = ansible.StatusFailed
		res.Msg = fmt.Sprintf("Task failed: Cannot resolve '%s' to an action or module.", call.Module)
		return res, nil
	}
}

func str(args map[string]any, k string) string {
	s, _ := args[k].(string)
	return s
}

func lineinfile(files map[string]string, call ansible.Call, res ansible.Result) ansible.Result {
	path, line := str(call.Args, "path"), str(call.Args, "line")
	state := str(call.Args, "state")
	if state == "" {
		state = "present"
	}
	create, _ := call.Args["create"].(bool)
	cur, exists := files[path]
	if !exists && !create && state == "present" {
		res.Status = ansible.StatusFailed
		res.Msg = fmt.Sprintf("Destination %s does not exist !", path)
		return res
	}
	var lines []string
	if cur != "" {
		lines = strings.Split(strings.TrimSuffix(cur, "\n"), "\n")
	}
	var out []string
	found := false
	for _, l := range lines {
		if l == line {
			found = true
			if state == "absent" {
				continue
			}
		}
		out = append(out, l)
	}
	if state == "present" && !found {
		out = append(out, line)
	}
	next := ""
	if len(out) > 0 {
		next = strings.Join(out, "\n") + "\n"
	}
	return change(files, path, cur, next, exists || state == "present", call, res)
}

func copyModule(files map[string]string, call ansible.Call, res ansible.Result) ansible.Result {
	dest := str(call.Args, "dest")
	if dest == "" {
		res.Status = ansible.StatusFailed
		res.Msg = "missing required arguments: dest"
		return res
	}
	cur := files[dest]
	_, exists := files[dest]
	if exists && cur == str(call.Args, "content") {
		return res
	}
	return change(files, dest, cur, str(call.Args, "content"), true, call, res)
}

// change は path を next にする（check なら変えない）。差分は本物の --diff に似た形。
func change(files map[string]string, path, cur, next string, write bool, call ansible.Call, res ansible.Result) ansible.Result {
	_, exists := files[path]
	if exists && cur == next {
		return res
	}
	if !exists && !write {
		return res
	}
	res.Changed = true
	res.Diff = fmt.Sprintf("--- before: %s\n+++ after: %s\n%s", path, path, lineDiff(cur, next))
	if !call.Check {
		files[path] = next
	}
	return res
}

func lineDiff(a, b string) string {
	in := map[string]bool{}
	for _, l := range strings.Split(a, "\n") {
		in[l] = true
	}
	var out []string
	for _, l := range strings.Split(b, "\n") {
		if l != "" && !in[l] {
			out = append(out, "+"+l)
		}
	}
	inB := map[string]bool{}
	for _, l := range strings.Split(b, "\n") {
		inB[l] = true
	}
	for _, l := range strings.Split(a, "\n") {
		if l != "" && !inB[l] {
			out = append(out, "-"+l)
		}
	}
	sort.Strings(out)
	return strings.Join(out, "\n") + "\n"
}
