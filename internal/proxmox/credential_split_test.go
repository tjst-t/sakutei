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

package proxmox_test

// credential_split_test.go — 観測用と書き込み用のトークン（§20.7、#186）。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/cause"
	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/proxmox"
	"github.com/tjst-t/sakutei/v2/internal/proxmox/pvetest"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

const auditToken = "sakutei@pve!audit"

// splitRig は読むだけのトークン（PVEAuditor）と書き込み用のトークンを受ける偽の
// Proxmox と、その 2 本を observe:・write: に書いたクラスタの一覧。
func splitRig(t *testing.T, store string) (*pvetest.Server, string) {
	t.Helper()
	srv := pvetest.New(pvetest.Options{AuditTokenID: auditToken, AuditSecret: "audit-secret"})
	t.Cleanup(srv.Close)
	srv.AddVM(pvetest.VM{VMID: 9000, Node: "pve1", Name: "debian-tmpl", Template: true, Config: map[string]string{
		"cores": "1", "memory": "1024", "scsi0": "local-lvm:base-9000-disk-0,size=4G", "net0": "virtio=BC:24:11:00:90:00,bridge=vmbr0",
	}})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "secrets.json"), []byte(store), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := "secret_store: secrets.json\nclusters:\n  lab:\n    endpoint: " + srv.URL + "\n    node: pve1\n" +
		"    observe:\n      token_id: " + auditToken + "\n      token_secret: secretref:file/pve/audit\n" +
		"    write:\n      token_id: " + srv.TokenID() + "\n      token_secret: secretref:file/pve/write\n"
	path := filepath.Join(dir, "pve.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return srv, path
}

func splitWant() map[string]string {
	return map[string]string{
		proxmox.FieldVMID: "210", proxmox.FieldTemplate: "debian-tmpl", proxmox.FieldCores: "2", proxmox.FieldMemory: "2048",
		proxmox.FieldDisks: "scsi0 storage=local-lvm size=8G", proxmox.FieldNICs: "net0 bridge=vmbr0", proxmox.FieldPower: "running",
	}
}

func splitPlan(t *testing.T, p *proxmox.Provider) (core.Plan, core.Observation) {
	t.Helper()
	obs, err := p.Read(context.Background(), "lab/app1")
	if err != nil || obs.Availability != core.AvailabilityOk {
		t.Fatalf("Read with the observe token: %v %+v", err, obs)
	}
	pl, err := diff.ForProvider(p).Plan(obs, diff.DesiredResource{ID: id("lab/app1"), Fields: splitWant()},
		cause.CauseContext{ResourceObservedAbsent: obs.State == nil})
	if err != nil {
		t.Fatal(err)
	}
	return pl, obs
}

func TestCredentialSplit_ObserveWithAuditTokenWriteWithWriteToken(t *testing.T) {
	// [AC-186-2-1]
	srv, path := splitRig(t, `{"secrets": {"pve/audit": {"value": "audit-secret"}, "pve/write": {"value": "test-secret"}}}`)
	p, err := proxmox.NewFromProviderConfig(map[string]string{"proxmox_config": path})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		pl, _ := splitPlan(t, p)
		if len(pl.Ops) == 0 {
			break
		}
		if err := p.Apply(provider.WithWrite(context.Background()), pl); err != nil {
			t.Fatalf("Apply with the write token: %v", err)
		}
	}
	if pl, _ := splitPlan(t, p); len(pl.Ops) != 0 {
		t.Fatalf("not converged: %+v", pl.Ops)
	}
	if _, err := p.List(context.Background(), core.NewScope(nil)); err != nil {
		t.Fatalf("List with the observe token: %v", err)
	}
	audit, write := srv.TokenCalls(auditToken), srv.TokenCalls(srv.TokenID())
	if audit["GET"] == 0 || audit["WRITE"] != 0 {
		t.Errorf("the audit token: %v; want reads only", audit)
	}
	if write["WRITE"] == 0 {
		t.Errorf("the write token: %v; want the writes", write)
	}
}

func TestCredentialSplit_ObserveOnlyRunNeedsNoWriteSecret(t *testing.T) {
	// [AC-186-2-2]
	srv, path := splitRig(t, `{"secrets": {"pve/audit": {"value": "audit-secret"}}}`)
	// A run that may write refuses to start without the write secret.
	if _, err := proxmox.NewFromProviderConfig(map[string]string{"proxmox_config": path}); err == nil ||
		!strings.Contains(err.Error(), "clusters.lab.write.token_secret") {
		t.Fatalf("a writing run without the write secret: %v", err)
	}
	p, err := proxmox.NewFromProviderConfig(map[string]string{"proxmox_config": path, provider.ConfigAccess: provider.AccessObserve})
	if err != nil {
		t.Fatalf("an observe-only run: %v", err)
	}
	pl, _ := splitPlan(t, p)
	if len(pl.Ops) == 0 {
		t.Fatal("want a plan to create the VM")
	}
	err = p.Apply(provider.WithWrite(context.Background()), pl)
	if !errors.Is(err, provider.ErrNoWriteAccess) {
		t.Fatalf("Apply in an observe-only run: %v", err)
	}
	if w := srv.TokenCalls(srv.TokenID()); len(w) != 0 {
		t.Errorf("the write token was used: %v", w)
	}
	if a := srv.TokenCalls(auditToken); a["WRITE"] != 0 {
		t.Errorf("the audit token tried to write: %v", a)
	}
	if _, err := proxmox.NewFromProviderConfig(map[string]string{"proxmox_config": path, provider.ConfigAccess: "read"}); err == nil {
		t.Error("an unknown access must be refused")
	}
}
