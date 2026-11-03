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

package proxmox

// vm.go — 種類 vm（E-D1・N-D1）。宣言した項目だけを比べ、書かないものは触らない。
//
//   - vmid（数）：作るときに使う（宣言するか採番で配る）。今の VM と違えば Apply の
//     誤り（番号を変えない）。node：省けばクラスタの node。今の VM と違えば Apply の
//     誤り（移行は扱わない）。
//   - template（名前か vmid）：作るときだけ使う full clone の元。観測せず、在る VM
//     とは比べない（FieldSpec.Match）。
//   - pool：作るときに渡し、違えば移す（"" は pool から外す）。pool は作らない。
//   - cores（数）・memory（"2048"＝MiB・"2048M"・"2G"）。
//   - disks："<slot> storage=<storage> size=<大きさ> [keep]" か "<slot> volume=<storage>:<volume>
//     [size=<大きさ>] [keep]" を "," でつなぐ。宣言した slot だけを比べる。大きく
//     するのはよい（resize）。小さくする・ストレージを変えるのは Apply の誤り。keep は
//     消さない印（VM の tags の sakutei-keep-<slot>）。volume= は slot が空のときに
//     既にある volume を付ける（ほかの VM のものなら Proxmox の reassign で付け替え
//     る。付いたあとは volume の名前を比べない）。付け替えは承認の要る壊す変更
//     （Classify、E-C2・N-C12・N-D10）。
//   - nics："<slot> bridge=<bridge> [vlan=<tag>] [mac=..] [model=..] [firewall=0|1]
//     [mtu=..]"。宣言した slot だけを、bridge と vlan はいつも、ほかは書いたときだけ
//     比べる。
//   - firewall：VM の FW の options のうち書いたキーだけ（enable・policy_in・
//     policy_out・ipfilter・macfilter）。
//   - ci_user・ci_ssh_keys・ci_ip・ci_gateway・ci_dns・ciupgrade（真偽）：cloud-init。
//     ciupgrade は書いたときだけ渡す（Proxmox の既定は true）。
//   - power：running・stopped（止めるのは正常停止 shutdown。強制の停止は Invoker の
//     stop、Procedure から）。
//   - apply_pending：reboot・never（既定）。reboot なら、再起動を待つ変更（pending）が
//     あれば走っている VM を 1 回再起動する。観測は「待つ変更が無い（か止まって
//     いる）」なら reboot、あれば never。
//
// 観測の値は VM の設定が言う値（pending があればその値）。宣言どおりの値が pending に
// あれば差分にならず、重ねて書かない。観測だけの項目：cluster・name・lock・
// pending（再起動を待つ項目の名前）・current_values（それらの今効いている値、JSON）。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
)

// 項目の名前。
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
	FieldFirewall      = "firewall"
	FieldCIUser        = "ci_user"
	FieldCISSHKeys     = "ci_ssh_keys"
	FieldCIIP          = "ci_ip"
	FieldCIGateway     = "ci_gateway"
	FieldCIDNS         = "ci_dns"
	FieldCIUpgrade     = "ciupgrade"
	FieldPower         = "power"
	FieldApplyPending  = "apply_pending"
	FieldLock          = "lock"
	FieldPending       = "pending"
	FieldCurrentValues = "current_values"
	FieldVolume        = "volume"
)

// 値。
const (
	PowerRunning       = "running"
	PowerStopped       = "stopped"
	ApplyPendingReboot = "reboot"
	ApplyPendingNever  = "never"
)

// 一回きりの操作（Invoker、Procedure から、E-D1・N-D4）。
const (
	OpReboot = "reboot" // 正常な再起動（pending の変更が効く）。止まっていれば起動
	OpReset  = "reset"  // 強制の再起動（壊す）。止まっていれば起動
	OpStop   = "stop"   // 強制の停止（壊す。正常停止で止まらない VM に）
)

// configFields は VM の設定から観測し、pending を比べる項目。
var configFields = []string{FieldCores, FieldMemory, FieldDisks, FieldNICs, FieldCIUser, FieldCISSHKeys, FieldCIIP, FieldCIGateway, FieldCIDNS, FieldCIUpgrade}

var ciFields = []string{FieldCIUser, FieldCISSHKeys, FieldCIIP, FieldCIGateway, FieldCIDNS, FieldCIUpgrade}

// declarableVM は宣言できる項目（ほかは観測だけ）。
var declarableVM = []string{FieldCluster, FieldName, FieldVMID, FieldNode, FieldTemplate, FieldPool, FieldCores, FieldMemory, FieldDisks, FieldNICs, FieldFirewall,
	FieldCIUser, FieldCISSHKeys, FieldCIIP, FieldCIGateway, FieldCIDNS, FieldCIUpgrade, FieldPower, FieldApplyPending}

// poolName は pool の名前（Proxmox の決まり。入れ子は "/" で 3 段まで）。
var poolName = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+){0,2}$`)

// vmFields は種類 vm の項目。等しさで比べられないもの（書き方の違い・宣言した
// slot・キーだけ・作るときだけ）は Match で比べる（E-I7・E-D1）。
func vmFields() map[string]core.FieldSpec {
	str := core.FieldSpec{Type: core.String}
	match := func(field string) core.FieldSpec {
		return core.FieldSpec{Type: core.String, Match: func(d, o any) bool {
			ds, ok1 := d.(string)
			os, ok2 := o.(string)
			if !ok1 || !ok2 {
				return false
			}
			ok, _ := fieldMatches(field, ds, os)
			return ok
		}}
	}
	return map[string]core.FieldSpec{
		FieldCluster:   str,
		FieldName:      str,
		FieldVMID:      {Type: core.Int},
		FieldNode:      str,
		FieldTemplate:  {Type: core.String, Match: func(any, any) bool { return true }}, // 作るときだけ（E-D1）
		FieldPool:      str,
		FieldCores:     {Type: core.Int},
		FieldMemory:    match(FieldMemory),
		FieldDisks:     match(FieldDisks),
		FieldNICs:      match(FieldNICs),
		FieldFirewall:  match(FieldFirewall),
		FieldCIUser:    str,
		FieldCISSHKeys: match(FieldCISSHKeys),
		FieldCIIP:      match(FieldCIIP),
		FieldCIGateway: match(FieldCIGateway),
		FieldCIDNS:     match(FieldCIDNS),
		FieldCIUpgrade: {Type: core.Bool},
		FieldPower:     str,
		FieldApplyPending: {Type: core.String, Match: func(d, o any) bool {
			return d == ApplyPendingNever || d == o
		}},
		FieldLock:          str,
		FieldPending:       {Type: core.List},
		FieldCurrentValues: str,
	}
}

// validateVM は宣言を確かめる（現実に触れない、E-I7）。
func validateVM(c *Cluster, id core.ID, f core.Fields) []string {
	var msgs []string
	bad := func(format string, a ...any) { msgs = append(msgs, fmt.Sprintf(format, a...)) }
	_, vm, _ := split(id)
	for _, k := range sortedKeys(f) {
		if !contains(declarableVM, k) {
			bad("項目 %q は観測だけで宣言できない（宣言できる項目: %s）", k, strings.Join(declarableVM, ", "))
		}
	}
	s := func(k string) (string, bool) {
		v, ok := f[k].(string)
		return v, ok
	}
	if v, ok := s(FieldCluster); ok && v != c.Name {
		bad("cluster %q は id のクラスタ %s と同じにする", v, c.Name)
	}
	if v, ok := s(FieldName); ok && v != vm {
		bad("name %q は id の VM 名 %s と同じにする", v, vm)
	}
	if v, ok := f[FieldVMID].(int64); ok && (v < 100 || v > 999999999) {
		bad("vmid %d は 100〜999999999", v)
	}
	if v, ok := s(FieldNode); ok && strings.TrimSpace(v) == "" {
		bad("node が空")
	}
	if _, ok := f[FieldNode]; !ok && c.Node == "" {
		bad("node を宣言していず、クラスタ %s に node が無い（settings.clusters.%s.node を書くか宣言する）", c.Name, c.Name)
	}
	if v, ok := s(FieldTemplate); ok && strings.TrimSpace(v) == "" {
		bad("template が空")
	}
	if v, ok := s(FieldPool); ok && v != "" && !poolName.MatchString(v) {
		bad("pool %q は Proxmox の pool の名前（英数字・'.'・'_'・'-'。入れ子は 親/子）か、pool から外す \"\"", v)
	}
	if v, ok := f[FieldCores].(int64); ok && v < 1 {
		bad("cores %d は正の数", v)
	}
	checks := []struct {
		field string
		check func(string) error
	}{
		{FieldMemory, func(v string) error { _, err := ParseMemory(v); return err }},
		{FieldDisks, func(v string) error { _, err := ParseDisks(v); return err }},
		{FieldNICs, func(v string) error { _, err := ParseNICs(v); return err }},
		{FieldFirewall, func(v string) error { _, err := ParseFirewall(v); return err }},
		{FieldCIIP, func(v string) error {
			if v == "" {
				return nil
			}
			_, err := CanonIP(v)
			return err
		}},
		{FieldCIGateway, func(v string) error {
			if v == "" {
				return nil
			}
			_, err := CanonGateway(v)
			return err
		}},
		{FieldCIDNS, func(v string) error { _, err := CanonDNS(v); return err }},
		{FieldCISSHKeys, CheckSSHKeys},
	}
	for _, ch := range checks {
		if v, ok := s(ch.field); ok {
			if err := ch.check(v); err != nil {
				bad("%v", err)
			}
		}
	}
	if v, ok := s(FieldCIUser); ok && strings.ContainsAny(v, " \t\n:") {
		bad("ci_user %q に空白と ':' は書けない", v)
	}
	if v, ok := s(FieldPower); ok && v != PowerRunning && v != PowerStopped {
		bad("power %q は %s か %s（再起動・強制の停止は一回きりの操作：apply_pending: reboot か、手順の invoke %s・%s・%s）", v, PowerRunning, PowerStopped, OpReboot, OpReset, OpStop)
	}
	if v, ok := s(FieldApplyPending); ok && v != ApplyPendingReboot && v != ApplyPendingNever {
		bad("apply_pending %q は %s か %s", v, ApplyPendingReboot, ApplyPendingNever)
	}
	return msgs
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
	keep := keepSlots(cfg)
	for i := range disks {
		disks[i].Keep = keep[disks[i].Slot]
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
	out[FieldCIUpgrade] = strconv.FormatBool(strings.TrimSpace(cfg["ciupgrade"]) != "0")
	return out
}

// pendingFields は再起動を待つ項目の名前（configFields の順）。
func pendingFields(cur, next map[string]string) []string {
	now, after := configState(cur), configState(next)
	var names []string
	for _, f := range configFields {
		if after[f] != now[f] {
			names = append(names, f)
		}
	}
	return names
}

// observeVM は VM を観測する（Present。電源と lock は lookupVM が読んだ）。
func observeVM(ctx context.Context, c *Cluster, cl *Client, r *Resource) core.Observation {
	entries, err := cl.Pending(ctx, r.Node, r.VMID)
	if err != nil {
		return unknown(err)
	}
	cur, next := splitPending(entries)
	if cur["lock"] != "" {
		return core.Observation{Availability: core.Converging, Note: fmt.Sprintf("VM %d に lock %s がある", r.VMID, cur["lock"])}
	}
	fw, err := cl.FirewallOptions(ctx, r.Node, r.VMID)
	if err != nil {
		return unknown(fmt.Errorf("FW の options: %w", err))
	}
	now, after := configState(cur), configState(next)
	f := core.Fields{
		FieldCluster: c.Name, FieldName: r.Name, FieldVMID: int64(r.VMID), FieldNode: r.Node, FieldPool: r.Pool,
		FieldPower: r.Status, FieldLock: "", FieldFirewall: observedFirewall(fw),
	}
	current := map[string]string{}
	pending := []any{}
	for _, k := range configFields {
		switch k {
		case FieldCores:
			n, _ := strconv.ParseInt(after[k], 10, 64)
			f[k] = n
		case FieldCIUpgrade:
			f[k] = after[k] == "true"
		default:
			f[k] = after[k]
		}
		if after[k] != now[k] {
			current[k] = now[k]
			pending = append(pending, k)
		}
	}
	f[FieldPending] = pending
	f[FieldCurrentValues] = ""
	if len(current) > 0 {
		data, _ := json.Marshal(current)
		f[FieldCurrentValues] = string(data)
	}
	f[FieldApplyPending] = ApplyPendingReboot
	if len(pending) > 0 && r.Status == PowerRunning {
		f[FieldApplyPending] = ApplyPendingNever
	}
	return core.Observation{Availability: core.Present, Fields: f}
}

// observeVolume は VM の付け先の無い volume（unusedN）を観測する。
func observeVolume(ctx context.Context, c *Cluster, cl *Client, r *Resource, key string) core.Observation {
	if !unusedKeyRe.MatchString(key) {
		return core.Observation{Availability: core.Unknown, Note: fmt.Sprintf("%q は unusedN でない", key)}
	}
	cfg, err := cl.Config(ctx, r.Node, r.VMID)
	if err != nil {
		return unknown(err)
	}
	vol, ok := unusedVolumes(cfg)[key]
	if !ok {
		return core.Observation{Availability: core.Absent}
	}
	return core.Observation{Availability: core.Present, Fields: core.Fields{
		FieldCluster: c.Name, FieldName: r.Name, FieldVMID: int64(r.VMID), FieldNode: r.Node, FieldVolume: vol,
	}}
}

// fieldMatches は宣言の値 want が観測の正規形 have と同じかを返す。宣言どおりに
// できない違い（ディスクを小さくする・ストレージを変える）は error（Apply が断る）。
func fieldMatches(field, want, have string) (bool, error) {
	switch field {
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
				return false, fmt.Errorf("ディスク %s はストレージ %s にあり、宣言は %s。ディスクを動かすのは扱わない（手で動かすか storage=%s と宣言する）", w.Slot, h.Storage, w.Storage, h.Storage)
			}
			if w.Size > 0 && w.Size < h.Size {
				return false, fmt.Errorf("ディスク %s は %s で、宣言は %s。ディスクは小さくしない（size=%s 以上を宣言する）", w.Slot, FormatSize(h.Size), FormatSize(w.Size), FormatSize(h.Size))
			}
			// volume= は slot が空のときに付けるものなので、付いたあとは比べない
			// （付け替えで Proxmox が volume の名前を変える、E-C2）。
			if w.Size > h.Size || w.Keep != h.Keep {
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
	case FieldFirewall:
		ws, err := ParseFirewall(want)
		return err == nil && len(firewallDiff(ws, have)) == 0, nil
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

// nicMatches は宣言の NIC w が観測 h と同じか（mac・model・firewall・mtu は
// 書いたときだけ。観測の firewall の "" は 0）。
func nicMatches(w, h NIC) bool {
	hfw := h.Firewall
	if hfw == "" {
		hfw = "0"
	}
	return w.Bridge == h.Bridge && w.VLAN == h.VLAN &&
		(w.MAC == "" || w.MAC == h.MAC) && (w.Model == "" || w.Model == h.Model) &&
		(w.Firewall == "" || w.Firewall == hfw) && (w.MTU == 0 || w.MTU == h.MTU)
}

// parseObservedDisks は観測の正規形の disks を slot → Disk にする。
func parseObservedDisks(s string) (map[string]Disk, error) {
	out := map[string]Disk{}
	for _, e := range splitEntries(s) {
		slot, kv, keep, err := diskEntry(e, "storage", "size", "volume")
		if err != nil {
			return nil, err
		}
		size, _ := ParseSize(kv["size"])
		out[slot] = Disk{Slot: slot, Storage: kv["storage"], Size: size, Volume: kv["volume"], Keep: keep}
	}
	return out, nil
}

// parseObservedNICs は観測の正規形の nics を slot → NIC にする。
func parseObservedNICs(s string) map[string]NIC {
	out := map[string]NIC{}
	for _, e := range splitEntries(s) {
		slot, kv, err := entryKV(e, nicKeys...)
		if err != nil {
			continue
		}
		n := NIC{Slot: slot, Bridge: kv["bridge"], MAC: kv["mac"], Model: kv["model"], Firewall: kv["firewall"]}
		n.VLAN, _ = strconv.Atoi(kv["vlan"])
		n.MTU, _ = strconv.Atoi(kv["mtu"])
		out[slot] = n
	}
	return out
}

// want は宣言の値を Proxmox に渡す文字列にする（数は 10 進、真偽は true・false）。
func wantOf(desired core.Fields) map[string]string {
	out := map[string]string{}
	for k, v := range desired {
		switch x := v.(type) {
		case string:
			out[k] = x
		case int64:
			out[k] = strconv.FormatInt(x, 10)
		case bool:
			out[k] = strconv.FormatBool(x)
		default:
			out[k] = fmt.Sprint(x)
		}
	}
	return out
}

// Classify は既にある volume を付ける（ほかの VM から付け替える）変更を、承認の
// 要る壊す変更と言う（§3.3 の Classifier、E-C2・N-C12・N-D10）。宣言の volume= の
// slot が観測に無いときだけ（付いたあとは要らない）。fw-rules の変更は壊す変更
// （E-B6）。現実に触れない。
func (p *Provider) Classify(id core.ID, current core.Observation, desired core.Fields) []core.Hint {
	switch id.Kind() {
	case KindFWRules:
		return []core.Hint{{Field: FieldRules, Destructive: true, Reason: "VM の FW の規則を置く・変える・外す（FW の変更として壊す変更に数える）"}}
	case KindVM:
	default:
		return nil
	}
	v, ok := desired[FieldDisks].(string)
	if !ok {
		return nil
	}
	ws, err := ParseDisks(v)
	if err != nil {
		return nil
	}
	have := map[string]Disk{}
	if s, ok := current.Fields[FieldDisks].(string); ok && current.Availability == core.Present {
		have, _ = parseObservedDisks(s)
	}
	var vols []string
	for _, w := range ws {
		if _, attached := have[w.Slot]; w.Volume != "" && !attached {
			vols = append(vols, w.Slot+"="+w.Volume)
		}
	}
	if len(vols) == 0 {
		return nil
	}
	return []core.Hint{{Field: FieldDisks, Destructive: true, NeedsApproval: true,
		Reason: fmt.Sprintf("既にある volume %s を付ける（持ち主の VM から外して付け替える。volume は消さない）", strings.Join(vols, ", "))}}
}

// applyVM は VM を宣言に合わせる。無ければ作り、今の現実を読み直してずれて
// いるものだけを変える（冪等）。
func (p *Provider) applyVM(ctx context.Context, c *Cluster, cl *Client, vm string, desired core.Fields) error {
	want := wantOf(desired)
	res, err := c.resources(ctx, cl)
	if err != nil {
		return err
	}
	found := findVM(res, vm)
	if len(found) > 1 {
		return fmt.Errorf("名前 %s の VM が %d 台ある", vm, len(found))
	}
	created := false
	if len(found) == 0 {
		if err := p.create(ctx, c, cl, vm, want, res); err != nil {
			return err
		}
		if res, err = c.resources(ctx, cl); err != nil {
			return err
		}
		if found = findVM(res, vm); len(found) != 1 {
			return fmt.Errorf("作った VM %s が一覧に無い（次の周に続ける）", vm)
		}
		created = true
	}
	cur := found[0]
	if cur.Template {
		return fmt.Errorf("VM %d はテンプレートで、VM でない", cur.VMID)
	}
	if err := c.powerOf(ctx, cl, &cur); err != nil {
		return err
	}
	if cur.Status != PowerRunning && cur.Status != PowerStopped {
		return fmt.Errorf("VM %d の status が %q", cur.VMID, cur.Status)
	}
	if err := p.update(ctx, c, cl, cur, want, res); err != nil {
		if created {
			// 途中で落ちた：どこまで進んだかを知らせる（次の周は変える操作で続ける）。
			return fmt.Errorf("%w（VM %d は作った。残りは次の周に続ける）", err, cur.VMID)
		}
		return err
	}
	return nil
}

// create は VM を作る（テンプレートの複製か、空から）。仕事が task_wait のうちに
// 終わらなければ誤りでなく戻る（仕事が走る間は Converging）。
func (p *Provider) create(ctx context.Context, c *Cluster, cl *Client, vm string, want map[string]string, res []Resource) error {
	fail := func(format string, args ...any) error { return fmt.Errorf("作る: "+format, args...) }
	node := want[FieldNode]
	if node == "" {
		node = c.Node
	}
	if node == "" {
		return fail("node を宣言していず、クラスタに node が無い")
	}
	if want[FieldVMID] == "" {
		return fail("vmid を宣言していない（宣言するか、採番で配る：vmid: {from_pool: <プール>}）")
	}
	vmid, err := strconv.Atoi(want[FieldVMID])
	if err != nil {
		return fail("vmid %q が数でない", want[FieldVMID])
	}
	for _, r := range res {
		if r.VMID == vmid {
			return fail("vmid %d はもう %s %q が使っている", vmid, r.Type, r.Name)
		}
	}
	var disks []Disk
	if v, ok := want[FieldDisks]; ok {
		if disks, err = ParseDisks(v); err != nil {
			return fail("%w", err)
		}
	}
	pool := want[FieldPool]
	var upid string
	if tmpl := want[FieldTemplate]; tmpl != "" {
		var src *Resource
		for i, r := range res {
			if r.Type == "qemu" && (r.Name == tmpl || strconv.Itoa(r.VMID) == tmpl) {
				if src != nil {
					return fail("template %q に当たる VM が 2 つ以上ある", tmpl)
				}
				src = &res[i]
			}
		}
		if src == nil {
			return fail("template %q がクラスタに無い（名前か vmid で書く）", tmpl)
		}
		if !src.Template {
			// template の印も pvestatd の写し。設定で確かめる。
			cfg, err := cl.Config(ctx, src.Node, src.VMID)
			if err != nil {
				return fail("template %q: %w", tmpl, err)
			}
			if cfg["template"] != "1" {
				return fail("%q（VM %d）はテンプレートでない", tmpl, src.VMID)
			}
		}
		params := url.Values{"newid": {strconv.Itoa(vmid)}, "name": {vm}, "target": {node}, "full": {"1"}}
		if st := oneStorage(newDisks(disks)); st != "" {
			params.Set("storage", st)
		}
		if pool != "" {
			params.Set("pool", pool)
		}
		if upid, err = cl.Clone(ctx, src.Node, src.VMID, params); err != nil {
			return createError(fail("複製 %d: %w", src.VMID, err), vmid, pool)
		}
	} else {
		params := url.Values{"vmid": {strconv.Itoa(vmid)}, "name": {vm}}
		if pool != "" {
			params.Set("pool", pool)
		}
		if v, ok := want[FieldCores]; ok {
			params.Set("cores", v)
		}
		if v, ok := want[FieldMemory]; ok {
			m, err := ParseMemory(v)
			if err != nil {
				return fail("%w", err)
			}
			params.Set("memory", strconv.Itoa(m))
		}
		scsi := false
		for _, d := range disks {
			// volume= のディスクは作ったあとに付ける（update、E-C2）。
			if d.Volume == "" {
				params.Set(d.Slot, d.Storage+":"+strconv.FormatInt(d.Size>>30, 10))
			}
			scsi = scsi || strings.HasPrefix(d.Slot, "scsi")
		}
		if scsi {
			params.Set("scsihw", "virtio-scsi-single")
		}
		if v, ok := want[FieldNICs]; ok {
			nics, err := ParseNICs(v)
			if err != nil {
				return fail("%w", err)
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
				return fail("cloud-init のドライブにはストレージが要る：disks を宣言する（ドライブは最初のディスクのストレージに置く）")
			}
			params.Set("ide2", disks[0].Storage+":cloudinit")
			if err := ciParams(params, want, nil); err != nil {
				return fail("%w", err)
			}
		}
		if upid, err = cl.Create(ctx, node, params); err != nil {
			return createError(fail("%w", err), vmid, pool)
		}
	}
	return p.waitTask(ctx, c, cl, upid)
}

// createError は作る・複製するときの失敗に、直し方を足す。pool が無い（Proxmox は
// 403 で断る）・pool に権限が無いことと、見えない VM がその vmid を使っている
// こと（pool に絞ったトークンには pool の外の VM が見えない）。
func createError(err error, vmid int, pool string) error {
	switch {
	case pool != "" && KindOf(err) == KindAuth:
		return fmt.Errorf("%w（pool %s は Proxmox VE に在ること（Sakutei は pool を作らない）、トークンに /pool/%s の VM.Allocate が要る）", err, pool, pool)
	case strings.Contains(err.Error(), "already exists"):
		return fmt.Errorf("%w（vmid %d はこのトークンに見えない VM（トークンの pool の外など）が使っている：別の vmid を宣言するか、その VM を pool に戻す）", err, vmid)
	}
	return err
}

// movePool は VM を pool に移す（to が "" なら今の pool から外す）。
func movePool(ctx context.Context, cl *Client, vm Resource, to string) error {
	var err error
	if to == "" {
		err = cl.SetPool(ctx, vm.Pool, vm.VMID, true)
	} else {
		err = cl.SetPool(ctx, to, vm.VMID, false)
	}
	if err == nil {
		return nil
	}
	err = fmt.Errorf("pool %q → %q: %w", vm.Pool, to, err)
	switch {
	case KindOf(err) == KindAuth:
		return fmt.Errorf("%w（pool の間で VM を動かすには pool の Pool.Allocate が要る。pool の Pool.Audit が無いトークンには VM が pool に無いように見える）", err)
	case strings.Contains(err.Error(), "does not exist"):
		return fmt.Errorf("%w（pool を Proxmox VE で作る：Sakutei は pool を作らない）", err)
	}
	return err
}

// newDisks は disks のうち新しく切り出すもの（volume= でないもの）。
func newDisks(disks []Disk) []Disk {
	var out []Disk
	for _, d := range disks {
		if d.Volume == "" {
			out = append(out, d)
		}
	}
	return out
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
			return fmt.Errorf("ciupgrade %q は true か false", v)
		}
		set("ciupgrade", map[bool]string{true: "1", false: "0"}[b])
	}
	if len(del) > 0 {
		if prev := params.Get("delete"); prev != "" {
			del = append([]string{prev}, del...)
		}
		params.Set("delete", strings.Join(del, ","))
	}
	return nil
}

// update は今ある VM を want に合わせる。設定は再起動の後の値（pending を含む）と
// 比べ、違うものだけを書く（重ねて pending を作らない、E-D1）。
func (p *Provider) update(ctx context.Context, c *Cluster, cl *Client, vm Resource, want map[string]string, res []Resource) error {
	if vm.Lock != "" {
		return fmt.Errorf("VM %d に lock %s がある", vm.VMID, vm.Lock)
	}
	if v, ok := want[FieldVMID]; ok && v != strconv.Itoa(vm.VMID) {
		return fmt.Errorf("vmid %s を宣言したが、VM の vmid は %d。VM の番号は変えない（vmid を直すか作り直す）", v, vm.VMID)
	}
	if v, ok := want[FieldNode]; ok && v != vm.Node {
		return fmt.Errorf("node %s を宣言したが、VM はノード %s にある。移行は扱わない（手で移すか node: %s と宣言する）", v, vm.Node, vm.Node)
	}
	if v, ok := want[FieldPool]; ok && v != vm.Pool {
		if err := movePool(ctx, cl, vm, v); err != nil {
			return err
		}
	}
	entries, err := cl.Pending(ctx, vm.Node, vm.VMID)
	if err != nil {
		return err
	}
	cur, next := splitPending(entries)
	if cur["lock"] != "" {
		return fmt.Errorf("VM %d に lock %s がある", vm.VMID, cur["lock"])
	}
	nextState := configState(next)

	params := url.Values{}
	if v, ok := want[FieldCores]; ok && v != nextState[FieldCores] {
		params.Set("cores", v)
	}
	if v, ok := want[FieldMemory]; ok {
		if m, _ := fieldMatches(FieldMemory, v, nextState[FieldMemory]); !m {
			mib, err := ParseMemory(v)
			if err != nil {
				return err
			}
			params.Set("memory", strconv.Itoa(mib))
		}
	}
	type resize struct{ slot, size string }
	var resizes []resize
	type attach struct{ slot, volume string }
	var attaches []attach
	if v, ok := want[FieldDisks]; ok {
		ws, err := ParseDisks(v)
		if err != nil {
			return err
		}
		have, _ := parseObservedDisks(nextState[FieldDisks])
		for _, w := range ws {
			h, ok := have[w.Slot]
			switch {
			case !ok:
				if _, used := next[w.Slot]; used {
					return fmt.Errorf("slot %s にディスクでないもの（CD-ROM・cloud-init のドライブ）がある", w.Slot)
				}
				if w.Volume != "" {
					attaches = append(attaches, attach{w.Slot, w.Volume})
					continue
				}
				params.Set(w.Slot, w.Storage+":"+strconv.FormatInt(w.Size>>30, 10))
			case h.Storage != w.Storage:
				return fmt.Errorf("ディスク %s はストレージ %s にあり、%s でない（ディスクを動かすのは扱わない）", w.Slot, h.Storage, w.Storage)
			case w.Size > 0 && w.Size < h.Size:
				return fmt.Errorf("ディスク %s は %s で、%s に小さくはしない", w.Slot, FormatSize(h.Size), FormatSize(w.Size))
			case w.Size > h.Size:
				resizes = append(resizes, resize{w.Slot, FormatSize(w.Size)})
			}
		}
		// keep の印（E-C2）：宣言した slot の印だけを VM の tags で合わせる。
		if tags, changed := keepTagsFor(next["tags"], ws); changed {
			if tags == "" {
				params.Add("delete", "tags")
			} else {
				params.Set("tags", tags)
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
			return err
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
			return err
		}
	}
	if len(params) > 0 {
		if err := cl.SetConfig(ctx, vm.Node, vm.VMID, params); err != nil {
			return fmt.Errorf("設定を書く: %w", err)
		}
	}
	if v, ok := want[FieldFirewall]; ok {
		ws, err := ParseFirewall(v)
		if err != nil {
			return err
		}
		fw, err := cl.FirewallOptions(ctx, vm.Node, vm.VMID)
		if err != nil {
			return fmt.Errorf("FW の options: %w", err)
		}
		if d := firewallDiff(ws, observedFirewall(fw)); len(d) > 0 {
			fp := url.Values{}
			for k, x := range d {
				fp.Set(k, x)
			}
			if err := cl.SetFirewallOptions(ctx, vm.Node, vm.VMID, fp); err != nil {
				err = fmt.Errorf("FW の options を書く: %w", err)
				if KindOf(err) == KindAuth {
					return fmt.Errorf("%w（トークンに VM の VM.Config.Network が要る）", err)
				}
				return err
			}
		}
	}
	for _, a := range attaches {
		if err := p.attachVolume(ctx, c, cl, vm, a.slot, a.volume, next, res); err != nil {
			return err
		}
	}
	for _, r := range resizes {
		upid, err := cl.Resize(ctx, vm.Node, vm.VMID, r.slot, r.size)
		if err != nil {
			return fmt.Errorf("%s を %s に大きくする: %w", r.slot, r.size, err)
		}
		if err := p.waitTask(ctx, c, cl, upid); err != nil {
			return err
		}
	}

	// 電源。
	switch want[FieldPower] {
	case PowerRunning:
		if vm.Status == PowerStopped {
			return p.power(ctx, c, cl, vm, "start")
		}
	case PowerStopped:
		if vm.Status == PowerRunning {
			return p.power(ctx, c, cl, vm, "shutdown")
		}
		return nil
	}

	// 再起動を待つ変更の反映（宣言したときだけ）。
	if want[FieldApplyPending] == ApplyPendingReboot && vm.Status == PowerRunning {
		entries, err := cl.Pending(ctx, vm.Node, vm.VMID)
		if err != nil {
			return err
		}
		if len(pendingFields(splitPending(entries))) > 0 {
			return p.power(ctx, c, cl, vm, "reboot")
		}
	}
	return nil
}

// power は電源の操作を送り、仕事を待つ。
func (p *Provider) power(ctx context.Context, c *Cluster, cl *Client, vm Resource, action string) error {
	upid, err := cl.PowerAction(ctx, vm.Node, vm.VMID, action, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	return p.waitTask(ctx, c, cl, upid)
}

// destroy は VM を消す（ensure: absent、壊す変更として予算と承認を通っている）。
// 走っていれば先に止める（stop。ディスクごと消すので正常停止は待たない）。
func (p *Provider) destroy(ctx context.Context, c *Cluster, cl *Client, vm string) error {
	res, err := c.resources(ctx, cl)
	if err != nil {
		return err
	}
	found := findVM(res, vm)
	switch {
	case len(found) == 0:
		return nil
	case len(found) > 1:
		return fmt.Errorf("名前 %s の VM が %d 台ある", vm, len(found))
	}
	r := found[0]
	if r.Template {
		return &core.RefuseError{Reason: fmt.Sprintf("VM %d はテンプレートで、消さない", r.VMID)}
	}
	if err := c.powerOf(ctx, cl, &r); err != nil {
		return err
	}
	if r.Lock != "" {
		return fmt.Errorf("VM %d に lock %s がある", r.VMID, r.Lock)
	}
	cfg, err := cl.Config(ctx, r.Node, r.VMID)
	if err != nil {
		return err
	}
	if r.Status == PowerRunning {
		if err := p.power(ctx, c, cl, r, "stop"); err != nil {
			return err
		}
	}
	// keep の印のある VM は、設定に無い volume も消さない（E-C2）。
	upid, err := cl.Destroy(ctx, r.Node, r.VMID, len(keepSlots(cfg)) == 0)
	if err != nil {
		return err
	}
	return p.waitTask(ctx, c, cl, upid)
}

// waitTask は upid の仕事の終わりを c.TaskWait まで待つ。失敗で終われば error。
// 終わらなければ誤りにせず戻る（その VM の仕事が走る間の観測は Converging、
// E-D1）。upid が空（同期で終わった）なら何もしない。
func (p *Provider) waitTask(ctx context.Context, c *Cluster, cl *Client, upid string) error {
	if upid == "" {
		return nil
	}
	deadline := time.Now().Add(c.TaskWait)
	for {
		st, err := cl.Task(ctx, upid)
		if err == nil && !st.Running {
			if !st.OK() {
				return fmt.Errorf("仕事 %s が失敗した: %s", upid, st.ExitStatus)
			}
			return nil
		}
		if err != nil || !time.Now().Add(p.poll).Before(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(p.poll):
		}
	}
}

// Ops は種類 vm の一回きりの操作（E-D1・N-D4）。reset と stop は壊す操作。
func (p *Provider) Ops(kind string) map[string]core.OpSpec {
	if kind != KindVM {
		return nil
	}
	return map[string]core.OpSpec{OpReboot: {}, OpReset: {Destructive: true}, OpStop: {Destructive: true}}
}

// Invoke は一回きりの操作を 1 回だけ送る。reboot・reset は止まっている VM には
// 起動を送る。stop は止まっている VM には何も送らない。送る前に分かった送れない
// 状況（届かない・lock・仕事が走っている）は core.ErrNotSent（不変条件 16、E-A2）。
func (p *Provider) Invoke(ctx context.Context, w core.WriteContext, id core.ID, op string, args core.Fields) error {
	if _, ok := p.Ops(id.Kind())[op]; !ok {
		return fmt.Errorf("%w: 種類 %s に操作 %q は無い", core.ErrNotSent, id.Kind(), op)
	}
	if len(args) > 0 {
		return fmt.Errorf("%w: 操作 %s は引数を取らない", core.ErrNotSent, op)
	}
	c, vm, rest, err := p.coords(id)
	if err != nil || rest != "" {
		return fmt.Errorf("%w: %s: %v", core.ErrNotSent, id, err)
	}
	notSent := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s %s: %s", core.ErrNotSent, op, id, fmt.Sprintf(format, a...))
	}
	cl, err := c.writer(w)
	if err != nil {
		return notSent("%v", err)
	}
	res, err := c.resources(ctx, cl)
	if err != nil {
		return notSent("%v", err)
	}
	r, o, ok := lookupVM(ctx, c, cl, vm, res)
	if !ok {
		return notSent("VM が %s（%s）", o.Availability, o.Note)
	}
	action := op
	switch {
	case r.Status == PowerStopped && op == OpStop:
		return nil
	case r.Status == PowerStopped:
		action = "start"
	}
	upid, err := cl.PowerAction(ctx, r.Node, r.VMID, action, nil)
	if err != nil {
		// 送ったあとに切れたかもしれない：送れたか分からないので二度送らない。
		return fmt.Errorf("%s %s: %w", op, id, err)
	}
	return p.waitTask(ctx, c, cl, upid)
}
