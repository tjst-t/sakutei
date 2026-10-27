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

// read_api_test.go — #136（外の道具が Sakutei の状態を読むための読み取り専用の口）:
//
//	[AC-136-1] デーモンは --read-listen を明示したときだけ待ち受け、GET /status・/plan は
//	           一回性の "status --json"・"plan --json" と同じ形（同じキー・同じリソースの
//	           状態・同じ操作）に周の番号と時刻を足して返す。止めた操作は理由つき
//	[AC-136-2] GET /events はずれの firing と resolved を Server-Sent Events で流す。接続
//	           した時点で firing 中の level の Event を先に送る
//	[AC-136-3] GET /graph は依存の順（wave）と ready_condition・今の ready を返し、
//	           "sakutei graph --json" も同じ順を返す
//	[AC-136-4] 書き込みの要求（POST・PUT・PATCH・DELETE）は 405 で断る
//	[AC-136-5] status（JSON と文）と GET /status に claim・shard・fencing の世代が出る
//	[AC-136-6] git の配布のときは、ミラーの状態（最後に取れた revision と時刻、届いて
//	           いるか）が出る
package acceptance

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

var readListenRe = regexp.MustCompile(`read-only API listening on (http://\S+) `)

// readAPIURL waits for the daemon's listening line and returns the base URL.
func readAPIURL(t *testing.T, out, errOut *syncBuffer) string {
	t.Helper()
	var url string
	if !waitForCondition(10*time.Second, func() bool {
		if m := readListenRe.FindStringSubmatch(out.String()); m != nil {
			url = m[1]
			return true
		}
		return false
	}) {
		t.Fatalf("no read-only API listening line:\n%s\n%s", out.String(), errOut.String())
	}
	return url
}

// getJSON GETs url and decodes the body; ok is false unless the status is 200.
func getJSON(url string) (map[string]any, bool) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return nil, false
	}
	return m, true
}

// waitJSON polls url until ok(m) holds.
func waitJSON(t *testing.T, url string, ok func(map[string]any) bool, logs ...*syncBuffer) map[string]any {
	t.Helper()
	var last map[string]any
	if !waitForCondition(15*time.Second, func() bool {
		m, got := getJSON(url)
		if got {
			last = m
		}
		return got && ok(m)
	}) {
		b, _ := json.MarshalIndent(last, "", "  ")
		var l strings.Builder
		for _, s := range logs {
			l.WriteString(s.String())
		}
		t.Fatalf("GET %s never satisfied the condition; last:\n%s\nlogs:\n%s", url, b, l.String())
	}
	return last
}

func jsonKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// missingKeys returns the keys of want that got lacks.
func missingKeys(want, got map[string]any) []string {
	var out []string
	for k := range want {
		if _, ok := got[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func listOf(m map[string]any, key string) []map[string]any {
	raw, _ := m[key].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if e, ok := r.(map[string]any); ok {
			out = append(out, e)
		}
	}
	return out
}

func runJSONCmd(t *testing.T, args ...string) map[string]any {
	t.Helper()
	cmd := exec.Command(buildBinary(t), args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, _ := cmd.Output()
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("sakutei %v: not JSON: %v\n%s\n%s", args, err, out, stderr.String())
	}
	return m
}

// [AC-136-1] [AC-136-4]
func TestReadAPI_StatusAndPlanMatchTheCLI(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	writeIntentFile(t, intentDir, map[string]any{"resources": []any{
		fakeRes("in-sync", "1"), fakeRes("drifted", "2"), fakeRes("absent", "3"), fakeRes("unreachable", "4"),
	}})
	seedState(t, statePath, map[string]map[string]any{
		"in-sync":     {"name": "in-sync", "size": "1"},
		"drifted":     {"name": "drifted", "size": "999"},
		"unreachable": {"name": "unreachable", "size": "4"},
	})
	seedStateObserveUnknown(t, statePath, "unreachable")

	// Without --read-listen nothing listens.
	cmd, out, _ := startDaemonWithArgs(t, intentDir, statePath, "100ms", "--mode", "alert-only")
	time.Sleep(500 * time.Millisecond)
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if strings.Contains(out.String(), "read-only API listening") {
		t.Fatalf("[AC-136-1] the read-only API listened without --read-listen:\n%s", out.String())
	}

	cmd, out, errOut := startDaemonWithArgs(t, intentDir, statePath, "100ms",
		"--mode", "alert-only", "--read-listen", "127.0.0.1:0")
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	base := readAPIURL(t, out, errOut)

	apiStatus := waitJSON(t, base+"/status", func(m map[string]any) bool { return m["cycle"].(float64) >= 1 }, out, errOut)
	cliStatus := runJSONCmd(t, "status", "--json", "--intent", intentDir, "--state", statePath)

	if miss := missingKeys(cliStatus, apiStatus); len(miss) > 0 {
		t.Errorf("[AC-136-1] /status lacks keys of status --json: %v (has %v)", miss, jsonKeys(apiStatus))
	}
	for _, k := range []string{"cycle", "cycle_time", "last_cycle"} {
		if _, ok := apiStatus[k]; !ok {
			t.Errorf("[AC-136-1] /status has no %q", k)
		}
	}
	if _, err := time.Parse(time.RFC3339, apiStatus["cycle_time"].(string)); err != nil {
		t.Errorf("[AC-136-1] cycle_time %v: %v", apiStatus["cycle_time"], err)
	}
	stateOf := func(m map[string]any) map[string]string {
		out := map[string]string{}
		for _, r := range listOf(m, "resources") {
			out[r["id"].(string)] = r["state"].(string)
		}
		return out
	}
	wantStates := map[string]string{
		"fake//in-sync": "conformant", "fake//drifted": "drift", "fake//absent": "drift", "fake//unreachable": "unobservable",
	}
	for name, m := range map[string]map[string]any{"status --json": cliStatus, "/status": apiStatus} {
		got := stateOf(m)
		for id, want := range wantStates {
			if got[id] != want {
				t.Errorf("[AC-136-1] %s: %s is %q, want %q (all: %v)", name, id, got[id], want, got)
			}
		}
	}
	if a, c := apiStatus["summary"], cliStatus["summary"]; !jsonEqual(a, c) {
		t.Errorf("[AC-136-1] /status summary %v, status --json %v", a, c)
	}
	if apiStatus["complete"] != false || cliStatus["complete"] != false {
		t.Errorf("[AC-136-1] complete: /status %v, status --json %v; want false (unreachable is unobservable)", apiStatus["complete"], cliStatus["complete"])
	}

	apiPlan := waitJSON(t, base+"/plan", func(m map[string]any) bool { return true })
	cliPlan := runJSONCmd(t, "plan", "--json", "--intent", intentDir, "--state", statePath,
		"--generated", filepath.Join(tmp, "plan-generated"))
	if miss := missingKeys(cliPlan, apiPlan); len(miss) > 0 {
		t.Errorf("[AC-136-1] /plan lacks keys of plan --json: %v (has %v)", miss, jsonKeys(apiPlan))
	}
	opsOf := func(m map[string]any) []string {
		var out []string
		for _, o := range listOf(m, "operations") {
			out = append(out, o["kind"].(string)+" "+o["id"].(string))
		}
		sort.Strings(out)
		return out
	}
	if a, c := opsOf(apiPlan), opsOf(cliPlan); strings.Join(a, ",") != strings.Join(c, ",") || len(a) != 2 {
		t.Errorf("[AC-136-1] /plan operations %v, plan --json %v; want CREATE absent and UPDATE drifted", a, c)
	}
	for _, o := range listOf(apiPlan, "operations") {
		if o["blocked"] != true || o["blocked_by"] != "alert_only" || o["block_reason"] == "" {
			t.Errorf("[AC-136-1] /plan op %v %v not blocked by alert_only with a reason: %v", o["kind"], o["id"], o)
		}
	}
	unobs := listOf(apiPlan, "unobservable")
	if len(unobs) != 1 || unobs[0]["id"] != "fake//unreachable" {
		t.Errorf("[AC-136-1] /plan unobservable %v, want fake//unreachable", unobs)
	}
	if rem, _ := apiPlan["remediation"].(map[string]any); rem == nil || rem["mode"] != "alert-only" {
		t.Errorf("[AC-136-1] /plan remediation %v, want mode alert-only", apiPlan["remediation"])
	}

	// [AC-136-4] the API takes no writes.
	for _, path := range []string{"/status", "/plan", "/events", "/graph"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			req, _ := http.NewRequest(method, base+path, strings.NewReader(`{}`))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed || !strings.Contains(resp.Header.Get("Allow"), "GET") {
				t.Errorf("[AC-136-4] %s %s: %d Allow=%q, want 405", method, path, resp.StatusCode, resp.Header.Get("Allow"))
			}
		}
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// sseClient reads the data events of GET /events into a channel.
type sseClient struct {
	events chan map[string]any
	resp   *http.Response
	once   sync.Once
}

func openSSE(t *testing.T, url string) *sseClient {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("GET /events: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	c := &sseClient{events: make(chan map[string]any, 1024), resp: resp}
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var ev map[string]any
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) == nil && ev["kind"] != nil {
				c.events <- ev
			}
		}
		close(c.events)
	}()
	t.Cleanup(c.close)
	return c
}

func (c *sseClient) close() { c.once.Do(func() { c.resp.Body.Close() }) }

// next waits for an event that match accepts; it returns the events seen.
func (c *sseClient) waitFor(t *testing.T, what string, match func(map[string]any) bool) []map[string]any {
	t.Helper()
	var seen []map[string]any
	deadline := time.After(15 * time.Second)
	for {
		select {
		case ev, ok := <-c.events:
			if !ok {
				t.Fatalf("[AC-136-2] the stream closed before %s; seen %v", what, seen)
			}
			seen = append(seen, ev)
			if match(ev) {
				return seen
			}
		case <-deadline:
			t.Fatalf("[AC-136-2] no %s on /events; seen %v", what, seen)
		}
	}
}

// [AC-136-2]
func TestReadAPI_EventsStreamFiringAndResolved(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	writeIntentFile(t, intentDir, map[string]any{"resources": []any{fakeRes("web", "1")}})
	seedState(t, statePath, map[string]map[string]any{"web": {"name": "web", "size": "1"}})

	cmd, out, errOut := startDaemonWithArgs(t, intentDir, statePath, "100ms",
		"--mode", "alert-only", "--read-listen", "127.0.0.1:0")
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	base := readAPIURL(t, out, errOut)
	waitJSON(t, base+"/status", func(m map[string]any) bool { return true }, out, errOut)

	a := openSSE(t, base+"/events")
	driftOn := func(state string) func(map[string]any) bool {
		return func(ev map[string]any) bool {
			return ev["kind"] == "Drift" && ev["resource"] == "fake//web" && ev["state"] == state
		}
	}
	setRealityField(t, statePath, "web", "size", "9")
	a.waitFor(t, "firing Drift of fake//web", driftOn("firing"))

	// A reader connecting now gets what is firing first.
	b := openSSE(t, base+"/events")
	select {
	case ev := <-b.events:
		if ev["state"] != "firing" {
			t.Errorf("[AC-136-2] the first event of a new reader is %v, want a firing level Event", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("[AC-136-2] a new reader got nothing")
	}
	b.waitFor(t, "the firing Drift of fake//web replayed", driftOn("firing"))

	setRealityField(t, statePath, "web", "size", "1")
	a.waitFor(t, "resolved Drift of fake//web", driftOn("resolved"))
}

// [AC-136-3]
func TestReadAPI_GraphShowsDependencyOrderAndReadiness(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	writeIntentFile(t, intentDir, map[string]any{"resources": []any{
		map[string]any{"provider": "fake", "id": "app-1",
			"depends_on": []any{map[string]string{"id": "svc-1"}},
			"fields":     map[string]string{"name": "app-1", "size": "1"}},
		map[string]any{"provider": "fake", "id": "svc-1",
			"depends_on": []any{map[string]string{"id": "vm-1", "ready_condition": "cloud_init_done"}},
			"fields":     map[string]string{"name": "svc-1", "size": "1"}},
		map[string]any{"provider": "fake", "id": "vm-1", "fields": map[string]string{"name": "vm-1", "size": "4"}},
	}})
	seedFakeState(t, statePath, map[string]any{
		"vm-1":  map[string]any{"fields": map[string]any{"name": "vm-1", "size": "4"}},
		"svc-1": map[string]any{"fields": map[string]any{"name": "svc-1", "size": "1"}},
		"app-1": map[string]any{"fields": map[string]any{"name": "app-1", "size": "1"}},
	}, map[string]map[string]bool{"vm-1": {"cloud_init_done": false}})

	cmd, out, errOut := startDaemonWithArgs(t, intentDir, statePath, "100ms", "--read-listen", "127.0.0.1:0")
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	base := readAPIURL(t, out, errOut)

	wantWaves := `[["vm-1"],["svc-1"],["app-1"]]`
	edgeReady := func(m map[string]any) any {
		for _, e := range listOf(m, "edges") {
			if e["from"] == "vm-1" && e["to"] == "svc-1" {
				return e["ready"]
			}
		}
		return "no edge"
	}
	g := waitJSON(t, base+"/graph", func(m map[string]any) bool { return true }, out, errOut)
	if w, _ := json.Marshal(g["waves"]); string(w) != wantWaves {
		t.Errorf("[AC-136-3] /graph waves %s, want %s", w, wantWaves)
	}
	if r := edgeReady(g); r != false {
		t.Errorf("[AC-136-3] vm-1 -> svc-1 ready %v, want false (cloud_init_done is false)", r)
	}
	for _, e := range listOf(g, "edges") {
		if e["from"] == "svc-1" && e["to"] == "app-1" {
			if _, has := e["ready"]; has || e["ready_condition"] != nil {
				t.Errorf("[AC-136-3] svc-1 -> app-1 has no ready_condition, got %v", e)
			}
		}
	}
	for _, n := range listOf(g, "nodes") {
		if n["local_id"] == "svc-1" && (n["ready"] != false || n["waiting_for"] != "vm-1:cloud_init_done") {
			t.Errorf("[AC-136-3] svc-1 node %v, want ready false waiting for vm-1:cloud_init_done", n)
		}
	}
	for _, k := range []string{"cycle", "cycle_time"} {
		if _, ok := g[k]; !ok {
			t.Errorf("[AC-136-3] /graph has no %q", k)
		}
	}

	setReady(t, statePath, map[string]map[string]bool{"vm-1": {"cloud_init_done": true}})
	waitJSON(t, base+"/graph", func(m map[string]any) bool { return edgeReady(m) == true }, out, errOut)

	cli := runJSONCmd(t, "graph", "--json", "--intent", intentDir, "--state", statePath)
	if w, _ := json.Marshal(cli["waves"]); string(w) != wantWaves {
		t.Errorf("[AC-136-3] graph --json waves %s, want %s", w, wantWaves)
	}
	if r := edgeReady(cli); r != true {
		t.Errorf("[AC-136-3] graph --json vm-1 -> svc-1 ready %v, want true", r)
	}
	if miss := missingKeys(cli, g); len(miss) > 0 {
		t.Errorf("[AC-136-3] /graph lacks keys of graph --json: %v", miss)
	}
}

// [AC-136-5]
func TestReadAPI_StatusShowsClaimAndFencing(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	initStateFile(t, statePath)
	writeIntentFile(t, intentDir, map[string]any{"resources": []any{
		map[string]any{"provider": "fake", "id": "a-vm", "scope": map[string]string{"site": "a"},
			"fields": map[string]string{"name": "a-vm", "size": "1"}},
	}})
	daemonStatus := filepath.Join(tmp, "daemon-status.json")
	cmd, out, errOut := startDaemonWithArgs(t, intentDir, statePath, "100ms",
		"--claim", "site=a", "--fencing-store", filepath.Join(tmp, "fencing.json"), "--shard-id", "shard-1",
		"--daemon-status", daemonStatus, "--read-listen", "127.0.0.1:0")
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	base := readAPIURL(t, out, errOut)

	checkDaemon := func(where string, m map[string]any) {
		t.Helper()
		d, _ := m["daemon"].(map[string]any)
		if d == nil {
			t.Fatalf("[AC-136-5] %s has no daemon: %v", where, jsonKeys(m))
		}
		if d["claim"] != "site=a" || d["shard"] != "shard-1" {
			t.Errorf("[AC-136-5] %s daemon claim %v shard %v, want site=a shard-1", where, d["claim"], d["shard"])
		}
		f := listOf(d, "fencing")
		if len(f) != 1 || f[0]["scope"] != "site=a" || f[0]["generation"] != float64(1) || f[0]["holder"] != "shard-1" || f[0]["fenced"] != false {
			t.Errorf("[AC-136-5] %s daemon fencing %v, want site=a generation 1 holder shard-1", where, f)
		}
		if _, has := d["mirror"]; has {
			t.Errorf("[AC-136-5] %s shows a mirror without git distribution: %v", where, d["mirror"])
		}
	}
	api := waitJSON(t, base+"/status", func(m map[string]any) bool {
		d, _ := m["daemon"].(map[string]any)
		return d != nil && len(listOf(d, "fencing")) > 0
	}, out, errOut)
	checkDaemon("/status", api)

	waitForCondition(10*time.Second, func() bool { _, err := os.Stat(daemonStatus); return err == nil })
	cli := runJSONCmd(t, "status", "--json", "--intent", intentDir, "--state", statePath,
		"--claim", "site=a", "--daemon-status", daemonStatus)
	checkDaemon("status --json", cli)

	text, _, _ := runStatusCmd(t, intentDir, statePath, "--claim", "site=a", "--daemon-status", daemonStatus)
	for _, want := range []string{"claim: site=a", "shard: shard-1", "fencing: site=a generation=1 holder=shard-1"} {
		if !strings.Contains(text, want) {
			t.Errorf("[AC-136-5] status text lacks %q:\n%s", want, text)
		}
	}
}

// [AC-136-6]
func TestReadAPI_StatusShowsMirrorState(t *testing.T) {
	f := newGitFixture(t, map[string]string{
		"intent/site=a/vms.yaml": siteIntent("a", map[string]string{"a-vm": "1"}),
		"shards/store-a.yaml":    shardYAML(map[string]string{"site": "a"}),
	})
	head := gitCmd(t, f.repo.dir, "rev-parse", "HEAD")
	_, stdout, stderr := startEdge(t,
		"--intent-repo", f.remote(), "--workdir", filepath.Join(f.root, "host-a"), "--shard", "store-a", "--s2-free",
		"--git-poll", "100ms", "--interval", "100ms", "--state", f.state, "--read-listen", "127.0.0.1:0")
	base := readAPIURL(t, stdout, stderr)

	mirrorOf := func(m map[string]any) map[string]any {
		d, _ := m["daemon"].(map[string]any)
		if d == nil {
			return nil
		}
		mm, _ := d["mirror"].(map[string]any)
		return mm
	}
	m := waitJSON(t, base+"/status", func(m map[string]any) bool { mm := mirrorOf(m); return mm != nil && mm["reachable"] == true }, stdout, stderr)
	mm := mirrorOf(m)
	if mm["revision"] != strings.TrimSpace(head) || mm["remote"] != f.remote() {
		t.Errorf("[AC-136-6] mirror %v, want revision %s remote %s", mm, head, f.remote())
	}
	fetched, err := time.Parse(time.RFC3339, mm["fetched_at"].(string))
	if err != nil || time.Since(fetched) > time.Minute {
		t.Errorf("[AC-136-6] fetched_at %v: %v", mm["fetched_at"], err)
	}
	if d := m["daemon"].(map[string]any); d["claim"] != "site=a" {
		t.Errorf("[AC-136-6] claim %v, want site=a (from shards/store-a.yaml)", d["claim"])
	}

	// The mirror is gone (WAN down): not reachable, the last revision stays.
	if err := os.RemoveAll(f.mirror); err != nil {
		t.Fatal(err)
	}
	m = waitJSON(t, base+"/status", func(m map[string]any) bool { mm := mirrorOf(m); return mm != nil && mm["reachable"] == false }, stdout, stderr)
	mm = mirrorOf(m)
	if mm["revision"] != strings.TrimSpace(head) || mm["error"] == "" || mm["fetched_at"] == nil {
		t.Errorf("[AC-136-6] mirror down: %v, want the last revision %s, its time and an error", mm, head)
	}
}
