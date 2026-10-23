// Copyright 2026 tjst-t
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package core

import (
	"fmt"
	"testing"
)

// The one-shot reconcile prints a held op with %v; it printed the bare number
// ("blocked 1 op for ...") before OpKind had a String (2026-10-23 trial).
func TestOpKindString(t *testing.T) {
	for k, want := range map[OpKind]string{
		OpKindCreate: "CREATE", OpKindUpdate: "UPDATE", OpKindDelete: "DELETE", OpKind(9): "OpKind(9)",
	} {
		if got := fmt.Sprintf("%v", k); got != want {
			t.Errorf("%%v of OpKind %d = %q, want %q", int(k), got, want)
		}
	}
}
