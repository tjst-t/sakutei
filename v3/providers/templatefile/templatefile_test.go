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

package templatefile_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/providers/templatefile"
	"github.com/tjst-t/sakutei/v3/sdk/contract"
)

// trialTemplates は試用と統合テストが使う Alpine のテンプレートのディレクトリ（v2 と同じもの）。
const trialTemplates = "../../../docs/trial/redfish-os-install/templates"

const answersTmpl = `HOSTNAME={{shquote .Hostname}}
ADDRESS={{shquote .Address}}
GATEWAY={{shquote .Vars.gateway}}
MARK={{shquote .InstallMark}}
{{- range lines .SSHAuthorizedKeys}}
KEY={{json .}}
{{- end}}
ID={{.ID}}
`

type env struct {
	p    *templatefile.Provider
	tdir string
	odir string
}

func newEnv(t *testing.T) env {
	t.Helper()
	tdir, odir := t.TempDir(), filepath.Join(t.TempDir(), "answers")
	writeTemplate(t, tdir, "answers.tmpl", answersTmpl)
	p, err := templatefile.Module(map[string]any{"template_dir": tdir, "output_dir": odir})
	if err != nil {
		t.Fatal(err)
	}
	return env{p: p.(*templatefile.Provider), tdir: tdir, odir: odir}
}

func writeTemplate(t *testing.T, dir, name, src string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fileID(name string) core.ID { return core.ID{Type: "answers/file", Name: name} }

func answer(name string) core.Resource {
	return core.Resource{ID: fileID(name), Fields: core.Fields{
		templatefile.FieldTemplate:          "answers.tmpl",
		templatefile.FieldHostname:          "node1",
		templatefile.FieldAddress:           "10.0.2.15/24",
		templatefile.FieldSSHAuthorizedKeys: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample sakutei\n\nssh-ed25519 AAAAC3second ops",
		templatefile.FieldInstallMark:       "install-1",
		templatefile.FieldVars:              map[string]any{"gateway": "10.0.2.2"},
	}}
}

const wantAnswers = `HOSTNAME='node1'
ADDRESS='10.0.2.15/24'
GATEWAY='10.0.2.2'
MARK='install-1'
KEY="ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample sakutei"
KEY="ssh-ed25519 AAAAC3second ops"
ID=node1/answers.sh
`

func (e env) validate(t *testing.T, res ...core.Resource) {
	t.Helper()
	if d := e.p.Validate(res, nil); len(d) > 0 {
		t.Fatal(d)
	}
}

func (e env) observe(t *testing.T, id core.ID) core.Observation {
	t.Helper()
	o, err := e.p.Observe(context.Background(), []core.ID{id})
	if err != nil {
		t.Fatal(err)
	}
	return o[id]
}

func (e env) apply(t *testing.T, r core.Resource) {
	t.Helper()
	if err := e.p.Apply(context.Background(), core.NewWriteContext(nil), r.ID, r.Fields, e.observe(t, r.ID)); err != nil {
		t.Fatal(err)
	}
}

func (e env) compare(t *testing.T, r core.Resource) plan.Change {
	t.Helper()
	return plan.Compare(r, e.p.Schema().Kinds[templatefile.Kind], e.observe(t, r.ID))
}

// 無ければ Absent で作る。親のディレクトリを作り、0644 で書き、render した中身と
// 同じなら宣言どおり（E-E5・N-E8）。
func TestCreateAndConverged(t *testing.T) {
	e := newEnv(t)
	r := answer("node1/answers.sh")
	e.validate(t, r)
	if c := e.compare(t, r); c.Op != plan.Create {
		t.Fatalf("作る前: %+v", c)
	}
	e.apply(t, r)
	file := filepath.Join(e.odir, "node1", "answers.sh")
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != wantAnswers {
		t.Fatalf("書いた中身:\n%s", b)
	}
	st, _ := os.Stat(file)
	if st.Mode().Perm() != 0o644 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
	if c := e.compare(t, r); c.Op != plan.None {
		t.Fatalf("書いたあと: %+v", c)
	}
	// 一時ファイルを残さない。
	ents, _ := os.ReadDir(filepath.Dir(file))
	if len(ents) != 1 {
		t.Errorf("ディレクトリに %d 個", len(ents))
	}
}

// 手で直されたファイル・テンプレートを直したときは field template が違うと観測し、
// 読める Update を出して書き直す。ファイルの中身は観測に出さない（E-E5）。
func TestDriftIsUpdateWithoutContent(t *testing.T) {
	e := newEnv(t)
	r := answer("node1/answers.sh")
	e.validate(t, r)
	e.apply(t, r)
	file := filepath.Join(e.odir, "node1", "answers.sh")
	if err := os.WriteFile(file, []byte("HOSTNAME='hand-edited-content'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := e.compare(t, r)
	if c.Op != plan.Update || len(c.Fields) != 1 || c.Fields[0].Name != templatefile.FieldTemplate {
		t.Fatalf("手で直したあと: %+v", c)
	}
	obs, _ := c.Fields[0].Observed.(string)
	if !strings.Contains(obs, core.Digest("HOSTNAME='hand-edited-content'\n")) || !strings.Contains(obs, core.Digest(wantAnswers)) {
		t.Errorf("観測の値に sha256 が無い: %q", obs)
	}
	if strings.Contains(obs, "hand-edited") {
		t.Errorf("ファイルの中身が観測に出た: %q", obs)
	}
	if c.Replace {
		t.Error("作り直しにした")
	}
	e.apply(t, r)
	if c := e.compare(t, r); c.Op != plan.None {
		t.Fatalf("書き直したあと: %+v", c)
	}

	// テンプレートを直したら次の観測で違い、書き直す。
	writeTemplate(t, e.tdir, "answers.tmpl", answersTmpl+"# v2\n")
	if c := e.compare(t, r); c.Op != plan.Update {
		t.Fatalf("テンプレートを直したあと: %+v", c)
	}
	e.apply(t, r)
	if b, _ := os.ReadFile(file); string(b) != wantAnswers+"# v2\n" {
		t.Fatalf("書き直した中身:\n%s", b)
	}

	// 宣言の値を変えたら違う。
	r.Fields[templatefile.FieldHostname] = "node2"
	e.validate(t, r)
	if c := e.compare(t, r); c.Op != plan.Update {
		t.Fatalf("宣言を変えたあと: %+v", c)
	}
}

// 宣言どおりなら書かない（ファイルを置き換えない）。
func TestApplyIdempotent(t *testing.T) {
	e := newEnv(t)
	r := answer("a.sh")
	e.validate(t, r)
	e.apply(t, r)
	file := filepath.Join(e.odir, "a.sh")
	before, _ := os.Stat(file)
	e.apply(t, r)
	after, _ := os.Stat(file)
	if !os.SameFile(before, after) {
		t.Error("宣言どおりなのに書き直した")
	}
}

// 宣言の無い ID・テンプレートが無い・壊れたテンプレート・書き出す先が読めないは Unknown
// （Absent にしない、E-K1）。
func TestUnknown(t *testing.T) {
	cases := map[string]func(t *testing.T, e env){
		"宣言が無い": func(t *testing.T, e env) { e.validate(t) },
		"テンプレートが無い": func(t *testing.T, e env) {
			if err := os.Remove(filepath.Join(e.tdir, "answers.tmpl")); err != nil {
				t.Fatal(err)
			}
		},
		"壊れたテンプレート":         func(t *testing.T, e env) { writeTemplate(t, e.tdir, "answers.tmpl", "{{.Hostname") },
		"render できないテンプレート": func(t *testing.T, e env) { writeTemplate(t, e.tdir, "answers.tmpl", "{{.Vars.missing}}") },
		"書き出す先がファイル": func(t *testing.T, e env) {
			if err := os.WriteFile(e.odir, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"書き出すファイルがディレクトリ": func(t *testing.T, e env) {
			if err := os.MkdirAll(filepath.Join(e.odir, "node1", "answers.sh"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			r := answer("node1/answers.sh")
			e.validate(t, r)
			f(t, e)
			if o := e.observe(t, r.ID); o.Availability != core.Unknown || o.Note == "" {
				t.Fatalf("%+v", o)
			}
		})
	}
	t.Run("type が違う", func(t *testing.T) {
		e := newEnv(t)
		id := core.ID{Type: "answers/other", Name: "x"}
		if o := e.observe(t, id); o.Availability != core.Unknown {
			t.Fatalf("%+v", o)
		}
	})
}

// 書き方の誤りと secret を読み込み時にエラーにする（E-E5・E-M3）。
func TestValidateRejects(t *testing.T) {
	cases := map[string]func(r *core.Resource){
		"絶対パスの id":        func(r *core.Resource) { r.ID.Name = "/etc/passwd" },
		".. の id":         func(r *core.Resource) { r.ID.Name = "../x" },
		"正規でない id":        func(r *core.Resource) { r.ID.Name = "a//b" },
		".. の template":   func(r *core.Resource) { r.Fields[templatefile.FieldTemplate] = "../answers.tmpl" },
		"template が無い":    func(r *core.Resource) { delete(r.Fields, templatefile.FieldTemplate) },
		"secret の参照":      func(r *core.Resource) { r.Fields[templatefile.FieldHostname] = "secretref:vault:/x" },
		"secret の参照の map": func(r *core.Resource) { r.Fields[templatefile.FieldHostname] = map[string]any{"secret": "vault:/x"} },
		"秘密鍵": func(r *core.Resource) {
			r.Fields[templatefile.FieldSSHAuthorizedKeys] = "-----BEGIN OPENSSH PRIVATE KEY-----"
		},
		"vars の秘密鍵": func(r *core.Resource) {
			r.Fields[templatefile.FieldVars] = map[string]any{"k": "-----BEGIN RSA PRIVATE KEY-----"}
		},
		"vars の secret の参照": func(r *core.Resource) {
			r.Fields[templatefile.FieldVars] = map[string]any{"k": map[string]any{"secret": "vault:/x"}}
		},
		"vars の名前":       func(r *core.Resource) { r.Fields[templatefile.FieldVars] = map[string]any{"gate-way": "x"} },
		"vars が map でない": func(r *core.Resource) { r.Fields[templatefile.FieldVars] = "x" },
		"vars に無い値を使う": func(r *core.Resource) {
			r.Fields[templatefile.FieldVars] = map[string]any{"other": "x"}
		},
		"知らない field":     func(r *core.Resource) { r.Fields["var.gateway"] = "x" },
		"type が違う":       func(r *core.Resource) { r.ID.Type = "answers/other" },
		"ensure: absent": func(r *core.Resource) { r.Ensure = core.EnsureAbsent },
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			r := answer("node1/answers.sh")
			f(&r)
			d := e.p.Validate([]core.Resource{r}, nil)
			if len(d) == 0 || d[0].Severity != core.Reject {
				t.Fatalf("エラーにならなかった: %v", d)
			}
			for _, s := range []string{"PRIVATE KEY-----"} {
				if strings.Contains(d[0].Message, s) {
					t.Errorf("エラーの文に値が出た: %s", d[0].Message)
				}
			}
		})
	}
	t.Run("Range", func(t *testing.T) {
		e := newEnv(t)
		if d := e.p.Validate(nil, []core.Range{{Provider: "answers"}}); len(d) == 0 {
			t.Fatal("Range がエラーにならなかった")
		}
	})
	// エラーにした読み込みは前に覚えた宣言を変えない。
	t.Run("エラーなら覚えない", func(t *testing.T) {
		e := newEnv(t)
		r := answer("a.sh")
		e.validate(t, r)
		bad := answer("b.sh")
		bad.Fields[templatefile.FieldHostname] = "secretref:x:y"
		if d := e.p.Validate([]core.Resource{bad}, nil); len(d) == 0 {
			t.Fatal("エラーにならなかった")
		}
		if o := e.observe(t, r.ID); o.Availability != core.Absent {
			t.Fatalf("%+v", o)
		}
	})
}

// テンプレートが今読めない（権限）ときは読み込み時にエラーにせず、観測で Unknown（E-K1）。
func TestTemplateUnreadableIsNotReject(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root では権限で読めなくできない")
	}
	e := newEnv(t)
	tmpl := filepath.Join(e.tdir, "answers.tmpl")
	if err := os.Chmod(tmpl, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tmpl, 0o644) })
	r := answer("a.sh")
	e.validate(t, r)
	if o := e.observe(t, r.ID); o.Availability != core.Unknown {
		t.Fatalf("%+v", o)
	}
}

func TestModuleSettings(t *testing.T) {
	for name, s := range map[string]map[string]any{
		"空":               {},
		"output_dir が無い":  {"template_dir": "t"},
		"template_dir が空": {"template_dir": "", "output_dir": "o"},
		"知らない field":      {"template_dir": "t", "output_dir": "o", "zone_dir": "z"},
		"文字列でない":          {"template_dir": 1, "output_dir": "o"},
	} {
		if _, err := templatefile.Module(s); err == nil {
			t.Errorf("%s: エラーにならなかった", name)
		}
	}
}

// 試用の Alpine のテンプレート（v2 と同じもの）を render できる。index .Vars の無い値は空。
func TestTrialTemplate(t *testing.T) {
	if _, err := os.Stat(trialTemplates); err != nil {
		t.Skip("試用のテンプレートが無い")
	}
	odir := t.TempDir()
	p, err := templatefile.New(trialTemplates, odir)
	if err != nil {
		t.Fatal(err)
	}
	r := core.Resource{ID: fileID("6f1b7c52-0e1a-4d2b-9c11-000000008501"), Fields: core.Fields{
		templatefile.FieldTemplate: "alpine-answers.sh.tmpl", templatefile.FieldHostname: "node1",
		templatefile.FieldAddress: "10.0.2.15/24", templatefile.FieldInstallMark: "install-1",
		templatefile.FieldVars: map[string]any{"gateway": "10.0.2.2"},
	}}
	if d := p.Validate([]core.Resource{r}, nil); len(d) > 0 {
		t.Fatal(d)
	}
	if err := p.Apply(context.Background(), core.NewWriteContext(nil), r.ID, r.Fields, core.Observation{Availability: core.Absent}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(odir, r.ID.Name))
	for _, want := range []string{"HOSTNAME='node1'", "GATEWAY='10.0.2.2'", "FAIL=''", "# resource: " + r.ID.Name} {
		if !strings.Contains(string(b), want) {
			t.Errorf("%q が無い:\n%s", want, b)
		}
	}
}

func TestDeleteRefuses(t *testing.T) {
	e := newEnv(t)
	var re *core.RefuseError
	if err := e.p.Delete(context.Background(), core.NewWriteContext(nil), fileID("a.sh"), core.Observation{}); !errors.As(err, &re) {
		t.Fatalf("%v", err)
	}
	if _, err := e.p.List(context.Background(), core.Range{}); err == nil {
		t.Fatal("List がエラーを返さない")
	}
}

// inode は書き出したファイルの inode の和（rename で書けば変わる）。
func inode(paths ...string) func() int {
	return func() int {
		n := 0
		for _, p := range paths {
			if st, err := os.Stat(p); err == nil {
				if s, ok := st.Sys().(*syscall.Stat_t); ok {
					n += int(s.Ino)
				}
			}
		}
		return n
	}
}

func TestContract(t *testing.T) {
	contract.Run(t, func(t *testing.T) contract.Target {
		e := newEnv(t)
		created, drifted, kept := answer("new/answers.sh"), answer("drifted.sh"), answer("kept.sh")
		drifted.Fields[templatefile.FieldHostname] = "node2"
		// drifted は前の中身（secret に見立てた値）、kept は宣言どおりの中身で置く。
		if err := os.MkdirAll(e.odir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(e.odir, "drifted.sh"), []byte("PREVIOUS-CONTENT-NOT-TO-LEAK\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		e.validate(t, kept)
		if err := e.p.Apply(context.Background(), core.NewWriteContext(nil), kept.ID, kept.Fields, core.Observation{}); err != nil {
			t.Fatal(err)
		}
		files := []string{filepath.Join(e.odir, "new", "answers.sh"), filepath.Join(e.odir, "drifted.sh"), filepath.Join(e.odir, "kept.sh")}
		return contract.Target{
			Provider:  e.p,
			Resources: []core.Resource{kept},
			Absent:    []core.ID{created.ID},
			Create:    []core.Resource{created},
			Converge:  []core.Resource{drifted},
			Observed:  []core.ID{kept.ID, drifted.ID},
			Faults: map[string]func(){
				"テンプレートが無い": func() { _ = os.Remove(filepath.Join(e.tdir, "answers.tmpl")) },
				"壊れたテンプレート": func() { writeTemplate(t, e.tdir, "answers.tmpl", "{{.Hostname") },
				"書き出す先がファイル": func() {
					_ = os.RemoveAll(e.odir)
					_ = os.WriteFile(e.odir, []byte("x"), 0o644)
				},
			},
			Writes:  inode(files...),
			Secrets: []string{"PREVIOUS-CONTENT-NOT-TO-LEAK"},
			Refuses: []contract.Refuse{{ID: kept.ID}},
		}
	})
}

// テンプレートのファイルが無いのは警告するだけでエラーにせず、そのファイルだけを
// Unknown にする（Validate は cycle ごとに呼ばれるので、daemon の動作中にテンプレートを
// 消しても intent 全体はエラーにしない、E-K1）。
func TestMissingTemplateWarns(t *testing.T) {
	e := newEnv(t)
	r := answer("node1/answers.sh")
	r.Fields[templatefile.FieldTemplate] = "nope.tmpl"
	d := e.p.Validate([]core.Resource{r}, nil)
	if len(d) != 1 || d[0].Severity != core.Warn || !strings.Contains(d[0].Message, "nope.tmpl") {
		t.Fatalf("診断 = %v（want 1 つの warn）", d)
	}
	obs, err := e.p.Observe(context.Background(), []core.ID{r.ID})
	if err != nil {
		t.Fatal(err)
	}
	if o := obs[r.ID]; o.Availability != core.Unknown || !strings.Contains(o.Note, "nope.tmpl") {
		t.Fatalf("観測 = %+v（want unknown）", o)
	}
}
