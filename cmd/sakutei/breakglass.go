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
	"crypto/ed25519"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/tjst-t/sakutei/internal/gitdist"
)

const breakGlassUsage = `usage:
  sakutei breakglass keygen --out <key file>
  sakutei breakglass commit --repo <clone of the local mirror> --key <key file> --actor <id> --reason <text> --audit-log <S4 log>
  sakutei breakglass verify --repo <dir> --pubkey <key.pub>[,<key.pub>...] [--range <from>..<to> | <commit>...]
  sakutei breakglass status --mirror <local mirror>`

// runBreakGlass implements "sakutei breakglass": the write break-glass of the
// DR mode (§25.1 (b)).  A signed commit to the site's local mirror is allowed
// only while the central git is unreachable (§29.1 criterion), is recorded in
// S4 before it is pushed, and raises a BreakGlassWriteUsed Event.
// "sakutei mirror" opens the merge-back once the central is back.
func runBreakGlass(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, breakGlassUsage)
		os.Exit(1)
	}
	fail := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "sakutei breakglass: "+format+"\n", a...)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	switch args[0] {
	case "keygen":
		fs := flag.NewFlagSet("sakutei breakglass keygen", flag.ExitOnError)
		out := fs.String("out", "", "private key file to write (mode 0600); the public key goes to <out>.pub (required)")
		_ = fs.Parse(args[1:])
		if *out == "" {
			fail("--out is required")
		}
		fp, err := gitdist.GenerateBreakGlassKey(*out)
		if err != nil {
			fail("%v", err)
		}
		fmt.Fprintf(os.Stdout, "sakutei breakglass: wrote %s and %s.pub (key %s); keep the private key offline until a DR\n", *out, *out, fp)

	case "commit":
		fs := flag.NewFlagSet("sakutei breakglass commit", flag.ExitOnError)
		repo := fs.String("repo", "", "clone of the site's local mirror holding the emergency change (required)")
		branch := fs.String("branch", gitdist.DefaultBranch, "branch to write")
		keyPath := fs.String("key", "", "break-glass key (required)")
		actor := fs.String("actor", "", "who writes (recorded in the commit and in S4, required)")
		reason := fs.String("reason", "", "why (the commit subject, required)")
		auditLog := fs.String("audit-log", "", "S4 audit log (append-only, required)")
		attempts := fs.Int("probe-attempts", gitdist.DefaultProbeAttempts,
			fmt.Sprintf("consecutive failed probes of the central git needed to call it unreachable (at least %d)", gitdist.MinProbeAttempts))
		interval := fs.Duration("probe-interval", gitdist.DefaultProbeInterval, "pause between probes")
		timeout := fs.Duration("probe-timeout", gitdist.DefaultProbeTimeout, "timeout of one probe")
		_ = fs.Parse(args[1:])
		if *repo == "" || *keyPath == "" || *actor == "" || *reason == "" || *auditLog == "" {
			fail("--repo, --key, --actor, --reason and --audit-log are required (a break-glass write is never anonymous or silent)")
		}
		key, err := gitdist.LoadBreakGlassKey(*keyPath)
		if err != nil {
			fail("%v", err)
		}
		w := &gitdist.Write{
			Repo: *repo, Branch: *branch, Key: key, Actor: *actor, Reason: *reason, AuditLog: *auditLog,
			Probe: gitdist.Probe{Attempts: *attempts, Interval: *interval, Timeout: *timeout},
		}
		res, err := w.Commit(ctx)
		if err != nil {
			fail("%v", err)
		}
		fmt.Fprintf(os.Stderr, "sakutei: EVENT BreakGlassWriteUsed severity=critical: %s wrote %s to the local mirror %s with key %s (session %s); recorded in S4 %s; the central %s is unreachable\n",
			*actor, res.Commit, res.Mirror, res.Key, res.Session, *auditLog, res.Upstream)
		fmt.Fprintf(os.Stdout, "sakutei breakglass: committed %s (session %s); \"sakutei mirror\" opens the merge-back PR once the central git is back\n",
			res.Commit, res.Session)

	case "verify":
		fs := flag.NewFlagSet("sakutei breakglass verify", flag.ExitOnError)
		repo := fs.String("repo", ".", "repository holding the commits")
		pubs := fs.String("pubkey", "", "trusted break-glass public key(s), comma-separated (required)")
		rng := fs.String("range", "", "verify every break-glass commit in <from>..<to>")
		_ = fs.Parse(args[1:])
		if *pubs == "" {
			fail("--pubkey is required")
		}
		var keys []ed25519.PublicKey
		for _, p := range strings.Split(*pubs, ",") {
			k, err := gitdist.LoadBreakGlassPublicKey(p)
			if err != nil {
				fail("%v", err)
			}
			keys = append(keys, k)
		}
		commits := fs.Args()
		if *rng != "" {
			from, to, ok := strings.Cut(*rng, "..")
			if !ok {
				fail("--range must be <from>..<to>")
			}
			commits = append(commits, gitdist.BreakGlassCommits(ctx, *repo, from, to)...)
		}
		if len(commits) == 0 {
			fail("no break-glass commit to verify")
		}
		bad := 0
		for _, c := range commits {
			s, err := gitdist.VerifyBreakGlassCommit(ctx, *repo, c, keys)
			if err != nil {
				fmt.Fprintf(os.Stdout, "FAIL %s: %v\n", c, err)
				bad++
				continue
			}
			fmt.Fprintf(os.Stdout, "ok   %s session=%s actor=%s key=%s\n", s.Commit, s.Session, s.Actor, s.Key)
		}
		if bad > 0 {
			os.Exit(1)
		}

	case "status":
		fs := flag.NewFlagSet("sakutei breakglass status", flag.ExitOnError)
		mirror := fs.String("mirror", "", "local mirror (required)")
		_ = fs.Parse(args[1:])
		if *mirror == "" {
			fail("--mirror is required")
		}
		s, err := gitdist.ReadSession(*mirror)
		if err != nil {
			fail("%v", err)
		}
		if s == nil {
			fmt.Fprintln(os.Stdout, "no break-glass session: the mirror follows the central git")
			return
		}
		merge := "not opened yet (central unreachable)"
		if s.MergeBranch != "" {
			merge = fmt.Sprintf("branch %s, PR draft %s", s.MergeBranch, s.PRDraft)
		}
		fmt.Fprintf(os.Stdout, "session %s on %s: %d commit(s) since %s, keys %v, opened %s\nmerge-back: %s\n",
			s.ID, s.Branch, len(s.Commits), s.Base, s.Keys, s.Opened.Format("2006-01-02T15:04:05Z07:00"), merge)

	default:
		fmt.Fprintln(os.Stderr, breakGlassUsage)
		os.Exit(1)
	}
}
