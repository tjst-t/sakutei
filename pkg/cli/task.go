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
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/audit"
	"github.com/tjst-t/sakutei/v2/internal/task"
)

// runTask implements "sakutei task" (#41, §6.2):
//
//	sakutei task list   --store <file>
//	sakutei task cancel --store <file> <resource>
//
// cancel removes the outstanding record of a resource — an interrupted or
// failed Task, or one whose completion mark is never observed.  It is the
// human's "I checked reality" step: the daemon never re-runs a non-idempotent
// Task on its own, and launches a new one only after the record is gone and the
// diff still remains (inside the window).
//
//	sakutei task approve --store <file> --audit-log <file> --approver <id> [--role <role>]... [--signature <sig>] <resource>
//	sakutei task reject  --store <file> --audit-log <file> --approver <id> [--role <role>]... [--signature <sig>] <resource>
//
// approve / reject decide the approval gate of a phased Task that stopped inside
// its critical section (#52, §29.1; phase awaiting_approval): approve hands it
// back to the daemon at the start of that step, reject halts it.  Recorded in
// the S4 audit log first; no authorization of its own (§7).
func runTask(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: sakutei task list|cancel|approve|reject --store <file> [resource]")
		os.Exit(1)
	}
	verb := args[0]
	fs := flag.NewFlagSet("sakutei task "+verb, flag.ExitOnError)
	store := fs.String("store", "", "task store file (required; same as the daemon's --task-store)")
	// #52: approve / reject the approval gate of a phased Task stopped inside
	// its critical section (§29.1).  Recorded in S4 first, like `sakutei approve`.
	var auditLog, approver, signature *string
	var roles stringList
	if verb == "approve" || verb == "reject" {
		auditLog = fs.String("audit-log", "", "S4 audit log file (required; same as the daemon's --audit-log)")
		approver = fs.String("approver", "", "approver ID (required)")
		signature = fs.String("signature", "", "signature of the approver (recorded, not checked: §7)")
		fs.Var(&roles, "role", "role the approver holds (repeatable or comma-separated; recorded)")
	}
	_ = fs.Parse(args[1:])
	if *store == "" {
		fmt.Fprintf(os.Stderr, "sakutei task %s: --store <file> is required\n", verb)
		os.Exit(1)
	}
	st := task.NewFileStore(*store)
	ctx := context.Background()
	switch verb {
	case "list":
		recs, err := st.List(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sakutei task list: %v\n", err)
			os.Exit(1)
		}
		if len(recs) == 0 {
			fmt.Fprintln(os.Stdout, "no outstanding task")
			return
		}
		// #113: ROLLBACK says whether rollback_on_fail rolled the failed Task back.
		fmt.Fprintf(os.Stdout, "%-40s %-21s %-11s %-30s %-20s %s\n", "RESOURCE", "PHASE", "ROLLBACK", "TASK", "STARTED", "ERROR")
		for _, r := range recs {
			rb := "-"
			if r.RolledBack {
				rb = "rolled-back"
			}
			// #51: a phased Task shows the step it is in (running:rebooting).
			phase := string(r.Phase)
			if r.Step != "" {
				phase += ":" + r.Step
			}
			// #150: a manual-procedure Task also says what the person is asked to do.
			msg := r.Error
			if r.Notice != "" {
				msg += " — " + r.Notice
			}
			fmt.Fprintf(os.Stdout, "%-40s %-21s %-11s %-30s %-20s %s\n", r.Resource, phase, rb, r.TaskID, r.StartedAt.Format(time.RFC3339), msg)
		}
	case "cancel":
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: sakutei task cancel --store <file> <resource>")
			os.Exit(1)
		}
		rec, ok, err := st.Delete(ctx, fs.Arg(0), "")
		if err != nil {
			fmt.Fprintf(os.Stderr, "sakutei task cancel: %v\n", err)
			os.Exit(1)
		}
		if !ok {
			fmt.Fprintf(os.Stderr, "sakutei task cancel: no outstanding task for %s\n", fs.Arg(0))
			os.Exit(1)
		}
		fmt.Fprintf(os.Stdout, "cancelled task %s for %s (was %s)\n", rec.TaskID, rec.Resource, rec.Phase)
	case "approve", "reject":
		if *auditLog == "" || *approver == "" || fs.NArg() != 1 {
			fmt.Fprintf(os.Stderr, "usage: sakutei task %s --store <file> --audit-log <file> --approver <id> [--role <role>]... [--signature <sig>] <resource>\n", verb)
			os.Exit(1)
		}
		rec, err := task.Decide(ctx, st, audit.NewLog(*auditLog), fs.Arg(0),
			task.Approval{Approver: *approver, Roles: roles, Signature: *signature, Reject: verb == "reject"}, time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "sakutei task %s: %v\n", verb, err)
			os.Exit(1)
		}
		if verb == "reject" {
			fmt.Fprintf(os.Stdout, "rejected task %s for %s; halted until cancelled (sakutei task cancel)\n", rec.TaskID, rec.Resource)
			return
		}
		fmt.Fprintf(os.Stdout, "approved task %s for %s; the daemon resumes it at step %s on its next cycle\n", rec.TaskID, rec.Resource, rec.Step)
	default:
		fmt.Fprintf(os.Stderr, "sakutei task: unknown verb %q (list|cancel|approve|reject)\n", verb)
		os.Exit(1)
	}
}
