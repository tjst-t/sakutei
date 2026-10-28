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

package redfishtest

// update.go は偽 BMC の FW の更新（#148）。DMTF の Redfish の仕様に沿って次を
// 真似る（qemu-bmc は 0.11.0 から UpdateService の模擬を持ち、統合テストの
// 10 段目が本物で確かめる。違いは Options の NoTaskPayload と
// SetImageRejected で寄せる、#168）:
//
//   - /redfish/v1/UpdateService: ServiceEnabled・FirmwareInventory・
//     #UpdateService.SimpleUpdate・MultipartHttpPushUri（Options.NoMultipartPush
//     なら無い）・@Redfish.OperationApplyTimeSupport（Immediate・OnReset）。
//   - FirmwareInventory/<Id>: Version・Updateable・SoftwareId（既定は BMC と
//     BIOS、Options.Firmware で差し替え）。
//   - SimpleUpdate（ImageURI・Targets・@Redfish.OperationApplyTime・Username・
//     Password）と multipart の送信（UpdateParameters・UpdateFile）は 202 と
//     Location（Task の URI）を返す。Targets は必須で、無い・Updateable でない
//     部品は 400。
//   - Task は Running で始まり、Options.UpdateDuration で終わる（時刻は
//     Options.Now、要求を受けたときに進める）。終わるとき、イメージ（AddImage
//     で置いたもの。SimpleUpdate は URI、送信は中身で引く）が無い・資格が合わない・
//     SetUpdateFails のときは Exception。Immediate なら部品の Version をすぐ
//     イメージの版に、OnReset なら次の起動（Reset）で効く保留にして Completed。
//   - Task の Payload の JsonBody は要求の本文（JSON の文字列）から Password を
//     除いたもの。Options.NoTaskPayload なら Payload を返さない（qemu-bmc
//     0.11.0 と同じ）。
//   - Task の Messages は MessageId・MessageArgs を持つ（DMTF の Update.1.0）:
//     受けたときに TargetDetermined（部品の URI、イメージ。イメージは
//     SimpleUpdate なら ImageURI、送信なら UpdateFile のファイル名）、終わる
//     ときに UpdateSuccessful・AwaitToActivate（OnReset）・ApplyFailed
//     （Exception）。
//
// 試験用の口: AddImage・SetUpdateFails・SetImageRejected（受けた要求を 400
// Update.1.0.VerificationFailed で断り Task を作らない。qemu-bmc の障害
// firmware-image-rejected と同じ）・FirmwareVersion・SetFirmwareVersion・
// UpdateRequests（更新の要求を受けた回数）・ClearTasks（BMC の再起動で Task が
// 消えた、の代役）。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime"
	"mime/multipart"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// パス。
const (
	UpdateServicePath     = "/redfish/v1/UpdateService"
	FirmwareInventoryPath = UpdateServicePath + "/FirmwareInventory"
	SimpleUpdatePath      = UpdateServicePath + "/Actions/UpdateService.SimpleUpdate"
	MultipartPushPath     = UpdateServicePath + "/upload"
	TaskServicePath       = "/redfish/v1/TaskService"
	TasksPath             = TaskServicePath + "/Tasks"
)

// FirmwareItem は偽 BMC の FirmwareInventory の 1 部品。
type FirmwareItem struct {
	ID      string
	Version string
	// NotUpdateable は Updateable が false の部品。
	NotUpdateable bool
}

// DefaultFirmware は偽 BMC の既定の FirmwareInventory。
var DefaultFirmware = []FirmwareItem{
	{ID: "BMC", Version: "1.0.0"},
	{ID: "BIOS", Version: "1.0.0"},
	{ID: "CPLD", Version: "3", NotUpdateable: true},
}

// UpdateOptions は FW の更新の初期値（Options に埋め込む）。
type UpdateOptions struct {
	// NoUpdateService は UpdateService を持たない BMC（qemu-bmc と同じ）。
	NoUpdateService bool
	// NoTaskService は TaskService を持たない BMC。
	NoTaskService bool
	// NoMultipartPush は MultipartHttpPushUri を持たない BMC。
	NoMultipartPush bool
	// Firmware は FirmwareInventory（nil なら DefaultFirmware）。
	Firmware []FirmwareItem
	// UpdateDuration は更新の Task が Running から終わるまでの時間（既定 0 =
	// 次に観測したときには終わっている）。
	UpdateDuration time.Duration
	// NoTaskPayload は Task に Payload を返さない BMC（qemu-bmc 0.11.0 と同じ。
	// どの更新の Task かは Messages の TargetDetermined で分かる）。
	NoTaskPayload bool
}

// Image は偽 BMC が取りに行ける（送られてくる）イメージ。
type Image struct {
	// URI は SimpleUpdate の ImageURI で引くときの鍵。
	URI string
	// Content は multipart の送信で引くときの鍵（中身そのもの）。
	Content []byte
	// Version はこのイメージを入れたあとの部品の Version。
	Version string
	// Username・Password は URI を取りに行くのに要る資格（空なら要らない）。
	Username string
	Password string
}

type fwComponent struct {
	FirmwareItem
	pending string // OnReset で次の起動に効く版
}

type fwTask struct {
	id        int
	state     string
	status    string
	start     time.Time
	targetURI string
	body      string // Password を除いた JSON
	targets   []string
	applyTime string
	imageURI  string
	content   []byte
	username  string
	password  string
	filename  string // 送信の UpdateFile のファイル名
	pushed    bool
	messages  []fwMessage
}

// fwMessage は Task の Messages の 1 つ（DMTF の Update.1.0）。
type fwMessage struct {
	id, text string
	args     []string
}

type fwState struct {
	components map[string]*fwComponent
	images     []Image
	tasks      []*fwTask
	nextTask   int
	fail       bool
	rejected   bool
	requests   int
}

func (s *Server) initUpdate() {
	items := s.opts.Firmware
	if items == nil {
		items = DefaultFirmware
	}
	s.fw.components = map[string]*fwComponent{}
	for _, it := range items {
		s.fw.components[it.ID] = &fwComponent{FirmwareItem: it}
	}
}

// ---- 試験用の口 ----

// AddImage は BMC が取りに行ける（送られてくる）イメージを置く。
func (s *Server) AddImage(img Image) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fw.images = append(s.fw.images, img)
}

// SetUpdateFails は true のあいだ、終わる更新の Task を Exception にする。
func (s *Server) SetUpdateFails(v bool) { s.mu.Lock(); s.fw.fail = v; s.mu.Unlock() }

// SetImageRejected は true のあいだ、受けた更新の要求を 400
// （Update.1.0.VerificationFailed）で断り、Task を作らない（数えもしない）。
func (s *Server) SetImageRejected(v bool) { s.mu.Lock(); s.fw.rejected = v; s.mu.Unlock() }

// FirmwareVersion は部品 id の今の Version を返す。
func (s *Server) FirmwareVersion(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.advance()
	if c, ok := s.fw.components[id]; ok {
		return c.Version
	}
	return ""
}

// FirmwarePending は部品 id の、次の起動で効く版を返す（無ければ空）。
func (s *Server) FirmwarePending(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.advance()
	if c, ok := s.fw.components[id]; ok {
		return c.pending
	}
	return ""
}

// SetFirmwareVersion は部品 id の Version を直接変える（人が別の道具で更新した、
// の代役）。
func (s *Server) SetFirmwareVersion(id, version string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.fw.components[id]; ok {
		c.Version = version
	}
}

// UpdateRequests は受け付けた更新の要求（SimpleUpdate・送信）の回数を返す。
func (s *Server) UpdateRequests() int { s.mu.Lock(); defer s.mu.Unlock(); return s.fw.requests }

// ClearTasks は Task をすべて消す（BMC の再起動で Task が消えた、の代役）。
func (s *Server) ClearTasks() { s.mu.Lock(); s.fw.tasks = nil; s.mu.Unlock() }

// ---- 状態の移り変わり（s.mu を持って呼ぶ） ----

// bootFirmware は起動で OnReset の保留を効かせる。
func (s *Server) bootFirmware() {
	for _, c := range s.fw.components {
		if c.pending != "" {
			c.Version, c.pending = c.pending, ""
		}
	}
}

func (s *Server) advanceUpdates(now time.Time) {
	for _, t := range s.fw.tasks {
		if t.state != "Running" || now.Sub(t.start) < s.opts.UpdateDuration {
			continue
		}
		s.finishTask(t)
	}
}

func (s *Server) finishTask(t *fwTask) {
	image := t.imageURI
	if t.pushed {
		image = t.filename
	}
	exception := func(msg string) {
		t.state, t.status = "Exception", "Critical"
		t.messages = append(t.messages, fwMessage{id: "Update.1.0.ApplyFailed", text: msg, args: []string{image, t.targets[0]}})
	}
	if s.fw.fail {
		exception("The update failed (injected failure).")
		return
	}
	var img *Image
	for i := range s.fw.images {
		im := &s.fw.images[i]
		if (t.pushed && im.Content != nil && bytes.Equal(im.Content, t.content)) || (!t.pushed && im.URI != "" && im.URI == t.imageURI) {
			img = im
			break
		}
	}
	switch {
	case img == nil && t.pushed:
		exception("The uploaded image is not a valid firmware image.")
		return
	case img == nil:
		exception(fmt.Sprintf("The image %s could not be fetched.", t.imageURI))
		return
	case !t.pushed && (img.Username != t.username || img.Password != t.password):
		exception(fmt.Sprintf("Access to the image %s was denied.", t.imageURI))
		return
	}
	for _, target := range t.targets {
		c := s.fw.components[strings.TrimPrefix(target, FirmwareInventoryPath+"/")]
		if t.applyTime == "OnReset" {
			c.pending = img.Version
		} else {
			c.Version = img.Version
		}
	}
	t.state, t.status = "Completed", "OK"
	if t.applyTime == "OnReset" {
		t.messages = append(t.messages, fwMessage{id: "Update.1.0.AwaitToActivate", text: "The update is staged and applies at the next reset.", args: []string{image, t.targets[0]}})
	} else {
		t.messages = append(t.messages, fwMessage{id: "Update.1.0.UpdateSuccessful", text: "The update completed.", args: []string{t.targets[0], image}})
	}
}

// ---- HTTP ----

// serveUpdate は FW の更新の POST を受ける。受けたら true。
func (s *Server) serveUpdate(w http.ResponseWriter, r *http.Request, path string, body []byte) bool {
	if s.opts.NoUpdateService {
		return false
	}
	switch {
	case path == SimpleUpdatePath:
		s.postSimpleUpdate(w, body)
	case path == MultipartPushPath && !s.opts.NoMultipartPush:
		s.postPush(w, r, body)
	default:
		return false
	}
	return true
}

type updateParamsDoc struct {
	ImageURI  string   `json:"ImageURI"`
	Targets   []string `json:"Targets"`
	ApplyTime string   `json:"@Redfish.OperationApplyTime"`
	Username  string   `json:"Username"`
	Password  string   `json:"Password"`
}

// checkParams は Targets と ApplyTime を確かめ、断るなら理由を返す。
func (s *Server) checkParams(p *updateParamsDoc) string {
	if len(p.Targets) == 0 {
		return "Targets is required."
	}
	for _, t := range p.Targets {
		c, ok := s.fw.components[strings.TrimPrefix(t, FirmwareInventoryPath+"/")]
		if !ok || !strings.HasPrefix(t, FirmwareInventoryPath+"/") {
			return fmt.Sprintf("The target %s is not in the FirmwareInventory.", t)
		}
		if c.NotUpdateable {
			return fmt.Sprintf("The target %s is not updateable.", t)
		}
	}
	switch p.ApplyTime {
	case "":
		p.ApplyTime = "Immediate"
	case "Immediate", "OnReset":
	default:
		return fmt.Sprintf("The value %s for @Redfish.OperationApplyTime is not supported.", p.ApplyTime)
	}
	return ""
}

func (s *Server) postSimpleUpdate(w http.ResponseWriter, body []byte) {
	var p updateParamsDoc
	if err := json.Unmarshal(body, &p); err != nil {
		writeError(w, http.StatusBadRequest, "Base.1.0.MalformedJSON", err.Error())
		return
	}
	if p.ImageURI == "" {
		writeError(w, http.StatusBadRequest, "Base.1.0.ActionParameterMissing", "ImageURI is required.")
		return
	}
	if msg := s.checkParams(&p); msg != "" {
		writeError(w, http.StatusBadRequest, "Base.1.0.ActionParameterValueError", msg)
		return
	}
	if s.rejectImage(w, p.ImageURI, p.Targets) {
		return
	}
	t := s.newTask(SimpleUpdatePath, body, p)
	t.imageURI, t.username, t.password = p.ImageURI, p.Username, p.Password
	t.determined(p.ImageURI)
	s.accepted(w, t)
}

func (s *Server) postPush(w http.ResponseWriter, r *http.Request, body []byte) {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || params["boundary"] == "" {
		writeError(w, http.StatusBadRequest, "Base.1.0.MalformedJSON", "a multipart/form-data body is required")
		return
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var p updateParamsDoc
	var paramsRaw, content []byte
	var filename string
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(part)
		switch part.FormName() {
		case "UpdateParameters":
			paramsRaw = buf.Bytes()
		case "UpdateFile":
			content, filename = buf.Bytes(), part.FileName()
		}
	}
	if paramsRaw == nil || content == nil {
		writeError(w, http.StatusBadRequest, "Base.1.0.ActionParameterMissing", "UpdateParameters and UpdateFile are required.")
		return
	}
	if err := json.Unmarshal(paramsRaw, &p); err != nil {
		writeError(w, http.StatusBadRequest, "Base.1.0.MalformedJSON", err.Error())
		return
	}
	if msg := s.checkParams(&p); msg != "" {
		writeError(w, http.StatusBadRequest, "Base.1.0.ActionParameterValueError", msg)
		return
	}
	if s.rejectImage(w, filename, p.Targets) {
		return
	}
	t := s.newTask(MultipartPushPath, paramsRaw, p)
	t.pushed, t.content, t.filename = true, content, filename
	t.determined(filename)
	s.accepted(w, t)
}

// rejectImage は SetImageRejected のあいだ 400（Update.1.0.VerificationFailed）を
// 返して true。
func (s *Server) rejectImage(w http.ResponseWriter, image string, targets []string) bool {
	if !s.fw.rejected {
		return false
	}
	writeError(w, http.StatusBadRequest, "Update.1.0.VerificationFailed",
		fmt.Sprintf("Verification of image '%s' at '%s' failed.", image, strings.Join(targets, ", ")))
	return true
}

// determined は受けたときのメッセージ TargetDetermined（部品の URI、イメージ）を足す。
func (t *fwTask) determined(image string) {
	for _, target := range t.targets {
		t.messages = append(t.messages, fwMessage{id: "Update.1.0.TargetDetermined",
			text: fmt.Sprintf("The target device '%s' will be updated with image '%s'.", target, image), args: []string{target, image}})
	}
}

func (s *Server) newTask(targetURI string, body []byte, p updateParamsDoc) *fwTask {
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	delete(m, "Password")
	echo, _ := json.Marshal(m)
	s.fw.nextTask++
	s.fw.requests++
	t := &fwTask{id: s.fw.nextTask, state: "Running", status: "OK", start: s.opts.Now(),
		targetURI: targetURI, body: string(echo), targets: p.Targets, applyTime: p.ApplyTime}
	s.fw.tasks = append(s.fw.tasks, t)
	return t
}

func (s *Server) accepted(w http.ResponseWriter, t *fwTask) {
	s.advance()
	loc := TasksPath + "/" + strconv.Itoa(t.id)
	w.Header().Set("Location", loc)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(s.taskDoc(t))
}

func (s *Server) taskDoc(t *fwTask) map[string]any {
	var msgs []any
	for _, m := range t.messages {
		msgs = append(msgs, map[string]any{"MessageId": m.id, "Message": m.text, "MessageArgs": m.args})
	}
	pct := 0
	if t.state != "Running" {
		pct = 100
	}
	doc := map[string]any{
		"@odata.id": TasksPath + "/" + strconv.Itoa(t.id), "Id": strconv.Itoa(t.id),
		"TaskState": t.state, "TaskStatus": t.status, "PercentComplete": pct,
		"StartTime": t.start.UTC().Format(time.RFC3339Nano),
		"Messages":  msgs,
	}
	if !s.opts.NoTaskPayload {
		doc["Payload"] = map[string]any{
			"TargetUri": t.targetURI, "HttpOperation": "POST", "JsonBody": t.body,
		}
	}
	return doc
}

// updateDocument は FW の更新の GET の文書を返す。
func (s *Server) updateDocument(path string) (any, bool) {
	ref := func(p string) map[string]string { return map[string]string{"@odata.id": p} }
	if !s.opts.NoUpdateService {
		switch {
		case path == UpdateServicePath:
			doc := map[string]any{
				"@odata.id": UpdateServicePath, "Id": "UpdateService", "ServiceEnabled": true,
				"FirmwareInventory": ref(FirmwareInventoryPath),
				"Actions": map[string]any{
					"#UpdateService.SimpleUpdate": map[string]any{
						"target": SimpleUpdatePath,
						"TransferProtocol@Redfish.AllowableValues": []string{"HTTP", "HTTPS"},
					},
				},
				"@Redfish.OperationApplyTimeSupport": map[string]any{
					"SupportedValues": []string{"Immediate", "OnReset"},
				},
			}
			if !s.opts.NoMultipartPush {
				doc["MultipartHttpPushUri"] = MultipartPushPath
			}
			return doc, true
		case path == FirmwareInventoryPath:
			ids := make([]string, 0, len(s.fw.components))
			for id := range s.fw.components {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			ms := []any{}
			for _, id := range ids {
				ms = append(ms, ref(FirmwareInventoryPath+"/"+id))
			}
			return map[string]any{"@odata.id": path, "Members": ms, "Members@odata.count": len(ms)}, true
		case strings.HasPrefix(path, FirmwareInventoryPath+"/"):
			c, ok := s.fw.components[strings.TrimPrefix(path, FirmwareInventoryPath+"/")]
			if !ok {
				return nil, false
			}
			return map[string]any{
				"@odata.id": path, "Id": c.ID, "Name": c.ID + " firmware",
				"Version": c.Version, "Updateable": !c.NotUpdateable, "SoftwareId": "redfishtest-" + strings.ToLower(c.ID),
			}, true
		}
	}
	if !s.opts.NoTaskService {
		switch {
		case path == TaskServicePath:
			return map[string]any{"@odata.id": path, "Id": "TaskService", "ServiceEnabled": true, "Tasks": ref(TasksPath)}, true
		case path == TasksPath:
			ms := []any{}
			for _, t := range s.fw.tasks {
				ms = append(ms, ref(TasksPath+"/"+strconv.Itoa(t.id)))
			}
			return map[string]any{"@odata.id": path, "Members": ms, "Members@odata.count": len(ms)}, true
		case strings.HasPrefix(path, TasksPath+"/"):
			for _, t := range s.fw.tasks {
				if path == TasksPath+"/"+strconv.Itoa(t.id) {
					return s.taskDoc(t), true
				}
			}
		}
	}
	return nil, false
}
