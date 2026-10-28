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
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/audit"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
)

// exitApplyIneffective is the exit code of the one-shot reconcile when an
// Apply reported success but reality did not become what is declared (#166):
// verified after the settle, or not applied because the resource's Apply is
// stopped.  2 is not used: Go exits 2 on a panic, and wrappers (make) report
// their own failures as 2.
const exitApplyIneffective = 4

// applyCheckFlags are the flags of the Apply check (§11.1, Backlog #166),
// shared by the daemon and the one-shot reconcile.
type applyCheckFlags struct {
	fs          *flag.FlagSet
	settle      *time.Duration
	maxApplies  *int
	maxDuration *time.Duration
	state       *string
}

func addApplyCheckFlags(fs *flag.FlagSet) *applyCheckFlags {
	return &applyCheckFlags{
		fs: fs,
		settle: fs.Duration("settle", 0,
			"#166 (§11.1 Tier2): how long after an Apply that reported success reality may still differ before the Apply is judged not to have taken (default: the provider's, e.g. redfish-system 15s, proxmox-vm 30s; others re-read once); with the intent's apply_check: the longer wins"),
		maxApplies: fs.Int("max-ineffective-applies", 0,
			fmt.Sprintf("#166 (§11.1 Tier3): Applies of the same diff that may report success without changing reality before the resource's Apply is stopped (oscillating) (default %d); with the intent's apply_check: the smaller wins", reconcile.DefaultMaxIneffectiveApplies)),
		maxDuration: fs.Duration("max-ineffective-duration", 0,
			"#166: stop applying a resource once its first Apply that did not take is this long ago (default: no time limit); with the intent's apply_check: the shorter wins"),
		state: fs.String("apply-check-state", "",
			"#166: working record of the Applies that did not take (JSON); counts across runs of the one-shot reconcile, survives a daemon restart, and carries `sakutei apply-check release` requests; empty = memory only"),
	}
}

// flags returns the values given on the command line (nil when not given).
// It exits 1 on a value out of range.
func (a *applyCheckFlags) flags(verb string) reconcile.ApplyCheckFlags {
	var out reconcile.ApplyCheckFlags
	a.fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "settle":
			if *a.settle < 0 {
				fmt.Fprintf(os.Stderr, "%s: --settle must be >= 0\n", verb)
				os.Exit(1)
			}
			out.Settle = a.settle
		case "max-ineffective-applies":
			if *a.maxApplies < 1 {
				fmt.Fprintf(os.Stderr, "%s: --max-ineffective-applies must be >= 1\n", verb)
				os.Exit(1)
			}
			out.MaxApplies = a.maxApplies
		case "max-ineffective-duration":
			if *a.maxDuration < 0 {
				fmt.Fprintf(os.Stderr, "%s: --max-ineffective-duration must be >= 0\n", verb)
				os.Exit(1)
			}
			out.MaxDuration = a.maxDuration
		}
	})
	return out
}

// reobserveFlags are the limits of the re-observation after an upstream's
// Apply (§8.1, Backlog #177), shared by the daemon and the one-shot reconcile.
type reobserveFlags struct {
	max *int
	dur *time.Duration
}

func addReobserveFlags(fs *flag.FlagSet) *reobserveFlags {
	return &reobserveFlags{
		max: fs.Int("reobserve-max", reconcile.DefaultReobserveMax,
			"#177 (§8.1): how many times a resource not observed at the head of the run is observed again after an upstream's Apply has taken (0 = never: left to the next run)"),
		dur: fs.Duration("reobserve-time", reconcile.DefaultReobserveTime,
			"#177: the time a run (a daemon cycle) may spend observing again after an upstream's Apply; when it is used up the rest is left to the next run"),
	}
}

// limits returns the limits given.  It exits 1 on a value out of range.
func (r *reobserveFlags) limits(verb string) reconcile.ReobserveLimits {
	if *r.max < 0 {
		fmt.Fprintf(os.Stderr, "%s: --reobserve-max must be >= 0\n", verb)
		os.Exit(1)
	}
	if *r.dur <= 0 {
		fmt.Fprintf(os.Stderr, "%s: --reobserve-time must be > 0\n", verb)
		os.Exit(1)
	}
	return reconcile.ReobserveLimits{Off: *r.max == 0, MaxPerResource: *r.max, Time: *r.dur}
}

// runApplyCheck implements "sakutei apply-check": the working record of the
// Applies that did not take (§11.1, Backlog #166).
//
//	sakutei apply-check list    --state <file> [--json]
//	sakutei apply-check release --state <file> --audit-log <file> --approver <id> <resource>
//
// release lifts the stop of a resource (oscillating) in the record: the next
// one-shot reconcile, or the daemon's next cycle (with the same
// --apply-check-state), applies it again.  Like `sakutei task approve`, the
// release is appended to the S4 audit log before the record is written; the
// core holds no authorization of its own (§7).  <resource> is the ID shown by
// list (provider/scope/local) or the local ID.
//
// Exit codes: 0 done; 1 error (usage, unreadable file, nothing to release).
func runApplyCheck(args []string) {
	usage := func() {
		fmt.Fprintln(os.Stderr, "usage: sakutei apply-check list    --state <file> [--json]")
		fmt.Fprintln(os.Stderr, "       sakutei apply-check release --state <file> --audit-log <file> --approver <id> <resource>")
	}
	if len(args) == 0 {
		usage()
		os.Exit(1)
	}
	verb := args[0]
	fs := flag.NewFlagSet("apply-check "+verb, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	state := fs.String("state", "", "the --apply-check-state file of the daemon / the one-shot reconcile (required)")
	var asJSON *bool
	var auditLog, approver *string
	var auditSink *auditSinkFlags
	switch verb {
	case "list":
		asJSON = fs.Bool("json", false, "print the records as one JSON object")
	case "release":
		auditLog = fs.String("audit-log", "", "S4 audit log file (required; the release is recorded there first)")
		auditSink = addAuditSinkFlags(fs)
		approver = fs.String("approver", "", "who releases it (required; recorded)")
	default:
		usage()
		os.Exit(1)
	}
	if err := fs.Parse(args[1:]); err != nil {
		os.Exit(1)
	}
	if *state == "" {
		usage()
		os.Exit(1)
	}
	switch verb {
	case "list":
		st, err := reconcile.ReadApplyCheckState(*state)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sakutei apply-check list: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			if st.Resources == nil {
				st.Resources = []reconcile.ApplyRecordJSON{}
			}
			b, _ := json.MarshalIndent(st, "", "  ")
			fmt.Println(string(b))
			return
		}
		if len(st.Resources) == 0 {
			fmt.Println("no Apply that did not take is recorded")
		}
		for _, r := range st.Resources {
			state := fmt.Sprintf("counting (%d of %d)", r.IneffectiveApplies, r.MaxApplies)
			if r.Stopped {
				state = fmt.Sprintf("STOPPED since %s (%s)", r.StoppedAt.UTC().Format(time.RFC3339), r.Reason)
			}
			fmt.Printf("%s: %s\n  diff: %s\n  ineffective applies: %d (settle %s, first %s, last %s)\n",
				r.Resource, state, r.Diff, r.IneffectiveApplies, r.Settle,
				r.FirstIneffective.UTC().Format(time.RFC3339), r.LastIneffective.UTC().Format(time.RFC3339))
		}
		for _, rq := range st.Releases {
			fmt.Printf("release requested: %s by %q at %s (taken in by the next cycle / run)\n",
				rq.Resource, rq.Approver, rq.RequestedAt.UTC().Format(time.RFC3339))
		}
	case "release":
		if *auditLog == "" || *approver == "" || fs.NArg() != 1 {
			usage()
			os.Exit(1)
		}
		resource := fs.Arg(0)
		now := time.Now().UTC()
		err := reconcile.UpdateApplyCheckState(*state, func(st *reconcile.ApplyCheckState) error {
			kept := st.Resources[:0]
			found := false
			for _, r := range st.Resources {
				if r.Resource == resource || r.LocalID == resource {
					found = true
					continue
				}
				kept = append(kept, r)
			}
			if !found {
				return fmt.Errorf("%s is not recorded in %s (see sakutei apply-check list)", resource, *state)
			}
			if err := auditSink.mustOpen("sakutei apply-check "+verb, *auditLog).Append(audit.Record{
				Time: now, Kind: audit.KindApplyCheckReleased, Resource: resource, Actor: *approver,
				Detail: "the stop of the resource's Apply (oscillating, §11.1 Tier3) is lifted; it is applied again",
			}); err != nil {
				return fmt.Errorf("audit log: %w", err)
			}
			st.Resources = kept
			st.Releases = append(st.Releases, reconcile.ApplyReleaseJSON{Resource: resource, Approver: *approver, RequestedAt: now})
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "sakutei apply-check release: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("released %s: the next cycle of the daemon (or the next reconcile) applies it again\n", resource)
	}
}
