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

// invoke.go は観測に残らない一回きりの操作（core.Invoker、Procedure の invoke
// からだけ呼ぶ、不変条件 15、E-A2・N-A2・E-A5・N-A4）。
//
//	type system：
//	  boot_once      {target: Cd}   起動先の上書きを Once・target にする（PATCH だけで
//	                                再起動しない。BMC は次の 1 回の起動で Disabled に戻す）
//	  reset          {}             GracefulRestart（切れていれば On）
//	  force_restart  {}             ForceRestart（切れていれば On）。破壊的な操作
//	  force_off      {}             ForceOff。破壊的な操作（graceful で止まらない台、E-A1）
//	type firmware：
//	  verify         {image_uri, sha256, image_username?, image_password?}
//	                 イメージを取りに行って sha256 を確かめる（BMC には送らない）
//	  stage          {image_uri, transfer?, apply_time?, sha256?, image_username?, image_password?}
//	                 SimpleUpdate（transfer: simple_update、既定）か MultipartHttpPushUri への
//	                 送信（transfer: multipart。sha256 を書けば送る中身を確かめる）。破壊的な操作
//	  reset・force_restart  その部品のノードの再起動（OnReset の更新を効かせる）
//
// 送れていない（BMC に届かない・電源の切り替えの途中・イメージを取りに行けない）
// ときだけ core.ErrNotSent を返す（次の cycle で送り直してよい、不変条件 16）。
// 直さないと通らない理由（BMC が持たない起動先・更新できない部品・sha256 の不一致・
// 未知の引数）と、送ったあとのエラーは ErrNotSent でないエラーで、Procedure を止める。
// image_password は secret の参照（"<backend>:<path>"）で書き、WriteContext.Secret で
// 使う直前に取り出す（エラーメッセージに出さない、E-G2）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/providers/redfish/client"
)

// 一回きりの操作の名前。
const (
	OpBootOnce     = "boot_once"
	OpReset        = "reset"
	OpForceRestart = "force_restart"
	OpForceOff     = "force_off"
	OpVerify       = "verify"
	OpStage        = "stage"
)

// stage の transfer の値。
const (
	TransferSimpleUpdate = "simple_update"
	TransferMultipart    = "multipart"
)

// Ops は type kind の操作（E-A2・E-A5・E-C1）。
func (p *Provider) Ops(kind string) map[string]core.OpSpec {
	switch kind {
	case KindSystem:
		return map[string]core.OpSpec{
			OpBootOnce: {}, OpReset: {},
			OpForceRestart: {Destructive: true}, OpForceOff: {Destructive: true},
		}
	case KindFirmware:
		return map[string]core.OpSpec{
			OpVerify: {}, OpStage: {Destructive: true},
			OpReset: {}, OpForceRestart: {Destructive: true},
		}
	}
	return nil
}

// opArgs は操作ごとに書ける引数。
var opArgs = map[string][]string{
	OpBootOnce:     {"target"},
	OpReset:        {},
	OpForceRestart: {},
	OpForceOff:     {},
	OpVerify:       {"image_uri", "sha256", "image_username", "image_password"},
	OpStage:        {"image_uri", "transfer", "apply_time", "sha256", "image_username", "image_password"},
}

func notSent(format string, a ...any) error {
	return fmt.Errorf("%w: %s", core.ErrNotSent, fmt.Sprintf(format, a...))
}

// Invoke は操作を 1 回だけ送る。
func (p *Provider) Invoke(ctx context.Context, w core.WriteContext, id core.ID, op string, args core.Fields) error {
	if _, ok := p.Ops(id.Kind())[op]; !ok {
		return fmt.Errorf("type %q に操作 %q が無い", id.Kind(), op)
	}
	for k := range args {
		if !has(opArgs[op], k) {
			return fmt.Errorf("操作 %s に未知の引数 %q（書ける引数: %s）", op, k, strings.Join(opArgs[op], ", "))
		}
	}
	nodeName, comp := split(id)
	n, ok := p.nodes[nodeName]
	if !ok {
		return fmt.Errorf("ノード %q の BMC が settings.nodes に無い", nodeName)
	}
	c, err := n.writer(w)
	if err != nil {
		return err
	}
	switch op {
	case OpBootOnce:
		return bootOnce(ctx, c, argString(args, "target"))
	case OpReset:
		return reset(ctx, c, client.ResetGracefulRestart)
	case OpForceRestart:
		return reset(ctx, c, client.ResetForceRestart)
	case OpForceOff:
		return forceOff(ctx, c)
	case OpVerify:
		spec, err := parseImage(args, op)
		if err != nil {
			return err
		}
		sum, err := p.fetch(ctx, w, spec, io.Discard)
		if err != nil {
			return notSent("%v", err)
		}
		if spec.sha256 != "" && sum != spec.sha256 {
			return fmt.Errorf("イメージ %s の sha256 が %s（want %s）", redact(spec.uri), sum, spec.sha256)
		}
		return nil
	case OpStage:
		spec, err := parseImage(args, op)
		if err != nil {
			return err
		}
		return p.stage(ctx, w, c, comp, spec)
	}
	return fmt.Errorf("未知の操作 %q", op)
}

func argString(args core.Fields, k string) string {
	s, _ := args[k].(string)
	return strings.TrimSpace(s)
}

func bootOnce(ctx context.Context, c client.Client, target string) error {
	if target == "" {
		return errors.New("boot_once には args.target（例 Cd）を書く")
	}
	sys, err := c.GetSystem(ctx)
	if err != nil {
		return notSent("%v", err)
	}
	if len(sys.Boot.AllowableTargets) > 0 && !has(sys.Boot.AllowableTargets, target) {
		return fmt.Errorf("起動先 %q を BMC が持たない（選べる値: %s）", target, strings.Join(sys.Boot.AllowableTargets, ", "))
	}
	if err := c.PatchBoot(ctx, client.BootOnce, target); err != nil {
		return fmt.Errorf("起動先の PATCH: %w", err)
	}
	after, err := c.GetSystem(ctx)
	if err != nil {
		return fmt.Errorf("起動先の読み直し: %w", err)
	}
	if after.Boot.Enabled != client.BootOnce || after.Boot.Target != target {
		return fmt.Errorf("PATCH のあと BMC の起動先の上書きが %s %s（want %s %s）", after.Boot.Enabled, after.Boot.Target, client.BootOnce, target)
	}
	return nil
}

// reset は再起動する。切れた台の再起動は電源オン。切り替えの途中なら送らない。
func reset(ctx context.Context, c client.Client, t string) error {
	sys, err := c.GetSystem(ctx)
	if err != nil {
		return notSent("%v", err)
	}
	switch sys.PowerState {
	case client.PowerOff:
		t = client.ResetOn
	case client.PowerOn:
	default:
		return notSent("電源が %s（ほかの電源の操作の途中）", sys.PowerState)
	}
	return sendReset(ctx, c, sys, t)
}

func forceOff(ctx context.Context, c client.Client) error {
	sys, err := c.GetSystem(ctx)
	if err != nil {
		return notSent("%v", err)
	}
	if sys.PowerState == client.PowerOff {
		return nil // もう切れている
	}
	return sendReset(ctx, c, sys, client.ResetForceOff)
}

func sendReset(ctx context.Context, c client.Client, sys client.System, t string) error {
	if len(sys.ResetTypes) > 0 && !has(sys.ResetTypes, t) {
		return fmt.Errorf("BMC が ResetType %s を持たない（選べる値: %s）", t, strings.Join(sys.ResetTypes, ", "))
	}
	if err := c.Reset(ctx, t); err != nil {
		return fmt.Errorf("Reset %s: %w", t, err)
	}
	return nil
}

// imageSpec は verify・stage の引数。
type imageSpec struct {
	uri, sha256, transfer, applyTime string
	username                         string
	password                         any // secret の参照（無ければ nil）
}

func parseImage(args core.Fields, op string) (imageSpec, error) {
	s := imageSpec{
		uri: argString(args, "image_uri"), sha256: strings.ToLower(argString(args, "sha256")),
		transfer: argString(args, "transfer"), applyTime: argString(args, "apply_time"),
		username: argString(args, "image_username"),
	}
	if v, ok := args["image_password"]; ok {
		if m, isMap := v.(map[string]any); isMap {
			v = m["secret"]
		}
		ref, isStr := v.(string)
		if _, err := core.ParseSecretRef(ref); !isStr || err != nil {
			return s, errors.New("image_password は secret の参照（\"<backend>:<path>\"）で書く（平文は書けない）")
		}
		s.password = ref
	}
	if s.password != nil && s.username == "" {
		return s, errors.New("image_password には image_username を書く")
	}
	if s.transfer == "" {
		s.transfer = TransferSimpleUpdate
	}
	if s.transfer != TransferSimpleUpdate && s.transfer != TransferMultipart {
		return s, fmt.Errorf("transfer %q: %s か %s", s.transfer, TransferSimpleUpdate, TransferMultipart)
	}
	if s.applyTime == "" {
		s.applyTime = client.ApplyImmediate
	}
	if s.applyTime != client.ApplyImmediate && s.applyTime != client.ApplyOnReset {
		return s, fmt.Errorf("apply_time %q: %s か %s", s.applyTime, client.ApplyImmediate, client.ApplyOnReset)
	}
	if s.sha256 != "" {
		if b, err := hex.DecodeString(s.sha256); err != nil || len(b) != 32 {
			return s, errors.New("sha256 は 16 進 64 文字")
		}
	} else if op == OpVerify {
		return s, errors.New("verify には sha256 を書く")
	}
	u, err := url.Parse(s.uri)
	switch {
	case s.uri == "" || err != nil:
		return s, fmt.Errorf("%s には image_uri（http・https の URL）を書く", op)
	case u.User != nil:
		return s, errors.New("image_uri に credential を書かない（image_username・image_password を使う）")
	case u.Scheme == "http" || u.Scheme == "https":
		if u.Host == "" {
			return s, fmt.Errorf("image_uri %q にホストが無い", s.uri)
		}
	case u.Scheme == "file" && (op == OpVerify || s.transfer == TransferMultipart):
		if u.Path == "" {
			return s, fmt.Errorf("image_uri %q にパスが無い", s.uri)
		}
	default:
		return s, fmt.Errorf("image_uri %q は http・https（file は transfer: multipart と verify だけ。BMC は Sakutei のファイルを読めない）", redact(s.uri))
	}
	return s, nil
}

func redact(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	u.User = url.User("***")
	return u.String()
}

// fetch はイメージを取りに行って w に書き、sha256（小文字の 16 進）を返す。エラーに
// credential を出さない。
func (p *Provider) fetch(ctx context.Context, w core.WriteContext, s imageSpec, dst io.Writer) (string, error) {
	var rd io.Reader
	u, _ := url.Parse(s.uri)
	if u.Scheme == "file" {
		f, err := os.Open(u.Path)
		if err != nil {
			return "", fmt.Errorf("イメージを開けない: %w", err)
		}
		defer f.Close()
		rd = f
	} else {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.uri, nil)
		if err != nil {
			return "", fmt.Errorf("GET %s: %w", redact(s.uri), err)
		}
		if s.username != "" {
			pw := ""
			if s.password != nil {
				if pw, err = w.Secret(ctx, s.password); err != nil {
					return "", fmt.Errorf("image_password を取り出せない: %w", err)
				}
			}
			req.SetBasicAuth(s.username, pw)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			var uerr *url.Error
			if errors.As(err, &uerr) {
				err = uerr.Err
			}
			return "", fmt.Errorf("GET %s: %v", redact(s.uri), err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("GET %s: %s", redact(s.uri), resp.Status)
		}
		rd = resp.Body
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(h, dst), rd); err != nil {
		return "", fmt.Errorf("イメージ %s を読めない: %w", redact(s.uri), err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// stage は更新を BMC に渡す。送ったあと、その部品の新しい Task が観測できなければ
// エラーにする（もう一度は送らない）。
func (p *Provider) stage(ctx context.Context, w core.WriteContext, c client.Client, comp string, s imageSpec) error {
	u, err := c.GetUpdateService(ctx)
	if client.IsNoUpdateService(err) {
		return errors.New("BMC が UpdateService を持たない")
	}
	if err != nil {
		return notSent("%v", err)
	}
	inv, err := c.GetFirmwareInventory(ctx)
	if err != nil {
		return notSent("%v", err)
	}
	var fc *client.FirmwareComponent
	for i := range inv {
		if inv[i].ID == comp {
			fc = &inv[i]
		}
	}
	switch {
	case fc == nil:
		return fmt.Errorf("FirmwareInventory に部品 %s が無い", comp)
	case !fc.Updateable:
		return fmt.Errorf("部品 %s は更新できない（Updateable が false）", comp)
	case !u.ServiceEnabled:
		return errors.New("UpdateService が止まっている（ServiceEnabled が false）")
	case !u.TaskService:
		return errors.New("BMC が TaskService を持たない（更新の終わりを観測できない）")
	case s.transfer == TransferMultipart && u.MultipartHTTPPushURI == "":
		return fmt.Errorf("transfer: %s だが UpdateService に MultipartHttpPushUri が無い（%s を使う）", TransferMultipart, TransferSimpleUpdate)
	case len(u.ApplyTimes) > 0 && !has(u.ApplyTimes, s.applyTime):
		return fmt.Errorf("apply_time %s を BMC が持たない（選べる値: %s）", s.applyTime, strings.Join(u.ApplyTimes, ", "))
	}
	before, err := c.ListTasks(ctx)
	if err != nil {
		return notSent("%v", err)
	}
	seen := map[string]bool{}
	for _, t := range before {
		seen[t.Path] = true
	}
	req := client.UpdateRequest{Targets: []string{fc.Path}, ApplyTime: s.applyTime}
	var location string
	switch s.transfer {
	case TransferSimpleUpdate:
		req.ImageURI = s.uri
		if s.username != "" {
			req.Username = s.username
			if s.password != nil {
				if req.Password, err = w.Secret(ctx, s.password); err != nil {
					return notSent("image_password を取り出せない: %v", err)
				}
			}
		}
		if location, err = c.SimpleUpdate(ctx, req); err != nil {
			return fmt.Errorf("SimpleUpdate: %w", err)
		}
	case TransferMultipart:
		f, err := os.CreateTemp(p.tempDir, "sakutei-firmware-*")
		if err != nil {
			return notSent("%v", err)
		}
		defer os.Remove(f.Name())
		defer f.Close()
		sum, err := p.fetch(ctx, w, s, f)
		if err != nil {
			return notSent("%v", err)
		}
		if s.sha256 != "" && sum != s.sha256 {
			return fmt.Errorf("イメージ %s の sha256 が %s（want %s）。送らない", redact(s.uri), sum, s.sha256)
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return notSent("%v", err)
		}
		if location, err = c.PushUpdate(ctx, req, imageName(s.uri), f); err != nil {
			return fmt.Errorf("multipart の送信: %w", err)
		}
	}
	if location != "" {
		return nil
	}
	after, err := c.ListTasks(ctx)
	if err != nil {
		return fmt.Errorf("BMC は更新を受けたが、Task の読み直しに失敗した: %w", err)
	}
	for i := range after {
		if _, ok := taskImage(&after[i], fc.Path); ok && !seen[after[i].Path] {
			return nil
		}
	}
	return errors.New("BMC は更新を受けたが、その Task が TaskService に見えない（Payload か Update.TargetDetermined が部品を名指していない）。もう一度は送らない")
}

func imageName(uri string) string {
	if u, err := url.Parse(uri); err == nil {
		if b := path.Base(u.Path); b != "." && b != "/" && b != "" {
			return b
		}
	}
	return "firmware.bin"
}
