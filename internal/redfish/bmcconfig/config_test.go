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

package bmcconfig

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/redfish"
	"github.com/tjst-t/sakutei/internal/redfish/redfishtest"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const store = `{"secrets": {"bmc/n1": {"gen": 2, "value": "pw1"}, "bmc/n2": {"gen": 1, "value": "pw2"}}}`

func TestLoad_YAMLAndJSON(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "secrets.json", store)
	y := writeFile(t, dir, "bmcs.yaml", `secret_store: secrets.json
bmcs:
  n1:
    address: https://10.0.0.1
    system: /redfish/v1/Systems/1
    manager: /redfish/v1/Managers/1
    username: admin
    password: secretref:file/bmc/n1#gen2
    ca_file: ca.pem
    timeout: 3s
  n2:
    address: 10.0.0.2
    username: root
    password:
      $secret: bmc/n2
      backend: file
    insecure_skip_verify: true
`)
	j := writeFile(t, dir, "bmcs.json", `{"secret_store": "secrets.json", "bmcs": {
  "n1": {"address": "https://10.0.0.1", "system": "/redfish/v1/Systems/1", "manager": "/redfish/v1/Managers/1",
         "username": "admin", "password": "secretref:file/bmc/n1#gen2", "ca_file": "ca.pem", "timeout": "3s"},
  "n2": {"address": "10.0.0.2", "username": "root", "password": {"$secret": "bmc/n2", "backend": "file"},
         "insecure_skip_verify": true}}}`)
	for _, p := range []string{y, j} {
		cfg, err := Load(p)
		if err != nil {
			t.Fatalf("Load(%s): %v", p, err)
		}
		if cfg.SecretStore != filepath.Join(dir, "secrets.json") {
			t.Errorf("%s: secret_store = %q", p, cfg.SecretStore)
		}
		n1, n2 := cfg.BMCs["n1"], cfg.BMCs["n2"]
		if n1.Address != "https://10.0.0.1" || n1.SystemPath != "/redfish/v1/Systems/1" || n1.ManagerPath != "/redfish/v1/Managers/1" ||
			n1.Username != "admin" || n1.CAFile != filepath.Join(dir, "ca.pem") || n1.Timeout != 3*time.Second || n1.InsecureSkipVerify {
			t.Errorf("%s: n1 = %+v", p, n1)
		}
		if n1.Password.String() != "secretref:file/bmc/n1#gen2" || n2.Password.String() != "secretref:file/bmc/n2#gen0" {
			t.Errorf("%s: passwords = %s, %s", p, n1.Password, n2.Password)
		}
		if !n2.InsecureSkipVerify || n2.CAFile != "" || n2.Timeout != 0 {
			t.Errorf("%s: n2 = %+v", p, n2)
		}
	}
}

func TestLoad_Errors(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "secrets.json", store)
	bmc := func(extra string) string {
		return "secret_store: secrets.json\nbmcs:\n  n1:\n    address: https://10.0.0.1\n    username: admin\n" + extra
	}
	for _, c := range []struct {
		body, want string
	}{
		{bmc("    password: pw1\n"), "must be a SecretRef"},
		{bmc("    password: secretref:vault/bmc/n1\n"), "only the file SecretBackend"},
		{bmc("    password: secretref:file/bmc/none\n"), "not in the secret store"},
		{bmc(""), "password is required"},
		{bmc("    password: secretref:file/bmc/n1\n    insecure_skip_verify: yes\n"), "want true or false"},
		{bmc("    password: secretref:file/bmc/n1\n    insecure_skip_verify: true\n    ca_file: ca.pem\n"), "exclusive"},
		{bmc("    password: secretref:file/bmc/n1\n    timeout: soon\n"), "positive duration"},
		{bmc("    password: secretref:file/bmc/n1\n    colour: red\n"), `unknown key "bmcs.n1.colour"`},
		{"secret_store: secrets.json\nbmcs:\n  n1:\n    username: admin\n    password: secretref:file/bmc/n1\n", "address is required"},
		{"bmcs:\n  n1:\n    address: https://x\n    username: admin\n    password: secretref:file/bmc/n1\n", "secret_store"},
		{"secret_store: secrets.json\nnodes:\n  n1: x\n", `unknown key "nodes"`},
	} {
		p := writeFile(t, dir, "bmcs.yaml", c.body)
		_, err := Load(p)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Load(%q) = %v, want an error containing %q", c.body, err, c.want)
		}
		if err != nil && strings.Contains(err.Error(), "pw1") {
			t.Errorf("the error repeats a password: %v", err)
		}
	}
}

// password は要求のたびに store から解決する（store を書き換えれば次の要求から
// 新しい値、持ち続けない）。
func TestClients_ResolvesPasswordPerRequest(t *testing.T) {
	srv := redfishtest.New(redfishtest.Options{Password: "pw1"})
	defer srv.Close()
	dir := t.TempDir()
	if _, err := srv.WriteCAFile(dir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "secrets.json", store)
	p := writeFile(t, dir, "bmcs.yaml", "secret_store: secrets.json\nbmcs:\n  n1:\n    address: "+srv.URL+
		"\n    username: admin\n    password: secretref:file/bmc/n1\n    ca_file: ca.pem\n")
	cs, err := LoadClients(p)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := cs["n1"].GetSystem(ctx); err != nil {
		t.Fatalf("GetSystem: %v", err)
	}
	writeFile(t, dir, "secrets.json", `{"secrets": {"bmc/n1": {"gen": 3, "value": "rotated"}}}`)
	if _, err := cs["n1"].GetSystem(ctx); !redfish.IsAuth(err) {
		t.Fatalf("after the rotation in the store: %v, want the BMC to reject the new password", err)
	}
	if err := os.Remove(filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := cs["n1"].GetSystem(ctx); !redfish.IsUnreachable(err) {
		t.Fatalf("store gone: %v, want unreachable (no operation)", err)
	}
}
