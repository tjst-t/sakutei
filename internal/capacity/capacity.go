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

// Package capacity implements the capacity gate (§11.4, §13.4).
//
// The capacity gate is a rate limit keyed on the CURRENT utilization of a
// capacity pool (e.g. a cluster): when the observed utilization exceeds the
// threshold, new changes are halted.  What the reconciler does here is only
// "stop when in the danger zone":
//
//   - utilization is observed every cycle by reading the pool resource
//     (Provider.Read) — no time series is kept and nothing is predicted
//     (prediction belongs to the monitoring system, §13.4);
//   - the judgement is the binary "is it within the threshold right now";
//   - remediation (add hosts / migrate / tighten quota) is outside the
//     reconciler.  On exceed a CapacityThresholdExceeded Event is emitted and
//     a Workflow is triggered explicitly by a human (no automatic chaining).
//
// An unobservable pool (read error, Availability != Ok, absent, missing or
// non-numeric fields) is treated as exceeded: unknown ≠ within capacity
// (§3 asymmetry principle — when unsure, do not add).
//
// Gate decisions use the same budget.CycleDecision shape as the change budget
// so the loop can merge both gates and filter ops in one place.
package capacity

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/tjst-t/sakutei/internal/budget"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// EventName is the Event emitted when a capacity gate halts changes (§13.4).
const EventName = "CapacityThresholdExceeded"

// Gate declares one capacity gate:
//
//	safety:
//	  capacity_gate:
//	    max_utilization: 80%
//	    on_exceed: halt-destructive-only
//
// plus where the utilization is observed: the pool resource and the two
// State fields that carry the used and total amounts.
type Gate struct {
	// Provider is the provider that observes the pool resource.  Empty means
	// the caller's default provider.
	Provider string
	// Resource is the Local ID of the capacity pool resource (e.g. a cluster).
	Resource string
	// UsedField / TotalField name the observed State fields holding the used
	// and total capacity.  Utilization = used / total × 100.
	UsedField  string
	TotalField string
	// MaxUtilization is the threshold in percent (0 < x ≤ 100).  Utilization
	// strictly above it is "exceeded".
	MaxUtilization float64
	// OnExceed selects which changes are halted (budget semantics; the zero
	// value OnExceedHaltAll is the conservative default: a full pool must not
	// receive new consumers).
	OnExceed budget.OnExceedPolicy
}

// ParseGate parses the CLI form of a gate:
//
//	resource=<id>,used=<field>,total=<field>,max_utilization=80%[,on_exceed=halt-all|halt-destructive-only][,provider=<name>]
func ParseGate(s string) (Gate, error) {
	var g Gate
	seenMax := false
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return Gate{}, fmt.Errorf("capacity gate: %q is not key=value", part)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "provider":
			g.Provider = v
		case "resource":
			g.Resource = v
		case "used":
			g.UsedField = v
		case "total":
			g.TotalField = v
		case "max_utilization":
			f, err := strconv.ParseFloat(strings.TrimSuffix(v, "%"), 64)
			if err != nil {
				return Gate{}, fmt.Errorf("capacity gate: max_utilization %q: %v", v, err)
			}
			g.MaxUtilization = f
			seenMax = true
		case "on_exceed":
			switch v {
			case "halt-all":
				g.OnExceed = budget.OnExceedHaltAll
			case "halt-destructive-only":
				g.OnExceed = budget.OnExceedHaltDestructiveOnly
			default:
				return Gate{}, fmt.Errorf("capacity gate: unknown on_exceed %q; use halt-all or halt-destructive-only", v)
			}
		default:
			return Gate{}, fmt.Errorf("capacity gate: unknown key %q", k)
		}
	}
	if g.Resource == "" || g.UsedField == "" || g.TotalField == "" {
		return Gate{}, fmt.Errorf("capacity gate: resource, used and total are required")
	}
	if !seenMax || g.MaxUtilization <= 0 || g.MaxUtilization > 100 {
		return Gate{}, fmt.Errorf("capacity gate: max_utilization must be in (0, 100]")
	}
	return g, nil
}

// Evaluate judges one observation of the pool resource.  readErr is the
// error returned by Provider.Read, if any.
func (g Gate) Evaluate(obs core.Observation, readErr error) budget.CycleDecision {
	if readErr != nil {
		return g.halt(fmt.Sprintf("capacity gate %s: utilization unknown (observe error: %v); halting (unknown ≠ within capacity)", g.Resource, readErr))
	}
	if obs.Availability != core.AvailabilityOk || obs.State == nil {
		return g.halt(fmt.Sprintf("capacity gate %s: utilization unknown (pool not observed); halting (unknown ≠ within capacity)", g.Resource))
	}
	used, err := numericField(obs.State, g.UsedField)
	if err != nil {
		return g.halt(fmt.Sprintf("capacity gate %s: utilization unknown (%v); halting", g.Resource, err))
	}
	total, err := numericField(obs.State, g.TotalField)
	if err != nil {
		return g.halt(fmt.Sprintf("capacity gate %s: utilization unknown (%v); halting", g.Resource, err))
	}
	if total <= 0 {
		return g.halt(fmt.Sprintf("capacity gate %s: total capacity %v is not positive; halting", g.Resource, total))
	}
	util := used / total * 100
	if util > g.MaxUtilization {
		return g.halt(fmt.Sprintf("capacity gate %s: utilization %.1f%% (%v/%v) exceeds max_utilization=%.1f%%",
			g.Resource, util, used, total, g.MaxUtilization))
	}
	return budget.CycleDecision{}
}

// Check observes the pool resource through fp and evaluates the gate.
func (g Gate) Check(ctx context.Context, fp provider.Provider) budget.CycleDecision {
	if fp == nil {
		return g.halt(fmt.Sprintf("capacity gate %s: provider %q not available; halting", g.Resource, g.Provider))
	}
	obs, err := fp.Read(ctx, g.Resource)
	return g.Evaluate(obs, err)
}

// CheckAll evaluates every gate (each observing its pool fresh) and merges the
// decisions.  providerFor resolves a gate's provider; it may return nil.
func CheckAll(ctx context.Context, gates []Gate, providerFor func(Gate) provider.Provider) budget.CycleDecision {
	var d budget.CycleDecision
	for _, g := range gates {
		d = budget.Merge(d, g.Check(ctx, providerFor(g)))
	}
	return d
}

func (g Gate) halt(reason string) budget.CycleDecision {
	if g.OnExceed == budget.OnExceedHaltDestructiveOnly {
		return budget.CycleDecision{HaltDestructive: true, Reason: reason}
	}
	return budget.CycleDecision{HaltAll: true, HaltDestructive: true, Reason: reason}
}

func numericField(st core.State, field string) (float64, error) {
	tv, ok := st[field]
	if !ok {
		return 0, fmt.Errorf("field %q not observed", field)
	}
	f, err := strconv.ParseFloat(fmt.Sprintf("%v", tv.Value), 64)
	if err != nil {
		return 0, fmt.Errorf("field %q is not numeric: %v", field, tv.Value)
	}
	return f, nil
}
