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

// system.go は type system（電源・起動先の上書き、E-A1・N-A1）と virtual-media
// （CD、E-A3）。

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/providers/redfish/client"
)

// system の field。
const (
	FieldPower       = "power"
	FieldBootEnabled = "boot_enabled"
	FieldBootTarget  = "boot_target"
	// 観測だけ。
	FieldPowerState  = "power_state"
	FieldUUID        = "uuid"
	FieldMACs        = "macs"
	FieldCPUCount    = "cpu_count"
	FieldMemoryGiB   = "memory_gib"
	FieldBootMode    = "boot_mode"
	FieldBootTargets = "boot_targets" // BootSourceOverrideTarget@Redfish.AllowableValues
	FieldResetTypes  = "reset_types"  // ResetType@Redfish.AllowableValues
)

// virtual-media の field。
const (
	FieldImage = "image"
	// 観測だけ。
	FieldInserted   = "inserted"
	FieldMediaImage = "media_image"
)

func validateSystem(f core.Fields) []string {
	var out []string
	if v, ok := f[FieldPower].(string); ok {
		switch v {
		case client.PowerOn, client.PowerOff:
		case client.ResetForceRestart, client.ResetGracefulRestart, client.ResetForceOff, "Restart", "Reboot":
			out = append(out, fmt.Sprintf("power %q は宣言できない：再起動・強制停止は観測に残らない一回きりの操作（Procedure の invoke: {op: %s}・{op: %s}）。On か Off を書く", v, OpReset, OpForceOff))
		default:
			out = append(out, fmt.Sprintf("power %q: On か Off を書く", v))
		}
	}
	enabled, hasEnabled := f[FieldBootEnabled].(string)
	target, hasTarget := f[FieldBootTarget].(string)
	if hasEnabled {
		switch enabled {
		case client.BootContinuous, client.BootDisabled:
		case client.BootOnce:
			out = append(out, fmt.Sprintf("boot_enabled \"Once\" は宣言できない：BMC は 1 回起動すると Disabled に戻すので収束しない（Procedure の invoke: {op: %s, args: {target: Cd}}）。Continuous か Disabled を書く", OpBootOnce))
		default:
			out = append(out, fmt.Sprintf("boot_enabled %q: Continuous か Disabled を書く", enabled))
		}
	}
	switch {
	case hasTarget && enabled != client.BootContinuous:
		out = append(out, "boot_target は boot_enabled: Continuous と書く（Disabled の起動先は比べない）")
	case enabled == client.BootContinuous && (!hasTarget || target == ""):
		out = append(out, "boot_enabled: Continuous には boot_target を書く")
	}
	return out
}

func observeSystem(ctx context.Context, c client.Client, ids []core.ID) map[core.ID]core.Observation {
	sys, err := c.GetSystem(ctx)
	if err != nil {
		return fill(ids, unknown(err))
	}
	switch sys.PowerState {
	case client.PowerPoweringOn, client.PowerPoweringOff:
		// BMC が電源を切り替えている途中。重ねて送らない（E-A1）。
		return fill(ids, core.Observation{Availability: core.Converging, Note: "電源が " + sys.PowerState})
	case client.PowerOn, client.PowerOff:
	default:
		return fill(ids, core.Observation{Availability: core.Unknown, Note: fmt.Sprintf("未知の PowerState %q", sys.PowerState)})
	}
	macs := sys.MACAddresses
	if macs == nil {
		macs = []string{}
	}
	return fill(ids, core.Observation{Availability: core.Present, Fields: core.Fields{
		FieldPower:       sys.PowerState,
		FieldBootEnabled: sys.Boot.Enabled,
		FieldBootTarget:  sys.Boot.Target,
		FieldPowerState:  sys.PowerState,
		FieldUUID:        sys.UUID,
		FieldMACs:        strList(macs),
		FieldCPUCount:    int64(sys.ProcessorCount),
		FieldMemoryGiB:   sys.TotalMemoryGiB,
		FieldBootMode:    sys.Boot.Mode,
		FieldBootTargets: strList(sys.Boot.AllowableTargets),
		FieldResetTypes:  strList(sys.ResetTypes),
	}})
}

// applySystem は起動先を先に、電源を後に変える。BMC を読み直して、ずれている
// ものだけを送る（冪等）。電源の切り替えの途中なら重ねて送らない。
func applySystem(ctx context.Context, c client.Client, want core.Fields) error {
	sys, err := c.GetSystem(ctx)
	if err != nil {
		return err
	}
	if enabled, ok := want[FieldBootEnabled].(string); ok {
		switch enabled {
		case client.BootContinuous:
			target, _ := want[FieldBootTarget].(string)
			if len(sys.Boot.AllowableTargets) > 0 && !has(sys.Boot.AllowableTargets, target) {
				return fmt.Errorf("boot_target %q を BMC が持たない（選べる値: %s）", target, strings.Join(sys.Boot.AllowableTargets, ", "))
			}
			if sys.Boot.Enabled != enabled || sys.Boot.Target != target {
				if err := c.PatchBoot(ctx, enabled, target); err != nil {
					return fmt.Errorf("起動先の PATCH: %w", err)
				}
			}
		case client.BootDisabled:
			if sys.Boot.Enabled != enabled {
				if err := c.PatchBoot(ctx, enabled, ""); err != nil {
					return fmt.Errorf("起動先の PATCH: %w", err)
				}
			}
		default:
			return fmt.Errorf("boot_enabled %q は Apply できない", enabled)
		}
	}
	power, ok := want[FieldPower].(string)
	if !ok {
		return nil
	}
	var reset string
	switch {
	case power == client.PowerOn && sys.PowerState == client.PowerOff:
		reset = client.ResetOn
	case power == client.PowerOff && sys.PowerState == client.PowerOn:
		// 強制停止はしない（破壊的な操作は force_off、E-A1）。
		reset = client.ResetGracefulShutdown
	case power != client.PowerOn && power != client.PowerOff:
		return fmt.Errorf("power %q は Apply できない", power)
	default:
		return nil // 宣言どおりか切り替えの途中：重ねて送らない
	}
	if len(sys.ResetTypes) > 0 && !has(sys.ResetTypes, reset) {
		return fmt.Errorf("BMC が ResetType %s を持たない（選べる値: %s）", reset, strings.Join(sys.ResetTypes, ", "))
	}
	if err := c.Reset(ctx, reset); err != nil {
		return fmt.Errorf("Reset %s: %w", reset, err)
	}
	return nil
}

// imageForm は image に書けるもの：URL（scheme://…）か絶対パス。
var imageForm = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*://.+|/.*)$`)

func insertedImage(vm client.VirtualMedia) string {
	if !vm.Inserted {
		return ""
	}
	return vm.Image
}

func observeMedia(ctx context.Context, c client.Client, ids []core.ID) map[core.ID]core.Observation {
	vm, err := c.GetVirtualMedia(ctx)
	if err != nil {
		return fill(ids, unknown(err))
	}
	return fill(ids, core.Observation{Availability: core.Present, Fields: core.Fields{
		FieldImage:      insertedImage(vm),
		FieldInserted:   vm.Inserted,
		FieldMediaImage: vm.Image,
	}})
}

// applyMedia は違う image が入っていれば取り出してから入れる。"" なら取り出す。
func applyMedia(ctx context.Context, c client.Client, want core.Fields) error {
	image, ok := want[FieldImage].(string)
	if !ok {
		return nil
	}
	vm, err := c.GetVirtualMedia(ctx)
	if err != nil {
		return err
	}
	if insertedImage(vm) == image {
		return nil
	}
	if vm.Inserted {
		if err := c.EjectMedia(ctx); err != nil {
			return fmt.Errorf("EjectMedia: %w", err)
		}
	}
	if image == "" {
		return nil
	}
	if err := c.InsertMedia(ctx, image); err != nil {
		return fmt.Errorf("InsertMedia %q: %w", image, err)
	}
	return nil
}
