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

// Package guestos はゲスト OS を ssh 越しに観測するだけの provider（module 名
// `guest-os`、設計 §7.1 の guest-os、E-E4・E-A8）。v2 の internal/guestos を移した。
// ホストに届くか・OS の識別（/etc/os-release）・導入の目印（利用者が決める
// ファイルの中身）を読み、OS の導入の完了を「BMC から見て電源が切れた」でなく
// 「入れた OS が目印つきで起動した」で判断できるようにする（E-E4）。使い道は
// 手順の wait・precondition と depends_on の when（E-A8）。
//
//	providers:                                   # つなぎ方の設定（つなぎ方だけ）
//	  guest:
//	    module: guest-os
//	    settings:
//	      hosts:
//	        node1:
//	          ssh: {address: 192.0.2.21, user: monitor, key: /etc/sakutei/id_ed25519, known_hosts: /etc/sakutei/known_hosts}
//
//	resources:                                   # intent（目印は Resource の項目）
//	  - type: guest/os
//	    id: node1                                # ホストの名前（settings.hosts の鍵）
//	    fields:
//	      markers: {installed: /etc/sakutei-installed}
//	      os_id: alpine                          # 任意。違えば観測だけのずれとして報告する
//	procedures:
//	  - name: os-install
//	    steps:
//	      - name: booted
//	        wait: {when: 'observe(guest/os/node1).marker.installed == "sakutei-2026-10-27"', timeout: 30m}
//
// 種類 os（観測だけ、ID はホストの名前）。項目：
//
//   - os_id・os_version_id・os_pretty_name：os-release の ID・VERSION_ID・
//     PRETTY_NAME（/etc/os-release、無ければ /usr/lib/os-release。どちらも無ければ
//     項目が無い）（E-E4）
//   - hostname：/proc/sys/kernel/hostname。boot_id：/proc/sys/kernel/random/boot_id
//     （起動ごとに変わる。再起動したかを Condition で比べる）（E-E4・E-A8）
//   - markers（Map、宣言する項目）：目印の名前 → ホストの上の絶対パス。観測は
//     読んだ目印のパスをそのまま返す（宣言どおりなのでずれにならない）（E-E4）
//   - marker（Map、観測だけ）：目印の名前 → ファイルの中身（末尾の空白・改行を
//     落とす。4 KiB まで）。ファイルが無い目印は鍵が無い（Condition では null）。
//     Condition は observe(guest/os/node1).marker.installed で読む（E-E4・E-A8）
//
// 決まり：
//
//   - 目印のパスは Resource の項目 markers に書く（設計 §7.1「目印は Resource の
//     項目」）。観測はそのパスを知らなければならないので、Validate が宣言の
//     markers を ID ごとに覚え、Observe がそれを読む（nftables が範囲を Validate で
//     覚えるのと同じ。コアは宣言を Observe に渡さない）。宣言の無い ID（Condition
//     だけが参照する）は目印なしで観測する（E-E4）。
//   - 宣言できる項目は markers と、期待の値 os_id・os_version_id・hostname だけ
//     （redfish の firmware の version と同じく、違えば観測だけのずれとして報告
//     し、手順の desired で読める）。os_pretty_name・boot_id・marker は観測だけで
//     宣言を断る。範囲・ensure: absent も断る（List と削除が無い）（E-E4・E-M3）。
//   - 1 回の観測は 1 本のコマンド（sh の小さなスクリプト）。読むパスは引数で渡し、
//     中身は od で 16 進にして返す（どんなバイトでも区切りとぶつからない）。最後の
//     行 "end" が無ければ途中で切れたとして Unknown（E-K1）。
//   - 届かない・認証の失敗・ホスト鍵の不一致・sudo が通らない・壊れた出力・読めない
//     目印（権限・ディレクトリ・4 KiB より大きい）は Unknown。Absent にしない
//     （不変条件 3、E-K1）。「まだ起動していない」と「壊れた」は観測では
//     見分けられないので、見分けるのは手順の wait の timeout。
//   - 無いパスを「無い」とするのは、上の一番近いディレクトリを覗けるときだけ。
//     覗けない観測用のユーザーは見分けられないので読めない（Unknown）。
//   - 書き込みが無いので、つなぎ方は観測のためだけで書き込み用の資格を持たない
//     （E-G5）。ホストにはエージェントを置かず、sh・dd・od（busybox でもよい）
//     だけを使う（E-E1）。値を出すコマンドは目印にできない（観測で任意の
//     コマンドを走らせない）。
//   - List を持たず（誤りを返す）、Apply は誤り、Delete は断る（観測だけ）。
package guestos

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/sdk/hostexec"
)

// Version は Schema の版。読み方・項目を変えたら上げる（E-J12）。
const Version = "guest-os/1"

// Kind は種類の名前。
const Kind = "os"

// 項目。
const (
	FieldOSID         = "os_id"
	FieldOSVersionID  = "os_version_id"
	FieldOSPrettyName = "os_pretty_name"
	FieldHostname     = "hostname"
	FieldBootID       = "boot_id"
	// FieldMarkers は宣言する目印（名前 → 絶対パス）。
	FieldMarkers = "markers"
	// FieldMarker は観測した目印の中身（名前 → 値）。
	FieldMarker = "marker"
)

// MaxMarkerBytes は目印のファイルの大きさの上限。目印は短い値で、これより
// 大きいファイルは読めない（Unknown）として扱う。
const MaxMarkerBytes = 4096

// 読むパス（ホストの上の）。
const (
	osReleasePath    = "/etc/os-release"
	osReleaseAltPath = "/usr/lib/os-release"
	hostnamePath     = "/proc/sys/kernel/hostname"
	bootIDPath       = "/proc/sys/kernel/random/boot_id"
)

// declarable は宣言できる項目（ほかは観測だけ）。
var declarable = []string{FieldHostname, FieldMarkers, FieldOSID, FieldOSVersionID}

var markerName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Marker は目印 1 つ：名前と、ホストの上のファイル。
type Marker struct {
	Name string
	Path string
}

// Provider は guest-os の provider。
type Provider struct {
	hosts map[string]hostexec.Runner
	mu    sync.Mutex
	// markers は Validate が覚えた ID（ホストの名前）ごとの目印（名前の順）。
	markers map[string][]Marker
}

var (
	_ core.Provider  = (*Provider)(nil)
	_ core.Validator = (*Provider)(nil)
)

// New は Runner から provider を作る（テスト・自前のバイナリ用）。
func New(hosts map[string]hostexec.Runner) *Provider {
	return &Provider{hosts: hosts, markers: map[string][]Marker{}}
}

// Module は settings から provider を作る（load.Module）。settings は hosts
// （つなぎ方）だけ。
func Module(settings map[string]any) (core.Provider, error) {
	var hosts map[string]hostexec.Runner
	for k, v := range settings {
		if k != "hosts" {
			return nil, fmt.Errorf("settings に知らない項目 %q（書ける項目: hosts。目印は Resource の項目 markers に書く）", k)
		}
		hs, err := hostexec.ParseHosts(v)
		if err != nil {
			return nil, err
		}
		hosts = map[string]hostexec.Runner{}
		for n, h := range hs {
			hosts[n] = h.Runner
		}
	}
	if hosts == nil {
		return nil, errors.New("settings.hosts を書く")
	}
	return New(hosts), nil
}

// Schema は種類 os（観測だけ、E-E4）。観測だけの項目も Condition から読めるよう
// Schema に置く（宣言は Validate が断る）。
func (p *Provider) Schema() core.Schema {
	str := core.FieldSpec{Type: core.String}
	return core.Schema{Version: Version, Kinds: map[string]core.KindSchema{
		Kind: {ObserveOnly: true, Fields: map[string]core.FieldSpec{
			FieldOSID: str, FieldOSVersionID: str, FieldOSPrettyName: str,
			FieldHostname: str, FieldBootID: str,
			FieldMarkers: {Type: core.Map}, FieldMarker: {Type: core.Map},
		}},
	}}
}

// ParseMarkers は宣言の markers（名前 → 絶対パス）を読む（名前の順）。
func ParseMarkers(v any) ([]Marker, error) {
	var m map[string]any
	switch x := v.(type) {
	case map[string]any:
		m = x
	case core.Fields:
		m = x
	default:
		return nil, errors.New("markers は目印の名前 → 絶対パスのマップで書く（例 {installed: /etc/sakutei-installed}）")
	}
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]Marker, 0, len(names))
	for _, n := range names {
		if !markerName.MatchString(n) {
			return nil, fmt.Errorf("目印の名前 %q は英小文字・数字・_・-（頭は英小文字か数字）", n)
		}
		s, ok := m[n].(string)
		if !ok || !path.IsAbs(s) || path.Clean(s) != s || s == "/" || strings.ContainsAny(s, "\x00\n") {
			return nil, fmt.Errorf("目印 %s のパスは正規の絶対パスで書く（例 /etc/sakutei-installed）", n)
		}
		out = append(out, Marker{Name: n, Path: s})
	}
	return out, nil
}

// Validate は宣言を確かめ、ID ごとの目印を覚える（E-E4・E-M3）。範囲は書けない
// （List を持たない）。
func (p *Provider) Validate(res []core.Resource, ranges []core.Range) []core.Diagnostic {
	var diags []core.Diagnostic
	rej := func(o core.Origin, target, format string, a ...any) {
		diags = append(diags, core.Diagnostic{Severity: core.Reject, Origin: o, Target: target, Message: fmt.Sprintf(format, a...)})
	}
	for _, rg := range ranges {
		rej(rg.Origin, "ranges", "guest-os は範囲を持たない（観測だけで List と prune が無い）")
	}
	markers := map[string][]Marker{}
	for _, r := range res {
		target := r.ID.String()
		if r.ID.Kind() != Kind {
			rej(r.Origin, target, "guest-os の種類は %s だけ", Kind)
			continue
		}
		if _, ok := p.hosts[r.ID.Name]; !ok {
			rej(r.Origin, target, "id はホストの名前（settings.hosts の鍵）。%q が無い（ある: %s）", r.ID.Name, strings.Join(p.hostNames(), ", "))
			continue
		}
		if r.Ensure == core.EnsureAbsent {
			rej(r.Origin, target, "guest-os は観測だけで消せない（ensure: absent は書けない）")
		}
		for _, k := range sortedKeys(r.Fields) {
			if !contains(declarable, k) {
				rej(r.Origin, target, "項目 %q は観測だけで宣言できない（宣言できる項目: %s。目印の値は Condition で observe(%s).marker.<名前> を比べる）",
					k, strings.Join(declarable, ", "), target)
			}
		}
		if v, ok := r.Fields[FieldMarkers]; ok {
			ms, err := ParseMarkers(v)
			if err != nil {
				rej(r.Origin, target, "%v", err)
				continue
			}
			markers[r.ID.Name] = ms
		}
	}
	if len(diags) == 0 {
		p.mu.Lock()
		p.markers = markers
		p.mu.Unlock()
	}
	return diags
}

func (p *Provider) hostNames() []string {
	names := make([]string, 0, len(p.hosts))
	for n := range p.hosts {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Observe はホストごとに 1 本のコマンドで観測する。誤りは ID ごとの Unknown にする。
func (p *Provider) Observe(ctx context.Context, ids []core.ID) (map[core.ID]core.Observation, error) {
	out := map[core.ID]core.Observation{}
	for _, id := range ids {
		out[id] = p.observe(ctx, id)
	}
	return out, nil
}

func (p *Provider) observe(ctx context.Context, id core.ID) core.Observation {
	unknown := func(format string, a ...any) core.Observation {
		return core.Observation{Availability: core.Unknown, Note: fmt.Sprintf(format, a...)}
	}
	if id.Kind() != Kind {
		return unknown("guest-os に種類 %q は無い", id.Kind())
	}
	r, ok := p.hosts[id.Name]
	if !ok {
		return unknown("ホスト %q が settings.hosts に無い", id.Name)
	}
	p.mu.Lock()
	markers := p.markers[id.Name]
	p.mu.Unlock()
	raw, err := r.Run(ctx, argv(markers), nil)
	if err != nil {
		// 届かない・認証の失敗・ホスト鍵の不一致・sudo が通らない（不変条件 3）。
		return unknown("ホスト %s を観測できない: %v", id.Name, err)
	}
	f, err := parse(raw, markers)
	if err != nil {
		return unknown("ホスト %s: %v", id.Name, err)
	}
	return core.Observation{Availability: core.Present, Fields: f}
}

// script はホストの上で走る観測のスクリプト。引数は os-release の 2 つの
// パス・ホスト名・boot_id、そのあと目印の名前とパスの組。無いパスを「無い」と
// するのは、上の一番近いディレクトリを覗けるときだけ（覗けない観測用の
// ユーザーは見分けられないので unreadable）。
const script = `emit() {
  if [ ! -e "$2" ]; then
    d=$2
    while d=$(dirname -- "$d"); [ ! -d "$d" ] && [ "$d" != / ] && [ "$d" != . ]; do :; done
    if [ -x "$d" ]; then echo "$1 absent"; else echo "$1 unreadable"; fi
  elif [ -d "$2" ] || [ ! -r "$2" ]; then echo "$1 unreadable"
  else
    echo "$1 ok"
    dd if="$2" bs=4097 count=1 2>/dev/null | od -An -v -tx1 || exit 3
  fi
  echo .
}
f=$1; [ -e "$f" ] || f=$2
emit os-release "$f"
emit hostname "$3"
emit boot-id "$4"
shift 4
while [ $# -ge 2 ]; do emit "marker.$1" "$2"; shift 2; done
echo end
`

func argv(markers []Marker) []string {
	a := []string{"sh", "-c", script, "sh", osReleasePath, osReleaseAltPath, hostnamePath, bootIDPath}
	for _, m := range markers {
		a = append(a, m.Name, m.Path)
	}
	return a
}

// item はスクリプトの出力の 1 項目。
type item struct {
	status string // ok | absent | unreadable
	data   []byte
}

// parse はスクリプトの出力を観測の項目にする。壊れた・途中で切れた出力・読めない
// 項目は誤り（Unknown にする、E-K1）。
func parse(out []byte, markers []Marker) (core.Fields, error) {
	items := map[string]item{}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	ended := false
	for sc.Scan() {
		line := sc.Text()
		if line == "end" {
			ended = true
			break
		}
		key, status, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("思わない出力の行 %q", firstChars(line))
		}
		var hexText strings.Builder
		closed := false
		for sc.Scan() {
			l := sc.Text()
			if l == "." {
				closed = true
				break
			}
			hexText.WriteString(strings.Join(strings.Fields(l), ""))
		}
		if !closed {
			return nil, fmt.Errorf("出力が %s の途中で切れた", key)
		}
		data, err := hex.DecodeString(hexText.String())
		if err != nil {
			return nil, fmt.Errorf("%s の出力を読めない: %v", key, err)
		}
		items[key] = item{status: status, data: data}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !ended {
		return nil, errors.New("出力が終わりの前に切れた")
	}
	need := func(key string) (item, error) {
		it, ok := items[key]
		switch {
		case !ok:
			return item{}, fmt.Errorf("出力に %s が無い", key)
		case it.status == "unreadable":
			return item{}, fmt.Errorf("%s をホストの上で読めない", key)
		case it.status != "ok" && it.status != "absent":
			return item{}, fmt.Errorf("%s: 思わない状態 %q", key, it.status)
		}
		return it, nil
	}

	f := core.Fields{}
	it, err := need("os-release")
	if err != nil {
		return nil, err
	}
	if it.status == "ok" {
		rel := parseOSRelease(string(it.data))
		for field, key := range map[string]string{FieldOSID: "ID", FieldOSVersionID: "VERSION_ID", FieldOSPrettyName: "PRETTY_NAME"} {
			if v, ok := rel[key]; ok {
				f[field] = v
			}
		}
	}
	for field, key := range map[string]string{FieldHostname: "hostname", FieldBootID: "boot-id"} {
		it, err := need(key)
		if err != nil {
			return nil, err
		}
		if it.status == "ok" {
			f[field] = trim(it.data)
		}
	}
	if len(markers) == 0 {
		return f, nil
	}
	paths, values := map[string]any{}, map[string]any{}
	for _, m := range markers {
		key := "marker." + m.Name
		it, err := need(key)
		if err != nil {
			return nil, fmt.Errorf("%v（%s）", err, m.Path)
		}
		paths[m.Name] = m.Path
		if it.status != "ok" {
			continue // 目印のファイルが無い：鍵も無い
		}
		if len(it.data) > MaxMarkerBytes {
			return nil, fmt.Errorf("目印 %s（%s）が %d バイトより大きい（目印は短い値）", m.Name, m.Path, MaxMarkerBytes)
		}
		values[m.Name] = trim(it.data)
	}
	f[FieldMarkers], f[FieldMarker] = paths, values
	return f, nil
}

func trim(b []byte) string { return strings.TrimRight(string(b), " \t\r\n") }

func firstChars(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

// parseOSRelease は os-release（KEY=value、値は "…" か '…' で囲めて、"…" の
// 中では \ で $ " \ ` を逃がせる）を読む。読めない行は飛ばす。
func parseOSRelease(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, v, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			continue
		}
		switch {
		case len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"':
			v = v[1 : len(v)-1]
			var b strings.Builder
			for i := 0; i < len(v); i++ {
				if v[i] == '\\' && i+1 < len(v) && strings.IndexByte("$\"\\`", v[i+1]) >= 0 {
					i++
				}
				b.WriteByte(v[i])
			}
			v = b.String()
		case len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'':
			v = v[1 : len(v)-1]
		}
		out[key] = v
	}
	return out
}

// List は持たない（観測だけ。範囲は Validate が断る）。
func (p *Provider) List(context.Context, core.Range) (core.Listing, error) {
	return core.Listing{}, errors.New("guest-os は List を持たない（観測だけ）")
}

// Apply は持たない（観測だけ。コアは ObserveOnly の種類に呼ばない）。ゲスト OS の
// 中を変えるのは別の provider（hostconf・nftables など）か provisioner。
func (p *Provider) Apply(context.Context, core.WriteContext, core.ID, core.Fields, core.Observation) error {
	return errors.New("guest-os は観測だけで Apply を持たない")
}

// Delete は断る（観測だけ）。
func (p *Provider) Delete(context.Context, core.WriteContext, core.ID, core.Observation) error {
	return &core.RefuseError{Reason: "guest-os は観測だけで消せない"}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func sortedKeys(m core.Fields) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
