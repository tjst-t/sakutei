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
	"errors"
	"fmt"
	"os"
	"syscall"
)

// lockFile takes an flock on path (created if needed) — exclusive or shared —
// so that several processes sharing one file serialise their
// read-modify-write.
func lockFile(path string, exclusive bool) (func(), error) {
	unlock, _, err := flockFile(path, exclusive, false)
	return unlock, err
}

// tryLockFile takes the exclusive flock on path without waiting; ok is false
// when another holder has it.
func tryLockFile(path string) (func(), bool, error) { return flockFile(path, true, true) }

func flockFile(path string, exclusive, try bool) (func(), bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("filelock: open %s: %w", path, err)
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if try {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		if try && errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("filelock: lock %s: %w", path, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, true, nil
}
