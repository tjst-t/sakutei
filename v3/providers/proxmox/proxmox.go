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

// Package proxmox は Proxmox VE の VM と VM の FW の規則を宣言どおりに保つ
// provider（設計 §7.1 の proxmox の vm・fw-rules、E-D1・E-C2・E-B6・N-D1・N-D10・
// N-C12）。v2 の proxmox-vm と proxmox-fw を 1 つのモジュールの種類にした。REST API
// を net/http で叩き、認証は API トークン。コアの外のモジュールで、コアの口
// core.Provider・core.Validator・core.Classifier・core.Invoker だけを持つ。
//
//	providers:                                  # つなぎ方の設定
//	  pve:
//	    module: proxmox
//	    settings:
//	      settle: 30s                           # Apply のあと読み直すまで（既定 30s）
//	      clusters:
//	        lab:
//	          endpoint: https://pve1.example.net:8006
//	          api_base_path: /api2/json         # 省けば /api2/json（"/" は前置き無し）
//	          node: pve1                        # node を宣言しない VM を作るノード
//	          ca_file: /etc/sakutei/pve-ca.pem  # か insecure_skip_verify: true（明示したときだけ）
//	          timeout: 15s                      # 1 つの要求の時間切れ
//	          task_wait: 2m                     # Apply が非同期の仕事（UPID）を待つ長さ
//	          observe: {token_id: "sakutei-ro@pve!audit", token_file: /etc/sakutei/pve-ro.token}
//	          write:   {token_id: "sakutei@pve!sakutei", token_file: /etc/sakutei/pve-rw.token}   # 省けば observe
//
//	ranges:                                     # intent（扱うクラスタは intent に書く）
//	  - provider: pve
//	    args: {cluster: lab}
//	    external_owners:
//	      - {owner: stage0, id: vm/lab/sakutei-ctl, reason: "Sakutei 自身が動く VM"}
//	resources:
//	  - type: pve/vm
//	    id: lab/mon1                            # <クラスタ>/<VM 名>
//	    fields:
//	      vmid: {from_pool: vmids}              # 採番（vmid: 501 と書いてもよい）
//	      template: debian-13-cloud
//	      pool: apps
//	      cores: 2
//	      memory: 4G
//	      disks: "scsi0 storage=local-lvm size=32G, scsi1 storage=local-lvm size=100G keep"
//	      nics: "net0 bridge=vmbr0 vlan=20 firewall=1"
//	      firewall: "enable=1 policy_in=DROP policy_out=ACCEPT"
//	      ci_user: ops
//	      ci_ip: 192.0.2.21/24
//	      power: running
//	  - type: pve/fw-rules
//	    id: lab/mon1
//	    fields: {rules: ["in ACCEPT iface=net0 proto=tcp dport=443 comment=web"]}
//
// 種類（ID の <クラスタ> は settings.clusters の名前）：
//
//   - vm（ID <クラスタ>/<VM 名>）：VM を名前で引く（vmid を ID にしないのは採番で
//     配れるようにするため）。項目と比べ方は vm.go。無ければ template から full
//     clone するか空から作り、pool・CPU・メモリ・ディスク（大きくするだけ）・NIC・
//     VM の FW の options・cloud-init・電源を合わせる（E-D1・N-D1）。ensure: absent
//     で消す（走っていれば止めてからディスクごと）。keep の印のあるディスクが残る
//     VM の削除は断る（*core.RefuseError、E-C2）。既にある volume の付け替えは
//     承認の要る壊す変更（Classifier、E-C2・N-C12・N-D10）。
//   - fw-rules（ID <クラスタ>/<VM 名>）：VM の FW の規則のうち Sakutei が置いた
//     もの（comment の印 "sakutei"）の並び。fw.go（E-B6）。規則の変更は壊す変更
//     （Classifier）。
//   - volume（ID <クラスタ>/<VM 名>/unusedN）：VM の付け先の無い volume。観測だけ
//     で、List が出す（status の宣言外に出る、E-C2・N-D10）。宣言はできない。
//   - fw-rule（ID <クラスタ>/<VM 名>/<位置>）：印の無い規則 1 つ。観測だけで、
//     List が出す（宣言外として見せ、消さない、E-B6）。宣言はできない。
//
// 決まり：
//
//   - 届かない・5xx・時間切れ・認証の失敗・壊れた出力・ノードが落ちて status が
//     unknown は Unknown（無いと読まない、不変条件 3、E-K1）。VM に lock がある・
//     その VM の仕事（UPID）が走っている間は Converging（重ねない、E-D1）。
//   - 書き込み用のトークン（settings.clusters.<名前>.write）は Apply・Delete・
//     Invoke の中でだけ読む（E-G5）。観測と List は observe のトークンだけを使う。
//   - VM と規則の並びは宣言の外で消さない（DestructiveDelete、E-C3）。消すのは
//     ensure: absent の名指しだけ。触らない VM は Range の external_owners に書く
//     （E-D2）。
//   - Range は args {cluster: <名前>}。Range を書いた provider では、どの Range にも
//     入らないクラスタの宣言を断る（E-I6）。
package proxmox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
)

// Version は Schema の版。比べ方・正規形を変えたら上げる（E-J12）。
const Version = "proxmox/1"

// 種類の名前。
const (
	KindVM      = "vm"
	KindFWRules = "fw-rules"
	KindVolume  = "volume"
	KindFWRule  = "fw-rule"
)

// DefaultSettle は Apply のあと読み直すまでの待ちの既定（settings.settle で
// 変える）。電源の操作は Proxmox の仕事が終わってからも VM の status が少しの
// あいだ前の値を返すことがある（E-J6・E-R3）。
const DefaultSettle = 30 * time.Second

// DefaultTaskWait は Apply が非同期の仕事（UPID）の終わりを待つ長さの既定。
// 過ぎれば待つのをやめ、仕事が走る間の観測は Converging（E-D1）。
const DefaultTaskWait = 2 * time.Minute

// vmName は VM の名前（Proxmox の決まり：DNS の名前）。
var vmName = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9.-]{0,61}[a-zA-Z0-9])?$`)

// Provider は proxmox の provider。
type Provider struct {
	clusters map[string]*Cluster
	settle   time.Duration
	poll     time.Duration
}

var (
	_ core.Provider   = (*Provider)(nil)
	_ core.Validator  = (*Provider)(nil)
	_ core.Classifier = (*Provider)(nil)
	_ core.Invoker    = (*Provider)(nil)
)

// Cluster は 1 つのクラスタへのつなぎ方。
type Cluster struct {
	Name string
	// obs は観測用のトークンの Client。
	obs *Client
	// cfg と write は書き込み用の Client を作る材料（Apply・Delete・Invoke の
	// 中でだけ作る、E-G5）。
	cfg   ClientConfig
	write tokenFile
	// Node は node を宣言しない VM を作るノード（空なら node の宣言が要る）。
	Node string
	// TaskWait は Apply が非同期の仕事を待つ長さ。
	TaskWait time.Duration
}

// tokenFile は API トークンの ID と、secret を置いたファイル。secret は要求の
// たびに読む（値を持たない、E-G2）。
type tokenFile struct {
	id, file string
}

func (t tokenFile) token() Token {
	return Token{ID: t.id, Secret: func(context.Context) (string, error) {
		b, err := os.ReadFile(t.file)
		if err != nil {
			return "", fmt.Errorf("トークンのファイル %s を読めない", t.file)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}}
}

// writer は書き込み用のトークンの Client（WriteContext の中でだけ作る、E-G5）。
func (c *Cluster) writer(core.WriteContext) (*Client, error) {
	cfg := c.cfg
	cfg.Token = c.write.token()
	return NewClient(cfg)
}

// Module は settings から provider を作る（load.Module）。
func Module(settings map[string]any) (core.Provider, error) {
	p := &Provider{clusters: map[string]*Cluster{}, settle: DefaultSettle, poll: time.Second}
	for k, v := range settings {
		switch k {
		case "clusters":
			m, ok := v.(map[string]any)
			if !ok || len(m) == 0 {
				return nil, errors.New("settings.clusters にクラスタを 1 つ以上書く（名前 → つなぎ方）")
			}
			for name, cv := range m {
				cm, ok := cv.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("settings.clusters.%s はマップで書く", name)
				}
				c, err := parseCluster(name, cm)
				if err != nil {
					return nil, fmt.Errorf("settings.clusters.%s: %w", name, err)
				}
				p.clusters[name] = c
			}
		case "settle", "poll":
			s, _ := v.(string)
			d, err := time.ParseDuration(s)
			if err != nil || d < 0 || (k == "poll" && d == 0) {
				return nil, fmt.Errorf("settings.%s は長さ（30s など）", k)
			}
			if k == "settle" {
				p.settle = d
			} else {
				p.poll = d
			}
		default:
			return nil, fmt.Errorf("settings に知らない項目 %q（書ける項目: clusters, poll, settle）", k)
		}
	}
	if len(p.clusters) == 0 {
		return nil, errors.New("settings.clusters を書く")
	}
	return p, nil
}

func parseCluster(name string, m map[string]any) (*Cluster, error) {
	if name == "" || strings.ContainsAny(name, "/ :") {
		return nil, errors.New("クラスタの名前に / : 空白は使えない")
	}
	c := &Cluster{Name: name, TaskWait: DefaultTaskWait}
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
	dur := func(k string, zero bool) (time.Duration, bool, error) {
		s, err := str(k)
		if err != nil || s == "" {
			return 0, false, err
		}
		d, err := time.ParseDuration(s)
		if err != nil || d < 0 || (!zero && d == 0) {
			return 0, false, fmt.Errorf("%s %q は正の長さ（15s など）", k, s)
		}
		return d, true, nil
	}
	tok := func(k string) (tokenFile, bool, error) {
		v, ok := m[k]
		if !ok {
			return tokenFile{}, false, nil
		}
		tm, isMap := v.(map[string]any)
		if !isMap {
			return tokenFile{}, false, fmt.Errorf("%s は {token_id, token_file} で書く", k)
		}
		var t tokenFile
		for tk, tv := range tm {
			s, isStr := tv.(string)
			if !isStr || s == "" {
				return tokenFile{}, false, fmt.Errorf("%s.%s は文字列で書く", k, tk)
			}
			switch tk {
			case "token_id":
				t.id = s
			case "token_file":
				t.file = s
			default:
				return tokenFile{}, false, fmt.Errorf("%s に知らない項目 %q（書ける項目: token_file, token_id。secret そのものは書けない）", k, tk)
			}
		}
		if t.id == "" || t.file == "" {
			return tokenFile{}, false, fmt.Errorf("%s に token_id と token_file を書く", k)
		}
		return t, true, nil
	}
	for k := range m {
		switch k {
		case "endpoint", "api_base_path", "node", "ca_file", "insecure_skip_verify", "timeout", "task_wait", "observe", "write":
		default:
			return nil, fmt.Errorf("知らない項目 %q（書ける項目: api_base_path, ca_file, endpoint, insecure_skip_verify, node, observe, task_wait, timeout, write）", k)
		}
	}
	var err error
	if c.cfg.Endpoint, err = str("endpoint"); err != nil {
		return nil, err
	}
	if c.cfg.Endpoint == "" {
		return nil, errors.New("endpoint（https://host:8006）を書く")
	}
	if c.cfg.APIBasePath, err = str("api_base_path"); err != nil {
		return nil, err
	}
	if c.Node, err = str("node"); err != nil {
		return nil, err
	}
	if c.cfg.CAFile, err = str("ca_file"); err != nil {
		return nil, err
	}
	if v, ok := m["insecure_skip_verify"]; ok {
		b, isBool := v.(bool)
		if !isBool {
			return nil, errors.New("insecure_skip_verify は true か false")
		}
		c.cfg.InsecureSkipVerify = b
	}
	if d, ok, err := dur("timeout", false); err != nil {
		return nil, err
	} else if ok {
		c.cfg.Timeout = d
	}
	if d, ok, err := dur("task_wait", true); err != nil {
		return nil, err
	} else if ok {
		c.TaskWait = d
	}
	obs, ok, err := tok("observe")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("observe（観測用のトークン {token_id, token_file}）を書く")
	}
	c.write = obs
	if w, ok, err := tok("write"); err != nil {
		return nil, err
	} else if ok {
		c.write = w
	}
	cfg := c.cfg
	cfg.Token = obs.token()
	if c.obs, err = NewClient(cfg); err != nil {
		return nil, err
	}
	// 書き込み用の Client の設定も今確かめる（トークンのファイルは読まない）。
	cfg.Token = c.write.token()
	if _, err := NewClient(cfg); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}
	return c, nil
}

// split は ID の名前をクラスタ・VM 名・残り（volume の unusedN・規則の位置）に分ける。
func split(id core.ID) (cluster, vm, rest string) {
	parts := strings.SplitN(id.Name, "/", 3)
	switch len(parts) {
	case 3:
		return parts[0], parts[1], parts[2]
	case 2:
		return parts[0], parts[1], ""
	}
	return id.Name, "", ""
}

// coords は ID のクラスタと VM 名を引く。
func (p *Provider) coords(id core.ID) (*Cluster, string, string, error) {
	name, vm, rest := split(id)
	c, ok := p.clusters[name]
	if !ok {
		return nil, "", "", fmt.Errorf("クラスタ %q が settings.clusters に無い（ある: %s）", name, strings.Join(p.clusterNames(), ", "))
	}
	if !vmName.MatchString(vm) {
		return nil, "", "", fmt.Errorf("%q は VM の名前でない（英数字・'-'・'.'、英数字で始まり英数字で終わる）", vm)
	}
	return c, vm, rest, nil
}

func (p *Provider) clusterNames() []string {
	names := make([]string, 0, len(p.clusters))
	for n := range p.clusters {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Schema は 4 つの種類。観測だけの項目も Condition から読めるよう Schema に置く
// （宣言は Validate が断る）。
func (p *Provider) Schema() core.Schema {
	str := core.FieldSpec{Type: core.String}
	num := core.FieldSpec{Type: core.Int}
	return core.Schema{Version: Version, Kinds: map[string]core.KindSchema{
		KindVM:      {Settle: p.settle, DestructiveDelete: true, Fields: vmFields()},
		KindFWRules: {DestructiveDelete: true, Fields: fwFields()},
		KindVolume: {ObserveOnly: true, Fields: map[string]core.FieldSpec{
			FieldCluster: str, FieldName: str, FieldVMID: num, FieldNode: str, FieldVolume: str,
		}},
		KindFWRule: {ObserveOnly: true, Fields: map[string]core.FieldSpec{
			FieldCluster: str, FieldName: str, FieldVMID: num, FieldNode: str, FieldRule: str, FieldPos: num,
		}},
	}}
}

// Validate は宣言を確かめる（現実に触れない、E-I7・E-M3・E-I6）。範囲は
// args {cluster: <名前>} だけ。
func (p *Provider) Validate(res []core.Resource, ranges []core.Range) []core.Diagnostic {
	var diags []core.Diagnostic
	rej := func(o core.Origin, target, format string, a ...any) {
		diags = append(diags, core.Diagnostic{Severity: core.Reject, Origin: o, Target: target, Message: fmt.Sprintf(format, a...)})
	}
	ranged := map[string]bool{}
	for _, rg := range ranges {
		name, ok := rg.Args["cluster"]
		switch {
		case len(rg.Args) != 1 || !ok:
			rej(rg.Origin, "ranges", "proxmox の範囲は args {cluster: <名前>} だけ")
			continue
		case p.clusters[name] == nil:
			rej(rg.Origin, "ranges", "クラスタ %q が settings.clusters に無い（ある: %s）", name, strings.Join(p.clusterNames(), ", "))
			continue
		}
		ranged[name] = true
	}
	for _, r := range res {
		target := r.ID.String()
		c, _, rest, err := p.coords(r.ID)
		if err != nil {
			rej(r.Origin, target, "%v", err)
			continue
		}
		if len(ranged) > 0 && !ranged[c.Name] {
			rej(r.Origin, target, "クラスタ %s がどの範囲（ranges の args.cluster）にも入らない", c.Name)
		}
		switch r.ID.Kind() {
		case KindVolume, KindFWRule:
			rej(r.Origin, target, "種類 %s は宣言の外のものを見せるためだけで、宣言できない（volume は vm の disks の volume= で付け、手の規則は fw-rules の rules に書いてから手で消す）", r.ID.Kind())
			continue
		}
		if rest != "" {
			rej(r.Origin, target, "%s の id は <クラスタ>/<VM 名>", r.ID.Kind())
			continue
		}
		if r.Ensure == core.EnsureAbsent {
			continue
		}
		var msgs []string
		switch r.ID.Kind() {
		case KindVM:
			msgs = validateVM(c, r.ID, r.Fields)
		case KindFWRules:
			msgs = validateFW(r.Fields)
		}
		for _, m := range msgs {
			rej(r.Origin, target, "%s", m)
		}
	}
	return diags
}

// Observe は ID をクラスタごとにまとめ、/cluster/resources を 1 回だけ読む
// （E-D8）。各 VM の観測は /status/current・/pending・FW を読む。
func (p *Provider) Observe(ctx context.Context, ids []core.ID) (map[core.ID]core.Observation, error) {
	out := map[core.ID]core.Observation{}
	groups := map[string][]core.ID{}
	for _, id := range ids {
		c, _, _, err := p.coords(id)
		if err != nil {
			out[id] = core.Observation{Availability: core.Unknown, Note: err.Error()}
			continue
		}
		groups[c.Name] = append(groups[c.Name], id)
	}
	for name, group := range groups {
		c := p.clusters[name]
		res, err := c.resources(ctx, c.obs)
		if err != nil {
			for _, id := range group {
				out[id] = unknown(err)
			}
			continue
		}
		for _, id := range group {
			out[id] = p.observe(ctx, c, c.obs, id, res)
		}
	}
	return out, nil
}

// observe は 1 つの ID を観測する。
func (p *Provider) observe(ctx context.Context, c *Cluster, cl *Client, id core.ID, res []Resource) core.Observation {
	_, vm, rest, _ := p.coords(id)
	r, o, ok := lookupVM(ctx, c, cl, vm, res)
	if !ok {
		return o
	}
	switch id.Kind() {
	case KindVM:
		return observeVM(ctx, c, cl, r)
	case KindFWRules, KindFWRule:
		return observeFW(ctx, c, cl, r, id.Kind(), rest)
	case KindVolume:
		return observeVolume(ctx, c, cl, r, rest)
	}
	return core.Observation{Availability: core.Unknown, Note: fmt.Sprintf("種類 %q を知らない", id.Kind())}
}

// lookupVM は res から名前が vm の VM を引き、電源と lock を読む。ok が false なら
// 観測は o で決まる（無い・分からない・作業中）。
func lookupVM(ctx context.Context, c *Cluster, cl *Client, vm string, res []Resource) (*Resource, core.Observation, bool) {
	found := findVM(res, vm)
	switch {
	case len(found) == 0:
		return nil, core.Observation{Availability: core.Absent}, false
	case len(found) > 1:
		ids := make([]string, len(found))
		for i, r := range found {
			ids[i] = fmt.Sprint(r.VMID)
		}
		return nil, core.Observation{Availability: core.Unknown, Note: fmt.Sprintf("名前 %s の VM が %d 台ある（vmid %s）。1 台を残して名前を変える", vm, len(found), strings.Join(ids, ", "))}, false
	}
	r := found[0]
	if r.Template {
		return nil, core.Observation{Availability: core.Unknown, Note: fmt.Sprintf("VM %d はテンプレートで、VM でない", r.VMID)}, false
	}
	if err := c.powerOf(ctx, cl, &r); err != nil {
		return nil, unknown(err), false
	}
	if r.Status != PowerRunning && r.Status != PowerStopped {
		return nil, core.Observation{Availability: core.Unknown, Note: fmt.Sprintf("ノード %s の VM %d の status が %q", r.Node, r.VMID, r.Status)}, false
	}
	if r.Lock != "" {
		return nil, core.Observation{Availability: core.Converging, Note: fmt.Sprintf("VM %d に lock %s がある", r.VMID, r.Lock)}, false
	}
	busy, err := cl.ActiveTasks(ctx, r.Node, r.VMID)
	if err != nil {
		return nil, unknown(err), false
	}
	if len(busy) > 0 {
		return nil, core.Observation{Availability: core.Converging, Note: fmt.Sprintf("VM %d の仕事 %s が走っている", r.VMID, strings.Join(busy, ", "))}, false
	}
	return &r, core.Observation{}, true
}

// List は範囲のクラスタの VM（テンプレートを除く）と、その付け先の無い volume・
// Sakutei の規則の並び・印の無い規則を返す（E-C2・E-B6・E-D4）。一覧と VM の
// 設定・規則が全部読め、どの VM も名前で一意に引けるときだけ完全（E-K2）。
func (p *Provider) List(ctx context.Context, rg core.Range) (core.Listing, error) {
	name := rg.Args["cluster"]
	c, ok := p.clusters[name]
	if !ok {
		return core.Listing{}, fmt.Errorf("クラスタ %q が settings.clusters に無い", name)
	}
	res, err := c.resources(ctx, c.obs)
	if err != nil {
		return core.Listing{}, err
	}
	vms, err := managedVMs(res)
	if err != nil {
		return core.Listing{}, err
	}
	var ids []core.ID
	typ := func(kind string) string { return rg.Provider + "/" + kind }
	for i := range vms {
		r := &vms[i]
		base := name + "/" + r.Name
		ids = append(ids, core.ID{Type: typ(KindVM), Name: base})
		cfg, err := c.obs.Config(ctx, r.Node, r.VMID)
		if err != nil {
			return core.Listing{}, fmt.Errorf("VM %d: %w", r.VMID, err)
		}
		for _, k := range sortedSlots(unusedVolumes(cfg)) {
			ids = append(ids, core.ID{Type: typ(KindVolume), Name: base + "/" + k})
		}
		rules, err := readRules(ctx, c.obs, r)
		if err != nil {
			return core.Listing{}, fmt.Errorf("VM %d の FW の規則: %w", r.VMID, err)
		}
		marked := false
		for _, x := range rules {
			if x.marked {
				marked = true
				continue
			}
			ids = append(ids, core.ID{Type: typ(KindFWRule), Name: fmt.Sprintf("%s/%d", base, x.pos)})
		}
		if marked {
			ids = append(ids, core.ID{Type: typ(KindFWRules), Name: base})
		}
	}
	return core.Listing{IDs: ids, Complete: true}, nil
}

// managedVMs は res の中の扱う VM（qemu でテンプレートでないもの）を返す。名前の
// 無い・使えない・重なる VM があれば、List は完全にならない。
func managedVMs(res []Resource) ([]Resource, error) {
	var out []Resource
	seen := map[string]int{}
	var bad []string
	for _, r := range res {
		if r.Type != "qemu" || r.Template {
			continue
		}
		if !vmName.MatchString(r.Name) {
			bad = append(bad, fmt.Sprintf("VM %d に使える名前が無い（%q）", r.VMID, r.Name))
			continue
		}
		if other, dup := seen[r.Name]; dup {
			bad = append(bad, fmt.Sprintf("VM %d と %d が同じ名前 %s", other, r.VMID, r.Name))
			continue
		}
		seen[r.Name] = r.VMID
		out = append(out, r)
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("VM を名前で見分けられないので一覧は完全でない: %s（名前を一意にする）", strings.Join(bad, "; "))
	}
	return out, nil
}

// Apply は種類ごとに現実を読み直し、宣言とずれているものだけを変える（冪等）。
func (p *Provider) Apply(ctx context.Context, w core.WriteContext, id core.ID, desired core.Fields, _ core.Observation) error {
	c, vm, rest, err := p.coords(id)
	if err != nil {
		return err
	}
	if rest != "" {
		return fmt.Errorf("%s は宣言の外のものを見せるための ID で、変えない", id)
	}
	cl, err := c.writer(w)
	if err != nil {
		return err
	}
	switch id.Kind() {
	case KindVM:
		return p.applyVM(ctx, c, cl, vm, desired)
	case KindFWRules:
		return applyFW(ctx, c, cl, vm, desired)
	}
	return fmt.Errorf("種類 %q には Apply が無い（観測だけ）", id.Kind())
}

// Delete は VM を消す・Sakutei の規則を外す。宣言の外のものを見せる種類は断る。
func (p *Provider) Delete(ctx context.Context, w core.WriteContext, id core.ID, _ core.Observation) error {
	c, vm, rest, err := p.coords(id)
	if err != nil {
		return err
	}
	switch id.Kind() {
	case KindVolume, KindFWRule:
		return &core.RefuseError{Reason: "付け先の無い volume と手の規則は Sakutei が消さない（Proxmox で手で消す）"}
	}
	if rest != "" {
		return fmt.Errorf("%s の id は <クラスタ>/<VM 名>", id.Kind())
	}
	// keep の volume が残る VM は消さない（観測用のトークンで先に確かめ、何も
	// 書かずに断る、E-C2）。
	if id.Kind() == KindVM {
		if err := p.keepGuard(ctx, c, c.obs, vm); err != nil {
			return err
		}
	}
	cl, err := c.writer(w)
	if err != nil {
		return err
	}
	if id.Kind() == KindFWRules {
		return applyFW(ctx, c, cl, vm, nil)
	}
	return p.destroy(ctx, c, cl, vm)
}

// unknown は Client の誤りの観測（届かない・認証の失敗・壊れた出力、E-K1）。
func unknown(err error) core.Observation {
	return core.Observation{Availability: core.Unknown, Note: err.Error()}
}

// resources はクラスタの VM の一覧を返す。/cluster/resources の名前は pvestatd の
// 写しで、作った直後の VM は名前が空のことがある（「無い」と見てもう一度作らない
// ように）。名前が空の qemu の項目は、VM の設定で名前とテンプレートかを確かめる。
func (c *Cluster) resources(ctx context.Context, cl *Client) ([]Resource, error) {
	res, err := cl.Resources(ctx)
	if err != nil {
		return nil, err
	}
	for i := range res {
		r := &res[i]
		if r.Type != "qemu" || r.Name != "" {
			continue
		}
		cfg, err := cl.Config(ctx, r.Node, r.VMID)
		if err != nil {
			return nil, fmt.Errorf("名前の無い VM %d の設定を読めない: %w", r.VMID, err)
		}
		r.Name, r.Template = cfg["name"], cfg["template"] == "1"
	}
	return res, nil
}

// powerOf は VM の今の電源と lock を /status/current で読む（/cluster/resources の
// status は古いことがある）。
func (c *Cluster) powerOf(ctx context.Context, cl *Client, r *Resource) error {
	st, err := cl.CurrentStatus(ctx, r.Node, r.VMID)
	if err != nil {
		return err
	}
	r.Status = st.Status
	if st.Lock != "" {
		r.Lock = st.Lock
	}
	return nil
}

// findVM は res から名前が vm の qemu の VM を探す。
func findVM(res []Resource, vm string) []Resource {
	var out []Resource
	for _, r := range res {
		if r.Type == "qemu" && r.Name == vm {
			out = append(out, r)
		}
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
