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
//
// テストから: 現実を手で変える（SetConfig・SetStatus・SetLock・SetPool）、ノードを落とす
// （SetNodeOffline：資源の status が unknown、ノードの API が 595）、届かなく
// する（SetUnreachable）、次の仕事を失敗させる（FailNextTask）。
package pvetest

import (
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
		failNext: map[string]string{}, calls: map[string]int{}, byToken: map[string]map[string]int{}}
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
// "start"・"shutdown"・"stop"・"reboot"・"reset"・"destroy"・"task"・"pools"）ごとの
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
	vm.rrdName, vm.rrdStatus, vm.rrdTemplate = vm.Name, vm.Status, vm.Template
	s.vms[vm.VMID] = &vm
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
	out.Config, out.Pending = copyMap(v.Config), copyMap(v.Pending)
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
	netRe  = regexp.MustCompile(`^net[0-9]+$`)
	models = []string{"virtio", "e1000", "e1000e", "rtl8139", "vmxnet3"}
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
	case rest == "/clone" && method == http.MethodPost:
		s.calls["clone"]++
		if aerr := s.checkPoolParam(tok, form.Get("pool")); aerr != nil {
			return nil, aerr
		}
		return s.clone(vm, form)
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
		return s.newTask(vm.Node, "qmdestroy", vm.VMID, func() { delete(s.vms, vmid) }), nil
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
			disk := 0
			for _, v := range vm.Config {
				if strings.Contains(v, fmt.Sprintf("vm-%d-disk-", vm.VMID)) {
					disk++
				}
			}
			out := fmt.Sprintf("%s:vm-%d-disk-%d", st, vm.VMID, disk)
			if opts != "" {
				out += "," + opts
			}
			return out + fmt.Sprintf(",size=%dG", n), nil
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
		delete(vm.Config, k)
	}
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
