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


package reconcile

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/runtimeargs"
)

// #213: a daemon keeps its role's start values; the role gone from the
// intent is reported as RuntimeDeclarationChanged until it is back.
func TestRuntimeReport_RoleNoLongerDeclared(t *testing.T) {
	full := &runtimeargs.Declaration{Values: map[string]string{"mode": "alert-only"},
		Roles: map[string]map[string]string{"os-install": {"mode": "auto"}}}
	start, err := full.ForRole("os-install")
	if err != nil {
		t.Fatal(err)
	}
	r := &RuntimeReport{Role: "os-install", Start: start}
	var buf bytes.Buffer
	r.observe(&buf, full, 1)
	if r.changed != "" {
		t.Fatalf("same declaration: changed = %q", r.changed)
	}
	r.observe(&buf, &runtimeargs.Declaration{Values: map[string]string{"mode": "alert-only"}}, 2)
	if !strings.Contains(r.changed, "role os-install: no longer declared") || !strings.Contains(r.changed, "mode: started with auto, now alert-only") {
		t.Errorf("role removed: changed = %q", r.changed)
	}
	r.observe(&buf, full, 3)
	if r.changed != "" {
		t.Errorf("role back: changed = %q", r.changed)
	}
}
