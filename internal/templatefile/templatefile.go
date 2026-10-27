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

// Package templatefile は利用者の雛形（text/template）に対象ごとの値を入れた
// ファイルを書き出す provider "template-file"（Backlog #139、§29.1）。使い道は
// OS の自動導入の応答ファイル（kickstart・autoinstall・preseed・Alpine の回答
// など）：ホスト名・アドレス・Sakutei の ssh の公開鍵・導入の目印を宣言から作る。
// 書き出したファイルを ISO や HTTP の置き場に入れるのは利用者の側（置き場の
// ディレクトリを HTTP で配る、など）。
//
//	resources:
//	  - provider: template-file
//	    id: answers/6f1b7c52-0e1a-4d2b-9c11-000000008601   # output_dir からの相対パス
//	    fields:
//	      template: alpine-answers.tmpl                     # template_dir からの相対パス
//	      hostname: node1
//	      address: 10.0.2.15/24
//	      ssh_authorized_keys: ssh-ed25519 AAAA... sakutei
//	      install_mark: install-2026-10-27
//	      var.gateway: 10.0.2.2                             # ほかの値は var.<名前>
//
// 雛形が受け取る値（.ID・.Hostname・.Address・.SSHAuthorizedKeys・.InstallMark・
// .Vars.<名前>）と関数（shquote・json・lines）は Data と funcs。宣言に無い値を
// 雛形が使うと描けない（missingkey=error）。
//
// 設計判断:
//
//   - 最小階級（§5.6）：書き出したファイルからは宣言の値を読み戻せないので、
//     観測は provider.CheckReader で「今の宣言で描いた中身と、ファイルの中身が
//     同じか」だけを答える（Capabilities.TrueObserve=false）。違えば（無ければ）
//     書き直す。雛形のファイルを直したときも、次の周に描き直して書き直す。
//   - 秘密は書き出さない：値に SecretRef（secretref:…）や秘密鍵（"PRIVATE KEY"）を
//     書くと読み込みの時点で断る。値は intent にそのまま残る公開の値だけ。
//   - List を持たず、消さない（Pruneable:false）。要らなくなったファイルは人が消す。
//   - 書き込みは同じディレクトリの一時ファイルに書いて rename（読み手が半端な
//     中身を見ない）。親のディレクトリは作る。ファイルは 0644。
//   - ID と template はそれぞれの置き場の中の相対パスだけ（絶対パス・".." は断る）。
package templatefile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

const (
	// Name は provider 名。
	Name = "template-file"
	// ProviderVersion は diff / 描き方の版（§10.4）。Data・funcs を変えたら上げる。
	ProviderVersion = Name + "-v1"
)

// フィールド名。
const (
	// FieldPath は書き出すファイル（= ID、identity）。書くなら ID と同じ値。
	FieldPath              = "path"
	FieldTemplate          = "template"
	FieldHostname          = "hostname"
	FieldAddress           = "address"
	FieldSSHAuthorizedKeys = "ssh_authorized_keys"
	FieldInstallMark       = "install_mark"
	// VarPrefix はそのほかの値（var.<名前>）。
	VarPrefix = "var."
)

var varName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Data は雛形が受け取る値。
type Data struct {
	// ID はリソースの ID（output_dir からの相対パス）。
	ID                string
	Hostname          string
	Address           string
	SSHAuthorizedKeys string
	InstallMark       string
	// Vars は var.<名前> の値（名前 → 値）。
	Vars map[string]string
}

// funcs は雛形で使える関数。
var funcs = template.FuncMap{
	// shquote は sh の単一引用符で囲む（' は '\'' にする）。
	"shquote": func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" },
	// json は JSON の文字列にする（YAML の値にも使える）。
	"json": func(s string) (string, error) {
		b, err := json.Marshal(s)
		return string(b), err
	},
	// lines は改行で分け、空行を落とす（公開鍵を 1 行ずつ書くなど）。
	"lines": func(s string) []string {
		var out []string
		for _, l := range strings.Split(s, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				out = append(out, l)
			}
		}
		return out
	},
}

// Provider は provider.Provider・CheckReader・IntentValidator を実装する。
type Provider struct {
	templateDir string
	outputDir   string
}

var (
	_ provider.Provider        = (*Provider)(nil)
	_ provider.CheckReader     = (*Provider)(nil)
	_ provider.IntentValidator = (*Provider)(nil)
)

// New は templateDir の雛形から outputDir の下にファイルを書く Provider を返す。
func New(templateDir, outputDir string) (*Provider, error) {
	if templateDir == "" {
		return nil, errors.New("template-file: template_dir (the directory of the templates) is not set")
	}
	if outputDir == "" {
		return nil, errors.New("template-file: output_dir (where the files are written) is not set")
	}
	return &Provider{templateDir: templateDir, outputDir: outputDir}, nil
}

// Schema はフィールドの宣言。var.<名前> は Schema に無い（ValidateIntent が見る）。
func (p *Provider) Schema() provider.Schema {
	str := provider.FieldSpec{Kind: core.FieldKindString}
	return provider.Schema{Fields: map[string]provider.FieldSpec{
		FieldPath:              {Kind: core.FieldKindString, Identity: true},
		FieldTemplate:          {Kind: core.FieldKindString, Required: true},
		FieldHostname:          str,
		FieldAddress:           str,
		FieldSSHAuthorizedKeys: str,
		FieldInstallMark:       str,
	}}
}

// Capabilities は最小階級：State を返さず、CheckReader で答える。
func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		TrueObserve:     false,
		List:            false,
		BulkObserve:     false,
		Pruneable:       false,
		ProviderVersion: ProviderVersion,
	}
}

// Read は宣言が無いので答えられない（ReadCheck で観測する）。
func (p *Provider) Read(_ context.Context, localID string) (core.Observation, error) {
	return unknownObs(), provider.NewPermanentError(fmt.Errorf(
		"%s: Read %q: the file is compared with the template rendered from the desired (minimal tier, §28.7); there is no State to read", Name, localID))
}

// ReadCheck は desired で雛形を描き、ファイルの中身と比べる。
func (p *Provider) ReadCheck(_ context.Context, localID string, desired map[string]string) (core.Observation, error) {
	want, err := p.render(localID, desired)
	if err != nil {
		return unknownObs(), provider.NewPermanentError(fmt.Errorf("%s: observe %q: %w", Name, localID, err))
	}
	file, err := p.outputPath(localID)
	if err != nil {
		return unknownObs(), provider.NewPermanentError(fmt.Errorf("%s: observe %q: %w", Name, localID, err))
	}
	got, err := os.ReadFile(file)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return checked(true, "the file does not exist yet"), nil
	case err != nil:
		return unknownObs(), provider.NewTransientError(fmt.Errorf("%s: observe %q: %w", Name, localID, err))
	case bytes.Equal(got, want):
		return checked(false, ""), nil
	}
	return checked(true, fmt.Sprintf("the file differs from the rendered template (sha256 %s, want %s)", digest(got), digest(want))), nil
}

// Apply は描いた中身を書く（一時ファイル → rename）。
func (p *Provider) Apply(_ context.Context, plan core.Plan) error {
	for _, op := range plan.Ops {
		id := op.Target.Local
		if op.Kind == core.OpKindDelete {
			return provider.NewPermanentError(fmt.Errorf("%s: Apply %q: the template-file provider never deletes a file", Name, id))
		}
		fields := make(map[string]string, len(op.Fields))
		for _, f := range op.Fields {
			fields[f.Field] = f.To
		}
		data, err := p.render(id, fields)
		if err != nil {
			return provider.NewPermanentError(fmt.Errorf("%s: Apply %q: %w", Name, id, err))
		}
		file, err := p.outputPath(id)
		if err != nil {
			return provider.NewPermanentError(fmt.Errorf("%s: Apply %q: %w", Name, id, err))
		}
		if err := writeFile(file, data); err != nil {
			return fmt.Errorf("%s: Apply %q: %w", Name, id, err)
		}
	}
	return nil
}

// ValidateIntent は書き方の誤りと秘密を読み込みの時点で断る。雛形が読めて
// 描けることもここで確かめる（雛形は intent と同じく人が書く入力）。
func (p *Provider) ValidateIntent(localID string, fields map[string]string) error {
	if _, err := p.outputPath(localID); err != nil {
		return fmt.Errorf("%s %q: %w", Name, localID, err)
	}
	schema := p.Schema()
	names := make([]string, 0, len(fields))
	for k := range fields {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if _, ok := schema.Fields[k]; !ok {
			v, isVar := strings.CutPrefix(k, VarPrefix)
			if !isVar {
				return fmt.Errorf("%s %q: unknown field %q (path | template | hostname | address | ssh_authorized_keys | install_mark | var.<name>)", Name, localID, k)
			}
			if !varName.MatchString(v) {
				return fmt.Errorf("%s %q: field %q: the name after var. must be letters, digits and _", Name, localID, k)
			}
		}
		if why := secretLike(fields[k]); why != "" {
			return fmt.Errorf("%s %q: field %q looks like %s; a template-file never writes a secret (write public values only, such as the public key)", Name, localID, k, why)
		}
	}
	if v, ok := fields[FieldPath]; ok && v != localID {
		return fmt.Errorf("%s %q: path %q must equal the resource id (the file under output_dir)", Name, localID, v)
	}
	if fields[FieldTemplate] == "" {
		return fmt.Errorf("%s %q: template is required (a file in template_dir)", Name, localID)
	}
	if _, err := p.render(localID, fields); err != nil {
		return fmt.Errorf("%s %q: %w", Name, localID, err)
	}
	return nil
}

// render は fields で雛形を描く。
func (p *Provider) render(localID string, fields map[string]string) ([]byte, error) {
	name := fields[FieldTemplate]
	file, err := inside(p.templateDir, name)
	if err != nil {
		return nil, fmt.Errorf("template %q: %w", name, err)
	}
	src, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("template %q: %w", name, err)
	}
	t, err := template.New(name).Funcs(funcs).Option("missingkey=error").Parse(string(src))
	if err != nil {
		return nil, fmt.Errorf("template %q: %w", name, err)
	}
	d := Data{
		ID:                localID,
		Hostname:          fields[FieldHostname],
		Address:           fields[FieldAddress],
		SSHAuthorizedKeys: fields[FieldSSHAuthorizedKeys],
		InstallMark:       fields[FieldInstallMark],
		Vars:              map[string]string{},
	}
	for k, v := range fields {
		if n, ok := strings.CutPrefix(k, VarPrefix); ok {
			d.Vars[n] = v
		}
	}
	var out bytes.Buffer
	if err := t.Execute(&out, d); err != nil {
		return nil, fmt.Errorf("template %q: %w", name, err)
	}
	return out.Bytes(), nil
}

// outputPath は ID のファイルのパス。
func (p *Provider) outputPath(localID string) (string, error) {
	return inside(p.outputDir, localID)
}

// inside は dir の中の相対パス rel を返す（絶対パス・".."・空は断る）。
func inside(dir, rel string) (string, error) {
	if rel == "" || strings.HasPrefix(rel, "/") || strings.Contains(rel, `\`) || path.Clean(rel) != rel || rel == "." ||
		rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("want a clean relative path without .. (got %q)", rel)
	}
	return filepath.Join(dir, filepath.FromSlash(rel)), nil
}

// secretLike は v が秘密に見える理由を返す（見えなければ空）。
func secretLike(v string) string {
	switch {
	case strings.HasPrefix(strings.TrimSpace(v), "secretref:"):
		return "a SecretRef"
	case strings.Contains(v, "PRIVATE KEY"):
		return "a private key"
	}
	return ""
}

func writeFile(file string, data []byte) error {
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(file)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file)
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

func checked(changed bool, detail string) core.Observation {
	return core.Observation{Availability: core.AvailabilityOk, ObservedAt: time.Now(),
		Check: &core.CheckResult{Changed: changed, Detail: detail}}
}

func unknownObs() core.Observation {
	return core.Observation{Availability: core.AvailabilityUnknown, ObservedAt: time.Now()}
}
