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

// Package guestos はゲスト OS を観測するだけの provider "guest-os"（Backlog #86、
// §29.1）。ssh 越しの実行の層（internal/remote、#130）の上で、ホストに届くか・
// OS の識別（/etc/os-release）・導入の目印（利用者が決めるファイルの中身）を読む。
//
// 使い道は Workflow の wait / require（#87）と、多相 Task（#51）の相ごとの入口の
// Observe: OS の導入の完了を「BMC から見て電源が切れた」ではなく「ゲスト OS が
// 目印つきで起動した」で判断する。
//
//	steps:
//	  - wait:
//	      resource: guest-os/{target}
//	      fields:
//	        os_id: alpine
//	        marker.installed: sakutei-2026-10-27
//	      timeout: 10m
//
// 設計判断:
//
//   - 観測だけ。Apply は常に PermanentError で、intent の resources に書くことも
//     読み込みの時点で断る（ValidateIntent）。ゲスト OS の中を変えるのは provisioner
//     （責務の外、§29.3 (c)）か、別の provider（nftables・dnszone など）の仕事。
//   - 届かない・認証の失敗・ホスト鍵の不一致・読めない目印は観測できない
//     （unknown）。absent にしない（§3）。「まだ起動していない」と「壊れた」は
//     観測からは見分けられないので、見分けるのは Workflow の待ちの timeout
//     （過ぎれば halt）。
//   - 1 回の Read は 1 本の ssh のコマンド（sh の小さなスクリプト）。読むパスは
//     引数で渡し、ファイルの中身は od で 16 進にして返す（どんなバイトでも
//     区切りとぶつからない）。最後の行 "end" が無ければ途中で切れたとして unknown。
//   - 目印のファイルが無いのは観測の事実（そのフィールドが無い）。待ちの値とは
//     一致しないので、wait は満たされない。
//   - ID はホストの名前（remote_config の hosts の名前）。Workflow の対象と同じ名前
//     にしておけば resource: guest-os/{target} と書ける。
package guestos

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/internal/remote"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// Name は provider の名前。
const Name = "guest-os"

// ProviderVersion は観測の正規化の版（§10.4）。読み方・フィールドを変えたら上げる。
const ProviderVersion = Name + "-v1"

// フィールド。
const (
	// FieldHost はホストの名前（= ID、identity）。
	FieldHost = "host"
	// FieldOSID は os-release の ID（例 "alpine"・"debian"・"rhel"）。
	FieldOSID = "os_id"
	// FieldOSVersionID は os-release の VERSION_ID（例 "3.24.3"）。
	FieldOSVersionID = "os_version_id"
	// FieldOSPrettyName は os-release の PRETTY_NAME。
	FieldOSPrettyName = "os_pretty_name"
	// FieldHostname はカーネルのホスト名（/proc/sys/kernel/hostname）。
	FieldHostname = "hostname"
	// FieldBootID は起動ごとに変わる ID（/proc/sys/kernel/random/boot_id）。
	FieldBootID = "boot_id"
	// MarkerPrefix は目印のフィールドの頭（"marker.<名前>"）。
	MarkerPrefix = "marker."
)

// MaxMarkerBytes は目印のファイルの大きさの上限。目印は短い値で、これより
// 大きいファイルは読めない（unknown）として扱う。
const MaxMarkerBytes = 4096

// 読むパス（ホストの上の）。
const (
	osReleasePath    = "/etc/os-release"
	osReleaseAltPath = "/usr/lib/os-release"
	hostnamePath     = "/proc/sys/kernel/hostname"
	bootIDPath       = "/proc/sys/kernel/random/boot_id"
)

// Marker は導入の目印: 名前（フィールド marker.<名前>）と、ホストの上のファイル。
type Marker struct {
	Name string
	Path string
}

var markerName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// ParseMarkers は "installed=/etc/sakutei-installed,role=/etc/role" を読む。
// 名前は英小文字・数字・_・-、パスは絶対パス。空なら目印なし。
func ParseMarkers(s string) ([]Marker, error) {
	var out []Marker
	seen := map[string]bool{}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, path, ok := strings.Cut(item, "=")
		name, path = strings.TrimSpace(name), strings.TrimSpace(path)
		if !ok || name == "" || path == "" {
			return nil, fmt.Errorf("marker %q: want <name>=<absolute path>", item)
		}
		if !markerName.MatchString(name) {
			return nil, fmt.Errorf("marker %q: the name must match %s", name, markerName)
		}
		if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\x00\n") {
			return nil, fmt.Errorf("marker %s: %q is not an absolute path", name, path)
		}
		if seen[name] {
			return nil, fmt.Errorf("marker %s is listed twice", name)
		}
		seen[name] = true
		out = append(out, Marker{Name: name, Path: path})
	}
	return out, nil
}

// Provider は guest-os の provider。
type Provider struct {
	hosts   map[string]remote.Executor
	markers []Marker
}

var (
	_ provider.Provider        = (*Provider)(nil)
	_ provider.IntentValidator = (*Provider)(nil)
)

// New は hosts（名前 → Executor）を観測する provider を作る。
func New(hosts map[string]remote.Executor, markers []Marker) (*Provider, error) {
	if len(hosts) == 0 {
		return nil, errors.New("guest-os: no host is named")
	}
	return &Provider{hosts: hosts, markers: markers}, nil
}

// Schema は観測するフィールド。どれも Computed にしない: Workflow の wait /
// require が Diff で比べるから（Computed は Diff が比べない）。intent に書く
// ことは ValidateIntent が断る。
func (p *Provider) Schema() provider.Schema {
	str := provider.FieldSpec{Kind: core.FieldKindString}
	fields := map[string]provider.FieldSpec{
		FieldHost:         {Kind: core.FieldKindString, Identity: true},
		FieldOSID:         str,
		FieldOSVersionID:  str,
		FieldOSPrettyName: str,
		FieldHostname:     str,
		FieldBootID:       str,
	}
	for _, m := range p.markers {
		fields[MarkerPrefix+m.Name] = str
	}
	return provider.Schema{Fields: fields}
}

// Capabilities は標準階級の観測だけ: List も BulkObserve も無く、消さない。
func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		TrueObserve:     true,
		ProviderVersion: ProviderVersion,
	}
}

// ValidateIntent は intent の resources に guest-os を書くのを断る（観測だけ）。
func (p *Provider) ValidateIntent(localID string, _ map[string]string) error {
	return fmt.Errorf("%s %q: %s observes only and cannot be declared as a resource; wait for it in a Workflow step (resource: %s/{target})",
		Name, localID, Name, Name)
}

// Apply は常に断る（観測だけ）。
func (p *Provider) Apply(_ context.Context, plan core.Plan) error {
	if len(plan.Ops) == 0 {
		return nil
	}
	return provider.NewPermanentError(fmt.Errorf("%s: observes only; nothing can be applied to %s", Name, plan.Ops[0].Target))
}

// Read はホスト id のゲスト OS を観測する。desired を見ない（ADR-0002）。
func (p *Provider) Read(ctx context.Context, id string) (core.Observation, error) {
	now := time.Now()
	unknown := core.Observation{Availability: core.AvailabilityUnknown, ObservedAt: now}
	h, ok := p.hosts[id]
	if !ok {
		return unknown, provider.NewPermanentError(fmt.Errorf("%s: Read %q: no such host (guest_hosts)", Name, id))
	}
	out, err := h.Run(ctx, p.argv(), nil)
	if err != nil {
		var ce *remote.ConnError
		if errors.As(err, &ce) {
			return unknown, remote.ProviderError(fmt.Errorf("%s: Read %q: %w", Name, id, err))
		}
		return unknown, provider.NewTransientError(fmt.Errorf("%s: Read %q: %w", Name, id, err))
	}
	state, err := p.parse(out)
	if err != nil {
		return unknown, provider.NewTransientError(fmt.Errorf("%s: Read %q: %w", Name, id, err))
	}
	state[FieldHost] = core.TypedValue{Kind: core.FieldKindString, Value: id}
	return core.Observation{Availability: core.AvailabilityOk, State: state, ObservedAt: time.Now()}, nil
}

// script はホストの上で走る観測のスクリプト。引数は os-release の 2 つの
// パス・ホスト名・boot_id、そのあと目印の名前とパスの組。
const script = `emit() {
  if [ ! -e "$2" ]; then echo "$1 absent"
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

func (p *Provider) argv() []string {
	argv := []string{"sh", "-c", script, "sh", osReleasePath, osReleaseAltPath, hostnamePath, bootIDPath}
	for _, m := range p.markers {
		argv = append(argv, m.Name, m.Path)
	}
	return argv
}

// item はスクリプトの出力の 1 項目。
type item struct {
	status string // ok | absent | unreadable
	data   []byte
}

// parse はスクリプトの出力を State にする。
func (p *Provider) parse(out []byte) (core.State, error) {
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
			return nil, fmt.Errorf("unexpected output line %q", firstChars(line))
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
			return nil, errors.New("the output ended in the middle of " + key)
		}
		data, err := hex.DecodeString(hexText.String())
		if err != nil {
			return nil, fmt.Errorf("%s: cannot decode the output: %v", key, err)
		}
		items[key] = item{status: status, data: data}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !ended {
		return nil, errors.New("the output ended before it was complete")
	}

	str := func(v string) core.TypedValue { return core.TypedValue{Kind: core.FieldKindString, Value: v} }
	state := core.State{}
	need := func(key string) (item, error) {
		it, ok := items[key]
		if !ok {
			return item{}, fmt.Errorf("%s is missing from the output", key)
		}
		if it.status == "unreadable" {
			return item{}, fmt.Errorf("%s cannot be read on the host", key)
		}
		if it.status != "ok" && it.status != "absent" {
			return item{}, fmt.Errorf("%s: unexpected status %q", key, it.status)
		}
		return it, nil
	}

	it, err := need("os-release")
	if err != nil {
		return nil, err
	}
	if it.status == "ok" {
		rel := parseOSRelease(string(it.data))
		for field, key := range map[string]string{FieldOSID: "ID", FieldOSVersionID: "VERSION_ID", FieldOSPrettyName: "PRETTY_NAME"} {
			if v, ok := rel[key]; ok {
				state[field] = str(v)
			}
		}
	}
	for field, key := range map[string]string{FieldHostname: "hostname", FieldBootID: "boot-id"} {
		it, err := need(key)
		if err != nil {
			return nil, err
		}
		if it.status == "ok" {
			state[field] = str(trim(it.data))
		}
	}
	for _, m := range p.markers {
		key := MarkerPrefix + m.Name
		it, err := need(key)
		if err != nil {
			return nil, fmt.Errorf("%s (%s)", err, m.Path)
		}
		if it.status != "ok" {
			continue // 目印のファイルが無い: フィールドも無い
		}
		if len(it.data) > MaxMarkerBytes {
			return nil, fmt.Errorf("%s (%s) is larger than %d bytes; a marker is a short value", key, m.Path, MaxMarkerBytes)
		}
		state[key] = str(trim(it.data))
	}
	return state, nil
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
