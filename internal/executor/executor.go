// Copyright 2026 The Sakutei Authors
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

// Package executor defines the Executor seam between the reconcile loop and
// provider.Provider.Apply (§7 in spec/part3-convergence).
//
// Story Sead944-3 scope: LocalExecutor only (in-process, synchronous).
// Distributed / async executors are deferred to v2+ (ADR-0021).
//
// Design note: the Executor interface is intentionally minimal.  It receives a
// Plan and a Provider and calls Apply — nothing more.  Future extensions
// (backoff, in-flight tracking, rate limiting) will be added to the interface
// or via wrapping when those Stories arrive; the signature here MUST NOT be
// widened without filing an escalation.
package executor

import (
	"context"

	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// Executor is the seam between the reconcile loop and provider.Provider.Apply.
//
// In this Story, the only implementation is LocalExecutor.  The interface
// exists so the reconcile loop depends on the abstraction, not the concrete
// type, keeping the split-point (§6 seam principle) intact for later Stories.
type Executor interface {
	// Execute applies the given Plan using the given Provider.
	//
	// Apply is invoked only when plan.Ops is non-empty; the reconcile loop
	// skips resources that are already converged and does not call Execute
	// for them.  Even so, implementations SHOULD remain idempotent, since a
	// no-op apply is valid and must not error.
	Execute(ctx context.Context, p provider.Provider, plan core.Plan) error
}

// LocalExecutor is the in-process, synchronous Executor implementation.
// It delegates directly to provider.Provider.Apply with no additional
// orchestration — that is the full scope of this Story.
//
// Thread-safety: LocalExecutor holds no mutable state; it is safe for
// concurrent use.  The Provider implementations it is paired with are
// responsible for their own concurrency guarantees.
type LocalExecutor struct{}

// Compile-time assertion that LocalExecutor satisfies the Executor seam.
var _ Executor = (*LocalExecutor)(nil)

// New returns a new LocalExecutor.
func New() *LocalExecutor {
	return &LocalExecutor{}
}

// Execute calls provider.Apply with the given plan.
//
// The reconcile loop only calls Execute when plan.Ops is non-empty (already
// converged resources are skipped by the caller), so Apply here always has
// work to do.  A no-op apply would still be valid and must not error.
func (e *LocalExecutor) Execute(ctx context.Context, p provider.Provider, plan core.Plan) error {
	return p.Apply(ctx, plan)
}
