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

// bmc_test.go — redfish と metrics-probe を CLI から（偽の BMC と偽の /metrics に
// 向けて）回す。電源は上流のキューが空くまで待つ（depends_on の when）。FW の
// バージョンのずれは Procedure（invoke stage → Task の完了を待つ → バージョンで閉じる）
// で直す（§3.7、E-A5・E-H4・N-T4）。

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/providers/redfish/redfishtest"
)

func TestBMCAndMetrics(t *testing.T) {
	bmc := redfishtest.New(redfishtest.Options{PowerState: "Off"})
	defer bmc.Close()
	bmc.AddImage(redfishtest.Image{URI: "http://images.example/bmc-2.0.0.bin", Version: "2.0.0"})
	var mu sync.Mutex
	queue := 500
	exp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(w, "rabbitmq_queue_messages{queue=\"orders\"} %d\n", queue)
	}))
	defer exp.Close()

	dir := t.TempDir()
	ca, err := bmc.WriteCAFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "bmc-password"), []byte("password\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(fmt.Sprintf(`providers:
  bmc:
    module: redfish
    settings:
      settle: 0s
      nodes:
        node1:
          endpoint: %s
          ca_file: %s
          observe: {username: admin, password_file: %s}
  metrics:
    module: metrics-probe
    settings:
      endpoints:
        mq: {url: %s/metrics}
`, bmc.URL, ca, filepath.Join(dir, "bmc-password"), exp.URL)), 0o644)
	os.MkdirAll(filepath.Join(dir, "intent"), 0o755)
	os.WriteFile(filepath.Join(dir, "intent", "bmc.yaml"), []byte(`resources:
  - type: bmc/system
    id: node1
    fields: {power: "On"}
    depends_on:
      - when: 'observe(metrics/series/mq/rabbitmq_queue_messages{queue="orders"}).sum < 100'
  - type: bmc/firmware
    id: node1/BMC
    fields: {version: "2.0.0"}
procedures:
  - name: bmc-fw
    target: {type: bmc/firmware, id: node1/BMC}
    trigger: on_drift
    drift_fields: [version]
    steps:
      - name: stage
        skip_if: 'observe(bmc/firmware/{target}).version == desired.version'
        invoke: {op: stage, args: {image_uri: "http://images.example/bmc-2.0.0.bin"}}
      - name: applied
        wait: {when: 'observe(bmc/firmware/{target}).update_state == "Completed"', timeout: 10m}
    done_when: 'observe(bmc/firmware/{target}).version == desired.version'
`), 0o644)
	run := func(verb string) (int, string) {
		var out, errOut bytes.Buffer
		args := []string{verb, "--intent", filepath.Join(dir, "intent"), "--config", filepath.Join(dir, "config.yaml")}
		if verb == "reconcile" {
			args = append(args, "--record", filepath.Join(dir, "record"))
		}
		code := cli.Run(context.Background(), args, &out, &errOut, Options()...)
		return code, out.String() + errOut.String()
	}

	// plan は比べるだけ。キューが溜まっている間は電源を入れない。
	code, out := run("plan")
	if code != cli.ExitOK || !strings.Contains(out, "bmc/system/node1") || !strings.Contains(out, "Procedure bmc-fw bmc/firmware/node1/BMC: start する") {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	if code, out = run("reconcile"); code != cli.ExitRemaining || bmc.PowerState() != "Off" {
		t.Fatalf("キューが溜まっているのに: exit=%d power=%s\n%s", code, bmc.PowerState(), out)
	}
	if bmc.UpdateRequests() != 1 {
		t.Fatalf("更新の要求 %d 回\n%s", bmc.UpdateRequests(), out)
	}
	mu.Lock()
	queue = 3
	mu.Unlock()
	code, out = run("reconcile")
	if bmc.PowerState() != "On" {
		t.Fatalf("キューが空いても電源が入らない: exit=%d\n%s", code, out)
	}
	if code, out = run("reconcile"); code != cli.ExitOK {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	if v := bmc.FirmwareVersion("BMC"); v != "2.0.0" || bmc.UpdateRequests() != 1 {
		t.Fatalf("バージョン %s・更新の要求 %d 回", v, bmc.UpdateRequests())
	}
	if code, out = run("plan"); code != cli.ExitOK || strings.Contains(out, "~ bmc") {
		t.Fatalf("exit=%d\n%s", code, out)
	}
}
