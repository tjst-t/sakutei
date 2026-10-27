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

// ansible_test.go — Backlog #127: the ansible provider (the minimal tier of
// §5.6 / §28.7: a module observed in check mode, applied by a normal run).
// The provider is driven against the fake runner (internal/ansible/ansiblefake)
// and, through the CLI, against a fake ad-hoc ansible executable
// (testdata/fakeansible).  The real ansible-core is exercised by
// tests/integration/ansible (make integration-ansible).
//
// AC tags:
//
//	AC-127-1  DefaultRegistry builds ansible from ansible_config; without it is
//	          an error; the provider passes ValidateProvider and the check-mode
//	          contracts of sdk.ContractTester (incl. unreachable → unknown)
//	AC-127-2  CLI: status shows the not-yet-applied call as drift → reconcile
//	          applies it → conformant; a hand edit is drift and reconcile puts it
//	          back; further cycles find nothing to do
//	AC-127-3  a host that cannot be reached, and a module without check mode,
//	          are unobservable: nothing is applied to them
//	AC-127-4  destructive: true is counted by the change budget: over the limit
//	          the destructive calls halt while the additive one is applied
//	AC-127-5  the core's degradation: no prune (List:false) — PruneWithinScope is
//	          rejected; a Delete is never sent
package acceptance

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/ansible"
	"github.com/tjst-t/sakutei/v2/internal/ansible/ansiblefake"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
	"github.com/tjst-t/sakutei/v2/pkg/registry"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
)

var (
	fakeAnsibleOnce sync.Once
	fakeAnsibleBin  string
	fakeAnsibleErr  error
)

// fakeAnsible builds testdata/fakeansible once.
func fakeAnsible(t *testing.T) string {
	t.Helper()
	fakeAnsibleOnce.Do(func() {
		fakeAnsibleBin = filepath.Join(binaryDir, "fakeansible")
		out, err := exec.Command("go", "build", "-o", fakeAnsibleBin, "./testdata/fakeansible").CombinedOutput()
		if err != nil {
			fakeAnsibleErr = err
			fakeAnsibleBin = string(out)
		}
	})
	if fakeAnsibleErr != nil {
		t.Fatalf("build fakeansible: %v\n%s", fakeAnsibleErr, fakeAnsibleBin)
	}
	return fakeAnsibleBin
}

// ansibleLab is one CLI run's directory: the inventory, the settings, the
// intent and the files the fake ansible changes.
type ansibleLab struct {
	t   *testing.T
	dir string
	cfg string
}

func newAnsibleLab(t *testing.T, inventory string) *ansibleLab {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("inventory.ini", inventory)
	cfg := "inventory: inventory.ini\nansible: " + fakeAnsible(t) + "\nenv:\n  FAKE_ANSIBLE_LOG: " + filepath.Join(dir, "runs.log") + "\nmax_parallel: 2\n"
	write("ansible.yaml", cfg)
	if err := os.MkdirAll(filepath.Join(dir, "intent"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &ansibleLab{t: t, dir: dir, cfg: filepath.Join(dir, "ansible.yaml")}
}

func (l *ansibleLab) path(name string) string { return filepath.Join(l.dir, name) }

// lineRes is a lineinfile call that keeps line in file (under the lab dir).
func (l *ansibleLab) lineRes(id, host, file, line string, extra map[string]string) map[string]any {
	args, _ := json.Marshal(map[string]any{"path": l.path(file), "line": line, "create": true})
	fields := map[string]string{"host": host, "module": "ansible.builtin.lineinfile", "args": string(args)}
	for k, v := range extra {
		fields[k] = v
	}
	return map[string]any{"provider": "ansible", "id": id, "fields": fields}
}

func (l *ansibleLab) intent(rs ...map[string]any) {
	l.t.Helper()
	b, _ := json.MarshalIndent(map[string]any{"resources": rs}, "", "  ")
	if err := os.WriteFile(filepath.Join(l.dir, "intent", "ansible.json"), b, 0o644); err != nil {
		l.t.Fatal(err)
	}
}

func (l *ansibleLab) run(verb string, extra ...string) (string, int) {
	l.t.Helper()
	args := append([]string{verb, "--intent", l.path("intent"), "--state", l.path("fake-state.json"),
		"--cause-state", l.path("cause-state.json"), "--ansible-config", l.cfg}, extra...)
	out, err := exec.Command(buildBinary(l.t), args...).CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	} else if err != nil {
		l.t.Fatalf("sakutei %s: %v", verb, err)
	}
	return string(out), 0
}

type ansibleStatus struct {
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

func (l *ansibleLab) status() ansibleStatus {
	l.t.Helper()
	cmd := exec.Command(buildBinary(l.t), "status", "--json", "--intent", l.path("intent"), "--state", l.path("fake-state.json"),
		"--cause-state", l.path("cause-state.json"), "--ansible-config", l.cfg)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if _, isExit := err.(*exec.ExitError); err != nil && !isExit {
		l.t.Fatalf("status: %v", err)
	}
	var doc ansibleStatus
	if jerr := json.Unmarshal(out, &doc); jerr != nil {
		l.t.Fatalf("status --json: %v\n%s%s", jerr, out, stderr.String())
	}
	return doc
}

func (l *ansibleLab) stateOf(doc ansibleStatus, id string) string {
	for _, r := range doc.Resources {
		if r.LocalID == id {
			return r.State
		}
	}
	return ""
}

func (l *ansibleLab) file(name string) string {
	b, _ := os.ReadFile(l.path(name))
	return string(b)
}

func (l *ansibleLab) applies() int {
	return strings.Count(l.file("runs.log"), "apply ")
}

// AC-127-1
func TestAnsible_RegistryAndContract(t *testing.T) {
	reg := registry.DefaultRegistry()
	if _, err := reg.Get(ansible.Name, map[string]string{}); err == nil || !strings.Contains(err.Error(), "--ansible-config") {
		t.Fatalf("without ansible_config: err = %v", err)
	}
	lab := newAnsibleLab(t, "web1\n")
	p, err := reg.Get(ansible.Name, map[string]string{"ansible_config": lab.cfg})
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.ValidateProvider(p); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(provider.CheckReader); !ok || p.Capabilities().TrueObserve {
		t.Fatal("ansible must be a CheckReader without TrueObserve")
	}
	// the registry-built provider against the fake executable
	sample := lab.lineRes("motd", "web1", "motd", "hello", nil)["fields"].(map[string]string)
	ct := sdk.ContractTester{Provider: p, Scope: core.NewScope(nil), SampleID: "motd", SampleDesired: sample}
	ct.RunAll(t)

	fake := ansiblefake.New("web1")
	ct = sdk.ContractTester{
		Provider: ansible.New(fake, ansible.Options{}), Scope: core.NewScope(nil), SampleID: "motd",
		SampleDesired: map[string]string{"host": "web1", "module": "ansible.builtin.copy", "args": `{"dest": "/etc/motd", "content": "hi\n"}`},
		MakeUnreachable: func() (func(), error) {
			fake.Unreachable["web1"] = true
			return func() { fake.Unreachable["web1"] = false }, nil
		},
	}
	ct.RunAll(t)
}

// AC-127-2
func TestAnsible_CLIConvergesDriftsAndComesBack(t *testing.T) {
	lab := newAnsibleLab(t, "web1\nweb2\n")
	lab.intent(lab.lineRes("motd-web1", "web1", "motd1", "managed by sakutei", nil),
		lab.lineRes("motd-web2", "web2", "motd2", "managed by sakutei", nil))

	doc := lab.status()
	if doc.Summary.Drift != 2 || doc.Summary.Conformant != 0 {
		t.Fatalf("before apply: summary = %+v, want drift 2", doc.Summary)
	}
	if out, code := lab.run("plan"); code != 0 || !strings.Contains(out, "+managed by sakutei") {
		t.Fatalf("plan (code %d) does not show the check-mode diff:\n%s", code, out)
	}
	if lab.applies() != 0 {
		t.Fatal("status / plan applied something")
	}
	if out, code := lab.run("reconcile"); code != 0 {
		t.Fatalf("reconcile exited %d:\n%s", code, out)
	}
	if lab.file("motd1") != "managed by sakutei\n" || lab.file("motd2") != "managed by sakutei\n" {
		t.Fatalf("files after reconcile: %q %q", lab.file("motd1"), lab.file("motd2"))
	}
	if doc := lab.status(); doc.Summary.Conformant != 2 || doc.Summary.Drift != 0 {
		t.Fatalf("after reconcile: summary = %+v", doc.Summary)
	}

	if err := os.WriteFile(lab.path("motd1"), []byte("edited by hand\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	doc = lab.status()
	if lab.stateOf(doc, "motd-web1") != "drift" || lab.stateOf(doc, "motd-web2") != "conformant" {
		t.Fatalf("after the hand edit: %+v", doc.Resources)
	}
	if out, code := lab.run("reconcile"); code != 0 {
		t.Fatalf("reconcile exited %d:\n%s", code, out)
	}
	if lab.file("motd1") != "edited by hand\nmanaged by sakutei\n" {
		t.Fatalf("motd1 = %q", lab.file("motd1"))
	}
	before := lab.applies()
	for i := 0; i < 3; i++ {
		if out, code := lab.run("reconcile"); code != 0 {
			t.Fatalf("cycle %d: reconcile exited %d:\n%s", i, code, out)
		}
	}
	if lab.applies() != before {
		t.Fatalf("converged cycles applied %d calls", lab.applies()-before)
	}
	if doc := lab.status(); doc.Summary.Conformant != 2 {
		t.Fatalf("summary = %+v", doc.Summary)
	}
}

// AC-127-3
func TestAnsible_UnobservableDoesNothing(t *testing.T) {
	lab := newAnsibleLab(t, "web1\nweb2 fake_unreachable=1\n")
	cmd := map[string]any{"provider": "ansible", "id": "uptime", "fields": map[string]string{
		"host": "web1", "module": "ansible.builtin.command", "args": `{"cmd": "uptime"}`}}
	lab.intent(lab.lineRes("motd-web2", "web2", "motd2", "x", nil), cmd)
	doc := lab.status()
	if doc.Summary.Unobservable != 2 || doc.Summary.Drift != 0 {
		t.Fatalf("summary = %+v, want 2 unobservable", doc.Summary)
	}
	out, _ := lab.run("reconcile")
	if lab.applies() != 0 {
		t.Fatalf("an unobservable call was applied:\n%s", out)
	}
	if !strings.Contains(out, "unreachable") || !strings.Contains(out, "check mode") {
		t.Fatalf("reconcile does not say why:\n%s", out)
	}
}

// AC-127-4
func TestAnsible_DestructiveIsBudgeted(t *testing.T) {
	lab := newAnsibleLab(t, "web1\n")
	d := map[string]string{"destructive": "true"}
	lab.intent(lab.lineRes("add", "web1", "a", "additive", nil),
		lab.lineRes("d1", "web1", "d1", "one", d), lab.lineRes("d2", "web1", "d2", "two", d))
	out, _ := lab.run("reconcile", "--max-destructive-per-cycle", "1", "--on-exceed", "halt-destructive-only")
	if lab.file("a") != "additive\n" {
		t.Fatalf("the additive call was not applied:\n%s", out)
	}
	if lab.file("d1") != "" || lab.file("d2") != "" {
		t.Fatalf("destructive calls over the budget were applied:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "budget") {
		t.Fatalf("reconcile does not report the budget halt:\n%s", out)
	}
	if out, code := lab.run("reconcile", "--approve-large-change", "2"); code != 0 || lab.file("d1") != "one\n" || lab.file("d2") != "two\n" {
		t.Fatalf("approved: code %d, d1 %q d2 %q\n%s", code, lab.file("d1"), lab.file("d2"), out)
	}
}

// AC-127-5
func TestAnsible_NoPrune(t *testing.T) {
	lab := newAnsibleLab(t, "web1\n")
	r := lab.lineRes("motd", "web1", "motd", "x", nil)
	r["deletion_policy"] = "PruneWithinScope"
	lab.intent(r)
	out, code := lab.run("reconcile")
	if code == 0 || lab.applies() != 0 {
		t.Fatalf("PruneWithinScope on a provider without List was accepted (code %d):\n%s", code, out)
	}
	p := ansible.New(ansiblefake.New("web1"), ansible.Options{})
	if err := p.Apply(context.Background(), core.Plan{Ops: []core.Op{{Kind: core.OpKindDelete,
		Target: core.NewID(ansible.Name, core.NewScope(nil), "motd")}}}); err == nil {
		t.Fatal("a Delete was accepted")
	}
}
