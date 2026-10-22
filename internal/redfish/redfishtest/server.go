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

// Package redfishtest は httptest.NewTLSServer で立てる偽の BMC。qemu-bmc
// v0.8.0 の Redfish の振る舞い（§29.1 の統合テストの相手）を真似て、
// redfish のクライアントと provider の単体テスト、および統合テストの代役に使う。
//
// 真似る振る舞い:
//
//   - /redfish/v1/Systems/1: PowerState（On / Off / PoweringOff / PoweringOn）・
//     UUID・MemorySummary・ProcessorSummary・EthernetInterfaces・Boot・
//     Actions の #ComputerSystem.Reset（ResetType@Redfish.AllowableValues）。
//   - Reset（204）: On・ForceOff・GracefulShutdown・ForceRestart・
//     GracefulRestart。GracefulShutdown は PoweringOff を経て GracefulDelay で
//     Off になる。SetIgnoreGraceful(true)（OS が ACPI に応えない、BIOS のままの
//     VM）なら GracefulTimeout（既定 120 秒）で On に戻る。PoweringOff の途中で
//     別の電源操作が来たら打ち切ってそれに従う。
//   - Boot の PATCH: Once は次の起動（Off → On・ForceRestart・GracefulRestart）
//     で Disabled・None に戻る。Continuous は残る。Disabled でも Target は設定
//     した値のまま返す。
//   - /redfish/v1/Managers/1/VirtualMedia/CD1: InsertMedia・EjectMedia（204）。
//     電源オフ中も残る。SetMediaFetchFails(true) なら入れたあと
//     MediaFetchDelay で Inserted が false に戻る（Image は残す）。
//
// 時刻は Options.Now で差し替えられる（状態の移り変わりは要求を受けたときに
// その時刻で進めるので、goroutine を持たない）。
//
// 試験用の口: SetUnreachable（接続を切って到達不能にする）・SetFailStatus
// （すべての要求に 5xx などを返す）・SetDelay（応答を遅らせる）・Requests
// （受けた要求の記録）。
package redfishtest

import (
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// パス。
const (
	SystemPath       = "/redfish/v1/Systems/1"
	ManagerPath      = "/redfish/v1/Managers/1"
	VirtualMediaPath = "/redfish/v1/Managers/1/VirtualMedia/CD1"
	ResetPath        = SystemPath + "/Actions/ComputerSystem.Reset"
	InsertMediaPath  = VirtualMediaPath + "/Actions/VirtualMedia.InsertMedia"
	EjectMediaPath   = VirtualMediaPath + "/Actions/VirtualMedia.EjectMedia"
)

// Options は偽 BMC の初期値。ゼロ値の項目は既定値になる。
type Options struct {
	// Username・Password は Basic 認証の資格（既定 admin / password）。
	Username string
	Password string
	// PowerState は最初の電源状態（既定 On）。
	PowerState string
	// UUID・MACs・MemoryGiB・CPUs は qemu-bmc の SYSTEM_UUID・VM_MACS・
	// VM_MEMORY・VM_CPUS に当たる観測値。
	UUID      string
	MACs      []string
	MemoryGiB float64
	CPUs      int
	// BootMode は Boot.BootSourceOverrideMode（既定 UEFI）。
	BootMode string
	// BootTargets は BootSourceOverrideTarget@Redfish.AllowableValues。
	BootTargets []string
	// ResetTypes は ResetType@Redfish.AllowableValues。
	ResetTypes []string
	// GracefulDelay は GracefulShutdown・GracefulRestart で OS が応えたときに
	// PoweringOff が続く時間（既定 0 = 次に観測したときには終わっている）。
	GracefulDelay time.Duration
	// GracefulTimeout は OS が応えないとき PoweringOff から On に戻るまでの時間
	// （qemu-bmc の GRACEFUL_TIMEOUT、既定 120 秒）。
	GracefulTimeout time.Duration
	// PowerOnDelay は Off からの On で PoweringOn が続く時間（既定 0）。
	PowerOnDelay time.Duration
	// MediaFetchDelay は SetMediaFetchFails のとき Inserted が false に戻るまでの
	// 時間（既定 0）。
	MediaFetchDelay time.Duration
	// Now は時刻（既定 time.Now）。
	Now func() time.Time
}

// Request は受けた要求の記録。
type Request struct {
	Method string
	Path   string
	Body   string
}

// Server は偽 BMC。
type Server struct {
	*httptest.Server

	opts Options

	mu sync.Mutex
	// 電源。
	power      string
	transition string    // 進行中の Reset（GracefulShutdown / GracefulRestart / On）
	transStart time.Time // transition を始めた時刻
	boots      int
	// 起動先の上書き。
	bootEnabled string
	bootTarget  string
	// 仮想メディア。
	inserted       bool
	image          string
	insertedAt     time.Time
	mediaFetchFail bool
	// 試験用の口。
	ignoreGraceful bool
	unreachable    bool
	failStatus     int
	delay          time.Duration
	requests       []Request
}

// New は偽 BMC を立てる。終わったら Close する。
func New(opts Options) *Server {
	if opts.Username == "" {
		opts.Username = "admin"
	}
	if opts.Password == "" {
		opts.Password = "password"
	}
	if opts.PowerState == "" {
		opts.PowerState = "On"
	}
	if opts.UUID == "" {
		opts.UUID = "4c4c4544-0000-1000-8000-000000000001"
	}
	if opts.MACs == nil {
		opts.MACs = []string{"52:54:00:12:34:56"}
	}
	if opts.MemoryGiB == 0 {
		opts.MemoryGiB = 2
	}
	if opts.CPUs == 0 {
		opts.CPUs = 2
	}
	if opts.BootMode == "" {
		opts.BootMode = "UEFI"
	}
	if opts.BootTargets == nil {
		opts.BootTargets = []string{"None", "Pxe", "Hdd", "Cd", "BiosSetup"}
	}
	if opts.ResetTypes == nil {
		opts.ResetTypes = []string{"On", "ForceOff", "GracefulShutdown", "ForceRestart", "GracefulRestart"}
	}
	if opts.GracefulTimeout == 0 {
		opts.GracefulTimeout = 120 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Server{
		opts:        opts,
		power:       opts.PowerState,
		bootEnabled: "Disabled",
		bootTarget:  "None",
	}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	// 証明書を拒むクライアントの TLS ハンドシェイク失敗をログに撒かない。
	s.Server.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.Server.StartTLS()
	return s
}

// CAPEM は偽 BMC の証明書（自己署名）を PEM で返す。
func (s *Server) CAPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})
}

// WriteCAFile は CAPEM を dir/ca.pem に書き、そのパスを返す。
func (s *Server) WriteCAFile(dir string) (string, error) {
	p := filepath.Join(dir, "ca.pem")
	return p, os.WriteFile(p, s.CAPEM(), 0o600)
}

// ---- 試験用の口 ----

// SetUnreachable は true のあいだ、受けた接続を応答せずに切る（BMC に届かない）。
func (s *Server) SetUnreachable(v bool) { s.mu.Lock(); s.unreachable = v; s.mu.Unlock() }

// SetFailStatus は 0 でないあいだ、すべての要求にその状態コードを返す。
func (s *Server) SetFailStatus(code int) { s.mu.Lock(); s.failStatus = code; s.mu.Unlock() }

// SetDelay は応答を d だけ遅らせる（クライアントの時間切れの試験用）。
func (s *Server) SetDelay(d time.Duration) { s.mu.Lock(); s.delay = d; s.mu.Unlock() }

// SetIgnoreGraceful は OS が GracefulShutdown に応えないかを決める。
func (s *Server) SetIgnoreGraceful(v bool) { s.mu.Lock(); s.ignoreGraceful = v; s.mu.Unlock() }

// SetMediaFetchFails は以後の InsertMedia で image の取得に失敗するかを決める。
func (s *Server) SetMediaFetchFails(v bool) { s.mu.Lock(); s.mediaFetchFail = v; s.mu.Unlock() }

// SetPowerState は電源状態を直接変える（進行中の操作は捨てる）。
func (s *Server) SetPowerState(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.power, s.transition = state, ""
}

// SetBoot は起動先の上書きを直接変える（人が BMC の画面で変えた、の代役）。
func (s *Server) SetBoot(enabled, target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bootEnabled, s.bootTarget = enabled, target
}

// PowerState は今の電源状態を返す。
func (s *Server) PowerState() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.advance()
	return s.power
}

// Boot は今の起動先の上書きを返す。
func (s *Server) Boot() (enabled, target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.advance()
	return s.bootEnabled, s.bootTarget
}

// Media は今の仮想メディアを返す。
func (s *Server) Media() (inserted bool, image string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.advance()
	return s.inserted, s.image
}

// Boots は起動した回数（Off → On・再起動）を返す。
func (s *Server) Boots() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.advance()
	return s.boots
}

// Requests は受けた要求（到達不能・失敗の口で返したものも含む）の写しを返す。
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// ClearRequests は要求の記録を消す。
func (s *Server) ClearRequests() { s.mu.Lock(); s.requests = nil; s.mu.Unlock() }

// ---- 状態の移り変わり（s.mu を持って呼ぶ） ----

// boot は電源が入って OS が起動し始めたときの処理。Once は 1 回で戻る。
func (s *Server) boot() {
	s.boots++
	if s.bootEnabled == "Once" {
		s.bootEnabled, s.bootTarget = "Disabled", "None"
	}
}

// advance は進行中の操作を今の時刻まで進める。
func (s *Server) advance() {
	now := s.opts.Now()
	switch s.transition {
	case "GracefulShutdown", "GracefulRestart":
		el := now.Sub(s.transStart)
		switch {
		case !s.ignoreGraceful && el >= s.opts.GracefulDelay:
			if s.transition == "GracefulRestart" {
				s.power = "On"
				s.boot()
			} else {
				s.power = "Off"
			}
			s.transition = ""
		case s.ignoreGraceful && el >= s.opts.GracefulTimeout:
			s.power, s.transition = "On", ""
		}
	case "On":
		if now.Sub(s.transStart) >= s.opts.PowerOnDelay {
			s.power, s.transition = "On", ""
		}
	}
	if s.inserted && s.mediaFetchFail && now.Sub(s.insertedAt) >= s.opts.MediaFetchDelay {
		s.inserted = false
	}
}

// reset は ResetType を受ける。途中の操作は打ち切る。
func (s *Server) reset(t string) {
	s.advance()
	interrupted := s.transition != ""
	if interrupted && s.power == "PoweringOff" {
		// PoweringOff のあいだ VM はまだ動いている。
		s.power = "On"
	}
	s.transition = ""
	now := s.opts.Now()
	switch t {
	case "On":
		if s.power == "Off" {
			s.power, s.transition, s.transStart = "PoweringOn", "On", now
			s.boot()
			s.advance()
		} else if s.power == "PoweringOn" {
			s.power = "On"
		}
	case "ForceOff":
		s.power = "Off"
	case "GracefulShutdown", "GracefulRestart":
		if s.power == "Off" {
			return
		}
		s.power, s.transition, s.transStart = "PoweringOff", t, now
	case "ForceRestart":
		s.power = "On"
		s.boot()
	}
}

// ---- HTTP ----

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))

	s.mu.Lock()
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, Body: string(body)})
	unreachable, failStatus, delay := s.unreachable, s.failStatus, s.delay
	s.mu.Unlock()

	if unreachable {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
				return
			}
		}
		panic(http.ErrAbortHandler)
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if failStatus != 0 {
		writeError(w, failStatus, "Base.1.0.InternalError", "injected failure")
		return
	}
	if u, p, ok := r.BasicAuth(); !ok || u != s.opts.Username || p != s.opts.Password {
		w.Header().Set("WWW-Authenticate", `Basic realm="qemu-bmc"`)
		writeError(w, http.StatusUnauthorized, "Base.1.0.InsufficientPrivilege", "authentication required")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.advance()

	path := strings.TrimSuffix(r.URL.Path, "/")
	switch {
	case r.Method == http.MethodGet:
		doc, ok := s.document(path)
		if !ok {
			writeError(w, http.StatusNotFound, "Base.1.0.ResourceMissingAtURI", "no resource at "+path)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	case r.Method == http.MethodPost && path == ResetPath:
		s.postReset(w, body)
	case r.Method == http.MethodPatch && path == SystemPath:
		s.patchSystem(w, body)
	case r.Method == http.MethodPost && path == InsertMediaPath:
		s.postInsert(w, body)
	case r.Method == http.MethodPost && path == EjectMediaPath:
		s.inserted, s.image = false, ""
		w.WriteHeader(http.StatusNoContent)
	default:
		if _, ok := s.document(path); ok {
			writeError(w, http.StatusMethodNotAllowed, "Base.1.0.ActionNotSupported", r.Method+" not allowed on "+path)
			return
		}
		writeError(w, http.StatusNotFound, "Base.1.0.ResourceMissingAtURI", "no resource at "+path)
	}
}

func (s *Server) postReset(w http.ResponseWriter, body []byte) {
	var req struct{ ResetType string }
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Base.1.0.MalformedJSON", err.Error())
		return
	}
	if !contains(s.opts.ResetTypes, req.ResetType) {
		writeError(w, http.StatusBadRequest, "Base.1.0.ActionParameterValueNotInList",
			fmt.Sprintf("ResetType %q is not supported", req.ResetType))
		return
	}
	s.reset(req.ResetType)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) patchSystem(w http.ResponseWriter, body []byte) {
	var req struct {
		Boot *struct {
			Enabled *string `json:"BootSourceOverrideEnabled"`
			Target  *string `json:"BootSourceOverrideTarget"`
		}
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Base.1.0.MalformedJSON", err.Error())
		return
	}
	if req.Boot == nil {
		writeError(w, http.StatusBadRequest, "Base.1.0.PropertyMissing", "only Boot can be patched")
		return
	}
	if e := req.Boot.Enabled; e != nil && !contains([]string{"Disabled", "Once", "Continuous"}, *e) {
		writeError(w, http.StatusBadRequest, "Base.1.0.PropertyValueNotInList",
			fmt.Sprintf("BootSourceOverrideEnabled %q is not supported", *e))
		return
	}
	if t := req.Boot.Target; t != nil && !contains(s.opts.BootTargets, *t) {
		writeError(w, http.StatusBadRequest, "Base.1.0.PropertyValueNotInList",
			fmt.Sprintf("BootSourceOverrideTarget %q is not supported", *t))
		return
	}
	if e := req.Boot.Enabled; e != nil {
		s.bootEnabled = *e
	}
	if t := req.Boot.Target; t != nil {
		s.bootTarget = *t
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) postInsert(w http.ResponseWriter, body []byte) {
	var req struct{ Image string }
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Base.1.0.MalformedJSON", err.Error())
		return
	}
	img := req.Image
	if img == "" || !(strings.HasPrefix(img, "/") || strings.HasPrefix(img, "http://") || strings.HasPrefix(img, "https://")) {
		writeError(w, http.StatusBadRequest, "Base.1.0.PropertyValueFormatError",
			fmt.Sprintf("Image %q must be an absolute path or an http(s) URL", img))
		return
	}
	if s.inserted {
		writeError(w, http.StatusBadRequest, "Base.1.0.ActionNotSupported", "media is already inserted; eject first")
		return
	}
	s.inserted, s.image, s.insertedAt = true, img, s.opts.Now()
	s.advance()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) document(path string) (any, bool) {
	ref := func(p string) map[string]string { return map[string]string{"@odata.id": p} }
	coll := func(p string, members ...string) map[string]any {
		ms := []any{}
		for _, m := range members {
			ms = append(ms, ref(m))
		}
		return map[string]any{"@odata.id": p, "Members": ms, "Members@odata.count": len(ms)}
	}
	switch path {
	case "/redfish/v1":
		return map[string]any{
			"@odata.id": "/redfish/v1", "RedfishVersion": "1.6.0",
			"Systems": ref("/redfish/v1/Systems"), "Managers": ref("/redfish/v1/Managers"),
		}, true
	case "/redfish/v1/Systems":
		return coll(path, SystemPath), true
	case SystemPath:
		return map[string]any{
			"@odata.id":          SystemPath,
			"Id":                 "1",
			"PowerState":         s.power,
			"UUID":               s.opts.UUID,
			"MemorySummary":      map[string]any{"TotalSystemMemoryGiB": s.opts.MemoryGiB},
			"ProcessorSummary":   map[string]any{"Count": s.opts.CPUs},
			"EthernetInterfaces": ref(SystemPath + "/EthernetInterfaces"),
			"Boot": map[string]any{
				"BootSourceOverrideEnabled":                        s.bootEnabled,
				"BootSourceOverrideTarget":                         s.bootTarget,
				"BootSourceOverrideMode":                           s.opts.BootMode,
				"BootSourceOverrideTarget@Redfish.AllowableValues": s.opts.BootTargets,
			},
			"Actions": map[string]any{
				"#ComputerSystem.Reset": map[string]any{
					"target":                            ResetPath,
					"ResetType@Redfish.AllowableValues": s.opts.ResetTypes,
				},
			},
		}, true
	case SystemPath + "/EthernetInterfaces":
		var ms []string
		for i := range s.opts.MACs {
			ms = append(ms, fmt.Sprintf("%s/EthernetInterfaces/%d", SystemPath, i+1))
		}
		return coll(path, ms...), true
	case "/redfish/v1/Managers":
		return coll(path, ManagerPath), true
	case ManagerPath:
		return map[string]any{"@odata.id": ManagerPath, "Id": "1", "VirtualMedia": ref(ManagerPath + "/VirtualMedia")}, true
	case ManagerPath + "/VirtualMedia":
		return coll(path, VirtualMediaPath), true
	case VirtualMediaPath:
		return map[string]any{
			"@odata.id":  VirtualMediaPath,
			"Id":         "CD1",
			"MediaTypes": []string{"CD", "DVD"},
			"Inserted":   s.inserted,
			"Image":      s.image,
			"Actions": map[string]any{
				"#VirtualMedia.InsertMedia": map[string]any{"target": InsertMediaPath},
				"#VirtualMedia.EjectMedia":  map[string]any{"target": EjectMediaPath},
			},
		}, true
	}
	prefix := SystemPath + "/EthernetInterfaces/"
	if strings.HasPrefix(path, prefix) {
		if i, err := strconv.Atoi(strings.TrimPrefix(path, prefix)); err == nil && i >= 1 && i <= len(s.opts.MACs) {
			return map[string]any{
				"@odata.id": path, "Id": fmt.Sprint(i),
				"MACAddress": s.opts.MACs[i-1], "LinkStatus": "LinkUp",
			}, true
		}
	}
	return nil, false
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": msg,
			"@Message.ExtendedInfo": []any{
				map[string]any{"MessageId": code, "Message": msg},
			},
		},
	})
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
