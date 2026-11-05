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

// Package templatefile は利用者のテンプレート（text/template）にホストごとの値を入れた
// ファイルを書き出し、宣言どおりに保つ provider（module 名 `template-file`、
// 設計 §7.1 の template-file、E-E5・N-E8）。v2 の internal/templatefile を移した。
// 使い道は OS の自動導入の応答ファイル（kickstart・autoinstall・preseed・Alpine の
// 回答など）。ホストごとの差（ホスト名・アドレス・ssh の公開鍵・導入の marker）だけを
// 宣言に書き、中身はテンプレートから作る（E-E5・N-E8）。書き出したファイルを ISO や
// HTTP の配信ディレクトリに入れるのは利用者の側。
//
//	providers:                                   # 接続設定（ディレクトリだけ）
//	  answers:
//	    module: template-file
//	    settings:
//	      template_dir: /etc/sakutei/templates   # テンプレートのディレクトリ
//	      output_dir: /srv/http/answers          # 書き出す先
//
//	resources:                                   # intent（ホストごとの値は Resource の field）
//	  - type: answers/file
//	    id: node1/answers.sh                     # output_dir からの相対パス
//	    fields:
//	      template: alpine-answers.sh.tmpl       # template_dir からの相対パス（必須）
//	      hostname: node1
//	      address: 10.0.2.15/24
//	      ssh_authorized_keys: ssh-ed25519 AAAA... sakutei
//	      install_mark: install-2026-10-27
//	      vars: {gateway: 10.0.2.2}              # そのほかの値（名前 → 文字列）
//
// type file（ID は output_dir からの相対パス）。field は template・hostname・
// address・ssh_authorized_keys・install_mark（文字列）と vars（Map、名前 → 文字列）。
// テンプレートが受け取る値は Data（.ID・.Hostname・.Address・.SSHAuthorizedKeys・
// .InstallMark・.Vars.<名前>）、関数は shquote・json・lines。宣言に無い値をテンプレートが
// 使うと render できない（missingkey=error）。
//
// 決まり：
//
//   - 書き出したファイルからは宣言の値を読み戻せない。そこで観測は「今の宣言で
//     render した中身と、ファイルの中身が同じか」だけを答える（設計 §3.3 の
//     DesiredObserver の縮退）。コアは宣言を Observe に渡さないので、Validate が
//     宣言の field を ID ごとに覚え、Observe がそれで render する（guest-os の marker と
//     同じ）。覚えた宣言の無い ID は Unknown（E-E5・E-K1）。
//   - ファイルが無ければ Absent（コアが作る）。同じなら Present で、field は宣言の
//     コピー（ずれ無し）。違えば field template だけを「ファイルの sha256 が render した
//     中身の sha256 と違う」という値に変えて返し、plan に読める Update を出す
//     （中身の平文は観測・plan・Record に出さない）。手で直されたファイルも、
//     テンプレートを直したときも、次の cycle に宣言どおりに書き直す（E-E5）。
//   - テンプレートが読めない・render できない・書き出す先が読めない（ディレクトリでない・
//     権限）は Unknown。Absent にしない（不変条件 3、E-K1）。
//   - secret は書き出さない。値に secret の参照（{secret: …}・secretref:…）や秘密鍵
//     （"PRIVATE KEY"）を書くと読み込み時にエラーにする。応答ファイルは HTTP などで
//     配られる前提なので、入れる値は intent にそのまま残る公開の値だけ（E-E5）。
//   - Range・ensure: absent は書けない（List を持たず prune しない）。Delete は拒否する
//     （要らなくなったファイルは人が消す）（E-E5）。
//   - 書き込みは同じディレクトリの一時ファイルに書いて rename（読み手が半端な
//     中身を見ない）。親のディレクトリは作る。ファイルは 0644。中身が render したものと
//     同じなら書かない（E-E5）。
//   - ID と template はそれぞれのディレクトリの中の相対パスだけ（絶対パス・".." は
//     エラーにする）。
package templatefile

import (
	"bytes"
	"context"
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
	"sync"
	"text/template"

	"github.com/tjst-t/sakutei/v3/core"
)

// Version は Schema のバージョン。Data・funcs・比べ方を変えたら上げる（E-J12）。
const Version = "template-file/1"

// Kind は type の名前。
const Kind = "file"

// field の名前。
const (
	FieldTemplate          = "template"
	FieldHostname          = "hostname"
	FieldAddress           = "address"
	FieldSSHAuthorizedKeys = "ssh_authorized_keys"
	FieldInstallMark       = "install_mark"
	// FieldVars はそのほかの値（名前 → 文字列）。テンプレートでは .Vars.<名前>。
	FieldVars = "vars"
)

var varName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Data はテンプレートが受け取る値。
type Data struct {
	// ID は output_dir からの相対パス（Resource の ID の名前）。
	ID                string
	Hostname          string
	Address           string
	SSHAuthorizedKeys string
	InstallMark       string
	// Vars は field vars（名前 → 値）。
	Vars map[string]string
}

// funcs はテンプレートで使える関数。
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

// Provider は template-file の provider。
type Provider struct {
	templateDir string
	outputDir   string
	mu          sync.Mutex
	// declared は Validate が覚えた ID ごとの宣言の field（正規形）。
	declared map[core.ID]core.Fields
}

var (
	_ core.Provider  = (*Provider)(nil)
	_ core.Validator = (*Provider)(nil)
)

// New は templateDir のテンプレートから outputDir の下にファイルを書く provider を作る。
func New(templateDir, outputDir string) (*Provider, error) {
	if templateDir == "" {
		return nil, errors.New("settings.template_dir（テンプレートのディレクトリ）を書く")
	}
	if outputDir == "" {
		return nil, errors.New("settings.output_dir（書き出す先）を書く")
	}
	return &Provider{templateDir: templateDir, outputDir: outputDir, declared: map[core.ID]core.Fields{}}, nil
}

// Module は settings から provider を作る（load.Module）。settings は
// template_dir・output_dir（ディレクトリ）だけ。
func Module(settings map[string]any) (core.Provider, error) {
	var tdir, odir string
	for k, v := range settings {
		s, ok := v.(string)
		switch k {
		case "template_dir", "output_dir":
			if !ok || s == "" {
				return nil, fmt.Errorf("settings.%s にディレクトリを書く", k)
			}
			if k == "template_dir" {
				tdir = s
			} else {
				odir = s
			}
		default:
			return nil, fmt.Errorf("settings に知らない field %q（書ける field: output_dir, template_dir。ホストごとの値は Resource の field に書く）", k)
		}
	}
	return New(tdir, odir)
}

// Schema は type file（E-E5）。
func (p *Provider) Schema() core.Schema {
	str := core.FieldSpec{Type: core.String}
	return core.Schema{Version: Version, Kinds: map[string]core.KindSchema{
		Kind: {Fields: map[string]core.FieldSpec{
			FieldTemplate: str, FieldHostname: str, FieldAddress: str,
			FieldSSHAuthorizedKeys: str, FieldInstallMark: str,
			FieldVars: {Type: core.Map},
		}},
	}}
}

// Validate は書き方の誤りと secret をエラーにし、ID ごとの宣言を覚える（E-E5・E-M3）。
// テンプレートを render できることもここで確かめる（テンプレートは intent と同じく
// 人が書く入力）。テンプレートが今無い・読めない（権限など）ときはエラーにせず
// （無ければ警告する）、観測で Unknown にする。Validate は cycle ごとに呼ばれる。
// daemon の動作中にテンプレートを消しても intent 全体はエラーにせず、そのファイル
// だけを止める（E-K1）。
func (p *Provider) Validate(res []core.Resource, ranges []core.Range) []core.Diagnostic {
	var diags []core.Diagnostic
	rej := func(o core.Origin, target, format string, a ...any) {
		diags = append(diags, core.Diagnostic{Severity: core.Reject, Origin: o, Target: target, Message: fmt.Sprintf(format, a...)})
	}
	for _, rg := range ranges {
		rej(rg.Origin, "ranges", "template-file は Range を持たない（List と prune が無い。要らなくなったファイルは人が消す）")
	}
	declared := map[core.ID]core.Fields{}
	for _, r := range res {
		target := r.ID.String()
		if r.ID.Kind() != Kind {
			rej(r.Origin, target, "template-file の type は %s だけ", Kind)
			continue
		}
		if r.Ensure == core.EnsureAbsent {
			rej(r.Origin, target, "template-file はファイルを消さない（ensure: absent は書けない。要らなくなったファイルは人が消す）")
			continue
		}
		if _, err := inside(p.outputDir, r.ID.Name); err != nil {
			rej(r.Origin, target, "id は output_dir からの相対パス: %v", err)
			continue
		}
		if msg := checkFields(r.Fields); msg != "" {
			rej(r.Origin, target, "%s", msg)
			continue
		}
		switch _, err := p.render(r.ID, r.Fields); {
		case errors.Is(err, errTemplateMissing):
			diags = append(diags, core.Diagnostic{Severity: core.Warn, Origin: r.Origin, Target: target, Message: err.Error()})
		case err != nil && !errors.Is(err, errTemplateUnreadable):
			rej(r.Origin, target, "%v", err)
			continue
		}
		declared[r.ID] = copyFields(r.Fields)
	}
	rejected := false
	for _, d := range diags {
		rejected = rejected || d.Severity == core.Reject
	}
	if !rejected {
		p.mu.Lock()
		p.declared = declared
		p.mu.Unlock()
	}
	return diags
}

// checkFields は field の誤りと secret を見る（エラーにする理由を返す。無ければ空）。
func checkFields(f core.Fields) string {
	known := map[string]bool{FieldTemplate: true, FieldHostname: true, FieldAddress: true,
		FieldSSHAuthorizedKeys: true, FieldInstallMark: true, FieldVars: true}
	for _, k := range sortedKeys(f) {
		if !known[k] {
			return fmt.Sprintf("知らない field %q（書ける field: address, hostname, install_mark, ssh_authorized_keys, template, vars）", k)
		}
		if k == FieldVars {
			continue
		}
		s, ok := f[k].(string)
		if !ok {
			return fmt.Sprintf("field %q は文字列で書く（secret の参照は書けない）", k)
		}
		if why := secretLike(s); why != "" {
			return fmt.Sprintf("field %q が%sに見える。template-file は secret を書き出さない（公開鍵など公開の値だけを書く）", k, why)
		}
	}
	if s, _ := f[FieldTemplate].(string); s == "" {
		return "template（template_dir の中のテンプレートのファイル）を書く"
	}
	if _, err := inside(".", f[FieldTemplate].(string)); err != nil {
		return fmt.Sprintf("template は template_dir からの相対パス: %v", err)
	}
	if v, ok := f[FieldVars]; ok {
		m, ok := v.(map[string]any)
		if !ok {
			return "vars は名前 → 文字列のマップで書く（例 {gateway: 10.0.2.2}）"
		}
		names := make([]string, 0, len(m))
		for n := range m {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if !varName.MatchString(n) {
				return fmt.Sprintf("vars の名前 %q は英字・数字・_（頭は英字か _）", n)
			}
			s, ok := m[n].(string)
			if !ok {
				return fmt.Sprintf("vars.%s は文字列で書く（secret の参照・入れ子は書けない）", n)
			}
			if why := secretLike(s); why != "" {
				return fmt.Sprintf("vars.%s が%sに見える。template-file は secret を書き出さない（公開の値だけを書く）", n, why)
			}
		}
	}
	return ""
}

// secretLike は v が secret に見える理由を返す（見えなければ空）。
func secretLike(v string) string {
	switch {
	case strings.HasPrefix(strings.TrimSpace(v), "secretref:"):
		return " secret の参照"
	case strings.Contains(v, "PRIVATE KEY"):
		return "秘密鍵"
	}
	return ""
}

// Observe は覚えた宣言でテンプレートを render し、ファイルの中身と比べる。エラーは
// ID ごとの Unknown にする（E-E5・E-K1）。
func (p *Provider) Observe(_ context.Context, ids []core.ID) (map[core.ID]core.Observation, error) {
	out := make(map[core.ID]core.Observation, len(ids))
	for _, id := range ids {
		out[id] = p.observe(id)
	}
	return out, nil
}

func (p *Provider) observe(id core.ID) core.Observation {
	unknown := func(format string, a ...any) core.Observation {
		return core.Observation{Availability: core.Unknown, Note: fmt.Sprintf(format, a...)}
	}
	if id.Kind() != Kind {
		return unknown("template-file に type %q は無い", id.Kind())
	}
	p.mu.Lock()
	fields, ok := p.declared[id]
	p.mu.Unlock()
	if !ok {
		return unknown("%s の宣言が無い（観測は宣言で render した中身とファイルを比べるので、宣言の無いファイルは観測できない）", id)
	}
	file, err := inside(p.outputDir, id.Name)
	if err != nil {
		return unknown("%v", err)
	}
	want, err := p.render(id, fields)
	if err != nil {
		return unknown("%v", err)
	}
	got, err := os.ReadFile(file)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return core.Observation{Availability: core.Absent}
	case err != nil:
		return unknown("書き出したファイル %s を読めない: %v", id.Name, err)
	}
	obs := copyFields(fields)
	if !bytes.Equal(got, want) {
		obs[FieldTemplate] = fmt.Sprintf("%s で render した中身と違うファイル（ファイル %s、render した中身 %s）",
			fields[FieldTemplate], core.Digest(string(got)), core.Digest(string(want)))
	}
	return core.Observation{Availability: core.Present, Fields: obs}
}

// errTemplateUnreadable はテンプレートのファイルを今読めない（無い以外の理由）。
var errTemplateUnreadable = errors.New("テンプレートを読めない")

// errTemplateMissing はテンプレートのファイルが template_dir に無い。
var errTemplateMissing = errors.New("template_dir に無いテンプレート")

// render は fields でテンプレートを render する。
func (p *Provider) render(id core.ID, fields core.Fields) ([]byte, error) {
	name, _ := fields[FieldTemplate].(string)
	file, err := inside(p.templateDir, name)
	if err != nil {
		return nil, fmt.Errorf("テンプレート %q: %v", name, err)
	}
	src, err := os.ReadFile(file)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("%w %q", errTemplateMissing, name)
	case err != nil:
		return nil, fmt.Errorf("%w %q: %v", errTemplateUnreadable, name, err)
	}
	t, err := template.New(name).Funcs(funcs).Option("missingkey=error").Parse(string(src))
	if err != nil {
		return nil, fmt.Errorf("テンプレート %q を読めない: %v", name, err)
	}
	str := func(k string) string { s, _ := fields[k].(string); return s }
	d := Data{
		ID:                id.Name,
		Hostname:          str(FieldHostname),
		Address:           str(FieldAddress),
		SSHAuthorizedKeys: str(FieldSSHAuthorizedKeys),
		InstallMark:       str(FieldInstallMark),
		Vars:              map[string]string{},
	}
	if m, ok := fields[FieldVars].(map[string]any); ok {
		for k, v := range m {
			if s, ok := v.(string); ok {
				d.Vars[k] = s
			}
		}
	}
	var out bytes.Buffer
	if err := t.Execute(&out, d); err != nil {
		return nil, fmt.Errorf("テンプレート %q を render できない: %v", name, err)
	}
	return out.Bytes(), nil
}

// List は持たない（Range は Validate がエラーにする）。
func (p *Provider) List(context.Context, core.Range) (core.Listing, error) {
	return core.Listing{}, errors.New("template-file は List を持たない（書き出したファイルを数えない・消さない）")
}

// Apply は宣言で render した中身を書く（一時ファイル → rename）。中身が同じなら
// 書かない（E-E5）。
func (p *Provider) Apply(_ context.Context, _ core.WriteContext, id core.ID, desired core.Fields, _ core.Observation) error {
	if id.Kind() != Kind {
		return fmt.Errorf("template-file に type %q は無い", id.Kind())
	}
	if msg := checkFields(desired); msg != "" {
		return fmt.Errorf("%s: %s", id, msg)
	}
	file, err := inside(p.outputDir, id.Name)
	if err != nil {
		return fmt.Errorf("%s: %v", id, err)
	}
	data, err := p.render(id, desired)
	if err != nil {
		return fmt.Errorf("%s: %v", id, err)
	}
	if got, err := os.ReadFile(file); err == nil && bytes.Equal(got, data) {
		return nil
	}
	if err := writeFile(file, data); err != nil {
		return fmt.Errorf("%s: 書けない: %v", id, err)
	}
	return nil
}

// Delete は拒否する（要らなくなったファイルは人が消す、E-E5）。
func (p *Provider) Delete(context.Context, core.WriteContext, core.ID, core.Observation) error {
	return &core.RefuseError{Reason: "template-file はファイルを消さない（要らなくなったファイルは人が消す）"}
}

// inside は dir の中の相対パス rel を返す（絶対パス・".."・空はエラー）。
func inside(dir, rel string) (string, error) {
	if rel == "" || strings.HasPrefix(rel, "/") || strings.ContainsAny(rel, "\\\x00") || path.Clean(rel) != rel || rel == "." ||
		rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("\"..\" を含まない正規の相対パスで書く（%q）", rel)
	}
	return filepath.Join(dir, filepath.FromSlash(rel)), nil
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

// copyFields は宣言の field をコピーする（vars のマップもコピーする）。
func copyFields(f core.Fields) core.Fields {
	out := make(core.Fields, len(f))
	for k, v := range f {
		if m, ok := v.(map[string]any); ok {
			c := make(map[string]any, len(m))
			for mk, mv := range m {
				c[mk] = mv
			}
			v = c
		}
		out[k] = v
	}
	return out
}

func sortedKeys(m core.Fields) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
