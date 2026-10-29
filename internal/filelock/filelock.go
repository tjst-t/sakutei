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

// Package filelock serialises every read-modify-write of one file (Backlog
// #72).  Providers that keep their reality in a file (dnszone, nftables)
// read the whole file, change it and rename a new file over it (§4.9).  Two
// such writes that read the same old content lose one change, so each one
// runs inside Lock: an in-process mutex per path (the daemon runs Applies
// concurrently) plus an flock on "<path>.lock" (plan and reconcile run as
// separate processes), as the allocator's FileAuthority does.
//
// Reads (Read, List, BulkObserve) take RLock, a shared lock: they never see a
// half-written file anyway (the write is a rename), but they do not run in
// the middle of a writer's read-modify-write.
//
// The lock is on a side file, not the file itself: rename replaces the
// file's inode, so an flock on it would not exclude the next writer.
package filelock

import (
	"path/filepath"
	"sync"
)

var (
	mapMu sync.Mutex
	locks = map[string]*sync.RWMutex{}
)

// mutexFor returns the process-wide mutex for path (made absolute and clean,
// so two providers naming one file by different paths share it).
func mutexFor(path string) *sync.RWMutex {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	mapMu.Lock()
	defer mapMu.Unlock()
	m, ok := locks[path]
	if !ok {
		m = &sync.RWMutex{}
		locks[path] = m
	}
	return m
}

// Lock takes the exclusive lock for path and returns the function that
// releases it.  It blocks until no other goroutine of this process, and no
// other process, holds it.  An error (the lock file cannot be opened or
// locked) leaves nothing held.
func Lock(path string) (func(), error) {
	m := mutexFor(path)
	m.Lock()
	unlock, err := lockFile(path+".lock", true)
	if err != nil {
		m.Unlock()
		return nil, err
	}
	return func() {
		unlock()
		m.Unlock()
	}, nil
}

// RLock takes the shared lock for path and returns the function that
// releases it: it waits for a writer holding Lock, but not for other readers.
// Where the lock file cannot be opened (a read-only directory, as when plan
// runs without write access), it holds only the in-process lock: a reader
// that cannot create the lock file has no writer of its own to exclude, and
// the rename still keeps it from reading a half-written file.
func RLock(path string) func() {
	m := mutexFor(path)
	m.RLock()
	unlock, err := lockFile(path+".lock", false)
	if err != nil {
		return m.RUnlock
	}
	return func() {
		unlock()
		m.RUnlock()
	}
}

// TryLock takes the exclusive lock for path without waiting (Backlog #209:
// only one process advances the Workflows of a store).  ok is false, with
// nothing held, when another goroutine of this process or another process
// holds it.  An error (the lock file cannot be opened) leaves nothing held.
func TryLock(path string) (unlock func(), ok bool, err error) {
	m := mutexFor(path)
	if !m.TryLock() {
		return nil, false, nil
	}
	unlockFile, ok, err := tryLockFile(path + ".lock")
	if err != nil || !ok {
		m.Unlock()
		return nil, false, err
	}
	return func() {
		unlockFile()
		m.Unlock()
	}, true, nil
}
