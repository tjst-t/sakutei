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

// keep.go — 作り直すときにデータのディスクを引き継ぐ（v2 の #204 を移したもの、
// E-C2・N-C12・N-D10）。
//
//   - keep の印は現実（Proxmox の VM の tags、KeepTag(slot) = "sakutei-keep-<slot>"）
//     に置く。ensure: absent は項目を持たないので、消す VM のどのディスクを残すかは
//     宣言でなく VM の側の印で分かる必要がある（Sakutei の中に持たない）。宣言した
//     slot の印だけを合わせ、ほかの tags は残す。
//   - 付け替えは Proxmox の reassign（move_disk の target-vmid）で行う。volume を
//     消さずに付け先の VM の名前に変える。Proxmox は持ち主の VM の設定から外した
//     volume を unusedN にし、VM を消すと unused も消すので、keep の印のある VM は、
//     keep の slot にディスクがある間と unused がある間は消さない（確信が持てない
//     ときは消さない）。Delete は何も書かずに断る（*core.RefuseError）。
//   - 走っている VM からは slot のディスクを付け替えない（Proxmox も断る）。止める
//     のは持ち主の VM の宣言 power: stopped。
//   - 既にある volume を付ける変更は承認の要る壊す変更（Provider.Classify）。
//   - 付け先の無い volume（VM の unusedN）は List が種類 volume の
//     "<クラスタ>/<VM 名>/unusedN" で返し、status の宣言外に出る。

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/v3/core"
)

// unusedKeyRe は VM の設定の unused の キー。
var unusedKeyRe = regexp.MustCompile(`^unused[0-9]+$`)

// volumeOf はドライブの値（"local-lvm:vm-100-disk-1,size=32G"）の volume。
func volumeOf(raw string) string {
	v, _, _ := strings.Cut(raw, ",")
	return strings.TrimSpace(v)
}

// splitTags は設定の tags（";"・","・空白で区切る）を返す。
func splitTags(v string) []string {
	var out []string
	for _, t := range strings.FieldsFunc(v, func(r rune) bool { return r == ';' || r == ',' || r == ' ' }) {
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// keepSlots は設定の tags の keep の印のある slot。
func keepSlots(cfg map[string]string) map[string]bool {
	out := map[string]bool{}
	for _, t := range splitTags(cfg["tags"]) {
		if slot, ok := strings.CutPrefix(strings.ToLower(t), KeepTagPrefix); ok && slot != "" {
			out[slot] = true
		}
	}
	return out
}

// keepTagsFor は今の tags cur を、宣言した ws の keep に合わせた tags を返す
// （宣言した slot の印だけを足し・外し、ほかの tags は残す）。changed は違うか。
func keepTagsFor(cur string, ws []Disk) (string, bool) {
	tags := splitTags(cur)
	has := map[string]bool{}
	for _, t := range tags {
		has[strings.ToLower(t)] = true
	}
	changed := false
	drop := map[string]bool{}
	for _, w := range ws {
		tag := KeepTag(w.Slot)
		switch {
		case w.Keep && !has[tag]:
			tags = append(tags, tag)
			has[tag] = true
			changed = true
		case !w.Keep && has[tag]:
			drop[tag] = true
			changed = true
		}
	}
	if !changed {
		return cur, false
	}
	var out []string
	for _, t := range tags {
		if !drop[strings.ToLower(t)] {
			out = append(out, t)
		}
	}
	return strings.Join(out, ";"), true
}

// unusedVolumes は設定の unusedN（キー → volume）。
func unusedVolumes(cfg map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range cfg {
		if unusedKeyRe.MatchString(k) {
			out[k] = volumeOf(v)
		}
	}
	return out
}

func sortedSlots(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return slotLess(keys[i], keys[j]) })
	return keys
}

// keepHeld は VM の設定のうち、消すと失う keep の volume を返す（slot か
// unusedN → volume）：keep の印のある slot のディスクと、keep の印が 1 つでも
// あれば unused の全部（どの slot から外れたかは分からないので消さない側）。
func keepHeld(cfg map[string]string) map[string]string {
	keep := keepSlots(cfg)
	if len(keep) == 0 {
		return nil
	}
	held := map[string]string{}
	for slot := range keep {
		if raw, ok := cfg[slot]; ok && diskSlotRe.MatchString(slot) {
			if _, ok := diskFromConfig(slot, raw); ok {
				held[slot] = volumeOf(raw)
			}
		}
	}
	for k, v := range unusedVolumes(cfg) {
		held[k] = v
	}
	return held
}

// describeHeld は keepHeld を "scsi1=local-lvm:vm-120-disk-1, ..." にする。
func describeHeld(held map[string]string) string {
	var parts []string
	for _, k := range sortedSlots(held) {
		parts = append(parts, k+"="+held[k])
	}
	return strings.Join(parts, ", ")
}

// attachVolume は既にある volume vol を VM target の slot に付ける。vol がほかの VM
// （持ち主）の設定にあれば、Proxmox の reassign で付け替える（volume は消さず、
// 持ち主の設定から外れる）。持ち主の VM が無ければ（外したまま残った volume）
// そのまま付ける。next は target の今の設定（再起動の後の値）。
func (p *Provider) attachVolume(ctx context.Context, c *Cluster, cl *Client, target Resource, slot, vol string, next map[string]string, res []Resource) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("volume %s を %s に付ける: "+format, append([]any{vol, slot}, args...)...)
	}
	set := func() error {
		if err := cl.SetConfig(ctx, target.Node, target.VMID, url.Values{slot: {vol}}); err != nil {
			return fail("%w（何も消していない）", err)
		}
		return nil
	}
	for _, k := range sortedSlots(next) {
		if volumeOf(next[k]) != vol {
			continue
		}
		if unusedKeyRe.MatchString(k) {
			return set() // この VM の unused（外したまま）を付け直す。
		}
		if diskSlotRe.MatchString(k) {
			return fail("volume はもうこの VM の slot %s にある（%s に宣言するか、手で動かす）", k, k)
		}
	}
	owner := VolumeOwner(vol)
	if owner == 0 {
		return fail("volume の名前から持ち主の VM が分からない（vm-<vmid>-… か base-<vmid>-… の形）")
	}
	if owner == target.VMID {
		return set() // この VM の、設定に無い volume。
	}
	var holder *Resource
	for i, r := range res {
		if r.Type == "qemu" && r.VMID == owner {
			holder = &res[i]
		}
	}
	if holder == nil {
		// 持ち主の VM が無い（外したまま残った volume）。そのまま付ける。
		return set()
	}
	switch {
	case holder.Template:
		return fail("volume はテンプレート %d（%s）のもの。テンプレートのディスクは動かさない（template: を宣言する）", holder.VMID, holder.Name)
	case holder.Node != target.Node:
		return fail("volume はノード %s の VM %d（%s）にあり、この VM はノード %s。ディスクは同じノードの VM の間でだけ動かす。何も変えていない", holder.Node, holder.VMID, holder.Name, target.Node)
	}
	if err := c.powerOf(ctx, cl, holder); err != nil {
		return fail("VM %d: %w", holder.VMID, err)
	}
	if holder.Lock != "" {
		return fail("volume を持つ VM %d（%s）に lock %s がある。何も変えていない", holder.VMID, holder.Name, holder.Lock)
	}
	cfg, err := cl.Config(ctx, holder.Node, holder.VMID)
	if err != nil {
		return fail("VM %d を読む: %w", holder.VMID, err)
	}
	from := ""
	for _, k := range sortedSlots(cfg) {
		if (diskSlotRe.MatchString(k) || unusedKeyRe.MatchString(k)) && volumeOf(cfg[k]) == vol {
			from = k
			break
		}
	}
	switch {
	case from == "":
		return fail("volume は VM %d（%s）のものだが、その設定に無い（`qm disk rescan --vmid %d` で unusedN に戻せる）。何も変えていない", holder.VMID, holder.Name, holder.VMID)
	case diskSlotRe.MatchString(from) && holder.Status != PowerStopped:
		return fail("volume は VM %d（%s）の slot %s にあり、その VM は %s。ディスクは止まった VM からだけ動かす（その VM に power: stopped を宣言する）。何も変えていない。止まれば次の周に動かす", holder.VMID, holder.Name, from, holder.Status)
	}
	upid, err := cl.MoveDiskToVM(ctx, holder.Node, holder.VMID, from, target.VMID, slot)
	if err != nil {
		return fail("VM %d（%s）の %s から外す: %w（volume は消していない：VM %d に残る）", holder.VMID, holder.Name, from, err, holder.VMID)
	}
	if err := p.waitTask(ctx, c, cl, upid); err != nil {
		return fmt.Errorf("%w（volume %s は消していない：VM %d に残るか、もうこの VM にある。次の周に見直す）", err, vol, holder.VMID)
	}
	return nil
}

// keepGuard は VM vm を消してよいかを確かめる（E-C2）。keep の volume が残れば、
// 何も書かずに *core.RefuseError で断る。VM が無い・読めないときは誤りにせず
// 消す処理に任せる（消す処理が読み直す）。
func (p *Provider) keepGuard(ctx context.Context, c *Cluster, cl *Client, vm string) error {
	res, err := c.resources(ctx, cl)
	if err != nil {
		return err
	}
	found := findVM(res, vm)
	if len(found) != 1 {
		return nil
	}
	r := found[0]
	cfg, err := cl.Config(ctx, r.Node, r.VMID)
	if err != nil {
		return err
	}
	held := keepHeld(cfg)
	if len(held) == 0 {
		return nil
	}
	return &core.RefuseError{Reason: fmt.Sprintf("VM %d は keep の volume %s を持つので消さない。disks に volume=<その volume> を宣言した VM が引き取れば消せる（持ち主の VM は power: stopped で止める）。keep のディスクも消すなら、先に keep を外した宣言を収束させる",
		r.VMID, describeHeld(held))}
}
