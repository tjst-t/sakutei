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
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tjst-t/sakutei/internal/gitdist"
)

// gitDistFlags は常駐ループの claim の git 配布（§17.4）・S2-free シャード（§17.5）・
// 資格局所性（§20.5）のフラグ。
type gitDistFlags struct {
	repo        *string
	workdir     *string
	shard       *string
	branch      *string
	poll        *string
	s2free      *bool
	secretStore *string
}

func addGitDistFlags(fs *flag.FlagSet) gitDistFlags {
	return gitDistFlags{
		repo: fs.String("intent-repo", "",
			"pull intent from this git repository (a read-only mirror at a site) with sparse checkout of the claim's intent/<scope>/ + intent/platform/ + shards/ (§17.4); replaces --intent"),
		workdir: fs.String("workdir", "",
			"with --intent-repo: disposable working directory for the checkout (<workdir>/repo) and render output (<workdir>/generated)"),
		shard: fs.String("shard", "",
			"with --intent-repo: read this shard's claim from shards/<name>.yaml in the repository (claim-as-intent, §17.1); a claim change PR hands scopes over (§17.7)"),
		branch: fs.String("git-branch", gitdist.DefaultBranch, "with --intent-repo: branch to follow"),
		poll: fs.String("git-poll", "",
			"with --intent-repo: how often to pull, plus up to 50% jitter (default: 60s)"),
		s2free: fs.Bool("s2-free", false,
			"S2-free shard (§17.5): Resource convergence only, no durable local state; requires --intent-repo and refuses --alloc-store"),
		secretStore: fs.String("secret-store", "",
			"site-local file SecretBackend; must hold only this claim's site credentials (<scope>/... paths, §20.5)"),
	}
}

// setupGitDist validates the git distribution flags and returns the Source
// (nil when --intent-repo is not set).  It exits on a usage error.
func setupGitDist(f gitDistFlags, intentDir, claim, allocStore string) *gitdist.Source {
	fail := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "sakutei: "+format+"\n", a...)
		os.Exit(1)
	}
	if *f.repo == "" {
		for name, set := range map[string]bool{
			"--workdir": *f.workdir != "", "--shard": *f.shard != "", "--git-poll": *f.poll != "",
			"--s2-free": *f.s2free,
		} {
			if set {
				fail("%s requires --intent-repo", name)
			}
		}
		if *f.secretStore != "" {
			sel, err := parseClaimSelector(claim)
			if err != nil || len(sel.Labels) == 0 {
				fail("--secret-store requires a claim (--claim or --shard): a site-local store is scoped to one site (§20.5)")
			}
			if err := gitdist.CheckSecretLocality(*f.secretStore, sel); err != nil {
				fail("%v", err)
			}
		}
		return nil
	}
	if intentDir != "" {
		fail("--intent and --intent-repo are exclusive")
	}
	if *f.workdir == "" {
		fail("--intent-repo requires --workdir <dir>")
	}
	if (*f.shard == "") == (claim == "") {
		fail("--intent-repo requires exactly one of --shard <name> (claim from git) or --claim <selector>")
	}
	if *f.s2free && allocStore != "" {
		fail("--s2-free refuses --alloc-store: a local allocation store is durable state the shard would lose with its host (use --alloc-url, §17.5)")
	}
	poll := gitdist.DefaultPoll
	if *f.poll != "" {
		d, err := time.ParseDuration(*f.poll)
		if err != nil || d <= 0 {
			fail("invalid --git-poll %q", *f.poll)
		}
		poll = d
	}
	src := &gitdist.Source{
		Checkout: &gitdist.Checkout{
			Remote: *f.repo,
			Dir:    filepath.Join(*f.workdir, "repo"),
			Branch: *f.branch,
		},
		Shard:       *f.shard,
		SecretStore: *f.secretStore,
		Poll:        poll,
	}
	if claim != "" {
		sel, err := parseClaimSelector(claim)
		if err != nil {
			fail("invalid --claim %q: %v", claim, err)
		}
		if _, err := gitdist.ScopeDir(sel); err != nil {
			fail("%v", err)
		}
		src.Claim = sel
	}
	if *f.s2free {
		fmt.Fprintf(os.Stdout, "sakutei: shard class: S2-free (Resource convergence only; everything local lives under %s and is rebuilt by pull, §17.5)\n", *f.workdir)
	}
	return src
}

// runMirror implements "sakutei mirror": keep a read-only git mirror of the
// central intent repository that sites pull from (§17.4 ミラー中継層).  The
// mirror holds no state of its own: delete it and the next sync rebuilds it.
func runMirror(args []string) {
	fs := flag.NewFlagSet("sakutei mirror", flag.ExitOnError)
	upstream := fs.String("upstream", "", "central intent repository (required)")
	dir := fs.String("dir", "", "bare mirror repository to serve sites from (required)")
	interval := fs.Duration("interval", 30*time.Second, "how often to follow the upstream")
	once := fs.Bool("once", false, "sync once and exit")
	_ = fs.Parse(args)
	if *upstream == "" || *dir == "" {
		fmt.Fprintln(os.Stderr, "sakutei mirror: --upstream and --dir are required")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	m := &gitdist.Mirror{Upstream: *upstream, Dir: *dir, Log: os.Stdout}
	for {
		if err := m.Sync(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "sakutei mirror: %v\n", err)
			if *once {
				os.Exit(1)
			}
		} else {
			fmt.Fprintf(os.Stdout, "sakutei mirror: synced %s from %s\n", *dir, *upstream)
		}
		if *once {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(*interval):
		}
	}
}

// runSecrets implements "sakutei secrets extract": build a site-local file
// SecretBackend holding only one site's credentials (§20.5 資格局所性).
func runSecrets(args []string) {
	if len(args) == 0 || args[0] != "extract" {
		fmt.Fprintln(os.Stderr, "usage: sakutei secrets extract --from <central store> --claim <selector> --out <site store>")
		os.Exit(1)
	}
	fs := flag.NewFlagSet("sakutei secrets extract", flag.ExitOnError)
	from := fs.String("from", "", "central file secret store (required)")
	claim := fs.String("claim", "", "the site's claim, e.g. site=tokyo (required)")
	out := fs.String("out", "", "site-local store to write (mode 0600, required)")
	_ = fs.Parse(args[1:])
	if *from == "" || *claim == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "sakutei secrets extract: --from, --claim and --out are required")
		os.Exit(1)
	}
	sel, err := parseClaimSelector(*claim)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sakutei secrets extract: invalid --claim %q: %v\n", *claim, err)
		os.Exit(1)
	}
	n, err := gitdist.ExtractSiteSecrets(*from, *out, sel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sakutei secrets extract: %v\n", err)
		os.Exit(1)
	}
	prefix, _ := gitdist.SecretPrefix(sel)
	fmt.Fprintf(os.Stdout, "sakutei secrets extract: wrote %d credential(s) under %s to %s\n", n, prefix, *out)
}
