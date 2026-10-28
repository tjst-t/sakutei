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
)

// runtime:（§26.7、Backlog #183）の読み込み。
func TestRuntime_Load(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "00-runtime.yaml"), []byte("runtime:\n  interval: 1m\n  max_changes_per_cycle: 4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	in, err := LoadIntent(dir)
	if err != nil {
		t.Fatal(err)
	}
	if in.Runtime == nil || in.Runtime.File != "00-runtime.yaml" || in.Runtime.Values["interval"] != "1m0s" || in.Runtime.Values["max_changes_per_cycle"] != "4" {
		t.Errorf("Runtime = %+v", in.Runtime)
	}
	inv := filepath.Join(dir, "inventory.yaml")
	if err := os.WriteFile(inv, []byte("runtime:\n  interval: 1m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadResourcesFile(inv); err == nil || !strings.Contains(err.Error(), "runtime") {
		t.Errorf("an inventory file must not declare runtime:, got %v", err)
	}
}
