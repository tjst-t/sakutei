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

// Package proxmox は Proxmox VE の VM を収束させる provider（proxmox-vm、
// Backlog #143、設計書 §29.1「Proxmox VE の VM provider」）。REST API
// （/api2/json）を net/http で叩き、認証は API トークン（client.go）。
//
// 完全の階級：Read・Apply・List・BulkObserve（/cluster/resources を 1 回と、
// VM ごとの設定。Read と等価）。VM は破壊的な型なので Pruneable:false
// （§12.3、ADR-0005）。消すのは intent の "ensure: absent" の明示だけで
// （§12.2、Backlog #163）、Delete は破壊的として予算（§13）に数え、planned_change
// （kind: delete）のレーンが開いているときだけ走る（承認、§13.3・§14.4）。走って
// いる VM は止めて（stop。ディスクごと消すので正常停止は待たない）から消す。
//
// ID は "<クラスタ>/<VM 名>"（クラスタは設定ファイルの名前付きの一覧、
// config.go）。VM は名前で引く（vmid は ID にしない：採番から配れるように）。
//
// 宣言するフィールド（宣言したものだけを管理し、書かないものは触らない）:
//
//   - vmid: VM の番号。作るときに使う（宣言するか、採番 from_pool から配る）。
//     今ある VM と違えば計画のエラー（番号は変えない）。
//   - node: ノード。省略すればクラスタの既定のノードに作る。今ある VM と違えば
//     計画のエラー（移行は扱わない）。
//   - template: 複製する元のテンプレート（名前か vmid）。作るときだけ使い、
//     観測しない（差分から落とす）。full clone。
//   - pool: Proxmox の pool（Backlog #165）。作る・複製するときに渡し（API
//     トークンを pool の ACL だけで絞れるように）、観測して違えば移す（PUT
//     /pools、allow-move。"" は pool から外す）。pool は作らない（無ければ Apply
//     のエラー）。pool は /cluster/resources で読み、トークンにその pool の
//     Pool.Audit が無ければ「pool に無い」に見える。
//   - cores・memory（MiB の整数か "2G" など）。
//   - disks: "<slot> storage=<storage> size=<大きさ>" を "," でつなぐ。宣言した
//     slot だけを比べる。大きくするのはよい（resize）、小さくするのと
//     ストレージを変えるのは計画のエラー。無い slot は足す。宣言しない
//     ディスクは消さない。
//   - nics: "<slot> bridge=<bridge> [vlan=<tag>] [mac=<mac>] [model=<model>]"。
//     宣言した slot だけを比べる。bridge と vlan（書かなければタグ無し）は
//     いつも、mac と model は書いたときだけ比べる。
//   - ci_user・ci_ssh_keys（改行で区切る）・ci_ip（dhcp か CIDR、ipconfig0）・
//     ci_gateway・ci_dns: cloud-init。
//   - ciupgrade: true / false。cloud-init の初回の起動でパッケージを更新するか
//     （Proxmox の ciupgrade。書かれていなければ Proxmox の既定 1 を true と
//     観測する）。書かなければ Proxmox に渡さず比べない（既定を変えない）。
//     外への出口が無い網では false を明示すると、初回の起動が更新の時間切れを
//     待たない（Backlog #170）。
//   - power: running / stopped。停止は正常停止（shutdown）。power_off:
//     graceful_then_force と force_after を宣言したときだけ、正常停止から
//     force_after を過ぎて強制停止（stop）に進み、その Op は破壊的（§13）。
//   - apply_pending: reboot / never（既定）。reboot のときだけ、宣言した
//     フィールドに再起動を待つ変更（pending）があれば VM を再起動する。
//
// 観測の値は VM の設定が言う値（再起動を待つ変更 pending があればその値）。
// 観測だけの値（Computed）: lock（Proxmox の lock）、pending（再起動を待つ
// フィールドの名前）、current_values（それらの今効いている値、JSON）。
//
// 再起動が要る変更：走っている VM の設定の一部（cores・memory など、hotplug
// できないもの）は Proxmox が pending に置き、再起動で効く。宣言は再起動の
// 後の値（観測の値）と比べる：宣言の値が pending にあれば差分にならず（重ねて
// 書かない）、pending に名前が出る。手の変更が pending にあれば（今効いている
// 値は宣言どおりでも）差分にして宣言の値を書き戻す（Proxmox は今の値と同じ
// pending を消す）。再起動は apply_pending: reboot か、Workflow の action（reboot・reset、
// provider.Actioner）で明示したときだけ。
//
// 非同期の仕事（UPID）：作る・複製する・電源・消すは Proxmox の仕事として
// 走る。Apply は task_wait まで終わりを待ち、終わらなければ UPID をインメモリ
// （S3）に持って戻る。仕事が終わるまで（と、VM に lock がある間）Read は
// Converging。仕事が失敗で終わったら、次の Read が 1 回だけその失敗を返す
// （Unknown）。UPID を失っても（再起動）、lock が残る間は Converging のまま。
//
// 触らない VM（設定の protected_vmids・protected_names。Sakutei 自身が動く
// VM など段0 のもの、§23）は List に出さず、Read・Apply もしない。intent で
// 宣言すれば計画のエラー。テンプレートも List に出さない（複製の元）。
//
// /cluster/resources の名前・status・template は pvestatd の写しで遅れる（作った
// 直後の VM は名前が空）。名前が空の項目は VM の設定で確かめ、電源と lock は
// /status/current で読む（Cluster.resources・Cluster.powerOf）。
//
// 届かない・5xx・認証の失敗は観測できない（Unknown。absent にしない）。List は
// 全クラスタの /cluster/resources が取れ、どの VM も名前で一意に引けるときだけ
// ListComplete（名前の無い・重なる VM があればエラー）。
package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/render"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

const (
	// Name は provider 名（core.ID.Provider に入る）。
	Name = "proxmox-vm"

	// ProviderVersion は diff / 正規化の版（§10.4）。正規形の作り方を変えたら上げる。
	ProviderVersion = Name + "-v1"
)

// フィールド名。
const (
	FieldCluster       = "cluster"
	FieldName          = "name"
	FieldVMID          = "vmid"
	FieldNode          = "node"
	FieldTemplate      = "template"
	FieldPool          = "pool"
	FieldCores         = "cores"
	FieldMemory        = "memory"
	FieldDisks         = "disks"
	FieldNICs          = "nics"
	FieldCIUser        = "ci_user"
	FieldCISSHKeys     = "ci_ssh_keys"
	FieldCIIP          = "ci_ip"
	FieldCIGateway     = "ci_gateway"
	FieldCIDNS         = "ci_dns"
	FieldCIUpgrade     = "ciupgrade"
	FieldPower         = "power"
	FieldPowerOff      = "power_off"
	FieldForceAfter    = "force_after"
	FieldApplyPending  = "apply_pending"
	FieldLock          = "lock"
	FieldPending       = "pending"
	FieldCurrentValues = "current_values"
)

// 値。
const (
	PowerRunning = "running"
	PowerStopped = "stopped"

	PowerOffGraceful          = "graceful"
	PowerOffGracefulThenForce = "graceful_then_force"

	ApplyPendingReboot = "reboot"
	ApplyPendingNever  = "never"
)

// 一度きりの操作（provider.Actioner、Workflow の action 手順）。
const (
	ActionReboot = "reboot"
	ActionReset  = "reset"
)

// ListSep は pending（StringList）の区切り。
const ListSep = ","

// configFields は VM の設定から観測するフィールド（pending を比べるもの）。
var configFields = []string{FieldCores, FieldMemory, FieldDisks, FieldNICs, FieldCIUser, FieldCISSHKeys, FieldCIIP, FieldCIGateway, FieldCIDNS, FieldCIUpgrade}

var ciFields = []string{FieldCIUser, FieldCISSHKeys, FieldCIIP, FieldCIGateway, FieldCIDNS, FieldCIUpgrade}

// poolName は pool の名前（Proxmox の決まり。入れ子は "/" で 3 段まで）。
var poolName = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+){0,2}$`)

// vmName は VM の名前（Proxmox の決まり：DNS の名前）。
var vmName = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9.-]{0,61}[a-zA-Z0-9])?$`)

// Cluster は 1 つのクラスタ。
type Cluster struct {
	Client *Client
	// Node は node を宣言しない VM を作るノード（空なら node の宣言が要る）。
	Node string
	// Scope は List・BulkObserve が返すこのクラスタの ID の scope。
	Scope core.Scope
	// ProtectedVMIDs・ProtectedNames は触らない VM。
	ProtectedVMIDs []int
	ProtectedNames []string
	// TaskWait は Apply が非同期の仕事を待つ長さ（0 なら待たない）。
	TaskWait time.Duration
}

func (c Cluster) protected(vmid int, name string) bool {
	for _, v := range c.ProtectedVMIDs {
		if v == vmid {
			return true
		}
	}
	return contains(c.ProtectedNames, name)
}

// Options は Provider の設定。
type Options struct {
	// Now は時刻（force_after の計測）。nil なら time.Now。
	Now func() time.Time
	// PollInterval は仕事の終わりを待つときの問い合わせの間隔（既定 1s）。
	PollInterval time.Duration
}

// Provider は provider.Provider・Lister・BulkObserver・IntentValidator・
// PlanRefiner・Actioner を実装する。
type Provider struct {
	clusters map[string]Cluster
	now      func() time.Time
	poll     time.Duration

	mu sync.Mutex
	// tasks は VM（Local ID）ごとの走っている仕事の UPID（S3）。
	tasks map[string]string
	// gracefulSent は VM ごとに最初の shutdown を送った時刻（S3）。
	gracefulSent map[string]time.Time
}

var (
	_ provider.Provider        = (*Provider)(nil)
	_ provider.Lister          = (*Provider)(nil)
	_ provider.BulkObserver    = (*Provider)(nil)
	_ provider.IntentValidator = (*Provider)(nil)
	_ provider.AbsentValidator = (*Provider)(nil)
	_ provider.PlanRefiner     = (*Provider)(nil)
	_ provider.Actioner        = (*Provider)(nil)
	// §7.3 (#158): intent の external_owners から vmid などで選べ、設定の
	// protected_vmids・protected_names を intent と突き合わせられる。
	_ provider.OwnerFieldSelector = (*Provider)(nil)
	_ provider.ConfigExcluder     = (*Provider)(nil)
)

// New は clusters（名前 → Cluster）を使う Provider を返す。
func New(clusters map[string]Cluster, opts Options) *Provider {
	cs := make(map[string]Cluster, len(clusters))
	for k, v := range clusters {
		cs[k] = v
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	poll := opts.PollInterval
	if poll <= 0 {
		poll = time.Second
	}
	return &Provider{clusters: cs, now: now, poll: poll, tasks: map[string]string{}, gracefulSent: map[string]time.Time{}}
}

// Schema: identity は cluster と name。観測だけの値は Computed。
func (p *Provider) Schema() provider.Schema {
	s := func() provider.FieldSpec { return provider.FieldSpec{Kind: core.FieldKindString} }
	computed := func() provider.FieldSpec { return provider.FieldSpec{Kind: core.FieldKindString, Computed: true} }
	return provider.Schema{Fields: map[string]provider.FieldSpec{
		FieldCluster:       {Kind: core.FieldKindString, Identity: true},
		FieldName:          {Kind: core.FieldKindString, Identity: true},
		FieldVMID:          {Kind: core.FieldKindInt},
		FieldNode:          s(),
		FieldTemplate:      s(),
		FieldPool:          s(),
		FieldCores:         {Kind: core.FieldKindInt},
		FieldMemory:        s(),
		FieldDisks:         s(),
		FieldNICs:          s(),
		FieldCIUser:        s(),
		FieldCISSHKeys:     s(),
		FieldCIIP:          s(),
		FieldCIGateway:     s(),
		FieldCIDNS:         s(),
		FieldCIUpgrade:     {Kind: core.FieldKindBool},
		FieldPower:         s(),
		FieldPowerOff:      s(),
		FieldForceAfter:    s(),
		FieldApplyPending:  s(),
		FieldLock:          computed(),
		FieldPending:       {Kind: core.FieldKindStringList, Computed: true, ListSep: ListSep},
		FieldCurrentValues: computed(),
	}}
}

// Capabilities は完全の階級。VM は破壊的な型なので prune しない（§12.3）。
func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		TrueObserve:     true,
		List:            true,
		BulkObserve:     true,
		Pruneable:       false,
		Converging:      true,
		ProviderVersion: ProviderVersion,
	}
}

// ---- ID ----

// LocalID はクラスタと VM 名から ID を作る。
func LocalID(cluster, vm string) string { return cluster + "/" + vm }

func (p *Provider) coords(localID string) (string, Cluster, string, error) {
	name, vm, ok := strings.Cut(localID, "/")
	if !ok || name == "" || vm == "" {
		return "", Cluster{}, "", fmt.Errorf("id %q: want <cluster>/<vm name>", localID)
	}
	c, ok := p.clusters[name]
	if !ok {
		return "", Cluster{}, "", fmt.Errorf("id %q: no cluster %q is configured (add it to the cluster list, proxmox_config)", localID, name)
	}
	if !vmName.MatchString(vm) {
		return "", Cluster{}, "", fmt.Errorf("id %q: %q is not a valid VM name (letters, digits, '-' and '.', starting and ending with a letter or digit)", localID, vm)
	}
	return name, c, vm, nil
}

func (p *Provider) clusterNames() []string {
	names := make([]string, 0, len(p.clusters))
	for n := range p.clusters {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ---- 仕事（UPID）の記録（S3） ----

func (p *Provider) track(localID, upid string) {
	p.mu.Lock()
	p.tasks[localID] = upid
	p.mu.Unlock()
}

func (p *Provider) tracked(localID string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	u, ok := p.tasks[localID]
	return u, ok
}

func (p *Provider) untrack(localID, upid string) {
	p.mu.Lock()
	if p.tasks[localID] == upid {
		delete(p.tasks, localID)
	}
	p.mu.Unlock()
}

// TrackedTask は localID の走っている仕事の UPID を返す（テストと文言用）。
func (p *Provider) TrackedTask(localID string) (string, bool) { return p.tracked(localID) }

func (p *Provider) forget(localID string) {
	p.mu.Lock()
	delete(p.gracefulSent, localID)
	p.mu.Unlock()
}

// ---- 観測 ----

func obsOf(a core.Availability) core.Observation {
	return core.Observation{Availability: a, ObservedAt: time.Now()}
}

// Read は VM を観測する。desired を見ない（ADR-0002）。
func (p *Provider) Read(ctx context.Context, localID string) (core.Observation, error) {
	_, c, vm, err := p.coords(localID)
	if err != nil {
		return obsOf(core.AvailabilityUnknown), provider.NewPermanentError(fmt.Errorf("%s: Read: %w", Name, err))
	}
	if contains(c.ProtectedNames, vm) {
		return obsOf(core.AvailabilityUnknown), provider.NewPermanentError(fmt.Errorf("%s: Read %q: the VM is protected (protected_names): Sakutei never touches it", Name, localID))
	}
	if o, done, err := p.observeTask(ctx, c, localID); done {
		return o, err
	}
	res, err := c.resources(ctx)
	if err != nil {
		return obsOf(core.AvailabilityUnknown), classify(fmt.Errorf("%s: Read %q: %w", Name, localID, err))
	}
	return p.observeVM(ctx, c, localID, vm, res)
}

// observeTask は localID の記録した仕事を見る。done が true なら観測はそこで
// 決まる（走っている＝Converging、失敗で終わった＝Unknown と error）。
func (p *Provider) observeTask(ctx context.Context, c Cluster, localID string) (core.Observation, bool, error) {
	upid, ok := p.tracked(localID)
	if !ok {
		return core.Observation{}, false, nil
	}
	st, err := c.Client.Task(ctx, upid)
	if err != nil {
		return obsOf(core.AvailabilityUnknown), true, classify(fmt.Errorf("%s: Read %q: the status of task %s: %w", Name, localID, upid, err))
	}
	if st.Running {
		return obsOf(core.AvailabilityConverging), true, nil
	}
	p.untrack(localID, upid)
	if !st.OK() {
		return obsOf(core.AvailabilityUnknown), true, provider.NewTransientError(fmt.Errorf("%s: %q: task %s failed: %s", Name, localID, upid, st.ExitStatus))
	}
	return core.Observation{}, false, nil
}

// resources はクラスタの VM の一覧を返す。/cluster/resources の名前は
// pvestatd の写しで、作った直後の VM は名前が空のことがある（「無い」と見て
// もう一度作らないように）。名前が空の qemu の項目は、VM の設定（pmxcfs が正）で
// 名前とテンプレートかを確かめて埋める。確かめられなければ error。
func (c Cluster) resources(ctx context.Context) ([]Resource, error) {
	res, err := c.Client.Resources(ctx)
	if err != nil {
		return nil, err
	}
	for i := range res {
		r := &res[i]
		if r.Type != "qemu" || r.Name != "" {
			continue
		}
		cfg, err := c.Client.Config(ctx, r.Node, r.VMID)
		if err != nil {
			return nil, fmt.Errorf("VM %d is listed without a name; reading its config: %w", r.VMID, err)
		}
		r.Name, r.Template = cfg["name"], cfg["template"] == "1"
	}
	return res, nil
}

// powerOf は VM の今の電源と lock を /status/current で読み、r に入れる
// （/cluster/resources の status は古いことがある）。
func (c Cluster) powerOf(ctx context.Context, r *Resource) error {
	st, err := c.Client.CurrentStatus(ctx, r.Node, r.VMID)
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

// observeVM は res（/cluster/resources）の中の vm を観測する。
func (p *Provider) observeVM(ctx context.Context, c Cluster, localID, vm string, res []Resource) (core.Observation, error) {
	found := findVM(res, vm)
	switch {
	case len(found) == 0:
		return obsOf(core.AvailabilityOk), nil
	case len(found) > 1:
		ids := make([]string, len(found))
		for i, r := range found {
			ids[i] = strconv.Itoa(r.VMID)
		}
		return obsOf(core.AvailabilityUnknown), provider.NewPermanentError(fmt.Errorf("%s: Read %q: %d VMs are named %s (vmid %s); rename all but one", Name, localID, len(found), vm, strings.Join(ids, ", ")))
	}
	r := found[0]
	if c.protected(r.VMID, r.Name) {
		return obsOf(core.AvailabilityUnknown), provider.NewPermanentError(fmt.Errorf("%s: Read %q: VM %d is protected: Sakutei never touches it", Name, localID, r.VMID))
	}
	if r.Template {
		return obsOf(core.AvailabilityUnknown), provider.NewPermanentError(fmt.Errorf("%s: Read %q: VM %d is a template, not a VM", Name, localID, r.VMID))
	}
	if err := c.powerOf(ctx, &r); err != nil {
		return obsOf(core.AvailabilityUnknown), classify(fmt.Errorf("%s: Read %q: %w", Name, localID, err))
	}
	if r.Status != PowerRunning && r.Status != PowerStopped {
		return obsOf(core.AvailabilityUnknown), provider.NewTransientError(fmt.Errorf("%s: Read %q: VM %d on node %s has status %q", Name, localID, r.VMID, r.Node, r.Status))
	}
	if r.Lock != "" {
		return obsOf(core.AvailabilityConverging), nil
	}
	entries, err := c.Client.Pending(ctx, r.Node, r.VMID)
	if err != nil {
		return obsOf(core.AvailabilityUnknown), classify(fmt.Errorf("%s: Read %q: %w", Name, localID, err))
	}
	cur, next := splitPending(entries)
	if cur["lock"] != "" {
		return obsOf(core.AvailabilityConverging), nil
	}
	if r.Status == PowerStopped {
		p.forget(localID)
	}
	clusterName, _, _ := strings.Cut(localID, "/")
	return core.Observation{Availability: core.AvailabilityOk, State: toState(clusterName, r, cur, next), ObservedAt: time.Now()}, nil
}

// splitPending は /pending を今の設定と、再起動の後の設定に分ける。
func splitPending(entries []PendingEntry) (cur, next map[string]string) {
	cur, next = map[string]string{}, map[string]string{}
	for _, e := range entries {
		if e.HasValue {
			cur[e.Key] = e.Value
		}
		switch {
		case e.Delete:
		case e.HasPending:
			next[e.Key] = e.Pending
		case e.HasValue:
			next[e.Key] = e.Value
		}
	}
	return cur, next
}

// configState は設定の値を configFields の正規形にする。
func configState(cfg map[string]string) map[string]string {
	out := map[string]string{}
	cores := strings.TrimSpace(cfg["cores"])
	if cores == "" {
		cores = "1"
	}
	out[FieldCores] = cores
	out[FieldMemory] = observedMemory(cfg["memory"])
	var disks []Disk
	var nics []NIC
	for k, v := range cfg {
		if diskSlotRe.MatchString(k) {
			if d, ok := diskFromConfig(k, v); ok {
				disks = append(disks, d)
			}
		}
		if nicSlotRe.MatchString(k) {
			nics = append(nics, nicFromConfig(k, v))
		}
	}
	sortDisks(disks)
	sortNICs(nics)
	out[FieldDisks] = FormatDisks(disks)
	out[FieldNICs] = FormatNICs(nics)
	out[FieldCIUser] = cfg["ciuser"]
	out[FieldCISSHKeys] = observedSSHKeys(cfg["sshkeys"])
	ip, gw := ipconfigParts(cfg["ipconfig0"])
	out[FieldCIIP], out[FieldCIGateway] = ip, gw
	if c, err := CanonIP(ip); err == nil {
		out[FieldCIIP] = c
	}
	dns := cfg["nameserver"]
	if c, err := CanonDNS(dns); err == nil {
		dns = c
	}
	out[FieldCIDNS] = dns
	out[FieldCIUpgrade] = observedBool(cfg["ciupgrade"], true)
	return out
}

// observedBool は Proxmox の真偽値（0 / 1、無ければ def）を "true" / "false" にする。
func observedBool(v string, def bool) string {
	switch strings.TrimSpace(v) {
	case "":
		return strconv.FormatBool(def)
	case "0":
		return "false"
	}
	return "true"
}

// toState は観測を Schema どおりの State にする。
func toState(cluster string, r Resource, cur, next map[string]string) core.State {
	str := func(v string) core.TypedValue { return core.TypedValue{Kind: core.FieldKindString, Value: v} }
	now, after := configState(cur), configState(next)
	st := core.State{
		FieldCluster: str(cluster),
		FieldName:    str(r.Name),
		FieldVMID:    {Kind: core.FieldKindInt, Value: int64(r.VMID)},
		FieldNode:    str(r.Node),
		FieldPool:    str(r.Pool),
		FieldPower:   str(r.Status),
		FieldLock:    str(cur["lock"]),
	}
	current := map[string]string{}
	var names []string
	for _, f := range configFields {
		switch f {
		case FieldCores:
			n, _ := strconv.ParseInt(after[f], 10, 64)
			st[f] = core.TypedValue{Kind: core.FieldKindInt, Value: n}
		case FieldCIUpgrade:
			st[f] = core.TypedValue{Kind: core.FieldKindBool, Value: after[f] == "true"}
		default:
			st[f] = str(after[f])
		}
		if after[f] != now[f] {
			current[f] = now[f]
			names = append(names, f)
		}
	}
	st[FieldPending] = core.TypedValue{Kind: core.FieldKindStringList, Value: strings.Join(names, ListSep)}
	cv := ""
	if len(current) > 0 {
		data, _ := json.Marshal(current)
		cv = string(data)
	}
	st[FieldCurrentValues] = str(cv)
	return st
}

// List は全クラスタの VM（テンプレートと触らない VM を除く）を返す。全
// クラスタの一覧が取れ、どの VM も名前で一意に引けるときだけ ListComplete、
// そうでなければ error（complete-or-error、§5.4）。
func (p *Provider) List(ctx context.Context, _ core.Scope) (core.ListResult, error) {
	var ids []core.ID
	for _, name := range p.clusterNames() {
		c := p.clusters[name]
		res, err := c.resources(ctx)
		if err != nil {
			return core.ListResult{}, classify(fmt.Errorf("%s: List %s: %w", Name, name, err))
		}
		vms, err := managedVMs(c, res)
		if err != nil {
			return core.ListResult{}, provider.NewPermanentError(fmt.Errorf("%s: List %s: %w", Name, name, err))
		}
		for _, r := range vms {
			ids = append(ids, core.NewID(Name, c.Scope, LocalID(name, r.Name)))
		}
	}
	return core.ListResult{IDs: ids, Status: core.ListComplete}, nil
}

// managedVMs は res の中の、扱う VM（qemu で、テンプレートでも触らない VM でも
// ないもの）を返す。名前の無い・使えない・重なる VM があればエラー。
func managedVMs(c Cluster, res []Resource) ([]Resource, error) {
	var out []Resource
	seen := map[string]int{}
	var bad []string
	for _, r := range res {
		if r.Type != "qemu" || r.Template || c.protected(r.VMID, r.Name) {
			continue
		}
		if !vmName.MatchString(r.Name) {
			bad = append(bad, fmt.Sprintf("VM %d has no usable name (%q)", r.VMID, r.Name))
			continue
		}
		if other, dup := seen[r.Name]; dup {
			bad = append(bad, fmt.Sprintf("VMs %d and %d are both named %s", other, r.VMID, r.Name))
			continue
		}
		seen[r.Name] = r.VMID
		out = append(out, r)
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("the VMs cannot all be told apart by name, so the list is not complete: %s (name them uniquely, or add them to protected_vmids)", strings.Join(bad, "; "))
	}
	return out, nil
}

// BulkObserve はクラスタごとに /cluster/resources を 1 回だけ呼び、VM ごとに
// 設定を読む（§5.7）。各エントリは Read が返すものと同じ。
//
// 一覧が取れないクラスタの VM は結果に含めない（「この呼び出しでは観測して
// いない」。SDK が Read に回し、Read が Unknown にする）。名前で一意に引けない
// VM も含めない（Read がエラーにする）。全クラスタが失敗したときだけ error。
func (p *Provider) BulkObserve(ctx context.Context, _ core.Scope) (provider.BulkObservation, error) {
	obs := map[core.IDKey]core.Observation{}
	var firstErr error
	failed := 0
	for _, name := range p.clusterNames() {
		c := p.clusters[name]
		res, err := c.resources(ctx)
		if err != nil {
			failed++
			if firstErr == nil {
				firstErr = classify(fmt.Errorf("%s: BulkObserve %s: %w", Name, name, err))
			}
			continue
		}
		count := map[string]int{}
		for _, r := range res {
			if r.Type == "qemu" {
				count[r.Name]++
			}
		}
		for _, r := range res {
			if r.Type != "qemu" || r.Template || c.protected(r.VMID, r.Name) || !vmName.MatchString(r.Name) || count[r.Name] != 1 {
				continue
			}
			localID := LocalID(name, r.Name)
			key := core.NewID(Name, c.Scope, localID).Key()
			if o, done, _ := p.observeTask(ctx, c, localID); done {
				obs[key] = o
				continue
			}
			o, _ := p.observeVM(ctx, c, localID, r.Name, res)
			if o.Availability == core.AvailabilityOk && o.State == nil {
				continue // 読む間に消えた。Read に回す。
			}
			obs[key] = o
		}
	}
	if failed > 0 && failed == len(p.clusters) {
		return provider.BulkObservation{}, firstErr
	}
	return provider.BulkObservation{Observations: obs}, nil
}

// ---- 計画の時点の検査 ----

// OwnerSelectFields は intent の external_owners（触らない宣言、§7.3・
// Backlog #158）が観測の値で選べるフィールド：vmid・node・pool。
func (p *Provider) OwnerSelectFields() []string {
	return []string{FieldVMID, FieldNode, FieldPool}
}

// ConfigExclusions は設定の触らない VM（protected_vmids・protected_names）を
// external_owners と同じ選び方で申告する。コアはこれを最後の安全の網として
// intent と突き合わせる：intent がこれを管理すれば読み込みで断り、intent の
// external_owners に無ければ書くように知らせる（§7.3・§29.1）。
func (p *Provider) ConfigExclusions() []provider.ConfigExclusion {
	var out []provider.ConfigExclusion
	for _, name := range p.clusterNames() {
		c := p.clusters[name]
		for _, vmid := range c.ProtectedVMIDs {
			out = append(out, provider.ConfigExclusion{
				Setting:  fmt.Sprintf("clusters.%s.protected_vmids", name),
				Scope:    c.Scope,
				IDPrefix: name + "/",
				Fields:   map[string]string{FieldVMID: strconv.Itoa(vmid)},
			})
		}
		for _, vm := range c.ProtectedNames {
			out = append(out, provider.ConfigExclusion{
				Setting: fmt.Sprintf("clusters.%s.protected_names", name),
				Scope:   c.Scope,
				ID:      name + "/" + vm,
			})
		}
	}
	return out
}

// ValidateAbsent は ensure: absent（宣言で消す、§12.2・Backlog #163）の誤りを、
// 読み込みの時点で断る：ID の形と、触らない VM（protected_names。Sakutei 自身が
// 動く VM など段0 のもの、§23）を消す宣言。protected_vmids の VM は名前では
// 分からないので、Read・Apply が触らないことで守る（消さずに Unknown）。
func (p *Provider) ValidateAbsent(localID string) error {
	name, c, vm, err := p.coords(localID)
	if err != nil {
		return fmt.Errorf("%s: %w", Name, err)
	}
	if contains(c.ProtectedNames, vm) {
		return fmt.Errorf("%s %q: VM %s is protected in cluster %s (protected_names): Sakutei never touches it, so it cannot be declared absent (§23)", Name, localID, vm, name)
	}
	return nil
}

// ValidateIntent は現実を見ずに決まる誤りを、読み込みの時点で断る（§28.4）。
func (p *Provider) ValidateIntent(localID string, fields map[string]string) error {
	name, c, vm, err := p.coords(localID)
	if err != nil {
		return fmt.Errorf("%s: %w", Name, err)
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s %q: "+format, append([]any{Name, localID}, args...)...)
	}
	schema := p.Schema()
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		spec, ok := schema.Fields[k]
		switch {
		case !ok:
			return fail("unknown field %q", k)
		case spec.Computed:
			return fail("field %q is observed only and cannot be declared", k)
		}
	}
	if contains(c.ProtectedNames, vm) {
		return fail("VM %s is protected in cluster %s (protected_names): Sakutei never touches it, so it cannot be declared (§23)", vm, name)
	}
	if v, ok := fields[FieldCluster]; ok && v != name {
		return fail("cluster %q must equal the cluster in the id (%s)", v, name)
	}
	if v, ok := fields[FieldName]; ok && v != vm {
		return fail("name %q must equal the VM name in the id (%s)", v, vm)
	}
	if v, ok := fields[FieldVMID]; ok && !allocationPlaceholder(v) {
		n, err := strconv.Atoi(v)
		if err != nil || n < 100 || n > 999999999 {
			return fail("vmid %q: want a number from 100 to 999999999", v)
		}
		if c.protected(n, "") {
			return fail("vmid %d is protected in cluster %s (protected_vmids): Sakutei never touches it, so it cannot be declared (§23)", n, name)
		}
	}
	if v, ok := fields[FieldNode]; ok && strings.TrimSpace(v) == "" {
		return fail("node is empty")
	}
	if _, ok := fields[FieldNode]; !ok && c.Node == "" {
		return fail("node is not declared and cluster %s has no default node (set node in the cluster config, or declare it)", name)
	}
	if v, ok := fields[FieldTemplate]; ok && strings.TrimSpace(v) == "" {
		return fail("template is empty")
	}
	if v, ok := fields[FieldPool]; ok && v != "" && !poolName.MatchString(v) {
		return fail("pool %q: want a Proxmox VE pool name (letters, digits, '.', '_' and '-'; nested pools as parent/child), or \"\" for no pool", v)
	}
	if v, ok := fields[FieldCores]; ok {
		if n, err := strconv.Atoi(v); err != nil || n < 1 {
			return fail("cores %q: want a positive number", v)
		}
	}
	if v, ok := fields[FieldMemory]; ok {
		if _, err := ParseMemory(v); err != nil {
			return fail("%w", err)
		}
	}
	if v, ok := fields[FieldDisks]; ok {
		if _, err := ParseDisks(v); err != nil {
			return fail("%w", err)
		}
	}
	if v, ok := fields[FieldNICs]; ok {
		if _, err := ParseNICs(v); err != nil {
			return fail("%w", err)
		}
	}
	if v, ok := fields[FieldCIIP]; ok && v != "" {
		if _, err := CanonIP(v); err != nil {
			return fail("%w", err)
		}
	}
	if v, ok := fields[FieldCIGateway]; ok && v != "" {
		if _, err := CanonGateway(v); err != nil {
			return fail("%w", err)
		}
	}
	if v, ok := fields[FieldCIDNS]; ok {
		if _, err := CanonDNS(v); err != nil {
			return fail("%w", err)
		}
	}
	if v, ok := fields[FieldCISSHKeys]; ok {
		if err := CheckSSHKeys(v); err != nil {
			return fail("%w", err)
		}
	}
	if v, ok := fields[FieldCIUpgrade]; ok && v != "true" && v != "false" {
		return fail("ciupgrade %q: want true or false", v)
	}
	if v, ok := fields[FieldCIUser]; ok && strings.ContainsAny(v, " \t\n:") {
		return fail("ci_user %q: want a user name without spaces or ':'", v)
	}
	if v, ok := fields[FieldPower]; ok && v != PowerRunning && v != PowerStopped {
		return fail("power %q: want %s or %s (a restart is a one-time operation: use apply_pending: reboot, or a Workflow step action: { op: %s })", v, PowerRunning, PowerStopped, ActionReboot)
	}
	if v, ok := fields[FieldApplyPending]; ok && v != ApplyPendingReboot && v != ApplyPendingNever {
		return fail("apply_pending %q: want %s or %s", v, ApplyPendingReboot, ApplyPendingNever)
	}
	mode, hasMode := fields[FieldPowerOff]
	if hasMode && mode != PowerOffGraceful && mode != PowerOffGracefulThenForce {
		return fail("power_off %q: want %s or %s", mode, PowerOffGraceful, PowerOffGracefulThenForce)
	}
	fa, hasFA := fields[FieldForceAfter]
	switch {
	case mode == PowerOffGracefulThenForce && !hasFA:
		return fail("power_off %s needs force_after (a duration such as \"5m\")", PowerOffGracefulThenForce)
	case mode != PowerOffGracefulThenForce && hasFA:
		return fail("force_after is only for power_off %s (the default %s never forces the power off)", PowerOffGracefulThenForce, PowerOffGraceful)
	case hasFA:
		if d, err := time.ParseDuration(fa); err != nil || d <= 0 {
			return fail("force_after %q: want a positive duration such as \"5m\"", fa)
		}
	}
	return nil
}

// RefinePlan は core の差分を Proxmox の決まりで直す（§28.4）。I/O はしない。
//
//   - template・power_off・force_after（作り方・止め方の宣言で、観測しない）の
//     差分は落とす。
//   - vmid・node が今の VM と違えば計画のエラー（番号を変えない・移行しない）。
//   - 正規形が観測（再起動の後の値）と同じなら落とす（Proxmox の書き換える
//     表記を drift にしない）。disks・nics は宣言した slot だけを比べる。
//     ディスクを小さくする・ストレージを変えるのは計画のエラー。
//   - apply_pending: reboot は、宣言したフィールドに pending があり、VM が
//     走っていて、power を stopped と宣言していないときだけ残す。
//   - power_off: graceful_then_force で最初の shutdown から force_after を
//     過ぎた停止は強制停止（stop）になるので、その Op を破壊的にする（§13）。
func (p *Provider) RefinePlan(localID string, observed core.State, desired map[string]string, ops []core.Op) ([]core.Op, error) {
	if desired[FieldPower] != PowerStopped {
		p.forget(localID)
	}
	obs := func(f string) string {
		tv, ok := observed[f]
		if !ok {
			return ""
		}
		return fmt.Sprintf("%v", tv.Value)
	}
	if v, ok := desired[FieldVMID]; ok && !allocationPlaceholder(v) && obs(FieldVMID) != "" && v != obs(FieldVMID) {
		return nil, fmt.Errorf("%s %q: vmid %s is declared but the VM has vmid %s; a VM is never renumbered (declare its vmid, or recreate it)", Name, localID, v, obs(FieldVMID))
	}
	if v, ok := desired[FieldNode]; ok && obs(FieldNode) != "" && v != obs(FieldNode) {
		return nil, fmt.Errorf("%s %q: node %s is declared but the VM is on node %s; migration is not managed (migrate it by hand, or declare node %s)", Name, localID, v, obs(FieldNode), obs(FieldNode))
	}
	pendingFields := map[string]bool{}
	if pl := obs(FieldPending); pl != "" {
		for _, f := range strings.Split(pl, ListSep) {
			pendingFields[f] = true
		}
	}
	var pendingDeclared []string
	for _, f := range configFields {
		if _, declared := desired[f]; declared && pendingFields[f] {
			pendingDeclared = append(pendingDeclared, f)
		}
	}

	out := make([]core.Op, 0, len(ops))
	for _, op := range ops {
		var fields []core.FieldDiff
		for _, f := range op.Fields {
			switch f.Field {
			case FieldTemplate, FieldPowerOff, FieldForceAfter:
				continue
			case FieldApplyPending:
				if f.To == ApplyPendingReboot && len(pendingDeclared) > 0 && obs(FieldPower) == PowerRunning && desired[FieldPower] != PowerStopped {
					f.From = "pending: " + strings.Join(pendingDeclared, ", ")
					fields = append(fields, f)
				}
				continue
			}
			match, err := fieldMatches(f.Field, f.To, obs(f.Field))
			if err != nil {
				return nil, fmt.Errorf("%s %q: %w", Name, localID, err)
			}
			if match {
				continue
			}
			fields = append(fields, f)
		}
		op.Fields = fields
		for _, f := range fields {
			if f.Field == FieldPower && f.To == PowerStopped {
				if force, why := p.forceDue(localID, desired); force {
					op.Destructive = true
					op.Diagnostic = fmt.Sprintf("resource %s did not stop after shutdown; %s: will stop it by force (destructive)", op.Target, why)
				}
			}
		}
		out = append(out, op)
	}
	return out, nil
}

// allocationPlaceholder は v が採番を待つ仮の値（plan・status の Lookup で
// まだ払い出していない、または allocator が答えない）かを返す。仮の値は
// Apply に届かない（§19.9）。
func allocationPlaceholder(v string) bool {
	return strings.HasPrefix(v, render.PendingPrefix) || strings.HasPrefix(v, render.WaitingPrefix)
}

// fieldMatches は宣言の値 want が観測の正規形 have と同じかを返す。
// 計画のエラー（ディスクを小さくするなど）は error。
func fieldMatches(field, want, have string) (bool, error) {
	switch field {
	case FieldCores:
		a, err1 := strconv.Atoi(strings.TrimSpace(want))
		b, err2 := strconv.Atoi(strings.TrimSpace(have))
		return err1 == nil && err2 == nil && a == b, nil
	case FieldMemory:
		m, err := ParseMemory(want)
		return err == nil && strconv.Itoa(m) == have, nil
	case FieldDisks:
		ws, err := ParseDisks(want)
		if err != nil {
			return false, nil
		}
		hs, _ := parseObservedDisks(have)
		match := true
		for _, w := range ws {
			h, ok := hs[w.Slot]
			if !ok {
				match = false
				continue
			}
			if h.Storage != w.Storage {
				return false, fmt.Errorf("disk %s is on storage %s but %s is declared; moving a disk is not managed (move it by hand, or declare storage=%s)", w.Slot, h.Storage, w.Storage, h.Storage)
			}
			if w.Size < h.Size {
				return false, fmt.Errorf("disk %s is %s but %s is declared; a disk is never shrunk (declare size=%s or larger)", w.Slot, FormatSize(h.Size), FormatSize(w.Size), FormatSize(h.Size))
			}
			if w.Size > h.Size {
				match = false
			}
		}
		return match, nil
	case FieldNICs:
		ws, err := ParseNICs(want)
		if err != nil {
			return false, nil
		}
		hs := parseObservedNICs(have)
		for _, w := range ws {
			h, ok := hs[w.Slot]
			if !ok || !nicMatches(w, h) {
				return false, nil
			}
		}
		return true, nil
	case FieldCISSHKeys:
		return CanonSSHKeys(want) == have, nil
	case FieldCIIP:
		if want == "" {
			return have == "", nil
		}
		c, err := CanonIP(want)
		return err == nil && c == have, nil
	case FieldCIGateway:
		if want == "" {
			return have == "", nil
		}
		c, err := CanonGateway(want)
		return err == nil && c == have, nil
	case FieldCIDNS:
		c, err := CanonDNS(want)
		return err == nil && c == have, nil
	}
	return want == have, nil
}

// nicMatches は宣言の NIC w が観測 h と同じか（mac・model は書いたときだけ）。
func nicMatches(w, h NIC) bool {
	return w.Bridge == h.Bridge && w.VLAN == h.VLAN &&
		(w.MAC == "" || w.MAC == h.MAC) && (w.Model == "" || w.Model == h.Model)
}

// parseObservedDisks は観測の正規形の disks を slot → Disk にする。
func parseObservedDisks(s string) (map[string]Disk, error) {
	out := map[string]Disk{}
	for _, e := range splitEntries(s) {
		slot, kv, err := entryKV(e, "storage", "size")
		if err != nil {
			return nil, err
		}
		size, _ := ParseSize(kv["size"])
		out[slot] = Disk{Slot: slot, Storage: kv["storage"], Size: size}
	}
	return out, nil
}

// parseObservedNICs は観測の正規形の nics を slot → NIC にする。
func parseObservedNICs(s string) map[string]NIC {
	out := map[string]NIC{}
	for _, e := range splitEntries(s) {
		slot, kv, err := entryKV(e, "bridge", "vlan", "mac", "model")
		if err != nil {
			continue
		}
		n := NIC{Slot: slot, Bridge: kv["bridge"], MAC: kv["mac"], Model: kv["model"]}
		n.VLAN, _ = strconv.Atoi(kv["vlan"])
		out[slot] = n
	}
	return out
}

// forceDue は localID の停止が強制停止に進む時かを返す。
func (p *Provider) forceDue(localID string, desired map[string]string) (bool, string) {
	if desired[FieldPowerOff] != PowerOffGracefulThenForce {
		return false, ""
	}
	after, err := time.ParseDuration(desired[FieldForceAfter])
	if err != nil || after <= 0 {
		return false, ""
	}
	p.mu.Lock()
	sent, ok := p.gracefulSent[localID]
	p.mu.Unlock()
	if !ok {
		return false, ""
	}
	if el := p.now().Sub(sent); el >= after {
		return true, fmt.Sprintf("force_after %s elapsed since the first shutdown (%s ago)", after, el.Round(time.Second))
	}
	return false, ""
}

// GracefulSentAt は localID に最初の shutdown を送った時刻（S3）を返す。
func (p *Provider) GracefulSentAt(localID string) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t, ok := p.gracefulSent[localID]
	return t, ok
}

// ---- Apply ----

// Apply は plan を適用する。VM を読み直し、今の現実とずれているものだけを
// 操作する（同じ plan を 2 回適用しても結果は同じ）。
func (p *Provider) Apply(ctx context.Context, plan core.Plan) error {
	for _, op := range plan.Ops {
		if err := p.applyOp(ctx, op); err != nil {
			return err
		}
	}
	return nil
}

func (p *Provider) applyOp(ctx context.Context, op core.Op) error {
	localID := op.Target.Local
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s: Apply %q: "+format, append([]any{Name, localID}, args...)...)
	}
	_, c, vm, err := p.coords(localID)
	if err != nil {
		return provider.NewPermanentError(fail("%w", err))
	}
	if contains(c.ProtectedNames, vm) {
		return provider.NewPermanentError(fail("the VM is protected (protected_names): Sakutei never touches it"))
	}
	if upid, ok := p.tracked(localID); ok {
		if st, err := c.Client.Task(ctx, upid); err != nil || st.Running {
			return provider.NewTransientError(fail("task %s is still running", upid))
		}
		p.untrack(localID, upid)
	}
	res, err := c.resources(ctx)
	if err != nil {
		return classify(fail("%w", err))
	}
	found := findVM(res, vm)
	if len(found) > 1 {
		return provider.NewPermanentError(fail("%d VMs are named %s", len(found), vm))
	}
	var cur *Resource
	if len(found) == 1 {
		cur = &found[0]
		if c.protected(cur.VMID, cur.Name) {
			return provider.NewPermanentError(fail("VM %d is protected: Sakutei never touches it", cur.VMID))
		}
		if cur.Template {
			return provider.NewPermanentError(fail("VM %d is a template, not a VM", cur.VMID))
		}
		if err := c.powerOf(ctx, cur); err != nil {
			return classify(fail("%w", err))
		}
	}

	if op.Kind == core.OpKindDelete {
		if cur == nil {
			return nil
		}
		return p.destroy(ctx, c, localID, *cur)
	}

	want := map[string]string{}
	for _, f := range op.Fields {
		want[f.Field] = f.To
	}
	if cur == nil {
		if op.Kind != core.OpKindCreate {
			return provider.NewTransientError(fail("the VM disappeared"))
		}
		done, err := p.create(ctx, c, localID, vm, want, res)
		if err != nil || !done {
			return err
		}
		if res, err = c.resources(ctx); err != nil {
			return classify(fail("%w", err))
		}
		found = findVM(res, vm)
		if len(found) != 1 {
			return provider.NewTransientError(fail("the VM is not listed after it was created"))
		}
		cur = &found[0]
		if err := c.powerOf(ctx, cur); err != nil {
			return classify(fail("%w", err))
		}
	}
	if cur.Status != PowerRunning && cur.Status != PowerStopped {
		return provider.NewTransientError(fail("VM %d has status %q", cur.VMID, cur.Status))
	}
	return p.update(ctx, c, localID, *cur, want, op)
}

// create は VM を作る（テンプレートの複製か、空から）。仕事が task_wait の
// うちに終われば done。
func (p *Provider) create(ctx context.Context, c Cluster, localID, vm string, want map[string]string, res []Resource) (bool, error) {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s: Apply %q: create: "+format, append([]any{Name, localID}, args...)...)
	}
	node := want[FieldNode]
	if node == "" {
		node = c.Node
	}
	if node == "" {
		return false, provider.NewPermanentError(fail("node is not declared and the cluster has no default node"))
	}
	if want[FieldVMID] == "" {
		return false, provider.NewPermanentError(fail("vmid is not declared (declare it, or allocate it from a pool: vmid: { from_pool: <pool> })"))
	}
	vmid, err := strconv.Atoi(want[FieldVMID])
	if err != nil {
		return false, provider.NewPermanentError(fail("vmid %q is not a number", want[FieldVMID]))
	}
	if c.protected(vmid, "") {
		return false, provider.NewPermanentError(fail("vmid %d is protected: Sakutei never touches it", vmid))
	}
	for _, r := range res {
		if r.VMID == vmid {
			return false, provider.NewPermanentError(fail("vmid %d is already used by %s %q", vmid, r.Type, r.Name))
		}
	}
	var disks []Disk
	if v, ok := want[FieldDisks]; ok {
		if disks, err = ParseDisks(v); err != nil {
			return false, provider.NewPermanentError(fail("%w", err))
		}
	}

	pool := want[FieldPool]
	var upid string
	if tmpl := want[FieldTemplate]; tmpl != "" {
		var src *Resource
		for i, r := range res {
			if r.Type == "qemu" && (r.Name == tmpl || strconv.Itoa(r.VMID) == tmpl) {
				if src != nil {
					return false, provider.NewPermanentError(fail("template %q matches more than one VM", tmpl))
				}
				src = &res[i]
			}
		}
		if src == nil {
			return false, provider.NewPermanentError(fail("template %q is not found in the cluster (name it by its name or vmid)", tmpl))
		}
		if !src.Template {
			// template の印も pvestatd の写し。設定で確かめる。
			cfg, err := c.Client.Config(ctx, src.Node, src.VMID)
			if err != nil {
				return false, classify(fail("template %q: %w", tmpl, err))
			}
			if cfg["template"] != "1" {
				return false, provider.NewPermanentError(fail("%q (VM %d) is not a template", tmpl, src.VMID))
			}
		}
		params := url.Values{"newid": {strconv.Itoa(vmid)}, "name": {vm}, "target": {node}, "full": {"1"}}
		if st := oneStorage(disks); st != "" {
			params.Set("storage", st)
		}
		if pool != "" {
			params.Set("pool", pool)
		}
		if upid, err = c.Client.Clone(ctx, src.Node, src.VMID, params); err != nil {
			return false, createError(fail("clone %d: %w", src.VMID, err), vmid, pool)
		}
	} else {
		params := url.Values{"vmid": {strconv.Itoa(vmid)}, "name": {vm}}
		if pool != "" {
			params.Set("pool", pool)
		}
		for _, f := range []struct{ field, key string }{{FieldCores, "cores"}} {
			if v, ok := want[f.field]; ok {
				params.Set(f.key, v)
			}
		}
		if v, ok := want[FieldMemory]; ok {
			m, err := ParseMemory(v)
			if err != nil {
				return false, provider.NewPermanentError(fail("%w", err))
			}
			params.Set("memory", strconv.Itoa(m))
		}
		scsi := false
		for _, d := range disks {
			params.Set(d.Slot, d.Storage+":"+strconv.FormatInt(d.Size>>30, 10))
			scsi = scsi || strings.HasPrefix(d.Slot, "scsi")
		}
		if scsi {
			params.Set("scsihw", "virtio-scsi-single")
		}
		if v, ok := want[FieldNICs]; ok {
			nics, err := ParseNICs(v)
			if err != nil {
				return false, provider.NewPermanentError(fail("%w", err))
			}
			for _, n := range nics {
				params.Set(n.Slot, nicConfig(n, ""))
			}
		}
		ci := false
		for _, f := range ciFields {
			if _, ok := want[f]; ok {
				ci = true
			}
		}
		if ci {
			if len(disks) == 0 {
				return false, provider.NewPermanentError(fail("a cloud-init drive needs a storage: declare disks (the drive goes on the storage of the first disk)"))
			}
			params.Set("ide2", disks[0].Storage+":cloudinit")
			if err := ciParams(params, want, nil); err != nil {
				return false, provider.NewPermanentError(fail("%w", err))
			}
		}
		if upid, err = c.Client.Create(ctx, node, params); err != nil {
			return false, createError(fail("%w", err), vmid, pool)
		}
	}
	return p.waitTask(ctx, c, localID, upid)
}

// createError は作る・複製するときの失敗を分ける。pool が無い（Proxmox は 403
// で断る）・pool に権限が無いことと、見えない VM がその vmid を使っている
// こと（pool に絞ったトークンには pool の外の VM が見えない。Proxmox は 500 で
// 断る）は、設定か宣言を直すまで治らない。
func createError(err error, vmid int, pool string) error {
	switch {
	case pool != "" && KindOf(err) == KindAuth:
		return provider.NewPermanentError(fmt.Errorf("%w (pool %s must exist in Proxmox VE — Sakutei does not create pools — and the token needs VM.Allocate on /pool/%s)", err, pool, pool))
	case strings.Contains(err.Error(), "already exists"):
		return provider.NewPermanentError(fmt.Errorf("%w (vmid %d is used by a VM this token cannot see, e.g. one outside the token's pool: declare another vmid, or put that VM back into the pool)", err, vmid))
	}
	return classify(err)
}

// movePool は VM を pool に移す（to が "" なら今の pool から外す）。fail は
// エラーの文言の頭を付ける。
func movePool(ctx context.Context, c Cluster, vm Resource, to string, fail func(string, ...any) error) error {
	var err error
	if to == "" {
		err = c.Client.SetPool(ctx, vm.Pool, vm.VMID, true)
	} else {
		err = c.Client.SetPool(ctx, to, vm.VMID, false)
	}
	if err == nil {
		return nil
	}
	err = fail("pool %q → %q: %w", vm.Pool, to, err)
	switch {
	case KindOf(err) == KindAuth:
		return provider.NewPermanentError(fmt.Errorf("%w (moving a VM between pools needs Pool.Allocate on the pools; the token sees the pool of a VM only with Pool.Audit on that pool, and without it the VM looks to be in no pool)", err))
	case strings.Contains(err.Error(), "does not exist"):
		return provider.NewPermanentError(fmt.Errorf("%w (create the pool in Proxmox VE: Sakutei does not create pools)", err))
	}
	return classify(err)
}

// oneStorage は disks が全部同じストレージならそれを返す（複製の先）。
func oneStorage(disks []Disk) string {
	st := ""
	for _, d := range disks {
		if st != "" && st != d.Storage {
			return ""
		}
		st = d.Storage
	}
	return st
}

// ciParams は cloud-init の宣言を params に入れる。空の値は消す（delete）。
// next は今の設定（再起動の後の値。ipconfig0 のほかのキーを残すため）。
func ciParams(params url.Values, want map[string]string, next map[string]string) error {
	var del []string
	set := func(key, v string) {
		if v == "" {
			if next != nil && next[key] != "" {
				del = append(del, key)
			}
			return
		}
		params.Set(key, v)
	}
	if v, ok := want[FieldCIUser]; ok {
		set("ciuser", v)
	}
	if v, ok := want[FieldCISSHKeys]; ok {
		k := CanonSSHKeys(v)
		if k != "" {
			k = encodeURIComponent(k + "\n")
		}
		set("sshkeys", k)
	}
	ip, setIP := want[FieldCIIP]
	gw, setGW := want[FieldCIGateway]
	if setIP || setGW {
		var err error
		if ip != "" {
			if ip, err = CanonIP(ip); err != nil {
				return err
			}
		}
		if gw != "" {
			if gw, err = CanonGateway(gw); err != nil {
				return err
			}
		}
		set("ipconfig0", ipconfigValue(next["ipconfig0"], ip, setIP, gw, setGW))
	}
	if v, ok := want[FieldCIDNS]; ok {
		d, err := CanonDNS(v)
		if err != nil {
			return err
		}
		set("nameserver", d)
	}
	if v, ok := want[FieldCIUpgrade]; ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("ciupgrade %q: want true or false", v)
		}
		set("ciupgrade", map[bool]string{true: "1", false: "0"}[b])
	}
	if len(del) > 0 {
		params.Set("delete", strings.Join(del, ","))
	}
	return nil
}

// update は今ある VM を want に合わせる。設定は再起動の後の値（pending を
// 含む）と比べ、違うものだけを書く（重ねて pending を作らない）。
func (p *Provider) update(ctx context.Context, c Cluster, localID string, vm Resource, want map[string]string, op core.Op) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s: Apply %q: "+format, append([]any{Name, localID}, args...)...)
	}
	if vm.Lock != "" {
		return provider.NewTransientError(fail("VM %d is locked (%s)", vm.VMID, vm.Lock))
	}
	if v, ok := want[FieldVMID]; ok && v != strconv.Itoa(vm.VMID) {
		return provider.NewPermanentError(fail("vmid %s is declared but the VM has vmid %d", v, vm.VMID))
	}
	if v, ok := want[FieldNode]; ok && v != vm.Node {
		return provider.NewPermanentError(fail("node %s is declared but the VM is on node %s", v, vm.Node))
	}
	if v, ok := want[FieldPool]; ok && v != vm.Pool {
		if err := movePool(ctx, c, vm, v, fail); err != nil {
			return err
		}
	}
	entries, err := c.Client.Pending(ctx, vm.Node, vm.VMID)
	if err != nil {
		return classify(fail("%w", err))
	}
	cur, next := splitPending(entries)
	if cur["lock"] != "" {
		return provider.NewTransientError(fail("VM %d is locked (%s)", vm.VMID, cur["lock"]))
	}
	nextState := configState(next)

	params := url.Values{}
	if v, ok := want[FieldCores]; ok {
		if m, _ := fieldMatches(FieldCores, v, nextState[FieldCores]); !m {
			params.Set("cores", strings.TrimSpace(v))
		}
	}
	if v, ok := want[FieldMemory]; ok {
		if m, _ := fieldMatches(FieldMemory, v, nextState[FieldMemory]); !m {
			mib, err := ParseMemory(v)
			if err != nil {
				return provider.NewPermanentError(fail("%w", err))
			}
			params.Set("memory", strconv.Itoa(mib))
		}
	}
	type resize struct{ slot, size string }
	var resizes []resize
	if v, ok := want[FieldDisks]; ok {
		ws, err := ParseDisks(v)
		if err != nil {
			return provider.NewPermanentError(fail("%w", err))
		}
		have, _ := parseObservedDisks(nextState[FieldDisks])
		for _, w := range ws {
			h, ok := have[w.Slot]
			switch {
			case !ok:
				if _, used := next[w.Slot]; used {
					return provider.NewPermanentError(fail("slot %s holds something that is not a disk (a CD-ROM or the cloud-init drive)", w.Slot))
				}
				params.Set(w.Slot, w.Storage+":"+strconv.FormatInt(w.Size>>30, 10))
			case h.Storage != w.Storage:
				return provider.NewPermanentError(fail("disk %s is on storage %s, not %s (moving a disk is not managed)", w.Slot, h.Storage, w.Storage))
			case w.Size < h.Size:
				return provider.NewPermanentError(fail("disk %s is %s; it is never shrunk to %s", w.Slot, FormatSize(h.Size), FormatSize(w.Size)))
			case w.Size > h.Size:
				resizes = append(resizes, resize{w.Slot, FormatSize(w.Size)})
			}
		}
		if _, ok := next["scsihw"]; !ok {
			for _, w := range ws {
				if strings.HasPrefix(w.Slot, "scsi") && params.Has(w.Slot) {
					params.Set("scsihw", "virtio-scsi-single")
				}
			}
		}
	}
	if v, ok := want[FieldNICs]; ok {
		ws, err := ParseNICs(v)
		if err != nil {
			return provider.NewPermanentError(fail("%w", err))
		}
		have := parseObservedNICs(nextState[FieldNICs])
		for _, w := range ws {
			if h, ok := have[w.Slot]; !ok || !nicMatches(w, h) {
				params.Set(w.Slot, nicConfig(w, next[w.Slot]))
			}
		}
	}
	ciWant := map[string]string{}
	for _, f := range ciFields {
		if v, ok := want[f]; ok {
			if m, _ := fieldMatches(f, v, nextState[f]); !m {
				ciWant[f] = v
			}
		}
	}
	if len(ciWant) > 0 {
		// ipconfig0 は ip と gw をまとめて書くので、片方だけ変わるときも両方の宣言を渡す。
		_, ipChanged := ciWant[FieldCIIP]
		_, gwChanged := ciWant[FieldCIGateway]
		if ipChanged || gwChanged {
			for _, f := range []string{FieldCIIP, FieldCIGateway} {
				if v, ok := want[f]; ok {
					ciWant[f] = v
				}
			}
		}
		if err := ciParams(params, ciWant, next); err != nil {
			return provider.NewPermanentError(fail("%w", err))
		}
	}
	if len(params) > 0 {
		if err := c.Client.SetConfig(ctx, vm.Node, vm.VMID, params); err != nil {
			return classify(fail("set config: %w", err))
		}
	}
	for _, r := range resizes {
		upid, err := c.Client.Resize(ctx, vm.Node, vm.VMID, r.slot, r.size)
		if err != nil {
			return classify(fail("resize %s to %s: %w", r.slot, r.size, err))
		}
		if done, err := p.waitTask(ctx, c, localID, upid); err != nil || !done {
			return err
		}
	}

	// 電源。
	switch want[FieldPower] {
	case PowerRunning:
		if vm.Status == PowerStopped {
			p.forget(localID)
			upid, err := c.Client.PowerAction(ctx, vm.Node, vm.VMID, "start", nil)
			if err != nil {
				return classify(fail("start: %w", err))
			}
			_, err = p.waitTask(ctx, c, localID, upid)
			return err
		}
	case PowerStopped:
		if vm.Status != PowerRunning {
			p.forget(localID)
			return nil
		}
		action := "shutdown"
		if op.Destructive {
			action = "stop"
		} else {
			p.mu.Lock()
			if _, sent := p.gracefulSent[localID]; !sent {
				p.gracefulSent[localID] = p.now()
			}
			p.mu.Unlock()
		}
		upid, err := c.Client.PowerAction(ctx, vm.Node, vm.VMID, action, nil)
		if err != nil {
			return classify(fail("%s: %w", action, err))
		}
		_, err = p.waitTask(ctx, c, localID, upid)
		return err
	}

	// 再起動を待つ変更の反映（宣言したときだけ。RefinePlan が要ると決めた）。
	if want[FieldApplyPending] == ApplyPendingReboot && vm.Status == PowerRunning && want[FieldPower] != PowerStopped {
		entries, err := c.Client.Pending(ctx, vm.Node, vm.VMID)
		if err != nil {
			return classify(fail("%w", err))
		}
		cur, next := splitPending(entries)
		now, after := configState(cur), configState(next)
		pending := false
		for _, f := range configFields {
			pending = pending || now[f] != after[f]
		}
		if pending {
			upid, err := c.Client.PowerAction(ctx, vm.Node, vm.VMID, "reboot", nil)
			if err != nil {
				return classify(fail("reboot (apply_pending): %w", err))
			}
			_, err = p.waitTask(ctx, c, localID, upid)
			return err
		}
	}
	return nil
}

// destroy は VM を消す。走っていれば先に止める（Delete は破壊的として予算を
// 通っている）。
func (p *Provider) destroy(ctx context.Context, c Cluster, localID string, vm Resource) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s: Apply %q: delete: "+format, append([]any{Name, localID}, args...)...)
	}
	if vm.Lock != "" {
		return provider.NewTransientError(fail("VM %d is locked (%s)", vm.VMID, vm.Lock))
	}
	if vm.Status == PowerRunning {
		upid, err := c.Client.PowerAction(ctx, vm.Node, vm.VMID, "stop", nil)
		if err != nil {
			return classify(fail("stop: %w", err))
		}
		if done, err := p.waitTask(ctx, c, localID, upid); err != nil || !done {
			return err
		}
	}
	upid, err := c.Client.Destroy(ctx, vm.Node, vm.VMID)
	if err != nil {
		return classify(fail("%w", err))
	}
	p.forget(localID)
	_, err = p.waitTask(ctx, c, localID, upid)
	return err
}

// waitTask は upid の仕事の終わりを c.TaskWait まで待つ。終われば true（失敗で
// 終われば error）。終わらなければ UPID を記録して false（その間 Read は
// Converging）。upid が空（同期で終わった）なら true。
func (p *Provider) waitTask(ctx context.Context, c Cluster, localID, upid string) (bool, error) {
	if upid == "" {
		return true, nil
	}
	deadline := time.Now().Add(c.TaskWait)
	for {
		st, err := c.Client.Task(ctx, upid)
		if err == nil && !st.Running {
			if !st.OK() {
				return false, provider.NewTransientError(fmt.Errorf("%s: Apply %q: task %s failed: %s", Name, localID, upid, st.ExitStatus))
			}
			return true, nil
		}
		if err != nil || !time.Now().Add(p.poll).Before(deadline) {
			p.track(localID, upid)
			return false, nil
		}
		select {
		case <-ctx.Done():
			p.track(localID, upid)
			return false, nil
		case <-time.After(p.poll):
		}
	}
}

// ---- 一度きりの操作（provider.Actioner） ----

// PlanAction は op と args を現実を見ずに検査する。reboot は正常の再起動
// （pending の変更が効く）、reset は強制の再起動で破壊的（予算 §13 に数える）。
// どちらも止まっている VM には start を送る。
func (p *Provider) PlanAction(localID, op string, args map[string]string) (provider.ActionPlan, error) {
	_, c, vm, err := p.coords(localID)
	if err != nil {
		return provider.ActionPlan{}, fmt.Errorf("%s: %w", Name, err)
	}
	if contains(c.ProtectedNames, vm) {
		return provider.ActionPlan{}, fmt.Errorf("%s %q: the VM is protected: Sakutei never touches it", Name, localID)
	}
	if len(args) > 0 {
		return provider.ActionPlan{}, fmt.Errorf("%s %q: action %s takes no arguments", Name, localID, op)
	}
	switch op {
	case ActionReboot:
		return provider.ActionPlan{Description: "reboot (start if stopped)"}, nil
	case ActionReset:
		return provider.ActionPlan{Destructive: true, Description: "reset (start if stopped; destructive)"}, nil
	}
	return provider.ActionPlan{}, fmt.Errorf("%s %q: unknown action %q (want %s or %s)", Name, localID, op, ActionReboot, ActionReset)
}

// Action は op を 1 回だけ施す。送れないと分かれば ErrActionNotSent。
func (p *Provider) Action(ctx context.Context, localID, op string, args map[string]string) error {
	if _, err := p.PlanAction(localID, op, args); err != nil {
		return provider.NewPermanentError(err)
	}
	_, c, vm, _ := p.coords(localID)
	notSent := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s: action %s on %q: "+format, append([]any{provider.ErrActionNotSent, Name, op, localID}, a...)...)
	}
	res, err := c.resources(ctx)
	if err != nil {
		return notSent("%v", err)
	}
	found := findVM(res, vm)
	if len(found) != 1 {
		return provider.NewPermanentError(fmt.Errorf("%s: action %s on %q: %d VMs have this name", Name, op, localID, len(found)))
	}
	r := found[0]
	if c.protected(r.VMID, r.Name) || r.Template {
		return provider.NewPermanentError(fmt.Errorf("%s: action %s on %q: VM %d is protected or a template", Name, op, localID, r.VMID))
	}
	if err := c.powerOf(ctx, &r); err != nil {
		return notSent("%v", err)
	}
	if r.Lock != "" {
		return notSent("VM %d is locked (%s)", r.VMID, r.Lock)
	}
	action := op
	switch r.Status {
	case PowerStopped:
		action = "start"
	case PowerRunning:
	default:
		return notSent("VM %d has status %q", r.VMID, r.Status)
	}
	upid, err := c.Client.PowerAction(ctx, r.Node, r.VMID, action, nil)
	if err != nil {
		return classify(fmt.Errorf("%s: action %s on %q: %w", Name, op, localID, err))
	}
	_, err = p.waitTask(ctx, c, localID, upid)
	return err
}

// classify は Client のエラーを provider のエラー型にする。届かない・5xx・
// 時間切れは一時的、認証・権限・拒まれた要求は設定を直すまで治らない。
func classify(err error) error {
	if err == nil {
		return nil
	}
	if KindOf(err) == KindUnreachable {
		return provider.NewTransientError(err)
	}
	return provider.NewPermanentError(err)
}
