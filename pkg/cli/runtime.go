// Copyright 2026 tjst-t
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
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
	"strings"

	"github.com/tjst-t/sakutei/v2/internal/gitdist"
	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/internal/runtimeargs"
)

// The runtime: keys each verb takes (§26.7, Backlog #183); a key the verb
// has no flag for is not looked at (the one-shot reconcile has no interval).
// The daemon takes them all (nil).
var (
	runtimeBudgetKeys = []string{"max_changes_per_cycle", "max_destructive_per_cycle",
		"max_destructive_per_window", "window_duration", "on_exceed"}
	runtimeReconcileKeys = append([]string{"mode", "settle", "max_ineffective_applies",
		"max_ineffective_duration", "reobserve_max", "reobserve_time"}, runtimeBudgetKeys...)
)

// applyRuntime checks the start flags given on fs against the intent's
// runtime: decl (§26.7, Backlog #183) and returns the flags that tighten it
// (a smaller budget, a weaker mode: they win, as a temporary tightening).  A
// value only the intent declares is set on fs, so the rest of the verb reads
// it as if given.  A flag loosening the declaration, or an argument without a
// direction that differs from it, refuses the start: one line each with the
// argument's name, both values and how to fix it; exit 1.
func applyRuntime(verb string, fs *flag.FlagSet, decl *runtimeargs.Declaration, used []string) []runtimeargs.Tightening {
	ts, err := checkRuntime(verb, fs, decl, used)
	if err != nil {
		fmt.Fprintln(logErr, err)
		exitAfterEvents(1)
	}
	return ts
}

// checkRuntime is applyRuntime returning the refusal (its lines, each
// prefixed with verb) instead of exiting.
func checkRuntime(verb string, fs *flag.FlagSet, decl *runtimeargs.Declaration, used []string) ([]runtimeargs.Tightening, error) {
	flags := map[string]string{}
	var lines []string
	fs.Visit(func(f *flag.Flag) {
		a, ok := runtimeargs.ByFlag(f.Name)
		if !ok || (used != nil && !containsString(used, a.Key)) {
			return
		}
		v, err := a.Normalize(f.Value.String())
		if err != nil {
			lines = append(lines, fmt.Sprintf("%s: invalid --%s: %v", verb, f.Name, err))
			return
		}
		flags[a.Key] = v
	})
	if len(lines) == 0 {
		res := runtimeargs.Resolve(decl, flags, used)
		for _, c := range res.Conflicts {
			lines = append(lines, fmt.Sprintf("%s: %s", verb, c.Error()))
		}
		if len(lines) == 0 {
			for _, k := range res.FromIntent {
				a, _ := runtimeargs.Lookup(k)
				if err := fs.Set(a.Flag, res.Values[k]); err != nil {
					lines = append(lines, fmt.Sprintf("%s: runtime.%s %s: %v", verb, k, res.Values[k], err))
				}
			}
		}
		if len(lines) == 0 {
			return res.Tightened, nil
		}
	}
	return nil, errors.New(strings.Join(lines, "\n"))
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// daemonRuntime reads the runtime: the daemon starts with (runtime.daemon:,
// else runtime:, #193) and checks the flags against it.  With --intent-repo the intent is pulled once here (the
// loop's first cycle takes that pull, src is primed with it); when the intent
// cannot be read yet the daemon starts on the flags alone and reports any
// runtime: it reads later as a change (RuntimeDeclarationChanged).
func daemonRuntime(fs *flag.FlagSet, intentDir string, src *gitdist.Source) (*reconcile.RuntimeReport, reconcile.Source) {
	rt := &reconcile.RuntimeReport{Unread: true}
	var source reconcile.Source
	if src != nil {
		p := &primedSource{Source: src}
		source = p
		if st, err := src.Refresh(context.Background()); err == nil {
			p.first = &st
			if !st.Idle {
				intentDir = src.IntentDir()
			} else {
				intentDir = ""
			}
		} else {
			intentDir = ""
		}
	}
	if intentDir != "" {
		load := loader.LoadIntent
		if src != nil {
			load = loader.LoadIntentTree
		}
		if in, err := load(intentDir); err == nil {
			rt.Start, rt.Unread = in.Runtime.For(runtimeargs.Daemon), false
		}
	}
	rt.Tightened = applyRuntime("sakutei", fs, rt.Start, nil)
	if rt.Unread {
		fmt.Fprintln(logErr, "sakutei: note: the intent could not be read at start; the start arguments are the flags alone, and a runtime: read later is reported as RuntimeDeclarationChanged until restart (§26.7)")
	}
	for _, t := range rt.Tightened {
		fmt.Fprintf(logErr, "sakutei: %s\n", t)
	}
	return rt, source
}

// primedSource hands the loop's first Refresh the pull daemonRuntime made at
// start, so nothing it carried (break-glass commits) is lost.
type primedSource struct {
	*gitdist.Source
	first *reconcile.SourceState
}

func (p *primedSource) Refresh(ctx context.Context) (reconcile.SourceState, error) {
	if p.first != nil {
		st := *p.first
		p.first = nil
		return st, nil
	}
	return p.Source.Refresh(ctx)
}
