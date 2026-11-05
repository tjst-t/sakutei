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

// Package redfish は物理サーバの BMC を Redfish で宣言どおりに保つ provider
// （設計 §7.1、E-A1〜E-A5・N-A1〜N-A4）。v2 の redfish-system・
// redfish-virtual-media・redfish-bios・redfish-firmware を 1 つのモジュールの
// 4 つの type にした。コアの外のモジュールで、コアの interface core.Provider・
// core.Validator・core.Invoker だけを実装する。
//
//	providers:                                # 接続設定
//	  bmc:
//	    module: redfish
//	    settings:
//	      settle: 15s                         # 電源・起動先の Apply のあと読み直すまで（既定 15s）
//	      nodes:
//	        node1:
//	          endpoint: https://192.0.2.11     # https だけ
//	          system: /redfish/v1/Systems/1     # 省けば Systems の最初のメンバー
//	          manager: /redfish/v1/Managers/1   # 省けば Managers の最初のメンバー
//	          ca_file: /etc/sakutei/bmc-ca.pem  # か insecure_skip_verify: true（明示したときだけ）
//	          timeout: 10s
//	          observe: {username: monitor, password_file: /etc/sakutei/bmc-monitor}
//	          write: {username: admin, password_file: /etc/sakutei/bmc-admin}   # 省けば observe を使う
//
//	resources:                                # intent
//	  - {type: bmc/system, id: node1, fields: {power: "On", boot_enabled: Continuous, boot_target: Pxe}}
//	  - {type: bmc/virtual-media, id: node1/cd, fields: {image: "http://192.0.2.5/os.iso"}}
//	  - {type: bmc/bios, id: node1/ProcVirtualization, fields: {value: Enabled}}
//	  - {type: bmc/firmware, id: node1/BMC, fields: {version: "2.0.0"}}
//
// type（ID の <node> は settings.nodes の名前）：
//
//   - system（ID <node>）：電源 power（On・Off）と起動先の上書き boot_enabled
//     （Continuous・Disabled）・boot_target。Off へは GracefulShutdown だけを送る
//     （止まらなければ ineffective Apply になり、その回数の上限で halt する、E-J6）。
//     強制停止は破壊的な操作 force_off（Invoker、Procedure から。E-A1・N-A1）。電源の切り替えの
//     途中は Converging（E-A1）。再起動と 1 回だけの起動先は宣言できない（観測に
//     残らない一回きりの操作、Invoker の reset・boot_once、E-A2・N-A2）。
//   - virtual-media（ID <node>/cd）：CD の image（"" は取り出した状態、E-A3）。
//     Inserted が false なら image は "" として比べる（BMC が取得に失敗して
//     戻したとき、次の cycle で入れ直す）。
//   - bios（ID <node>/<属性>）：BIOS の属性 1 つの value（文字列で書く。数・真偽の
//     属性は "3"・"true"）。Apply は Bios/Settings に PATCH するだけで再起動しない。
//     保留中（Settings の値が今の値と違う・既定値に戻す保留）のあいだは
//     Converging（E-A4・N-A3）。属性の registry（読み取り専用・選べる値・範囲）は
//     Apply の前に BMC から読んで確かめ、合わなければ書かずにエラーにする。
//   - firmware（ID <node>/<部品の Id>）：観測だけ（E-A5・N-A4）。FirmwareInventory
//     の version と、その部品の最新の更新の Task（update_state・update_image・
//     update_percent・update_message）。更新は Procedure が Invoker の verify・
//     stage・reset で進め、Task とバージョンを観測で待つ（§3.7）。Payload を返さない BMC
//     （qemu-bmc 0.11.0）の Task は、Update の message registry の TargetDetermined
//     の MessageArgs で部品とイメージを見分ける。
//
// 決まり：
//
//   - BMC に届かない・5xx・タイムアウト・認証の失敗・壊れた出力・未知の
//     PowerState は Unknown（Absent とは読まない、不変条件 3、E-K1）。BMC が Bios・
//     UpdateService を持たない・部品が無いのは Absent。
//   - 書き込み用の credential（settings.nodes.<node>.write）は Apply・Invoke の
//     中でだけ読む（E-G5・N-G3）。観測は observe の credential だけを使う。
//   - 装置は作りも消しもしない：List を持たず（Range を書けば読み込み時にエラーにする）、
//     Delete は拒否する（*core.RefuseError）。
package redfish

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/providers/redfish/client"
)

// Version は Schema のバージョン。比べ方・正規形を変えたら上げる（E-J12）。
const Version = "redfish/1"

// type の名前。
const (
	KindSystem   = "system"
	KindMedia    = "virtual-media"
	KindBios     = "bios"
	KindFirmware = "firmware"
)

// DefaultSystemSettle は電源・起動先の Apply のあと読み直すまでの待ち時間の既定値
// （settings.settle で変える）。BMC は Reset に 204 を返してから PowerState を
// 変える（E-J6・E-R3）。
const DefaultSystemSettle = 15 * time.Second

// Provider は redfish の provider。
type Provider struct {
	nodes   map[string]*node
	tempDir string
	settle  time.Duration
}

var (
	_ core.Provider  = (*Provider)(nil)
	_ core.Validator = (*Provider)(nil)
	_ core.Invoker   = (*Provider)(nil)
)

// node は 1 台の BMC への接続設定。
type node struct {
	name   string
	cfg    client.Config // credential は除く
	obs    client.Client // 観測用の credential の Client
	write  credFile      // 書き込み用の credential（Apply・Invoke の中でだけ読む）
	cfgErr error
}

// credFile はユーザー名とパスワードのファイル。パスワードは要求のたびに読む（値を
// 持たない、E-G2）。
type credFile struct {
	username, passwordFile string
}

func (c credFile) credential() client.Credential {
	return client.Credential{Username: c.username, PasswordFunc: func(context.Context) (string, error) {
		b, err := os.ReadFile(c.passwordFile)
		if err != nil {
			return "", fmt.Errorf("パスワードのファイル %s を読めない", c.passwordFile)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}}
}

// writer は書き込み用の credential の Client（WriteContext の中でだけ作る、E-G5）。
func (n *node) writer(core.WriteContext) (client.Client, error) {
	cfg := n.cfg
	cfg.Credential = n.write.credential()
	return client.New(cfg)
}

// Module は settings から provider を作る（load.Module）。
func Module(settings map[string]any) (core.Provider, error) {
	p := &Provider{nodes: map[string]*node{}, settle: DefaultSystemSettle}
	for k, v := range settings {
		switch k {
		case "nodes":
			m, ok := v.(map[string]any)
			if !ok || len(m) == 0 {
				return nil, errors.New("settings.nodes に BMC を 1 台以上書く（ノード名 → 接続設定）")
			}
			for name, nv := range m {
				nm, ok := nv.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("settings.nodes.%s はマップで書く", name)
				}
				n, err := parseNode(name, nm)
				if err != nil {
					return nil, fmt.Errorf("settings.nodes.%s: %w", name, err)
				}
				p.nodes[name] = n
			}
		case "temp_dir":
			s, ok := v.(string)
			if !ok || s == "" {
				return nil, errors.New("settings.temp_dir にディレクトリを書く")
			}
			p.tempDir = s
		case "settle":
			s, _ := v.(string)
			d, err := time.ParseDuration(s)
			if err != nil || d < 0 {
				return nil, errors.New("settings.settle は長さ（15s など）")
			}
			p.settle = d
		default:
			return nil, fmt.Errorf("settings に未知の field %q（書ける field: nodes, settle, temp_dir）", k)
		}
	}
	if len(p.nodes) == 0 {
		return nil, errors.New("settings.nodes を書く")
	}
	return p, nil
}

func parseNode(name string, m map[string]any) (*node, error) {
	if name == "" || strings.ContainsAny(name, "/ ") {
		return nil, errors.New("ノード名に / と空白は使えない")
	}
	n := &node{name: name}
	str := func(k string) (string, error) {
		v, ok := m[k]
		if !ok {
			return "", nil
		}
		s, isStr := v.(string)
		if !isStr || s == "" {
			return "", fmt.Errorf("%s は文字列で書く", k)
		}
		return s, nil
	}
	cred := func(k string) (credFile, bool, error) {
		v, ok := m[k]
		if !ok {
			return credFile{}, false, nil
		}
		cm, isMap := v.(map[string]any)
		if !isMap {
			return credFile{}, false, fmt.Errorf("%s は {username, password_file} で書く", k)
		}
		var c credFile
		for ck, cv := range cm {
			s, isStr := cv.(string)
			if !isStr || s == "" {
				return credFile{}, false, fmt.Errorf("%s.%s は文字列で書く", k, ck)
			}
			switch ck {
			case "username":
				c.username = s
			case "password_file":
				c.passwordFile = s
			default:
				return credFile{}, false, fmt.Errorf("%s に未知の field %q（書ける field: password_file, username。パスワードそのものは書けない）", k, ck)
			}
		}
		if c.username == "" || c.passwordFile == "" {
			return credFile{}, false, fmt.Errorf("%s に username と password_file を書く", k)
		}
		return c, true, nil
	}
	var err error
	for k := range m {
		switch k {
		case "endpoint", "system", "manager", "ca_file", "insecure_skip_verify", "timeout", "observe", "write":
		default:
			return nil, fmt.Errorf("未知の field %q（書ける field: ca_file, endpoint, insecure_skip_verify, manager, observe, system, timeout, write）", k)
		}
	}
	if n.cfg.Endpoint, err = str("endpoint"); err != nil {
		return nil, err
	}
	if n.cfg.Endpoint == "" {
		return nil, errors.New("endpoint（https://…）を書く")
	}
	if n.cfg.SystemPath, err = str("system"); err != nil {
		return nil, err
	}
	if n.cfg.ManagerPath, err = str("manager"); err != nil {
		return nil, err
	}
	if n.cfg.CAFile, err = str("ca_file"); err != nil {
		return nil, err
	}
	if v, ok := m["insecure_skip_verify"]; ok {
		b, isBool := v.(bool)
		if !isBool {
			return nil, errors.New("insecure_skip_verify は true か false")
		}
		n.cfg.InsecureSkipVerify = b
	}
	if n.cfg.InsecureSkipVerify && n.cfg.CAFile != "" {
		return nil, errors.New("ca_file と insecure_skip_verify は一緒に書けない")
	}
	if t, err := str("timeout"); err != nil {
		return nil, err
	} else if t != "" {
		d, err := time.ParseDuration(t)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("timeout %q は正の長さ（10s など）", t)
		}
		n.cfg.Timeout = d
	}
	obs, ok, err := cred("observe")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("observe（観測用の credential {username, password_file}）を書く")
	}
	n.write = obs
	if w, ok, err := cred("write"); err != nil {
		return nil, err
	} else if ok {
		n.write = w
	}
	cfg := n.cfg
	cfg.Credential = obs.credential()
	c, err := client.New(cfg)
	if err != nil {
		return nil, err
	}
	n.obs = c
	return n, nil
}

// Schema は 4 つの type。観測だけの field も Condition から読めるよう Schema に置く
// （宣言は Validate がエラーにする）。
func (p *Provider) Schema() core.Schema {
	str := core.FieldSpec{Type: core.String}
	list := core.FieldSpec{Type: core.List}
	return core.Schema{Version: Version, Kinds: map[string]core.KindSchema{
		KindSystem: {Settle: p.settle, Fields: map[string]core.FieldSpec{
			FieldPower: str, FieldBootEnabled: str, FieldBootTarget: str,
			FieldPowerState: str, FieldUUID: str, FieldMACs: list, FieldCPUCount: {Type: core.Int},
			FieldMemoryGiB: {Type: core.Float}, FieldBootMode: str, FieldBootTargets: list, FieldResetTypes: list,
		}},
		KindMedia: {Fields: map[string]core.FieldSpec{
			FieldImage: str, FieldInserted: {Type: core.Bool}, FieldMediaImage: str,
		}},
		KindBios: {Fields: map[string]core.FieldSpec{
			FieldValue: str, FieldPending: str,
		}},
		KindFirmware: {ObserveOnly: true, Fields: map[string]core.FieldSpec{
			FieldVersion: str, FieldUpdateable: {Type: core.Bool}, FieldSoftwareID: str,
			FieldUpdateState: str, FieldUpdateImage: str, FieldUpdatePercent: {Type: core.Int}, FieldUpdateMessage: str,
		}},
	}}
}

// declarable は宣言できる field（ほかは観測だけ）。
var declarable = map[string][]string{
	KindSystem:   {FieldPower, FieldBootEnabled, FieldBootTarget},
	KindMedia:    {FieldImage},
	KindBios:     {FieldValue},
	KindFirmware: {FieldVersion},
}

// split は ID の名前をノードと残り（system は空）に分ける。
func split(id core.ID) (nodeName, rest string) {
	nodeName, rest, _ = strings.Cut(id.Name, "/")
	return nodeName, rest
}

// Validate は宣言を確かめる（E-I7・E-M3）。Range は書けない（List を持たない）。
func (p *Provider) Validate(res []core.Resource, ranges []core.Range) []core.Diagnostic {
	var diags []core.Diagnostic
	rej := func(o core.Origin, target, format string, a ...any) {
		diags = append(diags, core.Diagnostic{Severity: core.Reject, Origin: o, Target: target, Message: fmt.Sprintf(format, a...)})
	}
	for _, rg := range ranges {
		rej(rg.Origin, "ranges", "redfish は Range を持たない（BMC の装置は作りも消しもしないので List と prune が無い）")
	}
	for _, r := range res {
		target := r.ID.String()
		nodeName, rest := split(r.ID)
		if _, ok := p.nodes[nodeName]; !ok {
			rej(r.Origin, target, "ノード %q の BMC が settings.nodes に無い（ある: %s）", nodeName, strings.Join(p.nodeNames(), ", "))
			continue
		}
		kind := r.ID.Kind()
		switch kind {
		case KindSystem:
			if rest != "" {
				rej(r.Origin, target, "system の id はノード名（例 node1）")
				continue
			}
		case KindMedia:
			if rest != "cd" {
				rej(r.Origin, target, "virtual-media の id は <node>/cd（例 node1/cd）")
				continue
			}
		case KindBios, KindFirmware:
			if rest == "" || strings.Contains(rest, "/") {
				rej(r.Origin, target, "%s の id は <node>/<名前>（例 node1/%s）", kind, map[string]string{KindBios: "ProcVirtualization", KindFirmware: "BMC"}[kind])
				continue
			}
		}
		for _, k := range sortedKeys(r.Fields) {
			if !has(declarable[kind], k) {
				rej(r.Origin, target, "field %q は観測だけで宣言できない（宣言できる field: %s）", k, strings.Join(declarable[kind], ", "))
			}
		}
		if r.Ensure == core.EnsureAbsent {
			rej(r.Origin, target, "BMC の装置は消せない（ensure: absent は書けない）")
		}
		switch kind {
		case KindSystem:
			for _, msg := range validateSystem(r.Fields) {
				rej(r.Origin, target, "%s", msg)
			}
		case KindMedia:
			if v, ok := r.Fields[FieldImage].(string); ok && v != "" && !imageForm.MatchString(v) {
				rej(r.Origin, target, "image %q は URL（https://host/x.iso など）か BMC から見える絶対パス。空は取り出した状態", v)
			}
		}
	}
	return diags
}

func (p *Provider) nodeNames() []string { return sortedKeys(p.nodes) }

// Observe は ID をノードと type ごとにまとめて、BMC を type ごとに 1 回読む（E-D8）。
func (p *Provider) Observe(ctx context.Context, ids []core.ID) (map[core.ID]core.Observation, error) {
	out := map[core.ID]core.Observation{}
	type key struct{ node, kind string }
	groups := map[key][]core.ID{}
	for _, id := range ids {
		nodeName, _ := split(id)
		if _, ok := p.nodes[nodeName]; !ok {
			out[id] = core.Observation{Availability: core.Unknown, Note: fmt.Sprintf("ノード %q の BMC が settings.nodes に無い", nodeName)}
			continue
		}
		k := key{nodeName, id.Kind()}
		groups[k] = append(groups[k], id)
	}
	for k, group := range groups {
		n := p.nodes[k.node]
		var obs map[core.ID]core.Observation
		switch k.kind {
		case KindSystem:
			obs = observeSystem(ctx, n.obs, group)
		case KindMedia:
			obs = observeMedia(ctx, n.obs, group)
		case KindBios:
			obs = observeBios(ctx, n.obs, group)
		case KindFirmware:
			obs = observeFirmware(ctx, n.obs, group)
		default:
			obs = map[core.ID]core.Observation{}
			for _, id := range group {
				obs[id] = core.Observation{Availability: core.Unknown, Note: fmt.Sprintf("未知の type %q", k.kind)}
			}
		}
		for id, o := range obs {
			out[id] = o
		}
	}
	return out, nil
}

// List は持たない（BMC の装置は作りも消しもしない）。
func (p *Provider) List(context.Context, core.Range) (core.Listing, error) {
	return core.Listing{}, errors.New("redfish は List を持たない")
}

// Apply は type ごとに BMC を読み直し、宣言とずれているものだけを変える（冪等）。
func (p *Provider) Apply(ctx context.Context, w core.WriteContext, id core.ID, desired core.Fields, _ core.Observation) error {
	nodeName, rest := split(id)
	n, ok := p.nodes[nodeName]
	if !ok {
		return fmt.Errorf("ノード %q の BMC が settings.nodes に無い", nodeName)
	}
	c, err := n.writer(w)
	if err != nil {
		return err
	}
	switch id.Kind() {
	case KindSystem:
		return applySystem(ctx, c, desired)
	case KindMedia:
		return applyMedia(ctx, c, desired)
	case KindBios:
		return applyBios(ctx, c, rest, desired)
	}
	return fmt.Errorf("type %q には Apply が無い（観測だけ）", id.Kind())
}

// Delete は拒否する（BMC の装置は消さない、E-F1）。
func (p *Provider) Delete(context.Context, core.WriteContext, core.ID, core.Observation) error {
	return &core.RefuseError{Reason: "BMC の装置（電源・仮想メディア・BIOS の属性・FW）は消せない"}
}

// unknown は Client のエラーを観測にする（届かない・認証の失敗・壊れた出力、E-K1）。
func unknown(err error) core.Observation {
	return core.Observation{Availability: core.Unknown, Note: err.Error()}
}

func fill(ids []core.ID, o core.Observation) map[core.ID]core.Observation {
	out := make(map[core.ID]core.Observation, len(ids))
	for _, id := range ids {
		out[id] = o
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func has(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func strList(xs []string) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}
