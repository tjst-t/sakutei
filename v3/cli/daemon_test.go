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

package cli_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/sdk/fake"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// stdout の sink（§8 の Stage 3）：one-shot 実行でも Event を 1 行 1 件の JSON で書く。
func TestReconcileStdoutSink(t *testing.T) {
	e := setup(t)
	write(t, e.config, readFile(t, e.config)+"sinks:\n  out:\n    module: stdout\n")
	code, out, errOut := runCLI("reconcile", "--intent", e.intent, "--config", e.config, "--record", e.record)
	if code != cli.ExitRemaining {
		t.Fatalf("exit=%d %s\n%s", code, errOut, out)
	}
	for _, want := range []string{`"kind":"Unmanaged","type":"state","state":"firing","resource":"dns/record/stray"`, `"kind":"CycleDone"`, "reconcile: "} {
		if !strings.Contains(out, want) {
			t.Errorf("%q が無い:\n%s", want, out)
		}
	}
	// 送り終えた Event は次の cycle に送らない。
	_, out, _ = runCLI("reconcile", "--intent", e.intent, "--config", e.config, "--record", e.record)
	if strings.Contains(out, `"state":"firing","resource":"dns/record/stray"`) || !strings.Contains(out, `"state":"resolved","resource":"dns/record/stray"`) {
		t.Errorf("2 cycle 目:\n%s", out)
	}
}

// daemon（§2.2）：cycle を回し、read-only HTTP API で /status・/health・/plan を返す。
// stdout は sink、ログは stderr。ctx の終わりで終了コード 0。
func TestDaemonServesReadHTTP(t *testing.T) {
	e := setup(t)
	write(t, e.config, readFile(t, e.config)+"listen:\n  read: {address: \"127.0.0.1:0\"}\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cycled := make(chan struct{}, 1)
	wait := func(ctx context.Context, _ time.Duration, _ <-chan struct{}) bool {
		select {
		case cycled <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return false
	}
	var stdout, stderr syncBuf
	done := make(chan int, 1)
	go func() {
		done <- cli.Run(ctx, []string{"--intent", e.intent, "--config", e.config, "--record", e.record}, &stdout, &stderr,
			cli.WithModule("fake", fake.Module), cli.WithDaemonWait(wait))
	}()
	select {
	case <-cycled:
	case code := <-done:
		t.Fatalf("daemon が終わった exit=%d %s", code, stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatalf("cycle を回さない: %s", stderr.String())
	}
	m := regexp.MustCompile(`read-only HTTP API: (http://\S+)`).FindStringSubmatch(stderr.String())
	if m == nil {
		t.Fatalf("待ち受けの宛先を書かない: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "cycle 1: reconcile: ") {
		t.Errorf("ログ: %s", stderr.String())
	}
	for path, want := range map[string]string{
		"/status": `"resource": "dns/record/www"`,
		"/health": `"consecutive_errors": 0`,
		"/plan":   `"changes"`,
		"/graph":  `"dns/record/api"`,
	} {
		resp, err := http.Get(m[1] + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(body), want) {
			t.Errorf("%s: %d\n%s", path, resp.StatusCode, body)
		}
	}
	cancel()
	select {
	case code := <-done:
		if code != cli.ExitOK {
			t.Fatalf("exit=%d %s", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ctx が終わっても daemon が終わらない")
	}
	if stdout.String() != "" {
		t.Errorf("sink の無い daemon が stdout に書いた: %s", stdout.String())
	}
}

// 最初の読み込みで reject すれば daemon を始めない（終了コード 2）。
func TestDaemonRejectsAtStart(t *testing.T) {
	e := setup(t)
	write(t, filepath.Join(e.intent, "dup.yaml"), "resources:\n  - {type: dns/record, id: www}\n")
	if code, _, errOut := runCLI("--intent", e.intent, "--config", e.config, "--record", e.record); code != cli.ExitRejected {
		t.Fatalf("exit=%d %s", code, errOut)
	}
	if code, _, _ := runCLI("--intent", e.intent); code != cli.ExitError {
		t.Fatalf("引数の足りない daemon exit=%d", code)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
