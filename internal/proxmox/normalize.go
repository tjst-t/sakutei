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

// normalize.go — 宣言の書き方と、Proxmox が設定に書き戻す表記を同じ正規形に
// そろえる（永久 drift を出さない）。
//
//   - 大きさ: "32G"・"32768M"・"33554432K" は同じ。正規形は割り切れる一番
//     大きい単位（T・G・M・K、どれでも割り切れなければバイト数）。
//   - メモリ: MiB の整数。宣言は "2048"（MiB）・"2048M"・"2G"。観測は
//     "2048" か "current=2048,..."（新しい版の書き方）。
//   - ディスク: "<slot> storage=<storage> size=<大きさ>" を ", " でつなぐ（slot
//     の順）。設定の "local-lvm:vm-100-disk-0,iothread=1,size=32G" から storage
//     と size だけを取る。CD-ROM（media=cdrom）・cloud-init のドライブ・
//     unused は入れない。
//   - NIC: "<slot> bridge=<bridge> [vlan=<tag>] mac=<mac> model=<model>
//     [firewall=1] [mtu=<mtu>]"。MAC は小文字。設定の
//     "virtio=BC:24:11:..,bridge=vmbr0,tag=10,firewall=1" から。firewall は 1 の
//     ときだけ出す（既定 0）、mtu は書かれていれば出す。
//   - VM の FW の options: "enable=<0|1> policy_in=<policy> policy_out=<policy>
//     ipfilter=<0|1> macfilter=<0|1>"。書かれていないキーは Proxmox の既定
//     （enable=0・policy_in=DROP・policy_out=ACCEPT・ipfilter=0・macfilter=1）。
//   - ssh の公開鍵: Proxmox は URL エンコードして持つ。行ごとに前後の空白を
//     落とし、空行を除いて "\n" でつなぐ。
//   - DNS: 空白かカンマで区切ったアドレスを空白 1 つでつなぐ（順は意味がある）。
//   - IP: "dhcp" か CIDR（netip の書き方）。ゲートウェイは IP。

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// EntrySep は disks・nics の項目の区切り（正規形）。宣言は "," で区切れば
// よく、前後の空白は無視する。
const EntrySep = ", "

var units = []struct {
	suffix string
	bytes  int64
}{{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}}

var sizeRe = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)([KMGT]?)$`)

// ParseSize は "32G" などをバイト数にする（単位が無ければバイト）。
func ParseSize(s string) (int64, error) {
	m := sizeRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("size %q: want a number with K, M, G or T (such as 32G)", s)
	}
	mult := int64(1)
	for _, u := range units {
		if u.suffix == m[2] {
			mult = u.bytes
		}
	}
	if !strings.Contains(m[1], ".") {
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil || n > (1<<62)/mult {
			return 0, fmt.Errorf("size %q: too large", s)
		}
		return n * mult, nil
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", s, err)
	}
	return int64(f * float64(mult)), nil
}

// FormatSize はバイト数を正規形にする。
func FormatSize(b int64) string {
	for _, u := range units {
		if b > 0 && b%u.bytes == 0 {
			return strconv.FormatInt(b/u.bytes, 10) + u.suffix
		}
	}
	return strconv.FormatInt(b, 10)
}

// ParseMemory は宣言のメモリ（"2048"＝MiB、"2048M"、"2G"）を MiB にする。
func ParseMemory(s string) (int, error) {
	s = strings.TrimSpace(s)
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n, nil
	}
	b, err := ParseSize(s)
	if err != nil || b <= 0 || b%(1<<20) != 0 || !strings.ContainsAny(s, "MGT") {
		return 0, fmt.Errorf("memory %q: want MiB (such as 2048) or a size in M, G or T (such as 2G)", s)
	}
	return int(b >> 20), nil
}

// observedMemory は設定の memory（"2048" か "current=2048,..."）を MiB の文字列
// にする。無ければ Proxmox の既定 512。
func observedMemory(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "512"
	}
	for _, kv := range strings.Split(raw, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return strings.TrimSpace(k)
		}
		if k == "current" {
			return strings.TrimSpace(v)
		}
	}
	return raw
}

// ---- slot ----

var (
	diskSlotRe = regexp.MustCompile(`^(scsi|virtio|sata|ide)([0-9]+)$`)
	nicSlotRe  = regexp.MustCompile(`^net([0-9]+)$`)
	slotRe     = regexp.MustCompile(`^([a-z]+)([0-9]+)$`)
)

// slotLess は "scsi2" < "scsi10" の順。
func slotLess(a, b string) bool {
	ma, mb := slotRe.FindStringSubmatch(a), slotRe.FindStringSubmatch(b)
	if ma == nil || mb == nil || ma[1] != mb[1] {
		return a < b
	}
	na, _ := strconv.Atoi(ma[2])
	nb, _ := strconv.Atoi(mb[2])
	return na < nb
}

// splitEntries は ", " か "," で区切った項目を返す（空は除く）。
func splitEntries(s string) []string {
	var out []string
	for _, e := range strings.Split(s, ",") {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// entryKV は "<slot> k=v k=v" を slot と k→v にする。
func entryKV(e string, allowed ...string) (string, map[string]string, error) {
	toks := strings.Fields(e)
	slot := toks[0]
	kv := map[string]string{}
	for _, t := range toks[1:] {
		k, v, ok := strings.Cut(t, "=")
		if !ok || v == "" {
			return "", nil, fmt.Errorf("%q: %q is not key=value", e, t)
		}
		if !contains(allowed, k) {
			return "", nil, fmt.Errorf("%q: unknown key %q (want %s)", e, k, strings.Join(allowed, ", "))
		}
		if _, dup := kv[k]; dup {
			return "", nil, fmt.Errorf("%q: duplicate key %q", e, k)
		}
		kv[k] = v
	}
	return slot, kv, nil
}

// ---- ディスク ----

// Disk は 1 つのディスク（slot・ストレージ・大きさ）。
type Disk struct {
	Slot    string
	Storage string
	Size    int64
}

func (d Disk) String() string {
	return d.Slot + " storage=" + d.Storage + " size=" + FormatSize(d.Size)
}

// ParseDisks は宣言の disks を読む。大きさは GiB で割り切れること（新しい
// ディスクは GiB で切り出すため）。
func ParseDisks(s string) ([]Disk, error) {
	var out []Disk
	seen := map[string]bool{}
	for _, e := range splitEntries(s) {
		slot, kv, err := entryKV(e, "storage", "size")
		if err != nil {
			return nil, fmt.Errorf("disks: %w", err)
		}
		if !diskSlotRe.MatchString(slot) {
			return nil, fmt.Errorf("disks: %q: the slot must be scsiN, virtioN, sataN or ideN", e)
		}
		if seen[slot] {
			return nil, fmt.Errorf("disks: slot %s is declared twice", slot)
		}
		seen[slot] = true
		if kv["storage"] == "" || kv["size"] == "" {
			return nil, fmt.Errorf("disks: %q: want \"<slot> storage=<storage> size=<size>\"", e)
		}
		size, err := ParseSize(kv["size"])
		if err != nil {
			return nil, fmt.Errorf("disks: %q: %w", e, err)
		}
		if size <= 0 || size%(1<<30) != 0 {
			return nil, fmt.Errorf("disks: %q: the size must be a whole number of GiB (such as 32G)", e)
		}
		out = append(out, Disk{Slot: slot, Storage: kv["storage"], Size: size})
	}
	sortDisks(out)
	return out, nil
}

func sortDisks(ds []Disk) {
	sort.Slice(ds, func(i, j int) bool { return slotLess(ds[i].Slot, ds[j].Slot) })
}

// FormatDisks は正規形。
func FormatDisks(ds []Disk) string {
	parts := make([]string, len(ds))
	for i, d := range ds {
		parts[i] = d.String()
	}
	return strings.Join(parts, EntrySep)
}

// diskFromConfig は設定のディスクの値を読む。CD-ROM・cloud-init のドライブ・
// 空のドライブは false。
func diskFromConfig(slot, raw string) (Disk, bool) {
	opts := strings.Split(raw, ",")
	vol := strings.TrimSpace(opts[0])
	if vol == "" || vol == "none" || strings.Contains(vol, "cloudinit") {
		return Disk{}, false
	}
	d := Disk{Slot: slot}
	if st, _, ok := strings.Cut(vol, ":"); ok {
		d.Storage = st
	} else {
		return Disk{}, false // パススルーのデバイスなど。
	}
	for _, o := range opts[1:] {
		k, v, _ := strings.Cut(strings.TrimSpace(o), "=")
		switch k {
		case "media":
			if v == "cdrom" {
				return Disk{}, false
			}
		case "size":
			if n, err := ParseSize(v); err == nil {
				d.Size = n
			}
		}
	}
	return d, true
}

// ---- NIC ----

// NICModels は NIC の型。
var NICModels = []string{"virtio", "e1000", "e1000e", "rtl8139", "vmxnet3"}

// NIC は 1 つの NIC。宣言では MAC・型・firewall・mtu は書いたときだけ比べる。
type NIC struct {
	Slot   string
	Bridge string
	VLAN   int // 0 はタグ無し。
	MAC    string
	Model  string
	// Firewall は口の FW（"0"・"1"）。宣言では "" は書いていない、観測では
	// "" は 0（Proxmox の既定）。
	Firewall string
	// MTU は口の MTU（0 は書いていない。1 は bridge の MTU を使う）。
	MTU int
}

func (n NIC) String() string {
	s := n.Slot + " bridge=" + n.Bridge
	if n.VLAN != 0 {
		s += " vlan=" + strconv.Itoa(n.VLAN)
	}
	if n.MAC != "" {
		s += " mac=" + n.MAC
	}
	if n.Model != "" {
		s += " model=" + n.Model
	}
	if n.Firewall != "" {
		s += " firewall=" + n.Firewall
	}
	if n.MTU != 0 {
		s += " mtu=" + strconv.Itoa(n.MTU)
	}
	return s
}

// nicKeys は nics の項目のキー。
var nicKeys = []string{"bridge", "vlan", "mac", "model", "firewall", "mtu"}

var macRe = regexp.MustCompile(`^[0-9a-f]{2}(:[0-9a-f]{2}){5}$`)

// ParseNICs は宣言の nics を読む。
func ParseNICs(s string) ([]NIC, error) {
	var out []NIC
	seen := map[string]bool{}
	for _, e := range splitEntries(s) {
		slot, kv, err := entryKV(e, nicKeys...)
		if err != nil {
			return nil, fmt.Errorf("nics: %w", err)
		}
		if !nicSlotRe.MatchString(slot) {
			return nil, fmt.Errorf("nics: %q: the slot must be netN", e)
		}
		if seen[slot] {
			return nil, fmt.Errorf("nics: slot %s is declared twice", slot)
		}
		seen[slot] = true
		n := NIC{Slot: slot, Bridge: kv["bridge"], MAC: strings.ToLower(kv["mac"]), Model: kv["model"], Firewall: kv["firewall"]}
		if n.Bridge == "" {
			return nil, fmt.Errorf("nics: %q: bridge is required", e)
		}
		if v, ok := kv["vlan"]; ok {
			tag, err := strconv.Atoi(v)
			if err != nil || tag < 1 || tag > 4094 {
				return nil, fmt.Errorf("nics: %q: vlan %q: want 1-4094 (leave it out for no tag)", e, v)
			}
			n.VLAN = tag
		}
		if n.MAC != "" && !macRe.MatchString(n.MAC) {
			return nil, fmt.Errorf("nics: %q: mac %q: want six hex octets such as bc:24:11:00:00:01", e, kv["mac"])
		}
		if n.Model != "" && !contains(NICModels, n.Model) {
			return nil, fmt.Errorf("nics: %q: model %q: want one of %s", e, n.Model, strings.Join(NICModels, ", "))
		}
		if _, ok := kv["firewall"]; ok && n.Firewall != "0" && n.Firewall != "1" {
			return nil, fmt.Errorf("nics: %q: firewall %q: want 0 or 1", e, n.Firewall)
		}
		if v, ok := kv["mtu"]; ok {
			mtu, err := strconv.Atoi(v)
			if err != nil || mtu < 1 || mtu > 65520 {
				return nil, fmt.Errorf("nics: %q: mtu %q: want 1-65520 (1 uses the MTU of the bridge)", e, v)
			}
			if n.Model != "" && n.Model != "virtio" {
				return nil, fmt.Errorf("nics: %q: mtu is only for model=virtio (Proxmox VE sets the MTU of virtio NICs only)", e)
			}
			n.MTU = mtu
		}
		out = append(out, n)
	}
	sortNICs(out)
	return out, nil
}

func sortNICs(ns []NIC) {
	sort.Slice(ns, func(i, j int) bool { return slotLess(ns[i].Slot, ns[j].Slot) })
}

// FormatNICs は正規形。
func FormatNICs(ns []NIC) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = n.String()
	}
	return strings.Join(parts, EntrySep)
}

// kv は設定の値の 1 つの key=value（順を保つため）。
type kv struct{ k, v string }

func parseKVs(raw string) []kv {
	var out []kv
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		k, v, _ := strings.Cut(p, "=")
		out = append(out, kv{k, v})
	}
	return out
}

func joinKVs(kvs []kv) string {
	parts := make([]string, len(kvs))
	for i, x := range kvs {
		if x.v == "" {
			parts[i] = x.k
		} else {
			parts[i] = x.k + "=" + x.v
		}
	}
	return strings.Join(parts, ",")
}

// nicFromConfig は設定の NIC の値を読む。firewall は 1 のときだけ "1"（0 と
// 書かれていないのは ""）。
func nicFromConfig(slot, raw string) NIC {
	n := NIC{Slot: slot}
	for _, x := range parseKVs(raw) {
		switch {
		case contains(NICModels, x.k):
			n.Model, n.MAC = x.k, strings.ToLower(x.v)
		case x.k == "model":
			n.Model = x.v
		case x.k == "macaddr":
			n.MAC = strings.ToLower(x.v)
		case x.k == "bridge":
			n.Bridge = x.v
		case x.k == "tag":
			n.VLAN, _ = strconv.Atoi(x.v)
		case x.k == "firewall":
			if observedBool01(x.v) == "1" {
				n.Firewall = "1"
			}
		case x.k == "mtu":
			n.MTU, _ = strconv.Atoi(x.v)
		}
	}
	return n
}

// observedBool01 は Proxmox の真偽値（0・1・true など）を "0"・"1" にする。
func observedBool01(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "off", "no":
		return "0"
	}
	return "1"
}

// nicConfig は宣言の NIC を設定の値にする。old（今の設定の値。無ければ ""）の
// ほかのキー（rate・queues など）・宣言しない MAC・型・firewall・mtu は残す。
func nicConfig(want NIC, old string) string {
	cur := nicFromConfig(want.Slot, old)
	model, mac := want.Model, want.MAC
	if model == "" {
		model = cur.Model
	}
	if model == "" {
		model = "virtio"
	}
	if mac == "" {
		mac = cur.MAC
	}
	out := []kv{{model, strings.ToUpper(mac)}, {"bridge", want.Bridge}}
	if want.VLAN != 0 {
		out = append(out, kv{"tag", strconv.Itoa(want.VLAN)})
	}
	if want.Firewall != "" {
		out = append(out, kv{"firewall", want.Firewall})
	}
	if want.MTU != 0 {
		out = append(out, kv{"mtu", strconv.Itoa(want.MTU)})
	}
	for _, x := range parseKVs(old) {
		switch {
		case contains(NICModels, x.k), x.k == "model", x.k == "macaddr", x.k == "bridge", x.k == "tag":
		case x.k == "firewall" && want.Firewall != "", x.k == "mtu" && want.MTU != 0:
		default:
			out = append(out, x)
		}
	}
	return joinKVs(out)
}

// ---- VM の FW の options ----

// FirewallKeys は VM の FW の options のキー（正規形の順）。
var FirewallKeys = []string{"enable", "policy_in", "policy_out", "ipfilter", "macfilter"}

// FirewallPolicies は policy_in・policy_out の値。
var FirewallPolicies = []string{"ACCEPT", "DROP", "REJECT"}

// firewallDefaults は書かれていないキーの Proxmox の既定（pve-firewall が使う値）。
var firewallDefaults = map[string]string{"enable": "0", "policy_in": "DROP", "policy_out": "ACCEPT", "ipfilter": "0", "macfilter": "1"}

// ParseFirewall は宣言の firewall（"enable=1 policy_in=DROP ..."、空白かカンマで
// 区切る）を読む。書いたキーだけを返す。
func ParseFirewall(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, t := range strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' || r == '\t' || r == '\n' }) {
		k, v, ok := strings.Cut(t, "=")
		if !ok || v == "" {
			return nil, fmt.Errorf("firewall: %q is not key=value", t)
		}
		if !contains(FirewallKeys, k) {
			return nil, fmt.Errorf("firewall: unknown key %q (want %s)", k, strings.Join(FirewallKeys, ", "))
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("firewall: duplicate key %q", k)
		}
		switch k {
		case "policy_in", "policy_out":
			if !contains(FirewallPolicies, v) {
				return nil, fmt.Errorf("firewall: %s %q: want one of %s", k, v, strings.Join(FirewallPolicies, ", "))
			}
		default:
			if v != "0" && v != "1" {
				return nil, fmt.Errorf("firewall: %s %q: want 0 or 1", k, v)
			}
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("firewall: no option is declared (want key=value of %s)", strings.Join(FirewallKeys, ", "))
	}
	return out, nil
}

// FormatFirewall は options を正規形にする（書かれていないキーは既定）。
func FormatFirewall(opts map[string]string) string {
	parts := make([]string, len(FirewallKeys))
	for i, k := range FirewallKeys {
		v, ok := opts[k]
		if !ok || v == "" {
			v = firewallDefaults[k]
		}
		parts[i] = k + "=" + v
	}
	return strings.Join(parts, " ")
}

// observedFirewall は /firewall/options の値を正規形にする。
func observedFirewall(raw map[string]string) string {
	opts := map[string]string{}
	for _, k := range FirewallKeys {
		v, ok := raw[k]
		if !ok || v == "" {
			continue
		}
		if k == "policy_in" || k == "policy_out" {
			opts[k] = strings.ToUpper(v)
		} else {
			opts[k] = observedBool01(v)
		}
	}
	return FormatFirewall(opts)
}

// firewallDiff は宣言 want（書いたキーだけ）のうち、観測の正規形 have と違う
// キー → 宣言の値を返す。
func firewallDiff(want map[string]string, have string) map[string]string {
	cur := map[string]string{}
	for _, t := range strings.Fields(have) {
		if k, v, ok := strings.Cut(t, "="); ok {
			cur[k] = v
		}
	}
	out := map[string]string{}
	for k, v := range want {
		h, ok := cur[k]
		if !ok {
			h = firewallDefaults[k]
		}
		if h != v {
			out[k] = v
		}
	}
	return out
}

// ---- cloud-init ----

// CanonSSHKeys は ssh の公開鍵の正規形。
func CanonSSHKeys(s string) string {
	var lines []string
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n")
}

// CheckSSHKeys は公開鍵の各行が "<型> <base64> [コメント]"（前に authorized_keys
// のオプションがあってもよい）で、base64 の中身の型が書いた型と同じかを確かめる
// （Proxmox は形の悪い鍵を設定の書き込みで断る。計画の時点で分かるようにする）。
// 鍵の中身は文言に出さない。
func CheckSSHKeys(s string) error {
	for i, line := range strings.Split(CanonSSHKeys(s), "\n") {
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		k := -1
		for j, x := range f {
			if strings.HasPrefix(x, "ssh-") || strings.HasPrefix(x, "ecdsa-sha2-") || strings.HasPrefix(x, "sk-") {
				k = j
				break
			}
		}
		if k < 0 || k+1 >= len(f) {
			return fmt.Errorf("ci_ssh_keys: line %d is not \"<type> <base64> [comment]\"", i+1)
		}
		blob, err := base64.StdEncoding.DecodeString(f[k+1])
		if err != nil || len(blob) < 4 {
			return fmt.Errorf("ci_ssh_keys: line %d: the key is not base64", i+1)
		}
		n := int(binary.BigEndian.Uint32(blob))
		if n > len(blob)-4 || string(blob[4:4+n]) != f[k] {
			return fmt.Errorf("ci_ssh_keys: line %d: the key data is not a %s key", i+1, f[k])
		}
	}
	return nil
}

// observedSSHKeys は設定の sshkeys（URL エンコード）を正規形にする。
func observedSSHKeys(raw string) string {
	if d, err := url.PathUnescape(raw); err == nil {
		raw = d
	}
	return CanonSSHKeys(raw)
}

// encodeURIComponent は Proxmox が sshkeys に求めるエンコード（英数字と
// -_.!~*'() 以外を %XX に）。
func encodeURIComponent(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9') || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// CanonDNS は DNS サーバの並びの正規形。
func CanonDNS(s string) (string, error) {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' || r == '\t' }) {
		a, err := netip.ParseAddr(f)
		if err != nil {
			return "", fmt.Errorf("ci_dns: %q is not an IP address", f)
		}
		out = append(out, a.String())
	}
	return strings.Join(out, " "), nil
}

// CanonIP は ci_ip の正規形（"dhcp" か CIDR）。
func CanonIP(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "dhcp" {
		return s, nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil || !p.Addr().Is4() {
		return "", fmt.Errorf("ci_ip %q: want dhcp or an IPv4 address with its prefix length (such as 192.0.2.10/24)", s)
	}
	return p.String(), nil
}

// CanonGateway は ci_gateway の正規形。
func CanonGateway(s string) (string, error) {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil || !a.Is4() {
		return "", fmt.Errorf("ci_gateway %q: want an IPv4 address", s)
	}
	return a.String(), nil
}

// ipconfigParts は設定の ipconfig0 の ip と gw を返す。
func ipconfigParts(raw string) (ip, gw string) {
	for _, x := range parseKVs(raw) {
		switch x.k {
		case "ip":
			ip = x.v
		case "gw":
			gw = x.v
		}
	}
	return ip, gw
}

// ipconfigValue は ip・gw を old（今の ipconfig0）に入れた値を返す。ip6 などの
// ほかのキーは残す。setIP・setGW が false のものは old のまま。
func ipconfigValue(old string, ip string, setIP bool, gw string, setGW bool) string {
	curIP, curGW := ipconfigParts(old)
	if setIP {
		curIP = ip
	}
	if setGW {
		curGW = gw
	}
	var out []kv
	if curIP != "" {
		out = append(out, kv{"ip", curIP})
	}
	if curGW != "" && curIP != "dhcp" {
		out = append(out, kv{"gw", curGW})
	}
	for _, x := range parseKVs(old) {
		if x.k != "ip" && x.k != "gw" {
			out = append(out, x)
		}
	}
	return joinKVs(out)
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func sortStrings(xs []string) { sort.Strings(xs) }
