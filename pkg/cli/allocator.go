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
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/allocator"
	"github.com/tjst-t/sakutei/v2/internal/allocwriteback"
	"github.com/tjst-t/sakutei/v2/internal/audit"
	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/pkg/allocation"
)

// runAllocator is "sakutei allocator": the allocation authority as a service,
// the HA minimal form of §19.8 (#25).
//
//	sakutei allocator --store <file> --listen <addr> [--standby]
//
// Run one instance per node of the stage-0 VM-HA, all on the same store (the
// S2 location).  Without --standby the instance promotes itself on start: it
// takes a new fencing generation, so a former active still alive is fenced
// and steps down.  With --standby it answers 503 until POST /v1/promote.
// Clients (`--alloc-url a,b`) fail over to whichever instance is active.
//
// The first argument "release", "list" or "import" selects the thin admin CLI
// instead (#116, #157, runAllocatorAdmin); the service itself is unchanged.
func runAllocator(args []string) {
	if len(args) > 0 && (args[0] == "release" || args[0] == "list" || args[0] == "import") {
		runAllocatorAdmin(args[0], args[1:])
		return
	}
	fs := flag.NewFlagSet("sakutei allocator", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	store := fs.String("store", "", "path to the allocation store file shared by the active/standby pair (required)")
	listen := fs.String("listen", "127.0.0.1:7070", "address to listen on (port 0 picks a free port)")
	standby := fs.Bool("standby", false, "start as standby: serve 503 until POST /v1/promote")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if *store == "" {
		fmt.Fprintln(os.Stderr, "sakutei allocator: --store is required")
		os.Exit(2)
	}

	// Listen before promoting: a promotion fences the current active, so an
	// instance that then fails to start (port in use) would leave the pair
	// with no active at all.
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sakutei allocator: listen: %v\n", err)
		os.Exit(1)
	}
	srv := allocator.NewServer(allocator.New(*store))
	role := "standby"
	if !*standby {
		gen, err := srv.Promote()
		if err != nil {
			fmt.Fprintf(os.Stderr, "sakutei allocator: promote: %v\n", err)
			os.Exit(1)
		}
		role = fmt.Sprintf("active (generation %d)", gen)
	}

	fmt.Printf("sakutei allocator: %s listening on http://%s\n", role, ln.Addr())

	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(shutdownCtx)
	}()
	if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "sakutei allocator: %v\n", err)
		os.Exit(1)
	}
}

// allocAdmin is what `sakutei allocator release|list` needs of an authority:
// the store file (FileAuthority) or the service (Client).
type allocAdmin interface {
	allocation.AllocationAuthority
	ReleaseOwner(ctx context.Context, pool, owner string) (string, bool, error)
	List(ctx context.Context, pool string) ([]allocator.Record, error)
	ImportPool(ctx context.Context, pool string, spec allocation.PoolSpec, entries []allocator.ImportEntry, last map[string]int) (int, error)
}

// runAllocatorAdmin implements the thin admin CLI of the allocator (#116):
//
//	sakutei allocator release --pool <p> --owner <o> (--store <file> | --url <a,b>) [--audit-log <file>]
//	sakutei allocator list    (--store <file> | --url <a,b>) [--pool <p>]
//	sakutei allocator import  --intent <dir> (--store <file> | --url <a,b>) [--pool <p>] [--alloc-write-back <file>] [--audit-log <file>]
//
// With --intent (and --alloc-write-back, as the other verbs), release also
// removes the owner from the pool's write-back file when the pool is
// declared write_back: true (#157), the same way reconcile writes it.  import
// rebuilds the store from the write-back files (all write_back pools, or
// --pool): an entry that disagrees with a record already in the store makes
// it refuse that pool and write nothing to it (§19.7).
//
// release is the human path to give a value back (§19.3): v1 has no automatic
// Release (§19.5).  It goes through AllocationAuthority.Release, so the value
// cools down (§19.5 (b)) and is not handed out again at once.  --store works
// on the file directly (the `--alloc-store` mode, flock); --url calls
// `POST /v1/release` on the active allocator service.  A store served by
// `sakutei allocator` is fenced against direct writes: use --url for it.
func runAllocatorAdmin(verb string, args []string) {
	fs := flag.NewFlagSet("sakutei allocator "+verb, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	store := fs.String("store", "", "allocation store file (as --alloc-store / the service's --store)")
	urls := fs.String("url", "", "comma-separated base URLs of the allocator service (as --alloc-url)")
	pool := fs.String("pool", "", "pool name (required for release; list shows every pool when omitted)")
	owner := fs.String("owner", "", "owner to release (release only, required)")
	auditLog := fs.String("audit-log", "", "S4 audit log file to record the release / import in (optional)")
	auditSink := addAuditSinkFlags(fs)
	intentDir := fs.String("intent", "", "#157: intent directory, to find the pools declared write_back: true (import: required; release: removes the owner from the write-back file)")
	writeBack := fs.String("alloc-write-back", "", allocWriteBackUsage)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	usage := "usage: sakutei allocator list (--store <file> | --url <a,b>) [--pool <p>]"
	switch verb {
	case "release":
		usage = "usage: sakutei allocator release --pool <p> --owner <o> (--store <file> | --url <a,b>) [--intent <dir> [--alloc-write-back <file>]] [--audit-log <file>]"
	case "import":
		usage = "usage: sakutei allocator import --intent <dir> (--store <file> | --url <a,b>) [--pool <p>] [--alloc-write-back <file>] [--audit-log <file>]"
	}
	if (*store == "") == (*urls == "") || fs.NArg() != 0 || (verb == "release" && (*pool == "" || *owner == "")) || (verb == "import" && *intentDir == "") {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	var auth allocAdmin
	if *store != "" {
		auth = allocator.New(*store)
	} else {
		auth = allocator.NewClient(strings.Split(*urls, ","))
	}
	ctx := context.Background()
	prefix := "sakutei allocator " + verb
	var wb *allocwriteback.Authority
	if *intentDir != "" {
		in, err := loader.LoadIntent(*intentDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: load intent: %v\n", prefix, err)
			os.Exit(1)
		}
		cfg, err := allocwriteback.LoadConfig(*writeBack)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: --alloc-write-back: %v\n", prefix, err)
			os.Exit(1)
		}
		wb = allocwriteback.New(auth, cfg, *intentDir)
		wb.SetIntentPools(in.Pools)
	}
	switch verb {
	case "import":
		os.Exit(runAllocatorImport(ctx, prefix, auth, wb, *pool, auditSink.mustOpen("sakutei allocator "+verb, *auditLog)))
	case "release":
		value, ok, err := auth.ReleaseOwner(ctx, *pool, *owner)
		if errors.Is(err, allocation.ErrFenced) {
			err = fmt.Errorf("%w (the store is served by `sakutei allocator`: release through --url)", err)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", prefix, err)
			os.Exit(1)
		}
		if !ok {
			fmt.Fprintf(os.Stderr, "%s: pool %q has no allocation for owner %q: nothing released\n", prefix, *pool, *owner)
			os.Exit(1)
		}
		if wb != nil {
			// #157: the owner leaves the write-back file the same way an
			// allocation enters it (commit / review / file).
			if err := wb.Forget(ctx, *pool, *owner); err != nil {
				fmt.Fprintf(os.Stderr, "%s: released in the store, but removing it from the write-back file %s failed: %v (remove the entry by hand)\n", prefix, wb.FilePath(*pool), err)
				os.Exit(1)
			}
		} else {
			fmt.Fprintf(os.Stderr, "%s: note: without --intent the owner is not removed from a write-back file (write_back: true pools, §19.7); if the pool has one, remove the entry from it\n", prefix)
		}
		until := cooldownUntil(ctx, auth, *pool, value)
		if *auditLog != "" {
			detail := fmt.Sprintf("released pool %s owner %s value %s", *pool, *owner, value)
			if !until.IsZero() {
				detail += " (cooldown until " + until.UTC().Format(time.RFC3339) + ")"
			}
			if err := auditSink.mustOpen("sakutei allocator "+verb, *auditLog).Append(audit.Record{Kind: audit.KindAllocationReleased, Detail: detail}); err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", prefix, err)
				os.Exit(1)
			}
		}
		line := fmt.Sprintf("released value %s of pool %s from owner %s", value, *pool, *owner)
		switch {
		case !until.IsZero():
			line += fmt.Sprintf("; cooling down until %s, not reused before", until.UTC().Format(time.RFC3339))
		case retired(ctx, auth, *pool, value):
			line += "; retired (reuse: false): never handed out again"
		}
		fmt.Fprintln(os.Stdout, line)
	case "list":
		recs, err := auth.List(ctx, *pool)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", prefix, err)
			os.Exit(1)
		}
		if len(recs) == 0 {
			fmt.Fprintln(os.Stdout, "no allocation")
			return
		}
		now := time.Now()
		fmt.Fprintf(os.Stdout, "%-16s %-32s %-8s %-10s %s\n", "POOL", "OWNER", "VALUE", "STATE", "LEFT")
		for _, r := range recs {
			owner, left := r.Owner, "-"
			if owner == "" {
				owner = "-"
			}
			if r.Until != nil {
				left = r.Until.Sub(now).Truncate(time.Second).String()
			}
			if r.State == allocator.StateRetired {
				left = "-" // released at Until; never comes back
			}
			fmt.Fprintf(os.Stdout, "%-16s %-32s %-8d %-10s %s\n", allocation.BlockRef(r.Pool, r.Block), owner, r.Value, r.State, left)
		}
	}
}

// runAllocatorImport rebuilds the store from the write-back files (#157,
// §19.7) and returns the exit code.  Each pool is all or nothing: an entry
// that disagrees with the store makes the pool refused, nothing written to
// it, and the exit code 1.  Every pool imported is recorded in S4.
func runAllocatorImport(ctx context.Context, prefix string, auth allocAdmin, wb *allocwriteback.Authority, only string, auditLog *audit.Log) int {
	pools := wb.WriteBackPools()
	if only != "" {
		var sel []loader.Pool
		for _, p := range pools {
			if p.Name == only {
				sel = append(sel, p)
			}
		}
		if len(sel) == 0 {
			fmt.Fprintf(os.Stderr, "%s: pool %q is not declared write_back: true in the intent\n", prefix, only)
			return 1
		}
		pools = sel
	}
	if len(pools) == 0 {
		fmt.Fprintf(os.Stderr, "%s: the intent declares no pool with write_back: true; nothing to import\n", prefix)
		return 1
	}
	code := 0
	for _, p := range pools {
		f, exists, err := wb.ReadFile(p.Name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: pool %s: %v\n", prefix, p.Name, err)
			code = 1
			continue
		}
		if !exists {
			fmt.Fprintf(os.Stdout, "pool %s: no write-back file %s; nothing to import\n", p.Name, wb.FilePath(p.Name))
			continue
		}
		entries := make([]allocator.ImportEntry, 0, len(f.Allocations))
		for _, e := range f.Allocations {
			entries = append(entries, allocator.ImportEntry{Owner: e.Owner, Value: e.Value})
		}
		// the pool's blocks and reuse: false come along, and so do the file's
		// last values: a rebuilt store hands out above them (#206).
		added, err := auth.ImportPool(ctx, p.Name, p.Spec(), entries, f.Last)
		if errors.Is(err, allocation.ErrFenced) {
			err = fmt.Errorf("%w (the store is served by `sakutei allocator`: import through --url)", err)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: pool %s: refused, nothing written: %v\n", prefix, p.Name, err)
			code = 1
			continue
		}
		if auditLog != nil {
			detail := fmt.Sprintf("imported pool %s from %s: %d added, %d already in the store", p.Name, wb.FilePath(p.Name), added, len(entries)-added)
			if err := auditLog.Append(audit.Record{Kind: audit.KindAllocationImported, Detail: detail, Count: added}); err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", prefix, err)
				return 1
			}
		}
		fmt.Fprintf(os.Stdout, "pool %s: imported %d allocation(s) from %s (%d already in the store)\n", p.Name, added, wb.FilePath(p.Name), len(entries)-added)
	}
	return code
}

// retired reports whether the released value of pool is retired (reuse:
// false, #206).
func retired(ctx context.Context, auth allocAdmin, pool, value string) bool {
	recs, err := auth.List(ctx, pool)
	if err != nil {
		return false
	}
	for _, r := range recs {
		if r.State == allocator.StateRetired && fmt.Sprint(r.Value) == value {
			return true
		}
	}
	return false
}

// cooldownUntil reads back when the released value of pool comes out of its
// cooldown (zero when it cannot be read: the release itself has succeeded).
func cooldownUntil(ctx context.Context, auth allocAdmin, pool, value string) time.Time {
	recs, err := auth.List(ctx, pool)
	if err != nil {
		return time.Time{}
	}
	for _, r := range recs {
		if r.State == allocator.StateCooldown && fmt.Sprint(r.Value) == value && r.Until != nil {
			return *r.Until
		}
	}
	return time.Time{}
}
