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

package nftables

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/filelock"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

const concurrentRuleset = `table ip filter {
	chain input {
		type filter hook input priority 0; policy drop;
	}
}
`

// applyConcurrently runs n Applies at once against one ruleset file, each
// creating its own chain, while BulkObserve and Read run alongside.  It
// returns the chains missing afterwards.
func applyConcurrently(t *testing.T, n int) (missing []string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fw.rules")
	if err := os.WriteFile(path, []byte(concurrentRuleset), 0o644); err != nil {
		t.Fatal(err)
	}
	p := New(path)
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	for i := 0; i < n; i++ {
		localID := fmt.Sprintf("ip/filter/c%d", i)
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs <- p.Apply(ctx, core.Plan{Ops: []core.Op{{
				Kind:   core.OpKindCreate,
				Target: core.NewID(ProviderName, core.NewScope(nil), localID),
				Fields: []core.FieldDiff{{Field: "rules", To: "tcp dport 22 accept"}},
			}}})
		}()
		go func() {
			defer wg.Done()
			if _, err := p.BulkObserve(ctx, core.NewScope(nil)); err != nil {
				errs <- err
			}
			if _, err := p.Read(ctx, localID); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent call: %v", err)
		}
	}

	chains, err := p.parseRuleset()
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, c := range chains {
		have[c.localID()] = true
	}
	for i := 0; i < n; i++ {
		if id := fmt.Sprintf("ip/filter/c%d", i); !have[id] {
			missing = append(missing, id)
		}
	}
	if !have["ip/filter/input"] {
		t.Error("the base chain input was lost")
	}
	return missing
}

// Backlog #72: concurrent Applies to one ruleset file keep every change.
func TestApply_ConcurrentKeepsEveryChange(t *testing.T) {
	beforeWrite = func() { time.Sleep(time.Millisecond) }
	t.Cleanup(func() { beforeWrite = func() {} })

	const n = 50
	if missing := applyConcurrently(t, n); len(missing) > 0 {
		t.Fatalf("%d of %d chains lost: %v", len(missing), n, missing)
	}
}

// Backlog #72: the test above fails without the lock — two Applies that read
// the same old file lose one change.
func TestApply_ConcurrentWithoutLockLosesChanges(t *testing.T) {
	lockRuleset = func(string) (func(), error) { return func() {}, nil }
	beforeWrite = func() { time.Sleep(time.Millisecond) }
	t.Cleanup(func() {
		lockRuleset = filelock.Lock
		beforeWrite = func() {}
	})

	missing := applyConcurrently(t, 50)
	if len(missing) == 0 {
		t.Fatal("without the lock no change was lost; the test does not exercise the race")
	}
	t.Logf("without the lock %d of 50 chains were lost", len(missing))
}
