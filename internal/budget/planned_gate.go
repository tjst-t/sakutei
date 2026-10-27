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

// planned_gate.go: planned_change のレーンを開く条件（§14.4 / §14.5、Backlog #44）。
//
// レーンは窓の中でも、次の 2 つが揃うまで開かない（開かない間、scope 内の変更は通常予算で
// 評価される＝保守側）:
//
//   - 承認: approval が requirements（宣言の requirements、なければ運用プロファイルの既定）を
//     満たす。判定は loader が行い、満たさなければ Gate.Unapproved に理由が入る。
//   - cooling-off（solo）: 宣言を初めて見た時刻から Gate.CoolingOff が過ぎている。宣言の時刻は
//     S4 に planned_change.declared として残し、常駐の再起動や非常駐の reconcile（段0-zero、
//     §23.7）は S4 から読み直す。だから cooling-off は「N 時間後に動くタイマー」ではなく
//     「評価した時点で N 時間経っていれば通すゲート」であり、常駐でも reconcile でも同じ式で
//     判定する。その間に宣言を消せば（PR の revert）レーンは開かずに止まる。
//
// 宣言・開く前の取り下げ・レーンが開いたこと・レーンで発射した件数を S4 に残す（declared /
// withdrawn / opened / executed）。取り下げた宣言の時計は捨てるので、書き戻すと cooling-off はやり直し。
// executed の合計は reconcile が実行をまたいで max_count を守るのに使う。S4 に書けない記録に
// 基づく判断は効かせない（宣言が書けなければ cooling-off の時計は動かさず、opened が書け
// なければレーンを開かない）。
package budget

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/audit"
)

// PlannedGate is what must hold, besides the window, before a planned_change
// lane opens (§14.4 / §14.5).  The loader resolves it from the declaration and
// the operational profile.
type PlannedGate struct {
	// Profile is the operational profile the gate was resolved under (solo /
	// team / enterprise).  Empty for a declaration built without the loader;
	// such a declaration has no gate and leaves no S4 records but declared.
	Profile string
	// Unapproved is non-empty when the approval does not satisfy the
	// requirements: the lane never opens.
	Unapproved string
	// Approved describes the approval that satisfied the requirements (for the
	// S4 record), e.g. "2 of 2 approver(s) [alice bob], separation applicant_ne_approver".
	Approved string
	// CoolingOff delays the lane: it opens only CoolingOff after the
	// declaration was first seen (solo, §14.5).  0 is no cooling-off.
	CoolingOff time.Duration
}

// AttachAudit makes the engine record planned_change declarations, openings
// and lane executions in the S4 audit log, and restores from it what earlier
// runs recorded: when each declaration was first seen (the cooling-off clock)
// and how much of each lane was used (max_count across runs and restarts).
func (e *Engine) AttachAudit(log *audit.Log) error {
	recs, err := audit.ReadAll(log.Path)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.audit = log
	e.ensureGateMaps()
	for _, rec := range recs {
		switch rec.Kind {
		case audit.KindPlannedChangeDeclared:
			// The latest declared since the last withdrawn wins: a re-declaration
			// recorded after a withdrawal that could not be recorded still restarts
			// the cooling-off (later is the conservative side).
			fp := fmt.Sprintf("%s|%d", rec.PlannedChange, rec.Count)
			if at, ok := e.declaredAt[fp]; !ok || rec.Time.After(at) {
				e.declaredAt[fp] = rec.Time
				e.declaredMax[fp] = rec.Count
			}
		case audit.KindPlannedChangeWithdrawn:
			fp := fmt.Sprintf("%s|%d", rec.PlannedChange, rec.Count)
			delete(e.declaredAt, fp)
			delete(e.declaredMax, fp)
		case audit.KindPlannedChangeOpened:
			e.opened[fmt.Sprintf("%s|%d", rec.PlannedChange, rec.Count)] = true
		case audit.KindPlannedChangeExecuted:
			if e.laneUsed == nil {
				e.laneUsed = map[string]int{}
			}
			e.laneUsed[rec.PlannedChange] += rec.Count
			e.laneFlushed[rec.PlannedChange] += rec.Count
		}
	}
	return nil
}

// AttachAuditReadOnly is AttachAudit for a preview (sakutei plan, #135): the
// engine restores what earlier runs recorded in S4 and evaluates the gates as
// a run now would, but writes nothing to the log.  A declaration not yet in S4
// is taken as declared now (its cooling-off would start with that run).
func (e *Engine) AttachAuditReadOnly(log *audit.Log) error {
	if err := e.AttachAudit(log); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.auditRO = true
	return nil
}

// appendAudit appends rec to the attached S4 log, or does nothing when the log
// is attached read-only.  Must be called with e.mu held and e.audit set.
func (e *Engine) appendAudit(rec audit.Record) error {
	if e.auditRO {
		return nil
	}
	return e.audit.Append(rec)
}

// HasAudit reports whether an S4 audit log is attached.
func (e *Engine) HasAudit() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.audit != nil
}

// DeclaredAt returns when the declaration was first seen (zero if never).
func (e *Engine) DeclaredAt(p PlannedChange) time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.declaredAt[p.Fingerprint()]
}

// FlushLaneAudit records in S4 the lane usage not yet recorded (one
// planned_change.executed per declaration).  The resident loop calls it at the
// end of every cycle; the one-shot reconcile reserves its lanes up front and
// calls it before dispatching anything, so a run that cannot record its usage
// executes nothing in a lane.  Without an attached log it does nothing.
func (e *Engine) FlushLaneAudit() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.audit == nil {
		return nil
	}
	e.ensureGateMaps()
	keys := make([]string, 0, len(e.laneUsed))
	for k := range e.laneUsed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		delta := e.laneUsed[k] - e.laneFlushed[k]
		if delta <= 0 {
			continue
		}
		name := k
		for _, p := range e.planned {
			if p.Key() == k {
				name = p.String()
			}
		}
		if err := e.appendAudit(audit.Record{
			Time: e.cfg.now(), Kind: audit.KindPlannedChangeExecuted,
			PlannedChange: k, Count: delta,
			Detail: fmt.Sprintf("%s: %d change(s) dispatched in the lane (used %d in total)", name, delta, e.laneUsed[k]),
		}); err != nil {
			return err
		}
		e.laneFlushed[k] = e.laneUsed[k]
	}
	return nil
}

func (e *Engine) ensureGateMaps() {
	if e.declaredAt == nil {
		e.declaredAt = map[string]time.Time{}
	}
	if e.opened == nil {
		e.opened = map[string]bool{}
	}
	if e.gateNoted == nil {
		e.gateNoted = map[string]string{}
	}
	if e.laneFlushed == nil {
		e.laneFlushed = map[string]int{}
	}
	if e.declaredMax == nil {
		e.declaredMax = map[string]int{}
	}
}

// withdrawMissing forgets the cooling-off clock of every declaration that was
// seen (or restored from S4) but is no longer in intent and never opened its
// lane: removing a declaration during its cooling-off halts it, and writing
// the same declaration back starts the cooling-off over (§14.5).  The
// withdrawal is recorded in S4 so the next run (reconcile, restart) does not
// restore the old clock.  The resident loop sees the removal when it reloads
// intent; the one-shot reconcile compares the loaded declarations with the
// clocks restored from S4.  Must be called with e.mu held.
func (e *Engine) withdrawMissing(now time.Time) {
	e.ensureGateMaps()
	present := map[string]bool{}
	for _, p := range e.planned {
		present[p.Fingerprint()] = true
	}
	fps := make([]string, 0, len(e.declaredAt))
	for fp := range e.declaredAt {
		if !present[fp] && !e.opened[fp] {
			fps = append(fps, fp)
		}
	}
	sort.Strings(fps)
	for _, fp := range fps {
		key, max := strings.TrimSuffix(fp, fmt.Sprintf("|%d", e.declaredMax[fp])), e.declaredMax[fp]
		msg := fmt.Sprintf("PlannedChangeWithdrawn: %s (max_count %d) was removed from intent before its lane opened; writing it back starts its cooling-off over (§14.5)", keyString(key), max)
		if e.audit != nil {
			if err := e.appendAudit(audit.Record{
				Time: now, Kind: audit.KindPlannedChangeWithdrawn, PlannedChange: key, Count: max, Detail: msg,
			}); err != nil {
				// Forget the clock anyway: a re-declaration writes a later declared
				// record, which AttachAudit prefers (conservative).
				msg += fmt.Sprintf(" [the withdrawal could not be recorded in S4: %v]", err)
			}
		}
		delete(e.declaredAt, fp)
		delete(e.declaredMax, fp)
		delete(e.gateNoted, fp)
		e.pendingEvents = append(e.pendingEvents, msg)
	}
}

// keyString names a declaration known only by its Key (a withdrawn one is no
// longer in intent) the way PlannedChange.String does, so every Event names
// declarations alike (#111).  A key of another shape is returned as is.
func keyString(key string) string {
	parts := strings.Split(key, "|")
	if len(parts) < 4 {
		return key
	}
	n := len(parts)
	return fmt.Sprintf("planned_change(scope=%s kind=%s window=%s..%s)",
		strings.Join(parts[:n-3], "|"), parts[n-3], parts[n-2], parts[n-1])
}

// noteDeclared starts the declaration's cooling-off clock the first time it is
// seen, recording it in S4 first.  Must be called with e.mu held.
func (e *Engine) noteDeclared(p PlannedChange, now time.Time, events *[]string) {
	e.ensureGateMaps()
	fp := p.Fingerprint()
	if _, ok := e.declaredAt[fp]; ok {
		return
	}
	if e.audit != nil {
		if err := e.appendAudit(audit.Record{
			Time: now, Kind: audit.KindPlannedChangeDeclared,
			PlannedChange: p.Key(), Count: p.MaxCount, Actor: p.Applicant,
			Detail: fmt.Sprintf("%s declared (max_count %d, profile %s, pr %d, approvers %v)",
				p, p.MaxCount, profileName(p.Gate.Profile), p.Approval.PR, p.Approval.Approvers),
		}); err != nil {
			e.noteGate(fp, events, fmt.Sprintf(
				"PlannedChangeAuditFailed: %s: the declaration cannot be recorded in S4 (%v); its cooling-off has not started and its lane stays closed", p, err))
			return
		}
	}
	e.declaredAt[fp] = now
	e.declaredMax[fp] = p.MaxCount
}

// gateOpen reports whether the declaration's gate lets its lane open now.
// Must be called with e.mu held, after noteDeclared.
func (e *Engine) gateOpen(p PlannedChange, now time.Time, events *[]string) bool {
	g := p.Gate
	fp := p.Fingerprint()
	if g.Unapproved != "" {
		e.noteGate(fp, events, fmt.Sprintf(
			"PlannedChangeUnapproved: %s: %s; the lane stays closed and changes in its scope are evaluated by the normal budget (§14.4)", p, g.Unapproved))
		return false
	}
	var basis []string
	if g.Approved != "" {
		basis = append(basis, "approval "+g.Approved)
	}
	if g.CoolingOff > 0 {
		if e.audit == nil {
			e.noteGate(fp, events, fmt.Sprintf(
				"PlannedChangeCoolingOff: %s: cooling-off %s needs the S4 audit log (--audit-log) to record when it was declared; the lane stays closed", p, g.CoolingOff))
			return false
		}
		at, ok := e.declaredAt[fp]
		if !ok {
			return false // the declaration could not be recorded (noted by noteDeclared)
		}
		until := at.Add(g.CoolingOff)
		if now.Before(until) {
			e.noteGate(fp, events, fmt.Sprintf(
				"PlannedChangeCoolingOff: %s: declared %s, cooling-off %s until %s; the lane stays closed until then (remove the declaration to halt it, §14.5)",
				p, at.UTC().Format(time.RFC3339), g.CoolingOff, until.UTC().Format(time.RFC3339)))
			return false
		}
		basis = append(basis, fmt.Sprintf("cooling-off %s elapsed since the declaration at %s", g.CoolingOff, at.UTC().Format(time.RFC3339)))
	}
	if g.Profile == "" || e.opened[fp] {
		return true
	}
	detail := fmt.Sprintf("%s: lane opened (profile %s; %s)", p, g.Profile, strings.Join(basis, "; "))
	if len(basis) == 0 {
		detail = fmt.Sprintf("%s: lane opened (profile %s; no approval required)", p, g.Profile)
	}
	if e.audit != nil {
		if err := e.appendAudit(audit.Record{
			Time: now, Kind: audit.KindPlannedChangeOpened,
			PlannedChange: p.Key(), Count: p.MaxCount, Actor: p.Applicant, Detail: detail,
		}); err != nil {
			e.noteGate(fp, events, fmt.Sprintf(
				"PlannedChangeAuditFailed: %s: the opening cannot be recorded in S4 (%v); the lane stays closed", p, err))
			return false
		}
	}
	e.opened[fp] = true
	*events = append(*events, "PlannedChangeOpened: "+detail)
	return true
}

// noteGate appends msg unless it is the declaration's last notice (a gate that
// stays closed is reported once, not every cycle).
func (e *Engine) noteGate(fp string, events *[]string, msg string) {
	if e.gateNoted[fp] == msg {
		return
	}
	e.gateNoted[fp] = msg
	*events = append(*events, msg)
}

func profileName(p string) string {
	if p == "" {
		return "(none)"
	}
	return p
}
