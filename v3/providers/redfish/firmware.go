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

package redfish

// firmware.go は種類 firmware（部品の FW の版と、その部品の最新の更新の Task。
// 観測だけ、E-A5・N-A4）。更新そのものは Invoker の stage（invoke.go）で、
// Procedure が段を進める（§3.7）。
//
// 「その部品の更新の Task」は TaskService の Task のうち、Payload の JsonBody の
// Targets にその部品の @odata.id があるもの。Payload を返さない BMC（qemu-bmc
// 0.11.0）では、Update の登録簿のメッセージ TargetDetermined の MessageArgs の
// 1 つ目（部品の URI）で見分ける。いくつもあれば StartTime の新しいもの（読めない
// StartTime は最も古いとみなす）。

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/providers/redfish/client"
)

// firmware の項目。
const (
	FieldVersion = "version"
	// 観測だけ。
	FieldUpdateable    = "updateable"
	FieldSoftwareID    = "software_id"
	FieldUpdateState   = "update_state"   // 最新の更新の Task の TaskState（無ければ ""）
	FieldUpdateImage   = "update_image"   // その Task のイメージ（ImageURI か送ったファイルの名前）
	FieldUpdatePercent = "update_percent" // その Task の PercentComplete（無ければ -1）
	FieldUpdateMessage = "update_message" // その Task の Messages（失敗の理由）
)

func observeFirmware(ctx context.Context, c client.Client, ids []core.ID) map[core.ID]core.Observation {
	if _, err := c.GetUpdateService(ctx); client.IsNoUpdateService(err) {
		return fill(ids, core.Observation{Availability: core.Absent, Note: "BMC が UpdateService を持たない"})
	} else if err != nil {
		return fill(ids, unknown(err))
	}
	inv, err := c.GetFirmwareInventory(ctx)
	if err != nil {
		return fill(ids, unknown(err))
	}
	var tasks []client.Task
	if tasks, err = c.ListTasks(ctx); err != nil && !client.IsNoTaskService(err) {
		return fill(ids, unknown(err))
	}
	out := map[core.ID]core.Observation{}
	for _, id := range ids {
		_, comp := split(id)
		var fc *client.FirmwareComponent
		for i := range inv {
			if inv[i].ID == comp {
				fc = &inv[i]
			}
		}
		if fc == nil {
			out[id] = core.Observation{Availability: core.Absent, Note: "FirmwareInventory に部品 " + comp + " が無い"}
			continue
		}
		f := core.Fields{
			FieldVersion: strings.TrimSpace(fc.Version), FieldUpdateable: fc.Updateable, FieldSoftwareID: fc.SoftwareID,
			FieldUpdateState: "", FieldUpdateImage: "", FieldUpdatePercent: int64(-1), FieldUpdateMessage: "",
		}
		if t, image := latestTask(tasks, fc.Path); t != nil {
			f[FieldUpdateState], f[FieldUpdateImage] = t.State, image
			f[FieldUpdatePercent], f[FieldUpdateMessage] = int64(t.PercentComplete), t.Reason()
		}
		out[id] = core.Observation{Availability: core.Present, Fields: f}
	}
	return out
}

// latestTask は部品 compPath を更新する Task のうち最新のものと、そのイメージを返す。
func latestTask(tasks []client.Task, compPath string) (*client.Task, string) {
	var best *client.Task
	var bestAt time.Time
	bestImage := ""
	for i := range tasks {
		t := &tasks[i]
		image, ok := taskImage(t, compPath)
		if !ok {
			continue
		}
		at, _ := time.Parse(time.RFC3339Nano, strings.TrimSpace(t.StartTime))
		if best == nil || !at.Before(bestAt) {
			best, bestAt, bestImage = t, at, image
		}
	}
	return best, bestImage
}

// taskImage は Task が部品 compPath の更新かと、そのイメージを返す。
func taskImage(t *client.Task, compPath string) (string, bool) {
	if t.Body != nil {
		ts, _ := t.Body["Targets"].([]any)
		for _, x := range ts {
			if s, ok := x.(string); ok && samePath(s, compPath) {
				img, _ := t.Body["ImageURI"].(string)
				if img == "" {
					// multipart の送信：TargetDetermined にファイルの名前があれば使う。
					for _, m := range t.MessageDetails {
						if client.MessageIs(m.ID, client.MsgTargetDetermined) && len(m.Args) >= 2 {
							img = m.Args[1]
						}
					}
				}
				return img, true
			}
		}
		return "", false
	}
	for _, m := range t.MessageDetails {
		if client.MessageIs(m.ID, client.MsgTargetDetermined) && len(m.Args) >= 2 && samePath(m.Args[0], compPath) {
			return m.Args[1], true
		}
	}
	return "", false
}

func samePath(a, b string) bool {
	norm := func(s string) string {
		if u, err := url.Parse(s); err == nil && u.Path != "" {
			s = u.Path
		}
		return strings.TrimSuffix(s, "/")
	}
	return b != "" && norm(a) == norm(b)
}
