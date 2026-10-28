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

// external_owner_test.go — Backlog #158: the "never touch" declaration of
// §7.3 (external_owners) in the intent.  What it selects (by scope, provider
// and id / id prefix, or by an observed value the provider offers) is
// observed but never applied, deleted or pruned; an intent that also manages
// it is refused at load time.  A provider's own config exclusions
// (proxmox-vm's protected_vmids / protected_names) stay as the last safety
// net: an intent that disagrees with them is refused, and one only in the
// provider config is reported as a warning.
//
// AC tags:
//
//	AC-158-1  load-time refusals: the intent declares (or declares absent)
//	          what an external_owners entry selects, by id or by a declared
//	          value; an entry selecting everything; a field the provider
//	          does not offer
//	AC-158-2  prune: an orphan selected by id, or by its observed value, is
//	          not pruned (plan, reconcile, the daemon) while the other orphan
//	          is; status does not list it as unmanaged
//	AC-158-3  Apply: a declared resource selected by an observed value the
//	          intent does not declare is observed but not applied (reconcile,
//	          the daemon)
//	AC-158-4  proxmox-vm: the config's exclusions only in the config are a
//	          warning, gone once written in external_owners; a VM selected by
//	          vmid in the intent is not applied; declaring it is refused; an
//	          intent that manages what the config excludes is refused
package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/proxmox/pvetest"
)

// extOwnerState: web-1 (declared), orphan-t and orphan-u (orphans), all in
// site=tokyo; the fake is pruneable.
const extOwnerState = `{"resources": {"web-1": {"fields": {"name": "web-1", "size": 5}}, "orphan-t": {"fields": {"name": "orphan-t", "size": 1}}, "orphan-u": {"fields": {"name": "orphan-u", "size": 7}}},
 "scopes": {"web-1": {"site": "tokyo"}, "orphan-t": {"site": "tokyo"}, "orphan-u": {"site": "tokyo"}},
 "pruneable_capability": true, "provider_version": "v-test"}`

func extOwnerRig(t *testing.T, intentYAML string) (intent, state string) {
	t.Helper()
	root := t.TempDir()
	intent = filepath.Join(root, "intent")
	if err := os.MkdirAll(intent, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStateJSON(t, filepath.Join(intent, "intent.yaml"), intentYAML)
	state = filepath.Join(root, "state.json")
	writeStateJSON(t, state, extOwnerState)
	return intent, state
}

const extOwnerWeb = `resources:
  - provider: fake
    id: web-1
    scope:
      site: tokyo
    deletion_policy: PruneWithinScope
    fields:
      name: web-1
      size: "5"
`

func TestExternalOwner_LoadRefusals(t *testing.T) {
	// [AC-158-1]
	cases := map[string]struct {
		yaml string
		want string
	}{
		"declared by id": {extOwnerWeb + `external_owners:
  - owner: drs
    provider: fake
    id: web-1
`, "external_owner drs"},
		"declared by scope": {extOwnerWeb + `external_owners:
  - owner: drs
    scope:
      site: tokyo
`, "external_owner drs"},
		"declared by id prefix": {extOwnerWeb + `external_owners:
  - owner: drs
    provider: fake
    id_prefix: web-
`, "external_owner drs"},
		"declared absent": {`resources:
  - provider: fake
    id: orphan-t
    scope:
      site: tokyo
    ensure: absent
external_owners:
  - owner: drs
    provider: fake
    id: orphan-t
`, "declares absent"},
		"declared value": {extOwnerWeb + `external_owners:
  - owner: drs
    provider: fake
    fields:
      size: 5
`, "external_owner drs"},
		"selects everything": {extOwnerWeb + `external_owners:
  - owner: drs
`, "declare provider or a non-empty scope"},
		"field not offered": {extOwnerWeb + `external_owners:
  - owner: drs
    provider: fake
    fields:
      name: orphan-t
`, `does not offer field "name"`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			intent, state := extOwnerRig(t, c.yaml)
			for _, verb := range []string{"plan", "reconcile"} {
				out, code := runSakutei(t, verb, "--intent", intent, "--state", state)
				if code == 0 || !strings.Contains(out, c.want) {
					t.Errorf("[AC-158-1] %s: %s exit %d, want a load refusal naming %q:\n%s", name, verb, code, c.want, out)
				}
			}
			res := fakeResources(t, state)
			if len(res) != 3 || res["web-1"]["size"] != "5" {
				t.Errorf("[AC-158-1] %s: a refused intent changed reality: %v", name, res)
			}
		})
	}
}

// extOwnerPruneYAML closes site=tokyo (web-1 PruneWithinScope) and hands
// orphan-t (by id) and the fake's resources of size 7 (orphan-u, by the
// observed value) to external owners.
const extOwnerPruneYAML = extOwnerWeb + `external_owners:
  - owner: drs
    provider: fake
    id: orphan-t
  - owner: mha
    provider: fake
    scope:
      site: tokyo
    fields:
      size: 7
`

func TestExternalOwner_NotPruned(t *testing.T) {
	// [AC-158-2]
	intent, state := extOwnerRig(t, extOwnerPruneYAML)

	out := mustRun(t, "plan", "--intent", intent, "--state", state)
	if strings.Contains(out, "DELETE fake/site=tokyo/orphan-t") || strings.Contains(out, "DELETE fake/site=tokyo/orphan-u") {
		t.Fatalf("[AC-158-2] plan prunes what external_owners selects:\n%s", out)
	}
	if !strings.Contains(out, "external_owner drs") || !strings.Contains(out, "external_owner mha") {
		t.Errorf("[AC-158-2] plan does not say why the orphans are kept:\n%s", out)
	}

	// Without the declaration, both are pruned (the rig really closes the scope).
	intent2, state2 := extOwnerRig(t, extOwnerWeb)
	if out := mustRun(t, "plan", "--intent", intent2, "--state", state2); !strings.Contains(out, "DELETE fake/site=tokyo/orphan-t") || !strings.Contains(out, "DELETE fake/site=tokyo/orphan-u") {
		t.Fatalf("[AC-158-2] without external_owners the orphans should be pruned:\n%s", out)
	}

	mustRun(t, "reconcile", "--intent", intent, "--state", state)
	res := fakeResources(t, state)
	if _, ok := res["orphan-t"]; !ok {
		t.Errorf("[AC-158-2] reconcile pruned orphan-t (external_owner by id): %v", res)
	}
	if _, ok := res["orphan-u"]; !ok {
		t.Errorf("[AC-158-2] reconcile pruned orphan-u (external_owner by observed value): %v", res)
	}

	if out, _ := runSakutei(t, "status", "--intent", intent, "--state", state); strings.Contains(out, "UNMANAGED fake/site=tokyo/orphan-t") {
		t.Errorf("[AC-158-2] status lists the external_owner's orphan as unmanaged:\n%s", out)
	}

	// The daemon keeps them too.
	logPath := filepath.Join(t.TempDir(), "daemon.log")
	stop := startPlannedChangeDaemon(t, intent, state, logPath)
	waitForMoreCycles(t, logPath, 3)
	stop()
	res = fakeResources(t, state)
	if _, ok := res["orphan-t"]; !ok {
		t.Errorf("[AC-158-2] the daemon pruned orphan-t; log:\n%s", readFileString(logPath))
	}
	if _, ok := res["orphan-u"]; !ok {
		t.Errorf("[AC-158-2] the daemon pruned orphan-u; log:\n%s", readFileString(logPath))
	}
	if !strings.Contains(readFileString(logPath), "external_owner drs") {
		t.Errorf("[AC-158-2] the daemon does not say why orphan-t is kept; log:\n%s", readFileString(logPath))
	}
}

// extOwnerApplyYAML declares web-1 with size 2, while external_owners hands
// the fake's resources of observed size 5 to "drs".  Reality has web-1 at 5:
// the intent did not declare that value, so loading passes, and the
// observation selects it.
const extOwnerApplyYAML = `resources:
  - provider: fake
    id: web-1
    scope:
      site: tokyo
    fields:
      name: web-1
      size: "2"
external_owners:
  - owner: drs
    provider: fake
    fields:
      size: 5
`

func TestExternalOwner_NotApplied(t *testing.T) {
	// [AC-158-3]
	intent, state := extOwnerRig(t, extOwnerApplyYAML)
	out, _ := runSakutei(t, "reconcile", "--intent", intent, "--state", state)
	if !strings.Contains(out, "external_owner drs") || !strings.Contains(out, "not applied") {
		t.Errorf("[AC-158-3] reconcile does not say web-1 is held for its external owner:\n%s", out)
	}
	if res := fakeResources(t, state); res["web-1"]["size"] != "5" {
		t.Fatalf("[AC-158-3] reconcile applied a resource external_owners selects: %v\n%s", res, out)
	}
	if out := mustRun(t, "plan", "--intent", intent, "--state", state); strings.Contains(out, "UPDATE fake/site=tokyo/web-1") {
		t.Errorf("[AC-158-3] plan plans an update of a resource external_owners selects:\n%s", out)
	}

	logPath := filepath.Join(t.TempDir(), "daemon.log")
	stop := startPlannedChangeDaemon(t, intent, state, logPath)
	waitForMoreCycles(t, logPath, 3)
	stop()
	if res := fakeResources(t, state); res["web-1"]["size"] != "5" {
		t.Fatalf("[AC-158-3] the daemon applied a resource external_owners selects: %v; log:\n%s", res, readFileString(logPath))
	}
	if !strings.Contains(readFileString(logPath), "external_owner drs") {
		t.Errorf("[AC-158-3] the daemon does not say web-1 is held; log:\n%s", readFileString(logPath))
	}

	// Without the declaration it is applied (the rig really drifts).
	writeStateJSON(t, filepath.Join(intent, "intent.yaml"), strings.SplitN(extOwnerApplyYAML, "external_owners:", 2)[0])
	mustRun(t, "reconcile", "--intent", intent, "--state", state)
	if res := fakeResources(t, state); res["web-1"]["size"] != "2" {
		t.Fatalf("[AC-158-3] without external_owners web-1 should be applied: %v", res)
	}
}

// pveExtIntent writes an intent with the given top-level JSON members
// (resources, external_owners, ...) and returns the CLI arguments.
func pveExtIntent(t *testing.T, cfg, members string) []string {
	t.Helper()
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(intentDir, "vms.json"), []byte("{"+members+"}"), 0o644); err != nil {
		t.Fatal(err)
	}
	pc := filepath.Join(tmp, "provider-config.json")
	writeJSONFile(t, pc, map[string]any{"providers": map[string]any{"proxmox-vm": map[string]any{"proxmox_config": cfg}}})
	return []string{"--intent", intentDir, "--state", filepath.Join(tmp, "fake.json"), "--provider-config", pc,
		"--alloc-store", filepath.Join(tmp, "alloc.json")}
}

// pveStage0Owners writes the config's protected VMs (vmid 100, sakutei-ctl)
// in the intent.
const pveStage0Owners = `"external_owners": [
  {"owner": "stage0", "provider": "proxmox-vm", "id": "lab/sakutei-ctl"},
  {"owner": "stage0", "provider": "proxmox-vm", "id_prefix": "lab/", "fields": {"vmid": 100}}`

func TestExternalOwner_ProxmoxVM(t *testing.T) {
	// [AC-158-4]
	srv, cfg := pveRig(t)
	srv.AddVM(pvetest.VM{VMID: 101, Node: "pve1", Name: "legacy", Status: "running", Config: map[string]string{
		"cores": "2", "memory": "2048", "scsi0": "local-lvm:vm-101-disk-0,size=16G",
	}})
	bin := buildBinary(t)
	legacy := `{"provider": "proxmox-vm", "id": "lab/legacy", "fields": {"memory": "4096"}}`

	// The config's exclusions only in the config: a warning for each.
	common := pveExtIntent(t, cfg, `"resources": [`+legacy+`]`)
	o, code := runBin(t, bin, append([]string{"plan"}, common...)...)
	if code != 0 || !strings.Contains(o, "protected_vmids") || !strings.Contains(o, "protected_names") || !strings.Contains(o, "write it in the intent too") {
		t.Fatalf("[AC-158-4] plan with the exclusions only in the provider config: exit %d, want a warning for each:\n%s", code, o)
	}

	// Written in the intent too: no warning.  legacy (vmid 101) is handed to
	// "legacy-team" by its observed vmid: observed, not applied.
	owners := pveStage0Owners + `,
  {"owner": "legacy-team", "provider": "proxmox-vm", "id_prefix": "lab/", "fields": {"vmid": 101}}]`
	common = pveExtIntent(t, cfg, `"resources": [`+legacy+`], `+owners)
	o, code = runBin(t, bin, append([]string{"plan"}, common...)...)
	if code != 0 || strings.Contains(o, "write it in the intent too") || strings.Contains(o, "UPDATE") {
		t.Fatalf("[AC-158-4] plan with the exclusions in the intent: exit %d, want no warning and no UPDATE of legacy:\n%s", code, o)
	}
	o, code = runBin(t, bin, append([]string{"reconcile"}, common...)...)
	if !strings.Contains(o, "external_owner legacy-team") {
		t.Errorf("[AC-158-4] reconcile does not say legacy is held (exit %d):\n%s", code, o)
	}
	if vm, _ := srv.VM(101); vm.Config["memory"] != "2048" || len(vm.Pending) != 0 {
		t.Fatalf("[AC-158-4] reconcile changed a VM external_owners selects: %+v", vm)
	}

	// Declaring it with the selected vmid is refused.
	common = pveExtIntent(t, cfg, `"resources": [{"provider": "proxmox-vm", "id": "lab/legacy", "fields": {"vmid": "101", "memory": "4096"}}], `+owners)
	for _, verb := range []string{"plan", "reconcile"} {
		if o, code := runBin(t, bin, append([]string{verb}, common...)...); code == 0 || !strings.Contains(o, "external_owner legacy-team") {
			t.Errorf("[AC-158-4] %s declaring a VM external_owners selects: exit %d, want a refusal:\n%s", verb, code, o)
		}
	}

	// The provider config excludes vmid 100 / sakutei-ctl, the intent manages
	// it: refused (not settled on the weaker side), with or without the
	// intent's own external_owners.
	for name, res := range map[string]string{
		"by name": `{"provider": "proxmox-vm", "id": "lab/sakutei-ctl", "fields": {"memory": "4096"}}`,
		"by vmid": `{"provider": "proxmox-vm", "id": "lab/other", "fields": {"vmid": "100", "memory": "4096"}}`,
	} {
		for _, members := range []string{`"resources": [` + res + `]`, `"resources": [` + res + `], ` + pveStage0Owners + `]`} {
			common = pveExtIntent(t, cfg, members)
			for _, verb := range []string{"plan", "reconcile"} {
				o, code := runBin(t, bin, append([]string{verb}, common...)...)
				if code == 0 || !(strings.Contains(o, "protected") || strings.Contains(o, "external_owner")) {
					t.Errorf("[AC-158-4] %s: %s managing what the provider config excludes: exit %d, want a refusal:\n%s", name, verb, code, o)
				}
			}
		}
	}
	if vm, _ := srv.VM(100); vm.Config["memory"] != "2048" {
		t.Errorf("[AC-158-4] the protected VM changed: %v", vm.Config)
	}
}
