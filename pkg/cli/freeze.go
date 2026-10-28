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
	"os/user"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/audit"
	"github.com/tjst-t/sakutei/v2/internal/freeze"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
)

// freezeFlags are the flags of the global freeze (§14.3, Backlog #184),
// shared by the daemon and the one-shot reconcile.
type freezeFlags struct {
	fs     *flag.FlagSet
	freeze *bool
	reason *string
	by     *string
	until  *string
	state  *string
}

func addFreezeFlags(fs *flag.FlagSet) *freezeFlags {
	return &freezeFlags{
		fs: fs,
		freeze: fs.Bool("freeze", false,
			"global freeze: override to alert-only regardless of --mode (§14.3 escape hatch); needs --freeze-reason and --audit-log (#184: its start and end are recorded there)"),
		reason: fs.String("freeze-reason", "",
			"#184: why the run is frozen (required with --freeze; recorded in the audit log)"),
		by: fs.String("freeze-by", "",
			"#184: who freezes the run (recorded in the audit log; default: the OS user)"),
		until: fs.String("freeze-until", "",
			"#184: the --freeze deadline, an RFC 3339 time or a duration from now (e.g. 2h); once it passes the freeze is lifted and the expiry recorded (default: no deadline)"),
		state: fs.String("freeze-state", "",
			"#184: freeze state file written by `sakutei freeze start` / `end` (freeze a running daemon without stopping it); read every cycle, needs --audit-log (expiries are recorded there); empty=not used"),
	}
}

// parseFreezeUntil reads an RFC 3339 time or a duration from now (empty: no
// deadline).
func parseFreezeUntil(s string, now time.Time) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return time.Time{}, fmt.Errorf("%q is neither an RFC 3339 time nor a positive duration", s)
	}
	return now.Add(d).UTC(), nil
}

// freezeActor is who freezes: by, or the OS user.
func freezeActor(by string) string {
	if by = strings.TrimSpace(by); by != "" {
		return by
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

// control checks the flags and returns the freeze control of the run (nil:
// neither --freeze nor --freeze-state).  A --freeze is recorded as started in
// the audit log here, and as ended when the run exits (exitAfterEvents).  It
// exits 1 on a freeze without a reason, without an audit log, or with a flag
// that belongs to --freeze given alone.
func (f *freezeFlags) control(verb, auditLog string) *reconcile.FreezeControl {
	fail := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, verb+": "+format+"\n", args...)
		os.Exit(1)
	}
	if !*f.freeze && (*f.reason != "" || *f.by != "" || *f.until != "") {
		fail("--freeze-reason, --freeze-by and --freeze-until go with --freeze")
	}
	if !*f.freeze && *f.state == "" {
		return nil
	}
	if auditLog == "" {
		if *f.freeze {
			fail("--freeze needs --audit-log: the freeze's start and end, with who and why, are recorded there (§14.3, #184)")
		}
		fail("--freeze-state needs --audit-log: a freeze's expiry is recorded there (#184)")
	}
	c := &reconcile.FreezeControl{StatePath: *f.state, Audit: audit.NewLog(auditLog)}
	if !*f.freeze {
		return c
	}
	now := time.Now().UTC()
	until, err := parseFreezeUntil(*f.until, now)
	if err != nil {
		fail("invalid --freeze-until: %v", err)
	}
	fr := freeze.Freeze{Source: freeze.SourceFlag, By: freezeActor(*f.by), Reason: strings.TrimSpace(*f.reason), Since: now, Until: until}
	if strings.TrimSpace(*f.reason) == "" {
		fail("--freeze needs --freeze-reason: a freeze without a reason is refused (it is recorded in the audit log with who and until when, §14.3, #184)")
	}
	if err := fr.Validate(); err != nil {
		fail("--freeze: %v (use --freeze-by)", err)
	}
	if err := freeze.RecordStart(c.Audit, fr); err != nil {
		fail("--freeze: the start cannot be recorded: %v", err)
	}
	c.Flag = &fr
	exitHooks = append(exitHooks, func() { endFlagFreeze(verb, c) })
	return c
}

// endFlagFreeze records the end of the run's --freeze (unless it expired).
func endFlagFreeze(verb string, c *reconcile.FreezeControl) {
	if err := c.EndFlag("the run ended", time.Now().UTC()); err != nil {
		fmt.Fprintf(os.Stderr, "%s: warn: the end of --freeze cannot be recorded: %v\n", verb, err)
	}
}

// runFreeze implements "sakutei freeze": the write side of the freeze state
// file (Backlog #184).  A daemon (or one-shot reconcile) given the same
// --freeze-state reads it every cycle, so a running daemon is frozen without
// being stopped.
//
//	sakutei freeze start  --state <file> --audit-log <file> --reason <text> [--by <id>] [--until <time|duration>]
//	sakutei freeze end    --state <file> --audit-log <file> [--by <id>] [--reason <text>]
//	sakutei freeze status --state <file> [--audit-log <file>] [--json]
//
// The start and the end are appended to the S4 audit log before the file is
// written; a freeze past its deadline is lifted by whoever reads it next and
// recorded as expired.  start refuses a freeze without a reason, and one
// while another is in effect.
//
// Exit codes: 0 done; 1 error (usage, refused, unreadable file).
func runFreeze(args []string) {
	usage := func() {
		fmt.Fprintln(os.Stderr, "usage: sakutei freeze start  --state <file> --audit-log <file> --reason <text> [--by <id>] [--until <time|duration>]")
		fmt.Fprintln(os.Stderr, "       sakutei freeze end    --state <file> --audit-log <file> [--by <id>] [--reason <text>]")
		fmt.Fprintln(os.Stderr, "       sakutei freeze status --state <file> [--audit-log <file>] [--json]")
	}
	if len(args) == 0 {
		usage()
		os.Exit(1)
	}
	verb := args[0]
	fs := flag.NewFlagSet("freeze "+verb, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	state := fs.String("state", "", "the --freeze-state file of the daemon / the one-shot reconcile (required)")
	auditLog := fs.String("audit-log", "", "S4 audit log file (the start, end and expiry are recorded there)")
	var reason, by, until *string
	var asJSON *bool
	switch verb {
	case "start":
		reason = fs.String("reason", "", "why (required; recorded)")
		by = fs.String("by", "", "who freezes (recorded; default: the OS user)")
		until = fs.String("until", "", "deadline: an RFC 3339 time or a duration from now (e.g. 2h); default: none")
	case "end":
		reason = fs.String("reason", "", "why it is ended (optional; recorded)")
		by = fs.String("by", "", "who ends it (recorded; default: the OS user)")
	case "status":
		asJSON = fs.Bool("json", false, "print the freeze as one JSON object")
	default:
		usage()
		os.Exit(1)
	}
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		usage()
		os.Exit(1)
	}
	if *state == "" || (verb != "status" && *auditLog == "") {
		usage()
		os.Exit(1)
	}
	fail := func(err error) {
		fmt.Fprintf(os.Stderr, "sakutei freeze %s: %v\n", verb, err)
		os.Exit(1)
	}
	now := time.Now().UTC()
	switch verb {
	case "start":
		u, err := parseFreezeUntil(*until, now)
		if err != nil {
			fail(fmt.Errorf("invalid --until: %v", err))
		}
		f := freeze.Freeze{By: freezeActor(*by), Reason: strings.TrimSpace(*reason), Since: now, Until: u}
		if err := freeze.Start(*state, audit.NewLog(*auditLog), f); err != nil {
			fail(err)
		}
		f.Source = freeze.SourceCommand
		fmt.Printf("%s\nthe next cycle of the daemon (or the next reconcile) with --freeze-state %s is alert-only\n", f.Text(), *state)
	case "end":
		f, err := freeze.End(*state, audit.NewLog(*auditLog), freezeActor(*by), strings.TrimSpace(*reason), now)
		if err != nil {
			fail(err)
		}
		fmt.Printf("ended the freeze by %s (%s); the next cycle of the daemon (or the next reconcile) remediates again\n", f.By, f.Reason)
	case "status":
		var f *freeze.Freeze
		var err error
		if *auditLog != "" {
			// With the audit log a freeze past its deadline is lifted here.
			var expired *freeze.Freeze
			f, expired, err = freeze.Check(*state, audit.NewLog(*auditLog), now)
			if err == nil && expired != nil && !*asJSON {
				fmt.Printf("the freeze by %s expired at %s (recorded as expired)\n", expired.By, expired.Until.UTC().Format(time.RFC3339))
			}
		} else {
			f, err = freeze.Read(*state)
			if err == nil && f != nil && f.Expired(now) {
				// Past its deadline it is no longer in effect; recording the
				// expiry is left to whoever reads it with the audit log.
				f = nil
			}
		}
		if err != nil {
			fail(err)
		}
		if *asJSON {
			b, _ := json.MarshalIndent(struct {
				Frozen bool           `json:"frozen"`
				Freeze *freeze.Freeze `json:"freeze,omitempty"`
			}{f != nil, f}, "", "  ")
			fmt.Println(string(b))
			return
		}
		if f == nil {
			fmt.Println("not frozen")
			return
		}
		fmt.Println(f.Text())
	}
}
