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

package fakeprovider

import (
	"context"
	"fmt"
	"time"

	"github.com/tjst-t/sakutei/internal/task"
)

// The fake provider is also a task.PhasedRunner (#51, §6.2 多相型): a resource
// whose task kind is "firmware" (SetTaskKind) is updated by a phased Task that
// models a firmware update of a device.  The running firmware version is the
// resource's "size" field (what Observe sees and the diff compares); the device
// keeps the rest in the state file (Firmware):
//
//	staging     RunStep: transfer the image      (Staged = target)
//	verifying   RunStep: check the image          (Verified = target; fails unless staged)
//	applying    RunStep: write the flash          (Flashed = target)
//	rebooting   RunStep: reboot                   (Rebooting = true → Read is unknown)
//	confirming  check only: the running version is the target
//
// CheckStep is a pure Observe of that state.  While Rebooting, CheckStep of
// every step is unknown — the device is not seen, not gone.  FinishReboot
// brings the device back running the flashed version.  Every RunStep is
// counted per step (task_step_count) so a test can prove no step ran twice,
// also across a daemon restart.  Test-only knobs:
//
//   - task_step_fail[local] = step: RunStep of that step fails (after counting).
//   - HoldStep(local, step): RunStep of that step blocks after its effect until
//     released (in-memory: a daemon cut off right after the effect).
var _ task.PhasedRunner = (*Provider)(nil)

// TaskKindFirmware is the task kind of SetTaskKind for a phased firmware update.
const TaskKindFirmware = "firmware"

// FirmwareRebootTimeout is the reboot timeout of the fake firmware steps.
const FirmwareRebootTimeout = 10 * time.Minute

// Firmware is the fake device's firmware state (test-only, #51).
type Firmware struct {
	Staged    string `json:"staged,omitempty"`
	Verified  string `json:"verified,omitempty"`
	Flashed   string `json:"flashed,omitempty"`
	Rebooting bool   `json:"rebooting,omitempty"`
}

type stepHold struct {
	reached chan struct{}
	release chan struct{}
}

// TaskSteps implements task.PhasedRunner.
func (p *Provider) TaskSteps(_ context.Context, req task.Request) ([]task.StepSpec, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sf, err := p.load()
	if err != nil {
		return nil, fmt.Errorf("fakeprovider: load state: %w", err)
	}
	if sf.TaskKind[req.Resource.Local] != TaskKindFirmware {
		return nil, nil
	}
	return task.FirmwareSteps(FirmwareRebootTimeout), nil
}

// CheckStep implements task.PhasedRunner.
func (p *Provider) CheckStep(_ context.Context, req task.Request, step string) (task.StepState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sf, err := p.load()
	if err != nil {
		return task.StepUnknown, fmt.Errorf("fakeprovider: load state: %w", err)
	}
	local, target := req.Resource.Local, req.Desired["size"]
	fw := sf.Firmware[local]
	if fw == nil {
		fw = &Firmware{}
	}
	if fw.Rebooting {
		return task.StepUnknown, nil
	}
	running := ""
	if r, ok := sf.Resources[local]; ok {
		running = fmt.Sprint(r.Fields["size"])
	}
	done := running == target
	switch step {
	case task.StepStaging:
		done = done || fw.Staged == target
	case task.StepVerifying:
		done = done || fw.Verified == target
	case task.StepApplying:
		done = done || fw.Flashed == target
	case task.StepRebooting, task.StepConfirming:
	default:
		return task.StepUnknown, fmt.Errorf("fakeprovider: unknown step %q", step)
	}
	if done {
		return task.StepDone, nil
	}
	return task.StepPending, nil
}

// RunStep implements task.PhasedRunner.
func (p *Provider) RunStep(_ context.Context, req task.Request, step string) error {
	local, target := req.Resource.Local, req.Desired["size"]
	err := p.updateStateErr(func(sf *stateFile) error {
		if sf.TaskStepCount == nil {
			sf.TaskStepCount = map[string]map[string]int{}
		}
		if sf.TaskStepCount[local] == nil {
			sf.TaskStepCount[local] = map[string]int{}
		}
		sf.TaskStepCount[local][step]++
		if sf.TaskStepFail[local] == step {
			return fmt.Errorf("fakeprovider: injected failure of step %s for %s", step, local)
		}
		if sf.Firmware == nil {
			sf.Firmware = map[string]*Firmware{}
		}
		fw := sf.Firmware[local]
		if fw == nil {
			fw = &Firmware{}
			sf.Firmware[local] = fw
		}
		switch step {
		case task.StepStaging:
			fw.Staged = target
		case task.StepVerifying:
			if fw.Staged != target {
				return fmt.Errorf("fakeprovider: verify %s: staged image is %q, want %q", local, fw.Staged, target)
			}
			fw.Verified = target
		case task.StepApplying:
			if fw.Verified != target {
				return fmt.Errorf("fakeprovider: apply %s: verified image is %q, want %q", local, fw.Verified, target)
			}
			fw.Flashed = target
		case task.StepRebooting:
			fw.Rebooting = true
		default:
			return fmt.Errorf("fakeprovider: step %q has no action", step)
		}
		return nil
	})
	if err != nil {
		return err
	}

	p.holdMu.Lock()
	h := p.stepHolds[local+"/"+step]
	delete(p.stepHolds, local+"/"+step) // one-shot
	p.holdMu.Unlock()
	if h != nil {
		close(h.reached)
		<-h.release
	}
	return nil
}

// FinishReboot brings a rebooting device back, running the flashed version.
// Test-only.
func (p *Provider) FinishReboot(local string) error {
	return p.updateState(func(sf *stateFile) {
		fw := sf.Firmware[local]
		if fw == nil || !fw.Rebooting {
			return
		}
		fw.Rebooting = false
		r := sf.Resources[local]
		fields := make(map[string]any, len(r.Fields))
		for k, v := range r.Fields {
			fields[k] = v
		}
		if fw.Flashed != "" {
			fields["size"] = fw.Flashed
		}
		sf.Resources[local] = storedResource{Fields: fields}
	})
}

// SetTaskKind declares the task kind of a resource ("" clears).  Test-only.
func (p *Provider) SetTaskKind(local, kind string) error {
	return p.updateState(func(sf *stateFile) {
		if sf.TaskKind == nil {
			sf.TaskKind = map[string]string{}
		}
		if kind == "" {
			delete(sf.TaskKind, local)
			return
		}
		sf.TaskKind[local] = kind
	})
}

// SetFirmware sets the device's firmware state (nil clears).  Test-only.
func (p *Provider) SetFirmware(local string, fw *Firmware) error {
	return p.updateState(func(sf *stateFile) {
		if sf.Firmware == nil {
			sf.Firmware = map[string]*Firmware{}
		}
		if fw == nil {
			delete(sf.Firmware, local)
			return
		}
		c := *fw
		sf.Firmware[local] = &c
	})
}

// GetFirmware returns the device's firmware state.  Test-only.
func (p *Provider) GetFirmware(local string) (Firmware, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sf, err := p.load()
	if err != nil {
		return Firmware{}, err
	}
	if fw := sf.Firmware[local]; fw != nil {
		return *fw, nil
	}
	return Firmware{}, nil
}

// SetTaskStepFail makes RunStep of step fail for local ("" clears).  Test-only.
func (p *Provider) SetTaskStepFail(local, step string) error {
	return p.updateState(func(sf *stateFile) {
		if sf.TaskStepFail == nil {
			sf.TaskStepFail = map[string]string{}
		}
		if step == "" {
			delete(sf.TaskStepFail, local)
			return
		}
		sf.TaskStepFail[local] = step
	})
}

// TaskStepCounts returns how many times RunStep ran per step for local.
// Test-only.
func (p *Provider) TaskStepCounts(local string) (map[string]int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sf, err := p.load()
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for k, v := range sf.TaskStepCount[local] {
		out[k] = v
	}
	return out, nil
}

// HoldStep makes the next RunStep of step for local (once) block after its effect.
// reached is closed when RunStep blocks; release (idempotent) lets it return.
// Test-only.
func (p *Provider) HoldStep(local, step string) (reached <-chan struct{}, release func()) {
	h := &stepHold{reached: make(chan struct{}), release: make(chan struct{})}
	key := local + "/" + step
	p.holdMu.Lock()
	if p.stepHolds == nil {
		p.stepHolds = map[string]*stepHold{}
	}
	p.stepHolds[key] = h
	p.holdMu.Unlock()
	released := false
	return h.reached, func() {
		p.holdMu.Lock()
		defer p.holdMu.Unlock()
		if !released {
			released = true
			close(h.release)
			if p.stepHolds[key] == h {
				delete(p.stepHolds, key)
			}
		}
	}
}

func (p *Provider) updateStateErr(fn func(*stateFile) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	sf, err := p.load()
	if err != nil {
		return err
	}
	ferr := fn(sf)
	if err := p.save(sf); err != nil {
		return err
	}
	return ferr
}
