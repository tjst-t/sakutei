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
	"fmt"
)

// The observe / write credential split (§20.7, Backlog #185).
//
// A provider can be given two credentials: one to observe (Read, List,
// BulkObserve — what plan, status and the read-only API use) and one to
// write (Apply, Action).  Two seams tell it which one a call needs:
//
//   - The context of a write: the core calls Apply and Action with
//     WithWrite(ctx).  Writing(ctx) is true there (also in the Reads a
//     provider does inside its own Apply); everywhere else the provider uses
//     the observe credential only.  A provider resolves the write credential
//     just before the call that needs it and lets it go after (JIT, §20.2).
//   - The run's access, ConfigAccess in the factory's config: AccessObserve
//     when the run never writes (plan, status, import, sync, graph, and
//     reconcile with --mode alert-only or frozen, and the daemon with
//     --mode alert-only or a --freeze without a deadline).  Then the
//     provider neither resolves nor checks the write credential — it may be
//     missing from the secret store, and the run still starts — and refuses
//     a write with ErrNoWriteAccess.  Absent (or AccessWrite) is a run that
//     may write: a configured write credential is checked at load time.
const ConfigAccess = "access"

// The values of ConfigAccess.
const (
	AccessObserve = "observe"
	AccessWrite   = "write"
)

// ObserveOnlyRun reports whether the factory config says the run never
// writes (ConfigAccess is AccessObserve).
func ObserveOnlyRun(config map[string]string) bool {
	return config[ConfigAccess] == AccessObserve
}

// CheckAccess validates config[ConfigAccess] ("" is AccessWrite).
func CheckAccess(config map[string]string) error {
	switch v := config[ConfigAccess]; v {
	case "", AccessObserve, AccessWrite:
		return nil
	default:
		return fmt.Errorf("%s %q: want %q or %q", ConfigAccess, v, AccessObserve, AccessWrite)
	}
}

type writeKey struct{}

// WithWrite marks ctx as the context of a write (Apply, Action).
func WithWrite(ctx context.Context) context.Context {
	if Writing(ctx) {
		return ctx
	}
	return context.WithValue(ctx, writeKey{}, true)
}

// Writing reports whether ctx is the context of a write (WithWrite).
func Writing(ctx context.Context) bool {
	v, _ := ctx.Value(writeKey{}).(bool)
	return v
}

// ErrNoWriteAccess is the cause of the error a provider returns when asked
// to write in a run that does not (ConfigAccess AccessObserve): the write
// credential was never resolved.
var ErrNoWriteAccess = errors.New("this run does not write (" + ConfigAccess + ": " + AccessObserve + "): the write credential is not taken out")
