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

// event_log_test.go — #77: 知らせは単一の core.Event から出し（§22.3, §27.2）、
// --log-format json なら 1 Event 1 行の JSON lines で出す。
//
//	[AC-77-1]  --log-format json のデーモンで、ずれ（Drift・DriftWouldApply）が
//	           kind・resource・cause・revision を持つ 1 行 1 Event の JSON で出る。
//	           出力のどの行も JSON（周の要約は kind=CycleSummary の同じ形の行）。
//	[AC-77-2]  同じく prune（Pruned）・予算の halt（BudgetHalted）・prune の急減
//	           halt（PruneHalted）が JSON の Event で出る。
//	[AC-77-3]  text（既定）では従来と同じ文で出る。
//	[AC-77-4]  secret の値は Event に出ない（SecretRef のまま、§20.2）。
//	[AC-77-5]  知らせを fmt.Fprintf で直接書く箇所が cmd/・internal/ に残っていない。
package acceptance

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/eventlog"
	"github.com/tjst-t/sakutei/internal/lastseen"
	"github.com/tjst-t/sakutei/internal/reconcile"
	"github.com/tjst-t/sakutei/pkg/core"
)

// jsonEvent is one line of --log-format json.
type jsonEvent struct {
	Time             string            `json:"time"`
	Kind             string            `json:"kind"`
	Severity         string            `json:"severity"`
	Cycle            int               `json:"cycle"`
	Resource         string            `json:"resource"`
	Scope            string            `json:"scope"`
	IntentRevision   string            `json:"intent_revision"`
	RenderedRevision string            `json:"rendered_revision"`
	Cause            string            `json:"cause"`
	Message          string            `json:"message"`
	Attrs            map[string]string `json:"attrs"`
}

// parseJSONLines parses every line of log as a jsonEvent; a line that is not
// JSON fails the test.
func parseJSONLines(t *testing.T, log string) []jsonEvent {
	t.Helper()
	var evs []jsonEvent
	for i, line := range strings.Split(log, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev jsonEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line %d is not JSON (%v): %q\nlog:\n%s", i+1, err, line, log)
		}
		if ev.Kind == "" || ev.Time == "" || ev.Severity == "" {
			t.Fatalf("line %d lacks kind/time/severity: %q", i+1, line)
		}
		evs = append(evs, ev)
	}
	return evs
}

func eventsOfKind(evs []jsonEvent, kind string) []jsonEvent {
	var out []jsonEvent
	for _, ev := range evs {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// runDriftedAlertOnlyDaemon runs the daemon in alert-only on a resource whose
// reality drifted (size 30, desired 10) until it reaches cycle 2, and returns
// its merged log.
func runDriftedAlertOnlyDaemon(t *testing.T, format string) string {
	t.Helper()
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	logPath := filepath.Join(tmp, "daemon.log")
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{"provider": "fake", "id": "db-vm", "fields": map[string]string{"name": "db-vm", "size": "10"}},
		},
	})
	seedStateAtomic(t, statePath, map[string]map[string]any{
		"db-vm": {"name": "db-vm", "size": "30"},
	}, "v1.0.0")

	flags := []string{"--intent", intentDir, "--state", statePath, "--interval", "50ms", "--mode", "alert-only"}
	wait := "cycle 2:"
	if format != "" {
		flags = append(flags, "--log-format", format)
		if format == "json" {
			wait = `"cycle":2,`
		}
	}
	cmd, waitCmd := startDaemonWithFlags(t, logPath, flags...)
	ok := waitForLogFile(logPath, wait, 10*time.Second)
	_ = cmd.Process.Kill()
	_ = waitCmd()
	log := readFileString(logPath)
	if !ok {
		t.Fatalf("daemon did not reach cycle 2; log:\n%s", log)
	}
	return log
}

func TestEventLog_JSON_Drift(t *testing.T) {
	// [AC-77-1]
	evs := parseJSONLines(t, runDriftedAlertOnlyDaemon(t, "json"))

	drift := eventsOfKind(evs, string(core.EventDrift))
	if len(drift) == 0 {
		t.Fatalf("[AC-77-1] no Drift Event; events: %+v", evs)
	}
	for _, ev := range drift {
		if !strings.HasSuffix(ev.Resource, "/db-vm") || ev.Cause != "B" || ev.Cycle < 1 ||
			ev.IntentRevision == "" || ev.RenderedRevision == "" || ev.Message == "" {
			t.Errorf("[AC-77-1] Drift Event lacks resource/cause/cycle/revision/message: %+v", ev)
		}
	}
	would := eventsOfKind(evs, string(core.EventDriftWouldApply))
	if len(would) == 0 {
		t.Fatalf("[AC-77-1] no DriftWouldApply Event (alert-only); events: %+v", evs)
	}
	for _, ev := range would {
		if !strings.HasSuffix(ev.Resource, "/db-vm") || ev.Cause != "B" || ev.Cycle < 1 ||
			ev.IntentRevision == "" || ev.RenderedRevision == "" ||
			!strings.Contains(ev.Message, "would be UPDATE") || ev.Attrs["op"] != "UPDATE" {
			t.Errorf("[AC-77-1] DriftWouldApply Event lacks resource/cause/cycle/revision/op: %+v", ev)
		}
	}
	sums := eventsOfKind(evs, string(core.EventCycleSummary))
	found := false
	for _, ev := range sums {
		if ev.Cycle == 1 && strings.HasPrefix(ev.Message, "observed=1 drifted (alert-only") {
			found = true
		}
	}
	if !found {
		t.Errorf("[AC-77-1] no CycleSummary line for cycle 1; summaries: %+v", sums)
	}
}

func TestEventLog_Text_Unchanged(t *testing.T) {
	// [AC-77-3] default and explicit text are the sentences of before.
	for _, format := range []string{"", "text"} {
		log := runDriftedAlertOnlyDaemon(t, format)
		for _, want := range []string{
			"sakutei: DriftEvent: fake//db-vm [cause: B] ",
			"sakutei: cycle 1: observed=1 drifted (alert-only, no changes)\n",
			"sakutei: cycle 1: alert-only: 1 drifted resource(s) detected — no changes applied\n",
			"sakutei: cycle 1: DRIFT EVENT: would be UPDATE fake//db-vm\n",
		} {
			if !strings.Contains(log, want) {
				t.Errorf("[AC-77-3] format %q: log lacks %q; log:\n%s", format, want, log)
			}
		}
		if strings.Contains(log, `{"time"`) {
			t.Errorf("[AC-77-3] format %q: text log carries JSON; log:\n%s", format, log)
		}
	}
}

func TestEventLog_JSON_PrunedAndHalts(t *testing.T) {
	// [AC-77-2] Pruned: 2 orphans of 3 are deleted.
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	logPath := filepath.Join(tmp, "daemon.log")
	seedManyPruneableResources(t, statePath, 3)
	writeDesiredKeepOneResource(t, intentDir, "vm-01")

	cmd, waitCmd := startDaemonWithFlags(t, logPath,
		"--intent", intentDir, "--state", statePath, "--interval", "50ms", "--log-format", "json")
	ok := waitForLogFile(logPath, `"cycle":2,`, 10*time.Second)
	_ = cmd.Process.Kill()
	_ = waitCmd()
	log := readFileString(logPath)
	if !ok {
		t.Fatalf("[AC-77-2] daemon did not reach cycle 2; log:\n%s", log)
	}
	pruned := map[string]bool{}
	for _, ev := range eventsOfKind(parseJSONLines(t, log), string(core.EventPruned)) {
		if ev.Cycle != 1 || ev.IntentRevision == "" || ev.RenderedRevision == "" {
			t.Errorf("[AC-77-2] Pruned Event lacks cycle/revision: %+v", ev)
		}
		pruned[ev.Resource] = true
	}
	if !pruned["fake//vm-02"] || !pruned["fake//vm-03"] || len(pruned) != 2 {
		t.Errorf("[AC-77-2] Pruned Events for %v, want fake//vm-02 and fake//vm-03; log:\n%s", pruned, log)
	}

	// [AC-77-2] BudgetHalted, then PruneHalted on a List drop (the #75 scenario).
	tmp = t.TempDir()
	intentDir = filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath = filepath.Join(tmp, "state.json")
	logPath = filepath.Join(tmp, "daemon.log")
	seedManyPruneableResources(t, statePath, 3)
	writeDesiredKeepOneResource(t, intentDir, "vm-01")

	cmd, waitCmd = startDaemonWithFlags(t, logPath,
		"--intent", intentDir, "--state", statePath, "--interval", "50ms",
		"--max-destructive-per-cycle", "1", "--log-format", "json")
	killed := false
	defer func() {
		if !killed {
			_ = cmd.Process.Kill()
			_ = waitCmd()
		}
	}()
	if !waitForLogFile(logPath, `"kind":"BudgetHalted"`, 10*time.Second) {
		t.Fatalf("[AC-77-2] no BudgetHalted Event; log:\n%s", readFileString(logPath))
	}
	seedManyPruneableResources(t, statePath, 1)
	if !waitForLogFile(logPath, `"kind":"PruneHalted"`, 10*time.Second) {
		t.Fatalf("[AC-77-2] no PruneHalted Event; log:\n%s", readFileString(logPath))
	}
	_ = cmd.Process.Kill()
	_ = waitCmd()
	killed = true

	evs := parseJSONLines(t, readFileString(logPath))
	for _, ev := range eventsOfKind(evs, string(core.EventBudgetHalted)) {
		if ev.Cycle < 1 || ev.IntentRevision == "" || ev.RenderedRevision == "" || ev.Message == "" {
			t.Errorf("[AC-77-2] BudgetHalted Event lacks cycle/revision/reason: %+v", ev)
		}
	}
	for _, ev := range eventsOfKind(evs, string(core.EventPruneHalted)) {
		if ev.Cycle < 1 || ev.IntentRevision == "" || ev.RenderedRevision == "" ||
			ev.Attrs["guard"] != "count_drop" || !strings.Contains(ev.Message, "List count dropped from 3 to 1") {
			t.Errorf("[AC-77-2] PruneHalted Event lacks cycle/revision/guard/reason: %+v", ev)
		}
	}
}

func TestEventLog_JSON_ReconcileAndPlan(t *testing.T) {
	// [AC-77-1] reconcile and plan take --log-format json too: every line is
	// JSON, and reconcile's drift is a Drift Event with its revision.
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{"provider": "fake", "id": "db-vm", "fields": map[string]string{"name": "db-vm", "size": "10"}},
		},
	})
	seedStateAtomic(t, statePath, map[string]map[string]any{
		"db-vm": {"name": "db-vm", "size": "30"},
	}, "v1.0.0")

	planOut, _, code := runPlan(t, intentDir, statePath, "--log-format", "json")
	if code != 0 {
		t.Fatalf("[AC-77-1] plan exit %d; out:\n%s", code, planOut)
	}
	parseJSONLines(t, planOut)

	out, _, code := runReconcileCombined(t, intentDir, statePath, "--log-format", "json")
	if code != 0 {
		t.Fatalf("[AC-77-1] reconcile exit %d; out:\n%s", code, out)
	}
	evs := parseJSONLines(t, out)
	drift := eventsOfKind(evs, string(core.EventDrift))
	if len(drift) != 1 || drift[0].Resource != "fake//db-vm" || drift[0].Cause != "B" ||
		drift[0].IntentRevision == "" || drift[0].RenderedRevision == "" {
		t.Errorf("[AC-77-1] reconcile Drift Events %+v, want one for fake//db-vm with cause B and revision", drift)
	}
	if len(eventsOfKind(evs, string(core.EventCycleSummary))) == 0 {
		t.Errorf("[AC-77-1] reconcile has no result line of kind CycleSummary: %+v", evs)
	}

	// An unknown form is a usage error.
	if _, _, code := runReconcileCombined(t, intentDir, statePath, "--log-format", "yaml"); code == 0 {
		t.Errorf("[AC-77-1] --log-format yaml must be rejected")
	}
}

func TestEventLog_SecretNotInEvent(t *testing.T) {
	// [AC-77-4] A provider that (wrongly) reports a secret field's plaintext:
	// the drift Event quotes only the masked form.
	eng := diff.New(secretSchema())
	id := core.NewID("fake", core.NewScope(nil), "res1")
	ring := lastseen.NewResourceRing(4)
	prev := eng.SnapshotFields(core.State{
		"name":     {Kind: core.FieldKindString, Value: "res1"},
		"password": {Kind: core.FieldKindSecretRef, Value: core.NewSecretRef("file", "db/root", 3)},
	})
	ring.Record("res1", lastseen.Snapshot{LocalID: "res1", ObservedAt: time.Now(), Fields: prev})
	cur := eng.SnapshotFields(core.State{
		"name":     {Kind: core.FieldKindString, Value: "res1"},
		"password": {Kind: core.FieldKindString, Value: theSecret},
	})
	summary := ring.DriftSummary("res1", cur)

	for _, format := range []eventlog.Format{eventlog.FormatJSON, eventlog.FormatText} {
		var buf bytes.Buffer
		w := eventlog.New(&buf, format, core.SeverityWarning)
		reconcile.EmitDrift(w, "sakutei: ", id, core.Op{Kind: core.OpKindUpdate, Target: id, Cause: core.CauseTagB}, summary, cur)
		out := buf.String()
		if strings.Contains(out, theSecret) {
			t.Fatalf("[AC-77-4] %s: the secret value leaked into the Event: %s", format, out)
		}
		if !strings.Contains(out, "secretref:file/db/root#gen3") {
			t.Errorf("[AC-77-4] %s: the Event should quote the SecretRef form: %s", format, out)
		}
	}
}

// noticeMarkers are the words of the notices (§22.3) that must come from a
// core.Event, never from a direct fmt print.
var noticeMarkers = []string{
	"EVENT ", "DriftEvent", "DRIFT EVENT", "HALT", "pruned ", "ProviderUpgradeDiff",
	"RequiresTaskEvent", "SKIPPED", "FENCED", "apply %s:", "CapacityThresholdExceeded",
}

func TestEventLog_NoDirectNoticePrint(t *testing.T) {
	// [AC-77-5] Scan every fmt.Print*/Fprint* call of the non-test sources
	// of cmd/ and internal/ for a notice written directly.
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var bad []string
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.Walk(filepath.Join(root, dir), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "fmt" || !strings.Contains(sel.Sel.Name, "rint") ||
					strings.HasPrefix(sel.Sel.Name, "S") {
					return true
				}
				for _, arg := range call.Args {
					lit, ok := arg.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					s, _ := strconv.Unquote(lit.Value)
					for _, m := range noticeMarkers {
						if strings.Contains(s, m) {
							bad = append(bad, fset.Position(call.Pos()).String()+": "+s)
						}
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(bad) > 0 {
		t.Errorf("[AC-77-5] notices printed directly instead of through core.Event:\n%s", strings.Join(bad, "\n"))
	}
}
