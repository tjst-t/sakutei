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

package goldenfull

import (
	"context"
	"testing"

	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/sdk"
)

// 付属の契約テスト（§28.6）。Schema を書き換えたら Examples を合わせる。

func TestValidateProvider(t *testing.T) {
	if err := sdk.ValidateProvider(New(newFakeClient())); err != nil {
		t.Fatalf("ValidateProvider: %v", err)
	}
}

func TestContract(t *testing.T) {
	c := newFakeClient()
	p := New(c)
	ct := sdk.ContractTester{
		Provider: p,
		Lister:   p,
		Scope:    core.NewScope(nil),
		// 対象が値を検査するフィールドだけ例を与える（残りは Schema から生成）。
		Examples: map[string]string{"name": "contract-1", "value": "hello", "size": "3"},
		MakeUnreachable: func() (func(), error) {
			c.setDown(true)
			return func() { c.setDown(false) }, nil
		},
	}
	ct.RunAll(t)
}

func TestBulkEquivalence(t *testing.T) {
	c := newFakeClient()
	p := New(c)
	scope := core.NewScope(nil)
	for _, name := range []string{"a", "b", "c"} {
		plan := core.Plan{Ops: []core.Op{{
			Kind:   core.OpKindCreate,
			Target: core.NewID(Name, scope, name),
			Fields: []core.FieldDiff{{Field: "value", To: "v-" + name}, {Field: "size", To: "2"}},
		}}}
		if err := p.Apply(context.Background(), plan); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	bt := sdk.BulkEquivalenceTester{Provider: p, Lister: p, Scope: scope}
	bt.Run(t)
}
