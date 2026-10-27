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

// redfish_system_test.go — #81: Redfish の「システム」（電源・起動先）を偽 BMC を
// 相手にデーモンの経路（RunCycle）で収束させる（§29.1 の（2）〜（5））。
//
//	[AC-81-1]  電源と起動先の宣言に収束する（起動先の PATCH が先、電源が後）。
//	           次の周は何も操作しない。
//	[AC-81-2]  手で変えた電源を次の周で戻す。
//	[AC-81-3]  PoweringOff の間は操作を重ねない。Off になれば収束。
//	[AC-81-4]  BMC に届かない周は Unknown で、何も操作しない。
//	[AC-81-5]  boot_enabled: Once は計画の時点でエラー（BMC に何も送らない）。
//	[AC-81-6]  既定（graceful）では ForceOff に進まず GracefulShutdown を送り
//	           直し、ずれの level が既定 5 周で stuck に上がる。
//	[AC-81-7]  graceful_then_force は force_after を過ぎてから破壊的な ForceOff
//	           を送る（それまでは GracefulShutdown）。
//	[AC-81-8]  ForceOff は予算（max-destructive-per-cycle）で止まる。
//	           GracefulShutdown は破壊的に数えない。
package acceptance

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/budget"
	"github.com/tjst-t/sakutei/internal/cause"
	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/eventlog"
	"github.com/tjst-t/sakutei/internal/reconcile"
	"github.com/tjst-t/sakutei/internal/redfish"
	"github.com/tjst-t/sakutei/internal/redfish/redfishtest"
	"github.com/tjst-t/sakutei/internal/redfish/system"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// rfClock は偽 BMC と provider で共有する時刻。
type rfClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *rfClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *rfClock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type rfRig struct {
	t      *testing.T
	clk    *rfClock
	bmcs   map[string]*redfishtest.Server
	p      *system.Provider
	m      *system.MediaProvider
	d      *reconcile.Daemon
	dir    string
	out    *syncBuffer
	errOut *syncBuffer
	rec    *recordingSink
}

// newRFRig は nodes の偽 BMC（opts）と、それを相手にする Daemon を立てる。
// resources は intent の resources（JSON の配列の中身）。
func newRFRig(t *testing.T, nodes []string, opts redfishtest.Options, resources string, cfg reconcile.Config) *rfRig {
	t.Helper()
	clk := &rfClock{t: time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)}
	opts.Now = clk.Now
	r := &rfRig{t: t, clk: clk, bmcs: map[string]*redfishtest.Server{}, out: &syncBuffer{}, errOut: &syncBuffer{}, rec: &recordingSink{}}
	clients := map[string]redfish.Client{}
	for _, n := range nodes {
		srv := redfishtest.New(opts)
		t.Cleanup(srv.Close)
		c, err := redfish.New(redfish.Config{Endpoint: srv.URL, Username: "admin", Password: "password",
			InsecureSkipVerify: true, Timeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		r.bmcs[n], clients[n] = srv, c
	}
	r.p = system.New(clients, system.Options{Now: clk.Now})
	r.m = system.NewMedia(clients)

	root := t.TempDir()
	r.dir = filepath.Join(root, "intent")
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r.writeIntent(resources)
	cfg.IntentDir = r.dir
	cfg.GeneratedDir = filepath.Join(root, "generated")
	cfg.Providers = func(name string) (provider.Provider, error) {
		switch name {
		case system.Name:
			return r.p, nil
		case system.MediaName:
			return r.m, nil
		}
		return nil, fmt.Errorf("unknown provider %q", name)
	}
	cfg.Out, cfg.ErrOut = r.out, r.errOut
	cfg.EventSinks = []eventlog.Sink{r.rec}
	r.d = reconcile.NewDaemon(cfg)
	return r
}

func (r *rfRig) writeIntent(resources string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, "intent.json"), []byte(`{"resources": [`+resources+`]}`), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// cycle は 1 周回し、Apply を待ってから level の報告を閉じる。
func (r *rfRig) cycle() (reconcile.CycleResult, error) {
	r.t.Helper()
	res, err := r.d.RunCycle(context.Background())
	r.d.Wait(5 * time.Second)
	r.d.EndCycle(res, err)
	return res, err
}

func (r *rfRig) mustCycle() reconcile.CycleResult {
	r.t.Helper()
	res, err := r.cycle()
	if err != nil {
		r.t.Fatalf("RunCycle: %v\nerrOut:\n%s", err, r.errOut.String())
	}
	return res
}

// writes は node の偽 BMC が受けた GET 以外の要求（"METHOD body"）を返して記録を消す。
func (r *rfRig) writes(node string) []string {
	srv := r.bmcs[node]
	var out []string
	for _, q := range srv.Requests() {
		if q.Method != http.MethodGet {
			out = append(out, q.Method+" "+q.Body)
		}
	}
	srv.ClearRequests()
	return out
}

func rfResource(node, fields string) string {
	return `{"provider": "` + system.Name + `", "id": "` + node + `", "fields": {"name": "` + node + `", ` + fields + `}}`
}

func countContaining(xs []string, sub string) int {
	n := 0
	for _, x := range xs {
		if strings.Contains(x, sub) {
			n++
		}
	}
	return n
}

func TestRedfishSystem_ConvergesBootThenPower(t *testing.T) {
	// [AC-81-1]
	r := newRFRig(t, []string{"node1"}, redfishtest.Options{PowerState: "Off"},
		rfResource("node1", `"power": "On", "boot_enabled": "Continuous", "boot_target": "Pxe"`), reconcile.Config{})
	r.mustCycle()
	w := r.writes("node1")
	if len(w) != 2 || !strings.HasPrefix(w[0], "PATCH") || !strings.Contains(w[0], `"Pxe"`) || !strings.Contains(w[1], `"ResetType":"On"`) {
		t.Fatalf("[AC-81-1] cycle 1 writes = %v, want PATCH Boot (Continuous/Pxe) then Reset On", w)
	}
	if e, tg := r.bmcs["node1"].Boot(); r.bmcs["node1"].PowerState() != "On" || e != "Continuous" || tg != "Pxe" {
		t.Fatalf("[AC-81-1] BMC = %s %s/%s, want On Continuous/Pxe", r.bmcs["node1"].PowerState(), e, tg)
	}
	r.mustCycle()
	if w := r.writes("node1"); len(w) != 0 {
		t.Fatalf("[AC-81-1] cycle 2 writes = %v, want none (converged)", w)
	}
}

func TestRedfishSystem_RevertsHandChangedPower(t *testing.T) {
	// [AC-81-2]
	r := newRFRig(t, []string{"node1"}, redfishtest.Options{},
		rfResource("node1", `"power": "On"`), reconcile.Config{})
	r.mustCycle()
	if w := r.writes("node1"); len(w) != 0 {
		t.Fatalf("[AC-81-2] writes while converged = %v", w)
	}
	r.bmcs["node1"].SetPowerState("Off") // 人が BMC の画面で切った
	r.mustCycle()
	if w := r.writes("node1"); len(w) != 1 || !strings.Contains(w[0], `"ResetType":"On"`) {
		t.Fatalf("[AC-81-2] writes = %v, want Reset On", w)
	}
	if r.bmcs["node1"].PowerState() != "On" {
		t.Fatalf("[AC-81-2] power = %s, want On", r.bmcs["node1"].PowerState())
	}
}

func TestRedfishSystem_NoOpsWhilePoweringOff(t *testing.T) {
	// [AC-81-3]
	r := newRFRig(t, []string{"node1"}, redfishtest.Options{GracefulDelay: time.Minute},
		rfResource("node1", `"power": "Off"`), reconcile.Config{})
	r.mustCycle()
	if w := r.writes("node1"); len(w) != 1 || !strings.Contains(w[0], "GracefulShutdown") {
		t.Fatalf("[AC-81-3] cycle 1 writes = %v, want GracefulShutdown", w)
	}
	if _, ok := r.p.GracefulSentAt("node1"); !ok {
		t.Fatalf("[AC-81-3] the GracefulShutdown time is not recorded")
	}
	for i := 0; i < 3; i++ {
		r.clk.Advance(10 * time.Second)
		r.mustCycle()
		if w := r.writes("node1"); len(w) != 0 {
			t.Fatalf("[AC-81-3] writes while PoweringOff = %v, want none", w)
		}
	}
	r.clk.Advance(time.Minute)
	res := r.mustCycle()
	if w := r.writes("node1"); len(w) != 0 || res.Observed != 1 || r.bmcs["node1"].PowerState() != "Off" {
		t.Fatalf("[AC-81-3] after Off: writes=%v observed=%d power=%s", w, res.Observed, r.bmcs["node1"].PowerState())
	}
	if _, ok := r.p.GracefulSentAt("node1"); ok {
		t.Fatalf("[AC-81-3] the GracefulShutdown time is kept after Off was observed")
	}
}

func TestRedfishSystem_UnreachableIsUnknownAndNoOp(t *testing.T) {
	// [AC-81-4]
	r := newRFRig(t, []string{"node1"}, redfishtest.Options{PowerState: "Off"},
		rfResource("node1", `"power": "On"`), reconcile.Config{})
	r.bmcs["node1"].SetUnreachable(true)
	r.mustCycle()
	if w := r.writes("node1"); len(w) != 0 {
		t.Fatalf("[AC-81-4] writes while unreachable = %v, want none", w)
	}
	if !strings.Contains(r.errOut.String(), "observe "+system.Name+"//node1") {
		t.Fatalf("[AC-81-4] no unobservable report:\n%s", r.errOut.String())
	}
	r.bmcs["node1"].SetUnreachable(false)
	r.bmcs["node1"].SetFailStatus(http.StatusInternalServerError)
	r.mustCycle()
	if w := r.writes("node1"); len(w) != 0 || r.bmcs["node1"].PowerState() != "Off" {
		t.Fatalf("[AC-81-4] writes on 5xx = %v (power %s), want none", w, r.bmcs["node1"].PowerState())
	}
	r.bmcs["node1"].SetFailStatus(0)
	r.mustCycle()
	if r.bmcs["node1"].PowerState() != "On" {
		t.Fatalf("[AC-81-4] not converged once reachable again")
	}
}

func TestRedfishSystem_OnceIsAPlanError(t *testing.T) {
	// [AC-81-5]
	r := newRFRig(t, []string{"node1"}, redfishtest.Options{},
		rfResource("node1", `"boot_enabled": "Once", "boot_target": "Pxe"`), reconcile.Config{})
	_, err := r.cycle()
	if err == nil || !strings.Contains(err.Error(), `boot_enabled "Once" cannot be declared`) || !strings.Contains(err.Error(), "op: boot_once") {
		t.Fatalf("[AC-81-5] RunCycle err = %v, want the Once error that says why and points to the Workflow action boot_once (#139)", err)
	}
	if q := r.bmcs["node1"].Requests(); len(q) != 0 {
		t.Fatalf("[AC-81-5] the BMC got %d request(s); the plan should stop before Observe", len(q))
	}
}

func TestRedfishSystem_GracefulNeverForcesAndGoesStuck(t *testing.T) {
	// [AC-81-6]
	r := newRFRig(t, []string{"node1"}, redfishtest.Options{GracefulTimeout: 30 * time.Second},
		rfResource("node1", `"power": "Off"`), reconcile.Config{}) // StuckAfter: default (5)
	r.bmcs["node1"].SetIgnoreGraceful(true) // OS が ACPI に応えない
	var all []string
	const cycles = eventlog.DefaultStuckAfter + 2
	for i := 0; i < cycles; i++ {
		r.mustCycle()
		all = append(all, r.writes("node1")...)
		r.clk.Advance(time.Minute) // GRACEFUL_TIMEOUT を過ぎて On に戻る
	}
	if n := countContaining(all, "ForceOff"); n != 0 {
		t.Fatalf("[AC-81-6] ForceOff sent %d time(s) under the default graceful: %v", n, all)
	}
	if n := countContaining(all, "GracefulShutdown"); n != cycles {
		t.Fatalf("[AC-81-6] GracefulShutdown sent %d time(s) in %d cycles, want every cycle: %v", n, cycles, all)
	}
	r.rec.mu.Lock()
	evs := append([]core.Event(nil), r.rec.evs...)
	r.rec.mu.Unlock()
	var stuck []core.Event
	for _, ev := range evs {
		if ev.State == core.EventFiring && ev.Class == core.EventStuck && ev.Resource.Local == "node1" {
			stuck = append(stuck, ev)
		}
	}
	if len(stuck) == 0 || stuck[0].Count != eventlog.DefaultStuckAfter || stuck[0].Severity != core.SeverityCritical {
		t.Fatalf("[AC-81-6] stuck firing = %+v, want one at count %d (critical); events: %+v",
			stuck, eventlog.DefaultStuckAfter, evs)
	}
}

func TestRedfishSystem_GracefulThenForceAfterForceAfter(t *testing.T) {
	// [AC-81-7]
	r := newRFRig(t, []string{"node1"}, redfishtest.Options{GracefulTimeout: 10 * time.Second},
		rfResource("node1", `"power": "Off", "power_off": "graceful_then_force", "force_after": "5m"`), reconcile.Config{})
	r.bmcs["node1"].SetIgnoreGraceful(true)
	r.mustCycle()
	if w := r.writes("node1"); len(w) != 1 || !strings.Contains(w[0], "GracefulShutdown") {
		t.Fatalf("[AC-81-7] cycle 1 writes = %v, want GracefulShutdown first", w)
	}
	r.clk.Advance(2 * time.Minute) // On に戻った、force_after 前
	r.mustCycle()
	if w := r.writes("node1"); len(w) != 1 || !strings.Contains(w[0], "GracefulShutdown") {
		t.Fatalf("[AC-81-7] before force_after writes = %v, want GracefulShutdown again", w)
	}
	r.clk.Advance(4 * time.Minute) // 最初の GracefulShutdown から 6 分
	// 計画の時点で、この停止は破壊的な ForceOff になっている。
	obs, err := r.p.Read(context.Background(), "node1")
	if err != nil {
		t.Fatal(err)
	}
	pl, err := diff.ForProvider(r.p).Plan(obs, diff.DesiredResource{
		ID:     core.NewID(system.Name, core.NewScope(nil), "node1"),
		Fields: map[string]string{"name": "node1", "power": "Off", "power_off": "graceful_then_force", "force_after": "5m"},
	}, cause.CauseContext{})
	if err != nil || len(pl.Ops) != 1 || !pl.Ops[0].Destructive || !strings.Contains(pl.Ops[0].Diagnostic, "will ForceOff (destructive)") {
		t.Fatalf("[AC-81-7] plan after force_after = %+v, %v; want one destructive ForceOff op", pl, err)
	}
	r.mustCycle()
	if w := r.writes("node1"); len(w) != 1 || !strings.Contains(w[0], `"ResetType":"ForceOff"`) {
		t.Fatalf("[AC-81-7] after force_after writes = %v, want ForceOff", w)
	}
	r.mustCycle()
	if _, ok := r.p.GracefulSentAt("node1"); ok || r.bmcs["node1"].PowerState() != "Off" {
		t.Fatalf("[AC-81-7] after ForceOff: power %s, time kept %v", r.bmcs["node1"].PowerState(), ok)
	}
}

func TestRedfishSystem_ForceOffHeldByBudget(t *testing.T) {
	// [AC-81-8]
	fields := `"power": "Off", "power_off": "graceful_then_force", "force_after": "5m"`
	r := newRFRig(t, []string{"node1", "node2"}, redfishtest.Options{GracefulTimeout: 10 * time.Second},
		rfResource("node1", fields)+", "+rfResource("node2", fields),
		reconcile.Config{BudgetConfig: budget.Config{MaxDestructivePerCycle: 1}})
	for _, s := range r.bmcs {
		s.SetIgnoreGraceful(true)
	}
	// GracefulShutdown 2 つは破壊的に数えない（予算 1 で止まらない）。
	res := r.mustCycle()
	if res.BudgetHalted {
		t.Fatalf("[AC-81-8] two GracefulShutdowns halted the destructive budget: %s", res.BudgetReason)
	}
	for _, n := range []string{"node1", "node2"} {
		if w := r.writes(n); len(w) != 1 || !strings.Contains(w[0], "GracefulShutdown") {
			t.Fatalf("[AC-81-8] %s writes = %v, want GracefulShutdown", n, w)
		}
	}
	// force_after を過ぎると 2 つの ForceOff は予算 1 を超え、どちらも送らない。
	r.clk.Advance(6 * time.Minute)
	for i := 0; i < 2; i++ {
		res = r.mustCycle()
		if !res.BudgetHalted || !strings.Contains(res.BudgetReason, "max_destructive_per_cycle=1") {
			t.Fatalf("[AC-81-8] cycle with 2 ForceOffs: halted=%v reason=%q", res.BudgetHalted, res.BudgetReason)
		}
		for _, n := range []string{"node1", "node2"} {
			if w := r.writes(n); len(w) != 0 || r.bmcs[n].PowerState() != "On" {
				t.Fatalf("[AC-81-8] %s writes = %v (power %s), want held", n, w, r.bmcs[n].PowerState())
			}
		}
	}
}
