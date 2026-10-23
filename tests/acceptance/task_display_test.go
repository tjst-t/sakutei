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

// task_display_test.go — #113: Task の表示（sqlmigrate の試用の見つけたこと 6・7）。
// 本物の sqlite3 を使う（無ければ Skip）。
//
//	[AC-113-1]  rollback_on_fail で戻した Task は、sakutei task list の ROLLBACK に
//	            rolled-back と出る。
//	[AC-113-2]  sqlmigrate は rollback の後の版を記録のエラーに含める。
//	[AC-113-3]  Task を起動した周の要約に tasks=N が出る（launched= は非同期の Apply）。
package acceptance

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestTaskDisplay_RollbackAndCycleSummary(t *testing.T) {
	r := newSMCLIRig(t)
	r.write(t, filepath.Join("migrations", "app", "0003_create_orders.up.sql"),
		"CREATE TABLE orders (id INTEGER PRIMARY KEY);\nINSERT INTO no_such_table VALUES (1);")
	r.write(t, filepath.Join("migrations", "app", "0003_create_orders.down.sql"), "DROP TABLE orders;")
	r.writeIntent(t, "app", "3")

	store := filepath.Join(r.dir, "tasks.json")
	cmd, stdout, stderr := startDaemonWithArgs(t, r.intent, r.state, "100ms",
		"--zone", r.zone, "--sqlmigrate-config", r.config, "--task-store", store)
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	var list string
	end := time.Now().Add(20 * time.Second)
	for time.Now().Before(end) {
		list, _ = runSakutei(t, "task", "list", "--store", store)
		if strings.Contains(list, "failed") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if !strings.Contains(list, "failed") {
		t.Fatalf("the Task did not fail\nlist:\n%s\nstdout:\n%s\nstderr:\n%s", list, stdout.String(), stderr.String())
	}

	// [AC-113-1]
	if !strings.Contains(list, " ROLLBACK ") ||
		!regexp.MustCompile(`(?m)^sqlmigrate//app\s+failed\s+rolled-back\s`).MatchString(list) {
		t.Errorf("[AC-113-1] task list does not say the Task was rolled back:\n%s", list)
	}

	// [AC-113-2]
	v := r.version(t)
	if want := "rolled back: the database is at version " + v; !strings.Contains(list, want) {
		t.Errorf("[AC-113-2] task list error does not say %q (the version after the rollback):\n%s", want, list)
	}
	if !strings.Contains(list, "0003_create_orders.up.sql failed (its transaction was rolled back; the database was left at version 2)") {
		t.Errorf("[AC-113-2] the failed migration and the version it left are not named:\n%s", list)
	}

	// [AC-113-3]
	out := stdout.String()
	launched := regexp.MustCompile(`(?m)^sakutei: cycle (\d+): 1 task\(s\) launched`).FindStringSubmatch(out)
	if launched == nil {
		t.Fatalf("[AC-113-3] no cycle launched the Task:\n%s", out)
	}
	summary := regexp.MustCompile(`(?m)^sakutei: cycle ` + launched[1] + `: (observed=.*)$`).FindStringSubmatch(out)
	if summary == nil || !strings.Contains(summary[1], " tasks=1") {
		t.Errorf("[AC-113-3] the summary of cycle %s does not count the Task launched: %v\n%s", launched[1], summary, out)
	}
	for _, m := range cycleSummaryRe.FindAllStringSubmatch(out, -1) {
		if m[1] != launched[1] && strings.Contains(m[2], "tasks=") {
			t.Errorf("[AC-113-3] cycle %s launched no Task but says %q", m[1], m[2])
		}
	}
}
