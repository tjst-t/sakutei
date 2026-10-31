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

package cycle_test

import (
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/record"
)

// 性質のテスト（§4 の不変条件 16・18）：偽の provider に、送る途中で止まる・送れて
// いない・送れたか分からない・効かない操作と、観測できない対象・一回実行と常駐の
// 入れ替わり・続ける承認をランダムに与えて周を重ね、どの周でも次が成り立つことを
// 確かめる。
//
//   - 不変条件 16：送る前に送信中の記録が Record にある。同じ run の同じ段を、
//     送れたかもしれない形で 2 回送るのは、人が続けると承認した回だけ。critical の
//     段は常駐だけが送る。
//   - 不変条件 18：手順が「済んだ」になるのは done_when が真のときだけ（観測でき
//     ない対象は済まない）。済んでいる（done_when が真の）対象には手順を起こさない。
func TestProcedureProperties(t *testing.T) {
	seeds := 150
	if testing.Short() || raceEnabled {
		seeds = 15
	}
	for seed := 1; seed <= seeds; seed++ {
		seed := seed
		t.Run(fmt.Sprint(seed), func(t *testing.T) { procedureProperty(t, int64(seed)) })
	}
}

// 操作の答え。
const (
	sendOK      = iota // 送れて効く
	sendNoop           // 送れたが効かない
	sendNotSent        // 送れていない（ErrNotSent）
	sendError          // 送れたか分からない誤り
	sendCrash          // 送る途中でプロセスが止まる
	sendKinds
)

func procedureProperty(t *testing.T, seed int64) {
	rng := rand.New(rand.NewSource(seed))
	h := newHarness(t)
	h.in.Ranges = nil
	nTargets := 1 + rng.Intn(3)
	var targets []core.ID
	for i := 0; i < nTargets; i++ {
		name := fmt.Sprintf("sw%d", i)
		h.in.Resources = append(h.in.Resources, swDecl(name, "v2"))
		targets = append(targets, rid("rec", name))
	}
	var steps []core.Step
	critical := map[int]bool{}
	for i := 0; i < 1+rng.Intn(3); i++ {
		s := core.Step{Name: fmt.Sprintf("s%d", i), Kind: core.StepInvoke, Op: "upgrade", Critical: rng.Intn(3) == 0}
		if rng.Intn(4) == 0 {
			s = core.Step{Name: fmt.Sprintf("s%d", i), Kind: core.StepWait, For: time.Duration(rng.Intn(3)) * time.Minute}
		}
		critical[i] = s.Critical
		steps = append(steps, s)
	}
	proc := upgrade(steps...)
	proc.Concurrency = 1 + rng.Intn(nTargets)
	h.in.Procedures = []core.Procedure{proc}

	sent := map[string]int{}    // run と段 → 送れたかもしれない回数
	allowed := map[string]int{} // run → 人が続けると承認した回数
	var behaviour int
	blind := map[core.ID]bool{}
	var failures []string
	fail := func(format string, a ...any) { failures = append(failures, fmt.Sprintf(format, a...)) }

	h.p.SetOp("rec", "upgrade", core.OpSpec{}, nil)
	h.p.OnInvoke(func(id core.ID, op string, _ core.Fields) error {
		runs, err := record.ReadRuns(h.rec.Dir())
		if err != nil {
			fail("runs が読めない: %v", err)
		}
		var run *record.Run
		for i := range runs {
			if runs[i].Target == id.String() {
				run = &runs[i]
			}
		}
		if run == nil || run.Sending == nil {
			fail("送信中の記録が無いまま %s に送った: %+v", id, run)
			return nil
		}
		if critical[run.Sending.Step] && !h.in.Daemon {
			fail("一回実行が critical の段 %d を送った", run.Sending.Step)
		}
		if behaviour != sendNotSent {
			k := fmt.Sprintf("%s#%d", run.RunID, run.Sending.Step)
			sent[k]++
			if sent[k] > 1+allowed[run.RunID] {
				fail("%s の段 %d を %d 回送った（続ける承認 %d 回）", run.RunID, run.Sending.Step, sent[k], allowed[run.RunID])
			}
		}
		switch behaviour {
		case sendOK:
			h.p.Set(id, present(core.Fields{"value": "v2"}))
			if blind[id] {
				h.p.Omit(id) // 効いても観測できないまま
			}
		case sendNotSent:
			return fmt.Errorf("届かない: %w", core.ErrNotSent)
		case sendError:
			return errors.New("時間切れ")
		case sendCrash:
			panic(crash{})
		}
		return nil
	})

	for cyc := 0; cyc < 10; cyc++ {
		// 対象の観測：ずれ・宣言どおり・観測できない。
		blind = map[core.ID]bool{}
		for _, id := range targets {
			switch rng.Intn(5) {
			case 0:
				h.p.Set(id, present(core.Fields{"value": "v2"}))
			case 1:
				h.p.Set(id, present(core.Fields{"value": "v1"}))
				h.p.Omit(id)
				blind[id] = true
			default:
				if cyc == 0 || rng.Intn(2) == 0 {
					h.p.Set(id, present(core.Fields{"value": "v1"}))
				}
			}
		}
		before := map[core.ID]any{}
		for _, id := range targets {
			before[id] = got(h, id)
		}
		runsBefore, _ := record.ReadRuns(h.rec.Dir())
		hadRun := map[string]bool{}
		for _, r := range runsBefore {
			hadRun[r.Target] = true
		}
		behaviour = rng.Intn(sendKinds)
		h.in.Daemon = rng.Intn(2) == 0
		h.clock.t = h.clock.t.Add(time.Duration(rng.Intn(3)) * time.Minute)
		invBefore := len(h.p.Invocations())

		r, crashed := h.runCrash()
		if len(failures) > 0 {
			t.Fatalf("周 %d: %s", cyc, strings.Join(failures, "\n"))
		}
		if crashed {
			continue
		}
		// 済んでいる対象には起こさない（不変条件 18）。
		for _, inv := range h.p.Invocations()[invBefore:] {
			if !hadRun[inv.ID.String()] && before[inv.ID] == "v2" && !blind[inv.ID] {
				t.Fatalf("周 %d: done_when が真の %s に手順を起こした", cyc, inv.ID)
			}
		}
		for _, s := range r.Runs {
			id, _ := core.ParseRef(s.Target)
			if s.State == "done" && (blind[id] || got(h, id) != "v2") {
				t.Fatalf("周 %d: done_when が真でない %s を済んだにした: %+v", cyc, id, s)
			}
			// 人がときどき続けると承認する。
			if s.State == "halted" && strings.Contains(s.Request, "-resume-") && rng.Intn(3) == 0 {
				if err := record.WriteDecision(h.rec.Dir(), s.Request, record.Decision{By: "bob", Decision: record.Approve, At: h.clock.t}); err != nil {
					t.Fatal(err)
				}
				allowed[s.RunID]++
			}
		}
		if len(r.Errors) > 0 {
			t.Fatalf("周 %d: 周の誤り %v", cyc, r.Errors)
		}
	}
	if _, err := record.Verify(h.rec.Dir()); err != nil {
		t.Fatalf("監査の鎖: %v", err)
	}
}
