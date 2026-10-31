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

package record

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
)

// 段 5 の置き場（§5.1）。runs/ は周（ロックを持つ処理）だけが書く。approvals/・
// temp-policies/・commands/ は CLI と承認ツールが書き（ロックは取らない。1 件 1
// ファイルを一時ファイル＋rename で）、周が読んで監査に残し、片づける。
const (
	runsDir     = "runs"
	approvalDir = "approvals"
	tempDir     = "temp-policies"
	commandDir  = "commands"
)

// 手順の進みの状態。済んだ・取り消した手順は runs/ から消す（履歴は監査）。
const (
	RunRunning = "running"
	RunHalted  = "halted"
)

// Run は手順 1 つ・対象 1 つの進み（§5.1 の runs/<procedure>/<target>.json、
// E-K5・N-K5・E-A2・E-F2）。観測の値は置かない（不変条件 1）。
type Run struct {
	Procedure string    `json:"procedure"`
	Target    string    `json:"target"` // 対象の ID（"<provider>/<種類>/<名前>"）
	RunID     string    `json:"run_id"`
	StartedAt time.Time `json:"started_at"`
	StartedBy string    `json:"started_by,omitempty"` // sakutei start の --as（申請者）
	Step      int       `json:"step"`                 // 今の段（0 から）。段の数と同じなら段は済んだ
	StepName  string    `json:"step_name,omitempty"`
	// StepStartedAt は今の段に入った時刻（wait の for と段の timeout）。
	StepStartedAt time.Time `json:"step_started_at"`
	// Sending は送信中の記録。送る前に書き、送り終えたら消す。再開のときに残って
	// いれば二度送らず止める（不変条件 16、E-A2・N-A2）。
	Sending *Sending `json:"sending,omitempty"`
	State   string   `json:"state"`
	Reason  string   `json:"reason,omitempty"` // 止めた理由
	// Resume は止めた手順を続けるための承認の依頼（送れたか分からない・critical
	// の段で止まった、E-A5・E-K5）。
	Resume string `json:"resume,omitempty"`
	// Policies は段 policy で足した、この手順の間だけ効く Policy（E-Q3・N-D4）。
	Policies []TempPolicy `json:"policies,omitempty"`
}

// Sending は送信中の記録。
type Sending struct {
	Step int       `json:"step"`
	Op   string    `json:"op"` // invoke の操作。critical の apply は "apply"
	At   time.Time `json:"at"`
}

func runPath(dir, procedure, target string) string {
	return filepath.Join(dir, runsDir, url.PathEscape(procedure), url.PathEscape(target)+".json")
}

// ReadRuns は runs/ の全部を読む。Record を作らず、ロックも取らない（plan）。
// ディレクトリが無ければ空。
func ReadRuns(dir string) ([]Run, error) {
	root := filepath.Join(dir, runsDir)
	procs, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Run
	for _, p := range procs {
		if !p.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, p.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") || strings.HasPrefix(f.Name(), ".") {
				continue
			}
			path := filepath.Join(root, p.Name(), f.Name())
			var r Run
			if err := readJSON(path, &r); err != nil {
				return nil, err
			}
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Procedure != out[j].Procedure {
			return out[i].Procedure < out[j].Procedure
		}
		return out[i].Target < out[j].Target
	})
	return out, nil
}

// Runs は手順の進みを読む。
func (r *Record) Runs() ([]Run, error) { return ReadRuns(r.dir) }

// WriteRun は手順の進みを書く。ロックを持つ処理だけが呼ぶ。
func (r *Record) WriteRun(run Run) error {
	path := runPath(r.dir, run.Procedure, run.Target)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeJSON(path, run)
}

// RemoveRun は済んだ・取り消した手順の進みを消す。
func (r *Record) RemoveRun(procedure, target string) error {
	err := os.Remove(runPath(r.dir, procedure, target))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// 承認の依頼の状態。
const (
	RequestPending  = "pending"
	RequestUsed     = "used"     // 承認を使った（発射の前に書く、不変条件 21）
	RequestRejected = "rejected" // 拒否された
	RequestExpired  = "expired"  // 期限が切れた
)

// ApprovalRequest は承認の依頼（approvals/<id>/request.json、E-L2・E-L6・N-L3）。
// 周が書き、CLI と承認ツールは読むだけ。
type ApprovalRequest struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind"`    // change・gate・resume
	Subject   string     `json:"subject"` // 変える Resource、または手順の run と段
	Applicant string     `json:"applicant,omitempty"`
	Rules     []Rule     `json:"rules"`
	CreatedAt time.Time  `json:"created_at"`
	Expires   *time.Time `json:"expires,omitempty"` // 期限（無ければ無期限）
	State     string     `json:"state"`
	// Audited は監査に残した判断（承認者の名前 → 判断）。
	Audited map[string]string `json:"audited,omitempty"`
}

// Rule は依頼に要る承認 1 つ（当たる Policy・gate ごと）。
type Rule struct {
	From       string   `json:"from"` // policy の名前・gate の段
	N          int      `json:"n"`
	Of         []string `json:"of,omitempty"` // 空なら申請者でない誰でも
	Separation bool     `json:"separation,omitempty"`
}

// 判断。
const (
	Approve = "approve"
	Reject  = "reject"
)

// Decision は承認者 1 人の判断（approvals/<id>/by-<名前>.json）。CLI と承認ツール
// が書く。認証はしない（承認ツールが前段の認証の結果から By を書く、§5.4）。
type Decision struct {
	By       string    `json:"by"`
	Decision string    `json:"decision"`
	Comment  string    `json:"comment,omitempty"`
	At       time.Time `json:"at"`
}

// Approval は依頼と判断の組。
type Approval struct {
	Request   ApprovalRequest
	Decisions []Decision // 名前順
}

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]*$`)

// ValidName は依頼の ID・承認者の名前・一時的な Policy の名前に使える文字か
// （ファイル名にする）。
func ValidName(s string) bool { return safeName.MatchString(s) && len(s) <= 200 }

// ReadApprovals は approvals/ の全部を読む（ID → 依頼と判断）。依頼の無い
// ディレクトリ（書きかけ）は飛ばす。Record を作らず、ロックも取らない。
func ReadApprovals(dir string) (map[string]Approval, error) {
	root := filepath.Join(dir, approvalDir)
	ents, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Approval{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]Approval{}
	for _, e := range ents {
		if !e.IsDir() || !ValidName(e.Name()) {
			continue
		}
		a, ok, err := readApproval(root, e.Name())
		if err != nil {
			return nil, err
		}
		if ok {
			out[e.Name()] = a
		}
	}
	return out, nil
}

// ReadApproval は依頼 1 つを読む。無ければ false。
func ReadApproval(dir, id string) (Approval, bool, error) {
	if !ValidName(id) {
		return Approval{}, false, fmt.Errorf("依頼の ID %q に使えない文字がある", id)
	}
	return readApproval(filepath.Join(dir, approvalDir), id)
}

func readApproval(root, id string) (Approval, bool, error) {
	var a Approval
	err := readJSON(filepath.Join(root, id, "request.json"), &a.Request)
	if errors.Is(err, os.ErrNotExist) {
		return a, false, nil
	}
	if err != nil {
		return a, false, err
	}
	files, err := os.ReadDir(filepath.Join(root, id))
	if err != nil {
		return a, false, err
	}
	for _, f := range files {
		n := f.Name()
		if !strings.HasPrefix(n, "by-") || !strings.HasSuffix(n, ".json") {
			continue
		}
		var d Decision
		if err := readJSON(filepath.Join(root, id, n), &d); err != nil {
			return a, false, err
		}
		// ファイル名と中身の名前が違えば数えない（書き間違い・すり替え）。
		if "by-"+d.By+".json" != n {
			continue
		}
		a.Decisions = append(a.Decisions, d)
	}
	sort.Slice(a.Decisions, func(i, j int) bool { return a.Decisions[i].By < a.Decisions[j].By })
	return a, true, nil
}

// WriteRequest は依頼を書く。fresh なら前の判断を消してから書く（期限切れ・
// 使った依頼を新しく頼み直す）。ロックを持つ処理だけが呼ぶ。
func (r *Record) WriteRequest(req ApprovalRequest, fresh bool) error {
	if !ValidName(req.ID) {
		return fmt.Errorf("依頼の ID %q に使えない文字がある", req.ID)
	}
	d := filepath.Join(r.dir, approvalDir, req.ID)
	if fresh {
		files, err := os.ReadDir(d)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for _, f := range files {
			if strings.HasPrefix(f.Name(), "by-") {
				if err := os.Remove(filepath.Join(d, f.Name())); err != nil {
					return err
				}
			}
		}
	}
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	return writeJSON(filepath.Join(d, "request.json"), req)
}

// WriteDecision は承認者 1 人の判断を書く（CLI・承認ツール）。
func WriteDecision(dir, id string, d Decision) error {
	if !ValidName(id) {
		return fmt.Errorf("依頼の ID %q に使えない文字がある", id)
	}
	if !ValidName(d.By) {
		return fmt.Errorf("承認者の名前 %q に使えない文字がある", d.By)
	}
	return writeJSON(filepath.Join(dir, approvalDir, id, "by-"+d.By+".json"), d)
}

// TempPolicy は一時的な Policy（temp-policies/<name>.json、§3.6、E-J4・N-Q1・
// N-Q2）。理由と期限が必須。CLI が書き、周が監査に残して期限で消す。
type TempPolicy struct {
	Name   string        `json:"name"`
	Select core.Selector `json:"select"`
	Mode   string        `json:"mode"` // report だけ（締める向きにしか効かない、不変条件 19）
	Reason string        `json:"reason"`
	By     string        `json:"by"`
	At     time.Time     `json:"at"`
	Until  time.Time     `json:"until"`
	// Ended は unfreeze で終えた（<name>.json.end から読む。周が監査に残してから消す）。
	Ended *Ended `json:"-"`
	// Audited は周が始まりを監査に残した。
	Audited bool `json:"audited,omitempty"`
}

// Ended は一時的な Policy を終えた人と理由。
type Ended struct {
	By     string    `json:"by"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// Policy は一時的な Policy を core.Policy にする（report の Policy）。
func (t TempPolicy) Policy() core.Policy {
	return core.Policy{Name: t.Name, Select: t.Select, Mode: core.ModeReport, Until: t.Until, Reason: t.Reason, By: t.By}
}

// Validate は理由・期限・名前・mode を確かめる（不変条件 22）。
func (t TempPolicy) Validate() error {
	switch {
	case !ValidName(t.Name):
		return fmt.Errorf("名前 %q に使えない文字がある", t.Name)
	case strings.TrimSpace(t.Reason) == "":
		return errors.New("理由（reason）を書く")
	case t.Until.IsZero():
		return errors.New("期限（until）を書く")
	case t.By == "":
		return errors.New("誰が（by）を書く")
	case t.Mode != "report":
		return fmt.Errorf("mode は report だけ（%q）", t.Mode)
	}
	return nil
}

func tempPath(dir, name string) string { return filepath.Join(dir, tempDir, name+".json") }

// ReadTempPolicies は temp-policies/ の全部を名前順で読む。読めないファイルが
// あれば誤り（周は全体を止める側に寄せる、不変条件 22）。
func ReadTempPolicies(dir string) ([]TempPolicy, error) {
	root := filepath.Join(dir, tempDir)
	ents, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []TempPolicy
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || strings.HasPrefix(n, ".") || !strings.HasSuffix(n, ".json") {
			continue
		}
		t, _, err := ReadTempPolicy(dir, strings.TrimSuffix(n, ".json"))
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// ReadTempPolicy は 1 つ読む。無ければ false。
func ReadTempPolicy(dir, name string) (TempPolicy, bool, error) {
	if !ValidName(name) {
		return TempPolicy{}, false, fmt.Errorf("名前 %q に使えない文字がある", name)
	}
	var t TempPolicy
	path := tempPath(dir, name)
	err := readJSON(path, &t)
	if errors.Is(err, os.ErrNotExist) {
		return t, false, nil
	}
	if err != nil {
		return t, false, err
	}
	if t.Name != name {
		return t, false, fmt.Errorf("%s: 名前 %q がファイル名と違う", path, t.Name)
	}
	if err := t.Validate(); err != nil {
		return t, false, fmt.Errorf("%s: %w", path, err)
	}
	// 終えた印は別のファイル（CLI と周が同じファイルを書き合わない）。
	var e Ended
	switch err := readJSON(path+".end", &e); {
	case err == nil:
		t.Ended = &e
	case !errors.Is(err, os.ErrNotExist):
		return t, false, err
	}
	return t, true, nil
}

// EndTempPolicy は一時的な Policy を終えた印を書く（sakutei unfreeze）。周が監査に
// 残してから消す。
func EndTempPolicy(dir, name string, e Ended) error {
	if !ValidName(name) {
		return fmt.Errorf("名前 %q に使えない文字がある", name)
	}
	if strings.TrimSpace(e.Reason) == "" {
		return errors.New("理由（reason）を書く")
	}
	return writeJSON(tempPath(dir, name)+".end", e)
}

// WriteTempPolicy は一時的な Policy を書く（CLI、周が監査の印を付ける）。
func WriteTempPolicy(dir string, t TempPolicy) error {
	if err := t.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, tempDir), 0o755); err != nil {
		return err
	}
	return writeJSON(tempPath(dir, t.Name), t)
}

// RemoveTempPolicy は期限が切れた・終えた一時的な Policy を消す（周だけ）。
func (r *Record) RemoveTempPolicy(name string) error {
	for _, p := range []string{tempPath(r.dir, name), tempPath(r.dir, name) + ".end"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// MarkTempPolicyAudited は始まりを監査に残した印を付ける（周だけ）。
func (r *Record) MarkTempPolicyAudited(t TempPolicy) error {
	t.Audited, t.Ended = true, nil
	return writeJSON(tempPath(r.dir, t.Name), t)
}

// Command は CLI から周への頼み（commands/<名前>.json）：手順を起こす（start）・
// 取り消す（cancel）。周が監査に残してから消す。
type Command struct {
	Kind      string    `json:"kind"` // start・cancel
	Procedure string    `json:"procedure,omitempty"`
	Target    string    `json:"target,omitempty"` // start の --target（名前）
	RunID     string    `json:"run_id,omitempty"` // cancel
	By        string    `json:"by"`
	Reason    string    `json:"reason,omitempty"`
	At        time.Time `json:"at"`
	File      string    `json:"-"` // 読んだファイルの名前
}

// WriteCommand は頼みを書く。名前は時刻と中身から作る。
func WriteCommand(dir string, c Command) (string, error) {
	if err := os.MkdirAll(filepath.Join(dir, commandDir), 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Join(dir, commandDir), c.At.UTC().Format("20060102T150405.000000000")+"-"+c.Kind+"-*.json")
	if err != nil {
		return "", err
	}
	name := f.Name()
	f.Close()
	if err := writeJSON(name, c); err != nil {
		os.Remove(name)
		return "", err
	}
	return filepath.Base(name), nil
}

// Commands は頼みを名前順（書いた順）に読む。
func (r *Record) Commands() ([]Command, error) {
	root := filepath.Join(r.dir, commandDir)
	ents, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Command
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || strings.HasPrefix(n, ".") || !strings.HasSuffix(n, ".json") {
			continue
		}
		var c Command
		if err := readJSON(filepath.Join(root, n), &c); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		c.File = n
		out = append(out, c)
	}
	return out, nil
}

// RemoveCommand は片づけた頼みを消す。
func (r *Record) RemoveCommand(file string) error {
	err := os.Remove(filepath.Join(r.dir, commandDir, filepath.Base(file)))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Requests は CLI・承認ツールが書く置き場（常駐は書き込みがあれば起きる、§2.2）。
func Requests(dir string) []string {
	return []string{filepath.Join(dir, approvalDir), filepath.Join(dir, tempDir), filepath.Join(dir, commandDir)}
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, append(data, '\n'))
}
