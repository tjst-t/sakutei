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
//go:build unix

package filelock

import (
	"os"
	"path/filepath"
	"testing"
)

// The flock excludes another open of the lock file — what another process
// does — not only this process's mutex.
func TestLockFile_ExcludesAnotherOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zone.lock")
	unlock, err := lockFile(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if !blocks(t, func() {
		u, err := lockFile(path, true)
		if err != nil {
			t.Error(err)
			return
		}
		u()
	}, unlock) {
		t.Error("a second flock did not wait for the first")
	}
}

// A reader that cannot create the lock file still reads.
func TestRLock_ReadOnlyDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	RLock(filepath.Join(dir, "zone"))()
	if _, err := Lock(filepath.Join(dir, "zone")); err == nil && os.Geteuid() != 0 {
		t.Error("Lock in a read-only directory: want an error")
	}
}
