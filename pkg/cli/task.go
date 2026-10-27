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

	"github.com/tjst-t/sakutei/internal/task"
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
func runTask(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: sakutei task list|cancel --store <file> [resource]")
		os.Exit(1)
	}
	verb := args[0]
	fs := flag.NewFlagSet("sakutei task "+verb, flag.ExitOnError)
	store := fs.String("store", "", "task store file (required; same as the daemon's --task-store)")
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
			fmt.Fprintf(os.Stdout, "%-40s %-21s %-11s %-30s %-20s %s\n", r.Resource, phase, rb, r.TaskID, r.StartedAt.Format(time.RFC3339), r.Error)
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
	default:
		fmt.Fprintf(os.Stderr, "sakutei task: unknown verb %q (list|cancel)\n", verb)
		os.Exit(1)
	}
}
