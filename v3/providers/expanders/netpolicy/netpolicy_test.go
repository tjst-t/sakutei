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

package netpolicy_test

import (
	"path/filepath"
	"testing"

	"github.com/tjst-t/sakutei/v3/expand"
	"github.com/tjst-t/sakutei/v3/expand/expandtest"
	"github.com/tjst-t/sakutei/v3/providers/expanders/netpolicy"
)

// TestGolden は network_policy: の展開と検査を golden で確かめる（N-P5・E-B5）。
// basic は nftables の base chain への展開（v4・v6、例外、隠れた許可と通らない許可の
// 通知）、reject は重なり・理由の無い例外・重複・宣言に無いネットワーク・domain を
// またぐ許可のエラー。
func TestGolden(t *testing.T) {
	for _, dir := range expandtest.Cases(t, "testdata") {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			expandtest.Golden(t, dir, map[string]expand.Expander{"netpolicy": netpolicy.New()}, nil)
		})
	}
}
