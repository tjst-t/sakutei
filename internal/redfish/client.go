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

// Package redfish は物理サーバの BMC を Redfish で扱うための HTTP クライアント
// （設計書 v3.15 §29.1「Redfish の BMC provider」、Backlog #48）。
//
// 設計判断:
//
//   - stdlib only: net/http と encoding/json で書く（gofish は使わない）。
//   - HTTPS と Basic 認証だけ。自己署名の証明書は、CA ファイルを指定したか
//     InsecureSkipVerify を明示したときだけ許す。
//   - Reset・Boot の PATCH・InsertMedia・EjectMedia・Bios/Settings の PATCH は
//     200 / 204 を成功とし、本文は期待しない。Redfish の Task（202）は使わない。
//   - 観測 state レス（§3 原則 1）: System・Manager・VirtualMedia・Bios のパスは
//     キャッシュせず、呼ぶたびに辿り直す。
//   - エラーは Error の Kind で分ける（errors.go）。届かない・タイムアウト・
//     5xx・認証以外の想定外は KindUnreachable、401 / 403 は KindAuth、証明書の
//     検証失敗は KindCertificate、その他の 4xx は KindRequest（BMC の内容つき）。
package redfish

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
	"strings"
	"time"
)

// 電源の状態（ComputerSystem.PowerState）。
const (
	PowerOn          = "On"
	PowerOff         = "Off"
	PowerPoweringOn  = "PoweringOn"
	PowerPoweringOff = "PoweringOff"
)

// Reset の種類（ResetType）。
const (
	ResetOn               = "On"
	ResetForceOff         = "ForceOff"
	ResetGracefulShutdown = "GracefulShutdown"
	ResetForceRestart     = "ForceRestart"
	ResetGracefulRestart  = "GracefulRestart"
)

// 起動先の上書きの有無（Boot.BootSourceOverrideEnabled）。
const (
	BootDisabled   = "Disabled"
	BootOnce       = "Once"
	BootContinuous = "Continuous"
)

// ServiceRoot は Redfish のサービスルート。
const ServiceRoot = "/redfish/v1"

// DefaultTimeout は Config.Timeout を指定しないときの 1 要求あたりの時間切れ。
const DefaultTimeout = 10 * time.Second

// maxBody は読む応答本文の上限（壊れた BMC に読み続けさせない）。
const maxBody = 4 << 20

// Config は 1 台の BMC への接続設定。
type Config struct {
	// Endpoint は BMC のアドレス（例 "https://172.20.20.2"）。scheme を省けば
	// https とみなす。http は受け付けない。
	Endpoint string
	// Username・Password は Basic 認証の資格。Password は provider が SecretRef
	// （§20）から解決して渡す。
	Username string
	Password string
	// PasswordFunc は要求のたびに Password を解決する（SecretRef の JIT 解決、
	// §20.2。値を Client に持たない）。設定すれば Password より優先する。解決
	// できなければその要求は KindUnreachable（何も操作しない側）。
	PasswordFunc func(ctx context.Context) (string, error)
	// CAFile は BMC の証明書を検証する CA（PEM）のファイル。指定すれば
	// システムの CA の代わりにこれだけを使う。
	CAFile string
	// InsecureSkipVerify は証明書を検証しない（自己署名をそのまま許す）。
	// 明示したときだけ使う。
	InsecureSkipVerify bool
	// Timeout は 1 要求あたりの時間切れ。0 なら DefaultTimeout。
	Timeout time.Duration
	// SystemPath は使う ComputerSystem のパス（例 "/redfish/v1/Systems/1"）。
	// 空なら /redfish/v1/Systems の最初のメンバー。
	SystemPath string
	// ManagerPath は VirtualMedia を持つ Manager のパス（例
	// "/redfish/v1/Managers/1"）。空なら /redfish/v1/Managers の最初のメンバー。
	ManagerPath string
}

// Boot は起動先の上書きの設定。
type Boot struct {
	// Enabled は BootSourceOverrideEnabled（Disabled / Once / Continuous）。
	Enabled string
	// Target は BootSourceOverrideTarget。Disabled でも BMC は設定した値を
	// 返し続けることがある（比べるかどうかは provider が決める）。
	Target string
	// Mode は BootSourceOverrideMode（UEFI / Legacy）。観測だけ。
	Mode string
	// AllowableTargets は BootSourceOverrideTarget@Redfish.AllowableValues。
	AllowableTargets []string
}

// System は ComputerSystem の観測。
type System struct {
	// Path は読んだ ComputerSystem のパス。
	Path           string
	PowerState     string
	UUID           string
	TotalMemoryGiB float64
	ProcessorCount int
	Boot           Boot
	// ResetTypes は #ComputerSystem.Reset の ResetType@Redfish.AllowableValues。
	ResetTypes []string
	// ResetTarget は #ComputerSystem.Reset の target。
	ResetTarget string
	// MACAddresses は EthernetInterfaces のメンバーの MACAddress（BMC が返した順）。
	MACAddresses []string
}

// VirtualMedia は CD を扱える VirtualMedia の観測。
type VirtualMedia struct {
	// Path は読んだ VirtualMedia のパス。
	Path       string
	MediaTypes []string
	Inserted   bool
	Image      string
	// InsertTarget・EjectTarget は Actions の target。
	InsertTarget string
	EjectTarget  string
}

// Bios は ComputerSystem の Bios の観測（§29.1、#140）。値は JSON の
// 文字列・数・真偽（encoding/json で読んだ string / float64 / bool）。
type Bios struct {
	// Path は読んだ Bios のパス。
	Path string
	// AttributeRegistry は Bios.AttributeRegistry（属性の登録簿の Id）。
	AttributeRegistry string
	// Attributes は今の値（Bios.Attributes）。
	Attributes map[string]any
	// SettingsPath は保留中の変更の置き場（@Redfish.Settings の
	// SettingsObject、無ければ Path + "/Settings"）。
	SettingsPath string
	// Pending は Settings の Attributes（次の再起動で効く）。BMC によっては
	// 今の値と同じものも返す。Settings を持たない BMC では空。
	Pending map[string]any
	// ResetPending は Bios.ResetBiosToDefaultsPending（次の再起動で既定値に
	// 戻る）。
	ResetPending bool
}

// AttributeRegistry は Bios の属性の登録簿（必要なところだけ）。
type AttributeRegistry struct {
	ID         string
	Attributes []RegistryAttribute
}

// RegistryAttribute は登録簿の 1 属性。
type RegistryAttribute struct {
	AttributeName string   `json:"AttributeName"`
	Type          string   `json:"Type"` // Enumeration / String / Integer / Boolean / Password
	ReadOnly      bool     `json:"ReadOnly"`
	Immutable     bool     `json:"Immutable"`
	Values        []string `json:"-"` // Enumeration の Value[].ValueName
	MinLength     *int     `json:"MinLength"`
	MaxLength     *int     `json:"MaxLength"`
	LowerBound    *float64 `json:"LowerBound"`
	UpperBound    *float64 `json:"UpperBound"`
}

// ErrNoBios は System が Bios を持たないこと（Bios のリンクが無い・404）。
var ErrNoBios = errors.New("the system has no Bios resource")

// IsNoBios は err が ErrNoBios（BMC が Bios を持たない）かを返す。
func IsNoBios(err error) bool { return errors.Is(err, ErrNoBios) }

// Client は 1 台の BMC への Redfish の操作。provider はこの interface に対して
// 書き、テストは偽 BMC（redfishtest）に向けた実装か差し替えで回す。
type Client interface {
	// GetSystem は ComputerSystem を読む（EthernetInterfaces も辿る）。
	GetSystem(ctx context.Context) (System, error)
	// Reset は #ComputerSystem.Reset を resetType で送る。
	Reset(ctx context.Context, resetType string) error
	// PatchBoot は Boot の BootSourceOverrideEnabled と Target を PATCH する。
	// target が空なら Enabled だけを送る。
	PatchBoot(ctx context.Context, enabled, target string) error
	// GetVirtualMedia は CD を扱える VirtualMedia を読む。
	GetVirtualMedia(ctx context.Context) (VirtualMedia, error)
	// InsertMedia は image（URL か BMC から見えるパス）を CD に入れる。
	InsertMedia(ctx context.Context, image string) error
	// EjectMedia は CD を取り出す。
	EjectMedia(ctx context.Context) error
	// GetBios は ComputerSystem の Bios（今の値）と、その Settings（次の再起動で
	// 効く保留中の変更）を読む。System に Bios が無ければ ErrNoBios を包んだ
	// KindRequest のエラー。
	GetBios(ctx context.Context) (Bios, error)
	// PatchBiosSettings は attrs を Bios の Settings に PATCH する（保留中の変更に
	// 足す。再起動はしない）。
	PatchBiosSettings(ctx context.Context, attrs map[string]any) error
	// GetAttributeRegistry は Id が id の AttributeRegistry（Bios の
	// AttributeRegistry）を /redfish/v1/Registries から探して読む。無ければ
	// KindRequest のエラー。
	GetAttributeRegistry(ctx context.Context, id string) (AttributeRegistry, error)
}

// HTTPClient は net/http による Client の実装。
type HTTPClient struct {
	base     *url.URL
	username string
	password string
	pwFunc   func(ctx context.Context) (string, error)
	sysPath  string
	mgrPath  string
	hc       *http.Client
}

var _ Client = (*HTTPClient)(nil)

// New は cfg から HTTPClient を作る。Endpoint が https でない・CA ファイルが
// 読めないときはエラー。
func New(cfg Config) (*HTTPClient, error) {
	ep := strings.TrimSpace(cfg.Endpoint)
	if ep == "" {
		return nil, errors.New("redfish: endpoint is empty")
	}
	if !strings.Contains(ep, "://") {
		ep = "https://" + ep
	}
	u, err := url.Parse(ep)
	if err != nil {
		return nil, fmt.Errorf("redfish: endpoint %q: %w", cfg.Endpoint, err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("redfish: endpoint %q: only https is supported", cfg.Endpoint)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("redfish: endpoint %q: no host", cfg.Endpoint)
	}
	u.Path, u.RawPath, u.RawQuery, u.Fragment = "", "", "", ""

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	switch {
	case cfg.CAFile != "":
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("redfish: ca file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("redfish: ca file %s: no PEM certificate", cfg.CAFile)
		}
		tlsCfg.RootCAs = pool
	case cfg.InsecureSkipVerify:
		tlsCfg.InsecureSkipVerify = true
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	tr.Proxy = nil

	return &HTTPClient{
		base:     u,
		username: cfg.Username,
		password: cfg.Password,
		pwFunc:   cfg.PasswordFunc,
		sysPath:  cfg.SystemPath,
		mgrPath:  cfg.ManagerPath,
		hc: &http.Client{
			Transport: tr,
			Timeout:   timeout,
			// リダイレクトは辿らない（資格を別の場所へ送らない）。3xx は想定外。
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// ---- Redfish の文書（必要なところだけ） ----

type odataRef struct {
	ID string `json:"@odata.id"`
}

type collectionDoc struct {
	Members []odataRef `json:"Members"`
}

type systemDoc struct {
	PowerState    string `json:"PowerState"`
	UUID          string `json:"UUID"`
	MemorySummary struct {
		TotalSystemMemoryGiB float64 `json:"TotalSystemMemoryGiB"`
	} `json:"MemorySummary"`
	ProcessorSummary struct {
		Count int `json:"Count"`
	} `json:"ProcessorSummary"`
	Boot struct {
		Enabled          string   `json:"BootSourceOverrideEnabled"`
		Target           string   `json:"BootSourceOverrideTarget"`
		Mode             string   `json:"BootSourceOverrideMode"`
		AllowableTargets []string `json:"BootSourceOverrideTarget@Redfish.AllowableValues"`
	} `json:"Boot"`
	EthernetInterfaces odataRef `json:"EthernetInterfaces"`
	Bios               odataRef `json:"Bios"`
	Actions            struct {
		Reset struct {
			Target     string   `json:"target"`
			ResetTypes []string `json:"ResetType@Redfish.AllowableValues"`
		} `json:"#ComputerSystem.Reset"`
	} `json:"Actions"`
}

type ethernetDoc struct {
	MACAddress string `json:"MACAddress"`
}

type managerDoc struct {
	VirtualMedia odataRef `json:"VirtualMedia"`
}

type virtualMediaDoc struct {
	MediaTypes []string `json:"MediaTypes"`
	Inserted   bool     `json:"Inserted"`
	Image      string   `json:"Image"`
	Actions    struct {
		Insert struct {
			Target string `json:"target"`
		} `json:"#VirtualMedia.InsertMedia"`
		Eject struct {
			Target string `json:"target"`
		} `json:"#VirtualMedia.EjectMedia"`
	} `json:"Actions"`
}

type biosDoc struct {
	AttributeRegistry string         `json:"AttributeRegistry"`
	Attributes        map[string]any `json:"Attributes"`
	ResetPending      bool           `json:"ResetBiosToDefaultsPending"`
	Settings          struct {
		Object odataRef `json:"SettingsObject"`
	} `json:"@Redfish.Settings"`
}

type registryFileDoc struct {
	ID       string `json:"Id"`
	Registry string `json:"Registry"`
	Location []struct {
		Language string `json:"Language"`
		URI      string `json:"Uri"`
	} `json:"Location"`
}

type registryDoc struct {
	ID              string `json:"Id"`
	RegistryEntries struct {
		Attributes []struct {
			RegistryAttribute
			Value []struct {
				ValueName string `json:"ValueName"`
			} `json:"Value"`
		} `json:"Attributes"`
	} `json:"RegistryEntries"`
}

// ---- 操作 ----

// GetSystem は Client の実装。
func (c *HTTPClient) GetSystem(ctx context.Context) (System, error) {
	path, doc, err := c.system(ctx)
	if err != nil {
		return System{}, err
	}
	s := System{
		Path:           path,
		PowerState:     doc.PowerState,
		UUID:           doc.UUID,
		TotalMemoryGiB: doc.MemorySummary.TotalSystemMemoryGiB,
		ProcessorCount: doc.ProcessorSummary.Count,
		Boot: Boot{
			Enabled:          doc.Boot.Enabled,
			Target:           doc.Boot.Target,
			Mode:             doc.Boot.Mode,
			AllowableTargets: doc.Boot.AllowableTargets,
		},
		ResetTypes:  doc.Actions.Reset.ResetTypes,
		ResetTarget: doc.Actions.Reset.Target,
	}
	if doc.EthernetInterfaces.ID != "" {
		var coll collectionDoc
		if err := c.getJSON(ctx, doc.EthernetInterfaces.ID, &coll); err != nil {
			return System{}, err
		}
		for _, m := range coll.Members {
			var eth ethernetDoc
			if err := c.getJSON(ctx, m.ID, &eth); err != nil {
				return System{}, err
			}
			if eth.MACAddress != "" {
				s.MACAddresses = append(s.MACAddresses, eth.MACAddress)
			}
		}
	}
	return s, nil
}

// Reset は Client の実装。
func (c *HTTPClient) Reset(ctx context.Context, resetType string) error {
	path, doc, err := c.system(ctx)
	if err != nil {
		return err
	}
	target := doc.Actions.Reset.Target
	if target == "" {
		target = path + "/Actions/ComputerSystem.Reset"
	}
	return c.send(ctx, http.MethodPost, target, map[string]string{"ResetType": resetType})
}

// PatchBoot は Client の実装。
func (c *HTTPClient) PatchBoot(ctx context.Context, enabled, target string) error {
	path, err := c.systemPath(ctx)
	if err != nil {
		return err
	}
	boot := map[string]string{"BootSourceOverrideEnabled": enabled}
	if target != "" {
		boot["BootSourceOverrideTarget"] = target
	}
	return c.send(ctx, http.MethodPatch, path, map[string]any{"Boot": boot})
}

// GetVirtualMedia は Client の実装。
func (c *HTTPClient) GetVirtualMedia(ctx context.Context) (VirtualMedia, error) {
	path, doc, err := c.virtualMedia(ctx)
	if err != nil {
		return VirtualMedia{}, err
	}
	return VirtualMedia{
		Path:         path,
		MediaTypes:   doc.MediaTypes,
		Inserted:     doc.Inserted,
		Image:        doc.Image,
		InsertTarget: doc.Actions.Insert.Target,
		EjectTarget:  doc.Actions.Eject.Target,
	}, nil
}

// InsertMedia は Client の実装。
func (c *HTTPClient) InsertMedia(ctx context.Context, image string) error {
	path, doc, err := c.virtualMedia(ctx)
	if err != nil {
		return err
	}
	target := doc.Actions.Insert.Target
	if target == "" {
		target = path + "/Actions/VirtualMedia.InsertMedia"
	}
	return c.send(ctx, http.MethodPost, target, map[string]any{"Image": image})
}

// EjectMedia は Client の実装。
func (c *HTTPClient) EjectMedia(ctx context.Context) error {
	path, doc, err := c.virtualMedia(ctx)
	if err != nil {
		return err
	}
	target := doc.Actions.Eject.Target
	if target == "" {
		target = path + "/Actions/VirtualMedia.EjectMedia"
	}
	return c.send(ctx, http.MethodPost, target, map[string]any{})
}

// GetBios は Client の実装。
func (c *HTTPClient) GetBios(ctx context.Context) (Bios, error) {
	sysPath, sys, err := c.system(ctx)
	if err != nil {
		return Bios{}, err
	}
	path := sys.Bios.ID
	if path == "" {
		return Bios{}, &Error{Kind: KindRequest, Method: http.MethodGet, Path: sysPath, Err: ErrNoBios,
			Message: "the ComputerSystem has no Bios link"}
	}
	var doc biosDoc
	if err := c.getJSON(ctx, path, &doc); err != nil {
		if re := (*Error)(nil); errors.As(err, &re) && re.StatusCode == http.StatusNotFound {
			re.Err = ErrNoBios
		}
		return Bios{}, err
	}
	b := Bios{
		Path:              path,
		AttributeRegistry: doc.AttributeRegistry,
		Attributes:        doc.Attributes,
		SettingsPath:      doc.Settings.Object.ID,
		ResetPending:      doc.ResetPending,
	}
	if b.Attributes == nil {
		b.Attributes = map[string]any{}
	}
	if b.SettingsPath == "" {
		b.SettingsPath = strings.TrimSuffix(path, "/") + "/Settings"
	}
	var settings biosDoc
	switch err := c.getJSON(ctx, b.SettingsPath, &settings); {
	case err == nil:
		b.Pending = settings.Attributes
	case IsRequest(err):
		// Settings を持たない BMC: 保留中の変更は無い（PATCH は BMC が断る）。
	default:
		return Bios{}, err
	}
	if b.Pending == nil {
		b.Pending = map[string]any{}
	}
	return b, nil
}

// PatchBiosSettings は Client の実装。
func (c *HTTPClient) PatchBiosSettings(ctx context.Context, attrs map[string]any) error {
	b, err := c.GetBios(ctx)
	if err != nil {
		return err
	}
	return c.send(ctx, http.MethodPatch, b.SettingsPath, map[string]any{"Attributes": attrs})
}

// GetAttributeRegistry は Client の実装。/redfish/v1/Registries のメンバー
// （MessageRegistryFile）のうち、Id か Registry が id に当たるものの Location
// の Uri（BMC の上のもの、英語を先に）を読む。
func (c *HTTPClient) GetAttributeRegistry(ctx context.Context, id string) (AttributeRegistry, error) {
	const coll = ServiceRoot + "/Registries"
	var members collectionDoc
	if err := c.getJSON(ctx, coll, &members); err != nil {
		return AttributeRegistry{}, err
	}
	for _, m := range members.Members {
		var f registryFileDoc
		if err := c.getJSON(ctx, m.ID, &f); err != nil {
			if IsRequest(err) {
				continue
			}
			return AttributeRegistry{}, err
		}
		if !registryMatches(f, id) {
			continue
		}
		var uris []string
		for _, l := range f.Location {
			if l.URI == "" {
				continue
			}
			if strings.HasPrefix(strings.ToLower(l.Language), "en") {
				uris = append([]string{l.URI}, uris...)
			} else {
				uris = append(uris, l.URI)
			}
		}
		for _, u := range uris {
			var doc registryDoc
			if err := c.getJSON(ctx, u, &doc); err != nil {
				if IsRequest(err) {
					continue
				}
				return AttributeRegistry{}, err
			}
			if doc.ID != "" && doc.ID != id {
				continue
			}
			reg := AttributeRegistry{ID: id}
			for _, a := range doc.RegistryEntries.Attributes {
				ra := a.RegistryAttribute
				for _, v := range a.Value {
					ra.Values = append(ra.Values, v.ValueName)
				}
				reg.Attributes = append(reg.Attributes, ra)
			}
			return reg, nil
		}
	}
	return AttributeRegistry{}, &Error{Kind: KindRequest, Method: http.MethodGet, Path: coll,
		Message: fmt.Sprintf("no attribute registry %q", id)}
}

// registryMatches は登録簿のファイル f が Id id の登録簿か（Id が同じか、
// Registry〈例 "BiosAttributeRegistry.1.0"〉が id〈例
// "BiosAttributeRegistry.1.0.0"〉の版を除いた頭か）を返す。
func registryMatches(f registryFileDoc, id string) bool {
	if f.ID == id || f.Registry == id {
		return true
	}
	return f.Registry != "" && strings.HasPrefix(id, f.Registry+".")
}

// ---- パスの解決 ----

func (c *HTTPClient) systemPath(ctx context.Context) (string, error) {
	if c.sysPath != "" {
		return c.sysPath, nil
	}
	return c.firstMember(ctx, ServiceRoot+"/Systems")
}

func (c *HTTPClient) system(ctx context.Context) (string, systemDoc, error) {
	path, err := c.systemPath(ctx)
	if err != nil {
		return "", systemDoc{}, err
	}
	var doc systemDoc
	if err := c.getJSON(ctx, path, &doc); err != nil {
		return "", systemDoc{}, err
	}
	return path, doc, nil
}

// virtualMedia は Manager の VirtualMedia のうち MediaTypes に CD を含む最初の
// ものを返す。
func (c *HTTPClient) virtualMedia(ctx context.Context) (string, virtualMediaDoc, error) {
	mgr := c.mgrPath
	if mgr == "" {
		var err error
		if mgr, err = c.firstMember(ctx, ServiceRoot+"/Managers"); err != nil {
			return "", virtualMediaDoc{}, err
		}
	}
	var m managerDoc
	if err := c.getJSON(ctx, mgr, &m); err != nil {
		return "", virtualMediaDoc{}, err
	}
	collPath := m.VirtualMedia.ID
	if collPath == "" {
		collPath = strings.TrimSuffix(mgr, "/") + "/VirtualMedia"
	}
	var coll collectionDoc
	if err := c.getJSON(ctx, collPath, &coll); err != nil {
		return "", virtualMediaDoc{}, err
	}
	for _, ref := range coll.Members {
		var vm virtualMediaDoc
		if err := c.getJSON(ctx, ref.ID, &vm); err != nil {
			return "", virtualMediaDoc{}, err
		}
		for _, t := range vm.MediaTypes {
			if t == "CD" {
				return ref.ID, vm, nil
			}
		}
	}
	return "", virtualMediaDoc{}, &Error{Kind: KindRequest, Method: http.MethodGet, Path: collPath,
		Message: "no VirtualMedia with MediaTypes CD"}
}

func (c *HTTPClient) firstMember(ctx context.Context, path string) (string, error) {
	var coll collectionDoc
	if err := c.getJSON(ctx, path, &coll); err != nil {
		return "", err
	}
	if len(coll.Members) == 0 || coll.Members[0].ID == "" {
		return "", &Error{Kind: KindRequest, Method: http.MethodGet, Path: path, Message: "collection has no members"}
	}
	return coll.Members[0].ID, nil
}

// ---- HTTP ----

// getJSON は path を GET し、200 の本文を v に読む。
func (c *HTTPClient) getJSON(ctx context.Context, path string, v any) error {
	status, body, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return &Error{Kind: KindUnreachable, Method: http.MethodGet, Path: path, StatusCode: status,
			Message: "unexpected status"}
	}
	if err := json.Unmarshal(body, v); err != nil {
		return &Error{Kind: KindUnreachable, Method: http.MethodGet, Path: path, StatusCode: status,
			Message: "malformed JSON", Err: err}
	}
	return nil
}

// send は path に JSON の本文で method を送り、200 / 204 を成功とする。
// 本文は読まない。202（Task）は想定外として KindUnreachable。
func (c *HTTPClient) send(ctx context.Context, method, path string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("redfish: %s %s: %w", method, path, err)
	}
	status, _, err := c.do(ctx, method, path, b)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusNoContent {
		msg := "unexpected status"
		if status == http.StatusAccepted {
			msg = "BMC started a Task (202); tasks are not supported"
		}
		return &Error{Kind: KindUnreachable, Method: method, Path: path, StatusCode: status, Message: msg}
	}
	return nil
}

// do は 1 要求を送り、2xx / 3xx の状態と本文を返す。届かない・4xx・5xx は
// Error にする。
func (c *HTTPClient) do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	ref, err := url.Parse(path)
	if err != nil {
		return 0, nil, fmt.Errorf("redfish: path %q: %w", path, err)
	}
	u := c.base.ResolveReference(ref)
	if u.Host != c.base.Host || u.Scheme != c.base.Scheme {
		// BMC が返したリンクで別のホストへ資格を送らない。
		return 0, nil, &Error{Kind: KindUnreachable, Method: method, Path: path,
			Message: "link points outside the BMC endpoint"}
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return 0, nil, fmt.Errorf("redfish: %s %s: %w", method, path, err)
	}
	password := c.password
	if c.pwFunc != nil {
		pw, err := c.pwFunc(ctx)
		if err != nil {
			return 0, nil, &Error{Kind: KindUnreachable, Method: method, Path: path,
				Message: "cannot resolve the password", Err: err}
		}
		password = pw
	}
	req.SetBasicAuth(c.username, password)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		kind := KindUnreachable
		if isCertError(err) {
			kind = KindCertificate
		}
		return 0, nil, &Error{Kind: kind, Method: method, Path: path, Err: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return 0, nil, &Error{Kind: KindUnreachable, Method: method, Path: path, StatusCode: resp.StatusCode, Err: err}
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return 0, nil, &Error{Kind: KindAuth, Method: method, Path: path, StatusCode: resp.StatusCode,
			Message: errorMessage(data)}
	case resp.StatusCode >= 500:
		return 0, nil, &Error{Kind: KindUnreachable, Method: method, Path: path, StatusCode: resp.StatusCode,
			Message: errorMessage(data)}
	case resp.StatusCode >= 400:
		return 0, nil, &Error{Kind: KindRequest, Method: method, Path: path, StatusCode: resp.StatusCode,
			Message: errorMessage(data)}
	}
	return resp.StatusCode, data, nil
}

func isCertError(err error) bool {
	var verr *tls.CertificateVerificationError
	var uerr x509.UnknownAuthorityError
	var herr x509.HostnameError
	var ierr x509.CertificateInvalidError
	return errors.As(err, &verr) || errors.As(err, &uerr) || errors.As(err, &herr) || errors.As(err, &ierr)
}

// errorMessage は Redfish のエラー本文から人が読む内容を取り出す。JSON で
// なければ本文の先頭を返す。
func errorMessage(body []byte) string {
	var doc struct {
		Error struct {
			Code     string `json:"code"`
			Message  string `json:"message"`
			Extended []struct {
				Message string `json:"Message"`
			} `json:"@Message.ExtendedInfo"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &doc) == nil && (doc.Error.Message != "" || len(doc.Error.Extended) > 0) {
		parts := []string{}
		if doc.Error.Message != "" {
			parts = append(parts, doc.Error.Message)
		}
		for _, e := range doc.Error.Extended {
			if e.Message != "" && e.Message != doc.Error.Message {
				parts = append(parts, e.Message)
			}
		}
		return strings.Join(parts, "; ")
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
