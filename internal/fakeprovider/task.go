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

	"github.com/tjst-t/sakutei/internal/task"
)

// The fake provider is also a task.Runner (#41, §6.2): the executor of a
// simple Task (a migration) against the fake reality.  RunTask writes the
// desired fields into the resource — the completion mark the next Observe sees
// (like a version row in schema_migrations) — and counts every execution in
// the state file (task_run_count), so a test can prove a non-idempotent Task
// ran exactly once, also across a daemon restart.  Test-only knobs:
//
//   - task_fail_inject[local]: RunTask fails (after counting).
//   - task_skip_mark[local]: RunTask succeeds but leaves no completion mark.
//   - HoldTask(local): RunTask blocks until the returned release is called
//     (in-memory: a long-running Task, or one cut off by a daemon crash).
//
// RollbackTask only counts (task_rollback_count).
var _ task.Runner = (*Provider)(nil)

// RunTask implements task.Runner.
func (p *Provider) RunTask(ctx context.Context, req task.Request) error {
	local := req.Resource.Local
	if err := p.bumpTaskCount(local, false); err != nil {
		return err
	}

	p.holdMu.Lock()
	hold := p.taskHolds[local]
	p.holdMu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	sf, err := p.load()
	if err != nil {
		return fmt.Errorf("fakeprovider: load state: %w", err)
	}
	if reason, ok := sf.TaskFailInject[local]; ok {
		return fmt.Errorf("fakeprovider: injected task failure for %s: %s", local, reason)
	}
	if sf.TaskSkipMark[local] {
		return nil
	}
	fields := make(map[string]any, len(req.Desired))
	for k, v := range sf.Resources[local].Fields {
		fields[k] = v
	}
	for k, v := range req.Desired {
		fields[k] = v
	}
	sf.Resources[local] = storedResource{Fields: fields}
	return p.save(sf)
}

// RollbackTask implements task.Runner.
func (p *Provider) RollbackTask(_ context.Context, req task.Request) error {
	return p.bumpTaskCount(req.Resource.Local, true)
}

func (p *Provider) bumpTaskCount(local string, rollback bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	sf, err := p.load()
	if err != nil {
		return fmt.Errorf("fakeprovider: load state: %w", err)
	}
	m := &sf.TaskRunCount
	if rollback {
		m = &sf.TaskRollbackCount
	}
	if *m == nil {
		*m = map[string]int{}
	}
	(*m)[local]++
	return p.save(sf)
}

// HoldTask makes RunTask for local block until release is called.  It returns
// release (idempotent).  Test-only.
func (p *Provider) HoldTask(local string) (release func()) {
	ch := make(chan struct{})
	p.holdMu.Lock()
	if p.taskHolds == nil {
		p.taskHolds = map[string]chan struct{}{}
	}
	p.taskHolds[local] = ch
	p.holdMu.Unlock()
	released := false
	return func() {
		p.holdMu.Lock()
		defer p.holdMu.Unlock()
		if !released {
			released = true
			close(ch)
			if p.taskHolds[local] == ch {
				delete(p.taskHolds, local)
			}
		}
	}
}

// TaskCounts returns how many times RunTask and RollbackTask ran per resource.
// Test-only.
func (p *Provider) TaskCounts() (runs, rollbacks map[string]int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sf, err := p.load()
	if err != nil {
		return nil, nil, err
	}
	runs, rollbacks = map[string]int{}, map[string]int{}
	for k, v := range sf.TaskRunCount {
		runs[k] = v
	}
	for k, v := range sf.TaskRollbackCount {
		rollbacks[k] = v
	}
	return runs, rollbacks, nil
}

// SetTaskFailInject makes RunTask fail for the given resources (nil clears).
// Test-only.
func (p *Provider) SetTaskFailInject(fail map[string]string) error {
	return p.updateState(func(sf *stateFile) { sf.TaskFailInject = fail })
}

// SetTaskSkipMark makes RunTask succeed without leaving the completion mark for
// the given resources (nil clears).  Test-only.
func (p *Provider) SetTaskSkipMark(skip map[string]bool) error {
	return p.updateState(func(sf *stateFile) { sf.TaskSkipMark = skip })
}

func (p *Provider) updateState(fn func(*stateFile)) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	sf, err := p.load()
	if err != nil {
		return err
	}
	fn(sf)
	return p.save(sf)
}
