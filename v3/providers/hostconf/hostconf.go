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

// Package hostconf はホストのファイルと systemd のユニットを宣言どおりに保つ
// provider（設計 §7.1 の hostconf、E-D5・E-E2・N-D7・N-E1・N-E6・E-H7）。v2 の
// host-file と systemd-unit を 1 つのモジュールにした。コアの外のモジュールで、
// コアの interface core.Provider・core.Validator を実装する。
//
//	providers:                                   # 接続設定
//	  host:
//	    module: hostconf
//	    settings:
//	      hosts:
//	        web1:
//	          ssh: {address: 192.0.2.21, user: sakutei, key: /etc/sakutei/id_ed25519, known_hosts: /etc/sakutei/known_hosts}
//	          sudo: true
//
//	ranges:                                      # intent（扱うホストは intent に書く）
//	  - provider: host
//	    args: {host: web1}
//	resources:
//	  - type: host/file
//	    id: web1:/etc/containers/systemd/app.env  # <host>:<絶対パス>
//	    fields: {content: "APP_PORT=8080\n", mode: "0640", owner: root, group: app}
//	  - type: host/unit
//	    id: web1:app.service                     # <host>:<ユニット名>
//	    fields: {enabled: true, active: true, restart_on: ["web1:/etc/containers/systemd/app.env"]}
//	    depends_on: [{id: host/file/web1:/etc/containers/systemd/app.env}]
//
// 決まり：
//
//   - ファイルの中身は sha256 だけを観測する（content は FieldSpec.Digest。平文を
//     観測・plan・Record に出さない、E-E2）。mode・owner・group は stat で観測する。
//     書き込みは同じディレクトリの一時ファイルに書いて rename（読み手が半端な中身を
//     見ない）。親のディレクトリは無ければ作る。ふつうのファイルでないもの
//     （シンボリックリンク・ディレクトリ）は Unknown。
//   - ユニットは enabled・active・content（/etc/systemd/system/<名前> の中身）と
//     restart_on。restart_on は依存するファイルの一覧で、そのファイルの中身が
//     変われば 1 回だけ restart する。restart した時のファイルの digest を
//     ホスト側の記録（<state_dir>/restart/<名前>.json）に置く。観測は今の digest と
//     記録が合えば restart_on を返す（level で判定、E-D5・N-E6）。
//   - 届かない・sudo が通らない・読めないは Unknown（Absent とは見なさない、不変条件 3）。
//   - List はファイル・ユニットの集合を数えられないので「不完全」（宣言に無いもの
//     （unmanaged）を見ない・消さない）。
package hostconf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/sdk/hostexec"
)

// Version は Schema のバージョン。
const Version = "hostconf/1"

// type の名前。
const (
	KindFile = "file"
	KindUnit = "unit"
)

// 既定のディレクトリ。
const (
	DefaultUnitDir  = "/etc/systemd/system"
	DefaultStateDir = "/var/lib/sakutei/hostconf"
)

// Provider は hostconf の provider。
type Provider struct {
	hosts    map[string]hostexec.Runner
	unitDir  string
	stateDir string
}

var (
	_ core.Provider  = (*Provider)(nil)
	_ core.Validator = (*Provider)(nil)
)

// New は Runner から provider を作る（テスト・自前のバイナリ用）。
func New(hosts map[string]hostexec.Runner, unitDir, stateDir string) *Provider {
	if unitDir == "" {
		unitDir = DefaultUnitDir
	}
	if stateDir == "" {
		stateDir = DefaultStateDir
	}
	return &Provider{hosts: hosts, unitDir: unitDir, stateDir: stateDir}
}

// Module は settings から provider を作る（load.Module）。
func Module(settings map[string]any) (core.Provider, error) {
	var unitDir, stateDir string
	var hosts map[string]hostexec.Runner
	for k, v := range settings {
		switch k {
		case "hosts":
			hs, err := hostexec.ParseHosts(v)
			if err != nil {
				return nil, err
			}
			hosts = map[string]hostexec.Runner{}
			for n, h := range hs {
				hosts[n] = h.Runner
			}
		case "unit_dir", "state_dir":
			s, ok := v.(string)
			if !ok || !path.IsAbs(s) {
				return nil, fmt.Errorf("settings.%s に絶対パスを書く", k)
			}
			if k == "unit_dir" {
				unitDir = s
			} else {
				stateDir = s
			}
		default:
			return nil, fmt.Errorf("settings に知らない field %q（書ける field: hosts, state_dir, unit_dir）", k)
		}
	}
	if hosts == nil {
		return nil, errors.New("settings.hosts を書く")
	}
	return New(hosts, unitDir, stateDir), nil
}

// Schema は file と unit の field。
func (p *Provider) Schema() core.Schema {
	return core.Schema{Version: Version, Kinds: map[string]core.KindSchema{
		KindFile: {Fields: map[string]core.FieldSpec{
			"content": {Type: core.String, Digest: true},
			"mode":    {Type: core.String},
			"owner":   {Type: core.String},
			"group":   {Type: core.String},
		}},
		KindUnit: {Fields: map[string]core.FieldSpec{
			"content":    {Type: core.String, Digest: true},
			"enabled":    {Type: core.Bool},
			"active":     {Type: core.Bool},
			"restart_on": {Type: core.Set},
		}},
	}}
}

var (
	unitRe = regexp.MustCompile(`^[A-Za-z0-9:_.@\\-]+\.(service|socket|timer|path|mount|target)$`)
	modeRe = regexp.MustCompile(`^[0-7]{4}$`)
	nameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$|^[0-9]+$`)
)

// split は ID の名前を host と対象（パスかユニット名）に分ける。
func split(name string) (string, string, bool) {
	h, rest, ok := strings.Cut(name, ":")
	return h, rest, ok && h != "" && rest != ""
}

func cleanAbs(p string) bool { return path.IsAbs(p) && path.Clean(p) == p && p != "/" }

// Validate は Range と宣言を確かめる（E-I6・E-M3）。
func (p *Provider) Validate(res []core.Resource, ranges []core.Range) []core.Diagnostic {
	var diags []core.Diagnostic
	rej := func(o core.Origin, target, format string, a ...any) {
		diags = append(diags, core.Diagnostic{Severity: core.Reject, Origin: o, Target: target, Message: fmt.Sprintf(format, a...)})
	}
	ranged := map[string]bool{}
	for _, rg := range ranges {
		for k := range rg.Args {
			if k != "host" {
				rej(rg.Origin, "ranges", "hostconf の Range の引数は host だけ（%q が書かれている）", k)
			}
		}
		h := rg.Args["host"]
		if p.hosts[h] == nil {
			rej(rg.Origin, "ranges", "host %q が接続設定の hosts に無い", h)
		}
		ranged[h] = true
	}
	for _, r := range res {
		target := r.ID.String()
		h, obj, ok := split(r.ID.Name)
		switch {
		case !ok:
			rej(r.Origin, target, "id は <host>:<絶対パス> か <host>:<ユニット名>")
			continue
		case p.hosts[h] == nil:
			rej(r.Origin, target, "host %q が接続設定の hosts に無い", h)
			continue
		case len(ranges) > 0 && !ranged[h]:
			rej(r.Origin, target, "host %s はどの Range（ranges の host）にも入らない", h)
			continue
		}
		switch r.ID.Kind() {
		case KindFile:
			if !cleanAbs(obj) {
				rej(r.Origin, target, "パス %q は正規の絶対パスで書く", obj)
			}
			if m, ok := r.Fields["mode"].(string); ok && !modeRe.MatchString(m) {
				rej(r.Origin, target, "mode は 8 進の 4 桁（例 \"0644\"）")
			}
			for _, k := range []string{"owner", "group"} {
				if s, ok := r.Fields[k].(string); ok && !nameRe.MatchString(s) {
					rej(r.Origin, target, "%s %q は名前か数", k, s)
				}
			}
		case KindUnit:
			if !unitRe.MatchString(obj) {
				rej(r.Origin, target, "ユニット名 %q は <名前>.service などで書く", obj)
			}
			if l, ok := r.Fields["restart_on"].([]any); ok {
				for _, e := range l {
					s, _ := e.(string)
					fh, fp, ok := split(s)
					if !ok || fh != h || !cleanAbs(fp) {
						rej(r.Origin, target, "restart_on の %q は同じホストのファイル <host>:<絶対パス>", s)
					}
				}
			}
		}
	}
	return diags
}

func (p *Provider) runner(id core.ID) (hostexec.Runner, string, error) {
	h, obj, ok := split(id.Name)
	if !ok {
		return nil, "", fmt.Errorf("id %q の形が違う", id.Name)
	}
	r := p.hosts[h]
	if r == nil {
		return nil, "", fmt.Errorf("host %q が無い", h)
	}
	return r, obj, nil
}

func unknown(err error) core.Observation {
	return core.Observation{Availability: core.Unknown, Note: err.Error()}
}

// Observe は 1 つずつ観測する。
func (p *Provider) Observe(ctx context.Context, ids []core.ID) (map[core.ID]core.Observation, error) {
	out := map[core.ID]core.Observation{}
	for _, id := range ids {
		r, obj, err := p.runner(id)
		if err != nil {
			out[id] = unknown(err)
			continue
		}
		switch id.Kind() {
		case KindFile:
			out[id] = observeFile(ctx, r, obj)
		case KindUnit:
			out[id] = p.observeUnit(ctx, r, obj)
		default:
			out[id] = unknown(fmt.Errorf("type %q を知らない", id.Kind()))
		}
	}
	return out, nil
}

// fileStat は stat の答え。
type fileStat struct {
	exists             bool
	mode, owner, group string
	regular            bool
}

func statFile(ctx context.Context, r hostexec.Runner, p string) (fileStat, error) {
	out, err := r.Run(ctx, []string{"stat", "-c", "%a %U %G %F", "--", p}, nil)
	var ee *hostexec.ExitError
	if errors.As(err, &ee) && strings.Contains(ee.Stderr, "No such file or directory") {
		return fileStat{}, nil
	}
	if err != nil {
		return fileStat{}, fmt.Errorf("stat %s: %w", p, err)
	}
	f := strings.SplitN(strings.TrimSpace(string(out)), " ", 4)
	if len(f) != 4 {
		return fileStat{}, fmt.Errorf("stat %s の出力を読めない", p)
	}
	n, err := strconv.ParseUint(f[0], 8, 32)
	if err != nil {
		return fileStat{}, fmt.Errorf("stat %s の mode を読めない", p)
	}
	return fileStat{exists: true, mode: fmt.Sprintf("%04o", n), owner: f[1], group: f[2], regular: f[3] == "regular file" || f[3] == "regular empty file"}, nil
}

func digests(ctx context.Context, r hostexec.Runner, paths ...string) (map[string]string, error) {
	out, err := r.Run(ctx, append([]string{"sha256sum", "--"}, paths...), nil)
	if err != nil {
		return nil, fmt.Errorf("sha256sum: %w", err)
	}
	m := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		sum, p, ok := strings.Cut(line, "  ")
		if !ok || len(sum) != 64 {
			return nil, errors.New("sha256sum の出力を読めない")
		}
		m[p] = "sha256:" + sum
	}
	return m, nil
}

func observeFile(ctx context.Context, r hostexec.Runner, p string) core.Observation {
	st, err := statFile(ctx, r, p)
	if err != nil {
		return unknown(err)
	}
	if !st.exists {
		return core.Observation{Availability: core.Absent}
	}
	if !st.regular {
		return unknown(fmt.Errorf("%s はふつうのファイルでない", p))
	}
	d, err := digests(ctx, r, p)
	if err != nil {
		return unknown(err)
	}
	return core.Observation{Availability: core.Present, Fields: core.Fields{
		"content": d[p], "mode": st.mode, "owner": st.owner, "group": st.group}}
}

// Apply は宣言どおりにする。
func (p *Provider) Apply(ctx context.Context, _ core.WriteContext, id core.ID, desired core.Fields, current core.Observation) error {
	r, obj, err := p.runner(id)
	if err != nil {
		return err
	}
	switch id.Kind() {
	case KindFile:
		return writeFile(ctx, r, obj, desired, current)
	case KindUnit:
		return p.applyUnit(ctx, r, obj, desired, current)
	}
	return fmt.Errorf("type %q を知らない", id.Kind())
}

// Delete は消す。
func (p *Provider) Delete(ctx context.Context, _ core.WriteContext, id core.ID, _ core.Observation) error {
	r, obj, err := p.runner(id)
	if err != nil {
		return err
	}
	switch id.Kind() {
	case KindFile:
		_, err := r.Run(ctx, []string{"rm", "-f", "--", obj}, nil)
		return err
	case KindUnit:
		return p.deleteUnit(ctx, r, obj)
	}
	return fmt.Errorf("type %q を知らない", id.Kind())
}

// writeFile は中身・mode・owner・group を合わせる。中身を変えるときは一時ファイルに
// 書いて rename する。
func writeFile(ctx context.Context, r hostexec.Runner, p string, desired core.Fields, current core.Observation) error {
	content, hasContent := desired["content"].(string)
	exists := current.Availability == core.Present
	mode, _ := desired["mode"].(string)
	if mode == "" {
		if m, ok := current.Fields["mode"].(string); ok && exists {
			mode = m
		} else {
			mode = "0644"
		}
	}
	owner, _ := desired["owner"].(string)
	group, _ := desired["group"].(string)
	if hasContent && current.Fields["content"] != core.Digest(content) || !exists {
		if _, err := r.Run(ctx, []string{"mkdir", "-p", "--", path.Dir(p)}, nil); err != nil {
			return fmt.Errorf("mkdir %s: %w", path.Dir(p), err)
		}
		out, err := r.Run(ctx, []string{"mktemp", "--", path.Join(path.Dir(p), "."+path.Base(p)+".sakutei-XXXXXX")}, nil)
		if err != nil {
			return fmt.Errorf("mktemp: %w", err)
		}
		tmp := strings.TrimSpace(string(out))
		steps := [][]string{{"chmod", mode, "--", tmp}}
		o, g := owner, group
		if exists {
			// 書かない持ち主は今のファイルのものを保つ。
			if o == "" {
				o, _ = current.Fields["owner"].(string)
			}
			if g == "" {
				g, _ = current.Fields["group"].(string)
			}
		}
		if spec := chownSpec(o, g); spec != "" {
			steps = append(steps, []string{"chown", spec, "--", tmp})
		}
		steps = append(steps, []string{"mv", "-f", "--", tmp, p})
		if _, err := r.Run(ctx, []string{"tee", "--", tmp}, []byte(content)); err != nil {
			r.Run(ctx, []string{"rm", "-f", "--", tmp}, nil)
			return fmt.Errorf("書き込み: %w", err)
		}
		for _, s := range steps {
			if _, err := r.Run(ctx, s, nil); err != nil {
				r.Run(ctx, []string{"rm", "-f", "--", tmp}, nil)
				return fmt.Errorf("%s: %w", s[0], err)
			}
		}
		return nil
	}
	if m, ok := desired["mode"].(string); ok && current.Fields["mode"] != m {
		if _, err := r.Run(ctx, []string{"chmod", m, "--", p}, nil); err != nil {
			return fmt.Errorf("chmod: %w", err)
		}
	}
	if (owner != "" && current.Fields["owner"] != owner) || (group != "" && current.Fields["group"] != group) {
		if _, err := r.Run(ctx, []string{"chown", chownSpec(owner, group), "--", p}, nil); err != nil {
			return fmt.Errorf("chown: %w", err)
		}
	}
	return nil
}

// chownSpec は chown の持ち主の書き方（書かない側は変えない）。
func chownSpec(owner, group string) string {
	switch {
	case owner != "" && group != "":
		return owner + ":" + group
	case owner != "":
		return owner
	case group != "":
		return ":" + group
	}
	return ""
}

// List はファイル・ユニットの集合を数えられないので不完全（宣言に無いもの（unmanaged）を見ない・消さない）。
func (p *Provider) List(context.Context, core.Range) (core.Listing, error) {
	return core.Listing{Complete: false}, nil
}

// ─── ユニット ───

// mark は restart_on のホスト側の記録。
type mark struct {
	Paths  []string `json:"paths"`
	Digest string   `json:"digest"`
}

func (p *Provider) markPath(unit string) string {
	return path.Join(p.stateDir, "restart", unit+".json")
}

func (p *Provider) unitFile(unit string) string { return path.Join(p.unitDir, unit) }

func (p *Provider) observeUnit(ctx context.Context, r hostexec.Runner, unit string) core.Observation {
	out, err := r.Run(ctx, []string{"systemctl", "show", "--property=LoadState,ActiveState,UnitFileState", "--", unit}, nil)
	if err != nil {
		return unknown(fmt.Errorf("systemctl show: %w", err))
	}
	props := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[k] = v
		}
	}
	if props["LoadState"] == "" {
		return unknown(errors.New("systemctl show の出力を読めない"))
	}
	f := core.Fields{"active": props["ActiveState"] == "active"}
	switch props["UnitFileState"] {
	case "enabled", "enabled-runtime":
		f["enabled"] = true
	case "disabled":
		f["enabled"] = false
	}
	file := observeFile(ctx, r, p.unitFile(unit))
	switch file.Availability {
	case core.Present:
		f["content"] = file.Fields["content"]
	case core.Unknown:
		return file
	}
	if props["LoadState"] == "not-found" && file.Availability == core.Absent {
		return core.Observation{Availability: core.Absent}
	}
	switch props["ActiveState"] {
	case "activating", "deactivating", "reloading", "refreshing":
		return core.Observation{Availability: core.Converging, Note: "ユニットが " + props["ActiveState"]}
	}
	m, err := p.readMark(ctx, r, unit)
	if err != nil {
		return unknown(err)
	}
	if m != nil {
		cur, err := markDigest(ctx, r, m.Paths)
		if err != nil {
			return unknown(err)
		}
		on := []any{}
		if cur == m.Digest {
			for _, s := range m.Paths {
				on = append(on, r.Name()+":"+s)
			}
		}
		f["restart_on"] = on // 記録が古ければ空：restart が要る
	}
	return core.Observation{Availability: core.Present, Fields: f}
}

func (p *Provider) readMark(ctx context.Context, r hostexec.Runner, unit string) (*mark, error) {
	out, err := r.Run(ctx, []string{"cat", "--", p.markPath(unit)}, nil)
	var ee *hostexec.ExitError
	if errors.As(err, &ee) && strings.Contains(ee.Stderr, "No such file or directory") {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("restart の記録: %w", err)
	}
	var m mark
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, fmt.Errorf("restart の記録を読めない: %v", err)
	}
	return &m, nil
}

// markDigest は依存するファイルの digest をまとめたもの。無いファイルは "absent"。
func markDigest(ctx context.Context, r hostexec.Runner, paths []string) (string, error) {
	var parts []string
	for _, p := range paths {
		obs := observeFile(ctx, r, p)
		switch obs.Availability {
		case core.Present:
			parts = append(parts, p+"="+obs.Fields["content"].(string))
		case core.Absent:
			parts = append(parts, p+"=absent")
		default:
			return "", fmt.Errorf("%s: %s", p, obs.Note)
		}
	}
	return core.Digest(strings.Join(parts, "\n")), nil
}

func (p *Provider) applyUnit(ctx context.Context, r hostexec.Runner, unit string, desired core.Fields, current core.Observation) error {
	systemctl := func(args ...string) error {
		_, err := r.Run(ctx, append([]string{"systemctl"}, args...), nil)
		if err != nil {
			return fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
		}
		return nil
	}
	restart := false
	if c, ok := desired["content"].(string); ok && current.Fields["content"] != core.Digest(c) {
		file := observeFile(ctx, r, p.unitFile(unit))
		if file.Availability == core.Unknown {
			return errors.New(file.Note)
		}
		if err := writeFile(ctx, r, p.unitFile(unit), core.Fields{"content": c}, file); err != nil {
			return err
		}
		if err := systemctl("daemon-reload"); err != nil {
			return err
		}
		restart = current.Availability == core.Present
	}
	if e, ok := desired["enabled"].(bool); ok && current.Fields["enabled"] != e {
		verb := "disable"
		if e {
			verb = "enable"
		}
		if err := systemctl(verb, "--", unit); err != nil {
			return err
		}
	}
	var paths []string
	if l, ok := desired["restart_on"].([]any); ok {
		for _, e := range l {
			_, fp, _ := split(fmt.Sprint(e))
			paths = append(paths, fp)
		}
		sort.Strings(paths)
		restart = restart || !sameSet(current.Fields["restart_on"], l)
	}
	active, wantActive := desired["active"].(bool)
	isActive, _ := current.Fields["active"].(bool)
	switch {
	case wantActive && !active:
		if isActive {
			if err := systemctl("stop", "--", unit); err != nil {
				return err
			}
		}
	case wantActive && !isActive:
		if err := systemctl("start", "--", unit); err != nil {
			return err
		}
	case restart && isActive:
		if err := systemctl("restart", "--", unit); err != nil {
			return err
		}
	}
	if paths != nil {
		d, err := markDigest(ctx, r, paths)
		if err != nil {
			return err
		}
		data, _ := json.Marshal(mark{Paths: paths, Digest: d})
		mp := p.markPath(unit)
		cur := observeFile(ctx, r, mp)
		if err := writeFile(ctx, r, mp, core.Fields{"content": string(data), "mode": "0600"}, cur); err != nil {
			return fmt.Errorf("restart の記録: %w", err)
		}
	}
	return nil
}

func sameSet(observed any, want []any) bool {
	l, ok := observed.([]any)
	if !ok || len(l) != len(want) {
		return false
	}
	return core.Equal(core.Set, l, want)
}

func (p *Provider) deleteUnit(ctx context.Context, r hostexec.Runner, unit string) error {
	if _, err := r.Run(ctx, []string{"systemctl", "disable", "--now", "--", unit}, nil); err != nil {
		var ee *hostexec.ExitError
		if !errors.As(err, &ee) {
			return err
		}
	}
	for _, f := range []string{p.unitFile(unit), p.markPath(unit)} {
		if _, err := r.Run(ctx, []string{"rm", "-f", "--", f}, nil); err != nil {
			return err
		}
	}
	_, err := r.Run(ctx, []string{"systemctl", "daemon-reload"}, nil)
	return err
}
