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

package proxmox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const testStore = `{"secrets": {"proxmox/lab": {"gen": 1, "value": "very-secret-token"}}}`

func TestLoadConfig(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"secrets.json": testStore,
		"pve.yaml": `secret_store: secrets.json
clusters:
  lab:
    endpoint: https://pve1.example.net:8006
    api_base_path: /mock/api2/json/
    token_id: sakutei@pve!sakutei
    token_secret: secretref:file/proxmox/lab
    node: pve1
    scope: site=tokyo
    protected_vmids:
      - 100
      - "101"
    protected_names:
      - sakutei-ctl
    insecure_skip_verify: true
    timeout: 5s
    task_wait: 0s
  dc2:
    endpoint: https://pve.dc2.example.net:8006
    token_id: sakutei@pve!sakutei
    token_secret:
      $secret: proxmox/lab
      backend: file
`,
	})
	cfg, err := LoadConfig(filepath.Join(dir, "pve.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	lab := cfg.Clusters["lab"]
	if lab.Node != "pve1" || lab.Scope.Labels["site"] != "tokyo" || !lab.InsecureSkipVerify || lab.Timeout != 5*time.Second || lab.TaskWait != 0 {
		t.Errorf("lab = %+v", lab)
	}
	if len(lab.ProtectedVMIDs) != 2 || lab.ProtectedVMIDs[1] != 101 || lab.ProtectedNames[0] != "sakutei-ctl" {
		t.Errorf("lab protected = %v %v", lab.ProtectedVMIDs, lab.ProtectedNames)
	}
	if d := cfg.Clusters["dc2"]; d.TaskWait != DefaultTaskWait || d.TokenSecret.Path() != "proxmox/lab" {
		t.Errorf("dc2 = %+v", d)
	}
	p, err := NewFromConfig(filepath.Join(dir, "pve.yaml"))
	if err != nil || len(p.clusters) != 2 {
		t.Fatalf("NewFromConfig = %v, %v", p, err)
	}
	// API のパスの前置き：書けばその値、省けば /api2/json（#160）。
	if lab.APIBasePath != "/mock/api2/json/" || cfg.Clusters["dc2"].APIBasePath != DefaultAPIBasePath {
		t.Errorf("api_base_path = %q, %q", lab.APIBasePath, cfg.Clusters["dc2"].APIBasePath)
	}
	if b := p.clusters["lab"].Client.base; b != "https://pve1.example.net:8006/mock/api2/json" {
		t.Errorf("lab base = %q", b)
	}
	if b := p.clusters["dc2"].Client.base; b != "https://pve.dc2.example.net:8006/api2/json" {
		t.Errorf("dc2 base = %q", b)
	}
	// secret は要求の直前に解決する。
	got, err := p.clusters["lab"].Client.secret(context.Background())
	if err != nil || got != "very-secret-token" {
		t.Errorf("secret = %q, %v", got, err)
	}
}

func TestLoadConfig_Errors(t *testing.T) {
	cases := []struct {
		name, cluster, want string
	}{
		{"raw secret", "    endpoint: https://h:8006\n    token_id: a@pve!b\n    token_secret: plain-value-xyz\n", "must be a SecretRef"},
		{"missing secret path", "    endpoint: https://h:8006\n    token_id: a@pve!b\n    token_secret: secretref:file/nope\n", "is not in the secret store"},
		{"unknown key", "    endpoint: https://h:8006\n    token_id: a@pve!b\n    token_secret: secretref:file/proxmox/lab\n    nodes: x\n", `unknown key "clusters.lab.nodes"`},
		{"no endpoint", "    token_id: a@pve!b\n    token_secret: secretref:file/proxmox/lab\n", "endpoint is required"},
		{"no token id", "    endpoint: https://h:8006\n    token_secret: secretref:file/proxmox/lab\n", "token_id is required"},
		{"bad vmid", "    endpoint: https://h:8006\n    token_id: a@pve!b\n    token_secret: secretref:file/proxmox/lab\n    protected_vmids:\n      - abc\n", "want a VM id"},
		{"bad api_base_path", "    endpoint: https://h:8006\n    api_base_path: api2/json\n    token_id: a@pve!b\n    token_secret: secretref:file/proxmox/lab\n", `clusters.lab.api_base_path "api2/json": want a path`},
		{"api_base_path with ..", "    endpoint: https://h:8006\n    api_base_path: /a/../b\n    token_id: a@pve!b\n    token_secret: secretref:file/proxmox/lab\n", "segments are not allowed"},
		{"ca and insecure", "    endpoint: https://h:8006\n    token_id: a@pve!b\n    token_secret: secretref:file/proxmox/lab\n    ca_file: ca.pem\n    insecure_skip_verify: true\n", "exclusive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeFiles(t, map[string]string{
				"secrets.json": testStore,
				"pve.yaml":     "secret_store: secrets.json\nclusters:\n  lab:\n" + tc.cluster,
			})
			_, err := LoadConfig(filepath.Join(dir, "pve.yaml"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "plain-value-xyz") {
				t.Errorf("the error shows the raw secret: %v", err)
			}
		})
	}
}
