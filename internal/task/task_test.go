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

package task

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/pkg/core"
)

// 起こし直したデーモンは既定で別の holder（<ホスト名>-<pid>）になるので、前の
// デーモンが running のまま残した記録を「別のところで実行中」とだけ言わず、
// 中断かもしれないこと・起動した時刻・cancel の案内を出す（#95 の試用の手順 5）。
func TestTrigger_RunningRecordOfAnotherHolder(t *testing.T) {
	ctx := context.Background()
	st := NewFileStore(filepath.Join(t.TempDir(), "tasks.json"))
	started := time.Date(2026, 10, 23, 16, 38, 11, 0, time.UTC)
	if _, ok, err := st.Begin(ctx, Record{Resource: "sqlmigrate//app", TaskID: "host-100-1", Phase: PhaseRunning,
		Holder: "host-100", StartedAt: started}); err != nil || !ok {
		t.Fatalf("seed: ok=%v err=%v", ok, err)
	}
	m := NewManager(st, nil, "host-200", nil)
	id := core.ID{Provider: "sqlmigrate", Local: "app"}
	dec, err := m.Trigger(ctx, id, map[string]string{"version": "3"}, core.Plan{}, core.UpdatePolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Outcome != Outstanding {
		t.Fatalf("outcome %s, want outstanding", dec.Outcome)
	}
	for _, want := range []string{`"host-100"`, "2026-10-23T16:38:11Z", "interrupted if that daemon is gone", "cancel"} {
		if !strings.Contains(dec.Reason, want) {
			t.Errorf("reason does not mention %q: %s", want, dec.Reason)
		}
	}
}
