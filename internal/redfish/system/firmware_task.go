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

package system

// firmware_task.go は FW の更新の多相 Task（§6.2 多相型、#51・#52、§29.1
// 「Redfish の FW の更新」、Backlog #148）。FirmwareProvider が
// task.PhasedRunner として相を宣言し、相ごとに現実を観測して進める。
//
//	verifying   承認ゲート（§6.4）の後、sha256 を書いていればイメージを取りに
//	            行って確かめる
//	staging     SimpleUpdate を送る（transfer: multipart ならイメージを
//	            MultipartHttpPushUri に送る。そのときも送る中身を確かめる）
//	applying    BMC の Task の完了を待つ（危険区間、#52: この中で落ちた Task は
//	            人手ゲート）
//	rebooting   reboot を宣言していればホストを再起動し、版が宣言どおりになるのを
//	            待つ（reboot: none なら待つだけ）。再起動を送る RunStep は危険区間
//	            （この中で落ちた Task は人手ゲート）。RunStep が返ったことは S2 に
//	            印（task.Record.Awaiting）として置くので、待ちの間に落ちても
//	            引き継いだデーモンは待つだけで、二度目の Reset を送らない
//	confirming  版が宣言どおりかを確かめる
//
// 各相の入口で「もう済んでいるか」を観測で問う（S2 に置くのは相の名前だけ）:
//
//   - 版が宣言どおりなら、どの相も済んでいる（Task は Settle で閉じる）。
//   - verifying: 版が宣言どおりのときだけ済んでいる（下の「残りかす」）。
//   - staging: この更新の BMC の Task が、失敗で終わっていないものとして
//     TaskService にあれば済んでいる（もう送った。二重に流さない）。
//   - applying: その Task が Completed（apply_time: OnReset なら Pending も＝再起動を
//     待っている）なら済んでいる。
//   - rebooting・confirming: 版が宣言どおりのときだけ済んでいる。
//   - BMC に届かないのは StepUnknown（リブート中。absent ではない）。
//
// 「この更新の BMC の Task」は、TaskService の Task のうち Payload の TargetUri が
// SimpleUpdate の target（multipart なら MultipartHttpPushUri）で、Payload の
// JsonBody の Targets にこの部品があり、SimpleUpdate なら ImageURI が宣言と同じ
// もの。いくつもあれば StartTime の新しいもの。Task の URI は持たない（観測 state
// レス）。Payload を返さない BMC（qemu-bmc 0.11.0、#168）では、DMTF の Update の
// 登録簿のメッセージ TargetDetermined（MessageArgs が「部品の URI、イメージ」）で
// 見分ける: 部品が同じで、イメージが SimpleUpdate なら image_uri、multipart なら
// 送ったファイルの名前（image_uri の最後の要素）のもの。どちらも無い BMC では、
// 送ったことを観測できないので staging の後で失敗（halt）する（二重に送らない側）。
//
// 前の更新の残りかす（同じイメージで前に済んだ Task が TaskService に残ったまま、
// 人が版を戻した、など）を「もう送った」と取らないために:
//
//   - verifying の入口は、版が宣言どおりのときだけ済んでいる（BMC の Task では
//     済みにしない）。承認の要る相なので、残りかすで承認を飛ばさない。
//   - BMC の Task の StartTime を時刻として読み、この Task の起動の時刻
//     （task.Request.StartedAt）から FirmwareOptions.ClockSkew（既定 5 分、
//     BMC とこのホストの時計のずれの許し）を引いた時刻より前に始まったものは
//     数えない。StartTime を読めない Task は数える（自分の送った Task を見落として
//     二重に送る側に倒さない）。
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
	"time"

	"github.com/tjst-t/sakutei/v2/internal/redfish"
	"github.com/tjst-t/sakutei/v2/internal/task"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

var _ task.PhasedRunner = (*FirmwareProvider)(nil)

// TaskSteps implements task.PhasedRunner.
func (p *FirmwareProvider) TaskSteps(_ context.Context, req task.Request) ([]task.StepSpec, error) {
	s, err := parseFirmware(req.Resource.Local, req.Desired)
	if err != nil {
		return nil, err
	}
	return []task.StepSpec{
		{Name: task.StepVerifying, Action: true, Approval: true},
		{Name: task.StepStaging, Action: true},
		{Name: task.StepApplying, Action: true, Critical: true, Timeout: s.updateTimeout},
		// 再起動を送る相は危険区間（Reset の最中に落ちたら人手ゲート）。送った後の
		// 待ちは S2 の Awaiting の印で引き継ぎ、二度目の Reset を送らない。
		{Name: task.StepRebooting, Action: true, Await: true, Critical: s.reboot != RebootNone, Timeout: s.rebootTimeout},
		{Name: task.StepConfirming, Timeout: s.rebootTimeout},
	}, nil
}

// RunTask implements task.Runner: FW の更新は多相 Task だけ（TaskSteps が相を返す）。
func (p *FirmwareProvider) RunTask(context.Context, task.Request) error {
	return errors.New(FirmwareName + ": a firmware update runs only as a phased Task")
}

// RollbackTask implements task.Runner: FW は自動では戻さない
// （ValidateUpdatePolicy が rollback_on_fail を断る）。
func (p *FirmwareProvider) RollbackTask(context.Context, task.Request) (string, error) {
	return "", errors.New(FirmwareName + ": a firmware update is not rolled back automatically")
}

// fwReality は相の入口で観測したもの。
type fwReality struct {
	version string
	task    *redfish.Task // この更新の BMC の Task（無ければ nil）
}

// observe は部品の版と、この更新の BMC の Task を観測する。
func (p *FirmwareProvider) observe(ctx context.Context, s fwSpec) (fwReality, error) {
	c, ok := p.clients[s.node]
	if !ok {
		return fwReality{}, fmt.Errorf("no BMC is configured for node %q", s.node)
	}
	u, err := c.GetUpdateService(ctx)
	if err != nil {
		return fwReality{}, err
	}
	inv, err := c.GetFirmwareInventory(ctx)
	if err != nil {
		return fwReality{}, err
	}
	var r fwReality
	compPath := ""
	for _, fc := range inv {
		if fc.ID == s.component {
			r.version, compPath = fc.Version, fc.Path
		}
	}
	if compPath == "" {
		return fwReality{}, fmt.Errorf("component %q is not in the FirmwareInventory", s.component)
	}
	tasks, err := c.ListTasks(ctx)
	if err != nil {
		return fwReality{}, err
	}
	r.task = matchTask(tasks, s, u, compPath, p.since(s))
	return r, nil
}

// since は数える BMC の Task の StartTime の下限（起動の時刻 − ClockSkew）。
// 起動の時刻が分からなければゼロ（下限なし）。
func (p *FirmwareProvider) since(s fwSpec) time.Time {
	if s.startedAt.IsZero() {
		return time.Time{}
	}
	return s.startedAt.Add(-p.opts.ClockSkew)
}

// matchTask は tasks からこの更新の BMC の Task を探す（パッケージの説明）。
// since より前に始まった Task は数えない。
func matchTask(tasks []redfish.Task, s fwSpec, u redfish.UpdateService, compPath string, since time.Time) *redfish.Task {
	target := u.SimpleUpdateTarget
	if target == "" {
		target = strings.TrimSuffix(u.Path, "/") + "/Actions/UpdateService.SimpleUpdate"
	}
	if s.transfer == TransferMultipart {
		target = u.MultipartHTTPPushURI
	}
	var best *redfish.Task
	var bestAt time.Time
	for i := range tasks {
		t := &tasks[i]
		at, parsed := taskStart(t)
		if parsed && !since.IsZero() && at.Before(since) {
			continue // この起動より前の更新の Task
		}
		if t.Body == nil {
			if !determined(t, s, compPath) {
				continue
			}
		} else {
			if !samePath(t.TargetURI, target) || !bodyTargets(t.Body, compPath) {
				continue
			}
			if s.transfer == TransferSimpleUpdate {
				if img, _ := t.Body["ImageURI"].(string); img != s.imageURI {
					continue
				}
			}
		}
		if best == nil || !at.Before(bestAt) {
			best, bestAt = t, at
		}
	}
	return best
}

// taskStart は Task の StartTime を時刻として読む。読めなければ false。
func taskStart(t *redfish.Task) (time.Time, bool) {
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(t.StartTime))
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

func samePath(a, b string) bool {
	norm := func(s string) string {
		if u, err := url.Parse(s); err == nil && u.Path != "" {
			s = u.Path
		}
		return strings.TrimSuffix(s, "/")
	}
	return b != "" && norm(a) == norm(b)
}

// determined は Payload の無い Task が、Update の登録簿の TargetDetermined で
// この更新（部品 compPath・イメージ）を名指しているか。
func determined(t *redfish.Task, s fwSpec, compPath string) bool {
	image := s.imageURI
	if s.transfer == TransferMultipart {
		image = imageName(s.imageURI)
	}
	for _, m := range t.MessageDetails {
		if redfish.MessageIs(m.ID, redfish.MsgTargetDetermined) && len(m.Args) >= 2 &&
			samePath(m.Args[0], compPath) && m.Args[1] == image {
			return true
		}
	}
	return false
}

func bodyTargets(body map[string]any, compPath string) bool {
	ts, _ := body["Targets"].([]any)
	for _, t := range ts {
		if s, ok := t.(string); ok && samePath(s, compPath) {
			return true
		}
	}
	return false
}

// CheckStep implements task.PhasedRunner.
func (p *FirmwareProvider) CheckStep(ctx context.Context, req task.Request, step string) (task.StepState, error) {
	s, err := parseFirmware(req.Resource.Local, req.Desired)
	if err != nil {
		return task.StepUnknown, err
	}
	s.startedAt = req.StartedAt
	r, err := p.observe(ctx, s)
	if err != nil {
		// 届かない（BMC のリブート中など）。absent ではない。
		return task.StepUnknown, err
	}
	if VersionsMatch(r.version, s.version) {
		return task.StepDone, nil
	}
	done := false
	switch step {
	case task.StepVerifying:
		// 版が合っていないかぎり済んでいない（BMC の Task で承認を飛ばさない）。
	case task.StepStaging:
		done = r.task != nil && !redfish.TaskFailed(r.task.State)
	case task.StepApplying:
		done = r.task != nil && taskApplied(r.task, s)
	case task.StepRebooting, task.StepConfirming:
	default:
		return task.StepUnknown, fmt.Errorf("%s: unknown step %q", FirmwareName, step)
	}
	if done {
		return task.StepDone, nil
	}
	return task.StepPending, nil
}

// taskApplied は BMC の Task が、更新を書き終えた（OnReset なら再起動を待つ）か。
func taskApplied(t *redfish.Task, s fwSpec) bool {
	return t.State == redfish.TaskCompleted || (s.applyTime == redfish.ApplyOnReset && t.State == redfish.TaskPending)
}

// RunStep implements task.PhasedRunner.
func (p *FirmwareProvider) RunStep(ctx context.Context, req task.Request, step string) error {
	s, err := parseFirmware(req.Resource.Local, req.Desired)
	if err != nil {
		return err
	}
	s.startedAt = req.StartedAt
	fail := func(format string, a ...any) error {
		return fmt.Errorf("%s %q: %s: "+format, append([]any{FirmwareName, req.Resource.Local, step}, a...)...)
	}
	switch step {
	case task.StepVerifying:
		if s.sha256 == "" {
			return nil
		}
		sum, _, err := p.fetchImage(ctx, s, io.Discard)
		if err != nil {
			return fail("%v", err)
		}
		if sum != s.sha256 {
			return fail("the image %s has sha256 %s, want %s", s.imageURI, sum, s.sha256)
		}
		return nil
	case task.StepStaging:
		if err := p.stage(ctx, s); err != nil {
			return fail("%v", err)
		}
		return nil
	case task.StepApplying:
		if err := p.waitApplied(ctx, s); err != nil {
			return fail("%v", err)
		}
		return nil
	case task.StepRebooting:
		if err := p.reboot(ctx, s); err != nil {
			return fail("%v", err)
		}
		return nil
	}
	return fail("the step has no action")
}

// stage は更新を BMC に渡す（SimpleUpdate か multipart の送信）。渡したあと、
// この更新の BMC の Task が観測できなければエラー（もう一度は送らない）。
func (p *FirmwareProvider) stage(ctx context.Context, s fwSpec) error {
	c := p.clients[s.node]
	inv, err := c.GetFirmwareInventory(ctx)
	if err != nil {
		return err
	}
	compPath := ""
	for _, fc := range inv {
		if fc.ID == s.component {
			compPath = fc.Path
		}
	}
	if compPath == "" {
		return fmt.Errorf("component %q is not in the FirmwareInventory", s.component)
	}
	req := redfish.UpdateRequest{Targets: []string{compPath}, ApplyTime: s.applyTime}
	switch s.transfer {
	case TransferSimpleUpdate:
		req.ImageURI = s.imageURI
		if s.password != "" {
			pw, err := p.password(ctx, s)
			if err != nil {
				return err
			}
			defer pw.Zero()
			req.Username, req.Password = s.username, pw.Expose()
		}
		if _, err := c.SimpleUpdate(ctx, req); err != nil {
			return fmt.Errorf("SimpleUpdate: %w", err)
		}
	case TransferMultipart:
		f, err := os.CreateTemp(p.opts.TempDir, "sakutei-firmware-*")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		defer f.Close()
		sum, _, err := p.fetchImage(ctx, s, f)
		if err != nil {
			return err
		}
		if s.sha256 != "" && sum != s.sha256 {
			return fmt.Errorf("the image %s has sha256 %s, want %s; not sent", s.imageURI, sum, s.sha256)
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if _, err := c.PushUpdate(ctx, req, imageName(s.imageURI), f); err != nil {
			return fmt.Errorf("multipart push: %w", err)
		}
	}
	r, err := p.observe(ctx, s)
	if err != nil {
		return fmt.Errorf("the BMC accepted the update, but reading it back failed: %w", err)
	}
	if r.task == nil && !VersionsMatch(r.version, s.version) {
		return errors.New("the BMC accepted the update, but no task for it is observed in the TaskService (its Payload, or an Update.TargetDetermined message, must name the target and image); not sending it again")
	}
	return nil
}

func imageName(uri string) string {
	if u, err := url.Parse(uri); err == nil {
		if b := path.Base(u.Path); b != "." && b != "/" && b != "" {
			return b
		}
	}
	return "firmware.bin"
}

// waitApplied は BMC の Task が書き終えるのを update_timeout まで待つ。BMC に
// 届かない間（BMC 自身の更新で再起動している）と、Task が見えない間（再起動で
// Task が消えた）は待ち続ける。
func (p *FirmwareProvider) waitApplied(ctx context.Context, s fwSpec) error {
	deadline := time.Now().Add(s.updateTimeout)
	var last string
	for {
		r, err := p.observe(ctx, s)
		switch {
		case err != nil:
			last = "the BMC is not observed: " + err.Error()
		case VersionsMatch(r.version, s.version):
			return nil
		case r.task == nil:
			last = "no task for the update is observed"
		case taskApplied(r.task, s):
			return nil
		case redfish.TaskFailed(r.task.State):
			msg := r.task.Reason()
			if msg == "" {
				msg = "no message"
			}
			return fmt.Errorf("the BMC task %s ended %s (%s)", r.task.Path, r.task.State, msg)
		default:
			last = fmt.Sprintf("the BMC task %s is %s", r.task.Path, r.task.State)
			if r.task.PercentComplete >= 0 {
				last += fmt.Sprintf(" (%d%%)", r.task.PercentComplete)
			}
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("the update did not finish within %s; last: %s", s.updateTimeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.opts.PollInterval):
		}
	}
}

// reboot はホストを再起動する（reboot: none なら何もしない）。電源が On なら
// GracefulRestart / ForceRestart、Off なら On（#48 の電源の決まりと同じ）。
func (p *FirmwareProvider) reboot(ctx context.Context, s fwSpec) error {
	if s.reboot == RebootNone {
		return nil
	}
	c := p.clients[s.node]
	sys, err := c.GetSystem(ctx)
	if err != nil {
		return err
	}
	t := redfish.ResetGracefulRestart
	if s.reboot == RebootForce {
		t = redfish.ResetForceRestart
	}
	switch sys.PowerState {
	case redfish.PowerOff:
		t = redfish.ResetOn
	case redfish.PowerOn:
	default:
		return fmt.Errorf("the power is %s (another power operation is in progress); not rebooting", sys.PowerState)
	}
	if len(sys.ResetTypes) > 0 && !contains(sys.ResetTypes, t) {
		return fmt.Errorf("the BMC does not offer ResetType %s (allowable: %s)", t, strings.Join(sys.ResetTypes, ", "))
	}
	if err := c.Reset(ctx, t); err != nil {
		return fmt.Errorf("Reset %s: %w", t, err)
	}
	return nil
}

// password は image_password を解決する（使ったら Zero する）。
func (p *FirmwareProvider) password(ctx context.Context, s fwSpec) (*core.SecretValue, error) {
	ref, err := core.ParseSecretRef(s.password)
	if err != nil {
		return nil, errors.New("image_password is not a SecretRef")
	}
	if p.opts.Secrets == nil {
		return nil, fmt.Errorf("image_password %s: no secret store is configured (secret_store of the BMC list)", ref)
	}
	v, err := ref.Resolve(ctx, p.opts.Secrets)
	if err != nil {
		return nil, fmt.Errorf("image_password %s: cannot resolve", ref)
	}
	return &v, nil
}

// fetchImage は image_uri を取りに行って w に書き、SHA-256（小文字の 16 進）と
// 長さを返す。エラーは資格を出さない。
func (p *FirmwareProvider) fetchImage(ctx context.Context, s fwSpec, w io.Writer) (string, int64, error) {
	var rd io.Reader
	u, err := url.Parse(s.imageURI)
	if err != nil {
		return "", 0, fmt.Errorf("image_uri: %w", err)
	}
	if u.Scheme == "file" {
		f, err := os.Open(u.Path)
		if err != nil {
			return "", 0, fmt.Errorf("open the image: %w", err)
		}
		defer f.Close()
		rd = f
	} else {
		hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, s.imageURI, nil)
		if err != nil {
			return "", 0, fmt.Errorf("GET %s: %w", s.imageURI, err)
		}
		if s.password != "" {
			pw, err := p.password(ctx, s)
			if err != nil {
				return "", 0, err
			}
			hreq.SetBasicAuth(s.username, pw.Expose())
			pw.Zero()
		} else if s.username != "" {
			hreq.SetBasicAuth(s.username, "")
		}
		resp, err := p.opts.ImageClient.Do(hreq)
		if err != nil {
			var uerr *url.Error
			if errors.As(err, &uerr) {
				err = uerr.Err
			}
			return "", 0, fmt.Errorf("GET %s: %v", s.imageURI, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", 0, fmt.Errorf("GET %s: status %d", s.imageURI, resp.StatusCode)
		}
		rd = resp.Body
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(h, w), rd)
	if err != nil {
		return "", n, fmt.Errorf("read the image %s: %w", s.imageURI, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
