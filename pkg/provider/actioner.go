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

package provider

import (
	"context"
	"errors"
)

// Actioner is an OPTIONAL Provider capability, discovered by type assertion
// like Lister and PlanRefiner: one-time operations a Workflow step can perform
// on a resource (the `action` step, §6.3, Backlog #139).  An operation is NOT
// a state to converge to — a Redfish "boot from the CD once" or "restart" is
// used up by the machine itself, so the convergence loop can never observe it
// as converged.  It is therefore never declared as a resource field (the
// provider's ValidateIntent keeps rejecting it, §29.1) and only runs as a
// procedure: the Workflow engine performs it exactly once, at its position,
// and then moves on.  Whether it took effect is confirmed by the following
// steps (wait / require), not by the operation itself.
type Actioner interface {
	// PlanAction validates op with args for localID without consulting
	// reality and returns how it must be accounted.  An unknown op or a bad
	// argument is an error (the step halts).
	PlanAction(localID, op string, args map[string]string) (ActionPlan, error)
	// Action performs op once.  An error wrapping ErrActionNotSent says
	// nothing reached the target (it was unreachable, or busy with another
	// operation): the engine waits and tries again.  Any other error may have
	// left the operation half done, and the engine does not repeat it.
	Action(ctx context.Context, localID, op string, args map[string]string) error
}

// ActionPlan is the accounting of one action.
type ActionPlan struct {
	// Destructive counts the action against the change budget (§13), like a
	// destructive Op (e.g. a forced restart).
	Destructive bool
	// Description is the action in words, for events.
	Description string
}

// ErrActionNotSent marks an Action error where the operation certainly did
// not reach the target, so trying it again later cannot do it twice.
var ErrActionNotSent = errors.New("the operation was not sent")
