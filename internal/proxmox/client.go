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

// client.go — Proxmox VE の REST API（/api2/json）を net/http で叩く。認証は
// API トークン（Authorization: PVEAPIToken=<user>!<id>=<secret>）。secret は
// 要求の直前に関数から受け取り（JIT、§20.2）、Client にも持たず、エラーの
// 文言にも出さない。

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout は 1 つの要求の時間切れの既定。
const DefaultTimeout = 15 * time.Second

// ErrorKind はエラーの分け方。
type ErrorKind int

// エラーの種類。
const (
	// KindUnreachable: 届かない・時間切れ・5xx（ノードが落ちている 595 を含む）。
	// 時間が経てば治りうる（観測できない、unknown）。
	KindUnreachable ErrorKind = iota + 1
	// KindAuth: 認証の失敗（401）・権限が無い（403）。設定を直すまで治らない。
	KindAuth
	// KindRejected: 要求が拒まれた（400 などの 4xx）。
	KindRejected
)

// Error は Client のエラー。資格は含まない。
type Error struct {
	Kind    ErrorKind
	Status  int
	Op      string
	Message string
	Err     error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Op)
	if e.Status != 0 {
		fmt.Fprintf(&b, ": HTTP %d", e.Status)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// KindOf は err の中の Error の種類を返す（無ければ 0）。
func KindOf(err error) ErrorKind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return 0
}

// ClientConfig は Client の設定。
type ClientConfig struct {
	// Endpoint は https://host:8006 の形（/api2/json は足す）。
	Endpoint string
	// TokenID は "<user>@<realm>!<tokenname>"。
	TokenID string
	// Secret はトークンの secret を要求の直前に返す。
	Secret func(ctx context.Context) (string, error)
	// CAFile が空でなければ、それだけを信頼する。
	CAFile string
	// InsecureSkipVerify は証明書を確かめない（明示したときだけ）。
	InsecureSkipVerify bool
	// Timeout は 1 つの要求の時間切れ（0 なら DefaultTimeout）。
	Timeout time.Duration
}

// Client は 1 つのクラスタ（の 1 つの endpoint）の API の口。
type Client struct {
	base    string
	tokenID string
	secret  func(ctx context.Context) (string, error)
	http    *http.Client
}

// NewClient は cfg の Client を作る。
func NewClient(cfg ClientConfig) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(cfg.Endpoint, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("endpoint %q: want https://host:port", cfg.Endpoint)
	}
	if !strings.Contains(cfg.TokenID, "!") || !strings.Contains(cfg.TokenID, "@") {
		return nil, fmt.Errorf("token_id %q: want <user>@<realm>!<tokenname>", cfg.TokenID)
	}
	if cfg.Secret == nil {
		return nil, errors.New("the token secret is not set")
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	switch {
	case cfg.InsecureSkipVerify && cfg.CAFile != "":
		return nil, errors.New("ca_file and insecure_skip_verify are exclusive")
	case cfg.InsecureSkipVerify:
		tlsCfg.InsecureSkipVerify = true
	case cfg.CAFile != "":
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_file %s: no certificate", cfg.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	tr.Proxy = nil
	return &Client{
		base:    u.String() + "/api2/json",
		tokenID: cfg.TokenID,
		secret:  cfg.Secret,
		http:    &http.Client{Transport: tr, Timeout: timeout},
	}, nil
}

// call は 1 つの要求を送り、応答の data を out に読む（out が nil なら読まない）。
// GET・DELETE の params はクエリ、POST・PUT は form の本文に入れる。
func (c *Client) call(ctx context.Context, method, path string, params url.Values, out any) error {
	op := method + " " + path
	secret, err := c.secret(ctx)
	if err != nil {
		return &Error{Kind: KindAuth, Op: op, Message: "the token secret cannot be resolved", Err: err}
	}
	target := c.base + path
	var body io.Reader
	if method == http.MethodGet || method == http.MethodDelete {
		if len(params) > 0 {
			target += "?" + params.Encode()
		}
	} else if params != nil {
		body = strings.NewReader(params.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return &Error{Kind: KindRejected, Op: op, Err: err}
	}
	req.Header.Set("Authorization", "PVEAPIToken="+c.tokenID+"="+secret)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return &Error{Kind: KindUnreachable, Op: op, Err: scrub(err, secret)}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return &Error{Kind: KindUnreachable, Op: op, Err: err}
	}
	if resp.StatusCode/100 != 2 {
		e := &Error{Op: op, Status: resp.StatusCode, Message: failureMessage(resp, data)}
		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			e.Kind = KindAuth
		case resp.StatusCode >= 500:
			e.Kind = KindUnreachable
		default:
			e.Kind = KindRejected
		}
		return e
	}
	if out == nil {
		return nil
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&env); err != nil {
		return &Error{Kind: KindUnreachable, Op: op, Message: "unreadable response", Err: err}
	}
	dec = json.NewDecoder(bytes.NewReader(env.Data))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil && !(len(env.Data) == 0 || string(env.Data) == "null") {
		return &Error{Kind: KindUnreachable, Op: op, Message: "unreadable response", Err: err}
	}
	return nil
}

// failureMessage は失敗の応答の文言（HTTP の理由句と、あれば errors の中身）。
func failureMessage(resp *http.Response, data []byte) string {
	msg := strings.TrimSpace(strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)))
	var env struct {
		Errors  map[string]any `json:"errors"`
		Message string         `json:"message"`
	}
	if json.Unmarshal(data, &env) == nil {
		if env.Message != "" {
			msg = strings.TrimSpace(env.Message)
		}
		if len(env.Errors) > 0 {
			var parts []string
			for k, v := range env.Errors {
				parts = append(parts, fmt.Sprintf("%s: %v", k, v))
			}
			sortStrings(parts)
			msg += " (" + strings.Join(parts, "; ") + ")"
		}
	}
	return msg
}

// scrub は err の文言から secret を消す（URL に入れることは無いが念のため）。
func scrub(err error, secret string) error {
	if secret != "" && strings.Contains(err.Error(), secret) {
		return errors.New(strings.ReplaceAll(err.Error(), secret, "***"))
	}
	return err
}

// ---- API ----

// Resource は /cluster/resources?type=vm の 1 件。vmid・ノード・種類は
// クラスタの設定（pmxcfs）から来るが、名前・status・template・lock は pvestatd
// が周期（約 10 秒）で更新する写しで、作った直後の VM は名前が空、status が
// 古いことがある。名前が空の項目は Provider が設定で確かめ、電源と lock は
// CurrentStatus で読む。
type Resource struct {
	Type     string
	VMID     int
	Name     string
	Node     string
	Status   string
	Template bool
	Lock     string
}

// Resources はクラスタの VM（qemu と lxc）の一覧を返す。
func (c *Client) Resources(ctx context.Context) ([]Resource, error) {
	var raw []map[string]any
	if err := c.call(ctx, http.MethodGet, "/cluster/resources", url.Values{"type": {"vm"}}, &raw); err != nil {
		return nil, err
	}
	out := make([]Resource, 0, len(raw))
	for _, m := range raw {
		vmid, err := strconv.Atoi(scalar(m["vmid"]))
		if err != nil {
			return nil, &Error{Kind: KindUnreachable, Op: "GET /cluster/resources", Message: fmt.Sprintf("unreadable vmid %v", m["vmid"])}
		}
		out = append(out, Resource{
			Type:     scalar(m["type"]),
			VMID:     vmid,
			Name:     scalar(m["name"]),
			Node:     scalar(m["node"]),
			Status:   scalar(m["status"]),
			Template: scalar(m["template"]) == "1",
			Lock:     scalar(m["lock"]),
		})
	}
	return out, nil
}

// PendingEntry は /pending の 1 件（設定のキーの今の値と、再起動を待つ値）。
type PendingEntry struct {
	Key        string
	Value      string
	HasValue   bool
	Pending    string
	HasPending bool
	Delete     bool
}

// Pending は VM の設定を、再起動を待つ変更つきで返す。
func (c *Client) Pending(ctx context.Context, node string, vmid int) ([]PendingEntry, error) {
	var raw []map[string]any
	if err := c.call(ctx, http.MethodGet, vmPath(node, vmid)+"/pending", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]PendingEntry, 0, len(raw))
	for _, m := range raw {
		e := PendingEntry{Key: scalar(m["key"])}
		if v, ok := m["value"]; ok && v != nil {
			e.Value, e.HasValue = scalar(v), true
		}
		if v, ok := m["pending"]; ok && v != nil {
			e.Pending, e.HasPending = scalar(v), true
		}
		if d := scalar(m["delete"]); d != "" && d != "0" {
			e.Delete = true
		}
		out = append(out, e)
	}
	return out, nil
}

// Config は VM の設定（今の値）を返す。
func (c *Client) Config(ctx context.Context, node string, vmid int) (map[string]string, error) {
	var raw map[string]any
	if err := c.call(ctx, http.MethodGet, vmPath(node, vmid)+"/config", nil, &raw); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		out[k] = scalar(v)
	}
	return out, nil
}

// VMStatus は /status/current の VM の状態（電源と lock）。
type VMStatus struct {
	Status string
	Lock   string
}

// CurrentStatus は VM の今の状態を返す。/cluster/resources の status は
// pvestatd が周期で更新する写しで古いことがあるので、電源はこちらで読む。
func (c *Client) CurrentStatus(ctx context.Context, node string, vmid int) (VMStatus, error) {
	var raw map[string]any
	if err := c.call(ctx, http.MethodGet, vmPath(node, vmid)+"/status/current", nil, &raw); err != nil {
		return VMStatus{}, err
	}
	return VMStatus{Status: scalar(raw["status"]), Lock: scalar(raw["lock"])}, nil
}

// SetConfig は設定を書く（PUT、同期）。走っている VM で再起動が要る変更は
// Proxmox が pending に置く。
func (c *Client) SetConfig(ctx context.Context, node string, vmid int, params url.Values) error {
	return c.call(ctx, http.MethodPut, vmPath(node, vmid)+"/config", params, nil)
}

// Create は VM を作る（UPID を返す）。
func (c *Client) Create(ctx context.Context, node string, params url.Values) (string, error) {
	return c.task(ctx, http.MethodPost, "/nodes/"+url.PathEscape(node)+"/qemu", params)
}

// Clone は node の vmid（テンプレート）を複製する（UPID を返す）。
func (c *Client) Clone(ctx context.Context, node string, vmid int, params url.Values) (string, error) {
	return c.task(ctx, http.MethodPost, vmPath(node, vmid)+"/clone", params)
}

// Resize はディスクを size（"32G" など、絶対値）にする。版によって UPID を返す
// （返さなければ ""）。
func (c *Client) Resize(ctx context.Context, node string, vmid int, disk, size string) (string, error) {
	return c.task(ctx, http.MethodPut, vmPath(node, vmid)+"/resize", url.Values{"disk": {disk}, "size": {size}})
}

// PowerAction は status/<action>（start・shutdown・stop・reboot・reset）を送る
// （UPID を返す）。
func (c *Client) PowerAction(ctx context.Context, node string, vmid int, action string, params url.Values) (string, error) {
	return c.task(ctx, http.MethodPost, vmPath(node, vmid)+"/status/"+action, params)
}

// Destroy は VM を消す（UPID を返す）。ディスクと、ジョブ・HA の設定も消す。
func (c *Client) Destroy(ctx context.Context, node string, vmid int) (string, error) {
	return c.task(ctx, http.MethodDelete, vmPath(node, vmid), url.Values{"purge": {"1"}, "destroy-unreferenced-disks": {"1"}})
}

// TaskStatus は非同期の仕事の状態。
type TaskStatus struct {
	Running    bool
	ExitStatus string
}

// OK は仕事が成功で終わったかを返す。
func (t TaskStatus) OK() bool { return !t.Running && t.ExitStatus == "OK" }

// Task は upid の仕事の状態を返す。
func (c *Client) Task(ctx context.Context, upid string) (TaskStatus, error) {
	node, err := UPIDNode(upid)
	if err != nil {
		return TaskStatus{}, &Error{Kind: KindRejected, Op: "task status", Err: err}
	}
	var raw map[string]any
	if err := c.call(ctx, http.MethodGet, "/nodes/"+url.PathEscape(node)+"/tasks/"+url.PathEscape(upid)+"/status", nil, &raw); err != nil {
		return TaskStatus{}, err
	}
	return TaskStatus{Running: scalar(raw["status"]) == "running", ExitStatus: scalar(raw["exitstatus"])}, nil
}

func (c *Client) task(ctx context.Context, method, path string, params url.Values) (string, error) {
	var upid any
	if err := c.call(ctx, method, path, params, &upid); err != nil {
		return "", err
	}
	if upid == nil {
		return "", nil
	}
	return scalar(upid), nil
}

// UPIDNode は UPID（"UPID:<node>:<pid>:..."）のノードを返す。
func UPIDNode(upid string) (string, error) {
	parts := strings.Split(upid, ":")
	if len(parts) < 3 || parts[0] != "UPID" || parts[1] == "" {
		return "", fmt.Errorf("%q is not a UPID", upid)
	}
	return parts[1], nil
}

func vmPath(node string, vmid int) string {
	return "/nodes/" + url.PathEscape(node) + "/qemu/" + strconv.Itoa(vmid)
}

// scalar は JSON の値（文字列・json.Number・bool）を文字列にする。
func scalar(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		if t {
			return "1"
		}
		return "0"
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", t)
	}
}
