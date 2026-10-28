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

package redfish

// update.go は FW の更新に使う Redfish の口（設計書 §29.1「Redfish の FW の
// 更新」、Backlog #148）: UpdateService とその FirmwareInventory、
// #UpdateService.SimpleUpdate、MultipartHttpPushUri への multipart の送信、
// TaskService の Task。
//
//   - 観測 state レス: UpdateService・TaskService のパスはキャッシュせず、
//     サービスルートから辿り直す。更新の Task の URI も持たない（provider は
//     Task の Payload を見て、どの更新の Task かを毎回見つけ直す。Payload を
//     返さない BMC（qemu-bmc 0.11.0）では、Update の登録簿のメッセージ
//     TargetDetermined の MessageArgs を見る、#168）。
//   - SimpleUpdate の Password（イメージを取りに行く資格）は本文にだけ載せ、
//     エラーの文言には本文を出さない（Error は Method と Path だけを持つ）。
//   - SimpleUpdate・送信は 200 / 202 / 204 を受け付ける。202 の Location は
//     Task の URI として返す（無くてもよい）。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"sort"
	"strings"
)

// Task の状態（TaskState）。
const (
	TaskNew         = "New"
	TaskStarting    = "Starting"
	TaskRunning     = "Running"
	TaskSuspended   = "Suspended"
	TaskInterrupted = "Interrupted"
	TaskPending     = "Pending"
	TaskStopping    = "Stopping"
	TaskCompleted   = "Completed"
	TaskKilled      = "Killed"
	TaskException   = "Exception"
	TaskInService   = "Service"
	TaskCancelling  = "Cancelling"
	TaskCancelled   = "Cancelled"
)

// TaskFailed は state が失敗で終わった Task の状態かを返す。
func TaskFailed(state string) bool {
	switch state {
	case TaskKilled, TaskException, TaskCancelled:
		return true
	}
	return false
}

// 更新を効かせる時（@Redfish.OperationApplyTime）。
const (
	ApplyImmediate = "Immediate"
	ApplyOnReset   = "OnReset"
)

// ErrNoUpdateService は BMC が UpdateService を持たないこと。
var ErrNoUpdateService = errors.New("the BMC has no UpdateService")

// ErrNoTaskService は BMC が TaskService を持たないこと。
var ErrNoTaskService = errors.New("the BMC has no TaskService")

// IsNoUpdateService は err が ErrNoUpdateService かを返す。
func IsNoUpdateService(err error) bool { return errors.Is(err, ErrNoUpdateService) }

// IsNoTaskService は err が ErrNoTaskService かを返す。
func IsNoTaskService(err error) bool { return errors.Is(err, ErrNoTaskService) }

// UpdateService は UpdateService の観測。
type UpdateService struct {
	// Path は読んだ UpdateService のパス。
	Path string
	// ServiceEnabled は ServiceEnabled（報告しなければ true）。
	ServiceEnabled bool
	// SimpleUpdateTarget は #UpdateService.SimpleUpdate の target（無ければ空）。
	SimpleUpdateTarget string
	// MultipartHTTPPushURI は MultipartHttpPushUri（無ければ空）。
	MultipartHTTPPushURI string
	// ApplyTimes は @Redfish.OperationApplyTimeSupport の SupportedValues
	// （報告しなければ空）。
	ApplyTimes []string
	// FirmwareInventory は FirmwareInventory のコレクションのパス。
	FirmwareInventory string
	// TaskService は TaskService があるか（サービスルートの Tasks）。
	TaskService bool
}

// FirmwareComponent は FirmwareInventory の 1 部品（SoftwareInventory）。
type FirmwareComponent struct {
	// ID は Id、Path は @odata.id。
	ID   string
	Path string
	// Version は Version。
	Version string
	// Updateable は Updateable（報告しなければ true）。
	Updateable bool
	// SoftwareID は SoftwareId。
	SoftwareID string
}

// UpdateRequest は 1 回の更新の要求（SimpleUpdate か multipart の送信）。
type UpdateRequest struct {
	// ImageURI は SimpleUpdate の ImageURI（BMC が取りに行く）。
	ImageURI string
	// Targets は更新する部品の @odata.id。
	Targets []string
	// ApplyTime は @Redfish.OperationApplyTime（空なら送らない）。
	ApplyTime string
	// Username・Password は ImageURI を取りに行く資格（空なら送らない）。
	// Password はエラーの文言にもログにも出さない。
	Username string
	Password string
}

// Task は TaskService の Task の観測（必要なところだけ）。
type Task struct {
	// Path は @odata.id、ID は Id。
	Path string
	ID   string
	// State は TaskState、Status は TaskStatus。
	State  string
	Status string
	// StartTime は StartTime（RFC 3339 の文字列のまま）。
	StartTime string
	// PercentComplete は PercentComplete（報告しなければ -1）。
	PercentComplete int
	// Messages は Messages の Message。
	Messages []string
	// MessageDetails は Messages の MessageId・Message・MessageArgs（Messages と
	// 同じ順。Message の空のものも含む）。
	MessageDetails []TaskMessage
	// TargetURI・Body は Payload の TargetUri と JsonBody（JSON の文字列を
	// 読んだもの。読めなければ nil）。
	TargetURI string
	Body      map[string]any
}

// TaskMessage は Task の Messages の 1 つ。
type TaskMessage struct {
	ID      string
	Message string
	Args    []string
}

// Update の登録簿（DMTF Update.1.0）のメッセージの名前（MessageId の
// "Update.<版>." の後ろ）。
const (
	// MsgTargetDetermined の MessageArgs は（更新する部品の URI、イメージ）。
	MsgTargetDetermined = "TargetDetermined"
)

// MessageIs は MessageId id が Update の登録簿（版を問わない）の name かを返す。
func MessageIs(id, name string) bool {
	return strings.HasPrefix(id, "Update.") && strings.HasSuffix(id, "."+name)
}

// Reason は Task の Messages を「Message（MessageId）」の形で "; " でつなぐ
// （失敗の理由を知らせる文言用。無ければ空）。
func (t *Task) Reason() string {
	var parts []string
	for _, m := range t.MessageDetails {
		switch {
		case m.Message != "" && m.ID != "":
			parts = append(parts, m.Message+" ("+m.ID+")")
		case m.Message != "":
			parts = append(parts, m.Message)
		case m.ID != "":
			parts = append(parts, m.ID)
		}
	}
	if len(parts) == 0 {
		return strings.Join(t.Messages, "; ")
	}
	return strings.Join(parts, "; ")
}

// UpdateClient は FW の更新の操作（Client に含まれる）。
type UpdateClient interface {
	// GetUpdateService は UpdateService を読む。無ければ ErrNoUpdateService を
	// 包んだ KindRequest のエラー。
	GetUpdateService(ctx context.Context) (UpdateService, error)
	// GetFirmwareInventory は FirmwareInventory の部品をすべて読む（Id 順）。
	GetFirmwareInventory(ctx context.Context) ([]FirmwareComponent, error)
	// SimpleUpdate は #UpdateService.SimpleUpdate を送り、Task の URI（202 の
	// Location。無ければ空）を返す。
	SimpleUpdate(ctx context.Context, req UpdateRequest) (string, error)
	// PushUpdate は MultipartHttpPushUri に UpdateParameters（Targets・
	// ApplyTime）と UpdateFile（image、名前 filename）を multipart で送り、
	// Task の URI を返す。時間切れは ctx に任せる。
	PushUpdate(ctx context.Context, req UpdateRequest, filename string, image io.Reader) (string, error)
	// ListTasks は TaskService の Task をすべて読む。無ければ ErrNoTaskService を
	// 包んだ KindRequest のエラー。
	ListTasks(ctx context.Context) ([]Task, error)
}

var _ UpdateClient = (*HTTPClient)(nil)

type rootDoc struct {
	UpdateService odataRef `json:"UpdateService"`
	Tasks         odataRef `json:"Tasks"`
}

type updateServiceDoc struct {
	ServiceEnabled       *bool    `json:"ServiceEnabled"`
	MultipartHTTPPushURI string   `json:"MultipartHttpPushUri"`
	FirmwareInventory    odataRef `json:"FirmwareInventory"`
	Actions              struct {
		SimpleUpdate struct {
			Target string `json:"target"`
		} `json:"#UpdateService.SimpleUpdate"`
	} `json:"Actions"`
	ApplyTimeSupport struct {
		SupportedValues []string `json:"SupportedValues"`
	} `json:"@Redfish.OperationApplyTimeSupport"`
}

type softwareDoc struct {
	ID         string `json:"Id"`
	Version    string `json:"Version"`
	Updateable *bool  `json:"Updateable"`
	SoftwareID string `json:"SoftwareId"`
}

type taskServiceDoc struct {
	Tasks odataRef `json:"Tasks"`
}

type taskDoc struct {
	ID              string `json:"Id"`
	TaskState       string `json:"TaskState"`
	TaskStatus      string `json:"TaskStatus"`
	StartTime       string `json:"StartTime"`
	PercentComplete *int   `json:"PercentComplete"`
	Messages        []struct {
		MessageID   string `json:"MessageId"`
		Message     string `json:"Message"`
		MessageArgs []any  `json:"MessageArgs"`
	} `json:"Messages"`
	Payload struct {
		TargetURI string          `json:"TargetUri"`
		JSONBody  json.RawMessage `json:"JsonBody"`
	} `json:"Payload"`
}

func (c *HTTPClient) root(ctx context.Context) (rootDoc, error) {
	var doc rootDoc
	err := c.getJSON(ctx, ServiceRoot, &doc)
	return doc, err
}

// GetUpdateService は Client の実装。
func (c *HTTPClient) GetUpdateService(ctx context.Context) (UpdateService, error) {
	r, err := c.root(ctx)
	if err != nil {
		return UpdateService{}, err
	}
	path := r.UpdateService.ID
	if path == "" {
		return UpdateService{}, &Error{Kind: KindRequest, Method: http.MethodGet, Path: ServiceRoot, Err: ErrNoUpdateService,
			Message: "the service root has no UpdateService link"}
	}
	var doc updateServiceDoc
	if err := c.getJSON(ctx, path, &doc); err != nil {
		if re := (*Error)(nil); errors.As(err, &re) && re.StatusCode == http.StatusNotFound {
			re.Err = ErrNoUpdateService
		}
		return UpdateService{}, err
	}
	u := UpdateService{
		Path:                 path,
		ServiceEnabled:       doc.ServiceEnabled == nil || *doc.ServiceEnabled,
		SimpleUpdateTarget:   doc.Actions.SimpleUpdate.Target,
		MultipartHTTPPushURI: doc.MultipartHTTPPushURI,
		ApplyTimes:           doc.ApplyTimeSupport.SupportedValues,
		FirmwareInventory:    doc.FirmwareInventory.ID,
		TaskService:          r.Tasks.ID != "",
	}
	if u.FirmwareInventory == "" {
		u.FirmwareInventory = strings.TrimSuffix(path, "/") + "/FirmwareInventory"
	}
	return u, nil
}

// GetFirmwareInventory は Client の実装。
func (c *HTTPClient) GetFirmwareInventory(ctx context.Context) ([]FirmwareComponent, error) {
	u, err := c.GetUpdateService(ctx)
	if err != nil {
		return nil, err
	}
	var coll collectionDoc
	if err := c.getJSON(ctx, u.FirmwareInventory, &coll); err != nil {
		return nil, err
	}
	out := make([]FirmwareComponent, 0, len(coll.Members))
	for _, m := range coll.Members {
		var doc softwareDoc
		if err := c.getJSON(ctx, m.ID, &doc); err != nil {
			return nil, err
		}
		id := doc.ID
		if id == "" {
			id = m.ID[strings.LastIndex(m.ID, "/")+1:]
		}
		out = append(out, FirmwareComponent{
			ID: id, Path: m.ID, Version: doc.Version,
			Updateable: doc.Updateable == nil || *doc.Updateable,
			SoftwareID: doc.SoftwareID,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// updateParams は SimpleUpdate の本文・UpdateParameters の共通部分。
func updateParams(req UpdateRequest) map[string]any {
	m := map[string]any{}
	if len(req.Targets) > 0 {
		m["Targets"] = req.Targets
	}
	if req.ApplyTime != "" {
		m["@Redfish.OperationApplyTime"] = req.ApplyTime
	}
	return m
}

// SimpleUpdate は Client の実装。
func (c *HTTPClient) SimpleUpdate(ctx context.Context, req UpdateRequest) (string, error) {
	u, err := c.GetUpdateService(ctx)
	if err != nil {
		return "", err
	}
	target := u.SimpleUpdateTarget
	if target == "" {
		target = strings.TrimSuffix(u.Path, "/") + "/Actions/UpdateService.SimpleUpdate"
	}
	body := updateParams(req)
	body["ImageURI"] = req.ImageURI
	if req.Username != "" {
		body["Username"] = req.Username
	}
	if req.Password != "" {
		body["Password"] = req.Password
	}
	b, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("redfish: POST %s: %w", target, err)
	}
	return c.accepted(ctx, c.hc, target, bytes.NewReader(b), "application/json")
}

// PushUpdate は Client の実装。
func (c *HTTPClient) PushUpdate(ctx context.Context, req UpdateRequest, filename string, image io.Reader) (string, error) {
	u, err := c.GetUpdateService(ctx)
	if err != nil {
		return "", err
	}
	if u.MultipartHTTPPushURI == "" {
		return "", &Error{Kind: KindRequest, Method: http.MethodPost, Path: u.Path,
			Message: "the UpdateService has no MultipartHttpPushUri"}
	}
	params, err := json.Marshal(updateParams(req))
	if err != nil {
		return "", fmt.Errorf("redfish: POST %s: %w", u.MultipartHTTPPushURI, err)
	}
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		pw.CloseWithError(writeMultipart(mw, params, filename, image))
	}()
	defer pr.Close()
	return c.accepted(ctx, c.hcLong, u.MultipartHTTPPushURI, pr, mw.FormDataContentType())
}

func writeMultipart(mw *multipart.Writer, params []byte, filename string, image io.Reader) error {
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="UpdateParameters"`)
	h.Set("Content-Type", "application/json")
	w, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	if _, err := w.Write(params); err != nil {
		return err
	}
	h = textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="UpdateFile"; filename=%q`, filename))
	h.Set("Content-Type", "application/octet-stream")
	if w, err = mw.CreatePart(h); err != nil {
		return err
	}
	if _, err := io.Copy(w, image); err != nil {
		return err
	}
	return mw.Close()
}

// accepted は path に本文を POST し、200 / 202 / 204 を成功として 202 の
// Location（Task の URI）を返す。
func (c *HTTPClient) accepted(ctx context.Context, hc *http.Client, path string, rd io.Reader, contentType string) (string, error) {
	status, hdr, _, err := c.doRaw(ctx, hc, http.MethodPost, path, rd, contentType)
	if err != nil {
		return "", err
	}
	switch status {
	case http.StatusOK, http.StatusNoContent:
		return "", nil
	case http.StatusAccepted:
		return hdr.Get("Location"), nil
	}
	return "", &Error{Kind: KindUnreachable, Method: http.MethodPost, Path: path, StatusCode: status, Message: "unexpected status"}
}

// ListTasks は Client の実装。
func (c *HTTPClient) ListTasks(ctx context.Context) ([]Task, error) {
	r, err := c.root(ctx)
	if err != nil {
		return nil, err
	}
	if r.Tasks.ID == "" {
		return nil, &Error{Kind: KindRequest, Method: http.MethodGet, Path: ServiceRoot, Err: ErrNoTaskService,
			Message: "the service root has no Tasks (TaskService) link"}
	}
	var ts taskServiceDoc
	if err := c.getJSON(ctx, r.Tasks.ID, &ts); err != nil {
		return nil, err
	}
	collPath := ts.Tasks.ID
	if collPath == "" {
		collPath = strings.TrimSuffix(r.Tasks.ID, "/") + "/Tasks"
	}
	var coll collectionDoc
	if err := c.getJSON(ctx, collPath, &coll); err != nil {
		return nil, err
	}
	out := make([]Task, 0, len(coll.Members))
	for _, m := range coll.Members {
		var doc taskDoc
		if err := c.getJSON(ctx, m.ID, &doc); err != nil {
			if IsRequest(err) {
				continue // 読む間に消えた Task
			}
			return nil, err
		}
		t := Task{
			Path: m.ID, ID: doc.ID, State: doc.TaskState, Status: doc.TaskStatus,
			StartTime: doc.StartTime, PercentComplete: -1, TargetURI: doc.Payload.TargetURI,
		}
		if doc.PercentComplete != nil {
			t.PercentComplete = *doc.PercentComplete
		}
		for _, msg := range doc.Messages {
			if msg.Message != "" {
				t.Messages = append(t.Messages, msg.Message)
			}
			args := make([]string, 0, len(msg.MessageArgs))
			for _, a := range msg.MessageArgs {
				args = append(args, fmt.Sprint(a))
			}
			t.MessageDetails = append(t.MessageDetails, TaskMessage{ID: msg.MessageID, Message: msg.Message, Args: args})
		}
		t.Body = payloadBody(doc.Payload.JSONBody)
		out = append(out, t)
	}
	return out, nil
}

// payloadBody は Payload の JsonBody（仕様では JSON を入れた文字列。オブジェクトの
// まま返す BMC もある）を読む。
func payloadBody(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		return m
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && json.Unmarshal([]byte(s), &m) == nil {
		return m
	}
	return nil
}
