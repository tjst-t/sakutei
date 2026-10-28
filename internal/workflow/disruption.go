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

package workflow

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/eventlog"
	"github.com/tjst-t/sakutei/v2/internal/task"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// 全体を止める型の更新（§6.3、Backlog #152）。Definition.Disruption を宣言した
// Workflow は、対象ごとに最初の手順の前で次を通す。進みは Run.Disrupt（S2）に
// 置き、上流の観測値は持たない（毎周 Observe し直す）。
//
//  1. 時間帯: いまが disruption.window の中で、notify_before と見積もり
//     （estimate）が時間帯の終わりまでに収まること。外なら待つ（起こさない）。remediation schedule が
//     alert-only の間は Workflow 自体が進まない（§14.2）。
//  2. 上流の積もり: upstream の各リソースを Observe し、fields に一致すること。
//     満たさない・観測できない（unknown ≠ ok）間は止めずに待つ。待ち始めてから
//     max_wait を過ぎたら halt して知らせる（WorkflowHalted、warning）。
//  3. 承認ゲート: 止める前のゲート（DisruptionGateName）を開き、見積もり・影響を
//     受ける下流・上流の観測値・時間帯を記録（Run.GateNotice・Reason）と Event に
//     出す。承認の要件・timeout・却下は stage のゲートと同じ（§6.4）。
//  4. 承認の後、時間帯と上流をもう一度確かめる（ゲートで待つ間に変わりうる）。
//     外れていれば 1・2 と同じく待つ（承認は残る）。
//  5. 知らせ: Disruption の Event（level、warning）を出し、その周は手順を始め
//     ない（知らせが止める前に届く）。notify_before を宣言すればその間も待つ。
//  6. 止める直前に時間帯（見積もりが終わりまでに収まること）と上流をもう一度
//     確かめる。外れていれば止めず、知らせを取り下げて（Disruption は resolved）
//     1・2 の待ちに戻る（承認は残る。max_wait の数えも同じ）。デーモンが落ちて
//     いる間に時間帯が閉じたときもここで止まらない。
//  7. 対象の手順を走らせる（Run.Disrupt.StartedAt）。手順の途中は確かめ直さない。
//     手順の間（halt を含む）は Disruption を firing に保ち、対象の手順を終えた周に
//     出さなくなる（resolved）。止めてから見積もりを過ぎても終わらなければ
//     overrun を付けて stuck（critical）に上げる。

// isDisruptionGate は gate が今の対象を止める前のゲートか。
func (s *stepper) isDisruptionGate(gate string) bool {
	st := s.run.Disrupt
	if s.def.Disruption == nil || st == nil || st.Stage >= len(s.def.Stages) {
		return false
	}
	return gate == DisruptionGateName(s.def.Stages[st.Stage].Name, st.Target)
}

// beforeDisruption は今の対象の手順を始めてよいか（知らせを出して notify_before を
// 過ぎたか）を返す。始められなければ待ち・ゲート・halt を記録して false。
func (s *stepper) beforeDisruption(ctx context.Context, stage Stage, target core.ID) bool {
	d := s.def.Disruption
	st := s.run.Disrupt
	if st == nil || st.Stage != s.run.Stage || st.Target != s.run.Target {
		st = &DisruptState{Stage: s.run.Stage, Target: s.run.Target}
		s.run.Disrupt = st
	}
	where := fmt.Sprintf("stage %s, target %s", stage.Name, target.Local)
	if !st.StartedAt.IsZero() {
		return true // 手順に入った（止めた）。手順の途中は確かめ直さない。
	}
	notified := !st.NotifiedAt.IsZero()
	if notified {
		if at := st.NotifiedAt.Add(d.NotifyBefore); s.now.Before(at) {
			s.wait(EventWaiting, "%s: disruption notified at %s; stopping %s from %s (notify_before %s)",
				where, st.NotifiedAt.Format(time.RFC3339), target.Local, at.Format(time.RFC3339), d.NotifyBefore)
			return false
		}
	}
	// 知らせた後、止める直前にも時間帯と上流を確かめ直す。外れていれば止めずに
	// 知らせを取り下げ（NotifiedAt を消す。Disruption はこの周に resolved）、
	// 承認は残したまま 1・2 の待ちに戻る。戻れば改めて知らせ、notify_before を置く。
	withdrawn := ""
	withdraw := func() {
		if notified {
			st.NotifiedAt = time.Time{}
			withdrawn = "; the disruption notice is withdrawn (not stopped)"
		}
	}

	// 1. 時間帯。知らせる前は notify_before と見積もりが、止める直前は見積もりが
	// 時間帯の終わりまでに収まること。
	lead := d.NotifyBefore
	if notified {
		lead = 0
	}
	if why := s.outsideWindow(d, lead); why != "" {
		st.WaitingSince = time.Time{}
		withdraw()
		s.wait(EventWaiting, "%s: not stopping %s: %s%s", where, target.Local, why, withdrawn)
		return false
	}

	// 2. 上流の積もり。
	upstream, ok := s.observeUpstream(ctx, d, target)
	if !ok {
		withdraw()
		if st.WaitingSince.IsZero() {
			st.WaitingSince = s.now.UTC()
		}
		if !s.now.Before(st.WaitingSince.Add(d.MaxWait)) {
			s.halt("%s: not stopping %s: the upstream backlog did not drop within max_wait %s (waiting since %s): %s%s",
				where, target.Local, d.MaxWait, st.WaitingSince.Format(time.RFC3339), upstream, withdrawn)
			return false
		}
		s.wait(EventWaiting, "%s: not stopping %s: upstream backlog over the limit (waiting since %s, up to max_wait %s): %s%s",
			where, target.Local, st.WaitingSince.Format(time.RFC3339), d.MaxWait, upstream, withdrawn)
		return false
	}
	if notified {
		// 6・7. 確かめを通った。止める（手順に入る）。
		st.WaitingSince = time.Time{}
		st.StartedAt = s.now.UTC()
		s.run.Reason = ""
		if !s.save() {
			return false
		}
		s.emit(EventStepDone, "%s: window and upstream re-checked (%s); stopping %s", where, upstream, target.Local)
		return true
	}
	st.WaitingSince = time.Time{}

	affects := s.affects(d, target)
	window := d.Window
	if window == "" {
		window = "(always)"
	}

	// 3. 止める前の承認ゲート。
	if !st.Approved {
		gate := DisruptionGateName(stage.Name, s.run.Target)
		notice := fmt.Sprintf("stops %s for an estimated %s; affects %s; upstream %s; window %s",
			target.Local, d.Estimate, affects, upstream, window)
		s.run.Phase = PhaseAwaitingApproval
		s.run.Gate = gate
		s.run.GateOpenedAt = s.now.UTC()
		s.run.GateDeadline = s.now.Add(d.Gate.Timeout).UTC()
		s.run.GateRequirements = d.Gate.Requirements
		s.run.GateNotice = map[string]string{
			"target": target.String(), "estimate": d.Estimate.String(), "affects": affects,
			"upstream": upstream, "window": window,
		}
		s.run.Reason = "disruptive update: " + notice
		if s.save() {
			s.events = append(s.events, Event{Kind: EventAwaiting, Workflow: s.def.Name, Resource: target,
				Attrs: copyAttrs(s.run.GateNotice),
				Message: fmt.Sprintf("%s: disruptive update — %s; awaiting approval at gate %s before stopping (%d approval(s)%s, timeout %s, deadline %s): POST /workflows/%s/gates/%s/approve",
					where, notice, gate, d.Gate.Requirements.Needed(), describe(d.Gate.Requirements),
					d.Gate.Timeout, s.run.GateDeadline.Format(time.RFC3339), s.def.Name, gate)})
		}
		return false
	}

	// 5. 知らせ。この周は手順を始めない（知らせが止める前に届く）。
	st.NotifiedAt = s.now.UTC()
	s.run.Reason = ""
	if !s.save() {
		return false
	}
	s.emit(EventWaiting, "%s: disruption notified; stopping %s from %s",
		where, target.Local, s.run.Disrupt.NotifiedAt.Add(d.NotifyBefore).Format(time.RFC3339))
	return false
}

// outsideWindow は今が止めてよい時間帯の外か、今から lead と見積もりを足した
// 時刻が時間帯の終わりまでに収まらなければ、その説明を返す（よければ空）。
func (s *stepper) outsideWindow(d *Disruption, lead time.Duration) string {
	win, err := task.ParseWindow(d.Window)
	if err != nil {
		return err.Error() // Validate が拒むので来ない
	}
	if !win.Contains(s.now) {
		return fmt.Sprintf("outside the disruption window %s (now %s)", win, s.now.Format("Mon 15:04 MST"))
	}
	if end := s.now.Add(lead + d.Estimate - time.Nanosecond); !win.Contains(end) {
		what := fmt.Sprintf("the estimate %s", d.Estimate)
		if lead > 0 {
			what = fmt.Sprintf("notify_before %s and the estimate %s", lead, d.Estimate)
		}
		return fmt.Sprintf("%s does not fit before the disruption window %s ends (now %s)",
			what, win, s.now.Format("Mon 15:04 MST"))
	}
	return ""
}

// observeUpstream は上流の各リソースを Observe し、観測値の説明と、全部が fields を
// 満たすかを返す。観測できないものは満たさない（unknown ≠ ok）。
func (s *stepper) observeUpstream(ctx context.Context, d *Disruption, target core.ID) (string, bool) {
	if len(d.Upstream) == 0 {
		return "(none declared)", true
	}
	all := true
	parts := make([]string, 0, len(d.Upstream))
	for _, u := range d.Upstream {
		id := refFor(u.Resource, target)
		obs := s.observe(ctx, id)
		ok := s.satisfied(obs, id, u.Fields)
		if !ok {
			all = false
		}
		parts = append(parts, fmt.Sprintf("%s/%s %s", id.Provider, id.Local, describeObserved(obs, u.Fields, ok, s.obsErr[id.Key()])))
	}
	return strings.Join(parts, "; "), all
}

// describeObserved は上流の観測値を、宣言した fields（と value の欄があればそれ）
// について「field=観測値」の並びで返す。
func describeObserved(obs core.Observation, want map[string]string, ok bool, obsErr error) string {
	if obs.Availability != core.AvailabilityOk || obs.State == nil {
		if obsErr != nil {
			return "unobservable (" + shortReason(obsErr) + "; unknown ≠ absent)"
		}
		return "unobservable (unknown ≠ absent)"
	}
	keys := make([]string, 0, len(want)+1)
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if _, declared := want["value"]; !declared {
		if _, has := obs.State["value"]; has {
			keys = append([]string{"value"}, keys...)
		}
	}
	var fs []string
	for _, k := range keys {
		got := "(none)"
		if tv, has := obs.State[k]; has {
			got = fmt.Sprintf("%v", tv.Value)
		}
		if w, declared := want[k]; declared {
			fs = append(fs, fmt.Sprintf("%s=%s (want %s)", k, got, w))
		} else {
			fs = append(fs, fmt.Sprintf("%s=%s", k, got))
		}
	}
	verdict := "within limits"
	if !ok {
		verdict = "over the limit"
	}
	return strings.Join(fs, ", ") + " — " + verdict
}

// affects は止めている間に使えなくなる下流を「provider/id」の並びで返す。
func (s *stepper) affects(d *Disruption, target core.ID) string {
	ids := d.AffectsFor(target)
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.Provider+"/"+id.Local)
	}
	return strings.Join(out, ", ")
}

// disrupting は知らせを出した対象の手順が終わっていなければ（halt を含む）、
// Disruption の Event をこの周にも出す（level: 続く間は firing、出さなくなった
// 周に resolved）。Message は周ごとに変えない。
func (s *stepper) disrupting() {
	d, st := s.def.Disruption, s.run.Disrupt
	if d == nil || st == nil || st.NotifiedAt.IsZero() || s.run.Phase == PhaseDone ||
		st.Stage >= len(s.def.Stages) || st.Target >= len(s.def.Stages[st.Stage].Targets) {
		return
	}
	target := s.def.Stages[st.Stage].Targets[st.Target]
	window := d.Window
	if window == "" {
		window = "(always)"
	}
	affects := s.affects(d, target)
	from := st.NotifiedAt.Add(d.NotifyBefore)
	msg := fmt.Sprintf("run %s stops %s for an estimated %s from %s; affects %s (window %s)",
		s.run.RunID, target.Local, d.Estimate, from.Format(time.RFC3339), affects, window)
	attrs := map[string]string{
		"run": s.run.RunID, "estimate": d.Estimate.String(), "affects": affects, "window": window,
		"notified_at": st.NotifiedAt.Format(time.RFC3339),
	}
	if !st.StartedAt.IsZero() && !s.now.Before(st.StartedAt.Add(d.Estimate)) {
		// 止めてから見積もりを過ぎても対象の手順が終わっていない（halt を含む）: stuck に上げる。
		attrs[eventlog.AttrOverrun] = "true"
		msg += fmt.Sprintf("; overrun: still not done after the estimate (until %s)", st.StartedAt.Add(d.Estimate).Format(time.RFC3339))
	}
	s.events = append(s.events, Event{Kind: EventDisruption, Workflow: s.def.Name, Resource: target, Message: msg, Attrs: attrs})
}

func copyAttrs(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
