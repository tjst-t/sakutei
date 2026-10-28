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

package loader

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAllocationFileRoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	f := AllocationFile{SchemaVersion: 1, Pool: "vm-ids", Allocations: []AllocationEntry{
		{Owner: "proxmox-vm/web-2#vmid", Value: 102, AllocatedAt: at},
		{Owner: "proxmox-vm/web-1#vmid", Value: 101, AllocatedAt: at},
	}}
	data := EmitAllocationFile(f)
	if !strings.HasPrefix(string(data), AllocationFileHeader+"\n") || !strings.Contains(string(data), "schema_version: 1\n") {
		t.Fatalf("header / schema_version missing:\n%s", data)
	}
	if i, j := strings.Index(string(data), "web-1"), strings.Index(string(data), "web-2"); i < 0 || j < i {
		t.Fatalf("allocations not sorted by value:\n%s", data)
	}
	path := filepath.Join(t.TempDir(), "vm-ids.yaml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, exists, err := ReadAllocationFile(path, "vm-ids")
	if err != nil || !exists {
		t.Fatalf("read: exists=%v err=%v\n%s", exists, err, data)
	}
	if len(got.Allocations) != 2 || got.Allocations[0].Owner != "proxmox-vm/web-1#vmid" || got.Allocations[0].Value != 101 || !got.Allocations[0].AllocatedAt.Equal(at) {
		t.Fatalf("round trip: %+v", got)
	}
	if string(EmitAllocationFile(got)) != string(data) {
		t.Fatalf("emit is not stable")
	}

	empty := EmitAllocationFile(AllocationFile{Pool: "vm-ids"})
	_ = os.WriteFile(path, empty, 0o644)
	if got, _, err := ReadAllocationFile(path, "vm-ids"); err != nil || len(got.Allocations) != 0 {
		t.Fatalf("empty file: %+v %v", got, err)
	}
}

func TestAllocationFileRejects(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"version":   "schema_version: 2\npool: p\nallocations:\n",
		"pool":      "schema_version: 1\npool: q\nallocations:\n",
		"dupvalue":  "schema_version: 1\npool: p\nallocations:\n  - owner: a\n    value: 1\n  - owner: b\n    value: 1\n",
		"dupowner":  "schema_version: 1\npool: p\nallocations:\n  - owner: a\n    value: 1\n  - owner: a\n    value: 2\n",
		"novalue":   "schema_version: 1\npool: p\nallocations:\n  - owner: a\n",
		"unknown":   "schema_version: 1\npool: p\nextra: 1\n",
		"noversion": "pool: p\n",
	} {
		path := filepath.Join(dir, name+".yaml")
		_ = os.WriteFile(path, []byte(body), 0o644)
		if _, _, err := ReadAllocationFile(path, "p"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, exists, err := ReadAllocationFile(filepath.Join(dir, "none.yaml"), "p"); exists || err != nil {
		t.Fatalf("missing file: exists=%v err=%v", exists, err)
	}
}
