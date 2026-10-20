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

// revision_yaml_test.go — Backlog bug: intent_revision ignored YAML intent
// files, so editing a .yaml intent left the revision unchanged (the edit was
// then misclassified as drift, cause B, instead of intent, cause A).
//
// Tag: [#34-1] a YAML intent edit changes intent_revision; a JSON-only
// directory hashes exactly as before.

package acceptance

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRevision_YAMLIntentChangeCausesRevisionChange(t *testing.T) {
	// [#34-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	statePath := filepath.Join(tmp, "state.json")
	writeYAML := func(size string) {
		t.Helper()
		content := "resources:\n  - provider: fake\n    id: vm-1\n    fields:\n      name: \"vm-1\"\n      size: \"" + size + "\"\n"
		if err := os.MkdirAll(intentDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := writeFileAtomic(filepath.Join(intentDir, "vm.yaml"), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	writeYAML("1")
	initStateFile(t, statePath)
	out1, stderr1, code1 := runPlan(t, intentDir, statePath)
	if code1 != 0 {
		t.Fatalf("[#34-1] plan failed: %s", stderr1)
	}
	rev1, _ := extractRevisions(out1)

	writeYAML("2")
	out2, stderr2, code2 := runPlan(t, intentDir, statePath)
	if code2 != 0 {
		t.Fatalf("[#34-1] plan failed: %s", stderr2)
	}
	rev2, _ := extractRevisions(out2)

	if rev1 == "" || rev2 == "" {
		t.Fatalf("[#34-1] intent_revision missing from plan output:\n%s\n%s", out1, out2)
	}
	if rev1 == rev2 {
		t.Fatalf("[#34-1] editing a YAML intent must change intent_revision; both runs report %s", rev1)
	}
	const emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if rev1 == emptyHash {
		t.Fatalf("[#34-1] intent_revision is the hash of nothing: the YAML file was not hashed")
	}
}
