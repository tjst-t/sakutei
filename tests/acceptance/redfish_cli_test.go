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

// redfish_cli_test.go — #83: Redfish の provider を CLI（plan → reconcile）に
// つなぐ。BMC の一覧は --redfish-config、パスワードは SecretRef（file
// SecretBackend）でしか渡せない（§29.1、§20）。
//
//	[AC-83-1]  plan は redfish のシステム・仮想メディアと dnszone を混ぜた
//	           intent の差分を出し、BMC に書かない。
//	[AC-83-2]  reconcile が偽 BMC の電源・起動先・仮想メディアと zone を収束させ、
//	           続く plan は Converged。
//	[AC-83-3]  パスワードを生の文字列で書いた設定はエラー（値は出さない）。
//	[AC-83-4]  設定に無いノードを intent に書くと plan の時点でエラー。
//	[AC-83-5]  --redfish-config 無しで redfish のリソースを書くとエラー。
//	[AC-83-6]  パスワードの平文は CLI のどの出力にも出ない。
package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/redfish/redfishtest"
	"github.com/tjst-t/sakutei/v2/internal/redfish/system"
)

const rfCLIPassword = "Sup3r-s3cret-BMC"

type rfCLIRig struct {
	bmc        *redfishtest.Server
	dir        string
	intent     string
	state      string
	zone       string
	config     string
	allOutputs []string
}

func newRFCLIRig(t *testing.T) *rfCLIRig {
	t.Helper()
	bmc := redfishtest.New(redfishtest.Options{Password: rfCLIPassword})
	t.Cleanup(bmc.Close)
	dir := t.TempDir()
	if _, err := bmc.WriteCAFile(dir); err != nil {
		t.Fatal(err)
	}
	r := &rfCLIRig{
		bmc:    bmc,
		dir:    dir,
		intent: filepath.Join(dir, "intent"),
		state:  filepath.Join(dir, "fake-state.json"),
		zone:   filepath.Join(dir, "example.com.zone"),
		config: filepath.Join(dir, "bmcs.yaml"),
	}
	if err := os.MkdirAll(r.intent, 0o755); err != nil {
		t.Fatal(err)
	}
	writeZoneFile(t, r.zone, multiZone)
	r.write(t, "secrets.json", `{"secrets": {"bmc/node1": {"gen": 1, "value": "`+rfCLIPassword+`"}}}`)
	r.writeConfig(t, "secretref:file/bmc/node1")
	return r
}

func (r *rfCLIRig) write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeConfig は BMC の一覧を YAML で書く（相対パスは設定ファイルの場所から）。
func (r *rfCLIRig) writeConfig(t *testing.T, password string) {
	t.Helper()
	r.write(t, "bmcs.yaml", `# BMC の一覧（#83）
secret_store: secrets.json
bmcs:
  node1:
    address: `+r.bmc.URL+`
    system: /redfish/v1/Systems/1
    username: admin
    password: "`+password+`"
    ca_file: ca.pem
    timeout: 5s
`)
}

func (r *rfCLIRig) writeIntent(t *testing.T, node string) {
	t.Helper()
	writeIntentFile(t, r.intent, map[string]any{
		"resources": []map[string]any{
			{
				"provider": system.Name,
				"id":       node,
				"fields": map[string]string{
					"power": "Off", "power_off": "graceful",
					"boot_enabled": "Continuous", "boot_target": "Pxe",
				},
			},
			{
				"provider": system.MediaName,
				"id":       system.MediaID(node),
				"fields":   map[string]string{"image": "https://repo.example/install.iso"},
			},
			dnszoneResource("www.example.com./A", "www.example.com.", "A", "192.0.2.10", "300", ""),
		},
	})
}

func (r *rfCLIRig) run(t *testing.T, verb string, extra ...string) (string, int) {
	t.Helper()
	args := append([]string{verb, "--intent", r.intent, "--state", r.state, "--zone", r.zone}, extra...)
	out, code := runSakutei(t, args...)
	r.allOutputs = append(r.allOutputs, out)
	return out, code
}

func (r *rfCLIRig) bmcWrites() []string {
	var w []string
	for _, q := range r.bmc.Requests() {
		if q.Method != "GET" {
			w = append(w, q.Method+" "+q.Path)
		}
	}
	return w
}

func TestRedfishCLI_PlanReconcileConverges(t *testing.T) {
	r := newRFCLIRig(t)
	r.writeIntent(t, "node1")

	// [AC-83-1]
	out, code := r.run(t, "plan", "--redfish-config", r.config)
	if code != 0 {
		t.Fatalf("[AC-83-1] plan exit %d:\n%s", code, out)
	}
	for _, want := range []string{system.Name, system.MediaName, "dnszone", "install.iso"} {
		if !strings.Contains(out, want) {
			t.Errorf("[AC-83-1] plan output does not mention %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Converged (no changes)") {
		t.Fatalf("[AC-83-1] plan reports no changes:\n%s", out)
	}
	if w := r.bmcWrites(); len(w) != 0 {
		t.Fatalf("[AC-83-1] plan wrote to the BMC: %v", w)
	}

	// [AC-83-2]
	out, code = r.run(t, "reconcile", "--redfish-config", r.config)
	if code != 0 {
		t.Fatalf("[AC-83-2] reconcile exit %d:\n%s", code, out)
	}
	if got := r.bmc.PowerState(); got != "Off" {
		t.Errorf("[AC-83-2] PowerState = %q, want Off\n%s", got, out)
	}
	if en, tg := r.bmc.Boot(); en != "Continuous" || tg != "Pxe" {
		t.Errorf("[AC-83-2] Boot = %s/%s, want Continuous/Pxe", en, tg)
	}
	if in, img := r.bmc.Media(); !in || img != "https://repo.example/install.iso" {
		t.Errorf("[AC-83-2] media = %v %q, want the install.iso inserted", in, img)
	}
	if z := readZoneFile(t, r.zone); !zoneHasA(z, "www.example.com.", "192.0.2.10") {
		t.Errorf("[AC-83-2] zone did not converge:\n%s", z)
	}
	out, code = r.run(t, "plan", "--redfish-config", r.config)
	if code != 0 || !strings.Contains(out, "Converged (no changes)") {
		t.Fatalf("[AC-83-2] plan after reconcile (exit %d) is not converged:\n%s", code, out)
	}

	// [AC-83-6]
	for _, o := range r.allOutputs {
		if strings.Contains(o, rfCLIPassword) {
			t.Fatalf("[AC-83-6] the password leaked into the CLI output:\n%s", o)
		}
	}
}

func TestRedfishCLI_ConfigErrors(t *testing.T) {
	r := newRFCLIRig(t)
	r.writeIntent(t, "node1")

	// [AC-83-3]
	r.writeConfig(t, rfCLIPassword)
	out, code := r.run(t, "plan", "--redfish-config", r.config)
	if code == 0 || !strings.Contains(out, "must be a SecretRef") {
		t.Fatalf("[AC-83-3] a raw password: exit %d, want an error saying it must be a SecretRef:\n%s", code, out)
	}
	if strings.Contains(out, rfCLIPassword) {
		t.Fatalf("[AC-83-3] the error repeats the raw password:\n%s", out)
	}
	r.writeConfig(t, "secretref:file/bmc/absent")
	if out, code := r.run(t, "plan", "--redfish-config", r.config); code == 0 || !strings.Contains(out, "not in the secret store") {
		t.Fatalf("[AC-83-3] an unknown secret path: exit %d:\n%s", code, out)
	}
	r.writeConfig(t, "secretref:file/bmc/node1")

	// [AC-83-4]
	r.writeIntent(t, "node9")
	out, code = r.run(t, "plan", "--redfish-config", r.config)
	if code == 0 || !strings.Contains(out, `no BMC is configured for node "node9"`) {
		t.Fatalf("[AC-83-4] a node missing from the config: exit %d:\n%s", code, out)
	}
	if out, code := r.run(t, "reconcile", "--redfish-config", r.config); code == 0 {
		t.Fatalf("[AC-83-4] reconcile accepted a node missing from the config:\n%s", out)
	}
	if w := r.bmcWrites(); len(w) != 0 {
		t.Fatalf("[AC-83-4] wrote to the BMC: %v", w)
	}

	// [AC-83-5]
	r.writeIntent(t, "node1")
	if out, code := r.run(t, "plan"); code == 0 || !strings.Contains(out, "--redfish-config") {
		t.Fatalf("[AC-83-5] no --redfish-config: exit %d:\n%s", code, out)
	}

	// [AC-83-6]
	for _, o := range r.allOutputs {
		if strings.Contains(o, rfCLIPassword) {
			t.Fatalf("[AC-83-6] the password leaked into the CLI output:\n%s", o)
		}
	}
}
