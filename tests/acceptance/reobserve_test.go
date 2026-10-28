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

// reobserve_test.go — #177: 観測は周の頭で Apply の前にまとめて行うので、FW の
// 許可（依存先）と、その許可を通って初めて届くホストの上のリソース（依存元）を
// 同じ回で組むと、依存元は観測できず skip され、一回性の reconcile は 1 で終わって
// いた。依存先の Apply が効いたあと、観測できなかった依存元だけを同じ回で観測し
// 直す（§8.1）。
//
//	[AC-177-1]  一回性: 依存先の Apply のあとにだけ届く依存元（fw → host → svc の
//	            鎖）を含む構成が、空から 1 回で収束する（終了コード 0）。
//	            --reobserve-max 0 ならいまどおり 1 で、依存元は作られない。
//	[AC-177-2]  1 つのリソースを観測し直す回数の上限に当たれば、いまどおり
//	            unknown で skip（終了コード 1）。次の回に任せる。上限の中で
//	            届けば（既定 3 回）1 回で収束する。
//	[AC-177-3]  回の全体で観測し直しに使う時間の上限に当たっても同じ。
//	[AC-177-4]  観測し直しても届かなければ、いまどおり unknown で skip（1）。
//	[AC-177-5]  観測し直して届いた依存元の変更も予算を通る（超えれば BUDGET
//	            HALTED で 1、Apply しない）。
//	[AC-177-6]  依存先の Apply が効かなかった（#166）ときは観測し直さない。
//	[AC-177-7]  常駐: 依存先の Apply が効いたあとに依存元が届けば、Interval を
//	            待たずに次の周を始め、鎖を続けて収束させる。
package acceptance

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/fakeprovider"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
)

// reobserveRig is fw → host → svc against the fake provider, where host is
// reached only once fw exists and svc only once host exists.
type reobserveRig struct {
	intent, state string
}

func newReobserveRig(t *testing.T, extra map[string]any) *reobserveRig {
	t.Helper()
	tmp := t.TempDir()
	r := &reobserveRig{intent: filepath.Join(tmp, "intent"), state: filepath.Join(tmp, "state.json")}
	if err := os.MkdirAll(r.intent, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIntentFile(t, r.intent, map[string]any{"resources": []map[string]any{
		{"provider": "fake", "id": "fw", "fields": map[string]string{"name": "fw", "size": "1"}},
		{"provider": "fake", "id": "host", "fields": map[string]string{"name": "host", "size": "2"}, "depends_on": []string{"fw"}},
		{"provider": "fake", "id": "svc", "fields": map[string]string{"name": "svc", "size": "3"}, "depends_on": []string{"host"}},
	}})
	st := map[string]any{
		"resources":        map[string]any{},
		"provider_version": "v1.0.0",
		"observe_after":    map[string]string{"host": "fw", "svc": "host"},
	}
	for k, v := range extra {
		st[k] = v
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(r.state, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *reobserveRig) reconcile(t *testing.T, args ...string) (string, int) {
	t.Helper()
	return runSakutei(t, append([]string{"reconcile", "--intent", r.intent, "--state", r.state}, args...)...)
}

// exists reports which of the locals exist in the fake's reality.
func (r *reobserveRig) exists(t *testing.T) map[string]bool {
	t.Helper()
	var sf struct {
		Resources map[string]any `json:"resources"`
	}
	if err := json.Unmarshal([]byte(readFile(t, r.state)), &sf); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for k := range sf.Resources {
		out[k] = true
	}
	return out
}

func TestReobserve_OneShotConvergesFromEmpty(t *testing.T) {
	// [AC-177-1]
	r := newReobserveRig(t, nil)
	out, code := r.reconcile(t)
	if code != 0 {
		t.Fatalf("[AC-177-1] exit %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "Reconcile: 3 change(s) applied") {
		t.Errorf("[AC-177-1] the summary must say 3 applied:\n%s", out)
	}
	for _, id := range []string{"host", "svc"} {
		if !strings.Contains(out, "reobserve fake//"+id+": observed after its upstream's Apply; planned in this run") {
			t.Errorf("[AC-177-1] no re-observation line for %s:\n%s", id, out)
		}
	}
	if ex := r.exists(t); !ex["fw"] || !ex["host"] || !ex["svc"] {
		t.Fatalf("[AC-177-1] reality after one run = %v, want fw, host, svc", ex)
	}
	// Wave order holds: each Applied once.
	for _, id := range []string{"fw", "host", "svc"} {
		if n := applyCount(t, r.state, id); n != 1 {
			t.Errorf("[AC-177-1] apply_count[%s] = %d, want 1", id, n)
		}
	}
	out, code = r.reconcile(t)
	if code != 0 || !strings.Contains(out, "Converged (no changes)") {
		t.Fatalf("[AC-177-1] the next run: exit %d, want 0 and converged:\n%s", code, out)
	}

	// host already exists (behind fw): observed again with no diff, it still
	// lets svc behind it be observed again in the same run.
	r3 := newReobserveRig(t, map[string]any{"resources": map[string]any{
		"host": map[string]any{"fields": map[string]string{"name": "host", "size": "2"}},
	}})
	out, code = r3.reconcile(t)
	if code != 0 || !strings.Contains(out, "Reconcile: 2 change(s) applied") {
		t.Fatalf("[AC-177-1] host already there: exit %d, want 0 and 2 applied:\n%s", code, out)
	}
	if ex := r3.exists(t); !ex["fw"] || !ex["svc"] {
		t.Fatalf("[AC-177-1] host already there: reality = %v, want fw and svc", ex)
	}

	// Without the re-observation the run ends as before: 1, host not built.
	r2 := newReobserveRig(t, nil)
	out, code = r2.reconcile(t, "--reobserve-max", "0")
	if code != 1 {
		t.Fatalf("[AC-177-1] --reobserve-max 0: exit %d, want 1:\n%s", code, out)
	}
	if ex := r2.exists(t); !ex["fw"] || ex["host"] || ex["svc"] {
		t.Fatalf("[AC-177-1] --reobserve-max 0: reality = %v, want only fw", ex)
	}
	if strings.Contains(out, "reobserve") {
		t.Errorf("[AC-177-1] --reobserve-max 0 must not observe again:\n%s", out)
	}
}

func TestReobserve_OneShotPerResourceLimit(t *testing.T) {
	// [AC-177-2] host still answers unknown to its first 2 Reads once fw exists.
	r := newReobserveRig(t, map[string]any{"observe_after_reads": map[string]int{"host": 2}})
	out, code := r.reconcile(t, "--reobserve-max", "2")
	if code != 1 {
		t.Fatalf("[AC-177-2] exit %d, want 1:\n%s", code, out)
	}
	if !strings.Contains(out, "reobserve fake//host: still unobservable after its upstream's Apply") ||
		!strings.Contains(out, "re-observation limit reached (2 per resource)") {
		t.Errorf("[AC-177-2] no limit line:\n%s", out)
	}
	if ex := r.exists(t); !ex["fw"] || ex["host"] || ex["svc"] {
		t.Fatalf("[AC-177-2] reality = %v, want only fw", ex)
	}
	// The default limit gives host its third Read: the same setup converges
	// in one run.
	r2 := newReobserveRig(t, map[string]any{"observe_after_reads": map[string]int{"host": 2}})
	out, code = r2.reconcile(t)
	if code != 0 {
		t.Fatalf("[AC-177-2] default limit %d: exit %d, want 0:\n%s", reconcile.DefaultReobserveMax, code, out)
	}
	if ex := r2.exists(t); !ex["host"] || !ex["svc"] {
		t.Fatalf("[AC-177-2] default limit: reality = %v, want host and svc", ex)
	}
}

func TestReobserve_OneShotTimeLimit(t *testing.T) {
	// [AC-177-3] each Read of host takes 2s; the run may spend 300ms.
	r := newReobserveRig(t, map[string]any{"observe_delay": map[string]string{"host": "2s"}})
	start := time.Now()
	out, code := r.reconcile(t, "--reobserve-time", "300ms")
	if code != 1 {
		t.Fatalf("[AC-177-3] exit %d, want 1:\n%s", code, out)
	}
	if !strings.Contains(out, "re-observation time limit reached (300ms per run)") {
		t.Errorf("[AC-177-3] no time limit line:\n%s", out)
	}
	// The head-of-run Read (2s) plus at most the 300ms of re-observation.
	if el := time.Since(start); el > 2*time.Second+2*time.Second {
		t.Errorf("[AC-177-3] the run took %s; the re-observation must stop at its time limit", el)
	}
	if ex := r.exists(t); !ex["fw"] || ex["host"] {
		t.Fatalf("[AC-177-3] reality = %v, want only fw", ex)
	}
}

func TestReobserve_OneShotStillUnreachable(t *testing.T) {
	// [AC-177-4] host never answers.
	r := newReobserveRig(t, map[string]any{"observe_unknown": map[string]string{"host": "unknown"}})
	out, code := r.reconcile(t)
	if code != 1 {
		t.Fatalf("[AC-177-4] exit %d, want 1:\n%s", code, out)
	}
	if !strings.Contains(out, "reobserve fake//host: still unobservable after its upstream's Apply (availability=unknown") {
		t.Errorf("[AC-177-4] no still-unobservable line:\n%s", out)
	}
	if !strings.Contains(out, "resource(s) were unobservable this cycle; convergence incomplete") {
		t.Errorf("[AC-177-4] the run must still say it is incomplete:\n%s", out)
	}
	if ex := r.exists(t); !ex["fw"] || ex["host"] || ex["svc"] {
		t.Fatalf("[AC-177-4] reality = %v, want only fw", ex)
	}
}

func TestReobserve_OneShotBudget(t *testing.T) {
	// [AC-177-5] 1 change per run: fw takes it, host is halted.
	r := newReobserveRig(t, nil)
	out, code := r.reconcile(t, "--max-changes-per-cycle", "1")
	if code != 1 {
		t.Fatalf("[AC-177-5] exit %d, want 1:\n%s", code, out)
	}
	if !strings.Contains(out, "BUDGET HALTED") || !strings.Contains(out, "exceeds max_changes_per_cycle_absolute=1") {
		t.Errorf("[AC-177-5] no budget halt:\n%s", out)
	}
	if ex := r.exists(t); !ex["fw"] || ex["host"] {
		t.Fatalf("[AC-177-5] reality = %v, want only fw", ex)
	}
}

func TestReobserve_OneShotIneffectiveUpstream(t *testing.T) {
	// [AC-177-6] fw's Apply reports success and does not take.
	r := newReobserveRig(t, map[string]any{"apply_ignore": map[string]bool{"fw": true}})
	out, code := r.reconcile(t)
	if code != 1 {
		t.Fatalf("[AC-177-6] exit %d, want 1:\n%s", code, out)
	}
	if strings.Contains(out, "reobserve") {
		t.Errorf("[AC-177-6] nothing is observed again behind an Apply that did not take:\n%s", out)
	}
	if !strings.Contains(out, "ApplyIneffective") {
		t.Errorf("[AC-177-6] the ineffective Apply must be reported:\n%s", out)
	}
}

func TestReobserve_DaemonStartsNextCycle(t *testing.T) {
	// [AC-177-7] Interval is an hour: only the wake moves the chain on.
	r := newReobserveRig(t, nil)
	var out, errOut syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- reconcile.Run(ctx, reconcile.Config{
			IntentDir:    r.intent,
			GeneratedDir: filepath.Join(filepath.Dir(r.intent), "generated"),
			Provider:     fakeprovider.New(r.state),
			Interval:     time.Hour,
			Out:          &out,
			ErrOut:       &errOut,
		})
	}()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if ex := r.exists(t); ex["fw"] && ex["host"] && ex["svc"] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("[AC-177-7] the chain did not converge without waiting out Interval; reality %v\nout:\n%s\nerr:\n%s",
				r.exists(t), out.String(), errOut.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, line := range []string{
		"reobserve fake//host: observed after fake//fw's Apply; the next cycle starts now",
		"reobserve fake//svc: observed after fake//host's Apply; the next cycle starts now",
	} {
		if !strings.Contains(errOut.String(), line) {
			t.Errorf("[AC-177-7] no %q:\n%s", line, errOut.String())
		}
	}
}
