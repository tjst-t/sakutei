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

package redfish_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/redfish"
	"github.com/tjst-t/sakutei/v2/internal/redfish/redfishtest"
)

func TestUpdateServiceAndInventory(t *testing.T) {
	ctx := context.Background()
	_, c := newPair(t, redfishtest.Options{})
	u, err := c.GetUpdateService(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if u.SimpleUpdateTarget != redfishtest.SimpleUpdatePath || u.MultipartHTTPPushURI != redfishtest.MultipartPushPath ||
		!u.TaskService || !u.ServiceEnabled || strings.Join(u.ApplyTimes, ",") != "Immediate,OnReset" {
		t.Fatalf("update service %+v", u)
	}
	inv, err := c.GetFirmwareInventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv) != 3 || inv[0].ID != "BIOS" || inv[1].ID != "BMC" || inv[1].Version != "1.0.0" || !inv[1].Updateable || inv[2].Updateable {
		t.Fatalf("inventory %+v", inv)
	}

	_, none := newPair(t, redfishtest.Options{UpdateOptions: redfishtest.UpdateOptions{NoUpdateService: true, NoTaskService: true}})
	if _, err := none.GetUpdateService(ctx); !redfish.IsNoUpdateService(err) || !redfish.IsRequest(err) {
		t.Fatalf("no update service: %v", err)
	}
	if _, err := none.ListTasks(ctx); !redfish.IsNoTaskService(err) {
		t.Fatalf("no task service: %v", err)
	}
}

func TestSimpleUpdateTask(t *testing.T) {
	ctx := context.Background()
	clk := newClock()
	srv, c := newPair(t, redfishtest.Options{Now: clk.Now, UpdateOptions: redfishtest.UpdateOptions{UpdateDuration: time.Minute}})
	srv.AddImage(redfishtest.Image{URI: "https://images.example/bmc-2.bin", Version: "2.0.0", Username: "fw", Password: "s3cret"})
	loc, err := c.SimpleUpdate(ctx, redfish.UpdateRequest{
		ImageURI: "https://images.example/bmc-2.bin", Targets: []string{redfishtest.FirmwareInventoryPath + "/BMC"},
		ApplyTime: redfish.ApplyImmediate, Username: "fw", Password: "s3cret",
	})
	if err != nil || loc != redfishtest.TasksPath+"/1" {
		t.Fatalf("SimpleUpdate: %q %v", loc, err)
	}
	tasks, err := c.ListTasks(ctx)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks %+v %v", tasks, err)
	}
	tk := tasks[0]
	if tk.State != redfish.TaskRunning || tk.TargetURI != redfishtest.SimpleUpdatePath ||
		tk.Body["ImageURI"] != "https://images.example/bmc-2.bin" || tk.Body["Password"] != nil {
		t.Fatalf("task %+v", tk)
	}
	// Messages の MessageId・MessageArgs（#168）。
	if d := tk.MessageDetails; len(d) != 1 || !redfish.MessageIs(d[0].ID, redfish.MsgTargetDetermined) ||
		len(d[0].Args) != 2 || d[0].Args[0] != redfishtest.FirmwareInventoryPath+"/BMC" || d[0].Args[1] != "https://images.example/bmc-2.bin" {
		t.Fatalf("task messages %+v", tk.MessageDetails)
	}
	clk.Advance(time.Minute)
	tasks, _ = c.ListTasks(ctx)
	if tasks[0].State != redfish.TaskCompleted || srv.FirmwareVersion("BMC") != "2.0.0" {
		t.Fatalf("task %+v version %s", tasks[0], srv.FirmwareVersion("BMC"))
	}

	// 資格が違えば Exception。
	if _, err := c.SimpleUpdate(ctx, redfish.UpdateRequest{
		ImageURI: "https://images.example/bmc-2.bin", Targets: []string{redfishtest.FirmwareInventoryPath + "/BIOS"},
	}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Minute)
	tasks, _ = c.ListTasks(ctx)
	if tasks[1].State != redfish.TaskException || !redfish.TaskFailed(tasks[1].State) || srv.FirmwareVersion("BIOS") != "1.0.0" {
		t.Fatalf("task %+v", tasks[1])
	}
	if r := tasks[1].Reason(); !strings.Contains(r, "denied. (Update.1.0.ApplyFailed)") {
		t.Fatalf("reason %q", r)
	}

	// Updateable でない部品は 400。エラーに Password を出さない。
	_, err = c.SimpleUpdate(ctx, redfish.UpdateRequest{
		ImageURI: "https://images.example/bmc-2.bin", Targets: []string{redfishtest.FirmwareInventoryPath + "/CPLD"}, Password: "s3cret",
	})
	if !redfish.IsRequest(err) || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("not updateable: %v", err)
	}
}

func TestPushUpdateOnReset(t *testing.T) {
	ctx := context.Background()
	srv, c := newPair(t, redfishtest.Options{})
	img := []byte("bios image 2.1")
	srv.AddImage(redfishtest.Image{Content: img, Version: "2.1"})
	loc, err := c.PushUpdate(ctx, redfish.UpdateRequest{
		Targets: []string{redfishtest.FirmwareInventoryPath + "/BIOS"}, ApplyTime: redfish.ApplyOnReset,
	}, "bios.bin", bytes.NewReader(img))
	if err != nil || loc == "" {
		t.Fatalf("PushUpdate: %q %v", loc, err)
	}
	tasks, _ := c.ListTasks(ctx)
	if len(tasks) != 1 || tasks[0].State != redfish.TaskCompleted || tasks[0].TargetURI != redfishtest.MultipartPushPath {
		t.Fatalf("tasks %+v", tasks)
	}
	if srv.FirmwareVersion("BIOS") != "1.0.0" || srv.FirmwarePending("BIOS") != "2.1" {
		t.Fatalf("before reset: %s pending %s", srv.FirmwareVersion("BIOS"), srv.FirmwarePending("BIOS"))
	}
	srv.Restart()
	if srv.FirmwareVersion("BIOS") != "2.1" {
		t.Fatalf("after reset: %s", srv.FirmwareVersion("BIOS"))
	}

	_, nopush := newPair(t, redfishtest.Options{UpdateOptions: redfishtest.UpdateOptions{NoMultipartPush: true}})
	if _, err := nopush.PushUpdate(ctx, redfish.UpdateRequest{Targets: []string{"x"}}, "a", bytes.NewReader(img)); !redfish.IsRequest(err) {
		t.Fatalf("no push uri: %v", err)
	}
}

// MessageIs は Update の登録簿の版を問わない。
func TestMessageIs(t *testing.T) {
	for id, want := range map[string]bool{
		"Update.1.0.TargetDetermined": true,
		"Update.1.1.TargetDetermined": true,
		"Base.1.8.TargetDetermined":   false,
		"Update.1.0.ApplyFailed":      false,
		"":                            false,
	} {
		if got := redfish.MessageIs(id, redfish.MsgTargetDetermined); got != want {
			t.Errorf("MessageIs(%q) = %v, want %v", id, got, want)
		}
	}
}
