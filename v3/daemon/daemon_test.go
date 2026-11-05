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

package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/daemon"
	"github.com/tjst-t/sakutei/v3/load"
	"github.com/tjst-t/sakutei/v3/record"
	"github.com/tjst-t/sakutei/v3/sdk/fake"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

var base = time.Date(2026, 11, 5, 22, 0, 0, 0, time.UTC)

// capture は送信を書き留める sink。
type capture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *capture) Deliver(_ context.Context, batch []core.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range batch {
		data, _ := json.Marshal(e)
		c.buf.Write(append(data, '\n'))
	}
	return nil
}

func (c *capture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

type env struct {
	root, intent, config, state, record string
	sink                                *capture
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const intentBody = `cycle:
  interval: 30s
  stuck_after: 2
ranges:
  - provider: dns
    prune: true
resources:
  - type: dns/record
    id: www
    fields: {value: 192.0.2.10}
  - type: dns/record
    id: api
    fields: {value: 192.0.2.20}
    depends_on: [{id: dns/record/www}]
`

func setup(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{root: root, intent: filepath.Join(root, "intent"), config: filepath.Join(root, "config.yaml"),
		state: filepath.Join(root, "state.json"), record: filepath.Join(root, "record"), sink: &capture{}}
	write(t, e.config, `providers:
  dns:
    module: fake
    settings:
      state: `+e.state+`
      kinds:
        record:
          fields:
            value: string
sinks:
  cap:
    module: capture
`)
	write(t, filepath.Join(e.intent, "dns.yaml"), intentBody)
	write(t, e.state, `{"dns/record/stray": {"availability": "present", "fields": {"value": "192.0.2.99"}}}`)
	return e
}

func (e *env) opts(clk *clock) []cli.Option {
	return []cli.Option{
		cli.WithModule("fake", fake.Module), cli.WithClock(clk),
		cli.WithSink("capture", func(map[string]any) (core.Sink, error) { return e.sink, nil }),
	}
}

// world は i cycle 目の前に現実と intent を変える。one-shot 実行の列と daemon で同じものを使う。
func world(t *testing.T, e *env, i int) {
	t.Helper()
	switch i {
	case 2: // 手で変えられた（ずれ）
		write(t, e.state, `{"dns/record/www": {"availability": "present", "fields": {"value": "192.0.2.66"}},
 "dns/record/api": {"availability": "present", "fields": {"value": "192.0.2.20"}}}`)
	case 3: // 上流が観測できない（下流も待つ、Unknown は resolved にしない）
		write(t, e.state, `{"dns/record/www": {"availability": "unknown", "note": "届かない"},
 "dns/record/api": {"availability": "present", "fields": {"value": "192.0.2.1"}}}`)
	case 4: // 届かないまま（続けば stuck）
	case 5: // intent の書き誤り（その cycle は何もしない）
		write(t, filepath.Join(e.intent, "dup.yaml"), "resources:\n  - {type: dns/record, id: www, fields: {value: x}}\n")
	case 6: // 直した。届くようになった
		os.Remove(filepath.Join(e.intent, "dup.yaml"))
		write(t, e.state, `{"dns/record/www": {"availability": "present", "fields": {"value": "192.0.2.10"}},
 "dns/record/api": {"availability": "present", "fields": {"value": "192.0.2.1"}},
 "dns/record/new-stray": {"availability": "present", "fields": {"value": "192.0.2.7"}}}`)
	}
}

const cycles = 8

// 不変条件 30（§8 の Stage 3）：one-shot 実行を並べた結果と daemon の結果が同じ。同じ現実の
// 並びに対して、cycle ごとの結果・Record の全ファイル（marks・監査・通知の状態・outbox・
// sink の位置・health）・sink に届いた Event が一致する（E-O3・N-O4）。
func TestOnceSequenceEqualsDaemon(t *testing.T) {
	// one-shot 実行の列。
	a := setup(t)
	clkA := &clock{t: base}
	var onceResults []string
	for i := 0; i < cycles; i++ {
		world(t, a, i)
		clkA.set(base.Add(time.Duration(i) * time.Minute))
		var out, errOut bytes.Buffer
		code := cli.Run(context.Background(), []string{"reconcile", "--intent", a.intent, "--config", a.config, "--record", a.record, "--json"},
			&out, &errOut, a.opts(clkA)...)
		if code == cli.ExitRejected {
			onceResults = append(onceResults, "rejected")
			continue
		}
		if code == cli.ExitError {
			t.Fatalf("one-shot 実行 %d: %s", i, errOut.String())
		}
		onceResults = append(onceResults, out.String())
	}

	// daemon。
	b := setup(t)
	clkB := &clock{t: base}
	world(t, b, 0)
	var daemonResults []string
	n := 0
	wait := func(ctx context.Context, interval time.Duration, _ <-chan struct{}) bool {
		if interval != 30*time.Second {
			t.Errorf("cycle.interval が効かない: %s", interval)
		}
		n++
		if n >= cycles {
			return false
		}
		world(t, b, n)
		clkB.set(base.Add(time.Duration(n) * time.Minute))
		return true
	}
	onCycle := func(c daemon.Cycle) {
		if c.Result == nil {
			if !c.Rejected {
				t.Errorf("daemon %d: %v", c.N, c.Err)
			}
			daemonResults = append(daemonResults, "rejected")
			return
		}
		var buf bytes.Buffer
		c.Result.WriteJSON(&buf)
		daemonResults = append(daemonResults, buf.String())
	}
	var errOut bytes.Buffer
	opts := append(b.opts(clkB), cli.WithDaemonWait(wait), cli.WithDaemonCycle(onCycle))
	if code := cli.Run(context.Background(), []string{"--intent", b.intent, "--config", b.config, "--record", b.record}, &bytes.Buffer{}, &errOut, opts...); code != cli.ExitOK {
		t.Fatalf("daemon exit=%d %s", code, errOut.String())
	}

	if len(onceResults) != cycles || len(daemonResults) != cycles {
		t.Fatalf("cycle の数: one-shot 実行 %d・daemon %d", len(onceResults), len(daemonResults))
	}
	for i := range onceResults {
		if onceResults[i] != daemonResults[i] {
			t.Errorf("%d cycle 目の結果が違う:\none-shot 実行:\n%s\ndaemon:\n%s", i, onceResults[i], daemonResults[i])
		}
	}
	ra, rb := files(t, a.record), files(t, b.record)
	for name, data := range ra {
		if rb[name] != data {
			t.Errorf("Record の %s が違う:\none-shot 実行:\n%s\ndaemon:\n%s", name, data, rb[name])
		}
	}
	for name := range rb {
		if _, ok := ra[name]; !ok {
			t.Errorf("daemon だけに %s がある", name)
		}
	}
	if a.sink.String() != b.sink.String() {
		t.Errorf("sink に届いた Event が違う:\none-shot 実行:\n%s\ndaemon:\n%s", a.sink.String(), b.sink.String())
	}

	// 筋書きのとおりに通知したか（E-H1）：ずれは始まりと終わりで 1 回ずつ、
	// Unknown の cycle に resolved にしない、続けば stuck。
	got := a.sink.String()
	for _, want := range []string{
		`"kind":"Applied","type":"incident","resource":"dns/record/www","at":"2026-11-05T22:02:00Z"`,
		`"kind":"Drift","type":"state","state":"firing","resource":"dns/record/api","at":"2026-11-05T22:03:00Z"`,
		`"kind":"Unknown","type":"state","state":"firing","resource":"dns/record/www","at":"2026-11-05T22:03:00Z"`,
		`"kind":"Unknown","type":"state","state":"firing","resource":"dns/record/www","at":"2026-11-05T22:04:00Z","since":"2026-11-05T22:03:00Z","count":2,"stuck":true`,
		`"kind":"Unknown","type":"state","state":"resolved","resource":"dns/record/www","at":"2026-11-05T22:06:00Z"`,
		`"kind":"Waiting","type":"state","state":"firing","resource":"dns/record/api"`,
		`"stuck":true`,
		`"kind":"Unmanaged","type":"state","state":"resolved","resource":"dns/record/stray"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("sink に %s が無い:\n%s", want, got)
		}
	}
	h, err := record.ReadHealth(a.record)
	if err != nil || h.ConsecutiveErrors != 0 || !strings.Contains(h.LastError, "同じ ID") {
		t.Errorf("health: %+v %v", h, err)
	}
	if _, err := record.Verify(a.record); err != nil {
		t.Errorf("audit のハッシュチェーン: %v", err)
	}
}

func files(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// wake（E-N5）：sakutei wake・intent のディレクトリの変更・外からの wake（SIGHUP）で
// 間隔を待たずに次の cycle を始める。何度起こしても次の cycle は 1 回。
func TestWake(t *testing.T) {
	defer func(d time.Duration) { daemon.PollInterval = d }(daemon.PollInterval)
	daemon.PollInterval = 10 * time.Millisecond
	root := t.TempDir()
	rec, err := record.Open(filepath.Join(root, "record"))
	if err != nil {
		t.Fatal(err)
	}
	intentDir := filepath.Join(root, "intent")
	write(t, filepath.Join(intentDir, "a.yaml"), "resources: []\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cycled := make(chan int, 10)
	hup := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- daemon.Run(ctx, daemon.Options{
			Load: func(context.Context) (*load.Loaded, error) {
				return &load.Loaded{Cycle: core.CycleSettings{Interval: time.Hour}}, nil
			},
			Record: rec, IntentDir: intentDir, Wake: hup,
			OnCycle: func(c daemon.Cycle) {
				if c.Err != nil {
					t.Errorf("cycle %d: %v", c.N, c.Err)
				}
				cycled <- c.N
			},
		})
	}()
	next := func(why string) {
		t.Helper()
		select {
		case <-cycled:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s で起きない", why)
		}
	}
	next("始まり")
	time.Sleep(30 * time.Millisecond) // 見張りが今の wake と intent を覚えるのを待つ
	var out, errOut bytes.Buffer
	if code := cli.Run(context.Background(), []string{"wake", "--record", rec.Dir()}, &out, &errOut); code != cli.ExitOK {
		t.Fatalf("wake exit=%d %s", code, errOut.String())
	}
	next("sakutei wake")
	write(t, filepath.Join(intentDir, "b.yaml"), "resources: []\n")
	next("intent の変更")
	hup <- struct{}{}
	next("SIGHUP")
	select {
	case n := <-cycled:
		t.Fatalf("起こしていないのに cycle %d を回した", n)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
