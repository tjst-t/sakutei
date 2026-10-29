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

package workflow

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/fakeprovider"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

func judgeDef() Definition {
	return Definition{
		Name:     "install",
		Stages:   []Stage{{Name: "s1", Targets: []core.ID{core.NewID("fake", core.NewScope(nil), "n1")}}},
		Steps:    []Step{{Kind: StepApply, Fields: map[string]string{"version": "2"}}},
		DoneWhen: &DoneWhen{Fields: map[string]string{"version": "2"}},
	}
}

// #209: done_when が観測できないものは満たされたと見ない（unknown ≠ done）。
func TestJudge_DoneWhenUnknownHolds(t *testing.T) {
	def := judgeDef()
	fp := fakeprovider.New(filepath.Join(t.TempDir(), "state.json"))
	for name, pf := range map[string]func(core.ID) provider.Provider{
		"no provider": func(core.ID) provider.Provider { return nil },
		"absent":      func(core.ID) provider.Provider { return fp },
	} {
		holds := Judge{ProviderFor: pf}.Holds(context.Background(), []Definition{def})
		if len(holds) != 1 || !holds[0].Unknown {
			t.Fatalf("%s: holds = %+v, want the Workflow held as unknown", name, holds)
		}
		if d := holds[0].Describe(false); !strings.Contains(d, "progress unknown (no --workflow-store") {
			t.Fatalf("%s: describe = %q", name, d)
		}
	}
	if err := fp.SeedResource("n1", map[string]any{"version": "2"}); err != nil {
		t.Fatal(err)
	}
	if holds := (Judge{ProviderFor: func(core.ID) provider.Provider { return fp }}).Holds(context.Background(), []Definition{def}); len(holds) != 0 {
		t.Fatalf("holds = %+v, want none once done_when is observed", holds)
	}
}

// #209: done_when は定義のハッシュに入れない（書き足しても done の run を起こし直さない）。
func TestDefinition_DoneWhenNotInHash(t *testing.T) {
	def := judgeDef()
	without := def
	without.DoneWhen = nil
	if def.Hash() != without.Hash() {
		t.Fatal("done_when changed the definition hash")
	}
	def.Trigger = TriggerReimage
	def.Stages[0].Targets[0] = core.NewID("fake", core.NewScope(map[string]string{"rack": "r1"}), "n1")
	if err := def.Validate(); err == nil || !strings.Contains(err.Error(), "done_when is not for trigger reimage") {
		t.Fatalf("done_when with trigger: reimage: err = %v", err)
	}
}

// #209: 進めるロックは 1 つの FileStore の上で 1 人だけが持つ。
func TestFileStore_AdvanceLockOneHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflows.json")
	a := &Engine{Store: NewFileStore(path)}
	b := &Engine{Store: NewFileStore(path)}
	if ok, err := a.AcquireAdvance(); !ok || err != nil {
		t.Fatalf("a: %v %v", ok, err)
	}
	if ok, err := a.AcquireAdvance(); !ok || err != nil {
		t.Fatalf("a again (already held): %v %v", ok, err)
	}
	if ok, err := b.AcquireAdvance(); ok || err != nil {
		t.Fatalf("b while a holds: %v %v, want not taken", ok, err)
	}
	a.Release()
	if ok, err := b.AcquireAdvance(); !ok || err != nil {
		t.Fatalf("b after a released: %v %v", ok, err)
	}
	b.Release()
}
