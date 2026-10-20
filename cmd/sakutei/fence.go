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
	"time"

	"github.com/tjst-t/sakutei/internal/fencing"
)

// runFence implements "sakutei fence": print the fencing generation and the
// holder of every scope in the store (#28, §17.6).  It is read-only: the
// generation is bumped only by a shard that starts converging the scope, so
// re-sharding never needs a hand edit of the store.
func runFence(args []string) {
	fs := flag.NewFlagSet("sakutei fence", flag.ExitOnError)
	store := fs.String("store", "", "fencing generation store file (required; same as the daemon's --fencing-store)")
	_ = fs.Parse(args)
	if *store == "" {
		fmt.Fprintln(os.Stderr, "sakutei fence: --store <file> is required")
		os.Exit(1)
	}
	toks, err := fencing.NewFileStore(*store).List(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "sakutei fence: %v\n", err)
		os.Exit(1)
	}
	if len(toks) == 0 {
		fmt.Fprintln(os.Stdout, "no scope has been claimed")
		return
	}
	fmt.Fprintf(os.Stdout, "%-40s %10s  %-30s %s\n", "SCOPE", "GENERATION", "HOLDER", "ACQUIRED")
	for _, t := range toks {
		fmt.Fprintf(os.Stdout, "%-40s %10d  %-30s %s\n", t.Scope, t.Generation, t.Holder, t.AcquiredAt.Format(time.RFC3339))
	}
}
