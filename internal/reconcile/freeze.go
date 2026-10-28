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

package reconcile

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/audit"
	"github.com/tjst-t/sakutei/v2/internal/eventlog"
	"github.com/tjst-t/sakutei/v2/internal/freeze"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// FreezeControl decides, every cycle, whether a global freeze (§14.3) is in
// effect, and records its expiry in the S4 audit log (Backlog #184).
//
//   - Flag is the freeze of the start flag --freeze (its start was recorded
//     by the caller).  Once its deadline passes it is lifted and the expiry
//     recorded.
//   - StatePath is the freeze state file (--freeze-state) written by
//     `sakutei freeze start` / `end` while the daemon runs; it is read every
//     cycle.  An unreadable file freezes the cycle (§3: unsure → do not
//     change).
//
// While a freeze is in effect each cycle raises the level Event Frozen (with
// who, why and until when); it resolves the first cycle without one.
type FreezeControl struct {
	Flag      *freeze.Freeze
	StatePath string
	Audit     *audit.Log
	// Now is the clock (nil: time.Now).
	Now func() time.Time

	mu      sync.Mutex
	current *freeze.Freeze
	checked int
}

// Active returns the freeze in effect as of the last check (nil: none).
func (c *FreezeControl) Active() *freeze.Freeze {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

// Check resolves the freeze for cycle and raises Frozen on w while one is in
// effect.  It reports whether the cycle is frozen.
func (c *FreezeControl) Check(w io.Writer, prefix string, cycle int) bool {
	if c == nil {
		return false
	}
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checked = cycle
	c.current = nil
	if c.Flag != nil && c.Flag.Expired(now) {
		if err := freeze.RecordExpired(c.Audit, *c.Flag, now); err != nil {
			// The expiry could not be recorded: stay frozen rather than lift
			// a freeze without its record.
			fmt.Fprintf(w, "%sfreeze: the --freeze deadline passed but the expiry could not be recorded (%v); staying frozen\n", prefix, err)
			c.current = c.Flag
		} else {
			fmt.Fprintf(w, "%sfreeze: the --freeze deadline %s passed; the freeze is lifted (recorded in the audit log)\n",
				prefix, c.Flag.Until.UTC().Format(time.RFC3339))
			c.Flag = nil
		}
	} else if c.Flag != nil {
		c.current = c.Flag
	}
	if c.StatePath != "" {
		active, expired, err := freeze.Check(c.StatePath, c.Audit, now)
		switch {
		case err != nil:
			fmt.Fprintf(w, "%sfreeze: the freeze state %s cannot be read (%v); this cycle is frozen (§3)\n", prefix, c.StatePath, err)
			if c.current == nil {
				c.current = &freeze.Freeze{Source: freeze.SourceCommand, By: "(unknown)", Since: now,
					Reason: fmt.Sprintf("the freeze state %s cannot be read: %v", c.StatePath, err)}
			}
		case expired != nil:
			fmt.Fprintf(w, "%sfreeze: the freeze by %s expired at %s; it is lifted (recorded in the audit log)\n",
				prefix, expired.By, expired.Until.UTC().Format(time.RFC3339))
		}
		if active != nil && c.current == nil {
			c.current = active
		}
	}
	if c.current == nil {
		return false
	}
	EmitFrozen(w, prefix, *c.current)
	return true
}

// EmitFrozen raises one Frozen Event for f.  The one-shot reconcile uses it
// too.
func EmitFrozen(w io.Writer, prefix string, f freeze.Freeze) {
	msg := f.Text() + " — alert-only: drift is detected, nothing is changed (§14.3)"
	eventlog.Emit(w, core.Event{
		Kind: core.EventFrozen, Severity: core.SeverityWarning, Message: msg,
		Attrs: f.Attrs(),
		Text:  prefix + "Frozen: " + msg,
	})
}

// resolves reports whether a Frozen Event that did not recur on cycle is
// gone: the freeze was checked on that cycle.
func (c *FreezeControl) resolves(cycle int) bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.checked == cycle
}

// EndFlag records the end of the --freeze freeze when the run ends (why), unless
// its deadline lifted it already.
func (c *FreezeControl) EndFlag(why string, now time.Time) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Flag == nil {
		return nil
	}
	f := *c.Flag
	c.Flag = nil
	if f.Expired(now) {
		return freeze.RecordExpired(c.Audit, f, now)
	}
	return freeze.RecordEnd(c.Audit, f, "", why, now)
}
