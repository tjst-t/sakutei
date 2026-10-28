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

package executor

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/audit"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

type stubExec struct{ err error }

func (s stubExec) Execute(context.Context, provider.Provider, core.Plan) error { return s.err }

func TestAudited_RecordsEachOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	log := audit.NewLog(path)
	plan := core.Plan{Ops: []core.Op{
		{Kind: core.OpKindUpdate, Target: core.ID{Provider: "fake", Local: "a"}, Cause: core.CauseTagB,
			Fields: []core.FieldDiff{{Field: "size", From: "1", To: "secret-ish"}}},
		{Kind: core.OpKindDelete, Target: core.ID{Provider: "fake", Local: "b"}, Cause: core.CauseTagA},
	}}
	ok := &Audited{Inner: stubExec{}, Log: log}
	ctx := WithApproval(context.Background(), func(op core.Op) (string, string) {
		if op.Target.Local == "b" {
			return "lane-1", "alice,bob"
		}
		return "", ""
	})
	if err := ok.Execute(ctx, nil, plan); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	bad := &Audited{Inner: stubExec{err: boom}, Log: log}
	if err := bad.Execute(WithPrune(context.Background()), nil, core.Plan{Ops: plan.Ops[1:]}); !errors.Is(err, boom) {
		t.Fatalf("the Apply error must pass through: %v", err)
	}
	recs, err := audit.ReadAll(path)
	if err != nil || len(recs) != 3 {
		t.Fatalf("records: %d %v", len(recs), err)
	}
	if r := recs[0]; r.Kind != audit.KindApply || r.Op != "update" || len(r.Fields) != 1 || r.Fields[0] != "size" ||
		r.Cause != "B" || r.Result != audit.ResultOK || r.PlannedChange != "" {
		t.Errorf("update record = %+v", r)
	}
	if r := recs[1]; r.Op != "delete" || r.PlannedChange != "lane-1" || r.Actor != "alice,bob" {
		t.Errorf("lane record = %+v", r)
	}
	if r := recs[2]; r.Kind != audit.KindPrune || r.Result != audit.ResultFailed || r.Error != "boom" {
		t.Errorf("failed prune record = %+v", r)
	}
}
