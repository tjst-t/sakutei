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

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"unicode/utf8"

	"github.com/tjst-t/sakutei/internal/eventlog"
	"github.com/tjst-t/sakutei/internal/prune"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/sdk"
)

// The notices of the convergence paths (the daemon, scoped runs, and the
// one-shot reconcile / plan in cmd/sakutei), each raised as a core.Event (§22.3).
// prefix is the text form's leading "sakutei: " / "sakutei reconcile: "; the
// text form is the line these paths printed before the Event type existed.

// emitApplyFailed raises ApplyFailed for an Apply (what = "apply") or a prune
// Delete (what = "prune apply") of id.
//
// A permanent failure (sdk.IsPermanent: permission, missing, ...) carries
// failure=permanent, which the daemon's level report raises as stuck at once
// (§22.3).
func EmitApplyFailed(w io.Writer, prefix, what string, id core.ID, err error) {
	attrs := map[string]string{"op": what}
	if sdk.IsPermanent(err) {
		attrs[eventlog.AttrFailure] = eventlog.FailurePermanent
	}
	eventlog.Emit(w, core.Event{
		Kind: core.EventApplyFailed, Severity: core.SeverityWarning,
		Resource: id, Message: err.Error(),
		Attrs: attrs,
		Text:  fmt.Sprintf("%s%s %s: %v", prefix, what, id, err),
	})
}

// emitPruned raises Pruned for an orphan deleted by prune; how is the path
// that allowed it ("PruneWithinScope" or "planned_change lane").
func EmitPruned(w io.Writer, prefix string, id core.ID, how string) {
	eventlog.Emit(w, core.Event{
		Kind: core.EventPruned, Severity: core.SeverityInfo,
		Resource: id, Message: "reality − desired; " + how,
		Attrs: map[string]string{"via": how},
		Text:  fmt.Sprintf("%spruned %s (reality − desired; %s)", prefix, id, how),
	})
}

// emitRequiresTask raises RequiresTask for a strategy: task resource (§6.5).
func EmitRequiresTask(w io.Writer, prefix string, sev core.Severity, id core.ID, msg string) {
	eventlog.Emit(w, core.Event{
		Kind: core.EventRequiresTask, Severity: sev, Resource: id, Message: msg,
		Text: fmt.Sprintf("%sRequiresTaskEvent: %s: %s", prefix, id, msg),
	})
}

// emitCapacityHalted raises CapacityHalted for the capacity gate (§13.4);
// text is the whole text-form line.
func EmitCapacityHalted(w io.Writer, reason, text string) {
	eventlog.Emit(w, core.Event{
		Kind: core.EventCapacityHalted, Severity: core.SeverityWarning,
		Message: reason, Text: text,
	})
}

// EmitPruneHalted raises the one PruneHalted Event of a prune safety guard
// halt (List error, incomplete List, count drop; §12.4); line is the caller's
// text-form line, after the count-drop guard's own line when it fired.
// providerName ("" when there is one provider) names whose List halted: the
// daemon's level report keeps one PruneHalted per provider (§22.3).
func EmitPruneHalted(w io.Writer, result prune.PruneResult, providerName, line string) {
	text := line
	attrs := map[string]string{"reason": result.HaltReason, "guard": "list"}
	if providerName != "" {
		attrs["provider"] = providerName
	}
	if h := result.HaltLine(); h != "" {
		text = h + "\n" + line
		attrs["guard"] = "count_drop"
	}
	eventlog.Emit(w, core.Event{
		Kind: core.EventPruneHalted, Severity: core.SeverityWarning,
		Message: result.HaltReason, Attrs: attrs, Text: text,
	})
}

// EmitProviderUpgradeDiff raises ProviderUpgradeDiff for a cause-C ghost diff held back from
// Apply (§10.4); action is what the path does about it ("NOT applying
// (review required)", or plan's "review before applying").
func EmitProviderUpgradeDiff(w io.Writer, prefix string, op core.Op, prevVersion, curVersion, action string) {
	msg := fmt.Sprintf("provider_version changed (%s → %s); %s", prevVersion, curVersion, action)
	eventlog.Emit(w, core.Event{
		Kind: core.EventProviderUpgradeDiff, Severity: core.SeverityWarning,
		Resource: op.Target, Cause: core.CauseTagC, Message: msg,
		Attrs: map[string]string{"provider_prev_version": prevVersion, "provider_cur_version": curVersion},
		Text:  fmt.Sprintf("%sProviderUpgradeDiff: %s [cause: C] %s", prefix, op.Target, msg),
	})
}

// EmitDrift raises Drift for a cause-B op of id; summary is the last-seen
// ring's account of what changed (secret fields in their ref form, §20.2).
// observed is the observed snapshot: its digest (attr "observed") is what the
// level report compares to tell a drift that moved again from one that
// merely persists (§22.3); the summary changes every cycle.
func EmitDrift(w io.Writer, prefix string, id core.ID, op core.Op, summary string, observed map[string]string) {
	eventlog.Emit(w, core.Event{
		Kind: core.EventDrift, Severity: core.SeverityWarning,
		Resource: id, Cause: core.CauseTagB, Message: summary,
		Attrs: map[string]string{"op": opKindLabel(op.Kind), "observed": snapshotDigest(observed)},
		Text:  fmt.Sprintf("%sDriftEvent: %s [cause: B] %s", prefix, id, summary),
	})
}

// EmitBudgetHalted raises BudgetHalted for the change budget (§13); text is
// the whole text-form line.
func EmitBudgetHalted(w io.Writer, reason, text string) {
	eventlog.Emit(w, core.Event{
		Kind: core.EventBudgetHalted, Severity: core.SeverityWarning,
		Message: reason, Text: text,
	})
}

// EmitPlannedChange raises PlannedChange for a planned_change lane notice
// (§13.3); text is the whole text-form line.
func EmitPlannedChange(w io.Writer, notice, text string) {
	eventlog.Emit(w, core.Event{
		Kind: core.EventPlannedChange, Severity: core.SeverityInfo,
		Message: notice, Text: text,
	})
}

// snapshotDigest is a short digest of an observed snapshot (secret fields are
// already in their ref form, §20.2), so an Event can say "reality moved"
// without quoting it.
func snapshotDigest(fields map[string]string) string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%s\x00", k, fields[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// checkDriftDetailMax bounds how much of a dry run's own diff a DriftEvent
// quotes (the full text is in the plan's diagnostic).
const checkDriftDetailMax = 512

// checkDriftSummary is the DriftEvent summary of a minimal-tier observation
// (§28.7): there is no snapshot to compare, only the target's dry run, so the
// summary says so and quotes the target's own diff when it gave one (on one
// line, cut at checkDriftDetailMax bytes).
func checkDriftSummary(check *core.CheckResult) string {
	s := "the target's dry run reports a change (minimal tier: no State to compare)"
	if check.Detail == "" {
		return s
	}
	d := check.Detail
	if len(d) > checkDriftDetailMax {
		n := checkDriftDetailMax
		for n > 0 && !utf8.RuneStart(d[n]) {
			n--
		}
		d = d[:n] + "…"
	}
	return s + "; diff: " + strconv.Quote(d)
}
