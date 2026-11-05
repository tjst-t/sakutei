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
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/sdk/contract"
	"github.com/tjst-t/sakutei/v3/sdk/fake"
)

// 偽の provider で Transaction のロールバックの契約テストが通る（E-P1・E-B3・
// 不変条件 17）。タイムアウトは Expire で起こす。
func TestFakeTxn(t *testing.T) {
	contract.RunTxn(t, func(t *testing.T) contract.TxnTarget {
		p := fake.New(schema)
		p.Set(rec("old"), core.Observation{Availability: core.Present, Fields: core.Fields{"value": "x"}})
		return contract.TxnTarget{
			Provider: p,
			Change:   core.Resource{ID: rec("old"), Fields: core.Fields{"value": "y"}},
			Timeout:  time.Second,
			Expire: func() {
				for _, txn := range p.Txns() {
					txn.Expire()
				}
			},
		}
	})
}
