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

package contract_test

import (
	"errors"
	"testing"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/sdk/contract"
	"github.com/tjst-t/sakutei/v3/sdk/fake"
)

var schema = core.Schema{Version: "fake/1", Kinds: map[string]core.KindSchema{
	"rec": {Fields: map[string]core.FieldSpec{"value": {Type: core.String}}},
}}

func rec(name string) core.ID { return core.ID{Type: "p/rec", Name: name} }

// 偽の provider で契約テストの全部の確かめが通る（E-P1・E-P2）。
func TestFake(t *testing.T) {
	contract.Run(t, func(t *testing.T) contract.Target {
		p := fake.New(schema)
		p.Set(rec("old"), core.Observation{Availability: core.Present, Fields: core.Fields{"value": "x"}})
		p.SetOp("rec", "kick", core.OpSpec{}, nil)
		p.RefuseDelete(rec("old"), "中身がある")
		var notSent, failed bool
		p.OnInvoke(func(core.ID, string, core.Fields) error {
			switch {
			case notSent:
				return core.ErrNotSent
			case failed:
				return errors.New("相手が断った")
			}
			return nil
		})
		return contract.Target{
			Provider: p,
			Ranges:   []core.Range{{Provider: "p", Args: map[string]string{"prefix": ""}}},
			Absent:   []core.ID{rec("none")},
			Create:   []core.Resource{{ID: rec("new"), Fields: core.Fields{"value": "v"}}},
			Observed: []core.ID{rec("old")},
			Faults: map[string]func(){
				"答えない": func() {
					p.Omit(rec("none"))
					p.Omit(rec("old"))
					p.Omit(rec("new"))
					p.SetListError(errors.New("届かない"))
				},
			},
			Truncate: func() { p.SetListIncomplete(true) },
			Invokes: []contract.Invoke{{ID: rec("old"), Op: "kick",
				NotSent: func() { notSent = true }, Failed: func() { failed = true }}},
			Refuses: []contract.Refuse{{ID: rec("old")}},
		}
	})
}
