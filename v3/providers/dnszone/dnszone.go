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

// Package dnszone は DNS のゾーンファイルのレコードを宣言どおりに保つ provider
// （設計 §7.1、E-B1・N-B11・E-I7）。コアの外のモジュールで、コアの口
// core.Provider・core.Validator だけを持つ。
//
//	providers:                       # つなぎ方の設定
//	  dns:
//	    module: dnszone
//	    settings:
//	      zone_dir: /var/lib/named   # ゾーン <zone> のファイルは <zone_dir>/<zone>zone（example.com.zone）
//	      reload_cmd: [rndc, reload, "{zone}"]   # 任意。書き換えたあとに走らせる
//
//	ranges:                          # intent（範囲は intent にだけ書く、不変条件 24）
//	  - provider: dns
//	    args: {zone: example.com.}
//	resources:
//	  - type: dns/record
//	    id: www.example.com./A       # <FQDN>/<型>
//	    fields: {value: 192.0.2.10, ttl: 300}
//
// 決まり：
//
//   - ゾーンは Range の args.zone（E-B1）。宣言はどれかのゾーンに入る（入らなければ
//     読み込みで断る、E-I6）。重なるゾーン（親と子）は断る（E-K8）。
//   - $ORIGIN・$TTL・@・相対名・省いた owner・括弧の複数行・コメントを読む。名前は
//     小文字の FQDN にそろえ、名前を持つ型（NS・CNAME・PTR・MX・SRV）の値の中の
//     名前も FQDN にする。宣言はこの正規形で書く（違えば正規形を示して断る、E-I7）。
//   - 項目は value（1 つの値）・values（値の集合、ラウンドロビン）・ttl。value と
//     values は一緒に書けない。SOA は宣言できない（provider が持つ）。
//   - 書き換えた Apply・Delete のときだけ SOA のシリアルを上げる（YYYYMMDDnn の形
//     なら今日の日付の 00 か +1 の大きいほう、ほかは +1、E-B1）。
//   - 書き込みは同じディレクトリの一時ファイルに書いて rename。触った RRset 以外の
//     行は書いたまま残す。
//   - ゾーンファイルが無い・読めない・形の誤りは Unknown（無いと読まない、不変条件 3）。
//   - List はゾーンの SOA 以外の RRset 全部（Complete）。
package dnszone

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
)

// Version は Schema の版。比べ方・正規形を変えたら上げる（E-J12）。
const Version = "dnszone/1"

// Kind はレコードの種類の名前。
const Kind = "record"

// Provider はゾーンファイルの provider。
type Provider struct {
	mu        sync.Mutex
	zoneDir   string
	reloadCmd []string
	// zones は intent の Range の zone（Validate で受け取る、E-B1）。
	zones []string
	// Now は SOA のシリアルの日付に使う（テストで差し替える）。
	Now func() time.Time
}

var (
	_ core.Provider  = (*Provider)(nil)
	_ core.Validator = (*Provider)(nil)
)

// Module は settings から provider を作る（load.Module）。
func Module(settings map[string]any) (core.Provider, error) {
	p := &Provider{Now: time.Now}
	for k, v := range settings {
		switch k {
		case "zone_dir":
			s, ok := v.(string)
			if !ok || s == "" {
				return nil, errors.New("settings.zone_dir にディレクトリを書く")
			}
			p.zoneDir = s
		case "reload_cmd":
			l, ok := v.([]any)
			if !ok || len(l) == 0 {
				return nil, errors.New("settings.reload_cmd にコマンドの argv を書く")
			}
			for _, e := range l {
				s, ok := e.(string)
				if !ok || s == "" {
					return nil, errors.New("settings.reload_cmd にコマンドの argv を書く")
				}
				p.reloadCmd = append(p.reloadCmd, s)
			}
		default:
			return nil, fmt.Errorf("settings に知らない項目 %q（書ける項目: reload_cmd, zone_dir）", k)
		}
	}
	if p.zoneDir == "" {
		return nil, errors.New("settings.zone_dir を書く")
	}
	return p, nil
}

// Schema は record の項目。
func (p *Provider) Schema() core.Schema {
	return core.Schema{Version: Version, Kinds: map[string]core.KindSchema{
		Kind: {Fields: map[string]core.FieldSpec{
			"value":  {Type: core.String},
			"values": {Type: core.Set},
			"ttl":    {Type: core.Int},
		}},
	}}
}

// supported は宣言できる型。
var supported = map[string]bool{"A": true, "AAAA": true, "CNAME": true, "MX": true, "TXT": true, "NS": true,
	"PTR": true, "SRV": true, "CAA": true}

// File は zone のファイルのパス。
func (p *Provider) File(zone string) string { return filepath.Join(p.zoneDir, zone+"zone") }

// split は ID の名前を FQDN と型に分ける。
func split(id core.ID) (name, typ string, ok bool) {
	i := strings.LastIndex(id.Name, "/")
	if i <= 0 || i == len(id.Name)-1 {
		return "", "", false
	}
	return id.Name[:i], id.Name[i+1:], true
}

// zoneOf は name を含むゾーンのうち最も深いもの。
func zoneOf(zones []string, name string) string {
	best := ""
	for _, z := range zones {
		if inZone(name, z) && len(z) > len(best) {
			best = z
		}
	}
	return best
}

func inZone(name, zone string) bool { return name == zone || strings.HasSuffix(name, "."+zone) }

// Validate は範囲と宣言を確かめ、範囲のゾーンを覚える（E-B1・E-I6・E-I7・E-K8）。
func (p *Provider) Validate(res []core.Resource, ranges []core.Range) []core.Diagnostic {
	var diags []core.Diagnostic
	rej := func(o core.Origin, target, format string, a ...any) {
		diags = append(diags, core.Diagnostic{Severity: core.Reject, Origin: o, Target: target, Message: fmt.Sprintf(format, a...)})
	}
	var zones []string
	for _, rg := range ranges {
		z, ok := rg.Args["zone"]
		for k := range rg.Args {
			if k != "zone" {
				rej(rg.Origin, "ranges", "dnszone の範囲の引数は zone だけ（%q が書かれている）", k)
			}
		}
		switch {
		case !ok:
			rej(rg.Origin, "ranges", "dnszone の範囲に args.zone を書く")
			continue
		case !fqdnOK(z):
			rej(rg.Origin, "ranges", "zone %q は小文字の FQDN（末尾の . つき）で書く", z)
			continue
		}
		for _, o := range zones {
			if inZone(z, o) || inZone(o, z) {
				rej(rg.Origin, "ranges", "zone %s が zone %s と重なる", z, o)
			}
		}
		zones = append(zones, z)
	}
	p.mu.Lock()
	p.zones = zones
	p.mu.Unlock()
	for _, r := range res {
		target := r.ID.String()
		name, typ, ok := split(r.ID)
		switch {
		case !ok:
			rej(r.Origin, target, "id は <FQDN>/<型>（例 www.example.com./A）")
			continue
		case !fqdnOK(name):
			rej(r.Origin, target, "名前 %q は小文字の FQDN（末尾の . つき）で書く（相対名は読み方で二重に作る、E-I7）", name)
			continue
		case typ == "SOA":
			rej(r.Origin, target, "SOA は宣言できない（シリアルは provider が上げる）")
			continue
		case !supported[typ]:
			rej(r.Origin, target, "型 %q は扱わない（扱う型: %s）", typ, strings.Join(sortedTypes(), ", "))
			continue
		}
		zone := zoneOf(zones, name)
		if zone == "" {
			rej(r.Origin, target, "どの範囲（ranges の zone）にも入らない")
			continue
		}
		_, hasValue := r.Fields["value"]
		_, hasValues := r.Fields["values"]
		if hasValue && hasValues {
			rej(r.Origin, target, "value と values は一緒に書けない")
		}
		var vals []string
		if v, ok := r.Fields["value"].(string); ok {
			vals = append(vals, v)
		}
		if l, ok := r.Fields["values"].([]any); ok {
			for _, e := range l {
				s, isStr := e.(string)
				if !isStr {
					rej(r.Origin, target, "values は文字列の集合")
					continue
				}
				vals = append(vals, s)
			}
		}
		for _, v := range vals {
			c, err := canonRdata(typ, v, zone)
			switch {
			case err != nil:
				rej(r.Origin, target, "値 %q: %v", v, err)
			case c != v:
				rej(r.Origin, target, "値 %q は正規形 %q で書く（E-I7）", v, c)
			}
		}
		if t, ok := r.Fields["ttl"].(int64); ok && (t < 0 || t > 2147483647) {
			rej(r.Origin, target, "ttl は 0〜2147483647")
		}
	}
	return diags
}

func sortedTypes() []string {
	out := make([]string, 0, len(supported))
	for t := range supported {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func fqdnOK(s string) bool {
	if !strings.HasSuffix(s, ".") || s == "." || s != strings.ToLower(s) || strings.ContainsAny(s, " \t;()\"/") {
		return false
	}
	return !strings.Contains(s, "..")
}

func (p *Provider) zoneList() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.zones...)
}

// Observe はゾーンごとにファイルを 1 回読む（E-D8）。
func (p *Provider) Observe(_ context.Context, ids []core.ID) (map[core.ID]core.Observation, error) {
	zones := p.zoneList()
	out := map[core.ID]core.Observation{}
	files := map[string]*zoneFile{}
	errs := map[string]error{}
	for _, id := range ids {
		name, typ, ok := split(id)
		if !ok {
			out[id] = core.Observation{Availability: core.Unknown, Note: "id の形が違う"}
			continue
		}
		zone := zoneOf(zones, name)
		if zone == "" {
			out[id] = core.Observation{Availability: core.Unknown, Note: "どの範囲のゾーンにも入らない"}
			continue
		}
		zf, done := files[zone]
		if !done && errs[zone] == nil {
			zf, errs[zone] = readZone(p.File(zone), zone)
			files[zone] = zf
		}
		if err := errs[zone]; err != nil {
			out[id] = core.Observation{Availability: core.Unknown, Note: err.Error()}
			continue
		}
		out[id] = zf.observe(name, typ)
	}
	return out, nil
}

// List はゾーンの SOA 以外の RRset（E-K2：読めなければ誤り）。
func (p *Provider) List(_ context.Context, r core.Range) (core.Listing, error) {
	zone := r.Args["zone"]
	zf, err := readZone(p.File(zone), zone)
	if err != nil {
		return core.Listing{}, err
	}
	seen := map[string]bool{}
	var ids []core.ID
	for _, rr := range zf.rrs {
		key := rr.owner + "/" + rr.typ
		if rr.typ == "SOA" || seen[key] || !inZone(rr.owner, zone) {
			continue
		}
		seen[key] = true
		ids = append(ids, core.ID{Type: r.Provider + "/" + Kind, Name: key})
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].Name < ids[j].Name })
	return core.Listing{IDs: ids, Complete: true}, nil
}

// Apply は RRset を宣言の値で置き換え、SOA のシリアルを上げる（E-B1）。
func (p *Provider) Apply(ctx context.Context, _ core.WriteContext, id core.ID, desired core.Fields, current core.Observation) error {
	name, typ, ok := split(id)
	if !ok {
		return fmt.Errorf("id %q の形が違う", id.Name)
	}
	var vals []string
	if v, ok := desired["value"].(string); ok {
		vals = []string{v}
	}
	if l, ok := desired["values"].([]any); ok {
		for _, e := range l {
			vals = append(vals, fmt.Sprint(e))
		}
		sort.Strings(vals)
	}
	ttl := int64(-1)
	if t, ok := desired["ttl"].(int64); ok {
		ttl = t
	}
	return p.rewrite(ctx, name, func(zf *zoneFile) error {
		if len(vals) == 0 {
			// 値を宣言しない（ttl だけ）：今の値を保つ。
			o := zf.observe(name, typ)
			if o.Availability != core.Present {
				return errors.New("無い RRset を作るには value か values を書く")
			}
			for _, e := range o.Fields["values"].([]any) {
				vals = append(vals, e.(string))
			}
		}
		if ttl < 0 {
			ttl = zf.ttlOf(name, typ)
		}
		var lines []string
		for _, v := range vals {
			lines = append(lines, rrLine(name, ttl, typ, v))
		}
		zf.replace(name, typ, lines)
		return nil
	})
}

// Delete は RRset を消し、SOA のシリアルを上げる。
func (p *Provider) Delete(ctx context.Context, _ core.WriteContext, id core.ID, _ core.Observation) error {
	name, typ, ok := split(id)
	if !ok {
		return fmt.Errorf("id %q の形が違う", id.Name)
	}
	return p.rewrite(ctx, name, func(zf *zoneFile) error {
		zf.replace(name, typ, nil)
		return nil
	})
}

func rrLine(name string, ttl int64, typ, v string) string {
	if ttl >= 0 {
		return fmt.Sprintf("%s\t%d\tIN\t%s\t%s", name, ttl, typ, v)
	}
	return fmt.Sprintf("%s\tIN\t%s\t%s", name, typ, v)
}

// rewrite はゾーンファイルを読み、edit で書き換え、シリアルを上げて書き、
// 再読み込みのコマンドを走らせる。
func (p *Provider) rewrite(ctx context.Context, name string, edit func(*zoneFile) error) error {
	zone := zoneOf(p.zoneList(), name)
	if zone == "" {
		return fmt.Errorf("%s はどの範囲のゾーンにも入らない", name)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	path := p.File(zone)
	zf, err := readZone(path, zone)
	if err != nil {
		return err
	}
	if err := edit(zf); err != nil {
		return err
	}
	if err := zf.bumpSerial(p.Now()); err != nil {
		return err
	}
	if err := writeAtomic(path, []byte(zf.render())); err != nil {
		return err
	}
	if len(p.reloadCmd) == 0 {
		return nil
	}
	argv := make([]string, len(p.reloadCmd))
	for i, a := range p.reloadCmd {
		argv[i] = strings.ReplaceAll(a, "{zone}", strings.TrimSuffix(zone, "."))
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ゾーンファイルは書いたが再読み込み %v に失敗: %v: %s", argv, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func writeAtomic(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".sakutei-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ParseSerial は SOA のシリアルを読む（テスト用）。
func ParseSerial(path, zone string) (int64, error) {
	zf, err := readZone(path, zone)
	if err != nil {
		return 0, err
	}
	_, _, _, serial, err := zf.serialToken()
	return serial, err
}

func atoi(s string) (int64, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}
