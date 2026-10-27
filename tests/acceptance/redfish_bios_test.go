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

// redfish_bios_test.go — #140: redfish-bios を CLI（plan → reconcile）で偽 BMC
// に向ける（§29.1「Redfish の BIOS の設定」）。
//
//	[AC-140-6]  plan は宣言した属性の違いだけを出し、BMC に書かない。reconcile
//	            は Bios/Settings に 1 度だけ PATCH し、再起動しない。保留中の
//	            あいだは converging で、reconcile は PATCH を重ねない。再起動で効けば plan は
//	            Converged。同じノードのシステム（ID はノード名）と並べて書ける。
//	[AC-140-7]  登録簿に合わない属性は plan・reconcile の時点でエラーで、BMC に
//	            書かない。
package acceptance

import (
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/internal/redfish/system"
)

func (r *rfCLIRig) writeBiosIntent(t *testing.T, attrs string) {
	t.Helper()
	writeIntentFile(t, r.intent, map[string]any{
		"resources": []map[string]any{
			{"provider": system.Name, "id": "node1", "fields": map[string]string{"power": "On"}},
			{"provider": system.BiosName, "id": system.BiosID("node1"), "fields": map[string]string{"attributes": attrs}},
		},
	})
}

func TestRedfishBiosCLI_PatchOnceAndConverge(t *testing.T) {
	r := newRFCLIRig(t)
	r.bmc.SetBiosAttribute("PowerProfile", "PowerSaving") // 宣言に無い属性: 触らない
	r.writeBiosIntent(t, `{"ProcVirtualization": "Disabled", "BootRetryCount": 5}`)

	out, code := r.run(t, "plan", "--redfish-config", r.config)
	if code != 0 || !strings.Contains(out, system.BiosName) || !strings.Contains(out, "ProcVirtualization") ||
		strings.Contains(out, "Converged (no changes)") {
		t.Fatalf("[AC-140-6] plan exit %d:\n%s", code, out)
	}
	if w := r.bmcWrites(); len(w) != 0 {
		t.Fatalf("[AC-140-6] plan wrote to the BMC: %v", w)
	}

	out, code = r.run(t, "reconcile", "--redfish-config", r.config)
	if code != 0 {
		t.Fatalf("[AC-140-6] reconcile exit %d:\n%s", code, out)
	}
	// 保留中のあいだは converging（観測できない周と同じく収束しきらない終了
	// コード）で、PATCH を重ねない。
	out, code = r.run(t, "reconcile", "--redfish-config", r.config)
	if code == 0 || !strings.Contains(out, "availability=converging") {
		t.Fatalf("[AC-140-6] reconcile while the change is pending: exit %d, want converging:\n%s", code, out)
	}
	out, _ = r.run(t, "plan", "--redfish-config", r.config)
	if !strings.Contains(out, "converging") {
		t.Fatalf("[AC-140-6] plan while the change is pending does not report converging:\n%s", out)
	}
	if w := r.bmcWrites(); len(w) != 1 || w[0] != "PATCH /redfish/v1/Systems/1/Bios/Settings" {
		t.Fatalf("[AC-140-6] BMC writes after two reconciles = %v, want one PATCH of Bios/Settings", w)
	}
	if p := r.bmc.BiosPending(); len(p) != 2 || p["ProcVirtualization"] != "Disabled" || p["BootRetryCount"] != float64(5) {
		t.Fatalf("[AC-140-6] pending = %v", p)
	}
	if r.bmc.Boots() != 0 {
		t.Fatalf("[AC-140-6] the BMC was rebooted %d times", r.bmc.Boots())
	}

	r.bmc.Restart()
	out, code = r.run(t, "plan", "--redfish-config", r.config)
	if code != 0 || !strings.Contains(out, "Converged (no changes)") {
		t.Fatalf("[AC-140-6] plan after the reboot (exit %d) is not converged:\n%s", code, out)
	}
	if a := r.bmc.BiosAttributes(); a["ProcVirtualization"] != "Disabled" || a["PowerProfile"] != "PowerSaving" {
		t.Fatalf("[AC-140-6] attributes after the reboot = %v", a)
	}
}

func TestRedfishBiosCLI_RegistryRejects(t *testing.T) {
	r := newRFCLIRig(t)
	r.writeBiosIntent(t, `{"ProcVirtualization": "Off"}`)
	for _, verb := range []string{"plan", "reconcile"} {
		out, code := r.run(t, verb, "--redfish-config", r.config)
		if code == 0 || !strings.Contains(out, `"Off" is not one of the values the BMC offers`) {
			t.Fatalf("[AC-140-7] %s exit %d:\n%s", verb, code, out)
		}
	}
	if w := r.bmcWrites(); len(w) != 0 {
		t.Fatalf("[AC-140-7] the BMC was written: %v", w)
	}
}
