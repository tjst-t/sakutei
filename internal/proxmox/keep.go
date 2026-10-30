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

// keep.go — 作り直すときにデータのディスクを引き継ぐ（Backlog #204）。
//
// 設計判断:
//
//   - keep の印は現実（Proxmox の VM の tags、KeepTag(slot) = "sakutei-keep-<slot>"）
//     に置く。ensure: absent は欄を持たないので、消す VM のどのディスクを残すかは
//     宣言でなく VM の側の印で分かる必要がある（観測 state レス：Sakutei の中に
//     持たない）。宣言した slot の印だけを合わせ、ほかの tags は残す。
//   - 付け替えは Proxmox の reassign（move_disk の target-vmid）で行う。volume を
//     消さずに付け先の VM の名前に変える（持ち主が変わるので、元の VM を消しても
//     消えない）。Proxmox は持ち主の VM の設定から外した volume を unusedN にし、
//     VM を消すと unused も消すので、「外したまま」で VM を消すと volume も消える。
//     だから、keep の印のある VM は、keep の slot にディスクがある間と unused が
//     ある間は消さない（確信が持てないときは消さない、§3 原則 3）。
//   - 走っている VM からは slot のディスクを付け替えない（Proxmox も断る。外す
//     のはゲストの中で使っているディスクを抜くことになる）。止めるのは宣言
//     （power: stopped）か、その VM の ensure: absent（承認された Delete は消す前に
//     止める。keep のディスクが残れば、止めて消さずに待つ）。
//   - 既にある volume を付ける Op は承認の要る壊す変更（ApprovalRequired）：
//     予算に数え、planned_change のレーン（承認）が開いているときだけ走る。
//   - 付け先の無い volume（VM の unusedN）は List が "<クラスタ>/<VM 名>/unusedN" の
//     ID で返し、status の unmanaged に出る。知らせるための ID で、宣言はできず
//     （volume= で付ける）、Read は観測できないと断る（import が書き出さない）。

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// unusedKeyRe は VM の設定の unused の キー。
var unusedKeyRe = regexp.MustCompile(`^unused[0-9]+$`)

// UnusedLocalID は VM の unused の ID を作る。
func UnusedLocalID(cluster, vm, key string) string { return cluster + "/" + vm + "/" + key }

// splitUnusedID は "<クラスタ>/<VM 名>/unusedN" を VM の ID と unused のキーに分ける。
func splitUnusedID(localID string) (vmLocal, key string, ok bool) {
	parts := strings.Split(localID, "/")
	if len(parts) != 3 || !unusedKeyRe.MatchString(parts[2]) {
		return "", "", false
	}
	return parts[0] + "/" + parts[1], parts[2], true
}

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

// unusedIDError は付け先の無い volume の ID を宣言した誤り。
func unusedIDError(localID string) error {
	return fmt.Errorf("%s %q: the id names a volume left detached from a VM (status lists it as unmanaged); it cannot be declared: attach it to a VM with volume=<storage>:<volume> in its disks, or remove it in Proxmox VE by hand", Name, localID)
}

// ApprovalRequired は既にある volume を付ける（ほかの VM から付け替える）Op を、
// 承認の要る壊す変更とする（provider.ApprovalRequirer、Backlog #204）。宣言の
// volume= の slot が観測に無いときだけ（付いたあとは要らない）。I/O はしない。
func (p *Provider) ApprovalRequired(localID string, observed core.State, desired map[string]string, op core.Op) string {
	if op.Kind == core.OpKindDelete {
		return ""
	}
	if op.Kind == core.OpKindUpdate {
		touches := false
		for _, f := range op.Fields {
			touches = touches || f.Field == FieldDisks
		}
		if !touches {
			return ""
		}
	}
	v, ok := desired[FieldDisks]
	if !ok {
		return ""
	}
	ws, err := ParseDisks(v)
	if err != nil {
		return ""
	}
	have := map[string]Disk{}
	if tv, ok := observed[FieldDisks]; ok {
		have, _ = parseObservedDisks(fmt.Sprintf("%v", tv.Value))
	}
	var vols []string
	for _, w := range ws {
		if _, attached := have[w.Slot]; w.Volume != "" && !attached {
			vols = append(vols, w.Slot+"="+w.Volume)
		}
	}
	if len(vols) == 0 {
		return ""
	}
	return fmt.Sprintf("attaches existing volume(s) %s, moving them off the VM that holds them without destroying them (a reattached disk, Backlog #204)", strings.Join(vols, ", "))
}

// attachVolume は既にある volume vol を VM target の slot に付ける。vol が
// ほかの VM（持ち主）の設定にあれば、Proxmox の reassign で付け替える（volume は
// 消さず、持ち主の設定から外れる）。持ち主の VM が無ければ（外したまま残った
// volume）そのまま付ける。next は target の今の設定（再起動の後の値）。
func (p *Provider) attachVolume(ctx context.Context, c Cluster, localID string, target Resource, slot, vol string, next map[string]string, res []Resource) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s: Apply %q: attach volume %s to %s: "+format, append([]any{Name, localID, vol, slot}, args...)...)
	}
	set := func() error {
		if err := c.Client.SetConfig(ctx, target.Node, target.VMID, url.Values{slot: {vol}}); err != nil {
			return classify(fail("%w (nothing was destroyed)", err))
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
			return provider.NewPermanentError(fail("the volume is already on slot %s of this VM (declare it on %s, or move it by hand)", k, k))
		}
	}
	owner := VolumeOwner(vol)
	if owner == 0 {
		return provider.NewPermanentError(fail("the VM that owns the volume cannot be told from its name (want vm-<vmid>-… or base-<vmid>-…)"))
	}
	if owner == target.VMID {
		return set() // この VM の、設定に無い volume。
	}
	if c.protected(owner, "") {
		return provider.NewPermanentError(fail("the volume belongs to VM %d, which is protected: Sakutei never touches it", owner))
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
	case c.protected(holder.VMID, holder.Name):
		return provider.NewPermanentError(fail("the volume belongs to VM %d (%s), which is protected: Sakutei never touches it", holder.VMID, holder.Name))
	case holder.Template:
		return provider.NewPermanentError(fail("the volume belongs to template %d (%s); a template's disk is never moved (declare template: instead)", holder.VMID, holder.Name))
	case holder.Node != target.Node:
		return provider.NewPermanentError(fail("the volume is on VM %d (%s) on node %s, but this VM is on node %s; a disk is moved only between VMs on the same node (declare node: %s, or migrate one of them by hand). Nothing was changed", holder.VMID, holder.Name, holder.Node, target.Node, holder.Node))
	}
	if err := c.powerOf(ctx, holder); err != nil {
		return classify(fail("VM %d: %w", holder.VMID, err))
	}
	if holder.Lock != "" {
		return provider.NewTransientError(fail("VM %d (%s) that holds the volume is locked (%s); nothing was changed", holder.VMID, holder.Name, holder.Lock))
	}
	cfg, err := c.Client.Config(ctx, holder.Node, holder.VMID)
	if err != nil {
		return classify(fail("reading VM %d: %w", holder.VMID, err))
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
		return provider.NewPermanentError(fail("the volume belongs to VM %d (%s) but is not in its configuration (`qm disk rescan --vmid %d` adds it as unusedN); nothing was changed", holder.VMID, holder.Name, holder.VMID))
	case diskSlotRe.MatchString(from) && holder.Status != PowerStopped:
		return provider.NewTransientError(fail("the volume is on slot %s of VM %d (%s), which is %s: a disk is moved only off a stopped VM (declare power: stopped on it, or its ensure: absent stops it). Nothing was changed; the next cycle moves it once the VM is stopped", from, holder.VMID, holder.Name, holder.Status))
	}
	upid, err := c.Client.MoveDiskToVM(ctx, holder.Node, holder.VMID, from, target.VMID, slot)
	if err != nil {
		return classify(fail("moving it off %s of VM %d (%s): %w (the volume was not destroyed: it stays on VM %d)", from, holder.VMID, holder.Name, err, holder.VMID))
	}
	if _, err := p.waitTask(ctx, c, localID, upid); err != nil {
		return withNote(err, fmt.Sprintf("moving volume %s off %s of VM %d failed; the volume was not destroyed (it stays on VM %d or is already on this VM; the next cycle looks again)", vol, from, holder.VMID, holder.VMID))
	}
	return nil
}

// withNote は provider のエラーに、どこまで進んだかの文を足す（型は保つ）。
func withNote(err error, note string) error {
	var te *provider.TransientError
	var pe *provider.PermanentError
	switch {
	case errors.As(err, &te):
		return provider.NewTransientError(fmt.Errorf("%w; %s", te.Cause, note))
	case errors.As(err, &pe):
		return provider.NewPermanentError(fmt.Errorf("%w; %s", pe.Cause, note))
	}
	return fmt.Errorf("%w; %s", err, note)
}

// keepGuard は VM vm を消してよいかを確かめる（Backlog #204）。keep の volume が
// 残れば、走っていれば止めて（Delete は承認されている。付け替えは止まった VM
// からだけ）、消さずに error を返す。unreferenced は Destroy に渡す
// destroy-unreferenced-disks（keep の印のある VM では設定に無い volume も消さない）。
func (p *Provider) keepGuard(ctx context.Context, c Cluster, localID string, vm Resource) (unreferenced bool, err error) {
	cfg, err := c.Client.Config(ctx, vm.Node, vm.VMID)
	if err != nil {
		return false, classify(fmt.Errorf("%s: Apply %q: delete: %w", Name, localID, err))
	}
	if len(keepSlots(cfg)) == 0 {
		return true, nil
	}
	held := keepHeld(cfg)
	if len(held) == 0 {
		return false, nil
	}
	stopped := ""
	if vm.Status == PowerRunning {
		upid, err := c.Client.PowerAction(ctx, vm.Node, vm.VMID, "stop", nil)
		if err != nil {
			return false, classify(fmt.Errorf("%s: Apply %q: delete: stop: %w (nothing was destroyed)", Name, localID, err))
		}
		if _, err := p.waitTask(ctx, c, localID, upid); err != nil {
			return false, withNote(err, "nothing was destroyed")
		}
		stopped = "stopped, but "
	}
	return false, provider.NewTransientError(fmt.Errorf("%s: Apply %q: delete: VM %d was %snot destroyed: it holds keep volume(s) %s. A VM that declares volume=<the volume> in its disks takes them (then this VM can be destroyed), or declare this VM without keep on them first. Nothing was destroyed",
		Name, localID, vm.VMID, stopped, describeHeld(held)))
}

// listUnused は VM の付け先の無い volume（unusedN）の ID を返す。
func listUnused(ctx context.Context, c Cluster, cluster string, vms []Resource) ([]core.ID, error) {
	var ids []core.ID
	for _, r := range vms {
		cfg, err := c.Client.Config(ctx, r.Node, r.VMID)
		if err != nil {
			return nil, fmt.Errorf("VM %d: %w", r.VMID, err)
		}
		un := unusedVolumes(cfg)
		for _, k := range sortedSlots(un) {
			ids = append(ids, core.NewID(Name, c.Scope, UnusedLocalID(cluster, r.Name, k)))
		}
	}
	return ids, nil
}
