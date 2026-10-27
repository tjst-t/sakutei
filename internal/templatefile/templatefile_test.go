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

package templatefile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/internal/cause"
	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
	"github.com/tjst-t/sakutei/pkg/sdk"
)

// trialTemplates は docs の Alpine の雛形の置き場（試用と統合テストが使うもの）。
const trialTemplates = "../../docs/trial/redfish-os-install/templates"

const trialTemplate = "alpine-answers.sh.tmpl"

func newTrial(t *testing.T) (*Provider, string) {
	t.Helper()
	out := t.TempDir()
	p, err := New(trialTemplates, out)
	if err != nil {
		t.Fatal(err)
	}
	return p, out
}

func answerFields() map[string]string {
	return map[string]string{
		FieldTemplate:          trialTemplate,
		FieldHostname:          "node1",
		FieldAddress:           "10.0.2.15/24",
		FieldSSHAuthorizedKeys: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample sakutei",
		FieldInstallMark:       "install-1",
		"var.gateway":          "10.0.2.2",
	}
}

func TestValidateProvider(t *testing.T) {
	p, _ := newTrial(t)
	if err := provider.ValidateProvider(p); err != nil {
		t.Fatalf("ValidateProvider: %v", err)
	}
	if err := sdk.ValidateProvider(p); err != nil {
		t.Fatalf("sdk.ValidateProvider (the daemon's lint): %v", err)
	}
	if c := p.Capabilities(); c.TrueObserve || c.List || c.Pruneable {
		t.Errorf("Capabilities = %+v, want the minimal tier without List / prune", c)
	}
	if _, err := New("", "x"); err == nil {
		t.Errorf("New without template_dir = nil error")
	}
	if _, err := New("x", ""); err == nil {
		t.Errorf("New without output_dir = nil error")
	}
}

// 宣言から応答ファイルが書き出され、同じ宣言なら適合、手で直されたり雛形の値が
// 変わったりすれば書き直す。
func TestRenderWriteAndConverge(t *testing.T) {
	p, out := newTrial(t)
	ctx := context.Background()
	id := core.NewID(Name, core.NewScope(nil), "answers/6f1b7c52-0e1a-4d2b-9c11-000000008601")
	fields := answerFields()
	if err := p.ValidateIntent(id.Local, fields); err != nil {
		t.Fatalf("ValidateIntent: %v", err)
	}
	if _, err := p.Read(ctx, id.Local); err == nil {
		t.Errorf("Read without the desired = nil error, want an error (minimal tier)")
	}

	d := diff.ForProvider(p)
	converge := func() core.Plan {
		t.Helper()
		obs, err := provider.ReadFor(ctx, p, id.Local, fields)
		if err != nil {
			t.Fatalf("ReadCheck: %v", err)
		}
		plan, err := d.Plan(obs, diff.DesiredResource{ID: id, Fields: fields}, cause.CauseContext{})
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Ops) > 0 {
			if err := p.Apply(ctx, plan); err != nil {
				t.Fatalf("Apply: %v", err)
			}
		}
		return plan
	}

	if plan := converge(); len(plan.Ops) != 1 || plan.Ops[0].Destructive {
		t.Fatalf("first plan = %+v, want one non-destructive write", plan)
	}
	file := filepath.Join(out, "answers", "6f1b7c52-0e1a-4d2b-9c11-000000008601")
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"HOSTNAME='node1'\n", "ADDRESS='10.0.2.15/24'\n", "GATEWAY='10.0.2.2'\n", "MARK='install-1'\n",
		"SSH_AUTHORIZED_KEYS='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample sakutei'\n", "FAIL=''\n",
		"# resource: answers/6f1b7c52-0e1a-4d2b-9c11-000000008601\n",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("answer file lacks %q:\n%s", want, got)
		}
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644", fi.Mode().Perm())
	}
	if plan := converge(); len(plan.Ops) != 0 {
		t.Fatalf("second plan = %+v, want converged", plan)
	}

	// 手で直された → 書き直す。
	if err := os.WriteFile(file, []byte("HOSTNAME=evil\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if plan := converge(); len(plan.Ops) != 1 || !strings.Contains(plan.Ops[0].Diagnostic, "differs") {
		t.Fatalf("plan after a hand edit = %+v, want a rewrite", plan)
	}
	if got, _ := os.ReadFile(file); !strings.Contains(string(got), "HOSTNAME='node1'") {
		t.Fatalf("not rewritten: %s", got)
	}

	// 宣言の値が変わった → 書き直す。sh の引用符も守る。
	fields["var.fail"] = "it's"
	if plan := converge(); len(plan.Ops) != 1 {
		t.Fatalf("plan after a value change = %+v", plan)
	}
	if got, _ := os.ReadFile(file); !strings.Contains(string(got), `FAIL='it'\''s'`) {
		t.Fatalf("shquote: %s", got)
	}
}

func TestValidateIntentRejects(t *testing.T) {
	p, _ := newTrial(t)
	tmpl := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpl, "uses-missing.tmpl"), []byte("{{.Vars.nothere}}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpl, "broken.tmpl"), []byte("{{.Hostname"), 0o644); err != nil {
		t.Fatal(err)
	}
	q, err := New(tmpl, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	with := func(k, v string) map[string]string {
		f := answerFields()
		f[k] = v
		return f
	}
	for _, c := range []struct {
		p      *Provider
		id     string
		fields map[string]string
		want   string
	}{
		{p, "/etc/passwd", answerFields(), "relative path"},
		{p, "../x", answerFields(), "relative path"},
		{p, "a/../../x", answerFields(), "relative path"},
		{p, "a", with(FieldTemplate, "../templatefile.go"), "relative path"},
		{p, "a", with(FieldTemplate, ""), "template is required"},
		{p, "a", with(FieldTemplate, "nope.tmpl"), "no such file"},
		{p, "a", with("colour", "x"), "unknown field"},
		{p, "a", with(FieldPath, "b"), "must equal the resource id"},
		{p, "a", with("var.bad-name", "x"), "letters, digits"},
		{p, "a", with(FieldSSHAuthorizedKeys, "-----BEGIN OPENSSH PRIVATE KEY-----\nxx"), "private key"},
		{p, "a", with("var.password", "secretref:file/root"), "SecretRef"},
		{q, "a", map[string]string{FieldTemplate: "uses-missing.tmpl"}, "nothere"},
		{q, "a", map[string]string{FieldTemplate: "broken.tmpl"}, "unclosed action"},
	} {
		err := c.p.ValidateIntent(c.id, c.fields)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("ValidateIntent(%q, %v) = %v, want an error containing %q", c.id, c.fields, err, c.want)
		}
	}
}

func TestApplyNeverDeletes(t *testing.T) {
	p, _ := newTrial(t)
	err := p.Apply(context.Background(), core.Plan{Ops: []core.Op{{Kind: core.OpKindDelete, Target: core.NewID(Name, core.NewScope(nil), "a")}}})
	if err == nil {
		t.Fatalf("Apply(delete) = nil, want an error")
	}
}

// docs/providers/template-file.md の cloud-init の例が描ける（lines・json）。
func TestCloudInitDocExample(t *testing.T) {
	tmpl := t.TempDir()
	src := "#cloud-config\nhostname: {{.Hostname}}\nssh_authorized_keys:\n{{- range lines .SSHAuthorizedKeys}}\n  - {{json .}}\n{{- end}}\n" +
		"write_files:\n  - path: /etc/sakutei-installed\n    content: {{json .InstallMark}}\n"
	if err := os.WriteFile(filepath.Join(tmpl, "user-data.tmpl"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := New(tmpl, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.render("node1/user-data", map[string]string{FieldTemplate: "user-data.tmpl", FieldHostname: "node1",
		FieldSSHAuthorizedKeys: "ssh-ed25519 AAAA a\n\nssh-ed25519 BBBB b\n", FieldInstallMark: "m-1"})
	if err != nil {
		t.Fatal(err)
	}
	want := "#cloud-config\nhostname: node1\nssh_authorized_keys:\n  - \"ssh-ed25519 AAAA a\"\n  - \"ssh-ed25519 BBBB b\"\n" +
		"write_files:\n  - path: /etc/sakutei-installed\n    content: \"m-1\"\n"
	if string(got) != want {
		t.Errorf("rendered:\n%s\nwant:\n%s", got, want)
	}
}
