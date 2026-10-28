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
)

func TestApplyCheck_Load(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte(`version: "1"
apply_check:
  - provider: redfish
    settle: 60s
    max_ineffective_applies: 4
resources:
  - provider: redfish
    id: node1
    apply_check:
      max_ineffective_duration: 10m
    fields:
      power: "On"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	in, err := LoadIntent(dir)
	if err != nil {
		t.Fatal(err)
	}
	typ := in.ApplyCheckByProvider()["redfish"]
	if typ.Settle == nil || *typ.Settle != time.Minute || typ.MaxIneffectiveApplies == nil || *typ.MaxIneffectiveApplies != 4 {
		t.Fatalf("type apply_check = %+v", typ)
	}
	ac := in.Resources[0].ApplyCheck
	if ac == nil || ac.MaxIneffectiveDuration == nil || *ac.MaxIneffectiveDuration != 10*time.Minute || ac.Settle != nil {
		t.Fatalf("resource apply_check = %+v", ac)
	}
}

func TestApplyCheck_Rejects(t *testing.T) {
	for name, body := range map[string]string{
		"zero applies":      "apply_check:\n  - provider: fake\n    max_ineffective_applies: 0\n",
		"bad duration":      "apply_check:\n  - provider: fake\n    settle: soon\n",
		"no provider":       "apply_check:\n  - settle: 1s\n",
		"twice":             "apply_check:\n  - provider: fake\n  - provider: fake\n",
		"provider on a res": "resources:\n  - provider: fake\n    id: a\n    apply_check:\n      provider: fake\n",
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte("version: \"1\"\n"+body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadIntent(dir); err == nil || !strings.Contains(err.Error(), "apply_check") {
			t.Errorf("%s: err = %v, want an apply_check error", name, err)
		}
	}
}
