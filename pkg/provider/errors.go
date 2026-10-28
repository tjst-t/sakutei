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
	"errors"
	"fmt"
)

// TransientError wraps an Apply (or Read) error that is expected to resolve
// on its own with time.  The controller will retry with exponential back-off
// (ceiling ~15 minutes) (§5.5).
//
// Examples: API timeout, transient network failure, lock contention.
type TransientError struct {
	// Cause is the underlying error.
	Cause error
}

func (e *TransientError) Error() string {
	return fmt.Sprintf("transient: %v", e.Cause)
}

func (e *TransientError) Unwrap() error { return e.Cause }

// PermanentError wraps an Apply (or Read) error that will not resolve without
// a change to desired state or the provider version.  The controller moves the
// resource to "stuck" and does not retry until the desired state or
// provider_version changes (§5.5).
//
// Examples: permission denied, schema validation failure.
type PermanentError struct {
	// Cause is the underlying error.
	Cause error
}

func (e *PermanentError) Error() string {
	return fmt.Sprintf("permanent: %v", e.Cause)
}

func (e *PermanentError) Unwrap() error { return e.Cause }

// NewTransientError constructs a TransientError from an underlying error.
func NewTransientError(cause error) *TransientError {
	return &TransientError{Cause: cause}
}

// NewPermanentError constructs a PermanentError from an underlying error.
func NewPermanentError(cause error) *PermanentError {
	return &PermanentError{Cause: cause}
}

// RolledBackError marks an Apply whose change reached the target but was
// undone there (rolled back) because the change could not be confirmed —
// e.g. the nftables lockout guard (Backlog #173): a change to a base chain is
// rolled back by a timer on the host when a new connection to the host fails
// after it.  The Apply failed, and the same Apply would very likely be undone
// again, so the core does not repeat it: like an Apply that did not take
// (§11.1 Tier3, #166), the resource's Apply of the same diff is stopped
// (oscillating) until the diff changes, reality converges or a person
// releases it ("sakutei apply-check release").
//
// Always build it with NewRolledBackError, which wraps it in a
// TransientError (the target is back where it was; nothing is stuck).
type RolledBackError struct {
	// Cause says what could not be confirmed and what was restored.
	Cause error
}

func (e *RolledBackError) Error() string {
	return fmt.Sprintf("rolled back: %v", e.Cause)
}

func (e *RolledBackError) Unwrap() error { return e.Cause }

// NewRolledBackError constructs a TransientError wrapping a RolledBackError.
func NewRolledBackError(cause error) *TransientError {
	return NewTransientError(&RolledBackError{Cause: cause})
}

// IsRolledBack reports whether err (or any error in its chain) is a
// RolledBackError.
func IsRolledBack(err error) bool {
	var rb *RolledBackError
	return errors.As(err, &rb)
}
