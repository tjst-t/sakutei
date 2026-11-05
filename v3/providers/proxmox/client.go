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

// client.go — Proxmox VE の REST API（既定の prefix は /api2/json）を net/http で呼ぶ
// （v2 の proxmox-vm の client を移したもの、E-D1）。認証は API トークン
// （Authorization: PVEAPIToken=<user>!<id>=<secret>）。secret はリクエストの直前に関数から
// 受け取る。Client には持たず、エラーメッセージにも出さない（E-G2）。1 つの Client は 1 本の
// トークンだけを持つ。観測用と書き込み用は別の Client にする（E-G5）。

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
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout は 1 つのリクエストのタイムアウトの既定値。
const DefaultTimeout = 15 * time.Second

// ErrorKind はエラーの分類。
type ErrorKind int

// エラーの種類。
const (
	// KindUnreachable: 届かない・タイムアウト・5xx（ノードが落ちている 595 を含む）。
	// 観測なら Unknown（E-K1）。
	KindUnreachable ErrorKind = iota + 1
	// KindAuth: 認証の失敗（401）・権限が無い（403）。
	KindAuth
	// KindRejected: リクエストが拒否された（400 などの 4xx）。
	KindRejected
)

// Error は Client のエラー。credential は含まない（E-G2）。
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

// DefaultAPIBasePath は Proxmox VE の API のパスの prefix の既定値。
const DefaultAPIBasePath = "/api2/json"

// ClientConfig は Client の設定。
type ClientConfig struct {
	// Endpoint は https://host:8006 の形（API のパスの prefix は Client が足す）。
	Endpoint string
	// APIBasePath は API のパスの prefix（空なら DefaultAPIBasePath、"/" は
	// prefix 無し）。実機と mock で違うときに変える（§6.1 の HTTP の下回り、E-D1）。
	APIBasePath string
	// Token は API トークン。
	Token Token
	// CAFile が空でなければ、それだけを信頼する。
	CAFile string
	// InsecureSkipVerify は証明書を検証しない（明示したときだけ）。
	InsecureSkipVerify bool
	// Timeout は 1 つのリクエストのタイムアウト（0 なら DefaultTimeout）。
	Timeout time.Duration
}

// Token は 1 本の API トークン。
type Token struct {
	// ID は "<user>@<realm>!<tokenname>"。
	ID string
	// Secret はトークンの secret をリクエストの直前に返す。
	Secret func(ctx context.Context) (string, error)
}

// Client は 1 つのクラスタ（の 1 つの endpoint）の API client。
type Client struct {
	base  string
	token Token
	http  *http.Client
}

// CleanBasePath は API のパスの prefix を検証して正規化する（"/" で始まる素のパス。
// 末尾の "/" は落とし、"/" は prefix 無し）。
func CleanBasePath(p string) (string, error) {
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("api_base_path %q は \"/\" で始まるパスで書く（/api2/json など。prefix 無しは \"/\"）", p)
	}
	if strings.ContainsAny(p, "?#% \t\r\n\\") {
		return "", fmt.Errorf("api_base_path %q に query・fragment・エスケープ・空白は書けない", p)
	}
	out := strings.TrimRight(p, "/")
	if out == "" {
		return "", nil
	}
	for _, seg := range strings.Split(out[1:], "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("api_base_path %q に空・\".\"・\"..\" の segment は書けない", p)
		}
	}
	return out, nil
}

// NewClient は cfg の Client を作る。
func NewClient(cfg ClientConfig) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(cfg.Endpoint, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("endpoint %q は https://host:port の形で書く", cfg.Endpoint)
	}
	if !strings.Contains(cfg.Token.ID, "!") || !strings.Contains(cfg.Token.ID, "@") {
		return nil, fmt.Errorf("token_id %q は <user>@<realm>!<tokenname> の形で書く", cfg.Token.ID)
	}
	if cfg.Token.Secret == nil {
		return nil, errors.New("トークンの secret が無い")
	}
	basePath := cfg.APIBasePath
	if basePath == "" {
		basePath = DefaultAPIBasePath
	}
	if basePath, err = CleanBasePath(basePath); err != nil {
		return nil, err
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	switch {
	case cfg.InsecureSkipVerify && cfg.CAFile != "":
		return nil, errors.New("ca_file と insecure_skip_verify は一緒に書けない")
	case cfg.InsecureSkipVerify:
		tlsCfg.InsecureSkipVerify = true
	case cfg.CAFile != "":
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_file %s に証明書が無い", cfg.CAFile)
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
		base:  u.String() + basePath,
		token: cfg.Token,
		http:  &http.Client{Transport: tr, Timeout: timeout},
	}, nil
}

// call は 1 つのリクエストを送り、レスポンスの data を out に読む（out が nil なら読まない）。
// GET・DELETE の params はクエリ、POST・PUT は form の body に入れる。
func (c *Client) call(ctx context.Context, method, path string, params url.Values, out any) error {
	op := method + " " + path
	token := c.token
	secret, err := token.Secret(ctx)
	if err != nil {
		return &Error{Kind: KindAuth, Op: op, Message: "トークンの secret を読めない", Err: err}
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
	req.Header.Set("Authorization", "PVEAPIToken="+token.ID+"="+secret)
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
		return &Error{Kind: KindUnreachable, Op: op, Message: "レスポンスを読めない", Err: err}
	}
	dec = json.NewDecoder(bytes.NewReader(env.Data))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil && !(len(env.Data) == 0 || string(env.Data) == "null") {
		return &Error{Kind: KindUnreachable, Op: op, Message: "レスポンスを読めない", Err: err}
	}
	return nil
}

// failureMessage は失敗したレスポンスのメッセージ（HTTP の reason phrase と、あれば errors の中身）。
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

// scrub は err のメッセージから secret を消す（URL に入れることは無いが念のため）。
func scrub(err error, secret string) error {
	if secret != "" && strings.Contains(err.Error(), secret) {
		return errors.New(strings.ReplaceAll(err.Error(), secret, "***"))
	}
	return err
}

// ---- API ----

// Resource は /cluster/resources?type=vm の 1 件。vmid・ノード・type は
// クラスタの設定（pmxcfs）から来る。名前・status・template・lock は pvestatd
// が約 10 秒ごとに更新するキャッシュなので、作った直後の VM は名前が空で、status が
// 古いことがある。名前が空の field は Provider が設定で確かめ、電源と lock は
// CurrentStatus で読む。Pool は pool の設定（user.cfg）から来る今の値で、
// トークンにその pool の Pool.Audit が無ければ載らない（空）。
type Resource struct {
	Type     string
	VMID     int
	Name     string
	Node     string
	Status   string
	Template bool
	Lock     string
	Pool     string
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
			return nil, &Error{Kind: KindUnreachable, Op: "GET /cluster/resources", Message: fmt.Sprintf("読めない vmid %v", m["vmid"])}
		}
		out = append(out, Resource{
			Type:     scalar(m["type"]),
			VMID:     vmid,
			Name:     scalar(m["name"]),
			Node:     scalar(m["node"]),
			Status:   scalar(m["status"]),
			Template: scalar(m["template"]) == "1",
			Lock:     scalar(m["lock"]),
			Pool:     scalar(m["pool"]),
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
// pvestatd が定期的に更新するキャッシュで古いことがあるので、電源はこちらで読む。
func (c *Client) CurrentStatus(ctx context.Context, node string, vmid int) (VMStatus, error) {
	var raw map[string]any
	if err := c.call(ctx, http.MethodGet, vmPath(node, vmid)+"/status/current", nil, &raw); err != nil {
		return VMStatus{}, err
	}
	return VMStatus{Status: scalar(raw["status"]), Lock: scalar(raw["lock"])}, nil
}

// FirewallOptions は VM の FW の options（/firewall/options。書かれていない
// キーは返らない）を返す。要る権限は VM.Audit。
func (c *Client) FirewallOptions(ctx context.Context, node string, vmid int) (map[string]string, error) {
	var raw map[string]any
	if err := c.call(ctx, http.MethodGet, vmPath(node, vmid)+"/firewall/options", nil, &raw); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		out[k] = scalar(v)
	}
	return out, nil
}

// SetFirewallOptions は VM の FW の options を書く（PUT、同期。渡さないキーは
// そのまま）。要る権限は VM.Config.Network。
func (c *Client) SetFirewallOptions(ctx context.Context, node string, vmid int, params url.Values) error {
	return c.call(ctx, http.MethodPut, vmPath(node, vmid)+"/firewall/options", params, nil)
}

// FirewallRule は VM の FW の規則の 1 つ（GET /firewall/rules の要素）。
type FirewallRule struct {
	// Pos は並びの位置（0 が先頭）。
	Pos int
	// Digest は並び全体の digest（書き換えに渡すと、間に変わっていれば拒否される）。
	Digest string
	// Fields は field（type・action・iface・source・dest・proto・dport・sport・
	// icmp-type・macro・log・enable・comment。書かれていない field は無い）。
	Fields map[string]string
}

// FirewallRules は VM の FW の規則を並びの順に返す。要る権限は VM.Audit。
func (c *Client) FirewallRules(ctx context.Context, node string, vmid int) ([]FirewallRule, error) {
	var raw []map[string]any
	if err := c.call(ctx, http.MethodGet, vmPath(node, vmid)+"/firewall/rules", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]FirewallRule, 0, len(raw))
	for i, m := range raw {
		r := FirewallRule{Pos: i, Fields: map[string]string{}}
		for k, v := range m {
			switch k {
			case "pos":
				if n, err := strconv.Atoi(scalar(v)); err == nil {
					r.Pos = n
				}
			case "digest":
				r.Digest = scalar(v)
			case "ipversion":
			default:
				r.Fields[k] = scalar(v)
			}
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Pos < out[j].Pos })
	return out, nil
}

// CreateFirewallRule は規則を作る（POST、同期）。Proxmox はいつも並びの先頭に
// 入れ、enable を渡さなければ 0 にする。要る権限は VM.Config.Network。
func (c *Client) CreateFirewallRule(ctx context.Context, node string, vmid int, params url.Values) error {
	return c.call(ctx, http.MethodPost, vmPath(node, vmid)+"/firewall/rules", params, nil)
}

// UpdateFirewallRule は pos の規則を書き換える・動かす（PUT、同期。moveto は
// 「元の位置 moveto の規則の前」に入れる）。要る権限は VM.Config.Network。
func (c *Client) UpdateFirewallRule(ctx context.Context, node string, vmid, pos int, params url.Values) error {
	return c.call(ctx, http.MethodPut, vmPath(node, vmid)+"/firewall/rules/"+strconv.Itoa(pos), params, nil)
}

// DeleteFirewallRule は pos の規則を消す（DELETE、同期）。要る権限は VM.Config.Network。
func (c *Client) DeleteFirewallRule(ctx context.Context, node string, vmid, pos int, digest string) error {
	var params url.Values
	if digest != "" {
		params = url.Values{"digest": {digest}}
	}
	return c.call(ctx, http.MethodDelete, vmPath(node, vmid)+"/firewall/rules/"+strconv.Itoa(pos), params, nil)
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

// Resize はディスクを size（"32G" など、絶対値）にする。バージョンによって UPID を返す
// （返さなければ ""）。
func (c *Client) Resize(ctx context.Context, node string, vmid int, disk, size string) (string, error) {
	return c.task(ctx, http.MethodPut, vmPath(node, vmid)+"/resize", url.Values{"disk": {disk}, "size": {size}})
}

// PowerAction は status/<action>（start・shutdown・stop・reboot・reset）を送る
// （UPID を返す）。
func (c *Client) PowerAction(ctx context.Context, node string, vmid int, action string, params url.Values) (string, error) {
	return c.task(ctx, http.MethodPost, vmPath(node, vmid)+"/status/"+action, params)
}

// Destroy は VM を消す（UPID を返す）。設定にあるディスク（unused を含む）と、
// ジョブ・HA の設定も消す。unreferenced なら、設定に無いがその vmid の名前の
// volume も消す（destroy-unreferenced-disks）。
func (c *Client) Destroy(ctx context.Context, node string, vmid int, unreferenced bool) (string, error) {
	params := url.Values{"purge": {"1"}, "destroy-unreferenced-disks": {"0"}}
	if unreferenced {
		params.Set("destroy-unreferenced-disks", "1")
	}
	return c.task(ctx, http.MethodDelete, vmPath(node, vmid), params)
}

// MoveDiskToVM は vmid の disk（slot か unusedN）を targetVMID の targetDisk に
// 付け替える（Proxmox の reassign、POST /move_disk の target-vmid。UPID を
// 返す、Backlog #204）。volume は消さずに targetVMID の名前に変わり、元の VM の
// 設定から外れる。2 つの VM は同じノードにあること、元の VM が走っていれば
// disk は unusedN であること（Proxmox の決まり）。
func (c *Client) MoveDiskToVM(ctx context.Context, node string, vmid int, disk string, targetVMID int, targetDisk string) (string, error) {
	return c.task(ctx, http.MethodPost, vmPath(node, vmid)+"/move_disk", url.Values{
		"disk": {disk}, "target-vmid": {strconv.Itoa(targetVMID)}, "target-disk": {targetDisk},
	})
}

// SetPool は VM を pool に入れる（ほかの pool にあれば移す、allow-move）。
// remove なら pool から外す。PUT /pools（Proxmox VE 8.1 から）、同期。
func (c *Client) SetPool(ctx context.Context, pool string, vmid int, remove bool) error {
	params := url.Values{"poolid": {pool}, "vms": {strconv.Itoa(vmid)}}
	if remove {
		params.Set("delete", "1")
	} else {
		params.Set("allow-move", "1")
	}
	return c.call(ctx, http.MethodPut, "/pools", params, nil)
}

// ActiveTasks は node の vmid の実行中の task（UPID の type）を返す（GET
// /nodes/<node>/tasks?source=active&vmid=<vmid>）。Apply が待ち切れなかった task を、
// 覚えておかずに観測で見つける（E-D1）。他人の始めた task を読むには /nodes/<node> の
// Sys.Audit が要る（PVEAuditor にある）。
func (c *Client) ActiveTasks(ctx context.Context, node string, vmid int) ([]string, error) {
	var raw []map[string]any
	params := url.Values{"source": {"active"}, "vmid": {strconv.Itoa(vmid)}}
	if err := c.call(ctx, http.MethodGet, "/nodes/"+url.PathEscape(node)+"/tasks", params, &raw); err != nil {
		return nil, err
	}
	var out []string
	for _, m := range raw {
		if scalar(m["id"]) != strconv.Itoa(vmid) && scalar(m["id"]) != "" {
			continue
		}
		if st := scalar(m["status"]); st != "" && st != "running" {
			continue // 終わった task（active でも終わった直後に載ることがある）
		}
		out = append(out, scalar(m["type"]))
	}
	return out, nil
}

// TaskStatus は非同期の task の状態。
type TaskStatus struct {
	Running    bool
	ExitStatus string
}

// OK は task が成功で終わったかを返す。
func (t TaskStatus) OK() bool { return !t.Running && t.ExitStatus == "OK" }

// Task は upid の task の状態を返す。
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
		return "", fmt.Errorf("%q は UPID でない", upid)
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
