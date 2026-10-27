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

// daemon_observe_status_test.go — #104（試用 docs/trial/observe/ で見つけたこと）:
//
//	[AC-104-1] デーモンは provider ごとに、どちらの Observe の経路（BulkObserve か
//	           per-resource Read か、§5.7）を使うかを 1 度だけログに出す
//	[AC-104-2] 周がエラーで終わり続けると、--daemon-status のファイルに連続回数と
//	           最後のエラーが載り、sakutei status が警告を出す。エラーなく終わる
//	           周が来れば 0 に戻る
package acceptance

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/dnszone"
	"github.com/tjst-t/sakutei/v2/internal/fakeprovider"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/internal/statusreport"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// [AC-104-1]
func TestDaemon_LogsObservePathOncePerProvider(t *testing.T) {
	for _, bulk := range []bool{true, false} {
		root := t.TempDir()
		intentDir := filepath.Join(root, "intent")
		if err := os.MkdirAll(intentDir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeIntentFile(t, intentDir, map[string]any{"resources": dnsBulkResources(3)})
		zone := filepath.Join(root, "zone.db")
		writeZoneFile(t, zone, dnsBulkZone(3))
		cp := &countingProvider{inner: dnszone.New(zone), bulk: bulk}
		var p provider.Provider = cp
		if bulk {
			p = &countingBulkProvider{cp}
		}
		var out bytes.Buffer
		d := reconcile.NewDaemon(reconcile.Config{
			IntentDir:    intentDir,
			GeneratedDir: filepath.Join(root, "generated"),
			Provider:     p,
			Out:          &out,
			ErrOut:       &out,
		})
		for i := 0; i < 3; i++ {
			if _, err := d.RunCycle(context.Background()); err != nil {
				t.Fatalf("RunCycle: %v", err)
			}
			d.Wait(5 * time.Second)
		}
		want := "sakutei: provider dnszone: Observe via BulkObserve"
		other := "per-resource Read"
		if !bulk {
			want, other = "sakutei: provider dnszone: Observe via per-resource Read", "BulkObserve (one call"
		}
		if n := strings.Count(out.String(), want); n != 1 {
			t.Errorf("[AC-104-1] bulk=%v: %q logged %d times over 3 cycles, want once:\n%s", bulk, want, n, out.String())
		}
		if strings.Contains(out.String(), other) {
			t.Errorf("[AC-104-1] bulk=%v: the other path %q is logged:\n%s", bulk, other, out.String())
		}
	}
}

// [AC-104-2]
func TestDaemonStatus_ReportsErrorCycles(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	initStateFile(t, statePath)
	// 読めない intent：毎周 load intent のエラーで終わる。
	if err := os.WriteFile(filepath.Join(intentDir, "intent.json"), []byte("{ broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	dsPath := filepath.Join(tmp, "daemon-status.json")

	var out, errOut syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- reconcile.Run(ctx, reconcile.Config{
			IntentDir:        intentDir,
			GeneratedDir:     filepath.Join(tmp, "generated"),
			Provider:         fakeprovider.New(statePath),
			Interval:         20 * time.Millisecond,
			DaemonStatusPath: dsPath,
			Out:              &out,
			ErrOut:           &errOut,
		})
	}()
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { cancel(); <-done }) }
	defer stop()

	health := func() statusreport.DaemonHealth {
		h, _ := statusreport.ReadDaemonHealth(dsPath)
		return h
	}
	if !waitForCondition(10*time.Second, func() bool { return health().ConsecutiveErrorCycles >= 3 }) {
		t.Fatalf("[AC-104-2] consecutive_error_cycles did not reach 3: %+v\n%s", health(), errOut.String())
	}
	h := health()
	if !strings.Contains(h.LastError, "load intent") {
		t.Errorf("[AC-104-2] last_error = %q, want the load intent error", h.LastError)
	}
	if h.Interval != "20ms" {
		t.Errorf("[AC-104-2] interval = %q, want 20ms", h.Interval)
	}

	// status は別プロセスで同じファイルを読み、警告を出す（status 自身は正しい
	// intent で回す。デーモンの intent が壊れていることは status には見えない）。
	okIntent := filepath.Join(tmp, "ok-intent")
	if err := os.MkdirAll(okIntent, 0o755); err != nil {
		t.Fatal(err)
	}
	writeOneResourceIntent(t, okIntent)
	stdout, stderr, code := runStatusCmd(t, okIntent, statePath, "--daemon-status", dsPath)
	if code != 0 {
		t.Fatalf("[AC-104-2] status exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "consecutive_error_cycles=") || !strings.Contains(stdout, "load intent") {
		t.Errorf("[AC-104-2] status lacks the error-cycle warning:\n%s", stdout)
	}

	// 直すと 0 に戻る。
	if err := os.Remove(filepath.Join(intentDir, "intent.json")); err != nil {
		t.Fatal(err)
	}
	writeOneResourceIntent(t, intentDir)
	if !waitForCondition(10*time.Second, func() bool {
		h := health()
		return h.ConsecutiveErrorCycles == 0 && h.LastError == "" && h.Cycle > 0
	}) {
		t.Fatalf("[AC-104-2] consecutive_error_cycles not reset: %+v\n%s", health(), errOut.String())
	}
}
