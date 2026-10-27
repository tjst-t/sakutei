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

package dnszone

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/filelock"
	"github.com/tjst-t/sakutei/pkg/core"
)

const concurrentZone = `$ORIGIN example.com.
@	300 IN SOA ns1 admin 2026102100 3600 900 604800 300
@	300 IN NS ns1.example.com.
`

// applyConcurrently runs n Applies at once against one zone file, each
// creating its own A record, while BulkObserve and Read run alongside.  It
// returns the names missing afterwards and the final SOA serial.
func applyConcurrently(t *testing.T, n int) (missing []string, serial string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "example.com.zone")
	if err := os.WriteFile(path, []byte(concurrentZone), 0o644); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	p := New(path, WithClock(func() time.Time { return day }))
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("h%d.example.com.", i)
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs <- p.Apply(ctx, core.Plan{Ops: []core.Op{{
				Kind:   core.OpKindCreate,
				Target: core.NewID(ProviderName, core.NewScope(nil), name+"/A"),
				Fields: []core.FieldDiff{{Field: "value", To: "192.0.2.1"}, {Field: "ttl", To: "300"}},
			}}})
		}()
		go func() {
			defer wg.Done()
			if _, err := p.BulkObserve(ctx, core.NewScope(nil)); err != nil {
				errs <- err
			}
			if _, err := p.Read(ctx, name+"/A"); err != nil {
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

	records, err := p.parseZone(ctx, p.zones[0], false)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, r := range records {
		have[r.name] = true
	}
	for i := 0; i < n; i++ {
		if name := fmt.Sprintf("h%d.example.com.", i); !have[name] {
			missing = append(missing, name)
		}
	}
	lines, err := p.readLines(ctx, p.zones[0], false)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		if f := strings.Fields(l); len(f) > 6 && f[3] == "SOA" {
			serial = f[6]
		}
	}
	return missing, serial
}

// Backlog #72: concurrent Applies to one zone file keep every change, and
// every one of them raises the SOA serial.
func TestApply_ConcurrentKeepsEveryChange(t *testing.T) {
	beforeWrite = func() { time.Sleep(time.Millisecond) }
	t.Cleanup(func() { beforeWrite = func() {} })

	const n = 50
	missing, serial := applyConcurrently(t, n)
	if len(missing) > 0 {
		t.Fatalf("%d of %d records lost: %v", len(missing), n, missing)
	}
	if want := fmt.Sprint(2026102100 + n); serial != want {
		t.Errorf("SOA serial = %s, want %s (one raise per Apply)", serial, want)
	}
}

// Backlog #72: the test above fails without the lock — two Applies that read
// the same old file lose one change.
func TestApply_ConcurrentWithoutLockLosesChanges(t *testing.T) {
	lockZone = func(string) (func(), error) { return func() {}, nil }
	beforeWrite = func() { time.Sleep(time.Millisecond) }
	t.Cleanup(func() {
		lockZone = filelock.Lock
		beforeWrite = func() {}
	})

	missing, _ := applyConcurrently(t, 50)
	if len(missing) == 0 {
		t.Fatal("without the lock no change was lost; the test does not exercise the race")
	}
	t.Logf("without the lock %d of 50 records were lost", len(missing))
}
