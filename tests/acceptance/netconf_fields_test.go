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

// netconf_fields_test.go — #211: netconf の宣言に provider の知らない欄が
// あれば、読み込みの時点で欄の名前とリソースの ID を挙げて断る（drift にして
// Apply の「the operation carries no config」で断るのではなく）。
//
//	[AC-211-1-1] validate（ValidateIntent）: 知らない欄（name など）は欄の名前・
//	             ID・直し方（欄を外す）つきで断る。観測だけの欄（capabilities）も
//	             断る。知っている欄だけの宣言は今どおり通る
//	[AC-211-1-2] CLI の plan と reconcile: 知らない欄を含む宣言は読み込みで
//	             欄の名前つきで断られ、機器には何も送らない。欄を外せば今どおり
package acceptance

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/netconf"
	"github.com/tjst-t/sakutei/v2/internal/netconf/netconffake"
)

func TestNetconfFields_ValidateRefusesUnknown(t *testing.T) {
	// [AC-211-1-1]
	nc := ncAttrProvider(t, netconffake.New(ncRunning, true))
	const id = "sw1/if-uplink"
	err := nc.ValidateIntent(id, map[string]string{"config": ncDeclared, "name": "uplink", "vendor": "x"})
	if err == nil {
		t.Fatal("[AC-211-1-1] unknown fields were accepted")
	}
	for _, want := range []string{`"name"`, `"vendor"`, id, "unknown field", "remove them from the fields"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("[AC-211-1-1] %q lacks %q", err, want)
		}
	}
	err = nc.ValidateIntent(id, map[string]string{"config": ncDeclared, "capabilities": "base:1.1"})
	if err == nil || !strings.Contains(err.Error(), `"capabilities"`) || !strings.Contains(err.Error(), "observed only") {
		t.Fatalf("[AC-211-1-1] a computed field: %v", err)
	}
	// Only known fields: accepted as before.
	for _, f := range []map[string]string{
		{"config": ncDeclared},
		{"config": ncDeclared, "ordered": "", "host": "sw1", "subtree": "if-uplink"},
	} {
		if err := nc.ValidateIntent(id, f); err != nil {
			t.Fatalf("[AC-211-1-1] known fields %v: %v", f, err)
		}
	}
}

func TestNetconfFields_CLIRefusesUnknown(t *testing.T) {
	// [AC-211-1-2]
	bin := buildBinary(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	initStateFile(t, statePath)
	runningPath := filepath.Join(tmp, "running.xml")
	opsPath := filepath.Join(tmp, "ops.log")
	writeFile := func(path, body string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(runningPath, ncRunning, 0o600)
	writeFile(filepath.Join(tmp, "ssh"), ncFakeSSH, 0o755)
	writeFile(filepath.Join(tmp, "known_hosts"), "", 0o600)
	ctrl, err := os.MkdirTemp("/tmp", "snc-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(ctrl)
	writeFile(filepath.Join(tmp, "hosts.yaml"), "ssh: ./ssh\nknown_hosts: known_hosts\ncontrol_dir: "+ctrl+"\nhosts:\n  sw1:\n    address: 192.0.2.1\n    port: 830\n    user: netconf\n    connect_timeout: 1s\n", 0o600)
	subtreesBody, _ := json.Marshal(map[string]any{"subtrees": map[string]any{"if-uplink": map[string]string{"filter": ncFilter}}})
	writeFile(filepath.Join(tmp, "subtrees.json"), string(subtreesBody), 0o600)
	writeJSONFile(t, filepath.Join(tmp, "providers.json"), map[string]any{"providers": map[string]any{
		netconf.Name: map[string]string{
			"remote_config":    filepath.Join(tmp, "hosts.yaml"),
			"netconf_hosts":    "sw1",
			"netconf_subtrees": filepath.Join(tmp, "subtrees.json"),
		},
	}})
	common := []string{"--intent", intentDir, "--state", statePath, "--provider-config", filepath.Join(tmp, "providers.json")}
	run := func(args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(bin, append(args, common...)...)
		cmd.Dir = tmp
		cmd.Env = append(os.Environ(), ncBinEnv+"="+self, ncRunningEnv+"="+runningPath, ncOpsEnv+"="+opsPath, ncDownEnv+"="+filepath.Join(tmp, "down"))
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out.String(), ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return out.String(), 0
	}
	intent := func(fields map[string]string) {
		writeIntentFile(t, intentDir, map[string]any{"resources": []map[string]any{
			{"provider": netconf.Name, "id": "sw1/if-uplink", "fields": fields},
		}})
	}

	intent(map[string]string{"config": ncDeclared, "name": "uplink"})
	for _, verb := range []string{"plan", "reconcile"} {
		out, code := run(verb)
		if code == 0 || !strings.Contains(out, `unknown field(s) ["name"]`) || !strings.Contains(out, "sw1/if-uplink") ||
			!strings.Contains(out, "remove them from the fields") || strings.Contains(out, "carries no config") {
			t.Fatalf("[AC-211-1-2] %s: exit %d\n%s", verb, code, out)
		}
	}
	if b, err := os.ReadFile(opsPath); err == nil && len(bytes.TrimSpace(b)) != 0 {
		t.Fatalf("[AC-211-1-2] the device received rpcs while the intent was refused:\n%s", b)
	}

	// Without the unknown field: as before.
	intent(map[string]string{"config": ncDeclared})
	for _, verb := range []string{"plan", "reconcile"} {
		if out, code := run(verb); code != 0 || strings.Contains(out, "unknown field") {
			t.Fatalf("[AC-211-1-2] %s with known fields: exit %d\n%s", verb, code, out)
		}
	}
	if out, _ := run("plan"); !strings.Contains(out, "Converged") {
		t.Fatalf("[AC-211-1-2] plan with known fields:\n%s", out)
	}
}
