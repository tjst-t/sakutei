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
package loader

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/workflow"
)

const redfishOSInstallDir = "../../docs/trial/redfish-os-install"

// #91: 手順書（docs/trial/redfish-os-install）の intent は、そのまま loader で読める。
func TestRedfishOSInstallTrialIntentLoads(t *testing.T) {
	in, err := LoadIntent(filepath.Join(redfishOSInstallDir, "intent"))
	if err != nil {
		t.Fatalf("手順書の intent が読み込めません: %v", err)
	}
	fields := map[string]map[string]string{}
	for _, r := range in.Resources {
		fields[r.ID.Provider+"/"+r.ID.Local] = r.Fields
	}
	if sys := fields["redfish-system/node1"]; sys["power"] != "On" || sys["boot_enabled"] != "Disabled" {
		t.Errorf("redfish-system node1 = %v", sys)
	}
	if cd, ok := fields["redfish-virtual-media/node1/cd"]; !ok || cd["image"] != "" {
		t.Errorf("redfish-virtual-media node1/cd = %v (want declared, ejected after the install)", cd)
	}
	// #139: 応答ファイル（template-file）は宣言から作る。秘密は書かない。
	if a := fields["template-file/6f1b7c52-0e1a-4d2b-9c11-000000008501"]; a["template"] != "alpine-answers.sh.tmpl" ||
		a["hostname"] != "node1" || a["install_mark"] == "" || a["ssh_authorized_keys"] == "" {
		t.Errorf("template-file answer = %v", a)
	}
	if len(in.Workflows) != 1 {
		t.Fatalf("workflows = %+v", in.Workflows)
	}
	def := in.Workflows[0]
	if def.Name != "os-install" || len(def.Targets()) != 1 || def.Targets()[0].Provider != "redfish-system" || def.Targets()[0].Local != "node1" {
		t.Errorf("workflow = %s, targets = %+v", def.Name, def.Targets())
	}
	// #87: CD を差す → 差さったことを確かめる → #139: 1 回だけ CD から起動 → 再起動 →
	// 電源オフを待つ → 電源 On（ディスクから）→ #86: 入れた OS が目印つきで起動したのを
	// 確かめる → CD を抜く。
	kinds := []workflow.StepKind{workflow.StepApply, workflow.StepRequire, workflow.StepAction, workflow.StepAction,
		workflow.StepWait, workflow.StepApply, workflow.StepRequire, workflow.StepApply}
	if len(def.Steps) != len(kinds) {
		t.Fatalf("steps = %+v", def.Steps)
	}
	for i, k := range kinds {
		if def.Steps[i].Kind != k {
			t.Errorf("steps[%d].kind = %q, want %q", i, def.Steps[i].Kind, k)
		}
	}
	if err := def.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
	const iso = "/iso/sakutei-autoinstall.iso"
	node1 := def.Targets()[0]
	for i, image := range map[int]string{0: iso, 1: iso, 7: ""} {
		id := def.Steps[i].ResourceFor(node1)
		if id.Provider != "redfish-virtual-media" || id.Local != "node1/cd" || def.Steps[i].Fields["image"] != image {
			t.Errorf("steps[%d] = %+v (resource %+v)", i, def.Steps[i], id)
		}
	}
	if def.Steps[1].Timeout != 5*time.Minute {
		t.Errorf("steps[1].timeout = %s", def.Steps[1].Timeout)
	}
	if s := def.Steps[2]; s.Resource != "" || s.Op != "boot_once" || s.Args["target"] != "Cd" {
		t.Errorf("steps[2] = %+v", s)
	}
	if s := def.Steps[3]; s.Resource != "" || s.Op != "reset" || len(s.Args) != 0 {
		t.Errorf("steps[3] = %+v", s)
	}
	if s := def.Steps[4]; s.Fields["power"] != "Off" || s.Timeout != 15*time.Minute {
		t.Errorf("steps[4] = %+v", s)
	}
	if s := def.Steps[5]; len(s.Fields) != 1 || s.Fields["power"] != "On" {
		t.Errorf("steps[5] = %+v", s)
	}
	// #86: 入れた OS（guest-os/node1）が目印つきで起動したのを確かめる。
	if s := def.Steps[6]; s.ResourceFor(node1).Provider != "guest-os" || s.ResourceFor(node1).Local != "node1" ||
		s.Fields["os_id"] != "alpine" || s.Fields["marker.installed"] == "" || s.Fields["marker.hostname"] != "node1" || s.Timeout != 10*time.Minute {
		t.Errorf("steps[6] = %+v", s)
	}
	// Workflow が触るのは node1 と node1/cd と guest-os の node1（done まで収束ループが触らない）。
	if res := def.Resources(); len(res) != 3 || res[1].Local != "node1/cd" || res[2].Provider != "guest-os" {
		t.Errorf("Resources = %+v", res)
	}
}

// #91: 手順書の README に載せた intent は intent/os.yaml と同じ（食い違わない）。
func TestRedfishOSInstallTrialReadmeMatchesIntent(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join(redfishOSInstallDir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	intent, err := os.ReadFile(filepath.Join(redfishOSInstallDir, "intent", "os.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// os.yaml の先頭のコメント（ファイルの説明）を除いた本体が README の yaml のブロックにある
	body := string(intent)
	if i := strings.Index(body, "resources:"); i >= 0 {
		body = body[i:]
	}
	if !strings.Contains(string(readme), "```yaml\n"+body+"```") {
		t.Error("README.md の intent の例が intent/os.yaml と食い違っています")
	}
}
