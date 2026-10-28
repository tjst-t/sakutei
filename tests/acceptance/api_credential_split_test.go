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

// api_credential_split_test.go — Backlog #186 (§20.7): the observe and the
// write credential of an HTTP API provider, through the sakutei binary.
// proxmox-vm runs against the fake Proxmox VE (pvetest) that accepts a
// read-only token (PVEAuditor: GET only) and the write token, and counts the
// requests of each.  The other API providers are covered by their unit tests
// and integration tests (tests/integration/{proxmox,redfish,ldap,postgresql,s3}).
//
// AC tags:
//
//	AC-186-7-1  with only the observe token in the store, status, plan and
//	            reconcile --mode alert-only start and observe with the observe
//	            token only; a reconcile that may write refuses to start
//	AC-186-7-2  with both tokens, reconcile converges: the observe token made
//	            reads only, the write token made the writes
package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/proxmox/pvetest"
)

const pveAuditToken = "sakutei@pve!audit"

type pveSplitRig struct {
	t                *testing.T
	bin, dir         string
	srv              *pvetest.Server
	roOnly, bothKeys string // --provider-config
}

func newPVESplitRig(t *testing.T) *pveSplitRig {
	t.Helper()
	r := &pveSplitRig{t: t, bin: buildBinary(t), dir: t.TempDir()}
	r.srv = pvetest.New(pvetest.Options{AuditTokenID: pveAuditToken, AuditSecret: "audit-secret"})
	t.Cleanup(r.srv.Close)
	r.srv.AddVM(pvetest.VM{VMID: 9000, Node: "pve1", Name: "debian-tmpl", Template: true, Config: map[string]string{
		"cores": "1", "memory": "1024", "scsi0": "local-lvm:base-9000-disk-0,size=4G", "net0": "virtio=BC:24:11:00:90:00,bridge=vmbr0",
	}})
	for _, store := range []string{"ro", "both"} {
		secrets := `"pve/audit": {"value": "audit-secret", "gen": 1}`
		if store == "both" {
			secrets += `, "pve/write": {"value": "` + r.srv.Secret() + `", "gen": 1}`
		}
		if err := os.WriteFile(filepath.Join(r.dir, store+"-secrets.json"), []byte(`{"secrets": {`+secrets+`}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		clusters := filepath.Join(r.dir, store+"-pve.json")
		writeJSONFile(t, clusters, map[string]any{
			"secret_store": store + "-secrets.json",
			"clusters": map[string]any{"lab": map[string]any{
				"endpoint": r.srv.URL, "node": "pve1", "task_wait": "5s",
				"observe": map[string]any{"token_id": pveAuditToken, "token_secret": "secretref:file/pve/audit"},
				"write":   map[string]any{"token_id": r.srv.TokenID(), "token_secret": "secretref:file/pve/write"},
			}},
		})
		pc := filepath.Join(r.dir, store+"-providers.json")
		writeJSONFile(t, pc, map[string]any{"providers": map[string]any{"proxmox-vm": map[string]any{"proxmox_config": clusters}}})
		if store == "ro" {
			r.roOnly = pc
		} else {
			r.bothKeys = pc
		}
	}
	intentDir := filepath.Join(r.dir, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIntentFile(t, intentDir, map[string]any{"resources": []map[string]any{{
		"provider": "proxmox-vm", "id": "lab/app1",
		"fields": map[string]string{"vmid": "210", "template": "debian-tmpl", "cores": "2", "memory": "2048",
			"disks": "scsi0 storage=local-lvm size=8G", "nics": "net0 bridge=vmbr0", "power": "running"},
	}}})
	return r
}

func (r *pveSplitRig) run(config string, args ...string) (string, int) {
	r.t.Helper()
	full := append(args, "--intent", filepath.Join(r.dir, "intent"), "--state", filepath.Join(r.dir, "fake.json"), "--provider-config", config)
	return runBin(r.t, r.bin, full...)
}

func TestAPICredentialSplit_ObserveOnlyRunsWithTheObserveToken(t *testing.T) {
	// [AC-186-7-1]
	r := newPVESplitRig(t)
	for _, args := range [][]string{
		{"status", "--json"},
		{"plan"},
		{"reconcile", "--mode", "alert-only"},
	} {
		out, code := r.run(r.roOnly, args...)
		if code != 0 {
			t.Fatalf("[AC-186-7-1] %v with the observe token only: exit %d\n%s", args, code, out)
		}
		if args[0] == "plan" && !strings.Contains(out, "lab/app1") {
			t.Errorf("[AC-186-7-1] plan did not plan the VM:\n%s", out)
		}
	}
	if w := r.srv.TokenCalls(r.srv.TokenID()); len(w) != 0 {
		t.Errorf("[AC-186-7-1] the write token was used: %v", w)
	}
	if a := r.srv.TokenCalls(pveAuditToken); a["GET"] == 0 || a["WRITE"] != 0 {
		t.Errorf("[AC-186-7-1] the observe token: %v; want reads only", a)
	}
	out, code := r.run(r.roOnly, "reconcile")
	if code == 0 || !strings.Contains(out, "clusters.lab.write.token_secret") || !strings.Contains(out, "not in the secret store") {
		t.Errorf("[AC-186-7-1] reconcile (auto) without the write token: exit %d\n%s", code, out)
	}
	if w := r.srv.TokenCalls(r.srv.TokenID()); len(w) != 0 {
		t.Errorf("[AC-186-7-1] the write token was used: %v", w)
	}
	if _, ok := r.srv.VM(210); ok {
		t.Error("[AC-186-7-1] the VM was created in a run that does not write")
	}
}

func TestAPICredentialSplit_ApplyUsesTheWriteToken(t *testing.T) {
	// [AC-186-7-2]
	r := newPVESplitRig(t)
	converged := false
	for i := 0; i < 5 && !converged; i++ {
		out, code := r.run(r.bothKeys, "reconcile")
		if code != 0 {
			t.Fatalf("[AC-186-7-2] reconcile %d: exit %d\n%s", i, code, out)
		}
		converged = strings.Contains(out, "Converged (no changes)")
	}
	if !converged {
		t.Fatal("[AC-186-7-2] not converged after 5 reconciles")
	}
	if _, ok := r.srv.VM(210); !ok {
		t.Fatal("[AC-186-7-2] the VM was not created")
	}
	if a := r.srv.TokenCalls(pveAuditToken); a["GET"] == 0 || a["WRITE"] != 0 {
		t.Errorf("[AC-186-7-2] the observe token: %v; want reads only", a)
	}
	if w := r.srv.TokenCalls(r.srv.TokenID()); w["WRITE"] == 0 {
		t.Errorf("[AC-186-7-2] the write token: %v; want the writes", w)
	}
}
