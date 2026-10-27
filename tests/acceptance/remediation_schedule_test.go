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

// remediation_schedule_test.go: remediation schedule（時間帯連動、§14.2）と
// most-restrictive-wins（§14.3）。#40。
//
// 常駐ループを reconcile.Daemon で1周ずつ回し、時刻は remediation.Config.Now で
// 注入する。保守窓は「土曜 02:00-04:00（Asia/Tokyo）だけ auto、他は alert-only」。
//
//	AC-v1.5-remediation-schedule-1 — 窓の外の drift は報告だけで修復されず、窓が開いた周に修復される
//	AC-v1.5-remediation-schedule-2 — freeze は schedule の auto に勝つ
//	AC-v1.5-remediation-schedule-3 — scope 明示の alert-only は schedule の auto に勝つ（他の scope は修復される）
//	AC-v1.5-remediation-schedule-4 — --mode alert-only は schedule の auto に勝つ
//	AC-v1.5-remediation-schedule-5 — remediation: の宣言の誤りはロード時に拒否される
//	AC-v1.5-remediation-schedule-6 — 1回収束（sakutei reconcile）も schedule と scope 明示の alert-only に従う
package acceptance

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/fakeprovider"
	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/internal/remediation"
)

// windowIntent は「土曜 02:00-04:00 だけ auto」の schedule と、web（scope なし）・
// db（scope tier=db）の2リソースを宣言する。extraScopes は scopes: の中身。
func windowIntent(extraScopes string) string {
	return `{
  "remediation": {
    "default": "alert-only",
    "schedule": [
      { "window": "Sat 02:00-04:00", "mode": "auto" },
      { "window": "*", "mode": "alert-only" }
    ],
    "tz": "Asia/Tokyo"
  },
  "scopes": [` + extraScopes + `],
  "resources": [
    { "provider": "fake", "id": "web", "fields": { "name": "web", "size": "42" } },
    { "provider": "fake", "id": "db", "scope": { "tier": "db" }, "fields": { "name": "db", "size": "42" } }
  ]
}`
}

// jst は Asia/Tokyo の時刻を作る。
func jst(t *testing.T, s string) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	tm, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

// 2026-10-24 は土曜日。
const (
	friNight     = "2026-10-23 23:00" // 窓の前
	satBefore    = "2026-10-24 01:59" // 窓の直前
	satWindow    = "2026-10-24 02:00" // 窓が開く
	satWindowEnd = "2026-10-24 04:00" // 窓が閉じる（終端は含まない）
)

type scheduleRig struct {
	t   *testing.T
	fp  *fakeprovider.Provider
	d   *reconcile.Daemon
	now time.Time
}

func newScheduleRig(t *testing.T, intent string, rem remediation.Config) *scheduleRig {
	t.Helper()
	root := t.TempDir()
	intentDir := filepath.Join(root, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(intentDir, "intent.json"), []byte(intent), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &scheduleRig{t: t, fp: fakeprovider.New(filepath.Join(root, "state.json"))}
	rem.Now = func() time.Time { return r.now }
	r.d = reconcile.NewDaemon(reconcile.Config{
		IntentDir:    intentDir,
		GeneratedDir: filepath.Join(root, "generated"),
		Provider:     r.fp,
		Remediation:  rem,
		Out:          io.Discard,
		ErrOut:       io.Discard,
	})
	// 両方とも drift（現実 size=1、理想 size=42）。
	mustSeed(t, r.fp, "web", 1)
	mustSeed(t, r.fp, "db", 1)
	return r
}

// cycleAt は時刻 at で1周回し、Apply を待ってから結果を返す。
func (r *scheduleRig) cycleAt(at string) reconcile.CycleResult {
	r.t.Helper()
	r.now = jst(r.t, at)
	res, err := r.d.RunCycle(context.Background())
	if err != nil {
		r.t.Fatalf("RunCycle at %s: %v", at, err)
	}
	r.d.Wait(5 * time.Second)
	return res
}

func (r *scheduleRig) applies() int {
	r.t.Helper()
	counts, err := r.fp.GetApplyCount()
	if err != nil {
		r.t.Fatal(err)
	}
	n := 0
	for _, c := range counts {
		n += c
	}
	return n
}

// [AC-v1.5-remediation-schedule-1]
func TestRemediationSchedule_DriftOutsideWindowCarriedUntilWindowOpens(t *testing.T) {
	r := newScheduleRig(t, windowIntent(""), remediation.Config{})

	// 窓の外: drift は検知・報告されるが、Apply は1回も呼ばれない。
	for _, at := range []string{friNight, satBefore} {
		res := r.cycleAt(at)
		if !res.AlertOnly || res.Mode.Mode != remediation.ModeAlertOnly {
			t.Fatalf("[AC-v1.5-remediation-schedule-1] %s は窓の外なので alert-only のはず: AlertOnly=%v mode=%+v", at, res.AlertOnly, res.Mode)
		}
		if res.DriftCount != 2 {
			t.Errorf("[AC-v1.5-remediation-schedule-1] %s: drift 2 件が報告されるはず: got %d (%v)", at, res.DriftCount, res.DriftEvents)
		}
		if !strings.Contains(res.Mode.Reason, `"*"`) {
			t.Errorf("[AC-v1.5-remediation-schedule-1] %s: 理由に決めた窓が出るはず: %q", at, res.Mode.Reason)
		}
	}
	if n := r.applies(); n != 0 {
		t.Fatalf("[AC-v1.5-remediation-schedule-1] 窓の外で Apply が %d 回呼ばれた", n)
	}
	if mustSize(t, r.fp, "web") != 1 || mustSize(t, r.fp, "db") != 1 {
		t.Fatalf("[AC-v1.5-remediation-schedule-1] 窓の外で現実が変わった")
	}

	// 窓が開いた周: 持ち越した drift が（新たな変更なしに）修復される。
	res := r.cycleAt(satWindow)
	if res.AlertOnly || res.Mode.Mode != remediation.ModeAuto {
		t.Fatalf("[AC-v1.5-remediation-schedule-1] 窓の中は auto のはず: %+v", res.Mode)
	}
	if !strings.Contains(res.Mode.Reason, "Sat 02:00-04:00") {
		t.Errorf("[AC-v1.5-remediation-schedule-1] 理由に保守窓が出るはず: %q", res.Mode.Reason)
	}
	if got := mustSize(t, r.fp, "web"); got != 42 {
		t.Errorf("[AC-v1.5-remediation-schedule-1] 窓が開いた周に web が修復されるはず: size=%d", got)
	}
	if got := mustSize(t, r.fp, "db"); got != 42 {
		t.Errorf("[AC-v1.5-remediation-schedule-1] 窓が開いた周に db が修復されるはず: size=%d", got)
	}

	// 窓が閉じたら（終端は含まない）また alert-only: 新しい drift は直さない。
	mustSeed(t, r.fp, "web", 7)
	before := r.applies()
	res = r.cycleAt(satWindowEnd)
	if !res.AlertOnly {
		t.Fatalf("[AC-v1.5-remediation-schedule-1] 04:00 は窓の外のはず: %+v", res.Mode)
	}
	if r.applies() != before || mustSize(t, r.fp, "web") != 7 {
		t.Errorf("[AC-v1.5-remediation-schedule-1] 窓が閉じた後の drift は修復されないはず")
	}
}

// [AC-v1.5-remediation-schedule-2]
func TestRemediationSchedule_FreezeBeatsScheduleAuto(t *testing.T) {
	r := newScheduleRig(t, windowIntent(""), remediation.Config{Freeze: true})
	res := r.cycleAt(satWindow)
	if !res.AlertOnly || res.Mode.Mode != remediation.ModeAlertOnly {
		t.Fatalf("[AC-v1.5-remediation-schedule-2] 窓の中でも freeze なら alert-only のはず: %+v", res.Mode)
	}
	if !strings.Contains(res.Mode.Reason, "freeze") {
		t.Errorf("[AC-v1.5-remediation-schedule-2] 理由に freeze が出るはず: %q", res.Mode.Reason)
	}
	if res.DriftCount != 2 {
		t.Errorf("[AC-v1.5-remediation-schedule-2] drift は報告され続けるはず: got %d", res.DriftCount)
	}
	if n := r.applies(); n != 0 {
		t.Errorf("[AC-v1.5-remediation-schedule-2] freeze 中に Apply が %d 回呼ばれた", n)
	}
}

// [AC-v1.5-remediation-schedule-3]
func TestRemediationSchedule_ScopeAlertOnlyBeatsScheduleAuto(t *testing.T) {
	r := newScheduleRig(t, windowIntent(`{ "scope": { "tier": "db" }, "remediation": "alert-only" }`), remediation.Config{})

	// 窓の外: どちらも直さない。
	if res := r.cycleAt(satBefore); !res.AlertOnly {
		t.Fatalf("[AC-v1.5-remediation-schedule-3] 窓の外は alert-only のはず: %+v", res.Mode)
	}

	// 窓の中: run 全体は auto だが、tier=db は scope 明示の alert-only で止まる。
	res := r.cycleAt(satWindow)
	if res.AlertOnly || res.Mode.Mode != remediation.ModeAuto {
		t.Fatalf("[AC-v1.5-remediation-schedule-3] 窓の中の run 全体の mode は auto のはず: %+v", res.Mode)
	}
	if got := mustSize(t, r.fp, "web"); got != 42 {
		t.Errorf("[AC-v1.5-remediation-schedule-3] scope 外の web は修復されるはず: size=%d", got)
	}
	if got := mustSize(t, r.fp, "db"); got != 1 {
		t.Errorf("[AC-v1.5-remediation-schedule-3] alert-only scope の db は修復されないはず: size=%d", got)
	}
	if res.DriftCount != 1 || len(res.DriftEvents) != 1 || !strings.Contains(res.DriftEvents[0].Message, "db") {
		t.Errorf("[AC-v1.5-remediation-schedule-3] db の drift だけが報告されるはず: count=%d events=%v", res.DriftCount, res.DriftEvents)
	}
	counts, _ := r.fp.GetApplyCount()
	if counts["db"] != 0 {
		t.Errorf("[AC-v1.5-remediation-schedule-3] db に Apply が %d 回呼ばれた", counts["db"])
	}

	// scope 明示の auto は他の決定源の alert-only を覆せない（窓の外）。
	r2 := newScheduleRig(t, windowIntent(`{ "scope": { "tier": "db" }, "remediation": "auto" }`), remediation.Config{})
	if res := r2.cycleAt(satBefore); !res.AlertOnly || r2.applies() != 0 {
		t.Errorf("[AC-v1.5-remediation-schedule-3] scope の auto は窓の外の alert-only に勝たないはず: AlertOnly=%v applies=%d", res.AlertOnly, r2.applies())
	}
}

// [AC-v1.5-remediation-schedule-4]
func TestRemediationSchedule_ModeFlagAlertOnlyBeatsScheduleAuto(t *testing.T) {
	r := newScheduleRig(t, windowIntent(""), remediation.Config{Mode: remediation.ModeAlertOnly})
	res := r.cycleAt(satWindow)
	if !res.AlertOnly {
		t.Fatalf("[AC-v1.5-remediation-schedule-4] --mode alert-only は窓の auto に勝つはず: %+v", res.Mode)
	}
	if !strings.Contains(res.Mode.Reason, "--mode alert-only") {
		t.Errorf("[AC-v1.5-remediation-schedule-4] 理由に --mode が出るはず: %q", res.Mode.Reason)
	}
	if n := r.applies(); n != 0 {
		t.Errorf("[AC-v1.5-remediation-schedule-4] Apply が %d 回呼ばれた", n)
	}
}

// [AC-v1.5-remediation-schedule-5]
func TestRemediationSchedule_InvalidDeclarationsRejected(t *testing.T) {
	cases := map[string]map[string]string{
		"unknown day": {"a.json": `{ "remediation": { "schedule": [ { "window": "Sab 02:00-04:00", "mode": "auto" } ] } }`},
		"bad time":    {"a.json": `{ "remediation": { "schedule": [ { "window": "Sat 2:00-04:00", "mode": "auto" } ] } }`},
		"empty range": {"a.json": `{ "remediation": { "schedule": [ { "window": "Sat 02:00-02:00", "mode": "auto" } ] } }`},
		"bad mode":    {"a.json": `{ "remediation": { "schedule": [ { "window": "*", "mode": "sometimes" } ] } }`},
		"bad tz":      {"a.json": `{ "remediation": { "tz": "Mars/Olympus" } }`},
		"unknown key": {"a.json": `{ "remediation": { "windows": [] } }`},
		"twice": {
			"a.json": `{ "remediation": { "default": "auto" } }`,
			"b.json": `{ "remediation": { "default": "alert-only" } }`,
		},
		"scope remediation on sync scope": {"a.json": `{ "scopes": [ { "scope": { "site": "x" }, "mode": "sync", "sync": { "target_path": "generated/x/" }, "remediation": "alert-only" } ] }`},
		"unknown scope remediation":       {"a.json": `{ "scopes": [ { "scope": { "site": "x" }, "remediation": "never" } ] }`},
	}
	for name, files := range cases {
		dir := t.TempDir()
		for fn, body := range files {
			if err := os.WriteFile(filepath.Join(dir, fn), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := loader.LoadIntent(dir); err == nil {
			t.Errorf("[AC-v1.5-remediation-schedule-5] %s: ロードは失敗するはず", name)
		}
	}

	// YAML で書いた設計書の例（§14.2）はそのまま読める。
	dir := t.TempDir()
	yaml := `remediation:
  default: alert-only
  schedule:
    - window: "Sat 02:00-04:00"
      mode: auto
    - window: "*"
      mode: alert-only
  tz: Asia/Tokyo
`
	if err := os.WriteFile(filepath.Join(dir, "remediation.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	in, err := loader.LoadIntent(dir)
	if err != nil {
		t.Fatalf("[AC-v1.5-remediation-schedule-5] §14.2 の例が読めない: %v", err)
	}
	if in.Remediation == nil || len(in.Remediation.Entries) != 2 {
		t.Fatalf("[AC-v1.5-remediation-schedule-5] schedule が読めていない: %+v", in.Remediation)
	}
	if m, _ := in.Remediation.ModeAt(jst(t, satWindow)); m != remediation.ModeAuto {
		t.Errorf("[AC-v1.5-remediation-schedule-5] 土曜 02:00 JST は auto のはず: %v", m)
	}
	if m, _ := in.Remediation.ModeAt(jst(t, satWindowEnd)); m != remediation.ModeAlertOnly {
		t.Errorf("[AC-v1.5-remediation-schedule-5] 土曜 04:00 JST は alert-only のはず: %v", m)
	}
}

// [AC-v1.5-remediation-schedule-6]
func TestRemediationSchedule_OneShotReconcileFollowsScheduleAndScope(t *testing.T) {
	// 常に開いている窓（"*" → auto）+ tier=db は scope 明示の alert-only。
	intent := `{
  "remediation": { "default": "alert-only", "schedule": [ { "window": "*", "mode": "auto" } ] },
  "scopes": [ { "scope": { "tier": "db" }, "remediation": "alert-only" } ],
  "resources": [
    { "provider": "fake", "id": "web", "fields": { "name": "web", "size": "42" } },
    { "provider": "fake", "id": "db", "scope": { "tier": "db" }, "fields": { "name": "db", "size": "42" } }
  ]
}`
	root := t.TempDir()
	intentDir := filepath.Join(root, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(intentDir, "intent.json"), []byte(intent), 0o644); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "state.json")
	fp := fakeprovider.New(statePath)
	if err := fp.SetProviderVersion(testProviderVersion); err != nil {
		t.Fatal(err)
	}
	mustSeed(t, fp, "web", 1)
	mustSeed(t, fp, "db", 1)

	stdout, stderr, code := runReconcile(t, intentDir, statePath)
	if code != 0 {
		t.Fatalf("[AC-v1.5-remediation-schedule-6] exit %d (held は失敗ではない)\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Remediation mode: auto") || !strings.Contains(stdout, "DRIFT EVENT: would be UPDATE") {
		t.Errorf("[AC-v1.5-remediation-schedule-6] mode と held drift が報告されるはず:\n%s", stdout)
	}
	if got := mustSize(t, fp, "web"); got != 42 {
		t.Errorf("[AC-v1.5-remediation-schedule-6] web は修復されるはず: size=%d", got)
	}
	if got := mustSize(t, fp, "db"); got != 1 {
		t.Errorf("[AC-v1.5-remediation-schedule-6] db は修復されないはず: size=%d", got)
	}

	// --freeze は schedule の auto に勝つ。
	mustSeed(t, fp, "web", 1)
	stdout, _, code = runReconcile(t, intentDir, statePath, "--freeze")
	if code != 0 || !strings.Contains(stdout, "FROZEN") {
		t.Errorf("[AC-v1.5-remediation-schedule-6] --freeze で FROZEN になるはず (exit %d):\n%s", code, stdout)
	}
	if got := mustSize(t, fp, "web"); got != 1 {
		t.Errorf("[AC-v1.5-remediation-schedule-6] freeze 中に web が変わった: size=%d", got)
	}
}
