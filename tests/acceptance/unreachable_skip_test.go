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

// unreachable_skip_test.go — #15 (旧 ROADMAP backlog, S072a14 review D-3):
// 外部入力が初回から届かず前回値も無いとき、デーモンはその周期を halt-safe に
// スキップする（§9.2）。スキップが続いても静かに止まり続けないよう、連続スキップ
// 回数をログと "sakutei status" に出し、上限に達したら通知（Event）を上げる。
//
//	[AC-15-1]  外部入力を不達にしたまま N 周期回すと、連続スキップ回数がログと
//	           sakutei status（--daemon-status）に出る。上限で Prolonged Event。
//	[AC-15-2]  外部入力が届くようになれば回数は 0 に戻り、収束が再開する。
package acceptance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/fakeprovider"
	"github.com/tjst-t/sakutei/internal/reconcile"
	"github.com/tjst-t/sakutei/internal/render"
)

var skipCountRe = regexp.MustCompile(`consecutive_unreachable_skips=(\d+)`)

// lastSkipCount returns the last consecutive_unreachable_skips=N in s, or -1.
func lastSkipCount(s string) int {
	m := skipCountRe.FindAllStringSubmatch(s, -1)
	if len(m) == 0 {
		return -1
	}
	n, _ := strconv.Atoi(m[len(m)-1][1])
	return n
}

func TestUnreachableSkip_CountInLogAndStatus(t *testing.T) {
	// [AC-15-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	seedState(t, statePath, map[string]map[string]any{
		"phase1-vm": {"name": "phase1-vm", "size": "2"}, // drifted: an Apply would change it
	})
	writeOneResourceIntent(t, intentDir)
	stateBefore, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(tmp, "daemon.log")
	dsPath := filepath.Join(tmp, "daemon-status.json")

	const n = 5
	cmd, wait := startDaemonWithFlags(t, logPath,
		"--intent", intentDir, "--state", statePath, "--interval", "30ms",
		"--generated", filepath.Join(tmp, "generated"),
		"--inventory-unreachable", "--unreachable-skip-limit", "3",
		"--daemon-status", dsPath)
	if !waitForLogFile(logPath, "consecutive_unreachable_skips="+strconv.Itoa(n), 20*time.Second) {
		_ = cmd.Process.Kill()
		_ = wait()
		data, _ := os.ReadFile(logPath)
		t.Fatalf("[AC-15-1] no consecutive_unreachable_skips=%d in the daemon log:\n%s", n, data)
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	if err := wait(); err != nil {
		t.Fatalf("[AC-15-1] daemon exit: %v", err)
	}
	logData, _ := os.ReadFile(logPath)
	log := string(logData)

	// Log: the count rises one per cycle from 1, and the alert fires at the limit.
	for i := 1; i <= n; i++ {
		if !strings.Contains(log, "consecutive_unreachable_skips="+strconv.Itoa(i)+"\n") {
			t.Errorf("[AC-15-1] log lacks consecutive_unreachable_skips=%d", i)
		}
	}
	// #79 (§22.3): the alert is level — firing once at the limit, not every cycle.
	if c := strings.Count(log, "ExternalInputUnreachableProlonged"); c != 1 {
		t.Errorf("[AC-15-1] ExternalInputUnreachableProlonged logged %d times, want 1 (firing once at the limit 3)", c)
	}
	if strings.Contains(log, "converged (no changes)") {
		t.Errorf("[AC-15-1] a skipped cycle was reported as converged:\n%s", log)
	}
	logged := lastSkipCount(log)

	// Halt-safe: reality untouched.
	stateAfter, _ := os.ReadFile(statePath)
	if string(stateAfter) != string(stateBefore) {
		t.Errorf("[AC-15-1] reality changed during skipped cycles:\nbefore=%s\nafter=%s", stateBefore, stateAfter)
	}

	// status: the same count, with the alert.
	out, stderr, code := runStatusCmd(t, intentDir, statePath, "--daemon-status", dsPath)
	if code != 0 {
		t.Fatalf("[AC-15-1] status exit=%d stderr=%s", code, stderr)
	}
	if got := lastSkipCount(out); got != logged || got < n {
		t.Errorf("[AC-15-1] status consecutive_unreachable_skips=%d, want the last logged %d (>= %d):\n%s", got, logged, n, out)
	}
	if !strings.Contains(out, "ExternalInputUnreachableProlonged") {
		t.Errorf("[AC-15-1] status lacks the Prolonged warning:\n%s", out)
	}
	assertNoApplyVerbs(t, "[AC-15-1]", out)
	t.Logf("[AC-15-1] %d consecutive skips in log and status", logged)
}

// toggleResolver is an ExternalInputResolver whose reachability the test flips.
type toggleResolver struct{ down atomic.Bool }

func (r *toggleResolver) Resolve(context.Context) (map[string]string, error) {
	if r.down.Load() {
		return nil, errors.New("authority unreachable")
	}
	return map[string]string{"inventory_hash": "h1"}, nil
}

func TestUnreachableSkip_ResetsWhenReachable(t *testing.T) {
	// [AC-15-2]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	initStateFile(t, statePath)
	writeOneResourceIntent(t, intentDir)
	dsPath := filepath.Join(tmp, "daemon-status.json")

	res := &toggleResolver{}
	res.down.Store(true)
	var errOut, out syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- reconcile.Run(ctx, reconcile.Config{
			IntentDir:             intentDir,
			GeneratedDir:          filepath.Join(tmp, "generated"),
			Provider:              fakeprovider.New(statePath),
			Interval:              20 * time.Millisecond,
			ExternalInputResolver: render.NewResolverCache(res, time.Hour),
			UnreachableSkipLimit:  2,
			DaemonStatusPath:      dsPath,
			Out:                   &out,
			ErrOut:                &errOut,
		})
	}()
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { cancel(); <-done }) }
	defer stop()

	if !waitForStringInBuf(&errOut, "consecutive_unreachable_skips=3", 10*time.Second) {
		t.Fatalf("[AC-15-2] no 3 consecutive skips:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "ExternalInputUnreachableProlonged") {
		t.Errorf("[AC-15-2] no Prolonged alert at limit 2:\n%s", errOut.String())
	}
	res.down.Store(false)
	if !waitForStringInBuf(&out, "external input reachable again after", 10*time.Second) {
		t.Fatalf("[AC-15-2] no recovery line:\nout=%s\nerr=%s", out.String(), errOut.String())
	}
	if !waitForResourceInStateQuiet(statePath, "phase1-vm", 10*time.Second) {
		t.Errorf("[AC-15-2] convergence did not resume: phase1-vm not created")
	}
	stop()

	b, err := os.ReadFile(dsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"consecutive_unreachable_skips": 0`) {
		t.Errorf("[AC-15-2] daemon status not reset after recovery: %s", b)
	}
}

func waitForResourceInStateQuiet(statePath, local string, deadline time.Duration) bool {
	return waitForCondition(deadline, func() bool {
		b, err := os.ReadFile(statePath)
		return err == nil && strings.Contains(string(b), `"`+local+`"`)
	})
}
