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

package main

// guestos_test.go — guest-os を CLI から（手元のホストに向けて）回す。下流は
// ゲスト OS の marker が置かれるまで操作しない（depends_on の when）。marker のパスは
// Resource の field markers に書く。宣言どおりなので guest/os はずれにならない
// （§7.1 の guest-os、E-E4・E-A8）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v3/cli"
)

func TestGuestOSMarkerGatesDownstream(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "sakutei-installed")
	state := filepath.Join(dir, "state.json")
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(fmt.Sprintf(`providers:
  guest:
    module: guest-os
    settings:
      hosts:
        node1: {local: true}
  dns:
    module: fake
    settings:
      state: %s
      kinds:
        record:
          fields:
            value: string
`, state)), 0o644)
	os.MkdirAll(filepath.Join(dir, "intent"), 0o755)
	os.WriteFile(filepath.Join(dir, "intent", "guest.yaml"), []byte(fmt.Sprintf(`resources:
  - type: guest/os
    id: node1
    fields:
      markers: {installed: %s}
  - type: dns/record
    id: node1
    fields: {value: 192.0.2.21}
    depends_on:
      - when: 'observe(guest/os/node1).marker.installed == "sakutei-2026-10-31"'
`, marker)), 0o644)
	run := func(verb string) (int, string) {
		var out, errOut bytes.Buffer
		args := []string{verb, "--intent", filepath.Join(dir, "intent"), "--config", filepath.Join(dir, "config.yaml")}
		if verb == "reconcile" {
			args = append(args, "--record", filepath.Join(dir, "record"))
		}
		code := cli.Run(context.Background(), args, &out, &errOut, Options()...)
		return code, out.String() + errOut.String()
	}
	applied := func() bool {
		b, err := os.ReadFile(state)
		if err != nil {
			return false
		}
		var m map[string]any
		return json.Unmarshal(b, &m) == nil && m["dns/record/node1"] != nil
	}

	// marker が無い間は下流を作らない。
	if code, out := run("reconcile"); code != cli.ExitRemaining || applied() {
		t.Fatalf("marker が無いのに: exit=%d\n%s", code, out)
	}
	os.WriteFile(marker, []byte("sakutei-2026-10-31\n"), 0o644)
	if code, out := run("reconcile"); code != cli.ExitOK || !applied() {
		t.Fatalf("marker が置かれても: exit=%d\n%s", code, out)
	}
	if code, out := run("plan"); code != cli.ExitOK || strings.Contains(out, "guest/os/node1") {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	// 観測だけの field は宣言できない（終了コード 2）。
	os.WriteFile(filepath.Join(dir, "intent", "guest.yaml"), []byte(`resources:
  - type: guest/os
    id: node1
    fields: {boot_id: x}
`), 0o644)
	if code, out := run("plan"); code != cli.ExitRejected || !strings.Contains(out, "観測だけで宣言できない") {
		t.Fatalf("exit=%d\n%s", code, out)
	}
}
