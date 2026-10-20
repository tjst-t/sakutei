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

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/tjst-t/sakutei/internal/audit"
	"github.com/tjst-t/sakutei/internal/workflow"
)

// runApprove implements "sakutei approve" and "sakutei reject" (#42, §6.4):
//
//	sakutei approve --store <file> --audit-log <file> --approver <id> --signature <sig> <workflow> <gate>
//	sakutei reject  --store <file> --audit-log <file> --approver <id> --signature <sig> <workflow> <gate>
//
// The same decision as POST /workflows/{id}/gates/{gate}/approve: recorded in
// the S4 audit log first, then in the S2 workflow store.  Like the API it has
// no authorization of its own (§7).
func runApprove(decision workflow.Decision, args []string) {
	verb := string(decision)
	fs := flag.NewFlagSet("sakutei "+verb, flag.ExitOnError)
	store := fs.String("store", "", "workflow store file (required; same as the daemon's --workflow-store)")
	auditLog := fs.String("audit-log", "", "S4 audit log file (required; same as the daemon's --audit-log)")
	approver := fs.String("approver", "", "approver ID (required)")
	signature := fs.String("signature", "", "signature of the approver (required)")
	_ = fs.Parse(args)
	if *store == "" || *auditLog == "" || fs.NArg() != 2 {
		fmt.Fprintf(os.Stderr, "usage: sakutei %s --store <file> --audit-log <file> --approver <id> --signature <sig> <workflow> <gate>\n", verb)
		os.Exit(1)
	}
	run, err := workflow.Decide(context.Background(), workflow.NewFileStore(*store), audit.NewLog(*auditLog),
		fs.Arg(0), fs.Arg(1), workflow.Approval{Approver: *approver, Signature: *signature, Decision: decision}, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "sakutei %s: %v\n", verb, err)
		os.Exit(1)
	}
	if decision == workflow.DecisionReject {
		fmt.Fprintf(os.Stdout, "rejected gate %s of workflow %s (run %s); the workflow is halted\n", fs.Arg(1), fs.Arg(0), run.RunID)
		return
	}
	fmt.Fprintf(os.Stdout, "approved gate %s of workflow %s (run %s); the daemon proceeds on its next cycle\n", fs.Arg(1), fs.Arg(0), run.RunID)
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
		fmt.Fprintf(os.Stdout, "%-24s %-18s %-12s %-8s %s\n", "WORKFLOW", "PHASE", "GATE", "POS", "REASON")
		for _, r := range runs {
			fmt.Fprintf(os.Stdout, "%-24s %-18s %-12s %-8s %s\n", r.Workflow, r.Phase, r.Gate,
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
			if err := audit.NewLog(*auditLog).Append(audit.Record{Kind: audit.KindWorkflowCancelled,
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
