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

	"github.com/tjst-t/sakutei/internal/workflow"
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
	if cd := fields["redfish-virtual-media/node1/cd"]; cd["image"] != "/iso/sakutei-autoinstall.iso" {
		t.Errorf("redfish-virtual-media node1/cd = %v", cd)
	}
	if len(in.Workflows) != 1 {
		t.Fatalf("workflows = %+v", in.Workflows)
	}
	def := in.Workflows[0]
	if def.Name != "os-install" || len(def.Targets()) != 1 || def.Targets()[0].Provider != "redfish-system" || def.Targets()[0].Local != "node1" {
		t.Errorf("workflow = %s, targets = %+v", def.Name, def.Targets())
	}
	kinds := []workflow.StepKind{workflow.StepApply, workflow.StepWait, workflow.StepApply}
	if len(def.Steps) != len(kinds) {
		t.Fatalf("steps = %+v", def.Steps)
	}
	for i, k := range kinds {
		if def.Steps[i].Kind != k {
			t.Errorf("steps[%d].kind = %q, want %q", i, def.Steps[i].Kind, k)
		}
	}
	s1, s2, s3 := def.Steps[0].Fields, def.Steps[1], def.Steps[2].Fields
	if s1["boot_enabled"] != "Continuous" || s1["boot_target"] != "Cd" || s1["power"] != "On" {
		t.Errorf("steps[0] = %v", s1)
	}
	if s2.Fields["power"] != "Off" || s2.Timeout != 15*time.Minute {
		t.Errorf("steps[1] = %+v", s2)
	}
	if s3["boot_enabled"] != "Disabled" || s3["power"] != "On" {
		t.Errorf("steps[2] = %v", s3)
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
