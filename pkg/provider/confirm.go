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
	"fmt"
	"regexp"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// The checks after a change (§23.4, Backlog #200).
//
// A provider that confirms a change before it commits it (CommitConfirm,
// e.g. netconf's commit confirmed) can let the declaration name, besides
// reaching the target itself, two more kinds of check, both made only after
// the change:
//
//   - observe: another resource of the intent, read again until a subset of
//     its fields is as expected.  The core reads it as it observes it every
//     cycle: through its own provider, with the observe credential (never the
//     write credential of the Apply that asks, §20.7) and within its range
//     (only a resource the intent declares, so its provider's range — e.g.
//     netconf's subtree and the device's NACM — is all that is read).
//   - check: a function registered by name (pkg/cli.WithConfirmCheck, or the
//     Checks of a network_policy realizer's Output).  What it reads and how it
//     judges is the function's.
//
// The core gives such a provider a Confirmer (ConfirmUser) before it
// validates the intent; the provider checks the names at load time
// (CheckObserve, HasCheck) and calls Observe / Check while its change waits to
// be confirmed, again and again until nil or its time limit.

var confirmCheckName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// ValidConfirmCheckName checks the name of a check function (letters,
// digits, '_', '.', '-'; it is written after check: in a declaration).
func ValidConfirmCheckName(name string) error {
	if !confirmCheckName.MatchString(name) {
		return fmt.Errorf("check name %q: want letters, digits, '_', '.', '-' (starting with a letter or a digit)", name)
	}
	return nil
}

// ConfirmTarget is the change a check is asked about.
type ConfirmTarget struct {
	// ID is the resource that was changed.
	ID core.ID
	// Fields are the declared values the change carries (field → value).
	Fields map[string]string
}

// ConfirmCheck is a check function: nil when the change has taken as it
// should, an error saying why not otherwise.  It is called again until nil or
// the caller's time limit (ctx's deadline), with ctx marked as an observation
// (not Writing): a check reads, it never writes.
type ConfirmCheck func(ctx context.Context, t ConfirmTarget) error

// Confirmer is the core's part of the checks after a change.
type Confirmer interface {
	// CheckObserve is the load-time check of an observe: the resource must
	// be declared in the intent (not ensure: absent) and every expected field
	// must be one its provider compares (in its Schema; not computed,
	// managed or secret).  It MUST NOT do I/O.
	CheckObserve(id core.ID, expect map[string]string) error
	// HasCheck reports whether a check function is registered under name.
	HasCheck(name string) bool
	// Observe reads id once with the observe credential and returns nil when
	// every field of expect is as expected (compared as the core compares
	// the declaration, including the provider's PlanRefiner), otherwise why
	// not (also: not observable, absent).
	Observe(ctx context.Context, id core.ID, expect map[string]string) error
	// Check calls the check function name once.
	Check(ctx context.Context, name string, t ConfirmTarget) error
}

// ConfirmUser is an OPTIONAL Provider capability, discovered by type
// assertion: the core hands it the Confirmer of the loaded intent before it
// validates the intent, and again whenever the intent is loaded anew.
// UseConfirmer may be called while an Apply runs (the daemon), so the
// provider keeps it safely for concurrent use.
type ConfirmUser interface {
	UseConfirmer(Confirmer)
}

// WithObserve marks ctx as an observation, also inside a write: Writing is
// false under it, so a provider takes its observe credential (§20.7).  The
// core reads the observe: of a check after a change with it.
func WithObserve(ctx context.Context) context.Context {
	if !Writing(ctx) {
		return ctx
	}
	return context.WithValue(ctx, writeKey{}, false)
}
