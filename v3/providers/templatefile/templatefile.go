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

// Package templatefile は利用者の雛形（text/template）に台ごとの値を入れた
// ファイルを書き出し、宣言どおりに保つ provider（module 名 `template-file`、
// 設計 §7.1 の template-file、E-E5・N-E8）。v2 の internal/templatefile を移した。
// 使い道は OS の自動導入の応答ファイル（kickstart・autoinstall・preseed・Alpine の
// 回答など）：台ごとの差（ホスト名・アドレス・ssh の公開鍵・導入の目印）だけを
// 宣言に書き、中身は雛形から作る（E-E5・N-E8）。書き出したファイルを ISO や
// HTTP の置き場に入れるのは利用者の側。
//
//	providers:                                   # つなぎ方の設定（置き場だけ）
//	  answers:
//	    module: template-file
//	    settings:
//	      template_dir: /etc/sakutei/templates   # 雛形の置き場
//	      output_dir: /srv/http/answers          # 書き出す先
//
//	resources:                                   # intent（台ごとの値は Resource の項目）
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
// 種類 file（ID は output_dir からの相対パス）。項目は template・hostname・
// address・ssh_authorized_keys・install_mark（文字列）と vars（Map、名前 → 文字列）。
// 雛形が受け取る値は Data（.ID・.Hostname・.Address・.SSHAuthorizedKeys・
// .InstallMark・.Vars.<名前>）、関数は shquote・json・lines。宣言に無い値を雛形が
// 使うと描けない（missingkey=error）。
//
// 決まり：
//
//   - 書き出したファイルからは宣言の値を読み戻せないので、観測は「今の宣言で
//     描いた中身と、ファイルの中身が同じか」だけを答える（設計 §3.3 の
//     DesiredObserver の縮退）。コアは宣言を Observe に渡さないので、Validate が
//     宣言の項目を ID ごとに覚え、Observe がそれで描く（guest-os の目印と同じ）。
//     覚えた宣言の無い ID は Unknown（E-E5・E-K1）。
//   - ファイルが無ければ Absent（コアが作る）。同じなら Present で、項目は宣言の
//     写し（ずれ無し）。違えば項目 template だけを「ファイルの sha256 が描いた
//     中身の sha256 と違う」という値に変えて返し、plan に読める Update を出す
//     （中身の平文は観測・plan・Record に出さない）。手で直されたファイルも、
//     雛形を直したときも、次の周に宣言どおりに書き直す（E-E5）。
//   - 雛形が読めない・描けない・書き出す先が読めない（ディレクトリでない・権限）は
//     Unknown。Absent にしない（不変条件 3、E-K1）。
//   - 秘密は書き出さない：値に秘密の参照（{secret: …}・secretref:…）や秘密鍵
//     （"PRIVATE KEY"）を書くと読み込みで断る。応答ファイルは HTTP などで配られる
//     前提なので、入れる値は intent にそのまま残る公開の値だけ（E-E5）。
//   - 範囲・ensure: absent は書けない（List を持たず prune しない）。Delete は断る
//     （要らなくなったファイルは人が消す）（E-E5）。
//   - 書き込みは同じディレクトリの一時ファイルに書いて rename（読み手が半端な
//     中身を見ない）。親のディレクトリは作る。ファイルは 0644。中身が描いたものと
//     同じなら書かない（E-E5）。
//   - ID と template はそれぞれの置き場の中の相対パスだけ（絶対パス・".." は断る）。
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

// Version は Schema の版。Data・funcs・比べ方を変えたら上げる（E-J12）。
const Version = "template-file/1"

// Kind は種類の名前。
const Kind = "file"

// 項目。
const (
	FieldTemplate          = "template"
	FieldHostname          = "hostname"
	FieldAddress           = "address"
	FieldSSHAuthorizedKeys = "ssh_authorized_keys"
	FieldInstallMark       = "install_mark"
	// FieldVars はそのほかの値（名前 → 文字列）。雛形では .Vars.<名前>。
	FieldVars = "vars"
)

var varName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Data は雛形が受け取る値。
type Data struct {
	// ID は output_dir からの相対パス（Resource の ID の名前）。
	ID                string
	Hostname          string
	Address           string
	SSHAuthorizedKeys string
	InstallMark       string
	// Vars は項目 vars（名前 → 値）。
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

// Provider は template-file の provider。
type Provider struct {
	templateDir string
	outputDir   string
	mu          sync.Mutex
	// declared は Validate が覚えた ID ごとの宣言の項目（正規形）。
	declared map[core.ID]core.Fields
}

var (
	_ core.Provider  = (*Provider)(nil)
	_ core.Validator = (*Provider)(nil)
)

// New は templateDir の雛形から outputDir の下にファイルを書く provider を作る。
func New(templateDir, outputDir string) (*Provider, error) {
	if templateDir == "" {
		return nil, errors.New("settings.template_dir（雛形の置き場）を書く")
	}
	if outputDir == "" {
		return nil, errors.New("settings.output_dir（書き出す先）を書く")
	}
	return &Provider{templateDir: templateDir, outputDir: outputDir, declared: map[core.ID]core.Fields{}}, nil
}

// Module は settings から provider を作る（load.Module）。settings は
// template_dir・output_dir（置き場）だけ。
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
			return nil, fmt.Errorf("settings に知らない項目 %q（書ける項目: output_dir, template_dir。台ごとの値は Resource の項目に書く）", k)
		}
	}
	return New(tdir, odir)
}

// Schema は種類 file（E-E5）。
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

// Validate は書き方の誤りと秘密を断り、ID ごとの宣言を覚える（E-E5・E-M3）。
// 雛形が描けることもここで確かめる（雛形は intent と同じく人が書く入力）。
// 雛形が今無い・読めない（権限など）ときは断らず（無ければ知らせる）、観測で
// Unknown にする。Validate は周ごとに呼ばれるので、常駐の間に雛形を消しても
// intent 全体を断らず、そのファイルだけを止める（E-K1）。
func (p *Provider) Validate(res []core.Resource, ranges []core.Range) []core.Diagnostic {
	var diags []core.Diagnostic
	rej := func(o core.Origin, target, format string, a ...any) {
		diags = append(diags, core.Diagnostic{Severity: core.Reject, Origin: o, Target: target, Message: fmt.Sprintf(format, a...)})
	}
	for _, rg := range ranges {
		rej(rg.Origin, "ranges", "template-file は範囲を持たない（List と prune が無い。要らなくなったファイルは人が消す）")
	}
	declared := map[core.ID]core.Fields{}
	for _, r := range res {
		target := r.ID.String()
		if r.ID.Kind() != Kind {
			rej(r.Origin, target, "template-file の種類は %s だけ", Kind)
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

// checkFields は項目の誤りと秘密を見る（断る理由を返す。無ければ空）。
func checkFields(f core.Fields) string {
	known := map[string]bool{FieldTemplate: true, FieldHostname: true, FieldAddress: true,
		FieldSSHAuthorizedKeys: true, FieldInstallMark: true, FieldVars: true}
	for _, k := range sortedKeys(f) {
		if !known[k] {
			return fmt.Sprintf("知らない項目 %q（書ける項目: address, hostname, install_mark, ssh_authorized_keys, template, vars）", k)
		}
		if k == FieldVars {
			continue
		}
		s, ok := f[k].(string)
		if !ok {
			return fmt.Sprintf("項目 %q は文字列で書く（秘密の参照は書けない）", k)
		}
		if why := secretLike(s); why != "" {
			return fmt.Sprintf("項目 %q が%sに見える。template-file は秘密を書き出さない（公開鍵など公開の値だけを書く）", k, why)
		}
	}
	if s, _ := f[FieldTemplate].(string); s == "" {
		return "template（template_dir の中の雛形のファイル）を書く"
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
				return fmt.Sprintf("vars.%s は文字列で書く（秘密の参照・入れ子は書けない）", n)
			}
			if why := secretLike(s); why != "" {
				return fmt.Sprintf("vars.%s が%sに見える。template-file は秘密を書き出さない（公開の値だけを書く）", n, why)
			}
		}
	}
	return ""
}

// secretLike は v が秘密に見える理由を返す（見えなければ空）。
func secretLike(v string) string {
	switch {
	case strings.HasPrefix(strings.TrimSpace(v), "secretref:"):
		return "秘密の参照"
	case strings.Contains(v, "PRIVATE KEY"):
		return "秘密鍵"
	}
	return ""
}

// Observe は覚えた宣言で雛形を描き、ファイルの中身と比べる。誤りは ID ごとの
// Unknown にする（E-E5・E-K1）。
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
		return unknown("template-file に種類 %q は無い", id.Kind())
	}
	p.mu.Lock()
	fields, ok := p.declared[id]
	p.mu.Unlock()
	if !ok {
		return unknown("%s の宣言が無い（観測は宣言で描いた中身とファイルを比べるので、宣言の無いファイルは観測できない）", id)
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
		obs[FieldTemplate] = fmt.Sprintf("%s で描いた中身と違うファイル（ファイル %s、描いた中身 %s）",
			fields[FieldTemplate], core.Digest(string(got)), core.Digest(string(want)))
	}
	return core.Observation{Availability: core.Present, Fields: obs}
}

// errTemplateUnreadable は雛形のファイルを今読めない（無い以外の理由）。
var errTemplateUnreadable = errors.New("雛形を読めない")

// errTemplateMissing は雛形のファイルが template_dir に無い。
var errTemplateMissing = errors.New("template_dir に無い雛形")

// render は fields で雛形を描く。
func (p *Provider) render(id core.ID, fields core.Fields) ([]byte, error) {
	name, _ := fields[FieldTemplate].(string)
	file, err := inside(p.templateDir, name)
	if err != nil {
		return nil, fmt.Errorf("雛形 %q: %v", name, err)
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
		return nil, fmt.Errorf("雛形 %q を読めない: %v", name, err)
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
		return nil, fmt.Errorf("雛形 %q を描けない: %v", name, err)
	}
	return out.Bytes(), nil
}

// List は持たない（範囲は Validate が断る）。
func (p *Provider) List(context.Context, core.Range) (core.Listing, error) {
	return core.Listing{}, errors.New("template-file は List を持たない（書き出したファイルを数えない・消さない）")
}

// Apply は宣言で描いた中身を書く（一時ファイル → rename）。中身が同じなら
// 書かない（E-E5）。
func (p *Provider) Apply(_ context.Context, _ core.WriteContext, id core.ID, desired core.Fields, _ core.Observation) error {
	if id.Kind() != Kind {
		return fmt.Errorf("template-file に種類 %q は無い", id.Kind())
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

// Delete は断る（要らなくなったファイルは人が消す、E-E5）。
func (p *Provider) Delete(context.Context, core.WriteContext, core.ID, core.Observation) error {
	return &core.RefuseError{Reason: "template-file はファイルを消さない（要らなくなったファイルは人が消す）"}
}

// inside は dir の中の相対パス rel を返す（絶対パス・".."・空は断る）。
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

// copyFields は宣言の項目を写す（vars のマップも写す）。
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
