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

package filelock

import (
	"path/filepath"
	"testing"
	"time"
)

// blocks reports whether f is still running after a short wait, then waits
// for it to finish once release has been called.
func blocks(t *testing.T, f func(), release func()) bool {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
		release()
		return false
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("still blocked after release")
	}
	return true
}

func TestLock_ExcludesLockAndRLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zone")
	unlock, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	if !blocks(t, func() {
		u, err := Lock(path)
		if err != nil {
			t.Error(err)
			return
		}
		u()
	}, unlock) {
		t.Error("a second Lock did not wait for the first")
	}

	unlock, _ = Lock(path)
	if !blocks(t, func() { RLock(path)() }, unlock) {
		t.Error("RLock did not wait for Lock")
	}

	// Readers do not wait for each other.
	runlock := RLock(path)
	if blocks(t, func() { RLock(path)() }, runlock) {
		t.Error("RLock waited for another RLock")
	}
}
