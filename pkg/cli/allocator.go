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

	"github.com/tjst-t/sakutei/internal/allocator"
	"github.com/tjst-t/sakutei/internal/audit"
	"github.com/tjst-t/sakutei/pkg/allocation"
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
// The first argument "release" or "list" selects the thin admin CLI instead
// (#116, runAllocatorAdmin); the service itself is unchanged.
func runAllocator(args []string) {
	if len(args) > 0 && (args[0] == "release" || args[0] == "list") {
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
	ReleaseOwner(ctx context.Context, pool, owner string) (string, bool, error)
	List(ctx context.Context, pool string) ([]allocator.Record, error)
}

// runAllocatorAdmin implements the thin admin CLI of the allocator (#116):
//
//	sakutei allocator release --pool <p> --owner <o> (--store <file> | --url <a,b>) [--audit-log <file>]
//	sakutei allocator list    (--store <file> | --url <a,b>) [--pool <p>]
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
	auditLog := fs.String("audit-log", "", "S4 audit log file to record the release in (release only, optional)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	usage := "usage: sakutei allocator list (--store <file> | --url <a,b>) [--pool <p>]"
	if verb == "release" {
		usage = "usage: sakutei allocator release --pool <p> --owner <o> (--store <file> | --url <a,b>) [--audit-log <file>]"
	}
	if (*store == "") == (*urls == "") || fs.NArg() != 0 || (verb == "release" && (*pool == "" || *owner == "")) {
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
	switch verb {
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
		until := cooldownUntil(ctx, auth, *pool, value)
		if *auditLog != "" {
			detail := fmt.Sprintf("released pool %s owner %s value %s", *pool, *owner, value)
			if !until.IsZero() {
				detail += " (cooldown until " + until.UTC().Format(time.RFC3339) + ")"
			}
			if err := audit.NewLog(*auditLog).Append(audit.Record{Kind: audit.KindAllocationReleased, Detail: detail}); err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", prefix, err)
				os.Exit(1)
			}
		}
		line := fmt.Sprintf("released value %s of pool %s from owner %s", value, *pool, *owner)
		if !until.IsZero() {
			line += fmt.Sprintf("; cooling down until %s, not reused before", until.UTC().Format(time.RFC3339))
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
			fmt.Fprintf(os.Stdout, "%-16s %-32s %-8d %-10s %s\n", r.Pool, owner, r.Value, r.State, left)
		}
	}
}

// cooldownUntil reads back when the released value of pool comes out of its
// cooldown (zero when it cannot be read: the release itself has succeeded).
func cooldownUntil(ctx context.Context, auth allocAdmin, pool, value string) time.Time {
	recs, err := auth.List(ctx, pool)
	if err != nil {
		return time.Time{}
	}
	for _, r := range recs {
		if r.State == "cooldown" && fmt.Sprint(r.Value) == value && r.Until != nil {
			return *r.Until
		}
	}
	return time.Time{}
}
