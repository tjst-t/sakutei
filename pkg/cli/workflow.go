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

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/audit"
	"github.com/tjst-t/sakutei/v2/internal/budget"
	"github.com/tjst-t/sakutei/v2/internal/workflow"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// runApprove implements "sakutei approve" and "sakutei reject" (#42, §6.4):
//
//	sakutei approve --store <file> --audit-log <file> --approver <id> [--role <role>]... --signature <sig> <workflow> <gate>
//	sakutei reject  --store <file> --audit-log <file> --approver <id> --signature <sig> <workflow> <gate>
//
// The same decision as POST /workflows/{id}/gates/{gate}/approve: recorded in
// the S4 audit log first, then in the S2 workflow store.  Like the API it has
// no authorization of its own (§7): --role is the role the approver asserts
// (verified by the upstream IdP), counted against the gate's requirements.
func runApprove(decision workflow.Decision, args []string) {
	verb := string(decision)
	fs := flag.NewFlagSet("sakutei "+verb, flag.ExitOnError)
	store := fs.String("store", "", "workflow store file (required; same as the daemon's --workflow-store)")
	auditLog := fs.String("audit-log", "", "S4 audit log file (required; same as the daemon's --audit-log)")
	auditSink := addAuditSinkFlags(fs)
	approver := fs.String("approver", "", "approver ID (required)")
	signature := fs.String("signature", "", "signature of the approver (required)")
	var roles stringList
	fs.Var(&roles, "role", "role the approver holds (repeatable or comma-separated; checked against the gate's requirements.roles)")
	_ = fs.Parse(args)
	if *store == "" || *auditLog == "" || fs.NArg() != 2 {
		fmt.Fprintf(os.Stderr, "usage: sakutei %s --store <file> --audit-log <file> --approver <id> [--role <role>]... --signature <sig> <workflow> <gate>\n", verb)
		os.Exit(1)
	}
	run, err := workflow.Decide(context.Background(), workflow.NewFileStore(*store), auditSink.mustOpen("sakutei "+verb, *auditLog),
		fs.Arg(0), fs.Arg(1), workflow.Approval{Approver: *approver, Roles: roles, Signature: *signature, Decision: decision}, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "sakutei %s: %v\n", verb, err)
		os.Exit(1)
	}
	if decision == workflow.DecisionReject {
		fmt.Fprintf(os.Stdout, "rejected gate %s of workflow %s (run %s); the workflow is halted\n", fs.Arg(1), fs.Arg(0), run.RunID)
		return
	}
	approvers, need := run.Tally()
	if len(approvers) < need {
		fmt.Fprintf(os.Stdout, "approved gate %s of workflow %s (run %s); %d of %d approval(s), still awaiting more\n",
			fs.Arg(1), fs.Arg(0), run.RunID, len(approvers), need)
		return
	}
	fmt.Fprintf(os.Stdout, "approved gate %s of workflow %s (run %s); the daemon proceeds on its next cycle\n", fs.Arg(1), fs.Arg(0), run.RunID)
}

// stringList is a repeatable, comma-separated string flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			*l = append(*l, s)
		}
	}
	return nil
}

// runWorkflow implements "sakutei workflow" (#42, §6.3):
//
//	sakutei workflow list   --store <file>
//	sakutei workflow cancel --store <file> [--audit-log <file>] <workflow>
//
// cancel removes the run of a Workflow — typically a halted one, after a human
// checked reality.  The next cycle starts the Workflow again from the first
// stage (every step is idempotent and re-checked against reality).
func runWorkflow(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: sakutei workflow list|cancel --store <file> [workflow]")
		os.Exit(1)
	}
	verb := args[0]
	fs := flag.NewFlagSet("sakutei workflow "+verb, flag.ExitOnError)
	store := fs.String("store", "", "workflow store file (required; same as the daemon's --workflow-store)")
	auditLog := fs.String("audit-log", "", "S4 audit log file to record the cancel in (optional)")
	auditSink := addAuditSinkFlags(fs)
	_ = fs.Parse(args[1:])
	if *store == "" {
		fmt.Fprintf(os.Stderr, "sakutei workflow %s: --store <file> is required\n", verb)
		os.Exit(1)
	}
	st := workflow.NewFileStore(*store)
	ctx := context.Background()
	switch verb {
	case "list":
		runs, err := st.List(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sakutei workflow list: %v\n", err)
			os.Exit(1)
		}
		if len(runs) == 0 {
			fmt.Fprintln(os.Stdout, "no workflow run")
			return
		}
		// #111: a gate's approvals so far and its deadline, as in
		// GET /workflows/{id} and status (APPROVALS is "got/need").
		fmt.Fprintf(os.Stdout, "%-24s %-18s %-12s %-9s %-20s %-8s %s\n", "WORKFLOW", "PHASE", "GATE", "APPROVALS", "DEADLINE", "POS", "REASON")
		for _, r := range runs {
			approvals, deadline := "-", "-"
			if r.Phase == workflow.PhaseAwaitingApproval {
				got, need := r.Tally()
				approvals = fmt.Sprintf("%d/%d", len(got), need)
				if !r.GateDeadline.IsZero() {
					deadline = r.GateDeadline.UTC().Format(time.RFC3339)
				} else {
					deadline = "(none)"
				}
			}
			fmt.Fprintf(os.Stdout, "%-24s %-18s %-12s %-9s %-20s %-8s %s\n", r.Workflow, r.Phase, r.Gate, approvals, deadline,
				fmt.Sprintf("%d/%d/%d", r.Stage, r.Target, r.Step), r.Reason)
		}
	case "cancel":
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: sakutei workflow cancel --store <file> [--audit-log <file>] <workflow>")
			os.Exit(1)
		}
		run, ok, err := st.Delete(ctx, fs.Arg(0))
		if err != nil {
			fmt.Fprintf(os.Stderr, "sakutei workflow cancel: %v\n", err)
			os.Exit(1)
		}
		if !ok {
			fmt.Fprintf(os.Stderr, "sakutei workflow cancel: workflow %s has no run\n", fs.Arg(0))
			os.Exit(1)
		}
		if *auditLog != "" {
			if err := auditSink.mustOpen("sakutei workflow "+verb, *auditLog).Append(audit.Record{Kind: audit.KindWorkflowCancelled,
				Workflow: run.Workflow, RunID: run.RunID, Detail: fmt.Sprintf("cancelled at phase %s", run.Phase)}); err != nil {
				fmt.Fprintf(os.Stderr, "sakutei workflow cancel: %v\n", err)
				os.Exit(1)
			}
		}
		fmt.Fprintf(os.Stdout, "cancelled run %s of workflow %s (was %s)\n", run.RunID, run.Workflow, run.Phase)
	default:
		fmt.Fprintf(os.Stderr, "sakutei workflow: unknown verb %q (list|cancel)\n", verb)
		os.Exit(1)
	}
}

// serveApprovalAPI serves the approval gate API (§6.4) on addr in the
// background.  It has no authorization (§7): bind it to a trusted address or
// put an authenticating proxy in front.
func serveApprovalAPI(addr string, store workflow.Store, log *audit.Log) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("approval API: %w", err)
	}
	srv := &http.Server{Handler: (&workflow.API{Store: store, Audit: log}).Handler(), ReadHeaderTimeout: 10 * time.Second}
	fmt.Fprintf(os.Stdout, "sakutei: approval API listening on %s (POST /workflows/{id}/gates/{gate}/approve; no authorization, §7)\n", ln.Addr())
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "sakutei: approval API: %v\n", err)
		}
	}()
	return nil
}

// workflowHeldTarget is a target that plan / status show as driven by its
// Workflow instead of as a diff (#89): the daemon does not converge it until
// the Workflow is done.
type workflowHeldTarget struct {
	ID       core.ID
	Workflow string
	Stage    string
}

// workflowHeld returns the targets of the Workflows that are not done, keyed by
// Local ID, with their run's stage read from storePath (empty = no store).
// What is not done is decided as in a one-shot reconcile (#209): an open
// planned_change (by its window, approval not checked: the side that holds)
// for trigger: reimage, and done_when observed through providerFor.  Like
// the daemon it never writes the store.
func workflowHeld(defs []workflow.Definition, storePath string, planned []budget.PlannedChange, providerFor func(core.ID) provider.Provider) map[string]workflowHeldTarget {
	holds, storeKnown := judgeWorkflows(context.Background(), defs, storePath, declaredReimage(planned, time.Now), providerFor)
	held := map[string]workflowHeldTarget{}
	for _, h := range holds {
		stage := h.Describe(storeKnown)
		for _, id := range h.Targets {
			held[id.Local] = workflowHeldTarget{ID: id, Workflow: h.Workflow, Stage: stage}
		}
	}
	return held
}

// judgeWorkflows returns the Workflows that are not done (§6.3, #209), reading
// storePath when given (never writing it, never advancing a Workflow).
// storeKnown reports whether a store was given.
func judgeWorkflows(ctx context.Context, defs []workflow.Definition, storePath string, lanes workflow.ReimageOpener, providerFor func(core.ID) provider.Provider) (holds []workflow.Hold, storeKnown bool) {
	j := workflow.Judge{Lanes: lanes, ProviderFor: providerFor}
	if storePath != "" {
		j.Store = workflow.NewFileStore(storePath)
	}
	return j.Holds(ctx, defs), storePath != ""
}

// declaredReimage opens a planned_change kind: reimage by its window alone
// (#209): where the lanes are not evaluated (no S4 audit log, plan, status),
// a declaration in its window covering a trigger: reimage Workflow's scope
// leaves the targets to the reimage rather than converging them (the side
// that holds; an unapproved declaration is counted as open).
type declaredReimageLanes struct {
	planned []budget.PlannedChange
	now     func() time.Time
}

func declaredReimage(planned []budget.PlannedChange, now func() time.Time) workflow.ReimageOpener {
	return declaredReimageLanes{planned: planned, now: now}
}

func (d declaredReimageLanes) OpenReimage(scope core.Scope) (key, name string, left, ok bool) {
	now := d.now()
	for _, p := range d.planned {
		if p.Kind == budget.PlannedReimage && p.Active(now) && scope.Matches(p.Scope) {
			return p.Key(), p.String(), true, true
		}
	}
	return "", "", false, false
}
