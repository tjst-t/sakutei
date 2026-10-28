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

package reconcile

// Apply の効きの確かめ（§11.1 Tier2 settle・Tier3 自動隔離、§11.3、§15.3、
// Backlog #166）。
//
// provider の Apply が成功を返しても、現実が変わったとは限らない（BMC が電源の
// 命令に 204 を返して電源を変えない、など）。そこで Apply が成功を返したリソース
// は、その場で読み直して Diff が消えたかを確かめる（VerifyApply）。型ごとの
// settle の間は待って読み直す。
//
//   - Diff が消えた: 効いた。そのリソースの記録を消す。
//   - Diff が Apply したときと同じまま（Availability Ok）: 効かなかった。同じ
//     Diff での回数を 1 つ上げる（ApplyChecker.Ineffective）。上限（回数か、最初
//     の効かない Apply からの時間）に達したら、そのリソースへの Apply を止める
//     （stopped。§9.7 の oscillating。そのリソースだけ alert-only）。
//   - Diff が変わった（縮んだ）: 進んだので記録を消す（次の周は残りを Apply）。
//   - 観測できない（settle の終わりまで）・Converging（相手が作業中と言う。
//     Tier1 なので待たずにそこで終える）: 確かめられなかった（unknown ≠
//     ineffective）。数えない。
//
// 止めたリソースは、Diff の中身（desired か観測の値）が変わったとき（Gate）、
// Diff が消えたとき（Settled）、人が解いたとき（Release、"sakutei apply-check
// release"）に解く。
//
// 記録は S3（メモリ）。失っても数え直すだけ（安全側）。一回性の reconcile と、
// 望むなら常駐も、作業の記録として状態のファイル（--apply-check-state）に書く。
// 正しい状態ではない（収束の判断は毎回の観測による）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/cause"
	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/eventlog"
	"github.com/tjst-t/sakutei/v2/internal/filelock"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// DefaultMaxIneffectiveApplies は、同じ Diff で効かない Apply を何回まで許すかの
// 既定値（§11.1 Tier3 の M）。
const DefaultMaxIneffectiveApplies = 3

// ApplyCheckLimits はリソース 1 つの settle と止める上限。
type ApplyCheckLimits struct {
	// Settle は Apply が成功を返してから、現実が古いままでも待つ時間。
	Settle time.Duration
	// MaxApplies は同じ Diff で効かない Apply を何回で止めるか（>= 1）。
	MaxApplies int
	// MaxDuration は最初の効かない Apply からこの時間が経ったら止める（0: 時間の
	// 上限なし）。
	MaxDuration time.Duration
}

// ApplyCheckFlags は CLI の旗で明示した値（nil は旗が無い）。
type ApplyCheckFlags struct {
	Settle      *time.Duration
	MaxApplies  *int
	MaxDuration *time.Duration
}

// ResolveApplyCheck は res の settle と上限を決める。
//
//  1. 既定: settle は provider の Capabilities.Settle、回数は
//     DefaultMaxIneffectiveApplies、時間の上限なし。
//  2. intent: 型の `apply_check:`（top-level、provider ごと）、その上にリソースの
//     `apply_check:`（より細かい宣言が勝つ）。
//  3. 旗（--settle・--max-ineffective-applies・--max-ineffective-duration）:
//     intent に宣言が無い値は旗が決める。intent にも宣言があって食い違うときは
//     厳しい方（most-restrictive-wins、§14.3）: settle は長い方、回数は少ない方、
//     時間は短い方（0＝上限なしより、上限のある方）。旗で intent を緩めることは
//     できない（長く効く値はレビューを通る intent が正）。
func ResolveApplyCheck(res diff.DesiredResource, types map[string]diff.ApplyCheck, flags ApplyCheckFlags, providerSettle time.Duration) ApplyCheckLimits {
	lim := ApplyCheckLimits{Settle: providerSettle, MaxApplies: DefaultMaxIneffectiveApplies}
	var settleSet, appliesSet, durationSet bool
	apply := func(ac *diff.ApplyCheck) {
		if ac == nil {
			return
		}
		if ac.Settle != nil {
			lim.Settle, settleSet = *ac.Settle, true
		}
		if ac.MaxIneffectiveApplies != nil {
			lim.MaxApplies, appliesSet = *ac.MaxIneffectiveApplies, true
		}
		if ac.MaxIneffectiveDuration != nil {
			lim.MaxDuration, durationSet = *ac.MaxIneffectiveDuration, true
		}
	}
	if ac, ok := types[res.ID.Provider]; ok {
		apply(&ac)
	}
	apply(res.ApplyCheck)
	if f := flags.Settle; f != nil {
		if !settleSet || *f > lim.Settle {
			lim.Settle = *f
		}
	}
	if f := flags.MaxApplies; f != nil {
		if !appliesSet || *f < lim.MaxApplies {
			lim.MaxApplies = *f
		}
	}
	if f := flags.MaxDuration; f != nil {
		switch {
		case !durationSet, lim.MaxDuration == 0:
			lim.MaxDuration = *f
		case *f > 0 && *f < lim.MaxDuration:
			lim.MaxDuration = *f
		}
	}
	if lim.MaxApplies < 1 {
		lim.MaxApplies = 1
	}
	return lim
}

// ApplyFingerprint は res に ops を Apply するときの Diff の中身の指紋。desired の
// 値と、各 op の種類・破壊性・欄ごとの現実の値と宣言の値で決まる（cause tag と
// 理由の文は含めない：同じ Diff でも周ごとに変わりうる）。
func ApplyFingerprint(res diff.DesiredResource, ops []core.Op) string {
	h := sha256.New()
	keys := make([]string, 0, len(res.Fields))
	for k := range res.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(h, "absent=%t\x00", res.Absent)
	for _, k := range keys {
		fmt.Fprintf(h, "d:%s=%s\x00", k, res.Fields[k])
	}
	for _, op := range ops {
		fmt.Fprintf(h, "op:%d:%t\x00", op.Kind, op.Destructive)
		fields := append([]core.FieldDiff(nil), op.Fields...)
		sort.Slice(fields, func(i, j int) bool { return fields[i].Field < fields[j].Field })
		for _, f := range fields {
			fmt.Fprintf(h, "f:%s:%s=>%s\x00", f.Field, f.From, f.To)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// DiffText は ops を 1 行で言う（`power: "Off" → "On"` など）。
func DiffText(ops []core.Op) string {
	var parts []string
	for _, op := range ops {
		switch {
		case op.Kind == core.OpKindCreate:
			parts = append(parts, "CREATE (absent in reality)")
		case op.Kind == core.OpKindDelete:
			parts = append(parts, "DELETE (present in reality)")
		default:
			for _, f := range op.Fields {
				parts = append(parts, fmt.Sprintf("%s: %q → %q", f.Field, f.From, f.To))
			}
		}
	}
	if len(parts) == 0 {
		return "(no field diff)"
	}
	return strings.Join(parts, ", ")
}

// VerifyOutcome は Apply のあとの確かめの結果。
type VerifyOutcome int

const (
	// VerifyConverged: Diff が消えた（効いた）。
	VerifyConverged VerifyOutcome = iota
	// VerifyIneffective: settle を過ぎても Diff が残った（Availability Ok）。
	VerifyIneffective
	// VerifyUnknown: settle の終わりに観測できない・Converging だった。
	VerifyUnknown
)

func (o VerifyOutcome) String() string {
	switch o {
	case VerifyConverged:
		return "converged"
	case VerifyIneffective:
		return "ineffective"
	}
	return "unknown"
}

// verifyPollInterval は settle の間に読み直す間隔。
func verifyPollInterval(settle time.Duration) time.Duration {
	iv := settle / 10
	switch {
	case iv < 50*time.Millisecond:
		iv = 50 * time.Millisecond
	case iv > 2*time.Second:
		iv = 2 * time.Second
	}
	return iv
}

// VerifyApply は Apply が成功を返した res を読み直し、Diff が消えるか settle が
// 過ぎるまで待つ（§11.1 Tier2）。settle が 0 なら 1 回だけ読む。相手が Converging
// と言えば（Tier1: 作業中と言える相手）そこで VerifyUnknown を返す（Apply は
// 何かを動かした。その先は収束ループの観測が見る）。返す ops は最後に観測した
// Diff（VerifyIneffective のとき）。ctx が終わったら VerifyUnknown。
func VerifyApply(ctx context.Context, p provider.Provider, engine *diff.Engine, res diff.DesiredResource, settle time.Duration) (VerifyOutcome, []core.Op) {
	deadline := time.Now().Add(settle)
	iv := verifyPollInterval(settle)
	for {
		out, ops, final := verifyOnce(ctx, p, engine, res)
		if out == VerifyConverged || final {
			return out, ops
		}
		left := time.Until(deadline)
		if left <= 0 {
			return out, ops
		}
		if left > iv {
			left = iv
		}
		select {
		case <-ctx.Done():
			return VerifyUnknown, nil
		case <-time.After(left):
		}
	}
}

// verifyOnce は res を 1 回読み直して Diff を見る。final は待たずに終えること
// （相手が Converging と言った）。
func verifyOnce(ctx context.Context, p provider.Provider, engine *diff.Engine, res diff.DesiredResource) (out VerifyOutcome, ops []core.Op, final bool) {
	var obs core.Observation
	var err error
	if _, ok := p.(provider.CheckReader); ok {
		obs, err = provider.ReadFor(ctx, p, res.ID.Local, res.Fields)
	} else {
		obs, err = p.Read(ctx, res.ID.Local)
	}
	if err == nil && obs.Availability == core.AvailabilityConverging {
		return VerifyUnknown, nil, true
	}
	if err != nil || obs.Availability != core.AvailabilityOk {
		return VerifyUnknown, nil, false
	}
	plan, err := engine.Plan(obs, res, cause.CauseContext{})
	if err != nil {
		return VerifyUnknown, nil, false
	}
	if len(plan.Ops) == 0 {
		return VerifyConverged, nil, false
	}
	return VerifyIneffective, plan.Ops, false
}

// ApplyRecord はリソース 1 つの、効かない Apply の記録。
type ApplyRecord struct {
	ID core.ID
	// Fingerprint は効かなかった Apply の Diff の指紋（ApplyFingerprint）。
	Fingerprint string
	// Diff は人が読む Diff（DiffText）。
	Diff string
	// Applies は同じ Diff で効かなかった Apply の回数。
	Applies int
	// First・Last は最初と最後の効かない Apply（を確かめた時刻）。
	First, Last time.Time
	// Limits は数えたときの settle と上限。
	Limits ApplyCheckLimits
	// Stopped はそのリソースへの Apply を止めたこと（oscillating）。StoppedAt は
	// 止めた時刻、Reason は止めたわけ（"max_ineffective_applies" /
	// "max_ineffective_duration"）。
	Stopped   bool
	StoppedAt time.Time
	Reason    string
}

// StopReasonText は止めたわけを文で言う。
func (r ApplyRecord) StopReasonText() string {
	switch r.Reason {
	case StopMaxApplies:
		return fmt.Sprintf("%d Apply(s) of the same diff reported success but reality did not change (limit %d)", r.Applies, r.Limits.MaxApplies)
	case StopMaxDuration:
		return fmt.Sprintf("Applies of the same diff have not taken for %s (limit %s; %d Apply(s))",
			r.Last.Sub(r.First).Round(time.Second), r.Limits.MaxDuration, r.Applies)
	}
	return fmt.Sprintf("%d Apply(s) of the same diff did not take", r.Applies)
}

// 止めたわけ（ApplyRecord.Reason）。
const (
	StopMaxApplies  = "max_ineffective_applies"
	StopMaxDuration = "max_ineffective_duration"
)

// ApplyChecker は効かない Apply の記録（S3）。安全に並行に呼べる（常駐では
// Apply の goroutine が Ineffective を呼ぶ）。
type ApplyChecker struct {
	mu   sync.Mutex
	recs map[core.IDKey]*ApplyRecord
	// fresh は前の TakeIneffective のあとに数えた効かない Apply。
	fresh []ApplyRecord
}

// NewApplyChecker は空の ApplyChecker を返す。
func NewApplyChecker() *ApplyChecker {
	return &ApplyChecker{recs: map[core.IDKey]*ApplyRecord{}}
}

// Gate は Apply の前に呼ぶ。fp は今の Diff の指紋。記録の Diff と違えば記録を消す
// （止めていたなら released に返す：Diff の中身が変わったので解く）。同じ Diff で
// 止めているなら stopped が true。
func (c *ApplyChecker) Gate(id core.ID, fp string) (rec ApplyRecord, stopped bool, released *ApplyRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.recs[id.Key()]
	if !ok {
		return ApplyRecord{}, false, nil
	}
	if r.Fingerprint != fp {
		delete(c.recs, id.Key())
		if r.Stopped {
			old := *r
			return ApplyRecord{}, false, &old
		}
		return ApplyRecord{}, false, nil
	}
	return *r, r.Stopped, nil
}

// Settled は id が Diff なしで観測されたときに呼ぶ。記録を消し、止めていたなら
// その記録を返す。
func (c *ApplyChecker) Settled(id core.ID) *ApplyRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.recs[id.Key()]
	if !ok {
		return nil
	}
	delete(c.recs, id.Key())
	if r.Stopped {
		old := *r
		return &old
	}
	return nil
}

// Effective は Apply が効いた（または Diff が進んだ）ときに呼ぶ。記録を消す。
func (c *ApplyChecker) Effective(id core.ID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.recs, id.Key())
}

// Ineffective は fp の Diff の Apply が効かなかったことを数え、記録を返す。記録の
// Diff と違えば 1 から数え直す。上限に達したら止める（Stopped）。
func (c *ApplyChecker) Ineffective(id core.ID, fp, diffText string, lim ApplyCheckLimits, now time.Time) ApplyRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.recs[id.Key()]
	if !ok || r.Fingerprint != fp {
		r = &ApplyRecord{ID: id, Fingerprint: fp, First: now}
		c.recs[id.Key()] = r
	}
	r.Diff, r.Limits, r.Last = diffText, lim, now
	r.Applies++
	if !r.Stopped {
		switch {
		case r.Applies >= lim.MaxApplies:
			r.Stopped, r.StoppedAt, r.Reason = true, now, StopMaxApplies
		case lim.MaxDuration > 0 && now.Sub(r.First) >= lim.MaxDuration:
			r.Stopped, r.StoppedAt, r.Reason = true, now, StopMaxDuration
		}
	}
	c.fresh = append(c.fresh, *r)
	return *r
}

// TakeIneffective は前に呼んでから数えた効かない Apply を返して空にする（常駐の
// 周の要約と metrics）。
func (c *ApplyChecker) TakeIneffective() []ApplyRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.fresh
	c.fresh = nil
	return out
}

// Release は人が解く（"sakutei apply-check release"）。resource は ID の文字列
// （ApplyRecord.ID.String()）か Local ID。消した記録を返す。
func (c *ApplyChecker) Release(resource string) []ApplyRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []ApplyRecord
	for k, r := range c.recs {
		if r.ID.String() == resource || r.ID.Local == resource {
			out = append(out, *r)
			delete(c.recs, k)
		}
	}
	sortRecords(out)
	return out
}

// Retain は keep が false のリソースの記録を消す（desired から外れたもの）。
func (c *ApplyChecker) Retain(keep func(core.ID) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, r := range c.recs {
		if !keep(r.ID) {
			delete(c.recs, k)
		}
	}
}

// IsStopped は id への Apply を止めているかを返す。
func (c *ApplyChecker) IsStopped(id core.ID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.recs[id.Key()]
	return ok && r.Stopped
}

// Records は記録をすべて返す（ID の文字列の順）。
func (c *ApplyChecker) Records() []ApplyRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ApplyRecord, 0, len(c.recs))
	for _, r := range c.recs {
		out = append(out, *r)
	}
	sortRecords(out)
	return out
}

// Stopped は止めているリソースの記録を返す（ID の文字列の順）。
func (c *ApplyChecker) Stopped() []ApplyRecord {
	var out []ApplyRecord
	for _, r := range c.Records() {
		if r.Stopped {
			out = append(out, r)
		}
	}
	return out
}

func sortRecords(rs []ApplyRecord) {
	sort.Slice(rs, func(i, j int) bool { return rs[i].ID.String() < rs[j].ID.String() })
}

// ---- Event ----

// EmitApplyIneffective は Apply が効かなかったこと（stopped なら止めたこと）を
// ApplyIneffective の Event で知らせる。level の Event で、常駐ではリソースに
// 記録がある間（数えている間と止めている間）firing、記録が消えたら（効いた・
// Diff が変わった・現実が宣言どおりになった・人が解いた）resolved（§22.3）。回数が
// 進むたびに firing の更新が出る。止めたものは人手を要するので stuck
// （failure=permanent、severity=critical）。
func EmitApplyIneffective(w io.Writer, prefix string, r ApplyRecord) {
	attrs := map[string]string{
		"ineffective_applies": strconv.Itoa(r.Applies),
		"max_applies":         strconv.Itoa(r.Limits.MaxApplies),
		"settle":              r.Limits.Settle.String(),
		"diff":                r.Fingerprint,
	}
	if r.Limits.MaxDuration > 0 {
		attrs["max_duration"] = r.Limits.MaxDuration.String()
	}
	var msg, text string
	if r.Stopped {
		attrs["stopped"] = "true"
		attrs["reason"] = r.Reason
		attrs[eventlog.AttrFailure] = eventlog.FailurePermanent
		msg = fmt.Sprintf("Apply stopped (oscillating, §11.1 Tier3): %s; diff %s. It is applied again when the diff (desired or reality) changes, when reality converges, or after `sakutei apply-check release`",
			r.StopReasonText(), r.Diff)
		text = fmt.Sprintf("%sApplyIneffective: %s: STOPPED: %s", prefix, r.ID, msg)
	} else {
		msg = fmt.Sprintf("Apply reported success but reality did not change after the settle (%s): diff %s remains (%d of %d before the loop stops applying it)",
			r.Limits.Settle, r.Diff, r.Applies, r.Limits.MaxApplies)
		text = fmt.Sprintf("%sApplyIneffective: %s: %s", prefix, r.ID, msg)
	}
	eventlog.Emit(w, core.Event{
		Kind: core.EventApplyIneffective, Severity: core.SeverityWarning,
		Resource: r.ID, Message: msg, Attrs: attrs, Text: text,
	})
}

// ---- 状態のファイル（--apply-check-state）----

// ApplyCheckStateVersion は状態のファイルの版。
const ApplyCheckStateVersion = 1

// ApplyCheckState は状態のファイルの形。作業の記録で、正しい状態ではない（失っても
// 数え直すだけ）。
type ApplyCheckState struct {
	Version   int                `json:"version"`
	UpdatedAt time.Time          `json:"updated_at"`
	Resources []ApplyRecordJSON  `json:"resources"`
	Releases  []ApplyReleaseJSON `json:"release_requests,omitempty"`
}

// ApplyRecordJSON は ApplyRecord のファイルと --json の形。
type ApplyRecordJSON struct {
	Resource           string            `json:"resource"`
	Provider           string            `json:"provider"`
	Scope              map[string]string `json:"scope"`
	LocalID            string            `json:"local_id"`
	Fingerprint        string            `json:"fingerprint"`
	Diff               string            `json:"diff"`
	IneffectiveApplies int               `json:"ineffective_applies"`
	FirstIneffective   time.Time         `json:"first_ineffective"`
	LastIneffective    time.Time         `json:"last_ineffective"`
	Settle             string            `json:"settle"`
	MaxApplies         int               `json:"max_applies"`
	MaxDuration        string            `json:"max_duration,omitempty"`
	Stopped            bool              `json:"stopped"`
	StoppedAt          *time.Time        `json:"stopped_at,omitempty"`
	Reason             string            `json:"reason,omitempty"`
}

// ApplyReleaseJSON は人が解いた求め（"sakutei apply-check release"）。常駐は次の
// 周の初めに読んで解き、ファイルから消す。
type ApplyReleaseJSON struct {
	Resource    string    `json:"resource"`
	Approver    string    `json:"approver,omitempty"`
	RequestedAt time.Time `json:"requested_at"`
}

// ToJSON は r をファイルの形にする。
func (r ApplyRecord) ToJSON() ApplyRecordJSON {
	scope := r.ID.Scope.Labels
	if scope == nil {
		scope = map[string]string{}
	}
	j := ApplyRecordJSON{
		Resource: r.ID.String(), Provider: r.ID.Provider, Scope: scope, LocalID: r.ID.Local,
		Fingerprint: r.Fingerprint, Diff: r.Diff, IneffectiveApplies: r.Applies,
		FirstIneffective: r.First.UTC(), LastIneffective: r.Last.UTC(),
		Settle: r.Limits.Settle.String(), MaxApplies: r.Limits.MaxApplies,
		Stopped: r.Stopped, Reason: r.Reason,
	}
	if r.Limits.MaxDuration > 0 {
		j.MaxDuration = r.Limits.MaxDuration.String()
	}
	if r.Stopped {
		t := r.StoppedAt.UTC()
		j.StoppedAt = &t
	}
	return j
}

func (j ApplyRecordJSON) record() ApplyRecord {
	r := ApplyRecord{
		ID:          core.NewID(j.Provider, core.NewScope(j.Scope), j.LocalID),
		Fingerprint: j.Fingerprint, Diff: j.Diff, Applies: j.IneffectiveApplies,
		First: j.FirstIneffective, Last: j.LastIneffective,
		Stopped: j.Stopped, Reason: j.Reason,
	}
	r.Limits.Settle, _ = time.ParseDuration(j.Settle)
	r.Limits.MaxApplies = j.MaxApplies
	if j.MaxDuration != "" {
		r.Limits.MaxDuration, _ = time.ParseDuration(j.MaxDuration)
	}
	if j.StoppedAt != nil {
		r.StoppedAt = *j.StoppedAt
	}
	return r
}

// ReadApplyCheckState は状態のファイルを読む。無ければ空。
func ReadApplyCheckState(path string) (ApplyCheckState, error) {
	defer filelock.RLock(path)()
	return readApplyCheckState(path)
}

func readApplyCheckState(path string) (ApplyCheckState, error) {
	st := ApplyCheckState{Version: ApplyCheckStateVersion}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(strings.TrimSpace(string(b))) == 0) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("parse apply-check state %s: %w", path, err)
	}
	if st.Version > ApplyCheckStateVersion {
		return st, fmt.Errorf("apply-check state %s: version %d is newer than this sakutei (%d)", path, st.Version, ApplyCheckStateVersion)
	}
	return st, nil
}

func writeApplyCheckState(path string, st ApplyCheckState) error {
	st.Version = ApplyCheckStateVersion
	if st.Resources == nil {
		st.Resources = []ApplyRecordJSON{}
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".apply-check-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// UpdateApplyCheckState は状態のファイルを排他して読み、f で直して書く。
func UpdateApplyCheckState(path string, f func(*ApplyCheckState) error) error {
	unlock, err := filelock.Lock(path)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := readApplyCheckState(path)
	if err != nil {
		return err
	}
	if err := f(&st); err != nil {
		return err
	}
	st.UpdatedAt = time.Now().UTC()
	return writeApplyCheckState(path, st)
}

// LoadApplyChecker は状態のファイルから記録を読む（無ければ空）。
func LoadApplyChecker(path string) (*ApplyChecker, error) {
	c := NewApplyChecker()
	if path == "" {
		return c, nil
	}
	st, err := ReadApplyCheckState(path)
	if err != nil {
		return c, err
	}
	for _, j := range st.Resources {
		r := j.record()
		c.recs[r.ID.Key()] = &r
	}
	return c, nil
}

// Sync は状態のファイルと記録を合わせる: ファイルにある人の解く求めを取り込んで
// 解き（解いた記録を返す）、今の記録を書く。求めはファイルから消す。
func (c *ApplyChecker) Sync(path string) ([]ApplyRecord, []ApplyReleaseJSON, error) {
	if path == "" {
		return nil, nil, nil
	}
	var released []ApplyRecord
	var requests []ApplyReleaseJSON
	err := UpdateApplyCheckState(path, func(st *ApplyCheckState) error {
		requests = st.Releases
		for _, rq := range st.Releases {
			released = append(released, c.Release(rq.Resource)...)
		}
		st.Releases = nil
		st.Resources = st.Resources[:0]
		for _, r := range c.Records() {
			st.Resources = append(st.Resources, r.ToJSON())
		}
		return nil
	})
	return released, requests, err
}
