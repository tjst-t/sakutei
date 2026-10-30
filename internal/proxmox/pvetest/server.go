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

// Package pvetest は proxmox-vm provider のテスト用の偽の Proxmox VE（httptest）。
// provider が使う API（/cluster/resources・VM の作成・複製・設定・pending・
// resize・電源・削除・仕事の状態）だけを持ち、API トークンの認証を確かめる。
//
// 本物に合わせた振る舞い:
//
//   - 設定の書き戻し: 新しいディスク "local-lvm:32" は
//     "local-lvm:vm-<vmid>-disk-<n>,size=32G"、cloud-init のドライブは
//     "local-lvm:vm-<vmid>-cloudinit,media=cdrom"、MAC の無い NIC には MAC を
//     付け（大文字）、"virtio=<MAC>,bridge=..." の形で持つ。
//   - 走っている VM の ColdKeys（既定 cores・memory）の変更は pending に置き、
//     start・reboot で効く。
//   - 作成・複製・電源・削除・resize は仕事（UPID）。Options.TaskPolls が 0 なら
//     すぐに終わり、正なら状態をその回数だけ問い合わせると終わる（効き目は
//     終わったとき。複製の間は VM に lock: clone）。
//   - /pending と /cluster/resources の数は JSON の数で返す。
//   - Options.ResourceLag が正なら、/cluster/resources の名前・status・template は
//     pvestatd のように周期で写した古い値（作った直後の VM は名前が空）。今の
//     電源と lock は /status/current、名前とテンプレートは /config が正。
//   - pool（Backlog #165）：作成・複製の pool（無い pool は 403 "Permission check
//     failed (pool 'x' does not exist)"）、PUT /pools（poolid・vms・allow-move・
//     delete。無い pool・ほかの pool にある VM・pool に無い VM は 500）、
//     /cluster/resources の pool（設定の今の値で、遅れない）。
//   - pool に絞ったトークン（Options.PoolTokens）：その pool の VM と、Templates に
//     挙げたテンプレートだけが見え、触れる（ほかは 403）。作成・複製はその pool を
//     渡したときだけ。/cluster/resources の pool は PoolAudit のときだけ載り、
//     PUT /pools は PoolAllocate のときだけその pool に。
//   - VM の FW の options（Backlog #196）：GET・PUT /firewall/options（enable・
//     policy_in・policy_out・ipfilter・macfilter と delete・digest。ほかのキーと
//     値の誤りは 400）。GET は書いたキーだけを返す（本物と同じく既定は返さない）。
//   - VM の FW の規則（Backlog #197）：GET・POST /firewall/rules、PUT・DELETE
//     /firewall/rules/<pos>。Proxmox VE 9.2 で確かめた決まりに合わせる：作ると
//     いつも先頭（pos 0）に入り、enable を渡さなければ 0。PUT の moveto は「元の
//     位置 moveto の規則の前」に入れる（後ろへ動かすと moveto-1 に着く、並びの
//     外なら最後）。GET の項目は並び全体の digest を持ち、PUT・DELETE に渡した
//     digest が違えば断る。PUT の delete は欄を消す。type・action・iface（netN）・
//     知らないキーの誤りは 400。
//   - volume（Backlog #204）：ディスクの volume を台帳に持つ（作る・複製で
//     切り出し、在る volume を指すドライブは台帳に無ければ 500）。本物と同じく、
//     VM の持ち物（vm-<vmid>-…）の slot を設定から外すと unusedN になり（ほかの VM
//     の volume は外れるだけ）、unusedN を消すと volume も消える。unusedN の
//     volume を slot に付け直すと unusedN は消える。POST /move_disk の
//     target-vmid（reassign）は volume を消さずに付け先の vmid の名前に変えて
//     付け替える（同じノード、元が走っていれば unusedN だけ、付け先の slot が
//     空いていること）。VM を消すと設定にある持ち物の volume（unused を含む）を
//     消し、destroy-unreferenced-disks=1 なら設定に無い持ち物も消す。
//
// テストから: 現実を手で変える（SetConfig・SetFirewall・SetRules・SetStatus・SetLock・SetPool）、ノードを落とす
// （SetNodeOffline：資源の status が unknown、ノードの API が 595）、届かなく
// する（SetUnreachable）、次の仕事を失敗させる（FailNextTask）。
package pvetest

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Options は偽サーバの設定。
type Options struct {
	// TokenID・Secret は受け付けるトークン（既定 "root@pam!sakutei" / "test-secret"）。
	TokenID, Secret string
	// Nodes はノードの名前（既定 ["pve1", "pve2"]）。
	Nodes []string
	// TaskPolls は仕事が終わるまでの状態の問い合わせの回数（0 ならすぐ終わる）。
	TaskPolls int
	// ColdKeys は走っている VM では pending に置くキー（既定 cores・memory）。
	ColdKeys []string
	// ResourceLag が正なら、/cluster/resources の名前・status・template を
	// pvestatd のように周期で写す：この回数の一覧ごとに 1 回だけ今の値に
	// 更新する。作った直後の VM は名前が空・status が空で載る。
	ResourceLag int
	// Pools は在る pool の名前。
	Pools []string
	// PoolTokens は pool に絞ったトークン（TokenID・Secret のほかに受け付ける）。
	PoolTokens []PoolToken
	// BasePath は API のパスの前置き（既定 "/api2/json"、"/" は前置き無し）。
	// 前置きの違う模擬の再現（#160）。
	BasePath string
	// AuditTokenID・AuditSecret は読むだけのトークン（ACL が / に PVEAuditor、
	// #186）。GET だけを受け付け、ほかは 403。
	AuditTokenID, AuditSecret string
}

// PoolToken は 1 つの pool に絞ったトークン（ACL が /pool/<Pool> に PVEVMAdmin、
// テンプレートに PVETemplateUser）。
type PoolToken struct {
	TokenID, Secret, Pool string
	// Templates は見えて複製できるテンプレートの vmid。
	Templates []int
	// PoolAudit は Pool.Audit（/cluster/resources に pool が載る）、PoolAllocate は
	// Pool.Allocate（PUT /pools でその pool に出し入れできる）。
	PoolAudit, PoolAllocate bool
}

// VM は偽サーバの VM。
type VM struct {
	VMID     int
	Node     string
	Name     string
	Template bool
	Status   string // running / stopped
	Lock     string
	Pool     string
	// Config は設定（キー → 値）。
	Config map[string]string
	// Pending は再起動を待つ値、PendingDelete は再起動で消えるキー。
	Pending       map[string]string
	PendingDelete map[string]bool
	// Firewall は VM の FW の options（書いたキーだけ）。
	Firewall map[string]string
	// Rules は VM の FW の規則（並びの順。キー → 値、enable は "0"・"1"）。
	Rules []map[string]string

	// rrd は /cluster/resources に載せる写し（ResourceLag のとき）。
	rrdName, rrdStatus string
	rrdTemplate        bool
}

type task struct {
	node   string
	polls  int
	done   bool
	exit   string
	finish func()
}

// Server は偽の Proxmox VE。
type Server struct {
	*httptest.Server
	opts Options

	mu          sync.Mutex
	vms         map[int]*VM
	pools       map[string]bool
	tasks       map[string]*task
	seq         int
	macSeq      int
	unreachable bool
	failStatus  int
	offline     map[string]bool
	failNext    map[string]string
	calls       map[string]int
	resCalls    int
	byToken     map[string]map[string]int // トークン → "GET"・"WRITE" → 回数
	// volumes は在る volume（"<storage>:<name>" → 大きさのバイト数、#204）。
	volumes map[string]int64
}

// New は偽サーバを立てる（HTTP）。
func New(opts Options) *Server {
	if opts.TokenID == "" {
		opts.TokenID = "root@pam!sakutei"
	}
	if opts.Secret == "" {
		opts.Secret = "test-secret"
	}
	if len(opts.Nodes) == 0 {
		opts.Nodes = []string{"pve1", "pve2"}
	}
	if opts.BasePath == "" {
		opts.BasePath = "/api2/json"
	}
	opts.BasePath = strings.TrimRight(opts.BasePath, "/")
	if opts.ColdKeys == nil {
		opts.ColdKeys = []string{"cores", "memory"}
	}
	s := &Server{opts: opts, vms: map[int]*VM{}, pools: map[string]bool{}, tasks: map[string]*task{}, offline: map[string]bool{},
		failNext: map[string]string{}, calls: map[string]int{}, byToken: map[string]map[string]int{}, volumes: map[string]int64{}}
	for _, p := range opts.Pools {
		s.pools[p] = true
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// TokenID と Secret は受け付けるトークン。
func (s *Server) TokenID() string { return s.opts.TokenID }
func (s *Server) Secret() string  { return s.opts.Secret }

// TokenCalls は tokenID のトークンで受けた要求の回数（"GET" と、ほかの "WRITE"。
// 断った要求も数える）。
func (s *Server) TokenCalls(tokenID string) map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for k, v := range s.byToken[tokenID] {
		out[k] = v
	}
	return out
}

// SetUnreachable は true の間、接続を切る。
func (s *Server) SetUnreachable(v bool) { s.mu.Lock(); s.unreachable = v; s.mu.Unlock() }

// SetFailStatus は 0 でない間、すべての要求にその状態コードを返す。
func (s *Server) SetFailStatus(code int) { s.mu.Lock(); s.failStatus = code; s.mu.Unlock() }

// SetNodeOffline はノードを落とす（true）・戻す（false）。
func (s *Server) SetNodeOffline(node string, v bool) { s.mu.Lock(); s.offline[node] = v; s.mu.Unlock() }

// FailNextTask は次の kind（"qmclone"・"qmstart" など）の仕事を msg で失敗させる。
func (s *Server) FailNextTask(kind, msg string) { s.mu.Lock(); s.failNext[kind] = msg; s.mu.Unlock() }

// Calls は操作（"resources"・"pending"・"config"・"create"・"clone"・"resize"・
// "start"・"shutdown"・"stop"・"reboot"・"reset"・"destroy"・"task"・"pools"・"firewall"・"fwrules"・"move_disk"）ごとの
// 呼ばれた回数を返す。
func (s *Server) Calls(op string) int { s.mu.Lock(); defer s.mu.Unlock(); return s.calls[op] }

// AddVM は VM を置く（現実を手で作る）。
func (s *Server) AddVM(vm VM) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if vm.Status == "" {
		vm.Status = "stopped"
	}
	c := map[string]string{}
	for k, v := range vm.Config {
		c[k] = v
	}
	if vm.Name != "" {
		c["name"] = vm.Name
	}
	if vm.Template {
		c["template"] = "1"
	}
	vm.Config = c
	vm.Pending, vm.PendingDelete = copyMap(vm.Pending), map[string]bool{}
	vm.Firewall = copyMap(vm.Firewall)
	vm.Rules = copyRules(vm.Rules)
	vm.rrdName, vm.rrdStatus, vm.rrdTemplate = vm.Name, vm.Status, vm.Template
	for k, v := range vm.Config {
		s.registerVolume(k, v)
	}
	s.vms[vm.VMID] = &vm
}

// Volume は volume が在るか（と大きさ）を返す（#204）。
func (s *Server) Volume(volid string) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.volumes[volid]
	return n, ok
}

// AddVolume は volume を台帳に置く（持ち主の VM の無い volume を手で作る）。
func (s *Server) AddVolume(volid string, size int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.volumes[volid] = size
}

// registerVolume は設定のドライブ（slot か unusedN）の volume を台帳に置く。
func (s *Server) registerVolume(key, value string) {
	if !diskRe.MatchString(key) && !unusedRe.MatchString(key) {
		return
	}
	vol, opts, _ := strings.Cut(value, ",")
	if !strings.Contains(vol, ":") || strings.Contains(vol, "cloudinit") || strings.Contains(opts, "media=cdrom") {
		return
	}
	size := int64(1 << 30)
	for _, o := range strings.Split(opts, ",") {
		if v, ok := strings.CutPrefix(o, "size="); ok {
			size = bytes(v)
		}
	}
	if _, ok := s.volumes[vol]; !ok {
		s.volumes[vol] = size
	}
}

// volumeOwner は volume の名前の持ち主の vmid（読めなければ 0）。
func volumeOwner(vol string) int {
	m := ownerRe.FindStringSubmatch(vol)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// freeDiskName は vmid の storage の次の空いた "vm-<vmid>-disk-<n>"。
func (s *Server) freeDiskName(storage string, vmid int) string {
	for n := 0; ; n++ {
		v := fmt.Sprintf("%s:vm-%d-disk-%d", storage, vmid, n)
		if _, used := s.volumes[v]; !used {
			return v
		}
	}
}

// addUnused は vm の設定に volume を次の空いた unusedN で足す。
func addUnused(vm *VM, vol string) {
	for n := 0; ; n++ {
		k := fmt.Sprintf("unused%d", n)
		if _, used := vm.Config[k]; !used {
			vm.Config[k] = vol
			return
		}
	}
}

// detachDrive は vm の設定から key のドライブを外す（本物と同じく、vm の持ち物の
// volume は unusedN に、unusedN は volume ごと消す）。
func (s *Server) detachDrive(vm *VM, key string) {
	value, ok := vm.Config[key]
	if !ok {
		return
	}
	delete(vm.Config, key)
	vol, opts, _ := strings.Cut(value, ",")
	switch {
	case unusedRe.MatchString(key):
		delete(s.volumes, vol)
	case diskRe.MatchString(key) && !strings.Contains(opts, "media=cdrom") && !strings.Contains(vol, "cloudinit") && volumeOwner(vol) == vm.VMID:
		addUnused(vm, vol)
	}
}

// moveDisk は POST /move_disk の target-vmid（reassign）。
func (s *Server) moveDisk(vm *VM, form url.Values) (any, *apiError) {
	disk, targetDisk := form.Get("disk"), form.Get("target-disk")
	if targetDisk == "" {
		targetDisk = disk
	}
	tid, err := strconv.Atoi(form.Get("target-vmid"))
	if err != nil {
		return nil, errf(http.StatusNotImplemented, "moving to another storage is not supported here")
	}
	target, ok := s.vms[tid]
	switch {
	case !ok:
		return nil, errf(http.StatusInternalServerError, "Configuration file 'nodes/%s/qemu-server/%d.conf' does not exist", vm.Node, tid)
	case tid == vm.VMID:
		return nil, errf(http.StatusBadRequest, "target-vmid: must be different than source VMID to reassign disk")
	case target.Node != vm.Node:
		return nil, errf(http.StatusInternalServerError, "Both VMs need to be on the same node (%s != %s)", vm.Node, target.Node)
	case vm.Lock != "" || target.Lock != "":
		return nil, errf(http.StatusInternalServerError, "VM is locked")
	case vm.Template || target.Template:
		return nil, errf(http.StatusInternalServerError, "Can't move disks from or to template VMs")
	}
	value, ok := vm.Config[disk]
	if !ok {
		return nil, errf(http.StatusInternalServerError, "Disk '%s' for VM '%d' does not exist", disk, vm.VMID)
	}
	if _, used := target.Config[targetDisk]; used {
		return nil, errf(http.StatusInternalServerError, "Target disk key '%s' is already in use for VM '%d'", targetDisk, tid)
	}
	if vm.Status == "running" && !unusedRe.MatchString(disk) {
		return nil, errf(http.StatusInternalServerError, "Cannot move disk to another VM while the source VM is running - detach first")
	}
	vol, opts, _ := strings.Cut(value, ",")
	size, ok := s.volumes[vol]
	if !ok {
		return nil, errf(http.StatusInternalServerError, "volume '%s' does not exist", vol)
	}
	st, _, _ := strings.Cut(vol, ":")
	return s.newTask(vm.Node, "qmmove", vm.VMID, func() {
		nv := s.freeDiskName(st, tid)
		delete(s.volumes, vol)
		s.volumes[nv] = size
		delete(vm.Config, disk)
		if unusedRe.MatchString(disk) {
			opts = "size=" + fmtSize(size)
		}
		if unusedRe.MatchString(targetDisk) {
			target.Config[targetDisk] = nv
		} else {
			target.Config[targetDisk] = nv + "," + opts
		}
	}), nil
}

// VM は vmid の VM の写しを返す。
func (s *Server) VM(vmid int) (VM, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.vms[vmid]
	if !ok {
		return VM{}, false
	}
	out := *v
	out.Config, out.Pending, out.Firewall = copyMap(v.Config), copyMap(v.Pending), copyMap(v.Firewall)
	out.Rules = copyRules(v.Rules)
	out.PendingDelete = map[string]bool{}
	for k := range v.PendingDelete {
		out.PendingDelete[k] = true
	}
	return out, true
}

// VMByName は名前の VM の写しを返す。
func (s *Server) VMByName(name string) (VM, bool) {
	s.mu.Lock()
	id := 0
	for _, v := range s.vms {
		if v.Name == name {
			id = v.VMID
		}
	}
	s.mu.Unlock()
	if id == 0 {
		return VM{}, false
	}
	return s.VM(id)
}

// SetConfig は設定を直に書く（手での変更。pending を通さない）。"" は消す。
func (s *Server) SetConfig(vmid int, key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.vms[vmid]; ok {
		if value == "" {
			delete(v.Config, key)
		} else {
			v.Config[key] = value
		}
	}
}

// SetFirewall は VM の FW の options を直に書く（手での変更）。"" は消す。
func (s *Server) SetFirewall(vmid int, key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.vms[vmid]; ok {
		if v.Firewall == nil {
			v.Firewall = map[string]string{}
		}
		if value == "" {
			delete(v.Firewall, key)
		} else {
			v.Firewall[key] = value
		}
	}
}

// SetRules は VM の FW の規則を直に置き換える（手での変更）。
func (s *Server) SetRules(vmid int, rules []map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.vms[vmid]; ok {
		v.Rules = copyRules(rules)
	}
}

// SetPending は走っている VM の設定を手で変えたときのように、key の値を
// pending に置く。
func (s *Server) SetPending(vmid int, key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.vms[vmid]; ok {
		v.Pending[key] = value
	}
}

// AddPool は pool を作る。
func (s *Server) AddPool(name string) { s.mu.Lock(); s.pools[name] = true; s.mu.Unlock() }

// SetPool は VM の pool を直に変える（手での変更。"" は外す）。
func (s *Server) SetPool(vmid int, pool string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.vms[vmid]; ok {
		v.Pool = pool
	}
}

// SetStatus は電源の状態を直に変える。
func (s *Server) SetStatus(vmid int, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.vms[vmid]; ok {
		v.Status = status
	}
}

// SetLock は VM の lock を直に変える（"" は外す）。
func (s *Server) SetLock(vmid int, lock string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.vms[vmid]; ok {
		v.Lock = lock
	}
}

// FinishTasks は走っている仕事をすべて終わらせる。
func (s *Server) FinishTasks() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tasks {
		s.finishLocked(t)
	}
}

// RunningTasks は走っている仕事の数を返す。
func (s *Server) RunningTasks() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, t := range s.tasks {
		if !t.done {
			n++
		}
	}
	return n
}

func copyRules(rules []map[string]string) []map[string]string {
	if rules == nil {
		return nil
	}
	out := make([]map[string]string, len(rules))
	for i, r := range rules {
		out[i] = copyMap(r)
	}
	return out
}

func copyMap(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ---- HTTP ----

type apiError struct {
	status int
	msg    string
}

func errf(status int, format string, args ...any) *apiError {
	return &apiError{status: status, msg: fmt.Sprintf(format, args...)}
}

var (
	vmRe   = regexp.MustCompile(`^/nodes/([^/]+)/qemu/([0-9]+)(/.*)?$`)
	taskRe = regexp.MustCompile(`^/nodes/([^/]+)/tasks/([^/]+)/status$`)
	diskRe = regexp.MustCompile(`^(scsi|virtio|sata|ide)[0-9]+$`)
	// unusedRe・ownerRe は外したままの volume のキーと volume の持ち主（#204）。
	unusedRe = regexp.MustCompile(`^unused[0-9]+$`)
	ownerRe  = regexp.MustCompile(`:(?:[0-9]+/)?(?:vm|base)-([0-9]+)-`)
	netRe    = regexp.MustCompile(`^net[0-9]+$`)
	models   = []string{"virtio", "e1000", "e1000e", "rtl8139", "vmxnet3"}
)

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	unreachable, fail := s.unreachable, s.failStatus
	s.mu.Unlock()
	if unreachable {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
				return
			}
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if fail != 0 {
		writeErr(w, errf(fail, "injected failure"))
		return
	}
	var tok *PoolToken
	auth := r.Header.Get("Authorization")
	kind := "WRITE"
	if r.Method == http.MethodGet {
		kind = "GET"
	}
	count := func(id string) {
		s.mu.Lock()
		if s.byToken[id] == nil {
			s.byToken[id] = map[string]int{}
		}
		s.byToken[id][kind]++
		s.mu.Unlock()
	}
	switch {
	case auth == "PVEAPIToken="+s.opts.TokenID+"="+s.opts.Secret:
		count(s.opts.TokenID)
	case s.opts.AuditTokenID != "" && auth == "PVEAPIToken="+s.opts.AuditTokenID+"="+s.opts.AuditSecret:
		count(s.opts.AuditTokenID)
		if r.Method != http.MethodGet {
			writeErr(w, errf(http.StatusForbidden, "Permission check failed (/, VM.Allocate)"))
			return
		}
	default:
		for i, pt := range s.opts.PoolTokens {
			if auth == "PVEAPIToken="+pt.TokenID+"="+pt.Secret {
				tok = &s.opts.PoolTokens[i]
				count(pt.TokenID)
			}
		}
		if tok == nil {
			writeErr(w, errf(http.StatusUnauthorized, "authentication failure"))
			return
		}
	}
	path, ok := strings.CutPrefix(r.URL.Path, s.opts.BasePath)
	if !ok {
		writeErr(w, errf(http.StatusNotImplemented, "Method '%s %s' not implemented", r.Method, r.URL.Path))
		return
	}
	if err := r.ParseForm(); err != nil {
		writeErr(w, errf(http.StatusBadRequest, "%v", err))
		return
	}
	data, aerr := s.route(tok, r.Method, path, r.Form)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func writeErr(w http.ResponseWriter, e *apiError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": nil, "message": e.msg + "\n"})
}

func (s *Server) route(tok *PoolToken, method, path string, form url.Values) (any, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if path == "/cluster/resources" && method == http.MethodGet {
		s.calls["resources"]++
		return s.resources(tok, form.Get("type")), nil
	}
	if path == "/pools" && method == http.MethodPut {
		s.calls["pools"]++
		return nil, s.updatePool(tok, form)
	}
	if m := taskRe.FindStringSubmatch(path); m != nil && method == http.MethodGet {
		s.calls["task"]++
		if s.offline[m[1]] {
			return nil, errf(595, "no route to host")
		}
		upid, _ := url.PathUnescape(m[2])
		t, ok := s.tasks[upid]
		if !ok || t.node != m[1] {
			return nil, errf(http.StatusInternalServerError, "no such task")
		}
		if !t.done {
			t.polls--
			if t.polls <= 0 {
				s.finishLocked(t)
			}
		}
		if !t.done {
			return map[string]any{"status": "running", "upid": upid}, nil
		}
		return map[string]any{"status": "stopped", "exitstatus": t.exit, "upid": upid}, nil
	}
	if parts := strings.Split(strings.Trim(path, "/"), "/"); len(parts) == 3 && parts[0] == "nodes" && parts[2] == "qemu" && method == http.MethodPost {
		s.calls["create"]++
		if !s.nodeKnown(parts[1]) {
			return nil, errf(http.StatusInternalServerError, "no such node %q", parts[1])
		}
		if s.offline[parts[1]] {
			return nil, errf(595, "no route to host")
		}
		if aerr := s.checkPoolParam(tok, form.Get("pool")); aerr != nil {
			return nil, aerr
		}
		return s.create(parts[1], form)
	}
	m := vmRe.FindStringSubmatch(path)
	if m == nil {
		return nil, errf(http.StatusNotImplemented, "Method '%s %s' not implemented", method, path)
	}
	node, rest := m[1], m[3]
	vmid, _ := strconv.Atoi(m[2])
	if s.offline[node] {
		return nil, errf(595, "no route to host")
	}
	vm, ok := s.vms[vmid]
	if tok != nil && !(ok && (vm.Pool == tok.Pool || (rest == "/clone" && containsInt(tok.Templates, vmid)))) {
		return nil, errf(http.StatusForbidden, "Permission check failed")
	}
	if !ok || vm.Node != node {
		return nil, errf(http.StatusInternalServerError, "Configuration file 'nodes/%s/qemu-server/%d.conf' does not exist", node, vmid)
	}
	switch {
	case rest == "/pending" && method == http.MethodGet:
		s.calls["pending"]++
		return s.pending(vm), nil
	case rest == "/config" && method == http.MethodGet:
		s.calls["config"]++
		out := map[string]any{}
		for k, v := range vm.Config {
			out[k] = num(k, v)
		}
		return out, nil
	case rest == "/status/current" && method == http.MethodGet:
		s.calls["status"]++
		out := map[string]any{"vmid": vm.VMID, "name": vm.Name, "status": vm.Status, "qmpstatus": vm.Status}
		if vm.Lock != "" {
			out["lock"] = vm.Lock
		}
		if vm.Template {
			out["template"] = 1
		}
		return out, nil
	case rest == "/config" && method == http.MethodPut:
		s.calls["config"]++
		return nil, s.setConfig(vm, form)
	case rest == "/firewall/options" && method == http.MethodGet:
		s.calls["firewall"]++
		out := map[string]any{"digest": "0000"}
		for k, v := range vm.Firewall {
			if n, err := strconv.Atoi(v); err == nil {
				out[k] = n
			} else {
				out[k] = v
			}
		}
		return out, nil
	case rest == "/firewall/options" && method == http.MethodPut:
		s.calls["firewall"]++
		return nil, s.setFirewall(vm, form)
	case rest == "/firewall/rules" && method == http.MethodGet:
		s.calls["fwrules"]++
		return s.listRules(vm), nil
	case rest == "/firewall/rules" && method == http.MethodPost:
		s.calls["fwrules"]++
		return nil, s.createRule(vm, form)
	case strings.HasPrefix(rest, "/firewall/rules/") && (method == http.MethodPut || method == http.MethodDelete):
		s.calls["fwrules"]++
		pos, err := strconv.Atoi(strings.TrimPrefix(rest, "/firewall/rules/"))
		if err != nil || pos < 0 {
			return nil, errf(http.StatusBadRequest, "pos: invalid integer value")
		}
		return nil, s.changeRule(vm, method, pos, form)
	case rest == "/clone" && method == http.MethodPost:
		s.calls["clone"]++
		if aerr := s.checkPoolParam(tok, form.Get("pool")); aerr != nil {
			return nil, aerr
		}
		return s.clone(vm, form)
	case rest == "/move_disk" && method == http.MethodPost:
		s.calls["move_disk"]++
		return s.moveDisk(vm, form)
	case rest == "/resize" && method == http.MethodPut:
		s.calls["resize"]++
		return s.resize(vm, form)
	case strings.HasPrefix(rest, "/status/") && method == http.MethodPost:
		action := strings.TrimPrefix(rest, "/status/")
		s.calls[action]++
		return s.power(vm, action)
	case rest == "" && method == http.MethodDelete:
		s.calls["destroy"]++
		if vm.Status == "running" {
			return nil, errf(http.StatusInternalServerError, "VM %d is running - destroy failed", vm.VMID)
		}
		if vm.Lock != "" {
			return nil, errf(http.StatusInternalServerError, "VM is locked (%s)", vm.Lock)
		}
		unreferenced := form.Get("destroy-unreferenced-disks") == "1"
		return s.newTask(vm.Node, "qmdestroy", vm.VMID, func() {
			for k, v := range vm.Config {
				vol, _, _ := strings.Cut(v, ",")
				if (diskRe.MatchString(k) || unusedRe.MatchString(k)) && volumeOwner(vol) == vmid {
					delete(s.volumes, vol)
				}
			}
			if unreferenced {
				for vol := range s.volumes {
					if volumeOwner(vol) == vmid {
						delete(s.volumes, vol)
					}
				}
			}
			delete(s.vms, vmid)
		}), nil
	}
	return nil, errf(http.StatusNotImplemented, "Method '%s %s' not implemented", method, path)
}

func (s *Server) nodeKnown(n string) bool {
	for _, x := range s.opts.Nodes {
		if x == n {
			return true
		}
	}
	return false
}

// checkPoolParam は作成・複製の pool を確かめる（本物と同じく、無い pool は
// 権限の検査で断る）。pool に絞ったトークンはその pool を渡したときだけ作れる。
func (s *Server) checkPoolParam(tok *PoolToken, pool string) *apiError {
	if pool != "" && !s.pools[pool] {
		return errf(http.StatusForbidden, "Permission check failed (pool '%s' does not exist)", pool)
	}
	if tok != nil && pool != tok.Pool {
		return errf(http.StatusForbidden, "Permission check failed")
	}
	return nil
}

// updatePool は PUT /pools（VM の出し入れだけ）。
func (s *Server) updatePool(tok *PoolToken, form url.Values) *apiError {
	pool := form.Get("poolid")
	if tok != nil && (pool != tok.Pool || !tok.PoolAllocate) {
		return errf(http.StatusForbidden, "Permission check failed (/pool/%s, Pool.Allocate)", pool)
	}
	if !s.pools[pool] {
		return errf(http.StatusInternalServerError, "update pools failed: pool '%s' does not exist", pool)
	}
	del, move := form.Get("delete") == "1", form.Get("allow-move") == "1"
	for _, f := range strings.FieldsFunc(form.Get("vms"), func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		vmid, _ := strconv.Atoi(f)
		vm, ok := s.vms[vmid]
		switch {
		case !ok:
			return errf(http.StatusInternalServerError, "update pools failed: guest '%s' does not exist", f)
		case del && vm.Pool != pool:
			return errf(http.StatusInternalServerError, "update pools failed: VM %d is not a pool member", vmid)
		case del:
			vm.Pool = ""
		case vm.Pool != "" && vm.Pool != pool && !move:
			return errf(http.StatusInternalServerError, "update pools failed: VM %d belongs already to pool '%s' and 'allow-move' is not set", vmid, vm.Pool)
		default:
			vm.Pool = pool
		}
	}
	return nil
}

func (s *Server) resources(tok *PoolToken, typ string) []map[string]any {
	s.resCalls++
	fresh := s.opts.ResourceLag <= 0 || s.resCalls%s.opts.ResourceLag == 0
	ids := make([]int, 0, len(s.vms))
	for id := range s.vms {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	out := []map[string]any{}
	for _, id := range ids {
		v := s.vms[id]
		if typ != "" && typ != "vm" {
			continue
		}
		if tok != nil && v.Pool != tok.Pool && !containsInt(tok.Templates, v.VMID) {
			continue
		}
		if fresh {
			v.rrdName, v.rrdStatus, v.rrdTemplate = v.Name, v.Status, v.Template
		}
		status := v.rrdStatus
		if s.offline[v.Node] {
			status = "unknown"
		}
		tmpl := 0
		if v.rrdTemplate {
			tmpl = 1
		}
		m := map[string]any{
			"id": fmt.Sprintf("qemu/%d", v.VMID), "type": "qemu", "vmid": v.VMID, "node": v.Node,
			"status": status, "template": tmpl, "maxmem": 0, "maxcpu": 0,
		}
		if v.rrdName != "" {
			m["name"] = v.rrdName
		}
		if v.Lock != "" {
			m["lock"] = v.Lock
		}
		if v.Pool != "" && (tok == nil || tok.PoolAudit) {
			m["pool"] = v.Pool
		}
		out = append(out, m)
	}
	return out
}

func num(key, v string) any {
	switch key {
	case "cores", "sockets", "vmid", "template", "ciupgrade":
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	case "memory":
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return v
}

func (s *Server) pending(vm *VM) []map[string]any {
	keys := map[string]bool{}
	for k := range vm.Config {
		keys[k] = true
	}
	for k := range vm.Pending {
		keys[k] = true
	}
	for k := range vm.PendingDelete {
		keys[k] = true
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	out := []map[string]any{}
	for _, k := range names {
		e := map[string]any{"key": k}
		if v, ok := vm.Config[k]; ok {
			e["value"] = num(k, v)
		}
		if v, ok := vm.Pending[k]; ok {
			e["pending"] = num(k, v)
		}
		if vm.PendingDelete[k] {
			e["delete"] = 1
		}
		out = append(out, e)
	}
	if vm.Lock != "" {
		out = append(out, map[string]any{"key": "lock", "value": vm.Lock})
	}
	return out
}

// normalize は設定の値を本物と同じ形に書き戻す（ディスクの切り出し・MAC）。
func (s *Server) normalize(vm *VM, key, value string) (string, *apiError) {
	switch {
	case diskRe.MatchString(key):
		vol, opts, _ := strings.Cut(value, ",")
		st, size, ok := strings.Cut(vol, ":")
		if !ok {
			return "", errf(http.StatusBadRequest, "%s: unable to parse drive options", key)
		}
		if size == "cloudinit" {
			return fmt.Sprintf("%s:vm-%d-cloudinit,media=cdrom", st, vm.VMID), nil
		}
		if n, err := strconv.Atoi(size); err == nil {
			out := s.freeDiskName(st, vm.VMID)
			s.volumes[out] = int64(n) << 30
			if opts != "" {
				out += "," + opts
			}
			return out + fmt.Sprintf(",size=%dG", n), nil
		}
		if !strings.Contains(opts, "media=cdrom") {
			// 在る volume を付ける（#204）。本物と同じく大きさを書き足す。
			bytesOf, ok := s.volumes[vol]
			if !ok {
				return "", errf(http.StatusInternalServerError, "%s: volume '%s' does not exist", key, vol)
			}
			if !strings.Contains(opts, "size=") {
				value += ",size=" + fmtSize(bytesOf)
			}
		}
		return value, nil
	case netRe.MatchString(key):
		var parts []string
		model, mac := "", ""
		for _, p := range strings.Split(value, ",") {
			k, v, _ := strings.Cut(p, "=")
			switch {
			case contains(models, k):
				model, mac = k, v
			case k == "model":
				model = v
			case k == "macaddr":
				mac = v
			default:
				parts = append(parts, p)
			}
		}
		if model == "" {
			return "", errf(http.StatusBadRequest, "%s: no model", key)
		}
		if mac == "" {
			s.macSeq++
			mac = fmt.Sprintf("BC:24:11:%02X:%02X:%02X", (s.macSeq>>16)&0xff, (s.macSeq>>8)&0xff, s.macSeq&0xff)
		}
		return strings.Join(append([]string{model + "=" + strings.ToUpper(mac)}, parts...), ","), nil
	}
	return value, nil
}

func (s *Server) create(node string, form url.Values) (any, *apiError) {
	vmid, err := strconv.Atoi(form.Get("vmid"))
	if err != nil {
		return nil, errf(http.StatusBadRequest, "vmid: type check failed")
	}
	if _, used := s.vms[vmid]; used {
		return nil, errf(http.StatusInternalServerError, "unable to create VM %d - VM %d already exists on node '%s'", vmid, vmid, s.vms[vmid].Node)
	}
	vm := &VM{VMID: vmid, Node: node, Name: form.Get("name"), Status: "stopped", Config: map[string]string{},
		Pending: map[string]string{}, PendingDelete: map[string]bool{}, Lock: "create", Pool: form.Get("pool")}
	keys := sortedKeys(form)
	for _, k := range keys {
		if k == "vmid" || k == "pool" {
			continue
		}
		v, aerr := s.normalize(vm, k, form.Get(k))
		if aerr != nil {
			return nil, aerr
		}
		vm.Config[k] = v
	}
	s.vms[vmid] = vm
	return s.newTask(node, "qmcreate", vmid, func() { vm.Lock = "" }), nil
}

func (s *Server) setConfig(vm *VM, form url.Values) *apiError {
	if vm.Lock != "" {
		return errf(http.StatusInternalServerError, "VM is locked (%s)", vm.Lock)
	}
	cold := func(k string) bool { return vm.Status == "running" && contains(s.opts.ColdKeys, k) }
	for _, k := range sortedKeys(form) {
		if k == "delete" || k == "digest" {
			continue
		}
		v, aerr := s.normalize(vm, k, form.Get(k))
		if aerr != nil {
			return aerr
		}
		if diskRe.MatchString(k) {
			// #204: 付け直した volume の unusedN は消え、置き換えた持ち物は unusedN へ。
			vol, _, _ := strings.Cut(v, ",")
			for uk, uv := range vm.Config {
				if unusedRe.MatchString(uk) && uv == vol {
					delete(vm.Config, uk)
				}
			}
			if old, ok := vm.Config[k]; ok && !strings.HasPrefix(old, vol+",") && old != vol {
				s.detachDrive(vm, k)
			}
		}
		if cold(k) {
			// 本物と同じく、今の値と同じ pending は残さない（cleanup_pending）。
			if vm.Config[k] == v {
				delete(vm.Pending, k)
			} else {
				vm.Pending[k] = v
			}
			delete(vm.PendingDelete, k)
			continue
		}
		vm.Config[k] = v
		if k == "name" {
			vm.Name = v
		}
	}
	for _, k := range strings.Split(form.Get("delete"), ",") {
		if k = strings.TrimSpace(k); k == "" {
			continue
		}
		if cold(k) {
			vm.PendingDelete[k] = true
			delete(vm.Pending, k)
			continue
		}
		if diskRe.MatchString(k) || unusedRe.MatchString(k) {
			s.detachDrive(vm, k)
			continue
		}
		delete(vm.Config, k)
	}
	return nil
}

var fwKeys = map[string]*regexp.Regexp{
	"enable": regexp.MustCompile(`^[01]$`), "ipfilter": regexp.MustCompile(`^[01]$`), "macfilter": regexp.MustCompile(`^[01]$`),
	"policy_in": regexp.MustCompile(`^(ACCEPT|DROP|REJECT)$`), "policy_out": regexp.MustCompile(`^(ACCEPT|DROP|REJECT)$`),
}

// setFirewall は PUT /firewall/options（同期。渡さないキーはそのまま）。
func (s *Server) setFirewall(vm *VM, form url.Values) *apiError {
	if vm.Firewall == nil {
		vm.Firewall = map[string]string{}
	}
	for _, k := range sortedKeys(form) {
		if k == "delete" || k == "digest" {
			continue
		}
		re, ok := fwKeys[k]
		if !ok {
			return errf(http.StatusBadRequest, "property is not defined in schema and the schema does not allow additional properties: %s", k)
		}
		if !re.MatchString(form.Get(k)) {
			return errf(http.StatusBadRequest, "%s: value '%s' does not have a value in the enumeration", k, form.Get(k))
		}
	}
	for _, k := range sortedKeys(form) {
		if k != "delete" && k != "digest" {
			vm.Firewall[k] = form.Get(k)
		}
	}
	for _, k := range strings.Split(form.Get("delete"), ",") {
		delete(vm.Firewall, strings.TrimSpace(k))
	}
	return nil
}

// ruleKeys は規則の欄（type・action のほか）。
var ruleKeys = map[string]*regexp.Regexp{
	"iface": netRe, "source": regexp.MustCompile(`^[A-Za-z0-9+_.:/,-]+$`), "dest": regexp.MustCompile(`^[A-Za-z0-9+_.:/,-]+$`),
	"proto": regexp.MustCompile(`^[a-z0-9-]+$`), "dport": regexp.MustCompile(`^[A-Za-z0-9:,-]+$`), "sport": regexp.MustCompile(`^[A-Za-z0-9:,-]+$`),
	"icmp-type": regexp.MustCompile(`^[a-z0-9-]+$`), "macro": regexp.MustCompile(`^[A-Za-z0-9_-]+$`),
	"log": regexp.MustCompile(`^(emerg|alert|crit|err|warning|notice|info|debug|nolog)$`), "enable": regexp.MustCompile(`^[01]$`),
	"comment": regexp.MustCompile(`^[^\n]*$`),
}

// rulesDigest は並び全体の digest（本物と同じく並びが変われば変わる）。
func rulesDigest(rules []map[string]string) string {
	h := sha1.New()
	for _, r := range rules {
		for _, k := range sortedKeysMap(r) {
			fmt.Fprintf(h, "%s:%s\n", k, r[k])
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Server) listRules(vm *VM) []map[string]any {
	digest := rulesDigest(vm.Rules)
	out := []map[string]any{}
	for i, r := range vm.Rules {
		e := map[string]any{"pos": i, "digest": digest}
		for k, v := range r {
			if k == "enable" {
				n, _ := strconv.Atoi(v)
				e[k] = n
			} else {
				e[k] = v
			}
		}
		out = append(out, e)
	}
	return out
}

// setRuleFields は form の欄を r に写す（確かめてから）。
func setRuleFields(r map[string]string, form url.Values) *apiError {
	for _, k := range sortedKeys(form) {
		v := form.Get(k)
		switch k {
		case "digest", "delete", "moveto", "pos":
			continue
		case "type":
			if v != "in" && v != "out" && v != "group" {
				return errf(http.StatusBadRequest, "type: value '%s' does not have a value in the enumeration 'in, out, forward, group'", v)
			}
		case "action":
			if v != "ACCEPT" && v != "DROP" && v != "REJECT" {
				return errf(http.StatusBadRequest, "action: value does not match the regex pattern")
			}
		default:
			re, ok := ruleKeys[k]
			if !ok {
				return errf(http.StatusBadRequest, "property is not defined in schema and the schema does not allow additional properties: %s", k)
			}
			if !re.MatchString(v) {
				return errf(http.StatusBadRequest, "%s: value '%s' does not match the format", k, v)
			}
		}
	}
	for _, k := range sortedKeys(form) {
		switch k {
		case "digest", "delete", "moveto", "pos":
		default:
			r[k] = form.Get(k)
		}
	}
	if (r["dport"] != "" || r["sport"] != "") && r["proto"] == "" && r["macro"] == "" {
		return errf(http.StatusBadRequest, "missing property - 'dport' requires 'proto'")
	}
	return nil
}

// createRule は POST /firewall/rules：先頭に入れる、enable の既定は 0。
func (s *Server) createRule(vm *VM, form url.Values) *apiError {
	if form.Get("type") == "" || form.Get("action") == "" {
		return errf(http.StatusBadRequest, "type and action are required")
	}
	r := map[string]string{}
	if aerr := setRuleFields(r, form); aerr != nil {
		return aerr
	}
	if r["enable"] == "" {
		r["enable"] = "0"
	}
	vm.Rules = append([]map[string]string{r}, vm.Rules...)
	return nil
}

// changeRule は PUT・DELETE /firewall/rules/<pos>。
func (s *Server) changeRule(vm *VM, method string, pos int, form url.Values) *apiError {
	if d := form.Get("digest"); d != "" && d != rulesDigest(vm.Rules) {
		return errf(http.StatusInternalServerError, "detected modified configuration - file changed by other user? Try again.")
	}
	if pos >= len(vm.Rules) {
		return errf(http.StatusBadRequest, "no rule at position %d", pos)
	}
	if method == http.MethodDelete {
		vm.Rules = append(vm.Rules[:pos:pos], vm.Rules[pos+1:]...)
		return nil
	}
	if mv := form.Get("moveto"); mv != "" {
		to, err := strconv.Atoi(mv)
		if err != nil || to < 0 {
			return errf(http.StatusBadRequest, "moveto: invalid integer value")
		}
		if to == pos {
			return nil
		}
		moved := vm.Rules[pos]
		var out []map[string]string
		for i, r := range vm.Rules {
			if i == pos {
				continue
			}
			if i == to {
				out = append(out, moved)
			}
			out = append(out, r)
		}
		if to >= len(vm.Rules) {
			out = append(out, moved)
		}
		vm.Rules = out
		return nil
	}
	r := copyMap(vm.Rules[pos])
	if aerr := setRuleFields(r, form); aerr != nil {
		return aerr
	}
	for _, k := range strings.Split(form.Get("delete"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			delete(r, k)
		}
	}
	vm.Rules[pos] = r
	return nil
}

func (s *Server) applyPending(vm *VM) {
	for k, v := range vm.Pending {
		vm.Config[k] = v
	}
	for k := range vm.PendingDelete {
		delete(vm.Config, k)
	}
	vm.Pending, vm.PendingDelete = map[string]string{}, map[string]bool{}
}

func (s *Server) clone(src *VM, form url.Values) (any, *apiError) {
	if !src.Template {
		return nil, errf(http.StatusInternalServerError, "VM %d is not a template (full clone of a running VM is not supported here)", src.VMID)
	}
	newid, err := strconv.Atoi(form.Get("newid"))
	if err != nil {
		return nil, errf(http.StatusBadRequest, "newid: type check failed")
	}
	if _, used := s.vms[newid]; used {
		return nil, errf(http.StatusInternalServerError, "unable to create VM %d: config file already exists", newid)
	}
	target := form.Get("target")
	if target == "" {
		target = src.Node
	}
	vm := &VM{VMID: newid, Node: target, Name: form.Get("name"), Status: "stopped", Config: map[string]string{},
		Pending: map[string]string{}, PendingDelete: map[string]bool{}, Lock: "clone", Pool: form.Get("pool")}
	storage := form.Get("storage")
	for _, k := range sortedKeysMap(src.Config) {
		v := src.Config[k]
		switch {
		case k == "template":
			continue
		case k == "name":
			v = vm.Name
		case diskRe.MatchString(k):
			v = strings.ReplaceAll(v, fmt.Sprintf("vm-%d-", src.VMID), fmt.Sprintf("vm-%d-", newid))
			v = strings.ReplaceAll(v, fmt.Sprintf("base-%d-", src.VMID), fmt.Sprintf("vm-%d-", newid))
			if storage != "" && !strings.Contains(v, "media=cdrom") {
				if _, rest, ok := strings.Cut(v, ":"); ok {
					v = storage + ":" + rest
				}
			}
		case netRe.MatchString(k):
			var parts []string
			for _, p := range strings.Split(v, ",") {
				pk, _, _ := strings.Cut(p, "=")
				if contains(models, pk) {
					p = pk
				}
				parts = append(parts, p)
			}
			nv, aerr := s.normalize(vm, k, strings.Join(parts, ","))
			if aerr != nil {
				return nil, aerr
			}
			v = nv
		}
		vm.Config[k] = v
		s.registerVolume(k, v)
	}
	s.vms[newid] = vm
	return s.newTask(src.Node, "qmclone", src.VMID, func() { vm.Lock = "" }), nil
}

func (s *Server) resize(vm *VM, form url.Values) (any, *apiError) {
	disk, size := form.Get("disk"), form.Get("size")
	cur, ok := vm.Config[disk]
	if !ok {
		return nil, errf(http.StatusBadRequest, "disk '%s' does not exist", disk)
	}
	var parts []string
	old := int64(0)
	for _, p := range strings.Split(cur, ",") {
		if v, ok := strings.CutPrefix(p, "size="); ok {
			old = bytes(v)
			continue
		}
		parts = append(parts, p)
	}
	n := bytes(strings.TrimPrefix(size, "+"))
	if strings.HasPrefix(size, "+") {
		n += old
	}
	if n < old {
		return nil, errf(http.StatusInternalServerError, "shrinking disks is not supported")
	}
	parts = append(parts, "size="+fmtSize(n))
	vm.Config[disk] = strings.Join(parts, ",")
	if vol, _, _ := strings.Cut(cur, ","); s.volumes != nil {
		if _, ok := s.volumes[vol]; ok {
			s.volumes[vol] = n
		}
	}
	return s.newTask(vm.Node, "resize", vm.VMID, func() {}), nil
}

func (s *Server) power(vm *VM, action string) (any, *apiError) {
	if vm.Lock != "" {
		return nil, errf(http.StatusInternalServerError, "VM is locked (%s)", vm.Lock)
	}
	if vm.Template {
		return nil, errf(http.StatusInternalServerError, "VM %d is a template", vm.VMID)
	}
	switch action {
	case "start":
		if vm.Status == "running" {
			return nil, errf(http.StatusInternalServerError, "VM %d already running", vm.VMID)
		}
		return s.newTask(vm.Node, "qmstart", vm.VMID, func() { s.applyPending(vm); vm.Status = "running" }), nil
	case "shutdown", "stop":
		return s.newTask(vm.Node, "qm"+action, vm.VMID, func() { vm.Status = "stopped" }), nil
	case "reboot":
		if vm.Status != "running" {
			return nil, errf(http.StatusInternalServerError, "VM %d not running", vm.VMID)
		}
		return s.newTask(vm.Node, "qmreboot", vm.VMID, func() { s.applyPending(vm) }), nil
	case "reset":
		if vm.Status != "running" {
			return nil, errf(http.StatusInternalServerError, "VM %d not running", vm.VMID)
		}
		return s.newTask(vm.Node, "qmreset", vm.VMID, func() {}), nil
	}
	return nil, errf(http.StatusNotImplemented, "no such action %q", action)
}

func (s *Server) newTask(node, kind string, vmid int, finish func()) string {
	s.seq++
	upid := fmt.Sprintf("UPID:%s:%08X:%08X:%08X:%s:%d:%s:", node, 1000+s.seq, s.seq, time.Now().Unix(), kind, vmid, s.opts.TokenID)
	t := &task{node: node, polls: s.opts.TaskPolls, finish: finish, exit: "OK"}
	if msg, ok := s.failNext[kind]; ok {
		delete(s.failNext, kind)
		t.exit, t.finish = msg, func() {}
	}
	s.tasks[upid] = t
	if s.opts.TaskPolls <= 0 {
		s.finishLocked(t)
	}
	return upid
}

func (s *Server) finishLocked(t *task) {
	if t.done {
		return
	}
	t.done = true
	t.finish()
}

func sortedKeys(form url.Values) []string {
	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedKeysMap(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func bytes(s string) int64 {
	mult := int64(1)
	for suf, m := range map[string]int64{"K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40} {
		if strings.HasSuffix(s, suf) {
			mult, s = m, strings.TrimSuffix(s, suf)
		}
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	return n * mult
}

func fmtSize(b int64) string {
	for _, u := range []struct {
		s string
		n int64
	}{{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}} {
		if b > 0 && b%u.n == 0 {
			return strconv.FormatInt(b/u.n, 10) + u.s
		}
	}
	return strconv.FormatInt(b, 10)
}

func containsInt(xs []int, x int) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
