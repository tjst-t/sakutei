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

// runtime_roles_test.go — #213: 同じ intent の中で常駐の役ごとに runtime: の
// 値を宣言する（runtime.roles.<name>:、§26.7）。常駐は --runtime-role で役を
// 選び、役の欄 → daemon: → 共通の runtime: の順に値が効く。
//
//	[AC-213-1]  役を選んだ常駐は役の値（mode auto）で動いて Apply し、役を
//	            選ばない常駐は同じ intent の既定（alert-only）のまま Apply
//	            しない。役の値は共通の値より緩くても断らない（宣言なので）。
//	            status（runtime_role）と S4 監査の記録（runtime_role）に役が出る。
//	[AC-213-2]  旗と役の値の突き合わせは今の決まりのまま: 役の値を緩める
//	            旗は断り（直し方は runtime.roles.<name>.<key> を指す）、締める
//	            旗は RuntimeTightened にその欄を出す。役を選ばない常駐の
//	            --mode auto は今どおり断る。
//	[AC-213-3]  宣言に無い役で起動すれば断る。役の欄の誤り（知らないキー・
//	            名前の形）は読み込みで断る。
package acceptance

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/audit"
	"github.com/tjst-t/sakutei/v2/internal/runtimeargs"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

const runtimeRolesYAML = "runtime:\n  mode: alert-only\n  max_changes_per_cycle: 20\n" +
	"  daemon:\n    interval: 150ms\n" +
	"  roles:\n    os-install:\n      mode: auto\n      max_changes_per_cycle: 50\n"

func TestRuntimeRoles_RoleRunsWithItsValues(t *testing.T) {
	// 役を選ばない常駐: 既定の alert-only のまま。
	d := newRuntimeRig(t, runtimeRolesYAML)
	_, h := startRuntimeDaemon(t, "[AC-213-1] default", d, 2)
	if h.RuntimeRole != "" || h.Interval != "150ms" || len(h.RuntimeTightened) != 0 {
		t.Errorf("[AC-213-1] default daemon health = %+v, want no role, runtime.daemon.interval", h)
	}
	if data := readStateFile(t, d.state); resourceInState(data, "web-1") {
		t.Errorf("[AC-213-1] the daemon without a role must stay alert-only; state:\n%s", data)
	}

	// 役を選んだ常駐: runtime.roles.os-install.mode auto で Apply する。
	r := newRuntimeRig(t, runtimeRolesYAML)
	auditPath := filepath.Join(r.dir, "audit.jsonl")
	out, h := startRuntimeDaemon(t, "[AC-213-1] role", r, 2, "--runtime-role", "os-install", "--audit-log", auditPath)
	if h.RuntimeRole != "os-install" || h.Interval != "150ms" || len(h.RuntimeTightened) != 0 || h.RuntimeChanged != "" {
		t.Errorf("[AC-213-1] role daemon health = %+v, want runtime_role os-install and no tightening", h)
	}
	if data := readStateFile(t, r.state); !resourceInState(data, "web-1") || !resourceInState(data, "web-2") {
		t.Errorf("[AC-213-1] the os-install role (mode auto) must Apply; state:\n%s", data)
	}
	if !strings.Contains(out, "role os-install") {
		t.Errorf("[AC-213-1] the daemon must say which role it runs as:\n%s", out)
	}
	if n := len(eventsOf(readEvents(t, r.events), core.EventRuntimeTightened, "")); n != 0 {
		t.Errorf("[AC-213-1] a role's own values must not raise RuntimeTightened (%d)", n)
	}
	text, _ := r.run(t, "status", "--intent", r.intent, "--state", r.state, "--daemon-status", r.health)
	if !strings.Contains(text, "runtime の役: os-install") {
		t.Errorf("[AC-213-1] sakutei status does not name the role:\n%s", text)
	}
	recs, err := audit.ReadAll(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	applies := 0
	for _, rec := range recs {
		if rec.Kind == audit.KindApply {
			applies++
			if rec.RuntimeRole != "os-install" {
				t.Errorf("[AC-213-1] audit record %+v lacks runtime_role os-install", rec)
			}
		}
	}
	if applies == 0 {
		t.Errorf("[AC-213-1] no resource.apply in the audit log: %+v", recs)
	}
}

func TestRuntimeRoles_FlagsAgainstTheRolesValues(t *testing.T) {
	r := newRuntimeRig(t, runtimeRolesYAML)
	// 役を選ばない常駐の --mode auto は今どおり断る。
	out, code := r.run(t, "--intent", r.intent, "--state", r.state, "--mode", "auto")
	assertRefusalLine(t, "[AC-213-2] default --mode auto", out, code, "mode", "mode", "auto", "alert-only")
	// 役の値（50）を緩める旗は断り、役の欄を指す。
	out, code = r.run(t, "--intent", r.intent, "--state", r.state, "--runtime-role", "os-install", "--max-changes-per-cycle", "0")
	assertRefusalLine(t, "[AC-213-2] role loosen", out, code, "max-changes-per-cycle", "roles.os-install.max_changes_per_cycle", "0", "50")
	// 向きの無い引数は役が書いていなければ daemon: と比べる。
	out, code = r.run(t, "--intent", r.intent, "--state", r.state, "--runtime-role", "os-install", "--interval", "5s")
	assertRefusalLine(t, "[AC-213-2] role interval", out, code, "interval", "daemon.interval", "5s", "150ms")
	if data := readStateFile(t, r.state); resourceInState(data, "web-1") {
		t.Errorf("[AC-213-2] a refused start must not Apply; state:\n%s", data)
	}

	// 締める旗: 役の mode auto を --mode alert-only で締める。
	d := newRuntimeRig(t, runtimeRolesYAML)
	_, h := startRuntimeDaemon(t, "[AC-213-2]", d, 2, "--runtime-role", "os-install", "--mode", "alert-only")
	want := runtimeargs.Tightening{Key: "mode", Flag: "mode", FlagValue: "alert-only", IntentValue: "auto"}
	if len(h.RuntimeTightened) != 1 || h.RuntimeTightened[0].Key != want.Key || h.RuntimeTightened[0].IntentValue != "auto" ||
		h.RuntimeTightened[0].IntentPath() != "runtime.roles.os-install.mode" {
		t.Errorf("[AC-213-2] runtime_tightened = %+v, want mode tightened against runtime.roles.os-install.mode", h.RuntimeTightened)
	}
	if data := readStateFile(t, d.state); resourceInState(data, "web-1") {
		t.Errorf("[AC-213-2] --mode alert-only must win over the role; state:\n%s", data)
	}
	evs := eventsOf(readEvents(t, d.events), core.EventRuntimeTightened, core.EventFiring)
	if len(evs) == 0 || !strings.Contains(evs[0].Message, "runtime.roles.os-install.mode auto") {
		t.Errorf("[AC-213-2] RuntimeTightened must name runtime.roles.os-install.mode: %+v", evs)
	}
}

func TestRuntimeRoles_Refusals(t *testing.T) {
	r := newRuntimeRig(t, runtimeRolesYAML)
	out, code := r.run(t, "--intent", r.intent, "--state", r.state, "--runtime-role", "canary")
	if code != 1 || !strings.Contains(out, "--runtime-role canary is not declared") || !strings.Contains(out, "os-install") {
		t.Errorf("[AC-213-3] an undeclared role must refuse the start naming the declared ones: exit %d:\n%s", code, out)
	}
	// roles: の無い runtime: で役を選んでも断る。
	n := newRuntimeRig(t, "runtime:\n  mode: alert-only\n")
	out, code = n.run(t, "--intent", n.intent, "--state", n.state, "--runtime-role", "os-install")
	if code != 1 || !strings.Contains(out, "--runtime-role os-install is not declared") {
		t.Errorf("[AC-213-3] a role without runtime.roles must refuse: exit %d:\n%s", code, out)
	}
	if data := readStateFile(t, r.state); resourceInState(data, "web-1") {
		t.Errorf("[AC-213-3] a refused start must not Apply; state:\n%s", data)
	}

	for _, c := range []struct{ yaml, want string }{
		{"runtime:\n  roles:\n    os-install:\n      mod: auto\n", "mod"},
		{"runtime:\n  roles:\n    OS:\n      mode: auto\n", "role name"},
		{"runtime:\n  roles:\n    os-install:\n      daemon:\n        mode: auto\n", "daemon"},
		{"runtime:\n  roles:\n    os-install: auto\n", "runtime.roles.os-install"},
	} {
		r.writeRuntime(t, c.yaml)
		out, code := r.run(t, "plan", "--intent", r.intent, "--state", r.state)
		if code != 1 || !strings.Contains(out, c.want) {
			t.Errorf("[AC-213-3] %q must be refused naming %q: exit %d:\n%s", c.yaml, c.want, code, out)
		}
	}
}
