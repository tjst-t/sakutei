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

// planned.go: planned_change 宣言と専用予算レーン（§13.3、Backlog #39）。
//
// planned_change は「窓内・scope 内・種別一致の変更を max_count まで」通常予算とは
// 別のレーンで通す宣言である。バイパスではない:
//
//   - レーンに入った op は通常の rate limit・累積時間窓予算を消費しない（CheckCycle は
//     レーン外の op だけで通常予算を評価する）。通常予算の halt はレーン外の op だけを
//     止める。capacity gate など予算以外のゲートはレーンの op も止める（Merge）。
//   - レーンの枠は宣言ごとに max_count。枠は op を実際に発射する直前に ReserveLanes で
//     消費する（発射しなかった op は枠を消費しない。発射して失敗した op の枠は戻さない
//     ＝保守側）。枠を超えた分・別 scope・別種別・窓の外は通常予算で評価される。
//   - 窓（[start, end)）を過ぎた宣言は自動で失効し、以後その scope の変更は通常予算に戻る。
//
// kind と Plan の操作種別の対応（§29.1「kind 整合検証」の決定）:
//
//   - delete   : Delete op
//   - update   : Update op
//   - reimage  : 対になった Delete+Create（下記）。対の 1 組で枠を 1 消費する。どちらの op も
//     通常予算の破壊的変更（delete）には数えない
//   - recreate : reimage と同じ対。予算上の扱いも同じで、違いは監査上の名前だけ（データを
//     保たない作り直し）
//
// reimage / recreate の対（§29.1「delete/create ペアリング判定」の決定）:
// 同じ周期の Plan にある Delete X と Create Y は、次をすべて満たすとき 1 組の対になる。
//
//  1. 同じ provider である
//  2. scope のラベル集合が完全に一致する（宣言の scope の内側にある、だけでは足りない。
//     同じ置き場所 = 同じ scope の「入れ替え」であることを要求する）
//  3. 1 対 1 である（同じ scope の中で Local ID の昇順に組にする。余った Delete は対の無い
//     純粋な削除、余った Create はただの追加）
//
// 対にならない Delete（scope 内の純粋な削除）は reimage 宣言のレーンに入れず、通常予算で
// 評価し、PlannedChangeKindMismatch を出す（宣言 kind と Plan の操作種別の不一致）。
// 同様に、有効な宣言の scope 内にあるのに kind が合わずレーンに入らない破壊的 op はすべて
// PlannedChangeKindMismatch になる。
package budget

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// PlannedKind is the change kind a planned_change declares (§13.3).
type PlannedKind string

const (
	// PlannedDelete admits Delete ops.
	PlannedDelete PlannedKind = "delete"
	// PlannedUpdate admits Update ops.
	PlannedUpdate PlannedKind = "update"
	// PlannedCreate admits Create ops (Backlog #179): the approval of a
	// Create a provider requires approval for (provider.ApprovalRequirer,
	// e.g. a new nftables base chain).
	PlannedCreate PlannedKind = "create"
	// PlannedReimage admits paired Delete+Create ops (re-imaging); the pair is
	// not counted as a delete.
	PlannedReimage PlannedKind = "reimage"
	// PlannedRecreate admits paired Delete+Create ops (re-creation without
	// keeping data).  Budget-wise identical to PlannedReimage.
	PlannedRecreate PlannedKind = "recreate"
)

// PlannedKinds lists the accepted kinds in declaration order (for messages
// and the intent schema enum).
var PlannedKinds = []PlannedKind{PlannedDelete, PlannedUpdate, PlannedCreate, PlannedReimage, PlannedRecreate}

// Approval is the approval information of a planned_change (§13.3).  The
// loader checks it against the requirements (§14.4) of the declaration or of
// the operational profile (§14.5) and records the verdict in Gate.
type Approval struct {
	PR        int
	Approvers []string
}

// PlannedChange is one planned_change declaration (§13.3).
type PlannedChange struct {
	// Scope is the target scope; an op is in scope when its target's scope
	// matches it (core.Scope.Matches).  Never empty (validated at load).
	Scope core.Scope
	// Kind is the declared change kind.
	Kind PlannedKind
	// MaxCount is the number of changes the declaration admits over its whole
	// window (a reimage pair counts once).
	MaxCount int
	// Start and End bound the window [Start, End).
	Start, End time.Time
	// Approval is the recorded approval information.
	Approval Approval
	// Applicant is the applicant ID (the PR author); separation:
	// applicant_ne_approver refuses its approval.
	Applicant string
	// Gate is what must hold before the lane opens (§14.4 / §14.5), resolved by
	// the loader from the declaration and the operational profile.  The zero
	// value opens the lane in its window unconditionally.
	Gate PlannedGate
}

// Key identifies the declaration for its usage counter, so the counter
// survives the intent being reloaded every cycle.
func (p PlannedChange) Key() string {
	return fmt.Sprintf("%s|%s|%s|%s", p.Scope.String(), p.Kind,
		p.Start.UTC().Format(time.RFC3339), p.End.UTC().Format(time.RFC3339))
}

// Fingerprint identifies the declaration for its cooling-off clock (§14.5): the
// Key plus max_count, so widening a declaration (a larger max_count) is a new
// declaration whose cooling-off starts over.
func (p PlannedChange) Fingerprint() string {
	return fmt.Sprintf("%s|%d", p.Key(), p.MaxCount)
}

// String is the human-readable name used in Events.
func (p PlannedChange) String() string {
	return fmt.Sprintf("planned_change(scope=%s kind=%s window=%s..%s)", p.Scope.String(), p.Kind,
		p.Start.UTC().Format(time.RFC3339), p.End.UTC().Format(time.RFC3339))
}

// Active reports whether now is inside the window [Start, End).
func (p PlannedChange) Active(now time.Time) bool {
	return !now.Before(p.Start) && now.Before(p.End)
}

// Validate checks the declaration (load time).
func (p PlannedChange) Validate() error {
	if len(p.Scope.Labels) == 0 {
		return fmt.Errorf("scope is empty; name the labels it applies to (an empty scope would open the lane for the whole world)")
	}
	known := false
	for _, k := range PlannedKinds {
		if p.Kind == k {
			known = true
		}
	}
	if !known {
		return fmt.Errorf("unknown kind %q (want one of %v)", p.Kind, PlannedKinds)
	}
	if p.MaxCount < 1 {
		return fmt.Errorf("max_count must be >= 1 (got %d)", p.MaxCount)
	}
	if !p.End.After(p.Start) {
		return fmt.Errorf("window end %s is not after start %s", p.End.Format(time.RFC3339), p.Start.Format(time.RFC3339))
	}
	return nil
}

// windowLayouts are the accepted forms of one end of a window.  A form without
// a zone is UTC.
var windowLayouts = []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04"}

// ParseWindow parses a planned_change window "<start> .. <end>", e.g.
// "2026-08-01 00:00 .. 2026-08-01 06:00" (UTC) or RFC 3339 on both ends.
func ParseWindow(s string) (start, end time.Time, err error) {
	parts := strings.Split(s, "..")
	if len(parts) != 2 {
		return time.Time{}, time.Time{}, fmt.Errorf("window %q: want \"<start> .. <end>\"", s)
	}
	if start, err = parseWindowTime(parts[0]); err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("window %q: start: %w", s, err)
	}
	if end, err = parseWindowTime(parts[1]); err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("window %q: end: %w", s, err)
	}
	return start, end, nil
}

func parseWindowTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range windowLayouts {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a time (use RFC 3339 or \"YYYY-MM-DD HH:MM\" in UTC)", s)
}

// laneSlot records that an op was admitted to a planned_change lane: which
// declaration, and how much of its max_count the op consumes when dispatched
// (the Create half of a reimage pair consumes 0, its Delete half 1).
type laneSlot struct {
	decl   string
	weight int
}

func opKey(op core.Op) string {
	return fmt.Sprintf("%d|%s", op.Kind, op.Target.Key())
}

// SetPlannedChanges replaces the planned_change declarations (call it every
// cycle with the freshly loaded intent).  The usage of a declaration is kept
// across calls as long as its Key is unchanged.
//
// A declaration is "seen" here, when the intent is loaded, so its cooling-off
// clock starts (planned_change.declared) even in a cycle that never reaches
// CheckCycle — an alert-only cycle outside the remediation schedule's window
// (§14.2) or under freeze.  Otherwise a solo cooling-off would only start when
// the maintenance window opens and could outlast it (§14.5: counted from when
// the declaration was first seen).
func (e *Engine) SetPlannedChanges(pcs []PlannedChange) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.planned = append([]PlannedChange(nil), pcs...)
	now := e.cfg.now()
	e.withdrawMissing(now)
	for _, p := range e.planned {
		if now.Before(p.End) {
			e.noteDeclared(p, now, &e.pendingEvents)
		}
	}
}

// TakePlannedEvents returns (and clears) the planned_change notices raised
// outside CheckCycle — withdrawals and S4 failures from SetPlannedChanges.
// CheckCycle reports them itself; a cycle that skips CheckCycle (alert-only)
// takes them with this.
func (e *Engine) TakePlannedEvents() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.pendingEvents
	e.pendingEvents = nil
	return out
}

// RequeueEvents puts planned_change notices back for the cycle's CheckCycle to
// report.  A check of a single op outside the cycle's Plan (a Workflow's
// one-time operation, #139) takes the pending notices with it; this keeps
// them from being lost (#88).
func (e *Engine) RequeueEvents(evs []string) {
	if len(evs) == 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pendingEvents = append(evs, e.pendingEvents...)
}

// ReserveLanes consumes lane capacity for the ops about to be dispatched.  Call
// it right before executing ops that passed d (only the ops actually sent to
// the provider).  Ops outside the lanes are ignored.
func (e *Engine) ReserveLanes(d CycleDecision, ops []core.Op) {
	if len(d.lane) == 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, op := range ops {
		if s, ok := d.lane[opKey(op)]; ok {
			if e.laneUsed == nil {
				e.laneUsed = map[string]int{}
			}
			e.laneUsed[s.decl] += s.weight
		}
	}
}

// LaneUsed returns how much of the declaration's max_count has been consumed.
func (e *Engine) LaneUsed(p PlannedChange) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.laneUsed[p.Key()]
}

// laneAdmission is the result of splitting a cycle's ops between the
// planned_change lanes and the normal budget.
type laneAdmission struct {
	lane   map[string]laneSlot
	normal []core.Op
	events []string
	// unapproved are the ops that require approval (core.Op.RequiresApproval)
	// and were not admitted to a lane: held, and not in normal.
	unapproved map[string]bool
}

// split moves the ops not admitted to a lane into normal, except those that
// require approval, which are held as unapproved.  An op that requires
// approval and was admitted goes to normal as well: the lane is its approval
// gate, and the normal budget still counts it (core.Op.RequiresApproval).
func (a *laneAdmission) split(ops []core.Op, inLane func(i int) bool) {
	for i, op := range ops {
		switch {
		case inLane(i):
			if op.RequiresApproval {
				a.normal = append(a.normal, op)
			}
		case op.RequiresApproval:
			if a.unapproved == nil {
				a.unapproved = map[string]bool{}
			}
			a.unapproved[opKey(op)] = true
		default:
			a.normal = append(a.normal, op)
		}
	}
}

// admitLanes splits ops into lane-admitted ops and normal ops.  It does not
// consume capacity (ReserveLanes does).  Must be called with e.mu held.
func (e *Engine) admitLanes(ops []core.Op) laneAdmission {
	now := e.cfg.now()
	var out laneAdmission
	out.events, e.pendingEvents = e.pendingEvents, nil
	var active []PlannedChange
	for _, p := range e.planned {
		if now.Before(p.End) {
			e.noteDeclared(p, now, &out.events)
		}
		switch {
		case p.Active(now):
			if e.gateOpen(p, now, &out.events) {
				active = append(active, p)
			}
		case !now.Before(p.End):
			if !e.expiredNoted[p.Key()] {
				if e.expiredNoted == nil {
					e.expiredNoted = map[string]bool{}
				}
				e.expiredNoted[p.Key()] = true
				out.events = append(out.events, fmt.Sprintf(
					"PlannedChangeExpired: %s: window closed (used %d of max_count %d); changes in its scope are evaluated by the normal budget again",
					p, e.laneUsed[p.Key()], p.MaxCount))
			}
		}
	}
	if len(active) == 0 {
		out.split(ops, func(int) bool { return false })
		return out
	}

	// Admission order is deterministic (List order is not): by kind, then ID.
	order := make([]int, len(ops))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return opKey(ops[order[a]]) < opKey(ops[order[b]]) })

	pairs := reimagePairs(ops, order)

	remaining := make([]int, len(active))
	for j, p := range active {
		remaining[j] = p.MaxCount - e.laneUsed[p.Key()]
	}
	admitted := make([]int, len(active))
	// counted are the admitted ops that require approval: the normal budget
	// still counts them (core.Op.RequiresApproval).
	counted := make([]int, len(active))
	exhausted := make([]int, len(active))
	mismatched := make([][]string, len(active))
	inLane := make(map[int]bool)
	out.lane = map[string]laneSlot{}

	for _, i := range order {
		if inLane[i] {
			continue // the Create half of an admitted pair
		}
		op := ops[i]
		partner, paired := pairs[i]
		mismatchDecl, capped := -1, false
		for j, p := range active {
			if !op.Target.Scope.Matches(p.Scope) {
				continue
			}
			fits := false
			switch p.Kind {
			case PlannedDelete:
				fits = op.Kind == core.OpKindDelete
			case PlannedUpdate:
				fits = op.Kind == core.OpKindUpdate
			case PlannedCreate:
				fits = op.Kind == core.OpKindCreate
			case PlannedReimage, PlannedRecreate:
				fits = op.Kind == core.OpKindDelete && paired
			}
			if !fits {
				if mismatchDecl < 0 {
					mismatchDecl = j
				}
				continue
			}
			if remaining[j] < 1 {
				exhausted[j]++
				capped = true // the kind fits; only the capacity ran out
				continue
			}
			remaining[j]--
			admitted[j]++
			if op.RequiresApproval {
				counted[j]++
			}
			inLane[i] = true
			out.lane[opKey(op)] = laneSlot{decl: p.Key(), weight: 1}
			if paired {
				inLane[partner] = true
				out.lane[opKey(ops[partner])] = laneSlot{decl: p.Key(), weight: 0}
			}
			break
		}
		if !inLane[i] && !capped && mismatchDecl >= 0 && op.Destructive && !op.RequiresApproval {
			mismatched[mismatchDecl] = append(mismatched[mismatchDecl], op.Target.String())
		}
	}
	out.split(ops, func(i int) bool { return inLane[i] })

	for j, p := range active {
		if admitted[j] > 0 {
			budgetNote := "not counted in the normal budget"
			if counted[j] > 0 {
				budgetNote = fmt.Sprintf("%d change(s) requiring approval (ensure: absent, or the provider's) among them still counted in the normal budget, the rest not", counted[j])
			}
			out.events = append(out.events, fmt.Sprintf(
				"PlannedChange: %s: %d change(s) admitted to the planned_change lane (used %d of max_count %d before this cycle); %s",
				p, admitted[j], e.laneUsed[p.Key()], p.MaxCount, budgetNote))
		}
		if exhausted[j] > 0 {
			out.events = append(out.events, fmt.Sprintf(
				"PlannedChangeExhausted: %s: max_count %d reached; %d more change(s) are evaluated by the normal budget",
				p, p.MaxCount, exhausted[j]))
		}
		if n := len(mismatched[j]); n > 0 {
			examples := mismatched[j]
			if len(examples) > 3 {
				examples = examples[:3]
			}
			out.events = append(out.events, fmt.Sprintf(
				"PlannedChangeKindMismatch: %s: %d destructive op(s) in its scope do not match kind %s (e.g. %s); evaluated by the normal budget (§29.1)",
				p, n, p.Kind, strings.Join(examples, ", ")))
		}
	}
	if len(out.lane) == 0 {
		out.lane = nil
	}
	return out
}

// reimagePairs pairs Delete and Create ops of the same provider and exactly
// the same scope, one to one in Local ID order (see the package comment).  It
// maps each paired Delete index to its Create index.
func reimagePairs(ops []core.Op, order []int) map[int]int {
	placement := func(op core.Op) core.IDKey {
		return core.NewID(op.Target.Provider, op.Target.Scope, "").Key()
	}
	creates := map[core.IDKey][]int{}
	for _, i := range order {
		if ops[i].Kind == core.OpKindCreate {
			k := placement(ops[i])
			creates[k] = append(creates[k], i)
		}
	}
	pairs := map[int]int{}
	for _, i := range order {
		if ops[i].Kind != core.OpKindDelete {
			continue
		}
		k := placement(ops[i])
		if c := creates[k]; len(c) > 0 {
			pairs[i] = c[0]
			creates[k] = c[1:]
		}
	}
	return pairs
}

// InLane reports whether op was admitted to a planned_change lane.
func (d CycleDecision) InLane(op core.Op) bool {
	_, ok := d.lane[opKey(op)]
	return ok
}

// Unapproved reports whether op requires approval (core.Op.RequiresApproval,
// the Delete of an explicit absent or a provider's change, Backlog #179) and
// was not admitted to an open
// planned_change lane: it is held (IsHalted) whatever the budget says.
func (d CycleDecision) Unapproved(op core.Op) bool {
	return d.unapproved[opKey(op)]
}

// LaneOf returns the Key of the planned_change declaration whose lane admitted
// op ("" when op is outside the lanes).
func (d CycleDecision) LaneOf(op core.Op) string {
	return d.lane[opKey(op)].decl
}

// ApprovalOf returns the Key of the planned_change lane that admitted op under
// d and who approved that declaration (its approvers, else the applicant when
// the profile needs no approval), for the S4 record of the Apply (#187).  Both
// are "" when op is outside the lanes.
func (e *Engine) ApprovalOf(d CycleDecision, op core.Op) (key, approvers string) {
	key = d.LaneOf(op)
	if key == "" {
		return "", ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, p := range e.planned {
		if p.Key() != key {
			continue
		}
		if len(p.Approval.Approvers) > 0 {
			return key, strings.Join(p.Approval.Approvers, ",")
		}
		return key, p.Applicant
	}
	return key, ""
}

// OutsideLanes returns the ops that were not admitted to a planned_change lane
// — the ones the normal budget's window counter must record (CommitOps) — and
// the lane ops that require approval, which the normal budget counts as well.
func (d CycleDecision) OutsideLanes(ops []core.Op) []core.Op {
	if len(d.lane) == 0 {
		return ops
	}
	var out []core.Op
	for _, op := range ops {
		if !d.InLane(op) || op.RequiresApproval {
			out = append(out, op)
		}
	}
	return out
}

// LaneAdmitted is the number of ops admitted to planned_change lanes.
func (d CycleDecision) LaneAdmitted() int {
	return len(d.lane)
}

// OpenReimage returns the planned_change of kind reimage that covers scope and
// whose lane is open now (in its window, its gate open, §14.4 / §14.5): its Key
// and name, and whether its max_count has any left (#88).  A reimage Workflow
// (trigger: reimage) starts a run only when one is open.  Among several, the
// first with max_count left in declaration order wins.  Gate notices go to the
// cycle's planned_change events.
func (e *Engine) OpenReimage(scope core.Scope) (key, name string, left, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.cfg.now()
	for _, p := range e.planned {
		if p.Kind != PlannedReimage || !p.Active(now) || !scope.Matches(p.Scope) {
			continue
		}
		e.noteDeclared(p, now, &e.pendingEvents)
		if !e.gateOpen(p, now, &e.pendingEvents) {
			continue
		}
		hasLeft := e.laneUsed[p.Key()] < p.MaxCount
		if !ok || (hasLeft && !left) {
			key, name, left, ok = p.Key(), p.String(), hasLeft, true
		}
	}
	return key, name, left, ok
}

// ReserveReimage consumes one of the max_count of the reimage declaration key
// for re-imaging target by a Workflow (#88).  It is the lane consumption of a
// change dispatched outside the Plan: like ReserveLanes it is recorded in S4 by
// FlushLaneAudit, so max_count holds across restarts.  It fails when the
// declaration is no longer open (withdrawn, past its window) or used up.
func (e *Engine) ReserveReimage(key string, target core.ID, workflow string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensureGateMaps()
	now := e.cfg.now()
	for _, p := range e.planned {
		if p.Key() != key {
			continue
		}
		if !p.Active(now) {
			return "", fmt.Errorf("%s is outside its window", p)
		}
		if !e.gateOpen(p, now, &e.pendingEvents) {
			return "", fmt.Errorf("the lane of %s is closed", p)
		}
		used := e.laneUsed[key]
		if used >= p.MaxCount {
			e.pendingEvents = append(e.pendingEvents, fmt.Sprintf(
				"PlannedChangeExhausted: %s: max_count %d reached; %s is not reimaged by workflow %s", p, p.MaxCount, target, workflow))
			return "", fmt.Errorf("%s has used its max_count %d", p, p.MaxCount)
		}
		if e.laneUsed == nil {
			e.laneUsed = map[string]int{}
		}
		e.laneUsed[key] = used + 1
		e.pendingEvents = append(e.pendingEvents, fmt.Sprintf(
			"PlannedChange: %s: reimage of %s by workflow %s admitted to the lane (used %d of max_count %d); not counted in the normal budget",
			p, target, workflow, used+1, p.MaxCount))
		return p.String(), nil
	}
	return "", fmt.Errorf("the planned_change that started the run (%s) is no longer declared", keyString(key))
}
