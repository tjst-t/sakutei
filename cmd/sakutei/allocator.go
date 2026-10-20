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
	"os/signal"
	"syscall"
	"time"

	"github.com/tjst-t/sakutei/internal/allocator"
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
func runAllocator(args []string) {
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

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sakutei allocator: listen: %v\n", err)
		os.Exit(1)
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
